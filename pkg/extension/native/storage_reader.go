package native

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type StorageRoot struct {
	mu     sync.Mutex
	keep   *os.File
	family sdk.StorageFamily
	ctx    context.Context
	lease  sdk.StorageReadLease
	read   int64
	closed bool
}

func OpenStorage(ctx context.Context, stream *sdk.Stream, lease sdk.StorageReadLease) (*StorageRoot, error) {
	access, err := stream.NativeStorage(ctx, lease)
	if err != nil {
		return nil, err
	}
	return openStorageAccess(ctx, access)
}

func (root *StorageRoot) Open(name string) (*os.File, error) {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed || root.ctx.Err() != nil || !root.family.Allows(name) {
		return nil, fs.ErrPermission
	}
	file, err := root.openFile(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
		file.Close()
		return nil, fs.ErrPermission
	}
	return file, nil
}

func (root *StorageRoot) Consume(bytes int64) error {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed || root.ctx.Err() != nil {
		return context.Canceled
	}
	if bytes < 0 || bytes > root.lease.Limits.MaxReadBytes-root.read {
		return errors.New("native storage read budget exhausted")
	}
	root.read += bytes
	return nil
}

func (root *StorageRoot) Family() sdk.StorageFamily {
	return sdk.StorageFamily{ID: root.family.ID, Primary: root.family.Primary,
		Sidecars: append([]string(nil), root.family.Sidecars...), Trees: append([]string(nil), root.family.Trees...)}
}

func (root *StorageRoot) Close() error {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed {
		return nil
	}
	root.closed = true
	return root.keep.Close()
}
