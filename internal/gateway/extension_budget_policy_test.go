package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwbudget"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installBudgetPolicyFixture(t *testing.T, manager *extensionhost.Manager, sink string) string {
	t.Helper()
	directory, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, entry), "./testdata/budgetworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build budget fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.budget", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract:       sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 13}, Capabilities: []sdk.Capability{sdk.BudgetPolicy}},
		BudgetPolicies: []sdk.BudgetPolicyDescriptor{{ID: "extra", TimeoutMs: 1000}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"sink": sink})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", config); err != nil {
		t.Fatal(err)
	}
	return manifest.ID
}

func TestSDKBudgetPolicyOrdinaryGatewayAndRevocation(t *testing.T) {
	fresh(t)
	budget.Forget()
	now := pinLimitClock(t)
	upstream := limitedUpstream(t, 0)
	keys, secrets := newCaller(t, "native-private-label", "policy-private-label", "issuer-private-label", "cancel-private-label")
	setLimit(t, keys[0].ID, &access.Limit{Period: "day", Tokens: 100})
	setLimit(t, keys[1].ID, &access.Limit{Period: "day", Tokens: 50})
	setLimit(t, keys[2].ID, &access.Limit{Period: "day", Tokens: 2000, Models: []string{"plan/m1"}})
	setLimit(t, keys[3].ID, &access.Limit{Period: "day", Tokens: 50})
	if _, err := access.Update("issuer-key", access.Change{Key: keys[2].ID, Issues: true}); err != nil {
		t.Fatal(err)
	}
	child, err := access.IssueKey(access.Issue{Name: "private-child", TTL: time.Hour, Issuer: keys[2].ID})
	if err != nil {
		t.Fatal(err)
	}

	var mode atomic.Int32
	var callbacks atomic.Int32
	type decisionGate struct {
		resume  chan struct{}
		replied chan bool
	}
	entered := make(chan decisionGate, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callbacks.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		for _, private := range append(append([]string{child.Key, "upstream-secret", "private-label", "syntheticprivateprompt", "plan/m1"}, secrets...), keys[2].ID) {
			if strings.Contains(string(raw), private) {
				t.Error("gateway credential or private request text reached the policy")
			}
		}
		var req sdk.BudgetPolicyRequest
		if err := json.Unmarshal(raw, &req); err != nil || req.Policy != "extra" || req.Time != now.UnixMilli() {
			t.Errorf("wrong worker projection: %+v, %v", req, err)
		}
		spent := r.URL.Path == "/"+sdk.MethodBudgetSpent
		if spent && (req.BodyBytes != nil || len(req.Scopes) != 1 || req.Scopes[0] != (sdk.BudgetScope{Owner: "issuer", Scoped: true, Tokens: "2000"})) {
			t.Errorf("wrong issuer scoped projection: %+v", req)
		}
		if !spent && (req.BodyBytes == nil || *req.BodyBytes < 1 || len(req.Scopes) != 1 || req.Scopes[0].Owner != "key" && req.Scopes[0] != (sdk.BudgetScope{Owner: "issuer", Scoped: true, Tokens: "2000"})) {
			t.Errorf("wrong reservation projection: %+v", req)
		}
		restrict := `{"restrictions":[{"scope":1,"retrySeconds":37}]}`
		switch mode.Load() {
		case 1:
			if spent || len(req.Scopes) == 1 && req.Scopes[0].Owner == "key" {
				io.WriteString(w, restrict)
				return
			}
		case 2:
			http.Error(w, "synthetic private worker failure", http.StatusBadGateway)
			return
		case 3:
			io.WriteString(w, `{"restrictions":[{"scope":2,"retrySeconds":37}]}`)
			return
		case 4:
			gate := decisionGate{resume: make(chan struct{}), replied: make(chan bool, 1)}
			select {
			case entered <- gate:
			case <-r.Context().Done():
				return
			}
			select {
			case <-r.Context().Done():
				gate.replied <- false
			case <-gate.resume:
				_, err := io.WriteString(w, `{"restrictions":[]}`)
				gate.replied <- err == nil && r.Context().Err() == nil
			}
			return
		}
		io.WriteString(w, `{"restrictions":[]}`)
	}))
	t.Cleanup(collector.Close)
	server := New()
	t.Cleanup(server.idle.stop)
	manager := server.extensions
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	h := server.Handler()
	body := `{"model":"plan/m1","messages":[{"role":"user","content":"syntheticprivateprompt` + strings.Repeat("x", 500) + `"}]}`
	call := func(ctx context.Context, secret string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	assert := func(w *httptest.ResponseRecorder, status int, retry, reset string) {
		t.Helper()
		if w.Code != status || w.Header().Get("Retry-After") != retry || w.Header().Get(remoteKeyLimitHeader) != reset ||
			status >= 400 && w.Header().Get("X-Should-Retry") != "false" ||
			strings.Contains(w.Body.String(), "synthetic private worker failure") {
			t.Fatalf("unexpected status=%d headers=%v response=%s", w.Code, w.Header(), w.Body)
		}
	}
	selectPolicy := func(name string) {
		t.Helper()
		cfg := settings.Load()
		cfg.GatewayBudgetPolicy = name
		if err := settings.Save(cfg); err != nil {
			t.Fatal(err)
		}
	}

	assert(call(t.Context(), secrets[0]), http.StatusOK, "", "")
	_, reset := access.Window("day", now)
	assert(call(t.Context(), secrets[0]), http.StatusTooManyRequests, strconv.Itoa(int(reset.Sub(now).Seconds())), reset.Format(time.RFC3339))
	if callbacks.Load() != 0 || upstream.Load() != 1 {
		t.Fatal("plugin-free request count", callbacks.Load(), upstream.Load())
	}
	selectPolicy("missing/extra")
	assert(call(t.Context(), secrets[1]), http.StatusServiceUnavailable, "", "")
	if upstream.Load() != 1 {
		t.Fatal("unavailable selection reached upstream")
	}

	module := installBudgetPolicyFixture(t, manager, collector.URL)
	selectPolicy(module + "/extra")
	removeLimiter := gwbudget.Limit(nil)
	t.Cleanup(removeLimiter)
	countRequest := func() *httptest.ResponseRecorder {
		return keysCall(h, http.MethodPost, "/v1/messages/count_tokens", fromThere, child.Key, body)
	}
	assert(call(t.Context(), secrets[1]), http.StatusServiceUnavailable, "", "")
	assert(countRequest(), http.StatusServiceUnavailable, "", "")
	selectPolicy("")
	assert(countRequest(), http.StatusOK, "", "")
	selectPolicy(module + "/extra")
	removeLimiter()
	if callbacks.Load() != 0 || upstream.Load() != 1 {
		t.Fatal("selected policy bypassed the native limiter availability fence")
	}
	mode.Store(1)
	before := callbacks.Load()
	assert(call(t.Context(), secrets[1]), http.StatusTooManyRequests, "37", now.Add(37*time.Second).Format(time.RFC3339))
	if callbacks.Load() != before+1 {
		t.Fatal("a reservation was retried by the gateway")
	}
	mode.Store(2)
	assert(call(t.Context(), secrets[1]), http.StatusServiceUnavailable, "", "")
	mode.Store(3)
	assert(call(t.Context(), secrets[1]), http.StatusServiceUnavailable, "", "")
	mode.Store(0)
	// Each rejected/error reservation must have been released. One leaked
	// hold would exhaust the 50-token key before this request can reserve.
	assert(call(t.Context(), secrets[1]), http.StatusOK, "", "")
	usage := budget.Of(access.Key{ID: keys[1].ID, Limit: &access.Limit{Period: "day", Tokens: 50}}, now)
	if usage == nil || usage.InFlight != 0 || usage.Tokens != 350 {
		t.Fatal("successful reservation did not settle at the native usage owner", usage)
	}

	mode.Store(1)
	assert(call(t.Context(), child.Key), http.StatusTooManyRequests, "37", "")
	if upstream.Load() != 2 {
		t.Fatal("spent issuer scope reached its model", upstream.Load())
	}
	mode.Store(0)
	// Native 350-token accounting still exhausts the first key even when
	// an installed SDK policy would otherwise allow it.
	before = callbacks.Load()
	assert(call(t.Context(), secrets[0]), http.StatusTooManyRequests, strconv.Itoa(int(reset.Sub(now).Seconds())), reset.Format(time.RFC3339))
	if callbacks.Load() != before {
		t.Fatal("native rejection called the selected policy")
	}

	inFlight := func(ctx context.Context, proveHold bool, revoke func()) {
		t.Helper()
		mode.Store(4)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- call(ctx, secrets[3]) }()
		var gate decisionGate
		select {
		case gate = <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("waiting policy request did not dispatch")
		}
		if proveHold {
			// The native reservation remains held while the module decides.
			before := callbacks.Load()
			assert(call(t.Context(), secrets[3]), http.StatusTooManyRequests,
				strconv.Itoa(int(reset.Sub(now).Seconds())), reset.Format(time.RFC3339))
			if callbacks.Load() != before {
				t.Fatal("concurrent request bypassed the native reservation")
			}
		}
		revoke()
		close(gate.resume)
		if !proveHold {
			select {
			case replied := <-gate.replied:
				if !replied {
					t.Fatal("revocation fixture failed to publish its allow response")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("revocation fixture did not reply")
			}
		}
		select {
		case response := <-done:
			assert(response, http.StatusServiceUnavailable, "", "")
		case <-time.After(10 * time.Second):
			t.Fatal("waiting gateway did not finish")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inFlight(ctx, true, cancel)
	inFlight(t.Context(), false, func() {
		if err := manager.SetEnabled(t.Context(), module, false); err != nil {
			t.Error(err)
		}
		if err := manager.SetEnabled(t.Context(), module, true); err != nil {
			t.Error(err)
		}
	})
	if err := manager.SetEnabled(t.Context(), module, false); err != nil {
		t.Fatal(err)
	}
	assert(call(t.Context(), secrets[3]), http.StatusServiceUnavailable, "", "")
	if upstream.Load() != 2 {
		t.Fatal("canceled, revoked or unavailable reservation reached upstream")
	}
	selectPolicy("")
	mode.Store(0)
	assert(call(t.Context(), secrets[3]), http.StatusOK, "", "")
	if upstream.Load() != 3 {
		t.Fatal("canceled reservations survived release", upstream.Load())
	}
}

func TestSDKBudgetPolicySpenderHoldsPaidTaskToDelegatedScope(t *testing.T) {
	server, manager, providerID, _, vendorCalls := taskGatewayFixture(t, "")
	budget.Forget()
	t.Cleanup(budget.Forget)
	keys, _ := newCaller(t, "synthetic-issuer")
	setLimit(t, keys[0].ID, &access.Limit{Period: "day", Tokens: 1000, Models: []string{providerID + "/m1"}})
	if _, err := access.Update("issuer-key", access.Change{Key: keys[0].ID, Issues: true}); err != nil {
		t.Fatal(err)
	}
	child, err := access.IssueKey(access.Issue{Name: "synthetic-child", TTL: time.Hour, Issuer: keys[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	who, err := access.Check(child.Key)
	if err != nil {
		t.Fatal(err)
	}

	var spentCalls, reserveCalls atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sdk.BudgetPolicyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Scopes) != 1 ||
			req.Scopes[0] != (sdk.BudgetScope{Owner: "issuer", Scoped: true, Tokens: "1000"}) {
			t.Errorf("wrong delegated paid scope: %+v, %v", req, err)
		}
		if r.URL.Path == "/"+sdk.MethodBudgetSpent {
			if spentCalls.Add(1) > 1 {
				io.WriteString(w, `{"restrictions":[{"scope":1,"retrySeconds":37}]}`)
				return
			}
		} else {
			reserveCalls.Add(1)
		}
		io.WriteString(w, `{"restrictions":[]}`)
	}))
	t.Cleanup(collector.Close)
	module := installBudgetPolicyFixture(t, manager, collector.URL)
	cfg := settings.Load()
	cfg.GatewayBudgetPolicy = module + "/extra"
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}

	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
	operation := scheduleGatewayTask(t, server, providerID, access.WithIdentity(t.Context(), who))
	var receipt extensionhost.JournalEntry
	waitFor(t, "terminal paid task receipt", func() bool {
		current, err := manager.Store().Operation("tasks.fixture", operation)
		if err != nil {
			return false
		}
		receipt = current
		return current.State == "not-started" || current.State == "completed" || current.State == "uncertain"
	})
	if receipt.State != "not-started" || spentCalls.Load() != 2 || reserveCalls.Load() != 1 || vendorCalls.Load() != 0 {
		t.Fatalf("paid task was not rejected by the delegated spender before reserve: outcome=%s spent=%d reserve=%d vendor=%d",
			receipt.State, spentCalls.Load(), reserveCalls.Load(), vendorCalls.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
