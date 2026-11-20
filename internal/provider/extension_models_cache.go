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
	key    string
	models []sdk.Model
	bytes  int
}

func (memo *extensionModelMemo) Load(key string) ([]sdk.Model, bool) {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	element := memo.entries[key]
	if element == nil {
		return nil, false
	}
	memo.order.MoveToFront(element)
	return element.Value.(extensionModelSnapshot).models, true
}

func (memo *extensionModelMemo) Store(key string, models []sdk.Model) {
	body, err := json.Marshal(sdk.ModelsResult{Models: models})
	size := len(body)
	memo.mu.Lock()
	defer memo.mu.Unlock()
	memo.remove(memo.entries[key])
	if err != nil || size > extensionModelCacheBytes {
		return
	}
	if memo.entries == nil {
		memo.entries = make(map[string]*list.Element)
	}
	for len(memo.entries) >= extensionModelCacheEntries || memo.bytes+size > extensionModelCacheBytes {
		memo.remove(memo.order.Back())
	}
	memo.entries[key] = memo.order.PushFront(extensionModelSnapshot{key: key, models: models, bytes: size})
	memo.bytes += size
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

func cloneExtensionModels(models []sdk.Model) []sdk.Model {
	return (sdk.Manifest{Providers: []sdk.ProviderDescriptor{{Models: models}}}).Clone().Providers[0].Models
}

type extensionModelCatalog struct {
	Schema int      `json:"schema"`
	Bytes  int      `json:"bytes"`
	SHA256 string   `json:"sha256"`
	Chunks []string `json:"chunks"`
}

func extensionModelRevision(ctx context.Context, host extensionProviderHost, entry extensionhost.Installed) (string, error) {
	version := ""
	if entry.Manifest.Contract.Version.Minor >= 9 ||
		entry.Manifest.Contract.Version.Minor >= 1 && entry.Manifest.Contract.Has(sdk.ModelDiscovery) {
		config, err := host.ModuleConfig(ctx, entry.Manifest.ID)
		if err != nil {
			return "", err
		}
		version = config.Version
	}
	return entry.Revision + "\x00" + version, nil
}

func (binding *extensionAccount) checkModelRevision(ctx context.Context) error {
	entry, err := enabledExtensionModule(ctx, binding.host, binding.manifest.ID)
	if err != nil {
		return err
	}
	revision, err := extensionModelRevision(ctx, binding.host, entry)
	if err != nil {
		return err
	}
	if revision != binding.modelKey {
		return errors.New("extension model catalog selection changed")
	}
	return nil
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
	return "models.catalog.v1." + extensionModelHash([]byte(binding.cacheKey()))
}

func extensionModelsJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("extension model catalog is unreadable")
	}
	return nil
}

func (binding *extensionAccount) readModelCatalog(ctx context.Context) ([]sdk.Model, bool, error) {
	scope := sdk.Scope{Plugin: binding.scope.Plugin}
	value, err := binding.host.Store().StateGetContext(ctx, scope, binding.modelCatalogKey())
	if err != nil || value.Version == 0 {
		return nil, false, err
	}
	var record extensionModelCatalog
	if extensionModelsJSON(value.Value, &record) != nil || record.Schema != 1 ||
		record.Bytes < 1 || record.Bytes > sdk.MaxFrameBytes || !validExtensionModelHash(record.SHA256) ||
		len(record.Chunks) != (record.Bytes+extensionModelChunkBytes-1)/extensionModelChunkBytes {
		return nil, false, errors.New("extension model catalog metadata is unreadable")
	}
	body := make([]byte, 0, record.Bytes)
	for index, digest := range record.Chunks {
		if !validExtensionModelHash(digest) {
			return nil, false, errors.New("extension model catalog chunk identity is invalid")
		}
		value, err := binding.host.Store().StateGetContext(ctx, scope, "models.chunk.v1."+digest)
		if err != nil {
			return nil, false, err
		}
		var chunk []byte
		expected := min(extensionModelChunkBytes, record.Bytes-index*extensionModelChunkBytes)
		if value.Version == 0 || extensionModelsJSON(value.Value, &chunk) != nil ||
			len(chunk) != expected || extensionModelHash(chunk) != digest {
			return nil, false, errors.New("extension model catalog chunk is unreadable")
		}
		body = append(body, chunk...)
	}
	var result sdk.ModelsResult
	if extensionModelHash(body) != record.SHA256 || extensionModelsJSON(body, &result) != nil ||
		binding.validateModels(result.Models) != nil {
		return nil, false, errors.New("extension model catalog content is invalid")
	}
	return result.Models, true, nil
}

func (binding *extensionAccount) writeModelCatalog(ctx context.Context, result sdk.ModelsResult) error {
	body, err := json.Marshal(result)
	if err != nil || len(body) > sdk.MaxFrameBytes {
		return errors.New("extension model catalog exceeds its cache budget")
	}
	record := extensionModelCatalog{Schema: 1, Bytes: len(body), SHA256: extensionModelHash(body)}
	for offset := 0; offset < len(body); offset += extensionModelChunkBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := body[offset:min(len(body), offset+extensionModelChunkBytes)]
		digest := extensionModelHash(chunk)
		value, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if err := binding.putModelState(ctx, "models.chunk.v1."+digest, value); err != nil {
			return err
		}
		record.Chunks = append(record.Chunks, digest)
	}
	if err := binding.check(ctx); err != nil {
		return err
	}
	if err := binding.checkModelRevision(ctx); err != nil {
		return err
	}
	value, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return binding.putModelState(ctx, binding.modelCatalogKey(), value)
}

func (binding *extensionAccount) putModelState(ctx context.Context, key string, value json.RawMessage) error {
	scope := sdk.Scope{Plugin: binding.scope.Plugin}
	for range 8 {
		before, err := binding.host.Store().StateGetContext(ctx, scope, key)
		if err != nil {
			return err
		}
		if bytes.Equal(before.Value, value) {
			return nil
		}
		_, err = binding.host.Store().StateCASContext(ctx, scope, key, before.Version, value)
		if err == nil {
			return nil
		}
		var conflict *sdk.Error
		if !errors.As(err, &conflict) || conflict.Code != "conflict" {
			return err
		}
	}
	return errors.New("extension model catalog changed during publication")
}
