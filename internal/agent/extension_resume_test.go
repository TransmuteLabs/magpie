package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
)

func TestExtensionResumeUsesOwnedConnectedProfile(t *testing.T) {
	host, _ := launchAgentFixture(t, launchAgentDescriptor())
	if _, err := PrepareExtensionResume(t.Context(), "foreign", "fixture-launch"); err == nil {
		t.Fatal("foreign module borrowed an agent profile")
	}
	if _, err := PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-launch"); err == nil {
		t.Fatal("disconnected profile silently launched outside the SDK")
	}
	selected := Starter("fixture-launch")
	if _, err := selected.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-launch")
	if err != nil || !prepared.Starting.Active || len(prepared.Starting.Env) != 2 {
		t.Fatal("owned profile lost its SDK environment", prepared, err)
	}
	if _, err := PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-start"); err == nil {
		t.Fatal("resume selected a profile through an alias")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(prepared.Recheck(ctx), context.Canceled) {
		t.Fatal("canceled resume retained launch authority")
	}
	if err := selected.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if prepared.Recheck(t.Context()) == nil {
		t.Fatal("old launch survived profile disconnection")
	}
	if _, err := selected.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	prepared, err = PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-launch")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := host.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Recheck(t.Context()); err != nil {
		t.Fatal("checked launch requires a live helper after CLI exec preparation", err)
	}
}

func TestExtensionResumeRechecksCurrentOwnerAfterHelpersStop(t *testing.T) {
	for _, change := range []string{"off", "off-on", "config", "package", "remove"} {
		t.Run(change, func(t *testing.T) {
			host, _ := launchAgentFixture(t, launchAgentDescriptor())
			if _, err := Starter("fixture-launch").ConnectHow(); err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-launch")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := prepared.Recheck(t.Context()); err != nil {
				t.Fatal("unchanged owner requires a running helper", err)
			}
			host = extension.Default()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := host.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			switch change {
			case "off", "off-on":
				if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
					t.Fatal(err)
				}
				if change == "off-on" {
					if err := host.SetEnabled(t.Context(), "fixture.agent", true); err != nil {
						t.Fatal(err)
					}
				}
			case "config":
				config, err := host.ModuleConfig(t.Context(), "fixture.agent")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := host.CompareAndSwapModuleConfig(t.Context(), "fixture.agent", config.Version, json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "package":
				replaceAgentFixture(t, host, launchAgentDescriptor(), "1.0.1", 5)
			case "remove":
				if err := host.Remove(t.Context(), "fixture.agent"); err != nil {
					t.Fatal(err)
				}
			}
			err = prepared.Recheck(t.Context())
			if err == nil {
				t.Fatal("resume retained authority after current SDK owner changed", change)
			}
			if (change == "config" || change == "package" || change == "off-on") && !errors.Is(err, extension.ErrActionChanged) {
				t.Fatal("replacement did not report stale launch authority", err)
			}
			if change == "off-on" {
				if _, err := PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-launch"); err != nil {
					t.Fatal("re-enabled owner could not prepare a fresh launch", err)
				}
			}
		})
	}
}

func TestExtensionResumeRejectsEditedLaunch(t *testing.T) {
	host, _ := launchAgentFixture(t, launchAgentDescriptor())
	if _, err := Starter("fixture-launch").ConnectHow(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-launch")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	captured := prepared.Starting
	for _, field := range []string{"bin", "env", "args", "model", "flag", "plain", "active", "err"} {
		t.Run(field, func(t *testing.T) {
			prepared.Starting = captured
			prepared.Starting.Env = slices.Clone(captured.Env)
			prepared.Starting.Args = slices.Clone(captured.Args)
			prepared.Starting.Plain = slices.Clone(captured.Plain)
			switch field {
			case "bin":
				prepared.Starting.Bin = "unrelated"
			case "env":
				prepared.Starting.Env[0] = "FIXTURE_URL=http://unrelated.invalid"
			case "args":
				prepared.Starting.Args[1] = "plain"
			case "model":
				prepared.Starting.Model = "unrelated"
			case "flag":
				prepared.Starting.Flag = "--unrelated"
			case "plain":
				prepared.Starting.Plain = append(prepared.Starting.Plain, "resume")
			case "active":
				prepared.Starting.Active = false
			case "err":
				prepared.Starting.Err = errors.New("launch failed")
			}
			if err := prepared.Recheck(t.Context()); !errors.Is(err, extension.ErrActionChanged) {
				t.Fatal("edited prepared launch retained SDK authority", field, err)
			}
		})
	}
	prepared.Starting = captured
	if err := prepared.Recheck(t.Context()); err != nil {
		t.Fatal("unchanged launch was invalidated", err)
	}
}

func TestExtensionResumeOwnerStopsAtDeadline(t *testing.T) {
	host, _ := launchAgentFixture(t, launchAgentDescriptor())
	if _, err := Starter("fixture-launch").ConnectHow(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareExtensionResume(t.Context(), "fixture.agent", "fixture-launch")
	if err != nil {
		t.Fatal(err)
	}
	unlock, held := filelock.Lock(filepath.Join(host.Namespace(), "catalog.lock"), time.Second)
	if !held {
		t.Fatal("could not hold the SDK catalog lock")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- prepared.Recheck(ctx) }()
	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("prepared launch returned %v instead of the deadline", err)
		}
	case <-time.After(3 * time.Second):
		unlock()
		<-done
		t.Fatal("prepared launch waited for the catalog after the deadline")
	}
	if err := prepared.Recheck(t.Context()); err != nil {
		t.Fatal("a canceled check broke the existing launch binding", err)
	}
}
