package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	desktopOtherProfile = "Claude Desktop uses another gateway configuration now (appliedId in configLibrary/_meta.json), not magpie's"
	desktopKeyMark      = "claude-desktop.gateway-key"
	desktopImported     = "claude-desktop.imported-fields"
	desktopModes        = "claude-desktop.previous-modes"
	desktopAPIKey       = "inferenceGatewayApiKey"
	desktopAuthScheme   = "inferenceGatewayAuthScheme"
)

func desktopFields(body []byte) (map[string]json.RawMessage, error) {
	body = bytes.TrimSpace(edit.TrimBOM(body))
	fields := map[string]json.RawMessage{}
	if len(body) == 0 {
		return fields, nil
	}
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("not a JSON object")
	}
	count := 0
	gjson.ParseBytes(body).ForEach(func(_, _ gjson.Result) bool { count++; return true })
	if count != len(fields) {
		return nil, errors.New("duplicate configuration fields")
	}
	return fields, nil
}

func desktopReadFields(path string) (map[string]json.RawMessage, error) {
	body, err := edit.Read(path)
	if err != nil {
		return nil, err
	}
	fields, err := desktopFields(body)
	return fields, edit.AtFile(path, err)
}

func desktopString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func desktopHybridCheck(meta map[string]json.RawMessage) error {
	if _, exists := meta["hybridPointer"]; exists {
		return errors.New("Claude Desktop has a hybridPointer in configLibrary/_meta.json. Select a local profile in Claude Desktop before connecting to magpie")
	}
	return nil
}

func desktopModeCheck(profile map[string]json.RawMessage) error {
	if mode, exists := profile["deploymentMode"]; exists {
		switch desktopString(mode) {
		case "any", "3p":
			return nil
		}
		return errors.New("Claude Desktop's profile deploymentMode must allow third-party gateways")
	}
	return nil
}

func desktopProfileCheck(profile map[string]json.RawMessage) error {
	if err := desktopModeCheck(profile); err != nil {
		return err
	}
	if strings.TrimSpace(desktopString(profile["selfHostedUrl"])) != "" {
		return errors.New("Claude Desktop's selfHostedUrl takes precedence over the inference gateway. Select a local gateway profile to connect to magpie")
	}
	if kind, exists := profile["inferenceCredentialKind"]; exists {
		if desktopString(kind) != "static" {
			return errors.New("Claude Desktop's inferenceCredentialKind must be static to use magpie's gateway key")
		}
		return nil
	}
	if desktopString(profile[desktopAuthScheme]) == "sso" {
		return errors.New("Claude Desktop's inferenceGatewayAuthScheme selects SSO. Select a profile with a static gateway key to connect to magpie")
	}
	for _, field := range []string{"inferenceIdpOidc", "inferenceGatewayOidc", "inferenceCredentialHelper"} {
		raw := bytes.TrimSpace(profile[field])
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) ||
			raw[0] == '"' && strings.TrimSpace(desktopString(raw)) == "" {
			continue
		}
		return fmt.Errorf("Claude Desktop's %s can take precedence over a gateway key. Select a profile with a static gateway key to connect to magpie", field)
	}
	return nil
}

func desktopProfilePath(p desktopPaths, id string) string {
	if id == "" || strings.ContainsAny(id, `/\:`) || filepath.Base(id) != id || strings.HasPrefix(id, ".") {
		return ""
	}
	return filepath.Join(p.library, id+".json")
}

func desktopIsGatewayProfile(fields map[string]json.RawMessage) bool {
	return desktopString(fields["inferenceProvider"]) == "gateway" &&
		desktopIsOurGateway(desktopString(fields["inferenceGatewayBaseUrl"]))
}

func desktopEntryID(raw json.RawMessage) string {
	var entry struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &entry)
	return entry.ID
}

func desktopOurs(raw json.RawMessage) bool { return desktopEntryID(raw) == desktopProfileID }

func desktopMetaEntries(fields map[string]json.RawMessage) ([]json.RawMessage, error) {
	raw, exists := fields["entries"]
	if !exists {
		return nil, nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return nil, errors.New("entries is not a list")
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		fields, err := desktopFields(entry)
		id := desktopString(fields["id"])
		if err != nil || id == "" || seen[id] {
			return nil, errors.New("configLibrary contains an invalid or duplicate profile")
		}
		seen[id] = true
	}
	return entries, nil
}

type desktopFieldClaim struct {
	Before json.RawMessage `json:"before,omitempty"`
	Hash   string          `json:"hash"`
}

type desktopImportedClaims map[string]map[string]desktopFieldClaim

func desktopClaims(saved string) (desktopImportedClaims, error) {
	claims := desktopImportedClaims{}
	if saved != "" {
		if json.Unmarshal([]byte(saved), &claims) != nil || claims == nil {
			return nil, errors.New("invalid saved Claude Desktop gateway fields")
		}
	}
	return claims, nil
}

type desktopDraft struct {
	files   map[string]sdk.ActionSettingFile
	changes map[string][]byte
	saved   map[string]string
	clean   []string
}

func (draft *desktopDraft) body(path string) []byte {
	if body, changed := draft.changes[path]; changed {
		return body
	}
	return draft.files[path].Body
}

func (draft *desktopDraft) fields(path string) (map[string]json.RawMessage, error) {
	fields, err := desktopFields(draft.body(path))
	return fields, edit.AtFile(path, err)
}

func (draft *desktopDraft) patch(path string, set []edit.KV, del ...string) error {
	if len(set) == 0 && len(del) == 0 {
		return nil
	}
	body, err := edit.PatchJSON(draft.body(path), set, del)
	if err != nil {
		return edit.AtFile(path, err)
	}
	draft.changes[path] = body
	return nil
}

func desktopUpdate(paths []desktopPaths, change func(*desktopDraft, desktopPaths) error) error {
	return desktopSettingsAction(paths, nil, func(draft *desktopDraft) error {
		for _, path := range paths {
			if err := change(draft, path); err != nil {
				return err
			}
		}
		return nil
	})
}

func desktopSettingsAction(paths []desktopPaths, commit func() error, change func(*desktopDraft) error, extra ...extension.LinkedActionFile) error {
	stashMu.Lock()
	defer stashMu.Unlock()
	stashFile := stashPath()
	inputs := []extension.LinkedActionFile{{Name: stashFile, Path: stashFile, Private: true}}
	seen := map[string]bool{stashFile: true}
	add := func(path string, private bool) {
		if path != "" && !seen[path] {
			inputs = append(inputs, extension.LinkedActionFile{Name: path, Path: path, Private: private})
			seen[path] = true
		}
	}
	prior := stashLoad()
	var missingDirs, cleanup []string
	for _, file := range extra {
		inputs = append(inputs, file)
		seen[file.Path] = true
		dir := filepath.Dir(file.Path)
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			missingDirs = append(missingDirs, dir)
		}
	}
	for _, p := range paths {
		meta, err := desktopReadFields(p.meta)
		if err != nil {
			return err
		}
		claims, err := desktopClaims(prior[desktopImported+p.key])
		if err != nil {
			return err
		}
		add(p.prof, true)
		add(desktopProfilePath(p, desktopString(meta["appliedId"])), true)
		add(desktopProfilePath(p, prior[desktopApplied+p.key]), true)
		for _, id := range slices.Sorted(maps.Keys(claims)) {
			add(desktopProfilePath(p, id), true)
		}
		add(p.app3p, false)
		add(p.meta, false)
		add(p.config3p, false)
		add(p.config, false)
		for _, path := range []string{p.dir, p.dir3p, p.library} {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				missingDirs = append(missingDirs, path)
			}
		}
	}
	err := extension.ApplyLinkedAgentPlanWithCommit(change, func(change func(*desktopDraft) error, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
		draft := desktopDraft{
			files:   make(map[string]sdk.ActionSettingFile, len(snapshot.Files)),
			changes: map[string][]byte{}, saved: map[string]string{},
		}
		for _, file := range snapshot.Files {
			draft.files[file.Path] = file
		}
		if raw := draft.body(stashFile); len(edit.TrimBOM(raw)) > 0 {
			if json.Unmarshal(edit.TrimBOM(raw), &draft.saved) != nil || draft.saved == nil {
				return sdk.AgentPlan{}, edit.AtFile(stashFile, errors.New("invalid saved agent settings"))
			}
		}
		if !maps.Equal(prior, draft.saved) {
			return sdk.AgentPlan{}, extension.ErrActionChanged
		}
		draft.adoptPackagedStash(paths)
		if err := change(&draft); err != nil {
			return sdk.AgentPlan{}, err
		}
		if !maps.Equal(prior, draft.saved) {
			body, err := json.MarshalIndent(draft.saved, "", "  ")
			if err != nil {
				return sdk.AgentPlan{}, err
			}
			draft.changes[stashFile] = body
		}
		cleanup = draft.clean
		var plan sdk.AgentPlan
		for _, file := range snapshot.Files {
			if body, touched := draft.changes[file.Path]; touched && (file.Exists != (body != nil) || !bytes.Equal(file.Body, body)) {
				plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: body, Remove: body == nil})
			}
		}
		return plan, nil
	}, commit, inputs...)
	if err != nil {
		cleanup = missingDirs
	}
	for _, path := range slices.Backward(cleanup) {
		if info, err := os.Lstat(path); err == nil && info.IsDir() {
			_ = os.Remove(path)
		}
	}
	return err
}

func (draft *desktopDraft) meta(p desktopPaths) (map[string]json.RawMessage, []json.RawMessage, error) {
	meta, err := draft.fields(p.meta)
	if err != nil {
		return nil, nil, err
	}
	entries, err := desktopMetaEntries(meta)
	return meta, entries, edit.AtFile(p.meta, err)
}

func (draft *desktopDraft) wired(paths ...desktopPaths) (bool, error) {
	wired := false
	for _, path := range paths {
		_, entries, err := draft.meta(path)
		if err != nil {
			return false, err
		}
		wired = wired || slices.ContainsFunc(entries, desktopOurs)
	}
	return wired, nil
}

func (draft *desktopDraft) adoptPackagedStash(paths []desktopPaths) {
	if len(paths) < 2 || draft.saved[desktopStashed] == "" || draft.saved[desktopStashed+".package"] != "" {
		return
	}
	var wired, packaged bool
	for _, p := range paths {
		_, entries, err := draft.meta(p)
		if err != nil || !slices.ContainsFunc(entries, desktopOurs) {
			continue
		}
		wired = wired || p.key == ""
		packaged = packaged || p.key == ".package"
	}
	if !wired && packaged {
		for _, key := range desktopStashKeys {
			if value, ok := draft.saved[key]; ok {
				draft.saved[key+".package"] = value
				delete(draft.saved, key)
			}
		}
	}
}

func (draft *desktopDraft) imported(p desktopPaths, id string) (string, map[string]json.RawMessage, error) {
	path := desktopProfilePath(p, id)
	if path == "" || path == p.prof {
		return "", nil, nil
	}
	if _, ok := draft.files[path]; !ok {
		return "", nil, edit.AtFile(p.meta, extension.ErrActionChanged)
	}
	fields, err := draft.fields(path)
	if err != nil {
		return "", nil, err
	}
	if !desktopIsGatewayProfile(fields) {
		return "", nil, nil
	}
	return path, fields, desktopProfileCheck(fields)
}

func desktopOn(p desktopPaths) error { return desktopUpdate([]desktopPaths{p}, desktopConnect) }

func desktopConnect(draft *desktopDraft, p desktopPaths) error {
	meta, entries, err := draft.meta(p)
	if err != nil {
		return err
	}
	if err = desktopHybridCheck(meta); err != nil {
		return err
	}
	profile, err := draft.fields(p.prof)
	if err != nil {
		return err
	}
	current := desktopString(meta["appliedId"])
	wired := slices.ContainsFunc(entries, desktopOurs)
	selected, foreign, err := draft.imported(p, current)
	if err != nil {
		return err
	}
	if !wired {
		var made []string
		for _, path := range []string{p.dir, p.config, p.dir3p, p.config3p, p.library, p.meta, p.app3p} {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				made = append(made, path)
			}
		}
		var modes []string
		for _, path := range []string{p.config, p.config3p} {
			fields, err := draft.fields(path)
			if err != nil {
				return err
			}
			modes = append(modes, string(fields["deploymentMode"]))
		}
		body, _ := json.Marshal(modes)
		draft.saved[desktopModes+p.key] = string(body)
		draft.saved[desktopStashed+p.key] = "1"
		draft.saved[desktopApplied+p.key] = current
		draft.saved[desktopMade+p.key] = strings.Join(made, "\n")
		for _, key := range []string{desktopMode, desktopMode3p, desktopLocale} {
			delete(draft.saved, key+p.key)
		}
	}
	if len(profile) == 0 {
		switch kept := draft.saved[desktopProfile+p.key]; {
		case kept != "":
			profile, err = desktopFields([]byte(kept))
			if err != nil {
				return err
			}
			draft.changes[p.prof] = []byte(kept)
		case selected != "" && !wired:
			profile = foreign
			draft.changes[p.prof] = draft.body(selected)
		}
	}
	if err := desktopProfileCheck(profile); err != nil {
		return err
	}
	gw := desktopGateway()
	key := desktopKey(gw)
	hash := desktopValueHash(key)
	set := []edit.KV{
		{Path: "inferenceProvider", Value: "gateway"},
		{Path: "inferenceGatewayBaseUrl", Value: gw},
		{Path: desktopAuthScheme, Value: "bearer"},
	}
	previous := desktopString(profile[desktopAPIKey])
	if open, known := desktopOpen(desktopAPIKey, previous); known && open != key ||
		!known && (desktopString(profile["inferenceGatewayBaseUrl"]) != gw || draft.saved[desktopKeyMark+p.key] != hash && draft.saved[desktopKeyMark+p.key] != "") {
		set = append(set, edit.KV{Path: desktopAPIKey, Value: key})
	}
	draft.saved[desktopKeyMark+p.key] = hash
	if profile["disableDeploymentModeChooser"] == nil {
		set = append(set, edit.KV{Path: "disableDeploymentModeChooser", Value: true})
	}
	if profile["coworkEgressAllowedHosts"] == nil {
		set = append(set, edit.KV{Path: "coworkEgressAllowedHosts", Value: []string{"*"}})
	}
	if err := draft.patch(p.prof, set); err != nil {
		return err
	}
	delete(draft.saved, desktopProfile+p.key)
	if wired && selected != "" {
		if err := draft.keyApplied(p, current, selected, foreign); err != nil {
			return err
		}
	}
	if !wired {
		entry, _ := json.Marshal(map[string]string{"id": desktopProfileID, "name": "magpie"})
		entries = append(entries, entry)
		if err := draft.patch(p.meta, []edit.KV{{Path: "entries", Value: entries}}); err != nil {
			return err
		}
	}
	if !wired || selected == "" {
		if err := draft.patch(p.meta, []edit.KV{{Path: "appliedId", Value: desktopProfileID}}); err != nil {
			return err
		}
	}
	if err := draft.carryLocale(p); err != nil {
		return err
	}
	if err := draft.carryMCP(p); err != nil {
		return err
	}
	if err := draft.seedProfile(p, gw != gateway.URL()); err != nil {
		return err
	}
	if err := draft.syncModels(p); err != nil {
		return err
	}
	for _, path := range []string{p.config3p, p.config} {
		if _, err := draft.fields(path); err != nil {
			return err
		}
		if err := draft.patch(path, []edit.KV{{Path: "deploymentMode", Value: "3p"}}); err != nil {
			return err
		}
	}
	return nil
}

func desktopSync(draft *desktopDraft, p desktopPaths) error {
	meta, entries, err := draft.meta(p)
	if err != nil || !slices.ContainsFunc(entries, desktopOurs) {
		return err
	}
	if err := desktopHybridCheck(meta); err != nil {
		return err
	}
	profile, err := draft.fields(p.prof)
	if err != nil {
		return err
	}
	if err := desktopProfileCheck(profile); err != nil {
		return err
	}
	id := desktopString(meta["appliedId"])
	if id != desktopProfileID {
		path, foreign, err := draft.imported(p, id)
		if err != nil {
			return err
		}
		if path == "" {
			return nil
		}
		if err := draft.keyApplied(p, id, path, foreign); err != nil {
			return err
		}
	}
	if err := draft.seedProfile(p, false); err != nil {
		return err
	}
	if draft.saved[desktopMCPSeen+p.key] == "" {
		if err := draft.carryMCP(p); err != nil {
			return err
		}
	}
	return draft.syncModels(p)
}

func desktopSyncSettings(paths []desktopPaths) error {
	if !desktopWiredAny(paths) {
		return nil
	}
	return desktopUpdate(paths, desktopSync)
}

func desktopValueHash(value string) string {
	body, _ := json.Marshal(value)
	return desktopModelsHash(body)
}

func (draft *desktopDraft) keyApplied(p desktopPaths, id, path string, profile map[string]json.RawMessage) error {
	want := desktopKey(desktopGateway())
	if open, known := desktopOpen(desktopAPIKey, desktopString(profile[desktopAPIKey])); !known || desktopKeyTaken(open, want) {
		return nil
	}
	claims, err := desktopClaims(draft.saved[desktopImported+p.key])
	if err != nil {
		return err
	}
	owned := claims[id]
	if owned == nil {
		owned = map[string]desktopFieldClaim{}
		claims[id] = owned
	}
	set := []edit.KV{{Path: desktopAPIKey, Value: want}}
	if desktopString(profile[desktopAuthScheme]) == "" {
		set = append(set, edit.KV{Path: desktopAuthScheme, Value: "bearer"})
	}
	for _, field := range set {
		claim := owned[field.Path]
		if claim.Hash == "" || desktopModelsHash(profile[field.Path]) != claim.Hash {
			claim.Before = profile[field.Path]
		}
		claim.Hash = desktopValueHash(field.Value.(string))
		owned[field.Path] = claim
	}
	body, _ := json.Marshal(claims)
	draft.saved[desktopImported+p.key] = string(body)
	return draft.patch(path, set)
}

func (draft *desktopDraft) restoreImported(p desktopPaths) error {
	claims, err := desktopClaims(draft.saved[desktopImported+p.key])
	if err != nil {
		return err
	}
	for id, owned := range claims {
		path := desktopProfilePath(p, id)
		if path == "" || path == p.prof {
			return errors.New("invalid saved Claude Desktop profile ID")
		}
		if !draft.files[path].Exists {
			continue
		}
		current, err := draft.fields(path)
		if err != nil {
			return err
		}
		var set []edit.KV
		var del []string
		for _, key := range []string{desktopAPIKey, desktopAuthScheme} {
			claim, claimed := owned[key]
			raw := current[key]
			if key == desktopAPIKey {
				if open, known := desktopOpen(key, desktopString(raw)); known && strings.HasPrefix(desktopString(raw), desktopSealed) {
					raw, _ = json.Marshal(open)
				}
			}
			if !claimed || claim.Hash == "" || desktopModelsHash(raw) != claim.Hash {
				continue
			}
			if claim.Before == nil {
				del = append(del, key)
			} else {
				set = append(set, edit.KV{Path: key, Value: claim.Before})
			}
		}
		if err := draft.patch(path, set, del...); err != nil {
			return err
		}
	}
	delete(draft.saved, desktopImported+p.key)
	return nil
}

func desktopDisconnect(draft *desktopDraft, p desktopPaths) error {
	meta, entries, err := draft.meta(p)
	if err != nil {
		return err
	}
	suffix := p.key
	if draft.saved[desktopStashed+suffix] == "" && !slices.ContainsFunc(entries, desktopOurs) {
		return nil
	}
	made := strings.FieldsFunc(draft.saved[desktopMade+suffix], func(char rune) bool { return char == '\n' })
	var modes []string
	if raw := draft.saved[desktopModes+suffix]; raw != "" {
		if json.Unmarshal([]byte(raw), &modes) != nil || len(modes) != 2 {
			return errors.New("invalid saved Claude Desktop deployment modes")
		}
	} else {
		for _, key := range []string{desktopMode, desktopMode3p} {
			var raw []byte
			if mode := draft.saved[key+suffix]; mode != "" {
				raw, _ = json.Marshal(mode)
			}
			modes = append(modes, string(raw))
		}
	}
	for index, path := range []string{p.config, p.config3p} {
		fields, err := draft.fields(path)
		if err != nil {
			return err
		}
		if desktopString(fields["deploymentMode"]) != "3p" {
			continue
		}
		if prior := json.RawMessage(modes[index]); len(prior) == 0 {
			err = draft.patch(path, nil, "deploymentMode")
		} else {
			err = draft.patch(path, []edit.KV{{Path: "deploymentMode", Value: prior}})
		}
		if err != nil {
			return err
		}
	}
	if slices.ContainsFunc(entries, desktopOurs) {
		entries = slices.DeleteFunc(entries, desktopOurs)
		if len(entries) == 0 && slices.Contains(made, p.meta) {
			err = draft.patch(p.meta, nil, "entries")
		} else {
			err = draft.patch(p.meta, []edit.KV{{Path: "entries", Value: entries}})
		}
		if err != nil {
			return err
		}
	}
	if desktopString(meta["appliedId"]) == desktopProfileID {
		next := draft.saved[desktopApplied+suffix]
		if !slices.ContainsFunc(entries, func(entry json.RawMessage) bool { return desktopEntryID(entry) == next }) {
			next = ""
			if len(entries) > 0 {
				next = desktopEntryID(entries[0])
			}
		}
		if next == "" {
			err = draft.patch(p.meta, nil, "appliedId")
		} else {
			err = draft.patch(p.meta, []edit.KV{{Path: "appliedId", Value: next}})
		}
		if err != nil {
			return err
		}
	}
	if err := draft.restoreImported(p); err != nil {
		return err
	}
	if profile, err := draft.fields(p.prof); err != nil {
		return err
	} else if len(profile) != 0 {
		draft.saved[desktopProfile+suffix] = string(draft.body(p.prof))
	}
	draft.changes[p.prof] = nil
	if err := draft.uncarryMCP(p); err != nil {
		return err
	}
	if loc := draft.saved[desktopLocale+suffix]; loc != "" && slices.Contains(made, p.app3p) {
		if current, _ := draft.fields(p.app3p); len(current) == 1 && desktopString(current["locale"]) == loc {
			draft.changes[p.app3p] = nil
		}
	}
	for _, path := range []string{p.config, p.config3p, p.meta, p.app3p} {
		if slices.Contains(made, path) && (draft.files[path].Exists || draft.body(path) != nil) {
			if fields, err := draft.fields(path); err == nil && len(fields) == 0 {
				draft.changes[path] = nil
			}
		}
	}
	for _, path := range []string{p.dir, p.dir3p, p.library} {
		if slices.Contains(made, path) {
			draft.clean = append(draft.clean, path)
		}
	}
	for _, key := range []string{desktopStashed, desktopMode, desktopMode3p, desktopModes, desktopApplied, desktopMade, desktopLocale, desktopKeyMark} {
		delete(draft.saved, key+suffix)
	}
	return nil
}

func (draft *desktopDraft) seedProfile(p desktopPaths, ssh bool) error {
	fields, err := draft.fields(p.prof)
	if err != nil {
		return err
	}
	var set []edit.KV
	for _, seed := range []struct {
		mark, name string
		value      any
	}{
		{desktopImport, "claudeAiImport", desktopImportOn},
		{desktopSearch, "toolSearchEnabled", true},
		{desktopLong, "modelPrefer1mContext", true},
		{desktopSSH, "sshHostAllowlist", []string{"*"}},
	} {
		if draft.saved[seed.mark+p.key] != "" || seed.mark == desktopSSH && !ssh {
			continue
		}
		if _, exists := fields[seed.name]; !exists {
			set = append(set, edit.KV{Path: seed.name, Value: seed.value})
		}
		draft.saved[seed.mark+p.key] = "1"
	}
	return draft.patch(p.prof, set)
}

func (draft *desktopDraft) carryLocale(p desktopPaths) error {
	if p.app == "" || p.app3p == "" || p.app == p.app3p {
		return nil
	}
	loc, _ := edit.GetJSON(p.app, "locale")
	if !desktopLocaleName.MatchString(loc) {
		return nil
	}
	fields, err := draft.fields(p.app3p)
	if err != nil || fields["locale"] != nil {
		return nil
	}
	draft.saved[desktopLocale+p.key] = loc
	return draft.patch(p.app3p, []edit.KV{{Path: "locale", Value: loc}})
}

func (draft *desktopDraft) carryMCP(p desktopPaths) error {
	draft.saved[desktopMCPSeen+p.key] = "1"
	own, _ := edit.GetJSONRaw(p.mcpFrom, "mcpServers")
	var source map[string]json.RawMessage
	if p.mcpFrom == p.config3p || json.Unmarshal([]byte(own), &source) != nil || len(source) == 0 {
		return nil
	}
	current, err := draft.fields(p.config3p)
	if err != nil {
		return err
	}
	have := map[string]json.RawMessage{}
	if raw := current["mcpServers"]; raw != nil && (json.Unmarshal(raw, &have) != nil || have == nil) {
		return nil
	}
	carried := map[string]json.RawMessage{}
	if prev := draft.saved[desktopMCP+p.key]; prev != "" {
		if json.Unmarshal([]byte(prev), &carried) != nil || carried == nil {
			return errors.New("invalid saved Claude Desktop MCP settings")
		}
	}
	added := false
	for name, value := range source {
		if _, exists := have[name]; !exists {
			have[name], carried[name], added = value, value, true
		}
	}
	if !added {
		return nil
	}
	saved, _ := json.Marshal(carried)
	draft.saved[desktopMCP+p.key] = string(saved)
	return draft.patch(p.config3p, []edit.KV{{Path: "mcpServers", Value: have}})
}

func (draft *desktopDraft) uncarryMCP(p desktopPaths) error {
	key := desktopMCP + p.key
	saved := draft.saved[key]
	if saved == "" {
		return nil
	}
	var carried map[string]json.RawMessage
	if json.Unmarshal([]byte(saved), &carried) != nil {
		return errors.New("invalid saved Claude Desktop MCP settings")
	}
	current, err := draft.fields(p.config3p)
	if err != nil {
		return err
	}
	delete(draft.saved, key)
	var have map[string]json.RawMessage
	if json.Unmarshal(current["mcpServers"], &have) != nil {
		return nil
	}
	count := len(have)
	for name, value := range carried {
		if desktopSameJSON(have[name], value) {
			delete(have, name)
		}
	}
	switch {
	case count == len(have):
		return nil
	case len(have) == 0:
		return draft.patch(p.config3p, nil, "mcpServers")
	default:
		return draft.patch(p.config3p, []edit.KV{{Path: "mcpServers", Value: have}})
	}
}
