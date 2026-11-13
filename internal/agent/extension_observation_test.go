package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionObservationFile struct {
	Mode fs.FileMode
	Size int64
	Time time.Time
	Body [32]byte
}

func extensionObservationTree(t *testing.T, home string) map[string]extensionObservationFile {
	t.Helper()
	files := map[string]extensionObservationFile{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := extensionObservationFile{Mode: info.Mode(), Size: info.Size(), Time: info.ModTime()}
		if !entry.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			record.Body = sha256.Sum256(raw)
		}
		name, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		files[name] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func extensionObservationNoOwnershipFiles(t *testing.T) {
	t.Helper()
	for _, path := range []string{extensionBindingPath(), filepath.Join(filepath.Dir(extensionBindingPath()), "sdk-agent-locks")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("read-only SDK profile discovery created ownership state", path, err)
		}
	}
}

func extensionObservationDescriptor() sdk.AgentDescriptor {
	descriptor := launchAgentDescriptor()
	descriptor.Profile = &sdk.AgentProfile{Target: "codex"}
	descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
	return descriptor
}

func TestExtensionAgentInactiveProfileObservationDoesNotCreateState(t *testing.T) {
	descriptor := extensionObservationDescriptor()
	installAgentActionFixture(t, descriptor)
	home, _ := os.UserHomeDir()
	extensionObservationNoOwnershipFiles(t)
	before := extensionObservationTree(t, home)
	for range 3 {
		if len(All()) == 0 {
			t.Fatal("discovery returned no ordinary agents")
		}
		base, err := Find("codex")
		if err != nil || base.ID != "codex" {
			t.Fatal("inactive profile hid its base", err)
		}
		profile, err := Find(descriptor.ID)
		if err != nil {
			t.Fatal(err)
		}
		if target, _, owned := profile.SDKLibraryTarget(); !owned || target != nil {
			t.Fatal("inactive profile received Library write authority")
		}
		if state := profile.Native.Read(); state.Provider != "disconnected" {
			t.Fatal("inactive profile observation lost its disconnected state", state)
		}
		if value := profile.Fields[0].Get(); value != "" {
			t.Fatal("inactive profile invented a field value", value)
		}
	}
	extensionObservationNoOwnershipFiles(t)
	if after := extensionObservationTree(t, home); !reflect.DeepEqual(before, after) {
		t.Fatal("read-only installed-profile observations wrote under the home")
	}
}

func TestExtensionAgentInactiveProfileFreshCLIHelpDoesNotCreateState(t *testing.T) {
	descriptor := extensionObservationDescriptor()
	installAgentActionFixture(t, descriptor)
	home, _ := os.UserHomeDir()
	extensionObservationNoOwnershipFiles(t)
	binary := filepath.Join(t.TempDir(), "magpie")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-mod=readonly", "-p=1", "-race", "-tags=nogui", "-o", binary, "../..")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=1")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ordinary CLI: %v\n%s", err, output)
	}
	before := extensionObservationTree(t, home)
	for _, id := range []string{descriptor.ID, descriptor.Aliases[0], descriptor.Profile.Target} {
		t.Run(id, func(t *testing.T) {
			command := exec.CommandContext(ctx, binary, id, "--help")
			command.Env = os.Environ()
			output, err := command.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "magpie "+id) && !strings.Contains(string(output), "magpie "+descriptor.ID) {
				t.Fatalf("fresh CLI help: %s: %v\n%s", id, err, output)
			}
			extensionObservationNoOwnershipFiles(t)
			if after := extensionObservationTree(t, home); !reflect.DeepEqual(before, after) {
				t.Fatal("fresh CLI help for an installed inactive SDK profile wrote under the home")
			}
		})
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(fmt.Errorf("fresh CLI help did not finish: %w", err))
	}
}

func TestExtensionAgentObservationRejectsUnknownOwnershipWithoutGrantingWrites(t *testing.T) {
	descriptor := extensionObservationDescriptor()
	descriptor.Profile.Target = "agy"
	installAgentActionFixture(t, descriptor)
	home, _ := os.UserHomeDir()
	base, err := Find("agy")
	if err != nil || base.Start == nil {
		t.Fatal("base launch fixture unavailable", err)
	}
	profile, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	lockRoot := filepath.Join(filepath.Dir(extensionBindingPath()), "sdk-agent-locks")
	cases := map[string][]byte{
		"syntax":          []byte("{broken"),
		"unknown":         append(bytes.TrimSuffix(bytes.Clone(original), []byte("}")), []byte(`,"unknown":true}`)...),
		"missing":         []byte(`{}`),
		"missing-records": []byte(`{"schema":1}`),
		"null-records":    []byte(`{"schema":1,"records":null}`),
		"duplicate":       append(bytes.TrimSuffix(bytes.Clone(original), []byte("}")), []byte(`,"records":{}}`)...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.RemoveAll(lockRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(extensionBindingPath(), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			before := extensionObservationTree(t, home)
			home, cfg := homes()
			ordinary := onMachine(home, cfg)
			errs := map[string]string{}
			if len(extensionAgents(home, ordinary, errs)) == 0 || errs["sdk-ownership"] == "" {
				t.Fatal("unknown ownership was silently discarded", errs)
			}
			visible, err := Find("agy")
			if err != nil || visible.ID != "agy" || visible.extensionMetadata != nil {
				t.Fatal("unknown ownership hid the base as owned", err)
			}
			profile, err := Find(descriptor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if target, _, owned := profile.SDKLibraryTarget(); !owned || target != nil {
				t.Fatal("unknown ownership granted Library write authority")
			}
			if state := profile.Native.Read(); state.Provider != "invalid" {
				t.Fatal("unknown ownership became a disconnected or connected record", state)
			}
			if after := extensionObservationTree(t, home); !reflect.DeepEqual(before, after) {
				t.Fatal("unknown ownership observation wrote under the home")
			}
			if err := base.Fields[0].Set("must-not-write"); err == nil {
				t.Fatal("unknown ownership permitted a base mutation")
			}
			if base.Start().Err == nil {
				t.Fatal("unknown ownership permitted a base launch")
			}
			if _, err := profile.ConnectHow(); err == nil {
				t.Fatal("unknown ownership permitted profile adoption")
			}
			if after, err := os.ReadFile(extensionBindingPath()); err != nil || !bytes.Equal(after, raw) {
				t.Fatal("unknown ownership was rewritten by a failed mutation", err)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("chmod does not revoke Windows read access")
		}
		if err := os.RemoveAll(lockRoot); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(extensionBindingPath(), original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(extensionBindingPath(), 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(extensionBindingPath(), 0o600)
		if file, err := os.Open(extensionBindingPath()); err == nil {
			file.Close()
			t.Fatal("unreadable ledger fixture is readable")
		}
		visible, err := Find("agy")
		if err != nil || visible.ID != "agy" {
			t.Fatal("unreadable ownership hid the base", err)
		}
		profile, err := Find(descriptor.ID)
		if err != nil {
			t.Fatal(err)
		}
		if target, _, owned := profile.SDKLibraryTarget(); !owned || target != nil {
			t.Fatal("unreadable ownership granted Library write authority")
		}
		if state := profile.Native.Read(); state.Provider != "invalid" {
			t.Fatal("unreadable ownership became known", state)
		}
		if _, err := os.Lstat(lockRoot); !os.IsNotExist(err) {
			t.Fatal("unreadable ownership observation created locks", err)
		}
		if err := base.Fields[0].Set("must-not-write"); err == nil || base.Start().Err == nil {
			t.Fatal("unreadable ownership permitted a base mutation or launch", err)
		}
	})
}

func TestExtensionAgentObservationRetriesAtomicRenameAndKeepsOldSnapshots(t *testing.T) {
	descriptor := extensionObservationDescriptor()
	installAgentActionFixture(t, descriptor)
	profile, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(extensionBindingPath())
	if err != nil {
		t.Fatal(err)
	}
	var next extensionBindingLedger
	if err := json.Unmarshal(original, &next); err != nil {
		t.Fatal(err)
	}
	for key, record := range next.Records {
		record.Revision++
		next.Records[key] = record
	}
	replacement, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	for _, timing := range []string{"before-open", "after-open"} {
		t.Run(timing, func(t *testing.T) {
			if err := os.WriteFile(extensionBindingPath(), original, 0o600); err != nil {
				t.Fatal(err)
			}
			lockRoot := filepath.Join(filepath.Dir(extensionBindingPath()), "sdk-agent-locks")
			if err := os.RemoveAll(lockRoot); err != nil {
				t.Fatal(err)
			}
			opens := 0
			replace := func(path string) error {
				pathNext := path + ".next"
				if err := os.WriteFile(pathNext, replacement, 0o600); err != nil {
					return err
				}
				return os.Rename(pathNext, path)
			}
			open := func(path string) (*os.File, error) {
				opens++
				if opens == 1 && timing == "before-open" {
					if err := replace(path); err != nil {
						return nil, err
					}
				}
				file, err := os.Open(path)
				if err == nil && opens == 1 && timing == "after-open" {
					if err = replace(path); err != nil {
						file.Close()
						return nil, err
					}
				}
				return file, err
			}
			snapshot, err := extensionBindingSnapshot(t.Context(), true, open)
			if err != nil || len(snapshot.Records) != 1 {
				t.Fatal("atomic rename became unknown or partial ownership", err)
			}
			for _, record := range snapshot.Records {
				want := uint64(1)
				if timing == "before-open" {
					want = 2
				}
				if record.Phase != "owned" || record.Revision != want {
					t.Fatal("atomic rename returned a partial or unowned record", record.Phase, record.Revision)
				}
			}
			if timing == "before-open" && opens != 2 || timing == "after-open" && opens != 1 {
				t.Fatal("observer did not preserve its opened snapshot or retry an identity mismatch", opens)
			}
			if _, err := os.Lstat(lockRoot); !os.IsNotExist(err) {
				t.Fatal("snapshot observation materialized a lock directory", err)
			}
		})
	}
}

func TestExtensionAgentObservationAdoptionSubprocess(t *testing.T) {
	home := os.Getenv("MAGPIE_OBSERVATION_ADOPT_HOME")
	if home == "" {
		return
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	profile, err := Find(os.Getenv("MAGPIE_OBSERVATION_ADOPT_AGENT"))
	if err != nil || profile.extensionMetadata == nil || profile.extensionMetadata.base == nil {
		t.Fatal("profile subprocess has no captured base", err)
	}
	metadata := profile.extensionMetadata
	host := extension.Default()
	defer host.Close(context.Background())
	controller := &extensionController{
		home: home, host: host, item: ExtensionAgent{Plugin: metadata.plugin, Agent: metadata.descriptor.Clone()},
		base: metadata.base, bin: profile.Bin, root: extensionBindingPath(), rootGuard: extensionAgentRootGuard(home),
	}
	plan, err := controller.prepare(t.Context(), "connect", "", "")
	if err != nil {
		t.Fatal("prepare adoption:", err)
	}
	fmt.Println("READY")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := controller.apply(t.Context(), plan); err != nil {
		if !strings.Contains(err.Error(), "already owned") {
			t.Fatal("adoption failed for an unrelated reason:", err)
		}
		fmt.Println("RESULT refused")
	} else {
		fmt.Println("RESULT adopted")
	}
}

func TestExtensionAgentObservationAdoptionCoordinatesProcesses(t *testing.T) {
	first := extensionObservationDescriptor()
	first.Profile.Target = "agy"
	host := installAgentActionFixture(t, first)
	home, _ := os.UserHomeDir()
	t.Setenv("SHELL", "/bin/bash")
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("# personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := host.List()
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	source := t.TempDir()
	installed := filepath.Join(host.Namespace(), filepath.FromSlash(entries[0].Directory))
	if err := os.CopyFS(source, os.DirFS(installed)); err != nil {
		t.Fatal(err)
	}
	manifest := entries[0].Manifest.Clone()
	second := first.Clone()
	second.ID, second.Name, second.Bin, second.Aliases = "fixture-second", "Second profile", "fixture-second", nil
	manifest.Version, manifest.Agents = "1.0.1", []sdk.AgentDescriptor{first, second}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	base, err := Find("agy")
	if err != nil || base.Start == nil {
		t.Fatal("ordinary base unavailable", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	type child struct {
		command *exec.Cmd
		input   io.WriteCloser
		output  *bufio.Reader
	}
	var children []child
	defer func() {
		cancel()
		for _, process := range children {
			process.input.Close()
			if process.command.ProcessState == nil {
				process.command.Wait()
			}
		}
	}()
	for _, id := range []string{first.ID, second.ID} {
		command := exec.CommandContext(ctx, executable, "-test.run=^TestExtensionAgentObservationAdoptionSubprocess$")
		command.Env = append(os.Environ(), "MAGPIE_OBSERVATION_ADOPT_HOME="+home, "MAGPIE_OBSERVATION_ADOPT_AGENT="+id)
		command.Stderr = os.Stderr
		input, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child{command: command, input: input, output: bufio.NewReader(output)})
	}
	line := func(process child) string {
		value, err := process.output.ReadString('\n')
		if err != nil {
			t.Fatal("adoption process did not reach its barrier:", err)
		}
		return strings.TrimSpace(value)
	}
	for _, process := range children {
		if got := line(process); got != "READY" {
			t.Fatal("adoption process did not prepare a plan", got)
		}
	}
	for _, process := range children {
		if _, err := process.input.Write([]byte("apply\n")); err != nil {
			t.Fatal(err)
		}
	}
	results := map[string]int{}
	for _, process := range children {
		results[line(process)]++
		process.input.Close()
		if err := process.command.Wait(); err != nil {
			t.Fatal("adoption process failed:", err)
		}
	}
	if results["RESULT adopted"] != 1 || results["RESULT refused"] != 1 {
		t.Fatal("two processes did not produce exactly one target owner", results)
	}
	ledger, err := extensionBindingObserve(t.Context())
	if err != nil || len(ledger.Records) != 1 {
		t.Fatal("adoption did not publish exactly one durable owner", err)
	}
	if err := base.Fields[0].Set("must-not-write"); err == nil || !strings.Contains(err.Error(), "owned by SDK profile") {
		t.Fatal("the held base wrote through the adopted profile", err)
	}
	if err := base.Start().Err; err == nil || !strings.Contains(err.Error(), "owned by SDK profile") {
		t.Fatal("the held base launched through the adopted profile", err)
	}
}
