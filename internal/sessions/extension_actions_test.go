package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sessionActionHost struct {
	entries      []extension.Installed
	request      sdk.SessionRequest
	scope        sdk.Scope
	method       string
	duringInvoke func()
}

func TestSessionExtensionOrdinaryCarryOver(test *testing.T) {
	claude, _ := setup(test)
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		test.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		test.Fatal(err)
	}
	// Route discovery uses the ordinary availability seam.
	previous := agentThere
	agentThere = func(target string) bool { return target == "omp" }
	test.Cleanup(func() { agentThere = previous })
	source := filepath.Join(claude, "projects", "-work", "source-id.jsonl")
	original := []byte("{\"type\":\"user\",\"uuid\":\"message\",\"sessionId\":\"source-id\",\"cwd\":\"/work\",\"timestamp\":\"2026-11-12T12:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(source, original, 0o600); err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1", "-o",
		filepath.Join(directory, entry), "../extension/testdata/actionworker")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		test.Fatalf("build action fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.sessions", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.SessionMigration}},
		Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation", From: "claude", To: "omp", OutputDir: ".fixture/sessions"}}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		test.Fatal(err)
	}
	host := extension.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	if _, err := host.Install(ctx, directory); err != nil {
		test.Fatal(err)
	}
	session, exists := Find("claude", "source-id")
	if !exists {
		test.Fatal("ordinary native session discovery failed")
	}
	if !slices.ContainsFunc(session.Carry, func(c Carry) bool { return c.Agent == "omp" && c.Convert }) {
		test.Fatalf("installed converter absent from ordinary carries: %+v", session.Carry)
	}
	// Another legitimate native store takes over lookup of the same ID after
	// the user selected A. CarryOver must keep A's document and Cwd together.
	otherRoot := test.TempDir()
	otherPath := filepath.Join(otherRoot, "projects", "-different-work", "source-id.jsonl")
	otherBody := bytes.ReplaceAll(original, []byte("/work"), []byte("/different-work"))
	if err := os.MkdirAll(filepath.Dir(otherPath), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(otherPath, otherBody, 0o600); err != nil {
		test.Fatal(err)
	}
	test.Setenv("CLAUDE_CONFIG_DIR", otherRoot)
	current, exists := Find("claude", "source-id")
	if !exists || current.Path != otherPath || current.Cwd != "/different-work" {
		test.Fatalf("fixture did not change native lookup: %+v", current)
	}
	output := filepath.Join(home, ".fixture", "sessions", "converted-session.jsonl")
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("listing executed converter")
	}
	result, err := CarryOver(session, "omp")
	command := "cd '/work' && omp --resume converted-session"
	if runtime.GOOS == "windows" {
		command = "Set-Location -LiteralPath '/work'; omp --resume converted-session"
	}
	if err != nil || result.ID != "converted-session" || result.Path != output || result.Command != command {
		test.Fatalf("ordinary SDK CarryOver: %+v %v", result, err)
	}
	body, err := os.ReadFile(output)
	var converted struct {
		Source  []sdk.SessionDocument `json:"source"`
		Options struct {
			Cwd string `json:"cwd"`
		} `json:"options"`
	}
	if err != nil || json.Unmarshal(body, &converted) != nil || len(converted.Source) != 1 ||
		!bytes.Equal(converted.Source[0].Body, original) || converted.Options.Cwd != "/work" {
		test.Fatalf("converter did not receive native source/context: %s %v", body, err)
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(after, original) {
		test.Fatal("ordinary conversion changed its source")
	}
	otherAfter, _ := os.ReadFile(otherPath)
	if !bytes.Equal(otherAfter, otherBody) {
		test.Fatal("ordinary conversion changed the other store's source")
	}
	// Publishing a second owner for the route must revoke ordinary dispatch.
	manifest.Version = "1.0.1"
	manifest.Sessions = append(manifest.Sessions, sdk.SessionDescriptor{ID: "ambiguous", Name: "Ambiguous",
		From: "claude", To: "omp", OutputDir: ".fixture/other"})
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		test.Fatal(err)
	}
	if _, err := host.Install(ctx, directory); err != nil {
		test.Fatal(err)
	}
	if _, err := CarryOver(session, "omp"); err == nil {
		test.Fatal("ambiguous installed route was selected")
	}
	if err := host.SetEnabled(test.Context(), manifest.ID, false); err != nil {
		test.Fatal(err)
	}
	if _, err := CarryOver(session, "omp"); err == nil {
		test.Fatal("disabled converter still dispatched")
	}
}

func (host *sessionActionHost) List() ([]extension.Installed, error) { return host.entries, nil }
func (host *sessionActionHost) Store() *extension.Store              { return nil }
func (host *sessionActionHost) Manifest(string) (sdk.Manifest, error) {
	return host.entries[0].Manifest, nil
}
func (host *sessionActionHost) Invoke(_ context.Context, _ string, method string, scope sdk.Scope, params, out any, _ func(extension.Event) error) error {
	host.request, host.scope, host.method = params.(sdk.SessionRequest), scope, method
	if host.duringInvoke != nil {
		host.duringInvoke()
	}
	body, err := json.Marshal(sdk.SessionResult{Files: []sdk.SessionDocument{{Path: "converted.jsonl", Format: "external", Body: []byte("converted")}}})
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func TestSessionExtensionSelectedSourceGuard(test *testing.T) {
	for _, phase := range []string{"during-plan", "before-apply"} {
		for _, mutation := range []string{"contents", "identity"} {
			test.Run(phase+"/"+mutation, func(test *testing.T) {
				setup(test)
				sourcePath := filepath.Join(test.TempDir(), "source.jsonl")
				original := []byte("native-original")
				if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
					test.Fatal(err)
				}
				info, err := os.Stat(sourcePath)
				if err != nil {
					test.Fatal(err)
				}
				mutate := func() {
					test.Helper()
					body := bytes.Clone(original)
					if mutation == "identity" {
						// Keep the original inode allocated, so identity cannot
						// be reused when the replacement has identical bytes.
						if err := os.Rename(sourcePath, sourcePath+".original"); err != nil {
							test.Fatal(err)
						}
					} else {
						body[0] = 'X'
					}
					if err := os.WriteFile(sourcePath, body, 0o600); err != nil {
						test.Fatal(err)
					}
					if err := os.Chtimes(sourcePath, info.ModTime(), info.ModTime()); err != nil {
						test.Fatal(err)
					}
				}
				host := &sessionActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{
					ID: "external", Version: "1.0.0", Contract: sdk.FullContract(),
					Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}},
				}}}}
				if phase == "during-plan" {
					host.duringInvoke = mutate
				}
				root := test.TempDir()
				policy := extension.ActionPolicy{Root: root, Paths: []string{"converted.jsonl"},
					BackupDir: filepath.Join(test.TempDir(), "backups")}
				selected := Session{Agent: "claude", ID: "selected", Path: sourcePath, Cwd: "/selected-work"}
				_, snapshot, err := convertExtensionSource(test.Context(), host, "external", "conversation", selected,
					"external", sdk.Scope{}, nil, policy)
				if phase == "before-apply" {
					if err != nil {
						test.Fatal(err)
					}
					mutate()
					err = snapshot.check(test.Context())
				}
				if !errors.Is(err, extension.ErrActionChanged) {
					test.Fatalf("source %s change at %s was accepted: %v", mutation, phase, err)
				}
				if _, err := os.Stat(filepath.Join(root, "converted.jsonl")); !errors.Is(err, os.ErrNotExist) {
					test.Fatal("source guard failure published output")
				}
				if _, err := os.Stat(policy.BackupDir); !errors.Is(err, os.ErrNotExist) {
					test.Fatal("source guard failure created a transaction")
				}
			})
		}
	}
}

func TestSessionExtensionConvertsFoundNativeSource(test *testing.T) {
	claude, _ := setup(test)
	source := filepath.Join(claude, "projects", "-work", "source-id.jsonl")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		test.Fatal(err)
	}
	original := []byte("{\"type\":\"user\",\"uuid\":\"message\",\"sessionId\":\"source-id\",\"cwd\":\"/work\",\"timestamp\":\"2026-11-12T12:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		test.Fatal(err)
	}
	host := &sessionActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{ID: "external", Version: "1.0.0", Contract: sdk.FullContract(),
		Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}}}}}}
	converters, err := SessionExtensions(host)
	if err != nil || len(converters) != 1 || converters[0].Plugin != "external" {
		test.Fatalf("converter discovery: %+v, %v", converters, err)
	}
	root := test.TempDir()
	policy := extension.ActionPolicy{Root: root, Paths: []string{"converted.jsonl"}, BackupDir: filepath.Join(test.TempDir(), "backups")}
	scope := sdk.Scope{Plugin: "external", Operation: "conversion", Attempt: 2}
	plan, err := ConvertExtension(context.Background(), host, "external", "conversation", "claude", "source-id", "external", scope, nil, policy)
	if err != nil {
		test.Fatal(err)
	}
	scope.Session = "source-id"
	if host.scope != scope || host.method != sdk.MethodSessionConvert || host.request.From != "claude" ||
		!bytes.Equal(host.request.Files[0].Body, original) {
		test.Fatalf("native conversion contract: %+v, %+v", host.scope, host.request)
	}
	if _, err := os.Stat(filepath.Join(root, "converted.jsonl")); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("conversion wrote before confirmation")
	}
	if _, err := plan.Execute(context.Background(), plan.Preview().Revision); err != nil {
		test.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, original) {
		test.Fatalf("native source changed: %q, %v", after, err)
	}
	result, err := os.ReadFile(filepath.Join(root, "converted.jsonl"))
	if err != nil || string(result) != "converted" {
		test.Fatalf("converted document: %q, %v", result, err)
	}
	host.method = ""
	scope.Session = "another-session"
	if _, err := ConvertExtension(context.Background(), host, "external", "conversation", "claude", "source-id", "external", scope, nil, policy); err == nil || host.method != "" {
		test.Fatal("another parent session reached the converter")
	}
}

func TestSessionExtensionRespectsReadConsent(test *testing.T) {
	setup(test)
	if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
		return &settings.SessionReads{Asked: true}
	}); err != nil {
		test.Fatal(err)
	}
	host := &sessionActionHost{}
	if _, err := ConvertExtension(context.Background(), host, "external", "conversation", "claude", "source-id", "external", sdk.Scope{}, nil, extension.ActionPolicy{}); !errors.Is(err, ErrNotRead) {
		test.Fatalf("read consent was bypassed: %v", err)
	}
	if host.method != "" {
		test.Fatal("a private session reached the converter")
	}
}
