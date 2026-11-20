package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type SessionExtension struct {
	Plugin  string                `json:"plugin"`
	Session sdk.SessionDescriptor `json:"session"`
}

type SessionExtensionRegistry interface {
	extension.ActionRuntime
	List() ([]extension.Installed, error)
}

var _ SessionExtensionRegistry = (*extension.Manager)(nil)

// Listing never invokes a converter. Ambiguous SDK routes are unavailable.
func extensionSessionRoutes(source string) []SessionExtension {
	list, err := SessionExtensions(extension.Default())
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	for _, item := range list {
		if item.Session.From == source {
			counts[item.Session.To]++
		}
	}
	var routes []SessionExtension
	for _, item := range list {
		desc := item.Session
		if desc.From != source || desc.To == "" || desc.OutputDir == "" || counts[desc.To] != 1 {
			continue
		}
		if resumeCommand("", desc.To, "probe", "/work", "") == "" || !extensionTargetThere(desc.To) || !extensionSessionTargetMatches(desc) {
			continue
		}
		routes = append(routes, item)
	}
	return routes
}

func extensionTargetThere(target string) bool { return agentThere(target) || lookOK(target) }

func extensionSessionTargetMatches(descriptor sdk.SessionDescriptor) bool {
	_, err := resolveSessionTargetStore(descriptor)
	return err == nil
}

func carryExtension(source Session, route SessionExtension) (CarryResult, error) {
	return carryExtensionContext(context.Background(), source, route)
}

func carryExtensionContext(parent context.Context, source Session, route SessionExtension) (CarryResult, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return CarryResult{}, err
	}
	owner, err := selectSessionHistoryOwner(ctx, source)
	if err != nil {
		return CarryResult{}, err
	}
	return carryExtensionOwned(ctx, owner, route)
}

func carryExtensionOwned(ctx context.Context, owner sessionHistoryOwner, route SessionExtension) (CarryResult, error) {
	if err := owner.check(ctx); err != nil {
		return CarryResult{}, err
	}
	source := owner.session
	manager := extension.Default()
	manifest, identity, err := sessionMigrationIdentity(ctx, manager, route.Plugin)
	if err != nil {
		return CarryResult{}, err
	}
	owned := false
	for _, descriptor := range manifest.Sessions {
		if descriptor == route.Session {
			owned = true
		}
	}
	if !owned {
		return CarryResult{}, extension.ErrActionChanged
	}
	store, err := resolveSessionTargetStore(route.Session)
	if err != nil {
		return CarryResult{}, err
	}
	options, err := json.Marshal(map[string]string{"cwd": source.Cwd})
	if manifest.Contract.Version.Minor >= 3 {
		options, err = sessionConversionOptions(source, route.Session.To)
	}
	if err != nil {
		return CarryResult{}, err
	}
	key := sessionMigrationKey(identity, source, route, store, options)
	release, err := acquireSessionMigration(ctx, key)
	if err != nil {
		return CarryResult{}, err
	}
	defer release()
	if receipt, ok := cachedSessionMigration(key); ok {
		if sessionMigrationReceiptValid(ctx, receipt) {
			_, current, err := sessionMigrationIdentity(ctx, manager, route.Plugin)
			if err != nil {
				return CarryResult{}, err
			}
			if current != identity {
				return CarryResult{}, extension.ErrActionChanged
			}
			if sourceBeforePublish != nil {
				sourceBeforePublish()
			}
			if err := owner.check(ctx); err != nil {
				return CarryResult{}, err
			}
			return receipt.result, nil
		}
		forgetSessionMigration(key)
	}
	store, err = resolveSessionTargetStore(route.Session)
	if err != nil {
		return CarryResult{}, err
	}
	if sessionMigrationKey(identity, source, route, store, options) != key {
		return CarryResult{}, extension.ErrActionChanged
	}
	plan, snapshot, err := convertHistorySource(ctx, extension.Default(), route.Plugin, route.Session.ID, owner,
		route.Session.To, sdk.Scope{Session: source.ID, Operation: "session.convert"}, options,
		extension.ActionPolicy{Root: store.root, CreateUnder: store.prefix,
			BackupDir: filepath.Join(appdir.Config(), "extensions", "action-backups", route.Plugin, route.Session.ID),
			RevalidateInputs: func(ctx context.Context, created map[string]fs.FileInfo) error {
				if err := checkSessionTargetDuringAction(ctx, store, created); err != nil {
					return err
				}
				if manifest.Contract.Version.Minor >= 3 {
					current, err := sessionConversionOptions(source, route.Session.To)
					if err != nil {
						return err
					}
					if !bytes.Equal(current, options) {
						return extension.ErrActionChanged
					}
				}
				return nil
			}})
	if err != nil {
		return CarryResult{}, err
	}
	preview := plan.Preview()
	target := route.Session.To
	if plan.SessionID() == "" || len(preview.Files) == 0 {
		return CarryResult{}, errors.New("ordinary session conversion requires a native session id and output file")
	}
	output := filepath.Join(plan.Root(), filepath.FromSlash(preview.Files[0].Path))
	var command string
	if target == "omp" && store.profile != "" {
		if run := namedOMPResumeCommand(plan.SessionID(), store.profile); run != "" {
			command = inFolder(source.Cwd, run)
		}
	} else {
		command = resumeCommand("", target, plan.SessionID(), source.Cwd, output)
	}
	if command == "" {
		return CarryResult{}, errors.New("converted session has no safe native resume command")
	}
	if err := snapshot.check(ctx); err != nil {
		return CarryResult{}, err
	}
	if err := store.check(ctx); err != nil {
		return CarryResult{}, err
	}
	currentOptions := options
	if manifest.Contract.Version.Minor >= 3 {
		currentOptions, err = sessionConversionOptions(source, target)
		if err != nil {
			return CarryResult{}, err
		}
	}
	_, currentIdentity, err := sessionMigrationIdentity(ctx, manager, route.Plugin)
	if err != nil {
		return CarryResult{}, err
	}
	if currentIdentity != identity || !bytes.Equal(options, currentOptions) {
		return CarryResult{}, extension.ErrActionChanged
	}
	if err := owner.check(ctx); err != nil {
		return CarryResult{}, err
	}
	var post *sessionTargetStore
	var outputs *sessionInputSnapshot
	_, err = plan.ExecuteCreateOnly(ctx, preview.Revision, func(published map[string]fs.FileInfo) error {
		if sourceBeforePublish != nil {
			sourceBeforePublish()
		}
		current, err := resolveSessionTargetStore(route.Session)
		if err != nil {
			return err
		}
		if current.selection() != store.selection() {
			return extension.ErrActionChanged
		}
		captured, err := captureMigrationOutputs(ctx, current, plan, published)
		if err != nil {
			return err
		}
		post, outputs = current, captured
		return nil
	})
	if err != nil {
		return CarryResult{}, err
	}
	result := CarryResult{Agent: target, ID: plan.SessionID(), Path: output, Command: command}
	rememberSessionMigration(key, sessionMigrationReceipt{result: result, source: snapshot, target: post, outputs: outputs})
	return result, nil
}

func checkSessionTargetDuringAction(ctx context.Context, store *sessionTargetStore, created map[string]fs.FileInfo) error {
	checked := *store
	checked.dirs = slices.Clone(store.dirs)
	for index, directory := range checked.dirs {
		if directory.info == nil {
			checked.dirs[index].info = created[filepath.ToSlash(directory.path)]
		}
	}
	return checked.check(ctx)
}

func sessionConversionOptions(source Session, target string) ([]byte, error) {
	options := map[string]string{"cwd": source.Cwd}
	canonical := func(name string) string {
		if resolved, err := filepath.EvalSymlinks(name); err == nil {
			return resolved
		}
		absolute, _ := filepath.Abs(name)
		return filepath.Clean(absolute)
	}
	switch target {
	case "omp":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		options["canonicalCwd"], options["home"], options["temp"] = canonical(source.Cwd), canonical(home), canonical(os.TempDir())
	case "codex":
		provider, _, err := sessionCodexTargetConfiguration(CodexDir())
		if err != nil {
			return nil, err
		}
		options["provider"] = provider
	}
	return json.Marshal(options)
}

func SessionExtensions(host SessionExtensionRegistry) ([]SessionExtension, error) {
	installed, err := host.List()
	if err != nil {
		return nil, err
	}
	extensions := []SessionExtension{}
	owners := map[string]bool{}
	for _, entry := range installed {
		if entry.Disabled || !entry.Manifest.Contract.Has(sdk.SessionMigration) {
			continue
		}
		for _, descriptor := range entry.Manifest.Sessions {
			if owners[descriptor.ID] {
				return nil, errors.New("session converter has multiple extension owners")
			}
			owners[descriptor.ID] = true
			extensions = append(extensions, SessionExtension{Plugin: entry.Manifest.ID, Session: descriptor})
		}
	}
	return extensions, nil
}

func ConvertExtension(ctx context.Context, host extension.ActionRuntime, plugin, converter, sourceAgent, sourceID, target string, scope sdk.Scope, options []byte, policy extension.ActionPolicy) (*extension.ActionPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !Reads(sourceAgent) {
		return nil, ErrNotRead
	}
	source, exists := Find(sourceAgent, sourceID)
	if !exists {
		return nil, errors.New("no such native session")
	}
	plan, _, err := convertExtensionSource(ctx, host, plugin, converter, source, target, scope, options, policy)
	return plan, err
}

// The selected record is authoritative: never resolve its ID to another store.
// Public explicit calls resolve once above; ordinary carry passes its selection.
func convertExtensionSource(ctx context.Context, host extension.ActionRuntime, plugin, converter string, source Session, target string, scope sdk.Scope, options []byte, policy extension.ActionPolicy) (*extension.ActionPlan, *sessionSourceSnapshot, error) {
	owner, err := selectSessionHistoryOwner(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	return convertHistorySource(ctx, host, plugin, converter, owner, target, scope, options, policy)
}

func convertHistorySource(ctx context.Context, host extension.ActionRuntime, plugin, converter string, owner sessionHistoryOwner, target string, scope sdk.Scope, options []byte, policy extension.ActionPolicy) (*extension.ActionPlan, *sessionSourceSnapshot, error) {
	source := owner.session
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := owner.check(ctx); err != nil {
		return nil, nil, err
	}
	admissionContext := ctx
	ctx = withSessionSourceAdmission(ctx, func() error { return owner.check(admissionContext) })
	if source.Agent == "" || source.ID == "" || source.ReadOnly || source.WSL != "" ||
		!filepath.IsAbs(source.Path) || source.Cwd == "" {
		return nil, nil, errors.New("this session cannot be converted here")
	}
	if scope.Session != "" && scope.Session != source.ID {
		return nil, nil, errors.New("extension parent scope belongs to another session")
	}
	scope.Session = source.ID
	manager, managed := host.(*extension.Manager)
	identity := ""
	if managed {
		_, current, err := sessionMigrationIdentity(ctx, manager, plugin)
		if err != nil {
			return nil, nil, err
		}
		identity = current
	}
	snapshot, body, err := readSessionSource(ctx, source.Path)
	if err != nil {
		return nil, nil, err
	}
	if source.Agent == "codex" && packed(source.Path) {
		decoder, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(extension.MaxActionBytes))
		if err != nil {
			return nil, nil, err
		}
		body, err = io.ReadAll(io.LimitReader(decoder, extension.MaxActionBytes+1))
		decoder.Close()
		if err != nil {
			return nil, nil, err
		}
		if len(body) > extension.MaxActionBytes {
			return nil, nil, errors.New("unpacked session source exceeds its byte budget")
		}
		if err := snapshot.check(ctx); err != nil {
			return nil, nil, err
		}
	}
	request := sdk.SessionRequest{ID: converter, From: source.Agent, To: target, Options: options,
		Files: []sdk.SessionDocument{{Path: filepath.Base(source.Path), Format: source.Agent, Body: body}}}
	host, release, err := sessionSourceRuntime(ctx, host, plugin)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	if err := collectSessionSourceInputs(ctx, host, plugin, scope, source, &request, snapshot); err != nil {
		return nil, nil, err
	}
	previous := policy.RevalidateInputs
	policy.InputIdentity += "\x00" + identity
	checkAuthority := func(ctx context.Context) error {
		if err := checkSessionSourceAdmission(ctx); err != nil {
			return err
		}
		if err := owner.check(ctx); err != nil {
			return err
		}
		if managed {
			_, current, err := sessionMigrationIdentity(ctx, manager, plugin)
			if err != nil {
				return err
			}
			if current != identity {
				return extension.ErrActionChanged
			}
		}
		return nil
	}
	policy.RevalidateInputs = func(ctx context.Context, created map[string]fs.FileInfo) error {
		if err := checkAuthority(ctx); err != nil {
			return err
		}
		if err := snapshot.check(ctx); err != nil {
			return err
		}
		if previous != nil {
			if err := previous(ctx, created); err != nil {
				return err
			}
		}
		return checkAuthority(ctx)
	}
	if err := policy.RevalidateInputs(ctx, nil); err != nil {
		return nil, nil, err
	}
	plan, err := extension.NewActions(host).SessionConvert(ctx, plugin, scope, request, policy)
	if err != nil {
		return nil, nil, err
	}
	if err := policy.RevalidateInputs(ctx, nil); err != nil {
		return nil, nil, err
	}
	return plan, snapshot, nil
}

type sessionSourceSnapshot struct {
	path   string
	info   os.FileInfo
	hash   [sha256.Size]byte
	inputs *sessionInputSnapshot
}

func (snapshot *sessionSourceSnapshot) check(ctx context.Context) error {
	current, _, err := readSessionSource(ctx, snapshot.path)
	if err != nil {
		return err
	}
	if !os.SameFile(snapshot.info, current.info) || snapshot.hash != current.hash {
		return extension.ErrActionChanged
	}
	if snapshot.inputs != nil {
		return snapshot.inputs.check(ctx)
	}
	return nil
}

func readSessionSource(ctx context.Context, path string) (*sessionSourceSnapshot, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > extension.MaxActionBytes {
		return nil, nil, errors.New("session conversion requires a bounded regular source file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, nil, extension.ErrActionChanged
	}
	body, err := io.ReadAll(io.LimitReader(file, extension.MaxActionBytes+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil || len(body) > extension.MaxActionBytes || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, nil, extension.ErrActionChanged
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return nil, nil, extension.ErrActionChanged
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return &sessionSourceSnapshot{path: path, info: opened, hash: sha256.Sum256(body)}, body, nil
}
