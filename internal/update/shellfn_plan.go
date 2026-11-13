package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/edit"
)

type ShellFunctionSnapshot struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Created bool   `json:"created"`
}

type ShellFunctionPlan struct {
	snapshots []ShellFunctionSnapshot
	revision  string
	write     func(ShellFunctionSnapshot, bool) error
}

type ShellFunctionChange struct {
	Path          string
	Before, After []byte
}

func (plan *ShellFunctionPlan) Preview() ([]ShellFunctionChange, error) {
	fnMu.Lock()
	defer fnMu.Unlock()
	var changes []ShellFunctionChange
	for _, snapshot := range plan.snapshots {
		raw, err := os.ReadFile(snapshot.Path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		current, err := shellFunctionBody(raw, snapshot.Name)
		if err != nil || current != snapshot.Before {
			return nil, errors.New("shell function changed before preview")
		}
		changes = append(changes, ShellFunctionChange{Path: snapshot.Path, Before: raw, After: shellFunctionOutput(raw, snapshot)})
	}
	return changes, nil
}

func PlanShellFunction(name string, body ShellBody) (*ShellFunctionPlan, error) {
	fnMu.Lock()
	defer fnMu.Unlock()
	if !shellPlanName(name) {
		return nil, errors.New("invalid shell function identity")
	}
	program := ""
	if body != nil {
		var err error
		program, err = cliExe()
		if err != nil {
			return nil, err
		}
	}
	plan := &ShellFunctionPlan{}
	for _, profile := range homeProfiles() {
		raw, err := os.ReadFile(profile.Path)
		created := false
		if errors.Is(err, fs.ErrNotExist) {
			if body == nil || !profile.Create {
				continue
			}
			created = true
		} else if err != nil {
			return nil, err
		}
		before, err := shellFunctionBody(raw, name)
		if err != nil {
			return nil, err
		}
		after := ""
		if body != nil {
			after = strings.TrimRight(body(profile.Kind, program), "\n")
			if strings.Contains(after, fnOpen(name)) || strings.Contains(after, fnClose(name)) {
				return nil, errors.New("shell function body contains ownership markers")
			}
		}
		plan.snapshots = append(plan.snapshots, ShellFunctionSnapshot{
			Path: profile.Path, Kind: profile.Kind, Name: name, Before: before, After: after, Created: created,
		})
	}
	raw, err := json.Marshal(plan.snapshots)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	plan.revision = hex.EncodeToString(digest[:])
	return plan, nil
}

func (plan *ShellFunctionPlan) Revision() string { return plan.revision }

func (plan *ShellFunctionPlan) Snapshots() []ShellFunctionSnapshot {
	return append([]ShellFunctionSnapshot(nil), plan.snapshots...)
}

func (plan *ShellFunctionPlan) Apply(revision string) ([]string, error) {
	if plan == nil || revision == "" || revision != plan.revision {
		return nil, errors.New("shell function plan changed")
	}
	fnMu.Lock()
	defer fnMu.Unlock()
	var applied []ShellFunctionSnapshot
	var profiles []string
	write := plan.write
	if write == nil {
		write = writeShellFunctionSnapshot
	}
	for _, snapshot := range plan.snapshots {
		if err := write(snapshot, false); err != nil {
			var rollback error
			for index := len(applied) - 1; index >= 0; index-- {
				rollback = errors.Join(rollback, write(applied[index], true))
			}
			return nil, errors.Join(err, rollback)
		}
		applied = append(applied, snapshot)
		if snapshot.After != "" {
			profiles = append(profiles, snapshot.Path)
		}
	}
	return profiles, nil
}

func RestoreShellFunctions(snapshots []ShellFunctionSnapshot, known ...[]ShellFunctionSnapshot) error {
	fnMu.Lock()
	defer fnMu.Unlock()
	resolved, err := shellFunctionRecovery(snapshots, known)
	if err != nil {
		return err
	}
	var result error
	for index := len(resolved) - 1; index >= 0; index-- {
		result = errors.Join(result, writeShellFunctionSnapshot(resolved[index], true))
	}
	return result
}

func ShellFunctionsMatch(snapshots []ShellFunctionSnapshot, after bool) error {
	fnMu.Lock()
	defer fnMu.Unlock()
	for _, snapshot := range snapshots {
		if !fnInHome(snapshot.Path) || !shellPlanName(snapshot.Name) {
			return errors.New("shell function snapshot has no host-owned path")
		}
		raw, err := os.ReadFile(snapshot.Path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		current, err := shellFunctionBody(raw, snapshot.Name)
		expected := snapshot.Before
		if after {
			expected = snapshot.After
		}
		if err != nil || current != expected {
			return errors.New("shell function ownership changed")
		}
	}
	return nil
}

func ShellFunctionsRecoverable(snapshots []ShellFunctionSnapshot, known ...[]ShellFunctionSnapshot) error {
	fnMu.Lock()
	defer fnMu.Unlock()
	_, err := shellFunctionRecovery(snapshots, known)
	return err
}

func shellFunctionRecovery(snapshots []ShellFunctionSnapshot, known [][]ShellFunctionSnapshot) ([]ShellFunctionSnapshot, error) {
	if len(known) > 1 {
		return nil, errors.New("too many shell recovery alternatives")
	}
	resolved := append([]ShellFunctionSnapshot(nil), snapshots...)
	for index, snapshot := range snapshots {
		if !fnInHome(snapshot.Path) || !shellPlanName(snapshot.Name) {
			return nil, errors.New("shell function snapshot has no host-owned path")
		}
		raw, err := os.ReadFile(snapshot.Path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		current, err := shellFunctionBody(raw, snapshot.Name)
		matched := current == snapshot.Before || current == snapshot.After
		for _, alternatives := range known {
			for _, alternative := range alternatives {
				if alternative.Path == snapshot.Path && alternative.Name == snapshot.Name && alternative.Kind == snapshot.Kind && current == alternative.After {
					matched = true
				}
			}
		}
		if err != nil || !matched {
			return nil, errors.New("shell function ownership changed")
		}
		resolved[index].After = current
	}
	return resolved, nil
}

func shellFunctionBody(raw []byte, name string) (string, error) {
	text := strings.ReplaceAll(string(edit.TrimBOM(raw)), "\r\n", "\n")
	ls := lines(text)
	start := fnFind(ls, name)
	if start < 0 {
		return "", nil
	}
	for index := start + 1; index < len(ls); index++ {
		if ls[index] == fnClose(name) {
			if index == start+1 {
				return "", errors.New("empty shell function ownership block")
			}
			return strings.Join(ls[start+1:index], "\n"), nil
		}
	}
	return "", errors.New("incomplete shell function ownership block")
}

func writeShellFunctionSnapshot(snapshot ShellFunctionSnapshot, restore bool) error {
	if !fnInHome(snapshot.Path) || !shellPlanName(snapshot.Name) {
		return errors.New("shell function snapshot has no host-owned path")
	}
	before, after := snapshot.Before, snapshot.After
	if restore {
		before, after = after, before
	}
	raw, err := os.ReadFile(snapshot.Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	current, err := shellFunctionBody(raw, snapshot.Name)
	if err != nil {
		return err
	}
	if current == after {
		return nil
	}
	if current != before {
		return errors.New("shell function changed before its planned update")
	}
	actual := snapshot
	actual.After = after
	out := shellFunctionOutput(raw, actual)
	if len(out) == 0 && raw == nil {
		return nil
	}
	if restore && snapshot.Created && strings.TrimSpace(string(edit.TrimBOM(out))) == "" {
		err := os.Remove(snapshot.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(snapshot.Path), 0o755); err != nil {
		return err
	}
	return edit.WriteAtomic(snapshot.Path, out)
}

func shellFunctionOutput(raw []byte, snapshot ShellFunctionSnapshot) []byte {
	text := string(edit.TrimBOM(raw))
	out := withFnBlock(text, snapshot.Name, snapshot.After, snapshot.Kind == "powershell")
	if len(raw) == 0 && snapshot.Kind == "powershell" && out != "" {
		out = "\xef\xbb\xbf" + out
	} else if strings.HasPrefix(string(raw), "\xef\xbb\xbf") {
		out = "\xef\xbb\xbf" + out
	}
	return []byte(out)
}

func shellPlanName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char != '_' && char != '-' && char != '.' && (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return value != "." && value != ".."
}
