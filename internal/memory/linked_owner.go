package memory

import (
	"context"
	"slices"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

var linkedOwner = sdk.MemoryOwner{
	All: All, Get: Get, Rank: Rank, Search: Search,
	Save: Save, Edit: Edit, Remove: Remove, SetStatus: SetStatus,
	Suggest: Suggest, Promote: Promote, Decline: Decline, MadeSkill: MadeSkill,
	Tidy: Tidy, Untidy: Untidy, Prune: Prune,
	Publish: Publish, PublishDue: PublishDue, Digest: Digest,
	CollectBundle: Collect, MergeBundles: Combine, ApplyBundle: Put,
}

func Owner() sdk.MemoryOwner { return linkedOwner }

func AgentScopes(agent string, project *Project) []string {
	scopes := []string{Global}
	if scope := ScopeOf(project); scope != "" {
		scopes = append(scopes, scope)
	}
	if agent != "" {
		scopes = append(scopes, "agent:"+agent)
		if agent == "codex" {
			scopes = append(scopes, "agent:codex-mcp-client")
		}
	}
	return scopes
}

type agentWriter struct {
	source  Source
	project *Project
	scopes  []string
}

func AgentWriter(source Source, project *Project) sdk.MemoryAgentWriter {
	if project != nil {
		copy := *project
		copy.Roots = slices.Clone(project.Roots)
		project = &copy
	}
	return agentWriter{source: source, project: project, scopes: AgentScopes(source.Agent, project)}
}

func (writer agentWriter) allows(in Input) bool {
	return in.Scope == nil || slices.Contains(writer.scopes, strings.TrimSpace(*in.Scope))
}

func (writer agentWriter) Save(ctx context.Context, in Input) (*Memory, bool, error) {
	if !writer.allows(in) {
		return nil, false, ErrNotFound
	}
	return saveRecord(ctx, in, FromAgent, &writer.source, writer.project)
}

func (writer agentWriter) Edit(ctx context.Context, id string, in Input) (*Memory, error) {
	if !writer.allows(in) {
		return nil, ErrNotFound
	}
	return editRecord(ctx, id, in, false, writer.scopes, writer.project)
}

func (writer agentWriter) Forget(ctx context.Context, id string) (*Memory, error) {
	status := Archived
	return writer.Edit(ctx, id, Input{Status: &status})
}
