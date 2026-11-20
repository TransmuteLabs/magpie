package extension

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	MaxActionFiles    = 128
	MaxActionBytes    = 16 << 20
	MissingActionHash = "missing"
)

var ErrActionChanged = errors.New("extension action changed; prepare it again")

type ActionRuntime interface {
	Manifest(string) (sdk.Manifest, error)
	Invoke(context.Context, string, string, sdk.Scope, any, any, func(Event) error) error
	Store() *Store
}

var _ ActionRuntime = (*Manager)(nil)

type ActionPolicy struct {
	Root             string
	Paths            []string
	CreateUnder      string
	BackupDir        string
	Binding          *sdk.AgentBindingSnapshot
	InputIdentity    string
	RevalidateInputs func(context.Context, map[string]fs.FileInfo) error
}

type ActionSettings struct {
	Files []ActionSettingFile `json:"files"`
}

type ActionSettingFile struct {
	Path       string `json:"path"`
	Body       []byte `json:"body"`
	BeforeHash string `json:"beforeHash"`
	Mode       uint32 `json:"mode"`
	Exists     bool   `json:"exists"`
}

type ActionPreview struct {
	Revision string                  `json:"revision"`
	Files    []sdk.FileChange        `json:"files"`
	Binding  *sdk.AgentBindingChange `json:"binding,omitempty"`
}

type ActionResult struct {
	Applied  bool   `json:"applied"`
	Revision string `json:"revision"`
	Backup   string `json:"backup,omitempty"`
}

type Actions struct {
	host  ActionRuntime
	mu    sync.Mutex
	write func(*os.Root, string, []byte, fs.FileMode) error
}

func NewActions(host ActionRuntime) *Actions {
	return &Actions{host: host, write: actionWrite}
}

type actionFile struct {
	Body   []byte
	Mode   fs.FileMode
	Exists bool
	info   fs.FileInfo
}

type ActionPlan struct {
	owner         *Actions
	plugin        string
	scope         sdk.Scope
	manifest      string
	method        string
	descriptor    string
	intent        string
	sessionID     string
	policy        ActionPolicy
	rootInfo      fs.FileInfo
	before        map[string]actionFile
	files         []sdk.FileChange
	revision      string
	used          bool
	agent         sdk.AgentDescriptor
	binding       *sdk.AgentBindingChange
	contract      sdk.Contract
	inputIdentity string
	createdDirs   map[string]fs.FileInfo
}

func (plan *ActionPlan) Preview() ActionPreview {
	files := slices.Clone(plan.files)
	for index := range files {
		files[index].Body = bytes.Clone(files[index].Body)
	}
	return ActionPreview{Revision: plan.revision, Files: files, Binding: plan.binding.Clone()}
}

func (plan *ActionPlan) Snapshots() []ActionSettingFile {
	files := make([]ActionSettingFile, 0, len(plan.policy.Paths))
	for _, name := range plan.policy.Paths {
		before := plan.before[name]
		files = append(files, ActionSettingFile{
			Path: name, Body: bytes.Clone(before.Body), BeforeHash: actionHash(before), Mode: uint32(before.Mode), Exists: before.Exists,
		})
	}
	return files
}

func (plan *ActionPlan) Root() string      { return plan.policy.Root }
func (plan *ActionPlan) SessionID() string { return plan.sessionID }

func (plan *ActionPlan) Execute(ctx context.Context, revision string) (ActionResult, error) {
	if plan == nil || plan.owner == nil {
		return ActionResult{}, ErrActionChanged
	}
	return plan.owner.Execute(ctx, plan, revision)
}

func (plan *ActionPlan) ExecuteCreateOnly(ctx context.Context, revision string, check func(map[string]fs.FileInfo) error) (ActionResult, error) {
	if plan == nil || plan.owner == nil || plan.policy.CreateUnder == "" || check == nil {
		return ActionResult{}, ErrActionChanged
	}
	return plan.owner.execute(ctx, plan, revision, check)
}

func (actions *Actions) AgentInspect(ctx context.Context, plugin string, scope sdk.Scope, request sdk.AgentInspectRequest, policy ActionPolicy) (sdk.AgentInspectResult, error) {
	plan, err := actions.prepare(ctx, plugin, scope, policy, sdk.AgentWiring, request.ID)
	if err != nil {
		return sdk.AgentInspectResult{}, err
	}
	// Inspection always uses host snapshots, never caller-provided file contents.
	request.Settings, err = json.Marshal(ActionSettings{Files: plan.Snapshots()})
	if err != nil {
		return sdk.AgentInspectResult{}, err
	}
	request.Options = bytes.Clone(request.Options)
	request.Binding = plan.policy.Binding.Clone()
	if err := actionRequest(request); err != nil {
		return sdk.AgentInspectResult{}, err
	}
	var result sdk.AgentInspectResult
	if err := actions.host.Invoke(ctx, plugin, sdk.MethodAgentInspect, plan.scope, request, &result, nil); err != nil {
		return sdk.AgentInspectResult{}, err
	}
	if err := actionRequest(result); err != nil {
		return sdk.AgentInspectResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return sdk.AgentInspectResult{}, err
	}
	return result, nil
}

func (actions *Actions) AgentPlan(ctx context.Context, plugin string, scope sdk.Scope, request sdk.AgentPlanRequest, policy ActionPolicy) (*ActionPlan, error) {
	plan, err := actions.prepare(ctx, plugin, scope, policy, sdk.AgentWiring, request.ID)
	if err != nil {
		return nil, err
	}
	switch request.Action {
	case "connect", "disconnect", "sync", "set":
	default:
		return nil, errors.New("unknown extension agent action")
	}
	plan.intent = request.Action
	request.Binding = plan.policy.Binding.Clone()
	request.Options, request.Settings = bytes.Clone(request.Options), bytes.Clone(request.Settings)
	if len(request.Settings) == 0 {
		request.Settings, err = json.Marshal(ActionSettings{Files: plan.Snapshots()})
		if err != nil {
			return nil, err
		}
	}
	if err := actionRequest(request); err != nil {
		return nil, err
	}
	var result sdk.AgentPlan
	if err := actions.host.Invoke(ctx, plugin, sdk.MethodAgentPlan, plan.scope, request, &result, nil); err != nil {
		return nil, err
	}
	if plan.policy.Binding != nil && result.Binding == nil {
		return nil, errors.New("agent binding action requires a checked binding change")
	}
	if err := sdk.ValidateAgentBindingChange(sdk.Manifest{Contract: plan.contract}, plan.agent, plan.policy.Binding, request.Action, result.Binding); err != nil {
		return nil, err
	}
	plan.binding = result.Binding.Clone()
	return actions.finish(ctx, plan, result.Files)
}

func (actions *Actions) SessionConvert(ctx context.Context, plugin string, scope sdk.Scope, request sdk.SessionRequest, policy ActionPolicy) (*ActionPlan, error) {
	plan, err := actions.prepare(ctx, plugin, scope, policy, sdk.SessionMigration, request.ID)
	if err != nil {
		return nil, err
	}
	if request.From == "" || request.To == "" || len(request.From) > 128 || len(request.To) > 128 ||
		strings.ContainsAny(request.From+request.To, "\x00\r\n") {
		return nil, errors.New("session conversion requires explicit formats")
	}
	plan.intent = request.From + "\x00" + request.To
	request.Files = slices.Clone(request.Files)
	total := 0
	seen := map[string]bool{}
	if len(request.Files) == 0 || len(request.Files) > MaxActionFiles {
		return nil, errors.New("session input file count is out of bounds")
	}
	for index := range request.Files {
		document := &request.Files[index]
		if !actionPath(document.Path) || document.Format != request.From || seen[document.Path] {
			return nil, errors.New("invalid session input document")
		}
		seen[document.Path] = true
		total += len(document.Body)
		if total > MaxActionBytes {
			return nil, errors.New("session input exceeds its byte budget")
		}
		document.Body = bytes.Clone(document.Body)
	}
	request.Options = bytes.Clone(request.Options)
	request.Related = request.Related.Clone()
	if err := sdk.ValidateSessionRelatedInputs(sdk.Manifest{Contract: plan.contract}, request); err != nil {
		return nil, err
	}
	if err := actionRequest(request); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	identity := sha256.Sum256(body)
	plan.inputIdentity = hex.EncodeToString(identity[:])
	if err := actions.revalidateInputs(ctx, plan); err != nil {
		return nil, err
	}
	var result sdk.SessionResult
	if err := actions.host.Invoke(ctx, plugin, sdk.MethodSessionConvert, plan.scope, request, &result, nil); err != nil {
		return nil, err
	}
	if err := actions.revalidateInputs(ctx, plan); err != nil {
		return nil, err
	}
	if len(result.Files) == 0 || len(result.Files) > MaxActionFiles {
		return nil, errors.New("session output file count is out of bounds")
	}
	if len(result.ID) > 256 || strings.ContainsAny(result.ID, "/\\:\x00\r\n") {
		return nil, errors.New("invalid converted session id")
	}
	plan.sessionID = result.ID
	plan.intent += "\x00" + result.ID
	files := make([]sdk.FileChange, len(result.Files))
	for index, document := range result.Files {
		if document.Format != request.To {
			return nil, errors.New("session output format differs from the requested format")
		}
		files[index] = sdk.FileChange{Path: document.Path, Body: document.Body}
	}
	return actions.finish(ctx, plan, files)
}

func actionRequest(request any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(body) > sdk.MaxFrameBytes-4096 {
		return errors.New("extension action request exceeds its byte budget")
	}
	return nil
}

func (actions *Actions) prepare(ctx context.Context, plugin string, scope sdk.Scope, policy ActionPolicy, capability sdk.Capability, descriptor string) (*ActionPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if actions == nil || actions.host == nil || plugin == "" || scope.Plugin != "" && scope.Plugin != plugin {
		return nil, errors.New("invalid extension action owner")
	}
	scope.Plugin = plugin
	manifest, err := actions.host.Manifest(plugin)
	if err != nil {
		return nil, err
	}
	if manifest.ID != plugin || !manifest.Contract.Has(capability) {
		return nil, errors.New("extension does not own this action capability")
	}
	found := false
	var agentDescriptor sdk.AgentDescriptor
	if capability == sdk.AgentWiring {
		for _, agent := range manifest.Agents {
			if agent.ID == descriptor {
				found, agentDescriptor = true, agent.Clone()
			}
		}
	} else {
		for _, session := range manifest.Sessions {
			found = found || session.ID == descriptor
		}
	}
	if descriptor == "" || !found {
		return nil, errors.New("extension does not own this action descriptor")
	}
	if err := actions.generation(scope); err != nil {
		return nil, err
	}
	createOnly := capability == sdk.SessionMigration && actionPath(policy.CreateUnder)
	policy.Binding = policy.Binding.Clone()
	if policy.Binding != nil && (capability != sdk.AgentWiring ||
		sdk.ValidateAgentBindingSnapshot(manifest, agentDescriptor, policy.Binding) != nil) {
		return nil, errors.New("extension action requires a valid host agent binding snapshot")
	}
	bindingOnly := capability == sdk.AgentWiring && manifest.Contract.Version.Minor >= 5 &&
		agentDescriptor.Launch != nil && len(agentDescriptor.ConfigFiles) == 0 && policy.Binding != nil
	if !filepath.IsAbs(policy.Root) || len(policy.Paths) > MaxActionFiles ||
		len(policy.Paths) == 0 && !createOnly && !bindingOnly || policy.CreateUnder != "" && (!createOnly || len(policy.Paths) != 0) {
		return nil, errors.New("extension actions require an explicit root and file allowlist")
	}
	policy.Root, err = filepath.EvalSymlinks(policy.Root)
	if err != nil {
		return nil, err
	}
	if policy.BackupDir != "" && !filepath.IsAbs(policy.BackupDir) {
		return nil, errors.New("extension backup directory must be absolute")
	}
	policy.Paths = slices.Clone(policy.Paths)
	root, err := os.OpenRoot(policy.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	before := make(map[string]actionFile, len(policy.Paths))
	seen := map[string]bool{}
	total := 0
	for _, name := range policy.Paths {
		folded := name
		if runtime.GOOS == "windows" {
			folded = strings.ToLower(folded)
		}
		if !actionPath(name) || seen[folded] {
			return nil, errors.New("invalid or duplicate extension action path")
		}
		seen[folded] = true
		snapshot, err := actionRead(root, name)
		if err != nil {
			return nil, err
		}
		total += len(snapshot.Body)
		if total > MaxActionBytes {
			return nil, errors.New("extension action snapshots exceed their byte budget")
		}
		before[name] = snapshot
	}
	method := sdk.MethodAgentPlan
	if capability == sdk.SessionMigration {
		method = sdk.MethodSessionConvert
	}
	manifest.Contract.Capabilities = slices.Clone(manifest.Contract.Capabilities)
	return &ActionPlan{owner: actions, plugin: plugin, scope: scope, manifest: actionManifest(manifest), method: method, descriptor: descriptor,
		policy: policy, rootInfo: info, before: before, agent: agentDescriptor, contract: manifest.Contract}, nil
}

func (actions *Actions) generation(scope sdk.Scope) error {
	if scope.Account == "" {
		if scope.Generation != 0 {
			return errors.New("an account generation requires an account")
		}
		return nil
	}
	if scope.Provider == "" || scope.Generation == 0 || actions.host.Store() == nil {
		return errors.New("an extension action requires a current account generation")
	}
	account, err := actions.host.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil {
		return err
	}
	if account.Generation != scope.Generation {
		return ErrActionChanged
	}
	return nil
}

func (actions *Actions) finish(ctx context.Context, plan *ActionPlan, files []sdk.FileChange) (*ActionPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(files) > MaxActionFiles {
		return nil, errors.New("extension action file count is out of bounds")
	}
	seen := map[string]bool{}
	total := 0
	plan.files = slices.Clone(files)
	var root *os.Root
	if plan.policy.CreateUnder != "" {
		var err error
		root, err = os.OpenRoot(plan.policy.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		info, err := root.Stat(".")
		if err != nil || !os.SameFile(info, plan.rootInfo) {
			return nil, ErrActionChanged
		}
	}
	for index := range plan.files {
		change := &plan.files[index]
		if root != nil {
			if !actionPath(change.Path) || change.Remove {
				return nil, errors.New("session output must create a file under its approved prefix")
			}
			change.Path = path.Join(plan.policy.CreateUnder, change.Path)
			if seen[change.Path] {
				return nil, errors.New("duplicate session output path")
			}
			before, err := actionRead(root, change.Path)
			if err != nil {
				return nil, err
			}
			if before.Exists {
				return nil, errors.New("session output already exists")
			}
			plan.before[change.Path] = before
			plan.policy.Paths = append(plan.policy.Paths, change.Path)
		}
		before, allowed := plan.before[change.Path]
		if !allowed || seen[change.Path] || change.Mode & ^uint32(0o777) != 0 || change.Remove && (len(change.Body) != 0 || change.Mode != 0) {
			return nil, errors.New("extension returned an unauthorized or invalid file change")
		}
		seen[change.Path] = true
		if change.BeforeHash != "" && change.BeforeHash != actionHash(before) {
			return nil, ErrActionChanged
		}
		total += len(change.Body)
		if total > MaxActionBytes {
			return nil, errors.New("extension action output exceeds its byte budget")
		}
		change.BeforeHash = actionHash(before)
		change.Body = bytes.Clone(change.Body)
	}
	slices.SortFunc(plan.files, func(first, second sdk.FileChange) int { return strings.Compare(first.Path, second.Path) })
	body, err := json.Marshal(struct {
		Plugin         string
		Scope          sdk.Scope
		Manifest       string
		Method         string
		Descriptor     string
		Intent         string
		Root           string
		BackupDir      string
		CreateUnder    string
		Before         map[string]actionFile
		Files          []sdk.FileChange
		BindingBefore  *sdk.AgentBindingSnapshot
		BindingAfter   *sdk.AgentBindingChange
		InputIdentity  string
		SourceIdentity string
	}{plan.plugin, plan.scope, plan.manifest, plan.method, plan.descriptor, plan.intent, plan.policy.Root, plan.policy.BackupDir, plan.policy.CreateUnder, plan.before, plan.files, plan.policy.Binding, plan.binding, plan.inputIdentity, plan.policy.InputIdentity})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	plan.revision = hex.EncodeToString(sum[:])
	return plan, nil
}

func actionManifest(manifest sdk.Manifest) string {
	body, _ := json.Marshal(manifest.Public())
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func actionHash(snapshot actionFile) string {
	if !snapshot.Exists {
		return MissingActionHash
	}
	sum := sha256.Sum256(snapshot.Body)
	return hex.EncodeToString(sum[:])
}

func actionPath(name string) bool {
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00\r\n") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if strings.HasPrefix(component, ".magpie-extension") {
			return false
		}
	}
	return true
}

func actionInfo(root *os.Root, name string) (fs.FileInfo, error) {
	components := strings.Split(name, "/")
	var info fs.FileInfo
	for index := range components {
		var err error
		info, err = root.Lstat(path.Join(components[:index+1]...))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || index < len(components)-1 && !info.IsDir() {
			return nil, errors.New("extension action paths cannot contain links or non-directories")
		}
	}
	return info, nil
}

func actionRead(root *os.Root, name string) (actionFile, error) {
	info, err := actionInfo(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return actionFile{}, nil
	}
	if err != nil {
		return actionFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxActionBytes {
		return actionFile{}, errors.New("extension action target must be a bounded regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return actionFile{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return actionFile{}, ErrActionChanged
	}
	body, err := io.ReadAll(io.LimitReader(file, MaxActionBytes+1))
	if err != nil {
		return actionFile{}, err
	}
	after, err := file.Stat()
	if err != nil || len(body) > MaxActionBytes || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return actionFile{}, ErrActionChanged
	}
	return actionFile{Body: body, Mode: opened.Mode().Perm(), Exists: true, info: opened}, nil
}

func (actions *Actions) Execute(ctx context.Context, plan *ActionPlan, revision string) (ActionResult, error) {
	return actions.execute(ctx, plan, revision, nil)
}

func (actions *Actions) execute(ctx context.Context, plan *ActionPlan, revision string, check func(map[string]fs.FileInfo) error) (ActionResult, error) {
	actions.mu.Lock()
	defer actions.mu.Unlock()
	if plan == nil || plan.owner != actions || plan.used || revision == "" || revision != plan.revision {
		return ActionResult{}, ErrActionChanged
	}
	result := ActionResult{Revision: revision}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := actions.checkAuthority(plan); err != nil {
		return result, err
	}
	if err := actions.revalidateInputs(ctx, plan); err != nil {
		return result, err
	}
	root, err := os.OpenRoot(plan.policy.Root)
	if err != nil {
		return result, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(plan.rootInfo, info) {
		return result, ErrActionChanged
	}
	unlock, err := actionLock(ctx, root)
	if err != nil {
		return result, err
	}
	defer unlock()
	if err := actions.checkAuthority(plan); err != nil {
		return result, err
	}
	if err := actions.revalidateInputs(ctx, plan); err != nil {
		return result, err
	}
	if err := plan.check(root); err != nil {
		return result, err
	}
	if len(plan.files) == 0 {
		plan.used, result.Applied = true, true
		return result, nil
	}
	backup, err := plan.backup()
	if err != nil {
		return result, err
	}
	result.Backup = backup
	if err := plan.check(root); err != nil {
		return result, err
	}
	plan.used = true
	type actionEffect struct {
		change    sdk.FileChange
		parent    *os.Root
		name      string
		temporary string
		created   fs.FileInfo
	}
	createOnly := plan.policy.CreateUnder != ""
	var changed []actionEffect
	var directories, createdParents []string
	var parents map[string]actionParent
	if createOnly {
		parents = make(map[string]actionParent)
		plan.createdDirs = make(map[string]fs.FileInfo)
		defer func() {
			for _, parent := range parents {
				if parent.handle != nil {
					parent.handle.Close()
				}
			}
		}()
	}
	verifyCreated := func() error {
		if err := plan.checkCreateParents(parents); err != nil {
			return err
		}
		for _, effect := range changed {
			current, err := actionRead(effect.parent, effect.name)
			if err != nil {
				return err
			}
			expected := actionFile{Body: effect.change.Body, Mode: actionMode(effect.change, plan.before[effect.change.Path]), Exists: true}
			if !actionEqual(expected, current) || !os.SameFile(effect.created, current.info) {
				return fmt.Errorf("%s changed before commit: %w", effect.change.Path, ErrActionChanged)
			}
		}
		return nil
	}
	for _, change := range plan.files {
		if err = ctx.Err(); err == nil {
			err = actions.checkAuthority(plan)
		}
		if err == nil {
			err = plan.checkFile(root, change.Path)
		}
		if err != nil {
			break
		}
		if createOnly {
			var parent *os.Root
			parent, err = plan.pinCreateParent(root, path.Dir(change.Path), parents, &createdParents)
			if err == nil {
				err = actions.revalidateInputs(ctx, plan)
			}
			if err == nil {
				err = plan.checkCreateParents(parents)
			}
			if err != nil {
				break
			}
			name := path.Base(change.Path)
			temporary, info, createErr := actionCreate(parent, name, change.Body, actionMode(change, plan.before[change.Path]))
			if info != nil {
				changed = append(changed, actionEffect{change: change, parent: parent, name: name, temporary: temporary, created: info})
			}
			if err = createErr; err != nil {
				break
			}
			continue
		}
		changed = append(changed, actionEffect{change: change, parent: root, name: change.Path})
		if change.Remove {
			err = actions.revalidateInputs(ctx, plan)
			if err == nil {
				err = root.Remove(change.Path)
				if errors.Is(err, fs.ErrNotExist) {
					err = nil
				} else if err == nil {
					err = actionSync(root, path.Dir(change.Path))
				}
			}
		} else {
			var made []string
			made, err = actionParents(root, change.Path)
			directories = append(directories, made...)
			if plan.createdDirs == nil {
				plan.createdDirs = make(map[string]fs.FileInfo)
			}
			for _, directory := range made {
				info, statErr := root.Lstat(directory)
				if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					err = errors.Join(err, ErrActionChanged)
					break
				}
				plan.createdDirs[directory] = info
			}
			if err == nil {
				err = actions.revalidateInputs(ctx, plan)
			}
			if err == nil {
				err = actions.write(root, change.Path, change.Body, actionMode(change, plan.before[change.Path]))
			}
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = actions.checkAuthority(plan)
	}
	if err == nil {
		err = actions.revalidateInputs(ctx, plan)
	}
	if err == nil && createOnly {
		err = verifyCreated()
	}
	if err == nil && check != nil {
		published := make(map[string]fs.FileInfo, len(changed))
		for _, effect := range changed {
			published[effect.change.Path] = effect.created
		}
		if err = check(published); err == nil {
			err = actions.checkAuthority(plan)
		}
		if err == nil {
			err = actions.revalidateInputs(ctx, plan)
		}
		if err == nil {
			err = verifyCreated()
		}
	}
	if err == nil && createOnly {
		for _, effect := range changed {
			if err = actionRemoveOwned(effect.parent, effect.temporary, effect.created); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = actionBackupStatus(backup, "applied")
	}
	var rollbackErr error
	if err != nil {
		for index := len(changed) - 1; index >= 0; index-- {
			effect := changed[index]
			change := effect.change
			name := change.Path
			before := plan.before[name]
			current, readErr := actionRead(effect.parent, effect.name)
			if readErr != nil {
				rollbackErr = errors.Join(rollbackErr, readErr)
				continue
			}
			if actionEqual(before, current) {
				continue
			}
			after := actionFile{}
			if !change.Remove {
				after = actionFile{Body: change.Body, Mode: actionMode(change, before), Exists: true}
			}
			if !actionEqual(after, current) || effect.created != nil && !os.SameFile(effect.created, current.info) {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s changed before rollback: %w", name, ErrActionChanged))
				continue
			}
			var restoreErr error
			if before.Exists {
				restoreErr = actionWrite(root, name, before.Body, before.Mode)
			} else if effect.created != nil {
				restoreErr = actionRemoveOwned(effect.parent, effect.name, effect.created)
			} else {
				restoreErr = root.Remove(name)
				if errors.Is(restoreErr, fs.ErrNotExist) {
					restoreErr = nil
				}
			}
			rollbackErr = errors.Join(rollbackErr, restoreErr)
		}
		if createOnly {
			for _, effect := range changed {
				rollbackErr = errors.Join(rollbackErr, actionRemoveOwned(effect.parent, effect.temporary, effect.created))
			}
			for index := len(createdParents) - 1; index >= 0; index-- {
				directory := createdParents[index]
				parent := parents[directory]
				if parent.handle != nil {
					parent.handle.Close()
				}
				delete(parents, directory)
				rollbackErr = errors.Join(rollbackErr, actionRemoveOwned(parent.owner, parent.name, parent.info))
			}
		}
		for index := len(directories) - 1; index >= 0; index-- {
			if removeErr := root.Remove(directories[index]); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, removeErr)
			}
		}
		if rollbackErr == nil {
			rollbackErr = actionBackupStatus(backup, "rolled_back")
		}
		return result, fmt.Errorf("extension action failed; originals retained in %s: %w", backup, errors.Join(err, rollbackErr))
	}
	result.Applied = true
	return result, nil
}

type actionParent struct {
	handle *os.Root
	owner  *os.Root
	name   string
	info   fs.FileInfo
}

func (plan *ActionPlan) pinCreateParent(root *os.Root, directory string, parents map[string]actionParent, created *[]string) (*os.Root, error) {
	owner, prefix := root, "."
	for name := range strings.SplitSeq(directory, "/") {
		prefix = path.Join(prefix, name)
		if parent, ok := parents[prefix]; ok {
			owner = parent.handle
			continue
		}
		info, err := owner.Lstat(name)
		made := errors.Is(err, fs.ErrNotExist)
		if made {
			if err := owner.Mkdir(name, 0o700); err != nil {
				return nil, err
			}
			info, err = owner.Lstat(name)
		}
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("%s: %w", prefix, ErrActionChanged)
		}
		parent := actionParent{owner: owner, name: name, info: info}
		if made {
			*created = append(*created, prefix)
			parents[prefix] = parent
			plan.createdDirs[prefix] = info
		}
		next, err := owner.OpenRoot(name)
		if err != nil {
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, fmt.Errorf("%s: %w", prefix, ErrActionChanged)
		}
		parent.handle = next
		parents[prefix], owner = parent, next
	}
	return owner, nil
}

func (plan *ActionPlan) checkCreateParents(parents map[string]actionParent) error {
	info, err := os.Lstat(plan.policy.Root)
	if err != nil || !os.SameFile(plan.rootInfo, info) {
		return ErrActionChanged
	}
	for directory, parent := range parents {
		info, err := parent.owner.Lstat(parent.name)
		if err != nil || !os.SameFile(parent.info, info) {
			return fmt.Errorf("%s: %w", directory, ErrActionChanged)
		}
	}
	return nil
}

func actionRemoveOwned(root *os.Root, name string, expected fs.FileInfo) error {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(expected, info) {
		return fmt.Errorf("%s changed before cleanup: %w", name, ErrActionChanged)
	}
	if err := root.Remove(name); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (actions *Actions) checkAuthority(plan *ActionPlan) error {
	manifest, err := actions.host.Manifest(plan.plugin)
	if err != nil {
		return err
	}
	if actionManifest(manifest) != plan.manifest {
		return ErrActionChanged
	}
	return actions.generation(plan.scope)
}

func (actions *Actions) revalidateInputs(ctx context.Context, plan *ActionPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.policy.RevalidateInputs != nil {
		if err := plan.policy.RevalidateInputs(ctx, maps.Clone(plan.createdDirs)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (plan *ActionPlan) check(root *os.Root) error {
	for name := range plan.before {
		if err := plan.checkFile(root, name); err != nil {
			return err
		}
	}
	return nil
}

func (plan *ActionPlan) checkFile(root *os.Root, name string) error {
	current, err := actionRead(root, name)
	if err != nil {
		return err
	}
	if !actionEqual(current, plan.before[name]) {
		return ErrActionChanged
	}
	return nil
}

func actionEqual(first, second actionFile) bool {
	sameMode := first.Mode == second.Mode
	if runtime.GOOS == "windows" {
		sameMode = first.Mode&0o200 == second.Mode&0o200
	}
	return first.Exists == second.Exists && sameMode && bytes.Equal(first.Body, second.Body)
}

func actionMode(change sdk.FileChange, before actionFile) fs.FileMode {
	if change.Mode != 0 {
		return fs.FileMode(change.Mode)
	}
	if before.Exists {
		return before.Mode
	}
	return 0o600
}

func actionLock(ctx context.Context, root *os.Root) (func(), error) {
	const name = ".magpie-extension.lock"
	var file *os.File
	for range 8 {
		info, err := root.Lstat(name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
			return nil, errors.New("extension action lock is unavailable")
		}
		flags := os.O_RDWR
		if errors.Is(err, fs.ErrNotExist) {
			flags |= os.O_CREATE | os.O_EXCL
		}
		file, err = root.OpenFile(name, flags, 0o600)
		if errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		opened, openErr := file.Stat()
		current, statErr := root.Lstat(name)
		if openErr != nil || statErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
			file.Close()
			return nil, errors.New("extension action lock changed while opening")
		}
		break
	}
	if file == nil {
		return nil, errors.New("extension action lock could not be opened")
	}
	deadline := time.NewTimer(filelock.Wait)
	defer deadline.Stop()
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		held, err := filelock.TryLock(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		if held {
			return func() { filelock.Unlock(file); file.Close() }, nil
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			file.Close()
			return nil, errors.New("extension action lock timed out")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func actionParents(root *os.Root, name string) ([]string, error) {
	var made []string
	components := strings.Split(path.Dir(name), "/")
	for index := range components {
		directory := path.Join(components[:index+1]...)
		if directory == "." {
			continue
		}
		info, err := root.Lstat(directory)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(directory, 0o700); err != nil {
				return made, err
			}
			made = append(made, directory)
		} else if err != nil {
			return made, err
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return made, errors.New("extension action parent is not a regular directory")
		}
	}
	return made, nil
}

func actionWrite(root *os.Root, name string, body []byte, mode fs.FileMode) error {
	temporary, _, err := actionStage(root, name, body, mode)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if err := root.Rename(temporary, name); err != nil {
		return err
	}
	return actionSync(root, path.Dir(name))
}

func actionCreate(root *os.Root, name string, body []byte, mode fs.FileMode) (string, fs.FileInfo, error) {
	temporary, info, err := actionStage(root, name, body, mode)
	if err != nil {
		return "", nil, err
	}
	if err := root.Link(temporary, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			err = fmt.Errorf("%s: %w", name, ErrActionChanged)
		}
		return "", nil, errors.Join(err, actionRemoveOwned(root, temporary, info))
	}
	return temporary, info, actionSync(root, path.Dir(name))
}

func actionStage(root *os.Root, name string, body []byte, mode fs.FileMode) (string, fs.FileInfo, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, err
	}
	temporary := path.Join(path.Dir(name), ".magpie-extension-"+hex.EncodeToString(nonce[:])+".tmp")
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	if _, err = file.Write(body); err == nil {
		err = file.Chmod(mode)
	}
	if err == nil {
		err = file.Sync()
	}
	var info fs.FileInfo
	if err == nil {
		info, err = file.Stat()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		root.Remove(temporary)
		return "", nil, err
	}
	return temporary, info, nil
}

func actionSync(root *os.Root, name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (plan *ActionPlan) backup() (string, error) {
	if plan.policy.BackupDir == "" {
		return "", errors.New("extension action writes require an explicit backup directory")
	}
	if err := os.MkdirAll(plan.policy.BackupDir, 0o700); err != nil {
		return "", err
	}
	directory, err := os.MkdirTemp(plan.policy.BackupDir, "action-")
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	type backupFile struct {
		Path       string      `json:"path"`
		Backup     string      `json:"backup,omitempty"`
		BeforeHash string      `json:"beforeHash"`
		BeforeMode fs.FileMode `json:"beforeMode"`
		AfterHash  string      `json:"afterHash,omitempty"`
		Remove     bool        `json:"remove,omitempty"`
	}
	files := make([]backupFile, len(plan.files))
	for index, change := range plan.files {
		before := plan.before[change.Path]
		file := backupFile{Path: change.Path, BeforeHash: actionHash(before), BeforeMode: before.Mode, Remove: change.Remove}
		if before.Exists {
			file.Backup = fmt.Sprintf("%03d.before", index)
			if err := actionWrite(root, file.Backup, before.Body, 0o600); err != nil {
				return "", err
			}
		}
		if !change.Remove {
			file.AfterHash = actionHash(actionFile{Body: change.Body, Exists: true})
		}
		files[index] = file
	}
	body, err := json.Marshal(struct {
		Plugin   string       `json:"plugin"`
		Scope    sdk.Scope    `json:"scope"`
		Root     string       `json:"root"`
		Revision string       `json:"revision"`
		Files    []backupFile `json:"files"`
	}{plan.plugin, plan.scope, plan.policy.Root, plan.revision, files})
	if err != nil {
		return "", err
	}
	if err := actionWrite(root, "manifest.json", body, 0o600); err != nil {
		return "", err
	}
	if err := actionWrite(root, "status", []byte("prepared\n"), 0o600); err != nil {
		return "", err
	}
	parent, err := os.OpenRoot(plan.policy.BackupDir)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	if err := actionSync(parent, "."); err != nil {
		return "", err
	}
	return directory, nil
}

func actionBackupStatus(directory, status string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	return actionWrite(root, "status", []byte(status+"\n"), 0o600)
}
