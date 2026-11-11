package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	fixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
)

func TestZCodeFallbackRealBundleRequiresGuardAndUsesCurrentAccount(t *testing.T) {
	claudeHome(t)
	t.Setenv("NATIVE_FIXTURE_PROVIDER", "zcode")
	vendor := fixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pp, account := fixture.Install(t, ctx)
	captured, err := ZCodeAccountOperation(ctx, PluginID("zcode"), account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	drawCtx := WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
	_, err = ZCodeAccountOperation(drawCtx, PluginID("zcode"), account, "execution.draw", map[string]any{"taskId": "task-1", "model": "GLM-5.3"})
	if err != nil {
		t.Fatalf("drawing fixture card: %v", err)
	}
	ask := func(callCtx context.Context, stream bool) (*http.Response, error) {
		t.Helper()
		body := `{"model":"GLM-5.3-Highspeed","messages":[{"role":"user","content":"ping"}],"max_tokens":32}`
		if stream {
			body = `{"model":"GLM-5.3-Highspeed","messages":[{"role":"user","content":"ping"}],"max_tokens":32,"stream":true}`
		}
		r, err := http.NewRequestWithContext(callCtx, "POST", pluginBase+"zcode/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set(SessionToPluginHeader, "task-1")
		r.Header.Set(zcodeFallbackHeader, "caller-must-not-forward")
		return pluginFetch(pp, account, r)
	}
	ordinary := func() []fixture.Call { return vendor.Calls("/api/anthropic/v1/messages") }
	if r, err := ask(ctx, false); err == nil {
		if r != nil {
			r.Body.Close()
		}
		t.Fatal("unguarded Highspeed fallback was accepted")
	}
	if got := ordinary(); len(got) != 0 {
		t.Fatalf("unguarded fallback sent ordinary traffic: %+v", got)
	}
	denied := WithZCodeFallback(ctx, func(ZCodeFallbackRequest) error { return errors.New("ordinary quota is full") })
	if r, err := ask(denied, false); err == nil || !strings.Contains(err.Error(), "ordinary quota is full") {
		if r != nil {
			r.Body.Close()
		}
		t.Fatalf("guard denial: response %v, error %v", r, err)
	}
	if got := ordinary(); len(got) != 0 {
		t.Fatalf("denied fallback sent ordinary traffic: %+v", got)
	}
	guardCalls := 0
	var guarded ZCodeFallbackRequest
	allowed := WithZCodeFallback(ctx, func(r ZCodeFallbackRequest) error {
		guardCalls++
		guarded = r
		return nil
	})
	r, err := ask(allowed, false)
	if err != nil {
		t.Fatal(err)
	}
	b, readErr := io.ReadAll(r.Body)
	r.Body.Close()
	if readErr != nil || r.StatusCode != http.StatusOK || guardCalls != 1 || !strings.Contains(string(b), "ordinary reply") {
		t.Fatalf("fallback: HTTP %d, body %s, guard %d, read %v", r.StatusCode, b, guardCalls, readErr)
	}
	if guarded.Provider != "zcode" || guarded.Account != account || guarded.RequestedModel != "GLM-5.3-Highspeed" || guarded.Model != "GLM-5.3" {
		t.Fatalf("authorization target %+v", guarded)
	}
	calls := ordinary()
	if len(calls) != 1 || calls[0].Model != "GLM-5.3" || calls[0].APIKey != "fixture-project-token" || calls[0].Marker != "" {
		t.Fatalf("ordinary request = %+v", calls)
	}
	vendor.SetPartial(true)
	partialCalls := 0
	partial := WithZCodeFallback(ctx, func(ZCodeFallbackRequest) error { partialCalls++; return nil })
	r, err = ask(partial, true)
	if err != nil {
		t.Fatal(err)
	}
	b, readErr = io.ReadAll(r.Body)
	r.Body.Close()
	if readErr != nil || !strings.Contains(string(b), "native partial") || partialCalls != 0 || len(ordinary()) != 1 {
		t.Fatalf("partially accepted stream was replayed: body %q, guard %d, read %v", b, partialCalls, readErr)
	}
	vendor.SetPartial(false)
	vendor.SetSpoof(true)
	spoofCalls := 0
	spoof := WithZCodeFallback(ctx, func(ZCodeFallbackRequest) error { spoofCalls++; return nil })
	r, err = ask(spoof, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusConflict || spoofCalls != 0 || r.Header.Get(zcodeFallbackHeader) != "" || len(ordinary()) != 1 {
		t.Fatalf("upstream spoof triggered an ordinary call: HTTP %d, guard %d, headers %v", r.StatusCode, spoofCalls, r.Header)
	}
	// An external preference change cannot replace the Core-owned ZCode
	// provider, including on an already-authorized fallback.
	vendor.SetSpoof(false)
	local, err := filepath.Abs("../plugin/testdata/nativeaccount/index.mjs")
	if err != nil {
		t.Fatal(err)
	}
	switched := WithZCodeFallback(ctx, func(ZCodeFallbackRequest) error {
		if _, err := plugin.Add(ctx, local); err != nil {
			return err
		}
		return plugin.Prefer(local, "zcode")
	})
	r, err = ask(switched, false)
	if err != nil || r == nil {
		t.Fatalf("external preference displaced intrinsic fallback: %v", err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if got := ordinary(); r.StatusCode != http.StatusOK || len(got) != 2 {
		t.Fatalf("intrinsic fallback did not retain source authority: HTTP %d calls=%+v", r.StatusCode, got)
	}
}

func TestZCodeFallbackRejectsReloginBeforeOrdinaryDispatch(t *testing.T) {
	claudeHome(t)
	vendor := fixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pp, account := fixture.Install(t, ctx)
	captured, err := ZCodeAccountOperation(ctx, PluginID("zcode"), account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	drawCtx := WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
	if _, err := ZCodeAccountOperation(drawCtx, PluginID("zcode"), account, "execution.draw", map[string]any{"taskId": "task-rotate", "model": "GLM-5.3"}); err != nil {
		t.Fatalf("drawing fixture card: %v", err)
	}
	guarded := WithZCodeFallback(ctx, func(ZCodeFallbackRequest) error {
		// Replace this exact slot after the native failure but before the
		// ordinary dispatch; the admitted generation must still match.
		var result struct {
			Account string `json:"account"`
		}
		if err := plugin.Call(ctx, "import", map[string]any{"provider": "zcode", "key": account,
			"auth": fixture.Auth("fixture-business-new", "fixture-zcode-new")}, &result); err != nil {
			return err
		}
		if result.Account != account {
			return errors.New("fixture account unexpectedly changed")
		}
		return nil
	})
	req, err := http.NewRequestWithContext(guarded, "POST", pluginBase+"zcode/v1/messages",
		strings.NewReader(`{"model":"GLM-5.3-Highspeed","max_tokens":32,"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SessionToPluginHeader, "task-rotate")
	res, err := pluginFetch(pp, account, req)
	if res != nil {
		res.Body.Close()
	}
	calls := vendor.Calls("/api/anthropic/v1/messages")
	if err == nil || len(calls) != 0 {
		t.Fatalf("changed login reached ordinary vendor: calls=%+v err=%v", calls, err)
	}
}
