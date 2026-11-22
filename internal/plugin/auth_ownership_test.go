package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/testenv"
)

const transferProvider = "transferco"

func writeCommunityOwnership(t *testing.T, active, pending bool) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(AuthPath()), "extensions", "private", ".saved-signins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "subscription-owners.json")
	digest := strings.Repeat("a", 64)
	record := fmt.Sprintf(`[{
		"module":"transfer-module","provider":"%s","publicId":"transferred",
		"revision":"sub-generation","active":%t,"pending":%t,
		"accounts":[{"key":"%s","id":"test-account","user":"test-user","loginId":"test-login"}],
		"grantRevision":"%s","tickets":["ticket"],"digests":{"%s":"%s"}
	}]`, transferProvider, active, pending, transferProvider, digest, transferProvider, digest)
	if err := writeWhole(path, []byte(record)); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTransferAuth(t *testing.T, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWhole(AuthPath(), []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

func addTransferFixture(t *testing.T, ctx context.Context) {
	t.Helper()
	target, err := filepath.Abs("testdata/transfer-auth/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, target); err != nil {
		t.Fatal(err)
	}
}

func TestSavedSubscriptionHoldsInstalledSourceThroughPublication(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	addTransferFixture(t, ctx)
	spec := Load().Plugins[0].Spec
	writeTransferAuth(t, `{"transferco":{"type":"api","key":"fixture-key"}}`)
	called := false
	if err := WithInstalledSavedAuths(ctx, transferProvider, spec, func(records map[string]json.RawMessage) error {
		called = true
		if len(records) != 1 || len(records[transferProvider]) == 0 {
			t.Fatal("the current saved account is missing")
		}
		file, err := os.OpenFile(listPath()+".lock", os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		held, err := filelock.TryLock(file)
		if held {
			filelock.Unlock(file)
		}
		if held || err != nil {
			t.Fatalf("the installed plugin could change during publication: held=%v, err=%v", held, err)
		}
		return nil
	}); err != nil || !called {
		t.Fatalf("current source did not reach publication: %v", err)
	}
	if err := SetOff(spec, true); err != nil {
		t.Fatal(err)
	}
	if err := WithInstalledSavedAuths(ctx, transferProvider, spec, func(map[string]json.RawMessage) error {
		t.Fatal("a disabled source reached publication")
		return nil
	}); err == nil {
		t.Fatal("a disabled source was admitted")
	}
}

func TestTransferredCommunityAuthCannotChange(t *testing.T) {
	for _, state := range []string{"active", "pending"} {
		t.Run(state, func(t *testing.T) {
			sandbox(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			const retained = `{"type": "api", "key": "sample\u003ckey", "expires": 9007199254740992}`
			writeTransferAuth(t, `{"transferco":`+retained+`,"neighbor":{"type":"api","key":"sample-neighbor"}}`)
			ownership := writeCommunityOwnership(t, state == "active", state == "pending")
			expectDenied := func(err error) {
				t.Helper()
				if err == nil || err.Error() != errCommunitySubscriptionOwned.Error() {
					t.Errorf("legacy auth mutation after %s ownership: %v", state, err)
				}
			}
			original, err := os.ReadFile(AuthPath())
			if err != nil {
				t.Fatal(err)
			}
			expectDenied(SignOut(ctx, transferProvider, transferProvider))
			expectDenied(EditAuth(func(records map[string]json.RawMessage) (bool, error) {
				records[transferProvider] = json.RawMessage(strings.ReplaceAll(retained, "9007199254740992", "9007199254740993"))
				return true, nil
			}))
			addTransferFixture(t, ctx)
			expectDenied(Restore(ctx, transferProvider, transferProvider+"#extra", map[string]any{"type": "api", "key": "sample-added"}))
			expectDenied(Call(ctx, "signOut", map[string]string{"provider": transferProvider}, nil))
			if after, err := os.ReadFile(AuthPath()); err != nil || !bytes.Equal(after, original) {
				t.Fatal("legacy writer changed the saved sign-in file despite the SDK reservation")
			}
			if err := Restore(ctx, "neighbor", "neighbor", map[string]any{"type": "api", "key": "sample-updated"}); err != nil {
				t.Fatalf("another provider's Bun write was refused: %v", err)
			}
			if err := EditAuth(func(records map[string]json.RawMessage) (bool, error) {
				records["neighbor#second"] = json.RawMessage(`{"type":"api","key":"sample-second"}`)
				return true, nil
			}); err != nil {
				t.Fatalf("another provider's Go write was refused: %v", err)
			}
			if err := WithSavedAuth(ctx, transferProvider, transferProvider, func(record json.RawMessage) error {
				if !bytes.Equal(record, []byte(retained)) {
					return errors.New("neighboring writes changed the rollback digest")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			beforeDamage, err := os.ReadFile(AuthPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ownership, []byte(`invalid`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Restore(ctx, "neighbor", "neighbor", map[string]any{"type": "api", "key": "sample-bad"}); err == nil {
				t.Error("Bun writer accepted unreadable ownership")
			}
			if after, err := os.ReadFile(AuthPath()); err != nil || !bytes.Equal(after, beforeDamage) {
				t.Fatal("unreadable ownership allowed a legacy writer to change sign-ins")
			}
			writeCommunityOwnership(t, false, false)
			if err := SignOut(ctx, transferProvider, transferProvider); err != nil || SignedIn(transferProvider) {
				t.Fatalf("legacy sign-out after rollback was refused: %v", err)
			}
			if !SignedIn("neighbor") {
				t.Fatal("rollback sign-out lost the neighboring account")
			}
		})
	}
}

func TestCommunityUseBlocksTransferThroughVendorWrite(t *testing.T) {
	for _, method := range []string{"refresh", "loader", "fetch", "usage", "usage canceled"} {
		t.Run(method, func(t *testing.T) {
			sandbox(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			arrived, finish := make(chan struct{}), make(chan struct{})
			endVendor := sync.OnceFunc(func() { close(finish) })
			var calls atomic.Int32
			srv := testenv.FakeVendor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(arrived)
				}
				select {
				case <-finish:
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"refresh":"after-renewal","access":"next","expires":9000000000000000}`)
				case <-ctx.Done():
				}
			}))
			defer endVendor()
			t.Setenv("TRANSFER_METHOD", strings.TrimSuffix(method, " canceled"))
			addTransferFixture(t, ctx)
			expires := "9000000000000000"
			if method == "refresh" {
				expires = "1"
			}
			writeTransferAuth(t, `{
				"transferco":{"type":"oauth","refresh":"before-renewal","access":"initial","expires":`+expires+`},
				"neighbor":{"type":"api","key":"sample-neighbor"}
			}`)
			run := func(ctx context.Context) error {
				switch method {
				case "refresh":
					_, err := KeepSignedIn(ctx, transferProvider, transferProvider)
					return err
				case "loader":
					return Call(ctx, "load", map[string]string{"provider": transferProvider}, nil)
				case "fetch":
					res, err := Fetch(ctx, FetchRequest{Provider: transferProvider, Model: "one", URL: srv.URL + "/v1/chat", Method: "POST"})
					if err != nil {
						return err
					}
					defer res.Body.Close()
					_, err = io.Copy(io.Discard, res.Body)
					return err
				default:
					_, err := AccountUsage(ctx, transferProvider, transferProvider)
					return err
				}
			}
			request, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			finished := make(chan error, 1)
			go func() { finished <- run(request) }()
			t.Cleanup(func() {
				cancelRequest()
				endVendor()
				if finished != nil {
					<-finished
				}
			})
			select {
			case <-arrived:
			case err := <-finished:
				finished = nil
				t.Fatalf("legacy use returned before reaching its vendor: %v", err)
			case <-ctx.Done():
				t.Fatal("legacy use never reached the vendor")
			}
			if method == "usage canceled" {
				cancelRequest()
				if err := <-finished; !errors.Is(err, context.Canceled) {
					t.Errorf("canceled reader returned %v", err)
				}
				finished = nil
			}
			if err := WithSavedAuths(ctx, transferProvider, func(map[string]json.RawMessage) error {
				return errors.New("transfer entered publication while a legacy vendor operation was in flight")
			}); !errors.Is(err, errCommunitySubscriptionBusy) {
				t.Errorf("cutover did not refuse the in-flight operation: %v", err)
			}
			releaseSecondCall, err := GuardCommunitySubscription(ctx, transferProvider)
			if err != nil {
				t.Errorf("shared admission serializes operations of the same provider: %v", err)
			} else {
				releaseSecondCall()
			}
			if err := WithSavedAuths(ctx, "neighbor", func(records map[string]json.RawMessage) error {
				if len(records) != 1 {
					return errors.New("neighbor sign-ins were not available")
				}
				return nil
			}); err != nil {
				t.Errorf("in-flight call blocked a different provider: %v", err)
			}
			endVendor()
			if finished != nil {
				if err := <-finished; err != nil {
					t.Errorf("legacy operation could not finish saving the replacement: %v", err)
				}
				finished = nil
			}
			for {
				err := WithSavedAuths(ctx, transferProvider, func(records map[string]json.RawMessage) error {
					if !bytes.Contains(records[transferProvider], []byte(`"after-renewal"`)) {
						return errors.New("transfer observed a spent refresh token after the operation finished")
					}
					writeCommunityOwnership(t, true, false)
					return nil
				})
				if !errors.Is(err, errCommunitySubscriptionBusy) {
					if err != nil {
						t.Fatalf("transfer after the legacy operation: %v", err)
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("the legacy operation never released its provider admission")
				case <-time.After(time.Millisecond):
				}
			}
			if err := run(ctx); err == nil || err.Error() != errCommunitySubscriptionOwned.Error() {
				t.Errorf("stale legacy call after cutover: %v", err)
			}
			if calls.Load() != 1 {
				t.Errorf("vendor received %d calls across transfer, want one", calls.Load())
			}
		})
	}
}
