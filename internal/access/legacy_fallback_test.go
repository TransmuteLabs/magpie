package access

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

func TestLegacyLANFallbackBeforeMigration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := settings.Settings{LAN: true, LANKey: "sk-magpie-old-client"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	before, ok := Authenticate(s.LANKey)
	if !ok || before.KeyID == "" || before.KeyName != "Magpie" {
		t.Fatal("unmigrated legacy key lost access", before, ok)
	}
	if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("authentication must not create a key store", err)
	}
	for _, secret := range []string{"", "wrong", "magpie", s.LANKey + "x"} {
		if _, ok := Authenticate(secret); ok {
			t.Fatal("legacy fallback accepted another token")
		}
	}
	if err := MigrateLegacyLANKey(); err != nil {
		t.Fatal(err)
	}
	if after, ok := Authenticate(s.LANKey); !ok || !reflect.DeepEqual(after, before) {
		t.Fatal("migration split the legacy caller's usage identity", before, after, ok)
	}
}

func TestLegacyLANFallbackDoesNotBypassRevocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  settings.Settings
		keys []Key
	}{
		{"not-shared", settings.Settings{LANKey: "legacy"}, nil},
		{"completed", settings.Settings{LAN: true, LANKey: "legacy", LANKeyID: "removed"}, nil},
		{"revoked-mirror", settings.Settings{LAN: true, LANKey: revokedLANPrefix + "old"}, nil},
		{"disabled", settings.Settings{LAN: true, LANKey: "legacy"}, []Key{{ID: "default", LAN: true, Secret: "legacy", Off: true}}},
		{"rotated", settings.Settings{LAN: true, LANKey: "legacy"}, []Key{{ID: "default", LAN: true, Secret: "new"}}},
		{"disabled-matching-key", settings.Settings{LAN: true, LANKey: "legacy"}, []Key{{ID: "other", Secret: "legacy", Off: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if err := settings.Save(tc.cfg); err != nil {
				t.Fatal(err)
			}
			if tc.keys != nil {
				if err := Restore(tc.keys); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := Authenticate(tc.cfg.LANKey); ok {
				t.Fatal("fallback accepted a revoked or inactive legacy credential")
			}
		})
	}
}

func TestLegacyLANFallbackWithReadOnlyDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := settings.Settings{LAN: true, LANKey: "sk-magpie-readonly"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settings.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(settings.Dir(), 0o700) })
	if err := MigrateLegacyLANKey(); err == nil {
		t.Skip("configuration directory remains writable")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	for range 3 {
		if who, ok := Authenticate(s.LANKey); !ok || who.KeyID == "" || who.KeyName != "Magpie" {
			t.Fatal("failed key-store creation broke legacy access", who, ok)
		}
	}
}

func TestLegacyLANFallbackRefusesCorruptStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := settings.Settings{LAN: true, LANKey: "legacy"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Authenticate(s.LANKey); ok {
		t.Fatal("an unreadable revocation state must not enable legacy fallback")
	}
}

func TestLegacyLANUsesOnlyCurrentSettings(t *testing.T) {
	const legacy = "sk-magpie-fixture-legacy"
	const named = "named-fixture-secret"
	for _, tc := range []struct {
		name      string
		primary   string
		recovered bool
		missing   bool
		invalid   bool
	}{
		{name: "truncated", primary: `{"lan":true,"lanKey":"sk-magpie-fixture-legacy"`, recovered: true, invalid: true},
		{name: "empty", recovered: true, invalid: true},
		{name: "null", primary: `null`, invalid: true},
		{name: "missing", missing: true},
		{name: "defaults", primary: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if err := settings.Save(settings.Settings{LAN: true, LANKey: legacy}); err != nil {
				t.Fatal(err)
			}
			previous, err := Check(legacy)
			if err != nil || previous.KeyID == "" || !On(previous.KeyID) || LANSecret() != legacy {
				t.Fatalf("a current legacy key did not work: %v", err)
			}
			digest := sha256.Sum256([]byte(legacy))
			proofID := hex.EncodeToString(digest[:])
			if proved, ok := ProveSecret(proofID); !ok || proved != legacy {
				t.Fatal("remote proof rejected a current legacy key")
			}
			if err := settings.Save(settings.Settings{LANKeyID: previous.KeyID}); err != nil {
				t.Fatal(err)
			}
			if err := Restore([]Key{{ID: "named", Name: "Travel", Secret: named}}); err != nil {
				t.Fatal(err)
			}
			originalKeys, err := os.ReadFile(Path())
			if err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				err = os.Remove(settings.Path())
			} else {
				err = os.WriteFile(settings.Path(), []byte(tc.primary), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.recovered {
				if shown := settings.Load(); !shown.LAN || shown.LANKey != legacy {
					t.Fatal("the ordinary settings reader lost its backup")
				}
			}
			if _, err := Check(legacy); !errors.Is(err, errUnknown) {
				t.Errorf("legacy key from unconfirmed settings authenticated: %v", err)
			}
			if On(previous.KeyID) {
				t.Error("legacy key from unconfirmed settings remained on")
			}
			if LANSecret() != "" {
				t.Error("agent configuration received an unconfirmed legacy key")
			}
			if _, ok := ProveSecret(proofID); ok {
				t.Error("remote proof used an unconfirmed legacy key")
			}
			if who, err := Check(named); err != nil || who.KeyID != "named" || who.Default {
				t.Errorf("independently stored named key lost its access: %v", err)
			}
			if err := MigrateLegacyLANKey(); (err != nil) != tc.invalid {
				t.Errorf("legacy migration validation: %v, want error=%v", err, tc.invalid)
			}
			if tc.invalid {
				if err := ConfigureLAN(true, false); err == nil {
					t.Error("started sharing with unreadable current settings")
				}
			}
			if saved, err := os.ReadFile(Path()); err != nil || !bytes.Equal(saved, originalKeys) {
				t.Errorf("legacy migration changed the key store: %v", err)
			}
			if saved, err := os.ReadFile(settings.Path()); tc.missing {
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("migration recreated missing settings: %v", err)
				}
			} else if err != nil || string(saved) != tc.primary {
				t.Errorf("legacy migration rewrote current settings: %v", err)
			}
		})
	}
}
