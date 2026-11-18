//go:build cgo && (darwin || linux || freebsd || windows)

package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

const ownedModuleProgram = `
package main
import (
 "context"
 "encoding/json"
 "errors"
 "os"
 "sync"
 sdk "github.com/yetone/magpie/pkg/extension"
 NATIVE_IMPORT
)
var declaration = DECLARATION
func module() sdk.Module {
 var manifest sdk.Manifest
 if json.Unmarshal([]byte(declaration), &manifest) != nil { panic("invalid fixture manifest") }
 unsupported := func(context.Context, sdk.Call, *sdk.Stream)(any,error) {
  return nil, errors.New("unused synthetic method")
 }
 return sdk.Module{Manifest:manifest, Handlers:map[string]sdk.Handler{
  sdk.MethodProviderExecute:unsupported, sdk.MethodProviderUsage:unsupported, sdk.MethodAgentPlan:unsupported,sdk.MethodAgentInspect:unsupported,
 }, Actions:map[string]sdk.Handler{"run":run,"other":run}}
}
func run(ctx context.Context, call sdk.Call, stream *sdk.Stream)(any,error) {
 callbackCtx,stop:=context.WithCancel(ctx)
 var callbacks sync.WaitGroup
 defer func(){stop();callbacks.Wait()}()
 var input struct { Mode string ` + "`json:\"mode\"`" + ` }
 if json.Unmarshal(call.Params,&input)!=nil { return nil,errors.New("invalid input") }
 if err:=stream.Emit(ctx,"accepted",sdk.AcceptedEvent{OperationID:call.Scope.Operation});err!=nil{return nil,err}
 var opened sdk.ProcessOpenResult
 if err:=stream.HostCall(ctx,sdk.MethodProcessOpen,sdk.ProcessOpenRequest{RecipeID:"run"},&opened);err!=nil{return nil,err}
 read:=func(pipe,seq string)(sdk.ProcessReadResult,error){
  var value sdk.ProcessReadResult
  err:=stream.HostCall(callbackCtx,sdk.MethodProcessRead,sdk.ProcessReadRequest{Handle:opened.Handle,Pipe:pipe,Seq:seq,MaxBytes:65536},&value)
  return value,err
 }
 write:=func(seq,value string)error{
  var result sdk.ProcessWriteResult
  err:=stream.HostCall(ctx,sdk.MethodProcessWrite,sdk.ProcessWriteRequest{Handle:opened.Handle,WriteID:"write-"+seq,Seq:seq,Bytes:[]byte(value)},&result)
  if err==nil && (result.Written!=len(value)||result.Delivery!="written"){return errors.New("input not delivered")}
  return err
 }
 if value,err:=read("stdout","1");err!=nil||string(value.Bytes)!="ready\n"{return nil,errors.New("stdout separated bytes lost")}
 if value,err:=read("stderr","1");err!=nil||string(value.Bytes)!="private-token=NEVER-LOG-THIS\n"{return nil,errors.New("stderr separated bytes lost")}
 if input.Mode=="loss" {
  if err:=write("1","X");err!=nil{return nil,err}
  return map[string]any{"ack":true},nil
 }
 output,diagnostics:=make(chan error,1),make(chan error,1)
 callbacks.Add(2)
 go func(){defer callbacks.Done();value,err:=read("stdout","2");if err==nil&&string(value.Bytes)!="reply\n"{err=errors.New("reverse reply lost")};output<-err}()
 go func(){defer callbacks.Done();value,err:=read("stderr","2");if err==nil&&!value.EOF{err=errors.New("stderr EOF lost")};diagnostics<-err}()
 if err:=write("1","R");err!=nil{return nil,err}
 if err:=<-output;err!=nil{return nil,err}
 if err:=write("2","X");err!=nil{return nil,err}
 if err:=<-diagnostics;err!=nil{return nil,err}
 var usage sdk.OwnCallResult
 if err:=stream.HostCall(ctx,sdk.MethodUsageAppend,sdk.OwnCallRequest{Call:sdk.OwnCall{
  Kind:"native_execution",Model:"model",Status:200,Usage:sdk.Usage{InputTokens:2,OutputTokens:3,Cost:0.01,Currency:"USD"},
 }},&usage);err!=nil||!usage.Accepted{return nil,errors.New("ordinary ledger rejected own call")}
 var closed sdk.ProcessCloseResult
 if err:=stream.HostCall(ctx,sdk.MethodProcessClose,sdk.ProcessHandleRequest{Handle:opened.Handle},&closed);err!=nil||!closed.Joined{return nil,errors.New("close did not join")}
 return map[string]any{"outcome":"completed","pid":os.Getpid()},nil
}
ENTRYPOINT
`

func ownedModuleManifest(runtimeName string) sdk.Manifest {
	entry := "primary"
	if runtimeName == "native" {
		entry = "primary.so"
		if runtime.GOOS == "darwin" {
			entry = "primary.dylib"
		} else if runtime.GOOS == "windows" {
			entry = "primary.dll"
		}
	} else if runtimeName == "bun" {
		entry = "primary.js"
	} else if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	return sdk.Manifest{
		Schema: 1, ID: "owned-process.fixture", Version: "1.0.0", Runtime: runtimeName, Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 10},
			Capabilities: []sdk.Capability{sdk.ModuleRPC, sdk.OutboundExecutor, sdk.UsageReporter, sdk.AgentWiring, sdk.AgentProcess}},
		Compatibility: &sdk.Compatibility{HostAPI: "^1.10"},
		Actions: []sdk.ActionDescriptor{
			{ID: "run", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{ID: "other", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Processes: []sdk.AgentProcessRecipe{{ID: "run", Agent: "agent", Argv: []string{"--owned-pipe-child"}}},
		Providers: []sdk.ProviderDescriptor{{ID: "provider", Name: "Synthetic", Protocols: []protocolext.Protocol{protocolext.Chat},
			Models: []sdk.Model{{ID: "model", Name: "Synthetic"}}}},
		Agents: []sdk.AgentDescriptor{{ID: "agent", Name: "Synthetic", Bin: "synthetic-agent", Launch: &sdk.AgentLaunch{}}},
	}
}

func ownedModulePackage(t *testing.T, runtimeName string) string {
	t.Helper()
	directory := t.TempDir()
	manifest := ownedModuleManifest(runtimeName)
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if runtimeName == "bun" {
		publicBody, err := json.Marshal(manifest.Public())
		if err != nil {
			t.Fatal(err)
		}
		program := strings.ReplaceAll(ownedBunProgram, "DECLARATION", string(publicBody))
		if err := os.WriteFile(filepath.Join(directory, manifest.Entry), []byte(program), 0o600); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	importNative, entrypoint := "", "func main(){if sdk.Serve(context.Background(),os.Stdin,os.Stdout,module())!=nil{os.Exit(1)}}"
	args := []string{"build", "-mod=readonly", "-p=2", "-trimpath", "-o", filepath.Join(directory, manifest.Entry)}
	if runtimeName == "native" {
		importNative = `"github.com/yetone/magpie/pkg/extension/native"`
		entrypoint = "func init(){if err:=native.Register(module());err!=nil{panic(err)}};func main(){}"
		args = append(args, "-buildmode=c-shared")
	}
	program := strings.NewReplacer("DECLARATION", strconv.Quote(string(body)), "NATIVE_IMPORT", importNative,
		"ENTRYPOINT", entrypoint).Replace(ownedModuleProgram)
	source := filepath.Join(t.TempDir(), "primary.go")
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(args, source)
	command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), "go", args...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic installed %s module: %v\n%s", runtimeName, err, output)
	}
	return directory
}

func TestOwnedProcessInstalledExecNativeBunUseOrdinaryOwnerAndLedger(t *testing.T) {
	for _, runtimeName := range []string{"exec", "native", "bun"} {
		t.Run(runtimeName, func(t *testing.T) {
			if runtimeName == "bun" && os.Getenv("OWNED_PROCESS_BUN") == "" {
				t.Skip("Bun executable is supplied only by the isolated replay runner")
			}
			directory := ownedModulePackage(t, runtimeName)
			manager := NewManager(t.TempDir())
			manager.Bun = os.Getenv("OWNED_PROCESS_BUN")
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := manager.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			entry, err := manager.Install(ctx, directory)
			if err != nil {
				t.Fatal("ordinary installation rejected synthetic process module", err)
			}
			lease, err := manager.Acquire(ctx, entry.Manifest.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			scope := sdk.Scope{Plugin: entry.Manifest.ID, Provider: "provider", Session: "installed-session", Operation: "installed-normal", Attempt: 1}
			var mu sync.Mutex
			var observations []moduleDiagnostic
			manager.diagnosticLog = func(record moduleDiagnostic) {
				mu.Lock()
				observations = append(observations, record)
				mu.Unlock()
			}
			var ledger []sdk.OwnCall
			restore := OnOwnCallContext(func(_ context.Context, actual sdk.Scope, call sdk.OwnCall) {
				if actual != scope {
					t.Error("ordinary ledger received a different host scope", actual)
				}
				mu.Lock()
				ledger = append(ledger, call)
				mu.Unlock()
			})
			defer restore()
			executable, _ := filepath.Abs(os.Args[0])
			launch := OwnedProcessLaunch{
				Recipe:     entry.Manifest.Processes[0],
				Executable: executable, Directory: t.TempDir(), Profile: "profile", Revision: "1", RouteID: 175,
				Check: func(ctx context.Context) error { return ctx.Err() },
			}
			resolutions := 0
			processAgentResolver.Lock()
			previous := processAgentResolver.resolve
			processAgentResolver.resolve = func(parent context.Context, actual *Lease, recipe sdk.AgentProcessRecipe) (OwnedProcessLaunch, error) {
				if actual != lease || recipe.ID != "run" || recipe.Agent != "agent" ||
					!slices.Equal(recipe.Argv, launch.Recipe.Argv) {
					return OwnedProcessLaunch{}, errors.New("ordinary dispatcher changed its installed declaration")
				}
				if _, finite := parent.Deadline(); !finite {
					return OwnedProcessLaunch{}, errors.New("ordinary process parent is not finite")
				}
				resolutions++
				return launch, nil
			}
			processAgentResolver.Unlock()
			defer func() {
				processAgentResolver.Lock()
				processAgentResolver.resolve = previous
				processAgentResolver.Unlock()
			}()
			raw, err := lease.Call(ctx, sdk.MethodModuleCall, scope,
				sdk.ModuleCallRequest{Action: "run", Input: json.RawMessage(`{}`)}, nil)
			if err != nil {
				t.Fatal("ordinary installed action did not receive its declared host-owned process grant", err)
			}
			if resolutions != 1 {
				t.Fatal("ordinary dispatch bypassed its host agent resolver", resolutions)
			}
			var result struct {
				Outcome string `json:"outcome"`
				PID     int    `json:"pid"`
			}
			if json.Unmarshal(raw, &result) != nil || result.Outcome != "completed" ||
				runtimeName == "native" && result.PID != os.Getpid() ||
				runtimeName != "native" && result.PID == os.Getpid() {
				t.Fatalf("installed runtime identity or explicit terminal changed: %s", raw)
			}
			journal, err := manager.Store().Operation(scope.Plugin, scope.Operation)
			if err != nil || journal.State != "completed" {
				t.Fatal("ordinary durable terminal was lost", journal, err)
			}
			mu.Lock()
			if len(ledger) != 1 || ledger[0].Usage.InputTokens != 2 || ledger[0].Usage.Cost != 0.01 {
				t.Error("native finance bypassed the ordinary usage appender", ledger)
			}
			joined, completed := false, false
			for _, record := range observations {
				value := record.Process
				if value == nil || value.Scope != scope || value.RouteID != 175 || value.Parent == "" || value.Incarnation == "" {
					t.Error("process diagnostics lost core correlation", record)
					continue
				}
				joined = joined || value.Phase == "joined" && value.Joined && value.StdinBytes == 2 &&
					value.StdoutBytes == 12 && value.StderrBytes > 0
				completed = completed || value.Phase == "completed" && value.Joined
				encoded, _ := json.Marshal(record)
				if strings.Contains(string(encoded), "NEVER-LOG-THIS") || strings.Contains(string(encoded), "--owned-pipe-child") {
					t.Error("diagnostics retained private stderr or arguments")
				}
			}
			if !joined || !completed {
				t.Error("process lifecycle diagnostics omitted joined or explicit completion", observations)
			}
			mu.Unlock()
			scope.Operation = "installed-loss"
			_, err = lease.Call(ctx, sdk.MethodModuleCall, scope,
				sdk.ModuleCallRequest{Action: "run", Input: json.RawMessage(`{"mode":"loss"}`)}, nil)
			if err == nil {
				t.Fatal("process ACK/exit0 fabricated a completed business operation")
			}
			callErr := err
			journal, err = manager.Store().Operation(scope.Plugin, scope.Operation)
			if err != nil || journal.State != "uncertain" {
				t.Fatal("loss after acceptance cleared ordinary durable uncertainty", journal, err, callErr, errors.Unwrap(callErr))
			}
			scope.Operation = "installed-other"
			if _, err := lease.Call(ctx, sdk.MethodModuleCall, scope,
				sdk.ModuleCallRequest{Action: "other", Input: json.RawMessage(`{}`)}, nil); err == nil {
				t.Fatal("another write action borrowed the declared process grant")
			}
			if resolutions != 2 {
				t.Fatal("another action acquired an unrelated process recipe", resolutions)
			}
		})
	}
}

const ownedBunProgram = `
const manifest=DECLARATION;
const pending=new Map();let next=0,configured;
const send=value=>process.stdout.write(JSON.stringify(value)+"\n");
function host(parent,method,params){const id="host-"+(++next);return new Promise((resolve,reject)=>{pending.set(id,{resolve,reject});send({kind:"host.call",id,parent,method,params});});}
async function run(frame){
 const parent=frame.id,scope=frame.scope;
 send({kind:"event",id:parent,seq:1,event:"accepted",data:{operationId:scope.operation}});
 await new Promise(resolve=>pending.set("ack-"+parent,{resolve}));
 const opened=await host(parent,"process.open",{recipeId:"run"});
 const read=(pipe,seq)=>host(parent,"process.read",{handle:opened.handle,pipe,seq,maxBytes:65536});
 const write=(seq,value)=>host(parent,"process.write",{handle:opened.handle,writeId:"write-"+seq,seq,bytes:Buffer.from(value).toString("base64")});
 if(Buffer.from((await read("stdout","1")).bytes,"base64").toString()!=="ready\n")throw Error("stdout lost");
 if(Buffer.from((await read("stderr","1")).bytes,"base64").toString()!=="private-token=NEVER-LOG-THIS\n")throw Error("stderr lost");
 if(frame.params.input.mode==="loss"){await write("1","X");return {ack:true};}
 const output=read("stdout","2"),diagnostics=read("stderr","2");
 await write("1","R");
 if(Buffer.from((await output).bytes,"base64").toString()!=="reply\n")throw Error("reverse progress lost");
 await write("2","X");
 if(!(await diagnostics).eof)throw Error("stderr EOF lost");
 const usage=await host(parent,"usage.append",{call:{kind:"native_execution",model:"model",status:200,usage:{inputTokens:2,outputTokens:3,cost:0.01,currency:"USD"}}});
 if(!usage.accepted)throw Error("ordinary ledger lost");
 const closed=await host(parent,"process.close",{handle:opened.handle});
 if(!closed.joined)throw Error("join lost");
 return {outcome:"completed",pid:process.pid};
}
let buffer="";const decoder=new TextDecoder();
for await(const chunk of Bun.stdin.stream()){
 buffer+=decoder.decode(chunk,{stream:true});
 let end;
 while((end=buffer.indexOf("\n"))>=0){
  const frame=JSON.parse(buffer.slice(0,end));buffer=buffer.slice(end+1);
  if(frame.kind==="host.result"){const waiter=pending.get(frame.id);pending.delete(frame.id);if(frame.error)waiter.reject(Error("host failure"));else waiter.resolve(frame.result);continue;}
  if(frame.kind==="ack"){const key="ack-"+frame.id;const waiter=pending.get(key);pending.delete(key);waiter.resolve();continue;}
  if(frame.kind!=="call")continue;
  let value;
  if(frame.method==="sdk.describe")value=manifest;
  else if(frame.method==="component.configure"){configured={component:frame.params.component,contract:frame.params.contract,instance:frame.params.instance,configVersion:frame.params.config.version,endpoints:{}};value=configured;}
  else if(frame.method==="component.ready")value=configured;
  else if(frame.method==="component.stop")value={};
  else if(frame.method==="module.call"){
   run(frame).then(result=>send({kind:"result",id:frame.id,result})).catch(()=>send({kind:"result",id:frame.id,error:{code:"fixture",message:"synthetic process failed"}}));
   continue;
  } else {send({kind:"result",id:frame.id,error:{code:"unsupported",message:"unused synthetic method"}});continue;}
  send({kind:"result",id:frame.id,result:value});
 }
}
`
