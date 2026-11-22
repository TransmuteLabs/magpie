package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
)

// While a model is picked for one of Claude Desktop's tiers, magpie's
// profile carries inferenceModels (gateway.DesktopInferenceModels): the
// list Desktop's Code tab hands Claude Code each pick's behavesAs from,
// so a pick runs with the prompt of the Claude model of its tier
// (moer2831), and while two rows Desktop names after one Claude model of
// its catalog would read alike (labelOverride, onkashiko on X). Without
// either there is none, and Desktop finds magpie's
// models on its own as before. The list is magpie's only while it is what
// magpie wrote: desktopModelsMark keeps the hash of each list magpie
// wrote (the one before a write too, until the write is done), and a list
// with any other hash is the user's, which magpie never writes over or
// takes away. Desktop reads its profile at start-up only.
const desktopModelsMark = "claude-desktop.models"

// desktopDiscoveryMark says magpie wrote the profile's
// modelDiscoveryEnabled (gateway.DesktopDiscovers): magpie takes it away
// only then, and only while it is still true. One the user wrote, either
// way, stays as they wrote it.
const desktopDiscoveryMark = "claude-desktop.discovery"

// desktopModelsHash is a list's hash, the same however it is spaced.
func desktopModelsHash(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (draft *desktopDraft) syncModels(p desktopPaths) error {
	want, ok := gateway.DesktopInferenceModels()
	if !ok {
		return nil // the picks unknown: what is there stays
	}
	key := desktopModelsMark + p.key
	marks := strings.Fields(draft.saved[key])
	fields, err := draft.fields(p.prof)
	if err != nil || draft.body(p.prof) == nil {
		return err
	}
	raw, has := fields["inferenceModels"]
	if has && !slices.Contains(marks, desktopModelsHash(raw)) {
		return draft.syncDiscovery(p, fields, false)
	}
	discovers, _ := gateway.DesktopDiscovers()
	if err := draft.syncDiscovery(p, fields, want != nil && discovers); err != nil {
		return err
	}
	if want == nil {
		delete(draft.saved, key)
		if has {
			return draft.patch(p.prof, nil, "inferenceModels")
		}
		return nil
	}
	next, err := json.Marshal(want)
	if err != nil {
		return err
	}
	h := desktopModelsHash(next)
	draft.saved[key] = h
	if has && desktopModelsHash(raw) == h {
		return nil
	}
	return draft.patch(p.prof, []edit.KV{{Path: "inferenceModels", Value: json.RawMessage(next)}})
}

func (draft *desktopDraft) syncDiscovery(p desktopPaths, fields map[string]json.RawMessage, on bool) error {
	key := desktopDiscoveryMark + p.key
	ours := draft.saved[key] != ""
	raw, has := fields["modelDiscoveryEnabled"]
	if ours && string(raw) != "true" {
		delete(draft.saved, key)
		ours = false
	}
	switch {
	case on && !has:
		draft.saved[key] = "1"
		return draft.patch(p.prof, []edit.KV{{Path: "modelDiscoveryEnabled", Value: true}})
	case !on && ours:
		delete(draft.saved, key)
		return draft.patch(p.prof, nil, "modelDiscoveryEnabled")
	}
	return nil
}
