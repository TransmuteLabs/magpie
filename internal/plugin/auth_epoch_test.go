package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
)

func TestAPIKeyResignInCommitsAuthEpochInExistingSlot(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, err := filepath.Abs("testdata/renew/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	const account = "renewco"
	const other = "renewco#other"
	const key = "same-account-key"
	if err := os.WriteFile(AuthPath(), []byte(`{"renewco":{"type":"api","key":"same-account-key"},"renewco#other":{"type":"api","key":"other-account-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Providers(ctx); err != nil {
		t.Fatal(err)
	}
	previous := ""
	for _, selected := range []string{NewAccount, account} {
		saved, err := APIKey(ctx, account, 0, nil, key, selected)
		if err != nil || saved.Account != account {
			t.Fatalf("replace slot through APIKey (%q) = %+v, %v", selected, saved, err)
		}
		records := authFile(t)
		if len(records) != 2 || records[other]["key"] != "other-account-key" {
			t.Fatal("sign-in left a temporary slot or changed another account")
		}
		record := records[account]
		epoch, _ := record[authEpochField].(string)
		if record["key"] != key || epoch == "" || epoch == previous {
			t.Fatalf("same-slot API-key sign-in did not publish a new auth epoch (previous present: %t, current present: %t)", previous != "", epoch != "")
		}
		if epoch != saved.AuthEpoch {
			t.Fatal("the RPC did not acknowledge the committed epoch")
		}
		epochs, err := CurrentAuthEpochs(ctx)
		if err != nil || epochs[account] != epoch {
			t.Fatalf("could not read the current epoch: %v", err)
		}
		if otherEpoch, present := epochs[other]; !present || otherEpoch != "" {
			t.Fatal("another account received an epoch")
		}
		var cached Account
		for _, provider := range Cached() {
			for _, candidate := range provider.Accounts {
				if candidate.Key == account {
					cached = candidate
				}
			}
		}
		if cached.AuthEpoch != epoch {
			t.Fatal("provider discovery missed the saved epoch")
		}
		public, err := json.Marshal(cached)
		if err != nil || strings.Contains(string(public), epoch) {
			t.Fatal("provider account JSON exposed the local epoch")
		}
		previous = epoch
	}

	assertExport := func(credential json.RawMessage) {
		t.Helper()
		var record map[string]any
		if err := json.Unmarshal(credential, &record); err != nil || record["key"] != key || len(record) != 2 {
			t.Fatal("the export changed the credential object or included the local epoch")
		}
	}
	if err := WithSavedAuth(ctx, account, account, func(record extension.SavedSignInRecord) error {
		assertExport(record.Credentials)
		if !strings.Contains(string(record.Raw), previous) {
			t.Fatal("the source record lost its committed epoch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := AuthEntries()
	if err != nil {
		t.Fatal(err)
	}
	assertExport(entries[account])
	if err := EditAuth(func(records map[string]json.RawMessage) (bool, error) {
		assertExport(records[account])
		delete(records, other)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WithAuthEpoch(ctx, account, previous, func() error { return nil }); err != nil {
		t.Fatalf("a different account's carry edit replaced the epoch: %v", err)
	}
	if err := WithAuthEpoch(ctx, account, "", func() error {
		t.Error("a legacy selection published after reauth")
		return nil
	}); err == nil {
		t.Fatal("missing previous generation rejection")
	}
}

func TestAuthEpochUsesCurrentBytesAndIgnoresBackup(t *testing.T) {
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(AuthPath(), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(os.Getenv("HOME"), "config"))
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	const first = `{"fakeco":{"type":"api","key":"fixture-key","_magpieAuthEpoch":"first"}}`
	second := strings.Replace(first, "first", "later", 1)
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	setTime := func() {
		t.Helper()
		if err := os.Chtimes(AuthPath(), past, past); err != nil {
			t.Fatal(err)
		}
	}
	write(first)
	setTime()
	epochs, err := CurrentAuthEpochs(ctx)
	if err != nil || epochs["fakeco"] != "first" {
		t.Fatalf("initial epoch = %v, %v", epochs, err)
	}
	epochs["fakeco"] = "forged"
	if err := WithAuthEpoch(ctx, "fakeco", "first", func() error { return nil }); err != nil {
		t.Fatalf("a caller changed the retained projection: %v", err)
	}
	write(second)
	setTime()
	if err := WithAuthEpoch(ctx, "fakeco", "later", func() error { return nil }); err != nil {
		t.Fatalf("same size and mtime hid the new epoch: %v", err)
	}
	if err := os.WriteFile(AuthPath()+".bak", []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	write(`null`)
	if _, err := CurrentAuthEpochs(ctx); err == nil {
		t.Fatal("JSON null accepted as a current projection")
	}
	if err := WithAuthEpoch(ctx, "fakeco", "later", func() error {
		t.Error("the backup authorized a current publication")
		return nil
	}); err == nil {
		t.Fatal("missing rejection after corrupt current file")
	}
	write(second)
	if err := WithAuthEpoch(ctx, "fakeco", "later", func() error { return nil }); err != nil {
		t.Fatalf("repair did not restore the current view: %v", err)
	}
}
