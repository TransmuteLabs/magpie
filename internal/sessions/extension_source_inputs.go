package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const sessionInputsAction = "session.inputs.v1"

type sessionInputReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type sessionInputPlan struct {
	Version int                     `json:"version"`
	Files   []sessionInputReference `json:"files"`
}

type sessionInputFile struct {
	path    string
	info    os.FileInfo
	hash    [sha256.Size]byte
	parents []os.FileInfo
}

type sessionInputSnapshot struct {
	root        string
	info        os.FileInfo
	files       []sessionInputFile
	descendants *sessionSourceTreeSnapshot
}

type sessionLeasedRuntime struct {
	extension.ActionRuntime
	lease *extension.Lease
}

func sessionSourceRuntime(ctx context.Context, host extension.ActionRuntime, plugin string) (extension.ActionRuntime, func(), error) {
	manifest, err := host.Manifest(plugin)
	if err != nil {
		return nil, nil, err
	}
	action, declared := manifest.ResolveAction(sessionInputsAction)
	if !declared {
		return host, func() {}, nil
	}
	if action.Effect != "read" || !manifest.Contract.Has(sdk.ModuleRPC) {
		return nil, nil, errors.New("session input planning requires a declared read-only module action")
	}
	if manager, ok := host.(*extension.Manager); ok {
		lease, err := manager.Acquire(ctx, plugin)
		if err != nil {
			return nil, nil, err
		}
		return &sessionLeasedRuntime{ActionRuntime: host, lease: lease}, lease.Close, nil
	}
	return host, func() {}, nil
}

func (host *sessionLeasedRuntime) Invoke(ctx context.Context, plugin, method string, scope sdk.Scope, params, out any, consume func(extension.Event) error) error {
	manifest, err := host.Manifest(plugin)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(manifest.Public(), host.lease.Manifest()) {
		return extension.ErrActionChanged
	}
	body, err := host.lease.Call(ctx, method, scope, params, consume)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func collectSessionSourceInputs(ctx context.Context, host extension.ActionRuntime, plugin string, scope sdk.Scope, source Session, request *sdk.SessionRequest, snapshot *sessionSourceSnapshot) error {
	manifest, err := host.Manifest(plugin)
	if err != nil {
		return err
	}
	action, declared := manifest.ResolveAction(sessionInputsAction)
	if !declared {
		return nil
	}
	if action.Effect != "read" || !manifest.Contract.Has(sdk.ModuleRPC) {
		return errors.New("session input planning requires a declared read-only module action")
	}
	root, relative, err := sessionSourceRoot(source)
	if err != nil {
		return err
	}
	return collectSessionInputsIn(ctx, host, plugin, scope, source.Agent, root, relative, request, snapshot)
}

func collectSessionInputsIn(ctx context.Context, host extension.ActionRuntime, plugin string, scope sdk.Scope, format, root, relative string, request *sdk.SessionRequest, snapshot *sessionSourceSnapshot) error {
	if err := checkSessionSourceAdmission(ctx); err != nil {
		return err
	}
	manifest, err := host.Manifest(plugin)
	if err != nil {
		return err
	}
	action, declared := manifest.ResolveAction(sessionInputsAction)
	if !declared {
		return nil
	}
	if action.Effect != "read" || !manifest.Contract.Has(sdk.ModuleRPC) {
		return errors.New("session input planning requires a declared read-only module action")
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	rootInfo, infoErr := opened.Stat(".")
	journal, _, readErr := readSessionInput(ctx, opened, relative, extension.MaxActionBytes)
	opened.Close()
	if infoErr != nil {
		return infoErr
	}
	if readErr != nil {
		return readErr
	}
	if !os.SameFile(snapshot.info, journal.info) || snapshot.hash != journal.hash {
		return extension.ErrActionChanged
	}
	request.Files[0].Path = relative
	input, err := json.Marshal(request)
	if err != nil || len(input) > sdk.MaxFrameBytes-4096 {
		return errors.New("session input planning exceeds its frame budget")
	}
	var raw json.RawMessage
	if err := host.Invoke(ctx, plugin, sdk.MethodModuleCall, scope,
		sdk.ModuleCallRequest{Action: sessionInputsAction, Input: input}, &raw, nil); err != nil {
		return err
	}
	if err := checkSessionSourceAdmission(ctx); err != nil {
		return err
	}
	if len(raw) > 64<<10 {
		return errors.New("session input plan exceeds its byte budget")
	}
	var plan sessionInputPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&plan) != nil || decoder.Decode(new(any)) != io.EOF || plan.Version != 1 {
		return errors.New("invalid session input plan")
	}
	inputs, documents, err := readSessionInputs(ctx, root, format, relative, plan.Files, max(len(request.Files[0].Body), int(snapshot.info.Size())))
	if err != nil {
		return err
	}
	if !os.SameFile(rootInfo, inputs.info) {
		return extension.ErrActionChanged
	}
	request.Files = append(request.Files, documents...)
	inputs.files = append(inputs.files, journal)
	snapshot.inputs = inputs
	return nil
}

func sessionSourceRoot(source Session) (string, string, error) {
	root := filepath.Dir(source.Path)
	if source.Agent == "omp" {
		root = ""
		for _, sessions := range ompSessionRoots() {
			relative, err := filepath.Rel(sessions, source.Path)
			if err == nil && filepath.IsLocal(relative) && relative != "." {
				root = filepath.Dir(sessions)
				break
			}
		}
		if root == "" {
			return "", "", errors.New("session source is outside its selected native store")
		}
	}
	relative, err := filepath.Rel(root, source.Path)
	if err != nil || !sessionInputPath(filepath.ToSlash(relative)) {
		return "", "", errors.New("invalid native session source path")
	}
	root, err = filepath.EvalSymlinks(root)
	return root, filepath.ToSlash(relative), err
}

func sessionInputPath(name string) bool {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".magpie-extension") {
			return false
		}
	}
	return true
}

func sessionInputKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}

func readSessionInputs(ctx context.Context, directory, format, source string, references []sessionInputReference, total int) (*sessionInputSnapshot, []sdk.SessionDocument, error) {
	if len(references) >= extension.MaxActionFiles || total > extension.MaxActionBytes {
		return nil, nil, errors.New("session source input budget exceeded")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return nil, nil, err
	}
	snapshot := &sessionInputSnapshot{root: directory, info: info}
	seen := map[string]bool{sessionInputKey(source): true}
	var documents []sdk.SessionDocument
	for _, reference := range references {
		digest, err := hex.DecodeString(reference.SHA256)
		if !sessionInputPath(reference.Path) || seen[sessionInputKey(reference.Path)] || err != nil || len(digest) != sha256.Size ||
			reference.SHA256 != strings.ToLower(reference.SHA256) {
			return nil, nil, errors.New("invalid or duplicate session input reference")
		}
		seen[sessionInputKey(reference.Path)] = true
		file, body, err := readSessionInput(ctx, root, reference.Path, extension.MaxActionBytes-total)
		if err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(file.hash[:], digest) {
			return nil, nil, errors.New("session input digest differs from its declared reference")
		}
		total += len(body)
		snapshot.files = append(snapshot.files, file)
		documents = append(documents, sdk.SessionDocument{Path: reference.Path, Format: format, Body: body})
	}
	return snapshot, documents, nil
}

func readSessionInput(ctx context.Context, root *os.Root, name string, limit int) (sessionInputFile, []byte, error) {
	if err := ctx.Err(); err != nil {
		return sessionInputFile{}, nil, err
	}
	parts := strings.Split(name, "/")
	var parents []os.FileInfo
	for index := range parts {
		info, err := root.Lstat(filepath.Join(parts[:index+1]...))
		if err != nil {
			return sessionInputFile{}, nil, err
		}
		if index < len(parts)-1 && !info.IsDir() || index == len(parts)-1 && !info.Mode().IsRegular() {
			return sessionInputFile{}, nil, errors.New("session inputs require regular files and non-symlink parents")
		}
		if index < len(parts)-1 {
			parents = append(parents, info)
		}
	}
	before, err := root.Lstat(name)
	if err != nil || before.Size() > int64(limit) {
		return sessionInputFile{}, nil, errors.New("session input exceeds its byte budget")
	}
	file, err := root.Open(name)
	if err != nil {
		return sessionInputFile{}, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return sessionInputFile{}, nil, extension.ErrActionChanged
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return sessionInputFile{}, nil, err
	}
	after, err := file.Stat()
	current, currentErr := root.Lstat(name)
	if err != nil || currentErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) ||
		len(body) > limit || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return sessionInputFile{}, nil, extension.ErrActionChanged
	}
	for index, expected := range parents {
		current, err := root.Lstat(filepath.Join(parts[:index+1]...))
		if err != nil || !current.IsDir() || !os.SameFile(expected, current) {
			return sessionInputFile{}, nil, extension.ErrActionChanged
		}
	}
	if err := ctx.Err(); err != nil {
		return sessionInputFile{}, nil, err
	}
	return sessionInputFile{path: name, info: opened, hash: sha256.Sum256(body), parents: parents}, body, nil
}

func (snapshot *sessionInputSnapshot) check(ctx context.Context) error {
	root, err := os.OpenRoot(snapshot.root)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(snapshot.info, info) || !sessionStoreModeEqual(snapshot.info.Mode(), info.Mode()) {
		return extension.ErrActionChanged
	}
	for _, expected := range snapshot.files {
		current, _, err := readSessionInput(ctx, root, expected.path, int(expected.info.Size()))
		if err != nil {
			return err
		}
		if !os.SameFile(expected.info, current.info) || expected.hash != current.hash ||
			!sessionStoreModeEqual(expected.info.Mode(), current.info.Mode()) || len(expected.parents) != len(current.parents) {
			return extension.ErrActionChanged
		}
		for index := range expected.parents {
			if !os.SameFile(expected.parents[index], current.parents[index]) ||
				!sessionStoreModeEqual(expected.parents[index].Mode(), current.parents[index].Mode()) {
				return extension.ErrActionChanged
			}
		}
	}
	if snapshot.descendants != nil {
		if err := snapshot.descendants.check(ctx, root); err != nil {
			return err
		}
	}
	return nil
}
