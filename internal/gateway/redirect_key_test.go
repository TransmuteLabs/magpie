package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// stealHost records requests to the redirect destination, except an
// empty anonymous GET to the fixture root.
type stealHost struct {
	*httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func anonymousFixtureRoot(method, uri string, headers http.Header, hasBody bool) bool {
	if method != http.MethodGet || uri != "/" || hasBody {
		return false
	}
	for name := range headers {
		switch name {
		case "Accept-Encoding", "Connection", "User-Agent":
		default:
			return false
		}
	}
	return true
}

func newStealHost(t *testing.T) *stealHost {
	t.Helper()
	s := &stealHost{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if anonymousFixtureRoot(r.Method, r.URL.RequestURI(), r.Header, r.ContentLength != 0 || len(r.TransferEncoding) != 0) {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Clone())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"stolen":true}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stealHost) reached() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.seen...)
}

func TestRedirectFixtureRecordsNonProbeRequests(t *testing.T) {
	steal := newStealHost(t)
	probeFixtureRoot(t, steal.Server)
	for _, target := range []struct {
		path string
		key  string
	}{
		{"/", "fixture-secret"},
		{"/steal", ""},
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, steal.URL+target.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if target.key != "" {
			request.Header.Set("X-Relay-Secret", target.key)
		}
		response, err := steal.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %s, want 200", target.path, response.Status)
		}
	}
	seen := steal.reached()
	if len(seen) != 2 || seen[0].Get("X-Relay-Secret") != "fixture-secret" {
		t.Fatalf("redirect fixture recorded %v, want the signed root request and the redirect target", seen)
	}
}

// A vendor answering the native route with a redirect to another host
// has its 3xx passed back to the caller as it came, and the host it names
// gets nothing: not the x-api-key and Authorization of an Anthropic
// provider, the x-goog-api-key of a Gemini one, nor a provider's own
// header. A redirect on the vendor's own origin is followed, signed.
func TestNativeRedirectKeepsTheKey(t *testing.T) {
	fresh(t)
	steal := newStealHost(t)
	up := newVendorAPI(t)
	up.answer = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/away"):
			w.Header().Set("Location", steal.URL+"/steal")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return true
		case strings.HasSuffix(r.URL.Path, "/here"):
			w.Header().Set("Location", strings.TrimSuffix(r.URL.Path, "/here")+"/landed")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return true
		}
		return false
	}
	hdr := map[string]string{"X-Relay-Secret": "relay-secret"}
	saveVendor(t, provider.Provider{ID: "claudeish", Name: "Claudeish", Anthropic: up.URL + "/anthropic", Key: "ak-secret", Models: []string{"c"}, Headers: hdr})
	saveVendor(t, provider.Provider{ID: "gem", Name: "Gem", Gemini: up.URL + "/v1beta", Key: "AIza-secret", Models: []string{"gemini-2.5-pro"}, Headers: hdr})
	h := nativeServe(t)
	for _, id := range []string{"claudeish", "gem"} {
		rec := nativeDo(h, "POST", "http://127.0.0.1:3425"+NativePath+id+"/v1/away", "127.0.0.1:50000", `{"model":"m"}`, "Content-Type", "application/json")
		if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != steal.URL+"/steal" {
			t.Errorf("%s: %d %q %s, want the vendor's 307 as it came", id, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
		before := len(up.asked())
		rec = nativeDo(h, "POST", "http://127.0.0.1:3425"+NativePath+id+"/v1/here", "127.0.0.1:50000", `{"model":"m"}`, "Content-Type", "application/json")
		if rec.Code != 201 {
			t.Fatalf("%s: a same-origin redirect: %d %s", id, rec.Code, rec.Body)
		}
		got := slices.DeleteFunc(up.asked()[before:], func(call vendorSeen) bool {
			return anonymousFixtureRoot(call.method, call.uri, call.header, call.body != "")
		})
		if len(got) != 2 || got[0].method != http.MethodPost || got[1].method != http.MethodPost || !strings.HasSuffix(got[1].uri, "/v1/landed") || got[1].body != `{"model":"m"}` || got[1].header.Get("X-Relay-Secret") != "relay-secret" {
			t.Fatalf("%s: a same-origin redirect reached %+v", id, got)
		}
		if id == "gem" && got[1].header.Get("X-Goog-Api-Key") != "AIza-secret" || id == "claudeish" && got[1].header.Get("X-Api-Key") != "ak-secret" {
			t.Errorf("%s: a same-origin redirect lost the key: %v", id, got[1].header)
		}
	}
	probeFixtureRoot(t, steal.Server)
	if seen := steal.reached(); len(seen) != 0 {
		t.Fatalf("the redirect's host was sent %d requests, the first with %v", len(seen), seen[0])
	}
}

// The same on a translated route: a vendor's redirect to another host
// is not followed, so a provider's own header and its key stay with it.
func TestChatRedirectKeepsTheKey(t *testing.T) {
	fresh(t)
	steal := newStealHost(t)
	up := newVendorAPI(t)
	up.answer = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		w.Header().Set("Location", steal.URL+"/v1/chat/completions")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return true
	}
	saveVendor(t, provider.Provider{ID: "vendor", Name: "Vendor", Chat: up.URL + "/v1", Key: "vk-secret", Models: []string{"m"}, Headers: map[string]string{"X-Relay-Secret": "relay-secret", "Api-Key": "vk-secret"}})
	h := nativeServe(t)
	rec := nativeDo(h, "POST", "http://127.0.0.1:3425/v1/chat/completions", "127.0.0.1:50000", `{"model":"vendor/m","messages":[{"role":"user","content":"hi"}]}`, "Content-Type", "application/json")
	if rec.Code == 200 {
		t.Errorf("the redirect's host answered: %s", rec.Body)
	}
	if len(up.asked()) == 0 {
		t.Fatal("the vendor was never asked")
	}
	if seen := steal.reached(); len(seen) != 0 {
		t.Fatalf("the redirect's host was sent %d requests, the first with %v", len(seen), seen[0])
	}
}
