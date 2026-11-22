package provider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
	"github.com/yetone/magpie/internal/testenv"
)

type pluginUsageReply struct {
	status int
	body   string
}

func startPluginUsageFixture(t *testing.T) (Login, <-chan chan pluginUsageReply) {
	return startPluginUsageFixtureWithAuth(t, false)
}

func startPluginUsageFixtureWithAuth(t *testing.T, public bool) (Login, <-chan chan pluginUsageReply) {
	t.Helper()
	bun := testenv.Bun(t)
	claudeHome(t)
	t.Setenv("FAKE_USAGE_PUBLIC", fmt.Sprint(public))
	writeFile(t, catalog.CachePath(), map[string]any{"fakeco": map[string]any{"models": map[string]any{}}})
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	requests := make(chan chan pluginUsageReply, 2)
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isFetch := r.Method == http.MethodPost && r.URL.Path == "/plugin-usage-fixture/chat/completions"
		isUsage := r.Method == http.MethodGet && r.URL.Path == "/plugin-usage-fixture"
		if !isFetch && !isUsage {
			http.NotFound(w, r)
			return
		}
		reply := make(chan pluginUsageReply, 1)
		select {
		case requests <- reply:
		case <-r.Context().Done():
			return
		}
		select {
		case value := <-reply:
			if isFetch {
				if value.body != "" {
					w.Header().Set(SignInHeader, value.body)
				}
				value.body = "{}"
			}
			w.WriteHeader(value.status)
			_, _ = fmt.Fprint(w, value.body)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(vendor.Close)
	t.Setenv("FAKE_USAGE", vendor.URL+"/plugin-usage-fixture")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	entry := filepath.Join(t.TempDir(), "usage.js")
	if err := os.WriteFile(entry, []byte(`export const UsageFixture = async () => ({
  config: (cfg) => {
    (cfg.provider ??= {}).fakeco = {
      name: "FakeCo", npm: "@ai-sdk/openai-compatible",
      models: { "fake-1": { name: "Fake One", limit: { context: 1000, output: 100 } } },
    }
  },
  auth: {
    provider: "fakeco",
    methods: [{ type: "api", label: "API key", ...(process.env.FAKE_USAGE_PUBLIC === "true" ? {
      keyless: true,
      authorize: async () => ({ type: "success", key: "public" }),
    } : {}) }],
    async loader() { return { baseURL: process.env.FAKE_USAGE } },
    async usage(getAuth) {
      await getAuth()
      const response = await fetch(process.env.FAKE_USAGE)
      if (response.status === 401) return { error: "the FakeCo sign-in has expired — sign in again" }
      const body = await response.text()
      return body.startsWith("{") ? JSON.parse(body) : { plan: body }
    },
  },
})`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(ctx, entry); err != nil {
		t.Fatal(err)
	}
	login := Login{Agent: "plugin:fakeco", User: "usage@fake", Plan: "Saved plan"}
	auth := map[string]any{
		"type": "oauth", "refresh": "r-ok", "access": "a", "expires": 9e15, "accountId": login.User,
	}
	if public {
		auth = map[string]any{"type": "api", "key": "public", "accountId": login.User}
	}
	key, err := plugin.Import(ctx, "fakeco", auth)
	if err != nil {
		t.Fatal(err)
	}
	saveLogins(t, savedLogin{Agent: login.Agent, User: login.User, Home: key, Plan: login.Plan, On: true})
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	return login, requests
}

func startPluginUsageRead(t *testing.T, ctx context.Context, login Login, requests <-chan chan pluginUsageReply) (chan<- pluginUsageReply, <-chan SubscriptionQuota) {
	t.Helper()
	return startPluginUsageCall(t, ctx, requests, func() SubscriptionQuota { return loginQuota(ctx, login) })
}

func startPluginUsageCall(t *testing.T, ctx context.Context, requests <-chan chan pluginUsageReply, read func() SubscriptionQuota) (chan<- pluginUsageReply, <-chan SubscriptionQuota) {
	t.Helper()
	result := make(chan SubscriptionQuota, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- read()
	}()
	t.Cleanup(func() {
		<-done
	})
	select {
	case reply := <-requests:
		return reply, result
	case quota := <-result:
		t.Fatalf("Usage returned without reading the vendor: %+v", quota)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil, nil
}

func TestPluginUsageEditorChecksDiscoveryBeforePublishing(t *testing.T) {
	const enabled = `{"noAccountDiscovery":false}`
	const disabled = `{"noAccountDiscovery":true }`
	for _, scenario := range []struct {
		name   string
		policy string
		reason string
		lock   bool
		public bool
	}{
		{name: "writer-wait", policy: disabled, reason: errAccountDiscoveryOff.Error(), lock: true},
		{name: "staged-file", policy: disabled, reason: errAccountDiscoveryOff.Error()},
		{name: "unreadable-policy", policy: disabled[:len(disabled)-1] + "!", reason: errPluginUsageDiscovery.Error()},
		{name: "public", policy: disabled, public: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			login, requests := startPluginUsageFixtureWithAuth(t, scenario.public)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			path := settings.Path()
			stamp := time.Now().Add(-time.Hour)
			replacePolicy := func(policy string) error {
				peer := path + ".peer"
				if err := os.WriteFile(peer, []byte(policy), 0o600); err != nil {
					return err
				}
				if err := os.Chtimes(peer, stamp, stamp); err != nil {
					return err
				}
				return steady.Rename(peer, path)
			}
			if err := replacePolicy(enabled); err != nil || settings.AccountDiscoveryOff() {
				t.Fatalf("could not begin with discovery enabled: %v", err)
			}
			before, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 14*time.Second)
			defer cancel()
			editor := func() SubscriptionQuota {
				quotas, err := LoginUsageEditor(ctx, login.Agent)
				if err != nil {
					return SubscriptionQuota{Error: err.Error()}
				}
				return quotas[login.User]
			}
			AskUsage()
			reply, result := startPluginUsageCall(t, ctx, requests, editor)
			vendor := pluginUsageReply{http.StatusOK, `{"plan":"Vendor plan","signIn":"expired","windows":[{"name":"Weekly","used":30}]}`}
			if scenario.lock {
				loginsMu.Lock()
				func() {
					defer loginsMu.Unlock()
					reply <- vendor
					waitPluginUsagePostRead(t, "filelock.LockMutex(", "(*Mutex).lock(")
					if err := replacePolicy(scenario.policy); err != nil {
						t.Fatal(err)
					}
				}()
			}
			var quota SubscriptionQuota
			if scenario.lock {
				quota = readPluginUsageResult(t, result)
			} else {
				var peerErr error
				replaced := false
				flushed := cancelHistoryFlush(t, loginsPath(), func() {
					if !replaced {
						peerErr = replacePolicy(scenario.policy)
						replaced = true
					}
				})
				reply <- vendor
				quota = readPluginUsageResult(t, result)
				if !*flushed || peerErr != nil {
					t.Fatalf("peer did not replace the settings during the staged logins flush: %v", peerErr)
				}
			}
			if scenario.public {
				if quota.ReadDeferred || quota.Error != "" || quota.Plan != "Vendor plan" || len(quota.Windows) != 1 {
					t.Errorf("public editor should still show the result with discovery off: %+v", quota)
				}
			} else if !quota.ReadDeferred || !strings.Contains(quota.Error, scenario.reason) || quota.Plan != "" || len(quota.Windows) != 0 {
				t.Errorf("editor served the revoked observation: %+v", quota)
			}
			if after, err := os.ReadFile(loginsPath()); err != nil || !bytes.Equal(after, before) {
				t.Errorf("Usage published a plan or sign-in observation after discovery changed: %v", err)
			}

			if scenario.public {
				return
			}
			if err := replacePolicy(enabled); err != nil {
				t.Fatal(err)
			}
			AskUsage()
			reply, result = startPluginUsageCall(t, ctx, requests, editor)
			reply <- pluginUsageReply{http.StatusOK, `{"plan":"Recovered plan","signIn":"renewed","windows":[{"name":"Weekly","used":30}]}`}
			if quota := readPluginUsageResult(t, result); quota.ReadDeferred || quota.Error != "" || quota.Plan != "Recovered plan" {
				t.Fatalf("editor after repairing discovery: %+v", quota)
			}
			if saved := loginOf(t, readLogins(), login.Agent, login.User); saved.Plan != "Recovered plan" || saved.Lapsed != "" {
				t.Errorf("recovered Usage did not persist the plan and sign-in: %q, %q", saved.Plan, saved.Lapsed)
			}
		})
	}
}

func readPluginUsageResult(t *testing.T, result <-chan SubscriptionQuota) SubscriptionQuota {
	t.Helper()
	select {
	case quota := <-result:
		return quota
	case <-time.After(8 * time.Second):
		t.Fatal("Usage did not return")
		return SubscriptionQuota{}
	}
}

func waitPluginUsagePostRead(t *testing.T, pending ...string) {
	t.Helper()
	stacks := make([]byte, 256<<10)
	deadline := time.After(4 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		for stack := range bytes.SplitSeq(stacks[:runtime.Stack(stacks, true)], []byte("\n\n")) {
			if bytes.Contains(stack, []byte("pluginLoginQuota(")) && !bytes.Contains(stack, []byte("plugin.AccountUsage(")) {
				for _, frame := range pending {
					if bytes.Contains(stack, []byte(frame)) {
						return
					}
				}
			}
		}
		select {
		case <-deadline:
			t.Fatalf("Usage did not reach %v after its vendor reply", pending)
		case <-tick.C:
		}
	}
}

func TestPluginUsageCanceledAfterVendorRead(t *testing.T) {
	for _, outcome := range []struct {
		name  string
		reply pluginUsageReply
	}{
		{"plan", pluginUsageReply{http.StatusOK, "Vendor plan"}},
		{"lapse", pluginUsageReply{status: http.StatusUnauthorized}},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			before, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			reply, result := startPluginUsageRead(t, ctx, login, requests)
			path := filepath.Join(filepath.Dir(plugin.AuthPath()), "extensions", "private", ".lock")
			unlock, held := filelock.Lock(path, time.Second)
			if !held {
				t.Fatal("could not hold the SDK store lock")
			}
			defer func() { unlock() }()
			reply <- outcome.reply
			waitPluginUsagePostRead(t, "storeWaitLock(")
			unlockLogins, held := filelock.Lock(loginsMu.Path(), 0)
			unlockLogins()
			if !held {
				t.Error("Usage held the logins lock while waiting for the SDK store")
			}

			cancel()
			var quota SubscriptionQuota
			returned := false
			select {
			case quota = <-result:
				returned = true
			case <-time.After(time.Second):
				t.Error("canceled Usage remained blocked on the SDK store")
			}
			unlock()
			unlock = func() {}
			if !returned {
				quota = readPluginUsageResult(t, result)
			}
			if quota.Error != context.Canceled.Error() {
				t.Errorf("canceled Usage reported %q", quota.Error)
			}
			if after, err := os.ReadFile(loginsPath()); err != nil || !bytes.Equal(after, before) {
				t.Errorf("canceled Usage changed saved logins after the store lock was released: %v", err)
			}
		})
	}
}

func TestPluginUsageCanceledWhileWaitingForLogins(t *testing.T) {
	for _, lock := range []struct {
		name    string
		acquire func(t *testing.T) func()
		waiting []string
	}{
		{
			name: "process",
			acquire: func(t *testing.T) func() {
				t.Helper()
				loginsMu.Lock()
				return loginsMu.Unlock
			},
			waiting: []string{"filelock.LockMutex(", "(*Mutex).lock("},
		},
		{
			name: "file",
			acquire: func(t *testing.T) func() {
				t.Helper()
				unlock, held := filelock.Lock(loginsMu.Path(), time.Second)
				if !held {
					t.Fatal("could not hold the logins file lock")
				}
				return unlock
			},
			waiting: []string{"filelock.lockPath(", "filelock.Lock("},
		},
	} {
		t.Run(lock.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			before, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			reply, result := startPluginUsageRead(t, ctx, login, requests)
			unlock := lock.acquire(t)
			defer func() { unlock() }()
			reply <- pluginUsageReply{http.StatusOK, "Vendor plan"}
			waitPluginUsagePostRead(t, lock.waiting...)
			cancel()
			var quota SubscriptionQuota
			returned := false
			select {
			case quota = <-result:
				returned = true
			case <-time.After(time.Second):
				t.Error("canceled Usage remained blocked on the logins lock")
			}
			unlock()
			unlock = func() {}
			if !returned {
				quota = readPluginUsageResult(t, result)
			}
			if quota.Error != context.Canceled.Error() {
				t.Errorf("canceled Usage reported %q", quota.Error)
			}
			if after, err := os.ReadFile(loginsPath()); err != nil || !bytes.Equal(after, before) {
				t.Errorf("canceled Usage changed saved logins after releasing the lock: %v", err)
			}
		})
	}
}

func TestPluginUsageCanceledBeforePublishingLogins(t *testing.T) {
	login, requests := startPluginUsageFixture(t)
	before, err := os.ReadFile(loginsPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	hook := beforeLoginsWrite
	t.Cleanup(func() { beforeLoginsWrite = hook })
	beforeLoginsWrite = cancelRead
	reply, result := startPluginUsageRead(t, readCtx, login, requests)
	plugin.Refreshed()
	plugin.Told()
	stamp := StampNow()
	reply <- pluginUsageReply{http.StatusOK, "Vendor plan"}
	if quota := readPluginUsageResult(t, result); quota.Error != context.Canceled.Error() {
		t.Errorf("canceled Usage reported %q", quota.Error)
	}
	if after, err := os.ReadFile(loginsPath()); err != nil || !bytes.Equal(after, before) {
		t.Errorf("canceled Usage published saved logins: %v", err)
	}
	if after := StampNow(); after != stamp {
		t.Errorf("canceled Usage invalidated the catalog stamp: %+v → %+v", stamp, after)
	}

	beforeLoginsWrite = hook
	stamp = StampNow()
	reply, result = startPluginUsageRead(t, ctx, login, requests)
	reply <- pluginUsageReply{http.StatusOK, "Vendor plan"}
	if quota := readPluginUsageResult(t, result); quota.Error != "" || quota.Plan != "Vendor plan" {
		t.Fatalf("retried Usage = %+v", quota)
	}
	if saved := loginOf(t, readLogins(), login.Agent, login.User); saved.Plan != "Vendor plan" {
		t.Errorf("successful Usage did not save the plan: %q", saved.Plan)
	}
	if StampNow() == stamp {
		t.Error("successful Usage left the catalog stamp unchanged")
	}
}

func TestPluginUsageOlderReadCannotReplaceNewerAccountState(t *testing.T) {
	for _, outcome := range []struct {
		name  string
		reply pluginUsageReply
	}{
		{"plan", pluginUsageReply{http.StatusOK, "Older plan"}},
		{"lapse", pluginUsageReply{status: http.StatusUnauthorized}},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			oldReply, oldResult := startPluginUsageRead(t, ctx, login, requests)
			newReply, newResult := startPluginUsageRead(t, ctx, login, requests)
			newReply <- pluginUsageReply{http.StatusOK, "Newer plan"}
			if quota := readPluginUsageResult(t, newResult); quota.Error != "" || quota.Plan != "Newer plan" {
				t.Fatalf("newer Usage = %+v", quota)
			}
			oldReply <- outcome.reply
			readPluginUsageResult(t, oldResult)
			saved := loginOf(t, readLogins(), login.Agent, login.User)
			if saved.Plan != "Newer plan" || saved.Lapsed != "" {
				t.Errorf("older Usage changed the newer state: plan %q, lapse %q", saved.Plan, saved.Lapsed)
			}
		})
	}
}
