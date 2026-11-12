package plugin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

// zeroedAuth is plugin-auth.json as #1505's Windows crash left it: its full
// length, 29,926 bytes, every one 0x00.
var zeroedAuth = make([]byte, 29926)

func addRenew(t *testing.T, ctx context.Context) {
	t.Helper()
	t.Setenv("FAKE_BASE", "http://127.0.0.1:1/v1")
	t.Setenv("RENEW_LOG", filepath.Join(t.TempDir(), "renewals"))
	abs, _ := filepath.Abs("testdata/renew/index.js")
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
}

func importRenew(t *testing.T, ctx context.Context, who string) string {
	t.Helper()
	k, err := Import(ctx, "renewco", map[string]any{"type": "oauth", "refresh": "r-ok-" + who, "access": "a", "expires": time.Now().Add(time.Hour).UnixMilli(), "accountId": who})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The host reads a zeroed plugin-auth.json from its last good generation,
// tells magpie, which tells the user, and a sign-in added keeps the
// accounts and the zeroed file aside.
func TestZeroedPluginAuthReadFromBackup(t *testing.T) {
	sandbox(t)
	lastgood.Reset()
	t.Cleanup(lastgood.Reset)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	addRenew(t, ctx)
	a := importRenew(t, ctx, "a@renew")
	b := importRenew(t, ctx, "b@renew")
	Restart()
	if _, err := os.Stat(lastgood.Bak(AuthPath())); err != nil {
		t.Fatalf("no last good generation kept: %v", err)
	}
	if err := os.WriteFile(AuthPath(), zeroedAuth, 0o600); err != nil {
		t.Fatal(err)
	}

	// magpie's own reads: the .bak lags one write, so it holds a
	if got := Auths("renewco"); len(got) != 1 || got[a] == nil {
		t.Fatalf("a zeroed plugin-auth.json read as %v", got)
	}
	if len(lastgood.Recovered()) != 1 {
		t.Fatalf("magpie's read not noted: %+v", lastgood.Recovered())
	}
	lastgood.Reset() // the host's own read is told magpie too

	// the host's
	c := importRenew(t, ctx, "c@renew")
	all := authFile(t)
	if all[a] == nil || all[c] == nil {
		t.Fatalf("after a sign-in added over a zeroed file: %v", all)
	}
	_ = b // the write the crash took
	kept, _ := filepath.Glob(AuthPath() + ".bad-*")
	if len(kept) != 1 {
		t.Fatalf("the zeroed file not kept aside: %v", kept)
	}
	if got, _ := os.ReadFile(kept[0]); !bytes.Equal(got, zeroedAuth) {
		t.Fatalf("kept %d bytes", len(got))
	}
	for wait := time.Now().Add(5 * time.Second); len(lastgood.Recovered()) == 0 && time.Now().Before(wait); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := lastgood.Recovered(); len(n) != 1 || n[0].File != "plugin-auth.json" || n[0].Why != "29926 bytes, every one zero" {
		t.Fatalf("the host's recovery isn't told: %+v", n)
	}
}

// With no backup, a zeroed plugin-auth.json is unknown, never none.
// Background auth.set and sign-out fail without touching it; an explicit
// import can seed one sign-in after preserving the damaged bytes.
func TestZeroedPluginAuthWithoutBackupNotWrittenOver(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	addRenew(t, ctx)
	status := filepath.Join(t.TempDir(), "background-auth")
	t.Setenv("AUTH_UNKNOWN_STATUS", status)
	fixture := filepath.Join(t.TempDir(), "index.js")
	source := `import fs from "node:fs"
export const BackgroundAuth = async ({ client }) => ({
  config: async () => {
    try {
      await client.auth.set({ path: { id: "background" }, body: { type: "api", key: "must-not-save" } })
      fs.writeFileSync(process.env.AUTH_UNKNOWN_STATUS, "saved")
    } catch {
      fs.writeFileSync(process.env.AUTH_UNKNOWN_STATUS, "refused")
    }
  },
})
`
	if err := os.WriteFile(fixture, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), zeroedAuth, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := get(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(status); err != nil || string(got) != "refused" {
		t.Fatalf("background auth.set on unknown store = %q, %v", got, err)
	}
	if got, _ := os.ReadFile(AuthPath()); !bytes.Equal(got, zeroedAuth) {
		t.Fatal("background auth.set overwrote the unreadable store")
	}
	// one account named, and every one of the provider's
	for _, account := range []string{"renewco#a@renew", ""} {
		if err := SignOut(ctx, "renewco", account); err == nil {
			t.Fatalf("the host's sign-out of %q reported success on an unreadable store", account)
		}
		if got, _ := os.ReadFile(AuthPath()); !bytes.Equal(got, zeroedAuth) {
			t.Fatalf("the host's sign-out of %q wrote %q over sign-ins that don't read", account, got)
		}
	}
	Restart()
	if err := SignOut(ctx, "renewco", ""); err == nil {
		t.Fatal("magpie's sign-out of sign-ins that don't read said done")
	}
	if got, _ := os.ReadFile(AuthPath()); !bytes.Equal(got, zeroedAuth) {
		t.Fatalf("magpie's sign-out wrote %q over sign-ins that don't read", got)
	}
	if kept, _ := filepath.Glob(AuthPath() + ".bad-*"); len(kept) != 0 {
		t.Fatalf("a refused background change unexpectedly kept bad copies: %v", kept)
	}
	// a sign-in added writes, the zeroed file kept aside first
	k := importRenew(t, ctx, "new@renew")
	all := authFile(t)
	if all[k] == nil || all["background"] != nil || len(all) != 1 {
		t.Fatalf("explicit import did not seed only its own sign-in: %v", all)
	}
	kept, _ := filepath.Glob(AuthPath() + ".bad-*")
	if len(kept) != 1 {
		t.Fatalf("the zeroed file not kept aside: %v", kept)
	}
	if got, _ := os.ReadFile(kept[0]); !bytes.Equal(got, zeroedAuth) {
		t.Fatalf("the preserved bad generation has %d bytes", len(got))
	}
}

func TestZeroedPluginAuthExplicitAPIKeySeeds(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("testdata/fake/index.js")
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(AuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), zeroedAuth, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := APIKey(ctx, "fakeco", 0, nil, "fresh-key", NewAccount)
	if err != nil {
		t.Fatal(err)
	}
	if a := authFile(t)[saved.Account]; a["key"] != "fresh-key" {
		t.Fatalf("explicit sign-in was not saved: %v", a)
	}
	kept, _ := filepath.Glob(AuthPath() + ".bad-*")
	if len(kept) != 1 {
		t.Fatalf("unreadable store not preserved: %v", kept)
	}
	if got, _ := os.ReadFile(kept[0]); !bytes.Equal(got, zeroedAuth) {
		t.Fatalf("the preserved bad generation has %d bytes", len(got))
	}
}

// Core flushes a host's plugin-auth.json proposal before renaming it over
// the old file: a flush that fails leaves the sign-ins there as they were.
func TestPluginAuthSyncedBeforeRename(t *testing.T) {
	sandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("testdata/fake/index.js")
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, "fakeco", map[string]any{"type": "api", "key": "k-old"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(AuthPath())
	if err != nil {
		t.Fatal(err)
	}
	syncFile := steady.Sync
	steady.Sync = func(f *os.File) error {
		name := filepath.Base(f.Name())
		if strings.HasPrefix(name, ".plugin-auth.json.") && !strings.HasPrefix(name, ".plugin-auth.json.bak.") {
			return errors.New("injected auth flush failure")
		}
		return syncFile(f)
	}
	t.Cleanup(func() { steady.Sync = syncFile })
	if _, err := Import(ctx, "fakeco", map[string]any{"type": "api", "key": "k-new"}); err == nil {
		t.Fatal("a sign-in saved without its flush said done")
	}
	if after, _ := os.ReadFile(AuthPath()); !bytes.Equal(after, before) {
		t.Fatalf("a failed flush changed plugin-auth.json:\n%s\nwas\n%s", after, before)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(AuthPath()), ".plugin-auth.json.*")); len(left) != 0 {
		t.Fatalf("a failed write left %v", left)
	}
}
