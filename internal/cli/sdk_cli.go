package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const sdkPluginUsage = `usage: magpie plugin sdk list [--json]
       magpie plugin sdk add|install <directory | github:owner/repo[@tag]>
       magpie plugin sdk update [<module>]
       magpie plugin sdk inspect <module>
       magpie plugin sdk config <module>
       magpie plugin sdk config <module> <json-object> --expected <version>
       magpie plugin sdk on|off|rm|reload <module>
       magpie plugin sdk call|invoke <module> <method> <json> [--scope <json>]
       magpie plugin sdk accounts <module> <provider> [--json]
       magpie plugin sdk auth start|finish <module> <provider> <json> [--scope <json>]
       magpie plugin sdk auth refresh|logout <module> <provider> <account> [--scope <json>]
       magpie plugin sdk agent <module> <json> --root <absolute-directory> --path <relative-file>... [--scope <json>] [--apply --backup-dir <absolute-directory>]
       magpie plugin sdk session <module> <json> --root <absolute-directory> --path <relative-file>... [--scope <json>] [--apply --backup-dir <absolute-directory>]

call/invoke emits JSON event records followed by a result record.
Account scopes resolve the current saved generation; an explicitly stale generation is refused.
agent takes an AgentPlanRequest; session takes a SessionRequest with base64 document bodies.
agent/session preview the bounded host plan. Only --apply writes files, with an explicit backup directory.
GitHub sources use release ZIPs for this platform and require checksums.txt; @tag pins a release.
update follows saved unpinned sources; pinned and local packages stay unchanged.
config reads SDK 1.1 module configuration without starting a worker.
config writes replace the whole object and require the CAS version from a previous read.
Removing a module retains its installed generations and private state.`

type sdkCLIHost interface {
	extensionhost.ActionRuntime
	List() ([]extensionhost.Installed, error)
	Install(context.Context, string) (extensionhost.Installed, error)
	Update(context.Context, string) (extensionhost.Installed, error)
	SetEnabled(context.Context, string, bool) error
	Remove(context.Context, string) error
	Restart(context.Context, string) error
	ModuleConfig(context.Context, string) (sdk.ConfigSnapshot, error)
	CompareAndSwapModuleConfig(context.Context, string, string, json.RawMessage) (sdk.ConfigSnapshot, error)
	Call(context.Context, string, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error)
	Close(context.Context) error
}

type sdkCLIAuth struct {
	start   func(context.Context, sdk.Scope, sdk.AuthStartRequest) (sdk.AuthStartResult, error)
	finish  func(context.Context, sdk.Scope, sdk.AuthFinishRequest) (sdk.AuthResult, error)
	refresh func(context.Context, sdk.Scope) (sdk.AuthResult, error)
	logout  func(context.Context, sdk.Scope) error
}

type sdkCLIOptions struct {
	args     []string
	json     bool
	scope    sdk.Scope
	root     string
	paths    []string
	backup   string
	apply    bool
	expected string
}

func sdkPluginCmd(ctx context.Context, args []string) error {
	if sdkCLIHelp(args) {
		_, err := fmt.Fprintln(os.Stdout, sdkPluginUsage)
		return err
	}
	return sdkPluginRun(ctx, args, extensionhost.Default(), os.Stdout, sdkCLIAuth{
		start: provider.StartExtensionAuth, finish: provider.FinishExtensionAuth,
		refresh: provider.RefreshExtensionAuth, logout: provider.LogoutExtensionAuth,
	})
}

func sdkCLIHelp(args []string) bool {
	return len(args) == 0 || args[0] == "help" || slices.Contains(args, "-h") || slices.Contains(args, "--help")
}

func sdkPluginRun(ctx context.Context, args []string, host sdkCLIHost, out io.Writer, auth sdkCLIAuth) (resultErr error) {
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, host.Close(closeCtx))
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if sdkCLIHelp(args) {
		_, err := fmt.Fprintln(out, sdkPluginUsage)
		return err
	}
	command, rest := args[0], args[1:]
	switch command {
	case "list", "ls":
		options, err := sdkCLIFlags(rest, "--json")
		if err != nil {
			return err
		}
		if len(options.args) != 0 {
			return sdkCLIUsage("list takes no module argument")
		}
		entries, err := host.List()
		if err != nil {
			return err
		}
		if entries == nil {
			entries = []extensionhost.Installed{}
		}
		if options.json {
			return sdkCLIJSON(out, entries)
		}
		if len(entries) == 0 {
			_, err := fmt.Fprintln(out, "No SDK modules installed. Add one: magpie plugin sdk add <directory | github:owner/repo[@tag]>")
			return err
		}
		for _, entry := range entries {
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", entry.Manifest.ID, entry.Manifest.Version, entry.Manifest.Runtime, sdkCLIEnabled(entry)); err != nil {
				return err
			}
		}
		return nil
	case "update":
		return sdkCLIUpdate(ctx, rest, host, out)
	case "config":
		return sdkCLIConfig(ctx, rest, host, out)
	case "add", "install", "inspect", "on", "off", "rm", "remove", "reload":
		options, err := sdkCLIFlags(rest)
		if err != nil {
			return err
		}
		if len(options.args) != 1 {
			return sdkCLIUsage(command + " requires exactly one package source or module")
		}
		id := options.args[0]
		switch command {
		case "add", "install":
			entry, err := host.Install(ctx, id)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "installed %s %s (%s)\n", entry.Manifest.ID, entry.Manifest.Version, sdkCLIEnabled(entry))
			return err
		case "inspect":
			entry, err := sdkCLIInstalled(host, id)
			if err != nil {
				return err
			}
			return sdkCLIJSON(out, entry)
		case "on", "off":
			if err := host.SetEnabled(ctx, id, command == "on"); err != nil {
				return err
			}
		case "rm", "remove":
			if err := host.Remove(ctx, id); err != nil {
				return err
			}
		case "reload":
			if err := host.Restart(ctx, id); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(out, "%s %s\n", id, command)
		return err
	case "call", "invoke":
		return sdkCLICall(ctx, rest, host, out)
	case "accounts":
		return sdkCLIAccounts(rest, host, out)
	case "auth":
		return sdkCLIAuthCommand(ctx, rest, host, auth, out)
	case "agent", "session":
		return sdkCLIAction(ctx, command, rest, host, out)
	default:
		return sdkCLIUsage("unknown SDK command")
	}
}

func sdkCLIConfig(ctx context.Context, args []string, host sdkCLIHost, out io.Writer) error {
	options, err := sdkCLIFlags(args, "--expected")
	if err != nil {
		return err
	}
	if len(options.args) < 1 || len(options.args) > 2 {
		return sdkCLIUsage("config requires a module and optional JSON object with --expected")
	}
	id := options.args[0]
	if len(options.args) == 1 {
		if options.expected != "" {
			return sdkCLIUsage("config --expected requires a JSON object")
		}
		snapshot, err := host.ModuleConfig(ctx, id)
		if err != nil {
			return err
		}
		return sdkCLIJSON(out, snapshot)
	}
	if options.expected == "" {
		return sdkCLIUsage("config writes require an explicit --expected version")
	}
	version, err := strconv.ParseUint(options.expected, 10, 64)
	if err != nil || strconv.FormatUint(version, 10) != options.expected {
		return errors.New("SDK configuration requires a canonical CAS version")
	}
	var object map[string]json.RawMessage
	if err := sdkCLIDecode(options.args[1], &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("SDK configuration must be a JSON object")
	}
	snapshot, err := host.CompareAndSwapModuleConfig(ctx, id, options.expected, json.RawMessage(options.args[1]))
	if err != nil {
		return err
	}
	return sdkCLIJSON(out, snapshot)
}

func sdkCLIEnabled(entry extensionhost.Installed) string {
	if entry.Disabled {
		return "off"
	}
	return "on"
}

func sdkCLIUpdate(ctx context.Context, args []string, host sdkCLIHost, out io.Writer) error {
	options, err := sdkCLIFlags(args)
	if err != nil {
		return err
	}
	if len(options.args) > 1 {
		return sdkCLIUsage("update takes at most one module")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := host.List()
	if err != nil {
		return err
	}
	if len(options.args) == 1 {
		id := options.args[0]
		index := slices.IndexFunc(entries, func(entry extensionhost.Installed) bool {
			return entry.Manifest.ID == id
		})
		if index < 0 {
			return fmt.Errorf("unknown SDK module %q", id)
		}
		entries = entries[index : index+1]
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(out, "No SDK modules installed.")
		return err
	}
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		id := entry.Manifest.ID
		switch {
		case entry.Source == nil:
			_, err = fmt.Fprintf(out, "%s skipped: local package has no update source\n", id)
		case strings.Contains(entry.Source.Reference, "@"):
			_, pin, _ := strings.Cut(entry.Source.Reference, "@")
			_, err = fmt.Fprintf(out, "%s unchanged: pinned to %s\n", id, pin)
		default:
			updated, updateErr := host.Update(ctx, id)
			if updateErr != nil {
				failures = append(failures, fmt.Errorf("%s: %w", id, updateErr))
				if ctx.Err() != nil {
					return errors.Join(failures...)
				}
				continue
			}
			status := "updated to"
			if updated.Revision == entry.Revision {
				status = "unchanged at"
			}
			_, err = fmt.Fprintf(out, "%s %s %s (%s)\n", id, status, updated.Manifest.Version, sdkCLIEnabled(updated))
		}
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	return errors.Join(failures...)
}

func sdkCLIInstalled(host sdkCLIHost, id string) (extensionhost.Installed, error) {
	entries, err := host.List()
	if err != nil {
		return extensionhost.Installed{}, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID == id {
			return entry, nil
		}
	}
	return extensionhost.Installed{}, fmt.Errorf("unknown SDK module %q", id)
}

func sdkCLICall(ctx context.Context, args []string, host sdkCLIHost, out io.Writer) error {
	options, err := sdkCLIFlags(args, "--scope")
	if err != nil {
		return err
	}
	if len(options.args) != 3 {
		return sdkCLIUsage("call requires a module, method and JSON input")
	}
	id, method, input := options.args[0], options.args[1], options.args[2]
	if err := sdkCLIInput(input); err != nil {
		return err
	}
	scope, err := sdkCLIScope(host, id, options.scope)
	if err != nil {
		return err
	}
	ctx, err = provider.ExtensionNetworkContext(ctx, scope)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	result, err := host.Call(ctx, id, method, scope, json.RawMessage(input), func(event extensionhost.Event) error {
		return encoder.Encode(struct {
			Kind  string          `json:"kind"`
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data,omitempty"`
		}{"event", event.Name, event.Data})
	})
	if err != nil {
		return err
	}
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	return encoder.Encode(struct {
		Kind   string          `json:"kind"`
		Result json.RawMessage `json:"result"`
	}{"result", result})
}

func sdkCLIAccounts(args []string, host sdkCLIHost, out io.Writer) error {
	options, err := sdkCLIFlags(args, "--json")
	if err != nil {
		return err
	}
	if len(options.args) != 2 {
		return sdkCLIUsage("accounts requires a module and provider")
	}
	entry, err := sdkCLIInstalled(host, options.args[0])
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(entry.Manifest.Providers, func(descriptor sdk.ProviderDescriptor) bool {
		return descriptor.ID == options.args[1]
	}) {
		return errors.New("SDK module does not declare that provider")
	}
	accounts, err := host.Store().Accounts(entry.Manifest.ID, options.args[1])
	if err != nil {
		return err
	}
	type accountView struct {
		Plugin     string `json:"plugin"`
		Provider   string `json:"provider"`
		ID         string `json:"id"`
		Generation uint64 `json:"generation"`
	}
	views := make([]accountView, len(accounts))
	for index, account := range accounts {
		views[index] = accountView{account.Plugin, account.Provider, account.ID, account.Generation}
	}
	if options.json {
		return sdkCLIJSON(out, views)
	}
	if len(views) == 0 {
		_, err := fmt.Fprintln(out, "No saved SDK accounts.")
		return err
	}
	for _, account := range views {
		if _, err := fmt.Fprintf(out, "%s\tgeneration %d\n", account.ID, account.Generation); err != nil {
			return err
		}
	}
	return nil
}

func sdkCLIScope(host sdkCLIHost, id string, scope sdk.Scope) (sdk.Scope, error) {
	if scope.Plugin != "" && scope.Plugin != id {
		return sdk.Scope{}, errors.New("SDK scope belongs to another module")
	}
	scope.Plugin = id
	if scope.Account == "" {
		if scope.Generation != 0 {
			return sdk.Scope{}, errors.New("SDK account generation requires an account")
		}
		return scope, nil
	}
	if scope.Provider == "" {
		return sdk.Scope{}, errors.New("SDK account scope requires a provider")
	}
	account, err := host.Store().GetAccount(id, scope.Provider, scope.Account)
	if err != nil {
		return sdk.Scope{}, err
	}
	if scope.Generation != 0 && scope.Generation != account.Generation {
		return sdk.Scope{}, errors.New("SDK account generation changed")
	}
	scope.Generation = account.Generation
	return scope, nil
}

func sdkCLIAuthCommand(ctx context.Context, args []string, host sdkCLIHost, auth sdkCLIAuth, out io.Writer) error {
	if len(args) == 0 {
		return sdkCLIUsage("auth requires start, finish, refresh or logout")
	}
	operation := args[0]
	if operation != "start" && operation != "finish" && operation != "refresh" && operation != "logout" {
		return sdkCLIUsage("unknown SDK authentication command")
	}
	options, err := sdkCLIFlags(args[1:], "--scope")
	if err != nil {
		return err
	}
	if len(options.args) != 3 {
		return sdkCLIUsage("auth requires a module, provider and JSON input or account")
	}
	id, providerID, input := options.args[0], options.args[1], options.args[2]
	if options.scope.Provider != "" && options.scope.Provider != providerID {
		return errors.New("SDK scope belongs to another provider")
	}
	options.scope.Provider = providerID
	if operation == "refresh" || operation == "logout" {
		if options.scope.Account != "" && options.scope.Account != input {
			return errors.New("SDK scope belongs to another account")
		}
		options.scope.Account = input
	}
	scope, err := sdkCLIScope(host, id, options.scope)
	if err != nil {
		return err
	}
	switch operation {
	case "start":
		var request sdk.AuthStartRequest
		if err := sdkCLIDecode(input, &request); err != nil {
			return err
		}
		result, err := auth.start(ctx, scope, request)
		if err != nil {
			return err
		}
		return sdkCLIJSON(out, result)
	case "finish":
		var request sdk.AuthFinishRequest
		if err := sdkCLIDecode(input, &request); err != nil {
			return err
		}
		result, err := auth.finish(ctx, scope, request)
		if err != nil {
			return err
		}
		result.Credentials = nil
		return sdkCLIJSON(out, result)
	case "refresh":
		result, err := auth.refresh(ctx, scope)
		if err != nil {
			return err
		}
		result.Credentials = nil
		return sdkCLIJSON(out, result)
	case "logout":
		if err := auth.logout(ctx, scope); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "%s signed out of %s/%s\n", input, id, providerID)
		return err
	}
	return nil
}

func sdkCLIAction(ctx context.Context, command string, args []string, host sdkCLIHost, out io.Writer) error {
	options, err := sdkCLIFlags(args, "--scope", "--root", "--path", "--apply", "--backup-dir")
	if err != nil {
		return err
	}
	if len(options.args) != 2 || !filepath.IsAbs(options.root) || len(options.paths) == 0 {
		return sdkCLIUsage(command + " requires a module, JSON input, an absolute --root and explicit --path entries")
	}
	if options.apply && !filepath.IsAbs(options.backup) {
		return sdkCLIUsage("--apply requires an absolute --backup-dir")
	}
	scope, err := sdkCLIScope(host, options.args[0], options.scope)
	if err != nil {
		return err
	}
	policy := extensionhost.ActionPolicy{Root: options.root, Paths: options.paths, BackupDir: options.backup}
	actions := extensionhost.NewActions(host)
	var plan *extensionhost.ActionPlan
	if command == "agent" {
		var request sdk.AgentPlanRequest
		if err := sdkCLIDecode(options.args[1], &request); err != nil {
			return err
		}
		plan, err = actions.AgentPlan(ctx, options.args[0], scope, request, policy)
	} else {
		var request sdk.SessionRequest
		if err := sdkCLIDecode(options.args[1], &request); err != nil {
			return err
		}
		plan, err = actions.SessionConvert(ctx, options.args[0], scope, request, policy)
	}
	if err != nil {
		return err
	}
	preview := plan.Preview()
	if !options.apply {
		return sdkCLIJSON(out, preview)
	}
	result, err := plan.Execute(ctx, preview.Revision)
	if err != nil {
		return err
	}
	return sdkCLIJSON(out, result)
}

func sdkCLIFlags(args []string, allowed ...string) (sdkCLIOptions, error) {
	var options sdkCLIOptions
	seen := map[string]bool{}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") || json.Valid([]byte(argument)) {
			options.args = append(options.args, argument)
			continue
		}
		flag, value, hasValue := strings.Cut(argument, "=")
		if !slices.Contains(allowed, flag) || seen[flag] && flag != "--path" {
			return options, sdkCLIUsage("unknown or repeated SDK option")
		}
		seen[flag] = true
		switch flag {
		case "--json", "--apply":
			if hasValue {
				return options, sdkCLIUsage(flag + " takes no value")
			}
			if flag == "--json" {
				options.json = true
			} else {
				options.apply = true
			}
		default:
			if !hasValue {
				index++
				if index >= len(args) || strings.HasPrefix(args[index], "--") {
					return options, sdkCLIUsage(flag + " requires a value")
				}
				value = args[index]
			}
			if value == "" {
				return options, sdkCLIUsage(flag + " requires a nonempty value")
			}
			switch flag {
			case "--scope":
				if len(value) > sdk.MaxChunkBytes {
					return options, errors.New("SDK scope exceeds its byte limit")
				}
				if err := sdkCLIDecode(value, &options.scope); err != nil {
					return options, err
				}
			case "--root":
				options.root = value
			case "--path":
				options.paths = append(options.paths, value)
			case "--backup-dir":
				options.backup = value
			case "--expected":
				options.expected = value
			}
		}
	}
	return options, nil
}

func sdkCLIInput(input string) error {
	if len(input) > sdk.MaxFrameBytes-4096 || !json.Valid([]byte(input)) {
		return errors.New("SDK input must be bounded valid JSON")
	}
	return nil
}

func sdkCLIDecode(input string, target any) error {
	if err := sdkCLIInput(input); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace([]byte(input)), []byte("null")) {
		return errors.New("SDK request must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid SDK request: %w", err)
	}
	return nil
}

func sdkCLIJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func sdkCLIUsage(reason string) error {
	return fmt.Errorf("%s\n\n%s", reason, sdkPluginUsage)
}
