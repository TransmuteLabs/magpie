package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// noVision turns describing off, for a test of images turned away.
func noVision(t *testing.T) {
	t.Helper()
	s := settings.Load()
	s.Vision = "off"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
}

// eyeUp is an upstream with a text-only model and one that sees: the one
// that sees describes every image as a red square, or fails when fail is
// set; what each model is sent is kept.
type eyeUp struct {
	mu     sync.Mutex
	fail   bool
	bodies map[string][]string
}

func (u *eyeUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	model := modelOf(body)
	u.mu.Lock()
	if u.bodies == nil {
		u.bodies = map[string][]string{}
	}
	u.bodies[model] = append(u.bodies[model], string(body))
	fail := u.fail
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(model, "eye") && fail {
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"message":"eye is down"}}`)
		return
	}
	answer := "ok"
	if strings.HasSuffix(model, "eye") {
		answer = "A red square with the word HELLO in it."
	}
	if strings.Contains(string(body), `"stream":true`) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"`+answer+`"}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"`+answer+`"},"finish_reason":"stop"}]}`)
}

func (u *eyeUp) sent(model string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bodies[model]...)
}

func eyed(t *testing.T) (*Server, *eyeUp) {
	t.Helper()
	fresh(t)
	u := &eyeUp{}
	up := httptest.NewServer(u)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "probe", Name: "Probe", Chat: up.URL + "/v1", Key: "key", Models: []string{"text", "eye"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("probe", up.URL+"/v1", []catalog.Model{
		{ID: "text", ImageInput: imageInputBool(false)},
		{ID: "eye", Images: true, ImageInput: imageInputBool(true)},
	}); err != nil {
		t.Fatal(err)
	}
	return New(), u
}

func TestOnlyImageTurnPicksAutomaticDescriber(t *testing.T) {
	s, up := eyed(t)
	if err := os.WriteFile(autoPicksPath(), []byte(`{"seeded":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const text = `{"model":"probe/text","messages":[{"role":"user","content":"Read this."}]}`
	if code, body := postAs(t, s, "", text); code != http.StatusOK {
		t.Fatalf("text turn: %d %s", code, body)
	}
	if pick := heldFile(t).Vision; pick != "" {
		t.Errorf("text turn picked a vision provider: %s", pick)
	}

	const image = `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`
	if code, body := postAs(t, s, "", image); code != http.StatusOK {
		t.Fatalf("image turn: %d %s", code, body)
	}
	if pick, described := heldFile(t).Vision, up.sent("eye"); pick != "probe" || len(described) != 1 {
		t.Errorf("image turn picked %q and described %d images", pick, len(described))
	}
	if sent := up.sent("text"); len(sent) != 2 || !strings.Contains(sent[1], "HELLO") {
		t.Errorf("text model requests: %v", sent)
	}
}

func TestTextOnlyModelIsGivenTheImagesDescription(t *testing.T) {
	s, u := eyed(t)
	cases := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`},
		{"/v1/responses", `{"model":"probe/text","input":[{"role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`},
		{"/v1/messages", `{"model":"probe/text","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`},
		{"/v1beta/models/probe/text:generateContent", `{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
		if rec.Code != 200 {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	got := u.sent("text")
	if len(got) != len(cases) {
		t.Fatalf("text model sent %d requests, want %d", len(got), len(cases))
	}
	for i, b := range got {
		if !strings.Contains(b, "HELLO") || !strings.Contains(b, "probe/eye describes it") || strings.Contains(b, "aGVsbG8=") {
			t.Errorf("%s: text model was sent %s", cases[i].path, b)
		}
	}
	// the same image in four requests is described once
	if n := len(u.sent("eye")); n != 1 {
		t.Fatalf("the image was described %d times", n)
	}
	if !strings.Contains(u.sent("eye")[0], "aGVsbG8=") {
		t.Fatalf("the describer wasn't sent the image: %s", u.sent("eye")[0])
	}
}

func TestVisionSettingPicksTheDescriber(t *testing.T) {
	s, u := eyed(t)
	// a second model that sees, which the setting names
	up2 := &eyeUp{}
	srv := httptest.NewServer(up2)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "other", Name: "Other", Chat: srv.URL + "/v1", Key: "key", Models: []string{"bigeye"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("other", srv.URL+"/v1", []catalog.Model{{ID: "bigeye", Images: true, ImageInput: imageInputBool(true)}}); err != nil {
		t.Fatal(err)
	}
	st := settings.Load()
	st.Vision = "other/bigeye"
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	code, body := postAs(t, s, "", `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if len(u.sent("eye")) != 0 || len(up2.sent("bigeye")) != 1 {
		t.Fatalf("eye asked %d times, bigeye %d", len(u.sent("eye")), len(up2.sent("bigeye")))
	}
	if !strings.Contains(u.sent("text")[0], "other/bigeye describes it") {
		t.Fatalf("text model was sent %s", u.sent("text")[0])
	}
}

func TestImageThatCantBeDescribed(t *testing.T) {
	s, u := eyed(t)
	u.fail = true
	code, body := postAs(t, s, "", `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	if code != 502 || !strings.Contains(body, "couldn't describe the image") || len(u.sent("text")) != 0 {
		t.Fatalf("current image: %d %s; text model sent %d", code, body, len(u.sent("text")))
	}
	// an older turn's image that can't be described is left out
	code, body = postAs(t, s, "", `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,b2xk"}}]},{"role":"assistant","content":"seen"},{"role":"user","content":"and now?"}]}`)
	if code != 200 {
		t.Fatalf("older image: %d %s", code, body)
	}
	if got := u.sent("text"); len(got) != 1 || !strings.Contains(got[0], "couldn't be described") {
		t.Fatalf("text model was sent %v", got)
	}
	// a failure isn't kept
	u.mu.Lock()
	u.fail = false
	u.mu.Unlock()
	if code, body := postAs(t, s, "", `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`); code != 200 {
		t.Fatalf("after recovery: %d %s", code, body)
	}
}

func TestGroupsTextOnlyMemberIsGivenTheDescription(t *testing.T) {
	s, u := eyed(t)
	if err := provider.SaveGroup(provider.Group{Name: "Mixed", Members: []string{"probe/text", "probe/eye"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	code, body := postAs(t, s, "", `{"model":"group/mixed","messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if got := u.sent("text"); len(got) != 1 || !strings.Contains(got[0], "HELLO") {
		t.Fatalf("the group's first member was sent %v", got)
	}
}

func TestDescriberThatCantSeeIsntAskedToDescribe(t *testing.T) {
	s, u := eyed(t)
	// the setting names a text-only model: its own request is turned away,
	// not described by itself again and again, and the next model that
	// sees describes the image in its place (kelvinjason11 on X)
	st := settings.Load()
	st.Vision = "probe/text"
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	code, body := postAs(t, s, "", `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	if got := u.sent("text"); code != 200 || len(got) != 1 || !strings.Contains(got[0], "probe/eye describes it") {
		t.Fatalf("%d %s; text model sent %v", code, body, got)
	}
}

func TestSeerPicksAModelThatSees(t *testing.T) {
	eyed(t)
	if m, ok := seer(); !ok || m != "probe/eye" {
		t.Fatalf("seer = %q %v", m, ok)
	}
	noVision(t)
	if _, ok := seer(); ok {
		t.Fatal("seer with vision off")
	}
}

func TestCodexCatalogAdvertisesVisionWithoutRanking(t *testing.T) {
	server, _ := eyed(t)
	p, err := provider.Find("probe")
	if err != nil {
		t.Fatal(err)
	}
	var quotaReads atomic.Int64
	oldAllowance := keyAllowance
	keyAllowance = func(p provider.Provider) (provider.Allowance, bool) {
		if p.ID == "probe" {
			quotaReads.Add(1)
			return week(100, time.Now().Add(time.Hour)), true
		}
		return oldAllowance(p)
	}
	t.Cleanup(func() { keyAllowance = oldAllowance })
	if err := os.WriteFile(autoPicksPath(), []byte(`{"seeded":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	type model struct {
		ID         string   `json:"slug"`
		Modalities []string `json:"input_modalities"`
	}
	handler := server.Handler()
	for _, tc := range []struct {
		name, choice   string
		eye, describes bool
	}{
		{"automatic with a spent describer", "", true, true},
		{"missing pick with a spent fallback", "removed/eye", true, true},
		{"off with a candidate", "off", true, false},
		{"automatic without a candidate", "", false, false},
		{"missing pick without a fallback", "removed/eye", false, false},
		{"explicit pick of a served model", "probe/text", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := []string{"text"}
			if tc.eye {
				models = append(models, "eye")
			}
			p.Models = models
			if err := provider.Save(*p); err != nil {
				t.Fatal(err)
			}
			pickVision(t, tc.choice)
			rec := httptest.NewRecorder()
			before := quotaReads.Load()
			held := heldFile(t).Vision
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, CodexCatalogPath, nil))
			var list struct {
				Models []model `json:"models"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("codex catalog: %d %s (%v)", rec.Code, rec.Body, err)
			}
			idx := slices.IndexFunc(list.Models, func(m model) bool { return m.ID == "probe/text" })
			if idx < 0 || slices.Contains(list.Models[idx].Modalities, "image") != tc.describes {
				t.Errorf("probe/text not listed with image support %t", tc.describes)
			}
			if reads := quotaReads.Load() - before; reads != 0 {
				t.Errorf("codex catalog ranked vision by quota: %d allowance reads", reads)
			}
			if pick := heldFile(t).Vision; pick != held {
				t.Errorf("codex catalog changed the Image recognition provider from %q to %q", held, pick)
			}
		})
	}
}

// A long conversation sends every past screenshot again on each turn
// (fflake33, Discord: hundreds of them). Each is described once, not again
// on the next turn once there are more than 256.
func TestManyImagesDescribedOnce(t *testing.T) {
	s, u := eyed(t)
	const n = 300
	var parts []string
	for i := range n {
		img := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("screenshot %d", i)))
		parts = append(parts, `{"type":"input_image","image_url":"data:image/png;base64,`+img+`"}`)
	}
	body := `{"model":"probe/text","input":[{"role":"user","content":[{"type":"input_text","text":"read"},` + strings.Join(parts, ",") + `]}]}`
	for turn := 1; turn <= 2; turn++ {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("turn %d: %d %.300s", turn, rec.Code, rec.Body.String())
		}
	}
	if got := len(u.sent("eye")); got != n {
		t.Fatalf("%d images described %d times over two turns, want each once", n, got)
	}
}
