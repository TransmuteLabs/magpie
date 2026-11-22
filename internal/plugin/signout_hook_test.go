package plugin

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An account signed out while one of its hooks runs stays signed out: the
// hook's auth.set (a models list spending a rotating refresh token) lands
// after the sign-out and must not sign it in again (#5126: a failed move's
// account came back in the plugin this way). One not signed out keeps the
// token the hook saved.
func TestSignOutOutlastsRunningHook(t *testing.T) {
	sandbox(t)
	hold := filepath.Join(t.TempDir(), "hold")
	t.Setenv("FAKE_MODELS_HOLD", hold)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("testdata/fake/index.js")
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Settle)
	signIn := func() string {
		t.Helper()
		k, err := Import(ctx, "fakeco", map[string]any{"type": "oauth", "refresh": "rot-x", "access": "a", "expires": 9e15, "accountId": "x@fake"})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	// the hook comes, and waits
	arrived := func() {
		t.Helper()
		for {
			if _, err := os.Stat(hold + ".in"); err == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("the models hook never came")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	checked := func(key string) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := Check(ctx, "fakeco", key)
			done <- err
		}()
		return done
	}

	// signed out while the hook holds the token it is about to save
	key := signIn()
	done := checked(key)
	arrived()
	if err := SignOut(ctx, "fakeco", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	if a := Auths("fakeco"); len(a) != 0 {
		t.Fatalf("the hook signed the account in again: %v", a)
	}

	// not signed out: the token it spent is replaced by one it saved
	key = signIn()
	if err := <-checked(key); err != nil {
		t.Fatal(err)
	}
	// (a listing the sign-in set off may spend it again too)
	if a := Auths("fakeco"); !strings.HasPrefix(fmt.Sprint(a[key]["refresh"]), "rot-x+") {
		t.Fatalf("the hook's renewed token wasn't kept: %v", a)
	}
}

func TestScopedAuthWritesKeepNewSignIns(t *testing.T) {
	for _, mode := range []string{"rotation", "sign-in again", "metadata update", "stale remove", "other account", "two writes after renewal"} {
		t.Run(mode, func(t *testing.T) {
			sandbox(t)
			hold := filepath.Join(t.TempDir(), "auth")
			t.Setenv("SEARCHAUTH_WRITE_HOLD", hold)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			abs, err := filepath.Abs("testdata/searchauth/index.js")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Add(ctx, abs); err != nil {
				t.Fatal(err)
			}
			original := map[string]any{"type": "oauth", "accountId": "selected", "access": "old-access", "refresh": "old-refresh", "expires": 9e15, "note": "first"}
			if mode == "two writes after renewal" {
				original["expires"] = 1
			}
			signIn := func(auth map[string]any) string {
				t.Helper()
				key, err := Import(ctx, "seekco", auth)
				if err != nil {
					t.Fatal(err)
				}
				return key
			}
			key := signIn(original)
			search := func(query string) error {
				_, err := RunSearchAs(ctx, abs, "seek", query, 5, key)
				return err
			}
			want := maps.Clone(original)
			var other string
			if mode == "two writes after renewal" {
				if err := search("rotate-twice"); err != nil {
					t.Fatal(err)
				}
				want["access"], want["refresh"] = "rotated-selected", "old-refresh++"
			} else {
				query := "hold-write"
				if mode == "stale remove" {
					query = "hold-remove"
				}
				done := make(chan error, 1)
				go func() { done <- search(query) }()
				defer func() {
					cancel()
					if done != nil {
						<-done
					}
				}()
				for {
					if _, err := os.Stat(hold + ".in"); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("the search hook never began")
					case <-time.After(10 * time.Millisecond):
					}
				}
				switch mode {
				case "rotation":
					want["access"], want["refresh"] = "new-access", "new-refresh"
				case "sign-in again", "stale remove":
					if err := SignOut(ctx, "seekco", key); err != nil {
						t.Fatal(err)
					}
					want["access"], want["refresh"] = "replaced-access", "replaced-refresh"
				case "metadata update":
					want["note"] = "new note about the account"
				case "other account":
					other = signIn(map[string]any{"type": "api", "key": "other-account", "accountId": "other"})
				}
				if other == "" {
					if replaced := signIn(want); replaced != key {
						t.Fatalf("same account changed key: %q → %q", key, replaced)
					}
				} else {
					want["access"] = "rotated-selected"
				}
				if err := os.WriteFile(hold, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				err := <-done
				done = nil
				if err != nil {
					t.Fatal(err)
				}
			}
			saved := Auths("seekco")
			for _, field := range []string{"access", "refresh", "note"} {
				if got := saved[key][field]; got != want[field] {
					t.Errorf("selected %s = %v, want %v", field, got, want[field])
				}
			}
			if other != "" && saved[other]["key"] != "other-account" {
				t.Fatal("the search hook changed the other sign-in")
			}
		})
	}
}
