package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func installExtensionMediaFixture(t *testing.T, configure ...func(*sdk.Manifest)) (sdk.Manifest, *extensionhost.Manager, string) {
	t.Helper()
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_ADDR", "")
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.media", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture media", Protocols: []protocolext.Protocol{"chat"},
			Operations: []sdk.ProviderOperation{sdk.OperationEmbeddings, sdk.OperationRerank, sdk.OperationImageGenerate,
				sdk.OperationImageEdit, sdk.OperationAudioSpeech, sdk.OperationAudioTranscribe, sdk.OperationAudioTranslate,
				sdk.OperationVideoCreate, sdk.OperationVideoGet, sdk.OperationVideoContent},
			Models: []sdk.Model{{ID: "fixture-vector", Kind: "embedding"}, {ID: "fixture-ranker", Kind: "rerank"},
				{ID: "fixture-picture", Kind: "image", OutputModalities: []string{"image"}},
				{ID: "fixture-voice", Kind: "speech", OutputModalities: []string{"audio"}},
				{ID: "fixture-listen", Kind: "transcription", InputModalities: []string{"audio"}},
				{ID: "fixture-movie", Kind: "video", OutputModalities: []string{"video"}}}}}}
	for _, change := range configure {
		change(&manifest)
	}
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	directory := t.TempDir()
	buildContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildContext, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o",
		filepath.Join(directory, manifest.Entry), "./testdata/extensionmediaworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build media fixture: %v\n%s", err, output)
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(buildContext, directory); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"alice", "bob"} {
		if _, err := manager.Store().SaveAccount(extensionhost.Account{Plugin: manifest.ID, Provider: "vendor", ID: account,
			Auth: json.RawMessage(`{"access":"fixture-` + account + `"}`)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	provider.Changed()
	return manifest, manager, provider.ExtensionProviderID(manifest.ID, "vendor")
}

func mediaFixtureCall(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestExtensionRetrievalDiscoveryMatchesGateway(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	const kinds = "/v1/models?kind=embedding,rerank"
	vector, ranker := id+"/fixture-vector", id+"/fixture-ranker"
	type row struct {
		ID, Kind        string
		NativeEndpoints []string `json:"native_endpoints"`
	}
	list := func(path, secret string) map[string]row {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:12345"
		if secret != "" {
			request.Header.Set("Authorization", "Bearer "+secret)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var result struct{ Data []row }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
		models := make(map[string]row, len(result.Data))
		for _, entry := range result.Data {
			if _, exists := models[entry.ID]; exists {
				t.Errorf("%s listed %s twice", path, entry.ID)
			}
			models[entry.ID] = entry
		}
		return models
	}
	listed := list(kinds, "")
	for _, check := range []struct{ ref, model, kind, path, input, want string }{
		{vector, "fixture-vector", "embedding", "/v1/embeddings", `"input":"fixture"`, `"embedding":[0.25,0.5]`},
		{ranker, "fixture-ranker", "rerank", "/v1/rerank", `"query":"fixture","documents":["fixture"]`, `"relevance_score":0.9`},
	} {
		model := listed[check.ref]
		if model.Kind != check.kind || len(model.NativeEndpoints) != 1 || model.NativeEndpoints[0] != check.path {
			t.Errorf("%s discovery: %#v", check.ref, model)
		}
		for _, ref := range []string{check.ref, check.model, "group/" + provider.KindGroupID(check.kind, check.model)} {
			response := mediaFixtureCall(handler, http.MethodPost, check.path, `{"model":"`+ref+`",`+check.input+`}`)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), check.want) {
				t.Errorf("%s through %s: %d %s", ref, check.path, response.Code, response.Body)
			}
		}
	}
	chat := list("/v1/models", "")
	if chat[vector].ID != "" || chat[ranker].ID != "" {
		t.Errorf("chat list includes a retrieval model: %v", chat)
	}
	if got := FirstServing("speech"); got != id+"/fixture-voice" {
		t.Errorf("speech pipeline discovery = %q", got)
	}
	if got := FirstServing("transcription"); got != id+"/fixture-listen" {
		t.Errorf("transcription pipeline discovery = %q", got)
	}
	secret, err := access.Update("add-key", access.Change{Name: "Retrieval fixture"})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := access.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("gateway keys: %v, %v", keys, err)
	}
	key := keys[0].ID
	if _, err := access.Update("models-key", access.Change{Key: key, Models: []string{vector}}); err != nil {
		t.Fatal(err)
	}
	if got := list(kinds, secret); len(got) != 1 || got[vector].ID == "" {
		t.Errorf("key-held discovery: %v", got)
	}
	if _, err := access.Update("accounts-key", access.Change{Key: key, Accounts: []string{id + "/" + provider.LoginID(id, "alice")}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetAccountModels(id, "alice", []string{"fixture-ranker"}); err != nil {
		t.Fatal(err)
	}
	if got := list(kinds, secret); len(got) != 0 {
		t.Errorf("held account doesn't serve the vector model: %v", got)
	}
	if err := provider.DisableModels(vector); err != nil {
		t.Fatal(err)
	}
	if got := list(kinds, ""); got[vector].ID != "" || got[ranker].ID == "" {
		t.Errorf("disabled vector model: %v", got)
	}
}

func TestExtensionRetrievalDiscoveryRequiresDeclaredOperation(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t, func(manifest *sdk.Manifest) {
		manifest.Providers[0].Operations = []sdk.ProviderOperation{sdk.OperationEmbeddings}
	})
	server := New()
	t.Cleanup(server.idle.stop)
	response := mediaFixtureCall(server.Front(), http.MethodGet, "/v1/models?kind=embedding,rerank", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), id+"/fixture-vector") ||
		strings.Contains(response.Body.String(), id+"/fixture-ranker") {
		t.Fatalf("retrieval models for a module that only declared embeddings: %d %s", response.Code, response.Body)
	}
}

func TestExtensionMediaOperationsThroughOrdinaryGateway(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	handler := New().Front()
	for _, test := range []struct{ path, model, extra, want string }{
		{"/v1/embeddings", "fixture-vector", `"input":"fixture"`, `"embedding":[0.25,0.5]`},
		{"/v1/rerank", "fixture-ranker", `"query":"fixture","documents":["fixture"]`, `"relevance_score":0.9`},
		{"/v1/audio/speech", "fixture-voice", `"input":"fixture","voice":"alloy"`, "\x00\xffaudio:alice"},
		{"/v1/images/generations", "fixture-picture", `"prompt":"fixture","response_format":"b64_json"`, `"b64_json":`},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := mediaFixtureCall(handler, http.MethodPost, test.path,
				`{"model":"`+id+`/`+test.model+`",`+test.extra+`}`)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("ordinary SDK operation: %d %s", response.Code, response.Body)
			}
		})
	}
	created := mediaFixtureCall(handler, http.MethodPost, "/v1/videos",
		`{"model":"`+id+`/fixture-movie","prompt":"fixture"}`)
	var video struct {
		ID string `json:"id"`
	}
	json.Unmarshal(created.Body.Bytes(), &video)
	if created.Code != 200 || video.ID == "" {
		t.Fatalf("create SDK video: %d %s", created.Code, created.Body)
	}
	if err := provider.SetLoginOn(id, "alice", false); err != nil {
		t.Fatal(err)
	}
	unavailable := mediaFixtureCall(handler, http.MethodGet, "/v1/videos/"+video.ID, "")
	if unavailable.Code == 200 {
		t.Fatalf("video switched to another account when its owner was paused: %s", unavailable.Body)
	}
	if err := provider.SetLoginOn(id, "alice", true); err != nil {
		t.Fatal(err)
	}
	status := mediaFixtureCall(handler, http.MethodGet, "/v1/videos/"+video.ID, "")
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"fixture_account":"alice"`) {
		t.Fatalf("video status lost original account: %d %s", status.Code, status.Body)
	}
	content := mediaFixtureCall(handler, http.MethodGet, "/v1/videos/"+video.ID+"/content", "")
	if content.Code != 200 || content.Body.String() != "video:alice" {
		t.Fatalf("video binary content lost original account: %d %q", content.Code, content.Body.String())
	}
}

func TestExtensionImageWorkerReceivesEffectiveResolution(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	handler := New().Front()
	for _, test := range []struct {
		extra string
		size  any
	}{
		{`"resolution":"auto","size":"4K"`, nil},
		{`"resolution":"auto","size":"1536x1024"`, "1536x1024"},
		{`"size":"4K"`, "4096x4096"},
	} {
		response := mediaFixtureCall(handler, http.MethodPost, "/v1/images/generations",
			`{"model":"`+id+`/fixture-picture","prompt":"echo image request",`+test.extra+`}`)
		var result struct {
			Data []struct {
				Revised string `json:"revised_prompt"`
			} `json:"data"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Data) != 1 {
			t.Fatalf("%s: %d %s", test.extra, response.Code, response.Body)
		}
		var payload map[string]any
		if json.Unmarshal([]byte(result.Data[0].Revised), &payload) != nil ||
			payload["size"] != test.size || payload["resolution"] != nil {
			t.Fatalf("%s: SDK worker received %s, want size %v", test.extra, result.Data[0].Revised, test.size)
		}
	}
}

func TestExtensionMediaAcceptanceStopsGroupFallback(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	var fallbackCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/embeddings" {
			http.NotFound(writer, request)
			return
		}
		fallbackCalls.Add(1)
		io.WriteString(writer, `{"data":[]}`)
	}))
	t.Cleanup(backend.Close)
	if err := provider.Save(provider.Provider{ID: "fallback", Name: "Fallback", Chat: backend.URL + "/v1",
		Key: "fixture", Models: []string{"fixture-vector"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "media", Name: "Media", Routing: "ordered",
		Members: []string{id + "/fixture-vector", "fallback/fixture-vector"}}); err != nil {
		t.Fatal(err)
	}
	response := mediaFixtureCall(New().Front(), http.MethodPost, "/v1/embeddings",
		`{"model":"group/media","input":"accepted failure"}`)
	if response.Code != http.StatusConflict || response.Header().Get("X-Should-Retry") != "false" ||
		response.Header().Get("X-Magpie-Operation") == "" || fallbackCalls.Load() != 0 {
		t.Fatalf("accepted SDK operation replayed or lacked refusal: %d %s fallback=%d", response.Code, response.Body, fallbackCalls.Load())
	}
}

func TestExtensionMediaCachedProviderUsesOnlyProvenRefreshSuccessor(t *testing.T) {
	manifest, manager, id := installExtensionMediaFixture(t)
	account, err := manager.Store().GetAccount(manifest.ID, "vendor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	account.ExpiresAt = time.Now().Add(time.Second).UnixMilli()
	if _, err := manager.Store().SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	selected, err := provider.Find(id)
	if err != nil || selected == nil {
		t.Fatalf("SDK provider unavailable: %v", err)
	}
	invoke := func() error {
		request, err := http.NewRequest(http.MethodPost, selected.Chat+"/embeddings", strings.NewReader(`{"model":"fixture-vector","input":"fixture"}`))
		if err != nil {
			return err
		}
		if err := selected.Sign(t.Context(), request, provider.Chat, nil); err != nil {
			return err
		}
		response, err := selected.Do(http.DefaultClient, request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.ReadAll(response.Body)
		return err
	}
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	if err := invoke(); err != nil {
		t.Fatalf("same cached provider lost its own refreshed account: %v", err)
	}
	current, err := manager.Store().GetAccount(manifest.ID, "vendor", "alice")
	if err != nil || current.Generation != 3 || string(current.Auth) != `{"access":"rotated-alice"}` {
		t.Fatalf("cached provider did not durably renew once: generation=%d %v", current.Generation, err)
	}
	current.Auth = json.RawMessage(`{"access":"manual-replacement"}`)
	if _, err := manager.Store().SaveAccount(current, 3); err != nil {
		t.Fatal(err)
	}
	if err := invoke(); err == nil {
		t.Fatal("cached provider adopted an unrelated manual credential replacement")
	}
}
