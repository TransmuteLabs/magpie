package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestWindowAuthConfirmsVisibleReceiptWithoutReplaying(t *testing.T) {
	for _, selected := range []bool{false, true} {
		name, generation := "new_account", "1"
		wantGeneration := uint64(1)
		if selected {
			name, generation = "reauthorization", "2"
			wantGeneration = 2
		}
		t.Run(name, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "trace")
			manager, id, revision := authWindowFixture(t, false, authfixture.Config{Trace: trace})
			options := sdk.WindowAuthStartOptions{Method: "device"}
			if selected {
				account, err := manager.Store().SaveAccount(Account{
					Plugin: id, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"token":"original"}`),
				}, 0)
				if err != nil {
					t.Fatal(err)
				}
				options.Account = &sdk.AccountSelection{ID: account.ID, ExpectedGeneration: "1"}
			}

			var failSync, sawPublished atomic.Bool
			var registrations atomic.Int32
			syncDirectory := func(root *os.Root) error {
				if !failSync.Load() {
					return storeSync(root)
				}
				if !sawPublished.Load() {
					records, err := storeReadAccounts(root, id)
					if err == nil && len(records.AuthFlows) == 1 && records.AuthFlows[0].Phase == "complete" &&
						records.AuthFlows[0].Receipt != nil && records.AuthFlows[0].Receipt.Generation == generation {
						sawPublished.Store(true)
					}
				}
				return errors.New("private fixture: directory sync failed")
			}
			manager.Store().windowAuthSync = syncDirectory
			ctx := WithWindowAuthAccountPolicy(authWindowContext(t.Context(), "confirm-receipt"),
				func(context.Context, *Manager, sdk.Scope, sdk.AccountSummary) { registrations.Add(1) })
			waiting, err := manager.CoreAuthStart(ctx, id, revision, "window:page", "signin", options)
			if err != nil || waiting.State != "waiting" {
				t.Fatalf("start: %s %v", waiting.State, err)
			}
			authWindowReady(t, manager, id, waiting)
			manager.mu.Lock()
			owner := manager.windowAuthOwners[waiting.Flow]
			manager.mu.Unlock()
			if owner == nil {
				t.Fatal("waiting flow has no owner")
			}

			failSync.Store(true)
			view, err := manager.CoreAuthFinish(ctx, id, revision, "window:page", "signin", waiting.Flow)
			if err == nil || err.Error() == "private fixture: directory sync failed" || view != (sdk.WindowAuthFlowView{}) {
				t.Errorf("first finish must return only a public error: %+v %v", view, err)
			}
			if !sawPublished.Load() {
				t.Error("the failed sync did not observe the real account-file rename")
			}
			account, err := manager.Store().GetAccount(id, "vendor", "chosen")
			if err != nil || account.Generation != wantGeneration {
				t.Fatalf("the account did not advance on disk: generation %d, %v", account.Generation, err)
			}
			record, err := manager.Store().windowAuthRecord(id, waiting.Flow)
			if err != nil || record.Phase != "complete" || record.Receipt == nil ||
				record.Receipt.Generation != generation || record.Private != nil || len(record.Continuation) != 0 {
				t.Fatalf("visible account publication did not keep its complete receipt: %s %v", record.Phase, err)
			}
			select {
			case <-owner.done:
			case <-time.After(3 * time.Second):
				t.Fatal("owner did not retire after publication")
			}

			reopened := NewManager(manager.Namespace())
			reopened.Store().windowAuthSync = syncDirectory
			t.Cleanup(func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := reopened.Close(closeCtx); err != nil {
					t.Error(err)
				}
			})
			view, err = reopened.CoreAuthFinish(ctx, id, revision, "window:page", "signin", waiting.Flow)
			if err == nil || view != (sdk.WindowAuthFlowView{}) {
				t.Errorf("reopened finish exposed an unconfirmed receipt: %+v %v", view, err)
			}
			view, err = reopened.CoreAuthCancel(ctx, id, revision, "window:page", "signin", waiting.Flow)
			if err == nil || view != (sdk.WindowAuthFlowView{}) {
				t.Errorf("cancel exposed an unconfirmed receipt: %+v %v", view, err)
			}
			if !selected {
				view, err = reopened.CoreAuthStart(ctx, id, revision, "window:page", "signin", options)
				if err == nil || view != (sdk.WindowAuthFlowView{}) {
					t.Errorf("same intent exposed an unconfirmed receipt: %+v %v", view, err)
				}
			}
			if registrations.Load() != 0 {
				t.Error("local account policy registered an unconfirmed account")
			}
			if calls := authWindowTrace(t, trace, "finish"); calls != 1 {
				t.Fatalf("finish dispatched %d times before confirmation", calls)
			}

			failSync.Store(false)
			view, err = reopened.CoreAuthFinish(ctx, id, revision, "window:page", "signin", waiting.Flow)
			if err != nil || view.State != "complete" || view.Account == nil || *view.Account != *record.Receipt {
				t.Fatalf("recovered finish: %+v %v", view, err)
			}
			if registrations.Load() != 1 {
				t.Fatal("recovered result did not register the confirmed account")
			}
			if !selected {
				archiveCtx := authWindowContext(t.Context(), "archive-receipt")
				if next, err := reopened.CoreAuthStart(archiveCtx, id, revision, "window:page", "signin", options); err != nil || next.State != "waiting" {
					t.Fatalf("reserve a new flow to archive the receipt: %s %v", next.State, err)
				}
				if _, err := os.Stat(filepath.Join(manager.Namespace(), "private", id, "auth-receipts", "flow-"+waiting.Flow+".json")); err != nil {
					t.Fatal("the flow receipt was not archived:", err)
				}
				failSync.Store(true)
				replay, err := reopened.CoreAuthFinish(ctx, id, revision, "window:page", "signin", waiting.Flow)
				if err == nil || replay != (sdk.WindowAuthFlowView{}) {
					t.Errorf("archived finish exposed an unconfirmed receipt: %+v %v", replay, err)
				}
				failSync.Store(false)
				replay, err = reopened.CoreAuthStart(ctx, id, revision, "window:page", "signin", options)
				if err != nil || replay.Flow != waiting.Flow || replay.State != "complete" || replay.Account == nil || *replay.Account != *record.Receipt {
					t.Fatalf("archived intent replay: %+v %v", replay, err)
				}
			}
			if calls := authWindowTrace(t, trace, "finish"); calls != 1 {
				t.Fatalf("finish dispatched %d times after confirmation", calls)
			}
		})
	}
}
