package agent

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinpi "github.com/yetone/magpie/internal/extension/builtin/pi"
)

// The protocol Pi asks magpie's models on, picked in Pi's row
// (xiaomanweiman on X): T3 Code's Pi puts all 71 of its tools in the
// context on openai-completions and 3 on openai-responses, loading the
// rest when asked, and they had to set providers.magpie.api by hand
// (pi_api_kept.go keeps that). The gateway serves each of pi-ai's APIs
// below, every model on each, so any of them can be asked for all of
// magpie's models. Unpicked, it is as before: the provider on
// openai-completions, each model on the API its provider serves natively
// (piModelJSON). An api the user typed in models.json still wins, and the
// row shows it; a pick made after it is the newer word and replaces it.

// piProtocols are the APIs of pi-ai's (KnownApi, pi-ai 1.4.0) the gateway
// serves, as dshAPIs: Chat at /v1/chat/completions, Responses at
// /v1/responses, Messages at /v1/messages. google-generative-ai is left
// out, as piAsked leaves it out of Pi's folder.
var piProtocols = dshAPIs

// piProtocolNames are the protocols as the row's picker lists them.
var piProtocolNames = map[string]string{
	"openai-completions": "OpenAI Chat Completions",
	"openai-responses":   "OpenAI Responses",
	"anthropic-messages": "Anthropic Messages",
}

// piProtocolKey is where the stash keeps the protocol picked for Pi at a
// place, "" for each model's own.
func piProtocolKey(at place) string { return at.key("pi.protocol") }

// piProtocol is the protocol picked for Pi at a place.
func piProtocol(at place) string {
	v := stashShared()[piProtocolKey(at)]
	if !slices.Contains(piProtocols, v) {
		return ""
	}
	return v
}

// piProviderOn is magpie's provider block for Pi's models.json, every
// model asked on the api on, or each on its own for "".
func piProviderOn(agentID string, models []catalog.Model, gw, on string) map[string]any {
	ms := []map[string]any{}
	for _, m := range models {
		ms = append(ms, piModelOn(piAsked(agentID, m), gw, true, on))
	}
	api := "openai-completions"
	if on != "" {
		api = on
	}
	// Pi names its session to a provider only when asked to:
	// sendSessionAffinityHeaders has it send it on Chat
	// (x-session-affinity, session_id) and Anthropic's Messages API
	// (x-session-affinity). On Responses it sends session_id anyway.
	// Without it a turn of Pi's on Chat or Messages reached the gateway
	// with no session: its usage had none and "Record gateway
	// conversations" kept none of it (akic404 on Discord).
	return map[string]any{"name": "magpie", "baseUrl": gw + "/v1", "api": api, "apiKey": keyAt(gw),
		"compat": map[string]any{"sendSessionAffinityHeaders": true}, "models": ms}
}

// piProtocolField is Pi's protocol, the api magpie's provider has in the
// models.json at modelsPath; pick rewrites the block when it is there,
// and off says magpie doesn't write the block (the pi-magpie extension
// serves the provider), so there is nothing to pick.
func piProtocolField(at place, modelsPath string, pick func(string) error, off func() bool) Field {
	return protocolField(at, piProtocolKey(at), modelsPath, func() string {
		v, _ := edit.GetJSON(modelsPath, "providers."+magpieID+".api")
		return v
	}, pick, off)
}

func pickPiProtocol(at place, modelsPath string, key func() string, off func() bool, syncExtension func() error, chosen string) error {
	updates := map[string]string{piProtocolKey(at): chosen}
	if off() {
		stash(updates)
		return nil
	}
	found := false
	prepare := func(current string) (any, error) {
		found = true
		block := piProviderOn("pi", magpieModels("pi"), at.gw(), chosen)
		block["apiKey"] = key()
		return piBlockKept(modelsPath, current, block, true, updates), nil
	}
	if err := extension.ApplyLinkedAgentPlan(prepare, builtinpi.PlanCatalogPick,
		extension.LinkedActionFile{Name: builtinpi.CatalogFile, Path: modelsPath}); err != nil {
		return err
	}
	stash(updates)
	if found {
		return syncExtension()
	}
	return nil
}

// protocolField is an agent's protocol, picked in its row and kept in the
// stash at key: the api magpie's provider has in the models file at
// modelsPath, which fileAPI reads (has: magpie's provider is in the file).
// pick rewrites the provider when it is there, and off says magpie
// doesn't write it, so there is nothing to pick. Pi's (piProtocolField)
// and omp's (omp_protocol.go).
func protocolField(at place, key, modelsPath string, fileAPI func() string, pick func(string) error, off func() bool) Field {
	picked := func() string {
		if v := stashShared()[key]; slices.Contains(piProtocols, v) {
			return v
		}
		return ""
	}
	return Field{
		Key: "protocol", Label: "protocol", Quiet: true,
		Get: func() string {
			picked := picked()
			if at.cold {
				// a stopped distro's files start it (#3896)
				return picked
			}
			file := fileAPI()
			if file == "" {
				return picked
			}
			want := picked
			if want == "" {
				want = "openai-completions"
			}
			// one the user typed there is what the agent asks on
			if file != want && !slices.Contains(piAPIsWanted(modelsPath), file) {
				return file
			}
			return picked
		},
		Set: func(v string) error {
			if v != "" && !slices.Contains(piProtocols, v) {
				return fmt.Errorf("protocol is one of %v or empty (each model's own), not %q", piProtocols, v)
			}
			return pick(v)
		},
		Options: func(map[string]string) []Option {
			if at.cold || off() {
				return nil
			}
			out := []Option{{Value: "", Label: "Each model's own", Note: "each model on the API its provider serves"}}
			for _, p := range piProtocols {
				out = append(out, Option{Value: p, Label: piProtocolNames[p], Note: p})
			}
			return out
		},
	}
}

// piAPIsWanted are the APIs magpie has handed its provider in the Pi
// models.json at path (piAPIsKept's record).
func piAPIsWanted(path string) []string {
	rec := map[string][]string{}
	if s := stashShared()[apisWantedKey(path)]; s != "" && json.Unmarshal([]byte(s), &rec) != nil {
		return nil
	}
	if _, known := rec[piProviderAPI]; !known {
		return []string{"openai-completions"}
	}
	return rec[piProviderAPI]
}
