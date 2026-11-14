package extension

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

const SessionSource Capability = "session-source"
const MethodSessionSourceRead = "session.source.read"

type SessionSourceDescriptor struct {
	ID          string                    `json:"id"`
	Agent       string                    `json:"agent"`
	Codec       string                    `json:"codec"`
	Container   string                    `json:"container"`
	Stores      []SessionSourceStore      `json:"stores"`
	Layout      SessionSourceLayout       `json:"layout"`
	Transcript  bool                      `json:"transcript"`
	Resume      string                    `json:"resume"`
	Descendants *SessionSourceDescendants `json:"descendants,omitempty"`
}

type SessionSourceDescendants struct {
	Kind               string   `json:"kind"`
	ExcludeDirectories []string `json:"excludeDirectories,omitempty"`
}

type SessionSourceIdentity struct {
	Path      string `json:"path"`
	SessionID string `json:"sessionId"`
}

type SessionSourceStore struct {
	Slot             string `json:"slot"`
	NativeRootSlot   string `json:"nativeRootSlot,omitempty"`
	HomeRelativePath string `json:"homeRelativePath,omitempty"`
}

type SessionSourceLayout struct {
	Kind   string `json:"kind"`
	IDFrom string `json:"idFrom"`
	Entry  string `json:"entry,omitempty"`
}

type SessionSourceLocator struct {
	StoreRef  string `json:"storeRef"`
	Path      string `json:"path"`
	SessionID string `json:"sessionId"`
}

type SessionSourceLimits struct {
	MaxParts     int `json:"maxParts"`
	MaxTextBytes int `json:"maxTextBytes"`
}

type SessionSourceReadRequest struct {
	SourceID    string               `json:"sourceId"`
	Locator     SessionSourceLocator `json:"locator"`
	Snapshot    string               `json:"snapshot"`
	Mode        string               `json:"mode"`
	Documents   []SessionDocument    `json:"documents"`
	Limits      SessionSourceLimits  `json:"limits"`
	Descendants []string             `json:"descendants,omitempty"`
}

type SessionSourceReadResult struct {
	Mode      string                `json:"mode"`
	SessionID string                `json:"sessionId"`
	Summary   *SessionSourceSummary `json:"summary,omitempty"`
	Parts     []SessionSourcePart   `json:"parts,omitempty"`
	Cut       bool                  `json:"cut,omitempty"`
}

type SessionSourceSummary struct {
	Title    string                  `json:"title"`
	Cwd      string                  `json:"cwd"`
	Start    string                  `json:"start,omitempty"`
	Last     string                  `json:"last,omitempty"`
	ParentID string                  `json:"parentId,omitempty"`
	RootID   string                  `json:"rootId,omitempty"`
	Usage    []SessionUsagePoint     `json:"usage,omitempty"`
	Subs     []SessionSourceIdentity `json:"subs,omitempty"`
}

type SessionUsagePoint struct {
	At           string `json:"at"`
	Model        string `json:"model"`
	Input        uint64 `json:"input"`
	Output       uint64 `json:"output"`
	CacheRead    uint64 `json:"cacheRead"`
	CacheWrite   uint64 `json:"cacheWrite"`
	CacheWrite1h uint64 `json:"cacheWrite1h"`
}

type SessionSourcePart struct {
	Role  string `json:"role"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Name  string `json:"name,omitempty"`
	Model string `json:"model,omitempty"`
	Cut   int    `json:"cut,omitempty"`
}

func SessionSourcePath(name string) bool {
	return fs.ValidPath(name) && name != "." && len(name) <= 1024 &&
		!strings.ContainsAny(name, "\\:\x00\r\n") &&
		!strings.ContainsFunc(name, unicode.IsControl) &&
		!strings.Contains(strings.ToLower(name), ".magpie-extension")
}

func (source *SessionSourceDescriptor) UnmarshalJSON(raw []byte) error {
	type descriptor SessionSourceDescriptor
	var value descriptor
	if err := sourceJSON(raw, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var transcript *bool
	if err := json.Unmarshal(fields["transcript"], &transcript); err != nil || transcript == nil {
		return errors.New("session source requires an explicit transcript flag")
	}
	*source = SessionSourceDescriptor(value)
	return nil
}

func (summary SessionSourceSummary) MarshalJSON() ([]byte, error) {
	type plain SessionSourceSummary
	var usage *[]SessionUsagePoint
	if summary.Usage != nil {
		usage = &summary.Usage
	}
	return json.Marshal(struct {
		plain
		Usage *[]SessionUsagePoint `json:"usage,omitempty"`
	}{plain: plain(summary), Usage: usage})
}

func (result SessionSourceReadResult) MarshalJSON() ([]byte, error) {
	if result.Mode == "transcript" {
		parts := result.Parts
		if parts == nil {
			parts = []SessionSourcePart{}
		}
		return json.Marshal(struct {
			Mode      string              `json:"mode"`
			SessionID string              `json:"sessionId"`
			Parts     []SessionSourcePart `json:"parts"`
			Cut       bool                `json:"cut"`
		}{result.Mode, result.SessionID, parts, result.Cut})
	}
	type plain SessionSourceReadResult
	return json.Marshal(plain(result))
}

func (layout SessionSourceLayout) SessionID(name string) (string, error) {
	if !SessionSourcePath(name) {
		return "", errors.New("invalid relative session locator")
	}
	parts := strings.Split(name, "/")
	var id string
	switch {
	case layout.Kind == "flat" && layout.IDFrom == "filename-stem" && layout.Entry == "" && len(parts) == 1:
		id, _ = strings.CutSuffix(parts[0], ".jsonl")
		if id == parts[0] {
			id = ""
		}
	case layout.Kind == "bucketed" && layout.IDFrom == "filename-after-first-underscore" && layout.Entry == "" && len(parts) == 2:
		if stem, ok := strings.CutSuffix(parts[1], ".jsonl"); ok {
			_, id, _ = strings.Cut(stem, "_")
		}
	case layout.Kind == "directory" && layout.IDFrom == "parent-directory" && layout.Entry == "log.jsonl" && len(parts) == 2 && parts[1] == layout.Entry:
		id = parts[0]
	}
	if id == "" || len(id) > 128 {
		return "", errors.New("session locator does not match its finite layout")
	}
	for _, character := range id {
		if character < 'a' || character > 'z' {
			if character < 'A' || character > 'Z' {
				if character < '0' || character > '9' {
					if character != '-' && character != '_' {
						return "", errors.New("invalid session identity")
					}
				}
			}
		}
	}
	return id, nil
}

func (manifest Manifest) validateSessionSources() error {
	if len(manifest.SessionSources) == 0 {
		if manifest.Contract.Has(SessionSource) {
			return errors.New("session-source requires a source descriptor")
		}
		return nil
	}
	if !manifest.Contract.Has(SessionSource) || manifest.Contract.Version.Minor < 6 || len(manifest.SessionSources) > 128 {
		return errors.New("session sources require SDK API 1.6 and session-source")
	}
	seen := map[string]bool{}
	for _, source := range manifest.SessionSources {
		if !validID(source.ID) || !validID(source.Agent) || !validID(source.Codec) || seen[source.ID] ||
			source.Container != "jsonl" || source.Resume != "none" && source.Resume != "native" ||
			len(source.Stores) == 0 || len(source.Stores) > 16 {
			return errors.New("invalid session source descriptor")
		}
		seen[source.ID] = true
		probe := map[string]string{"flat": "id.jsonl", "bucketed": "bucket/time_id.jsonl", "directory": "id/log.jsonl"}
		if _, err := source.Layout.SessionID(probe[source.Layout.Kind]); err != nil {
			return err
		}
		if source.Descendants != nil {
			if manifest.Contract.Version.Minor < 9 || source.Layout.Kind == "directory" {
				return errors.New("session source descendants require SDK API 1.9 and a journal filename layout")
			}
			if err := validateSessionSourceDescendants(source.Descendants); err != nil {
				return err
			}
		}
		slots := map[string]bool{}
		for _, store := range source.Stores {
			if !validID(store.Slot) || slots[store.Slot] || (store.NativeRootSlot == "") == (store.HomeRelativePath == "") {
				return errors.New("invalid session source store")
			}
			slots[store.Slot] = true
			if store.HomeRelativePath != "" && (!validHomePath(store.HomeRelativePath) || strings.ContainsAny(store.HomeRelativePath, "~$")) {
				return errors.New("session source root must be an exact HOME-relative directory")
			}
			switch store.NativeRootSlot {
			case "", "claude.projects", "codex.sessions", "codex.archived-sessions", "omp.sessions", "devin.file-sessions":
			default:
				return errors.New("unknown native session root slot")
			}
		}
	}
	return nil
}

func (manifest Manifest) ResolveSessionSource(id string) (SessionSourceDescriptor, bool) {
	for _, source := range manifest.SessionSources {
		if source.ID == id {
			return source, true
		}
	}
	return SessionSourceDescriptor{}, false
}

func sourceJSON(raw []byte, target any) error {
	if len(raw) > MaxFrameBytes-4096 {
		return errors.New("session source payload exceeds frame budget")
	}
	for index := 0; index < len(raw); index++ {
		if raw[index] == '"' {
			index++
			for index < len(raw) && raw[index] != '"' {
				if raw[index] == '\\' {
					index++
				}
				index++
			}
		} else if bytes.HasPrefix(raw[index:], []byte("null")) {
			return errors.New("session source fields cannot be null")
		}
	}
	if err := sourceWireShape(raw, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid trailing session source payload")
	}
	return nil
}

func DecodeSessionSourceRequest(manifest Manifest, raw []byte) (SessionSourceReadRequest, error) {
	var request SessionSourceReadRequest
	if err := sourceJSON(raw, &request); err != nil {
		return request, err
	}
	return request, ValidateSessionSourceRequest(manifest, request)
}

func ValidateSessionSourceRequest(manifest Manifest, request SessionSourceReadRequest) error {
	source, ok := manifest.ResolveSessionSource(request.SourceID)
	if !ok || request.Mode != "summary" && request.Mode != "transcript" || request.Mode == "transcript" && !source.Transcript {
		return errors.New("undeclared source or unsupported source read mode")
	}
	id, err := source.Layout.SessionID(request.Locator.Path)
	digest, digestErr := hex.DecodeString(request.Snapshot)
	if err != nil || id != request.Locator.SessionID || request.Locator.StoreRef == "" || len(request.Locator.StoreRef) > 128 ||
		digestErr != nil || len(digest) != 32 || strings.ToLower(request.Snapshot) != request.Snapshot ||
		request.Limits.MaxParts < 1 || request.Limits.MaxParts > 10000 || request.Limits.MaxTextBytes < 1 || request.Limits.MaxTextBytes > 400000 ||
		len(request.Documents) == 0 || len(request.Documents) > 128 {
		return errors.New("invalid session source selection or limits")
	}
	seen := map[string]bool{}
	total := 0
	for index, document := range request.Documents {
		if !SessionSourcePath(document.Path) || seen[document.Path] || document.Format != source.Codec || document.Body == nil ||
			index == 0 && document.Path != request.Locator.Path {
			return errors.New("invalid session source document")
		}
		seen[document.Path] = true
		total += len(document.Body)
		if total > 16<<20 {
			return errors.New("session source binary budget exceeded")
		}
	}
	if err := validateSessionSourceDescendantRequest(manifest, source, request, seen); err != nil {
		return err
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > MaxFrameBytes-4096 {
		return errors.New("session source encoded frame budget exceeded")
	}
	return nil
}

func DecodeSessionSourceResult(request SessionSourceReadRequest, raw []byte) (SessionSourceReadResult, error) {
	var result SessionSourceReadResult
	if err := sourceJSON(raw, &result); err != nil {
		return result, err
	}
	if result.SessionID != request.Locator.SessionID || result.Mode != request.Mode {
		return result, errors.New("session source result changed its selected identity or mode")
	}
	if err := sourceResultRequiredFields(request.Mode, raw); err != nil {
		return result, err
	}
	if request.Mode == "summary" {
		if result.Summary == nil || result.Parts != nil || result.Cut {
			return result, errors.New("invalid source summary result")
		}
		summary := result.Summary
		if err := validateSessionSourceIdentities(request, summary.Subs); err != nil {
			return result, err
		}
		if len(summary.Title) > 4096 || len(summary.Cwd) > 4096 || strings.ContainsRune(summary.Cwd, 0) ||
			len(summary.Usage) > 10000 || len(summary.ParentID) > 128 || len(summary.RootID) > 128 {
			return result, errors.New("source summary budget exceeded")
		}
		for _, stamp := range []string{summary.Start, summary.Last} {
			if stamp != "" {
				if !validSourceTimestamp(stamp) {
					return result, errors.New("invalid source timestamp")
				}
			}
		}
		var total uint64
		for _, point := range summary.Usage {
			if !validSourceTimestamp(point.At) || point.Model == "" || len(point.Model) > 300 {
				return result, errors.New("invalid source usage time or model")
			}
			if point.CacheWrite1h > point.CacheWrite {
				return result, errors.New("one-hour cache writes exceed total cache writes")
			}
			for _, value := range []uint64{point.Input, point.Output, point.CacheRead, point.CacheWrite, point.CacheWrite1h} {
				if value > math.MaxInt64-total {
					return result, errors.New("source usage counter overflow")
				}
				total += value
			}
		}
		return result, nil
	}
	if request.Mode != "transcript" || result.Summary != nil || len(result.Parts) > request.Limits.MaxParts {
		return result, errors.New("invalid source transcript result")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields["parts"] == nil || fields["cut"] == nil {
		return result, errors.New("source transcript requires explicit parts and cut")
	}
	total := 0
	for _, part := range result.Parts {
		if part.Role != "user" && part.Role != "assistant" && part.Role != "tool" || part.Cut < 0 || part.Cut > MaxNativeBodyBytes || len(part.Name) > 1024 || len(part.Model) > 300 {
			return result, errors.New("invalid source transcript part")
		}
		switch part.Kind {
		case "text", "thinking", "tool_use", "tool_result", "context", "image":
		default:
			return result, errors.New("unknown source transcript kind")
		}
		total += len(part.Text)
		if total > request.Limits.MaxTextBytes {
			return result, errors.New("source transcript text budget exceeded")
		}
	}
	return result, nil
}

func sourceRequiredFields(raw []byte, required ...string) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	for _, field := range required {
		if _, exists := value[field]; !exists {
			return nil, fmt.Errorf("missing required session source field %s", field)
		}
	}
	return value, nil
}

func sourceResultRequiredFields(mode string, raw []byte) error {
	root, err := sourceRequiredFields(raw, "mode", "sessionId")
	if err != nil {
		return err
	}
	if mode == "summary" {
		if root["parts"] != nil || root["cut"] != nil {
			return errors.New("summary result contains transcript fields")
		}
		summary, err := sourceRequiredFields(root["summary"], "title", "cwd")
		if err != nil {
			return err
		}
		if usage, exists := summary["usage"]; exists {
			var points []json.RawMessage
			if err := json.Unmarshal(usage, &points); err != nil {
				return err
			}
			for _, point := range points {
				if _, err := sourceRequiredFields(point, "at", "model", "input", "output", "cacheRead", "cacheWrite", "cacheWrite1h"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if _, err := sourceRequiredFields(raw, "parts", "cut"); err != nil {
		return err
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(root["parts"], &parts); err != nil {
		return err
	}
	for _, part := range parts {
		if _, err := sourceRequiredFields(part, "role", "kind", "text"); err != nil {
			return err
		}
	}
	return nil
}

var sourceTimestampPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)
})

func validSourceTimestamp(value string) bool {
	if !sourceTimestampPattern().MatchString(value) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func sourceWireShape(raw json.RawMessage, shape reflect.Type) error {
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	switch shape.Kind() {
	case reflect.Struct:
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		fields := map[string]reflect.Type{}
		for index := 0; index < shape.NumField(); index++ {
			field := shape.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.PkgPath == "" && name != "-" {
				if name == "" {
					name = field.Name
				}
				fields[name] = field.Type
			}
		}
		for name, body := range value {
			field, exists := fields[name]
			if !exists {
				return fmt.Errorf("unknown session source field %s", name)
			}
			if err := sourceWireShape(body, field); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if shape.Elem().Kind() == reflect.Uint8 {
			var encoded string
			if err := json.Unmarshal(raw, &encoded); err != nil {
				return err
			}
			body, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || base64.StdEncoding.EncodeToString(body) != encoded {
				return errors.New("source body requires canonical base64")
			}
			return nil
		}
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := sourceWireShape(value, shape.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
