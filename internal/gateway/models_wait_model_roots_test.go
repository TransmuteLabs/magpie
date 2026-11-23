package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestModelsListConditionalFollowsForeignSDKModelRoots(t *testing.T) {
	testModelsListConditionalFollowsForeignSDKModelRoots(t, false)
}

func TestModelsListConditionalRecoversSDKModelRootsAfterStoreLock(t *testing.T) {
	testModelsListConditionalFollowsForeignSDKModelRoots(t, true)
}

func testModelsListConditionalFollowsForeignSDKModelRoots(t *testing.T, lockedObservation bool) {
	const before, after = "old-entitlement", "new-entitlement"
	var updated atomic.Bool
	id := installPolicyRouteFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.ContentLength != 0 {
			t.Error("models list attempted SDK inference")
			http.Error(writer, "unexpected SDK execution", http.StatusBadGateway)
			return
		}
		model := before
		if updated.Load() {
			model = after
		}
		if err := json.NewEncoder(writer).Encode(sdk.ModelsResult{Models: []sdk.Model{{ID: model}}}); err != nil {
			t.Error(err)
		}
	}, func(manifest *sdk.Manifest) {
		manifest.ID = codexEtagFixtureID
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ModelDiscovery)
		manifest.Providers[0].Models = nil
	})
	oldID, newID := id+"/"+before, id+"/"+after
	keys, secrets := newCaller(t, "Both entitlements", "Old entitlement")
	for index, models := range [][]string{{oldID, newID}, {oldID}} {
		if _, err := access.Update("models-key", access.Change{Key: keys[index].ID, Models: models}); err != nil {
			t.Fatal(err)
		}
	}

	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	askList := func(secret, tag, wait string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/v1/models", nil)
		request.RemoteAddr = "192.0.2.1:12345"
		request.Header.Set("Authorization", "Bearer "+secret)
		if tag != "" {
			request.Header.Set("If-None-Match", tag)
		}
		if wait != "" {
			request.Header.Set(provider.ListWaitHeader, wait)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	ask := func(secret, tag string) *httptest.ResponseRecorder {
		return askList(secret, tag, "")
	}
	const private = "private, no-cache"
	readList := func(reply *httptest.ResponseRecorder, cacheControl string) (string, []string) {
		t.Helper()
		tag := reply.Header().Get("ETag")
		if reply.Code != http.StatusOK || tag == "" || reply.Header().Get("Cache-Control") != cacheControl {
			t.Fatalf("expected 200 model list with %q: status %d, ETag %q, cache %q, body %s",
				cacheControl, reply.Code, tag, reply.Header().Get("Cache-Control"), reply.Body)
		}
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(reply.Body.Bytes(), &payload); err != nil || payload.Data == nil {
			t.Fatalf("invalid models list: %v, body %s", err, reply.Body)
		}
		ids := make([]string, len(payload.Data))
		for index, model := range payload.Data {
			ids[index] = model.ID
		}
		return tag, ids
	}
	list := func(reply *httptest.ResponseRecorder, want ...string) string {
		t.Helper()
		tag, ids := readList(reply, private)
		if !slices.Equal(ids, want) {
			t.Fatalf("listed models %v, want %v", ids, want)
		}
		return tag
	}
	requireUnchanged := func(reply *httptest.ResponseRecorder, tag string) {
		t.Helper()
		if reply.Code != http.StatusNotModified || reply.Header().Get("ETag") != tag ||
			reply.Header().Get("Cache-Control") != private || reply.Body.Len() != 0 {
			t.Fatalf("expected private bodyless 304: status %d, ETag %q (want %q), cache %q, body %q",
				reply.Code, reply.Header().Get("ETag"), tag, reply.Header().Get("Cache-Control"), reply.Body.String())
		}
	}

	if lockedObservation {
		var lockedStamp provider.Stamp
		func() {
			defer provider.Hold()()
			lock := filepath.Join(filepath.Dir(extension.CatalogPath()), "private", ".lock")
			unlock, held := filelock.Lock(lock, time.Second)
			if !held {
				t.Fatal("could not hold the SDK Store lock")
			}
			defer unlock()
			started := time.Now()
			provider.RefreshModelListSources(t.Context())
			if elapsed := time.Since(started); elapsed < filelock.Wait {
				t.Fatalf("SDK root observation did not reach the Store lock budget: %v", elapsed)
			}
			lockedStamp = provider.StampNow()
		}()
		func() {
			defer provider.Hold()()
			if provider.StampNow() != lockedStamp {
				t.Fatal("releasing the Store lock changed the provider stamp")
			}
		}()
		t.Log("SDK Store root observation timed out; provider stamp unchanged when lock released")
	}
	oldTag := list(ask(secrets[0], ""), oldID)
	built := provider.EntriesBuilt()
	requireUnchanged(ask(secrets[0], oldTag), oldTag)
	list(ask(secrets[1], ""), oldID)
	if current := provider.EntriesBuilt(); current != built {
		t.Fatalf("public model list warm control rebuilt the entries projection: %d -> %d", built, current)
	}
	publish := func(model string) bool {
		t.Helper()
		roots := fixtureSDKModelRoots(t)
		config, err := extension.AccountStamp()
		if err != nil {
			t.Fatal(err)
		}
		stamp := provider.StampNow()
		updated.Store(model == after)
		if output, err := publishForeignCodexModels(t.Context(), model, false); err != nil {
			t.Fatalf("foreign model-only refresh: %v\n%s", err, output)
		}
		nextRoots := fixtureSDKModelRoots(t)
		for key, root := range roots {
			next, found := nextRoots[key]
			if !found || next.name != root.name || next.version != root.version+1 || os.SameFile(root.info, next.info) {
				t.Fatalf("peer did not atomically replace the same root %s: versions %d -> %d",
					root.name, root.version, next.version)
			}
		}
		nextConfig, err := extension.AccountStamp()
		if err != nil || !config.Equal(nextConfig) {
			t.Fatalf("peer model-only refresh changed the account/config/owner stamp: %v", err)
		}
		stampEqual := provider.StampNow() == stamp
		t.Logf("model-only peer refresh: two roots replaced, account/config/owner stamp equal, provider stamp equal=%t", stampEqual)
		return stampEqual
	}

	stampEqual := publish(after)
	reply := ask(secrets[0], oldTag)
	stale := reply.Code == http.StatusNotModified && reply.Header().Get("ETag") == oldTag
	if stale {
		requireUnchanged(reply, oldTag)
	} else if reply.Code == http.StatusOK {
		_, ids := readList(reply, private)
		stale = slices.Equal(ids, []string{oldID})
	}
	if stale {
		if current := provider.EntriesBuilt(); current != built || !stampEqual {
			t.Fatalf("stale list without reproduced warm cache conditions: entries %d -> %d, provider stamp equal=%t",
				built, current, stampEqual)
		}
		status := reply.Code
		provider.Changed()
		reply = ask(secrets[0], oldTag)
		list(reply, newID)
		t.Fatalf("public list reused stale SDK model projection after same-config peer publication: status %d; exact new list after local invalidation", status)
	}
	newTag := list(reply, newID)
	if newTag == oldTag {
		t.Fatalf("new model list kept its old ETag %q", oldTag)
	}
	requireUnchanged(ask(secrets[0], newTag), newTag)
	restrictedTag := list(ask(secrets[1], newTag))
	if restrictedTag == newTag {
		t.Fatalf("restricted list reused the other key's ETag %q", newTag)
	}
	requireUnchanged(ask(secrets[1], restrictedTag), restrictedTag)

	publish(before)
	reply = askList(secrets[0], newTag, "0")
	waitTag, waitIDs := readList(reply, "no-store")
	if waitTag == newTag || !slices.Equal(waitIDs, []string{oldID}) || reply.Header().Get(provider.ListWaitHeader) != "0" {
		t.Fatalf("first long-poll look returned ETag %q, models %v, wait %q",
			waitTag, waitIDs, reply.Header().Get(provider.ListWaitHeader))
	}

	nativeTag := provider.CodexListTag()
	requireUnchanged(ask(secrets[0], waitTag), waitTag)
	stamp := provider.StampNow()
	for range 2 {
		if tag := provider.CodexListTag(); tag != nativeTag {
			t.Fatalf("unchanged Codex list tag moved from %q to %q", nativeTag, tag)
		}
		requireUnchanged(ask(secrets[0], waitTag), waitTag)
	}
	if provider.StampNow() != stamp {
		t.Fatal("unchanged public and Codex lists repeatedly invalidated the catalog")
	}
}

type fixtureSDKModelRoot struct {
	name    string
	version uint64
	info    os.FileInfo
}

func fixtureSDKModelRoots(t *testing.T) map[string]fixtureSDKModelRoot {
	t.Helper()
	directory := filepath.Join(filepath.Dir(extension.CatalogPath()), "private", codexEtagFixtureID, "state")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]fixtureSDKModelRoot{}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Key     string `json:"key"`
			Version uint64 `json:"version"`
		}
		if err := json.Unmarshal(body, &record); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(record.Key, extension.ModelCatalogPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := roots[record.Key]; duplicate || record.Version == 0 {
			t.Fatalf("unexpected SDK root key or version in %s", entry.Name())
		}
		roots[record.Key] = fixtureSDKModelRoot{entry.Name(), record.Version, info}
	}
	if len(roots) != 2 {
		t.Fatalf("found %d fixture model roots, want one for each of two SDK accounts", len(roots))
	}
	return roots
}
