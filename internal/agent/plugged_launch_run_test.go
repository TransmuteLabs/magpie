package agent

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

	"github.com/yetone/magpie/internal/agentplug"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// agyLaunchDesc is agent-agy's launch: the gateway in env, magpie's own
// settings file through --settings, and remote-control as agy is.
func agyLaunchDesc() agentplug.Desc {
	return agentplug.Desc{ID: "agy", Bin: "agy", Format: "json", Model: "model", Prefix: "magpie/",
		Launch: &agentplug.Launch{Env: map[string]string{"GEMINI_API_KEY": "{key}"}, Model: "--model",
			Args: map[string]string{"--settings": "{config}"}, Plain: []string{"remote-control", "--remote-control", "-remote-control"}}}
}

// The function a distro's shell gets starts the agent with launch.args
// before the model, the user's arguments after, and runs a plain argument
// as the agent is: in bash, zsh and fish, whichever are here.
func TestPluggedDistroFunctionArgsAndPlain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the distro's shells")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "magpie settings.json")
	writeFile(t, cfg, `{"model": "magpie/relay/glm"}`)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "agy"), "#!/bin/sh\necho \"key=$GEMINI_API_KEY args=$*\"\n")
	os.Chmod(filepath.Join(bin, "agy"), 0o755)
	l := newLaunch(agyLaunchDesc(), cfg)
	read := modelSed("json", "model", "magpie/")
	vars := l.vars("http://gw", "k1")
	for _, sh := range []string{"bash", "zsh", "fish"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("no %s", sh)
			continue
		}
		kind := "posix"
		if sh == "fish" {
			kind = "fish"
		}
		fn := filepath.Join(t.TempDir(), "fn")
		writeFile(t, fn, l.distroFunction(kind, vars, read, cfg, ""))
		run := func(args string) string {
			t.Helper()
			cmd := exec.Command(path, "-c", "source "+posixQuote(fn)+"; agy "+args)
			cmd.Env = []string{"HOME=" + dir, "PATH=" + bin + ":/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s %s: %v\n%s", sh, args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		on := "key=k1 args=--settings " + cfg + " "
		for args, want := range map[string]string{
			"-p hi":                  on + "--model magpie/relay/glm -p hi",
			"--model=own -p hi":      on + "--model=own -p hi",
			"remote-control start":   "key= args=remote-control start",
			"--remote-control":       "key= args=--remote-control",
			"-p x -remote-control=y": "key= args=-p x -remote-control=y",
		} {
			if got := run(args); got != want {
				t.Errorf("%s agy %s:\n got %s\nwant %s", sh, args, got, want)
			}
		}
		// off magpie: as it is
		writeFile(t, cfg, `{"model": "gemini-3-pro"}`)
		if got := run("-p hi"); got != "key= args=-p hi" {
			t.Errorf("%s off magpie: %s", sh, got)
		}
		writeFile(t, cfg, `{"model": "magpie/relay/glm"}`)
	}
}

// The PowerShell function starts the agent with launch.args from what
// `magpie <id> env` prints (settings=…), and runs a plain argument as the
// agent is.
func TestPluggedPowerShellFunctionArgsAndPlain(t *testing.T) {
	ps, err := exec.LookPath("pwsh")
	if err != nil {
		if ps, err = exec.LookPath("powershell"); err != nil {
			t.Skip("no PowerShell")
		}
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "magpie settings.json")
	var prog string
	if runtime.GOOS == "windows" {
		prog = filepath.Join(dir, "magpie.cmd")
		writeFile(t, prog, "@echo GEMINI_API_KEY=k1\r\n@echo settings="+cfg+"\r\n@echo model=magpie/relay/glm\r\n")
		writeFile(t, filepath.Join(dir, "agy.cmd"), "@echo key=%GEMINI_API_KEY% args=%*\r\n")
	} else {
		prog = filepath.Join(dir, "magpie")
		writeFile(t, prog, "#!/bin/sh\necho GEMINI_API_KEY=k1\necho 'settings="+cfg+"'\necho model=magpie/relay/glm\n")
		writeFile(t, filepath.Join(dir, "agy"), "#!/bin/sh\necho \"key=$GEMINI_API_KEY args=$*\"\n")
		os.Chmod(prog, 0o755)
		os.Chmod(filepath.Join(dir, "agy"), 0o755)
	}
	l := newLaunch(agyLaunchDesc(), cfg)
	fn := filepath.Join(dir, "fn.ps1")
	writeFile(t, fn, l.function("powershell", prog))
	run := func(args string) string {
		t.Helper()
		cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", ". '"+fn+"'; agy "+args)
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("agy %s: %v\n%s", args, err, out)
		}
		return strings.Join(strings.Fields(strings.ReplaceAll(string(out), `"`, "")), " ")
	}
	for args, want := range map[string]string{
		"-p hi":                "key=k1 args=--settings " + cfg + " --model magpie/relay/glm -p hi",
		"--model=own -p hi":    "key=k1 args=--settings " + cfg + " --model=own -p hi",
		"remote-control start": "key= args=remote-control start",
		"--remote-control":     "key= args=--remote-control",
	} {
		if got := run(args); got != strings.Join(strings.Fields(want), " ") {
			t.Errorf("agy %s:\n got %s\nwant %s", args, got, want)
		}
	}
}

func TestSDKPowerShellFunctionUsesCheckedRun(t *testing.T) {
	launch := sdkLaunch("sdk-profile", "native-agent", "", sdk.AgentLaunch{
		Env:   map[string]string{"SDK_GATEWAY": "{url}"},
		Args:  map[string]string{"--mode": "sdk"},
		Model: "--model",
		Plain: []string{"login"},
	})
	got := launch.function("powershell", `C:\program's path\magpie.exe`)
	want := `function native-agent {
    $magpie = 'C:\program''s path\magpie.exe'
    if (-not (Test-Path -LiteralPath $magpie -PathType Leaf)) {
        $global:LASTEXITCODE = 127
        Write-Error 'magpie is unavailable'
        return
    }
    $arguments = @('sdk-profile', 'run') + $args
    $commandLine = @(foreach ($argument in $arguments) {
        '"' + (([string]$argument -replace '(\\*)"', '$1$1\"') -replace '(\\+)\z', '$1$1') + '"'
    }) -join ' '
    $global:LASTEXITCODE = 127
    $start = [System.Diagnostics.ProcessStartInfo]::new($magpie, $commandLine)
    $start.UseShellExecute = $false
    $start.WorkingDirectory = $ExecutionContext.SessionState.Path.CurrentFileSystemLocation.ProviderPath
    $process = [System.Diagnostics.Process]::Start($start)
    try {
        $process.WaitForExit()
        $global:LASTEXITCODE = $process.ExitCode
    } finally { $process.Dispose() }
}`
	if got != want {
		t.Fatalf("SDK PowerShell function must leave arguments, launch policy and exit status to checked run:\n%s", got)
	}
}

type sdkPowerShellLaunchResult struct {
	Path      string
	Directory string
	Arguments []string
}

func init() {
	if os.Getenv("MAGPIE_TEST_POWERSHELL_LAUNCH") == "1" && len(os.Args) >= 3 &&
		os.Args[1] == "sdk-profile" && os.Args[2] == "run" {
		path, pathErr := os.Executable()
		directory, directoryErr := os.Getwd()
		if pathErr != nil || directoryErr != nil {
			os.Exit(38)
		}
		report := sdkPowerShellLaunchResult{Path: path, Directory: directory, Arguments: os.Args[1:]}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			os.Exit(38)
		}
		os.Exit(37)
	}
}

func TestSDKPowerShellFunctionForwardsWindowsArgumentsAndExit(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Windows process launcher")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	magpie := filepath.Join(dir, "magpie [a]'s path.exe")
	if err := os.WriteFile(magpie, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "magpie a's path.exe"), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	launch := sdkLaunch("sdk-profile", "sdk-launch-fixture-powershell-agent", "", sdk.AgentLaunch{
		Args:  map[string]string{"--mode": "sdk"},
		Plain: []string{"login"},
	})
	userArgs := []string{"login=personal", "", "blanc espace", `a"b`, `x\\"y`, `C:\with space\`, "$value", ";|&", "line\nend\\"}
	want := append([]string{"sdk-profile", "run"}, userArgs...)
	script := launch.function("powershell", magpie) + "\nSet-Location -LiteralPath " + psQuote(workdir) + "\n" + launch.bin
	for _, arg := range userArgs {
		script += " " + psQuote(arg)
	}
	script += "\nexit $global:LASTEXITCODE\n"
	fn := filepath.Join(dir, "fixture.ps1")
	writeFile(t, fn, script)
	t.Setenv("MAGPIE_TEST_POWERSHELL_LAUNCH", "1")
	haveShell := false
	for _, shell := range []string{"pwsh", "powershell"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		haveShell = true
		t.Run(shell, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, path, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", fn)
			var callErr *exec.ExitError
			output, err := command.CombinedOutput()
			if !errors.As(err, &callErr) || callErr.ExitCode() != 37 {
				t.Fatalf("launch exit = %v, want 37\n%s", err, output)
			}
			var got sdkPowerShellLaunchResult
			if err := json.Unmarshal(output, &got); err != nil || !slices.Equal(got.Arguments, want) {
				t.Fatalf("launch arguments = %q, want %q, decode %v\n%s", got.Arguments, want, err, output)
			}
			if !strings.EqualFold(got.Path, magpie) || !strings.EqualFold(got.Directory, workdir) {
				t.Fatalf("launched executable %q in %q, want %q in %q", got.Path, got.Directory, magpie, workdir)
			}
		})
	}
	if !haveShell {
		t.Skip("no PowerShell")
	}
}
