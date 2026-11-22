package plugin

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/filelock"
)

var errCommunitySubscriptionBusy = errors.New("the community subscription is busy; try again when its current operations finish")

func GuardCommunitySubscription(ctx context.Context, provider string) (func(), error) {
	return guardCommunitySubscription(ctx, AuthPath(), provider)
}

func guardCommunitySubscription(ctx context.Context, path, provider string) (func(), error) {
	release, err := lockCommunitySubscription(ctx, path, provider, true)
	if err != nil {
		return nil, err
	}
	if err := checkCommunitySubscription(ctx, path, provider); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func lockCommunitySubscription(ctx context.Context, path, provider string, shared bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if provider == "" || ProviderOf(provider) != provider {
		return nil, errors.New("select a source provider")
	}
	lock := fmt.Sprintf("%s.use-%x.lock", path, sha256.Sum256([]byte(provider)))
	file, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
			return nil, err
		}
		file, err = os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("locking community subscription use: %w", err)
	}
	try := filelock.TryLock
	if shared {
		try = filelock.TryShared
	}
	held, err := try(file)
	if err == nil && held {
		info, statErr := file.Stat()
		if statErr == nil && info.Mode().IsRegular() && sameAuthFile(lock, info) {
			return func() {
				filelock.Unlock(file)
				_ = file.Close()
			}, nil
		}
		err = errors.New("community subscription use lock changed")
	}
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("locking community subscription use: %w", err)
	}
	return nil, errCommunitySubscriptionBusy
}

func (h *host) authUseLoop() {
	defer close(h.authUseDone)
	held := map[string]func(){}
	defer func() {
		_ = h.wait()
		for _, release := range held {
			release()
		}
	}()
	for {
		select {
		case <-h.ctx.Done():
			return
		case request := <-h.authUseRequests:
			reply := authStoreReply{ID: request.ID}
			if request.Release {
				if release := held[request.Use]; release != nil {
					release()
					delete(held, request.Use)
				}
			} else if request.Use == "" || request.Version != "" || request.Records != nil || held[request.Use] != nil || len(held) >= callLimit() {
				reply.Error = "invalid community subscription use request"
			} else {
				release, err := guardCommunitySubscription(h.ctx, h.authPath, request.Use)
				if err != nil {
					reply.Error = err.Error()
				} else {
					held[request.Use] = release
				}
			}
			if h.replyAuthStore(reply) != nil {
				return
			}
		}
	}
}
