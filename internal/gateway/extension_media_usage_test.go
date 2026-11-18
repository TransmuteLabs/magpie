package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

func TestExtensionMediaNormalizedUsageReachesOrdinaryAccounting(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	for _, sample := range []struct{ name, path, model, extra string }{
		{"embedding", "/v1/embeddings", "fixture-vector", `"input":"normalized usage"`},
		{"rerank", "/v1/rerank", "fixture-ranker", `"query":"normalized usage","documents":["fixture"]`},
		{"speech", "/v1/audio/speech", "fixture-voice", `"input":"normalized usage","voice":"alloy"`},
		{"image", "/v1/images/generations", "fixture-picture", `"prompt":"normalized usage","response_format":"b64_json"`},
		{"video", "/v1/videos", "fixture-movie", `"prompt":"normalized usage"`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, sample.path,
				strings.NewReader(`{"model":"`+id+`/`+sample.model+`",`+sample.extra+`}`))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			session := "normalized-" + sample.name
			request.Header.Set("X-Magpie-Session", session)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("media operation failed: %d %s", response.Code, response.Body)
			}
			var rows []usage.Record
			for _, record := range usage.Load(time.Time{}) {
				if record.Session == session {
					rows = append(rows, record)
				}
			}
			if len(rows) != 1 {
				t.Fatalf("media execution wrote %d accounting rows, want one", len(rows))
			}
			row := rows[0]
			if row.Input != 17 || row.Output != 5 || row.CacheRead != 4 || row.CacheWrite != 2 {
				t.Fatalf("normalized final media usage did not reach the ordinary ledger: %+v", row)
			}
			calls := server.Recent()
			if len(calls) == 0 || calls[0].Usage.Input != row.Input || calls[0].Usage.Output != row.Output ||
				calls[0].Usage.CacheRead != row.CacheRead || calls[0].Usage.CacheWrite != row.CacheWrite {
				t.Fatal("media request diagnostics disagree with the normalized ledger row", calls)
			}
			if row.RouteID == 0 || row.Provider != id || row.ProviderAccount != "alice" || row.Status != http.StatusOK {
				t.Fatalf("normalized usage lost its ordinary route/account identity: %+v", row)
			}
			matched := false
			for _, route := range server.Trace(t.Context(), 0, 0).Routes {
				if route.ID != row.RouteID {
					continue
				}
				matched = true
				if !route.Done || route.Status != row.Status || len(route.Usage) != 1 ||
					route.Usage[0].Input != row.Input || route.Usage[0].Output != row.Output ||
					route.Usage[0].CacheRead != row.CacheRead || route.Usage[0].CacheWrite != row.CacheWrite {
					t.Fatalf("media routing diagnostics disagree with normalized usage: %+v", route)
				}
			}
			if !matched {
				t.Fatal("media execution has no ordinary route trace")
			}
		})
	}
}

func TestExtensionAudioLateFailureKeepsUsageAndFailureAccounting(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"`+id+`/fixture-voice","input":"late normalized usage failure","voice":"alloy"}`))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Magpie-Session", "late-audio")
	response := httptest.NewRecorder()
	server.Front().ServeHTTP(response, request)
	var rows []usage.Record
	for _, record := range usage.Load(time.Time{}) {
		if record.Session == "late-audio" {
			rows = append(rows, record)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("late audio execution wrote %d accounting rows, want one", len(rows))
	}
	row := rows[0]
	if row.Status != http.StatusConflict || row.Input != 11 || row.Output != 3 || row.CacheRead != 2 || row.CacheWrite != 1 {
		t.Fatalf("late SDK audio failure was accounted as success or lost its measured usage: %+v", row)
	}
	calls := server.Recent()
	if len(calls) != 1 || calls[0].Status != http.StatusConflict || calls[0].Error == "" {
		t.Fatal("late SDK audio failure was absent from request diagnostics", calls)
	}
	expected := append([]byte{0, 255}, []byte("audio:alice")...)
	if response.Body.String() != string(expected) {
		t.Fatalf("late failure appended non-audio bytes to the streamed response: %q", response.Body.String())
	}
	for _, route := range server.Trace(t.Context(), 0, 0).Routes {
		if route.ID == row.RouteID && (route.Status != row.Status || len(route.Tries) != 1 ||
			route.Tries[0].Status != row.Status || route.Tries[0].Error == "") {
			t.Fatalf("late audio failure was absent from routing diagnostics: %+v", route)
		}
	}
}

func TestExtensionMediaNormalizedUsageSnapshotPresence(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	for _, sample := range []struct {
		input string
		want  Usage
	}{
		{"normalized usage event only", Usage{Input: 11, Output: 3, CacheRead: 2, CacheWrite: 1}},
		{"normalized usage zero", Usage{}},
	} {
		t.Run(sample.input, func(t *testing.T) {
			response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/embeddings",
				`{"model":"`+id+`/fixture-vector","input":"`+sample.input+`"}`)
			if response.Code != http.StatusOK {
				t.Fatalf("snapshot operation failed: %d %s", response.Code, response.Body)
			}
			calls := server.Recent()
			if len(calls) == 0 || calls[0].Usage.Input != sample.want.Input || calls[0].Usage.Output != sample.want.Output ||
				calls[0].Usage.CacheRead != sample.want.CacheRead || calls[0].Usage.CacheWrite != sample.want.CacheWrite {
				t.Fatalf("normalized snapshot presence was lost: %+v", calls)
			}
		})
	}
}

func TestExtensionMediaNormalizedUsageDoesNotLeakIntoRawFallback(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"data":[],"usage":{"prompt_tokens":7}}`))
	}))
	t.Cleanup(backend.Close)
	if err := provider.Save(provider.Provider{ID: "raw", Name: "Raw", Chat: backend.URL + "/v1",
		Key: "fixture", Models: []string{"fixture-vector"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "media", Name: "Media", Routing: "ordered",
		Members: []string{id + "/fixture-vector", "raw/fixture-vector"}}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/embeddings",
		`{"model":"group/media","input":"not started normalized usage"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("not-started execution prevented ordinary fallback: %d %s", response.Code, response.Body)
	}
	calls := server.Recent()
	if len(calls) != 1 || calls[0].Provider != "raw" || calls[0].Usage.Input != 7 {
		t.Fatalf("earlier extension usage was attributed to the raw fallback: %+v", calls)
	}
}
