package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type openCodeTestRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

type openCodeTestServer struct {
	sync.Mutex
	requests     []openCodeTestRequest
	polls        []string
	orgID        *string
	orgs         string
	userID       string
	email        string
	access       string
	refreshToken string
	expiry       int
	refresh      func(http.ResponseWriter, *http.Request)
	userHook     func()
}

func openCodeFixture(t *testing.T) (*openCodeTestServer, string) {
	t.Helper()
	detectHome(t)
	f := &openCodeTestServer{expiry: 60, userID: "user-1", email: "same@example.com", access: "access-1", refreshToken: "refresh-1", orgs: `[{"id":"org-b","name":"Beta"},{"id":"org-a","name":"Alpha"}]`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.Lock()
		f.requests = append(f.requests, openCodeTestRequest{r.URL.Path, r.Header.Clone(), body})
		orgID, orgs, uid, email, access, refreshToken, expiry, refresh, userHook := f.orgID, f.orgs, f.userID, f.email, f.access, f.refreshToken, f.expiry, f.refresh, f.userHook
		poll := ""
		if r.URL.Path == "/console/auth/device/token" && body["grant_type"] != "refresh_token" && len(f.polls) > 0 {
			poll, f.polls = f.polls[0], f.polls[1:]
		}
		f.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/console/auth/device/code":
			fmt.Fprintf(w, `{"device_code":"device","user_code":"CODE-123","verification_uri_complete":"verify?code=CODE-123","expires_in":%d,"interval":0}`, expiry)
		case "/console/auth/device/token":
			if body["grant_type"] == "refresh_token" && refresh != nil {
				refresh(w, r)
				return
			}
			if poll != "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q}`, poll)
				return
			}
			if orgID == nil {
				fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600}`, access, refreshToken)
			} else {
				fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600,"org_id":%q}`, access, refreshToken, *orgID)
			}
		case "/console/api/user":
			if userHook != nil {
				userHook()
			}
			fmt.Fprintf(w, `{"id":%q,"email":%q}`, uid, email)
		case "/console/api/orgs":
			io.WriteString(w, orgs)
		case "/zen/go/v1/usage":
			io.WriteString(w, `{"usage":{"rolling":{"percent":23},"weekly":{"percent":75},"monthly":{"percent":1}}}`)
		default:
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(OpenCodeOAuthForTest(srv.URL+"/console", srv.URL))
	old := devicePollFloor
	devicePollFloor = time.Millisecond
	t.Cleanup(func() { devicePollFloor = old })
	return f, srv.URL
}

func openCodeSignInTest(t *testing.T, agent string) SignInState {
	t.Helper()
	st, err := StartSignIn(agent)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err = WaitSignIn(ctx, st.ID)
	if err != nil {
		t.Fatalf("waiting: %+v %v", st, err)
	}
	return st
}

func (f *openCodeTestServer) calls() []openCodeTestRequest {
	f.Lock()
	defer f.Unlock()
	return slices.Clone(f.requests)
}

func openCodeSavedTest(t *testing.T, agent, user string) savedLogin {
	t.Helper()
	for _, l := range readLogins() {
		if l.Agent == agent && l.User == user {
			return l
		}
	}
	t.Fatalf("no %s login for %q", agent, user)
	return savedLogin{}
}

func TestOpenCodeOAuthDeviceConversionAndSignedProtocols(t *testing.T) {
	for _, agent := range []string{"opencode-go", "opencode-zen"} {
		t.Run(agent, func(t *testing.T) {
			f, origin := openCodeFixture(t)
			legacy, err := FromPreset(agent)
			if err != nil {
				t.Fatal(err)
			}
			legacy.Key = "legacy-secret"
			legacy.Name = "My OpenCode"
			legacy.Models = []string{"test-model"}
			legacy.Was = []string{"old-name"}
			legacy.Headers = map[string]string{"X-Fixed": "kept", "Authorization": "Bearer wrong", "X-Opencode-Org-Id": "wrong"}
			legacy.ExtraBody = ExtraBody{"temperature": json.RawMessage("0.4")}
			if err := Save(legacy); err != nil {
				t.Fatal(err)
			}
			st := openCodeSignInTest(t, agent)
			if st.State != "done" || st.Plan != "Alpha" || !strings.HasSuffix(st.URL, "/console/verify?code=CODE-123") {
				t.Fatalf("sign-in: %+v", st)
			}
			calls := f.calls()
			if len(calls) < 4 || calls[0].Body["client_id"] != "opencode-cli" || calls[0].Body["supports_org_scope"] != true || calls[1].Body["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Fatalf("device requests: %+v", calls)
			}
			for _, r := range calls {
				if !strings.HasPrefix(r.Header.Get("User-Agent"), "opencode/") {
					t.Fatalf("Console client identity: %+v", r)
				}
				if strings.HasPrefix(r.Path, "/console/api/") && r.Header.Get("Authorization") != "Bearer access-1" {
					t.Fatalf("profile request: %+v", r)
				}
			}
			p, err := Find(agent)
			if err != nil || p.Account == nil || p.Key != "" || p.Account.User != st.User {
				t.Fatalf("provider: %+v %v", p, err)
			}
			if p.Name != "My OpenCode" || !slices.Equal(p.Was, []string{"old-name"}) || p.Headers["X-Fixed"] != "kept" || string(p.ExtraBody["temperature"]) != "0.4" || len(p.Exposed()) != 1 || p.Exposed()[0].ID != "test-model" {
				t.Fatalf("picks lost: %+v", p)
			}
			body := p.WithExtraBody([]byte(`{"model":"test-model"}`), Conversation{})
			if !bytes.Contains(body, []byte(`"temperature":0.4`)) {
				t.Fatalf("extra body: %s", body)
			}
			prefix := "/inference"
			if agent == "opencode-go" {
				prefix += "/go"
			}
			for _, tc := range []struct {
				proto Protocol
				path  string
			}{
				{Chat, "/openai/v1/chat/completions"}, {Responses, "/openai/v1/responses"},
				{Anthropic, "/anthropic/v1/messages"}, {Gemini, "/google/v1beta/models/test-model:generateContent"},
			} {
				req, _ := http.NewRequest(http.MethodPost, p.Base(tc.proto)+strings.TrimPrefix(tc.path, map[Protocol]string{Chat: "/openai/v1", Responses: "/openai/v1", Anthropic: "/anthropic", Gemini: "/google/v1beta"}[tc.proto]), strings.NewReader(`{}`))
				req.Header.Set("X-Org-Id", "stale-console-org")
				if err := p.Sign(context.Background(), req, tc.proto, nil); err != nil {
					t.Fatal(err)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				calls := f.calls()
				r := calls[len(calls)-1]
				if r.Path != prefix+tc.path || r.Header.Get("X-Opencode-Org-Id") != "org-a" || r.Header.Get("X-Org-Id") != "" || r.Header.Get("X-Fixed") != "kept" {
					t.Fatalf("%s: %+v", tc.proto, r)
				}
				if tc.proto == Gemini {
					if r.Header.Get("X-Goog-Api-Key") != "access-1" || r.Header.Get("Authorization") != "" {
						t.Fatalf("Gemini: %+v", r.Header)
					}
				} else if r.Header.Get("Authorization") != "Bearer access-1" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
					t.Fatalf("%s: %+v", tc.proto, r.Header)
				}
			}
			if !strings.HasPrefix(p.Chat, origin+prefix) {
				t.Fatalf("root: %s", p.Chat)
			}
			if agent == "opencode-zen" {
				target, err := p.DecideURL(context.Background())
				if err != nil || target != origin+"/inference/systemone/v1/systemone" || !p.DecidesModel("jev-1.13") {
					t.Fatalf("Zen decisions lost: %q %v", target, err)
				}
				req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"jev-1.13","state":"urgent","questions":{}}`))
				if err := p.Sign(context.Background(), req, DecideAPI, nil); err != nil {
					t.Fatal(err)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				calls := f.calls()
				last := calls[len(calls)-1]
				if last.Path != "/inference/systemone/v1/systemone" || last.Header.Get("Authorization") != "Bearer access-1" || last.Header.Get("X-Opencode-Org-Id") != "org-a" {
					t.Fatalf("Zen decision request: %+v", last)
				}
			}
			if err := ForgetLogin(agent, st.User); err != nil {
				t.Fatal(err)
			}
			if p, _ := find(All(), agent); p.Key == "legacy-secret" {
				t.Fatal("legacy key resurrected after sign-out")
			}
		})
	}
}

func TestOpenCodeOAuthDefaultOrgCollation(t *testing.T) {
	for _, tc := range []struct {
		name, orgs, wantID, wantName string
	}{
		{"mixed case", `[{"id":"org-b","name":"Banana"},{"id":"org-a","name":"apple"}]`, "org-a", "apple"},
		{"case variants", `[{"id":"org-a","name":"Apple"},{"id":"org-z","name":"apple"}]`, "org-z", "apple"},
		{"accent", `[{"id":"org-z","name":"Zebra"},{"id":"org-e","name":"éclair"}]`, "org-e", "éclair"},
		{"id tie-break", `[{"id":"org-B","name":"Same"},{"id":"org-a","name":"Same"}]`, "org-a", "Same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := openCodeFixture(t)
			f.Lock()
			f.orgs = tc.orgs
			f.Unlock()
			st := openCodeSignInTest(t, "opencode-go")
			if st.State != "done" || st.Plan != tc.wantName {
				t.Fatalf("default workspace: %+v", st)
			}
			c, ok := openCodeSaved(openCodeSavedTest(t, "opencode-go", st.User))
			if !ok || c.OrgID != tc.wantID || c.InitialOrgID != tc.wantID {
				t.Fatalf("default workspace identity: %+v", c)
			}
		})
	}
}

func TestOpenCodeOAuthOrgAuthorityAndStableIdentity(t *testing.T) {
	f, _ := openCodeFixture(t)
	missing := "missing"
	f.Lock()
	f.orgID = &missing
	f.Unlock()
	st := openCodeSignInTest(t, "opencode-go")
	if st.State != "failed" || !strings.Contains(st.Error, "organization not found") || len(Logins("opencode-go")) != 0 {
		t.Fatalf("missing org: %+v", st)
	}
	orgA := "org-a"
	f.Lock()
	f.orgID = &orgA
	f.Unlock()
	a := openCodeSignInTest(t, "opencode-go")
	orgB := "org-b"
	f.Lock()
	f.orgID = &orgB
	f.Unlock()
	b := openCodeSignInTest(t, "opencode-go")
	if a.State != "done" || b.State != "done" || a.User == b.User || len(Logins("opencode-go")) != 2 {
		t.Fatalf("accounts: %+v %+v", a, b)
	}
	old := openCodeSavedTest(t, "opencode-go", a.User)
	c, _ := openCodeSaved(old)
	c.Expires = time.Now().Add(-time.Hour)
	old.Auth, _ = json.Marshal(c)
	loginsMu.Lock()
	ls := readLogins()
	for i := range ls {
		if ls[i].ID == old.ID {
			ls[i] = old
		}
	}
	if err := writeLogins(ls); err != nil {
		loginsMu.Unlock()
		t.Fatal(err)
	}
	loginsMu.Unlock()
	f.Lock()
	f.refresh = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"org_id":"org-b"}`)
	}
	f.Unlock()
	refreshed, err := openCodeToken(context.Background(), "opencode-go", a.User)
	if err != nil || refreshed.OrgID != "org-b" || refreshed.OrgName != "org-b" {
		t.Fatalf("refresh: %+v %v", refreshed, err)
	}
	after := openCodeSavedTest(t, "opencode-go", a.User)
	if after.ID != old.ID || after.Plan != "org-b" || len(Logins("opencode-go")) != 2 {
		t.Fatalf("identity moved: %+v", after)
	}
	f.Lock()
	f.orgID = &orgA
	f.Unlock()
	again := openCodeSignInTest(t, "opencode-go")
	if again.User != a.User || len(Logins("opencode-go")) != 2 {
		t.Fatalf("re-login: %+v", again)
	}
	if err := SwitchLogin("opencode-go", b.User); err != nil {
		t.Fatal(err)
	}
	p, err := Find("opencode-go")
	if err != nil || p.Account.User != b.User || len(p.AlsoOn()) != 1 || p.AlsoOn()[0].Account.User != a.User {
		t.Fatalf("switch: %+v %v", p, err)
	}
	if err := SetLoginOn("opencode-go", a.User, false); err != nil {
		t.Fatal(err)
	}
	p, err = Find("opencode-go")
	if err != nil || len(p.AlsoOn()) != 0 {
		t.Fatalf("account off: %+v %v", p, err)
	}
	if err := SetLoginOn("opencode-go", a.User, true); err != nil {
		t.Fatal(err)
	}
	p, err = Find("opencode-go")
	if err != nil || len(p.AlsoOn()) != 1 {
		t.Fatalf("account on: %+v %v", p, err)
	}
}

func TestOpenCodeOAuthSubscriptionsKeepSeparateTokens(t *testing.T) {
	f, _ := openCodeFixture(t)
	goLogin := openCodeSignInTest(t, "opencode-go")
	f.Lock()
	f.access, f.refreshToken = "zen-access", "zen-refresh"
	f.Unlock()
	zenLogin := openCodeSignInTest(t, "opencode-zen")
	if goLogin.State != "done" || zenLogin.State != "done" {
		t.Fatalf("sign-ins: %+v %+v", goLogin, zenLogin)
	}
	g, _ := openCodeSaved(openCodeSavedTest(t, "opencode-go", goLogin.User))
	z, _ := openCodeSaved(openCodeSavedTest(t, "opencode-zen", zenLogin.User))
	if g.Refresh != "refresh-1" || z.Refresh != "zen-refresh" {
		t.Fatalf("credentials crossed subscriptions: %+v %+v", g, z)
	}
	if err := ForgetLogin("opencode-go", goLogin.User); err != nil {
		t.Fatal(err)
	}
	z, _ = openCodeSaved(openCodeSavedTest(t, "opencode-zen", zenLogin.User))
	if z.Refresh != "zen-refresh" {
		t.Fatal("forgetting Go changed Zen's token")
	}
}

func TestOpenCodeOAuthUsesAccountProxy(t *testing.T) {
	f, proxy := openCodeFixture(t)
	dialed := dialsOut(t)
	t.Cleanup(OpenCodeOAuthForTest("http://opencode.invalid/console", "http://opencode.invalid"))
	st, err := StartSignInWith("opencode-go", SignInOptions{Proxy: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err = WaitSignIn(ctx, st.ID)
	if err != nil || st.State != "done" || st.Proxy != proxy {
		t.Fatalf("sign-in %+v %v", st, err)
	}
	if got := ProxyOfLogin("opencode-go", st.User); got != proxy {
		t.Fatalf("saved proxy %q", got)
	}
	l := openCodeSavedTest(t, "opencode-go", st.User)
	c, _ := openCodeSaved(l)
	c.Expires = time.Now().Add(-time.Minute)
	l.Auth, _ = json.Marshal(c)
	saveLogins(t, l)
	f.Lock()
	f.refresh = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"fresh","refresh_token":"fresh-refresh","expires_in":3600}`)
	}
	f.Unlock()
	p, err := Find("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, p.Chat+"/chat/completions", strings.NewReader(`{}`))
	if err := p.Sign(context.Background(), req, Chat, nil); err != nil {
		t.Fatal(err)
	}
	q := loginQuota(context.Background(), Login{Agent: "opencode-go", User: st.User})
	if q.Error != "" || len(q.Windows) != 3 {
		t.Fatalf("proxied usage: %+v", q)
	}
	if got := dialed(); len(got) != 0 {
		t.Fatalf("requests bypassed the account proxy: %v", got)
	}
	seen := map[string]bool{}
	for _, r := range f.calls() {
		seen[r.Path] = true
	}
	for _, path := range []string{"/console/auth/device/code", "/console/auth/device/token", "/console/api/user", "/console/api/orgs", "/zen/go/v1/usage"} {
		if !seen[path] {
			t.Fatalf("proxy did not receive %s: %+v", path, seen)
		}
	}
}

func TestOpenCodeOAuthPollingExpiryAndCancellation(t *testing.T) {
	t.Run("pending and slow down", func(t *testing.T) {
		f, _ := openCodeFixture(t)
		f.Lock()
		f.polls = []string{"authorization_pending", "slow_down"}
		f.Unlock()
		began := time.Now()
		st := openCodeSignInTest(t, "opencode-go")
		if st.State != "done" {
			t.Fatalf("polling: %+v", st)
		}
		if time.Since(began) < 5*time.Second {
			t.Fatal("slow_down did not add five seconds to the poll")
		}
		n := 0
		for _, r := range f.calls() {
			if r.Path == "/console/auth/device/token" {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("token polls: %d", n)
		}
	})
	t.Run("device expires", func(t *testing.T) {
		f, _ := openCodeFixture(t)
		f.Lock()
		f.expiry = 1
		f.Unlock()
		devicePollFloor = 2 * time.Second
		st := openCodeSignInTest(t, "opencode-go")
		if st.State != "failed" || !strings.Contains(st.Error, "expired") || len(Logins("opencode-go")) != 0 {
			t.Fatalf("expiry: %+v", st)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		_, _ = openCodeFixture(t)
		legacy, _ := FromPreset("opencode-go")
		legacy.Key, legacy.Models = "kept-key", []string{"kept-model"}
		if err := Save(legacy); err != nil {
			t.Fatal(err)
		}
		devicePollFloor = time.Hour
		st, err := StartSignIn("opencode-go")
		if err != nil {
			t.Fatal(err)
		}
		CancelSignIn(st.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err = WaitSignIn(ctx, st.ID)
		if err != nil || st.State != "canceled" || len(Logins("opencode-go")) != 0 {
			t.Fatalf("cancel: %+v %v", st, err)
		}
		p, err := Find("opencode-go")
		if err != nil || p.Key != "kept-key" || !slices.Equal(p.Models, legacy.Models) {
			t.Fatalf("cancel changed provider: %+v %v", p, err)
		}
	})
}

func TestOpenCodeOAuthRefreshAndGoUsage(t *testing.T) {
	f, _ := openCodeFixture(t)
	st := openCodeSignInTest(t, "opencode-go")
	if st.State != "done" {
		t.Fatal(st)
	}
	old := openCodeSavedTest(t, "opencode-go", st.User)
	c, _ := openCodeSaved(old)
	c.Expires = time.Now().Add(-time.Minute)
	old.Auth, _ = json.Marshal(c)
	saveLogins(t, old)
	var rotations atomic.Int32
	f.Lock()
	f.refresh = func(w http.ResponseWriter, r *http.Request) {
		rotations.Add(1)
		io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600,"org_id":"org-b"}`)
	}
	f.Unlock()
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := openCodeToken(context.Background(), "opencode-go", st.User)
			if err == nil && (c.Access != "rotated-access" || c.OrgID != "org-b") {
				err = errors.New("wrong refreshed credential")
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if rotations.Load() != 1 {
		t.Fatalf("refresh rotations %d", rotations.Load())
	}
	kept, ok := openCodeSaved(openCodeSavedTest(t, "opencode-go", st.User))
	if !ok || kept.Refresh != "rotated-refresh" || kept.OrgID != "org-b" {
		t.Fatalf("stored: %+v", kept)
	}
	p, err := Find("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	for _, proto := range []Protocol{Chat, Responses, Anthropic, Gemini} {
		req, _ := http.NewRequest(http.MethodPost, p.Base(proto)+"/probe", strings.NewReader(`{}`))
		if err := p.Sign(context.Background(), req, proto, nil); err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		calls := f.calls()
		got := calls[len(calls)-1].Header
		if got.Get("X-Opencode-Org-Id") != "org-b" {
			t.Fatalf("%s org: %v", proto, got)
		}
		if proto == Gemini {
			if got.Get("X-Goog-Api-Key") != "rotated-access" || got.Get("Authorization") != "" {
				t.Fatalf("Gemini: %v", got)
			}
		} else if got.Get("Authorization") != "Bearer rotated-access" || got.Get("X-Api-Key") != "" {
			t.Fatalf("%s: %v", proto, got)
		}
	}
	q := loginQuota(context.Background(), Login{Agent: "opencode-go", User: st.User})
	if q.Error != "" || len(q.Windows) != 3 || q.Windows[0].Used != 23 || q.Windows[1].Used != 75 {
		t.Fatalf("usage: %+v", q)
	}
	calls := f.calls()
	last := calls[len(calls)-1]
	if last.Path != "/zen/go/v1/usage" || last.Header.Get("Authorization") != "Bearer rotated-access" || last.Header.Get("X-Opencode-Org-Id") != "org-b" || last.Header.Get("X-Org-Id") != "" {
		t.Fatalf("usage request: %+v", last)
	}
	usage := LoginUsage(context.Background(), "opencode-go")
	if len(usage[st.User].Windows) != 3 || usage[st.User].Windows[1].Used != 75 {
		t.Fatalf("account usage: %+v", usage)
	}
	usedCache.Lock()
	delete(usedCache.m, "opencode-go")
	delete(usedCache.at, "opencode-go")
	usedCache.Unlock()
	allowances := Allowances("opencode-go")
	usedCache.Lock()
	pending := usedCache.loading["opencode-go"]
	usedCache.Unlock()
	if pending != nil {
		<-pending
		allowances = Allowances("opencode-go")
	}
	if len(allowances[st.User]) != 3 || allowances[st.User][1].Used != 75 {
		t.Fatalf("routing allowance: %+v", allowances)
	}
}

func TestOpenCodeOAuthRefreshForgetAndReplacementWin(t *testing.T) {
	for _, mode := range []string{"forget", "replace"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := openCodeFixture(t)
			st := openCodeSignInTest(t, "opencode-go")
			old := openCodeSavedTest(t, "opencode-go", st.User)
			c, _ := openCodeSaved(old)
			c.Expires = time.Now().Add(-time.Minute)
			old.Auth, _ = json.Marshal(c)
			saveLogins(t, old)
			started, release := make(chan struct{}), make(chan struct{})
			var released sync.Once
			t.Cleanup(func() { released.Do(func() { close(release) }) })
			f.Lock()
			f.refresh = func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				io.WriteString(w, `{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":3600}`)
			}
			f.Unlock()
			result := make(chan error, 1)
			go func() { _, err := openCodeToken(context.Background(), "opencode-go", st.User); result <- err }()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("refresh did not reach the fake")
			}
			if mode == "forget" {
				if err := ForgetLogin("opencode-go", st.User); err != nil {
					t.Fatal(err)
				}
			} else {
				c.Access, c.Refresh = "replacement", "replacement-refresh"
				old.Auth, _ = json.Marshal(c)
				saveLogins(t, old)
			}
			released.Do(func() { close(release) })
			if err := <-result; err == nil {
				t.Fatal("stale refresh succeeded")
			}
			if mode == "forget" {
				if len(Logins("opencode-go")) != 0 {
					t.Fatal("forgotten credential returned")
				}
			} else if got, _ := openCodeSaved(openCodeSavedTest(t, "opencode-go", st.User)); got.Refresh != "replacement-refresh" {
				t.Fatalf("replacement lost: %+v", got)
			}
		})
	}
}

func TestOpenCodeOAuthRefreshRefusedVersusTransient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		lapsed bool
	}{
		{"revoked", 400, `{"error":"invalid_grant"}`, true},
		{"temporary", 503, `{"error":"temporarily_unavailable"}`, false},
		{"other bad request", 400, `{"error":"invalid_request"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := openCodeFixture(t)
			st := openCodeSignInTest(t, "opencode-go")
			old := openCodeSavedTest(t, "opencode-go", st.User)
			c, _ := openCodeSaved(old)
			c.Expires = time.Now().Add(-time.Minute)
			old.Auth, _ = json.Marshal(c)
			saveLogins(t, old)
			f.Lock()
			f.refresh = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }
			f.Unlock()
			_, err := openCodeToken(context.Background(), "opencode-go", st.User)
			if err == nil {
				t.Fatal("refresh unexpectedly succeeded")
			}
			ls := Logins("opencode-go")
			if len(ls) != 1 || (ls[0].Lapsed != "") != tc.lapsed {
				t.Fatalf("lapse: %+v error %v", ls, err)
			}
		})
	}
}

func TestOpenCodeOAuthConversionWriteFailuresKeepKey(t *testing.T) {
	for _, failure := range []string{"login", "provider"} {
		t.Run(failure, func(t *testing.T) {
			_, _ = openCodeFixture(t)
			legacy, _ := FromPreset("opencode-go")
			legacy.Key, legacy.Models, legacy.Was = "key-to-keep", []string{"model-to-keep"}, []string{"old-id"}
			if err := Save(legacy); err != nil {
				t.Fatal(err)
			}
			other := openCodeSignInTest(t, "opencode-zen")
			if other.State != "done" {
				t.Fatal(other)
			}
			if failure == "login" {
				old := openCodeWriteLogin
				openCodeWriteLogin = func([]savedLogin) error { return errors.New("login write failed") }
				t.Cleanup(func() { openCodeWriteLogin = old })
			} else {
				old := openCodeWriteProvider
				openCodeWriteProvider = func(file) error { return errors.New("provider write failed") }
				t.Cleanup(func() { openCodeWriteProvider = old })
			}
			st := openCodeSignInTest(t, "opencode-go")
			if st.State != "failed" || !strings.Contains(st.Error, "write failed") {
				t.Fatalf("outcome %+v", st)
			}
			p, err := Find("opencode-go")
			if err != nil || p.Account != nil || p.Key != "key-to-keep" || !slices.Equal(p.Models, legacy.Models) || !slices.Equal(p.Was, legacy.Was) {
				t.Fatalf("key provider: %+v %v", p, err)
			}
			if len(Logins("opencode-go")) != 0 {
				t.Fatal("failed login was published")
			}
			if ls := Logins("opencode-zen"); len(ls) != 1 || ls[0].User != other.User {
				t.Fatalf("unrelated account changed: %+v", ls)
			}
		})
	}
}

func TestOpenCodeOAuthCustomProviderRefused(t *testing.T) {
	f, _ := openCodeFixture(t)
	p := Provider{ID: "opencode-go", Name: "Custom", Chat: "https://my-relay.example/v1", Key: "custom-key"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := StartSignIn("opencode-go"); err == nil {
		t.Fatal("custom provider overwritten")
	}
	if len(f.calls()) != 0 {
		t.Fatal("custom provider triggered device authorization")
	}
	got, _ := Find("opencode-go")
	if got.Key != "custom-key" || got.Chat != p.Chat {
		t.Fatalf("custom changed: %+v", got)
	}
	if err := Delete("opencode-go"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeOAuthCustomProviderAppearingDuringSignIn(t *testing.T) {
	f, _ := openCodeFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	f.Lock()
	f.userHook = func() { close(entered); <-release }
	f.Unlock()
	st, err := StartSignIn("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("profile request did not arrive")
	}
	custom := Provider{ID: "opencode-go", Name: "Custom", Chat: "https://my-relay.example/v1", Key: "custom-key"}
	if err := Save(custom); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err = WaitSignIn(ctx, st.ID)
	if err != nil || st.State != "failed" || len(Logins("opencode-go")) != 0 {
		t.Fatalf("outcome %+v %v", st, err)
	}
	p, err := Find("opencode-go")
	if err != nil || p.Key != "custom-key" || p.Chat != custom.Chat {
		t.Fatalf("custom changed: %+v %v", p, err)
	}
}

func TestOpenCodeOAuthCustomProviderAppearingDuringCommit(t *testing.T) {
	_, _ = openCodeFixture(t)
	legacy, _ := FromPreset("opencode-go")
	legacy.Key = "legacy-key"
	if err := Save(legacy); err != nil {
		t.Fatal(err)
	}
	custom := Provider{ID: "opencode-go", Name: "Custom", Chat: "https://my-relay.example/v1", Key: "custom-key"}
	old := openCodeWriteLogin
	first := true
	openCodeWriteLogin = func(ls []savedLogin) error {
		if err := writeLogins(ls); err != nil {
			return err
		}
		if first {
			first = false
			return store(file{Providers: []Provider{custom}})
		}
		return nil
	}
	t.Cleanup(func() { openCodeWriteLogin = old })
	st := openCodeSignInTest(t, "opencode-go")
	if st.State != "failed" || len(Logins("opencode-go")) != 0 {
		t.Fatalf("outcome %+v", st)
	}
	p, err := Find("opencode-go")
	if err != nil || p.Key != "custom-key" || p.Chat != custom.Chat {
		t.Fatalf("custom overwritten: %+v %v", p, err)
	}
}

func TestOpenCodeOAuthRollbackFailureIsReported(t *testing.T) {
	_, _ = openCodeFixture(t)
	legacy, _ := FromPreset("opencode-go")
	legacy.Key = "legacy-key"
	if err := Save(legacy); err != nil {
		t.Fatal(err)
	}
	oldLogin, oldProvider := openCodeWriteLogin, openCodeWriteProvider
	first := true
	openCodeWriteLogin = func(ls []savedLogin) error {
		if first {
			first = false
			return writeLogins(ls)
		}
		return errors.New("rollback failed")
	}
	openCodeWriteProvider = func(file) error { return errors.New("provider failed") }
	t.Cleanup(func() { openCodeWriteLogin, openCodeWriteProvider = oldLogin, oldProvider })
	st := openCodeSignInTest(t, "opencode-go")
	if st.State != "failed" || !strings.Contains(st.Error, "provider failed") || !strings.Contains(st.Error, "rollback failed") {
		t.Fatalf("outcome %+v", st)
	}
	p, err := Find("opencode-go")
	if err != nil || p.Account != nil || p.Key != "legacy-key" {
		t.Fatalf("legacy key changed: %+v %v", p, err)
	}
}

func TestOpenCodeOAuthFailedConversionRestoresPreviousLogin(t *testing.T) {
	_, _ = openCodeFixture(t)
	first := openCodeSignInTest(t, "opencode-go")
	if first.State != "done" {
		t.Fatal(first)
	}
	previous := openCodeSavedTest(t, "opencode-go", first.User)
	credential, _ := openCodeSaved(previous)
	credential.Access, credential.Refresh = "old-access", "old-refresh"
	previous.Auth, _ = json.Marshal(credential)
	saveLogins(t, previous)
	legacy, _ := FromPreset("opencode-go")
	legacy.Key, legacy.Models = "legacy-key", []string{"legacy-model"}
	if err := Save(legacy); err != nil {
		t.Fatal(err)
	}
	old := openCodeWriteProvider
	openCodeWriteProvider = func(file) error { return errors.New("provider write failed") }
	t.Cleanup(func() { openCodeWriteProvider = old })
	st := openCodeSignInTest(t, "opencode-go")
	if st.State != "failed" {
		t.Fatalf("outcome %+v", st)
	}
	current := openCodeSavedTest(t, "opencode-go", first.User)
	if current.ID != previous.ID || !sameAuth(current.Auth, previous.Auth) {
		t.Fatalf("previous login lost: %+v", current)
	}
	p, err := Find("opencode-go")
	if err != nil || p.Account != nil || p.Key != "legacy-key" || !slices.Equal(p.Models, legacy.Models) {
		t.Fatalf("legacy provider changed: %+v %v", p, err)
	}
}

func TestOpenCodeOAuthAnonymousFallbackStripsWorkspace(t *testing.T) {
	p, _ := FromPreset("opencode-go")
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/inference/go/openai/v1/chat/completions", strings.NewReader(`{"model":"big-pickle"}`))
	req.Header.Set("Authorization", "Bearer private-oauth")
	req.Header.Set("X-Api-Key", "private-oauth")
	req.Header.Set("X-Goog-Api-Key", "private-oauth")
	req.Header.Set("Cookie", "session=private")
	req.Header.Set("X-Org-Id", "private-org")
	req.Header.Set("X-Opencode-Org-Id", "private-org")
	res := &http.Response{StatusCode: 400, Status: "400 Bad Request", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Upstream request failed: Model is unavailable."}}`))}
	called := false
	got, err := RetryOpenCodeFreeOnZen(p, Chat, req, res, func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.Path != "/inference/openai/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer public" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Org-Id") != "" || r.Header.Get("X-Opencode-Org-Id") != "" {
			t.Fatalf("fallback: %s %+v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err != nil || !called || got.StatusCode != 200 {
		t.Fatalf("fallback: %+v %v", got, err)
	}
	got.Body.Close()
}
