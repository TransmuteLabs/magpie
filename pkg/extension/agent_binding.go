package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strconv"
	"strings"
	"unicode"
)

type AgentBindingSnapshot struct {
	Target    string            `json:"target"`
	Revision  string            `json:"revision"`
	Connected bool              `json:"connected"`
	Values    map[string]string `json:"values"`
}

type AgentBindingChange struct {
	Revision  string            `json:"revision"`
	Connected bool              `json:"connected"`
	Values    map[string]string `json:"values"`
}

func (value *AgentBindingSnapshot) Clone() *AgentBindingSnapshot {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Values = maps.Clone(value.Values)
	return &clone
}

func (value *AgentBindingChange) Clone() *AgentBindingChange {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Values = maps.Clone(value.Values)
	return &clone
}

func (snapshot *AgentBindingSnapshot) UnmarshalJSON(raw []byte) error {
	var value struct {
		Target    string            `json:"target"`
		Revision  string            `json:"revision"`
		Connected *bool             `json:"connected"`
		Values    map[string]string `json:"values"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > MaxChunkBytes || decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Connected == nil {
		return errors.New("invalid agent binding snapshot")
	}
	snapshot.Target, snapshot.Revision, snapshot.Connected, snapshot.Values = value.Target, value.Revision, *value.Connected, value.Values
	return nil
}

func (change *AgentBindingChange) UnmarshalJSON(raw []byte) error {
	var value struct {
		Revision  string            `json:"revision"`
		Connected *bool             `json:"connected"`
		Values    map[string]string `json:"values"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > MaxChunkBytes || decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Connected == nil {
		return errors.New("invalid agent binding change")
	}
	change.Revision, change.Connected, change.Values = value.Revision, *value.Connected, value.Values
	return nil
}

func ValidateAgentBindingSnapshot(manifest Manifest, descriptor AgentDescriptor, value *AgentBindingSnapshot) error {
	if value == nil {
		return nil
	}
	target := descriptor.ID
	if descriptor.Profile != nil {
		target = descriptor.Profile.Target
	}
	if manifest.Contract.Version.Minor < 5 || !manifest.Contract.Has(AgentWiring) ||
		value.Target != target || !agentBindingRevision(value.Revision) || value.Connected && value.Revision == "0" ||
		value.Values == nil ||
		value.Connected && descriptor.Launch != nil && descriptor.Launch.Model != "" && value.Values["model"] == "" {
		return errors.New("agent binding requires a current SDK 1.5 host snapshot")
	}
	return agentBindingValues(descriptor, value.Values)
}

func agentBindingHandler(manifest Manifest, method string, handler Handler) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		var request struct {
			ID      string                `json:"id"`
			Action  string                `json:"action"`
			Binding *AgentBindingSnapshot `json:"binding"`
		}
		if len(call.Params) != 0 && json.Unmarshal(call.Params, &request) != nil {
			return nil, &Error{Code: "invalid_params", Message: "invalid agent request"}
		}
		var descriptor AgentDescriptor
		found := false
		for _, agent := range manifest.Agents {
			if agent.ID == request.ID {
				descriptor, found = agent, true
			}
		}
		if request.Binding != nil {
			if !found || ValidateAgentBindingSnapshot(manifest, descriptor, request.Binding) != nil {
				return nil, &Error{Code: "invalid_params", Message: "invalid host agent binding snapshot"}
			}
		}
		result, err := handler(ctx, call, stream)
		if err != nil || method != MethodAgentPlan {
			return result, err
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		binding := fields["binding"]
		if request.Binding == nil && (len(binding) == 0 || bytes.Equal(binding, []byte("null"))) {
			return result, nil
		}
		var plan AgentPlan
		if !found || json.Unmarshal(raw, &plan) != nil || plan.Binding == nil ||
			ValidateAgentBindingChange(manifest, descriptor, request.Binding, request.Action, plan.Binding) != nil {
			return nil, &Error{Code: "invalid_params", Message: "invalid agent binding change"}
		}
		return json.RawMessage(raw), nil
	}
}

func ValidateAgentBindingChange(manifest Manifest, descriptor AgentDescriptor, before *AgentBindingSnapshot, action string, value *AgentBindingChange) error {
	if value == nil {
		return nil
	}
	if before == nil || ValidateAgentBindingSnapshot(manifest, descriptor, before) != nil ||
		value.Revision != before.Revision || action == "connect" && !value.Connected ||
		action == "disconnect" && value.Connected || action == "sync" && value.Connected != before.Connected ||
		action != "connect" && action != "disconnect" && action != "sync" && action != "set" ||
		value.Connected && descriptor.Launch != nil && descriptor.Launch.Model != "" && value.Values["model"] == "" {
		return errors.New("agent binding change does not match the host snapshot and action")
	}
	return agentBindingValues(descriptor, value.Values)
}

func agentBindingRevision(value string) bool {
	revision, err := strconv.ParseUint(value, 10, 64)
	return err == nil && strconv.FormatUint(revision, 10) == value
}

func agentBindingValues(descriptor AgentDescriptor, values map[string]string) error {
	if values == nil {
		return errors.New("agent binding values must be an object")
	}
	fields := map[string]bool{}
	for _, field := range descriptor.Fields {
		fields[field.Key] = true
	}
	for key, value := range values {
		if !fields[key] || len(value) > 8192 || strings.ContainsFunc(value, unicode.IsControl) {
			return errors.New("agent binding contains an undeclared or invalid value")
		}
	}
	raw, err := json.Marshal(values)
	if err != nil || len(raw) > MaxChunkBytes-1024 {
		return errors.New("agent binding exceeds its byte budget")
	}
	return nil
}
