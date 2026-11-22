package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func testSavedSignIn(ticket string, deadline, expires time.Time) savedSignIn {
	return savedSignIn{
		Ticket: ticket, Module: "removed-module", Provider: "vendor", Key: "vendor",
		Digest: strings.Repeat("a", 64), Revision: strings.Repeat("b", 64),
		Deadline: deadline.UnixMilli(),
		Result:   sdk.AuthResult{ID: ticket, Credentials: json.RawMessage(`{"token":"fixture"}`), ExpiresAt: expires.UnixMilli()},
	}
}

func withTestSavedSignIns(store *Store, ctx context.Context, action func(*os.Root) error) error {
	return store.withPrivateStore(ctx, func(private *os.Root) error {
		root, err := storeDirectory(private, savedSignInsDir)
		if err != nil {
			return err
		}
		defer root.Close()
		return action(root)
	})
}

func TestSavedSignInDigestIgnoresSourceReformatting(t *testing.T) {
	original := json.RawMessage(`{"key":"a<b","_magpieAuthEpoch":"first","extra":{"active":true}}`)
	rewritten, err := json.MarshalIndent(map[string]json.RawMessage{"vendor": original}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &records); err != nil {
		t.Fatal(err)
	}
	digest := savedSignInDigest(original)
	if digest == "" || savedSignInDigest(records["vendor"]) != digest {
		t.Fatal("the unchanged sign-in no longer matches after a source-file rewrite")
	}
	for _, replacement := range []json.RawMessage{
		json.RawMessage(`{"key":"a<b","_magpieAuthEpoch":"second","extra":{"active":true}}`),
		json.RawMessage(`{"key":"a<b","_magpieAuthEpoch":"first","extra":{"active":false}}`),
	} {
		if savedSignInDigest(replacement) == digest {
			t.Fatal("a changed sign-in still matches")
		}
	}
}

func TestPreparedSignInLeavesAccountStampUnchanged(t *testing.T) {
	path := t.TempDir()
	store := NewStore(path)
	account := Account{Plugin: "fixture", Provider: "vendor", ID: "alice", Auth: json.RawMessage(`{"token":"first"}`)}
	if _, err := store.SaveAccount(account, 0); err != nil {
		t.Fatal(err)
	}
	before, err := accountStampAt(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := withTestSavedSignIns(store, t.Context(), func(root *os.Root) error {
		return writeSavedSignIns(root, []savedSignIn{testSavedSignIn("current", now.Add(time.Minute), now.Add(time.Hour))})
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		after, err := accountStampAt(path)
		if err != nil || !before.Equal(after) {
			t.Fatalf("a prepared sign-in invalidated account discovery: %v", err)
		}
	}
	account.Auth = json.RawMessage(`{"token":"other"}`)
	if _, err := NewStore(path).SaveAccount(account, 1); err != nil {
		t.Fatal(err)
	}
	updated, err := accountStampAt(path)
	if err != nil || before.Equal(updated) {
		t.Fatalf("the account update was not observed while a sign-in was prepared: %v", err)
	}
}

func TestGatewayStartupCleansExpiredSavedSignInsForRemovedModules(t *testing.T) {
	path := t.TempDir()
	original := NewStore(path)
	now := time.Now()
	entries := []savedSignIn{
		testSavedSignIn("ticket-expired", now.Add(-time.Second), now.Add(time.Hour)),
		testSavedSignIn("credential-expired", now.Add(time.Hour), now.Add(-time.Second)),
		testSavedSignIn("current", now.Add(time.Hour), now.Add(time.Hour)),
	}
	if err := withTestSavedSignIns(original, t.Context(), func(root *os.Root) error {
		return writeSavedSignIns(root, entries)
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(path)
	owner, err := manager.BeginGatewayActivation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := owner.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var saved []savedSignIn
		err := withTestSavedSignIns(original, ctx, func(root *os.Root) error {
			var err error
			saved, err = readSavedSignIns(root)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(saved) == 1 && saved[0].Ticket == "current" {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("gateway startup retained expired prepared credentials")
		}
	}
	if err := withTestSavedSignIns(original, ctx, func(root *os.Root) error {
		remaining, err := activeSavedSignIns(root, now.Add(2*time.Hour).UnixMilli())
		if len(remaining) != 0 {
			t.Fatal("cleanup kept the last expired ticket")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "private", savedSignInsDir, savedSignInsFile)); !os.IsNotExist(err) {
		t.Fatal("cleanup did not remove the empty ticket store")
	}
}
