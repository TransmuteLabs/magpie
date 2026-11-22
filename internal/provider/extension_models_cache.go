package provider

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionModelChunkBytes = 512 << 10

const (
	extensionModelCacheEntries = 64
	extensionModelCacheBytes   = 32 << 20
)

var extensionModelCache extensionModelMemo

type extensionModelMemo struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	bytes   int
}

type extensionModelSnapshot struct {
	key         string
	rootVersion uint64
	rootDigest  [sha256.Size]byte
	models      []sdk.Model
	bytes       int
}

func (memo *extensionModelMemo) Load(key string, rootVersion uint64, rootDigest [sha256.Size]byte) (extensionModelSnapshot, bool) {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	element := memo.entries[key]
	if element == nil {
		return extensionModelSnapshot{}, false
	}
	snapshot := element.Value.(extensionModelSnapshot)
	if snapshot.rootVersion != rootVersion || snapshot.rootDigest != rootDigest {
		return extensionModelSnapshot{}, false
	}
	memo.order.MoveToFront(element)
	return snapshot, true
}

func (memo *extensionModelMemo) Store(snapshot extensionModelSnapshot) {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	memo.remove(memo.entries[snapshot.key])
	if snapshot.bytes > extensionModelCacheBytes {
		return
	}
	if memo.entries == nil {
		memo.entries = make(map[string]*list.Element)
	}
	for len(memo.entries) >= extensionModelCacheEntries || memo.bytes+snapshot.bytes > extensionModelCacheBytes {
		memo.remove(memo.order.Back())
	}
	memo.entries[snapshot.key] = memo.order.PushFront(snapshot)
	memo.bytes += snapshot.bytes
}

func (memo *extensionModelMemo) remove(element *list.Element) {
	if element == nil {
		return
	}
	snapshot := memo.order.Remove(element).(extensionModelSnapshot)
	delete(memo.entries, snapshot.key)
	memo.bytes -= snapshot.bytes
}

func (memo *extensionModelMemo) Delete(key string) {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	memo.remove(memo.entries[key])
}

func (memo *extensionModelMemo) Clear() {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	memo.entries = nil
	memo.order.Init()
	memo.bytes = 0
}

type extensionModelCatalog struct {
	Schema int      `json:"schema"`
	Bytes  int      `json:"bytes"`
	SHA256 string   `json:"sha256"`
	Chunks []string `json:"chunks"`
}

func extensionModelRevision(ctx context.Context, host extensionProviderHost, entry extensionhost.Installed) (string, error) {
	version, err := extensionConfigVersion(ctx, host, entry)
	if err != nil {
		return "", err
	}
	return entry.Revision + "\x00" + version, nil
}

func extensionConfigVersion(ctx context.Context, host extensionProviderHost, entry extensionhost.Installed) (string, error) {
	if entry.Manifest.Contract.Version.Minor < 1 {
		return "0", nil
	}
	config, err := host.ModuleConfig(ctx, entry.Manifest.ID)
	return config.Version, err
}

func (binding *extensionAccount) checkModelRevision(ctx context.Context) error {
	_, err := binding.modelRevisionEntry(ctx)
	return err
}

func (binding *extensionAccount) modelRevisionEntry(ctx context.Context) (extensionhost.Installed, error) {
	return selectedExtensionModule(ctx, binding.host, binding.manifest.ID, binding.modelKey, binding.enableGeneration,
		binding.manifest.Contract.Version.Minor >= 1)
}

func selectedExtensionModule(ctx context.Context, host extensionProviderHost, id, expected, enableGeneration string, configurable bool) (extensionhost.Installed, error) {
	entry, _, err := selectedExtensionModuleConfig(ctx, host, id, expected, enableGeneration, configurable)
	return entry, err
}

func selectedExtensionModuleConfig(ctx context.Context, host extensionProviderHost, id, expected, enableGeneration string, configurable bool) (extensionhost.Installed, sdk.ConfigSnapshot, error) {
	if registry, ok := host.(interface {
		ActiveModuleConfig(context.Context, string) (extensionhost.Installed, sdk.ConfigSnapshot, error)
	}); ok && configurable {
		entry, config, err := registry.ActiveModuleConfig(ctx, id)
		if err != nil {
			return extensionhost.Installed{}, sdk.ConfigSnapshot{}, err
		}
		if entry.Revision+"\x00"+config.Version != expected || entry.EnableGeneration != enableGeneration {
			return extensionhost.Installed{}, sdk.ConfigSnapshot{}, errors.New("extension package, configuration or enablement changed")
		}
		return entry, config, nil
	}
	entry, err := enabledExtensionModule(ctx, host, id)
	if err != nil {
		return extensionhost.Installed{}, sdk.ConfigSnapshot{}, err
	}
	config := sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)}
	if entry.Manifest.Contract.Version.Minor >= 1 {
		config, err = host.ModuleConfig(ctx, entry.Manifest.ID)
		if err != nil {
			return extensionhost.Installed{}, sdk.ConfigSnapshot{}, err
		}
	}
	if entry.Revision+"\x00"+config.Version != expected || entry.EnableGeneration != enableGeneration {
		return extensionhost.Installed{}, sdk.ConfigSnapshot{}, errors.New("extension package, configuration or enablement changed")
	}
	return entry, config, nil
}

func (binding *extensionAccount) validateModels(models []sdk.Model) error {
	manifest := binding.manifest
	manifest.Providers = slices.Clone(manifest.Providers)
	for index := range manifest.Providers {
		if manifest.Providers[index].ID == binding.scope.Provider {
			manifest.Providers[index].Models = models
		}
	}
	return manifest.Validate()
}

func extensionModelHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validExtensionModelHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func (binding *extensionAccount) modelCatalogKey() string {
	return extensionhost.ModelCatalogPrefix + extensionModelHash([]byte(binding.cacheKey()))
}

func extensionModelsJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("extension model catalog is unreadable")
	}
	return nil
}

func (binding *extensionAccount) readModelCatalog(ctx context.Context) (extensionModelSnapshot, bool, error) {
	key := binding.modelCatalogKey()
	scope := sdk.Scope{Plugin: binding.scope.Plugin}
	value, err := binding.host.Store().StateGetContext(ctx, scope, key)
	if err != nil || value.Version == 0 {
		return extensionModelSnapshot{}, false, err
	}
	digest := sha256.Sum256(value.Value)
	if snapshot, ok := extensionModelCache.Load(key, value.Version, digest); ok {
		return snapshot, false, nil
	}
	var record extensionModelCatalog
	if extensionModelsJSON(value.Value, &record) != nil || record.Schema != 1 ||
		record.Bytes < 1 || record.Bytes > sdk.MaxFrameBytes || !validExtensionModelHash(record.SHA256) ||
		len(record.Chunks) != (record.Bytes+extensionModelChunkBytes-1)/extensionModelChunkBytes {
		return extensionModelSnapshot{}, false, errors.New("extension model catalog metadata is unreadable")
	}
	body := make([]byte, 0, record.Bytes)
	for index, digest := range record.Chunks {
		if !validExtensionModelHash(digest) {
			return extensionModelSnapshot{}, false, errors.New("extension model catalog chunk identity is invalid")
		}
		value, err := binding.host.Store().StateGetContext(ctx, scope, "models.chunk.v1."+digest)
		if err != nil {
			return extensionModelSnapshot{}, false, err
		}
		var chunk []byte
		expected := min(extensionModelChunkBytes, record.Bytes-index*extensionModelChunkBytes)
		if value.Version == 0 || extensionModelsJSON(value.Value, &chunk) != nil ||
			len(chunk) != expected || extensionModelHash(chunk) != digest {
			return extensionModelSnapshot{}, false, errors.New("extension model catalog chunk is unreadable")
		}
		body = append(body, chunk...)
	}
	var result sdk.ModelsResult
	if extensionModelHash(body) != record.SHA256 || extensionModelsJSON(body, &result) != nil ||
		binding.validateModels(result.Models) != nil {
		return extensionModelSnapshot{}, false, errors.New("extension model catalog content is invalid")
	}
	return extensionModelSnapshot{key: key, rootVersion: value.Version, rootDigest: digest, models: result.Models, bytes: record.Bytes}, true, nil
}

func (binding *extensionAccount) writeModelCatalog(ctx context.Context, result sdk.ModelsResult, before extensionhost.VersionedValue) (extensionModelSnapshot, error) {
	selection := *binding
	selection.scope, selection.generation = binding.currentScope(), nil
	binding = &selection
	entry, err := binding.checkedEntry(ctx, true)
	if err != nil {
		return extensionModelSnapshot{}, err
	}
	_, configVersion, _ := strings.Cut(binding.modelKey, "\x00")

	body, err := json.Marshal(result)
	if err != nil || len(body) > sdk.MaxFrameBytes {
		return extensionModelSnapshot{}, errors.New("extension model catalog exceeds its cache budget")
	}
	record := extensionModelCatalog{Schema: 1, Bytes: len(body), SHA256: extensionModelHash(body)}
	for offset := 0; offset < len(body); offset += extensionModelChunkBytes {
		if err := ctx.Err(); err != nil {
			return extensionModelSnapshot{}, err
		}
		chunk := body[offset:min(len(body), offset+extensionModelChunkBytes)]
		digest := extensionModelHash(chunk)
		value, err := json.Marshal(chunk)
		if err != nil {
			return extensionModelSnapshot{}, err
		}
		if _, err := binding.putModelState(ctx, "models.chunk.v1."+digest, value); err != nil {
			return extensionModelSnapshot{}, err
		}
		record.Chunks = append(record.Chunks, digest)
	}
	value, err := json.Marshal(record)
	if err != nil {
		return extensionModelSnapshot{}, err
	}
	key := binding.modelCatalogKey()
	ctx = extensionhost.WithSubscriptionOwnerRevision(ctx, binding.identity().revision)
	written, err := binding.host.PublishModelCatalog(ctx, entry, configVersion, binding.scope, key, before.Version, value)
	if err != nil {
		return extensionModelSnapshot{}, err
	}
	return extensionModelSnapshot{key: key, rootVersion: written.Version, rootDigest: sha256.Sum256(value), models: result.Models, bytes: len(body)}, nil
}

func (binding *extensionAccount) putModelState(ctx context.Context, key string, value json.RawMessage) (uint64, error) {
	scope := sdk.Scope{Plugin: binding.scope.Plugin}
	for range 8 {
		before, err := binding.host.Store().StateGetContext(ctx, scope, key)
		if err != nil {
			return 0, err
		}
		if bytes.Equal(before.Value, value) {
			return before.Version, nil
		}
		after, err := binding.host.Store().StateCASContext(ctx, scope, key, before.Version, value)
		if err == nil {
			return after.Version, nil
		}
		var conflict *sdk.Error
		if !errors.As(err, &conflict) || conflict.Code != "conflict" {
			return 0, err
		}
	}
	return 0, errors.New("extension model catalog changed during publication")
}
