package library

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

func pluginMCPResult(scope, generation, token string) plugin.AccountResult {
	h := map[string]string{}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	data, _ := json.Marshal(map[string]any{"headers": h})
	return plugin.AccountResult{Context: plugin.AccountContext{Scope: scope, Generation: generation}, Data: data}
}

func withPluginMCPResolver(t *testing.T, fn func(context.Context, string, string, plugin.AccountRequest) (plugin.AccountResult, error)) {
	t.Helper()
	old := pluginMCPAccountOperation
	pluginMCPAccountOperation = fn
	t.Cleanup(func() { pluginMCPAccountOperation = old })
}

func TestPluginMCPReviewedTargetRejectsChangesBeforeAuth(t *testing.T) {
	for _, changeTarget := range []string{"http", "script", "preload equals", "preload separate", "executable", "environment", "missing trust"} {
		t.Run(changeTarget, func(t *testing.T) {
			h := hubSandbox(t)
			exe := filepath.Join(h, "program")
			script := filepath.Join(h, "script.js")
			preload := filepath.Join(h, "preload.js")
			write(t, exe, "program before")
			write(t, script, "script before")
			write(t, preload, "preload before")
			t.Setenv("MAGPIE_MCP_TEST_ENV", "environment before")
			s := Server{Name: "reviewed", Transport: "stdio", Command: exe, Args: []string{script},
				Env: map[string]string{"PRIVATE_VALUE": "${MAGPIE_MCP_TEST_ENV}"}}
			if changeTarget == "preload equals" {
				s.Args = []string{"--preload=" + preload, script}
			} else if changeTarget == "preload separate" {
				s.Args = []string{"--preload", preload, script}
			}
			if changeTarget == "http" {
				s = Server{Name: "reviewed", Transport: "http", URL: "https://example.test/mcp",
					Headers: map[string]string{"Authorization": "Bearer hidden-header"}}
			}
			if _, err := SaveServer("", s); err != nil {
				t.Fatal(err)
			}
			preview, err := LibraryPluginAccountTarget(s.Name)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(preview)
			if err != nil || strings.Contains(string(raw), "hidden-header") || strings.Contains(string(raw), "environment before") {
				t.Fatalf("preview disclosed a configured secret: %s %v", raw, err)
			}
			switch changeTarget {
			case "http":
				s.URL = "https://example.test/other"
				if _, err := SaveServer(s.Name, s); err != nil {
					t.Fatal(err)
				}
			case "script":
				write(t, script, "script after")
			case "preload equals", "preload separate":
				write(t, preload, "preload after")
			case "executable":
				write(t, exe, "program after")
			case "environment":
				t.Setenv("MAGPIE_MCP_TEST_ENV", "environment after")
			}
			var calls atomic.Int64
			withPluginMCPResolver(t, func(context.Context, string, string, plugin.AccountRequest) (plugin.AccountResult, error) {
				calls.Add(1)
				return pluginMCPResult("scope", "generation", "secret"), nil
			})
			ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
			ctx = WithPluginAccountExpectation(ctx, "scope", "generation")
			_, err = ServerPluginAccount(ctx, s.Name, &PluginAccount{Provider: "zcode", Account: "zcode#slot"}, changeTarget != "http" && changeTarget != "missing trust")
			if err == nil || calls.Load() != 0 || loaded(t).server(s.Name).PluginAccount != nil {
				t.Fatalf("%s accepted or looked up credentials: err=%v calls=%d", changeTarget, err, calls.Load())
			}
		})
	}
}

func TestPluginMCPTargetKeepsLoginEnvironmentReferences(t *testing.T) {
	h := hubSandbox(t)
	program := filepath.Join(h, "login-program")
	write(t, program, "program")
	for _, name := range []string{"MAGPIE_MCP_LOGIN_PROGRAM", "MAGPIE_MCP_LOGIN_SECRET"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	secret := "login-secret-sentinel"
	old := loginVars
	loginVars = func(names []string) map[string]string {
		values := map[string]string{}
		for _, name := range names {
			switch name {
			case "MAGPIE_MCP_LOGIN_PROGRAM":
				values[name] = program
			case "MAGPIE_MCP_LOGIN_SECRET":
				values[name] = secret
			}
		}
		return values
	}
	t.Cleanup(func() { loginVars = old })
	s := Server{Name: "login-references", Transport: "stdio", Command: "${MAGPIE_MCP_LOGIN_PROGRAM}",
		Args: []string{"--token=${MAGPIE_MCP_LOGIN_SECRET}"},
		Env:  map[string]string{"TOKEN": "${MAGPIE_MCP_LOGIN_SECRET}"}}
	if _, err := SaveServer("", s); err != nil {
		t.Fatal(err)
	}
	first, err := LibraryPluginAccountTarget(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(first)
	if err != nil || first.Command != s.Command || !slices.Equal(first.Args, s.Args) ||
		strings.Contains(string(raw), program) || strings.Contains(string(raw), secret) {
		t.Fatalf("preview exposed expanded login environment: %s %v", raw, err)
	}
	secret = "rotated-login-secret-sentinel"
	second, err := LibraryPluginAccountTarget(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(second)
	if err != nil || second.Fingerprint == first.Fingerprint || second.Command != s.Command ||
		!slices.Equal(second.Args, s.Args) || strings.Contains(string(raw), program) || strings.Contains(string(raw), secret) {
		t.Fatalf("login expansion was not privately fingerprinted: %s %v", raw, err)
	}
}

func TestPluginMCPOptionFilesAreFingerprintInputs(t *testing.T) {
	h := hubSandbox(t)
	program := filepath.Join(h, "program")
	bun := filepath.Join(h, "bun")
	node := filepath.Join(h, "node")
	for _, executable := range []string{program, bun, node} {
		write(t, executable, "executable")
	}
	preload := filepath.Join(h, "preload.js")
	t.Setenv("MAGPIE_MCP_PRELOAD_PATH", preload)
	for _, tc := range []struct {
		name, command string
		args          []string
	}{
		{"preload equals", program, []string{"--preload=" + preload}},
		{"preload separate", program, []string{"--preload", preload}},
		{"relative preload", bun, []string{"--preload=./preload.js"}},
		{"expanded preload", bun, []string{"--preload=${MAGPIE_MCP_PRELOAD_PATH}"}},
		{"bun short require", bun, []string{"-r", preload}},
		{"node short require", node, []string{"-r", preload}},
		{"node require separate", node, []string{"--require", preload}},
		{"node require equals", node, []string{"--require=" + preload}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, preload, "before")
			s := Server{Transport: "stdio", Command: tc.command, Args: tc.args}
			before, err := commandTrustDigest(&s)
			if err != nil {
				t.Fatal(err)
			}
			write(t, preload, "after")
			after, err := commandTrustDigest(&s)
			if err != nil || before == after {
				t.Fatalf("option file change was not fingerprinted: before=%s after=%s err=%v", before, after, err)
			}
		})
	}
}

func TestPluginMCPOptionFilesFailClosed(t *testing.T) {
	h := hubSandbox(t)
	bun := filepath.Join(h, "bun")
	node := filepath.Join(h, "node")
	write(t, bun, "executable")
	write(t, node, "executable")
	if err := os.Mkdir(filepath.Join(h, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, command string
		args          []string
	}{
		{"preload missing value", bun, []string{"--preload"}},
		{"preload empty equals", bun, []string{"--preload="}},
		{"preload empty separate", bun, []string{"--preload", ""}},
		{"preload missing equals", bun, []string{"--preload=./absent.js"}},
		{"preload missing separate", bun, []string{"--preload", "./absent.js"}},
		{"preload directory", bun, []string{"--preload=./directory"}},
		{"preload module specifier", bun, []string{"--preload=package-name"}},
		{"preload file URL", bun, []string{"--preload=file:///tmp/preload.js"}},
		{"node require missing value", node, []string{"-r"}},
		{"node require missing file", node, []string{"--require=./absent.js"}},
		{"node require builtin", node, []string{"--require=node:fs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Server{Transport: "stdio", Command: tc.command, Args: tc.args}
			if digest, err := commandTrustDigest(&s); err == nil {
				t.Fatalf("uninspectable option target was trusted: %s", digest)
			}
		})
	}
}

func TestPluginMCPBindingFencesContextAndKeepsRouterOff(t *testing.T) {
	hubSandbox(t)
	if _, err := SaveServer("", Server{Name: "reviewed", Transport: "http", URL: "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	preview, err := LibraryPluginAccountTarget("reviewed")
	if err != nil {
		t.Fatal(err)
	}
	var operations []string
	withPluginMCPResolver(t, func(_ context.Context, provider, account string, req plugin.AccountRequest) (plugin.AccountResult, error) {
		if provider != "zcode" || account != "zcode#slot" || req.Scope != "scope" || req.Generation != "generation" {
			t.Errorf("lost selected account or context: %s %s %+v", provider, account, req)
		}
		operations = append(operations, req.Operation)
		return pluginMCPResult("scope", "generation", "fixture-secret"), nil
	})
	ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
	ctx = WithPluginAccountExpectation(ctx, "scope", "generation")
	if _, err := ServerPluginAccount(ctx, "reviewed", &PluginAccount{Provider: "zcode", Account: "zcode#slot"}, false); err != nil {
		t.Fatal(err)
	}
	l := loaded(t)
	if !slices.Equal(operations, []string{"mcp.headers", "context"}) || l.MCPHub {
		t.Fatalf("operations %v, router enabled %v", operations, l.MCPHub)
	}
	if a := l.server("reviewed").PluginAccount; a == nil || a.Scope != "scope" || a.Provider != "zcode" || a.Account != "zcode#slot" {
		t.Fatalf("wrong saved binding: %+v", a)
	}
}

func TestPluginMCPBindingRejectsInflightAccountReplacement(t *testing.T) {
	hubSandbox(t)
	if _, err := SaveServer("", Server{Name: "reviewed", Transport: "http", URL: "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	preview, err := LibraryPluginAccountTarget("reviewed")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var current atomic.Value
	current.Store(plugin.AccountContext{Scope: "scope-a", Generation: "generation-a"})
	withPluginMCPResolver(t, func(_ context.Context, _, _ string, req plugin.AccountRequest) (plugin.AccountResult, error) {
		identity := current.Load().(plugin.AccountContext)
		if req.Operation == "mcp.headers" {
			close(entered)
			<-release
		}
		return pluginMCPResult(identity.Scope, identity.Generation, "secret"), nil
	})
	ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
	ctx = WithPluginAccountExpectation(ctx, "scope-a", "generation-a")
	result := make(chan error, 1)
	go func() {
		_, err := ServerPluginAccount(ctx, "reviewed", &PluginAccount{Provider: "zcode", Account: "zcode#slot"}, false)
		result <- err
	}()
	<-entered
	current.Store(plugin.AccountContext{Scope: "scope-b", Generation: "generation-b"})
	close(release)
	if err := <-result; err == nil {
		t.Fatal("an account replaced during credential resolution was bound")
	}
	if got := loaded(t).server("reviewed").PluginAccount; got != nil {
		t.Fatalf("stale account was saved: %+v", got)
	}
}

func TestPluginMCPBindingRejectsWrongContextReply(t *testing.T) {
	hubSandbox(t)
	if _, err := SaveServer("", Server{Name: "reviewed", Transport: "http", URL: "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	preview, err := LibraryPluginAccountTarget("reviewed")
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	withPluginMCPResolver(t, func(context.Context, string, string, plugin.AccountRequest) (plugin.AccountResult, error) {
		calls++
		return pluginMCPResult("scope", "different-generation", "secret"), nil
	})
	ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
	ctx = WithPluginAccountExpectation(ctx, "scope", "generation")
	if _, err := ServerPluginAccount(ctx, "reviewed", &PluginAccount{Provider: "zcode", Account: "zcode#slot"}, false); err == nil {
		t.Fatal("a reply for another generation was bound")
	}
	if calls != 1 || loaded(t).server("reviewed").PluginAccount != nil {
		t.Fatalf("wrong reply proceeded to commit: calls=%d", calls)
	}
}

func TestPluginMCPBindingRejectsChangesDuringAuthorization(t *testing.T) {
	for _, changed := range []string{"server", "script"} {
		t.Run(changed, func(t *testing.T) {
			h := hubSandbox(t)
			program := filepath.Join(h, "program")
			script := filepath.Join(h, "script.js")
			write(t, program, "program")
			write(t, script, "script before")
			s := Server{Name: "reviewed", Transport: "stdio", Command: program, Args: []string{script}}
			if changed == "server" {
				s = Server{Name: "reviewed", Transport: "http", URL: "https://example.test/mcp"}
			}
			if _, err := SaveServer("", s); err != nil {
				t.Fatal(err)
			}
			preview, err := LibraryPluginAccountTarget(s.Name)
			if err != nil {
				t.Fatal(err)
			}
			withPluginMCPResolver(t, func(_ context.Context, _, _ string, req plugin.AccountRequest) (plugin.AccountResult, error) {
				if req.Operation == "mcp.headers" {
					if changed == "server" {
						s.URL = "https://example.test/replaced"
						if _, err := SaveServer(s.Name, s); err != nil {
							t.Fatal(err)
						}
					} else {
						write(t, script, "script after")
					}
				}
				return pluginMCPResult("scope", "generation", "secret"), nil
			})
			ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
			ctx = WithPluginAccountExpectation(ctx, "scope", "generation")
			if _, err := ServerPluginAccount(ctx, s.Name, &PluginAccount{Provider: "zcode", Account: "zcode#slot"}, changed == "script"); err == nil {
				t.Fatal("a changed target was bound")
			}
			if got := loaded(t).server(s.Name).PluginAccount; got != nil {
				t.Fatalf("binding committed to a changed target: %+v", got)
			}
		})
	}
}

func TestPluginMCPBindingRejectsFileChangeDuringFinalContext(t *testing.T) {
	h := hubSandbox(t)
	program := filepath.Join(h, "program")
	script := filepath.Join(h, "server.js")
	write(t, program, "program")
	write(t, script, "script before")
	s := Server{Name: "reviewed", Transport: "stdio", Command: program, Args: []string{script}}
	if _, err := SaveServer("", s); err != nil {
		t.Fatal(err)
	}
	var replacementOps []string
	withPluginMCPResolver(t, func(_ context.Context, _, account string, req plugin.AccountRequest) (plugin.AccountResult, error) {
		if account == "zcode#replacement" {
			replacementOps = append(replacementOps, req.Operation)
			if req.Operation == "context" {
				write(t, script, "script replaced during final context")
			}
		}
		return pluginMCPResult("scope-"+account, "generation", "secret"), nil
	})
	oldAccount := &PluginAccount{Provider: "zcode", Account: "zcode#original"}
	if _, err := ServerPluginAccount(context.Background(), s.Name, oldAccount, true); err != nil {
		t.Fatal(err)
	}
	old := *loaded(t).server(s.Name).PluginAccount
	preview, err := LibraryPluginAccountTarget(s.Name)
	if err != nil || preview.Binding == nil || *preview.Binding != old {
		t.Fatalf("missing original binding from reviewed target: %+v %v", preview.Binding, err)
	}
	ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
	ctx = WithPluginAccountExpectation(ctx, "scope-zcode#replacement", "generation")
	replacement := &PluginAccount{Provider: "zcode", Account: "zcode#replacement"}
	if _, err := ServerPluginAccount(ctx, s.Name, replacement, true); err == nil {
		t.Fatal("a script changed during the final context call was bound")
	}
	if !slices.Equal(replacementOps, []string{"mcp.headers", "context"}) {
		t.Fatalf("did not change the script at the final context boundary: %v", replacementOps)
	}
	if got := loaded(t).server(s.Name).PluginAccount; got == nil || *got != old {
		t.Fatalf("the previous binding was not preserved: got %+v, want %+v", got, old)
	}
}

func TestPluginMCPReviewedBindingRejectsReplacement(t *testing.T) {
	for _, transport := range []string{"http", "stdio"} {
		for _, initial := range []string{"unbound", "account-a"} {
			for _, timing := range []string{"before auth", "during auth"} {
				t.Run(transport+"/"+initial+"/"+timing, func(t *testing.T) {
					h := hubSandbox(t)
					s := Server{Name: "reviewed", Transport: "http", URL: "https://example.test/mcp"}
					if transport == "stdio" {
						program := filepath.Join(h, "program")
						write(t, program, "program")
						s = Server{Name: "reviewed", Transport: "stdio", Command: program}
					}
					if _, err := SaveServer("", s); err != nil {
						t.Fatal(err)
					}
					entered, release := make(chan struct{}), make(chan struct{})
					var calls atomic.Int64
					withPluginMCPResolver(t, func(_ context.Context, _, account string, req plugin.AccountRequest) (plugin.AccountResult, error) {
						if account == "zcode#c" && req.Operation == "mcp.headers" {
							calls.Add(1)
							if timing == "during auth" {
								close(entered)
								<-release
							}
						}
						return pluginMCPResult("scope-"+account, "generation", "secret"), nil
					})
					bind := func(account string) error {
						_, err := ServerPluginAccount(context.Background(), s.Name, &PluginAccount{Provider: "zcode", Account: account}, transport == "stdio")
						return err
					}
					if initial == "account-a" {
						if err := bind("zcode#a"); err != nil {
							t.Fatal(err)
						}
					}
					preview, err := LibraryPluginAccountTarget(s.Name)
					if err != nil {
						t.Fatal(err)
					}
					if (preview.Binding == nil) != (initial == "unbound") {
						t.Fatalf("unexpected reviewed binding: %+v", preview.Binding)
					}
					ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
					ctx = WithPluginAccountExpectation(ctx, "scope-zcode#c", "generation")
					bindReviewed := func() error {
						_, err := ServerPluginAccount(ctx, s.Name, &PluginAccount{Provider: "zcode", Account: "zcode#c"}, transport == "stdio")
						return err
					}
					if timing == "before auth" {
						if err := bind("zcode#b"); err != nil {
							t.Fatal(err)
						}
						if err := bindReviewed(); err == nil || calls.Load() != 0 {
							t.Fatalf("changed binding reached authorization: err=%v calls=%d", err, calls.Load())
						}
					} else {
						result := make(chan error, 1)
						go func() { result <- bindReviewed() }()
						select {
						case <-entered:
						case err := <-result:
							t.Fatalf("binding ended before authorization: %v", err)
						}
						bErr := bind("zcode#b")
						close(release)
						err := <-result
						if bErr != nil {
							t.Fatal(bErr)
						}
						if err == nil || calls.Load() != 1 {
							t.Fatalf("changed binding was overwritten: err=%v calls=%d", err, calls.Load())
						}
					}
					if got := loaded(t).server(s.Name).PluginAccount; got == nil || got.Account != "zcode#b" {
						t.Fatalf("replacement was not retained: %+v", got)
					}
				})
			}
		}
	}
}

func TestPluginMCPUnbindComparesDisplayedBinding(t *testing.T) {
	hubSandbox(t)
	if _, err := SaveServer("", Server{Name: "reviewed", Transport: "http", URL: "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	withPluginMCPResolver(t, func(_ context.Context, _, account string, _ plugin.AccountRequest) (plugin.AccountResult, error) {
		return pluginMCPResult("scope-"+account, "generation", "secret"), nil
	})
	if _, err := ServerPluginAccount(context.Background(), "reviewed", &PluginAccount{Provider: "zcode", Account: "zcode#a"}, false); err != nil {
		t.Fatal(err)
	}
	preview, err := LibraryPluginAccountTarget("reviewed")
	if err != nil || preview.Binding == nil {
		t.Fatalf("missing displayed binding: %+v %v", preview, err)
	}
	if _, err := ServerPluginAccount(context.Background(), "reviewed", &PluginAccount{Provider: "zcode", Account: "zcode#b"}, false); err != nil {
		t.Fatal(err)
	}
	ctx := WithPluginAccountBinding(context.Background(), *preview.Binding)
	if _, err := ServerPluginAccount(ctx, "reviewed", nil, false); err == nil {
		t.Error("stale unbind removed a replacement")
	}
	if got := loaded(t).server("reviewed").PluginAccount; got == nil || got.Account != "zcode#b" {
		t.Errorf("replacement not retained: %+v", got)
	}
	if _, err := ServerPluginAccount(context.Background(), "reviewed", nil, false); err != nil {
		t.Errorf("CLI-style unbind failed: %v", err)
	}
}

func TestPluginMCPHTTPTransport(t *testing.T) {
	sandbox(t)
	for _, tc := range []struct {
		name       string
		kind       string
		status     int
		wantStatus int
		wantCalls  int
		wantAuth   []string
	}{
		{"untrusted", "untrusted_url", 200, 0, 0, nil},
		{"unavailable is anonymous", "unavailable", 401, 401, 1, []string{""}},
		{"401 refreshes once", "", 401, 200, 2, []string{"Bearer first", "Bearer rotated"}},
		{"403 is not retried", "", 403, 403, 1, []string{"Bearer first"}},
		{"redirect is not followed", "", 302, 302, 1, []string{"Bearer first"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var redirected atomic.Int64
			dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
			defer dest.Close()
			var mu sync.Mutex
			var auth []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				auth = append(auth, r.Header.Get("Authorization"))
				n := len(auth)
				mu.Unlock()
				if r.URL.RequestURI() != "/mcp?tenant=a%2Fb" {
					t.Errorf("destination changed after preflight: %s", r.URL.RequestURI())
				}
				for _, name := range []string{"X-Bigmodel-Authorization", "Bigmodel-Target-Type", "Bigmodel-Organization", "Bigmodel-Project", "X-Coding-Plan-Api-Key", "Mcp-Session-Id", "Mcp-Protocol-Version", "X-Request-Id", "X-Trace-Id"} {
					if v := r.Header.Get(name); v != "" {
						t.Errorf("static %s reached server: %q", name, v)
					}
				}
				if r.Header.Get("X-Other") != "kept" {
					t.Error("non-reserved header was lost")
				}
				if tc.status == 302 {
					http.Redirect(w, r, dest.URL, http.StatusFound)
				} else if tc.status == 401 && n > 1 {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(tc.status)
				}
			}))
			defer server.Close()
			endpoint := server.URL + "/mcp?tenant=a%2Fb"
			var resolutions int
			withPluginMCPResolver(t, func(_ context.Context, provider, account string, request plugin.AccountRequest) (plugin.AccountResult, error) {
				resolutions++
				if provider != "zcode" || account != "account-a" || request.Operation != "mcp.headers" || request.Params.(map[string]any)["url"] != endpoint || request.Scope != "scope-a" {
					t.Errorf("incorrect account request: %+v", request)
				}
				if resolutions > 1 && request.Generation != "epoch-a" {
					t.Errorf("retry lost captured generation: %+v", request)
				}
				if tc.kind != "" {
					return plugin.AccountResult{}, &plugin.AccountError{Kind: tc.kind}
				}
				token := "first"
				if resolutions > 1 {
					token = "rotated"
				}
				return pluginMCPResult("scope-a", "epoch-a", token), nil
			})
			s := &Server{Name: "official", Transport: "http", URL: endpoint,
				PluginAccount: &PluginAccount{Provider: "zcode", Account: "account-a", Scope: "scope-a"},
				Headers: map[string]string{"authorization": "static", "X-Bigmodel-Authorization": "static",
					"Bigmodel-Target-Type": "TEAM", "Bigmodel-Organization": "static", "Bigmodel-Project": "static",
					"X-Coding-Plan-Api-Key": "static", "Mcp-Session-Id": "static", "Mcp-Protocol-Version": "static",
					"X-Request-Id": "static", "X-Trace-Id": "static", "X-Other": "kept"}}
			session, bad := newHTTPSession(context.Background(), s)
			if bad != nil {
				t.Fatal(bad)
			}
			res, err := session.post(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			if tc.wantStatus == 0 {
				if err == nil {
					t.Fatal("untrusted destination was sent")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if res.StatusCode != tc.wantStatus {
					t.Fatalf("status %d, want %d", res.StatusCode, tc.wantStatus)
				}
			}
			mu.Lock()
			got := slices.Clone(auth)
			mu.Unlock()
			if len(got) != tc.wantCalls || !slices.Equal(got, tc.wantAuth) || redirected.Load() != 0 {
				t.Fatalf("requests %v, redirected %d", got, redirected.Load())
			}
		})
	}
}

func TestPluginMCPBindingAndHub(t *testing.T) {
	h := hubSandbox(t)
	write(t, filepath.Join(h, ".claude.json"), `{"mcpServers":{}}`)
	var requests atomic.Int64
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mcpHandler(t, false, &seen)(w, r)
	}))
	defer server.Close()
	var mu sync.Mutex
	scope, epoch, token := "scope-one", "epoch-one", "token-one"
	var bindCalls, requestCalls int
	withPluginMCPResolver(t, func(_ context.Context, provider, account string, req plugin.AccountRequest) (plugin.AccountResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if provider != "zcode" || account != "slot" || req.Operation != "mcp.headers" || req.Params.(map[string]any)["url"] != server.URL {
			t.Errorf("unexpected account operation: %+v", req)
		}
		if req.Scope == "" {
			bindCalls++
		} else {
			requestCalls++
			if req.Scope != scope {
				return plugin.AccountResult{}, &plugin.AccountError{Kind: "account_scope_changed"}
			}
			if req.Generation != "" && req.Generation != epoch {
				return plugin.AccountResult{}, &plugin.AccountError{Kind: "account_generation_changed"}
			}
		}
		return pluginMCPResult(scope, epoch, token), nil
	})
	ok(t)(SaveServer("", Server{Name: "official", Transport: "http", URL: server.URL,
		Headers: map[string]string{"Authorization": "Bearer old-static"}, Agents: []string{"claude"}}))
	res, err := ServerPluginAccount(context.Background(), "official", &PluginAccount{Provider: "zcode", Account: "slot"}, false)
	if err != nil || len(res.Problems) == 0 {
		t.Fatalf("router-off binding must report it cannot be given directly: %+v %v", res, err)
	}
	if cfg := read(t, filepath.Join(h, ".claude.json")); strings.Contains(cfg, server.URL) || strings.Contains(cfg, "old-static") {
		t.Fatalf("bound server left as a direct agent entry: %s", cfg)
	}
	stored := loaded(t).server("official")
	if stored.PluginAccount == nil || stored.PluginAccount.Scope != "scope-one" || stored.Headers["Authorization"] != "" {
		t.Fatalf("binding not stored safely: %+v", stored)
	}
	if bound, err := gateway.MCPAccountBound("official"); err != nil || !bound {
		t.Fatalf("relay did not read current binding: %v %v", bound, err)
	}
	cfg, err := ConfigOfServers("claude", []string{"official"})
	if err != nil || cfg.Text != "" || !slices.Equal(cfg.Skipped, []string{"official"}) {
		t.Fatalf("copy config exported a direct entry: %+v %v", cfg, err)
	}
	if _, err := ServerHub("official", true); err == nil {
		t.Fatal("bound server allowed direct mode")
	}
	ok(t)(SetMCPHub(true))
	if cfg := read(t, filepath.Join(h, ".claude.json")); !strings.Contains(cfg, `"magpie"`) || strings.Contains(cfg, server.URL) || strings.Contains(cfg, "token-one") {
		t.Fatalf("agent did not get only the hub: %s", cfg)
	}
	hub := theHub{}
	ctx := context.Background()
	if got := hubNames(hub.Tools(ctx, "claude")); !slices.Equal(got, []string{"official/a", "official/b"}) {
		t.Fatalf("bound tools: %v", got)
	}
	if _, err := hub.Call(ctx, "claude", "official", "a", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("local bound call: %v", err)
	}
	for _, visitor := range []gateway.HubVisitor{{Away: true}, {Away: true, Key: "full"}, {Away: true, Held: true}, {Held: true}} {
		other := gateway.WithHubVisitor(ctx, visitor)
		if got := hub.Tools(other, "claude"); len(got) != 0 {
			t.Errorf("%+v listed bound tools: %+v", visitor, got)
		}
		if _, err := hub.Call(other, "claude", "official", "a", json.RawMessage(`{}`)); err == nil {
			t.Errorf("%+v called the bound server", visitor)
		}
	}
	before := requests.Load()
	mu.Lock()
	scope, epoch, token = "scope-two", "epoch-two", "token-two"
	mu.Unlock()
	if _, err := hub.Call(ctx, "claude", "official", "a", json.RawMessage(`{}`)); err == nil {
		t.Fatal("replacement account used an old binding")
	}
	if got := hub.Tools(ctx, "claude"); len(got) != 0 {
		t.Fatal("replacement account retained the old tool list")
	}
	if requests.Load() != before {
		t.Fatal("replacement account reached the server")
	}
	mu.Lock()
	scope = "scope-one"
	mu.Unlock()
	if _, err := hub.Call(ctx, "claude", "official", "a", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("same-account relogin was not picked up: %v", err)
	}
	mu.Lock()
	if bindCalls != 1 || requestCalls < 2 {
		t.Errorf("binding/request resolutions: %d/%d", bindCalls, requestCalls)
	}
	mu.Unlock()
	edited := Server{Name: "official", Transport: "http", URL: server.URL, Agents: []string{"claude"}}
	ok(t)(SaveServer("official", edited))
	if loaded(t).server("official").PluginAccount == nil {
		t.Fatal("an editor omitting the binding removed it")
	}
	if _, err := SetMCPHub(false); err != nil {
		t.Fatal(err)
	}
	if cfg := read(t, filepath.Join(h, ".claude.json")); strings.Contains(cfg, server.URL) || strings.Contains(cfg, "token-two") {
		t.Fatalf("turning router off exported credentials or direct server: %s", cfg)
	}
	if _, err := ServerPluginAccount(ctx, "official", nil, false); err != nil {
		t.Fatal(err)
	}
	if bound, err := gateway.MCPAccountBound("official"); err != nil || bound {
		t.Fatalf("relay did not notice unbind: %v %v", bound, err)
	}
}

func TestPluginMCPHTTPFailuresAreSafe(t *testing.T) {
	sandbox(t)
	for _, tc := range []struct {
		status int
		detail string
	}{
		{http.StatusUnauthorized, "rejected"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusFound, "redirect"},
		{http.StatusOK, "invalid plugin-account MCP response"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "fixture-secret")
			}))
			defer server.Close()
			withPluginMCPResolver(t, func(context.Context, string, string, plugin.AccountRequest) (plugin.AccountResult, error) {
				return pluginMCPResult("scope", "epoch", "fixture-secret"), nil
			})
			s := &Server{Name: "official", Transport: "http", URL: server.URL,
				PluginAccount: &PluginAccount{Provider: "fixture", Account: "slot", Scope: "scope"}}
			c, bad := newHTTPSession(context.Background(), s)
			if bad != nil {
				t.Fatal(bad)
			}
			_, bad = c.call(context.Background(), 1, request(1, "initialize", initParams()))
			if bad == nil || !strings.Contains(bad.Detail, tc.detail) || strings.Contains(bad.Detail, "fixture-secret") {
				t.Fatalf("unsafe or misclassified response: %+v", bad)
			}
		})
	}
}

func TestPluginMCPInvalidBindingFailsClosed(t *testing.T) {
	sandbox(t)
	var requests, resolutions atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	withPluginMCPResolver(t, func(context.Context, string, string, plugin.AccountRequest) (plugin.AccountResult, error) {
		resolutions.Add(1)
		return pluginMCPResult("scope", "epoch", "fixture-token"), nil
	})
	s := &Server{Name: "broken", Transport: "sse", URL: server.URL,
		Headers:       map[string]string{"Authorization": "static-token"},
		PluginAccount: &PluginAccount{Provider: "fixture", Account: "slot", Scope: "scope"}}
	if _, err := dialHub(context.Background(), s, func() {}); err == nil {
		t.Fatal("an SSE binding bypassed the account transport")
	}
	if health := CheckServer(context.Background(), s); health.State != "error" {
		t.Fatalf("an SSE binding was checked directly: %+v", health)
	}
	s.Transport = "http"
	s.URL = strings.Replace(server.URL, "http://", "http://user:password@", 1)
	c, bad := newHTTPSession(context.Background(), s)
	if bad != nil {
		t.Fatal(bad)
	}
	if res, err := c.post(context.Background(), []byte(`{}`)); err == nil {
		res.Body.Close()
		t.Fatal("a URL with user information was sent")
	}
	if requests.Load() != 0 || resolutions.Load() != 0 {
		t.Fatal("invalid binding reached an account or server")
	}
}

func TestPluginMCPRealHostAndCredentialRotation(t *testing.T) {
	bun := testenv.Bun(t)
	hubSandbox(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
	t.Cleanup(plugin.Settle)
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() == "http://credential.invalid/material" {
			proxied.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	var mu sync.Mutex
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&msg) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, msg.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, msg.ID)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("NATIVE_MCP_TEST_ORIGIN", server.URL)
	file := filepath.Join(t.TempDir(), "native-mcp.mjs")
	source := `export const Plugin = async () => ({
  config: c => { c.provider ??= {}; c.provider["native-mcp-test"] = {
    name: "Fixture", npm: "@ai-sdk/openai-compatible", api: "https://fixture.invalid",
    models: {one: {name: "One", limit: {context: 1000, output: 100}}}
  }},
  auth: {
    provider: "native-mcp-test", methods: [{type: "api", label: "Fixture"}],
    accountOperations: ["mcp.headers"],
    accountPreflight({params}) {
      if (new URL(params.url).origin !== process.env.NATIVE_MCP_TEST_ORIGIN) throw new Error("untrusted")
    },
    async account(getAuth, _, {expectedScope, expectedGeneration}) {
      const a = await getAuth()
      if (expectedScope && expectedScope !== a.accountScope) {
        const e = new Error("scope changed"); e.kind = "account_scope_changed"; throw e
      }
      if (expectedGeneration && expectedGeneration !== a.accountEpoch) {
        const e = new Error("generation changed"); e.kind = "account_generation_changed"; throw e
      }
      const material = await fetch("http://credential.invalid/material")
      if (!material.ok) throw new Error("credential service unavailable")
      return {context: {scope: a.accountScope, generation: a.accountEpoch},
              data: {headers: {Authorization: "Bearer " + a.key}}}
    }
  }
})`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := plugin.Add(ctx, file); err != nil {
		t.Fatal(err)
	}
	account, err := plugin.Import(ctx, "native-mcp-test", map[string]any{
		"type": "api", "accountId": "fixture-account", "key": "fixture-first", "accountScope": "scope", "accountEpoch": "epoch-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	logins := provider.Logins(provider.PluginID("native-mcp-test"))
	if len(logins) != 1 {
		t.Fatalf("fixture account listing: %+v", logins)
	}
	p, err := provider.Find(provider.PluginID("native-mcp-test"))
	if err != nil {
		t.Fatal(err)
	}
	p.Proxy = "direct"
	p.AccountProxies = map[string]string{strings.ToLower(logins[0].User): proxy.URL}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	ok(t)(SaveServer("", Server{Name: "native", Transport: "http", URL: server.URL + "/mcp"}))
	if _, err := ServerPluginAccount(ctx, "native", &PluginAccount{Provider: "native-mcp-test", Account: account}, false); err != nil {
		t.Fatal(err)
	}
	ok(t)(SetMCPHub(true))
	hub := theHub{}
	if _, err := hub.Call(ctx, "", "native", "do", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	again, err := plugin.Import(ctx, "native-mcp-test", map[string]any{
		"type": "api", "accountId": "fixture-account", "key": "fixture-rotated", "accountScope": "scope", "accountEpoch": "epoch-two",
	})
	if err != nil || again != account {
		t.Fatalf("same-scope login did not keep the account slot: %q %q %v", again, account, err)
	}
	if _, err := hub.Call(ctx, "", "native", "do", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("same-account relogin failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(auth, "Bearer fixture-first") || !slices.Contains(auth, "Bearer fixture-rotated") {
		t.Fatalf("real plugin host did not rotate request credentials: %v", auth)
	}
	if proxied.Load() < 2 {
		t.Fatalf("credential acquisition bypassed the account proxy: %d", proxied.Load())
	}
}

func TestPluginMCPBunPreloadChangesRefuseBindingAndLaunch(t *testing.T) {
	bun := testenv.Bun(t)
	for _, form := range []string{"equals", "separate"} {
		t.Run(form, func(t *testing.T) {
			h := hubSandbox(t)
			t.Setenv("BUN_OPTIONS", "")
			t.Setenv("NODE_OPTIONS", "")
			preload := filepath.Join(h, "preload.mjs")
			script := filepath.Join(h, "server.mjs")
			marker := filepath.Join(h, "preload-ran")
			received := filepath.Join(h, "received-auth")
			preloadSource := func(value string) string {
				return fmt.Sprintf(`import {writeFileSync} from "node:fs"; writeFileSync(%q, %q);`, marker, value)
			}
			write(t, preload, preloadSource("original"))
			write(t, script, fmt.Sprintf(`
import {createInterface} from "node:readline";
import {appendFileSync} from "node:fs";
for await (const line of createInterface({input: process.stdin})) {
  const msg = JSON.parse(line);
  const auth = msg.params?._meta?.["com.zcode/official-mcp-auth"]?.headers?.Authorization;
  if (auth) appendFileSync(%q, auth + "\n");
  if (msg.method === "initialize") {
    console.log(JSON.stringify({jsonrpc: "2.0", id: msg.id, result: {protocolVersion: "2025-06-18"}}));
  }
}`, received))
			args := []string{"--preload=" + preload, script}
			if form == "separate" {
				args = []string{"--preload", preload, script}
			}
			s := Server{Name: "command", Transport: "stdio", Command: bun, Args: args}
			if _, err := SaveServer("", s); err != nil {
				t.Fatal(err)
			}
			preview, err := LibraryPluginAccountTarget(s.Name)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			var generation atomic.Value
			generation.Store("generation-one")
			withPluginMCPResolver(t, func(_ context.Context, _, _ string, req plugin.AccountRequest) (plugin.AccountResult, error) {
				calls.Add(1)
				current := generation.Load().(string)
				if req.Scope != "" && req.Scope != "scope" || req.Generation != "" && req.Generation != current {
					return plugin.AccountResult{}, &plugin.AccountError{Kind: "account_changed"}
				}
				return pluginMCPResult("scope", current, "fixture-token"), nil
			})
			ctx := WithPluginAccountTarget(context.Background(), preview.Fingerprint)
			ctx = WithPluginAccountExpectation(ctx, "scope", "generation-one")
			account := &PluginAccount{Provider: "zcode", Account: "zcode#slot"}
			write(t, preload, preloadSource("changed"))
			if _, err := ServerPluginAccount(ctx, s.Name, account, true); err == nil || calls.Load() != 0 {
				t.Fatalf("changed preload reached binding authorization: err=%v calls=%d", err, calls.Load())
			}
			if got := loaded(t).server(s.Name).PluginAccount; got != nil {
				t.Fatalf("changed preload committed a binding: %+v", got)
			}
			write(t, preload, preloadSource("original"))
			if _, err := ServerPluginAccount(ctx, s.Name, account, true); err != nil {
				t.Fatal(err)
			}
			bound := loaded(t).server(s.Name)
			if bound.PluginAccount == nil || bound.PluginAccount.Scope != "scope" || bound.PluginAccount.CommandDigest == "" {
				t.Fatalf("binding did not retain scope and command trust: %+v", bound.PluginAccount)
			}
			raw, err := json.Marshal(bound.PluginAccount)
			if err != nil || strings.Contains(string(raw), "generation-one") {
				t.Fatalf("binding stored a login generation: %s %v", raw, err)
			}
			// A relogin to the same identity changes the generation, not the
			// saved scope; the already bound command resolves fresh credentials.
			generation.Store("generation-two")
			conn, err := dialHub(context.Background(), bound, func() {})
			if err != nil {
				t.Fatalf("same-identity relogin prevented launch: %v", err)
			}
			conn.close()
			if data, err := os.ReadFile(marker); err != nil || string(data) != "original" {
				t.Fatalf("the reviewed Bun preload did not run: %q %v", data, err)
			}
			if data, err := os.ReadFile(received); err != nil || !strings.Contains(string(data), "Bearer fixture-token") {
				t.Fatalf("the reviewed server did not receive its scoped credentials: %q %v", data, err)
			}
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(received); err != nil {
				t.Fatal(err)
			}
			write(t, preload, preloadSource("changed"))
			before := calls.Load()
			next, err := dialHub(context.Background(), bound, func() {})
			if err == nil {
				next.close()
				t.Fatal("a changed Bun preload was launched")
			}
			if calls.Load() != before {
				t.Fatal("the changed command resolved credentials before launch refusal")
			}
			for _, path := range []string{marker, received} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("changed command left output %s: %v", path, err)
				}
			}
		})
	}
}

// The child speaks MCP over real stdio and checks the metadata it receives.
func TestPluginMCPStdioChild(t *testing.T) {
	if os.Getenv("MAGPIE_PLUGIN_MCP_CHILD") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Method == "" {
			os.Exit(3)
		}
		var auth struct {
			OK      bool              `json:"ok"`
			Headers map[string]string `json:"headers"`
			Reason  string            `json:"reason"`
		}
		if json.Unmarshal(msg.Params.Meta[pluginMCPMeta], &auth) != nil ||
			(auth.OK && auth.Headers["Authorization"] != "Bearer fixture-token") ||
			(!auth.OK && auth.Reason != "official_auth_unavailable") {
			os.Exit(4)
		}
		switch msg.Method {
		case "initialize":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`+"\n", msg.ID)
		case "tools/list":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"do"}]}}`+"\n", msg.ID)
		case "tools/call":
			if string(msg.Params.Meta["trace"]) != `"kept"` {
				os.Exit(5)
			}
			text := "authenticated"
			if !auth.OK {
				text = auth.Reason
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}]}}`+"\n", msg.ID, text)
		}
	}
	os.Exit(0)
}

func TestPluginMCPStdioTrustAndMetadata(t *testing.T) {
	h := hubSandbox(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(h, "script.js")
	write(t, script, "trusted contents")
	s := Server{Name: "command", Transport: "stdio", Command: executable,
		Args: []string{"-test.run=^TestPluginMCPStdioChild$", "--", script},
		Env:  map[string]string{"MAGPIE_PLUGIN_MCP_CHILD": "1"}, Agents: []string{"codex"}}
	ok(t)(SaveServer("", s))
	var calls atomic.Int64
	var unavailable atomic.Bool
	withPluginMCPResolver(t, func(_ context.Context, _, _ string, req plugin.AccountRequest) (plugin.AccountResult, error) {
		calls.Add(1)
		if req.Operation != "mcp.headers" || req.Params.(map[string]any)["stdio"] != true {
			t.Errorf("unexpected stdio account operation: %+v", req)
		}
		if req.Scope != "" && req.Scope != "scope" {
			t.Errorf("wrong stdio scope: %+v", req)
		}
		if unavailable.Load() {
			return plugin.AccountResult{}, &plugin.AccountError{Kind: "unavailable"}
		}
		return pluginMCPResult("scope", "epoch", "fixture-token"), nil
	})
	account := &PluginAccount{Provider: "zcode", Account: "slot"}
	if _, err := ServerPluginAccount(context.Background(), "command", account, false); err == nil || calls.Load() != 0 {
		t.Fatal("command was bound without trust before looking up credentials")
	}
	if _, err := ServerPluginAccount(context.Background(), "command", account, true); err != nil {
		t.Fatal(err)
	}
	bound := loaded(t).server("command")
	if bound.PluginAccount == nil || bound.PluginAccount.CommandDigest == "" || bound.PluginAccount.Scope != "scope" {
		t.Fatalf("missing command trust: %+v", bound.PluginAccount)
	}
	resetRuns(t)
	runGiven(map[string]runSpec{"command": {Command: executable}})
	if _, ok := runSpecOf("command"); ok {
		t.Fatal("a stale direct-run cache bypassed the account binding")
	}
	incoming := *bound
	incomingAccount := *bound.PluginAccount
	incomingAccount.CommandDigest = "forged-other-machine-trust"
	incoming.PluginAccount = &incomingAccount
	local := &Library{MCP: []*Server{bound}}
	local.putServers([]*Server{&incoming})
	if got := local.server("command").PluginAccount.CommandDigest; got != bound.PluginAccount.CommandDigest {
		t.Fatalf("sync did not preserve this machine's verified trust: %q", got)
	}
	other := &Library{}
	other.putServers([]*Server{&incoming})
	if got := other.server("command").PluginAccount.CommandDigest; got != "" {
		t.Fatalf("another machine's trust was imported: %q", got)
	}
	c, err := dialHub(context.Background(), bound, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if tools, err := listHubTools(context.Background(), c); err != nil || len(tools) != 1 {
		t.Fatalf("stdio tools: %v %v", tools, err)
	}
	result, err := callHubTool(context.Background(), c, "do", json.RawMessage(`{}`), map[string]any{"trace": "kept"}, true)
	if err != nil || hubText(t, result) != "authenticated" {
		t.Fatalf("stdio call: %s %v", result, err)
	}
	unavailable.Store(true)
	result, err = callHubTool(context.Background(), c, "do", json.RawMessage(`{}`), map[string]any{"trace": "kept"}, true)
	if err != nil || hubText(t, result) != "official_auth_unavailable" {
		t.Fatalf("stdio unavailable: %s %v", result, err)
	}
	write(t, script, "changed contents")
	unavailable.Store(false)
	before := calls.Load()
	result, err = callHubTool(context.Background(), c, "do", json.RawMessage(`{}`), map[string]any{"trace": "kept"}, true)
	if err != nil || hubText(t, result) != "authenticated" || calls.Load() != before+1 {
		t.Fatalf("launched command did not resolve current credentials: %s %v", result, err)
	}
	before = calls.Load()
	next, err := dialHub(context.Background(), bound, func() {})
	if err == nil {
		next.close()
		t.Fatal("a new launch accepted changed executable input")
	}
	if calls.Load() != before {
		t.Fatal("a changed command reached credentials before launch refusal")
	}
	bundle, err := Collect()
	if err != nil || len(bundle.MCP) != 1 || bundle.MCP[0].PluginAccount.CommandDigest != "" {
		t.Fatalf("command trust traveled in a carried library: %+v %v", bundle, err)
	}
}
