package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A look at Alma's providers decodes an answer once: the Agents page's
// model field, its check and its options each decoded all of Alma's
// providers again from the answer kept 5s. A new answer (Alma asked again
// after a change) is decoded anew.
func TestAlmaProvidersSeenDecodeOnce(t *testing.T) {
	var mu sync.Mutex
	gets, model := 0, "gpt-5.4"
	asked := func() int { mu.Lock(); defer mu.Unlock(); return gets }
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			return
		}
		mu.Lock()
		gets++
		m := model
		mu.Unlock()
		var b strings.Builder
		b.WriteString(`[{"id":"mg","name":"magpie","type":"openai","baseURL":"http://x/v1","enabled":true,"models":["` + m + `"],"availableModels":[`)
		for i := range 400 {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":"m-%d","name":"Model %d","capabilityOverrides":{"vision":true,"functionCalling":true}}`, i, i)
		}
		b.WriteString(`]}]`)
		rw.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldFor := almaAPI, almaReadFor
	almaAPI, almaReadFor = srv.URL, time.Minute
	t.Cleanup(func() { almaAPI, almaReadFor = oldAPI, oldFor })

	ps, err := almaProvidersSeen()
	if err != nil || len(ps) != 1 || ps[0].Name != "magpie" || len(ps[0].Known) != 400 || ps[0].Models[0] != "gpt-5.4" {
		t.Fatalf("first look: %v %+v", err, ps)
	}
	sameAnswer := func() {
		t.Helper()
		next, err := almaProvidersSeen()
		if err != nil || len(next) != len(ps) {
			t.Fatalf("same answer: %v %+v", err, next)
		}
		if &next[0] != &ps[0] {
			t.Fatal("the same answer was decoded into another provider slice")
		}
	}
	for range 6 {
		sameAnswer()
	}
	if g := asked(); g != 1 {
		t.Fatalf("looks asked Alma %d times", g)
	}
	// A fresh HTTP response with the same bytes reuses the decoded providers.
	// Checking their identity excludes the HTTP client's and fake server's
	// allocations, which vary independently of provider decoding.
	almaReadFor = 0
	for range 6 {
		sameAnswer()
	}
	if g := asked(); g != 7 {
		t.Fatalf("Alma asked %d times with nothing kept", g)
	}
	almaReadFor = time.Minute
	mu.Lock()
	model = "gpt-5.5"
	mu.Unlock()
	if err := almaDo("PUT", "/api/providers/mg", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	next, err := almaProvidersSeen()
	if err != nil || len(next) != 1 || next[0].Models[0] != "gpt-5.5" || asked() != 8 {
		t.Fatalf("after a change: %v, %v, %d asks", err, next, asked())
	}
	if &next[0] == &ps[0] || ps[0].Models[0] != "gpt-5.4" {
		t.Fatal("a changed answer reused or mutated the previous decoded providers")
	}
	// one that can't be read is an error, not the last good one
	almaReadFor = 0
	srv.Close()
	if _, err := almaProvidersSeen(); err == nil {
		t.Error("Alma gone: no error")
	}
}
