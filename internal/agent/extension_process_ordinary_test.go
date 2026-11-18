package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	if slices.Contains(os.Args, "--owned-agent-process-child") {
		directory, err := os.Getwd()
		if err != nil {
			os.Exit(3)
		}
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"directory": directory, "argv": os.Args[1:], "config": os.Getenv("OPENCODE_CONFIG")}) != nil {
			os.Exit(4)
		}
		os.Exit(0)
	}
}

const ownedAgentModuleProgram = `
package main
import(
 "context"
 "encoding/json"
 "errors"
 "os"
 sdk "github.com/yetone/magpie/pkg/extension"
)
func main(){
 raw,err:=os.ReadFile(sdk.ManifestFile);if err!=nil{panic(err)}
 var manifest sdk.Manifest;if json.Unmarshal(raw,&manifest)!=nil{panic("invalid manifest")}
 module:=sdk.Module{Manifest:manifest,Handlers:map[string]sdk.Handler{
  sdk.MethodAgentInspect:func(_ context.Context,call sdk.Call,_ *sdk.Stream)(any,error){
   var request sdk.AgentInspectRequest
   if json.Unmarshal(call.Params,&request)!=nil||request.Binding==nil{return nil,errors.New("missing host binding")}
   return sdk.AgentInspectResult{Connected:request.Binding.Connected,Values:request.Binding.Values},nil
  },
  sdk.MethodAgentPlan:func(_ context.Context,call sdk.Call,_ *sdk.Stream)(any,error){
   var request sdk.AgentPlanRequest
   if json.Unmarshal(call.Params,&request)!=nil||request.Binding==nil{return nil,errors.New("missing host binding")}
   return sdk.AgentPlan{Binding:&sdk.AgentBindingChange{Revision:request.Binding.Revision,Connected:request.Action!="disconnect",Values:request.Binding.Values}},nil
  },
 },Actions:map[string]sdk.Handler{"run":func(ctx context.Context,call sdk.Call,stream *sdk.Stream)(any,error){
  if err:=stream.Emit(ctx,"accepted",sdk.AcceptedEvent{OperationID:call.Scope.Operation});err!=nil{return nil,err}
  var opened sdk.ProcessOpenResult
  if err:=stream.HostCall(ctx,sdk.MethodProcessOpen,sdk.ProcessOpenRequest{RecipeID:"run"},&opened);err!=nil{return nil,err}
  var body []byte
  for seq:=uint64(1);;seq++{
   sequence,_:=json.Marshal(seq)
   var read sdk.ProcessReadResult
   if err:=stream.HostCall(ctx,sdk.MethodProcessRead,sdk.ProcessReadRequest{Handle:opened.Handle,Pipe:"stdout",Seq:string(sequence),MaxBytes:65536},&read);err!=nil{return nil,err}
   body=append(body,read.Bytes...);if read.EOF{break}
  }
  var child any;if json.Unmarshal(body,&child)!=nil{return nil,errors.New("invalid child proof")}
  var closed sdk.ProcessCloseResult
  if err:=stream.HostCall(ctx,sdk.MethodProcessClose,sdk.ProcessHandleRequest{Handle:opened.Handle},&closed);err!=nil||!closed.Joined{return nil,errors.New("child did not join")}
  return map[string]any{"outcome":"completed","child":child},nil
 }}}
 if sdk.Serve(context.Background(),os.Stdin,os.Stdout,module)!=nil{os.Exit(2)}
}
`

func TestOwnedProcessOrdinaryInstalledActionUsesRegisteredAgentBinding(t *testing.T) {
	for _, mode := range []string{"plain", "inherited", "explicit"} {
		t.Run(mode, func(t *testing.T) { ownedProcessInstalledAction(t, mode) })
	}
}

func ownedProcessInstalledAction(t *testing.T, mode string) {
	home := t.TempDir()
	for _, variable := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(variable, home)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	t.Setenv("SHELL", "/bin/bash")
	for _, name := range []string{".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binName := "owned-process-agent"
	moduleName := "primary"
	if runtime.GOOS == "windows" {
		binName += ".exe"
		moduleName += ".exe"
	}
	if err := os.Symlink(executable, filepath.Join(bin, binName)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	descriptor := sdk.AgentDescriptor{ID: "owned-process-agent", Name: "Synthetic owned process", Bin: binName, Launch: &sdk.AgentLaunch{}}
	config := ""
	if mode != "plain" {
		descriptor.Profile = &sdk.AgentProfile{Target: "opencode"}
		descriptor.Launch.Env = map[string]string{"OPENCODE_CONFIG": "{config}"}
		config = filepath.Join(home, "config", "opencode", "opencode.json")
		if mode == "explicit" {
			descriptor.ConfigFiles = []string{".custom config/opencode.json"}
			config = filepath.Join(home, filepath.FromSlash(descriptor.ConfigFiles[0]))
			// A declared file is part of the process observation set. Supply
			// its real parent and content, as a configured client would.
			if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte(`{"personal":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	manifest := sdk.Manifest{
		Schema: 1, ID: "owned-process.agent", Version: "1.0.0", Runtime: "exec", Entry: moduleName,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 10},
			Capabilities: []sdk.Capability{sdk.AgentWiring, sdk.ModuleRPC, sdk.AgentProcess}},
		Actions: []sdk.ActionDescriptor{{ID: "run", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Agents:  []sdk.AgentDescriptor{descriptor},
		Processes: []sdk.AgentProcessRecipe{{ID: "run", Agent: descriptor.ID,
			Argv: []string{"--owned-agent-process-child", `';$(literal-only)`}}},
	}
	source := t.TempDir()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "module.go")
	if err := os.WriteFile(program, []byte(ownedAgentModuleProgram), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=2",
		"-o", filepath.Join(source, moduleName), program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build ordinary synthetic module: %v\n%s", err, output)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(ctx, source); err != nil {
		t.Fatal(err)
	}
	owned, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: manifest.ID, Operation: "ordinary-before-connect", Attempt: 1}
	request := sdk.ModuleCallRequest{Action: "run", Input: json.RawMessage(`{"directory":"/untrusted-input-is-not-cwd"}`)}
	if _, err := manager.Call(ctx, manifest.ID, sdk.MethodModuleCall, scope, request, nil); err == nil {
		t.Fatal("ordinary dispatch granted a disconnected agent profile")
	}
	if _, err := owned.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	start := owned.Start()
	if start.Err != nil || !start.Active || config != "" && !slices.Contains(start.Env, "OPENCODE_CONFIG="+config) {
		t.Fatalf("ordinary launch did not preserve its configuration: %+v", start)
	}
	scope.Operation = "ordinary-connected"
	raw, err := manager.Call(ctx, manifest.ID, sdk.MethodModuleCall, scope, request, nil)
	if err != nil {
		t.Fatal("ordinary installed action did not use the registered production agent resolver", err, errors.Unwrap(err))
	}
	var result struct {
		Outcome string `json:"outcome"`
		Child   struct {
			Directory string   `json:"directory"`
			Config    string   `json:"config"`
			Argv      []string `json:"argv"`
		} `json:"child"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Outcome != "completed" ||
		result.Child.Config != config || !slices.Equal(result.Child.Argv, manifest.Processes[0].Argv) {
		t.Fatalf("ordinary process replaced host cwd or literal installed arguments: %s", raw)
	}
	expectedDirectory, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	actualDirectory, err := os.Stat(result.Child.Directory)
	if err != nil || !filepath.IsAbs(result.Child.Directory) || !os.SameFile(expectedDirectory, actualDirectory) {
		t.Fatalf("ordinary process replaced host cwd: got %q, want directory %q: %v", result.Child.Directory, home, err)
	}
	journal, err := manager.Store().Operation(manifest.ID, scope.Operation)
	if err != nil || journal.State != "completed" {
		t.Fatal("ordinary registered owner lost its terminal journal", journal, err)
	}
}
