package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionProviderTerminalCloseWaitsForExecuteResult(test *testing.T) {
	for _, protocol := range []string{"whole", "stream"} {
		test.Run(protocol, func(test *testing.T) {
			checkExtensionTerminalClose(test, protocol, "completed")
		})
	}
}

func TestExtensionProviderTerminalClosePreservesUncertainty(test *testing.T) {
	for _, protocol := range []string{"whole", "stream"} {
		for _, mode := range []string{"late-error", "trailing-invalid-envelope", "caller-canceled", "caller-abort-pending", "missing-receipt", "timeout-late-completed", "excess-tail", "excess-tail-completed", "early-close-pending", "parse-error-pending", "nonterminal-pending"} {
			test.Run(protocol+"/"+mode, func(test *testing.T) {
				checkExtensionTerminalClose(test, protocol, mode)
			})
		}
	}
}

func TestExtensionProviderTerminalCloseKeepsCompletedReceiptAfterProtocolRejection(test *testing.T) {
	for _, protocol := range []string{"whole", "stream"} {
		for _, mode := range []string{"early-close", "parse-error", "nonterminal"} {
			test.Run(protocol+"/"+mode, func(test *testing.T) {
				checkExtensionTerminalClose(test, protocol, mode)
			})
		}
	}
}

func checkExtensionTerminalClose(test *testing.T, protocol, mode string) {
	test.Helper()
	host := fixtureExtensionProvider(test, true)
	selected := extensionAccounts()[0]
	ctx, attempt := WithExtensionAttempt(test.Context())
	ctx, cancel := context.WithCancel(ctx)
	tailWritten, executed := make(chan struct{}), make(chan struct{})
	releaseReceipt := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseReceipt) }) }
	promptReceipt := mode == "early-close" || mode == "parse-error" || mode == "nonterminal"
	withoutTerminal := promptReceipt || strings.HasSuffix(mode, "-pending")
	lateReceipt := mode == "excess-tail-completed" || mode == "timeout-late-completed"
	if promptReceipt {
		release()
	}
	var calls atomic.Int32
	host.call = func(ctx context.Context, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
		calls.Add(1)
		completed := false
		defer func() {
			if !completed {
				if _, err := host.store.Journal(scope, "uncertain"); err != nil {
					test.Error(err)
				}
			}
			close(executed)
		}()
		for _, state := range []string{"dispatched", "accepted"} {
			if _, err := host.store.Journal(scope, state); err != nil {
				return nil, err
			}
		}
		emit := func(name string, value any) error {
			body, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return consume(extensionhost.Event{Name: name, Data: body})
		}
		if err := emit("accepted", sdk.AcceptedEvent{OperationID: scope.Operation}); err != nil {
			return nil, err
		}
		if err := emit("headers", sdk.HeadersEvent{Status: 200}); err != nil {
			return nil, err
		}
		terminal, tail := `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`, "\n"
		if protocol == "stream" {
			terminal = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
			tail = "data: [DONE]\n\n"
		}
		if mode == "parse-error" || mode == "parse-error-pending" {
			terminal = `{"choices":[INVALID]}`
			if protocol == "stream" {
				terminal = "data: {\"choices\":[INVALID]}\n\n"
			}
		}
		if mode == "nonterminal" || mode == "nonterminal-pending" {
			terminal = `{"choices":[{"message":{"content":"partial"}}]}`
			if protocol == "stream" {
				terminal = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
			}
		}
		if mode != "early-close" {
			if err := emit("data", []byte(terminal)); err != nil {
				return nil, err
			}
		}
		if mode == "excess-tail" || mode == "excess-tail-completed" {
			tail = strings.Repeat(" ", sdk.MaxChunkBytes)
		}
		for count := 0; !promptReceipt && count < 2; count++ {
			if mode == "excess-tail-completed" && count == 1 {
				tail = " "
			}
			if err := emit("data", []byte(tail)); err != nil {
				return nil, err
			}
			if mode != "excess-tail" && mode != "excess-tail-completed" {
				break
			}
		}
		close(tailWritten)
		if mode == "trailing-invalid-envelope" {
			if err := emit("unknown-envelope", "invalid"); err != nil {
				return nil, err
			}
			test.Error("invalid SDK envelope was accepted")
		}
		if mode == "missing-receipt" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if mode == "timeout-late-completed" {
			<-ctx.Done()
		} else if mode != "excess-tail-completed" && !promptReceipt {
			select {
			case <-releaseReceipt:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if mode == "late-error" {
			return nil, &extensionhost.OutcomeError{Outcome: "uncertain", Cause: errors.New("late worker error")}
		}
		if _, err := host.store.Journal(scope, "completed"); err != nil {
			return nil, err
		}
		completed = true
		return json.RawMessage(`{"status":200,"outcome":"completed","usage":{"inputTokens":13,"outputTokens":6,"cachedTokens":4}}`), nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, selected.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		test.Fatal(err)
	}
	response, err := selected.Do(http.DefaultClient, request)
	if err != nil {
		cancel()
		test.Fatal(err)
	}
	closeCount := 1
	if mode == "caller-canceled" || mode == "caller-abort-pending" {
		closeCount = 8
	}
	closed := make(chan error, closeCount)
	response.Body = struct{ io.ReadCloser }{response.Body}
	test.Cleanup(func() {
		cancel()
		release()
		response.Body.Close()
		select {
		case <-executed:
		case <-time.After(10 * time.Second):
			test.Error("terminal-close worker was not joined")
		}
	})
	if protocol == "whole" && !strings.HasPrefix(mode, "early-close") {
		var result map[string]any
		err := json.NewDecoder(response.Body).Decode(&result)
		if strings.HasPrefix(mode, "parse-error") && err == nil {
			test.Fatal("malformed response did not exercise a decoder error")
		}
		if !strings.HasPrefix(mode, "parse-error") && err != nil {
			test.Fatal(err)
		}
	} else if !strings.HasPrefix(mode, "early-close") {
		reader := bufio.NewReader(response.Body)
		for count := 0; count < 2; count++ {
			if _, err := reader.ReadString('\n'); err != nil {
				test.Fatal(err)
			}
		}
	}
	if !withoutTerminal {
		ExtensionResponseProtocolComplete(response)
	}
	started := time.Now()
	startClose := make(chan struct{})
	for count := 0; count < closeCount; count++ {
		go func() {
			<-startClose
			closed <- response.Body.Close()
		}()
	}
	close(startClose)
	if mode == "caller-canceled" || mode == "caller-abort-pending" {
		cancel()
	} else if mode == "completed" || mode == "late-error" {
		select {
		case <-tailWritten:
			select {
			case <-closed:
				test.Fatal("body close returned before its held ExecuteResult")
			default:
			}
		case <-executed:
			if mode == "completed" {
				test.Error("body close canceled the worker before its held ExecuteResult")
			}
		case <-time.After(10 * time.Second):
			test.Fatal("body close did not drain its bounded terminal tail")
		}
		release()
	} else if mode == "excess-tail" || mode == "early-close" || mode == "parse-error" || mode == "nonterminal" {
		release()
	}
	for count := 0; count < closeCount; count++ {
		select {
		case <-closed:
			select {
			case <-executed:
			default:
				test.Fatal("body close returned before joining the worker")
			}
		case <-time.After(10 * time.Second):
			test.Fatal("body close did not finish or honor its receipt bound")
		}
	}
	module, operation := attempt.Operation()
	journal, err := host.store.Operation(module, operation)
	if promptReceipt && (err != nil || journal.State != "completed") {
		test.Fatalf("negative did not race a promptly completed receipt: mode=%s state=%s error=%v", mode, journal.State, err)
	}
	if lateReceipt && (err != nil || journal.State != "completed") {
		test.Fatalf("late receipt fixture did not deliver its competing completed receipt: state=%s error=%v", journal.State, err)
	}
	if mode == "timeout-late-completed" && time.Since(started) < extensionReceiptGrace {
		test.Fatal("terminal close canceled the held receipt before its grace expired")
	}
	if strings.HasSuffix(mode, "-pending") {
		select {
		case <-tailWritten:
			test.Fatal("close without a decoded terminal drained the unfinished response")
		default:
		}
	}
	if mode == "completed" || promptReceipt {
		totals := attempt.UsageFor(selected)
		if attempt.Outcome(false) != nil || attempt.Outcome(true) == nil || err != nil || journal.State != "completed" ||
			totals == nil || totals.InputTokens != 13 || totals.OutputTokens != 6 || totals.CachedTokens != 4 {
			test.Fatalf("terminal close lost authoritative completion or usage: state=%s usage=%+v outcome=%v error=%v",
				journal.State, totals, attempt.Outcome(false), err)
		}
	} else if attempt.Outcome(false) == nil || err != nil ||
		!lateReceipt && journal.State == "completed" {
		test.Fatalf("terminal close fabricated completion: mode=%s state=%s outcome=%v error=%v", mode, journal.State, attempt.Outcome(false), err)
	}
	retry, err := http.NewRequestWithContext(ctx, http.MethodPost, selected.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := selected.Do(http.DefaultClient, retry); err == nil || calls.Load() != 1 {
		test.Fatalf("terminal close allowed replay: mode=%s calls=%d error=%v", mode, calls.Load(), err)
	}
}
