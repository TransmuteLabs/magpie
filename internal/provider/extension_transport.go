package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/requestpolicy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionResponseHead struct {
	headers     sdk.HeadersEvent
	err         error
	observation *requestpolicy.Observation
}

type extensionResponseKey struct{}

type extensionResponseBody struct {
	*io.PipeReader
	cancel           context.CancelFunc
	ctx              context.Context
	done             chan struct{}
	finished         atomic.Bool
	completedReceipt atomic.Bool
	protocolComplete atomic.Bool
	closeOnce        sync.Once
	closeErr         error
	mu               sync.Mutex
	usage            *sdk.Usage
}

const extensionReceiptGrace = 2 * time.Second

func (body *extensionResponseBody) ExtensionProtocolComplete() {
	body.protocolComplete.Store(true)
}

func ExtensionResponseProtocolComplete(response *http.Response) {
	if response.Request != nil {
		if body, ok := response.Request.Context().Value(extensionResponseKey{}).(*extensionResponseBody); ok {
			body.ExtensionProtocolComplete()
			return
		}
	}
	if terminal, ok := response.Body.(interface{ ExtensionProtocolComplete() }); ok {
		terminal.ExtensionProtocolComplete()
	}
}

func (body *extensionResponseBody) Close() error {
	body.closeOnce.Do(func() {
		uncertain := false
		if body.protocolComplete.Load() && !body.finished.Load() && body.ctx.Err() == nil {
			grace, cancelGrace := context.WithTimeout(body.ctx, extensionReceiptGrace)
			canceled := make(chan struct{})
			stopCancel := context.AfterFunc(grace, func() {
				body.cancel()
				_ = body.PipeReader.CloseWithError(grace.Err())
				close(canceled)
			})
			_, err := io.CopyN(io.Discard, body.PipeReader, sdk.MaxChunkBytes+1)
			if !stopCancel() {
				<-canceled
			}
			uncertain = err == nil || grace.Err() != nil
			cancelGrace()
		}
		body.cancel()
		body.closeErr = body.PipeReader.Close()
		<-body.done
		attempt := extensionAttemptOf(body.ctx)
		if (uncertain || !body.completedReceipt.Load()) && attempt.Outcome(true) != nil && attempt.Outcome(false) == nil {
			extensionOutcome(body.ctx, &extensionhost.OutcomeError{Outcome: "uncertain"})
		}
	})
	return body.closeErr
}

func (body *extensionResponseBody) ExtensionUsage() *sdk.Usage {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.usage.Clone()
}

func (body *extensionResponseBody) noteUsage(usage *sdk.Usage) {
	body.mu.Lock()
	defer body.mu.Unlock()
	if usage == nil {
		return
	}
	body.usage = usage.Clone()
}

func (binding *extensionAccount) execute(request *http.Request) (*http.Response, error) {
	ctx := extensionPolicyContext(request.Context())
	if err := extensionhost.CheckExecutor(ctx, binding.scope.Plugin, binding.host.Namespace()); err != nil {
		return nil, err
	}
	if err := ExtensionOutcome(ctx, true); err != nil {
		return nil, err
	}
	if attempt := extensionAttemptOf(ctx); attempt != nil && attempt.FinalRefusal() != nil {
		return nil, errors.New("final refusal requires an outer routing decision")
	}
	prepared, err := binding.prepare(ctx)
	if err != nil {
		return nil, err
	}
	binding = prepared
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
	protocol, operation, err := extensionRequestOperation(request, binding.descriptor)
	if err != nil {
		return nil, err
	}
	model := bodyModel(body)
	path := request.URL.Path
	if protocol == "gemini" {
		model = strings.Split(strings.SplitN(path, "/models/", 2)[1], ":")[0]
	}
	allowed := slicesContainProtocol(binding.descriptor, protocol)
	if operation == sdk.OperationSystemOne {
		allowed = protocol == "" && binding.decisionModel(model)
	} else if binding.decisionModel(model) {
		allowed = false
	} else {
		for _, declared := range binding.descriptor.Models {
			if declared.ID == model && declared.Kind == "decision" {
				allowed = false
			}
		}
	}
	if !allowed {
		return nil, errors.New("extension provider did not declare the requested protocol")
	}
	headers := request.Header.Clone()
	for name := range headers {
		switch strings.ToLower(name) {
		case "authorization", "x-api-key", "x-goog-api-key", "proxy-authorization", "cookie", "cookie2":
			delete(headers, name)
		}
	}
	scope := binding.scope
	scope.Operation, scope.Attempt = randomToken(18), 1
	scope.Session = request.Header.Get(ConversationHeader)
	if scope.Session == "" {
		scope.Session = request.Header.Get(SessionToPluginHeader)
	}
	optionsValue := sdk.ExecuteOptions{
		Method: request.Method, Path: path, Query: request.URL.RawQuery, Proxy: netproxy.Choice(ctx),
		CallerID: extensionhost.CallerIDFrom(ctx),
	}
	if binding.manifest.Contract.Version.Minor >= 3 {
		optionsValue.EscapedPath = request.URL.EscapedPath()
	}
	metadata := extensionhost.MetadataFrom(ctx)
	if metadata.Plugin == scope.Plugin {
		if len(metadata.Value) > 0 {
			optionsValue.Native = metadata.Value
		}
		if metadata.RequestID != "" {
			optionsValue.RequestID = metadata.RequestID
			parts := []any{scope.Plugin, scope.Provider, scope.Account,
				metadata.CallerID, scope.Session, metadata.RequestID}
			if operation == sdk.OperationSystemOne {
				digest := sha256.Sum256(body)
				parts = append(parts, operation, model, hex.EncodeToString(digest[:]))
			}
			identity, err := json.Marshal(parts)
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(identity)
			scope.Operation = "request-" + hex.EncodeToString(digest[:])
		}
	}
	if refusal, blocked := extensionAvailability.Blocked(binding.cacheKey(), model, time.Now()); blocked {
		return cachedRefusalResponse(ctx, request, scope, refusal), nil
	}
	if !requestpolicy.HasSelection(ctx) {
		ctx = requestpolicy.WithSelection(ctx, binding.cacheKey()+"\x00"+model, model)
	}
	selectedPolicy, policyErr := requestpolicy.FromContext(ctx).PolicyFor(ExtensionProviderID(scope.Plugin, scope.Provider), model, "", requestpolicy.Defaults())
	if policyErr != nil {
		return nil, policyErr
	}
	if !requestpolicy.ConsumeRoutePermit(ctx) {
		if err := requestpolicy.FromContext(ctx).Dispatch(ctx, selectedPolicy); err != nil {
			return nil, err
		}
	}
	extensionBegin(ctx, scope)
	options, err := json.Marshal(optionsValue)
	if err != nil {
		return nil, err
	}
	params := sdk.ExecuteRequest{Protocol: protocol, Operation: operation, Model: model, Body: body, Headers: headers,
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
				if sdk.ValidateExecutionUsage(binding.manifest, &usage) != nil {
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
		if sdk.ValidateExecutionUsage(binding.manifest, result.Usage) != nil || seenHead && result.Status != 0 && result.Status != responseStatus {
			finish(&extensionhost.OutcomeError{Outcome: "uncertain", Cause: errors.New("invalid extension execution totals or status")})
			return
		}
		refusal, refusalErr := binding.finalRequestRefusal(ctx, scope, model, result, raw, seenHead)
		if refusalErr != nil {
			finish(&extensionhost.OutcomeError{Outcome: "uncertain", Cause: refusalErr})
			return
		}
		if refusal != nil {
			seenHead = true
			head <- extensionResponseHead{headers: sdk.HeadersEvent{Status: refusalStatus(refusal), Headers: http.Header{"Content-Type": {"application/json"}}}, observation: &refusal.Observation}
			_, writeErr := writer.Write(refusalResponseBody(refusal))
			finish(writeErr)
			return
		}
		responseBody.noteUsage(result.Usage)
		extensionUsage(ctx, result.Usage)
		if result.Outcome == "uncertain" || result.Outcome == "accepted" {
			finish(&extensionhost.OutcomeError{Outcome: result.Outcome})
			return
		}
		if result.Outcome != "not-started" {
			extensionCompleted(ctx)
			responseBody.completedReceipt.Store(true)
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
		responseContext := context.WithValue(request.Context(), extensionResponseKey{}, responseBody)
		if value.observation != nil {
			responseContext = context.WithValue(responseContext, extensionRefusalResponseKey{}, *value.observation)
		}
		return &http.Response{StatusCode: value.headers.Status,
			Status: fmt.Sprintf("%d %s", value.headers.Status, http.StatusText(value.headers.Status)),
			Header: headers, Body: responseBody,
			Request: request.WithContext(responseContext), ContentLength: -1,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
	case <-ctx.Done():
		responseBody.Close()
		return nil, ctx.Err()
	}
}

func slicesContainProtocol(descriptor sdk.ProviderDescriptor, protocol string) bool {
	for _, candidate := range descriptor.Protocols {
		if string(extensionProtocol(string(candidate))) == protocol {
			return true
		}
	}
	return false
}
