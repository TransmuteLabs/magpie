package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSessionExtensionNativeCurrentStoresAreAvailable(test *testing.T) {
	setup(test)
	test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(test.TempDir(), "custom-claude"))
	test.Setenv("CODEX_HOME", filepath.Join(test.TempDir(), "custom-codex"))
	for _, descriptor := range []sdk.SessionDescriptor{{To: "claude", OutputDir: ".claude/projects"}, {To: "codex", OutputDir: ".codex/sessions"}} {
		if !extensionSessionTargetMatches(descriptor) {
			test.Fatalf("current native %s destination is unavailable", descriptor.To)
		}
	}
	data := test.TempDir()
	if err := os.Mkdir(filepath.Join(data, "omp"), 0o700); err != nil {
		test.Fatal(err)
	}
	test.Setenv("XDG_DATA_HOME", data)
	if !extensionSessionTargetMatches(sdk.SessionDescriptor{To: "omp", OutputDir: ".omp/agent/sessions"}) {
		test.Fatal("current migrated OMP destination is unavailable")
	}
}

func nativeStoreFixture(test *testing.T) (*extension.Manager, Session) {
	test.Helper()
	distribution, fixtures := os.Getenv("MAGPIE_OMP_TEST_DIST"), os.Getenv("MAGPIE_OMP_TEST_FIXTURES")
	if distribution == "" || fixtures == "" {
		test.Skip("requires explicitly built native OMP module and synthetic fixtures")
	}
	setup(test)
	home, _ := os.UserHomeDir()
	test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	test.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for _, name := range []string{"XDG_DATA_HOME", "PI_CONFIG_DIR", "PI_CODING_AGENT_DIR", "OMP_PROFILE", "PI_PROFILE"} {
		test.Setenv(name, "")
	}
	test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent-bun"))
	old := agentThere
	agentThere = func(string) bool { return true }
	test.Cleanup(func() { agentThere = old })
	journal, err := os.ReadFile(filepath.Join(fixtures, "omp-v3.jsonl"))
	if err != nil {
		test.Fatal(err)
	}
	image, err := os.ReadFile(filepath.Join(fixtures, "image.bin"))
	if err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(image)
	const id = "019a0000-0000-7000-8000-00000000000a"
	source := filepath.Join(OmpDir(), "sessions", "--work-omp--", "2026-10-25T03-50-25-123Z_"+id+".jsonl")
	for name, body := range map[string][]byte{source: journal, filepath.Join(OmpDir(), "blobs", hex.EncodeToString(digest[:])): image} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(name, body, 0o600); err != nil {
			test.Fatal(err)
		}
	}
	host := extension.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	if _, err := host.Install(test.Context(), distribution); err != nil {
		test.Fatal(err)
	}
	Reset()
	selected, ok := Find("omp", id)
	if !ok {
		test.Fatal("native OMP fixture not found")
	}
	return host, selected
}

func TestSessionExtensionNativeCustomStoreAndCheckedReuse(test *testing.T) {
	for _, target := range []string{"claude", "codex", "omp-xdg", "omp-profile", "omp-xdg-profile", "omp-config-root", "omp-agent-root"} {
		test.Run(target, func(test *testing.T) {
			_, source := nativeStoreFixture(test)
			var destination string
			agent := target
			switch target {
			case "claude":
				root := filepath.Join(test.TempDir(), "custom-claude")
				test.Setenv("CLAUDE_CONFIG_DIR", root)
				destination = filepath.Join(root, "projects")
			case "codex":
				root := filepath.Join(test.TempDir(), "custom-codex")
				test.Setenv("CODEX_HOME", root)
				destination = filepath.Join(root, "sessions")
				if err := os.MkdirAll(root, 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("profile = \"work\"\n[profiles.work]\nmodel_provider = \"profile-provider\"\n"), 0o600); err != nil {
					test.Fatal(err)
				}
			default:
				converted, err := CarryOver(source, "claude")
				if err != nil {
					test.Fatal(err)
				}
				Reset()
				var ok bool
				source, ok = Find("claude", converted.ID)
				if !ok {
					test.Fatal("native reverse source not found")
				}
				agent = "omp"
				home, _ := os.UserHomeDir()
				if target == "omp-config-root" {
					test.Setenv("PI_CONFIG_DIR", ".custom-omp")
					destination = filepath.Join(home, ".custom-omp", "agent", "sessions")
				} else if target == "omp-agent-root" {
					root := filepath.Join(test.TempDir(), "custom-agent")
					test.Setenv("PI_CODING_AGENT_DIR", root)
					destination = filepath.Join(root, "sessions")
				} else if target == "omp-profile" {
					test.Setenv("OMP_PROFILE", "work")
					destination = filepath.Join(home, ".omp", "profiles", "work", "agent", "sessions")
				} else {
					data := test.TempDir()
					test.Setenv("XDG_DATA_HOME", data)
					destination = filepath.Join(data, "omp", "sessions")
					if target == "omp-xdg-profile" {
						test.Setenv("OMP_PROFILE", "work")
						destination = filepath.Join(data, "omp", "profiles", "work", "sessions")
					}
					if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
						test.Fatal(err)
					}
				}
			}
			first, err := CarryOver(source, agent)
			if err != nil {
				test.Fatal("current-store carry failed:", err)
			}
			if !strings.HasPrefix(first.Path, filepath.Clean(destination)+string(filepath.Separator)) {
				test.Fatalf("wrong native target: %s, want %s", first.Path, destination)
			}
			Reset()
			output, ok := Find(agent, first.ID)
			if !ok {
				test.Fatal("current-store output absent from native discovery")
			}
			if transcript, err := TranscriptOf(output); err != nil || len(transcript.Parts) == 0 {
				test.Fatal("current-store native transcript unreadable:", err)
			}
			if strings.Contains(target, "profile") {
				nativeCommand := resumeCommand("", agent, output.ID, output.Cwd, output.Path)
				if !strings.Contains(first.Command, "--profile work") || !strings.Contains(nativeCommand, "--profile work") {
					test.Fatal("named OMP profile absent from migration/native resume:", first.Command, nativeCommand)
				}
			}
			again, err := CarryOver(source, agent)
			if err != nil || again != first {
				test.Fatalf("unchanged migration was not safely reused: %+v %+v %v", first, again, err)
			}
		})
	}
}

func TestSessionExtensionReuseChecksSourceOutputAndConfig(test *testing.T) {
	host, source := nativeStoreFixture(test)
	first, err := CarryOver(source, "claude")
	if err != nil {
		test.Fatal(err)
	}
	second, err := CarryOver(source, "claude")
	if err != nil || second != first {
		test.Fatal("unchanged first receipt was not reused")
	}
	sourceInfo, _ := os.Stat(source.Path)
	body, _ := os.ReadFile(source.Path)
	changed := bytes.ReplaceAll(body, []byte("human request"), []byte("human revised"))
	if len(changed) != len(body) || bytes.Equal(changed, body) {
		test.Fatal("source mutation fixture is ineffective")
	}
	if err := os.WriteFile(source.Path, changed, 0o600); err != nil {
		test.Fatal(err)
	}
	os.Chtimes(source.Path, sourceInfo.ModTime(), sourceInfo.ModTime())
	third, err := CarryOver(source, "claude")
	if err != nil || third.ID == first.ID {
		test.Fatal("same-size source change reused stale migration:", err)
	}
	output, _ := os.ReadFile(third.Path)
	if err := os.Rename(third.Path, third.Path+".retained"); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(third.Path, output, 0o600); err != nil {
		test.Fatal(err)
	}
	fourth, err := CarryOver(source, "claude")
	if err != nil || fourth.ID == third.ID {
		test.Fatal("replaced output inode was reused:", err)
	}
	current, err := host.ModuleConfig(test.Context(), "omp-sessions")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := host.CompareAndSwapModuleConfig(test.Context(), "omp-sessions", current.Version, []byte(`{"testSetting":true}`)); err != nil {
		test.Fatal(err)
	}
	fifth, err := CarryOver(source, "claude")
	if err != nil || fifth.ID == fourth.ID {
		test.Fatal("new module config reused old output:", err)
	}
}

func TestSessionExtensionConcurrentCarryReusesOneOutput(test *testing.T) {
	_, source := nativeStoreFixture(test)
	var wg sync.WaitGroup
	results := make(chan CarryResult, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := CarryOver(source, "claude")
			results <- result
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			test.Fatal(err)
		}
	}
	first := ""
	for result := range results {
		if first == "" {
			first = result.ID
		}
		if result.ID != first {
			test.Fatal("concurrent repeat created several native sessions")
		}
	}
}

func TestSessionExtensionCheckedReuseInvalidation(test *testing.T) {
	for _, change := range []string{"source-identity", "attachment-identity", "attachment-content", "output-content", "output-parent-mode", "package", "disabled", "removed", "destination", "provider"} {
		test.Run(change, func(test *testing.T) {
			if change == "output-parent-mode" && runtime.GOOS == "windows" {
				test.Skip("POSIX directory permissions")
			}
			host, source := nativeStoreFixture(test)
			target := "claude"
			if change == "provider" {
				target = "codex"
			}
			first, err := CarryOver(source, target)
			if err != nil {
				test.Fatal(err)
			}
			unchanged, err := CarryOver(source, target)
			if err != nil || unchanged != first {
				test.Fatal("unchanged migration was not reused:", err)
			}
			original, err := os.ReadFile(first.Path)
			if err != nil {
				test.Fatal(err)
			}
			switch change {
			case "source-identity", "attachment-identity":
				name := source.Path
				if change == "attachment-identity" {
					blobs, err := filepath.Glob(filepath.Join(OmpDir(), "blobs", "*"))
					if err != nil || len(blobs) != 1 {
						test.Fatal("expected one source attachment:", blobs, err)
					}
					name = blobs[0]
				}
				body, err := os.ReadFile(name)
				if err != nil {
					test.Fatal(err)
				}
				if err := os.Rename(name, name+".retained"); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(name, body, 0o600); err != nil {
					test.Fatal(err)
				}
			case "attachment-content":
				blobs, err := filepath.Glob(filepath.Join(OmpDir(), "blobs", "*"))
				if err != nil || len(blobs) != 1 {
					test.Fatal("expected one source attachment:", blobs, err)
				}
				body, err := os.ReadFile(blobs[0])
				if err != nil || len(body) == 0 {
					test.Fatal("source attachment unavailable:", err)
				}
				info, err := os.Stat(blobs[0])
				if err != nil {
					test.Fatal(err)
				}
				body[0] ^= 1
				if err := os.WriteFile(blobs[0], body, 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Chtimes(blobs[0], info.ModTime(), info.ModTime()); err != nil {
					test.Fatal(err)
				}
			case "output-content":
				info, err := os.Stat(first.Path)
				if err != nil {
					test.Fatal(err)
				}
				body := bytes.ReplaceAll(original, []byte("human request"), []byte("human revised"))
				if len(body) != len(original) || bytes.Equal(body, original) {
					test.Fatal("output mutation fixture is ineffective")
				}
				if err := os.WriteFile(first.Path, body, 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Chtimes(first.Path, info.ModTime(), info.ModTime()); err != nil {
					test.Fatal(err)
				}
				original = body
			case "output-parent-mode":
				if err := os.Chmod(filepath.Dir(first.Path), 0o755); err != nil {
					test.Fatal(err)
				}
			case "package":
				before, err := host.List()
				if err != nil || len(before) != 1 {
					test.Fatal("module catalog unavailable:", err)
				}
				after, err := host.Install(test.Context(), os.Getenv("MAGPIE_OMP_TEST_DIST"))
				if err != nil || after.Revision == before[0].Revision {
					test.Fatal("fixture did not replace package revision:", err)
				}
			case "disabled":
				if err := host.SetEnabled(test.Context(), "omp-sessions", false); err != nil {
					test.Fatal(err)
				}
			case "removed":
				if err := host.Remove(test.Context(), "omp-sessions"); err != nil {
					test.Fatal(err)
				}
			case "destination":
				test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(test.TempDir(), "changed-claude"))
			case "provider":
				if err := os.WriteFile(filepath.Join(CodexDir(), "config.toml"), []byte("model_provider = \"changed-provider\"\n"), 0o600); err != nil {
					test.Fatal(err)
				}
			}
			next, err := CarryOver(source, target)
			if change == "disabled" || change == "removed" || change == "attachment-content" {
				if err == nil {
					test.Fatal("revoked or invalid migration was reused")
				}
			} else {
				if err != nil || next.ID == first.ID {
					test.Fatal("changed migration reused old output:", err)
				}
				if change == "destination" && !strings.HasPrefix(next.Path, filepath.Join(ClaudeDir(), "projects")+string(filepath.Separator)) {
					test.Fatal("changed target still wrote to the old store:", next.Path)
				}
				if change == "provider" {
					body, err := os.ReadFile(next.Path)
					if err != nil || !bytes.Contains(body, []byte(`"model_provider":"changed-provider"`)) {
						test.Fatal("new native rollout did not use current provider:", err)
					}
				}
			}
			retained, err := os.ReadFile(first.Path)
			if err != nil || !bytes.Equal(original, retained) {
				test.Fatal("invalidated migration overwrote its old output:", err)
			}
		})
	}
}

func TestSessionExtensionNativeLargeOrdinaryCarryOver(test *testing.T) {
	for _, target := range []string{"claude", "codex"} {
		test.Run(target, func(test *testing.T) {
			_, source := nativeStoreFixture(test)
			original, err := os.ReadFile(source.Path)
			if err != nil {
				test.Fatal(err)
			}
			text := strings.Repeat("large context ", 224<<10) + "blob:not-an-attachment"
			journal := bytes.ReplaceAll(original, []byte("human request"), []byte(text))
			if len(journal) <= 3<<20 {
				test.Fatal("ordinary fixture did not cross three MiB")
			}
			if err := os.WriteFile(source.Path, journal, 0o600); err != nil {
				test.Fatal(err)
			}
			first, err := CarryOver(source, target)
			if err != nil {
				test.Fatal("large ordinary native conversion:", err)
			}
			body, err := os.ReadFile(first.Path)
			if err != nil || !bytes.Contains(body, []byte(text)) {
				test.Fatal("large native history lost its text:", err)
			}
			Reset()
			native, ok := Find(target, first.ID)
			if !ok {
				test.Fatal("large converted native session not discoverable")
			}
			reversed, err := CarryOver(native, "omp")
			if err != nil {
				test.Fatal("large ordinary native reverse:", err)
			}
			body, err = os.ReadFile(reversed.Path)
			if err != nil || !bytes.Contains(body, []byte(text)) || !bytes.Contains(body, []byte(`"type":"image"`)) {
				test.Fatal("large native reverse lost text or attachments:", err)
			}
			Reset()
			output, ok := Find("omp", reversed.ID)
			if !ok {
				test.Fatal("large reverse output not discoverable")
			}
			if transcript, err := TranscriptOf(output); err != nil || len(transcript.Parts) == 0 {
				test.Fatal("large native reverse transcript unreadable:", err)
			}
			again, err := CarryOver(native, "omp")
			if err != nil || again != reversed {
				test.Fatal("large native reverse receipt was not reused:", err)
			}
			retained, err := os.ReadFile(source.Path)
			if err != nil || !bytes.Equal(retained, journal) {
				test.Fatal("large conversion changed its source:", err)
			}
		})
	}
}

func TestSessionExtensionCodexTargetConfigFailsClosed(test *testing.T) {
	for _, malformed := range []string{"invalid-toml", "dangling-symlink", "directory"} {
		test.Run(malformed, func(test *testing.T) {
			setup(test)
			location := test.TempDir()
			test.Setenv("CODEX_HOME", location)
			name := filepath.Join(location, "config.toml")
			switch malformed {
			case "invalid-toml":
				if err := os.WriteFile(name, []byte("profile = ["), 0o600); err != nil {
					test.Fatal(err)
				}
			case "dangling-symlink":
				if err := os.Symlink(filepath.Join(location, "absent"), name); err != nil {
					test.Skipf("symlinks unavailable: %v", err)
				}
			case "directory":
				if err := os.Mkdir(name, 0o700); err != nil {
					test.Fatal(err)
				}
			}
			descriptor := sdk.SessionDescriptor{To: "codex", OutputDir: ".codex/sessions"}
			if _, err := resolveSessionTargetStore(descriptor); err == nil {
				test.Fatal("unreadable native target config treated as missing/default")
			}
		})
	}
}

func sessionStoreMutationWorker(test *testing.T) string {
	test.Helper()
	worker := test.TempDir()
	sourceCode := `package main
import (
 "context"
 "encoding/json"
 "os"
 "time"
 sdk "github.com/yetone/magpie/pkg/extension"
)
func main() {
 raw, err := os.ReadFile(sdk.ManifestFile)
 if err != nil { panic(err) }
 var manifest sdk.Manifest
 if err := json.Unmarshal(raw, &manifest); err != nil { panic(err) }
 module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
  sdk.MethodSessionConvert: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
   var request sdk.SessionRequest
   if err := json.Unmarshal(call.Params, &request); err != nil { return nil, err }
   id := "changed-destination"
   if os.Args[1] == "hold" {
    id = call.Scope.Session
    if id == "held" {
     if err := os.WriteFile(os.Args[2], []byte("entered"), 0600); err != nil { return nil, err }
     ticker := time.NewTicker(10*time.Millisecond)
     defer ticker.Stop()
     for {
      if _, err := os.Stat(os.Args[3]); err == nil { break }
      select {
      case <-ctx.Done(): return nil, ctx.Err()
      case <-ticker.C:
      }
     }
    }
   } else if os.Args[1] == "symlink" {
    if err := os.Remove(os.Args[2]); err != nil { return nil, err }
    if err := os.Symlink(os.Args[3], os.Args[2]); err != nil { return nil, err }
   } else {
    if err := os.WriteFile(os.Args[2], []byte("model_provider = \"changed\"\n"), 0600); err != nil { return nil, err }
   }
   return sdk.SessionResult{ID: id, Files: []sdk.SessionDocument{
    {Path: id+".jsonl", Format: request.To, Body: []byte("isolated fixture")},
   }}, nil
  },
 }}
 if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil { panic(err) }
}`
	sourceFile := filepath.Join(worker, "main.go")
	if err := os.WriteFile(sourceFile, []byte(sourceCode), 0o600); err != nil {
		test.Fatal(err)
	}
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	command := proc.CommandContext(testenv.DeadlineContext(test, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1", "-o", filepath.Join(worker, entry), sourceFile)
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("build isolated mutation fixture: %v\n%s", err, output)
	}
	return worker
}

func TestSessionExtensionDestinationChangedDuringOrdinaryPlan(test *testing.T) {
	worker := sessionStoreMutationWorker(test)
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	for _, mutation := range []string{"symlink", "provider"} {
		test.Run(mutation, func(test *testing.T) {
			claude, _ := setup(test)
			root := test.TempDir()
			descriptor := sdk.SessionDescriptor{ID: "fixture-target", Name: "Target", From: "claude", To: "omp", OutputDir: ".omp/agent/sessions"}
			args := []string{"symlink", filepath.Join(root, "selected"), filepath.Join(root, "new")}
			oldRoot := filepath.Join(root, "old")
			oldOutput := filepath.Join(oldRoot, "sessions", "changed-destination.jsonl")
			if err := os.MkdirAll(oldRoot, 0o700); err != nil {
				test.Fatal(err)
			}
			if mutation == "symlink" {
				if err := os.MkdirAll(args[2], 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(oldRoot, args[1]); err != nil {
					test.Skipf("symlinks unavailable: %v", err)
				}
				test.Setenv("PI_CODING_AGENT_DIR", args[1])
			} else {
				descriptor.From, descriptor.To, descriptor.OutputDir = "omp", "codex", ".codex/sessions"
				test.Setenv("CODEX_HOME", oldRoot)
				args = []string{"provider", filepath.Join(oldRoot, "config.toml")}
				if err := os.WriteFile(args[1], []byte("model_provider = \"initial\"\n"), 0o600); err != nil {
					test.Fatal(err)
				}
			}
			manifest := sdk.Manifest{Schema: 1, ID: "fixture.target", Version: "1.0.0", Runtime: "exec", Entry: entry, Args: args,
				Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.SessionMigration}},
				Sessions: []sdk.SessionDescriptor{descriptor}}
			raw, err := json.Marshal(manifest)
			if err != nil {
				test.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worker, sdk.ManifestFile), raw, 0o600); err != nil {
				test.Fatal(err)
			}
			sourcePath := filepath.Join(claude, "projects", "selected", "source.jsonl")
			if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
				test.Fatal(err)
			}
			if err := os.WriteFile(sourcePath, []byte("{}\n"), 0o600); err != nil {
				test.Fatal(err)
			}
			host := extension.Default()
			test.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := host.Close(ctx); err != nil {
					test.Error(err)
				}
			})
			if _, err := host.Install(test.Context(), worker); err != nil {
				test.Fatal(err)
			}
			selected := Session{Agent: descriptor.From, ID: "selected", Path: sourcePath, Cwd: "/work/selected"}
			_, err = carryExtension(selected, SessionExtension{Plugin: manifest.ID, Session: descriptor})
			if err == nil {
				test.Fatal("native target changed during planning but ordinary apply succeeded")
			}
			if _, err := os.Stat(oldOutput); !os.IsNotExist(err) {
				test.Fatal("ordinary apply published into its stale native destination:", err)
			}
			if _, err := os.Stat(filepath.Join(root, "new", "sessions", "changed-destination.jsonl")); !os.IsNotExist(err) {
				test.Fatal("ordinary apply published into an uncaptured native destination:", err)
			}
		})
	}
}

func TestSessionExtensionCanceledCarryDoesNotWaitForAnotherMigration(test *testing.T) {
	worker := sessionStoreMutationWorker(test)
	claude, _ := setup(test)
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	control := test.TempDir()
	entered, release := filepath.Join(control, "entered"), filepath.Join(control, "release")
	descriptor := sdk.SessionDescriptor{ID: "waiting", Name: "Waiting", From: "claude", To: "omp", OutputDir: ".fixture/sessions"}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.waiting", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Args:     []string{"hold", entered, release},
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.SessionMigration}},
		Sessions: []sdk.SessionDescriptor{descriptor}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worker, sdk.ManifestFile), raw, 0o600); err != nil {
		test.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".fixture", "sessions"), 0o700); err != nil {
		test.Fatal(err)
	}
	sourcePath := filepath.Join(claude, "projects", "selected", "source.jsonl")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("{}\n"), 0o600); err != nil {
		test.Fatal(err)
	}
	host := extension.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	if _, err := host.Install(test.Context(), worker); err != nil {
		test.Fatal(err)
	}
	route := SessionExtension{Plugin: manifest.ID, Session: descriptor}
	selected := Session{Agent: "claude", ID: "held", Path: sourcePath, Cwd: "/work/selected"}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
	defer cancel()
	go func() {
		_, err := carryExtensionContext(ctx, selected, route)
		done <- err
	}()
	defer func() {
		if err := os.WriteFile(release, []byte("released"), 0o600); err != nil {
			test.Error(err)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			test.Error("held fixture did not stop")
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case err := <-done:
			done <- err
			test.Fatal("held converter exited before its barrier:", err)
		case <-ctx.Done():
			test.Fatal("held converter never reached its barrier")
		case <-ticker.C:
		}
	}
	waiting, cancelWaiting := context.WithCancel(test.Context())
	canceled := make(chan error, 1)
	go func() {
		_, err := carryExtensionContext(waiting, selected, route)
		canceled <- err
	}()
	cancelWaiting()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			test.Fatal("same-key cancellation returned the wrong error:", err)
		}
	case <-time.After(5 * time.Second):
		os.WriteFile(release, []byte("released"), 0o600)
		cancel()
		<-canceled
		test.Fatal("canceled same-key migration waited behind another converter")
	}
	other := selected
	other.ID = "independent"
	completed := make(chan error, 1)
	go func() {
		_, err := carryExtensionContext(ctx, other, route)
		completed <- err
	}()
	select {
	case err := <-completed:
		if err != nil {
			test.Fatal("independent migration failed while another was held:", err)
		}
	case <-time.After(5 * time.Second):
		os.WriteFile(release, []byte("released"), 0o600)
		cancel()
		<-completed
		test.Fatal("independent migration serialized behind unrelated converter")
	}
}

type sessionWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *sessionWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestSessionExtensionMigrationGateCancellation(test *testing.T) {
	release, err := acquireSessionMigration(test.Context(), test.Name())
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	independent, err := acquireSessionMigration(test.Context(), test.Name()+"/independent")
	if err != nil {
		test.Fatal(err)
	}
	independent()
	ctx, cancel := context.WithCancel(test.Context())
	defer cancel()
	observed := &sessionWaitContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		next, err := acquireSessionMigration(observed, test.Name())
		if err == nil {
			next()
		}
		done <- err
	}()
	select {
	case <-observed.waiting:
	case <-time.After(5 * time.Second):
		test.Fatal("same-key waiter did not reach its context-aware wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			test.Fatal("same-key waiter ignored cancellation:", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("same-key waiter was not released by cancellation")
	}
	extensionMigrations.Lock()
	remaining := len(extensionMigrations.inflight)
	extensionMigrations.Unlock()
	if remaining != 1 {
		test.Fatal("canceled or independent migration leaked an in-flight gate:", remaining)
	}
}

func TestSessionExtensionDestinationSnapshotRejectsChanges(test *testing.T) {
	for _, change := range []string{"selection", "directory-identity", "missing-directory", "symlink", "provider-same-mtime"} {
		test.Run(change, func(test *testing.T) {
			setup(test)
			location := filepath.Join(test.TempDir(), "claude")
			test.Setenv("CLAUDE_CONFIG_DIR", location)
			descriptor := sdk.SessionDescriptor{To: "claude", OutputDir: ".claude/projects"}
			if change == "provider-same-mtime" {
				location = filepath.Join(test.TempDir(), "codex")
				test.Setenv("CODEX_HOME", location)
				descriptor = sdk.SessionDescriptor{To: "codex", OutputDir: ".codex/sessions"}
				if err := os.MkdirAll(location, 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(location, "config.toml"), []byte("model_provider = \"one\"\n"), 0o600); err != nil {
					test.Fatal(err)
				}
			} else if change != "missing-directory" {
				if err := os.MkdirAll(filepath.Join(location, "projects"), 0o700); err != nil {
					test.Fatal(err)
				}
			}
			var alias string
			if change == "symlink" {
				alias = location + ".alias"
				if err := os.Symlink(location, alias); err != nil {
					test.Skipf("symlinks unavailable: %v", err)
				}
				test.Setenv("CLAUDE_CONFIG_DIR", alias)
			}
			store, err := resolveSessionTargetStore(descriptor)
			if err != nil {
				test.Fatal(err)
			}
			if err := store.check(test.Context()); err != nil {
				test.Fatal(err)
			}
			switch change {
			case "selection":
				test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(test.TempDir(), "other"))
			case "directory-identity":
				path := filepath.Join(location, "projects")
				if err := os.Rename(path, path+".retained"); err != nil {
					test.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					test.Fatal(err)
				}
			case "missing-directory":
				if _, err := os.Stat(location); !os.IsNotExist(err) {
					test.Fatal("binding/preview created a target directory")
				}
				if err := os.MkdirAll(filepath.Join(location, "projects"), 0o700); err != nil {
					test.Fatal(err)
				}
			case "symlink":
				other := filepath.Join(test.TempDir(), "other")
				if err := os.MkdirAll(filepath.Join(other, "projects"), 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.Remove(alias); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(other, alias); err != nil {
					test.Fatal(err)
				}
			case "provider-same-mtime":
				path := filepath.Join(location, "config.toml")
				info, _ := os.Stat(path)
				if err := os.WriteFile(path, []byte("model_provider = \"two\"\n"), 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					test.Fatal(err)
				}
			}
			if err := store.check(test.Context()); err == nil {
				test.Fatal("changed native destination remained authorized")
			}
		})
	}
}

func TestSessionExtensionOMPProfileSelectionMatchesRecoveredStorage(test *testing.T) {
	setup(test)
	home, _ := os.UserHomeDir()
	for _, name := range []string{"PI_CONFIG_DIR", "PI_CODING_AGENT_DIR", "OMP_PROFILE", "PI_PROFILE", "XDG_DATA_HOME"} {
		test.Setenv(name, "")
	}
	data := test.TempDir()
	test.Setenv("XDG_DATA_HOME", data)
	if err := os.MkdirAll(filepath.Join(data, "omp"), 0o700); err != nil {
		test.Fatal(err)
	}
	test.Setenv("OMP_PROFILE", "work")
	path, profile, err := extensionOMPStore()
	if err != nil || profile != "work" || path != filepath.Join(home, ".omp", "profiles", "work", "agent", "sessions") {
		test.Fatal("base XDG root incorrectly moved a named profile", path, err)
	}
	if err := os.MkdirAll(filepath.Join(data, "omp", "profiles", "work"), 0o700); err != nil {
		test.Fatal(err)
	}
	path, _, err = extensionOMPStore()
	if err != nil || path != filepath.Join(data, "omp", "profiles", "work", "sessions") {
		test.Fatal("profile-specific XDG root not selected", path, err)
	}
	for _, invalid := range []string{"../escape", "con", "com0.log", "bad.", "Upper"} {
		test.Setenv("OMP_PROFILE", invalid)
		if _, _, err := extensionOMPStore(); err == nil {
			test.Fatalf("invalid native profile accepted: %q", invalid)
		}
	}
	test.Setenv("OMP_PROFILE", "")
	test.Setenv("PI_CODING_AGENT_DIR", "relative-agent")
	if _, _, err := extensionOMPStore(); err == nil {
		test.Fatal("relative target override silently selected the default store")
	}
	test.Setenv("OMP_PROFILE", "work")
	if _, _, err := extensionOMPStore(); err != nil {
		test.Fatal("named profile incorrectly inherited the default agent override:", err)
	}
}

func TestSessionExtensionOMPProfileResolvesSymlinkedStore(test *testing.T) {
	setup(test)
	test.Setenv("PI_CONFIG_DIR", ".custom-omp")
	test.Setenv("XDG_DATA_HOME", "")
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	alias := filepath.Join(home, ".custom-omp")
	if err := os.Symlink(directory, alias); err != nil {
		test.Skipf("symlinks unavailable: %v", err)
	}
	relative := filepath.Join("profiles", "work", "agent", "sessions", "bucket", "time_id.jsonl")
	physical := filepath.Join(directory, relative)
	if err := os.MkdirAll(filepath.Dir(physical), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte("{}"), 0o600); err != nil {
		test.Fatal(err)
	}
	for _, path := range []string{physical, filepath.Join(alias, relative)} {
		if profile := extensionOMPProfileForPath(path); profile != "work" {
			test.Fatal("native profile lost through its configured symlink:", path, profile)
		}
	}
}
