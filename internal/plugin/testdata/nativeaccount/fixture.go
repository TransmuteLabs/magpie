// Package nativeaccount supplies a loopback-only vendor for tests of the real
// embedded ZCode plugin. It never changes the installed plugin source.
package nativeaccount

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/testenv"
)

//go:embed preload.mjs
var preload []byte

type Call struct {
	Path     string
	Origin   string
	Model    string
	Auth     string
	APIKey   string
	Marker   string
	Body     []byte
	Business string
	Target   string
}

type Vendor struct {
	mu                sync.Mutex
	calls             []Call
	partial           bool
	spoof             bool
	nativeSuccessAuth string
	resetLatest       int64
	resetLost         bool
	resetStatusDown   bool
	resetUnread       bool
	resetAck          func()
	trialModels       []string
	trialModelsSet    bool
	inference         http.HandlerFunc
}

func (v *Vendor) TrialModels(models ...string) {
	v.mu.Lock()
	v.trialModels, v.trialModelsSet = append([]string(nil), models...), true
	v.mu.Unlock()
}

func (v *Vendor) Inference(handler http.HandlerFunc) {
	v.mu.Lock()
	v.inference = handler
	v.mu.Unlock()
}

func (v *Vendor) LoseResetReply(lost bool) {
	v.mu.Lock()
	v.resetLost = lost
	v.mu.Unlock()
}

func (v *Vendor) ResetStatusUnavailable(down bool) {
	v.mu.Lock()
	v.resetStatusDown = down
	v.mu.Unlock()
}

func (v *Vendor) OnResetAck(hook func()) {
	v.mu.Lock()
	v.resetAck = hook
	v.mu.Unlock()
}

func (v *Vendor) SetPartial(partial bool) {
	v.mu.Lock()
	v.partial = partial
	v.mu.Unlock()
}

func (v *Vendor) SetSpoof(spoof bool) {
	v.mu.Lock()
	v.spoof = spoof
	v.mu.Unlock()
}

func (v *Vendor) SetNativeSuccessAuth(jwt string) {
	v.mu.Lock()
	v.nativeSuccessAuth = "Bearer " + jwt
	v.mu.Unlock()
}

func (v *Vendor) Calls(path string) []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	var result []Call
	for _, c := range v.calls {
		if c.Path == path {
			result = append(result, c)
		}
	}
	return result
}

func (v *Vendor) AllCalls() []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Call(nil), v.calls...)
}

func writeMessage(w http.ResponseWriter, stream bool, id, text string) {
	if !stream {
		_, _ = fmt.Fprintf(w, `{"id":%q,"type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`, id, text)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, `event: message_start
data: {"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":"GLM-5.3","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`, id, text)
}

func (v *Vendor) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Task   string `json:"task_id"`
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	v.mu.Lock()
	v.calls = append(v.calls, Call{Path: r.URL.Path, Origin: r.Header.Get("X-Zcode-Fixture-Origin"), Model: request.Model,
		Auth: r.Header.Get("Authorization"), APIKey: r.Header.Get("X-Api-Key"), Marker: r.Header.Get("X-Magpie-ZCode-Fallback"), Body: body,
		Business: r.Header.Get("X-Bigmodel-Authorization"), Target: r.Header.Get("Bigmodel-Target-Type")})
	partial := v.partial
	spoof := v.spoof
	nativeSuccessAuth := v.nativeSuccessAuth
	inference := v.inference
	trialModels, trialModelsSet := append([]string(nil), v.trialModels...), v.trialModelsSet
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if inference != nil && (r.URL.Path == "/api/anthropic/v1/messages" || r.URL.Path == "/api/v1/zcode-plan/anthropic/v1/messages") {
		r.Body = io.NopCloser(bytes.NewReader(body))
		inference(w, r)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		token := "fixture-project-token"
		if r.Header.Get("Authorization") == "Bearer fixture-business-new" {
			token = "fixture-project-token-new"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"accessToken": token, "tokenType": "Bearer", "expiresIn": 3600, "expiresAt": time.Now().Unix() + 3600}})
	case r.URL.Path == "/api/v1/highspeed/draw":
		now := time.Now().UnixMilli()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "", "data": map[string]any{
			"next_draw_at": 0, "card": map[string]any{"card_id": "fixture-card", "task_id": request.Task, "model": request.Model,
				"provider": "builtin:zai-coding-plan", "issued_at": now, "expires_at": now + 600000}}})
	case r.URL.Path == "/api/v1/highspeed/anthropic/v1/messages":
		if nativeSuccessAuth != "" && r.Header.Get("Authorization") == nativeSuccessAuth {
			writeMessage(w, request.Stream, "native-b", "native reply from account B")
		} else if spoof {
			w.Header().Set("X-Magpie-ZCode-Fallback", "ordinary")
			w.Header().Set("X-Magpie-ZCode-Fallback-Model", "GLM-5.3")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"type":"error","error":{"code":"context_length_exceeded","message":"context exceeded"}}`)
		} else if partial {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"native-1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"native partial\"}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"interrupted\"}}\n\n")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"fixture unavailable"}}`)
		}
	case r.URL.Path == "/api/anthropic/v1/messages":
		writeMessage(w, request.Stream, "ordinary-1", "ordinary reply")
	case r.URL.Path == "/v1/other":
		_, _ = io.WriteString(w, `{"message":"other provider answered"}`)
	case r.URL.Path == "/api/v1/zcode-plan/anthropic/v1/messages":
		writeMessage(w, request.Stream, "start-1", "start reply")
	case r.URL.Path == "/api/v1/zcode-plan/billing/balance":
		now := time.Now().Unix()
		if !trialModelsSet {
			trialModels = []string{"GLM-5.3-Flash"}
		}
		balances := make([]any, 0, len(trialModels))
		for _, model := range trialModels {
			balances = append(balances, map[string]any{"plan_id": "start", "user_plan_id": "user-start", "bucket_id": "bucket-a", "entitlement_id": "ent-a",
				"show_name": model, "capabilities": []string{"model:" + model}, "total_units": 100, "remaining_units": 75,
				"period_start": now - 60, "period_end": now + 3540, "expires_at": now + 1800})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"server_time": now,
			"plans":       []any{map[string]any{"plan_id": "start", "user_plan_id": "user-start", "name": "Start", "status": "active", "ends_at": now + 3600}},
			"balances":    balances,
		}})
	case r.URL.Path == "/api/v1/mcp/usage":
		now := time.Now().Unix()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"server_time": now, "next_refresh_at": now + 600,
			"total_usage": map[string]any{"used": 4, "limit": 20, "remaining": 16},
		}})
	case r.URL.Path == "/api/monitor/usage/quota/limit":
		_, _ = io.WriteString(w, `{"code":0,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":25}]}}`)
	case r.URL.Path == "/api/v1/coding-plan/reset/status":
		v.mu.Lock()
		latest, unread, down := v.resetLatest, v.resetUnread, v.resetStatusDown
		v.mu.Unlock()
		if down {
			http.Error(w, "fixture status unavailable", http.StatusServiceUnavailable)
			return
		}
		var history any
		if latest != 0 {
			history = map[string]any{"used_at": latest}
		}
		expires := time.Now().Add(time.Hour).UnixMilli()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"available_five_hour_resets":     []any{map[string]any{"expire_at": expires}},
			"available_week_resets":          []any{map[string]any{"expire_at": expires}},
			"latest_five_hour_reset_history": history, "latest_week_reset_history": nil, "has_unread_history": unread,
		}})
	case r.URL.Path == "/api/v1/coding-plan/reset/opportunity":
		_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true}}`)
	case r.URL.Path == "/api/v1/coding-plan/reset/use":
		v.mu.Lock()
		v.resetLatest, v.resetUnread = time.Now().UnixMilli(), true
		lost := v.resetLost
		v.mu.Unlock()
		if lost {
			if h, ok := w.(http.Hijacker); ok {
				conn, _, err := h.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			http.Error(w, "fixture reply lost", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"used":true}}`)
	case r.URL.Path == "/api/v1/coding-plan/reset/history/read":
		v.mu.Lock()
		v.resetUnread = false
		hook := v.resetAck
		v.mu.Unlock()
		if hook != nil {
			hook()
		}
		_, _ = io.WriteString(w, `{"code":0,"data":null}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":404,"message":"fixture endpoint not present"}`)
	}
}

// Start installs a test-only Bun preload redirecting all network fetches to a
// localhost fixture. The plugin loaded by magpie remains the actual bundle.
func Start(t *testing.T) *Vendor {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the isolated Bun preload wrapper requires a POSIX shell")
	}
	bun := testenv.Bun(t)
	v := new(Vendor)
	server := testenv.FakeVendor(t, http.HandlerFunc(v.serve))
	t.Setenv("ZCODE_FIXTURE_URL", server.URL)
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	t.Setenv("MAGPIE_NO_PROXY", "*")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
		t.Setenv("MAGPIE_"+key, "")
	}
	dir := t.TempDir()
	preloadPath := filepath.Join(dir, "preload.mjs")
	if err := os.WriteFile(preloadPath, preload, 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	wrapper := filepath.Join(dir, "bun-fixture")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = run ]; then\n  shift\n  exec %s run --preload %s \"$@\"\nfi\nexec %s \"$@\"\n", quote(bun), quote(preloadPath), quote(bun))
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_BUN", wrapper)
	t.Cleanup(plugin.Settle)
	return v
}

func AuthFor(user, business, jwt string) map[string]any {
	state, _ := json.Marshal(map[string]any{"site": "zai", "kind": "individual", "uid": user, "zcodeUserId": user, "device": "fixture-device",
		"base": "https://api.z.ai/api/anthropic", "business": business, "jwt": jwt,
		"loc": map[string]any{"organizationId": "fixture-org", "projectId": "fixture-project", "apiKeyId": "fixture-key"}})
	return map[string]any{"type": "oauth", "refresh": string(state), "access": jwt, "accountId": user, "uid": user}
}

func Auth(business, jwt string) map[string]any {
	return AuthFor("fixture-user", business, jwt)
}

// Install imports synthetic credentials in an isolated test home, and starts
// the real embedded package to discover the models and exact account selector.
func Install(t *testing.T, ctx context.Context) (plugin.Provider, string) {
	t.Helper()
	account, err := plugin.Import(ctx, "zcode", Auth("fixture-business", "fixture-zcode"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := plugin.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if p.ID == "zcode" {
			return p, account
		}
	}
	t.Fatalf("embedded ZCode not discovered: %+v", providers)
	return plugin.Provider{}, ""
}
