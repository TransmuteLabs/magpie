package extension

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"runtime"
	"sync/atomic"

	"github.com/yetone/magpie/internal/filelock"
)

const (
	storeSnapshotLimit             = 64 << 10
	storeLargeAccountSnapshotLimit = 128 << 10
)

var errAccountListBudget = errors.New("extension account list exceeds its budget")
var errStoreReadBusy = errors.New("extension store reader would wait for a writer")
var spareLargeAccountReadBuffer = make(chan []byte, 1)

type storeReadSnapshot[T any] struct {
	namespace string
	digest    [sha256.Size]byte
	file      os.FileInfo
	value     T
}

type storeSyncConfirmation struct {
	namespace               string
	digest                  [sha256.Size]byte
	file, directory         *os.File
	fileInfo, directoryInfo os.FileInfo
}

func (confirmation *storeSyncConfirmation) close() {
	if confirmation != nil {
		confirmation.file.Close()
		confirmation.directory.Close()
	}
}

func (s *Store) closeAccountSync() {
	s.accountSyncMu.Lock()
	defer s.accountSyncMu.Unlock()
	s.accountSyncEnabled = false
	for _, pin := range s.accountSync {
		pin.close()
	}
	clear(s.accountSync[:])
}

func sameStorePublication(left, right os.FileInfo) bool {
	return os.SameFile(left, right) && left.Mode() == right.Mode() &&
		left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func storeLoadSnapshot[T any](root *os.Root, name, namespace string, snapshot *atomic.Pointer[storeReadSnapshot[T]], decode func([]byte) (T, error)) (T, bool, error) {
	view, err := storeLoadSnapshotView(root, name, namespace, snapshot, nil, decode)
	if view != nil {
		return view.value, true, nil
	}
	var zero T
	return zero, false, err
}

func storeLoadSnapshotView[T any](root *os.Root, name, namespace string, snapshot, large *atomic.Pointer[storeReadSnapshot[T]], decode func([]byte) (T, error)) (*storeReadSnapshot[T], error) {
	var scratch []byte
	data, file, err := storeReadFileWithBuffer(root, name, storeFileLimit, func(fileSize int64) []byte {
		if large == nil || fileSize <= storeSnapshotLimit || fileSize > storeLargeAccountSnapshotLimit {
			return nil
		}
		size := int(fileSize) + 1
		select {
		case scratch = <-spareLargeAccountReadBuffer:
		default:
		}
		if cap(scratch) < size {
			scratch = make([]byte, size)
		}
		scratch = scratch[:size]
		return scratch
	})
	if scratch != nil {
		defer func() {
			clear(scratch)
			select {
			case spareLargeAccountReadBuffer <- scratch:
			default:
			}
		}()
	}
	limit := storeSnapshotLimit
	if len(data) > limit && large != nil {
		snapshot, large = large, snapshot
		limit = storeLargeAccountSnapshotLimit
	}
	if large != nil {
		if previous := large.Load(); previous != nil && previous.namespace == namespace {
			large.CompareAndSwap(previous, nil)
		}
	}
	cacheable := snapshot != nil && err == nil && file != nil && len(data) <= limit
	var digest [sha256.Size]byte
	if snapshot != nil {
		cached := snapshot.Load()
		if cacheable {
			digest = sha256.Sum256(data)
			if cached != nil && cached.namespace == namespace && cached.digest == digest && sameStorePublication(cached.file, file) {
				return cached, nil
			}
		}
		if cached != nil && cached.namespace == namespace {
			snapshot.CompareAndSwap(cached, nil)
		}
	}
	if err != nil || file == nil {
		return nil, err
	}
	value, err := decode(data)
	if err != nil {
		return nil, err
	}
	view := &storeReadSnapshot[T]{namespace: namespace, digest: digest, file: file, value: value}
	if cacheable {
		snapshot.Store(view)
	}
	return view, nil
}

func (s *Store) withReadAccountsSnapshot(ctx context.Context, plugin string, read func(*os.Root, *storeReadSnapshot[storeAccounts]) error) error {
	return s.withReadStoreContext(ctx, plugin, func(root *os.Root) error {
		view, err := storeLoadAccountSnapshot(root, plugin, s)
		if err != nil {
			return err
		}
		return read(root, view)
	})
}

func (s *Store) confirmAccountSnapshot(ctx context.Context, root *os.Root, snapshot *storeReadSnapshot[storeAccounts], confirm func(*os.Root) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return confirmAccountPublication(ctx, root, snapshot, confirm)
	}
	if snapshot.file.Size() > storeSnapshotLimit {
		return s.confirmLargeAccountSnapshot(ctx, root, snapshot, confirm)
	}
	directory, err := root.Stat(".")
	if err != nil || !directory.IsDir() {
		return storeUnreadable()
	}
	slot := s.accountSnapshotIndex(snapshot.namespace)
	s.accountSyncMu.Lock()
	previous := s.accountSync[slot]
	known := previous != nil && previous.namespace == snapshot.namespace && previous.digest == snapshot.digest &&
		sameStorePublication(previous.fileInfo, snapshot.file) && sameStorePublication(previous.directoryInfo, directory)
	s.accountSyncMu.Unlock()
	if known {
		return nil
	}

	pin, err := openAccountConfirmation(root, directory, snapshot)
	if err != nil {
		return err
	}
	if err := confirmAccountPublication(ctx, root, snapshot, confirm); err != nil {
		pin.close()
		return err
	}
	s.accountSyncMu.Lock()
	if !s.accountSyncEnabled {
		s.accountSyncMu.Unlock()
		pin.close()
		return nil
	}
	s.accountSync[slot].close()
	s.accountSync[slot] = pin
	s.accountSyncMu.Unlock()
	return nil
}

func (s *Store) confirmLargeAccountSnapshot(ctx context.Context, root *os.Root, snapshot *storeReadSnapshot[storeAccounts], confirm func(*os.Root) error) error {
	slot := s.accountSnapshotIndex(snapshot.namespace)
	s.accountSyncMu.Lock()
	previous := s.accountSync[slot]
	s.accountSyncMu.Unlock()
	if err := confirmAccountPublication(ctx, root, snapshot, confirm); err != nil {
		return err
	}
	if previous == nil || previous.namespace != snapshot.namespace {
		return nil
	}
	directory, err := root.Stat(".")
	if err != nil || !os.SameFile(previous.directoryInfo, directory) {
		return nil
	}
	s.accountSyncMu.Lock()
	defer s.accountSyncMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.accountSync[slot] == previous {
		s.accountSync[slot] = nil
		previous.close()
	}
	return nil
}

func openAccountConfirmation(root *os.Root, directory os.FileInfo, snapshot *storeReadSnapshot[storeAccounts]) (*storeSyncConfirmation, error) {
	file, fileInfo, err := storeOpenReadFile(root, "accounts.json", storeFileLimit)
	if err != nil {
		return nil, storeUnreadable()
	}
	if !sameStorePublication(snapshot.file, fileInfo) {
		file.Close()
		return nil, storeUnreadable()
	}
	dir, err := root.Open(".")
	if err != nil {
		file.Close()
		return nil, storeUnreadable()
	}
	dirInfo, err := dir.Stat()
	if err != nil || !sameStorePublication(directory, dirInfo) {
		file.Close()
		dir.Close()
		return nil, storeUnreadable()
	}
	return &storeSyncConfirmation{
		namespace: snapshot.namespace, digest: snapshot.digest,
		file: file, directory: dir, fileInfo: fileInfo, directoryInfo: dirInfo,
	}, nil
}

func confirmAccountPublication(ctx context.Context, root *os.Root, snapshot *storeReadSnapshot[storeAccounts], confirm func(*os.Root) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := confirm(root); err != nil {
		return err
	}
	file, err := root.Lstat("accounts.json")
	if err != nil || !sameStorePublication(snapshot.file, file) {
		return storeUnreadable()
	}
	return ctx.Err()
}

func (s *Store) withReadStoreContext(ctx context.Context, name string, read func(*os.Root) error) error {
	return s.withReadPrivateContext(ctx, func(private *os.Root) error {
		return readStoreNamespace(ctx, private, name, read)
	})
}

func (s *Store) withReadPrivateContext(ctx context.Context, read func(*os.Root) error) error {
	return s.withReadPrivate(ctx, true, read)
}

func (s *Store) withReadPrivate(ctx context.Context, wait bool, read func(*os.Root) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.root == "" {
		return storeInvalid()
	}
	root, err := storeRoot(s.root, false)
	if errors.Is(err, os.ErrNotExist) {
		return read(nil)
	}
	if err != nil {
		return err
	}
	defer root.Close()
	private, err := storeOpenDirectory(root, "private", false)
	if errors.Is(err, os.ErrNotExist) {
		return read(nil)
	}
	if err != nil {
		return err
	}
	defer private.Close()

	lock, info, err := storeOpenReadFile(private, ".lock", -1)
	if errors.Is(err, os.ErrNotExist) {
		readErr := read(private)
		if err := ctx.Err(); err != nil {
			return err
		}
		lock, info, err = storeOpenReadFile(private, ".lock", -1)
		if errors.Is(err, os.ErrNotExist) {
			return readErr
		}
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if wait {
		if err := storeWaitLock(ctx, lock, filelock.TryShared); err != nil {
			return err
		}
	} else {
		held, err := filelock.TryShared(lock)
		if err != nil {
			return storeError("store_locked", "Extension store lock is unavailable.")
		}
		if !held {
			return errStoreReadBusy
		}
	}
	defer filelock.Unlock(lock)
	named, namedErr := private.Lstat(".lock")
	if namedErr != nil || !named.Mode().IsRegular() || !os.SameFile(info, named) {
		return storeUnreadable()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := read(private); err != nil {
		return err
	}
	return ctx.Err()
}

func readStoreNamespace(ctx context.Context, private *os.Root, name string, read func(*os.Root) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if private == nil {
		return read(nil)
	}
	namespace, err := storeOpenDirectory(private, name, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if namespace != nil {
		defer namespace.Close()
	}
	if err := read(namespace); err != nil {
		return err
	}
	return ctx.Err()
}

func projectProviderAccounts[T any](ctx context.Context, store *Store, plugin, provider string, limit int, project func(*os.Root, storeAccount) T) ([]T, error) {
	if !storePluginID(plugin) || !storeToken(provider) {
		return nil, storeInvalid()
	}
	var result []T
	err := store.withReadStoreContext(ctx, plugin, func(root *os.Root) error {
		records, err := store.readAccounts(root, plugin)
		if err != nil {
			return err
		}
		count := 0
		for _, record := range records.Accounts {
			if record.Deleted || record.Provider != provider {
				continue
			}
			count++
			if limit > 0 && count > limit {
				return errAccountListBudget
			}
		}
		result = make([]T, 0, count)
		for _, record := range records.Accounts {
			if record.Deleted || record.Provider != provider {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			result = append(result, project(root, record))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
