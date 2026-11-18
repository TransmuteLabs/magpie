package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/sessions"
)

type resumeCLIFixture struct {
	host             *extension.Manager
	home, cwd, owner string
	id, journal      string
	config, markers  string
	installed        extension.Installed
}

func resumeCLISource(t *testing.T, family string) *resumeCLIFixture {
	t.Helper()
	distribution, executable := os.Getenv("MAGPIE_RESUME_TEST_DIST"), os.Getenv("MAGPIE_RESUME_TEST_AGENT")
	if distribution == "" || executable == "" {
		t.Skip("requires explicitly built synthetic SDK resume package and fixture executable")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native storage fixture requires Linux or Darwin")
	}
	home := helpHome(t)
	cwd := filepath.Join(home, "selected work's directory")
	markers := filepath.Join(home, "helper-markers")
	for _, name := range []string{cwd, filepath.Join(home, "bin"), filepath.Join(home, ".fixture-resume/jsonl"),
		filepath.Join(home, ".fixture-resume/storage"), markers} {
		if err := os.MkdirAll(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	owner := "fixture-" + family + "-resume"
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("FIXTURE_RESUME_URL", "personal")
	t.Setenv("FIXTURE_RESUME_KEY", "personal")
	t.Setenv("FIXTURE_RESUME_CONFIG", "personal")
	t.Setenv("FIXTURE_RESUME_PERSONAL", "keep")
	t.Setenv("FIXTURE_RESUME_MARKERS", markers)
	t.Setenv("PATH", filepath.Join(home, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("# personal\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"jsonl", "storage"} {
		if err := os.WriteFile(filepath.Join(home, "bin", "fixture-"+kind+"-resume"), body, 0o700); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(home, ".fixture-resume", kind+"-config.json")
		if err := os.WriteFile(config, []byte(`{"model":"personal","note":"preserve personal config"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &resumeCLIFixture{home: home, cwd: cwd, owner: owner, id: "session's ; $text", markers: markers,
		config: filepath.Join(home, ".fixture-resume", family+"-config.json")}
	if family == "jsonl" {
		fixture.id = "journal-session"
		fixture.journal = filepath.Join(home, ".fixture-resume/jsonl", fixture.id+".jsonl")
	} else {
		fixture.journal = filepath.Join(home, ".fixture-resume/storage/history.json")
	}
	fixture.write(t, fixture.id, cwd)
	host := extension.Default()
	fixture.host = host
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := extension.Default().Close(ctx); err != nil {
			t.Error(err)
		}
	})
	fixture.installed, err = host.Install(t.Context(), distribution)
	if err != nil {
		t.Fatal(err)
	}
	config, err := host.ModuleConfig(t.Context(), "fixture.source-resume")
	if err != nil {
		t.Fatal(err)
	}
	values, _ := json.Marshal(map[string]string{"markers": markers})
	if _, err := host.CompareAndSwapModuleConfig(t.Context(), "fixture.source-resume", config.Version, values); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetReads(nil, map[string]bool{owner: true}); err != nil {
		t.Fatal(err)
	}
	if family == "storage" {
		roots, _, err := sessions.InspectStorageSources(t.Context())
		if err != nil || len(roots) != 1 {
			t.Fatal("synthetic storage root was not inspectable", roots, err)
		}
		root := roots[0]
		if err := sessions.BindStorageSource(t.Context(), root.Plugin, root.Source, root.RootSlot, root.RootRef, root.Revision); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := agent.Find(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selected.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *resumeCLIFixture) write(t *testing.T, id, cwd string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"id": id, "cwd": cwd, "title": "synthetic resume",
		"at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journal, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *resumeCLIFixture) arguments(t *testing.T) []string {
	t.Helper()
	if err := sessions.PrepareSessionSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	selected, ok := sessions.Find(fixture.owner, fixture.id)
	if !ok || selected.Resume == "" {
		t.Fatal("ordinary source row lost its owned resume command", selected)
	}
	_, token, found := strings.Cut(selected.Resume, " --expected ")
	token = strings.Trim(token, "'")
	if !found || len(token) != 64 {
		t.Fatal("displayed resume command has no selection identity", selected.Resume)
	}
	return []string{fixture.owner, fixture.id, "--expected", token}
}

func TestSessionResumeOrdinaryOwnedLaunch(t *testing.T) {
	for _, family := range []string{"jsonl", "storage"} {
		t.Run(family, func(t *testing.T) {
			fixture := resumeCLISource(t, family)
			arguments := fixture.arguments(t)
			before, err := os.ReadFile(fixture.config)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			originalExec := startExec
			t.Cleanup(func() { startExec = originalExec })
			calls := 0
			startExec = func(path, name string, args, env []string) error {
				calls++
				if name != fixture.owner || path != filepath.Join(fixture.home, "bin", fixture.owner) {
					t.Fatal("resume escaped its actual SDK executable", path, name)
				}
				command := exec.CommandContext(t.Context(), path, args...)
				command.Env = env
				body, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("actual fixture launch: %v\n%s", err, body)
				}
				var result struct {
					PID     int               `json:"pid"`
					Cwd     string            `json:"cwd"`
					Args    []string          `json:"args"`
					Env     map[string]string `json:"env"`
					Config  map[string]string `json:"config"`
					Stopped int               `json:"stopped"`
				}
				if err := json.Unmarshal(body, &result); err != nil {
					t.Fatal(err, string(body))
				}
				want := []string{"--config", fixture.config, "--mode", "sdk", "--model", "magpie/fixture-resume",
					"resume", "--exact", fixture.id}
				if result.PID <= 0 || result.PID == os.Getpid() || result.Cwd != fixture.cwd ||
					!slices.Equal(result.Args, want) || result.Stopped < 1 ||
					result.Env["FIXTURE_RESUME_URL"] != gateway.URL() ||
					result.Env["FIXTURE_RESUME_KEY"] == "" || result.Env["FIXTURE_RESUME_KEY"] == "personal" ||
					result.Env["FIXTURE_RESUME_CONFIG"] != fixture.config ||
					result.Env["FIXTURE_RESUME_PERSONAL"] != "keep" ||
					result.Config["gateway"] != result.Env["FIXTURE_RESUME_URL"] ||
					result.Config["key"] != result.Env["FIXTURE_RESUME_KEY"] ||
					result.Config["note"] != "preserve personal config" {
					t.Fatalf("ordinary SDK launch lost config/env/cwd/argv or helper shutdown: %+v", result)
				}
				return nil
			}
			if err := run(append([]string{"sessions", "resume"}, arguments...)); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(fixture.config)
			if err != nil || string(after) != string(before) {
				t.Fatal("resume rewrote the connected profile", err)
			}
			if cwd, err := os.Getwd(); err != nil || cwd != previous || calls != 1 {
				t.Fatal("resume did not restore cwd after returned exec", cwd, calls, err)
			}
		})
	}
}

func TestSessionResumeFinalBoundaryRefusesChangedAuthority(t *testing.T) {
	for _, family := range []string{"jsonl", "storage"} {
		for _, change := range []string{"cancel", "reads", "off", "config", "package", "root", "profile", "shell", "source", "storage-consent"} {
			if family == "storage" && change == "source" {
				continue
			}
			if family == "jsonl" && change == "storage-consent" {
				continue
			}
			t.Run(family+"/"+change, func(t *testing.T) {
				fixture := resumeCLISource(t, family)
				arguments := fixture.arguments(t)
				originalExec, originalLook := startExec, startLook
				t.Cleanup(func() { startExec, startLook = originalExec, originalLook })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				startExec = func(string, string, []string, []string) error {
					t.Fatal("changed owner reached final exec", change)
					return nil
				}
				looked := false
				startLook = func(name string) (string, error) {
					path, err := originalLook(name)
					if err != nil {
						return path, err
					}
					looked = true
					started, err := filepath.Glob(filepath.Join(fixture.markers, "*.started"))
					if err != nil || len(started) == 0 {
						t.Fatal("missing actual helper startup", err)
					}
					for _, marker := range started {
						if _, err := os.Stat(strings.TrimSuffix(marker, ".started") + ".stopped"); err != nil {
							t.Fatal("helper was still active at final PATH lookup", err)
						}
					}
					switch change {
					case "cancel":
						cancel()
					case "reads":
						if err := sessions.SetReads(nil, map[string]bool{fixture.owner: false}); err != nil {
							t.Fatal(err)
						}
					case "off":
						if err := extension.Default().SetEnabled(t.Context(), "fixture.source-resume", false); err != nil {
							t.Fatal(err)
						}
					case "config":
						host := extension.Default()
						config, err := host.ModuleConfig(t.Context(), "fixture.source-resume")
						if err != nil {
							t.Fatal(err)
						}
						values, _ := json.Marshal(map[string]any{"markers": fixture.markers, "changed": true})
						if _, err := host.CompareAndSwapModuleConfig(t.Context(), "fixture.source-resume", config.Version, values); err != nil {
							t.Fatal(err)
						}
					case "package":
						if err := extension.Default().Remove(t.Context(), "fixture.source-resume"); err != nil {
							t.Fatal(err)
						}
					case "root":
						root := filepath.Dir(fixture.journal)
						if err := os.Rename(root, root+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(root, 0o700); err != nil {
							t.Fatal(err)
						}
						body, err := os.ReadFile(filepath.Join(root+".old", filepath.Base(fixture.journal)))
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(fixture.journal, body, 0o600); err != nil {
							t.Fatal(err)
						}
					case "profile":
						if err := os.WriteFile(fixture.config, []byte(`{"model":"personal","note":"edited"}`), 0o600); err != nil {
							t.Fatal(err)
						}
					case "shell":
						if err := os.WriteFile(filepath.Join(fixture.home, ".bashrc"), []byte("# replaced\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					case "source":
						fixture.write(t, fixture.id, fixture.cwd)
					case "storage-consent":
						_, consents, err := sessions.InspectStorageSources(t.Context())
						if err != nil || len(consents) != 1 {
							t.Fatal("missing selected storage consent", err)
						}
						if err := sessions.UnbindStorageSource(t.Context(), fixture.owner, consents[0].Identity); err != nil {
							t.Fatal(err)
						}
					}
					return path, nil
				}
				previous, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				err = sessionResumeIn(ctx, arguments)
				if err == nil || !looked {
					t.Fatal("final admission did not observe changed source/SDK owner", change, looked, err)
				}
				if change == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("final admission discarded caller cancellation", err)
				}
				if cwd, err := os.Getwd(); err != nil || cwd != previous {
					t.Fatal("rejected exec left the process in selected cwd", cwd, err)
				}
			})
		}
	}
}

func TestSessionResumeNeverFallsBackToPlainLaunch(t *testing.T) {
	for _, family := range []string{"jsonl", "storage"} {
		for _, refusal := range []string{"disconnected", "plain", "missing", "cwd", "root"} {
			t.Run(family+"/"+refusal, func(t *testing.T) {
				fixture := resumeCLISource(t, family)
				arguments := fixture.arguments(t)
				switch refusal {
				case "disconnected":
					selected, err := agent.Find(fixture.owner)
					if err != nil {
						t.Fatal(err)
					}
					if err := selected.Disconnect(); err != nil {
						t.Fatal(err)
					}
				case "plain":
					if family == "jsonl" {
						old := fixture.journal
						fixture.journal = filepath.Join(filepath.Dir(old), "login.jsonl")
						if err := os.Rename(old, fixture.journal); err != nil {
							t.Fatal(err)
						}
					}
					fixture.id = "login"
					fixture.write(t, fixture.id, fixture.cwd)
					arguments = fixture.arguments(t)
				case "missing":
					if err := os.Remove(filepath.Join(fixture.home, "bin", fixture.owner)); err != nil {
						t.Fatal(err)
					}
				case "cwd":
					fixture.write(t, fixture.id, "relative/directory")
					arguments = fixture.arguments(t)
				case "root":
					root := filepath.Dir(fixture.journal)
					if err := os.Rename(root, root+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(root, 0o700); err != nil {
						t.Fatal(err)
					}
					body, err := os.ReadFile(filepath.Join(root+".old", filepath.Base(fixture.journal)))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(fixture.journal, body, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				originalExec := startExec
				t.Cleanup(func() { startExec = originalExec })
				startExec = func(string, string, []string, []string) error {
					t.Fatal("unsupported resume fell back to an unrelated/plain launch", refusal)
					return nil
				}
				if err := sessionResumeIn(t.Context(), arguments); err == nil {
					t.Fatal("unsupported resume was admitted", refusal)
				}
			})
		}
	}
}

func TestSessionResumeRefusesStaleDisplayedSource(t *testing.T) {
	for _, family := range []string{"jsonl", "storage"} {
		t.Run(family, func(t *testing.T) {
			fixture := resumeCLISource(t, family)
			arguments := fixture.arguments(t)
			fixture.write(t, fixture.id, filepath.Join(fixture.home, "other-cwd"))
			originalExec, originalLook := startExec, startLook
			t.Cleanup(func() { startExec, startLook = originalExec, originalLook })
			startExec = func(string, string, []string, []string) error {
				t.Fatal("stale displayed source reached exec")
				return nil
			}
			startLook = func(string) (string, error) {
				t.Fatal("stale displayed source reached executable lookup")
				return "", nil
			}
			if err := sessionResumeIn(t.Context(), arguments); !errors.Is(err, extension.ErrActionChanged) {
				t.Fatal("stale displayed token retained source selection", err)
			}
		})
	}
}

func TestSessionResumeRequiresFiniteCommand(t *testing.T) {
	helpHome(t)
	for _, args := range [][]string{nil, {"agent"}, {"agent", "id"}, {"agent", "id", "--expected"},
		{"agent", "id", "--token", "value"}, {"agent", "id", "--expected", "value", "extra"}} {
		if err := sessionResumeIn(t.Context(), args); err == nil || err.Error() != sessionResumeUsage {
			t.Fatal("unbounded resume command was admitted", args, err)
		}
	}
	for _, token := range []string{"", strings.Repeat("A", 64), strings.Repeat("x", 64)} {
		if err := sessionResumeIn(t.Context(), []string{"agent", "id", "--expected", token}); err == nil {
			t.Fatal("resume accepted an invalid source token", token)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sessionResumeIn(ctx, []string{"agent", "id", "--expected", strings.Repeat("a", 64)}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled caller began source acquisition", err)
	}
	var empty *agent.ExtensionResumeStart
	if !errors.Is(empty.Recheck(t.Context()), extension.ErrActionChanged) {
		t.Fatal("missing agent preparation retained authority")
	}
	var source *sessions.ExtensionSourceResume
	if !errors.Is(source.Recheck(t.Context()), extension.ErrActionChanged) {
		t.Fatal("missing source preparation retained authority")
	}
}
