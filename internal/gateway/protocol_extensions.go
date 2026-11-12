package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/protocolext"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/protocolext"
)

const protocolBodyLimit = 16 << 20

func NewWithProtocols(registry *sdk.Registry) (*Server, error) {
	host, err := protocolext.New(registry, protocolPathReserved)
	if err != nil {
		return nil, err
	}
	server := New()
	server.protocols = host
	server.extensions = nil
	return server, nil
}

func protocolPathReserved(endpoint string) bool {
	if ClientPath(endpoint) {
		return true
	}
	for _, prefix := range []string{"/api", "/_magpie", "/agent"} {
		if endpoint == prefix || strings.HasPrefix(endpoint, prefix+"/") {
			return true
		}
	}
	return strings.HasPrefix(endpoint, "/wsl-")
}

type protocolRegistryCtx struct{}

type protocolBinding struct {
	server *Server
	host   *protocolext.Host
	err    error
}

func (server *Server) protocolHost() (*protocolext.Host, error) {
	if server.protocols != nil || server.extensions == nil {
		return server.protocols, nil
	}
	registry, err := server.extensions.Protocols()
	if err != nil {
		return nil, err
	}
	return protocolext.New(registry, protocolPathReserved)
}

func (server *Server) withProtocolRegistry(next http.Handler) http.Handler {
	if server.protocols == nil && server.extensions == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if pinned, ok := request.Context().Value(protocolRegistryCtx{}).(protocolBinding); ok && pinned.server == server {
			next.ServeHTTP(writer, request)
			return
		}
		host, err := server.protocolHost()
		ctx := context.WithValue(request.Context(), protocolRegistryCtx{}, protocolBinding{server: server, host: host, err: err})
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func registeredProtocolRequest(request *http.Request) bool {
	binding, _ := request.Context().Value(protocolRegistryCtx{}).(protocolBinding)
	return binding.host.ClientPath(request.URL.Path)
}

func (server *Server) protocolEntry(next http.Handler) http.Handler {
	if server.protocols == nil && server.extensions == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		binding, _ := request.Context().Value(protocolRegistryCtx{}).(protocolBinding)
		if binding.server != server {
			host, err := server.protocolHost()
			binding = protocolBinding{server: server, host: host, err: err}
		}
		if binding.err != nil && !ClientPath(request.URL.Path) {
			writeError(writer, provider.Chat, http.StatusServiceUnavailable, "extension catalog is unavailable")
			return
		}
		inbound, found := binding.host.Lookup(request.URL.Path)
		if !found {
			next.ServeHTTP(writer, request)
			return
		}
		if server.extensions != nil {
			request = request.WithContext(extension.WithInboundLease(request.Context()))
		}
		inbound = sdk.Bind(inbound, request.Context())
		if inbound == nil {
			writeError(writer, provider.Chat, http.StatusBadGateway, "protocol adapter did not bind this request")
			return
		}
		began := time.Now()
		reply := inbound.Reply(writer)
		cancelCall := func() {}
		defer func() {
			defer cancelCall()
			if err := reply.Finish(request.Context().Err()); err != nil && request.Context().Err() == nil {
				log.Printf("protocol adapter %s finished with an error", inbound.ID())
			}
		}()
		errorProtocol := provider.Chat
		canonicalTool := false
		refuse := func(status int, message string) {
			if canonicalTool {
				reply.Header().Set("Content-Type", "application/json")
				reply.WriteHeader(status)
				_ = json.NewEncoder(reply).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32603, "message": message}})
				return
			}
			writeError(reply, errorProtocol, status, message)
		}
		if request.URL.RawPath != "" {
			refuse(http.StatusBadRequest, "protocol endpoint requires its literal path")
			return
		}
		if request.Method != http.MethodPost {
			reply.Header().Set("Allow", http.MethodPost)
			refuse(http.StatusMethodNotAllowed, "protocol endpoint requires POST")
			return
		}
		limits := server.requestLimits
		limits.body = protocolBodyLimit
		if server.requestLimits.body > 0 {
			limits.body = min(limits.body, server.requestLimits.body)
		}
		body, status, err := readBoundedRequestBody(writer, request, limits)
		if err != nil {
			refuse(status, "could not read bounded protocol request")
			return
		}
		decoded, err := inbound.Decode(request.Context(), request.Header.Clone(), body)
		if err != nil {
			status, message := http.StatusBadRequest, "invalid protocol request"
			var public *sdk.Error
			if errors.As(err, &public) {
				if public.Status >= 400 && public.Status <= 599 {
					status = public.Status
				}
				message = public.Message
			}
			refuse(status, message)
			return
		}
		if request.Context().Err() != nil {
			return
		}
		if decoded.Timeout < 0 {
			refuse(http.StatusBadGateway, "adapter produced an invalid timeout")
			return
		}
		if decoded.Timeout > 0 {
			ctx, cancel := context.WithDeadline(request.Context(), began.Add(decoded.Timeout))
			cancelCall = cancel
			request = request.WithContext(ctx)
			if ctx.Err() != nil {
				refuse(http.StatusGatewayTimeout, "protocol request deadline expired")
				return
			}
		}
		if contextual, ok := reply.(interface{ SetContext(context.Context) }); ok {
			contextual.SetContext(request.Context())
		}
		method, target := http.MethodPost, "/v1/chat/completions"
		switch decoded.Protocol {
		case sdk.Chat, sdk.Responses, sdk.Messages, sdk.Anthropic, sdk.Gemini:
			if !json.Valid(decoded.Body) || len(decoded.Body) > protocolBodyLimit {
				refuse(http.StatusBadGateway, "adapter produced an invalid chat request")
				return
			}
			switch decoded.Protocol {
			case sdk.Responses:
				target = "/v1/responses"
				errorProtocol = provider.Responses
			case sdk.Messages, sdk.Anthropic:
				target = "/v1/messages"
				errorProtocol = provider.Anthropic
			case sdk.Gemini:
				var value struct {
					Model  string `json:"model"`
					Stream bool   `json:"stream"`
				}
				if json.Unmarshal(decoded.Body, &value) != nil || !validProtocolModel(value.Model) {
					refuse(http.StatusBadGateway, "adapter produced an invalid Gemini model")
					return
				}
				target = "/v1beta/models/" + value.Model + ":generateContent"
				if value.Stream {
					target = "/v1beta/models/" + value.Model + ":streamGenerateContent"
				}
				errorProtocol = provider.Gemini
			}
		case sdk.Models:
			if !inbound.Contract().Has(sdk.ModelDiscovery) || len(decoded.Body) != 0 {
				refuse(http.StatusBadGateway, "adapter did not declare a valid catalog operation")
				return
			}
			method, target = http.MethodGet, "/v1/models"
		case sdk.Tool:
			canonicalTool = true
			var err error
			decoded.Body, err = server.protocolToolBody(request, inbound, decoded.Body)
			if err != nil {
				refuse(http.StatusBadGateway, "native module tool dispatch is unavailable")
				return
			}
			target = ToolsMCPPath
		default:
			refuse(http.StatusBadGateway, "adapter produced an unsupported wire request")
			return
		}
		if len(decoded.Metadata) > 64<<10 || len(decoded.Metadata) > 0 && !json.Valid(decoded.Metadata) ||
			len(decoded.RequestID) > 256 || strings.ContainsAny(decoded.RequestID, "\r\n\x00") {
			refuse(http.StatusBadGateway, "adapter produced invalid request metadata")
			return
		}
		normalized := request.Clone(request.Context())
		if len(decoded.Metadata) > 0 || decoded.RequestID != "" {
			owner, _, _ := strings.Cut(inbound.ID(), ":")
			normalized = normalized.WithContext(extension.WithRequestMetadata(normalized.Context(),
				extension.RequestMetadata{Plugin: owner, RequestID: decoded.RequestID, CallerID: extension.CallerIDFrom(request.Context()), Value: decoded.Metadata}))
		}
		normalized.Method = method
		normalized.URL.Path, normalized.URL.RawPath, normalized.URL.RawQuery = target, "", ""
		normalized.RequestURI = normalized.URL.RequestURI()
		normalized.Body = io.NopCloser(bytes.NewReader(decoded.Body))
		normalized.ContentLength = int64(len(decoded.Body))
		normalized.GetBody = nil
		normalized.TransferEncoding = nil
		normalized.Trailer = nil
		normalized.Header = make(http.Header)
		keptHeaders := append([]string{"User-Agent", "Authorization", "X-Api-Key", "X-Goog-Api-Key", PagePortHeader}, forwardHeaders...)
		keptHeaders = append(keptHeaders, sessionHeaders...)
		for _, name := range keptHeaders {
			if values := request.Header.Values(name); len(values) > 0 {
				normalized.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
			}
		}
		session := sessionOf(request.Header)
		if session == "" {
			session = boundedSession(decoded.Session)
		}
		if session != "" {
			normalized.Header.Set(SessionHeader, session)
			if original, ok := normalized.Context().Value(callerCtx{}).(caller); ok {
				original.session = session
				normalized = normalized.WithContext(context.WithValue(normalized.Context(), callerCtx{}, original))
			}
		}
		if decoded.Protocol != sdk.Models {
			normalized.Header.Set("Content-Type", "application/json")
			normalized.Header.Set("Accept", "text/event-stream")
			if decoded.Protocol == sdk.Tool {
				normalized.Header.Set("Accept", "application/json")
			}
		} else {
			normalized.Body = nil
			normalized.Header.Set("Accept", "application/json")
		}
		normalized = aliasRequest(normalized, access.Caller(normalized.Context()))
		next.ServeHTTP(reply, normalized)
	})
}

func validProtocolModel(model string) bool {
	checked, err := validateModel(model)
	if err != nil || checked != model || strings.ContainsAny(model, "?#%\\\r\n") {
		return false
	}
	for _, segment := range strings.Split(model, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
