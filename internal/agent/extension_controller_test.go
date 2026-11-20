package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/update"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func launchAgentDescriptor() sdk.AgentDescriptor {
	return sdk.AgentDescriptor{
		ID: "fixture-launch", Name: "Fixture launch", Bin: "fixture-launch", Aliases: []string{"fixture-start"},
		Launch: &sdk.AgentLaunch{Env: map[string]string{"FIXTURE_URL": "{url}", "FIXTURE_KEY": "{key}"},
			Args: map[string]string{"--mode": "sdk"}, Plain: []string{"login"}},
	}
}

func launchAgentFixture(t *testing.T, descriptor sdk.AgentDescriptor) (*extension.Manager, string) {
	t.Helper()
	host := installAgentActionFixture(t, descriptor)
	home, _ := os.UserHomeDir()
	t.Setenv("SHELL", "/bin/bash")
	if runtime.GOOS != "windows" {
		testenv.Program(t, filepath.Join(home, ".local", "bin", "zsh"), "#!/bin/sh\nexit 1\n")
	}
	for _, name := range []string{".bashrc", ".bash_profile", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("# personal\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, descriptor.Bin), []byte("#!/bin/sh\nprintf '%s\\n' \"$FIXTURE_URL\" \"$FIXTURE_KEY\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return host, home
}

func replaceAgentFixture(t *testing.T, host *extension.Manager, descriptor sdk.AgentDescriptor, version string, minor int) {
	t.Helper()
	entries, err := host.List()
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	source := t.TempDir()
	installed := filepath.Join(filepath.Dir(extensionBindingPath()), "extensions", filepath.FromSlash(entries[0].Directory))
	if err := os.CopyFS(source, os.DirFS(installed)); err != nil {
		t.Fatal(err)
	}
	manifest := entries[0].Manifest.Clone()
	manifest.Version, manifest.Agents = version, []sdk.AgentDescriptor{descriptor}
	manifest.Contract.Version.Minor = minor
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAgentLaunchOnlyOrdinaryLifecycle(t *testing.T) {
	host, home := launchAgentFixture(t, launchAgentDescriptor())
	a, err := Find("fixture-start")
	if err != nil || a.Start == nil || !a.Detected() || a.Path != "" {
		t.Fatalf("launch-only agent absent from ordinary discovery: %+v %v", a, err)
	}
	if Starter("fixture-start") == nil || a.Start().Active {
		t.Fatal("launch-only agent was connected without an explicit action")
	}
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	a, err = Find("fixture-launch")
	if err != nil || !a.Wired() {
		t.Fatal("launch-only binding was not durable across ordinary discovery", err)
	}
	start := a.Start()
	if start.Err != nil || !start.Active || start.Model != "" ||
		!slices.Equal(start.Args, []string{"--mode", "sdk"}) || len(start.Env) != 2 {
		t.Fatalf("env-only launch lost its binding: %+v", start)
	}
	if runtime.GOOS != "windows" {
		command := exec.Command(start.Bin, append(start.Args, "hello")...)
		command.Env = append(os.Environ(), start.Env...)
		raw, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(raw), "--mode\nsdk\nhello\n") || !strings.Contains(string(raw), "http://127.0.0.1:") {
			t.Fatalf("ordinary launch did not reach the executable with SDK argv/env: %s %v", raw, err)
		}
	}
	profiles := []string{".bashrc", ".zshrc"}
	if runtime.GOOS == "darwin" {
		profiles[0] = ".bash_profile"
	}
	for _, name := range profiles {
		raw, _ := os.ReadFile(filepath.Join(home, name))
		if !strings.Contains(string(raw), "fixture-launch run") || !strings.Contains(string(raw), "# personal") {
			t.Fatal("ordinary shell wrapper missing or discarded personal content", name)
		}
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	a, err = Find("fixture-launch")
	if err != nil || a.Start().Err == nil {
		t.Fatal("disabled launch-only module silently fell back to a plain own-account launch", err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal("disabled module could not recover its exact host-owned state", err)
	}
	for _, name := range []string{".bashrc", ".bash_profile", ".zshrc"} {
		raw, _ := os.ReadFile(filepath.Join(home, name))
		if string(raw) != "# personal\n" {
			t.Fatalf("offline disconnect did not restore shell preimage: %q", raw)
		}
	}
	if _, err := Find("fixture-launch"); err == nil {
		t.Fatal("fully disconnected disabled module remained registered")
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", true); err != nil {
		t.Fatal(err)
	}
	a, err = Find("fixture-launch")
	if err != nil || !a.Disconnected() || a.Start().Active || a.Start().Err != nil {
		t.Fatal("reenabling a disconnected module readopted it", err)
	}
}

func TestExtensionAgentProfileExplicitOwnershipAndHeldBaseFence(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Profile = &sdk.AgentProfile{Target: "codex"}
	descriptor.ConfigFiles = []string{".fixture/config.json"}
	descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
	descriptor.Launch.Model = "--model"
	host, home := launchAgentFixture(t, descriptor)
	path := filepath.Join(home, ".fixture/config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"model":"own","note":"personal"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := Find("codex")
	if err != nil || base.ID != "codex" {
		t.Fatal("explicit profile displaced the base before adoption", err)
	}
	profile, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if target, _, owned := profile.SDKLibraryTarget(); !owned || target != nil {
		t.Fatal("inactive profile became a second Library writer")
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	active, err := Find("codex")
	if err != nil || active.ID != descriptor.ID {
		t.Fatal("explicit adoption did not hand ordinary aliases to the profile", err)
	}
	if target, places, owned := active.SDKLibraryTarget(); !owned || target == nil || target.ID != "codex" || places != nil {
		t.Fatal("active profile failed to reuse the existing Library identity")
	}
	if err := base.Apply("model", "owned-by-base"); err == nil {
		t.Fatal("a held built-in object wrote through the adopted profile's fence")
	}
	if base.Sync != nil {
		if err := base.Sync(); err == nil {
			t.Fatal("a held background sync bypassed profile ownership")
		}
	}
	if err := active.Apply("model", "magpie/selected"); err != nil || active.Start().Model != "magpie/selected" {
		t.Fatal("profile model application did not reach ordinary launch", err)
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	active, err = Find("codex")
	if err != nil || active.ID != descriptor.ID || active.Start().Err == nil {
		t.Fatal("off profile silently returned its still-owned base agent", err)
	}
	if err := active.Disconnect(); err != nil {
		t.Fatal("offline profile restoration failed", err)
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, original) {
		t.Fatalf("offline profile restoration lost its original config: %s", raw)
	}
	if base, err := Find("codex"); err != nil || base.ID != "codex" {
		t.Fatal("disconnect did not release ordinary base identity", err)
	}
}

func TestExtensionAgentProfileFencesHeldHermesProxyChange(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Profile = &sdk.AgentProfile{Target: "hermes"}
	_, home := launchAgentFixture(t, descriptor)
	hermesHome := filepath.Join(home, ".hermes")
	t.Setenv("HERMES_HOME", hermesHome)
	if err := os.MkdirAll(hermesHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hermesHome, "config.yaml")
	original := []byte("providers:\n  magpie:\n    base_url: http://127.0.0.1:3425/v1\n    api_mode: chat_completions\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := Find("hermes")
	if err != nil || base.ID != "hermes" || base.Path != path {
		t.Fatalf("could not hold the built-in Hermes: %v", err)
	}
	profile, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	active, err := Find("hermes")
	if err != nil || active.ID != descriptor.ID {
		t.Fatalf("profile did not adopt Hermes: %v", err)
	}
	if err := HermesThrough(base, "http://127.0.0.1:8787/bili/"); err == nil || !strings.Contains(err.Error(), "owned by SDK profile") {
		t.Fatalf("held Hermes did not consult the SDK owner: %v", err)
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, original) {
		t.Fatalf("held Hermes changed the adopted profile's settings: %q, %v", raw, err)
	}
}

func TestExtensionAgentControllerStaleConcurrentPlanAndShellRollback(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.ConfigFiles = []string{".fixture.json"}
	host, home := launchAgentFixture(t, descriptor)
	original := []byte(`{"model":"own","note":"personal"}`)
	path := filepath.Join(home, ".fixture.json")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	controller := &extensionController{home: home, host: host, item: ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, bin: descriptor.Bin}
	plan, err := controller.prepare(t.Context(), "connect", "", "")
	if err != nil || plan.shell == nil || len(plan.shell.Snapshots()) < 2 {
		t.Fatal("fixture did not prepare file and shell changes", err)
	}
	shells := plan.shell.Snapshots()
	last := shells[len(shells)-1]
	userEdit := "# >>> magpie fixture-launch >>>\nuser owns this now\n# <<< magpie fixture-launch <<<\n"
	if err := os.WriteFile(last.Path, []byte(userEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := controller.apply(t.Context(), plan); err == nil {
		t.Fatal("stale shell plan applied")
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, original) {
		t.Fatal("failed shell application did not roll back agent config")
	}
	raw, _ = os.ReadFile(last.Path)
	if string(raw) != userEdit {
		t.Fatal("failed shell application overwrote user drift")
	}
	if err := os.WriteFile(last.Path, []byte("# personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := controller.recover(t.Context()); err != nil {
		t.Fatal("failed transition lost its recoverable ownership record", err)
	}
	first, err := controller.prepare(t.Context(), "connect", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.prepare(t.Context(), "connect", "", "")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, candidate := range []*extensionAgentPlan{first, second} {
		group.Go(func() { results <- controller.apply(t.Context(), candidate) })
	}
	group.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, extension.ErrActionChanged) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatal("same-revision concurrent profile plans did not have one durable winner", success, stale)
	}
}

func TestExtensionAgentControllerOfflineDriftAndPendingRecovery(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.ConfigFiles = []string{".fixture.json"}
	host, home := launchAgentFixture(t, descriptor)
	path := filepath.Join(home, ".fixture.json")
	original := []byte(`{"note":"personal"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := Find(descriptor.ID)
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	applied, _ := os.ReadFile(path)
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	a, _ = Find(descriptor.ID)
	if err := os.WriteFile(path, []byte(`{"note":"edited by user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err == nil {
		t.Fatal("offline recovery overwrote unknown user data")
	}
	if err := os.WriteFile(path, applied, 0o600); err != nil {
		t.Fatal(err)
	}
	controller := &extensionController{home: home, host: host, item: ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, bin: descriptor.Bin}
	unlock, err := extensionBindingLock(t.Context(), "", "ledger", false)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := extensionBindingRead()
	if err != nil {
		t.Fatal(err)
	}
	key := extensionBindingKey(home, "fixture.agent", descriptor.ID)
	record := ledger.Records[key]
	record.Phase = "pending"
	ledger.Records[key] = record
	if err := extensionBindingWrite(ledger); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := update.RestoreShellFunctions(record.Shells); err != nil {
		t.Fatal(err)
	}
	if err := controller.recover(t.Context()); err != nil {
		t.Fatal("interrupted transition with already-restored shells could not recover", err)
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, original) {
		t.Fatal("pending recovery did not restore the original file")
	}
}

func TestExtensionAgentControllerCancellationAndCorruptLedgerFailClosed(t *testing.T) {
	host, home := launchAgentFixture(t, launchAgentDescriptor())
	controller := &extensionController{home: home, host: host, item: ExtensionAgent{Plugin: "fixture.agent", Agent: launchAgentDescriptor()}, bin: "fixture-launch"}
	plan, err := controller.prepare(t.Context(), "connect", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := controller.apply(ctx, plan); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled adoption was admitted", err)
	}
	if record, err := controller.read(t.Context()); err != nil || record.Revision != 0 {
		t.Fatal("canceled adoption changed durable ownership", err)
	}
	if err := os.WriteFile(extensionBindingPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extensionBaseMutation(home, "codex", func() error { t.Fatal("corrupt ledger admitted a base write"); return nil }); err == nil {
		t.Fatal("corrupt ownership was read as empty")
	}
	if _, err := json.Marshal(plan.action.Preview()); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionAgentControllerConfigUpdateRequiresSyncAndPinsPreparedPlans(t *testing.T) {
	descriptor := launchAgentDescriptor()
	host, home := launchAgentFixture(t, descriptor)
	a, _ := Find(descriptor.ID)
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	controller := &extensionController{home: home, host: host, item: ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, bin: descriptor.Bin}
	prepared, err := controller.prepare(t.Context(), "sync", "", "")
	if err != nil {
		t.Fatal(err)
	}
	config, err := host.ModuleConfig(t.Context(), "fixture.agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.CompareAndSwapModuleConfig(t.Context(), "fixture.agent", config.Version, json.RawMessage(`{"updated":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := controller.apply(t.Context(), prepared); !errors.Is(err, extension.ErrActionChanged) {
		t.Fatal("prepared plan was admitted under a replacement configuration", err)
	}
	if start := a.Start(); start.Err == nil || start.Active {
		t.Fatal("old binding silently launched under a replacement configuration", start)
	}
	if err := a.Sync(); err != nil {
		t.Fatal("explicit sync did not renew binding under the current module configuration", err)
	}
	if start := a.Start(); start.Err != nil || !start.Active {
		t.Fatal("renewed binding did not launch", start)
	}
}

func TestExtensionAgentControllerDefinitionReplacementKeepsOriginalRecoveryTarget(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Profile = &sdk.AgentProfile{Target: "codex"}
	host, _ := launchAgentFixture(t, descriptor)
	a, _ := Find(descriptor.ID)
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	entries, err := host.List()
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	source := t.TempDir()
	installed := filepath.Join(filepath.Dir(extensionBindingPath()), "extensions", filepath.FromSlash(entries[0].Directory))
	if err := os.CopyFS(source, os.DirFS(installed)); err != nil {
		t.Fatal(err)
	}
	manifest := entries[0].Manifest.Clone()
	manifest.Version, manifest.Agents[0].Profile.Target = "2.0.0", "gemini"
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	old, err := Find("codex")
	if err != nil || old.ID != descriptor.ID || old.Start().Err == nil {
		t.Fatal("definition update silently retargeted the owned base", err)
	}
	if base, err := Find("gemini"); err != nil || base.ID != "gemini" {
		t.Fatal("definition update claimed its new target without adoption", err)
	}
	if err := old.Disconnect(); err != nil {
		t.Fatal("original definition could not recover after package replacement", err)
	}
	if base, err := Find("codex"); err != nil || base.ID != "codex" {
		t.Fatal("original base stayed owned after recovery", err)
	}
	replacement, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.ConnectHow(); err != nil {
		t.Fatal("explicit adoption of replacement failed", err)
	}
	if err := old.Disconnect(); err == nil {
		t.Fatal("held old-target object disconnected its replacement under the wrong target lock")
	}
	if active, err := Find("gemini"); err != nil || active.ID != descriptor.ID || !active.Wired() {
		t.Fatal("held old object released the new profile's ownership", err)
	}
}

func TestExtensionAgentControllerHeldObjectsRejectNewConfigRoot(t *testing.T) {
	_, home := launchAgentFixture(t, launchAgentDescriptor())
	a, _ := Find("fixture-launch")
	base, _ := Find("codex")
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", foreign)
	if start := a.Start(); start.Err == nil || start.Active {
		t.Fatal("held SDK object followed a fresh global configuration root")
	}
	if err := a.Native.Connect(); err == nil {
		t.Fatal("held SDK object wrote ownership into a foreign configuration root")
	}
	if err := base.Apply("model", "foreign"); err == nil {
		t.Fatal("held base object bypassed its original root's ownership fence")
	}
	if _, err := os.Stat(filepath.Join(foreign, "magpie", "sdk-agent-bindings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("held object wrote an ownership ledger into the new root", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex/config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("held base object mutated its original config under foreign authority", err)
	}
}

func TestExtensionAgentLaunchCommandCannotReplaceBuiltinWithoutProfile(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Bin = "codex"
	installAgentActionFixture(t, descriptor)
	if _, err := Find(descriptor.ID); err == nil {
		t.Fatal("ordinary launch declaration stole a built-in command without an explicit profile")
	}
	if PluginErrors()["fixture.agent"] == "" {
		t.Fatal("launch collision was not reported")
	}
}

func TestExtensionAgentControllerRejectsReadOnlyInputChangedByShell(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.ConfigFiles = []string{".fixture.json", ".fixture-models.json"}
	host, home := launchAgentFixture(t, descriptor)
	config := filepath.Join(home, ".fixture.json")
	models := filepath.Join(home, ".fixture-models.json")
	for _, file := range []string{config, models} {
		if err := os.WriteFile(file, []byte(`{"note":"personal"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	controller := &extensionController{home: home, host: host, item: ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, bin: descriptor.Bin}
	plan, err := controller.prepare(t.Context(), "connect", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.shell == nil || len(plan.shell.Snapshots()) == 0 || len(plan.action.Preview().Files) != 1 ||
		plan.action.Preview().Files[0].Path != ".fixture.json" {
		t.Fatal("fixture needs a shell change, one settings write and a read-only input")
	}
	plan.applyShell = func() error {
		if _, err := plan.shell.Apply(plan.shell.Revision()); err != nil {
			return err
		}
		return os.WriteFile(models, []byte(`{"note":"user edit"}`), 0o600)
	}
	if err := controller.apply(t.Context(), plan); !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("committed ownership with a stale read-only input: %v", err)
	}
	record, err := controller.read(t.Context())
	if err != nil || record.Phase != "" {
		t.Fatalf("failed initial connection left an ownership record: %+v, %v", record, err)
	}
	for file, want := range map[string]string{config: `{"note":"personal"}`, models: `{"note":"user edit"}`} {
		if raw, err := os.ReadFile(file); err != nil || string(raw) != want {
			t.Fatalf("%s after rollback: %q, %v", file, raw, err)
		}
	}
	if err := update.ShellFunctionsMatch(plan.shell.Snapshots(), false); err != nil {
		t.Fatalf("failed connection left its shell wrapper: %v", err)
	}
}

func TestExtensionAgentControllerRetainsPendingAfterPartialShellRollback(t *testing.T) {
	for _, userEdit := range []bool{false, true} {
		t.Run(strconv.FormatBool(userEdit), func(t *testing.T) {
			descriptor := launchAgentDescriptor()
			descriptor.ConfigFiles = []string{".fixture.json"}
			host, home := launchAgentFixture(t, descriptor)
			path := filepath.Join(home, ".fixture.json")
			original := []byte(`{"note":"personal"}`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			controller := &extensionController{home: home, host: host, item: ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor}, bin: descriptor.Bin}
			plan, err := controller.prepare(t.Context(), "connect", "", "")
			if err != nil || len(plan.shell.Snapshots()) < 2 {
				t.Fatal("fixture did not prepare multiple profiles", err)
			}
			first := plan.shell.Snapshots()[0]
			userBody := "# personal\n# >>> magpie fixture-launch >>>\nuser changed the partial wrapper\n# <<< magpie fixture-launch <<<\n"
			plan.applyShell = func() error {
				_, err := update.ShellFunctionAt([]update.ShellProfile{{Path: first.Path, Kind: first.Kind}},
					descriptor.Bin, func(string) string { return first.After })
				if err != nil {
					t.Fatal(err)
				}
				if userEdit {
					if err := os.WriteFile(first.Path, []byte(userBody), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return errors.Join(errors.New("second profile write failed"), errors.New("first profile rollback failed"))
			}
			applyErr := controller.apply(t.Context(), plan)
			if applyErr == nil {
				t.Fatal("partial rollback was reported as successful")
			}
			record, err := controller.read(t.Context())
			if err != nil || record.Phase != "pending" || len(record.Before) != 1 || len(record.Shells) < 2 {
				t.Fatal("partial shell rollback discarded its durable recovery journal", record, err)
			}
			if !strings.Contains(applyErr.Error(), "ownership remains pending") {
				t.Fatal("partial rollback did not report retained ownership", applyErr)
			}
			raw, _ := os.ReadFile(path)
			if !bytes.Equal(raw, original) {
				t.Fatal("partial shell failure did not roll back the config file")
			}
			if userEdit {
				if err := controller.recover(t.Context()); err == nil {
					t.Fatal("recovery blindly overwrote a user-edited partially applied wrapper")
				}
				raw, _ := os.ReadFile(first.Path)
				if string(raw) != userBody {
					t.Fatal("user edit arriving during rollback was overwritten")
				}
				if err := os.WriteFile(first.Path, []byte("# personal\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := controller.recover(t.Context()); err != nil {
				t.Fatal("known preimages could not be explicitly recovered", err)
			}
			record, err = controller.read(t.Context())
			if err != nil || record.Phase != "disconnected" {
				t.Fatal("complete recovery did not release ownership", err)
			}
		})
	}
}

func TestExtensionAgentLaunchUpgradePreservesLegacyOwnerAndRequiresItsDisconnect(t *testing.T) {
	legacy := sdk.AgentDescriptor{ID: "fixture-launch", Name: "Fixture launch", Bin: "fixture-launch",
		ConfigFiles: []string{".fixture.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "Model"}}}
	host := installAgentActionFixture(t, legacy)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture.json")
	if err := os.WriteFile(path, []byte(`{"model":"own","note":"original personal"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, _ := Find(legacy.ID)
	if _, err := old.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	values["note"] = "personal edit after legacy adoption"
	wired, _ := json.Marshal(values)
	if err := os.WriteFile(path, wired, 0o600); err != nil {
		t.Fatal(err)
	}
	stashed, err := os.ReadFile(stashPath())
	if err != nil {
		t.Fatal(err)
	}
	backupRoot := filepath.Join(filepath.Dir(extensionBindingPath()), "extensions/action-backups/fixture.agent/fixture-launch")
	backups := map[string][]byte{}
	if err := filepath.WalkDir(backupRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			body, err := os.ReadFile(path)
			backups[path] = body
			return err
		}
		return nil
	}); err != nil || len(backups) == 0 {
		t.Fatal("legacy fixture has no real durable backup", err)
	}
	updated := legacy.Clone()
	updated.Launch = launchAgentDescriptor().Launch
	replaceAgentFixture(t, host, updated, "2.0.0", 5)
	current, err := Find(updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if start := current.Start(); start.Err == nil || start.Active || !strings.Contains(start.Err.Error(), "fixture.agent/fixture-launch") {
		t.Fatal("launch upgrade silently treated legacy-owned config as a new preimage", start)
	}
	if err := current.Native.Connect(); err == nil {
		t.Fatal("new launch adapter adopted an existing legacy owner")
	}
	if err := current.Disconnect(); err == nil {
		t.Fatal("new adapter silently recovered another adapter's ownership")
	}
	after, _ := os.ReadFile(path)
	stashAfter, _ := os.ReadFile(stashPath())
	if !bytes.Equal(after, wired) || !bytes.Equal(stashAfter, stashed) {
		t.Fatal("refused ownership transition overwrote legacy files or preimages")
	}
	for path, before := range backups {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("refused upgrade changed a legacy durable backup", path, err)
		}
	}
	replaceAgentFixture(t, host, legacy, "3.0.0", 0)
	priorStyle, err := Find(legacy.ID)
	if err != nil || priorStyle.Start != nil {
		t.Fatal("prior descriptor was not restored", err)
	}
	if err := priorStyle.Disconnect(); err != nil {
		t.Fatal("previous adapter could not complete its own safe disconnect", err)
	}
	disconnected, _ := os.ReadFile(path)
	values = nil
	if err := json.Unmarshal(disconnected, &values); err != nil || values["model"] != "own" ||
		values["note"] != "personal edit after legacy adoption" || values["gateway"] != "" {
		t.Fatal("prior-style disconnect discarded a later user edit", string(disconnected), err)
	}
	replaceAgentFixture(t, host, updated, "4.0.0", 5)
	fresh, _ := Find(updated.ID)
	if _, err := fresh.ConnectHow(); err != nil {
		t.Fatal("fresh SDK5 adoption failed after prior ownership was released", err)
	}
	controller := &extensionController{home: home, host: host, item: ExtensionAgent{Plugin: "fixture.agent", Agent: updated}, bin: updated.Bin}
	record, err := controller.read(t.Context())
	if err != nil || record.Phase != "owned" || len(record.Before) != 1 || !bytes.Equal(record.Before[0].Body, disconnected) {
		t.Fatal("fresh SDK5 binding did not capture the actual disconnected preimage", err)
	}
	for path, before := range backups {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("successful transition discarded a legacy backup", path, err)
		}
	}
}

func TestExtensionAgentControllerCrossModuleSameDescriptorCannotStealRecovery(t *testing.T) {
	descriptor := launchAgentDescriptor()
	host, _ := launchAgentFixture(t, descriptor)
	old, _ := Find(descriptor.ID)
	if _, err := old.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	entries, err := host.List()
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	source := t.TempDir()
	installed := filepath.Join(filepath.Dir(extensionBindingPath()), "extensions", filepath.FromSlash(entries[0].Directory))
	if err := os.CopyFS(source, os.DirFS(installed)); err != nil {
		t.Fatal(err)
	}
	manifest := entries[0].Manifest.Clone()
	manifest.ID = "fixture.other-agent"
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	recovery, err := Find(descriptor.ID)
	if err != nil || recovery.extensionMetadata.plugin != "fixture.agent" || recovery.Start().Err == nil {
		t.Fatal("another module with identical descriptor stole the unavailable owner's recovery row", err)
	}
	if err := recovery.Native.Connect(); err == nil {
		t.Fatal("another module's enabled descriptor admitted adoption over the prior owner")
	}
	if err := recovery.Disconnect(); err != nil {
		t.Fatal("original unavailable owner could not be safely recovered", err)
	}
	replacement, err := Find(descriptor.ID)
	if err != nil || replacement.extensionMetadata.plugin != "fixture.other-agent" {
		t.Fatal("released identity did not become available for explicit adoption", err)
	}
	if _, err := replacement.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	if start := old.Start(); start.Err == nil || start.Active {
		t.Fatal("held old module silently launched under a different module's current ownership")
	}
	if err := old.Disconnect(); err == nil {
		t.Fatal("held prior module recovered another module's ownership")
	}
	if !replacement.Wired() {
		t.Fatal("another module's binding was released by the prior object")
	}
}

func TestExtensionAgentControllerRejectsRetargetedConfigurationSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating directory symlinks requires Windows privileges")
	}
	_, home := launchAgentFixture(t, launchAgentDescriptor())
	link := filepath.Join(home, "config-link")
	if err := os.Symlink(filepath.Join(home, ".config"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", link)
	a, err := Find("fixture-launch")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := Find("codex")
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	if err := os.MkdirAll(filepath.Join(foreign, "magpie"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	if start := a.Start(); start.Err == nil || start.Active {
		t.Fatal("same configuration path with a different target silently changed binding authority")
	}
	if err := a.Native.Connect(); err == nil {
		t.Fatal("retargeted configuration symlink admitted a held module's write")
	}
	if err := base.Apply("model", "foreign"); err == nil {
		t.Fatal("retargeted configuration symlink bypassed the held base's ownership fence")
	}
	if _, err := os.Stat(filepath.Join(foreign, "magpie", "sdk-agent-bindings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("held agent wrote into a symlink replacement root", err)
	}
}
