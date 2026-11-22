package extension

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestCatalogPublicationWarmAllocations(t *testing.T) {
	ctx := t.Context()
	manager := NewManager(t.TempDir())
	source := t.TempDir()
	manifest := authfixture.Manifest()
	manifest.Contract.Version.Minor = 18
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ProviderSignIn)
	manifest.Window = nil
	manifest.AuthBindings[0].From = sdk.ProviderSignInPart
	manifest.AuthBindings[0].Methods = []sdk.WindowAuthMethod{{ID: "browser", Mode: "poll", Label: "Sign in"}}
	manifest.Contributions = []sdk.ContributionDescriptor{{
		Point: sdk.ContributionLibraryCommand, Version: 1,
		LibraryCommand: &sdk.LibraryCommandDescriptor{ID: "review", Prompt: "Read the diff."},
	}}
	catalogTestWriteManifest(t, source, manifest)
	catalogTestWrite(t, filepath.Join(source, manifest.Entry), []byte("metadata fixture"), 0o700)
	for asset := range 128 {
		catalogTestWrite(t, filepath.Join(source, fmt.Sprintf("asset-%03d.txt", asset)), []byte("asset"), 0o600)
	}
	catalogTestInstall(t, manager.catalog, source)

	signIns := manager.ProviderSignIns(ctx)
	if len(signIns) != 1 {
		t.Fatalf("provider sign-in selections: %v", signIns)
	}
	record := windowAuthRecord{
		Scope:   sdk.Scope{Plugin: manifest.ID, Provider: signIns[0].Provider},
		Binding: signIns[0].Binding.ID, Part: "window:page", Revision: signIns[0].Revision, Config: "0",
	}
	commands, err := manager.LibraryCommands(ctx)
	if err != nil || len(commands) != 1 {
		t.Fatalf("Library commands: %v, %v", commands, err)
	}
	_, publication, err := manager.LibraryCommand(ctx, commands[0].Name, commands[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	readers := []struct {
		name  string
		check func(*Manager) error
	}{
		{"provider-sign-in", func(reader *Manager) error {
			return reader.withWindowAuthAuthority(ctx, record, func() error { return nil })
		}},
		{"library", func(reader *Manager) error {
			selected := publication
			selected.manager = reader
			release, err := selected.Lock(ctx)
			if err == nil {
				release()
			}
			return err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			read := func(manager *Manager) {
				if err := reader.check(manager); err != nil {
					t.Fatal(err)
				}
			}
			cold := testing.AllocsPerRun(10, func() { read(NewManager(manager.Namespace())) })
			warm := testing.AllocsPerRun(10, func() { read(manager) })
			t.Logf("publication allocations per check: warm %.0f, cold %.0f", warm, cold)
			if warm >= cold/2 {
				t.Fatalf("warm publication still allocates like a strict reparse: warm %.0f, cold %.0f", warm, cold)
			}
		})
	}

	foreign := NewCatalog(manager.Namespace())
	if err := foreign.SetEnabled(ctx, manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := foreign.SetEnabled(ctx, manifest.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, reader := range readers {
		if err := reader.check(manager); err == nil {
			t.Errorf("%s admitted a selection from before the enablement change", reader.name)
		}
	}
}
