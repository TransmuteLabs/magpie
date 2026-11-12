package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

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
		if resumeCommand("", desc.To, "probe", "/work", "") == "" || !extensionTargetThere(desc.To) {
			continue
		}
		routes = append(routes, item)
	}
	return routes
}

func extensionTargetThere(target string) bool { return agentThere(target) || lookOK(target) }

func carryExtension(source Session, route SessionExtension) (CarryResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return CarryResult{}, err
	}
	options, err := json.Marshal(struct {
		Cwd string `json:"cwd"`
	}{source.Cwd})
	if err != nil {
		return CarryResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plan, snapshot, err := convertExtensionSource(ctx, extension.Default(), route.Plugin, route.Session.ID, source,
		route.Session.To, sdk.Scope{Session: source.ID, Operation: "session.convert"}, options,
		extension.ActionPolicy{Root: home, CreateUnder: route.Session.OutputDir,
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
	if command == "" {
		return CarryResult{}, errors.New("converted session has no safe native resume command")
	}
	if err := snapshot.check(ctx); err != nil {
		return CarryResult{}, err
	}
	if _, err := plan.Execute(ctx, preview.Revision); err != nil {
		return CarryResult{}, err
	}
	return CarryResult{Agent: target, ID: plan.SessionID(), Path: output, Command: command}, nil
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
	request := sdk.SessionRequest{ID: converter, From: source.Agent, To: target, Options: options,
		Files: []sdk.SessionDocument{{Path: filepath.Base(source.Path), Format: source.Agent, Body: body}}}
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
	path string
	info os.FileInfo
	hash [sha256.Size]byte
}

func (snapshot *sessionSourceSnapshot) check(ctx context.Context) error {
	current, _, err := readSessionSource(ctx, snapshot.path)
	if err != nil {
		return err
	}
	if !os.SameFile(snapshot.info, current.info) || snapshot.hash != current.hash {
		return extension.ErrActionChanged
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
