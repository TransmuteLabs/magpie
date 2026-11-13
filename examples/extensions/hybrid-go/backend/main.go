package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	hybrid "github.com/yetone/magpie/examples/extensions/hybrid-go"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	manifest, err := sdk.ServiceManifest(hybrid.Manifest(), "backend")
	if err != nil {
		panic(err)
	}
	backend := &service{}
	module := sdk.Module{
		Manifest: manifest, Start: backend.start,
		Actions: map[string]sdk.Handler{"backend.pid": backend.read, "backend.run": backend.run},
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, "Hybrid backend stopped")
		os.Exit(1)
	}
}

type service struct {
	mu       sync.Mutex
	endpoint string
	active   atomic.Int64
}

func (backend *service) read(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
	var input hybrid.Input
	if err := json.Unmarshal(call.Params, &input); err != nil {
		return nil, err
	}
	result, err := hybrid.Describe(ctx, call, input.Text)
	if call.Action == "backend.run" {
		// A module action result is domain JSON, not ExecuteResult's protocol.
		result.Outcome = "accepted"
	}
	backend.mu.Lock()
	result.Endpoint = backend.endpoint
	backend.mu.Unlock()
	return result, err
}

func (backend *service) run(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	backend.active.Add(1)
	defer backend.active.Add(-1)
	var input hybrid.Input
	if err := json.Unmarshal(call.Params, &input); err != nil {
		return nil, err
	}
	if err := hybrid.Accept(ctx, call, stream); err != nil {
		return nil, err
	}
	var previous sdk.StateValue
	if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "hybrid.last"}, &previous); err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"text": input.Text})
	if err != nil {
		return nil, err
	}
	if err := stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{
		Key: "hybrid.last", Version: previous.Version, Value: body,
	}, new(sdk.StateCASResult)); err != nil {
		return nil, err
	}
	if input.Wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return backend.read(ctx, call, stream)
}

func (backend *service) start(ctx context.Context, module sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
	var config hybrid.Config
	if err := json.Unmarshal(module.Config.Values, &config); err != nil {
		return sdk.ModuleReady{}, nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return sdk.ModuleReady{}, nil, err
	}
	address := "http://" + listener.Addr().String()
	backend.mu.Lock()
	backend.endpoint = address
	backend.mu.Unlock()
	mux := http.NewServeMux()
	status := func() map[string]any {
		return map[string]any{
			"pid": os.Getpid(), "version": hybrid.Version, "component": module.Component,
			"instance": module.Instance, "configVersion": module.Config.Version, "value": config.Value,
			"active": backend.active.Load(),
		}
	}
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := json.Marshal(status())
		if _, err := fmt.Fprintf(w, "event: ready\ndata: %s\n\n", body); err != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
		case <-ctx.Done():
		}
	})
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for name, values := range module.HTTPHeaders() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			mux.ServeHTTP(w, r)
		}),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	done := make(chan struct{})
	var once sync.Once
	dispose := func() {
		once.Do(func() {
			_ = server.Close()
			_ = listener.Close()
			<-done
		})
	}
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "Hybrid HTTP service stopped")
		}
	}()
	stop := context.AfterFunc(ctx, dispose)
	cleanup := func() {
		stop()
		dispose()
	}
	ready := sdk.ModuleReady{Endpoints: map[string]string{"ui": address}}
	if config.FailStart {
		return ready, cleanup, &sdk.Error{
			Code: "fixture_start", Message: fmt.Sprintf("Hybrid startup failed pid=%d endpoint=%s", os.Getpid(), address),
			Outcome: "not-started",
		}
	}
	return ready, cleanup, nil
}
