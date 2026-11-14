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

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sessionSourceAdmissionKey struct{}

func withSessionSourceAdmission(ctx context.Context, admit func() error) context.Context {
	return context.WithValue(ctx, sessionSourceAdmissionKey{}, admit)
}

func checkSessionSourceAdmission(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if admit, ok := ctx.Value(sessionSourceAdmissionKey{}).(func() error); ok {
		return admit()
	}
	return nil
}

func decodeSourceJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid trailing source JSON")
	}
	return nil
}

func listSourceCandidates(ctx context.Context, grant sourceGrant) ([]string, error) {
	root, err := os.OpenRoot(grant.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var candidates []string
	seen := 0
	read := func(name string, visit func(fs.DirEntry) error) error {
		directory, err := root.Open(name)
		if err != nil {
			return err
		}
		defer directory.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			items, err := directory.ReadDir(256)
			for _, item := range items {
				seen++
				if seen > 10000 {
					return errors.New("source enumeration budget exceeded")
				}
				if err := visit(item); err != nil {
					return err
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	add := func(name string) {
		if _, err := grant.descriptor.Layout.SessionID(name); err == nil {
			candidates = append(candidates, name)
		}
	}
	err = read(".", func(item fs.DirEntry) error {
		if grant.descriptor.Layout.Kind == "flat" {
			if !item.IsDir() {
				add(item.Name())
			}
			return nil
		}
		if !item.IsDir() {
			return nil
		}
		if grant.descriptor.Layout.Kind == "directory" {
			if _, err := root.Lstat(item.Name() + "/log.jsonl"); errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			add(item.Name() + "/log.jsonl")
			return nil
		}
		return read(item.Name(), func(child fs.DirEntry) error {
			if !child.IsDir() {
				add(item.Name() + "/" + child.Name())
			}
			return nil
		})
	})
	return candidates, err
}

func captureSourceRequest(ctx context.Context, grant sourceGrant, relative, mode string) (sdk.SessionSourceReadRequest, *sessionInputSnapshot, error) {
	if err := checkSessionSourceAdmission(ctx); err != nil {
		return sdk.SessionSourceReadRequest{}, nil, err
	}
	root, err := os.OpenRoot(grant.root)
	if err != nil {
		return sdk.SessionSourceReadRequest{}, nil, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(info, grant.info) || !sessionStoreModeEqual(info.Mode(), grant.info.Mode()) {
		return sdk.SessionSourceReadRequest{}, nil, extension.ErrActionChanged
	}
	file, body, err := readSessionInput(ctx, root, relative, extension.MaxActionBytes)
	if err != nil {
		return sdk.SessionSourceReadRequest{}, nil, err
	}
	id, err := grant.descriptor.Layout.SessionID(relative)
	if err != nil {
		return sdk.SessionSourceReadRequest{}, nil, err
	}
	snapshot := &sessionInputSnapshot{root: grant.root, info: info, files: []sessionInputFile{file}}
	complete := bytes.LastIndexByte(body, '\n') + 1
	digest, _ := json.Marshal([]string{grant.reference, relative, relative, hex.EncodeToString(file.hash[:])})
	hash := sha256.Sum256(digest)
	request := sdk.SessionSourceReadRequest{SourceID: grant.descriptor.ID,
		Locator:  sdk.SessionSourceLocator{StoreRef: grant.reference, Path: relative, SessionID: id},
		Snapshot: hex.EncodeToString(hash[:]), Mode: mode,
		Documents: []sdk.SessionDocument{{Path: relative, Format: grant.descriptor.Codec, Body: bytes.Clone(body[:complete])}},
		Limits:    sdk.SessionSourceLimits{MaxParts: 10000, MaxTextBytes: transcriptMax}}
	return request, snapshot, nil
}

func sameSourceSelection(first, second sdk.SessionSourceReadRequest) bool {
	return first.SourceID == second.SourceID && first.Locator == second.Locator && first.Snapshot == second.Snapshot
}

func sourcePhysicalPath(grant sourceGrant, relative string) string {
	return filepath.Join(grant.root, filepath.FromSlash(relative))
}

func collectSourceDocuments(ctx context.Context, manager *extension.Manager, lease *extension.Lease, grant sourceGrant, request *sdk.SessionSourceReadRequest, snapshot *sessionInputSnapshot) (*sessionInputSnapshot, error) {
	journal := snapshot.files[0]
	envelope := sdk.SessionRequest{ID: grant.descriptor.ID, From: grant.descriptor.Codec, To: grant.descriptor.Codec,
		Files: request.Documents}
	source := &sessionSourceSnapshot{path: sourcePhysicalPath(grant, request.Locator.Path), info: journal.info, hash: journal.hash}
	runtime := &sessionLeasedRuntime{ActionRuntime: manager, lease: lease}
	if err := collectSessionInputsIn(ctx, runtime, grant.plugin,
		sdk.Scope{Session: request.Locator.SessionID, Operation: sdk.MethodSessionSourceRead},
		grant.descriptor.Codec, grant.root, request.Locator.Path, &envelope, source); err != nil {
		return nil, err
	}
	request.Documents = envelope.Files
	if source.inputs != nil {
		snapshot = source.inputs
		files := []sessionInputFile{journal}
		for _, file := range snapshot.files {
			if file.path != journal.path {
				files = append(files, file)
			}
		}
		snapshot.files = files
	}
	if err := collectSourceDescendants(ctx, grant, request, snapshot); err != nil {
		return nil, err
	}
	identity := []string{grant.reference, request.Locator.Path}
	for _, file := range snapshot.files {
		identity = append(identity, file.path, hex.EncodeToString(file.hash[:]))
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	request.Snapshot = hex.EncodeToString(digest[:])
	if err := sdk.ValidateSessionSourceRequest(lease.Manifest(), *request); err != nil {
		return nil, err
	}
	return snapshot, nil
}
