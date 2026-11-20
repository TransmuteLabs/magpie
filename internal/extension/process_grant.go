package extension

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type OwnedProcessLaunch struct {
	Recipe     sdk.AgentProcessRecipe
	Executable string
	Directory  string
	Env        []string
	Profile    string
	Revision   string
	RouteID    int64
	WatchPaths []string
	Check      func(context.Context) error
	Admit      func(context.Context) (func(), error)
	Observe    func(ProcessObservation)
}

type processContextKey struct{}

type processFileIdentity struct {
	original string
	path     string
	info     os.FileInfo
	sum      [32]byte
}

func captureProcessFile(path string) (processFileIdentity, error) {
	if !filepath.IsAbs(path) {
		return processFileIdentity{}, errors.New("owned process executable must be resolved by its host")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return processFileIdentity{}, errors.New("owned process executable is unavailable")
	}
	if runtime.GOOS == "windows" && (strings.EqualFold(filepath.Ext(resolved), ".cmd") ||
		strings.EqualFold(filepath.Ext(resolved), ".bat")) {
		return processFileIdentity{}, errors.New("owned process cannot use an implicit command shell")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return processFileIdentity{}, errors.New("owned process executable is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return processFileIdentity{}, errors.New("owned process executable must be a regular file")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return processFileIdentity{}, errors.New("owned process executable identity is unreadable")
	}
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return processFileIdentity{original: path, path: resolved, info: info, sum: sum}, nil
}

func (identity processFileIdentity) check() error {
	current, err := captureProcessFile(identity.original)
	if err != nil || current.path != identity.path || !os.SameFile(identity.info, current.info) || current.sum != identity.sum {
		return errors.New("owned process executable changed")
	}
	return nil
}

func (lease *Lease) CallWithProcess(ctx context.Context, launch OwnedProcessLaunch, method string, scope sdk.Scope,
	params any, consume func(Event) error,
) (json.RawMessage, error) {
	if ctx == nil || lease == nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("owned process requires its host parent")}
	}
	deadline, finite := ctx.Deadline()
	if !finite || time.Until(deadline) > 30*time.Minute ||
		method != sdk.MethodModuleCall && method != sdk.MethodToolCall ||
		scope.Operation == "" || scope.Attempt == 0 || ctx.Value(processContextKey{}) != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("owned process requires a finite foreground write intent")}
	}
	if sdk.ValidateAgentProcessRecipe(launch.Recipe) != nil || launch.Check == nil ||
		launch.Profile == "" || launch.Revision == "" || !filepath.IsAbs(launch.Directory) ||
		len(launch.Env) > 128 || len(launch.WatchPaths) > 128 {
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("owned process requires its checked host launch")}
	}
	envNames, envBytes := make(map[string]bool), 0
	for _, variable := range launch.Env {
		name, _, exists := strings.Cut(variable, "=")
		envBytes += len(variable) + 1
		if !exists || name == "" || envNames[name] || strings.ContainsAny(variable, "\x00") || envBytes > sdk.MaxProcessArgvBytes {
			return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("invalid host process environment")}
		}
		envNames[name] = true
	}
	for _, path := range launch.WatchPaths {
		if !filepath.IsAbs(path) {
			return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("process observation requires host-owned absolute paths")}
		}
	}
	if scope.Plugin != "" && scope.Plugin != lease.Manifest().ID {
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("owned process belongs to another module")}
	}
	scope.Plugin = lease.Manifest().ID
	if err := lease.CheckCurrent(ctx); err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	identity, err := captureProcessFile(launch.Executable)
	if err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	directory, err := os.Stat(launch.Directory)
	if err != nil || !directory.IsDir() {
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("owned process directory is unavailable")}
	}
	launch.Recipe.Argv = slices.Clone(launch.Recipe.Argv)
	launch.Env, launch.WatchPaths = slices.Clone(launch.Env), slices.Clone(launch.WatchPaths)
	parent, err := newOwnedProcess(ctx, lease, scope, launch, identity, directory)
	if err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	defer parent.finish()
	return lease.Call(context.WithValue(parent.ctx, processContextKey{}, parent), method, scope, params, consume)
}

func processParent(ctx context.Context) *ownedProcess {
	parent, _ := ctx.Value(processContextKey{}).(*ownedProcess)
	return parent
}

func (parent *ownedProcess) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := parent.ctx.Err(); err != nil {
		return err
	}
	if err := parent.lease.CheckCurrent(ctx); err != nil {
		return err
	}
	if _, err := parent.lease.manager.authForContext(ctx, parent.scope); err != nil {
		return err
	}
	if err := parent.launch.Check(ctx); err != nil {
		return err
	}
	directory, err := os.Stat(parent.launch.Directory)
	if err != nil || !os.SameFile(parent.directory, directory) {
		return errors.New("owned process directory identity changed")
	}
	return parent.executable.check()
}

func (parent *ownedProcess) result(raw json.RawMessage, failure error) error {
	parent.close("parent-finished")
	if failure != nil {
		return failure
	}
	if parent.receiptErr != nil {
		return &OutcomeError{Outcome: "uncertain", Cause: errors.New("owned process final receipt could not be persisted")}
	}
	if err := parent.check(context.WithoutCancel(parent.ctx)); err != nil {
		return &OutcomeError{Outcome: "uncertain", Cause: errors.New("owned process authority changed before publication")}
	}
	var outcome sdk.ProcessOutcome
	if json.Unmarshal(raw, &outcome) != nil {
		return &OutcomeError{Outcome: "uncertain", Cause: errors.New("owned process requires an explicit terminal outcome")}
	}
	switch outcome.Outcome {
	case "completed":
		return nil
	case "not-started":
		parent.mu.Lock()
		effected := parent.writeSeq != 0
		parent.mu.Unlock()
		if !effected {
			return &OutcomeError{Outcome: "not-started", Cause: errors.New("owned process operation did not start")}
		}
	}
	return &OutcomeError{Outcome: "uncertain", Cause: errors.New("owned process operation has no authoritative terminal")}
}
