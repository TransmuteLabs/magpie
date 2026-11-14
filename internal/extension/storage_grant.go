package extension

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type storageGrantKey struct{}
type storageParentKey struct{}

type storageParent struct {
	request sdk.StorageRequest
	scope   sdk.Scope
}

type storageFile struct {
	name string
	info fs.FileInfo
	hash [32]byte
	read bool
	body []byte
}

type storageDirectory struct {
	info fs.FileInfo
	page []sdk.StorageFileEntry
}

type StorageGrant struct {
	mu        sync.Mutex
	lease     *Lease
	request   sdk.StorageReadLease
	family    sdk.StorageFamily
	path      string
	root      *os.Root
	directory *os.File
	info      fs.FileInfo
	check     func(context.Context) error
	files     map[string]*storageFile
	pages     map[string]storageDirectory
	absent    map[string]bool
	read      int64
	entries   int
	active    bool
	parent    *storageParent
	closed    bool
	lifetime  *storageLifetime
}

func NewStorageGrant(ctx context.Context, lease *Lease, sourceID, familyID, rootRef, path string, check func(context.Context) error) (*StorageGrant, error) {
	source, found := lease.Manifest().ResolveStorageSource(sourceID)
	index := slices.IndexFunc(source.Families, func(family sdk.StorageFamily) bool { return family.ID == familyID })
	if !found || index < 0 || check == nil || rootRef == "" || lease.worker.entry.Manifest.Runtime != "native" {
		return nil, errors.New("storage grant requires a selected native source, root and owner")
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	lifetime, err := acquireStorageLifetime(ctx)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			lifetime.close()
		}
	}()
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	info, err := directory.Stat()
	snapshot, randomErr := randomID()
	if err != nil || randomErr != nil || !info.IsDir() {
		root.Close()
		directory.Close()
		return nil, errors.New("storage grant root could not be pinned")
	}
	owned = true
	return &StorageGrant{lease: lease, family: source.Families[index], path: path, root: root, directory: directory, lifetime: lifetime,
		info: info, check: check, files: map[string]*storageFile{}, pages: map[string]storageDirectory{}, absent: map[string]bool{},
		request: sdk.StorageReadLease{SourceID: sourceID, RootRef: rootRef, SnapshotRef: snapshot,
			FamilyID: familyID, Limits: sdk.DefaultStorageLimits()}}, nil
}

func (grant *StorageGrant) RequestLease() sdk.StorageReadLease { return grant.request }

func (grant *StorageGrant) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, storageGrantKey{}, grant)
}

func (grant *StorageGrant) Close() error {
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if grant.closed {
		return nil
	}
	grant.closed = true
	var err error
	if grant.lifetime == nil || !grant.lifetime.native || !grant.lifetime.started || grant.lifetime.ended {
		err = grant.lifetime.close()
	}
	return errors.Join(err, grant.root.Close(), grant.directory.Close())
}

func (grant *StorageGrant) Recheck(ctx context.Context) error {
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if err := grant.recheck(ctx); err != nil {
		return err
	}
	for _, selected := range grant.files {
		if !selected.read {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := grant.open(selected.name)
		if err != nil {
			return ErrActionChanged
		}
		hash := sha256.New()
		count, err := io.Copy(hash, io.LimitReader(file, selected.info.Size()+1))
		grant.closeMember(file)
		if err != nil || count != selected.info.Size() || !slices.Equal(hash.Sum(nil), selected.hash[:]) {
			return ErrActionChanged
		}
	}
	for name, selected := range grant.pages {
		directory, err := grant.open(name)
		if err != nil {
			return ErrActionChanged
		}
		entries, err := directory.ReadDir(len(selected.page) + 1)
		grant.closeMember(directory)
		if err != nil && !errors.Is(err, io.EOF) || len(entries) != len(selected.page) {
			return ErrActionChanged
		}
		slices.SortFunc(entries, func(first, second fs.DirEntry) int { return strings.Compare(first.Name(), second.Name()) })
		for index, entry := range entries {
			if name+"/"+entry.Name() != selected.page[index].RelativeName || entry.Type()&os.ModeSymlink != 0 {
				return ErrActionChanged
			}
		}
	}
	return grant.recheck(ctx)
}

func (grant *StorageGrant) recheck(ctx context.Context) error {
	if grant.closed || grant.lease.closed.Load() || ctx.Err() != nil || grant.lease.ctx.Err() != nil {
		return context.Canceled
	}
	if err := grant.check(ctx); err != nil {
		return err
	}
	info, err := os.Stat(grant.path)
	if err != nil || !storageSameFile(grant.info, info) || info.Mode() != grant.info.Mode() {
		return ErrActionChanged
	}
	manager := grant.lease.manager
	config, err := manager.ModuleConfig(ctx, grant.lease.worker.entry.Manifest.ID)
	if err != nil || config.Version != grant.lease.ConfigVersion() {
		return ErrActionChanged
	}
	entries, err := manager.List()
	if err != nil || !slices.ContainsFunc(entries, func(entry Installed) bool {
		return entry.Manifest.ID == grant.lease.worker.entry.Manifest.ID &&
			!entry.Disabled && entry.Revision == grant.lease.Revision()
	}) {
		return ErrActionChanged
	}
	if !grant.lease.client.alive() {
		return ErrActionChanged
	}
	for name := range grant.absent {
		_, err := grant.stat(name)
		if err == nil {
			return ErrActionChanged
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return ErrActionChanged
		}
	}
	for _, file := range grant.files {
		info, err := grant.stat(file.name)
		if err != nil || !storageSameFile(file.info, info) || info.Mode() != file.info.Mode() {
			return ErrActionChanged
		}
		if file.read {
			if info.Size() != file.info.Size() || !info.ModTime().Equal(file.info.ModTime()) {
				return ErrActionChanged
			}
		}
	}
	for name, selected := range grant.pages {
		info, err := grant.stat(name)
		if err != nil || !storageSameFile(info, selected.info) || info.Mode() != selected.info.Mode() ||
			info.Size() != selected.info.Size() || !info.ModTime().Equal(selected.info.ModTime()) {
			return ErrActionChanged
		}
	}
	return nil
}

func validateStorageScope(manifest sdk.Manifest, method string, scope sdk.Scope, params any) error {
	if !sdk.IsStorageMethod(method) {
		return nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	_, err = sdk.DecodeStorageRequest(manifest, method, scope, nil, raw)
	return err
}

func beginStorageCall(ctx context.Context, lease *Lease, method string, scope sdk.Scope, params any) (context.Context, func(), error) {
	if !sdk.IsStorageMethod(method) {
		return ctx, func() {}, nil
	}
	grant, ok := ctx.Value(storageGrantKey{}).(*StorageGrant)
	if !ok || grant.lease != lease {
		return ctx, nil, errors.New("storage call requires its host-selected read lease")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return ctx, nil, err
	}
	request, err := sdk.DecodeStorageRequest(lease.worker.entry.Manifest, method, scope, nil, raw)
	if err != nil || request.Lease != grant.request {
		return ctx, nil, errors.New("storage call changed its read lease")
	}
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if grant.active {
		return ctx, nil, errors.New("storage lease already has an active parent")
	}
	if err := grant.recheck(ctx); err != nil {
		return ctx, nil, err
	}
	grant.active = true
	parent := &storageParent{request: request, scope: scope}
	grant.parent = parent
	return context.WithValue(ctx, storageParentKey{}, parent), func() {
		grant.mu.Lock()
		grant.active = false
		grant.parent = nil
		grant.mu.Unlock()
	}, nil
}

func finishStorageCall(ctx context.Context, result json.RawMessage) error {
	grant, ok := ctx.Value(storageGrantKey{}).(*StorageGrant)
	if !ok {
		return errors.New("storage call lost its grant")
	}
	parent, ok := ctx.Value(storageParentKey{}).(*storageParent)
	if !ok {
		return errors.New("storage call lost its selected parent")
	}
	if _, err := sdk.DecodeStorageResult(parent.request, result); err != nil {
		return err
	}
	return grant.Recheck(ctx)
}

func (manager *Manager) storageHostCall(ctx context.Context, scope sdk.Scope, method string, raw json.RawMessage, authority hostAuthority) (any, error) {
	grant, ok := ctx.Value(storageGrantKey{}).(*StorageGrant)
	parent, owned := ctx.Value(storageParentKey{}).(*storageParent)
	if !ok || !owned || scope != parent.scope || grant.lease.manager != manager ||
		authority.manifest.ID != grant.lease.worker.entry.Manifest.ID || !sdk.IsStorageMethod(authority.method) ||
		scope.Provider != "" || scope.Account != "" || scope.Generation != 0 || scope.Attempt != 0 {
		return nil, errors.New("storage callbacks cannot borrow another parent or account authority")
	}
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if !grant.active || grant.parent != parent {
		return nil, ErrActionChanged
	}
	if err := grant.recheck(ctx); err != nil {
		return nil, err
	}
	switch method {
	case sdk.MethodStorageNative, sdk.MethodStorageCheck:
		var lease sdk.StorageReadLease
		if !decodeHostParams(raw, &lease) || lease != grant.request {
			return nil, errors.New("foreign storage read lease")
		}
		if method == sdk.MethodStorageCheck {
			return struct{}{}, nil
		}
		for _, name := range append([]string{grant.family.Primary}, grant.family.Sidecars...) {
			if name == "" {
				continue
			}
			info, err := grant.stat(name)
			if errors.Is(err, fs.ErrNotExist) {
				grant.absent[name] = true
				continue
			}
			if err != nil {
				return nil, err
			}
			grant.files["native:"+name] = &storageFile{name: name, info: info}
		}
		if grant.lifetime != nil {
			grant.lifetime.native = true
		}
		family := grant.family
		family.Trees = nil
		return sdk.StorageNativeAccess{Lease: grant.request, Descriptor: int(grant.directory.Fd()), Family: family}, nil
	case sdk.MethodStorageFiles:
		var request sdk.StorageFilesRequest
		if !decodeHostParams(raw, &request) || request.Lease != grant.request || request.Limit < 1 || request.Limit > 256 ||
			!grant.family.Allows(request.Directory) {
			return nil, errors.New("storage directory is outside its finite family")
		}
		return grant.list(request)
	case sdk.MethodStorageBytes:
		var request sdk.StorageBytesRequest
		if !decodeHostParams(raw, &request) || request.Lease != grant.request || request.Length < 1 ||
			request.Length > 256<<10 || request.Offset < 0 {
			return nil, errors.New("storage byte budget exceeded")
		}
		return grant.bytes(request)
	default:
		return nil, errors.New("unnegotiated storage callback")
	}
}

func (grant *StorageGrant) list(request sdk.StorageFilesRequest) (sdk.StorageDirectoryPage, error) {
	selected, exists := grant.pages[request.Directory]
	page := selected.page
	if !exists {
		directory, err := grant.open(request.Directory)
		if errors.Is(err, fs.ErrNotExist) {
			grant.absent[request.Directory] = true
			return sdk.StorageDirectoryPage{SnapshotRef: grant.request.SnapshotRef, Entries: []sdk.StorageFileEntry{}, Missing: true}, nil
		}
		if err != nil {
			return sdk.StorageDirectoryPage{}, err
		}
		defer grant.closeMember(directory)
		directoryInfo, err := directory.Stat()
		if err != nil {
			return sdk.StorageDirectoryPage{}, err
		}
		entries, err := directory.ReadDir(grant.request.Limits.MaxEntries + 1)
		if err != nil && !errors.Is(err, io.EOF) {
			return sdk.StorageDirectoryPage{}, err
		}
		if len(entries) > grant.request.Limits.MaxEntries-grant.entries {
			return sdk.StorageDirectoryPage{}, errors.New("storage enumeration budget exceeded")
		}
		grant.entries += len(entries)
		slices.SortFunc(entries, func(first, second fs.DirEntry) int { return strings.Compare(first.Name(), second.Name()) })
		page = []sdk.StorageFileEntry{}
		for _, entry := range entries {
			name := request.Directory + "/" + entry.Name()
			if !grant.family.Allows(name) || entry.Type()&os.ModeSymlink != 0 {
				return sdk.StorageDirectoryPage{}, errors.New("invalid storage directory member")
			}
			info, err := grant.stat(name)
			if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
				return sdk.StorageDirectoryPage{}, errors.New("unsupported storage file")
			}
			ref, err := randomID()
			if err != nil {
				return sdk.StorageDirectoryPage{}, err
			}
			grant.files[ref] = &storageFile{name: name, info: info}
			kind := "file"
			if info.IsDir() {
				kind = "directory"
			}
			page = append(page, sdk.StorageFileEntry{FileRef: ref, RelativeName: name, Kind: kind, Size: info.Size()})
		}
		grant.pages[request.Directory] = storageDirectory{info: directoryInfo, page: page}
	}
	offset := 0
	if request.Cursor != "" {
		prefix := grant.request.SnapshotRef + ":" + request.Directory + ":"
		if !strings.HasPrefix(request.Cursor, prefix) {
			return sdk.StorageDirectoryPage{}, errors.New("foreign storage directory cursor")
		}
		var err error
		offset, err = strconv.Atoi(strings.TrimPrefix(request.Cursor, prefix))
		if err != nil || offset < 0 || offset > len(page) {
			return sdk.StorageDirectoryPage{}, errors.New("invalid storage directory cursor")
		}
	}
	end := min(offset+request.Limit, len(page))
	result := sdk.StorageDirectoryPage{SnapshotRef: grant.request.SnapshotRef, Entries: slices.Clone(page[offset:end])}
	if end < len(page) {
		result.NextCursor = fmt.Sprintf("%s:%s:%d", grant.request.SnapshotRef, request.Directory, end)
	}
	return result, nil
}

func (grant *StorageGrant) bytes(request sdk.StorageBytesRequest) (sdk.StorageReadChunk, error) {
	selected := grant.files[request.FileRef]
	if selected == nil || !selected.info.Mode().IsRegular() || selected.info.Size() > grant.request.Limits.MaxReadBytes {
		return sdk.StorageReadChunk{}, errors.New("foreign or oversized storage file")
	}
	if !selected.read {
		if selected.info.Size() < 0 || selected.info.Size() > grant.request.Limits.MaxReadBytes-grant.read {
			return sdk.StorageReadChunk{}, errors.New("storage byte budget exceeded")
		}
		file, err := grant.open(selected.name)
		if err != nil {
			return sdk.StorageReadChunk{}, err
		}
		info, err := file.Stat()
		if err != nil || !storageSameFile(info, selected.info) || info.Mode() != selected.info.Mode() ||
			info.Size() != selected.info.Size() || !info.ModTime().Equal(selected.info.ModTime()) {
			grant.closeMember(file)
			return sdk.StorageReadChunk{}, ErrActionChanged
		}
		body, err := io.ReadAll(io.LimitReader(file, selected.info.Size()+1))
		grant.closeMember(file)
		if err != nil || int64(len(body)) != selected.info.Size() {
			return sdk.StorageReadChunk{}, ErrActionChanged
		}
		selected.read, selected.hash, selected.body = true, sha256.Sum256(body), body
		grant.read += int64(len(body))
	}
	body := selected.body
	if request.Offset > int64(len(body)) {
		return sdk.StorageReadChunk{}, errors.New("storage byte offset exceeds file")
	}
	end := min(request.Offset+int64(request.Length), int64(len(body)))
	chunk := slices.Clone(body[request.Offset:end])
	return sdk.StorageReadChunk{SnapshotRef: grant.request.SnapshotRef, FileRef: request.FileRef,
		Offset: request.Offset, Bytes: chunk, EOF: end == int64(len(body))}, nil
}
