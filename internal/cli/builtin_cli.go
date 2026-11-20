package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/yetone/magpie/internal/pluginrt"
)

// builtinCmd: `magpie plugin builtin [on|off <id>]` — magpie's built-in
// plugins (internal/pluginrt, plugins/), as Plugins › Built-in shows them.
// One in use is refused off, as the window refuses it.
func builtinCmd(rest []string) error {
	if len(rest) == 0 || rest[0] == "list" {
		snapshot, err := pluginrt.RefreshState()
		if err != nil {
			return err
		}
		for _, m := range pluginrt.Registered() {
			index := slices.IndexFunc(snapshot.Statuses, func(status pluginrt.Status) bool { return status.ID == m.ID })
			if index < 0 {
				continue
			}
			status := snapshot.Statuses[index]
			mark, how := muted.Render("○"), "off"
			switch status.State {
			case pluginrt.Active:
				mark, how = green.Render("●"), "on"
				if slices.ContainsFunc(snapshot.Configuration, func(c pluginrt.Composed) bool {
					return c.ID == m.ID && c.Source == pluginrt.FromInUse
				}) {
					how = "on, in use"
				}
			case pluginrt.Failed, pluginrt.Pending:
				how = string(status.State)
			}
			name := m.Name
			if status.Why != "" {
				name += " (" + status.Why + ")"
			}
			fmt.Printf("%s %-10s %-12s %s\n", mark, m.ID, how, muted.Render(name))
		}
		fmt.Println(muted.Render("in " + tildePath(pluginrt.Path())))
		return nil
	}
	if len(rest) != 2 || (rest[0] != "on" && rest[0] != "off") {
		return errors.New(pluginUsage)
	}
	on, id := rest[0] == "on", rest[1]
	cs, err := pluginrt.Refresh()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cs, func(c pluginrt.Composed) bool { return c.ID == id })
	if i < 0 {
		return fmt.Errorf("no built-in plugin %q", id)
	}
	if !on && cs[i].InUse > 0 {
		return fmt.Errorf("%s is in use and stays on", id)
	}
	if err := pluginrt.Set(id, on); err != nil {
		return err
	}
	fmt.Println(green.Render("✓"), id, "is", rest[0])
	return nil
}

// dumpPlugins: `magpie plugin dump` — what plugins.yml comes to after its
// layers: each built-in plugin, on or off and why, its config and the
// layer each value is from, and how it runs in this process.
func dumpPlugins(asJSON bool) error {
	snapshot, err := pluginrt.RefreshState()
	if err != nil {
		return err
	}
	type plug struct {
		pluginrt.Composed `yaml:",inline"`
		State             pluginrt.State `json:"state" yaml:"state"`
		Why               string         `json:"why,omitempty" yaml:"why,omitempty"`
	}
	st := map[string]pluginrt.Status{}
	for _, s := range snapshot.Statuses {
		st[s.ID] = s
	}
	out := struct {
		File    string `json:"file" yaml:"file"`
		There   bool   `json:"there" yaml:"there"`
		Preset  string `json:"preset" yaml:"preset"`
		API     string `json:"api" yaml:"api"`
		Plugins []plug `json:"plugins" yaml:"plugins"`
	}{File: pluginrt.Path(), There: snapshot.FilePresent, Preset: snapshot.Preset, API: pluginrt.API}
	if out.Preset == "" {
		out.Preset = pluginrt.PresetAll
	}
	for _, c := range snapshot.Configuration {
		out.Plugins = append(out.Plugins, plug{Composed: c, State: st[c.ID].State, Why: st[c.ID].Why})
	}
	if asJSON {
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	e := yaml.NewEncoder(os.Stdout)
	e.SetIndent(2)
	defer e.Close()
	return e.Encode(out)
}
