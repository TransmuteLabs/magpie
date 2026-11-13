package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func sourceManifest() Manifest {
	return Manifest{Schema: 1, ID: "fixture.source", Version: "1.0.0", Runtime: "native", Entry: "plugin.so",
		Contract: Contract{Version: APIVersion{Major: 1, Minor: 6}, Capabilities: []Capability{SessionSource}},
		SessionSources: []SessionSourceDescriptor{{ID: "history", Agent: "fixture", Codec: "fixture-jsonl-v1", Container: "jsonl",
			Stores: []SessionSourceStore{{Slot: "main", HomeRelativePath: ".fixture/sessions"}},
			Layout: SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}, Transcript: true, Resume: "none"}}}
}

func TestSessionSourceContract(t *testing.T) {
	manifest := sourceManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if methods := RequiredMethods(manifest); len(methods) != 1 || methods[0] != MethodSessionSourceRead {
		t.Fatalf("source capability methods: %v", methods)
	}
	for _, mutation := range []string{"old-api", "unknown-layout", "absolute", "traversal", "root", "shell", "unknown-slot", "both-roots"} {
		t.Run(mutation, func(t *testing.T) {
			value := sourceManifest()
			source := &value.SessionSources[0]
			switch mutation {
			case "old-api":
				value.Contract.Version.Minor = 5
			case "unknown-layout":
				source.Layout.Kind = "recursive"
			case "absolute":
				source.Stores[0].HomeRelativePath = "/etc"
			case "traversal":
				source.Stores[0].HomeRelativePath = "../etc"
			case "root":
				source.Stores[0].HomeRelativePath = "."
			case "shell":
				source.Resume = "sh -c x"
			case "unknown-slot":
				source.Stores[0] = SessionSourceStore{Slot: "main", NativeRootSlot: "anything"}
			case "both-roots":
				source.Stores[0].NativeRootSlot = "omp.sessions"
			}
			if value.Validate() == nil {
				t.Fatal("invalid source declaration accepted")
			}
		})
	}
	copy := manifest.Clone()
	copy.SessionSources[0].Stores[0].HomeRelativePath = ".other"
	if manifest.SessionSources[0].Stores[0].HomeRelativePath != ".fixture/sessions" {
		t.Fatal("source stores share mutable backing")
	}
}

func TestSessionSourceFiniteLayouts(t *testing.T) {
	for _, sample := range []struct {
		layout SessionSourceLayout
		path   string
		id     string
	}{
		{SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}, "a_b.jsonl", "a_b"},
		{SessionSourceLayout{Kind: "bucketed", IDFrom: "filename-after-first-underscore"}, "bucket/time_a_b.jsonl", "a_b"},
		{SessionSourceLayout{Kind: "directory", Entry: "log.jsonl", IDFrom: "parent-directory"}, "a_b/log.jsonl", "a_b"},
	} {
		id, err := sample.layout.SessionID(sample.path)
		if err != nil || id != sample.id {
			t.Fatalf("%+v: %q %v", sample, id, err)
		}
		for _, bad := range []string{"../" + sample.path, "/" + sample.path, "extra/" + sample.path, strings.ReplaceAll(sample.path, "/", `\`)} {
			if bad == sample.path {
				continue
			}
			if _, err := sample.layout.SessionID(bad); err == nil {
				t.Fatalf("layout accepted %q", bad)
			}
		}
	}
}

func TestSessionSourceReadIdentityAndAuthority(t *testing.T) {
	manifest := sourceManifest()
	request := SessionSourceReadRequest{SourceID: "history",
		Locator:  SessionSourceLocator{StoreRef: "store", Path: "sess.jsonl", SessionID: "sess"},
		Snapshot: strings.Repeat("a", 64), Mode: "summary",
		Documents: []SessionDocument{{Path: "sess.jsonl", Format: "fixture-jsonl-v1", Body: []byte("{}\n")}},
		Limits:    SessionSourceLimits{MaxParts: 100, MaxTextBytes: 400000}}
	if err := ValidateSessionSourceRequest(manifest, request); err != nil {
		t.Fatal(err)
	}
	good := []byte(`{"mode":"summary","sessionId":"sess","summary":{"title":"hi","cwd":"/work","usage":[{"at":"2026-11-13T00:00:00Z","model":"m","input":1,"output":2,"cacheRead":0,"cacheWrite":0,"cacheWrite1h":0}]}}`)
	if _, err := DecodeSessionSourceResult(request, good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(string(good), `"sess"`, `"other"`, 1),
		strings.Replace(string(good), `"input":1`, `"input":-1`, 1),
		strings.Replace(string(good), `"input":1`, `"input":18446744073709551615`, 1),
		strings.Replace(string(good), `"cwd":"/work"`, `"cwd":"/work","cost":1`, 1),
		strings.Replace(string(good), `"mode":"summary"`, `"mode":"transcript"`, 1),
		strings.TrimSuffix(string(good), "}") + `,"resume":"sh -c x"}`,
		string(good) + "{}",
		strings.Replace(string(good), `"cwd":"/work"`, `"cwd":null`, 1),
	} {
		if _, err := DecodeSessionSourceResult(request, []byte(bad)); err == nil {
			t.Fatalf("invalid result accepted: %s", bad)
		}
	}
	request.Documents[0].Body = make([]byte, (16<<20)+1)
	if ValidateSessionSourceRequest(manifest, request) == nil {
		t.Fatal("oversized binary document accepted")
	}
	request.Documents[0].Body = []byte("{}\n")
	request.Locator.SessionID = "other"
	raw, _ := json.Marshal(request)
	if _, err := DecodeSessionSourceRequest(manifest, raw); err == nil {
		t.Fatal("host extraction mismatch accepted")
	}
}

func TestSessionSourceEmptyMeasurementsAndTranscriptWireShape(t *testing.T) {
	unknown, err := json.Marshal(SessionSourceSummary{Title: "", Cwd: ""})
	if err != nil || strings.Contains(string(unknown), `"usage"`) {
		t.Fatal("unknown usage became an empty measurement", string(unknown), err)
	}
	measured, err := json.Marshal(SessionSourceSummary{Title: "", Cwd: "", Usage: []SessionUsagePoint{}})
	if err != nil || !strings.Contains(string(measured), `"usage":[]`) {
		t.Fatal("empty measurement became unknown", string(measured), err)
	}
	request := SessionSourceReadRequest{Mode: "transcript", Locator: SessionSourceLocator{SessionID: "sess"},
		Limits: SessionSourceLimits{MaxParts: 1, MaxTextBytes: 100}}
	body, err := json.Marshal(SessionSourceReadResult{Mode: "transcript", SessionID: "sess"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSessionSourceResult(request, body); err != nil {
		t.Fatal("Go writer omitted required empty parts/cut fields", string(body), err)
	}
	if _, err := DecodeSessionSourceResult(request, []byte(`{"mode":"transcript","sessionId":"sess","parts":[]}`)); err == nil {
		t.Fatal("missing cut marker accepted")
	}
}

func TestSessionSourceRequiredResultFields(t *testing.T) {
	request := SessionSourceReadRequest{Mode: "summary", Locator: SessionSourceLocator{SessionID: "sess"},
		Limits: SessionSourceLimits{MaxParts: 10, MaxTextBytes: 1000}}
	for _, missing := range []string{"title", "cwd", "input", "output", "cacheRead", "cacheWrite", "cacheWrite1h"} {
		t.Run(missing, func(t *testing.T) {
			point := map[string]any{"at": "2026-11-13T00:00:00Z", "model": "m", "input": 0, "output": 0,
				"cacheRead": 0, "cacheWrite": 0, "cacheWrite1h": 0}
			summary := map[string]any{"title": "", "cwd": "", "usage": []any{point}}
			if missing == "title" || missing == "cwd" {
				delete(summary, missing)
			} else {
				delete(point, missing)
			}
			raw, _ := json.Marshal(map[string]any{"mode": "summary", "sessionId": "sess", "summary": summary})
			if _, err := DecodeSessionSourceResult(request, raw); err == nil {
				t.Fatal("missing required source field became an implicit zero/empty value:", missing)
			}
		})
	}
	request.Mode = "transcript"
	if _, err := DecodeSessionSourceResult(request, []byte(`{"mode":"transcript","sessionId":"sess","parts":[{"role":"user","kind":"text"}],"cut":false}`)); err == nil {
		t.Fatal("missing transcript text became an implicit empty string")
	}
}

func TestSessionSourceSharedResultCorpus(t *testing.T) {
	body, err := os.ReadFile("testdata/session-source-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name       string `json:"name"`
		Mode       string `json:"mode"`
		Valid      bool   `json:"valid"`
		UsageKnown bool   `json:"usageKnown"`
		Raw        string `json:"raw"`
	}
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	for _, sample := range cases {
		t.Run(sample.Name, func(t *testing.T) {
			request := SessionSourceReadRequest{Mode: sample.Mode, Locator: SessionSourceLocator{SessionID: "sess"},
				Limits: SessionSourceLimits{MaxParts: 10, MaxTextBytes: 1000}}
			result, err := DecodeSessionSourceResult(request, []byte(sample.Raw))
			if (err == nil) != sample.Valid {
				t.Fatal("Go/JS corpus validity mismatch", sample.Name, err)
			}
			if sample.Valid && sample.Mode == "summary" && (result.Summary.Usage != nil) != sample.UsageKnown {
				t.Fatal("missing and measured-zero usage were conflated")
			}
		})
	}
}

func TestSessionSourceFrameLimitIncludesWhitespace(t *testing.T) {
	request := SessionSourceReadRequest{Mode: "transcript", Locator: SessionSourceLocator{SessionID: "sess"},
		Limits: SessionSourceLimits{MaxParts: 1, MaxTextBytes: 100}}
	body := append(bytes.Repeat([]byte(" "), MaxFrameBytes), []byte(`{"mode":"transcript","sessionId":"sess","parts":[],"cut":false}`)...)
	if _, err := DecodeSessionSourceResult(request, body); err == nil {
		t.Fatal("encoded frame cap was bypassed by whitespace")
	}
}

func TestSessionSourceSDKRejectsBorrowedAuthorityBeforeHandler(t *testing.T) {
	manifest := sourceManifest()
	request := SessionSourceReadRequest{SourceID: "history",
		Locator:  SessionSourceLocator{StoreRef: "store", Path: "sess.jsonl", SessionID: "sess"},
		Snapshot: strings.Repeat("a", 64), Mode: "summary",
		Documents: []SessionDocument{{Path: "sess.jsonl", Format: "fixture-jsonl-v1", Body: []byte("{}\n")}},
		Limits:    SessionSourceLimits{MaxParts: 1, MaxTextBytes: 100}}
	body, _ := json.Marshal(request)
	calls := 0
	module := Module{Manifest: manifest, Handlers: map[string]Handler{MethodSessionSourceRead: func(context.Context, Call, *Stream) (any, error) {
		calls++
		return SessionSourceReadResult{Mode: "summary", SessionID: "sess", Summary: &SessionSourceSummary{Title: "", Cwd: ""}}, nil
	}}}
	handlers, _, err := module.prepare()
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []Scope{{Session: "other"}, {Session: "sess", Provider: "foreign"},
		{Session: "sess", Account: "account", Generation: 1}} {
		if _, err := handlers[MethodSessionSourceRead](t.Context(), Call{Params: body, Scope: scope}, nil); err == nil {
			t.Fatal("borrowed source authority reached handler")
		}
	}
	if calls != 0 {
		t.Fatal("invalid source calls produced business effects", calls)
	}
	if _, err := handlers[MethodSessionSourceRead](t.Context(), Call{Params: body, Scope: Scope{Session: "sess"}}, nil); err != nil || calls != 1 {
		t.Fatal("valid local source authority was rejected", err, calls)
	}
}
