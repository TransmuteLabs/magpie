package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionProviderModelKindsAndModalitiesReachCatalog(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	if err := json.Unmarshal([]byte(`[
		{"id":"fixture-picture","kind":"image","inputModalities":["text","image"],"outputModalities":["image"],"released":"2026-11-01","temperature":false},
		{"id":"fixture-movie","kind":"video","outputModalities":["video"]},
		{"id":"fixture-vector","kind":"embedding","outputModalities":[]},
		{"id":"fixture-ranker","kind":"rerank"},
		{"id":"fixture-voice","kind":"speech","outputModalities":["audio"]},
		{"id":"fixture-listen","kind":"transcription","inputModalities":["audio"]},
		{"id":"fixture-chat","contextWindow":256000,"maxContextWindow":512000,"serviceTiers":["priority","ultrafast"],"free":true,"rate":0.5,"rateWas":1}
	]`), &host.manifest.Providers[0].Models); err != nil {
		t.Fatal(err)
	}
	models := extensionAccounts()[0].Available()
	if len(models) != 7 {
		t.Fatalf("catalog dropped declared SDK models: %+v", models)
	}
	picture := models[0]
	if !picture.Draws || !picture.Images || picture.ImageInput == nil || !*picture.ImageInput ||
		!slices.Equal(picture.InputModalities, []string{"text", "image"}) ||
		!slices.Equal(picture.OutputModalities, []string{"image"}) ||
		picture.Released != "2026-11-01" || picture.Temperature == nil || *picture.Temperature {
		t.Fatalf("image metadata was lost in ordinary catalog projection: %+v", picture)
	}
	if !models[1].Films || models[2].Serves != "embedding" || models[2].OutputModalities == nil ||
		models[3].Serves != "rerank" || models[4].Serves != "speech" || models[5].Serves != "transcription" {
		t.Fatalf("SDK media/retrieval model kinds became chat models: %+v", models)
	}
	chat := models[6]
	if chat.MaxContext != 512000 || !chat.Free || chat.Rate != 0.5 || chat.RateWas != 1 ||
		!slices.Equal(chat.Tiers, []string{"priority", "ultrafast"}) {
		t.Fatalf("SDK model limits, tiers or subscription prices were lost: %+v", chat)
	}
}

func TestExtensionProviderDeclaredOperationsPreserveRawBodiesAndScope(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	wire, err := json.Marshal(host.manifest.Providers[0])
	if err != nil {
		t.Fatal(err)
	}
	var descriptor map[string]any
	if err := json.Unmarshal(wire, &descriptor); err != nil {
		t.Fatal(err)
	}
	descriptor["operations"] = []string{"embeddings", "rerank", "images.generate", "images.edit",
		"audio.transcribe", "audio.translate", "audio.speech", "videos.create", "videos.get", "videos.content", "videos.delete", "videos.remix"}
	wire, _ = json.Marshal(descriptor)
	if err := json.Unmarshal(wire, &host.manifest.Providers[0]); err != nil {
		t.Fatal(err)
	}
	provider := extensionAccounts()[0]
	current := ""
	const body = "--fixture\r\nContent-Disposition: form-data; name=\"file\"; filename=\"sample.wav\"\r\n\r\n\x00\xffaudio\r\n--fixture--"
	host.call = func(_ context.Context, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.ExecuteRequest)
		raw, _ := json.Marshal(request)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		var operation string
		json.Unmarshal(fields["operation"], &operation)
		if operation != current || request.Protocol != "chat" || method != sdk.MethodProviderExecute ||
			scope.Account != "alice" || scope.Generation != 1 || string(request.Body) != body ||
			request.Headers.Get("Authorization") != "" || request.Headers.Get("Content-Type") != "multipart/form-data; boundary=fixture" {
			t.Errorf("raw operation or selected-account scope changed: %s %+v", raw, scope)
		}
		for _, event := range []extensionhost.Event{
			{Name: "headers", Data: json.RawMessage(`{"status":200,"headers":{"Content-Type":["application/octet-stream"]}}`)},
			{Name: "data", Data: json.RawMessage(`"AP9yZXN1bHQ="`)},
		} {
			if err := consume(event); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(`{"status":200}`), nil
	}
	for _, test := range []struct{ method, path, operation string }{
		{"POST", "/embeddings", "embeddings"}, {"POST", "/rerank", "rerank"},
		{"POST", "/images/generations", "images.generate"}, {"POST", "/images/edits", "images.edit"},
		{"POST", "/audio/transcriptions", "audio.transcribe"}, {"POST", "/audio/translations", "audio.translate"},
		{"POST", "/audio/speech", "audio.speech"}, {"POST", "/videos", "videos.create"},
		{"GET", "/videos/vendor-id", "videos.get"}, {"GET", "/videos/vendor-id/content", "videos.content"},
		{"DELETE", "/videos/vendor-id", "videos.delete"}, {"POST", "/videos/vendor-id/remix", "videos.remix"},
	} {
		t.Run(test.operation, func(t *testing.T) {
			current = test.operation
			request, _ := http.NewRequest(test.method, provider.Chat+test.path+"?fixture=true", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer host-only")
			request.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
			response, err := provider.Do(http.DefaultClient, request)
			if err != nil {
				t.Fatalf("declared SDK operation was refused: %v", err)
			}
			result, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(result) != "\x00\xffresult" {
				t.Fatalf("binary operation response changed: %q %v", result, err)
			}
		})
	}
}

func TestExtensionProviderUndeclaredOperationNeverCallsModule(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Error("undeclared operation reached module")
		return nil, nil
	}
	provider := extensionAccounts()[0]
	for _, path := range []string{"/embeddings", "/images/generations", "/messages/arbitrary", "/chat/completions/arbitrary"} {
		request, _ := http.NewRequest(http.MethodPost, provider.Chat+path, strings.NewReader(`{"model":"m1"}`))
		if response, err := provider.Do(http.DefaultClient, request); err == nil {
			response.Body.Close()
			t.Errorf("undeclared or ambiguous endpoint was admitted: %s", path)
		}
	}
}

func TestExtensionProviderExecutionOptionsRespectRequestedAPI(t *testing.T) {
	for _, minor := range []int{1, 2, 3} {
		t.Run(string(rune('0'+minor)), func(t *testing.T) {
			host := fixtureExtensionProvider(t, false)
			host.manifest.Contract.Version.Minor = minor
			host.call = func(_ context.Context, _ string, _ sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
				request := params.(sdk.ExecuteRequest)
				var options map[string]json.RawMessage
				if err := json.Unmarshal(request.Options, &options); err != nil {
					return nil, err
				}
				_, escaped := options["escapedPath"]
				if escaped != (minor >= 3) {
					t.Errorf("API1.%d received the wrong standard execution option set: %s", minor, request.Options)
				}
				for _, key := range []string{"method", "path", "query", "proxy"} {
					if options[key] == nil {
						t.Errorf("API1.%d lost standard option %s", minor, key)
					}
				}
				if err := consume(extensionhost.Event{Name: "headers", Data: json.RawMessage(`{"status":200}`)}); err != nil {
					return nil, err
				}
				return json.RawMessage(`{"status":200}`), nil
			}
			provider := extensionAccounts()[0]
			request, _ := http.NewRequest(http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
			response, err := provider.Do(http.DefaultClient, request)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtensionProviderStripsCredentialHeaderAliases(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	host.call = func(_ context.Context, _ string, _ sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.ExecuteRequest)
		for name := range request.Headers {
			switch strings.ToLower(name) {
			case "authorization", "proxy-authorization", "x-api-key", "x-goog-api-key", "cookie", "cookie2":
				t.Errorf("caller credential header reached module parameters: %s", name)
			}
		}
		if request.Headers.Get("X-Fixture-Trace") != "retained" {
			t.Error("noncredential request metadata was discarded")
		}
		if err := consume(extensionhost.Event{Name: "headers", Data: json.RawMessage(`{"status":200}`)}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"status":200}`), nil
	}
	provider := extensionAccounts()[0]
	request, _ := http.NewRequest(http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
	request.Header = http.Header{
		"aUtHoRiZaTiOn": {"fixture-secret"}, "proxy-authorization": {"fixture-secret"},
		"X-API-KEY": {"fixture-secret"}, "x-goog-api-key": {"fixture-secret"},
		"cOoKiE": {"fixture-secret"}, "cookie2": {"fixture-secret"}, "X-Fixture-Trace": {"retained"},
	}
	response, err := provider.Do(http.DefaultClient, request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
}
