package gateway

import (
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// mediaMembers is the routing group ref with its members ready now for
// an API that is no chat, kind's (catalog.Serves*, provider.KindVideo):
// embeddings, rerank, speech, transcriptions, videos and decisions.
//
// A group the user made: a provider's picks are of its chat models, and
// these models are in no chat list (a fetched list keeps them apart,
// catalog.Model's Serves, Draws and Films), so a member of a provider
// with picks was dropped as unpicked and a group of them was a 404
// (XYenon on Discord). Such a member is kept, as a drawing's image models
// are (FindDrawingGroup, jzw690 on X). A chat model the user unticked
// (#3911) is on its provider's list and still stays out.
//
// A group magpie finds (#5339): the providers that serve a model of kind
// under exactly the same id, asked for by the group's id
// ("group/auto-embedding:embed-1") or by the model's own on its API's
// route ("embed-1" on /v1/embeddings) when more than one provider serves
// it (provider.KindGroup). It is in no list of groups, so agents, keys
// and Routing never see it.
func mediaMembers(ref, kind string) (provider.Group, []provider.Member, bool) {
	if provider.IsGroupRef(ref) {
		if g, ms, ok := provider.KindGroup(kind, ref, servesKind(kind)); ok {
			return g, ms, true
		}
		serves := servesKind(kind)
		return provider.FindGroupAlso(ref, func(p provider.Provider, model string) bool {
			if p.IsExtension() {
				return serves(p, model)
			}
			return offChatList(p, model)
		})
	}
	if !bareRef(ref) {
		return provider.Group{}, nil, false
	}
	if g, ms, ok := provider.KindGroup(kind, ref, servesKind(kind)); ok && len(ms) > 1 {
		return g, ms, true
	}
	return provider.Group{}, nil, false
}

// bareRef is whether ref names no provider: "embed-1", or a vendor's id
// with a slash of its own ("BAAI/bge-m3") where no provider has the part
// before it as its id.
func bareRef(ref string) bool {
	pid, _, ok := strings.Cut(strings.TrimSpace(ref), "/")
	if !ok {
		return true
	}
	_, err := provider.Find(pid)
	return err != nil
}

// servesKind says whether a provider serves a model of kind's under that
// id: its fetched list says so (catalog.LiveServing; Videomakers for
// videos), its chat list has an id that says so (another magpie's list,
// which keeps them as it lists them), or, for embeddings and rerank, its
// preset names it (PlanRetrieval).
func servesKind(kind string) func(p provider.Provider, model string) bool {
	is := func(m catalog.Model, model string) bool { return m.ID == model }
	if kind == kindDecision {
		return servesDecision
	}
	if kind == provider.KindVideo {
		return func(p provider.Provider, model string) bool {
			return slices.ContainsFunc(Videomakers(p), func(m catalog.Model) bool { return is(m, model) })
		}
	}
	return func(p provider.Provider, model string) bool {
		if p.IsExtension() {
			return slices.ContainsFunc(extensionServing(p, kind), func(m catalog.Model) bool { return is(m, model) })
		}
		if slices.ContainsFunc(catalog.LiveServing(p.ID, kind), func(m catalog.Model) bool { return is(m, model) }) {
			return true
		}
		if catalog.ServesOf(model) == kind && slices.ContainsFunc(provider.KeptListing(p).Available, func(m catalog.Model) bool { return is(m, model) }) {
			return true
		}
		return slices.ContainsFunc(p.PlanRetrieval(kind), func(m catalog.Model) bool { return is(m, model) })
	}
}

func extensionServing(p provider.Provider, kind string) []catalog.Model {
	var operation sdk.ProviderOperation
	switch kind {
	case catalog.ServesEmbedding:
		operation = sdk.OperationEmbeddings
	case catalog.ServesRerank:
		operation = sdk.OperationRerank
	case catalog.ServesSpeech:
		operation = sdk.OperationAudioSpeech
	case catalog.ServesTranscription:
		operation = sdk.OperationAudioTranscribe
	default:
		return nil
	}
	if p.ServesNone() || !p.ExtensionSupportsOperation(operation) &&
		!(kind == catalog.ServesTranscription && p.ExtensionSupportsOperation(sdk.OperationAudioTranslate)) {
		return nil
	}
	var out []catalog.Model
	for _, model := range provider.KeptListing(p).Available {
		if model.Serves == kind {
			out = append(out, model)
		}
	}
	return out
}

// RetrievalModels are the embedding or rerank models, kind's, p serves
// on /v1/embeddings or /v1/rerank by their own ids: those its fetched
// list says are of that API (catalog.LiveServing), then those its preset
// names (PlanRetrieval), each id once. SDK providers use their validated
// account catalog and negotiated operations. They need no chat pick.
func RetrievalModels(p provider.Provider, kind string) []catalog.Model {
	if p.IsExtension() {
		return extensionServing(p, kind)
	}
	ms := slices.Clone(catalog.LiveServing(p.ID, kind))
	for _, m := range p.PlanRetrieval(kind) {
		if !slices.ContainsFunc(ms, func(x catalog.Model) bool { return x.ID == m.ID }) {
			ms = append(ms, m)
		}
	}
	return ms
}

// retrievalKind is the kind of model an operation of serveRetrieval's is
// asked of.
func retrievalKind(operation string) string {
	switch operation {
	case "rerank":
		return catalog.ServesRerank
	case "speech":
		return catalog.ServesSpeech
	case "transcriptions", "translations":
		return catalog.ServesTranscription
	case "decisions":
		return kindDecision
	}
	return catalog.ServesEmbedding
}

// offChatList is whether p's list of models to chat with lacks model:
// one made for another API, which no pick can name.
func offChatList(p provider.Provider, model string) bool {
	return !slices.ContainsFunc(provider.KeptListing(p).Available, func(m catalog.Model) bool { return m.ID == model })
}

// mediaGroupFirst is the model ref names for an API that is no chat,
// kind's: a group's first member (mediaMembers), or the one provider that
// serves a bare id of that kind (provider.KindGroup). Such a group or
// model is none agents are offered, so Resolve, which reads their
// catalog, doesn't take it.
func mediaGroupFirst(ref, kind string) (provider.Provider, string, bool) {
	if !provider.IsGroupRef(ref) && !bareRef(ref) {
		return provider.Provider{}, "", false
	}
	if _, ms, ok := mediaMembers(ref, kind); ok && len(ms) > 0 {
		return ms[0].Provider, ms[0].Model, true
	}
	if provider.IsGroupRef(ref) {
		return provider.Provider{}, "", false
	}
	if _, ms, ok := provider.KindGroup(kind, ref, servesKind(kind)); ok {
		return ms[0].Provider, ms[0].Model, true
	}
	return provider.Provider{}, "", false
}
