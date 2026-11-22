package plugin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

func TestFetchAdmissionStaysBoundToItsCall(t *testing.T) {
	for _, change := range []string{"cancel", "replace sign-in"} {
		t.Run(change, func(t *testing.T) {
			sandbox(t)
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			const account = "fetch-destination"
			var calls atomic.Int32
			vendor := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Setenv("DESTINATION_FALLBACK", vendor.URL)
			entry, err := filepath.Abs("testdata/fetch-destination/index.js")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Add(ctx, entry); err != nil {
				t.Fatal(err)
			}
			signIn := map[string]any{"type": "api", "key": "fixture-key"}
			if err := Restore(ctx, account, account, signIn); err != nil {
				t.Fatal(err)
			}
			epoch := savedEpoch(t, ctx, account)
			arrived, permit := make(chan FetchAuth, 1), make(chan struct{})
			request := FetchRequest{Provider: account, Account: account, NPM: "@ai-sdk/openai-compatible", Model: "chat", URL: vendor.URL}
			held := request
			held.CheckAdmission = func(check context.Context, auth FetchAuth, _ bool) error {
				select {
				case arrived <- auth:
				case <-check.Done():
					return check.Err()
				}
				select {
				case <-permit:
				case <-check.Done():
				}
				return nil
			}
			responses := make(chan error, 1)
			fetchCtx, cancelFetch := context.WithCancel(ctx)
			joined := make(chan struct{})
			defer func() {
				cancelFetch()
				<-joined
			}()
			go func() {
				defer close(joined)
				resp, _, err := FetchWithAuth(fetchCtx, held)
				if resp != nil {
					resp.Body.Close()
				}
				responses <- err
			}()
			select {
			case auth := <-arrived:
				assertFetchEpoch(t, auth, account, epoch)
			case err := <-responses:
				t.Fatalf("fetch ended before admission: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if change == "cancel" {
				cancelFetch()
			} else {
				if err := Restore(ctx, account, account, signIn); err != nil {
					t.Fatal(err)
				}
				close(permit)
			}
			select {
			case err := <-responses:
				if change == "cancel" && !errors.Is(err, context.Canceled) ||
					change != "cancel" && (err == nil || !strings.Contains(err.Error(), "sign-in changed")) {
					t.Errorf("fetch after %s: %v", change, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if calls.Load() != 0 {
				t.Fatalf("the stale request reached the vendor %d times", calls.Load())
			}
			request.CheckAdmission = func(context.Context, FetchAuth, bool) error { return nil }
			resp, auth, err := FetchWithAuth(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, err = io.Copy(io.Discard, resp.Body)
			if err != nil || resp.StatusCode != http.StatusNoContent || calls.Load() != 1 {
				t.Errorf("fresh fetch gave %d, %v, calls %d", resp.StatusCode, err, calls.Load())
			}
			assertFetchEpoch(t, auth, account, savedEpoch(t, ctx, account))
		})
	}
}
