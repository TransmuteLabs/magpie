package cli

import (
	"errors"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/agent"
)

func TestStartRunCheckedPreservesOrdinaryLaunch(t *testing.T) {
	helpHome(t)
	t.Setenv("FIXTURE_START_URL", "personal")
	t.Setenv("fixture_start_key", "personal")
	sentinel := errors.New("exec returned")
	originalExec, originalLook := startExec, startLook
	t.Cleanup(func() { startExec, startLook = originalExec, originalLook })
	startLook = func(string) (string, error) { return "/fixture/launch", nil }
	type launch struct {
		path, name string
		args, env  []string
	}
	var calls []launch
	startExec = func(path, name string, args, env []string) error {
		calls = append(calls, launch{path, name, slices.Clone(args), slices.Clone(env)})
		return sentinel
	}
	start := agent.Starting{Bin: "fixture-launch", Active: true,
		Env:  []string{"FIXTURE_START_URL=owned", "FIXTURE_START_KEY=owned"},
		Args: []string{"--config", "/work/config with spaces"}, Model: "magpie/fixture",
		Flag: "--model", Plain: []string{"login"}}
	for _, arguments := range [][]string{nil, {"resume", "session"}, {"--model=personal", "resume"}, {"login"}, {"login=personal"}} {
		calls = nil
		if err := startRun(start, arguments); !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		checked := false
		if err := startRunChecked(start, arguments, func() error {
			if len(calls) != 1 {
				t.Fatal("check ran after exec", calls)
			}
			checked = true
			return nil
		}); !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		if !checked || len(calls) != 2 || calls[0].path != calls[1].path || calls[0].name != calls[1].name ||
			!slices.Equal(calls[0].args, calls[1].args) || !slices.Equal(calls[0].env, calls[1].env) {
			t.Fatal("checked extraction changed ordinary launch", calls)
		}
	}
}

func TestStartRunCheckedRefusesRevokedFinalAdmission(t *testing.T) {
	helpHome(t)
	originalExec, originalLook := startExec, startLook
	t.Cleanup(func() { startExec, startLook = originalExec, originalLook })
	looked := false
	startLook = func(string) (string, error) {
		looked = true
		return "/fixture/launch", nil
	}
	startExec = func(string, string, []string, []string) error {
		t.Fatal("revoked launch reached exec")
		return nil
	}
	revoked := errors.New("owner revoked during PATH lookup")
	if err := startRunChecked(agent.Starting{Bin: "fixture-launch", Active: true}, []string{"resume"}, func() error {
		if !looked {
			t.Fatal("final admission preceded PATH lookup")
		}
		return revoked
	}); !errors.Is(err, revoked) {
		t.Fatal("final admission refusal was discarded", err)
	}
}
