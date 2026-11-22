package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/update"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionBindingLimit = 64 << 20

type extensionBindingRecord struct {
	Plugin           string                         `json:"plugin"`
	Agent            string                         `json:"agent"`
	Home             string                         `json:"home"`
	Revision         uint64                         `json:"revision"`
	Phase            string                         `json:"phase"`
	Descriptor       sdk.AgentDescriptor            `json:"descriptor"`
	Bin              string                         `json:"bin"`
	PackageRevision  string                         `json:"packageRevision"`
	ConfigVersion    string                         `json:"configVersion"`
	Values           map[string]string              `json:"values"`
	ModelOptions     json.RawMessage                `json:"modelOptions,omitempty"`
	Before           []extension.ActionSettingFile  `json:"before"`
	Applied          []extension.ActionSettingFile  `json:"applied"`
	PriorApplied     []extension.ActionSettingFile  `json:"priorApplied,omitempty"`
	Shells           []update.ShellFunctionSnapshot `json:"shells"`
	PriorShells      []update.ShellFunctionSnapshot `json:"priorShells,omitempty"`
	RecoverySafe     bool                           `json:"recoverySafe"`
	FinishDisconnect bool                           `json:"finishDisconnect,omitempty"`
	legacy           string
}

type extensionBindingLedger struct {
	Schema  int                               `json:"schema"`
	Records map[string]extensionBindingRecord `json:"records"`
}

func (record extensionBindingRecord) hasOwner() bool {
	return record.Phase == "owned" || record.Phase == "pending" || record.legacy != ""
}

func extensionBindingPath() string {
	return filepath.Join(filepath.Dir(provider.Path()), "sdk-agent-bindings.json")
}

func extensionAgentRootGuard(home string) func() error {
	anchor, anchorErr := extensionPathAnchor(extensionBindingPath())
	homeInfo, homeErr := os.Stat(home)
	rootInfo, rootErr := os.Stat(filepath.Dir(extensionBindingPath()))
	return func() error {
		if anchorErr != nil || homeErr != nil || rootErr != nil && !errors.Is(rootErr, fs.ErrNotExist) {
			return errors.New("agent configuration identity could not be captured")
		}
		current, err := extensionPathAnchor(extensionBindingPath())
		if err != nil || current != anchor {
			return errors.New("agent configuration root changed; rediscover the agent")
		}
		nowHome, err := os.Stat(home)
		if err != nil || !os.SameFile(homeInfo, nowHome) {
			return errors.New("agent home identity changed; rediscover the agent")
		}
		if rootInfo != nil {
			nowRoot, err := os.Stat(filepath.Dir(extensionBindingPath()))
			if err != nil || !os.SameFile(rootInfo, nowRoot) {
				return errors.New("agent configuration directory was replaced; rediscover the agent")
			}
		}
		return nil
	}
}

func extensionPathAnchor(path string) (string, error) {
	at, suffix := filepath.Clean(path), ""
	for {
		canonical, err := filepath.EvalSymlinks(at)
		if err == nil {
			return filepath.Join(canonical, suffix), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(at)
		if parent == at {
			return "", err
		}
		suffix, at = filepath.Join(filepath.Base(at), suffix), parent
	}
}

func extensionBindingKey(home, plugin, id string) string {
	if canonical, err := filepath.EvalSymlinks(home); err == nil {
		home = canonical
	}
	if runtime.GOOS == "windows" {
		home = strings.ToLower(home)
	}
	raw, _ := json.Marshal([]string{filepath.Clean(home), plugin, id})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func extensionBindingTarget(record extensionBindingRecord) string {
	if record.Descriptor.Profile != nil {
		return record.Descriptor.Profile.Target
	}
	return record.Agent
}

func extensionBindingRead() (extensionBindingLedger, error) {
	return extensionBindingSnapshot(context.Background(), false, os.Open)
}

func extensionBindingObserve(ctx context.Context) (extensionBindingLedger, error) {
	return extensionBindingSnapshot(ctx, true, os.Open)
}

func extensionBindingSnapshot(ctx context.Context, retry bool, open func(string) (*os.File, error)) (extensionBindingLedger, error) {
	ledger := extensionBindingLedger{Schema: 1, Records: map[string]extensionBindingRecord{}}
	for {
		if err := ctx.Err(); err != nil {
			return ledger, err
		}
		before, err := os.Lstat(extensionBindingPath())
		if errors.Is(err, fs.ErrNotExist) {
			return ledger, nil
		}
		if err != nil || !before.Mode().IsRegular() || before.Size() > extensionBindingLimit ||
			runtime.GOOS != "windows" && before.Mode().Perm()&0o077 != 0 {
			return ledger, errors.New("agent binding ownership ledger is unreadable")
		}
		file, err := open(extensionBindingPath())
		if err != nil {
			if retry && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return ledger, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > extensionBindingLimit ||
			runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			file.Close()
			return ledger, errors.New("agent binding ownership ledger is unreadable")
		}
		if !os.SameFile(before, info) {
			file.Close()
			if retry {
				continue
			}
			return ledger, errors.New("agent binding ownership ledger changed while opening")
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, extensionBindingLimit+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || statErr != nil || closeErr != nil || len(raw) > extensionBindingLimit ||
			after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return ledger, errors.New("agent binding ownership ledger changed while reading")
		}
		if err := ctx.Err(); err != nil {
			return ledger, err
		}
		return extensionBindingDecode(raw)
	}
}

func extensionBindingDecode(raw []byte) (extensionBindingLedger, error) {
	var ledger extensionBindingLedger
	probe := json.NewDecoder(bytes.NewReader(raw))
	probe.UseNumber()
	if !utf8.Valid(raw) || extensionBindingJSONValue(probe, 0) != nil || probe.Decode(new(any)) != io.EOF {
		return ledger, errors.New("agent binding ownership ledger is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ledger) != nil || decoder.Decode(new(any)) != io.EOF || ledger.Schema != 1 || ledger.Records == nil {
		return ledger, errors.New("agent binding ownership ledger is invalid")
	}
	return ledger, extensionBindingValidate(ledger)
}

func extensionBindingJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("agent binding ownership nesting exceeds its limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			token, err := decoder.Token()
			key, valid := token.(string)
			if err != nil || !valid || keys[key] {
				return errors.New("agent binding ownership contains duplicate keys")
			}
			keys[key] = true
		}
		if err := extensionBindingJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func extensionBindingValidate(ledger extensionBindingLedger) error {
	if ledger.Schema != 1 || ledger.Records == nil {
		return errors.New("agent binding ownership ledger is invalid")
	}
	claims := map[string]bool{}
	for key, record := range ledger.Records {
		manifest := sdk.Manifest{
			Schema: sdk.ManifestSchema, ID: record.Plugin, Version: "1.0.0", Runtime: "exec", Entry: "owner",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
			Agents:   []sdk.AgentDescriptor{record.Descriptor},
		}
		if !filepath.IsAbs(record.Home) || record.Agent != record.Descriptor.ID || record.Revision == 0 ||
			key != extensionBindingKey(record.Home, record.Plugin, record.Agent) || record.Values == nil ||
			manifest.Validate() != nil || record.Phase != "owned" && record.Phase != "pending" && record.Phase != "disconnected" {
			return errors.New("agent binding ownership record is invalid")
		}
		if sdk.ValidateAgentBindingSnapshot(manifest, record.Descriptor, &sdk.AgentBindingSnapshot{
			Target: extensionBindingTarget(record), Revision: strconv.FormatUint(record.Revision, 10),
			Connected: record.Phase == "owned", Values: record.Values,
		}) != nil {
			return errors.New("agent binding ownership values are invalid")
		}
		if record.FinishDisconnect && (record.Phase != "pending" || record.Descriptor.Launch != nil ||
			record.Descriptor.Profile != nil || len(record.PriorApplied) == 0) {
			return errors.New("agent binding disconnect snapshots are invalid")
		}
		for index, snapshots := range [][]extension.ActionSettingFile{record.Before, record.Applied, record.PriorApplied} {
			if index == 2 && len(snapshots) == 0 {
				continue
			}
			if len(snapshots) != len(record.Descriptor.ConfigFiles) {
				return errors.New("agent binding file snapshots are incomplete")
			}
			paths := map[string]bool{}
			for _, path := range record.Descriptor.ConfigFiles {
				paths[path] = true
			}
			seen, total := map[string]bool{}, 0
			for _, snapshot := range snapshots {
				total += len(snapshot.Body)
				digest := sha256.Sum256(snapshot.Body)
				expected := hex.EncodeToString(digest[:])
				if !snapshot.Exists {
					expected = extension.MissingActionHash
				}
				if !paths[snapshot.Path] || seen[snapshot.Path] || snapshot.Mode & ^uint32(0o777) != 0 ||
					snapshot.BeforeHash != expected || !snapshot.Exists && (len(snapshot.Body) != 0 || snapshot.Mode != 0) ||
					total > extension.MaxActionBytes {
					return errors.New("agent binding file snapshots are invalid")
				}
				seen[snapshot.Path] = true
			}
		}
		for _, snapshot := range append(slices.Clone(record.Shells), record.PriorShells...) {
			relative, err := filepath.Rel(record.Home, snapshot.Path)
			if err != nil || relative == "." || !filepath.IsLocal(relative) || snapshot.Name != record.Bin ||
				len(snapshot.Before)+len(snapshot.After) > 2<<20 {
				return errors.New("agent binding shell snapshots are invalid")
			}
		}
		if record.Phase != "disconnected" {
			claim := extensionBindingKey(record.Home, "", extensionBindingTarget(record))
			if claims[claim] {
				return errors.New("agent target has conflicting ownership records")
			}
			claims[claim] = true
			if record.Descriptor.Launch != nil {
				command := extensionBindingKey(record.Home, "shell", strings.ToLower(record.Bin))
				if record.Bin == "" || claims[command] {
					return errors.New("agent shell command has conflicting ownership records")
				}
				claims[command] = true
			}
		}
	}
	return nil
}

func extensionBindingWrite(ledger extensionBindingLedger) error {
	if err := extensionBindingValidate(ledger); err != nil {
		return err
	}
	raw, err := json.Marshal(ledger)
	if err != nil || len(raw) > extensionBindingLimit {
		return errors.New("agent binding ownership exceeds its storage budget")
	}
	path := extensionBindingPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".sdk-agent-bindings-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	_, writeErr := file.Write(raw)
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.Join(writeErr, syncErr, closeErr)
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func extensionBindingLock(ctx context.Context, home, target string, shared bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := filepath.Join(filepath.Dir(extensionBindingPath()), "sdk-agent-locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("agent binding lock directory is unavailable")
	}
	path := filepath.Join(root, extensionBindingKey(home, "", target)+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	opened, statErr := file.Stat()
	if err != nil || statErr != nil || !before.Mode().IsRegular() || !os.SameFile(before, opened) ||
		runtime.GOOS != "windows" && before.Mode().Perm()&0o077 != 0 {
		file.Close()
		return nil, errors.New("agent binding lock is not a private regular file")
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		held, err := extensionTryLock(file, shared)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("agent ownership lock failed: %w", err)
		}
		if held {
			return func() { extensionUnlock(file); file.Close() }, nil
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func extensionBindingSameDescriptor(first, second sdk.AgentDescriptor) bool {
	left, _ := json.Marshal(first)
	right, _ := json.Marshal(second)
	return bytes.Equal(left, right)
}
