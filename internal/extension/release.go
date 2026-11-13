package extension

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const releaseMetadataLimit = 1 << 20

type releaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type githubRelease struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

type ReleasePackage struct {
	Directory string
	Source    PackageSource
}

type PackageSource struct {
	Reference string `json:"reference"`
	Tag       string `json:"tag"`
	Archive   string `json:"archive"`
	SHA256    string `json:"sha256"`
}

func (source PackageSource) Validate() error {
	_, pinned, err := releaseSource(source.Reference)
	if err != nil {
		return err
	}
	if !releaseTag(source.Tag) || pinned != "" && pinned != source.Tag {
		return errors.New("release provenance tag is invalid or differs from its pin")
	}
	if source.Archive != "magpie-plugin_any.zip" {
		name, ok := strings.CutPrefix(source.Archive, "magpie-plugin_")
		name, suffix := strings.CutSuffix(name, ".zip")
		parts := strings.Split(name, "_")
		if !ok || !suffix || len(parts) != 2 || !releasePlatformPart(parts[0]) || !releasePlatformPart(parts[1]) {
			return errors.New("invalid release provenance archive name")
		}
	}
	if !catalogDigest(source.SHA256) {
		return errors.New("invalid release provenance SHA-256")
	}
	return nil
}

func releasePlatformPart(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func PrepareRelease(ctx context.Context, client *http.Client, source string) (prepared ReleasePackage, cleanup func(), err error) {
	return prepareRelease(ctx, client, source, defaultReleaseToken)
}

func prepareRelease(ctx context.Context, client *http.Client, source string, resolver releaseTokenResolver) (prepared ReleasePackage, cleanup func(), err error) {
	if ctx == nil || client == nil {
		return ReleasePackage{}, nil, errors.New("release preparation requires a context and HTTP client")
	}
	if err := ctx.Err(); err != nil {
		return ReleasePackage{}, nil, err
	}
	endpoint, pinned, err := releaseSource(source)
	if err != nil {
		return ReleasePackage{}, nil, err
	}
	safe := *client
	safe.Jar = nil
	safe.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
			request.Header.Del(name)
		}
		if len(via) >= 10 {
			return errors.New("too many release redirects")
		}
		if err := releaseURL(request.URL.String()); err != nil {
			return err
		}
		if client.CheckRedirect != nil {
			if err := client.CheckRedirect(request, via); err != nil {
				return err
			}
		}
		if err := releaseURL(request.URL.String()); err != nil {
			return err
		}
		for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
			request.Header.Del(name)
		}
		return nil
	}
	repository, _, _ := strings.Cut(strings.TrimPrefix(endpoint, "https://api.github.com/repos/"), "/releases/")
	authorization := releaseAuthorization{client: &safe, repository: repository, resolver: resolver}
	var metadata bytes.Buffer
	if err := authorization.metadata(ctx, endpoint, &metadata); err != nil {
		return ReleasePackage{}, nil, fmt.Errorf("read GitHub release: %w", err)
	}
	tokens := json.NewDecoder(bytes.NewReader(metadata.Bytes()))
	tokens.UseNumber()
	if err := catalogJSONValue(tokens, 0); err != nil {
		return ReleasePackage{}, nil, fmt.Errorf("invalid GitHub release metadata: %w", err)
	}
	var release githubRelease
	if err := json.Unmarshal(metadata.Bytes(), &release); err != nil {
		return ReleasePackage{}, nil, fmt.Errorf("invalid GitHub release metadata: %w", err)
	}
	if !releaseTag(release.Tag) || pinned != "" && release.Tag != pinned {
		return ReleasePackage{}, nil, errors.New("GitHub release tag is invalid or differs from the pinned tag")
	}
	asset, sums, universal, err := releaseSelect(release.Assets)
	if err != nil {
		return ReleasePackage{}, nil, err
	}
	if authorization.authenticated && (asset.ID <= 0 || sums.ID <= 0) {
		return ReleasePackage{}, nil, errors.New("authenticated GitHub releases require asset IDs for the archive and checksums.txt")
	}
	var checksums bytes.Buffer
	if _, err := authorization.asset(ctx, sums, &checksums, releaseMetadataLimit); err != nil {
		return ReleasePackage{}, nil, fmt.Errorf("read release checksums: %w", err)
	}
	expected, err := releaseChecksum(checksums.Bytes(), asset.Name)
	if err != nil {
		return ReleasePackage{}, nil, err
	}
	temporary, err := os.MkdirTemp("", "magpie-release-")
	if err != nil {
		return ReleasePackage{}, nil, err
	}
	remove := func() { _ = os.RemoveAll(temporary) }
	keep := false
	defer func() {
		if !keep {
			remove()
		}
	}()
	root, err := os.OpenRoot(temporary)
	if err != nil {
		return ReleasePackage{}, nil, err
	}
	defer root.Close()
	archive, err := root.OpenFile("download.zip", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return ReleasePackage{}, nil, err
	}
	defer archive.Close()
	digest := sha256.New()
	size, err := authorization.asset(ctx, asset, io.MultiWriter(archive, digest), catalogPackageLimit)
	if err != nil {
		return ReleasePackage{}, nil, fmt.Errorf("download release archive: %w", err)
	}
	if !bytes.Equal(digest.Sum(nil), expected) {
		return ReleasePackage{}, nil, errors.New("release archive SHA-256 does not match checksums.txt")
	}
	pack, err := releaseZIPReader(ctx, archive, size)
	if err != nil {
		return ReleasePackage{}, nil, fmt.Errorf("open release ZIP: %w", err)
	}
	if err := releaseCheckZIP(pack); err != nil {
		return ReleasePackage{}, nil, err
	}
	output, err := catalogPrivateDir(root, "package")
	if err != nil {
		return ReleasePackage{}, nil, err
	}
	defer output.Close()
	if err := releaseExtract(ctx, pack, output); err != nil {
		return ReleasePackage{}, nil, err
	}
	manifest, err := catalogManifest(output)
	if err != nil {
		return ReleasePackage{}, nil, fmt.Errorf("validate release manifest: %w", err)
	}
	if manifest.Version != strings.TrimPrefix(release.Tag, "v") {
		return ReleasePackage{}, nil, errors.New("release manifest version differs from the release tag")
	}
	if universal {
		if manifest.Runtime != "bun" {
			return ReleasePackage{}, nil, errors.New("universal release archives require the Bun runtime")
		}
		for _, service := range manifest.Services {
			if service.Runtime != "bun" {
				return ReleasePackage{}, nil, errors.New("universal release archives cannot contain native executable services")
			}
		}
	}
	entry, err := output.Lstat(manifest.Entry)
	if err != nil || !entry.Mode().IsRegular() {
		return ReleasePackage{}, nil, errors.New("release manifest entry is not a regular package file")
	}
	if err := ctx.Err(); err != nil {
		return ReleasePackage{}, nil, err
	}
	prepared = ReleasePackage{Directory: filepath.Join(temporary, "package"), Source: PackageSource{
		Reference: source, Tag: release.Tag, Archive: asset.Name, SHA256: hex.EncodeToString(digest.Sum(nil)),
	}}
	keep = true
	return prepared, remove, nil
}

func releaseSource(source string) (string, string, error) {
	repository, ok := strings.CutPrefix(source, "github:")
	if !ok || strings.Count(repository, "@") > 1 {
		return "", "", errors.New("release source must be github:owner/repo[@vSEMVER]")
	}
	repository, tag, pinned := strings.Cut(repository, "@")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !releaseSegment(parts[0], false) || !releaseSegment(parts[1], true) ||
		pinned && !releaseTag(tag) {
		return "", "", errors.New("invalid GitHub release source")
	}
	endpoint := "https://api.github.com/repos/" + parts[0] + "/" + parts[1] + "/releases/"
	if pinned {
		endpoint += "tags/" + url.PathEscape(tag)
	} else {
		endpoint += "latest"
	}
	if err := releaseURL(endpoint); err != nil {
		return "", "", err
	}
	return endpoint, tag, nil
}

func releaseSegment(value string, repository bool) bool {
	if value == "" || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' {
			continue
		}
		if !repository || char != '.' && char != '_' {
			return false
		}
	}
	return true
}

func releaseTag(tag string) bool {
	candidate := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "release", Version: strings.TrimPrefix(tag, "v"), Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OperationControl}},
	}
	return candidate.Validate() == nil
}

func releaseURL(raw string) error {
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.ContainsFunc(decoded, unicode.IsControl) || strings.Contains(raw, "#") {
		return errors.New("invalid release URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Fragment != "" {
		return errors.New("release URLs must use HTTPS without credentials or fragments")
	}
	return nil
}

func releaseSelect(assets []releaseAsset) (releaseAsset, releaseAsset, bool, error) {
	name := "magpie-plugin_" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
	var platform, universal, checksums *releaseAsset
	for index := range assets {
		asset := &assets[index]
		var selected **releaseAsset
		switch asset.Name {
		case name:
			selected = &platform
		case "magpie-plugin_any.zip":
			selected = &universal
		case "checksums.txt":
			selected = &checksums
		default:
			continue
		}
		if *selected != nil {
			return releaseAsset{}, releaseAsset{}, false, errors.New("release has duplicate matching assets")
		}
		*selected = asset
	}
	anyPlatform := platform == nil
	if platform == nil {
		platform = universal
	}
	if platform == nil || checksums == nil {
		return releaseAsset{}, releaseAsset{}, false, errors.New("release requires a platform archive and checksums.txt")
	}
	if platform.Size < 0 || platform.Size > catalogPackageLimit || checksums.Size < 0 || checksums.Size > releaseMetadataLimit {
		return releaseAsset{}, releaseAsset{}, false, errors.New("release assets exceed their size limits")
	}
	for _, asset := range []*releaseAsset{platform, checksums} {
		if asset.ID < 0 {
			return releaseAsset{}, releaseAsset{}, false, errors.New("release asset ID must not be negative")
		}
		if asset.ID == 0 {
			if err := releaseURL(asset.URL); err != nil {
				return releaseAsset{}, releaseAsset{}, false, err
			}
		}
	}
	return *platform, *checksums, anyPlatform, nil
}

func releaseChecksum(data []byte, target string) ([]byte, error) {
	var expected []byte
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(line) < 67 || line[64] != ' ' || line[65] != ' ' && line[65] != '*' {
			return nil, errors.New("checksums.txt requires GNU SHA-256 entries")
		}
		digest, err := hex.DecodeString(line[:64])
		if err != nil || len(digest) != sha256.Size || strings.ContainsFunc(line[66:], unicode.IsControl) {
			return nil, errors.New("checksums.txt contains an invalid SHA-256 entry")
		}
		if line[66:] == target {
			if expected != nil {
				return nil, errors.New("checksums.txt repeats the target archive")
			}
			expected = digest
		}
	}
	if expected == nil {
		return nil, errors.New("checksums.txt does not name the exact release archive")
	}
	return expected, nil
}

func releaseDownload(ctx context.Context, client *http.Client, location string, output io.Writer, limit int64) (int64, error) {
	return releaseDownloadRequest(ctx, client, location, output, limit, "", "", false)
}

func releaseDownloadRequest(ctx context.Context, client *http.Client, location string, output io.Writer, limit int64, token, repository string, asset bool) (int64, error) {
	if err := releaseURL(location); err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	if request.URL.Host == "api.github.com" && !asset {
		request.Header.Set("Accept", "application/vnd.github+json")
	}
	request.Header.Set("User-Agent", "magpie-extension-installer")
	if token != "" {
		if !releaseCredentialToken(token) || !releaseAuthTarget(location, repository) {
			return 0, errors.New("GitHub authorization requires the source repository's exact API endpoint")
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	follow := *client
	redirected := false
	follow.CheckRedirect = func(next *http.Request, previous []*http.Request) error {
		redirected = true
		if client.CheckRedirect != nil {
			return client.CheckRedirect(next, previous)
		}
		return http.ErrUseLastResponse
	}
	response, err := follow.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, &releaseHTTPRequestError{cause: err}
	}
	defer response.Body.Close()
	stop := context.AfterFunc(ctx, func() { response.Body.Close() })
	defer stop()
	if response.StatusCode != http.StatusOK {
		return 0, &releaseHTTPStatusError{status: response.StatusCode, direct: !redirected}
	}
	if response.ContentLength > limit || response.ContentLength < -1 {
		return 0, errors.New("release response exceeds its size limit")
	}
	size, err := releaseCopy(ctx, output, response.Body, limit)
	if err != nil {
		return size, err
	}
	if response.ContentLength >= 0 && size != response.ContentLength {
		return size, errors.New("release response length differs from Content-Length")
	}
	return size, nil
}

func releaseCopy(ctx context.Context, output io.Writer, input io.Reader, limit int64) (int64, error) {
	reader := io.LimitReader(input, limit+1)
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := reader.Read(buffer)
		if err := ctx.Err(); err != nil {
			return total, err
		}
		total += int64(count)
		if total > limit {
			return total, errors.New("release data exceeds its size limit")
		}
		written, writeErr := output.Write(buffer[:count])
		if writeErr != nil {
			return total, writeErr
		}
		if written != count {
			return total, io.ErrShortWrite
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func releaseCheckZIP(archive *zip.Reader) error {
	if len(archive.File) == 0 || len(archive.File) > catalogFilesLimit {
		return errors.New("release ZIP exceeds its entry limit")
	}
	entries, spellings, files := map[string]bool{}, map[string]string{}, map[string]bool{}
	var total uint64
	for _, file := range archive.File {
		if !releaseZIPPath(file.Name) {
			return fmt.Errorf("invalid release ZIP path %q", file.Name)
		}
		name := strings.TrimSuffix(file.Name, "/")
		lower := strings.ToLower(name)
		if entries[lower] {
			return fmt.Errorf("duplicate or case-colliding release ZIP entry %q", file.Name)
		}
		entries[lower] = true
		for current := name; current != "."; current = path.Dir(current) {
			key := strings.ToLower(current)
			if previous, found := spellings[key]; found && previous != current {
				return errors.New("release ZIP paths collide by case")
			}
			spellings[key] = current
		}
		mode := file.Mode()
		if mode.IsDir() {
			if mode.Type() != os.ModeDir || file.UncompressedSize64 != 0 {
				return errors.New("release ZIP directory has a special type or contains data")
			}
			continue
		}
		if !mode.IsRegular() || file.UncompressedSize64 > catalogFileLimit ||
			file.UncompressedSize64 > uint64(catalogPackageLimit)-total {
			return errors.New("release ZIP contains a special or oversized file")
		}
		total += file.UncompressedSize64
		files[lower] = true
	}
	for name := range spellings {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if files[parent] {
				return errors.New("release ZIP file is also used as a directory")
			}
		}
	}
	return nil
}

func releaseExtract(ctx context.Context, archive *zip.Reader, output *os.Root) error {
	var total int64
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if entry.Mode().IsDir() {
			if err := output.MkdirAll(name, 0o700); err != nil {
				return err
			}
			continue
		}
		if parent := path.Dir(name); parent != "." {
			if err := output.MkdirAll(parent, 0o700); err != nil {
				return err
			}
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		file, err := output.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			input.Close()
			return err
		}
		size, err := releaseCopy(ctx, file, input, min(int64(catalogFileLimit), int64(catalogPackageLimit)-total))
		if closeErr := input.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = file.Chmod(0o600 | entry.Mode().Perm()&0o100)
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if uint64(size) != entry.UncompressedSize64 {
			return errors.New("release ZIP entry differs from its declared size")
		}
		total += size
	}
	return ctx.Err()
}
