package plugin

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

func TestFetch401KeepsResignedInAccount(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	abs, err := filepath.Abs("testdata/fake/index.js")
	if err != nil {
		t.Fatal(err)
	}
	const account = "fakeco"
	expires := time.Now().Add(time.Hour).UnixMilli()
	credentials := map[string]any{"type": "oauth", "access": "first-token", "refresh": "first-refresh", "expires": expires}
	arrived, reply := make(chan string, 1), make(chan struct{})
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- r.Header.Get("Authorization")
		select {
		case <-reply:
			http.Error(w, "token refused", http.StatusUnauthorized)
		case <-r.Context().Done():
		case <-ctx.Done():
		}
	}))
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, account, account, credentials); err != nil {
		t.Fatal(err)
	}
	before := savedEpoch(t, ctx, account)
	type result struct {
		status int
		said   string
		auth   FetchAuth
		err    error
	}
	responses := make(chan result, 1)
	go func() {
		res, auth, err := FetchWithAuth(ctx, FetchRequest{
			Provider: account, Account: account, Model: "fake-1", Method: http.MethodPost, URL: vendor.URL, Body: []byte(`{}`),
		})
		got := result{auth: auth, err: err}
		if err == nil {
			got.status, got.said = res.StatusCode, res.Header.Get("X-Magpie-Sign-In")
			for _, values := range res.Header {
				for _, value := range values {
					if strings.Contains(value, before) {
						t.Error("host sign-in metadata reached an HTTP header")
					}
				}
			}
			_, got.err = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		responses <- got
	}()
	select {
	case token := <-arrived:
		if token != "Bearer first-token" {
			t.Errorf("vendor saw %q", token)
		}
	case got := <-responses:
		t.Fatalf("fetch did not reach the fixture: %+v", got)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The vendor has answered neither the old request nor the new sign-in.
	// Re-entering the same credentials still moves the account's lifetime.
	if err := Restore(ctx, account, account, credentials); err != nil {
		t.Fatal(err)
	}
	close(reply)
	select {
	case got := <-responses:
		if got.err != nil || got.status != http.StatusUnauthorized {
			t.Fatalf("reply = %+v, want the vendor's 401", got)
		}
		if got.said != "kept" {
			t.Errorf("a 401 for the prior login said %q, want kept", got.said)
		}
		assertFetchEpoch(t, got.auth, account, before)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if saved := Auths(account)[account]; saved["expires"] != float64(expires) {
		t.Fatal("the 401 changed the new sign-in's expiry")
	}
}

func TestPiRefreshKeepsResignedInAccount(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const account = "pi-guard"
	arrived, reply := make(chan string, 1), make(chan struct{})
	vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		select {
		case arrived <- string(data):
		default:
			t.Error("Pi called its refresh callback again")
		}
		select {
		case <-reply:
			io.WriteString(w, `{"type":"oauth","access":"old-renewal","refresh":"old-refresh","expires":9000000000000000}`)
		case <-r.Context().Done():
		case <-ctx.Done():
		}
	}))
	t.Setenv("PI_GUARD_VENDOR", vendor.URL)
	root := filepath.Join(Dir(), account)
	if err := os.CopyFS(root, os.DirFS("testdata/pi-credential-conflict")); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(root, "node_modules", piAgent)
	ai := filepath.Join(root, "node_modules", "@earendil-works/pi-ai")
	writeFile(t, filepath.Join(agent, "package.json"), `{"type":"module","exports":"./index.mjs"}`)
	runtimeJS, err := os.ReadFile(filepath.Join(root, "coding-agent.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(agent, "index.mjs"), string(runtimeJS))
	writeFile(t, filepath.Join(ai, "package.json"), `{"type":"module","exports":"./index.mjs"}`)
	writeFile(t, filepath.Join(ai, "index.mjs"), `export const getSupportedThinkingLevels = () => []`)
	if _, err := Add(ctx, root); err != nil {
		t.Fatal(err)
	}
	if listed, err := Plugins(ctx); err != nil || len(listed) != 1 || listed[0].Error != "" {
		t.Fatalf("Pi package did not load: %+v, %v", listed, err)
	}
	restore := func(access string) {
		t.Helper()
		if err := Restore(ctx, account, account, map[string]any{"type": "oauth", "access": access, "refresh": "fixture-refresh", "expires": 1}); err != nil {
			t.Fatal(err)
		}
	}
	restore("old-login")
	type result struct {
		status int
		reply  string
		auth   FetchAuth
		err    error
	}
	responses := make(chan result, 1)
	go func() {
		res, auth, err := FetchWithAuth(ctx, FetchRequest{
			Provider: account, Account: account, Model: "pi-model", URL: "http://pi.magpie/v1/messages",
			Body: []byte(`{"model":"pi-model","stream":false,"messages":[{"role":"user","content":"hello"}]}`),
		})
		if err != nil {
			responses <- result{err: err}
			return
		}
		defer res.Body.Close()
		reply, err := io.ReadAll(res.Body)
		responses <- result{status: res.StatusCode, reply: string(reply), auth: auth, err: err}
	}()
	select {
	case token := <-arrived:
		if token != "old-login" {
			t.Errorf("Pi's refresh saw %q", token)
		}
	case got := <-responses:
		t.Fatalf("Pi did not reach the fixture: %+v", got)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	restore("new-login")
	close(reply)
	select {
	case got := <-responses:
		const want = `"text":"new-login"`
		if got.err != nil || got.status != http.StatusOK || !strings.Contains(got.reply, want) {
			t.Errorf("Pi resolved the wrong credential after reauth: %+v", got)
		}
		assertFetchEpoch(t, got.auth, account, savedEpoch(t, ctx, account))
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if saved := Auths(account)[account]; saved["access"] != "new-login" {
		t.Errorf("a late refresh replaced the new sign-in: %v", saved["access"])
	}
}

func TestSignInNoticesKeepTheObservedEpoch(t *testing.T) {
	for _, phase := range []string{"loader", "models"} {
		t.Run(phase, func(t *testing.T) {
			sandbox(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			const account = "refusal-proof"
			fixture, err := filepath.Abs("testdata/refusal-proof/index.js")
			if err != nil {
				t.Fatal(err)
			}
			arrived, reply := make(chan struct{}, 1), make(chan struct{})
			vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case arrived <- struct{}{}:
				default:
				}
				select {
				case <-reply:
					w.WriteHeader(http.StatusUnauthorized)
				case <-ctx.Done():
				case <-r.Context().Done():
				}
			}))
			t.Setenv("REFUSAL_PROOF_URL", vendor.URL)
			t.Setenv("REFUSAL_PROOF_PHASE", phase)
			if _, err := Add(ctx, fixture); err != nil {
				t.Fatal(err)
			}
			credentials := map[string]any{"type": "api", "key": "fixture-key"}
			signIn := func() string {
				t.Helper()
				if err := Restore(ctx, account, account, credentials); err != nil {
					t.Fatal(err)
				}
				return savedEpoch(t, ctx, account)
			}
			original := signIn()
			type notice struct {
				provider, account, said string
				epoch                   *string
			}
			notices := make(chan notice, 4)
			OnSignIn(func(provider, account, said string, epoch *string) {
				select {
				case notices <- notice{provider, account, said, epoch}:
				default:
				}
			})
			t.Cleanup(func() { OnSignIn(nil) })
			done := make(chan error, 1)
			go func() {
				res, err := Fetch(ctx, FetchRequest{Provider: account, Account: account, Model: "fixture", URL: vendor.URL})
				if err == nil {
					res.Body.Close()
				}
				done <- err
			}()
			select {
			case <-arrived:
			case err := <-done:
				t.Fatalf("%s did not reach the fixture: %v", phase, err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if signIn() == original {
				t.Fatal("repeating the same sign-in did not change its epoch")
			}
			close(reply)
			select {
			case got := <-notices:
				if got.provider != account || got.said != "expired" {
					t.Errorf("wrong notice: %+v", got)
				}
				assertFetchEpoch(t, FetchAuth{Account: got.account, Epoch: got.epoch}, account, original)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-done:
				if err == nil || phase == "models" && !strings.Contains(err.Error(), "sign-in changed") {
					t.Errorf("%s refusal: fetch returned %v, want preparation rejected", phase, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func savedEpoch(t *testing.T, ctx context.Context, account string) string {
	t.Helper()
	epochs, err := CurrentAuthEpochs(ctx)
	if err != nil || epochs[account] == "" {
		t.Fatalf("saved epoch for %s missing: %v", account, err)
	}
	return epochs[account]
}

func assertFetchEpoch(t *testing.T, got FetchAuth, account, epoch string) {
	t.Helper()
	if got.Account != account || got.Epoch == nil || *got.Epoch != epoch {
		t.Errorf("reply for %q has epoch present %t, matches observed account and epoch %t", got.Account,
			got.Epoch != nil, got.Account == account && got.Epoch != nil && *got.Epoch == epoch)
	}
}
