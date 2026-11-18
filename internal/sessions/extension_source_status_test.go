package sessions

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSourceStatusReportsMalformedBindingsWhilePreparing(t *testing.T) {
	setup(t)
	previous := sourceView.Load()
	sourceView.Store(nil)
	t.Cleanup(func() { sourceView.Store(previous) })
	path := sourceBindingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{broken")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case sourcePreparing <- struct{}{}:
		defer func() { <-sourcePreparing }()
	default:
		t.Fatal("unexpected active source preparation")
	}
	status := InspectSessionSources(context.Background())
	if !status.Pending || len(status.Errors) == 0 || len(status.Bindings) != 0 {
		t.Fatal("busy source preparation suppressed an unreadable binding", status)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != string(original) {
		t.Fatal("status inspection changed unreadable source authority", err)
	}
}

func TestSourceCallsOnlyAgentsKeepTheirOrdinaryReaders(t *testing.T) {
	t.Run("devin", func(t *testing.T) {
		devinSetup(t)
		if len(devinCalls(t)) != 7 {
			t.Fatal("native Devin call fixture is missing")
		}
		if slices.ContainsFunc(allFiles(), func(item file) bool { return item.agent == "devin" }) ||
			slices.ContainsFunc(List(0), func(item Session) bool { return item.Agent == "devin" }) ||
			nativeSourceOwner("devin") {
			t.Fatal("call-only Devin acquired ordinary session ownership")
		}
	})
	t.Run("minimax-code", func(t *testing.T) {
		claude, _ := setup(t)
		copyTree(t, "testdata/minimax", filepath.Join(filepath.Dir(claude), "home", ".minimax"))
		if !slices.ContainsFunc(Calls(time.Time{}), func(item Call) bool { return item.Agent == "minimax-code" }) {
			t.Fatal("native MiniMax call fixture is missing")
		}
		if slices.ContainsFunc(allFiles(), func(item file) bool { return item.agent == "minimax-code" }) ||
			slices.ContainsFunc(List(0), func(item Session) bool { return item.Agent == "minimax-code" }) ||
			nativeSourceOwner("minimax-code") {
			t.Fatal("call-only MiniMax acquired ordinary session ownership")
		}
	})
}
