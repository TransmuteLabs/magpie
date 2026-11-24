package sessions

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOMPResumeUsesCheckedSessionIdentity(t *testing.T) {
	profileRoot := filepath.Join(t.TempDir(), ".omp", "profiles")
	t.Setenv("OMP_PROFILE", "")
	journal := func(profile string) string {
		return filepath.Join(profileRoot, profile, "agent", "sessions", "--workspace--", "session.jsonl")
	}
	for _, tc := range []struct {
		name, path, id, want string
	}{
		{name: "default", id: "abc-123", want: "omp --resume abc-123"},
		{name: "named", path: journal("client-a.v2"), id: "abc-123", want: "omp --profile client-a.v2 --resume abc-123"},
		{name: "opaque", path: "storage:profile/abc-123", id: "abc-123", want: "omp --resume abc-123"},
		{name: "invalid profile", path: journal("--help"), id: "abc-123"},
		{name: "flag as id", id: "--help"},
		{name: "flag as named id", path: journal("client-a.v2"), id: "--help"},
		{name: "flag as local default id", path: filepath.Join(filepath.Dir(profileRoot), "session.jsonl"), id: "--help"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeCommand("", "omp", tc.id, "", tc.path); got != tc.want {
				t.Fatalf("resume command = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOMPResumeFindsDefaultSessionFromNamedProfile(test *testing.T) {
	setup(test)
	test.Setenv("PI_CODING_AGENT_DIR", "")
	test.Setenv("OMP_PROFILE", "work")
	home, err := os.UserHomeDir()
	if err != nil {
		test.Fatal(err)
	}
	root := filepath.Join(home, ".omp")
	copyTree(test, "testdata/omp", root)
	journal := filepath.Join(root, "agent", "sessions", "--work-omp--",
		"2026-09-28T08-00-00-000Z_"+ompMain+".jsonl")
	session, found := Find("omp", ompMain)
	if !found || session.Path != journal {
		test.Fatalf("Find returned %q, want the default OMP journal %q", session.Path, journal)
	}
	want := "cd '/work/omp' && omp --profile default --resume " + ompMain
	if runtime.GOOS == "windows" {
		want = "Set-Location -LiteralPath '/work/omp'; omp --profile default --resume " + ompMain
	}
	command := ResumeNow(session)
	if session.Resume != want || command != want {
		test.Fatalf("default OMP resume: listed %q, terminal %q, want %q", session.Resume, command, want)
	}
}

func TestOMPResumeCopyKeepsDefaultProfile(test *testing.T) {
	setup(test)
	test.Setenv("PI_CODING_AGENT_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		test.Fatal(err)
	}
	root := filepath.Join(home, ".omp")
	copyTree(test, "testdata/omp", root)
	relative := filepath.Join("sessions", "--work-omp--", "2026-09-28T08-00-00-000Z_"+ompMain+".jsonl")
	journal := filepath.Join(root, "agent", relative)
	body, err := os.ReadFile(journal)
	if err != nil {
		test.Fatal(err)
	}
	work := test.TempDir()
	cwdJSON, err := json.Marshal(work)
	if err != nil {
		test.Fatal(err)
	}
	body = []byte(strings.ReplaceAll(string(body), `"/work/omp"`, string(cwdJSON)))
	if err := os.WriteFile(journal, body, 0o600); err != nil {
		test.Fatal(err)
	}
	session, found := Find("omp", ompMain)
	if !found || session.Path != journal || session.Cwd != work {
		test.Fatalf("Find chose %q in %q, want %q in %q", session.Path, session.Cwd, journal, work)
	}
	command := ResumeNow(session)
	if command != session.Resume || command == "" {
		test.Fatalf("copied resume = %q, terminal resume = %q", session.Resume, command)
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(command, "$env:OMP_PROFILE = 'default'; omp --resume "+ompMain) {
			test.Fatalf("PowerShell resume does not set the default profile: %q", command)
		}
		return
	}

	named := filepath.Join(root, "profiles", "work", "agent")
	other := filepath.Join(named, relative)
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(other, body, 0o600); err != nil {
		test.Fatal(err)
	}
	bin := test.TempDir()
	const stub = `#!/bin/sh
[ "$#" -eq 2 ] && [ "$1" = --resume ] || exit 41
agent=${PI_CODING_AGENT_DIR:-"$HOME/.omp/agent"}
if [ "${OMP_FIXTURE_PRE_PROFILE-}" != 1 ]; then
  profile=${OMP_PROFILE-${PI_PROFILE-}}
  if [ -n "$profile" ] && [ "$profile" != default ]; then
    agent="$HOME/.omp/profiles/$profile/agent"
  elif [ -n "${PI_PROFILE-}" ] && [ "$agent" = "$HOME/.omp/profiles/$PI_PROFILE/agent" ]; then
    agent="$HOME/.omp/agent"
  fi
fi
for journal in "$agent"/sessions/*/*_"$2".jsonl; do
  [ -f "$journal" ] || continue
  printf '%s\n' "$journal"
  exit
done
exit 42
`
	if err := os.WriteFile(filepath.Join(bin, "omp"), []byte(stub), 0o700); err != nil {
		test.Fatal(err)
	}
	for _, shell := range []struct {
		name, after string
		env         []string
	}{
		{name: "OMP_PROFILE", env: []string{"OMP_PROFILE=work"}, after: "work||"},
		{name: "PI_PROFILE", env: []string{"PI_PROFILE=work"}, after: "|work|"},
		{
			name:  "inherited_profile",
			env:   []string{"OMP_PROFILE=work", "PI_PROFILE=work", "PI_CODING_AGENT_DIR=" + named},
			after: "work|work|" + named,
		},
		{name: "older_omp", env: []string{"OMP_PROFILE=work", "OMP_FIXTURE_PRE_PROFILE=1"}, after: "work||"},
	} {
		test.Run(shell.name, func(test *testing.T) {
			script := command + "\n" + `printf '%s|%s|%s\n' "${OMP_PROFILE-}" "${PI_PROFILE-}" "${PI_CODING_AGENT_DIR-}"`
			shellCommand := exec.CommandContext(test.Context(), "/bin/sh", "-ec", script)
			shellCommand.Env = append([]string{"PATH=" + bin, "HOME=" + home}, shell.env...)
			out, err := shellCommand.CombinedOutput()
			want := journal + "\n" + shell.after + "\n"
			if err != nil || string(out) != want {
				test.Fatalf("copied resume in %s: %v, got %q, want %q", shell.name, err, out, want)
			}
		})
	}
}
