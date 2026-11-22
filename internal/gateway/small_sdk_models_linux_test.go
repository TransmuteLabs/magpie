package gateway

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
	"golang.org/x/sys/unix"
)

func TestModelsListSDKReadsNativeModelAPIsOnce(t *testing.T) {
	const model = "fixture-sdk-native-endpoint"
	p := smallSDKProvider(t, 8, sdk.Model{
		ID: model, Price: &sdk.ModelPrice{}, Protocols: []protocolext.Protocol{"chat"},
	})
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	serve := func(etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/v1/models", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("If-None-Match", etag)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	first := serve("")
	tag, body := first.Header().Get("ETag"), first.Body.String()
	if first.Code != http.StatusOK || tag == "" {
		t.Fatalf("warmup model list: status %d, ETag %q", first.Code, tag)
	}
	type modelRow struct {
		ID              string   `json:"id"`
		NativeEndpoints []string `json:"native_endpoints"`
	}
	var list struct {
		Data []modelRow `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	wantID, wantEndpoints := p.ID+"/"+model, []string{"/v1/chat/completions"}
	row := slices.IndexFunc(list.Data, func(entry modelRow) bool {
		return entry.ID == wantID
	})
	if row < 0 || !slices.Equal(list.Data[row].NativeEndpoints, wantEndpoints) {
		t.Fatalf("model list does not contain %q with endpoints %v: %s", wantID, wantEndpoints, body)
	}

	for _, phase := range []struct {
		name string
		tag  string
		code int
		body string
	}{
		{"full", "", http.StatusOK, body},
		{"conditional", tag, http.StatusNotModified, ""},
	} {
		t.Run(phase.name, func(t *testing.T) {
			var response *httptest.ResponseRecorder
			reads := countSDKModelCatalogReads(t, func() { response = serve(phase.tag) })
			if response.Code != phase.code || response.Header().Get("ETag") != tag || response.Body.String() != phase.body {
				t.Fatalf("model list: status %d, ETag %q, same body %v; want %d, %q and the same body",
					response.Code, response.Header().Get("ETag"), response.Body.String() == phase.body, phase.code, tag)
			}
			t.Logf("/v1/models opened and closed catalog.json %d times; ETag %s", reads, tag)
			if reads != 8 {
				t.Errorf("model list read catalog.json %d times, want 8 (native endpoints and price, each checking two accounts twice)", reads)
			}
		})
	}
}

func TestSmallCandidatesSDKReadsOneModelProjection(t *testing.T) {
	for _, api := range []struct {
		name  string
		minor int
	}{{"1.0", 0}, {"1.8", 8}} {
		t.Run(api.name, func(t *testing.T) {
			models := unpricedSmallSDKModels()
			p := smallSDKProvider(t, api.minor, models...)
			if got := len(p.Exposed()); got != len(models) {
				t.Fatalf("exposed models = %d, want %d", got, len(models))
			}
			reads := countSDKModelCatalogReads(t, func() {
				if got := len(smallCands(p, seesAt(p.ID))); got != len(models) {
					t.Fatalf("candidate models = %d, want %d", got, len(models))
				}
			})
			t.Logf("SDK model candidates opened and closed catalog.json %d times", reads)
			if reads != 4 {
				t.Fatalf("SDK model candidates read catalog.json %d times, want 4 (two checks for each account)", reads)
			}
		})
	}
}

func TestFreeFirstSDKReadsOneModelProjection(t *testing.T) {
	const (
		paid     = "fixture-sdk-freefirst-paid"
		alias    = "fixture-sdk-freefirst-local"
		zero     = "fixture-sdk-freefirst-zero"
		included = "fixture-sdk-freefirst-included"
	)
	p := smallSDKProvider(t, 8,
		sdk.Model{ID: paid, Price: &sdk.ModelPrice{Input: 3, Output: 5}},
		sdk.Model{ID: zero, Price: &sdk.ModelPrice{}},
		sdk.Model{ID: included, Free: true, Price: &sdk.ModelPrice{Input: 11}},
	)
	prefs := settings.Load()
	prefs.ModelIDs = map[string]string{p.ID + "/" + alias: paid}
	if err := settings.Save(prefs); err != nil {
		t.Fatal(err)
	}
	if got := len(p.Available()); got != 3 {
		t.Fatalf("available SDK models = %d, want 3", got)
	}
	for _, scenario := range []struct {
		name      string
		model     string
		countFree bool
		free      bool
		reads     int
	}{
		{name: "priced_zero", model: zero, free: true, reads: 4},
		{name: "paid_alias", model: alias, reads: 4},
		{name: "vendor_included", model: included, free: true, reads: 4},
		{name: "provider_count_free", model: alias, countFree: true, free: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			selected := p
			selected.CountFree = scenario.countFree
			var free bool
			reads := countSDKModelCatalogReads(t, func() { free = selected.FreeModel(scenario.model) })
			if free != scenario.free {
				t.Fatalf("FreeModel(%q) = %t, want %t", scenario.model, free, scenario.free)
			}
			t.Logf("free classification opened and closed catalog.json %d times", reads)
			if reads != scenario.reads {
				t.Fatalf("free classification read catalog.json %d times, want %d", reads, scenario.reads)
			}
		})
	}
	t.Run("price_quote_control", func(t *testing.T) {
		reads := countSDKModelCatalogReads(t, func() {
			price, known := p.PriceAt(settings.Settings{}, alias, time.Time{})
			if !known || price.Input != 3 || price.Output != 5 {
				t.Fatalf("PriceAt(%q) = %+v, known=%t, want a 3/5 price", alias, price, known)
			}
		})
		if reads != 4 {
			t.Fatalf("price quote read catalog.json %d times, want 4", reads)
		}
	})
	t.Run("peer_disable_between_operations", func(t *testing.T) {
		scope, ok := p.ExtensionScope()
		if !ok {
			t.Fatal("provider has no SDK scope")
		}
		peer := extension.NewManager(extension.Default().Namespace())
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := peer.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		if err := peer.SetEnabled(t.Context(), scope.Plugin, false); err != nil {
			t.Fatal(err)
		}
		if p.FreeModel(zero) || p.FreeModel(included) {
			t.Fatal("retained provider reused free pricing after peer disabled its module")
		}
	})
}

func countSDKModelCatalogReads(t *testing.T, run func()) int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, extension.CatalogPath(), unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}
	run()

	var reads int
	var events [4096]byte
	for {
		size, err := unix.Read(fd, events[:])
		if err == unix.EAGAIN {
			return reads
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if size == 0 || size%unix.SizeofInotifyEvent != 0 {
			t.Fatalf("unexpected catalog event bytes: %d", size)
		}
		for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
			mask := binary.NativeEndian.Uint32(events[offset+4:])
			switch mask {
			case unix.IN_OPEN:
			case unix.IN_CLOSE_NOWRITE:
				reads++
			default:
				t.Fatalf("unexpected catalog event: %#x", mask)
			}
		}
	}
}
