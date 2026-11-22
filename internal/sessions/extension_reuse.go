package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sessionMigrationReceipt struct {
	result  CarryResult
	source  *sessionSourceSnapshot
	target  *sessionTargetStore
	outputs *sessionInputSnapshot
	order   uint64
}

type sessionModuleSelection struct {
	entry                   extension.Installed
	identity, configVersion string
}

var extensionMigrations = struct {
	sync.Mutex
	entries  map[string]sessionMigrationReceipt
	inflight map[string]chan struct{}
	next     uint64
}{entries: map[string]sessionMigrationReceipt{}, inflight: map[string]chan struct{}{}}

func acquireSessionMigration(ctx context.Context, key string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		extensionMigrations.Lock()
		pending, busy := extensionMigrations.inflight[key]
		if !busy {
			pending = make(chan struct{})
			extensionMigrations.inflight[key] = pending
		}
		extensionMigrations.Unlock()
		if !busy {
			return func() {
				extensionMigrations.Lock()
				delete(extensionMigrations.inflight, key)
				close(pending)
				extensionMigrations.Unlock()
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending:
		}
	}
}

func cachedSessionMigration(key string) (sessionMigrationReceipt, bool) {
	extensionMigrations.Lock()
	defer extensionMigrations.Unlock()
	receipt, ok := extensionMigrations.entries[key]
	return receipt, ok
}

func forgetSessionMigration(key string) {
	extensionMigrations.Lock()
	defer extensionMigrations.Unlock()
	delete(extensionMigrations.entries, key)
}

func sessionMigrationIdentity(ctx context.Context, manager *extension.Manager, plugin string) (sdk.Manifest, string, error) {
	selected, err := sessionMigrationSelection(ctx, manager, plugin)
	return selected.entry.Manifest, selected.identity, err
}

func sessionMigrationSelection(ctx context.Context, manager *extension.Manager, plugin string) (sessionModuleSelection, error) {
	entry, err := manager.ActiveModule(ctx, plugin)
	if err != nil {
		return sessionModuleSelection{}, err
	}
	config := ""
	if entry.Manifest.Contract.Version.Minor >= 1 {
		current, value, err := manager.ActiveModuleConfig(ctx, plugin)
		if err != nil {
			return sessionModuleSelection{}, err
		}
		if !sameSessionModule(entry, current) {
			return sessionModuleSelection{}, extension.ErrActionChanged
		}
		entry = current
		config = value.Version
	}
	body, err := json.Marshal([]string{manager.Namespace(), plugin, entry.Revision, config})
	if err != nil {
		return sessionModuleSelection{}, err
	}
	digest := sha256.Sum256(body)
	return sessionModuleSelection{entry: entry, identity: hex.EncodeToString(digest[:]), configVersion: config}, nil
}

func sameSessionModule(before, after extension.Installed) bool {
	return before.Manifest.ID == after.Manifest.ID && before.Revision == after.Revision &&
		before.EnableGeneration == after.EnableGeneration
}

func (selected sessionModuleSelection) check(ctx context.Context, manager *extension.Manager) error {
	current, err := sessionMigrationSelection(ctx, manager, selected.entry.Manifest.ID)
	if err != nil {
		return err
	}
	if !sameSessionModule(selected.entry, current.entry) || selected.identity != current.identity {
		return extension.ErrActionChanged
	}
	return nil
}

func sessionMigrationKey(selected sessionModuleSelection, source Session, route SessionExtension, target *sessionTargetStore, options []byte) string {
	body, _ := json.Marshal(struct {
		Identity         string
		EnableGeneration string
		Agent            string
		ID               string
		Path             string
		Cwd              string
		Route            SessionExtension
		Target           string
		Options          []byte
	}{selected.identity, selected.entry.EnableGeneration, source.Agent, source.ID, source.Path, source.Cwd, route, target.selection(), options})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func sessionMigrationReceiptValid(ctx context.Context, receipt sessionMigrationReceipt) bool {
	return receipt.source.check(ctx) == nil && receipt.target.check(ctx) == nil && receipt.outputs.check(ctx) == nil
}

func rememberSessionMigration(key string, receipt sessionMigrationReceipt) {
	extensionMigrations.Lock()
	defer extensionMigrations.Unlock()
	if len(extensionMigrations.entries) >= 128 {
		oldest := ""
		order := ^uint64(0)
		for name, item := range extensionMigrations.entries {
			if item.order < order {
				oldest, order = name, item.order
			}
		}
		delete(extensionMigrations.entries, oldest)
	}
	extensionMigrations.next++
	receipt.order = extensionMigrations.next
	extensionMigrations.entries[key] = receipt
}

func captureMigrationOutputs(ctx context.Context, store *sessionTargetStore, plan *extension.ActionPlan, published map[string]fs.FileInfo) (*sessionInputSnapshot, error) {
	root, err := os.OpenRoot(store.directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	snapshot := &sessionInputSnapshot{root: store.directory, info: info}
	total := 0
	for _, effect := range plan.Preview().Files {
		if effect.Remove {
			return nil, extension.ErrActionChanged
		}
		path := filepath.Join(plan.Root(), filepath.FromSlash(effect.Path))
		relative, err := filepath.Rel(store.directory, path)
		if err != nil || !sessionInputPath(filepath.ToSlash(relative)) {
			return nil, extension.ErrActionChanged
		}
		current, body, err := readSessionInput(ctx, root, filepath.ToSlash(relative), extension.MaxActionBytes-total)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(effect.Body)
		if !os.SameFile(published[effect.Path], current.info) || current.hash != digest || !sessionStoreModeEqual(current.info.Mode(), 0o600) {
			return nil, extension.ErrActionChanged
		}
		total += len(body)
		snapshot.files = append(snapshot.files, current)
	}
	return snapshot, nil
}
