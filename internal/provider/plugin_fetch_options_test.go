package provider

import (
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

const destinationOptionPeer = "MAGPIE_TEST_SAVE_PLUGIN_OPTION"

func TestPluginFetchFollowsDurableProviderOptions(t *testing.T) {
	if os.Getenv(destinationOptionPeer) == "1" {
		saveDestinationOption(t, true)
		return
	}
	for _, phase := range []string{"warm", "headers", "loader", "default"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			type vendorRequest struct{ route, host, model, header string }
			received := make(chan vendorRequest, 8)
			vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- vendorRequest{
					route: r.Header.Get("X-Loader-Route"), host: r.Header.Get("X-Loader-Host"),
					model: r.Header.Get("X-Loader-Model"), header: r.Header.Get("X-Header-Route"),
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			hook, entered, release := holdDestinationSetup(t, ctx)
			t.Setenv("DESTINATION_HEADERS", hook)
			if phase == "loader" {
				t.Setenv("DESTINATION_LOADER_WAIT", hook)
			}
			p := destinationProvider(t, ctx, vendor.URL, destinationSignIn("fixture-key", vendor.URL))
			getVendor := func(want string) string {
				t.Helper()
				select {
				case request := <-received:
					model := "Old route chat"
					if want == "new" {
						model = "New route chat"
					}
					if request.route != want || request.header != want || request.model != model || request.host == "" {
						t.Errorf("vendor got %+v, want route/header %q and model %q on the same host", request, want, model)
					}
					return request.host
				default:
					t.Error("request did not reach the fixture")
					return ""
				}
			}
			send := func(want string) string {
				t.Helper()
				status, err := doDestinationRequest(ctx, p, fixturePluginChatURL, "", destinationChat)
				if err != nil || status != http.StatusNoContent {
					t.Fatalf("request with route %s gave status %d, error %v", want, status, err)
				}
				return getVendor(want)
			}
			var host string
			if phase != "loader" {
				host = send("old")
			}
			var pending <-chan destinationFetchResult
			switch phase {
			case "headers", "default":
				pending = startDestinationFetch(t, ctx, p, "held-headers", entered)
			case "loader":
				pending = startDestinationFetch(t, ctx, p, "", entered)
			}
			if phase == "default" {
				saveDestinationOption(t, false)
				for range 2 {
					if err := plugin.SetProviderOptions(ctx); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				result := <-pending
				if result.err != nil || result.status != http.StatusNoContent {
					t.Fatalf("unchanged option interrupted the request: status %d, error %v", result.status, result.err)
				}
				if got := getVendor("old"); got != host {
					t.Errorf("unchanged option replaced Bun host %s with %s", host, got)
				}
			} else {
				saveDestinationOptionFromPeer(t, ctx)
				if pending != nil {
					currentHost := send("new")
					if host != "" && currentHost != host {
						t.Errorf("peer save replaced Bun host %s with %s", host, currentHost)
					}
					host = currentHost
					close(release)
					result := <-pending
					if result.err == nil || !strings.Contains(result.err.Error(), "options changed") {
						t.Errorf("superseded preparation gave status %d, error %v", result.status, result.err)
					}
					select {
					case request := <-received:
						t.Errorf("superseded preparation sent route %q", request.route)
					default:
					}
				}
				if got := send("new"); host != "" && got != host {
					t.Errorf("peer save replaced Bun host %s with %s", host, got)
				}
			}
			if n := len(received); n != 0 {
				t.Errorf("vendor got %d extra requests", n)
			}
		})
	}
}

func TestPluginAccountRoutesFollowDurableProviderOptions(t *testing.T) {
	for _, entry := range []string{"Do", "Origin", "Refetch", "Gemini", "Changed API", "Image generation API", "Image edit API", "Multipart edit API", "Multipart edit", "Removed model", "Unavailable list"} {
		t.Run(entry, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			type request struct{ origin, path, route, model, body, host string }
			sent := make(chan request, 4)
			vendor := func(origin string) string {
				server := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					sent <- request{origin, r.URL.RequestURI(), r.Header.Get("X-Loader-Route"), bodyModel(body), string(body), r.Header.Get("X-Loader-Host")}
					w.WriteHeader(http.StatusNoContent)
				}))
				return server.URL
			}
			old := vendor("old")
			oldRun := os.Getenv("FAKE_RUN")
			current := vendor("new")
			t.Setenv("DESTINATION_ALTERNATE", current)
			t.Setenv("DESTINATION_ALTERNATE_RUN", os.Getenv("FAKE_RUN"))
			t.Setenv("FAKE_RUN", oldRun)
			signIn := destinationSignIn("fixture-key", "")
			if entry == "Origin" {
				signIn["metadata"] = map[string]bool{"useModelsBase": true}
			}
			if strings.HasSuffix(entry, "API") {
				t.Setenv("DESTINATION_NEW_NPM", "@ai-sdk/anthropic")
			}
			if entry == "Removed model" {
				t.Setenv("DESTINATION_REMOVE_MODEL", "yes")
			} else if entry == "Unavailable list" {
				t.Setenv("DESTINATION_REMOVE_MODEL", "fallback")
			}
			p := destinationProvider(t, ctx, old, signIn)
			const payload = `{"messages":[{"role":"user","content":"модель 💡"}], "model" : "chat", "unknown":1.23000}`
			send := func(want string) string {
				t.Helper()
				status, err := doDestinationRequest(ctx, p, fixturePluginChatURL, "", payload)
				if err != nil || status != http.StatusNoContent {
					t.Fatalf("request gave status %d, error %v", status, err)
				}
				select {
				case got := <-sent:
					wireModel := "chat-" + want
					if got.origin != want || got.route != want || got.path != "/tenant-"+want+"/v1/chat/completions" || got.model != wireModel {
						t.Errorf("prepared %s request went to %s %s as %s, want %s", got.route, got.origin, got.path, got.model, want)
					}
					if got.body != strings.Replace(payload, `"chat"`, `"`+wireModel+`"`, 1) {
						t.Errorf("rewriting the model changed the request: %s", got.body)
					}
					return got.host
				default:
					t.Fatal("request did not reach either fixture")
					return ""
				}
			}
			host := send("old")
			if entry == "Origin" && p.Origin(ctx) != old {
				t.Fatal("fixture did not warm the original origin")
			}
			saveDestinationOptionFromPeer(t, ctx)
			switch entry {
			case "Do":
				if got := send("new"); got == "" || got != host {
					t.Errorf("fetch changed Bun host from %q to %q", host, got)
				}
			case "Origin":
				if got := p.Origin(ctx); got != current {
					t.Errorf("warm origin = %q, want the saved destination %q", got, current)
				}
			case "Refetch":
				if _, _, err := p.Refetch(ctx); err != nil {
					t.Fatal(err)
				}
				listed, ok := PluginOf(p.ID)
				model, _ := pluginModel(listed, "chat")
				if !ok || model.APIID != "chat-new" || model.URL != current+"/tenant-new/v1" {
					t.Errorf("refetched API ID and URL = %q, %q, want the saved destination", model.APIID, model.URL)
				}
			case "Gemini", "Removed model", "Unavailable list":
				const request = `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`
				status, err := doDestinationRequest(ctx, p, pluginBase+destinationPlugin+"/v1internal:streamGenerateContent", "", `{"model":"gem","request":`+request+`}`)
				if entry == "Removed model" {
					if err == nil || !strings.Contains(err.Error(), "no longer lists") {
						t.Errorf("request for a removed model gave %d, %v", status, err)
					}
					break
				}
				if err != nil || status != http.StatusNoContent {
					t.Fatalf("Gemini request = %d, %v", status, err)
				}
				select {
				case got := <-sent:
					origin, path := "new", "/tenant-new/v1beta/models/fixture/gem%20new:streamGenerateContent?alt=sse"
					if entry == "Unavailable list" {
						origin, path = "old", "/tenant-old/v1beta/models/fixture/gem%20old:streamGenerateContent?alt=sse"
					}
					if got.origin != origin || got.path != path || got.body != request {
						t.Errorf("Gemini used %+v, want the current model path and unchanged contents", got)
					}
				default:
					t.Fatal("Gemini did not reach the fixture")
				}
			case "Changed API", "Image generation API", "Image edit API", "Multipart edit API", "Multipart edit":
				requestContext, url, body := ctx, fixturePluginChatURL, payload
				switch entry {
				case "Image generation API":
					url, body = pluginBase+destinationPlugin+"/v1/images/generations", `{"model":"chat","prompt":"draw a tree"}`
				case "Image edit API":
					url, body = pluginBase+destinationPlugin+"/v1/images/edits", `{"model":"chat","prompt":"add some fruit"}`
				case "Multipart edit API", "Multipart edit":
					var form strings.Builder
					writer := multipart.NewWriter(&form)
					if err := writer.WriteField("model", "chat"); err != nil {
						t.Fatal(err)
					}
					requestContext = WithPluginMultipartModel(ctx, "chat", form.Len()-len("chat"))
					if err := writer.WriteField("prompt", "add some fruit"); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					url, body = pluginBase+destinationPlugin+"/v1/images/edits", form.String()
				}
				status, err := doDestinationRequest(requestContext, p, url, "", body)
				if entry == "Multipart edit" {
					if err != nil || status != http.StatusNoContent {
						t.Fatalf("multipart edit gave %d, %v", status, err)
					}
					select {
					case got := <-sent:
						want := strings.Replace(body, "\r\n\r\nchat\r\n", "\r\n\r\nchat-new\r\n", 1)
						if got.origin != "new" || got.path != "/tenant-new/v1/images/edits" || got.body != want {
							t.Errorf("multipart edit used %+v, want only the model field rewritten for the new route", got)
						}
					default:
						t.Fatal("multipart edit did not reach the fixture")
					}
				} else if err == nil || !strings.Contains(err.Error(), "changed its API") {
					t.Errorf("request built for the old API gave %d, %v", status, err)
				}
			}
			if extra := len(sent); extra != 0 {
				t.Errorf("fixture saw %d additional requests", extra)
			}
		})
	}
}

func saveDestinationOptionFromPeer(t *testing.T, ctx context.Context) {
	t.Helper()
	child := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestPluginFetchFollowsDurableProviderOptions$", "-test.count=1")
	child.Env = append(os.Environ(), destinationOptionPeer+"=1")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("peer option save: %v\n%s", err, output)
	}
}

func saveDestinationOption(t *testing.T, on bool) {
	t.Helper()
	err := settings.EditPluginOptions(func(options map[string]map[string]bool) (map[string]map[string]bool, error) {
		options[destinationPlugin] = map[string]bool{"new-route": on}
		return options, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
