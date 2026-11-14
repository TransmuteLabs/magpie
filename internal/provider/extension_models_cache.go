package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionModelChunkBytes = 512 << 10

type extensionModelCatalog struct {
	Schema int      `json:"schema"`
	Bytes  int      `json:"bytes"`
	SHA256 string   `json:"sha256"`
	Chunks []string `json:"chunks"`
}

func extensionModelRevision(ctx context.Context, host extensionProviderHost, entry extensionhost.Installed) (string, error) {
	version := ""
	if entry.Manifest.Contract.Version.Minor >= 1 && entry.Manifest.Contract.Has(sdk.ModelDiscovery) {
		config, err := host.ModuleConfig(ctx, entry.Manifest.ID)
		if err != nil {
			return "", err
		}
		version = config.Version
	}
	return entry.Revision + "\x00" + version, nil
}

func (binding *extensionAccount) checkModelRevision(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := binding.host.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Manifest.ID != binding.manifest.ID {
			continue
		}
		revision, err := extensionModelRevision(ctx, binding.host, entry)
		if err != nil {
			return err
		}
		if revision == binding.modelKey {
			return nil
		}
	}
	return errors.New("extension model catalog selection changed")
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

func (binding *extensionAccount) readModelCatalog() ([]sdk.Model, bool, error) {
	scope := sdk.Scope{Plugin: binding.scope.Plugin}
	value, err := binding.host.Store().StateGet(scope, binding.modelCatalogKey())
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
		value, err := binding.host.Store().StateGet(scope, "models.chunk.v1."+digest)
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
	if err := binding.checkModelRevision(context.Background()); err != nil {
		return nil, false, err
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
		if err := binding.putModelState("models.chunk.v1."+digest, value); err != nil {
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
	return binding.putModelState(binding.modelCatalogKey(), value)
}

func (binding *extensionAccount) putModelState(key string, value json.RawMessage) error {
	scope := sdk.Scope{Plugin: binding.scope.Plugin}
	for range 8 {
		before, err := binding.host.Store().StateGet(scope, key)
		if err != nil {
			return err
		}
		if bytes.Equal(before.Value, value) {
			return nil
		}
		_, err = binding.host.Store().StateCAS(scope, key, before.Version, value)
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
