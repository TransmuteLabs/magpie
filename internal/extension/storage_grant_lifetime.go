package extension

import (
	"context"
	"errors"
	"os"
	"runtime"
)

const storageMaxHeldDescriptors = 256

var storageDarwinSerial = make(chan struct{}, 1)

type storageLifetime struct {
	release  func()
	started  bool
	ended    bool
	native   bool
	deferred []*os.File
}

func acquireStorageLifetime(ctx context.Context) (*storageLifetime, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	return acquireStorageSerial(ctx, storageDarwinSerial)
}

func acquireStorageSerial(ctx context.Context, serial chan struct{}) (*storageLifetime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case serial <- struct{}{}:
		return &storageLifetime{release: func() { <-serial }}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (lifetime *storageLifetime) close() error {
	if lifetime == nil || lifetime.release == nil {
		return nil
	}
	var result error
	for _, file := range lifetime.deferred {
		result = errors.Join(result, file.Close())
	}
	lifetime.deferred = nil
	lifetime.release()
	lifetime.release = nil
	return result
}

func (grant *StorageGrant) closeMember(file *os.File) error {
	if grant.lifetime == nil || !grant.lifetime.native {
		return file.Close()
	}
	grant.lifetime.deferred = append(grant.lifetime.deferred, file)
	return nil
}

func trackStorageCall(ctx context.Context) {
	grant, ok := ctx.Value(storageGrantKey{}).(*StorageGrant)
	if !ok {
		return
	}
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if grant.lifetime != nil {
		grant.lifetime.started, grant.lifetime.ended = true, false
	}
}

func endStorageCall(ctx context.Context) {
	grant, ok := ctx.Value(storageGrantKey{}).(*StorageGrant)
	if !ok {
		return
	}
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if grant.lifetime != nil {
		grant.lifetime.ended = true
		if grant.closed {
			grant.lifetime.close()
		}
	}
}
