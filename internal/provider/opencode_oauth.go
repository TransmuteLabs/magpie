package provider

// OpenCode Console's device sign-in is owned by magpie, independently for
// Go and Zen. Only logins.json holds its tokens; OpenCode's files are not read.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/filelock"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const openCodeClientID = "opencode-cli"

var (
	openCodeRootOverride  atomic.Pointer[openCodeRoots]
	openCodeRefreshMu     = filelock.Mutex{Path: func() string { return loginsPath() + ".opencode.lock" }}
	openCodeWriteLogin    = writeLogins
	openCodeWriteProvider = store
)

type openCodeRoots struct{ console, inference string }

func openCodeURLs() openCodeRoots {
	if roots := openCodeRootOverride.Load(); roots != nil {
		return *roots
	}
	return openCodeRoots{console: "https://opencode.ai/console", inference: "https://opencode.ai"}
}

// OpenCodeOAuthForTest points the Console and inference origin at a fake.
// Console is the full Console root; inference is the origin without a path.
func OpenCodeOAuthForTest(console, inference string) func() {
	old := openCodeRootOverride.Swap(&openCodeRoots{strings.TrimRight(console, "/"), strings.TrimRight(inference, "/")})
	return func() { openCodeRootOverride.Store(old) }
}

func isOpenCodeOAuth(id string) bool { return id == "opencode-go" || id == "opencode-zen" }

// openCodeLock also checks that an OS lock was acquired. filelock.Mutex's
// normal fail-open behavior must not spend the same rotating token twice.
func openCodeLock() (func(), error) {
	openCodeRefreshMu.Lock()
	unlock, held := filelock.Lock(loginsPath()+".opencode.guard.lock", filelock.Wait)
	if !held {
		openCodeRefreshMu.Unlock()
		return nil, errors.New("could not lock OpenCode sign-ins; try again")
	}
	return func() { unlock(); openCodeRefreshMu.Unlock() }, nil
}

type openCodeCredential struct {
	Access       string    `json:"access"`
	Refresh      string    `json:"refresh"`
	Expires      time.Time `json:"expires"`
	UserID       string    `json:"userId"`
	Email        string    `json:"email"`
	InitialOrgID string    `json:"initialOrgId,omitempty"`
	OrgID        string    `json:"orgId,omitempty"`
	OrgName      string    `json:"orgName,omitempty"`
}

type openCodeTokenReply struct {
	Access      string  `json:"access_token"`
	Refresh     string  `json:"refresh_token"`
	ExpiresIn   int64   `json:"expires_in"`
	OrgID       *string `json:"org_id"`
	Error       string  `json:"error"`
	Description string  `json:"error_description"`
}

func (t openCodeTokenReply) valid() bool {
	return t.Access != "" && t.Refresh != "" && t.ExpiresIn > 0
}

func openCodeSaved(l savedLogin) (openCodeCredential, bool) {
	var c openCodeCredential
	err := json.Unmarshal(l.Auth, &c)
	return c, err == nil && c.Access != "" && c.Refresh != "" && c.UserID != ""
}

func openCodeLogins(agent string) []sideLogin {
	return sideLogins(agent, "", func(l savedLogin) bool { _, ok := openCodeSaved(l); return ok })
}

func openCodeAccount(agent string) (Provider, bool) {
	ls := openCodeLogins(agent)
	if len(ls) == 0 {
		return Provider{}, false
	}
	return openCodeProvider(ls[0]), true
}

func openCodeAlsoOn(agent string) []Provider {
	var out []Provider
	for _, l := range openCodeLogins(agent) {
		if !l.Active && l.On {
			out = append(out, openCodeProvider(l))
		}
	}
	return out
}

func openCodeProvider(l sideLogin) Provider {
	p, _ := FromPreset(l.Agent)
	root := openCodeURLs().inference + "/inference"
	if l.Agent == "opencode-go" {
		root += "/go"
	}
	p.Chat, p.Responses = root+"/openai/v1", root+"/openai/v1"
	p.Anthropic, p.Gemini = root+"/anthropic", root+"/google/v1beta"
	if p.Decide != "" {
		p.Decide = root + "/systemone/v1"
	}
	p.Account = &Account{Agent: l.Agent, User: l.User, Plan: l.Plan}
	// These are the same catalog models used by the preset. OpenCode's
	// public model metadata determines which protocol each model speaks.
	catalogID := p.Catalog
	p.Account.models = func() []catalog.Model { return catalog.Provider(catalogID) }
	p.Account.magpieList = true
	return p
}

// Only a matching preset at its actual vendor routes can be converted. A
// custom provider using the same ID must never be shadowed or rewritten.
func openCodeProviderAllowed(agent string, p Provider) bool {
	if !hasEndpoint(p) {
		return p.Key == "" && len(p.Keys) == 0
	}
	if p.Preset != agent {
		return false
	}
	for _, base := range []string{p.Chat, p.Responses, p.Anthropic, p.Gemini, p.Decide} {
		if base == "" {
			continue
		}
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "https" || u.Host != "opencode.ai" || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		path := strings.TrimRight(u.Path, "/")
		root := "/zen"
		infer := "/inference"
		if agent == "opencode-go" {
			root += "/go"
			infer += "/go"
		}
		allowed := []string{root, root + "/v1", infer, infer + "/v1", infer + "/openai/v1", infer + "/anthropic", infer + "/google/v1beta"}
		if !slices.Contains(allowed, path) {
			return false
		}
	}
	return true
}

func checkOpenCodeFile(agent string, f file) error {
	for _, p := range f.Providers {
		if p.ID == agent && !openCodeProviderAllowed(agent, p) {
			return fmt.Errorf("%s is already a custom provider; rename it before signing in to OpenCode", agent)
		}
	}
	return nil
}

func checkOpenCodeProvider(agent string) error {
	f, err := read()
	if err != nil {
		return err
	}
	return checkOpenCodeFile(agent, f)
}

func openCodeRequest(ctx context.Context, method, target, token string, body any) (int, []byte, error) {
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, data)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", OpenCodeUserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := Keyed(http.DefaultClient).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, b, err
}

func startOpenCodeSignIn(s *signInFlow) error {
	hardDeadline := time.Now().Add(signInTimeout)
	ctx, cancel := context.WithCancel(s.via(context.Background()))
	start, end := context.WithDeadline(ctx, hardDeadline)
	status, b, err := openCodeRequest(start, http.MethodPost, openCodeURLs().console+"/auth/device/code", "", map[string]any{"client_id": openCodeClientID, "supports_org_scope": true})
	end()
	if err != nil {
		cancel()
		return err
	}
	if status < 200 || status >= 300 {
		cancel()
		return fmt.Errorf("OpenCode device authorization: %s", APIError(b, http.StatusText(status)))
	}
	var device struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		URI        string `json:"verification_uri_complete"`
		ExpiresIn  int64  `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	if err := json.Unmarshal(b, &device); err != nil {
		cancel()
		return err
	}
	base, err := url.Parse(openCodeURLs().console + "/")
	if err != nil {
		cancel()
		return err
	}
	uri, err := url.Parse(device.URI)
	if err != nil || device.URI == "" {
		cancel()
		return errors.New("OpenCode gave an invalid verification URL")
	}
	uri = base.ResolveReference(uri)
	if (uri.Scheme != "http" && uri.Scheme != "https") || uri.Host == "" || uri.User != nil || device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 {
		cancel()
		return errors.New("OpenCode gave an invalid device authorization")
	}
	s.st.URL, s.st.Code = uri.String(), device.UserCode
	s.stop = cancel
	deadline, timeoutMessage := hardDeadline, "the sign-in timed out; start it again"
	if device.ExpiresIn <= int64(signInTimeout/time.Second) {
		expires := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
		if expires.Before(deadline) {
			deadline, timeoutMessage = expires, "the OpenCode device code expired; start again"
		}
	}
	go pollOpenCodeSignIn(ctx, s, device.DeviceCode, devicePoll(device.Interval), deadline, timeoutMessage)
	return nil
}

func pollOpenCodeSignIn(ctx context.Context, s *signInFlow, device string, interval time.Duration, deadline time.Time, timeoutMessage string) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	fail := func(err error) { s.finish(SignInState{State: "failed", Error: err.Error()}) }
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fail(errors.New(timeoutMessage))
			}
			return
		case <-timer.C:
		}
		status, b, err := openCodeRequest(ctx, http.MethodPost, openCodeURLs().console+"/auth/device/token", "", map[string]any{
			"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": device, "client_id": openCodeClientID,
		})
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fail(errors.New(timeoutMessage))
			}
			return
		}
		if err != nil {
			fail(err)
			return
		}
		var token openCodeTokenReply
		if err := json.Unmarshal(b, &token); err != nil {
			fail(err)
			return
		}
		switch token.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "":
		default:
			fail(fmt.Errorf("OpenCode device authorization: %s", token.Error))
			return
		}
		if status < 200 || status >= 300 || !token.valid() {
			fail(errors.New("OpenCode did not return a valid device token"))
			return
		}
		l, err := openCodeIdentity(ctx, s.status().Agent, token)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = keepLogin(ctx, func() error { return commitOpenCodeLogin(&l) })
		}
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fail(errors.New(timeoutMessage))
			} else if ctx.Err() == nil {
				fail(err)
			}
			return
		}
		s.finish(SignInState{State: "done", User: l.User, Plan: l.Plan})
		return
	}
}

func openCodeIdentity(ctx context.Context, agent string, token openCodeTokenReply) (savedLogin, error) {
	get := func(path string, out any) error {
		status, b, err := openCodeRequest(ctx, http.MethodGet, openCodeURLs().console+path, token.Access, nil)
		if err != nil {
			return err
		}
		if status < 200 || status >= 300 {
			return fmt.Errorf("OpenCode %s: %s", path, APIError(b, http.StatusText(status)))
		}
		return json.Unmarshal(b, out)
	}
	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	type organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var orgs []organization
	if err := get("/api/user", &user); err != nil {
		return savedLogin{}, err
	}
	if err := get("/api/orgs", &orgs); err != nil {
		return savedLogin{}, err
	}
	if user.ID == "" || user.Email == "" {
		return savedLogin{}, errors.New("OpenCode did not identify the user")
	}
	orgID, orgName := "", ""
	if token.OrgID != nil {
		for _, org := range orgs {
			if org.ID == *token.OrgID {
				orgID, orgName = org.ID, org.Name
				break
			}
		}
		if orgID == "" {
			return savedLogin{}, fmt.Errorf("OpenCode organization not found: %s", *token.OrgID)
		}
	} else if len(orgs) > 0 {
		org := orgs[0]
		if len(orgs) > 1 {
			// OpenCode uses localeCompare, not UTF-8 byte order.
			order := collate.New(language.English)
			org = slices.MinFunc(orgs, func(a, b organization) int {
				if cmp := order.CompareString(a.Name, b.Name); cmp != 0 {
					return cmp
				}
				return order.CompareString(a.ID, b.ID)
			})
		}
		orgID, orgName = org.ID, org.Name
	}
	c := openCodeCredential{Access: token.Access, Refresh: token.Refresh, Expires: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second), UserID: user.ID, Email: user.Email, InitialOrgID: orgID, OrgID: orgID, OrgName: orgName}
	auth, err := json.Marshal(c)
	if err != nil {
		return savedLogin{}, err
	}
	sum := sha256.Sum256([]byte("OpenCode Console\x00" + agent + "\x00" + user.ID + "\x00" + orgID))
	id := hex.EncodeToString(sum[:16])
	return savedLogin{ID: id, Agent: agent, User: user.Email + " [" + id[:12] + "]", Plan: orgName, Auth: auth, On: true}, nil
}

// commitOpenCodeLogin publishes the login before replacing a legacy key by
// picks. A failed second write restores only this credential, not other edits.
func commitOpenCodeLogin(l *savedLogin) error {
	unlock, err := openCodeLock()
	if err != nil {
		return err
	}
	defer unlock()
	f, err := read()
	if err != nil {
		return err
	}
	if err := checkOpenCodeFile(l.Agent, f); err != nil {
		return err
	}
	loginsMu.Lock()
	ls := readLogins()
	i := slices.IndexFunc(ls, func(old savedLogin) bool { return old.Agent == l.Agent && loginID(old) == l.ID })
	var previous savedLogin
	had := i >= 0
	l.Seen = time.Now().UTC().Truncate(time.Second)
	if had {
		previous = ls[i]
		l.User, l.On, l.First, l.Order = previous.User, previous.On, previous.First, previous.Order
		ls[i] = *l
	} else {
		ls = append(ls, *l)
	}
	err = openCodeWriteLogin(ls)
	loginsMu.Unlock()
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		loginsMu.Lock()
		current := readLogins()
		j := slices.IndexFunc(current, func(v savedLogin) bool { return v.Agent == l.Agent && loginID(v) == l.ID && sameAuth(v.Auth, l.Auth) })
		var restoreErr error
		if j >= 0 {
			if had {
				current[j] = previous
			} else {
				current = append(current[:j], current[j+1:]...)
			}
			restoreErr = openCodeWriteLogin(current)
		}
		loginsMu.Unlock()
		if restoreErr != nil {
			return fmt.Errorf("saving OpenCode provider: %w; restoring its login also failed: %v", cause, restoreErr)
		}
		return fmt.Errorf("saving OpenCode provider: %w", cause)
	}
	// A provider may have been edited while logins.json was being written.
	// Take the latest picks and refuse a custom replacement before conversion.
	f, err = read()
	if err != nil {
		return rollback(err)
	}
	if err = checkOpenCodeFile(l.Agent, f); err != nil {
		return rollback(err)
	}
	idx := slices.IndexFunc(f.Providers, func(p Provider) bool { return p.ID == l.Agent })
	if idx >= 0 && hasEndpoint(f.Providers[idx]) {
		old := f.Providers[idx]
		f.Providers[idx] = accountSettings(Provider{ID: old.ID, Hidden: old.Hidden, Quiet: old.Quiet, Tucked: old.Tucked}, old)
		if err = openCodeWriteProvider(f); err != nil {
			return rollback(err)
		}
	}
	StaleAllowance(l.Agent, l.User)
	return nil
}

func openCodeLogin(agent, user string) (savedLogin, openCodeCredential, error) {
	loginsMu.Lock()
	ls := readLogins()
	i := slices.IndexFunc(ls, func(l savedLogin) bool { return l.Agent == agent && strings.EqualFold(l.User, user) })
	if i < 0 {
		loginsMu.Unlock()
		return savedLogin{}, openCodeCredential{}, errors.New("OpenCode account was removed")
	}
	old := ls[i]
	c, ok := openCodeSaved(old)
	loginsMu.Unlock()
	if !ok {
		return savedLogin{}, openCodeCredential{}, errors.New("OpenCode account has no valid sign-in")
	}
	if old.Lapsed != "" && strings.HasPrefix(old.Refused, revokedPrefix) {
		return savedLogin{}, openCodeCredential{}, errors.New(old.Lapsed)
	}
	return old, c, nil
}

// Fresh tokens need no OS lock. Refresh rereads after the cross-process lock;
// a forgotten or replaced credential wins over an in-flight refresh.
func openCodeToken(ctx context.Context, agent, user string) (openCodeCredential, error) {
	old, c, err := openCodeLogin(agent, user)
	if err != nil || time.Until(c.Expires) > 30*time.Second {
		return c, err
	}
	unlock, err := openCodeLock()
	if err != nil {
		return openCodeCredential{}, err
	}
	defer unlock()
	old, c, err = openCodeLogin(agent, user)
	if err != nil || time.Until(c.Expires) > 30*time.Second {
		return c, err
	}
	status, b, err := openCodeRequest(ctx, http.MethodPost, openCodeURLs().console+"/auth/device/token", "", map[string]any{
		"grant_type": "refresh_token", "refresh_token": c.Refresh, "client_id": openCodeClientID,
	})
	if err != nil {
		return openCodeCredential{}, err
	}
	var tok openCodeTokenReply
	if err := json.Unmarshal(b, &tok); err != nil {
		return openCodeCredential{}, fmt.Errorf("OpenCode refresh: %w", err)
	}
	refused := status == http.StatusBadRequest && tok.Error == "invalid_grant"
	if !refused && (status < 200 || status >= 300 || !tok.valid()) {
		return openCodeCredential{}, fmt.Errorf("OpenCode refresh failed (%d): %s", status, firstNonEmpty(tok.Error, http.StatusText(status)))
	}
	oldOrg := c.OrgID
	if !refused {
		c.Access, c.Refresh, c.Expires = tok.Access, tok.Refresh, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)
		if tok.OrgID != nil && *tok.OrgID != c.OrgID {
			c.OrgID, c.OrgName = *tok.OrgID, *tok.OrgID
		}
	}
	loginsMu.Lock()
	ls := readLogins()
	i := slices.IndexFunc(ls, func(l savedLogin) bool {
		return l.Agent == agent && loginID(l) == loginID(old) && sameAuth(l.Auth, old.Auth)
	})
	if i < 0 {
		loginsMu.Unlock()
		return openCodeCredential{}, errors.New("OpenCode account changed while refreshing; try again")
	}
	if refused {
		ls[i].Lapsed, ls[i].Refused = revokedText(user), revokedPrefix+bearerMark(c.Access)
	} else {
		auth, err := json.Marshal(c)
		if err != nil {
			loginsMu.Unlock()
			return openCodeCredential{}, err
		}
		ls[i].Auth, ls[i].Plan, ls[i].Lapsed, ls[i].Refused = auth, c.OrgName, "", ""
		ls[i].Renewed = time.Now().UTC()
	}
	err = writeLogins(ls)
	loginsMu.Unlock()
	if err != nil {
		return openCodeCredential{}, err
	}
	if c.OrgID != oldOrg {
		StaleAllowance(agent, user)
	}
	if refused {
		return openCodeCredential{}, refreshRefused(revokedText(user))
	}
	return c, nil
}

func clearOpenCodeAuth(h http.Header) {
	for k := range h {
		for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key", "Cookie", "X-Org-Id", "X-Opencode-Org-Id"} {
			if strings.EqualFold(k, name) {
				delete(h, k)
				break
			}
		}
	}
}

func (p Provider) signOpenCode(ctx context.Context, req *http.Request, proto Protocol, passed []string) error {
	c, err := openCodeToken(ctx, p.Account.Agent, p.Account.User)
	if err != nil {
		return err
	}
	OpenCodeSession(req.Header, req.URL.String())
	for k, v := range p.SentHeaders(ctx) {
		if slices.ContainsFunc(passed, func(n string) bool { return strings.EqualFold(n, k) }) {
			continue
		}
		if ListHeader(k) {
			MergeList(req.Header, k, v)
		} else {
			catalog.PutUserHeader(req.Header, k, v)
		}
	}
	clearOpenCodeAuth(req.Header)
	if proto == Gemini {
		req.Header.Set("x-goog-api-key", c.Access)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.Access)
	}
	if c.OrgID != "" {
		req.Header.Set("x-opencode-org-id", c.OrgID)
	}
	return nil
}

func openCodeQuota(ctx context.Context, l Login) SubscriptionQuota {
	q := SubscriptionQuota{Provider: l.Agent, Plan: l.Plan, Windows: []QuotaWindow{}}
	c, err := openCodeToken(ctx, l.Agent, l.User)
	if err == nil {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, openCodeURLs().inference+"/zen/go/v1/usage", nil)
		if err == nil {
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Authorization", "Bearer "+c.Access)
			if c.OrgID != "" {
				req.Header.Set("x-opencode-org-id", c.OrgID)
			}
			OpenCodeSession(req.Header, req.URL.String())
			var res *http.Response
			res, err = Keyed(http.DefaultClient).Do(req)
			if err == nil {
				defer res.Body.Close()
				var b []byte
				b, err = io.ReadAll(io.LimitReader(res.Body, 1<<20))
				if err == nil && (res.StatusCode < 200 || res.StatusCode >= 300) {
					err = fmt.Errorf("OpenCode Go usage: %s", APIError(b, res.Status))
				}
				if err == nil {
					_, q.Windows, err = readOpenCodeGo(b)
					q.Plan = c.OrgName
				}
			}
		}
	}
	if err != nil {
		q.Error = err.Error()
	}
	return q
}
