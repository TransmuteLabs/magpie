package gateway

import (
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// The reasoning level picked in Settings for a model magpie asks on its
// own, beside the model in use: Image recognition's and the searcher's
// (MOMO on Discord, "外置模型能不能提供思考深度选项"). Only those calls
// are asked so; every other request goes as it did.

// ModelEfforts are the reasoning levels model ("<provider>/<model>") has,
// weakest first, for Settings to offer beside it; none for one whose
// levels aren't known, or a routing group.
func ModelEfforts(model string) []string {
	p, m, ok := provider.Resolve(model)
	if !ok {
		return nil
	}
	return ByStrength(p.Efforts(m))
}

// sideEffort is want fitted to model's own levels: "" when none is
// wanted, or the model's levels aren't known, so nothing is sent that it
// might refuse.
func sideEffort(model, want string) string {
	levels := ModelEfforts(model)
	if want == "" || len(levels) == 0 {
		return ""
	}
	return fitEffort(want, levels)
}

// visionEffort returns the level and whether the user picked it, so the
// description can give an explicit level room to think.
func visionEffort(model string) (effort string, picked bool) {
	want := provider.HeldSettings().VisionEffort
	p, m, ok := provider.Resolve(model)
	if !ok {
		return "", false
	}
	levels := p.Efforts(m)
	if len(levels) == 0 {
		return "", false
	}
	if want != "" {
		return fitEffort(want, ByStrength(levels)), true
	}
	return fitEffort("none", levels), false
}

// leastEffort is the least reasoning model takes: none when it can go
// without, else its lowest level; "" when its levels aren't known, which
// leaves it to the vendor.
func leastEffort(model string) string {
	p, m, ok := provider.Resolve(model)
	if !ok {
		return ""
	}
	levels := p.Efforts(m)
	if len(levels) == 0 {
		return ""
	}
	return fitEffort("none", levels)
}

// VisionSettings gives the Settings row its automatic pick, any missing
// manual pick, and the effective model's offered and fitted levels.
func VisionSettings(picked, want string) (auto, missing string, levels []string, effort string) {
	vision := ProjectVisionSettings(picked, want)
	vision.Settle()
	return vision.Auto, vision.Missing, vision.Levels, vision.Effort
}

// VisionProjection is the Settings row and the automatic pick it can settle
// after its caller has accepted the catalog it was built from.
type VisionProjection struct {
	Auto, Missing string
	Levels        []string
	Effort        string
	pick          visionPick
}

func ProjectVisionSettings(picked, want string) VisionProjection {
	pick := chooseVision()
	vision := VisionProjection{Auto: pick.model, pick: pick}
	if picked == "off" {
		return vision
	}
	if picked != "" {
		if p, model, ok := provider.Resolve(picked); ok {
			vision.Levels = ByStrength(p.Efforts(model))
		} else {
			vision.Missing = picked
		}
	}
	if vision.Auto != "" && (picked == "" || vision.Missing != "") {
		vision.Levels = ModelEfforts(vision.Auto)
	}
	if want != "" && len(vision.Levels) != 0 {
		vision.Effort = fitEffort(want, vision.Levels)
	}
	return vision
}

func (vision VisionProjection) Settle() { vision.pick.settle() }

// searchEffort is the level the searcher's model, provider p's model, is
// asked to search at: the one picked in Settings fitted to its levels,
// or "" to send none.
func searchEffort(p provider.Provider, model string) string {
	return searchEffortWith(p, model, settings.Shared().SearcherEffort)
}

func searchEffortWith(p provider.Provider, model, want string) string {
	if model == "" || model == codexSearchModel {
		return ""
	}
	return sideEffort(p.ID+"/"+model, want)
}

// SearchEffortNow is the level the searcher in use is asked at, when one
// is picked in Settings.
func SearchEffortNow() string {
	if p, m, ok := searcher(); ok {
		return searchEffort(p, m)
	}
	return ""
}

// SearchEfforts are the levels of the model that searches now: none for
// a Kimi Code plan's search service, which has no model.
func SearchEfforts() []string {
	if p, m, ok := searcher(); ok && m != "" && m != codexSearchModel {
		return ModelEfforts(p.ID + "/" + m)
	}
	return nil
}

// replyRoom is the reply limit a side call asked to think at effort is
// sent with: its usual limit with room for the thinking above it, so a
// level picked higher doesn't spend the reply on thinking and leave no
// answer. The model's own limit still caps it (withMaxOutput).
func replyRoom(usual int, effort string) int {
	switch effort {
	case "", "none":
		return usual
	case "minimal":
		return usual + 1024
	}
	return usual + budgetOf(effortOf(effort))
}

// SideEffort is the level a call magpie makes on its own with model is
// asked at when want is picked for it: want fitted to the model's own
// levels, "" when none is picked or its levels aren't known.
func SideEffort(model, want string) string { return sideEffort(model, want) }

// WithSideEffort is an Anthropic Messages request magpie makes on its own
// with model (memory's reading of conversations and its nightly tidy)
// asked to reason at want, fitted to the model's levels as SideEffort
// fits it, as a group's member fixed at a level is asked (withFixedEffort):
// none turns thinking off, any other level asks for it, with room above
// the reply's usual limit to think in (replyRoom). Nothing picked, or a
// model whose levels aren't known: the body as it was.
func WithSideEffort(body []byte, model, want string) []byte {
	e := sideEffort(model, want)
	if e == "" {
		return body
	}
	var v struct {
		MaxTokens int `json:"max_tokens"`
	}
	if decodeJSON(body, &v) == nil && v.MaxTokens > 0 {
		body = withFields(body, map[string]any{"max_tokens": replyRoom(v.MaxTokens, e)})
	}
	return withFixedEffort(provider.Anthropic, body, e)
}
