package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// WindowModule describes one installed view generation without acquiring a
// backend. Revision includes configuration; PackageRevision names the bytes.
type WindowModule struct {
	ID              string
	Version         string
	Revision        string
	PackageRevision string
	ConfigVersion   string
	Directory       string
	Metadata        string
	Configuration   sdk.ConfigSnapshot
	Manifest        sdk.Manifest
}

type WindowServiceTarget struct {
	URL           string
	Instance      string
	Component     string
	ConfigVersion string
}

type moduleRequestKey struct{}

func WithModuleRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, moduleRequestKey{}, id)
}

func windowRevision(packageRevision, configVersion string) string {
	sum := sha256.Sum256([]byte(packageRevision + "\x00" + configVersion))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) Windows(ctx context.Context) ([]WindowModule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := m.List()
	if err != nil {
		return nil, err
	}
	out := make([]WindowModule, 0)
	for _, entry := range entries {
		if entry.Disabled || entry.Manifest.Window == nil {
			continue
		}
		if err := sdk.CheckCompatibility(entry.Manifest); err != nil {
			return nil, err
		}
		config, err := m.readModuleConfig(entry.Manifest.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, WindowModule{
			ID: entry.Manifest.ID, Version: entry.Manifest.Version,
			Revision: windowRevision(entry.Revision, config.Version), PackageRevision: entry.Revision,
			ConfigVersion: config.Version, Configuration: config,
			Directory: filepath.Join(m.root, filepath.FromSlash(entry.Directory)),
			Metadata:  entry.Manifest.Window.Metadata, Manifest: entry.Manifest.Clone(),
		})
	}
	return out, nil
}

func (m *Manager) windowEntry(ctx context.Context, id, revision string) (Installed, sdk.ConfigSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	entries, err := m.List()
	if err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID != id || entry.Disabled || entry.Manifest.Window == nil {
			continue
		}
		if err := sdk.CheckCompatibility(entry.Manifest); err != nil {
			return Installed{}, sdk.ConfigSnapshot{}, err
		}
		config, err := m.readModuleConfig(id)
		if err != nil {
			return Installed{}, sdk.ConfigSnapshot{}, err
		}
		if revision != windowRevision(entry.Revision, config.Version) {
			return Installed{}, sdk.ConfigSnapshot{}, errors.New("module window generation changed")
		}
		return entry, config, nil
	}
	return Installed{}, sdk.ConfigSnapshot{}, errors.New("module window is not enabled")
}

func (m *Manager) ReadWindowFile(ctx context.Context, id, revision, filename string) ([]byte, error) {
	entry, _, err := m.windowEntry(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	if !catalogRelative(filename) {
		return nil, errors.New("module window file must be package-relative")
	}
	expected, found := entry.Files[filename]
	if !found {
		return nil, errors.New("module window file is not in its frozen package")
	}
	root, err := m.catalog.open(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directory, err := openWindowDirectory(root, entry.Directory)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	parent, err := openWindowDirectory(directory, path.Dir(filename))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	var body bytes.Buffer
	digest, _, err := catalogHash(ctx, parent, path.Base(filename), catalogFileLimit, &body)
	if err != nil {
		return nil, err
	}
	if digest != expected {
		return nil, errors.New("module window file failed its frozen hash check")
	}
	return body.Bytes(), nil
}

func openWindowDirectory(root *os.Root, name string) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if name == "." {
		return current, nil
	}
	if !catalogRelative(name) {
		current.Close()
		return nil, errors.New("module window directory is not package-relative")
	}
	for _, segment := range strings.Split(name, "/") {
		info, err := current.Lstat(segment)
		if err != nil || !info.IsDir() {
			current.Close()
			return nil, errors.New("module window directory is unavailable or a symlink")
		}
		next, err := current.OpenRoot(segment)
		if err == nil {
			err = catalogDirectoryIdentity(next, info)
		}
		current.Close()
		if err != nil {
			if next != nil {
				next.Close()
			}
			return nil, err
		}
		current = next
	}
	return current, nil
}

func (m *Manager) windowLease(ctx context.Context, id, revision string) (*Lease, Installed, error) {
	entry, config, err := m.windowEntry(ctx, id, revision)
	if err != nil {
		return nil, Installed{}, err
	}
	lease, err := m.AcquireRevision(ctx, id, entry.Revision, config.Version)
	if err != nil {
		return nil, Installed{}, err
	}
	if lease.group == nil {
		lease.Close()
		return nil, Installed{}, errors.New("module window has no component group")
	}
	return lease, entry, nil
}

func windowBinding(manifest sdk.Manifest, part, id string) (sdk.BindingDescriptor, error) {
	binding, found := manifest.ResolveBinding(id)
	if !found || binding.From != part || !strings.HasPrefix(part, "window:") {
		return sdk.BindingDescriptor{}, errors.New("module window did not declare this binding")
	}
	return binding, nil
}

func (m *Manager) CallWindowBinding(ctx context.Context, id, revision, part, bindingID string, input json.RawMessage) (json.RawMessage, error) {
	entry, _, err := m.windowEntry(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	binding, err := windowBinding(entry.Manifest, part, bindingID)
	if err != nil || binding.Action == "" {
		return nil, errors.New("module window binding does not name an action")
	}
	if len(input) == 0 || len(input) > sdk.MaxHostBytes || !json.Valid(input) {
		return nil, errors.New("module window action requires bounded JSON input")
	}
	request, _ := ctx.Value(moduleRequestKey{}).(string)
	if request != "" && (len(request) > 256 || strings.TrimSpace(request) == "" || strings.ContainsFunc(request, unicode.IsControl)) {
		return nil, errors.New("module window request identity is invalid")
	}
	lease, _, err := m.windowLease(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	scope := sdk.Scope{Plugin: id}
	if request != "" {
		action, _ := entry.Manifest.ResolveAction(binding.Action)
		if action.Effect == "write" {
			key := strings.Join([]string{id, revision, part, bindingID, request}, "\x00")
			sum := sha256.Sum256([]byte(key))
			scope.Operation = hex.EncodeToString(sum[:])
		}
	}
	return lease.Call(ctx, sdk.MethodModuleCall, scope,
		sdk.ModuleCallRequest{Action: binding.Action, Input: bytes.Clone(input)}, nil)
}

func (m *Manager) CompareAndSwapWindowConfig(ctx context.Context, id, revision, expected string, values json.RawMessage) (sdk.ConfigSnapshot, error) {
	entry, config, err := m.windowEntry(ctx, id, revision)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	if expected != config.Version {
		return sdk.ConfigSnapshot{}, errors.New("module window configuration changed")
	}
	return m.compareAndSwapModuleConfig(ctx, id, entry.Revision, expected, values)
}

// The caller holds release until the entire HTTP/SSE proxy has completed.
func (m *Manager) ReadyWindowService(ctx context.Context, id, revision, part, bindingID string) (string, func(), error) {
	target, release, err := m.ReadyWindowServiceTarget(ctx, id, revision, part, bindingID)
	return target.URL, release, err
}

func (m *Manager) ReadyWindowServiceTarget(ctx context.Context, id, revision, part, bindingID string) (WindowServiceTarget, func(), error) {
	entry, _, err := m.windowEntry(ctx, id, revision)
	if err != nil {
		return WindowServiceTarget{}, nil, err
	}
	binding, err := windowBinding(entry.Manifest, part, bindingID)
	if err != nil || binding.Service == "" {
		return WindowServiceTarget{}, nil, errors.New("module window binding does not name a service")
	}
	lease, _, err := m.windowLease(ctx, id, revision)
	if err != nil {
		return WindowServiceTarget{}, nil, err
	}
	call := lease.group.withAuthority(ctx, part, "read")
	endpoint, err := lease.group.serviceEndpoint(call, part, bindingID)
	if err != nil {
		lease.Close()
		return WindowServiceTarget{}, nil, err
	}
	lease.group.mu.Lock()
	var target WindowServiceTarget
	if component := lease.group.components[binding.Service]; component != nil && !lease.group.closed {
		target = WindowServiceTarget{
			URL: endpoint, Instance: component.ready.Instance, Component: component.ready.Component,
			ConfigVersion: component.ready.ConfigVersion,
		}
	}
	lease.group.mu.Unlock()
	if target.Instance == "" || target.Component != binding.Service || target.ConfigVersion != lease.ConfigVersion() {
		lease.Close()
		return WindowServiceTarget{}, nil, errors.New("module service has no matching generation proof")
	}
	stop := context.AfterFunc(ctx, lease.Close)
	release := func() {
		stop()
		lease.Close()
	}
	if err := ctx.Err(); err != nil {
		release()
		return WindowServiceTarget{}, nil, fmt.Errorf("module window service canceled: %w", err)
	}
	return target, release, nil
}
