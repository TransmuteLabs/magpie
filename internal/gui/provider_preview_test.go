package gui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

func previewPost(t *testing.T, handler http.Handler, family string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/provider/"+family, strings.NewReader(string(raw))))
	return response
}

func TestPreviewKeyRotatedSaveUsesActualSlotIdentity(t *testing.T) {
	sandboxHome(t)
	before, after := provider.KeyID("sk-a"), provider.KeyID("sk-unsaved-synthetic-sentinel")
	original := provider.Provider{
		ID: "relay", Name: "Relay", Chat: "http://127.0.0.1:1/v1", Key: "sk-a", KeyName: "Work",
		Keys:          []provider.KeyAccount{{Key: "sk-b", Name: "Spare"}},
		Models:        []string{"gpt-5.1", "model-other"},
		AccountModels: map[string][]string{before: {"gpt-5.1"}},
		AccountProxies: map[string]string{
			before: "direct",
		},
		KeyPriceRates: map[string]float64{before: 0.5},
	}
	if err := provider.Save(original); err != nil {
		t.Fatal(err)
	}
	handler := http.NewServeMux()
	providerRoutes(handler, nil)
	response := httptest.NewRecorder()
	body, err := json.Marshal(map[string]string{"id": "relay", "ref": before, "key": "sk-unsaved-synthetic-sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/keys/replace", strings.NewReader(string(body))))
	if response.Code != http.StatusOK {
		t.Fatalf("actual replacement: %d %s", response.Code, response.Body)
	}
	var listed providersJSON
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var rotated *providerJSON
	for index := range listed.Providers {
		if listed.Providers[index].ID == "relay" {
			rotated = &listed.Providers[index]
		}
	}
	if rotated == nil || len(rotated.KeyList) != 2 || rotated.KeyList[0].ID != after || rotated.KeyList[1].ID != provider.KeyID("sk-b") {
		t.Fatalf("replacement reply must retain order and rotate the actual fingerprint: %+v", rotated)
	}
	if strings.Contains(response.Body.String(), "sk-unsaved-synthetic-sentinel") || strings.Contains(response.Body.String(), `"id":"`+before+`"`) {
		t.Fatal("replacement reply leaked the key or retained its retired slot")
	}
	if got := previewPost(t, handler, "accountmodels", map[string]any{"id": "relay", "account": before, "allow": []string{"model-other"}}); got.Code == http.StatusOK {
		t.Fatal("retired account fingerprint accepted pending model picks")
	}
	picks := []string{"gpt-5.1", "model-other"}
	if got := previewPost(t, handler, "accountmodels", map[string]any{"id": "relay", "account": after, "allow": picks}); got.Code != http.StatusOK {
		t.Fatalf("current account picks: %d %s", got.Code, got.Body)
	}
	save := map[string]any{
		"id": "relay", "from": "relay", "name": "Relay", "key": "", "chat": original.Chat,
		"icon": "generic", "models": picks, "headers": map[string]string{},
		"accountProxies": map[string]string{after: "direct"}, "keyPriceRates": map[string]float64{after: 0.5},
	}
	if got := previewPost(t, handler, "save", save); got.Code != http.StatusOK {
		t.Fatalf("Save after replacement: %d %s", got.Code, got.Body)
	}
	saved, err := provider.Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.AccountProxies, map[string]string{after: "direct"}) ||
		!reflect.DeepEqual(saved.KeyPriceRates, map[string]float64{after: 0.5}) ||
		!reflect.DeepEqual(saved.AccountModels, map[string][]string{after: picks}) {
		t.Fatalf("actual Save lost the accepted key's settings: proxies=%v rates=%v models=%v", saved.AccountProxies, saved.KeyPriceRates, saved.AccountModels)
	}
	if saved.Key != "sk-unsaved-synthetic-sentinel" || len(saved.Keys) != 1 || saved.Keys[0].Key != "sk-b" {
		t.Fatal("Save changed credential ownership or its neighboring slot")
	}
}

func TestPreviewKeyEditorOwnerIsServed(t *testing.T) {
	handler := Handler(nil, nil)
	for _, name := range []string{"app.js", "providerpreview.js", "tlscerts.js"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+name, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "javascript") {
			t.Fatalf("preview dependency %s: %d %s", name, response.Code, response.Header().Get("Content-Type"))
		}
		if name == "app.js" && !strings.Contains(response.Body.String(), `import("./providerpreview.js")`) {
			t.Fatal("editor does not load its preview owner")
		}
		if name == "providerpreview.js" && !strings.Contains(response.Body.String(), "export function providerPreview(") {
			t.Fatal("preview owner module was not embedded")
		}
	}
}

func TestPreviewKeyRoutesRejectUnknownSlot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var connections atomic.Int32
	vendor := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/models") {
			response.Write([]byte(`{"data":[{"id":"m"}]}`))
		} else {
			response.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}
	}))
	vendor.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	vendor.StartTLS()
	t.Cleanup(vendor.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "sk-a", Chat: vendor.URL + "/v1", Proxy: "direct", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	handler := http.NewServeMux()
	providerRoutes(handler, nil)
	for _, family := range []string{"models", "test", "detect", "list", "peekcert"} {
		t.Run(family, func(t *testing.T) {
			before := connections.Load()
			response := previewPost(t, handler, family, map[string]any{
				"id": "relay", "typed": true, "typedKeyRef": "missing", "key": "sk-unsaved",
				"chat": vendor.URL + "/v1", "base": vendor.URL + "/v1", "insecureTLS": true,
				"test": []string{"m"}, "model": "m",
			})
			if response.Code != http.StatusBadRequest {
				t.Errorf("unknown preview slot must refuse before work: status %d", response.Code)
			}
			if got := connections.Load() - before; got != 0 {
				t.Errorf("unknown preview slot reached vendor: %d connections", got)
			}
		})
	}
}

func TestPreviewKeySecondaryUnionKeepsOriginalFirstWriteMarks(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var credentials []string
	vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		credentials = append(credentials, key)
		mu.Unlock()
		model := map[string]string{"sk-a": "model-a", "sk-b": "old-b", "sk-new-b": "model-b"}[key]
		if model == "" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		json.NewEncoder(response).Encode(map[string]any{"data": []map[string]string{{"id": model}}})
	}))
	t.Cleanup(vendor.Close)
	original := provider.Provider{
		ID: "relay", Name: "Relay", Key: "sk-a", KeyName: "first", Chat: vendor.URL + "/v1", Proxy: "direct",
		Keys:   []provider.KeyAccount{{Name: "second", Key: "sk-b"}, {Name: "off", Key: "sk-c", Off: true}},
		Models: []string{"model-a", "model-b"},
	}
	if err := provider.Save(original); err != nil {
		t.Fatal(err)
	}
	handler := http.NewServeMux()
	providerRoutes(handler, nil)
	previous := catalog.ChangedHook()
	var publications [][]catalog.Model
	var publicationErrors []error
	catalog.SetChanged(func() {
		raw, err := os.ReadFile(catalog.LivePath("relay"))
		if os.IsNotExist(err) {
			return
		}
		var written struct{ Models []catalog.Model }
		if err == nil {
			err = json.Unmarshal(raw, &written)
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			publicationErrors = append(publicationErrors, err)
		} else {
			publications = append(publications, written.Models)
		}
	})
	t.Cleanup(func() { catalog.SetChanged(previous) })
	response := previewPost(t, handler, "models", map[string]any{
		"id": "relay", "typed": true, "typedKeyRef": provider.KeyID("sk-b"), "key": "sk-new-b", "chat": original.Chat,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("preview models: status %d", response.Code)
	}
	mu.Lock()
	seen := slices.Clone(credentials)
	written := slices.Clone(publications)
	errs := slices.Clone(publicationErrors)
	mu.Unlock()
	slices.Sort(seen)
	if !reflect.DeepEqual(seen, []string{"sk-a", "sk-new-b"}) {
		t.Error("aggregate preview did not ask exactly the original first and temporary second slots")
	}
	if len(errs) != 0 || len(written) == 0 {
		t.Fatalf("first-write observation missing or unreadable: publications=%d errors=%d", len(written), len(errs))
	}
	want := map[string][]string{"model-a": {provider.KeyID("sk-a")}, "model-b": {provider.KeyID("sk-b")}}
	for publication, models := range written {
		got := map[string][]string{}
		for _, model := range models {
			got[model.ID] = model.Keys
			if slices.Contains(model.Keys, provider.KeyID("sk-new-b")) {
				t.Errorf("publication %d persisted temporary key fingerprint before normalization", publication)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("publication %d did not preserve aggregate union and original-slot marks", publication)
		}
	}
	saved, err := provider.Find("relay")
	if err != nil || saved.Key != original.Key || !reflect.DeepEqual(saved.Keys, original.Keys) {
		t.Fatal("preview persisted key values or slot attributes")
	}
}

func TestPreviewKeyNamedAccountAndRefusal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var asked []string
	vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		asked = append(asked, key)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		if key == "sk-refused" {
			response.WriteHeader(http.StatusUnauthorized)
			response.Write([]byte(`{"error":{"message":"synthetic key refused"}}`))
			return
		}
		response.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(vendor.Close)
	saved := provider.Provider{ID: "relay", Name: "Relay", Key: "sk-a", KeyName: "first", KeyProtocol: provider.Chat,
		Keys: []provider.KeyAccount{{Key: "sk-b", Name: "second", Protocol: provider.Chat}},
		Chat: vendor.URL + "/v1", Proxy: "direct", Models: []string{"m"}}
	if err := provider.Save(saved); err != nil {
		t.Fatal(err)
	}
	handler := http.NewServeMux()
	providerRoutes(handler, nil)
	for _, row := range []struct {
		name, edited, key, account, want string
		refuse                           bool
	}{
		{"edited-selected", provider.KeyID("sk-b"), "sk-new-b", provider.KeyID("sk-b"), "sk-new-b", false},
		{"B-while-editing-A", "", "", provider.KeyID("sk-b"), "sk-b", false},
		{"contradictory-edited-A-target-B", provider.KeyID("sk-a"), "sk-new-a", provider.KeyID("sk-b"), "", true},
		{"refused-no-fallback", provider.KeyID("sk-b"), "sk-refused", provider.KeyID("sk-b"), "sk-refused", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			mu.Lock()
			asked = nil
			mu.Unlock()
			body := map[string]any{"id": "relay", "typed": true, "key": row.key, "account": row.account, "test": []string{"m"}}
			if row.edited != "" {
				body["typedKeyRef"] = row.edited
			}
			response := previewPost(t, handler, "test", body)
			mu.Lock()
			credentials := slices.Clone(asked)
			mu.Unlock()
			if row.refuse {
				if response.Code != http.StatusBadRequest || len(credentials) != 0 {
					t.Fatal("contradictory edited slot and explicit target was not refused before transport")
				}
				return
			}
			if response.Code != http.StatusOK || len(credentials) != 1 || credentials[0] != row.want {
				t.Fatal("preview did not use exactly the named credential without fallback")
			}
			var result struct{ Results []provider.Result }
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Results) != 1 {
				t.Fatal("preview result shape changed")
			}
			if result.Results[0].OK != (row.key != "sk-refused") {
				t.Fatal("preview refusal was hidden or successful named test was lost")
			}
		})
	}
	after, err := provider.Find("relay")
	if err != nil || after.Key != saved.Key || !reflect.DeepEqual(after.Keys, saved.Keys) {
		t.Fatal("named preview or refusal mutated saved credentials")
	}
}

func TestPreviewKeyAggregateFailureKeepsOriginalSlots(t *testing.T) {
	for _, failed := range []string{"temporary", "neighbor", "both"} {
		t.Run(failed, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			var mu sync.Mutex
			var asked []string
			vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
				mu.Lock()
				asked = append(asked, key)
				mu.Unlock()
				response.Header().Set("Content-Type", "application/json")
				if failed == "both" || failed == "temporary" && key == "sk-new-b" || failed == "neighbor" && key == "sk-a" {
					response.WriteHeader(http.StatusUnauthorized)
					response.Write([]byte(`{"error":{"message":"synthetic refusal"}}`))
					return
				}
				model := "new-a"
				if key == "sk-new-b" {
					model = "new-b"
				}
				json.NewEncoder(response).Encode(map[string]any{"data": []map[string]string{{"id": model}}})
			}))
			t.Cleanup(vendor.Close)
			original := provider.Provider{
				ID: "relay", Name: "Relay", Key: "sk-a", Chat: vendor.URL + "/v1", Proxy: "direct",
				Keys: []provider.KeyAccount{{Key: "sk-b", Name: "second"}},
			}
			if err := provider.Save(original); err != nil {
				t.Fatal(err)
			}
			first, second := provider.KeyID("sk-a"), provider.KeyID("sk-b")
			if err := catalog.SaveLive("relay", original.Chat, []catalog.Model{
				{ID: "old-a", Keys: []string{first}},
				{ID: "old-b", Keys: []string{second}},
			}); err != nil {
				t.Fatal(err)
			}
			handler := http.NewServeMux()
			providerRoutes(handler, nil)
			response := previewPost(t, handler, "models", map[string]any{
				"id": "relay", "typed": true, "typedKeyRef": second, "key": "sk-new-b", "chat": original.Chat,
			})
			if response.Code != http.StatusOK {
				t.Fatalf("aggregate preview with prior lists: %d %s", response.Code, response.Body)
			}
			models, _, ok := catalog.Live("relay")
			if !ok {
				t.Fatal("aggregate preview left no readable catalog")
			}
			got := map[string][]string{}
			for _, model := range models {
				got[model.ID] = model.Keys
			}
			want := map[string][]string{}
			if failed == "neighbor" || failed == "both" {
				want["old-a"] = []string{first}
			} else {
				want["new-a"] = []string{first}
			}
			if failed == "temporary" || failed == "both" {
				want["old-b"] = []string{second}
			} else {
				want["new-b"] = []string{second}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("failed aggregate side lost its original-slot history: got %v want %v", got, want)
			}
			mu.Lock()
			credentials := slices.Clone(asked)
			mu.Unlock()
			slices.Sort(credentials)
			if !reflect.DeepEqual(credentials, []string{"sk-a", "sk-new-b"}) {
				t.Errorf("aggregate failure replayed or fell back to a saved edited key: %v", credentials)
			}
			saved, err := provider.Find("relay")
			if err != nil || saved.Key != original.Key || !reflect.DeepEqual(saved.Keys, original.Keys) {
				t.Fatal("aggregate failure persisted temporary credential state")
			}
		})
	}
}

func TestPreviewKeyRoutesInvalidEnvelope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var requests atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(vendor.Close)
	if err := provider.Save(provider.Provider{
		ID: "relay", Name: "Relay", Key: "sk-a", Chat: vendor.URL + "/v1", Proxy: "direct",
		Keys: []provider.KeyAccount{{Key: "sk-b"}, {Key: "sk-off", Off: true}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(provider.Path())
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(provider.Path())
	if err != nil {
		t.Fatal(err)
	}
	handler := http.NewServeMux()
	providerRoutes(handler, nil)
	for _, family := range []string{"models", "test", "detect", "list", "peekcert"} {
		for _, row := range []struct {
			name, ref, key, account string
			typed                   bool
		}{
			{"off", provider.KeyID("sk-off"), "sk-new", "", true},
			{"empty-ref", "", "sk-new", "", true},
			{"empty-key", provider.KeyID("sk-b"), " ", "", true},
			{"duplicate", provider.KeyID("sk-b"), "sk-a", "", true},
			{"not-typed", provider.KeyID("sk-b"), "sk-new", "", false},
			{"conflicting-target", provider.KeyID("sk-b"), "sk-new", provider.KeyID("sk-a"), true},
		} {
			t.Run(family+"/"+row.name, func(t *testing.T) {
				response := previewPost(t, handler, family, map[string]any{
					"id": "relay", "typed": row.typed, "typedKeyRef": row.ref, "key": row.key,
					"account": row.account, "chat": vendor.URL + "/v1",
				})
				if response.Code != http.StatusBadRequest || requests.Load() != 0 {
					t.Error("invalid preview envelope was not refused before transport")
				}
			})
		}
	}
	after, err := os.ReadFile(provider.Path())
	current, statErr := os.Stat(provider.Path())
	if err != nil || statErr != nil || string(after) != string(before) || !stat.ModTime().Equal(current.ModTime()) {
		t.Fatal("refused preview rewrote provider configuration")
	}
}
