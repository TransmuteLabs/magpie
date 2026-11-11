package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/mcpauth"
	"github.com/yetone/magpie/internal/plugin"
)

const pluginMCPMeta = "com.zcode/official-mcp-auth"

type pluginAccountTargetKey struct{}
type pluginAccountExpectationKey struct{}
type pluginAccountBindingKey struct{}

type pluginAccountExpectation struct {
	scope, generation string
}

// WithPluginAccountTarget requires the server and command acknowledged by a
// caller to still be current before a binding obtains credentials.
func WithPluginAccountTarget(ctx context.Context, fingerprint string) context.Context {
	return context.WithValue(ctx, pluginAccountTargetKey{}, fingerprint)
}

// WithPluginAccountExpectation fences a binding to a captured account.
// Existing callers without an expectation keep their current-at-call behavior.
func WithPluginAccountExpectation(ctx context.Context, scope, generation string) context.Context {
	return context.WithValue(ctx, pluginAccountExpectationKey{}, pluginAccountExpectation{scope, generation})
}

// WithPluginAccountBinding makes unbinding conditional on the displayed binding.
func WithPluginAccountBinding(ctx context.Context, binding PluginAccount) context.Context {
	return context.WithValue(ctx, pluginAccountBindingKey{}, binding)
}

// PluginAccountTarget is the saved destination and binding to be acknowledged.
// Command and argument references are shown as configured; expanded values,
// environment values and HTTP headers are not returned. The fingerprint covers
// the expanded command and configured environment, along with the binding.
type PluginAccountTarget struct {
	Transport   string         `json:"transport"`
	URL         string         `json:"url,omitempty"`
	Command     string         `json:"command,omitempty"`
	Args        []string       `json:"args,omitempty"`
	Environment []string       `json:"environment,omitempty"`
	Fingerprint string         `json:"fingerprint"`
	Binding     *PluginAccount `json:"binding,omitempty"`
}

func combinedAccountTarget(s *Server, commandDigest string) string {
	raw := accountTargetFingerprint(s)
	h := sha256.New()
	h.Write([]byte("magpie-plugin-account-target\x00"))
	h.Write(raw[:])
	h.Write([]byte(commandDigest))
	h.Write([]byte{0})
	binding, _ := json.Marshal(s.PluginAccount)
	h.Write(binding)
	return hex.EncodeToString(h.Sum(nil))
}

func pluginAccountServer(name string) (*Server, error) {
	mu.Lock()
	defer mu.Unlock()
	l, err := load()
	if err != nil {
		return nil, err
	}
	s := l.server(name)
	if s == nil {
		return nil, fmt.Errorf("no server called %s", name)
	}
	snapshot := *s
	if s.PluginAccount != nil {
		binding := *s.PluginAccount
		snapshot.PluginAccount = &binding
	}
	return &snapshot, nil
}

// LibraryPluginAccountBinding reads the displayed binding without hashing the
// server's command; the actual unbind compares it again inside change.
func LibraryPluginAccountBinding(name string) (*PluginAccount, error) {
	s, err := pluginAccountServer(name)
	if err != nil {
		return nil, err
	}
	return s.PluginAccount, nil
}

// LibraryPluginAccountTarget snapshots the saved server under the Library
// lock, then inspects command files outside it.
func LibraryPluginAccountTarget(name string) (PluginAccountTarget, error) {
	snapshot, err := pluginAccountServer(name)
	if err != nil {
		return PluginAccountTarget{}, err
	}
	target := PluginAccountTarget{Transport: snapshot.Transport, Binding: snapshot.PluginAccount}
	var digest string
	switch snapshot.Transport {
	case "http":
		req, err := http.NewRequest(http.MethodPost, snapshot.URL, nil)
		if err != nil || req.URL.User != nil {
			return PluginAccountTarget{}, errors.New("invalid plugin-account MCP URL")
		}
		target.URL = req.URL.String()
	case "stdio":
		target.Command, target.Args = snapshot.Command, slices.Clone(snapshot.Args)
		digest, err = commandTrustDigest(snapshot)
		if err != nil {
			return PluginAccountTarget{}, fmt.Errorf("cannot establish command trust: %w", err)
		}
		for key := range snapshot.Env {
			target.Environment = append(target.Environment, key)
		}
		slices.Sort(target.Environment)
	default:
		return PluginAccountTarget{}, errors.New("a plugin account requires HTTP or a trusted command")
	}
	target.Fingerprint = combinedAccountTarget(snapshot, digest)
	return target, nil
}

func validPluginMCPServer(s *Server) bool {
	a := s.PluginAccount
	return a != nil && a.Provider != "" && a.Account != "" && a.Scope != "" &&
		(s.Transport == "http" || s.Transport == "stdio") && s.Hub != HubDirect &&
		s.Plugin == "" && s.Bridge == "" && s.OAuth == nil &&
		(s.Transport != "stdio" || a.CommandDigest != "")
}

// ServerPluginAccount binds a verified identity to a server, or removes the
// binding. The account hook itself establishes trust before looking up auth.
func ServerPluginAccount(ctx context.Context, name string, account *PluginAccount, trustCommand bool) (*Result, error) {
	if account == nil {
		res, err := change(func(l *Library) error {
			s := l.server(name)
			if s == nil {
				return fmt.Errorf("no server called %s", name)
			}
			if expected, ok := ctx.Value(pluginAccountBindingKey{}).(PluginAccount); ok &&
				(s.PluginAccount == nil || *s.PluginAccount != expected) {
				return errors.New("the account binding changed; reopen it before unbinding")
			}
			s.PluginAccount = nil
			return nil
		})
		if err == nil {
			invalidatePluginMCP(name)
		}
		return res, err
	}
	a := PluginAccount{Provider: strings.TrimSpace(account.Provider), Account: strings.TrimSpace(account.Account)}
	if a.Provider == "" || a.Account == "" || strings.ContainsAny(a.Provider+a.Account, "\r\n") {
		return nil, errors.New("a plugin provider and explicit account are required")
	}
	expected, fenced := ctx.Value(pluginAccountExpectationKey{}).(pluginAccountExpectation)
	if fenced && (expected.scope == "" || expected.generation == "" || plugin.ProviderOf(a.Account) != a.Provider) {
		return nil, errors.New("a captured identity and explicit account of the provider are required")
	}
	target, hasTarget := ctx.Value(pluginAccountTargetKey{}).(string)
	if hasTarget && target == "" {
		return nil, errors.New("a saved server target fingerprint is required")
	}
	mu.Lock()
	l, err := load()
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	s := l.server(name)
	if s == nil {
		mu.Unlock()
		return nil, fmt.Errorf("no server called %s", name)
	}
	snapshot := *s
	fingerprint := accountTargetFingerprint(s)
	mu.Unlock()
	if snapshot.Hub == HubDirect || snapshot.Plugin != "" || snapshot.Bridge != "" || snapshot.OAuth != nil {
		return nil, errors.New("a plugin account requires a shared MCP server without a separate MCP OAuth app")
	}
	var params map[string]any
	switch snapshot.Transport {
	case "http":
		if trustCommand {
			return nil, errors.New("--trust-command is only for a command server")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, snapshot.URL, nil)
		if err != nil {
			return nil, errors.New("invalid plugin-account MCP URL")
		}
		if req.URL.User != nil {
			return nil, errors.New("plugin-account MCP URLs cannot contain user information")
		}
		params = map[string]any{"url": req.URL.String()}
	case "stdio":
		if !trustCommand {
			return nil, errors.New("binding a command requires --trust-command: the command receives account credentials")
		}
		a.CommandDigest, err = commandTrustDigest(&snapshot)
		if err != nil {
			return nil, fmt.Errorf("cannot establish command trust: %w", err)
		}
		params = map[string]any{"stdio": true}
	default:
		return nil, errors.New("a plugin account requires HTTP or a trusted command")
	}
	if hasTarget && combinedAccountTarget(&snapshot, a.CommandDigest) != target {
		return nil, errors.New("the MCP server, command or account binding changed since it was reviewed; review it again")
	}
	if hasTarget {
		current, err := pluginAccountServer(name)
		if err != nil || combinedAccountTarget(current, a.CommandDigest) != target {
			return nil, errors.New("the MCP server or account binding changed since it was reviewed; review it again")
		}
	}
	result, err := pluginMCPAccountOperation(ctx, a.Provider, a.Account, plugin.AccountRequest{
		Operation: "mcp.headers", Params: params, Scope: expected.scope, Generation: expected.generation,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot bind plugin account: destination validation or account authorization failed")
	}
	if result.Context.Scope == "" || result.Context.Generation == "" {
		return nil, errors.New("cannot bind plugin account without a verified account identity")
	}
	if fenced && (result.Context.Scope != expected.scope || result.Context.Generation != expected.generation) {
		return nil, errors.New("the selected account changed; review it again")
	}
	if _, err := pluginMCPHeaders(result.Data); err != nil {
		return nil, err
	}
	a.Scope = result.Context.Scope
	if snapshot.Transport == "http" && mcpauth.SignedIn(snapshot.Name, snapshot.URL) {
		return nil, errors.New("sign out of the server's separate MCP OAuth login before binding a plugin account")
	}
	if fenced {
		current, err := pluginMCPAccountOperation(ctx, a.Provider, a.Account, plugin.AccountRequest{
			Operation: "context", Scope: expected.scope, Generation: expected.generation,
		})
		if err != nil || current.Context.Scope != expected.scope || current.Context.Generation != expected.generation {
			return nil, errors.New("the selected account changed while it was being bound")
		}
	}
	if snapshot.Transport == "stdio" {
		current, err := commandTrustDigest(&snapshot)
		if err != nil || current != a.CommandDigest {
			return nil, errors.New("the command changed while its account was being bound")
		}
	}
	res, err := change(func(l *Library) error {
		s := l.server(name)
		if s == nil || accountTargetFingerprint(s) != fingerprint ||
			hasTarget && combinedAccountTarget(s, a.CommandDigest) != target {
			return errors.New("the MCP server or account binding changed while its account was being bound; retry")
		}
		c := *s
		c.PluginAccount = &a
		if err := c.check(); err != nil {
			return err
		}
		*s = c
		return nil
	})
	if err == nil {
		invalidatePluginMCP(name)
	}
	return res, err
}

func invalidatePluginMCP(name string) {
	checked.Lock()
	delete(checked.m, name)
	checked.Unlock()
	hubPool.Lock()
	b := hubPool.m[name]
	delete(hubPool.m, name)
	hubPool.Unlock()
	if b != nil {
		b.stop()
	}
	cards.Lock()
	loadCards()
	if _, ok := cards.m[name]; ok {
		delete(cards.m, name)
		writeCards(cards.m)
	}
	cards.Unlock()
}

func accountTargetFingerprint(s *Server) [32]byte {
	b, _ := json.Marshal(struct {
		Transport, Command, URL, Hub, Plugin, Bridge string
		Args                                         []string
		Env, Headers                                 map[string]string
		OAuth                                        *OAuthApp
	}{s.Transport, s.Command, s.URL, s.Hub, s.Plugin, s.Bridge, s.Args, s.Env, s.Headers, s.OAuth})
	return sha256.Sum256(b)
}

// The digest includes the resolved executable's bytes, expanded command and
// arguments, and configured environment values. Existing file arguments and
// explicit local preload/require option files are included too. This does not
// discover arbitrary imports or files loaded by a command at runtime.
func commandTrustDigest(s *Server) (string, error) {
	command, args := launchCmd(s, "")
	return commandTrustDigestFor(s, command, args)
}

func commandTrustDigestFor(s *Server, command string, args []string) (string, error) {
	env, unset := expandAll(s.Env)
	if len(unset) != 0 {
		return "", errors.New("a command environment variable is unset")
	}
	prog, _ := stdioProgram(command)
	if !strings.ContainsAny(prog, `/\`) {
		var err error
		prog, err = exec.LookPath(prog)
		if err != nil {
			return "", errors.New("the command was not found")
		}
	}
	resolve := func(path string) string {
		if !filepath.IsAbs(path) {
			path = filepath.Join(home(), path)
		}
		return filepath.Clean(path)
	}
	prog = resolve(prog)
	type fileDigest struct {
		Path, SHA256 string
	}
	hashFile := func(path string) (fileDigest, error) {
		path, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fileDigest{}, err
		}
		f, err := os.Open(path)
		if err != nil {
			return fileDigest{}, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return fileDigest{}, errors.New("command is not a regular file")
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return fileDigest{}, err
		}
		return fileDigest{Path: path, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
	}
	executable, err := hashFile(prog)
	if err != nil {
		return "", errors.New("the command executable cannot be read")
	}
	files := []fileDigest{executable}
	runtime := strings.TrimSuffix(strings.ToLower(filepath.Base(prog)), ".exe")
	nodeOptions := runtime == "node" || runtime == "nodejs" || runtime == "bun"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		path := arg
		required := false
		switch {
		case arg == "--preload" || nodeOptions && (arg == "-r" || arg == "--require"):
			if i+1 == len(args) {
				return "", errors.New("a command option file cannot be inspected")
			}
			i++
			path, required = args[i], true
		case strings.HasPrefix(arg, "--preload="):
			path, required = strings.TrimPrefix(arg, "--preload="), true
		case nodeOptions && strings.HasPrefix(arg, "--require="):
			path, required = strings.TrimPrefix(arg, "--require="), true
		}
		if required {
			// A bare module name may resolve through a runtime's package
			// loader instead of the similarly named file in HOME.
			explicit := filepath.IsAbs(path) || strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") ||
				filepath.Separator == '\\' && (strings.HasPrefix(path, `.\`) || strings.HasPrefix(path, `..\`))
			if !explicit {
				return "", errors.New("a command option file cannot be inspected")
			}
			f, err := hashFile(resolve(path))
			if err != nil {
				return "", errors.New("a command option file cannot be read")
			}
			files = append(files, f)
			continue
		}
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		path = resolve(arg)
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", errors.New("a command argument cannot be inspected")
		}
		if !info.Mode().IsRegular() {
			continue
		}
		f, err := hashFile(path)
		if err != nil {
			return "", errors.New("a command argument cannot be read")
		}
		files = append(files, f)
	}
	b, err := json.Marshal(struct {
		Command string
		Args    []string
		Env     map[string]string
		Files   []fileDigest
	}{command, args, env, files})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func pluginMCPHeaders(data json.RawMessage) (http.Header, error) {
	var result struct {
		Headers map[string]string `json:"headers"`
	}
	if json.Unmarshal(data, &result) != nil || result.Headers == nil {
		return nil, errors.New("invalid plugin-account MCP authorization result")
	}
	h := http.Header{}
	for k, v := range result.Headers {
		if !pluginMCPAllowedHeader(k) || strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n") || h.Get(k) != "" {
			return nil, errors.New("invalid plugin-account MCP identity header")
		}
		h.Set(k, v)
	}
	return h, nil
}

// stdioAuthMessage resolves credentials for each request and notification.
// dialStdio verifies the acknowledged command before starting its process.
func stdioAuthMessage(ctx context.Context, s *Server, wire []byte) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(wire, &msg); err != nil {
		return nil, errors.New("invalid MCP message")
	}
	if len(msg["method"]) == 0 {
		return wire, nil
	}
	auth := map[string]any{"ok": false, "reason": "official_auth_unavailable"}
	result, err := pluginMCPAccountOperation(ctx, s.PluginAccount.Provider, s.PluginAccount.Account, plugin.AccountRequest{
		Operation: "mcp.headers", Params: map[string]any{"stdio": true}, Scope: s.PluginAccount.Scope,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var ae *plugin.AccountError
		if errors.As(err, &ae) {
			switch ae.Kind {
			case "untrusted_url":
				auth["reason"] = "official_mcp_origin_untrusted"
			case "plan_required":
				auth["reason"] = "official_auth_plan_required"
			}
		}
	} else {
		headers, err := pluginMCPHeaders(result.Data)
		if err != nil || result.Context.Scope != s.PluginAccount.Scope || result.Context.Generation == "" {
			return nil, errors.New("invalid plugin-account MCP authorization result")
		}
		values := make(map[string]string, len(headers))
		for k := range headers {
			values[k] = headers.Get(k)
		}
		auth = map[string]any{"ok": true, "headers": values}
	}
	params := map[string]json.RawMessage{}
	if raw := bytes.TrimSpace(msg["params"]); len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
		if json.Unmarshal(raw, &params) != nil || params == nil {
			return nil, errors.New("invalid MCP message parameters")
		}
	}
	meta := map[string]json.RawMessage{}
	if raw := bytes.TrimSpace(params["_meta"]); len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
		if json.Unmarshal(raw, &meta) != nil || meta == nil {
			return nil, errors.New("invalid MCP message metadata")
		}
	}
	meta[pluginMCPMeta], _ = json.Marshal(auth)
	params["_meta"], _ = json.Marshal(meta)
	msg["params"], _ = json.Marshal(params)
	return json.Marshal(msg)
}
