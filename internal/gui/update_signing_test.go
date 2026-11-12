package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/update"
)

func TestUpdateSigningRoutes(t *testing.T) {
	policy, ids, use, off, app, platform := localSigningPolicy, localSigningIDs, localSigningUse, localSigningOff, localSigningApp, localSigningOS
	t.Cleanup(func() {
		localSigningPolicy, localSigningIDs, localSigningUse, localSigningOff, localSigningApp, localSigningOS = policy, ids, use, off, app, platform
	})
	localSigningOS = "darwin"
	localSigningApp = func() string { return "/test/magpie.app" }
	var c *update.SigningConfig
	listCalls, useCalls, offCalls := 0, 0, 0
	localSigningPolicy = func() (*update.SigningConfig, error) { return c, nil }
	localSigningIDs = func(ctx context.Context) ([]update.SigningIdentity, error) {
		listCalls++
		return []update.SigningIdentity{{Fingerprint: strings.Repeat("A", 40), Name: "Same label", Team: "TEAM"}}, nil
	}
	localSigningUse = func(ctx context.Context, fp, bundle string) error {
		useCalls++
		if bundle != "/test/magpie.app" {
			t.Fatal(bundle)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded keychain call")
		}
		c = &update.SigningConfig{Fingerprint: fp, Team: "TEAM", BundleID: "com.example.magpie", Feed: "https://feed.test/latest"}
		return nil
	}
	localSigningOff = func() error { offCalls++; c = nil; return nil }
	mux := http.NewServeMux()
	updateSigningRoutes(mux)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := request("GET", "/api/update/signing", "")
	if w.Code != 200 || listCalls != 0 {
		t.Fatalf("status enumerated keys: %d %d", w.Code, listCalls)
	}
	w = request("GET", "/api/update/signing?identities=1", "")
	if w.Code != 200 || listCalls != 1 || !strings.Contains(w.Body.String(), "Same label") {
		t.Fatal(w.Code, w.Body.String())
	}
	fp := strings.Repeat("A", 40)
	w = request("POST", "/api/update/signing", `{"fingerprint":"`+fp+`"}`)
	if w.Code != 200 || useCalls != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	var state signingStatus
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Configured || state.Fingerprint != fp {
		t.Fatal(state)
	}
	w = request("POST", "/api/update/signing", `{"off":true}`)
	if w.Code != 200 || offCalls != 1 || c != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, bad := range []string{`{}`, `{"fingerprint":"A","off":true}`, `{"off":false}`, `{"off":true,"unknown":1}`, `{"off":true} {}`} {
		if got := request("POST", "/api/update/signing", bad); got.Code != 400 {
			t.Fatalf("bad request %s: %d", bad, got.Code)
		}
	}
	if useCalls != 1 || offCalls != 1 {
		t.Fatal("malformed request mutated policy")
	}
	localSigningPolicy = func() (*update.SigningConfig, error) { return nil, errors.New("policy unreadable") }
	if got := request("GET", "/api/update/signing", ""); got.Code != 400 || !strings.Contains(got.Body.String(), "policy unreadable") {
		t.Fatal(got.Code, got.Body.String())
	}
	localSigningOS = "linux"
	if got := request("POST", "/api/update/signing", `{"off":true}`); got.Code != 400 {
		t.Fatal("unsupported platform accepted", got.Code)
	}
	if got := request("GET", "/api/update/signing?identities=1", ""); got.Code != 200 || listCalls != 1 {
		t.Fatal("nonmac keychain call")
	}
}
