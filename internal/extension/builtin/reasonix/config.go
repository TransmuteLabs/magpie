package reasonix

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/lazyre"
)

const (
	ConfigFile  = "config.toml"
	EnvFile     = ".env"
	RestoreFile = "restore.json"
	ProviderID  = "magpie"
	Key         = "MAGPIE_REASONIX_API_KEY"
)

var keyLine = lazyre.New(`(?m)^\s*(?:export\s+)?` + Key + `\s*=`)

type Config struct {
	DefaultModel *string `toml:"default_model"`
	Agent        struct {
		Planner *string `toml:"planner_model"`
	} `toml:"agent"`
	Providers []Provider `toml:"providers"`
	Desktop   struct {
		Access []string `toml:"provider_access"`
	} `toml:"desktop"`
}

type Provider struct {
	Name      string                   `toml:"name"`
	Kind      string                   `toml:"kind"`
	BaseURL   string                   `toml:"base_url"`
	APIKeyEnv string                   `toml:"api_key_env"`
	Model     string                   `toml:"model,omitempty"`
	Models    []string                 `toml:"models,omitempty"`
	Default   string                   `toml:"default,omitempty"`
	Effort    string                   `toml:"effort,omitempty"`
	Protocol  string                   `toml:"reasoning_protocol,omitempty"`
	NoProxy   bool                     `toml:"no_proxy,omitempty"`
	Overrides map[string]modelOverride `toml:"model_overrides,omitempty"`
}

type modelOverride struct {
	Protocol string   `toml:"reasoning_protocol,omitempty"`
	Efforts  []string `toml:"supported_efforts,omitempty"`
	Vision   *bool    `toml:"vision,omitempty"`
	Context  int      `toml:"context_window,omitempty"`
	Output   int      `toml:"max_output_tokens,omitempty"`
}

type Restore struct {
	Version        int     `json:"version"`
	Planner        *string `json:"planner_model,omitempty"`
	PlannerManaged bool    `json:"planner_managed,omitempty"`
	Default        *string `json:"default_model"`
	NewConfig      bool    `json:"new_config"`
	NewEnv         bool    `json:"new_env"`
	AddedAccess    bool    `json:"added_access"`
}

func ParseConfig(raw []byte) (Config, error) {
	var cfg Config
	if err := toml.Unmarshal(edit.TrimBOM(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("Reasonix config.toml: %w", err)
	}
	names := map[string]bool{}
	for _, provider := range cfg.Providers {
		if strings.TrimSpace(provider.Name) == ProviderID && provider.Name != ProviderID {
			return cfg, fmt.Errorf("Reasonix provider has a noncanonical magpie name")
		}
		if names[provider.Name] {
			return cfg, fmt.Errorf("Reasonix has duplicate provider %q", provider.Name)
		}
		names[provider.Name] = true
	}
	return cfg, nil
}

func ParseRestore(raw []byte) (*Restore, error) {
	raw = edit.TrimBOM(raw)
	var state Restore
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("Reasonix restore record: %w", err)
	}
	if state.Version != 1 {
		return nil, fmt.Errorf("Reasonix restore record has an unsupported or missing version")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"default_model", "new_config", "new_env", "added_access"} {
		value, present := fields[key]
		if !present || key != "default_model" && strings.TrimSpace(string(value)) == "null" {
			return nil, fmt.Errorf("Reasonix restore record is incomplete")
		}
	}
	return &state, nil
}

func (cfg Config) Magpie() Provider {
	for _, provider := range cfg.Providers {
		if provider.Name == ProviderID {
			return provider
		}
	}
	return Provider{}
}

func (cfg Config) hasModel(ref string) bool {
	for _, provider := range cfg.Providers {
		if provider.Name == ProviderID {
			continue
		}
		if ref == provider.Name || ref == provider.Model || slices.Contains(provider.Models, ref) {
			return true
		}
		if name, model, ok := strings.Cut(ref, "/"); ok && name == provider.Name && (model == provider.Model || slices.Contains(provider.Models, model)) {
			return true
		}
	}
	return false
}

func CheckOwner(cfg Config, state *Restore, env []byte) error {
	for _, provider := range cfg.Providers {
		if provider.Name == ProviderID && (state == nil || provider.APIKeyEnv != Key) {
			return fmt.Errorf("Reasonix already has a provider named magpie that magpie does not own")
		}
		if provider.Name != ProviderID && provider.APIKeyEnv == Key {
			return fmt.Errorf("Reasonix's %s is used by another provider", Key)
		}
	}
	if len(keyLine.FindAllIndex(edit.TrimBOM(env), 2)) > 1 {
		return fmt.Errorf("Reasonix credentials repeat %s", Key)
	}
	if _, exists := edit.GetEnv(env, Key); exists && state == nil {
		return fmt.Errorf("Reasonix's %s already exists; it was not created by magpie", Key)
	}
	return nil
}

type Model struct {
	ID      string
	Efforts []string
	Vision  *bool
	Context int
	Output  int
}

func Catalog(models []Model, selected, effort, url string) Provider {
	provider := Provider{Name: ProviderID, Kind: "openai", BaseURL: url, APIKeyEnv: Key,
		Default: selected, Protocol: "openai", NoProxy: true, Overrides: map[string]modelOverride{}}
	for _, model := range models {
		provider.Models = append(provider.Models, model.ID)
		override := modelOverride{Vision: model.Vision, Context: model.Context, Output: model.Output}
		for _, level := range model.Efforts {
			if level == "off" {
				level = "none"
			}
			if slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, level) && !slices.Contains(override.Efforts, level) {
				override.Efforts = append(override.Efforts, level)
			}
		}
		if len(override.Efforts) == 0 {
			override.Protocol = "none"
		}
		provider.Overrides[model.ID] = override
	}
	if len(provider.Models) == 0 {
		provider.Default = ""
	} else if !slices.Contains(provider.Models, selected) {
		provider.Default = provider.Models[0]
	}
	if slices.Contains(provider.Overrides[provider.Default].Efforts, effort) {
		provider.Effort = effort
	}
	return provider
}
