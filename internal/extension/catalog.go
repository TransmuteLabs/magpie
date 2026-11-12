package extension

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	catalogFileLimit    = 64 << 20
	catalogPackageLimit = 256 << 20
	catalogFilesLimit   = 4096
	catalogJSONLimit    = catalogPackageLimit
)

var (
	ErrCatalogConflict    = errors.New("extension catalog changed during installation")
	errCatalogRootMissing = errors.New("extension catalog root does not exist")
)

type Catalog struct {
	Root string
}

type Installed struct {
	Manifest  sdk.Manifest      `json:"manifest"`
	Directory string            `json:"directory"`
	Files     map[string]string `json:"files"`
	Disabled  bool              `json:"disabled"`
	Revision  string            `json:"revision"`
}

func NewCatalog(root string) *Catalog {
	return &Catalog{Root: root}
}

func (catalog *Catalog) List() ([]Installed, error) {
	root, err := catalog.open(false)
	if errors.Is(err, errCatalogRootMissing) {
		return []Installed{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	unlock, err := catalogLock(context.Background(), root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, _, err := catalogRead(root)
	return entries, err
}

func (catalog *Catalog) Install(ctx context.Context, source string, check func(context.Context, Installed) error) (Installed, error) {
	if err := ctx.Err(); err != nil {
		return Installed{}, err
	}
	if check == nil {
		return Installed{}, errors.New("extension installation requires a handshake check")
	}
	source = filepath.Clean(source)
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return Installed{}, err
	}
	if !sourceInfo.IsDir() {
		return Installed{}, errors.New("extension package must be a directory, not a symlink")
	}
	input, err := os.OpenRoot(source)
	if err != nil {
		return Installed{}, err
	}
	defer input.Close()
	if err := catalogDirectoryIdentity(input, sourceInfo); err != nil {
		return Installed{}, err
	}
	manifest, err := catalogManifest(input)
	if err != nil {
		return Installed{}, err
	}
	root, err := catalog.open(true)
	if err != nil {
		return Installed{}, err
	}
	defer root.Close()
	sourcePath, err := filepath.EvalSymlinks(source)
	if err != nil {
		return Installed{}, err
	}
	rootPath, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return Installed{}, err
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return Installed{}, err
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return Installed{}, err
	}
	if relative, err := filepath.Rel(sourcePath, rootPath); err == nil && (relative == "." || filepath.IsLocal(relative)) {
		return Installed{}, errors.New("extension catalog cannot be inside the source package")
	}
	unlock, err := catalogLock(ctx, root)
	if err != nil {
		return Installed{}, err
	}
	previous, before, err := catalogRead(root)
	unlock()
	if err != nil {
		return Installed{}, err
	}
	parent, err := catalogPrivateDir(root, "packages")
	if err != nil {
		return Installed{}, err
	}
	defer parent.Close()
	releases, err := catalogPrivateDir(parent, manifest.ID)
	if err != nil {
		return Installed{}, err
	}
	defer releases.Close()
	release := manifest.Version + "-" + rand.Text()
	if err := releases.Mkdir(release, 0o700); err != nil {
		return Installed{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = releases.RemoveAll(release)
		}
	}()
	output, err := releases.OpenRoot(release)
	if err != nil {
		return Installed{}, err
	}
	defer output.Close()
	files, err := catalogCopy(ctx, input, output)
	if err != nil {
		return Installed{}, err
	}
	if err := catalogSync(releases); err != nil {
		return Installed{}, err
	}
	if err := catalogSync(parent); err != nil {
		return Installed{}, err
	}
	frozen, err := catalogManifest(output)
	if err != nil {
		return Installed{}, err
	}
	if !catalogSameManifest(manifest, frozen) {
		return Installed{}, errors.New("extension manifest changed during installation")
	}
	entry := Installed{
		Manifest:  frozen,
		Directory: path.Join("packages", manifest.ID, release),
		Files:     files,
	}
	for _, installed := range previous {
		if installed.Manifest.ID == entry.Manifest.ID {
			entry.Disabled = installed.Disabled
			break
		}
	}
	entry.Revision = catalogRevision(entry)
	if err := catalog.verify(ctx, entry); err != nil {
		return Installed{}, err
	}
	checkEntry, err := catalogClone(entry)
	if err != nil {
		return Installed{}, err
	}
	if err := check(ctx, checkEntry); err != nil {
		return Installed{}, fmt.Errorf("extension handshake: %w", err)
	}
	if err := catalog.verify(ctx, entry); err != nil {
		return Installed{}, err
	}
	unlock, err = catalogLock(ctx, root)
	if err != nil {
		return Installed{}, err
	}
	defer unlock()
	entries, after, err := catalogRead(root)
	if err != nil {
		return Installed{}, err
	}
	if before != after {
		return Installed{}, ErrCatalogConflict
	}
	found := false
	for index := range entries {
		if entries[index].Manifest.ID == entry.Manifest.ID {
			entries[index] = entry
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, entry)
	}
	keep, err = catalogWrite(ctx, root, entries)
	if err != nil {
		return Installed{}, err
	}
	return entry, nil
}

func (catalog *Catalog) SetEnabled(ctx context.Context, id string, on bool) error {
	return catalog.change(ctx, id, func(entries []Installed, index int) []Installed {
		entries[index].Disabled = !on
		return entries
	})
}

func (catalog *Catalog) Remove(ctx context.Context, id string) error {
	return catalog.change(ctx, id, func(entries []Installed, index int) []Installed {
		return slices.Delete(entries, index, index+1)
	})
}

func (catalog *Catalog) change(ctx context.Context, id string, change func([]Installed, int) []Installed) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := catalog.open(false)
	if errors.Is(err, errCatalogRootMissing) {
		return fmt.Errorf("unknown extension %q", id)
	}
	if err != nil {
		return err
	}
	defer root.Close()
	unlock, err := catalogLock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	entries, _, err := catalogRead(root)
	if err != nil {
		return err
	}
	for index, entry := range entries {
		if entry.Manifest.ID == id {
			_, err := catalogWrite(ctx, root, change(entries, index))
			return err
		}
	}
	return fmt.Errorf("unknown extension %q", id)
}

func (catalog *Catalog) Verify(entry Installed) error {
	return catalog.verify(context.Background(), entry)
}

func (catalog *Catalog) verify(ctx context.Context, entry Installed) error {
	if err := catalogValidate(entry); err != nil {
		return err
	}
	root, err := catalog.open(false)
	if err != nil {
		return err
	}
	defer root.Close()
	directory := root
	for _, segment := range strings.Split(entry.Directory, "/") {
		info, err := directory.Lstat(segment)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("extension directory %q is not a real directory", segment)
		}
		child, err := directory.OpenRoot(segment)
		if err != nil {
			return err
		}
		defer child.Close()
		if err := catalogDirectoryIdentity(child, info); err != nil {
			return err
		}
		directory = child
	}
	manifest, err := catalogManifest(directory)
	if err != nil {
		return err
	}
	if !catalogSameManifest(manifest, entry.Manifest) {
		return errors.New("extension manifest does not match its frozen catalog entry")
	}
	files := 0
	var total int64
	err = fs.WalkDir(directory.FS(), ".", func(name string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.IsDir() {
			return nil
		}
		expected, exists := entry.Files[name]
		if !exists {
			return fmt.Errorf("unexpected extension file %q", name)
		}
		digest, size, err := catalogHash(ctx, directory, name, catalogPackageLimit-total, nil)
		if err != nil {
			return err
		}
		if expected != digest {
			return fmt.Errorf("extension file %q failed its hash check", name)
		}
		if name == entry.Manifest.Entry && entry.Manifest.Runtime == "exec" && runtime.GOOS != "windows" {
			info, err := directory.Lstat(name)
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o100 == 0 {
				return errors.New("extension exec entry is not executable by its owner")
			}
		}
		total += size
		files++
		if total > catalogPackageLimit || files > catalogFilesLimit {
			return errors.New("extension package exceeds its size or file limit")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if files != len(entry.Files) {
		return errors.New("extension package is missing cataloged files")
	}
	return nil
}

func (catalog *Catalog) open(create bool) (*os.Root, error) {
	if catalog == nil || strings.TrimSpace(catalog.Root) == "" {
		return nil, errors.New("extension catalog root is required")
	}
	name := filepath.Clean(catalog.Root)
	if create {
		if err := os.MkdirAll(name, 0o700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, errCatalogRootMissing
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("extension catalog root is not a real directory")
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if err := catalogDirectoryIdentity(root, info); err != nil {
		root.Close()
		return nil, err
	}
	if create {
		if err := root.Chmod(".", 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	return root, nil
}

func catalogPrivateDir(root *os.Root, name string) (*os.Root, error) {
	if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("extension folder %q is not a real directory", name)
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if err := catalogDirectoryIdentity(directory, info); err != nil {
		directory.Close()
		return nil, err
	}
	if err := directory.Chmod(".", 0o700); err != nil {
		directory.Close()
		return nil, err
	}
	return directory, nil
}

func catalogDirectoryIdentity(directory *os.Root, before fs.FileInfo) error {
	current, err := directory.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(before, current) {
		return errors.New("extension directory changed while opening it")
	}
	return nil
}

func catalogLock(ctx context.Context, root *os.Root) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, filelock.Wait)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := root.OpenFile("catalog.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, fs.ErrExist) {
		info, statErr := root.Lstat("catalog.lock")
		if statErr != nil {
			return nil, statErr
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("extension catalog lock is not a regular file")
		}
		file, err = root.OpenFile("catalog.lock", os.O_RDWR, 0o600)
	}
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	named, err := root.Lstat("catalog.lock")
	if err != nil {
		file.Close()
		return nil, err
	}
	if !named.Mode().IsRegular() || !os.SameFile(opened, named) {
		file.Close()
		return nil, errors.New("extension catalog lock changed while opening it")
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		held, err := filelock.TryLock(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		if held {
			return func() { filelock.Unlock(file); file.Close() }, nil
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type catalogSnapshot struct {
	present bool
	digest  [sha256.Size]byte
}

func catalogRead(root *os.Root) ([]Installed, catalogSnapshot, error) {
	if _, err := root.Lstat("catalog.json"); errors.Is(err, fs.ErrNotExist) {
		return []Installed{}, catalogSnapshot{}, nil
	} else if err != nil {
		return nil, catalogSnapshot{}, err
	}
	data, err := catalogReadFile(root, "catalog.json", catalogJSONLimit)
	if err != nil {
		return nil, catalogSnapshot{}, err
	}
	var entries []Installed
	if err := catalogDecode(data, &entries); err != nil {
		return nil, catalogSnapshot{}, fmt.Errorf("read extension catalog: %w", err)
	}
	if entries == nil {
		return nil, catalogSnapshot{}, errors.New("extension catalog must be a JSON array")
	}
	if err := catalogValidateEntries(entries); err != nil {
		return nil, catalogSnapshot{}, fmt.Errorf("read extension catalog: %w", err)
	}
	return entries, catalogSnapshot{present: true, digest: sha256.Sum256(data)}, nil
}

func catalogWrite(ctx context.Context, root *os.Root, entries []Installed) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := catalogValidateEntries(entries); err != nil {
		return false, err
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return false, err
	}
	if len(data) > catalogJSONLimit {
		return false, errors.New("extension catalog exceeds its size limit")
	}
	temporary := ".catalog-" + rand.Text()
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer root.Remove(temporary)
	_, err = file.Write(data)
	if err == nil {
		err = file.Chmod(0o600)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err = root.Rename(temporary, "catalog.json")
		if err == nil {
			break
		}
		if runtime.GOOS != "windows" || attempt == 49 {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := catalogSync(root); err != nil {
		return true, err
	}
	return true, nil
}

func catalogValidateEntries(entries []Installed) error {
	ids := map[string]bool{}
	paths, providers, agents, sessions := map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
	for _, entry := range entries {
		if err := catalogValidate(entry); err != nil {
			return err
		}
		if ids[entry.Manifest.ID] {
			return fmt.Errorf("duplicate catalog extension %q", entry.Manifest.ID)
		}
		ids[entry.Manifest.ID] = true
		if entry.Disabled {
			continue
		}
		claim := func(owners map[string]string, kind, id string) error {
			if owner, found := owners[id]; found {
				return fmt.Errorf("extension %s %q is already owned by %q", kind, id, owner)
			}
			owners[id] = entry.Manifest.ID
			return nil
		}
		for _, inbound := range entry.Manifest.Inbounds {
			if err := claim(paths, "inbound path", inbound.Path); err != nil {
				return err
			}
		}
		for _, provider := range entry.Manifest.Providers {
			if err := claim(providers, "provider", provider.ID); err != nil {
				return err
			}
		}
		for _, agent := range entry.Manifest.Agents {
			if err := claim(agents, "agent", agent.ID); err != nil {
				return err
			}
		}
		for _, session := range entry.Manifest.Sessions {
			if err := claim(sessions, "session format", session.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func catalogValidate(entry Installed) error {
	if err := entry.Manifest.Validate(); err != nil {
		return err
	}
	parts := strings.Split(entry.Directory, "/")
	if !catalogRelative(entry.Directory) || len(parts) != 3 || parts[0] != "packages" ||
		parts[1] != entry.Manifest.ID || !strings.HasPrefix(parts[2], entry.Manifest.Version+"-") ||
		len(parts[2]) <= len(entry.Manifest.Version)+1 {
		return errors.New("extension directory must be a managed package generation")
	}
	if len(entry.Files) == 0 || len(entry.Files) > catalogFilesLimit {
		return errors.New("invalid extension file inventory")
	}
	for name, digest := range entry.Files {
		if !catalogRelative(name) || !catalogDigest(digest) {
			return errors.New("invalid extension file path or hash")
		}
	}
	if _, exists := entry.Files[sdk.ManifestFile]; !exists {
		return errors.New("extension inventory has no manifest")
	}
	if _, exists := entry.Files[entry.Manifest.Entry]; !exists {
		return errors.New("extension inventory has no entry")
	}
	if !catalogDigest(entry.Revision) || entry.Revision != catalogRevision(entry) {
		return errors.New("extension revision does not match its frozen metadata")
	}
	return nil
}

func catalogRevision(entry Installed) string {
	entry.Disabled, entry.Revision = false, ""
	data, _ := json.Marshal(entry)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func catalogDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func catalogRelative(name string) bool {
	return fs.ValidPath(name) && name != "." && filepath.IsLocal(filepath.FromSlash(name)) &&
		!strings.ContainsAny(name, "\\:") && !strings.ContainsFunc(name, unicode.IsControl)
}

func catalogManifest(root *os.Root) (sdk.Manifest, error) {
	data, err := catalogReadFile(root, sdk.ManifestFile, catalogFileLimit)
	if err != nil {
		return sdk.Manifest{}, err
	}
	var manifest sdk.Manifest
	if err := catalogDecode(data, &manifest); err != nil {
		return sdk.Manifest{}, fmt.Errorf("read extension manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return sdk.Manifest{}, err
	}
	return manifest, nil
}

func catalogDecode(data []byte, target any) error {
	tokens := json.NewDecoder(bytes.NewReader(data))
	tokens.UseNumber()
	if err := catalogJSONValue(tokens, 0); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func catalogJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("extension JSON nesting exceeds its limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || keys[key] {
				return fmt.Errorf("invalid or duplicate extension JSON key %q", key)
			}
			keys[key] = true
		}
		if err := catalogJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func catalogSameManifest(first, second sdk.Manifest) bool {
	left, leftErr := json.Marshal(first)
	right, rightErr := json.Marshal(second)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func catalogClone(entry Installed) (Installed, error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return Installed{}, err
	}
	var clone Installed
	err = json.Unmarshal(data, &clone)
	return clone, err
}

func catalogReadFile(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("extension file %q is not a bounded regular file", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("extension file %q changed while opening it", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("extension file %q exceeds its size limit", name)
	}
	return data, nil
}

func catalogCopy(ctx context.Context, input, output *os.Root) (map[string]string, error) {
	files := map[string]string{}
	directories := []string{"."}
	var total int64
	err := fs.WalkDir(input.FS(), ".", func(name string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if !catalogRelative(name) {
			return fmt.Errorf("invalid extension package path %q", name)
		}
		if item.IsDir() {
			if err := output.Mkdir(name, 0o700); err != nil {
				return err
			}
			directories = append(directories, name)
			return nil
		}
		if len(files) == catalogFilesLimit {
			return errors.New("extension package exceeds its file limit")
		}
		info, err := input.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > catalogFileLimit || total+info.Size() > catalogPackageLimit {
			return fmt.Errorf("extension file %q is not a bounded regular file", name)
		}
		file, err := output.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		digest, size, err := catalogHash(ctx, input, name, catalogPackageLimit-total, file)
		if err == nil {
			err = file.Chmod(0o600 | info.Mode().Perm()&0o100)
		}
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		total += size
		if total > catalogPackageLimit {
			return errors.New("extension package exceeds its size limit")
		}
		files[name] = digest
		return nil
	})
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		for index := len(directories) - 1; index >= 0; index-- {
			directory, err := output.OpenRoot(directories[index])
			if err != nil {
				return nil, err
			}
			err = catalogSync(directory)
			directory.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

func catalogHash(ctx context.Context, root *os.Root, name string, remaining int64, output io.Writer) (string, int64, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return "", 0, err
	}
	limit := min(int64(catalogFileLimit), remaining)
	if !info.Mode().IsRegular() || info.Size() > limit {
		return "", 0, fmt.Errorf("extension file %q is not a bounded regular file", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(info, opened) {
		return "", 0, fmt.Errorf("extension file %q changed while opening it", name)
	}
	hash := sha256.New()
	var writer io.Writer = hash
	if output != nil {
		writer = io.MultiWriter(hash, output)
	}
	reader := io.LimitReader(file, min(limit, info.Size())+1)
	buffer := make([]byte, 64<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", size, err
		}
		count, readErr := reader.Read(buffer)
		size += int64(count)
		if size > limit {
			return "", size, fmt.Errorf("extension file %q exceeds its size limit", name)
		}
		if _, err := writer.Write(buffer[:count]); err != nil {
			return "", size, err
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", size, readErr
		}
	}
	final, err := file.Stat()
	if err != nil {
		return "", size, err
	}
	if size != info.Size() || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
		return "", size, fmt.Errorf("extension file %q changed during its read", name)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func catalogSync(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
