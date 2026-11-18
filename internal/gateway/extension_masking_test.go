package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwmw"
	"github.com/yetone/magpie/internal/masking"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKMaskingRulesOrdinaryGateway(t *testing.T) {
	fresh(t)
	catalog, manager, module := maskingRulesFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	server.extensions = manager

	const token = "acme-abcdefghijk"
	const ticket = "ticket-12341234"
	const own = "SyntheticNativeWord"
	var calls atomic.Int32
	bodies := make(chan string, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodies <- string(body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
			http.Error(w, "bad fixture", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		content, _ := json.Marshal(req.Messages[0].Content)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(content)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "test", Name: "Test", Key: "synthetic", Chat: up.URL + "/v1", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	selectSet := func(selection string) {
		t.Helper()
		cfg := settings.Load()
		cfg.RedactWords = []string{own}
		cfg.RedactRuleSet = selection
		if err := settings.Save(cfg); err != nil {
			t.Fatal(err)
		}
	}
	call := func(fingerprint string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"test/m","messages":[{"role":"user","content":"`+token+" "+ticket+" "+own+`"}]}`))
		req.RemoteAddr = "127.0.0.1:12000"
		if fingerprint != "" {
			req.Header.Set("User-Agent", magpieUA())
			req.Header.Set(MaskedAsHeader, fingerprint)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)
		return w
	}
	selectSet("")
	oldFP := ownMaskedAs(t.Context())
	if oldFP == "" {
		t.Fatal("fixture did not establish native fingerprint")
	}
	if response := call(""); response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	if body := <-bodies; !strings.Contains(body, token) || !strings.Contains(body, ticket) || strings.Contains(body, own) {
		t.Fatal("install changed native behavior", body)
	}
	ctx := context.WithValue(t.Context(), protocolRegistryCtx{}, protocolBinding{server: server})
	preSelection, priorMasker, err := maskingFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selection := module + "/team"
	selectSet(selection)
	priorRealtime := &realtimeMask{ctx: preSelection, m: priorMasker}
	if _, err := priorRealtime.out([]byte(`{"text":"` + token + `"}`)); !errors.Is(err, errExtensionMasking) {
		t.Fatal("session opened before selection kept dispatching without it", err)
	}
	if ownMaskedAs(t.Context()) != "" {
		t.Fatal("selected declarative rules advertised unsafe relay equivalence")
	}
	if response := call(oldFP); response.Code != 200 || !strings.Contains(response.Body.String(), token+" "+ticket+" "+own) {
		t.Fatal(response.Code, response.Body)
	}
	firstBody := <-bodies
	if body := firstBody; strings.Contains(body, token) || strings.Contains(body, ticket) || strings.Contains(body, own) ||
		!strings.Contains(body, "{{SDK_TOKEN_") || !strings.Contains(body, "{{SDK_TICKET_") || !strings.Contains(body, "{{TERM_") {
		t.Fatal("vendor did not receive additive placeholders", body)
	}

	pinned, masker, err := maskingFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetEnabled(t.Context(), module, false); err != nil {
		t.Fatal(err)
	}
	if response := call(oldFP); response.Code != 503 || calls.Load() != 2 {
		t.Fatal("disabled selected rules reached vendor", response.Code, calls.Load())
	}
	if err := catalog.SetEnabled(t.Context(), module, true); err != nil {
		t.Fatal(err)
	}
	realtime := &realtimeMask{ctx: pinned, m: masker}
	if _, err := realtime.out([]byte(`{"text":"` + token + `"}`)); !errors.Is(err, errExtensionMasking) {
		t.Fatal("older snapshot survived enablement", err)
	}

	if response := call(oldFP); response.Code != 200 || !strings.Contains(response.Body.String(), token+" "+ticket+" "+own) {
		t.Fatal(response.Code, response.Body)
	}
	secondBody := <-bodies
	for _, kind := range []string{"SDK_TOKEN", "SDK_TICKET", "TERM"} {
		pattern := regexp.MustCompile(`\{\{` + kind + `_[a-z2-7]{8}\}\}`)
		first, second := pattern.FindString(firstBody), pattern.FindString(secondBody)
		if first == "" || second == "" || (first == second) != (kind == "TERM") {
			t.Errorf("%s placeholder has the wrong identity across catalog generations: %q, %q", kind, first, second)
		}
	}

	if _, _, err := maskedText(ctx, "acme-"+strings.Repeat("x", 4096)); err == nil {
		t.Fatal("an oversized declarative value was partially sent")
	}

	remove := gwmw.Mask(noExtensionMasker{})
	t.Cleanup(remove)
	if response := call(""); response.Code != 503 || calls.Load() != 3 {
		t.Fatal("replacement masker ignored required rules", response.Code, calls.Load())
	}
}

type noExtensionMasker struct{ masking.Masker }

func maskingRulesFixture(t *testing.T) (*extensionhost.Catalog, *extensionhost.Manager, string) {
	t.Helper()
	root := t.TempDir()
	catalog := extensionhost.NewCatalog(root)
	source := t.TempDir()
	manifest := sdk.Manifest{
		Schema: 1, ID: "fixture.mask", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 15}, Capabilities: []sdk.Capability{sdk.MaskingRules}},
		MaskingRuleSets: []sdk.MaskingRuleSet{{ID: "team", Rules: []sdk.MaskingRule{
			{Kind: "SDK_TOKEN", Prefix: "acme-"},
			{Kind: "SDK_TICKET", Regex: `ticket-[0-9]{8}`},
		}}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	// The installed package is intentionally unlaunchable. Rules are data,
	// so no request may start the extension process.
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("inert fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Install(t.Context(), source, func(context.Context, extensionhost.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	manager := extensionhost.NewManager(root)
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return catalog, manager, manifest.ID
}

func TestSDKMaskingRulesRealtimeOpaqueFrames(t *testing.T) {
	up, server, ts := rtServe(t)
	catalog, manager, module := maskingRulesFixture(t)
	server.extensions = manager
	if err := settings.Save(settings.Settings{RedactRuleSet: module + "/team"}); err != nil {
		t.Fatal(err)
	}
	const path = "/v1/realtime?model=oa/gpt-realtime"
	t.Run("text beside audio", func(t *testing.T) {
		c, resp, err := rtDial(t, ts, path, nil)
		if err != nil {
			t.Fatalf("dial: %v (%v)", err, resp)
		}
		for _, body := range []string{
			`{"type":"custom.audio.delta","delta":"QUJD","text":"acme-abcdefghijk"}`,
			`{"type":"input_audio_buffer.append","audio":"QUJD","text":"acme-abcdefghijk"}`,
			`{"type":"input_audio_buffer.append","audio":"acme-abcdefghijk","audio":"QUJD"}`,
			`{"type":"input_audio_buffer.append","audio":"acme-abcdefghijk"}`,
		} {
			rtSend(t, c, websocket.MessageText, body)
			rtRead(t, c)
			up.mu.Lock()
			sent := up.frames[len(up.frames)-1]
			up.mu.Unlock()
			if strings.Contains(sent, "acme-abcdefghijk") || !strings.Contains(sent, "{{SDK_TOKEN_") || (strings.Contains(body, "QUJD") && !strings.Contains(sent, "QUJD")) {
				t.Errorf("upstream received unmasked text or altered audio: %s", sent)
			}
		}
		c.Close(websocket.StatusNormalClosure, "")
		rtClosed(t, up)
	})
	for _, frame := range []struct {
		name       string
		typ        websocket.MessageType
		body, echo string
	}{
		{"audio", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"QUJD"}`, `{"type":"echo","got":{"type":"input_audio_buffer.append","audio":"QUJD"}}`},
		{"binary", websocket.MessageBinary, "\x00\xffABC", "\x00\xffABC"},
	} {
		t.Run(frame.name, func(t *testing.T) {
			c, resp, err := rtDial(t, ts, path, nil)
			if err != nil {
				t.Fatalf("dial: %v (%v)", err, resp)
			}
			rtSend(t, c, frame.typ, frame.body)
			if typ, got := rtRead(t, c); typ != frame.typ || got != frame.echo {
				t.Fatalf("active session changed opaque payload: %v %q", typ, got)
			}
			up.mu.Lock()
			texts, binaries := len(up.frames), up.binaries
			up.mu.Unlock()
			for _, enabled := range []bool{false, true} {
				if err := catalog.SetEnabled(t.Context(), module, enabled); err != nil {
					t.Fatal(err)
				}
			}
			rtSend(t, c, frame.typ, frame.body)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, got, err := c.Read(ctx)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("stale opaque frame: got %q, expected policy close, got %v", got, err)
			}
			rtClosed(t, up)
			up.mu.Lock()
			extraText, extraBinary := len(up.frames)-texts, up.binaries-binaries
			up.mu.Unlock()
			if extraText != 0 || extraBinary != 0 {
				t.Fatalf("vendor received stale opaque frame: %d text and %d binary", extraText, extraBinary)
			}
		})
	}
	if err := catalog.SetEnabled(t.Context(), module, false); err != nil {
		t.Fatal(err)
	}
	start := up.sessions()
	_, resp, err := rtDial(t, ts, path, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable || up.sessions() != start {
		t.Fatalf("disabled selection reached the realtime vendor: %v, %v, %d dials", resp, err, up.sessions()-start)
	}
}
