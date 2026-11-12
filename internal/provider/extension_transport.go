package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionResponseHead struct {
	headers sdk.HeadersEvent
	err     error
}

type extensionResponseBody struct {
	*io.PipeReader
	cancel   context.CancelFunc
	ctx      context.Context
	done     chan struct{}
	finished atomic.Bool
	mu       sync.Mutex
	usage    *sdk.Usage
}

func (body *extensionResponseBody) Close() error {
	body.cancel()
	err := body.PipeReader.Close()
	if !body.finished.Load() {
		extensionOutcome(body.ctx, &extensionhost.OutcomeError{Outcome: "uncertain"})
	}
	<-body.done
	return err
}

func (body *extensionResponseBody) ExtensionUsage() *sdk.Usage {
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.usage == nil {
		return nil
	}
	usage := *body.usage
	return &usage
}

func (body *extensionResponseBody) noteUsage(usage *sdk.Usage) {
	body.mu.Lock()
	defer body.mu.Unlock()
	if usage == nil {
		return
	}
	copy := *usage
	body.usage = &copy
}

func (binding *extensionAccount) execute(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	if err := extensionAttemptOf(ctx).Outcome(true); err != nil {
		return nil, err
	}
	if err := binding.check(ctx); err != nil {
		return nil, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return nil, err
	}
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(request.Body, sdk.MaxNativeBodyBytes+1))
		request.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > sdk.MaxNativeBodyBytes {
			return nil, errors.New("extension provider request exceeds its body limit")
		}
	}
	protocol, model := "", bodyModel(body)
	path := request.URL.Path
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		protocol = "chat"
	case strings.HasSuffix(path, "/responses"):
		protocol = "responses"
	case strings.Contains(path, "/messages"):
		protocol = "anthropic"
	case strings.Contains(path, "/models/") && strings.Contains(path, ":"):
		protocol = "gemini"
		model = strings.Split(strings.SplitN(path, "/models/", 2)[1], ":")[0]
	default:
		return nil, errors.New("extension provider does not support this operation")
	}
	allowed := slicesContainProtocol(binding.descriptor, protocol)
	if !allowed {
		return nil, errors.New("extension provider did not declare the requested protocol")
	}
	headers := request.Header.Clone()
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization"} {
		headers.Del(name)
	}
	scope := binding.scope
	scope.Operation, scope.Attempt = randomToken(18), 1
	scope.Session = request.Header.Get(ConversationHeader)
	if scope.Session == "" {
		scope.Session = request.Header.Get(SessionToPluginHeader)
	}
	optionsValue := map[string]any{
		"method": request.Method, "path": path, "query": request.URL.RawQuery, "proxy": netproxy.Choice(ctx),
	}
	metadata := extensionhost.MetadataFrom(ctx)
	if metadata.Plugin == scope.Plugin {
		if metadata.CallerID != "" {
			optionsValue["callerId"] = metadata.CallerID
		}
		if len(metadata.Value) > 0 {
			optionsValue["native"] = metadata.Value
		}
		if metadata.RequestID != "" {
			optionsValue["requestId"] = metadata.RequestID
			identity, err := json.Marshal([]any{scope.Plugin, scope.Provider, scope.Account,
				metadata.CallerID, scope.Session, metadata.RequestID})
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(identity)
			scope.Operation = "request-" + hex.EncodeToString(digest[:])
		}
	}
	extensionBegin(ctx, scope)
	options, err := json.Marshal(optionsValue)
	if err != nil {
		return nil, err
	}
	params := sdk.ExecuteRequest{Protocol: protocol, Model: model, Body: body, Headers: headers,
		OperationID: scope.Operation, Options: options}
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	responseBody := &extensionResponseBody{PipeReader: reader, cancel: cancel, ctx: ctx, done: make(chan struct{})}
	head := make(chan extensionResponseHead, 1)
	go func() {
		defer close(responseBody.done)
		seenHead := false
		responseStatus := 0
		finish := func(err error) {
			extensionOutcome(ctx, err)
			responseBody.finished.Store(true)
			if !seenHead {
				head <- extensionResponseHead{err: err}
			}
			_ = writer.CloseWithError(err)
		}
		raw, err := binding.host.Call(ctx, scope.Plugin, sdk.MethodProviderExecute, scope, params, func(event extensionhost.Event) error {
			switch event.Name {
			case "headers":
				var value sdk.HeadersEvent
				if seenHead || json.Unmarshal(event.Data, &value) != nil || value.Status < 200 || value.Status > 599 {
					return errors.New("extension provider returned invalid response headers")
				}
				seenHead = true
				responseStatus = value.Status
				head <- extensionResponseHead{headers: value}
			case "data":
				var chunk []byte
				if !seenHead || json.Unmarshal(event.Data, &chunk) != nil || len(chunk) > sdk.MaxChunkBytes {
					return errors.New("extension provider returned an invalid response chunk")
				}
				_, err := writer.Write(chunk)
				return err
			case "usage":
				var usage sdk.Usage
				if err := json.Unmarshal(event.Data, &usage); err != nil {
					return errors.New("extension provider returned invalid usage")
				}
				if !validExtensionUsage(&usage) {
					return errors.New("extension provider returned invalid usage totals")
				}
				responseBody.noteUsage(&usage)
				extensionUsage(ctx, &usage)
			case "accepted":
				extensionAccepted(ctx)
			case "flush":
			default:
				return errors.New("extension provider returned an unknown response event")
			}
			return nil
		})
		if err != nil {
			finish(err)
			return
		}
		var result sdk.ExecuteResult
		if err := json.Unmarshal(raw, &result); err != nil {
			finish(&extensionhost.OutcomeError{Outcome: "uncertain", Cause: errors.New("invalid extension execution result")})
			return
		}
		responseBody.noteUsage(result.Usage)
		if !validExtensionUsage(result.Usage) || seenHead && result.Status != 0 && result.Status != responseStatus {
			finish(&extensionhost.OutcomeError{Outcome: "uncertain", Cause: errors.New("invalid extension execution totals or status")})
			return
		}
		extensionUsage(ctx, result.Usage)
		if result.Outcome == "uncertain" || result.Outcome == "accepted" {
			finish(&extensionhost.OutcomeError{Outcome: result.Outcome})
			return
		}
		if result.Outcome != "not-started" {
			extensionCompleted(ctx)
		}
		if !seenHead {
			if result.Status < 200 || result.Status > 599 {
				finish(&extensionhost.OutcomeError{Outcome: "uncertain", Cause: errors.New("extension provider returned no response status")})
				return
			}
			seenHead = true
			head <- extensionResponseHead{headers: sdk.HeadersEvent{Status: result.Status, Headers: result.Headers}}
		}
		finish(nil)
	}()
	select {
	case value := <-head:
		if value.err != nil {
			responseBody.Close()
			return nil, value.err
		}
		headers := value.headers.Headers.Clone()
		if headers == nil {
			headers = http.Header{}
		}
		headers.Del("Content-Length")
		headers.Del("Content-Encoding")
		return &http.Response{StatusCode: value.headers.Status,
			Status: fmt.Sprintf("%d %s", value.headers.Status, http.StatusText(value.headers.Status)),
			Header: headers, Body: responseBody, Request: request, ContentLength: -1,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
	case <-ctx.Done():
		responseBody.Close()
		return nil, ctx.Err()
	}
}

func validExtensionUsage(usage *sdk.Usage) bool {
	if usage == nil {
		return true
	}
	for _, count := range []int64{usage.InputTokens, usage.OutputTokens, usage.CachedTokens, usage.CacheWriteTokens} {
		if count < 0 || int64(int(count)) != count {
			return false
		}
	}
	return !math.IsNaN(usage.Cost) && !math.IsInf(usage.Cost, 0) && usage.Cost >= 0
}

func slicesContainProtocol(descriptor sdk.ProviderDescriptor, protocol string) bool {
	for _, candidate := range descriptor.Protocols {
		if string(extensionProtocol(string(candidate))) == protocol {
			return true
		}
	}
	return false
}
