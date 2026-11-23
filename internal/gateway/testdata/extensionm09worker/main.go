package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/extension/native"
)

//go:embed magpie-plugin.json
var descriptor []byte

var upstreamHTTP = &http.Client{
	Transport: &http.Transport{DisableCompression: true},
	Timeout:   30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func module(kind string) sdk.Module {
	var manifest sdk.Manifest
	if err := json.Unmarshal(descriptor, &manifest); err != nil {
		panic(err)
	}
	manifest.Runtime = kind
	if kind == "native" {
		manifest.Entry = "worker.so"
		if runtime.GOOS == "darwin" {
			manifest.Entry = "worker.dylib"
		}
	}
	unsupported := func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, &sdk.Error{Code: "unsupported", Message: "the fixture uses a provisioned account", Outcome: "not-started"}
	}
	return sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderExecute: execute,
		sdk.MethodAuthStart:       unsupported,
		sdk.MethodAuthFinish:      unsupported,
		sdk.MethodAuthRefresh:     unsupported,
		sdk.MethodAuthLogout:      unsupported,
	}}
}

func init() {
	if err := native.Register(module("native")); err != nil && !errors.Is(err, native.ErrUnavailable) {
		panic(err)
	}
}

func execute(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ExecuteRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	var auth struct{ Origin, Marker string }
	if err := json.Unmarshal(call.Auth, &auth); err != nil {
		return nil, err
	}
	origin, err := url.Parse(auth.Origin)
	if err != nil || origin.Scheme != "http" || origin.Hostname() != "127.0.0.1" ||
		origin.Port() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" ||
		auth.Marker == "" || request.Protocol != "responses" || request.Model != "m1" || request.OperationID != call.Scope.Operation ||
		call.Scope.Account != "alice" || request.Headers.Get("Authorization") != "" {
		return nil, &sdk.Error{Code: "invalid_scope", Message: "unexpected M09 fixture route", Outcome: "not-started"}
	}
	target := origin.JoinPath("/v1/responses").String()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("X-Fake-Run", auth.Marker)
	upstream.Header.Set("X-Fixture-Operation", call.Scope.Operation)
	upstream.Header.Set("X-Fixture-Pid", strconv.Itoa(os.Getpid()))
	upstream.Header.Set("X-Fixture-Parent-Pid", strconv.Itoa(os.Getppid()))
	upstream.Header.Set("X-Fixture-Gomaxprocs", strconv.Itoa(runtime.GOMAXPROCS(0)))
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	reply, err := upstreamHTTP.Do(upstream)
	if err != nil {
		return nil, err
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK || reply.Header.Get("Content-Type") != "text/event-stream" {
		return nil, errors.New("unexpected M09 fixture response")
	}
	headers := http.Header{"Content-Type": {"text/event-stream"}}
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: reply.StatusCode, Headers: headers, SentModel: request.Model}); err != nil {
		return nil, err
	}
	if err := emitFrames(ctx, stream, reply.Body); err != nil {
		return nil, err
	}
	totals := &sdk.Usage{InputTokens: 16, OutputTokens: 32}
	if err := stream.Emit(ctx, "usage", totals); err != nil {
		return nil, err
	}
	return sdk.ExecuteResult{Status: reply.StatusCode, SentModel: request.Model, Outcome: "completed", Usage: totals}, nil
}

func emitFrames(ctx context.Context, stream *sdk.Stream, body io.Reader) error {
	input := bufio.NewReaderSize(body, sdk.MaxChunkBytes)
	frame := make([]byte, 0, 20<<10)
	for {
		line, err := input.ReadSlice('\n')
		if err == io.EOF {
			if len(line)+len(frame) != 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		if len(frame)+len(line) > sdk.MaxChunkBytes {
			return errors.New("M09 fixture frame exceeds SDK data limit")
		}
		frame = append(frame, line...)
		if len(line) == 1 {
			if err := stream.Emit(ctx, "data", frame); err != nil {
				return err
			}
			frame = frame[:0]
		}
	}
}

func main() {
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module("exec")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
