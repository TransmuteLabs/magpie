package reasonix

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type Action uint8

const (
	SelectModel Action = iota
	SelectPlanner
	SelectEffort
	SyncCatalog
)

type Options struct {
	Action  Action
	Value   string
	Catalog func() []Model
	URL     func() string
	Key     func() string
	OwnKey  func(token, key string) bool
}

type settingsEdit struct {
	config, env, restore sdk.ActionSettingFile
	body, envBody        []byte
	cfg                  Config
	state                *Restore
	removeConfig         bool
	removeEnv            bool
	writeRestore         bool
}

func Plan(options Options, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	files := settings.Files
	if len(files) != 3 || files[0].Path != ConfigFile || files[1].Path != EnvFile || files[2].Path != RestoreFile {
		return plan, errors.New("Reasonix settings plan needs config.toml, .env and its restore record")
	}
	update := settingsEdit{
		config: files[0], env: files[1], restore: files[2],
		body: edit.TrimBOM(files[0].Body), envBody: files[1].Body,
	}
	var err error
	update.cfg, err = ParseConfig(update.body)
	if err != nil {
		return plan, edit.AtFile(ConfigFile, err)
	}
	if update.restore.Exists {
		update.state, err = ParseRestore(update.restore.Body)
		if err != nil {
			return plan, edit.AtFile(RestoreFile, err)
		}
	}
	selection := options.Value
	ref, via := strings.CutPrefix(selection, ProviderID+"/")
	switch options.Action {
	case SelectModel:
		if via {
			err = update.ensureGateway(options, ref, ref)
			if err == nil {
				err = update.setDefault(selection)
			}
		} else if selection != "" && !update.cfg.hasModel(selection) &&
			!matches(update.cfg.DefaultModel, selection) && (update.state == nil || !matches(update.state.Default, selection)) {
			err = fmt.Errorf("Reasonix has no model %q", selection)
		} else {
			err = update.restoreGateway(selection, options.Key)
		}
	case SelectPlanner:
		err = update.selectPlanner(options, selection, ref, via)
	case SelectEffort:
		err = update.selectEffort(selection)
	case SyncCatalog:
		err = update.syncCatalog(options)
	default:
		err = errors.New("unknown Reasonix settings action")
	}
	if err != nil {
		return plan, err
	}
	appendChange(&plan, update.config, update.body, update.removeConfig)
	appendChange(&plan, update.env, update.envBody, update.removeEnv)
	switch {
	case update.state == nil:
		appendChange(&plan, update.restore, nil, true)
	case update.writeRestore:
		raw, err := json.Marshal(update.state)
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		appendChange(&plan, update.restore, raw, false)
	}
	return plan, nil
}

func (u *settingsEdit) selectPlanner(options Options, selection, ref string, via bool) error {
	off := selection == "off"
	if off {
		selection = ""
	}
	if via {
		selected := ref
		if usesGateway(u.cfg.DefaultModel) {
			selected = strings.TrimPrefix(*u.cfg.DefaultModel, ProviderID+"/")
		}
		if err := u.ensureGateway(options, ref, selected); err != nil {
			return err
		}
		if !u.state.PlannerManaged {
			u.state.Planner, u.state.PlannerManaged = u.cfg.Agent.Planner, true
		}
	} else if selection != "" && !u.cfg.hasModel(selection) && !matches(u.cfg.Agent.Planner, selection) &&
		!matches(u.cfg.DefaultModel, selection) &&
		(u.state == nil || !matches(u.state.Planner, selection) && !matches(u.state.Default, selection)) {
		return fmt.Errorf("Reasonix has no planner model %q", selection)
	}
	if selection == "" && !off && u.state != nil && u.state.PlannerManaged && u.state.Planner != nil {
		selection = *u.state.Planner
	}
	var err error
	if selection == "" {
		u.body, err = edit.RewriteTOMLWithoutKey(ConfigFile, u.body, "agent", "planner_model")
		u.cfg.Agent.Planner = nil
	} else {
		u.body, err = edit.RewriteTOMLKey(ConfigFile, u.body, "agent", "planner_model", selection)
		u.cfg.Agent.Planner = &selection
	}
	if err == nil && !via && !usesGateway(u.cfg.DefaultModel) {
		return u.restoreGateway("", options.Key)
	}
	u.writeRestore = u.state != nil
	return err
}

func (u *settingsEdit) selectEffort(effort string) error {
	if u.state == nil || !usesGateway(u.cfg.DefaultModel) {
		if effort == "" {
			return nil
		}
		return fmt.Errorf("select a magpie model in Reasonix before setting effort")
	}
	if err := CheckOwner(u.cfg, u.state, u.envBody); err != nil {
		return err
	}
	p := u.cfg.Magpie()
	ref := strings.TrimPrefix(*u.cfg.DefaultModel, ProviderID+"/")
	if effort == "auto" {
		effort = ""
	}
	if effort != "" && !slices.Contains(p.Overrides[ref].Efforts, effort) {
		return fmt.Errorf("Reasonix model %q does not support effort %q", ref, effort)
	}
	p.Effort = effort
	return u.setProvider(p)
}

func (u *settingsEdit) syncCatalog(options Options) error {
	if u.state == nil {
		return nil
	}
	if err := CheckOwner(u.cfg, u.state, u.envBody); err != nil {
		return err
	}
	current := u.cfg.Magpie()
	if current.Name == "" {
		return nil
	}
	models := options.Catalog()
	if len(models) == 0 {
		return fmt.Errorf("magpie has no visible Reasonix models")
	}
	next := Catalog(models, current.Default, current.Effort, options.URL())
	if reflect.DeepEqual(current, next) {
		return nil
	}
	return u.setProvider(next)
}

func (u *settingsEdit) ensureGateway(options Options, ref, selected string) error {
	models := options.Catalog()
	if !slices.ContainsFunc(models, func(model Model) bool { return model.ID == ref }) {
		return fmt.Errorf("magpie has no visible model %q", ref)
	}
	if err := CheckOwner(u.cfg, u.state, u.envBody); err != nil {
		return err
	}
	if u.state == nil {
		u.state = &Restore{Version: 1, Default: u.cfg.DefaultModel, NewConfig: !u.config.Exists, NewEnv: !u.env.Exists}
	}
	p := Catalog(models, selected, u.cfg.Magpie().Effort, options.URL())
	if err := u.setProvider(p); err != nil {
		return err
	}
	access := u.cfg.Desktop.Access
	if access != nil && !slices.Contains(access, ProviderID) {
		if err := u.setAccess(append(access, ProviderID)); err != nil {
			return err
		}
		u.state.AddedAccess = true
	}
	key := options.Key()
	if token, _ := edit.GetEnv(u.envBody, Key); !options.OwnKey(token, key) {
		u.envBody = edit.PatchEnv(u.envBody, edit.KV{Path: Key, Value: key})
	}
	u.writeRestore = true
	return nil
}

func (u *settingsEdit) restoreGateway(native string, gatewayKey func() string) error {
	if u.state == nil {
		if native != "" {
			return u.setDefault(native)
		}
		return nil
	}
	if err := CheckOwner(u.cfg, u.state, u.envBody); err != nil {
		return err
	}
	keep := usesGateway(u.cfg.Agent.Planner)
	if !keep {
		var err error
		u.body, err = edit.RewriteTOMLArrayTable(ConfigFile, u.body, "providers", "name", ProviderID, "")
		if err != nil {
			return err
		}
	}
	switch {
	case native != "":
		if err := u.setDefault(native); err != nil {
			return err
		}
	case usesGateway(u.cfg.DefaultModel):
		var err error
		if u.state.Default == nil {
			u.body, err = edit.RewriteTOMLWithoutTop(ConfigFile, u.body, "default_model")
		} else {
			err = u.setDefault(*u.state.Default)
		}
		if err != nil {
			return err
		}
	}
	if keep {
		return nil
	}
	if u.state.AddedAccess && slices.Contains(u.cfg.Desktop.Access, ProviderID) {
		access := slices.DeleteFunc(u.cfg.Desktop.Access, func(value string) bool { return value == ProviderID })
		if err := u.setAccess(access); err != nil {
			return err
		}
	}
	if token, exists := edit.GetEnv(u.envBody, Key); exists && token == gatewayKey() {
		u.envBody, _ = edit.PatchEnvWithout(u.envBody, Key)
		u.removeEnv = u.envBody == nil
	}
	u.removeConfig = u.state.NewConfig && len(bytes.TrimSpace(u.body)) == 0
	u.removeEnv = u.removeEnv || u.state.NewEnv && len(bytes.TrimSpace(edit.TrimBOM(u.envBody))) == 0
	u.state = nil
	return nil
}

func (u *settingsEdit) setDefault(selection string) (err error) {
	u.body, err = edit.RewriteTOMLTopPreserving(ConfigFile, u.body, edit.KV{Path: "default_model", Value: selection})
	return err
}

func (u *settingsEdit) setProvider(p Provider) (err error) {
	block, err := toml.Marshal(struct {
		Providers []Provider `toml:"providers"`
	}{[]Provider{p}})
	if err != nil {
		return err
	}
	u.body, err = edit.RewriteTOMLArrayTable(ConfigFile, u.body, "providers", "name", ProviderID, string(block))
	return err
}

func (u *settingsEdit) setAccess(access []string) (err error) {
	quoted := make([]string, len(access))
	for i, value := range access {
		quoted[i] = strconv.Quote(value)
	}
	u.body, err = edit.RewriteTOMLKey(ConfigFile, u.body, "desktop", "provider_access", edit.Raw("["+strings.Join(quoted, ", ")+"]"))
	return err
}

func usesGateway(value *string) bool {
	return value != nil && strings.HasPrefix(*value, ProviderID+"/")
}

func matches(value *string, selection string) bool {
	return value != nil && *value == selection
}

func appendChange(plan *sdk.AgentPlan, file sdk.ActionSettingFile, body []byte, remove bool) {
	if remove && !file.Exists || !remove && bytes.Equal(edit.TrimBOM(file.Body), edit.TrimBOM(body)) {
		return
	}
	if remove {
		body = nil
	}
	plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, Body: body, BeforeHash: file.BeforeHash, Remove: remove})
}
