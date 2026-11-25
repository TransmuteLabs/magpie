package usage

import (
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/yetone/magpie/internal/settings"
)

// clientMap is the user's ClientAgents (#1958) as last read, and a key
// for the caches built on what it counts.
type clientMap struct {
	m   map[string]string
	key string
}

var clientAs atomic.Pointer[clientMap]

// readClientAgents reads the user's ClientAgents again, for a pass over
// the ledger (readLogSnapshot) and the caches' keys: each record of the
// pass is then counted by what was read here.
func readClientAgents() *clientMap {
	m := settings.ClientAgentsNow()
	if cur := clientAs.Load(); cur != nil && maps.Equal(cur.m, m) {
		return cur
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(m)) {
		b.WriteString(k + "=" + m[k] + ";")
	}
	c := &clientMap{m: m, key: b.String()}
	clientAs.Store(c)
	return c
}

// clientAgentsKey is what the caches of counted records key on.
func clientAgentsKey() string { return readClientAgents().key }

// ReportedAs is the agent the Usage and Sessions pages count a client's
// calls under: AgentOf's, unless the user counts that client as an agent
// of theirs (settings.ClientAgents). Only for what is reported: the
// gateway's own choices (routing, keys, caps) go by AgentOf, the client's
// own name.
func ReportedAs(ua string) string {
	a, _ := reportedAs(ua)
	return a
}

// reportedAs is ReportedAs, and the client's own name when the user's
// counting changed it ("" when it didn't).
func reportedAs(ua string) (agent, client string) {
	id := AgentOf(ua)
	return clientAs.Load().report(id)
}

func (clients *clientMap) report(id string) (agent, client string) {
	if clients == nil || len(clients.m) == 0 {
		return id, ""
	}
	if to := clients.m[strings.ToLower(id)]; to != "" && to != id {
		return to, id
	}
	return id, ""
}

type sessionAgents struct {
	known   []Known
	clients *clientMap
}

var lastSessionAgents atomic.Pointer[sessionAgents]

func sameKnownAgents(cached, own, more []Known) bool {
	if len(cached) != len(own)+len(more) {
		return false
	}
	for index, item := range own {
		other := cached[index]
		if other.ID != item.ID || !slices.Equal(other.Names, item.Names) || !slices.Equal(other.UA, item.UA) {
			return false
		}
	}
	for index, item := range more {
		other := cached[len(own)+index]
		if other.ID != item.ID || !slices.Equal(other.Names, item.Names) || !slices.Equal(other.UA, item.UA) {
			return false
		}
	}
	return true
}

func readSessionAgents() *sessionAgents {
	clients, own, more := readClientAgents(), builtinAgents(), pluginAgents()
	if current := lastSessionAgents.Load(); current != nil && current.clients == clients && sameKnownAgents(current.known, own, more) {
		return current
	}
	known := make([]Known, 0, len(own)+len(more))
	for _, group := range [2][]Known{own, more} {
		for _, item := range group {
			item.Names = slices.Clone(item.Names)
			item.UA = slices.Clone(item.UA)
			known = append(known, item)
		}
	}
	next := &sessionAgents{known: known, clients: clients}
	lastSessionAgents.Store(next)
	return next
}

func (agents *sessionAgents) reportedAs(ua string) string {
	id, _ := agents.clients.report(agentOf(ua, agents.known))
	return id
}

// agentResolver snapshots known agents and ClientAgents once for a pass over
// usage rows, caching raw AgentOf lookups by User-Agent string.
type agentResolver struct {
	own     []Known
	more    []Known
	clients *clientMap
	raw     map[string]string
}

func newAgentResolver() agentResolver {
	return agentResolver{
		own:     builtinAgents(),
		more:    pluginAgents(),
		clients: clientAs.Load(),
	}
}

func (resolver *agentResolver) agentOf(ua string) string {
	if id, ok := resolver.raw[ua]; ok {
		return id
	}
	id := agentOfSplit(ua, resolver.own, resolver.more)
	if resolver.raw == nil {
		resolver.raw = map[string]string{}
	}
	resolver.raw[ua] = id
	return id
}

func (resolver *agentResolver) reportedAs(ua string) string {
	id, _ := resolver.clients.report(resolver.agentOf(ua))
	return id
}
