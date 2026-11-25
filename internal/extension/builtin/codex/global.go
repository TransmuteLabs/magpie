package codex

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const GlobalConfigFile = "config.toml"

type GatewayMoveOptions struct {
	From, To string
	OwnToken func(string) bool
}

func PlanGatewayMove(options GatewayMoveOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, errors.New("Codex gateway move requires a config.toml snapshot")
	}
	from, to := strings.TrimSuffix(options.From, "/"), strings.TrimSuffix(options.To, "/")
	if from == "" || to == "" || from == to {
		return sdk.AgentPlan{}, nil
	}
	config := settings.Files[0]
	raw := edit.TrimBOM(config.Body)
	move := func(_ edit.TOMLTable, current string) (*string, bool) {
		if !strings.HasPrefix(current, from+"/") {
			return nil, false
		}
		replacement := to + strings.TrimPrefix(current, from)
		return &replacement, true
	}
	body, err := edit.RewriteTOMLScalarMatching(config.Path, raw, "", "openai_base_url", move)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	body, err = edit.RewriteTOMLScalarMatching(config.Path, body, "model_providers.magpie", "base_url", func(table edit.TOMLTable, current string) (*string, bool) {
		if options.OwnToken == nil || !options.OwnToken(table.Scalar("experimental_bearer_token")) {
			return nil, false
		}
		return move(table, current)
	})
	if err != nil || bytes.Equal(body, raw) {
		return sdk.AgentPlan{}, err
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: config.Path, Body: body, BeforeHash: config.BeforeHash}}}, nil
}

type GlobalProviderOptions struct {
	BaseURL         string
	Token           string
	OpenAIAuth      bool
	OpenAIAuthKnown bool
}

type GlobalUnrouteOptions struct {
	Provider       string
	Catalog        string
	Effort         string
	Model          string
	PickModel      *string
	AppliedEfforts []string
	GatewayBase    func(string) bool
	OwnCatalog     func(string) bool
	DesktopCatalog func(string) bool
	RoutedModel    func(string) bool
	ProviderURLs   []ProviderURLRestore
}

type GlobalUnrouteResult struct {
	Routed       bool
	FromProvider bool
}

var ErrJoinedModelRouteChanged = errors.New("Codex's joined model routing changed")

type JoinedModelRoute struct {
	Provider string `toml:"model_provider"`
	BaseURL  string `toml:"openai_base_url"`
	Catalog  string `toml:"model_catalog_json"`
	Model    string `toml:"model"`
}

type JoinedModelOptions struct {
	Current    JoinedModelRoute
	Model      string
	RestoreURL string
	OwnCatalog func(string) bool
}

func PlanJoinedModel(options JoinedModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, errors.New("Codex joined model action requires a config.toml snapshot")
	}
	config := settings.Files[0]
	raw := edit.TrimBOM(config.Body)
	var current JoinedModelRoute
	if err := toml.Unmarshal(edit.TOMLWithout(raw, "projects"), &current); err != nil {
		return sdk.AgentPlan{}, edit.AtFile(config.Path, err)
	}
	if current != options.Current {
		return sdk.AgentPlan{}, ErrJoinedModelRouteChanged
	}
	body := raw
	values := []edit.KV{{Path: "model", Value: options.Model}}
	if options.RestoreURL != "" {
		if current.Provider != "magpie" {
			return sdk.AgentPlan{}, ErrJoinedModelRouteChanged
		}
		remove := []string{"model_provider"}
		if options.OwnCatalog != nil && options.OwnCatalog(current.Catalog) {
			remove = append(remove, "model_catalog_json")
		}
		var err error
		body, err = edit.RewriteTOMLWithoutTop(config.Path, body, remove...)
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		values = []edit.KV{{Path: "openai_base_url", Value: options.RestoreURL}, values[0]}
	}
	body, err := edit.RewriteTOMLTop(config.Path, body, values...)
	if err != nil || bytes.Equal(body, raw) {
		return sdk.AgentPlan{}, err
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: config.Path, Body: body, BeforeHash: config.BeforeHash}}}, nil
}

func PlanGlobalUnroute(options GlobalUnrouteOptions, settings sdk.ActionSettings) (sdk.AgentPlan, GlobalUnrouteResult, error) {
	var result GlobalUnrouteResult
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, result, errors.New("Codex unroute action requires a config.toml snapshot")
	}
	config := settings.Files[0]
	raw := edit.TrimBOM(config.Body)
	var current struct {
		Provider string `toml:"model_provider"`
		BaseURL  string `toml:"openai_base_url"`
		Catalog  string `toml:"model_catalog_json"`
		Effort   string `toml:"model_reasoning_effort"`
		Model    string `toml:"model"`
	}
	if err := toml.Unmarshal(edit.TOMLWithout(raw, "projects"), &current); err != nil {
		return sdk.AgentPlan{}, result, edit.AtFile(config.Path, err)
	}
	var remove []string
	var values []edit.KV
	base := options.GatewayBase(current.BaseURL)
	result.FromProvider = current.Provider == "magpie"
	result.Routed = base || result.FromProvider
	if base {
		remove = append(remove, "openai_base_url")
	}
	if result.Routed {
		if options.Provider != "" && options.Provider != "magpie" && (current.Provider == "" || result.FromProvider) {
			values = append(values, edit.KV{Path: "model_provider", Value: options.Provider})
		} else if result.FromProvider {
			remove = append(remove, "model_provider")
		}
		ownedCatalog := result.FromProvider && options.OwnCatalog(current.Catalog)
		if options.Catalog != "" && (current.Catalog == "" || ownedCatalog || options.DesktopCatalog(current.Catalog)) {
			values = append(values, edit.KV{Path: "model_catalog_json", Value: options.Catalog})
		} else if ownedCatalog {
			remove = append(remove, "model_catalog_json")
		}
		if options.Effort != "" && (current.Effort == "" || slices.Contains(options.AppliedEfforts, current.Effort)) {
			values = append(values, edit.KV{Path: "model_reasoning_effort", Value: options.Effort})
		}
	}
	if options.PickModel != nil {
		values = append(values, edit.KV{Path: "model", Value: *options.PickModel})
	} else if current.Model == "" || options.RoutedModel(current.Model) {
		if result.Routed && options.Model != "" && !options.RoutedModel(options.Model) {
			values = append(values, edit.KV{Path: "model", Value: options.Model})
		} else if current.Model != "" {
			remove = append(remove, "model")
		}
	}
	body, err := rewriteProviderURLs(config.Path, raw, options.ProviderURLs)
	if err != nil {
		return sdk.AgentPlan{}, result, err
	}
	if options.PickModel == nil || result.Routed {
		body, err = rewriteAuxiliaryModels(config.Path, body, AuxiliaryModelOptions{RoutedModel: options.RoutedModel})
		if err != nil {
			return sdk.AgentPlan{}, result, err
		}
	}
	if len(remove) != 0 {
		body, err = edit.RewriteTOMLWithoutTop(config.Path, body, remove...)
		if err != nil {
			return sdk.AgentPlan{}, result, err
		}
	}
	if len(values) != 0 {
		body, err = edit.RewriteTOMLTop(config.Path, body, values...)
		if err != nil {
			return sdk.AgentPlan{}, result, err
		}
	}
	if bytes.Equal(body, raw) {
		return sdk.AgentPlan{}, result, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: config.Path, Body: body, BeforeHash: config.BeforeHash}}}, result, nil
}

type ProviderURLRestore struct {
	ID       string
	Expected string
	Saved    *string
}

func PlanProviderURLs(restores []ProviderURLRestore, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, errors.New("Codex provider URL action requires a config.toml snapshot")
	}
	config := settings.Files[0]
	raw := edit.TrimBOM(config.Body)
	body, err := rewriteProviderURLs(config.Path, raw, restores)
	if err != nil || bytes.Equal(body, raw) {
		return sdk.AgentPlan{}, err
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: config.Path, Body: body, BeforeHash: config.BeforeHash}}}, nil
}

func rewriteProviderURLs(path string, raw []byte, restores []ProviderURLRestore) ([]byte, error) {
	body := raw
	for _, restore := range restores {
		next, err := edit.RewriteTOMLScalarIf(path, body, "model_providers."+restore.ID, "base_url", restore.Expected, restore.Saved)
		if err != nil {
			return nil, err
		}
		body = next
	}
	return body, nil
}

func PlanGlobalProvider(options GlobalProviderOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, errors.New("Codex global provider action requires a config.toml snapshot")
	}
	config := settings.Files[0]
	raw := edit.TrimBOM(config.Body)
	body, err := edit.RewriteTOMLTable(config.Path, raw, ProviderTablePath, func(table edit.TOMLTable) []edit.KV {
		fields := providerTable(options.BaseURL, options.Token)
		if caps := providerCapabilities(raw); caps != nil {
			fields = append(fields, edit.KV{Path: "capabilities", Value: caps})
		}
		switch {
		case table.Scalar("requires_openai_auth") == "false":
			fields = append(fields, edit.KV{Path: "requires_openai_auth", Value: false})
		case options.OpenAIAuth || (!options.OpenAIAuthKnown && table.Scalar("requires_openai_auth") == "true"):
			fields = append(fields, edit.KV{Path: "requires_openai_auth", Value: true})
		}
		return fields
	})
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: config.Path, Body: body, BeforeHash: config.BeforeHash}}}, nil
}

func providerCapabilities(raw []byte) edit.Inline {
	var config struct {
		ModelProviders any `toml:"model_providers"`
	}
	if err := toml.Unmarshal(edit.TOMLWithout(raw, "projects"), &config); err != nil {
		return nil
	}
	providers, _ := config.ModelProviders.(map[string]any)
	provider, _ := providers["magpie"].(map[string]any)
	caps, _ := provider["capabilities"].(map[string]any)
	var out edit.Inline
	for _, key := range slices.Sorted(maps.Keys(caps)) {
		switch value := caps[key].(type) {
		case string, bool, int64, float64:
			out = append(out, edit.KV{Path: key, Value: value})
		}
	}
	return out
}
