package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestHistoryOwnerSurvivesUnavailableCatalog(t *testing.T) {
	for _, catalog := range []string{"missing", "corrupt"} {
		for _, cold := range []bool{false, true} {
			t.Run(catalog+"/"+map[bool]string{false: "warm", true: "cold"}[cold], func(t *testing.T) {
				setup(t)
				consents := []StorageSourceConsent{{Plugin: "fixture", Source: "history", Agent: "opencode",
					RootSlot: "data", RootRef: strings.Repeat("a", 64), Identity: strings.Repeat("b", 64)}}
				body, err := json.Marshal(consents)
				if err != nil {
					t.Fatal(err)
				}
				writeHistoryAuthority(t, storageConsentsPath(), body)
				if catalog == "corrupt" {
					writeHistoryAuthority(t, extension.CatalogPath(), []byte("{invalid"))
				}
				if !cold {
					sourceView.Store(&sessionSourceView{epoch: sourceEpoch.Load(), owners: map[string]bool{"opencode": true}})
				}
				_ = PrepareSessionSources(t.Context())
				view := sourceView.Load()
				if view == nil || !view.owners["opencode"] {
					t.Fatal("unavailable catalog restored a durably delegated native history owner", view)
				}
			})
		}
	}
}

func TestHistoryOwnerPublicationHonorsCallerCancellation(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	reads := 0
	result := publishSourceReadContext(ctx, func() string {
		reads++
		cancel()
		return "private content"
	})
	if reads != 1 || result != "" {
		t.Fatal("canceled read published its result or selected another owner", reads, result)
	}
	result = publishSourceReadContext(ctx, func() string {
		t.Fatal("already canceled operation performed another read")
		return "private content"
	})
	if result != "" {
		t.Fatal("already canceled operation published content", result)
	}
}

func TestHistoryOwnerUnreadableClaimsBlockColdNativeReaders(t *testing.T) {
	for _, path := range []func() string{sourceBindingsPath, storageConsentsPath} {
		t.Run(filepath.Base(path()), func(t *testing.T) {
			setup(t)
			writeHistoryAuthority(t, path(), []byte("{invalid"))
			if err := PrepareSessionSources(t.Context()); err == nil {
				t.Fatal("unreadable authority was treated as no delegation")
			}
			view := sourceView.Load()
			for _, owner := range nativeHistoryOwners() {
				if nativeHistoryCanRead(view, owner) {
					t.Fatal("unknown authority granted a cold native history reader", owner)
				}
			}
			after, err := os.ReadFile(path())
			if err != nil || string(after) != "{invalid" {
				t.Fatal("failed authority read replaced original bytes", err)
			}
		})
	}
}

func TestHistoryOwnerRejectsHeldNativeReadsAfterDelegation(t *testing.T) {
	setup(t)
	path := filepath.Join(t.TempDir(), "held.jsonl")
	body := []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"native bypass token\"}}\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	held := Session{Agent: "claude", ID: "held", Path: path}
	sourceView.Store(&sessionSourceView{epoch: sourceEpoch.Load(), owners: map[string]bool{"claude": true},
		records: []*sessionSourceRecord{{grant: sourceGrant{
			descriptor: sdk.SessionSourceDescriptor{Agent: "claude", Transcript: true}}}}})
	if _, err := TranscriptOf(held); err == nil {
		t.Fatal("held native transcript bypassed its delegated owner")
	}
	if hit, found, err := SearchSession(t.Context(), held, "native bypass token"); err == nil || found || hit != (Hit{}) {
		t.Fatal("held native search bypassed its delegated owner", hit, found, err)
	}
	if err := readTranscript(held, func(bool, Part) bool {
		t.Fatal("direct transcript consumer saw content from a different history owner")
		return false
	}); err == nil {
		t.Fatal("direct native transcript path accepted a delegated owner")
	}
	if err := checkSourceBeforeCarry(held); err == nil {
		t.Fatal("held conversion source bypassed its delegated owner")
	}
}

func TestHistoryOwnerRechecksSameSelectionBeforePublication(t *testing.T) {
	setup(t)
	path := filepath.Join(t.TempDir(), "held.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"first prompt\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held := Session{Agent: "claude", ID: "held", Path: path}
	owner, err := selectSessionHistoryOwner(t.Context(), held)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = readHistoryTranscript(t.Context(), owner, nil, func(bool, Part) bool {
		calls++
		bindings := []SessionSourceBinding{{Plugin: "fixture", Source: "history", Agent: "claude", Identity: strings.Repeat("a", 64)}}
		body, err := json.Marshal(bindings)
		if err != nil {
			t.Fatal(err)
		}
		writeHistoryAuthority(t, sourceBindingsPath(), body)
		return false
	})
	if calls != 1 || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatal("history read published after its selected native owner changed", calls, err)
	}
	if err := (sessionHistoryOwner{}).check(context.Background()); !errors.Is(err, extension.ErrActionChanged) {
		t.Fatal("zero owner decision admitted a native reader", err)
	}
}

func TestHistoryOwnerRechecksTitlesAfterForeignDelegation(t *testing.T) {
	setup(t)
	data := t.TempDir()
	previous := callDesktopDirs
	callDesktopDirs = func() []string { return []string{data} }
	t.Cleanup(func() { callDesktopDirs = previous; Reset() })
	const id = "11111111-aaaa-4000-8000-000000000001"
	writeDesktopTitle(t, filepath.Join(data, "claude-code-sessions", "account", "org", "local_desktop.json"),
		`{"cliSessionId":"`+id+`","title":"Native title before takeover"}`)
	bindings := []SessionSourceBinding{{Plugin: "fixture", Source: "history", Agent: "claude-desktop", Identity: strings.Repeat("a", 64)}}
	body, err := json.Marshal(bindings)
	if err != nil {
		t.Fatal(err)
	}
	hook := sourceBeforePublish
	sourceBeforePublish = func() {
		writeHistoryAuthority(t, sourceBindingsPath(), body)
	}
	t.Cleanup(func() { sourceBeforePublish = hook })
	if got := Titles([]string{"claude-desktop:" + id}); len(got) != 0 {
		t.Fatal("title publication reused native results after external ownership change", got)
	}
}

func writeHistoryAuthority(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
