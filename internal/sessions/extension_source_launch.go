package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var sourceResumeProgram = sync.OnceValue(func() string {
	program, err := os.Executable()
	if err != nil {
		return ""
	}
	return program
})

type ExtensionSourceResume struct {
	Plugin string
	Agent  string
	Cwd    string
	Args   []string
	record *sessionSourceRecord
	token  string
}

func sourceResumeToken(record *sessionSourceRecord) string {
	if record == nil || record.summary == nil || record.grant.identity == "" || record.grant.reference == "" {
		return ""
	}
	selection := struct {
		Owner            []string
		Locator          sdk.SessionSourceLocator
		Snapshot         string
		Recipe           *sdk.SessionResumeRecipe
		Storage          *sdk.StorageSessionLocator
		Family           string
		EnableGeneration string `json:",omitempty"`
	}{
		Owner: []string{record.grant.plugin, record.grant.identity, record.grant.reference,
			record.grant.descriptor.ID, record.grant.descriptor.Agent, record.grant.descriptor.Resume,
			sourceRecordID(record), record.summary.Cwd},
		Locator: record.request.Locator, Snapshot: record.request.Snapshot, Recipe: record.grant.descriptor.AgentResume,
		EnableGeneration: record.grant.enableGeneration,
	}
	if record.storage != nil {
		selection.Storage = &record.storage.locator
		selection.Family = record.storage.family
	}
	raw, err := json.Marshal(selection)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func sourceAgentResume(record *sessionSourceRecord) string {
	recipe := record.grant.descriptor.AgentResume
	if recipe == nil || record.summary == nil {
		return ""
	}
	if _, err := recipe.Arguments(sourceRecordID(record)); err != nil {
		return ""
	}
	return sourceResumeLine(sourceResumeProgram(), record.grant.descriptor.Agent,
		sourceRecordID(record), sourceResumeToken(record), runtime.GOOS == "windows")
}

func sourceResumeLine(program, agent, id, token string, windows bool) string {
	if program == "" || token == "" {
		return ""
	}
	quote := shellQuote
	head := ""
	if windows {
		quote = func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
		head = "& "
	}
	return head + quote(program) + " sessions resume " + quote(agent) + " " + quote(id) + " --expected " + quote(token)
}

func PlanExtensionSourceResume(ctx context.Context, owner, id, expected string) (*ExtensionSourceResume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(expected)
	if !sdk.ValidID(owner) || err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != expected {
		return nil, errors.New("session resume requires its displayed source identity")
	}
	if err := PrepareSessionSources(ctx); err != nil {
		return nil, err
	}
	view := sourceView.Load()
	if view == nil {
		return nil, extension.ErrActionChanged
	}
	for _, record := range view.records {
		if record.grant.descriptor.Agent != owner || sourceRecordID(record) != id {
			continue
		}
		if err := checkSelectedSource(ctx, record); err != nil {
			return nil, err
		}
		if expected != sourceResumeToken(record) || record.grant.descriptor.Resume != "agent" {
			return nil, extension.ErrActionChanged
		}
		selected, err := sessionMigrationSelection(ctx, sourceManager(), record.grant.plugin)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || !record.grant.matches(selected) {
			return nil, extension.ErrActionChanged
		}
		recipe := record.grant.descriptor.AgentResume
		if err := sdk.ValidateSessionResume(selected.entry.Manifest, owner, "agent", recipe); err != nil {
			return nil, err
		}
		args, err := recipe.Arguments(id)
		if err != nil {
			return nil, err
		}
		plan := &ExtensionSourceResume{Plugin: record.grant.plugin, Agent: recipe.Agent,
			Cwd: record.summary.Cwd, Args: args, record: record, token: expected}
		return plan, plan.Recheck(ctx)
	}
	return nil, extension.ErrActionChanged
}

func (plan *ExtensionSourceResume) Recheck(ctx context.Context) error {
	if plan == nil || plan.record == nil || plan.record.summary == nil {
		return extension.ErrActionChanged
	}
	if err := checkSelectedSource(ctx, plan.record); err != nil {
		return err
	}
	recipe := plan.record.grant.descriptor.AgentResume
	if recipe == nil || plan.token != sourceResumeToken(plan.record) || plan.Plugin != plan.record.grant.plugin ||
		plan.Agent != recipe.Agent || plan.Cwd != plan.record.summary.Cwd {
		return extension.ErrActionChanged
	}
	args, err := recipe.Arguments(sourceRecordID(plan.record))
	if err != nil || !slices.Equal(args, plan.Args) {
		return extension.ErrActionChanged
	}
	return ctx.Err()
}
