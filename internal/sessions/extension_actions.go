package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// Listing never invokes a converter. Native routes keep precedence;
// ambiguous SDK routes are unavailable.
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
		if desc.From != source || desc.To == "" || desc.OutputDir == "" || counts[desc.To] != 1 ||
			convertsTo[source] == desc.To {
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
	if !Reads(source.Agent) {
		return CarryResult{}, ErrNotRead
	}
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
			if !Reads(source.Agent) {
				return CarryResult{}, ErrNotRead
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
	plan, snapshot, err := convertExtensionSource(ctx, extension.Default(), route.Plugin, route.Session.ID, source,
		route.Session.To, sdk.Scope{Session: source.ID, Operation: "session.convert"}, options,
		extension.ActionPolicy{Root: store.root, CreateUnder: store.prefix,
			BackupDir: filepath.Join(appdir.Config(), "extensions", "action-backups", route.Plugin, route.Session.ID)})
	if err != nil {
		return CarryResult{}, err
	}
	preview := plan.Preview()
	target := route.Session.To
	if plan.SessionID() == "" || len(preview.Files) == 0 {
		return CarryResult{}, errors.New("ordinary session conversion requires a native session id and output file")
	}
	output := filepath.Join(plan.Root(), filepath.FromSlash(preview.Files[0].Path))
	command := resumeCommand("", target, plan.SessionID(), source.Cwd, output)
	if target == "omp" && store.profile != "" && command != "" {
		command = inFolder(source.Cwd, "omp --profile "+store.profile+" --resume "+plan.SessionID())
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
	if !Reads(source.Agent) {
		return CarryResult{}, ErrNotRead
	}
	if _, err := plan.Execute(ctx, preview.Revision); err != nil {
		return CarryResult{}, err
	}
	post, err := resolveSessionTargetStore(route.Session)
	if err != nil {
		return CarryResult{}, err
	}
	if post.selection() != store.selection() {
		return CarryResult{}, extension.ErrActionChanged
	}
	outputs, err := captureMigrationOutputs(ctx, post, plan)
	if err != nil {
		return CarryResult{}, err
	}
	result := CarryResult{Agent: target, ID: plan.SessionID(), Path: output, Command: command}
	rememberSessionMigration(key, sessionMigrationReceipt{result: result, source: snapshot, target: post, outputs: outputs})
	return result, nil
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
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !Reads(source.Agent) {
		return nil, nil, ErrNotRead
	}
	if source.Agent == "" || source.ID == "" || source.ReadOnly || source.WSL != "" ||
		!filepath.IsAbs(source.Path) || source.Cwd == "" {
		return nil, nil, errors.New("this session cannot be converted here")
	}
	if scope.Session != "" && scope.Session != source.ID {
		return nil, nil, errors.New("extension parent scope belongs to another session")
	}
	scope.Session = source.ID
	snapshot, body, err := readSessionSource(ctx, source.Path)
	if err != nil {
		return nil, nil, err
	}
	if source.Agent == "codex" && packed(source.Path) {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(extension.MaxActionBytes))
		if err != nil {
			return nil, nil, err
		}
		body, err = decoder.DecodeAll(body, nil)
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
	plan, err := extension.NewActions(host).SessionConvert(ctx, plugin, scope, request, policy)
	if err != nil {
		return nil, nil, err
	}
	if err := snapshot.check(ctx); err != nil {
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
