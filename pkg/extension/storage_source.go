package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const StorageSource Capability = "session-storage-source"

const (
	MethodStorageList   = "session.storage.list"
	MethodStorageRead   = "session.storage.read"
	MethodStorageNative = "storage.native"
	MethodStorageFiles  = "storage.list"
	MethodStorageBytes  = "storage.read"
	MethodStorageCheck  = "storage.check"
)

type StorageSourceDescriptor struct {
	ID          string               `json:"id"`
	Agent       string               `json:"agent"`
	Codec       string               `json:"codec"`
	Roots       []SessionSourceStore `json:"roots"`
	Families    []StorageFamily      `json:"families"`
	Transcript  bool                 `json:"transcript"`
	Resume      string               `json:"resume"`
	AgentResume *SessionResumeRecipe `json:"agentResume,omitempty"`
}

func (source *StorageSourceDescriptor) UnmarshalJSON(raw []byte) error {
	type descriptor StorageSourceDescriptor
	var value descriptor
	if err := sourceJSON(raw, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var transcript *bool
	if json.Unmarshal(fields["transcript"], &transcript) != nil || transcript == nil {
		return errors.New("storage source requires an explicit transcript flag")
	}
	*source = StorageSourceDescriptor(value)
	return nil
}

type StorageFamily struct {
	ID       string   `json:"id"`
	Primary  string   `json:"primary,omitempty"`
	Sidecars []string `json:"sidecars,omitempty"`
	Trees    []string `json:"trees,omitempty"`
}

type StorageLimits struct {
	MaxEntries    int   `json:"maxEntries"`
	MaxReadBytes  int64 `json:"maxReadBytes"`
	MaxParts      int   `json:"maxParts"`
	MaxTextBytes  int   `json:"maxTextBytes"`
	MaxMediaBytes int   `json:"maxMediaBytes"`
}

func DefaultStorageLimits() StorageLimits {
	return StorageLimits{MaxEntries: 10000, MaxReadBytes: 64 << 20, MaxParts: 10000,
		MaxTextBytes: 4 << 20, MaxMediaBytes: 8 << 20}
}

type StorageReadLease struct {
	SourceID    string        `json:"sourceId"`
	RootRef     string        `json:"rootRef"`
	SnapshotRef string        `json:"snapshotRef"`
	FamilyID    string        `json:"familyId"`
	Limits      StorageLimits `json:"limits"`
}

type StorageSessionLocator struct {
	RootRef   string `json:"rootRef"`
	SessionID string `json:"sessionId"`
	Key       string `json:"key"`
}

type StorageRequest struct {
	Lease   StorageReadLease       `json:"lease"`
	Cursor  string                 `json:"cursor,omitempty"`
	Limit   int                    `json:"limit,omitempty"`
	Locator *StorageSessionLocator `json:"locator,omitempty"`
	Mode    string                 `json:"mode,omitempty"`
}

type StorageEntry struct {
	Locator StorageSessionLocator `json:"locator"`
	Summary SessionSourceSummary  `json:"summary"`
}

type StorageMedia struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	MIME  string `json:"mime"`
	Bytes []byte `json:"bytes"`
}

type StorageResult struct {
	SnapshotRef string                `json:"snapshotRef"`
	Entries     []StorageEntry        `json:"entries,omitempty"`
	NextCursor  string                `json:"nextCursor,omitempty"`
	SessionID   string                `json:"sessionId,omitempty"`
	Mode        string                `json:"mode,omitempty"`
	Summary     *SessionSourceSummary `json:"summary,omitempty"`
	Parts       []SessionSourcePart   `json:"parts,omitempty"`
	Media       []StorageMedia        `json:"media,omitempty"`
	Cut         bool                  `json:"cut"`
}

func IsStorageMethod(method string) bool {
	return method == MethodStorageList || method == MethodStorageRead
}

func IsStorageHostMethod(method string) bool {
	return method == MethodStorageNative || method == MethodStorageFiles ||
		method == MethodStorageBytes || method == MethodStorageCheck
}

func (family StorageFamily) Allows(name string) bool {
	if !SessionSourcePath(name) {
		return false
	}
	if name == family.Primary || slices.Contains(family.Sidecars, name) {
		return true
	}
	for _, tree := range family.Trees {
		if name == tree || strings.HasPrefix(name, tree+"/") {
			return true
		}
	}
	return false
}

func (manifest Manifest) ResolveStorageSource(id string) (StorageSourceDescriptor, bool) {
	for _, source := range manifest.StorageSources {
		if source.ID == id {
			return source, true
		}
	}
	return StorageSourceDescriptor{}, false
}

func cloneStorageSources(sources []StorageSourceDescriptor) []StorageSourceDescriptor {
	sources = slices.Clone(sources)
	for index := range sources {
		source := &sources[index]
		source.AgentResume = source.AgentResume.Clone()
		source.Roots = slices.Clone(source.Roots)
		source.Families = slices.Clone(source.Families)
		for family := range source.Families {
			source.Families[family].Sidecars = slices.Clone(source.Families[family].Sidecars)
			source.Families[family].Trees = slices.Clone(source.Families[family].Trees)
		}
	}
	return sources
}

func ValidateStorageSources(manifest Manifest) error {
	sources := manifest.StorageSources
	enabled := manifest.Contract.Has(StorageSource)
	if !enabled && len(sources) == 0 {
		return nil
	}
	if !enabled || manifest.Contract.Version.Minor < 8 || len(sources) == 0 || len(sources) > 16 ||
		manifest.Runtime != "native" {
		return errors.New("storage sources require native SDK API 1.8 and finite declarations")
	}
	ids := map[string]bool{}
	for _, source := range manifest.SessionSources {
		ids[source.ID] = true
	}
	for _, source := range sources {
		if !validID(source.ID) || ids[source.ID] || !validID(source.Agent) || !validID(source.Codec) ||
			len(source.Roots) == 0 || len(source.Roots) > 16 || len(source.Families) == 0 || len(source.Families) > 16 {
			return errors.New("invalid storage source descriptor")
		}
		ids[source.ID] = true
		if err := ValidateSessionResume(manifest, source.Agent, source.Resume, source.AgentResume); err != nil {
			return err
		}
		slots := map[string]bool{}
		for _, root := range source.Roots {
			if !validID(root.Slot) || slots[root.Slot] ||
				(root.HomeRelativePath == "") == (root.NativeRootSlot == "") ||
				root.HomeRelativePath != "" && !validHomePath(root.HomeRelativePath) ||
				root.NativeRootSlot != "" && !validID(root.NativeRootSlot) {
				return errors.New("invalid storage root")
			}
			slots[root.Slot] = true
		}
		families := map[string]bool{}
		for _, family := range source.Families {
			if !validID(family.ID) || families[family.ID] || len(family.Sidecars) > 8 ||
				len(family.Trees) > 16 || family.Primary == "" && len(family.Trees) == 0 {
				return errors.New("invalid storage family")
			}
			families[family.ID] = true
			names := map[string]bool{}
			paths := append(slices.Clone(family.Sidecars), family.Trees...)
			if family.Primary != "" {
				paths = append(paths, family.Primary)
			}
			for _, name := range paths {
				if !SessionSourcePath(name) || names[name] {
					return errors.New("invalid storage family path")
				}
				names[name] = true
			}
		}
	}
	return nil
}

func validStorageRef(value string) bool {
	return value != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n")
}

func validSessionSourceID(value string) bool {
	return len(value) <= 128 && SessionSourcePath(value) && !strings.Contains(value, "/")
}

func ValidateStorageRequest(manifest Manifest, method string, scope Scope, auth json.RawMessage, request StorageRequest) error {
	if !IsStorageMethod(method) || !manifest.Contract.Has(StorageSource) || manifest.Contract.Version.Minor < 8 {
		return errors.New("storage source method was not negotiated")
	}
	source, found := manifest.ResolveStorageSource(request.Lease.SourceID)
	if !found || !slices.ContainsFunc(source.Families, func(family StorageFamily) bool { return family.ID == request.Lease.FamilyID }) ||
		!validStorageRef(request.Lease.RootRef) || !validStorageRef(request.Lease.SnapshotRef) ||
		scope.Provider != "" || scope.Account != "" || scope.Generation != 0 || scope.Attempt != 0 || len(auth) != 0 {
		return errors.New("storage source cannot borrow account or foreign root authority")
	}
	limits, ceiling := request.Lease.Limits, DefaultStorageLimits()
	if limits.MaxEntries <= 0 || limits.MaxEntries > ceiling.MaxEntries ||
		limits.MaxReadBytes <= 0 || limits.MaxReadBytes > ceiling.MaxReadBytes ||
		limits.MaxParts <= 0 || limits.MaxParts > ceiling.MaxParts ||
		limits.MaxTextBytes <= 0 || limits.MaxTextBytes > ceiling.MaxTextBytes ||
		limits.MaxMediaBytes <= 0 || limits.MaxMediaBytes > ceiling.MaxMediaBytes {
		return errors.New("invalid storage source limits")
	}
	if method == MethodStorageList {
		if scope.Session != "" || request.Locator != nil || request.Mode != "" || request.Limit < 1 ||
			request.Limit > 256 || request.Cursor != "" && !validStorageRef(request.Cursor) {
			return errors.New("storage enumeration requires its source-only scope and bounded page")
		}
	} else if request.Locator == nil || request.Locator.RootRef != request.Lease.RootRef ||
		!validStorageRef(request.Locator.Key) || !validSessionSourceID(request.Locator.SessionID) ||
		request.Locator.SessionID != scope.Session || request.Cursor != "" || request.Limit != 0 ||
		request.Mode != "summary" && request.Mode != "transcript" || request.Mode == "transcript" && !source.Transcript {
		return errors.New("storage read requires its selected session and root")
	}
	return nil
}

func DecodeStorageRequest(manifest Manifest, method string, scope Scope, auth json.RawMessage, raw []byte) (StorageRequest, error) {
	var request StorageRequest
	if err := sourceJSON(raw, &request); err != nil {
		return request, err
	}
	return request, ValidateStorageRequest(manifest, method, scope, auth, request)
}

func ValidateStorageResult(request StorageRequest, result StorageResult) error {
	if result.SnapshotRef != request.Lease.SnapshotRef {
		return errors.New("storage result belongs to another snapshot")
	}
	if request.Locator == nil {
		if len(result.Entries) > request.Limit || result.Mode != "" || result.SessionID != "" || result.Summary != nil ||
			len(result.Parts) != 0 || len(result.Media) != 0 || result.NextCursor != "" && !validStorageRef(result.NextCursor) {
			return errors.New("invalid storage enumeration result")
		}
		seen := map[string]bool{}
		for _, entry := range result.Entries {
			if entry.Locator.RootRef != request.Lease.RootRef || !validSessionSourceID(entry.Locator.SessionID) ||
				!validStorageRef(entry.Locator.Key) || seen[entry.Locator.SessionID] {
				return errors.New("foreign or duplicate storage session locator")
			}
			seen[entry.Locator.SessionID] = true
			if err := validateStorageSummary(entry.Locator.SessionID, &entry.Summary); err != nil {
				return err
			}
		}
		return nil
	}
	if result.SessionID != request.Locator.SessionID || result.Mode != request.Mode ||
		len(result.Entries) != 0 || result.NextCursor != "" {
		return errors.New("storage result changed selected session")
	}
	if request.Mode == "summary" {
		if len(result.Media) != 0 || len(result.Parts) != 0 {
			return errors.New("storage summary cannot contain transcript or media")
		}
		return validateStorageSummary(result.SessionID, result.Summary)
	}
	probe := SessionSourceReadRequest{Mode: "transcript", Locator: SessionSourceLocator{SessionID: result.SessionID},
		Limits: SessionSourceLimits{MaxParts: request.Lease.Limits.MaxParts, MaxTextBytes: request.Lease.Limits.MaxTextBytes}}
	raw, err := json.Marshal(SessionSourceReadResult{Mode: result.Mode, SessionID: result.SessionID, Parts: result.Parts, Cut: result.Cut})
	if err != nil {
		return err
	}
	if _, err := DecodeSessionSourceResult(probe, raw); err != nil {
		return err
	}
	if result.Summary != nil {
		return errors.New("storage transcript cannot contain summary")
	}
	seen, total := map[string]bool{}, 0
	for _, media := range result.Media {
		total += len(media.Bytes)
		if !validStorageRef(media.ID) || seen[media.ID] || media.MIME == "" || len(media.MIME) > 256 ||
			len(media.Name) > 1024 || len(media.Bytes) > request.Lease.Limits.MaxMediaBytes ||
			total > request.Lease.Limits.MaxMediaBytes {
			return errors.New("invalid or oversized storage media")
		}
		seen[media.ID] = true
	}
	return nil
}

func DecodeStorageResult(request StorageRequest, raw []byte) (StorageResult, error) {
	var result StorageResult
	if err := sourceJSON(raw, &result); err != nil {
		return result, err
	}
	fields, err := sourceRequiredFields(raw, "snapshotRef", "cut")
	if err != nil || string(fields["cut"]) != "true" && string(fields["cut"]) != "false" {
		return result, errors.New("storage result requires explicit snapshot and cut")
	}
	return result, ValidateStorageResult(request, result)
}

func validateStorageSummary(id string, summary *SessionSourceSummary) error {
	raw, err := json.Marshal(SessionSourceReadResult{Mode: "summary", SessionID: id, Summary: summary})
	if err != nil {
		return err
	}
	_, err = DecodeSessionSourceResult(SessionSourceReadRequest{Mode: "summary", Locator: SessionSourceLocator{SessionID: id}}, raw)
	return err
}

func storageHandler(manifest Manifest, method string, handler Handler) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		request, err := DecodeStorageRequest(manifest, method, call.Scope, call.Auth, call.Params)
		if err != nil {
			return nil, &Error{Code: "invalid_params", Message: err.Error()}
		}
		result, err := handler(ctx, call, stream)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(result)
		var decoded StorageResult
		if err == nil {
			decoded, err = DecodeStorageResult(request, raw)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid storage source result: %w", err)
		}
		if err := stream.CheckStorage(ctx, request.Lease); err != nil {
			return nil, err
		}
		return decoded, nil
	}
}
