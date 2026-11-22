package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

const authDocumentLimit = 16 << 20

type authStoreRequest struct {
	ID      int64           `json:"id"`
	Version string          `json:"version,omitempty"`
	Records json.RawMessage `json:"records,omitempty"`
	Use     string          `json:"use,omitempty"`
	Release bool            `json:"release,omitempty"`
}

type authSnapshot struct {
	Version string          `json:"version"`
	Records json.RawMessage `json:"records"`
	Known   bool            `json:"known"`
}

type authStoreReply struct {
	ID       int64         `json:"id"`
	Snapshot *authSnapshot `json:"snapshot,omitempty"`
	Retry    bool          `json:"retry,omitempty"`
	Error    string        `json:"error,omitempty"`
}

func (h *host) authStoreLoop() {
	defer close(h.authDone)
	for {
		select {
		case <-h.ctx.Done():
			return
		case request := <-h.authRequests:
			reply, err := editHostAuth(h.ctx, h.authPath, request)
			if err != nil {
				reply.Error = err.Error()
			}
			if h.replyAuthStore(reply) != nil {
				return
			}
		}
	}
}

func (h *host) replyAuthStore(reply authStoreReply) error {
	return h.enqueue(h.ctx, map[string]any{"method": "authStore", "params": reply})
}

// editHostAuth uses a fresh snapshot of the effective sign-ins to check
// a Bun mutation. No plugin code or STDIO traffic runs under the file lock.
func editHostAuth(ctx context.Context, path string, request authStoreRequest) (authStoreReply, error) {
	reply := authStoreReply{ID: request.ID}
	if len(request.Records) >= authDocumentLimit || (request.Version == "") != (request.Records == nil) ||
		request.Records != nil && !authObject(request.Records) {
		return reply, errors.New("invalid saved plugin sign-in update")
	}
	unlock, err := lockAuth(ctx, path)
	if err != nil {
		return reply, err
	}
	defer unlock()
	snapshot, err := snapshotAuth(path)
	if err != nil {
		return reply, err
	}
	if request.Version == "" {
		reply.Snapshot = &snapshot
		return reply, nil
	}
	if snapshot.Version != request.Version {
		reply.Retry = true
		return reply, nil
	}
	if !snapshot.Known && len(bytes.Trim(request.Records, "{} \r\n\t")) == 0 {
		return reply, errors.New("unreadable saved plugin sign-ins cannot be cleared")
	}
	if err := ctx.Err(); err != nil {
		return reply, err
	}
	var before []byte
	if snapshot.Known {
		before = snapshot.Records
	}
	return reply, publishAuth(ctx, path, before, request.Records, authObject)
}

func authObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}

func snapshotAuth(path string) (authSnapshot, error) {
	raw, err := readAuthDocument(path)
	if errors.Is(err, os.ErrNotExist) {
		return authSnapshot{Version: "missing", Records: json.RawMessage("{}"), Known: true}, nil
	}
	if err != nil {
		return authSnapshot{}, err
	}
	if !authObject(raw) {
		backup, err := readAuthDocument(lastgood.Bak(path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return authSnapshot{}, err
		}
		if !authObject(backup) {
			return authSnapshot{Version: "unknown", Records: json.RawMessage("{}")}, nil
		}
		lastgood.Noted(path, authDamage(raw))
		raw = backup
	}
	return authSnapshot{Version: fmt.Sprintf("%x", sha256.Sum256(raw)), Records: raw, Known: true}, nil
}

func authDamage(raw []byte) string {
	switch {
	case len(raw) > 0 && len(bytes.Trim(raw, "\x00")) == 0:
		return fmt.Sprintf("%d bytes, every one zero", len(raw))
	case len(bytes.TrimSpace(raw)) == 0:
		return "it is empty"
	default:
		return "it doesn't parse"
	}
}

func readAuthDocument(path string) ([]byte, error) {
	info, err := steady.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > authDocumentLimit {
		return nil, errors.New("saved plugin sign-ins exceed the file limits")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("current saved plugin sign-ins changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, authDocumentLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > authDocumentLimit {
		return nil, errors.New("saved plugin sign-ins exceed the file limits")
	}
	return raw, nil
}

func publishAuth(ctx context.Context, path string, before, raw []byte, valid lastgood.Valid) error {
	frozen, err := frozenCommunitySubscriptions(ctx, path)
	if err != nil {
		return err
	}
	if len(frozen) > 0 {
		raw, err = preserveFrozenAuth(before, raw, frozen)
		if err != nil {
			return err
		}
	}
	if err := lastgood.Keep(path, valid, 0o600); err != nil {
		return err
	}
	return steady.WriteFileChecked(path, append(raw, '\n'), 0o600, ctx.Err)
}

var errCommunitySubscriptionOwned = errors.New("the saved subscription is managed by an SDK plugin or has a transfer in progress")

func CheckCommunitySubscription(ctx context.Context, provider string) error {
	return checkCommunitySubscription(ctx, AuthPath(), provider)
}

func checkCommunitySubscription(ctx context.Context, path, provider string) error {
	frozen, err := frozenCommunitySubscriptions(ctx, path)
	if err == nil && frozen[provider] {
		return errCommunitySubscriptionOwned
	}
	return err
}

func frozenCommunitySubscriptions(ctx context.Context, path string) (map[string]bool, error) {
	store := extension.NewStore(filepath.Join(filepath.Dir(path), "extensions"))
	rows, err := store.SubscriptionOwners(ctx)
	if err != nil {
		return nil, err
	}
	var frozen map[string]bool
	for _, owner := range rows {
		if owner.Active || owner.Pending {
			if frozen == nil {
				frozen = map[string]bool{}
			}
			frozen[owner.Provider] = true
		}
	}
	return frozen, nil
}

func preserveFrozenAuth(before, raw []byte, frozen map[string]bool) ([]byte, error) {
	var current, next map[string]json.RawMessage
	if json.Unmarshal(before, &current) != nil || current == nil || json.Unmarshal(raw, &next) != nil || next == nil {
		return nil, errors.New("the saved plugin sign-ins are unreadable while a subscription is transferred")
	}
	preserve := false
	for key, value := range next {
		if frozen[ProviderOf(key)] {
			if !sameAuthRecord(current[key], value) {
				return nil, errCommunitySubscriptionOwned
			}
			preserve = preserve || !bytes.Equal(current[key], value)
			next[key] = current[key]
		}
	}
	for key := range current {
		if frozen[ProviderOf(key)] && next[key] == nil {
			return nil, errCommunitySubscriptionOwned
		}
	}
	if !preserve {
		return raw, nil
	}
	var output bytes.Buffer
	output.WriteByte('{')
	for _, key := range slices.Sorted(maps.Keys(next)) {
		if output.Len() > 1 {
			output.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		output.Write(name)
		output.WriteByte(':')
		output.Write(next[key])
		if output.Len()+2 > authDocumentLimit {
			return nil, errors.New("saved plugin sign-ins exceed the file limits")
		}
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func sameAuthRecord(before, after json.RawMessage) bool {
	if bytes.Equal(before, after) {
		return true
	}
	var left, right any
	decode := func(raw []byte, value *any) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		return dec.Decode(value)
	}
	return decode(before, &left) == nil && decode(after, &right) == nil && reflect.DeepEqual(left, right)
}
