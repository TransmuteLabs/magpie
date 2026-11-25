package provider

import "slices"

// endpointOf is the gateway path a protocol is asked at.
var endpointOf = map[Protocol]string{Chat: "/v1/chat/completions", Responses: "/v1/responses", Anthropic: "/v1/messages"}

// NativeEndpoints are the paths a request for the model is relayed on to
// its provider as it is (/v1/models' and an agent plugin's connect's
// "native_endpoints"): the APIs the provider serves it on. A routing
// group's are GroupEndpoints, which need its members; none here. None
// either for a model every request to is translated anyway (a
// subscription served through its agent's own API).
func NativeEndpoints(e Entry) []string {
	if e.Group != "" {
		return nil
	}
	return endpointsOf(e.Provider.relayed(e.Model))
}

// AgentModelProtocols returns the preferred API and native gateway paths for
// an agent's model entry, using the same relay list for both.
func AgentModelProtocols(entry Entry, members []Member) (Protocol, []string) {
	if entry.Group != "" {
		return agentGroupProtocols(members, true, true)
	}
	provider, model := entry.Provider, entry.Model
	relayed := provider.relayed(model)
	if preferred := provider.nativeAmong(model, relayed); preferred != "" {
		return preferred, endpointsOf(relayed)
	}
	apis := provider.APIs(model)
	for _, protocol := range provider.Speaks() {
		if (protocol == Gemini || protocol == CodeAssist) && (apis == nil || slices.Contains(apis, protocol)) {
			return Gemini, nil
		}
	}
	return "", nil
}

// PreferredEndpoint is the path a request for the model is best asked on
// (/v1/models' magpie_prefers, for another magpie that has this one as its
// provider): its provider's Native API, or for a routing group the one
// GroupAPI wires an agent to, Responses where its members are best there.
// "" for a model translated anyway, or a group that stays on Chat.
// members is nil when the group's aren't known.
func PreferredEndpoint(e Entry, members func(id string) []Member) string {
	if e.Group == "" {
		return endpointOf[e.Provider.Native(e.Model)]
	}
	if members == nil {
		return ""
	}
	return endpointOf[GroupAPI(members(e.ID))]
}

// relayed are the APIs p takes a request for model on as it is, in the
// order p speaks them: none for a model every request to is translated
// anyway.
func (p Provider) relayed(model string) []Protocol {
	if p.Account != nil {
		switch p.Account.Agent {
		case "claude", "devin":
			return nil
		}
	}
	speaks := p.Speaks()
	apis := p.apisAmong(speaks, model)
	var out []Protocol
	for _, pr := range speaks {
		if slices.Contains(Protocols, pr) && (apis == nil || slices.Contains(apis, pr)) {
			out = append(out, pr)
		}
	}
	return out
}

func endpointsOf(prs []Protocol) []string {
	var out []string
	for _, pr := range prs {
		out = append(out, endpointOf[pr])
	}
	return out
}

// GroupEndpoints are the paths a request for a routing group is relayed
// on as it is to whichever of its members ms answers it: the APIs every
// member takes it on. None when a member translates every request anyway,
// or the members share no API.
func GroupEndpoints(ms []Member) []string {
	_, endpoints := agentGroupProtocols(ms, false, true)
	return endpoints
}

// GroupAPI is the API an agent that asks each model on one API is wired
// to ask a routing group on, "" for Chat. It is Responses when a member
// is served best there (a ChatGPT sign-in, GPT on OpenAI's API or
// Copilot's, a relay given as a Responses URL) and every other member
// takes a Responses request as it is too, or translates every request
// anyway (a Claude Code subscription): no member is then translated that
// Chat would have relayed, and the ones best on Responses are relayed
// instead of translated from Chat (zyh6036 on X: a group of
// subscriptions that all serve Responses was asked on Chat, as only a
// group whose members were all best there went on Responses, #1594). A
// group with a member Responses doesn't reach (a Chat-only key, a
// Messages-only Claude) stays on Chat, so that member keeps being
// relayed; a Messages-only group stays on Chat too, as the thinking a
// Claude member takes is known by its own model, not by the group's id.
// dsh, which has a route per API, puts such a group on Messages or
// Gemini's where every member is best there (agent's dshGroupAPIs).
func GroupAPI(ms []Member) Protocol {
	api, _ := agentGroupProtocols(ms, true, false)
	return api
}

func agentGroupProtocols(members []Member, withAPI, withEndpoints bool) (Protocol, []string) {
	var common []Protocol
	var preferResponses bool
	for index, member := range members {
		relayed := member.Provider.relayed(member.Model)
		if withEndpoints {
			if index == 0 {
				common = relayed
			} else {
				common = slices.DeleteFunc(common, func(protocol Protocol) bool {
					return !slices.Contains(relayed, protocol)
				})
			}
		}
		if !withAPI || len(relayed) == 0 {
			continue
		}
		if !slices.Contains(relayed, Responses) {
			if !withEndpoints {
				return "", nil
			}
			withAPI = false
			continue
		}
		preferResponses = preferResponses || member.Provider.nativeAmong(member.Model, relayed) == Responses
	}
	if withAPI && preferResponses {
		return Responses, endpointsOf(common)
	}
	return "", endpointsOf(common)
}
