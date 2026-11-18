package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestExtensionMediaGeminiImages(t *testing.T) {
	for _, mode := range []string{"gemini_only", "gemini_dual"} {
		t.Run(mode, func(t *testing.T) {
			_, _, id := installExtensionMediaFixture(t, func(manifest *sdk.Manifest) {
				declaration := &manifest.Providers[0]
				declaration.Protocols = []protocolext.Protocol{"chat", "gemini"}
				if mode == "gemini_only" {
					declaration.Protocols = []protocolext.Protocol{"gemini"}
					declaration.Operations = nil
					declaration.Models = []sdk.Model{{ID: "fixture-picture", Kind: "image", OutputModalities: []string{"image"}}}
				}
			})
			selected, err := provider.Find(id)
			if err != nil || selected == nil {
				t.Fatalf("provider: %v", err)
			}
			listed := false
			for _, model := range Drawers(*selected) {
				listed = listed || model.ID == "fixture-picture"
			}
			if !listed {
				t.Fatal("fixture was not advertised as an image model")
			}
			server := New()
			t.Cleanup(server.idle.stop)
			response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/images/generations",
				`{"model":"`+id+`/fixture-picture","prompt":"fixture","response_format":"b64_json"}`)
			if response.Code != http.StatusOK {
				t.Fatalf("advertised %s drawing failed: HTTP %d %s", mode, response.Code, response.Body)
			}
		})
	}
}

func TestExtensionMediaTerminalAccountUsage(t *testing.T) {
	for _, test := range []struct{ path, model, field, refusal string }{
		{"images/generations", "fixture-picture", "prompt", "429"},
		{"images/generations", "fixture-picture", "prompt", "404"},
		{"videos", "fixture-movie", "prompt", "404"},
		{"embeddings", "fixture-vector", "input", "429"},
		{"audio/speech", "fixture-voice", "input", "429"},
	} {
		t.Run(test.path+"/"+test.refusal, func(t *testing.T) {
			_, _, id := installExtensionMediaFixture(t)
			server := New()
			t.Cleanup(server.idle.stop)
			response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/"+test.path,
				`{"model":"`+id+`/`+test.model+`","`+test.field+`":"terminal media `+test.refusal+`"}`)
			if response.Code != http.StatusConflict || response.Header().Get("X-Should-Retry") != "false" ||
				response.Header().Get("X-Magpie-Operation") == "" {
				t.Fatalf("terminal response: %d %s", response.Code, response.Body)
			}
			records := usage.Load(time.Time{})
			if len(records) != 1 {
				t.Fatalf("usage rows=%d, want one", len(records))
			}
			row := records[0]
			if row.ProviderAccount != "alice" || row.Input != 17 {
				t.Fatalf("terminal Alice usage lost/reassigned: account=%s input=%d status=%d", row.ProviderAccount, row.Input, row.Status)
			}
		})
	}
}

func TestExtensionMediaMultipleImagesUsage(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t, func(m *sdk.Manifest) {
		m.Providers[0].Protocols = []protocolext.Protocol{"gemini"}
		m.Providers[0].Operations = nil
		m.Providers[0].Models = []sdk.Model{{ID: "fixture-picture", Kind: "image", OutputModalities: []string{"image"}}}
	})
	server := New()
	t.Cleanup(server.idle.stop)
	response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/images/generations", `{"model":"`+id+`/fixture-picture","prompt":"normalized usage","n":2,"response_format":"b64_json"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body)
	}
	var result struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != 2 {
		t.Errorf("requested 2 independent Gemini drawings, received %d", len(result.Data))
	}
	rows := usage.Load(time.Time{})
	if len(rows) != 1 {
		t.Fatalf("usage rows=%d", len(rows))
	}
	t.Logf("images=%d input=%d output=%d account=%s", len(result.Data), rows[0].Input, rows[0].Output, rows[0].ProviderAccount)
	if len(result.Data) == 2 && (rows[0].Input != 34 || rows[0].Output != 10) {
		t.Errorf("two drawings each report input=17 output=5, ledger records input=%d output=%d", rows[0].Input, rows[0].Output)
	}
}

func TestExtensionMediaMultipleImagesSnapshots(t *testing.T) {
	for _, protocol := range []string{"gemini", "chat"} {
		for _, sample := range []struct {
			name, prompt                               string
			input, output, read, write, images, status int
		}{
			{"final", "normalized usage", 34, 10, 8, 4, 2, 200},
			{"events", "normalized usage event only", 22, 6, 4, 2, 2, 200},
			{"zero", "normalized usage zero", 0, 0, 0, 0, 2, 200},
			{"partial", "partial normalized usage", 30, 12, 4, 2, 0, 409},
			{"sparse", "rich normalized usage", 56, 12, 8, 14, 2, 200},
		} {
			t.Run(protocol+"/"+sample.name, func(t *testing.T) {
				_, _, id := installExtensionMediaFixture(t, func(m *sdk.Manifest) {
					m.Providers[0].Protocols = []protocolext.Protocol{protocolext.Protocol(protocol)}
					m.Providers[0].Operations = nil
					kind := "image"
					if protocol == "chat" {
						kind = "chat"
					}
					m.Providers[0].Models = []sdk.Model{{ID: "fixture-picture", Kind: kind, OutputModalities: []string{"text", "image"}}}
				})
				server := New()
				t.Cleanup(server.idle.stop)
				response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/images/generations", `{"model":"`+id+`/fixture-picture","prompt":"`+sample.prompt+`","n":2,"response_format":"b64_json"}`)
				if response.Code != sample.status {
					t.Fatalf("HTTP %d: %s", response.Code, response.Body)
				}
				if sample.status == 200 {
					var result struct {
						Data []json.RawMessage `json:"data"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if len(result.Data) != sample.images {
						t.Fatalf("images=%d", len(result.Data))
					}
				} else if response.Header().Get("X-Should-Retry") != "false" || response.Header().Get("X-Magpie-Operation") == "" {
					t.Fatal("lost child replay fence", response.Header())
				}
				rows := usage.Load(time.Time{})
				if len(rows) != 1 {
					t.Fatalf("ledger rows=%d", len(rows))
				}
				row := rows[0]
				if row.Input != sample.input || row.Output != sample.output || row.CacheRead != sample.read || row.CacheWrite != sample.write || row.ProviderAccount != "alice" || row.Status != sample.status {
					t.Fatalf("independent child usage lost/reassigned: %+v", row)
				}
				if sample.name == "sparse" && (row.Credits == nil || *row.Credits != 0.5 || row.ReportedCost == nil || row.ReportedCost.Amount != 0.004 || row.CacheWrite1h != 6 || row.Reasoning != 4) {
					t.Fatalf("sparse child evidence lost: %+v", row)
				}
				recent := server.Recent()
				if len(recent) != 1 || recent[0].Usage.billed().Input != row.Input || recent[0].Status != row.Status {
					t.Fatal("diagnostics disagree", recent)
				}
				for _, route := range server.Trace(t.Context(), 0, 0).Routes {
					if route.ID == row.RouteID && len(route.Tries) != 1 {
						t.Fatal("batch was replayed on another account", route)
					}
				}
			})
		}
	}
}
