package extension

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const p19GatewayFinishQuery = "p19-sdk-finish-digest"

type p19GatewayTrace struct {
	Operation   string
	RequestSize int
	ReplySize   int
	RequestHash [sha256.Size]byte
	ReplyHash   [sha256.Size]byte
	Success     bool
}

func p19GatewayDigest(response sdk.GatewayResponse, operation string) p19GatewayTrace {
	return p19GatewayTrace{
		Operation: operation, RequestSize: len(response.Request), ReplySize: len(response.Reply),
		RequestHash: sha256.Sum256(response.Request), ReplyHash: sha256.Sum256(response.Reply),
		Success: response.Success,
	}
}

func p19GatewayPayload(size int, fill byte) json.RawMessage {
	payload := bytes.Repeat([]byte{fill}, size)
	copy(payload, `{"value":"`)
	copy(payload[size-2:], `"}`)
	return payload
}

func p19BeginGateway(t testing.TB, ctx context.Context, manager *Manager, request json.RawMessage) *GatewayRun {
	t.Helper()
	run := manager.BeginGateway(ctx, request, sdk.GatewayContext{
		Origin: "local", Protocol: "responses", Model: "fixture", Query: p19GatewayFinishQuery,
	})
	if run == nil || !run.ObservesResponse() {
		t.Fatal("response-only SDK hook was not admitted")
	}
	return run
}

func p19JoinGateway(t testing.TB, runs []*GatewayRun) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for _, run := range runs {
		select {
		case <-run.done:
		case <-timer.C:
			t.Fatal("response hook callbacks did not release their runs")
		}
	}
}

func p19VerifyGatewayTrace(t testing.TB, path string, count int, request, reply json.RawMessage) []string {
	t.Helper()
	trace, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	expected := p19GatewayDigest(sdk.GatewayResponse{Request: request, Reply: reply, Success: true}, "")
	seen := make(map[string]struct{}, count)
	operations := make([]string, 0, count)
	lines := bufio.NewScanner(trace)
	for lines.Scan() {
		var record p19GatewayTrace
		if err := json.Unmarshal(lines.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		operation := record.Operation
		if _, duplicate := seen[operation]; operation == "" || duplicate {
			t.Fatalf("duplicate or empty response operation %q", operation)
		}
		seen[operation] = struct{}{}
		operations = append(operations, operation)
		record.Operation = ""
		if record != expected {
			t.Fatalf("response hook operation %q observed %+v, want %+v", operation, record, expected)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if len(operations) != count {
		t.Fatalf("response hook callbacks = %d, want %d", len(operations), count)
	}
	return operations
}

func TestP19SDKGatewayFinishWorker(t *testing.T) {
	manager, trace := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Response: true, TimeoutMs: 10_000})
	request := p19GatewayPayload(1<<20, 'a')
	reply := p19GatewayPayload(1<<20, 'b')
	explicit := p19GatewayPayload(1<<20, 'c')
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			if err := os.WriteFile(trace, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			original := bytes.Clone(request)
			prepared := bytes.Clone(explicit)
			delivered := bytes.Clone(reply)
			run := p19BeginGateway(t, ctx, manager, original)
			defer run.Close()
			original[len(original)-3] = 'x'
			cancel()
			wanted := request
			if override {
				wanted = explicit
				run.Finish(ctx, delivered, true, prepared)
			} else {
				run.Finish(ctx, delivered, true)
			}
			prepared[len(prepared)-3] = 'y'
			delivered[len(delivered)-3] = 'z'
			run.Finish(ctx, nil, false)
			run.Close()
			p19JoinGateway(t, []*GatewayRun{run})
			for _, operation := range p19VerifyGatewayTrace(t, trace, 1, wanted, reply) {
				receipt, err := manager.Store().Operation("memory-fixture", operation)
				if err != nil || receipt.State != "completed" {
					t.Fatalf("response hook receipt = %+v, err = %v", receipt, err)
				}
			}
		})
	}
}

func BenchmarkP19SDKGatewayFinish(b *testing.B) {
	for _, size := range []int{4 << 10, 1 << 20} {
		for _, batch := range []int{1, 8} {
			b.Run(fmt.Sprintf("%dKiB/batch=%d", size>>10, batch), func(b *testing.B) {
				manager, trace := gatewayFixtureManager(b, sdk.GatewayHookDescriptor{Response: true, TimeoutMs: 10_000})
				request := p19GatewayPayload(size, 'a')
				reply := p19GatewayPayload(size, 'b')
				ctx := b.Context()
				runs := make([]*GatewayRun, batch)
				warm := p19BeginGateway(b, ctx, manager, request)
				warm.Finish(ctx, reply, true, request)
				p19JoinGateway(b, []*GatewayRun{warm})
				p19VerifyGatewayTrace(b, trace, 1, request, reply)
				if err := os.WriteFile(trace, nil, 0o600); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				var foreground, finishToJoin time.Duration
				for b.Loop() {
					for index := range runs {
						runs[index] = p19BeginGateway(b, ctx, manager, request)
					}
					start := time.Now()
					for _, run := range runs {
						run.Finish(ctx, reply, true, request)
					}
					foreground += time.Since(start)
					p19JoinGateway(b, runs)
					finishToJoin += time.Since(start)
					clear(runs)
				}
				turns := b.N * batch
				b.ReportMetric(float64(batch), "turns/op")
				b.ReportMetric(float64(foreground.Nanoseconds())/float64(turns), "ns/turn-finish")
				b.ReportMetric(float64(finishToJoin.Nanoseconds())/float64(turns), "ns/turn-finish+join")
				p19VerifyGatewayTrace(b, trace, turns, request, reply)
			})
		}
	}
}
