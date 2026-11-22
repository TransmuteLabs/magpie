package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

// lockAuth holds the persistent writer lock until the caller releases it.
// A retired or crashed process cannot leave the OS lock held. The separate
// reservation is for older magpies that still wait on plugin-auth.json.lock.
func lockAuth(ctx context.Context, path string) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, filelock.Wait)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".write.lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	locked := false
	defer func() {
		if !locked {
			_ = file.Close()
		}
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		held, err := filelock.TryLock(file)
		if err != nil {
			return nil, fmt.Errorf("locking saved plugin sign-ins: %w", err)
		}
		if held {
			info, err := file.Stat()
			if err != nil || !sameAuthFile(path+".write.lock", info) {
				return nil, errors.New("saved plugin sign-in writer lock changed")
			}
			release, err := reserveAuth(ctx, path, tick.C)
			if err != nil {
				return nil, err
			}
			locked = true
			return func() {
				release()
				filelock.Unlock(file)
				_ = file.Close()
			}, nil
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return nil, fmt.Errorf("waiting for the saved plugin sign-in writer: %w", ctx.Err())
}

// reserveAuth uses a hard link to record ownership in the same operation
// that takes the legacy reservation. After a crash only the next OS lock
// holder can clean up our link. The marker has no open handle to prevent
// leaving a non-deletable reservation on Windows.
func reserveAuth(ctx context.Context, path string, tick <-chan time.Time) (func(), error) {
	marker, lock := path+".write-owner", path+".lock"
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	closeErr := file.Close()
	if err != nil || closeErr != nil || !sameAuthFile(marker, info) {
		return nil, errors.New("saved plugin sign-in reservation marker is unavailable")
	}
	remove := func() error {
		if sameAuthFile(lock, info) {
			return os.Remove(lock)
		}
		return nil
	}
	if err := remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for ctx.Err() == nil {
		now := time.Now()
		if err := os.Chtimes(marker, now, now); err != nil {
			return nil, err
		}
		if err := os.Link(marker, lock); err == nil {
			if !sameAuthFile(lock, info) {
				return nil, errors.New("saved plugin sign-in reservation changed")
			}
			return func() { _ = remove() }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
		case <-tick:
		}
	}
	return nil, fmt.Errorf("waiting for the older saved plugin sign-in reservation (plugin-auth.json.lock): %w", ctx.Err())
}

func sameAuthFile(path string, saved os.FileInfo) bool {
	current, err := os.Lstat(path)
	return err == nil && current.Mode().IsRegular() && os.SameFile(saved, current)
}
