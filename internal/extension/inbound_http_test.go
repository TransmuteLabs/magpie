package extension

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

// This fixture process uses the real public SDK and host byte transport.
// Its synthetic codec has no vendor routes, credentials or network calls.
func init() {
	if !slices.Contains(os.Args, "--sdk-http-rejection-worker") {
		return
	}
	body, err := os.ReadFile(sdk.ManifestFile)
	var manifest sdk.Manifest
	if err != nil || json.Unmarshal(body, &manifest) != nil {
		os.Exit(1)
	}
	module := sdk.Module{Manifest: manifest, Inbounds: []codec.Inbound{
		httpRejectionCodec{streaming: true}, httpRejectionCodec{},
	}}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type httpRejectionCodec struct{ streaming bool }

func (c httpRejectionCodec) ID() string {
	if c.streaming {
		return "stream"
	}
	return "unary"
}
func (c httpRejectionCodec) Path() string { return "/fixture.errors/" + c.ID() }
func (httpRejectionCodec) Contract() codec.Contract {
	return codec.InboundContract()
}
func (httpRejectionCodec) Decode(ctx context.Context, headers http.Header, body []byte) (codec.Request, error) {
	if err := ctx.Err(); err != nil {
		return codec.Request{}, err
	}
	if headers.Get("X-Fixture-Private") != "" {
		return codec.Request{}, errors.New("private credential cause")
	}
	if code := headers.Get("X-Fixture-Code"); code != "" {
		return codec.Request{}, &sdk.Error{Code: code, Message: "safe custom error"}
	}
	status, message := 0, ""
	switch {
	case headers.Get("Content-Type") != "application/connect+proto":
		status, message = 415, "unsupported media type"
	case headers.Get("Connect-Timeout-Ms") == "0":
		status, message = 504, "native deadline expired"
	case string(body) == "oversize":
		status, message = 413, "fixture message exceeds its bound"
	case string(body) != "valid":
		status, message = 400, "invalid fixture message"
	}
	if status != 0 {
		return codec.Request{}, fmt.Errorf("private credential wrapper: %w", &codec.Error{Status: status, Message: message})
	}
	return codec.Request{Protocol: codec.Chat, Body: []byte(`{"model":"fixture/model","messages":[]}`)}, nil
}
func (c httpRejectionCodec) Reply(writer http.ResponseWriter) codec.Response {
	return &httpRejectionReply{writer: writer, streaming: c.streaming}
}

type httpRejectionReply struct {
	writer    http.ResponseWriter
	streaming bool
	status    int
	body      bytes.Buffer
	header    http.Header
}

func (r *httpRejectionReply) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}
func (r *httpRejectionReply) WriteHeader(status int) { r.status = status }
func (r *httpRejectionReply) Write(body []byte) (int, error) {
	return r.body.Write(body)
}
func (r *httpRejectionReply) Finish(cause error) error {
	code := "internal"
	switch r.status {
	case 400, 415:
		code = "invalid_argument"
	case 413:
		code = "resource_exhausted"
	case 504:
		code = "deadline_exceeded"
	}
	message := "safe rejection"
	var body struct {
		Error struct{ Message string } `json:"error"`
	}
	if json.Unmarshal(r.body.Bytes(), &body) == nil && body.Error.Message != "" {
		message = body.Error.Message
	}
	if errors.Is(cause, context.Canceled) {
		code, message = "canceled", "native request canceled"
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code, message = "deadline_exceeded", "native request deadline exceeded"
	}
	failure := map[string]any{"code": code, "message": message}
	if !r.streaming {
		r.writer.Header().Set("Content-Type", "application/json")
		r.writer.WriteHeader(r.status)
		return json.NewEncoder(r.writer).Encode(failure)
	}
	payload, err := json.Marshal(map[string]any{"error": failure})
	if err != nil {
		return err
	}
	r.writer.Header().Set("Content-Type", "application/connect+proto")
	r.writer.WriteHeader(http.StatusOK)
	frame := make([]byte, 5, len(payload)+5)
	frame[0] = 2 // Connect EndStreamResponse.
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	frame = append(frame, payload...)
	_, err = r.writer.Write(frame)
	return err
}

func httpRejectionManager(t *testing.T) (*Manager, *codec.Registry) {
	t.Helper()
	directory := fixturePackage(t)
	root, err := NewCatalog(directory).open(false)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := catalogManifest(root)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest.ID = "http-rejection"
	manifest.Args = []string{"--sdk-http-rejection-worker"}
	manifest.Contract = sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.InboundCodec}}
	manifest.Providers, manifest.Agents, manifest.Sessions, manifest.Tools = nil, nil, nil, nil
	manifest.Inbounds = []sdk.InboundDescriptor{
		{ID: "stream", Path: (httpRejectionCodec{streaming: true}).Path()},
		{ID: "unary", Path: (httpRejectionCodec{}).Path()},
	}
	catalogTestWriteManifest(t, directory, manifest)
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	registry, err := manager.Protocols()
	if err != nil {
		t.Fatal(err)
	}
	return manager, registry
}

func renderHTTPRejection(t *testing.T, inbound codec.Inbound, headers http.Header, body []byte) (*httptest.ResponseRecorder, *codec.Error) {
	t.Helper()
	writer := httptest.NewRecorder()
	reply := inbound.Reply(writer)
	_, err := inbound.Decode(t.Context(), headers, body)
	var public *codec.Error
	if !errors.As(err, &public) {
		t.Fatalf("expected a public codec rejection, got %v", err)
	}
	reply.WriteHeader(public.Status)
	if err := json.NewEncoder(reply).Encode(map[string]any{"error": map[string]string{"message": public.Message}}); err != nil {
		t.Fatal(err)
	}
	if err := reply.Finish(nil); err != nil {
		t.Fatal(err)
	}
	return writer, public
}

func TestInstalledInboundHTTPRejectionsMatchDirectCodec(t *testing.T) {
	_, registry := httpRejectionManager(t)
	for _, streaming := range []bool{false, true} {
		direct := httpRejectionCodec{streaming: streaming}
		for _, status := range []int{400, 413, 415, 504} {
			t.Run(direct.ID()+"/"+strconv.Itoa(status), func(t *testing.T) {
				headers := http.Header{"Content-Type": {"application/connect+proto"}}
				body := []byte("invalid")
				switch status {
				case 413:
					body = []byte("oversize")
				case 415:
					headers.Set("Content-Type", "text/plain")
				case 504:
					headers.Set("Connect-Timeout-Ms", "0")
				}
				want, rejection := renderHTTPRejection(t, direct, headers, body)
				remote, found := registry.Lookup(direct.Path())
				if !found {
					t.Fatal("installed codec route missing")
				}
				got, remoteRejection := renderHTTPRejection(t, codec.Bind(remote, t.Context()), headers, body)
				if rejection.Status != status || remoteRejection.Status != status ||
					got.Code != want.Code || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) ||
					got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
					t.Fatalf("native rejection changed across SDK: direct=%d/%d/%s remote=%d/%d/%s",
						rejection.Status, want.Code, want.Body.Bytes(), remoteRejection.Status, got.Code, got.Body.Bytes())
				}
				if strings.Contains(got.Body.String(), "credential") || got.Header().Get("Authorization") != "" {
					t.Fatal("native rejection exposed private authority")
				}
			})
		}
	}
}

func TestInstalledInboundCancellationMatchesDirectCodec(t *testing.T) {
	_, registry := httpRejectionManager(t)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			direct := httpRejectionCodec{streaming: true}
			writer := httptest.NewRecorder()
			reply := direct.Reply(writer)
			reply.WriteHeader(http.StatusOK)
			if err := reply.Finish(cause); err != nil {
				t.Fatal(err)
			}
			remote, _ := registry.Lookup(direct.Path())
			bound := codec.Bind(remote, t.Context())
			got := httptest.NewRecorder()
			response := bound.Reply(got)
			if _, err := bound.Decode(t.Context(), http.Header{"Content-Type": {"application/connect+proto"}}, []byte("valid")); err != nil {
				t.Fatal(err)
			}
			response.WriteHeader(http.StatusOK)
			if err := response.Finish(cause); err != nil {
				t.Fatal(err)
			}
			if got.Code != writer.Code || !bytes.Equal(got.Body.Bytes(), writer.Body.Bytes()) ||
				got.Header().Get("Content-Type") != writer.Header().Get("Content-Type") {
				t.Fatalf("cancellation changed across SDK: direct=%d/%s remote=%d/%s", writer.Code, writer.Body.Bytes(), got.Code, got.Body.Bytes())
			}
		})
	}
}

func TestInstalledInboundCanceledDecodePreservesContext(t *testing.T) {
	_, registry := httpRejectionManager(t)
	for _, deadline := range []bool{false, true} {
		cause := context.Canceled
		if deadline {
			cause = context.DeadlineExceeded
		}
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 0)
			}
			cancel()
			direct := httpRejectionCodec{}
			_, directErr := direct.Decode(ctx, nil, nil)
			remote, _ := registry.Lookup(direct.Path())
			_, remoteErr := codec.Bind(remote, ctx).Decode(ctx, nil, nil)
			if !errors.Is(directErr, cause) || !errors.Is(remoteErr, cause) {
				t.Fatalf("canceled decode changed across SDK: direct=%v remote=%v; want %v", directErr, remoteErr, cause)
			}
		})
	}
}

func TestInstalledInboundErrorFallbacksStayPrivate(t *testing.T) {
	_, registry := httpRejectionManager(t)
	for _, code := range []string{"http.302", "http.0504", "vendor.504", ""} {
		t.Run("code="+code, func(t *testing.T) {
			headers := http.Header{"Content-Type": {"application/connect+proto"}}
			if code == "" {
				headers.Set("X-Fixture-Private", "yes")
			} else {
				headers.Set("X-Fixture-Code", code)
			}
			remote, _ := registry.Lookup((httpRejectionCodec{}).Path())
			bound := codec.Bind(remote, t.Context())
			_, err := bound.Decode(t.Context(), headers, []byte("valid"))
			if err == nil || strings.Contains(err.Error(), "credential") {
				t.Fatal("private error was exposed or accepted", err)
			}
			var public *codec.Error
			if code == "" {
				if errors.As(err, &public) {
					t.Fatal("private cause became a public codec error", public)
				}
			} else if !errors.As(err, &public) || public.Status != 400 || public.Message != "safe custom error" {
				t.Fatal("unknown code changed the bounded fallback", err)
			}
			response := bound.Reply(httptest.NewRecorder())
			response.WriteHeader(http.StatusBadRequest)
			if err := response.Finish(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
