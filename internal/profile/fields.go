package profile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/yetone/magpie/internal/agent"
)

// Ordinary native fields retain agent.field. A dot in either component needs
// an explicit pair: otherwise two valid agent/field pairs can have one key.
const fieldPrefix = "@field:"

type fieldAddress struct{ agent, field string }

func fieldKey(id, field string) string {
	if !strings.Contains(id, ".") && !strings.Contains(field, ".") && !strings.HasPrefix(id, fieldPrefix) {
		return id + "." + field
	}
	raw, _ := json.Marshal([]string{id, field})
	return fieldPrefix + string(raw)
}

func readFieldKey(key string) (fieldAddress, error) {
	if raw, explicit := strings.CutPrefix(key, fieldPrefix); explicit {
		var pair []string
		if json.Unmarshal([]byte(raw), &pair) != nil || len(pair) != 2 || pair[0] == "" || pair[1] == "" {
			return fieldAddress{}, fmt.Errorf("invalid profile field address %q", key)
		}
		return fieldAddress{pair[0], pair[1]}, nil
	}
	if i := strings.LastIndex(key, "."); i >= 0 {
		return fieldAddress{key[:i], key[i+1:]}, nil
	}
	return fieldAddress{}, nil
}

// Legacy keys have no separator escaping. Resolve against declared fields,
// including dotted agent/WSL IDs, and refuse more than one possible owner.
// With no known field, retain the old split for displaying missing agents.
func resolveFieldKey(key string, agents map[string]*agent.Agent) (fieldAddress, error) {
	fallback, err := readFieldKey(key)
	if err != nil || strings.HasPrefix(key, fieldPrefix) {
		return fallback, err
	}
	var selected fieldAddress
	for id, a := range agents {
		field, matches := strings.CutPrefix(key, id+".")
		if !matches || a.Field(field) == nil {
			continue
		}
		if selected.agent != "" {
			return fieldAddress{}, fmt.Errorf("profile field %q is ambiguous; save a new profile with explicit field addresses", key)
		}
		selected = fieldAddress{id, field}
	}
	if selected.agent != "" {
		return selected, nil
	}
	return fallback, nil
}

type profileField struct {
	fieldAddress
	key, value string
	err        error
}

// Resolve once before applying any field, so ambiguity and duplicate aliases
// cannot produce partial writes or depend on map iteration order.
func resolveProfileFields(values map[string]string, agents map[string]*agent.Agent) []profileField {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []profileField
	seen := map[fieldAddress]int{}
	for _, key := range keys {
		address, err := resolveFieldKey(key, agents)
		item := profileField{fieldAddress: address, key: key, value: values[key], err: err}
		if err == nil && address.agent != "" {
			if previous, ok := seen[address]; ok {
				if out[previous].value == item.value {
					continue
				}
				item.err = fmt.Errorf("conflicting profile values for %q field %q", address.agent, address.field)
				out[previous].err = item.err
			} else {
				seen[address] = len(out)
			}
		}
		out = append(out, item)
	}
	return out
}

func profileFieldError(fields []profileField) error {
	for _, field := range fields {
		if field.err != nil {
			return field.err
		}
	}
	return nil
}

func agentsByID(all []*agent.Agent) map[string]*agent.Agent {
	byID := make(map[string]*agent.Agent, len(all))
	for _, a := range all {
		byID[a.ID] = a
	}
	return byID
}

// InUse receives detected agents. Only old keys with multiple dots need the
// rest of the declarations to rule out another owner: its absence must not
// reinterpret a legacy address. New explicit keys need no extra discovery.
func legacyFieldAgents(values map[string]string, detected []*agent.Agent) map[string]*agent.Agent {
	known := map[string]*agent.Agent{}
	for key := range values {
		if !strings.HasPrefix(key, fieldPrefix) && strings.Count(key, ".") > 1 {
			known = agentsByID(agent.All())
			break
		}
	}
	for _, a := range detected {
		known[a.ID] = a
	}
	return known
}
