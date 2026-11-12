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

func (server *Server) withProtocolRegistry(next http.Handler) http.Handler {
	if server.protocols == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := context.WithValue(request.Context(), protocolRegistryCtx{}, server.protocols)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func registeredProtocolRequest(request *http.Request) bool {
	host, _ := request.Context().Value(protocolRegistryCtx{}).(*protocolext.Host)
	return host.ClientPath(request.URL.Path)
}

func (server *Server) protocolEntry(next http.Handler) http.Handler {
	if server.protocols == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		inbound, found := server.protocols.Lookup(request.URL.Path)
		if !found {
			next.ServeHTTP(writer, request)
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
		refuse := func(status int, message string) {
			writeError(reply, provider.Chat, status, message)
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
		method, target := http.MethodPost, "/v1/chat/completions"
		switch decoded.Protocol {
		case sdk.Chat:
			if !json.Valid(decoded.Body) || len(decoded.Body) > protocolBodyLimit {
				refuse(http.StatusBadGateway, "adapter produced an invalid chat request")
				return
			}
		case sdk.Models:
			if !inbound.Contract().Has(sdk.ModelDiscovery) || len(decoded.Body) != 0 {
				refuse(http.StatusBadGateway, "adapter did not declare a valid catalog operation")
				return
			}
			method, target = http.MethodGet, "/v1/models"
		default:
			refuse(http.StatusBadGateway, "adapter produced an unsupported wire request")
			return
		}
		normalized := request.Clone(request.Context())
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
		if decoded.Protocol == sdk.Chat {
			normalized.Header.Set("Content-Type", "application/json")
			normalized.Header.Set("Accept", "text/event-stream")
		} else {
			normalized.Body = nil
			normalized.Header.Set("Accept", "application/json")
		}
		normalized = aliasRequest(normalized, access.Caller(normalized.Context()))
		next.ServeHTTP(reply, normalized)
	})
}
