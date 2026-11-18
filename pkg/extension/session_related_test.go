package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

func relatedFixture() (Manifest, SessionRequest) {
	source := func(id string, body []byte, first, last string) SessionInputProvenance {
		digest := sha256.Sum256(body)
		hash := hex.EncodeToString(digest[:])
		return SessionInputProvenance{Path: "sessions/2026/11/15/rollout-" + id + ".jsonl",
			RolloutID: id, Encoding: "jsonl", RawBytes: "6", DecodedBytes: "6",
			RawSHA256: hash, DecodedSHA256: hash, PrefixSHA256: hash,
			ByteStart: "0", ByteEndExclusive: "6", OrdinalStart: first, OrdinalEndExclusive: last}
	}
	child, parent := []byte("child\n"), []byte("prior\n")
	return Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 10},
			Capabilities: []Capability{SessionMigration, SessionRelatedInputs}}},
		SessionRequest{ID: "convert", From: "codex", To: "target",
			Files: []SessionDocument{{Path: "selected.jsonl", Format: "codex", Body: child}},
			Related: &SessionRelatedInputSet{Version: 1, Kind: CodexRolloutLineage,
				Selected: source("22222222-2222-4222-8222-222222222222", child, "9007199254740993", "18446744073709551615"),
				Ancestors: []SessionRelatedInput{{Body: parent,
					Source: source("11111111-1111-4111-8111-111111111111", parent, "0", "9007199254740993")}}}}
}

func TestSessionRelatedInputsKeepUint64RangesExact(test *testing.T) {
	manifest, request := relatedFixture()
	if err := ValidateSessionRelatedInputs(manifest, request); err != nil {
		test.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil || !strings.Contains(string(body), `"ordinalStart":"9007199254740993"`) ||
		!strings.Contains(string(body), `"ordinalEndExclusive":"18446744073709551615"`) {
		test.Fatal("uint64 lineage ranges lost their canonical string representation", err)
	}
	copy := request.Related.Clone()
	copy.Ancestors[0].Body[0] = 'x'
	if string(request.Related.Ancestors[0].Body) != "prior\n" {
		test.Fatal("cloned related inputs alias the captured bytes")
	}
}

func TestSessionRelatedInputsRejectInvalidAuthorityAndProvenance(test *testing.T) {
	for _, problem := range []string{"version", "kind", "capability", "api", "migration", "format", "multiple-selected",
		"path", "foreign-root", "encoding", "id", "duplicate", "missing", "noncanonical", "overflow",
		"numeric-range", "offset", "partial-selected", "digest", "raw-digest", "range-order", "body", "depth"} {
		test.Run(problem, func(test *testing.T) {
			manifest, request := relatedFixture()
			source := &request.Related.Selected
			switch problem {
			case "version":
				request.Related.Version = 2
			case "kind":
				request.Related.Kind = "filesystem-tree"
			case "capability":
				manifest.Contract.Capabilities = []Capability{SessionMigration}
			case "api":
				manifest.Contract.Version.Minor = 9
			case "migration":
				manifest.Contract.Capabilities = []Capability{SessionRelatedInputs}
			case "format":
				request.From = "other"
			case "multiple-selected":
				request.Files = append(request.Files, request.Files[0])
			case "path":
				source.Path = "../sessions/parent.jsonl"
			case "foreign-root":
				source.Path = "credentials/parent.jsonl"
			case "encoding":
				source.Encoding = "zip"
			case "id":
				source.RolloutID = "logical-owner"
			case "duplicate":
				request.Related.Ancestors[0].Source.RolloutID = source.RolloutID
			case "missing":
				request.Related.Ancestors = nil
			case "noncanonical":
				source.OrdinalStart = "09007199254740993"
			case "overflow":
				source.OrdinalEndExclusive = "18446744073709551616"
			case "numeric-range":
				body, _ := json.Marshal(request)
				body = []byte(strings.Replace(string(body), `"ordinalStart":"9007199254740993"`, `"ordinalStart":9007199254740993`, 1))
				if json.Unmarshal(body, &request) == nil {
					test.Fatal("numeric JSON range could lose uint64 precision in JS")
				}
				return
			case "offset":
				source.ByteStart = "1"
			case "partial-selected":
				source.DecodedBytes = "7"
			case "digest":
				source.PrefixSHA256 = strings.Repeat("F", 64)
			case "raw-digest":
				source.RawSHA256 = strings.Repeat("f", 64)
			case "range-order":
				source.OrdinalStart = source.OrdinalEndExclusive
			case "body":
				request.Related.Ancestors[0].Body[0] = 'x'
			case "depth":
				request.Related.Ancestors = make([]SessionRelatedInput, 32)
			}
			if err := ValidateSessionRelatedInputs(manifest, request); err == nil {
				test.Fatal("invalid related capture was admitted", problem)
			}
		})
	}
}

func TestSessionRelatedInputsBoundFullRawAndDecodedCaptures(test *testing.T) {
	for _, budget := range []string{"raw", "decoded"} {
		test.Run(budget, func(test *testing.T) {
			manifest, request := relatedFixture()
			source := &request.Related.Ancestors[0].Source
			source.Encoding = "jsonl.zst"
			source.Path += ".zst"
			if budget == "raw" {
				source.RawBytes = "16777216"
			} else {
				source.DecodedBytes = "16777216"
			}
			if err := ValidateSessionRelatedInputs(manifest, request); err == nil {
				test.Fatal("small supplied prefix bypassed the aggregate original capture budget", budget)
			}
		})
	}
}

func TestSessionRelatedPublicHandlerRejectsInvalidCarrierBeforeEffects(test *testing.T) {
	for _, problem := range []string{"capability", "digest", "range", "shape"} {
		test.Run(problem, func(test *testing.T) {
			contract, request := relatedFixture()
			manifest := testManifest(SessionMigration, SessionRelatedInputs)
			manifest.Contract = contract.Contract
			manifest.Sessions = []SessionDescriptor{{ID: request.ID, Name: "Converter"}}
			switch problem {
			case "capability":
				manifest.Contract.Capabilities = []Capability{SessionMigration}
			case "digest":
				request.Related.Ancestors[0].Body[0] = 'x'
			case "range":
				request.Related.Selected.ByteStart = "1"
			case "shape":
				request.Related.Kind = "filesystem-tree"
			}
			calls := 0
			module := Module{Manifest: manifest, Handlers: map[string]Handler{
				MethodSessionConvert: func(context.Context, Call, *Stream) (any, error) {
					calls++
					return SessionResult{}, nil
				},
			}}
			handlers, _, err := module.prepare()
			if err != nil {
				test.Fatal(err)
			}
			body, err := json.Marshal(request)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := handlers[MethodSessionConvert](test.Context(), Call{Params: body}, nil); err == nil {
				test.Fatal("invalid related carrier reached the public Go conversion handler")
			}
			if calls != 0 {
				test.Fatal("invalid related carrier produced business effects", calls)
			}
		})
	}
}

func TestSessionRelatedPublicHandlerKeepsLegacyAndCheckedInputs(test *testing.T) {
	for _, related := range []bool{false, true} {
		contract, request := relatedFixture()
		manifest := testManifest(SessionMigration, SessionRelatedInputs)
		manifest.Contract = contract.Contract
		manifest.Sessions = []SessionDescriptor{{ID: request.ID, Name: "Converter"}}
		if !related {
			request.Related = nil
			manifest.Contract.Version.Minor = 0
			manifest.Contract.Capabilities = []Capability{SessionMigration}
		}
		calls := 0
		module := Module{Manifest: manifest, Handlers: map[string]Handler{
			MethodSessionConvert: func(_ context.Context, call Call, _ *Stream) (any, error) {
				calls++
				var actual SessionRequest
				if err := json.Unmarshal(call.Params, &actual); err != nil {
					return nil, err
				}
				if string(actual.Files[0].Body) != "child\n" || (actual.Related != nil) != related {
					test.Fatal("public wrapper changed supplied captures or legacy payload")
				}
				return SessionResult{ID: "converted"}, nil
			},
		}}
		handlers, _, err := module.prepare()
		if err != nil {
			test.Fatal(err)
		}
		body, err := json.Marshal(request)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := handlers[MethodSessionConvert](test.Context(), Call{Params: body}, nil); err != nil || calls != 1 {
			test.Fatal("public Go conversion handler refused an admitted request", related, err, calls)
		}
	}
}

func TestSessionRelatedServeValidatesBeforeConversion(test *testing.T) {
	for _, problem := range []string{"checked", "legacy", "undeclared", "digest", "numeric-range"} {
		test.Run(problem, func(test *testing.T) {
			contract, request := relatedFixture()
			manifest := testManifest(SessionMigration, SessionRelatedInputs)
			manifest.Contract = contract.Contract
			manifest.Sessions = []SessionDescriptor{{ID: request.ID, Name: "Converter"}}
			switch problem {
			case "legacy":
				request.Related = nil
				manifest.Contract.Version.Minor = 0
				manifest.Contract.Capabilities = []Capability{SessionMigration}
			case "undeclared":
				manifest.Contract.Capabilities = []Capability{SessionMigration}
			case "digest":
				request.Related.Ancestors[0].Body[0] = 'x'
			}
			var calls atomic.Int64
			peer := startPeer(test, Module{Manifest: manifest, Handlers: map[string]Handler{
				MethodSessionConvert: func(context.Context, Call, *Stream) (any, error) {
					calls.Add(1)
					return SessionResult{ID: "converted"}, nil
				},
			}})
			if manifest.Contract.Version.Minor >= 1 {
				configurePeer(test, peer, manifest, "0")
			}
			frame := callFrame("convert", MethodSessionConvert, request)
			if problem == "numeric-range" {
				frame.Params = []byte(strings.Replace(string(frame.Params),
					`"ordinalStart":"9007199254740993"`, `"ordinalStart":9007199254740993`, 1))
			}
			peer.send(frame)
			result := peer.read()
			if result.ID != frame.ID || result.Kind != KindResult {
				test.Fatal("session conversion lost its public wire identity")
			}
			if problem == "checked" || problem == "legacy" {
				if result.Error != nil || calls.Load() != 1 {
					test.Fatal("Serve refused admitted session inputs", problem, result.Error, calls.Load())
				}
			} else if result.Error == nil || result.Error.Code != "invalid_params" || calls.Load() != 0 {
				test.Fatal("Serve admitted unchecked related inputs or produced business effects", problem, result.Error, calls.Load())
			}
		})
	}
}
