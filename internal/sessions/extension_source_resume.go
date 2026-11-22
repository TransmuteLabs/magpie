package sessions

import (
	"context"
	"os"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func checkSelectedSource(ctx context.Context, record *sessionSourceRecord) error {
	if record == nil || record.stale || !Reads(record.grant.descriptor.Agent) {
		return ErrNotRead
	}
	view := sourceView.Load()
	if view == nil || view.epoch != sourceEpoch.Load() {
		return extension.ErrActionChanged
	}
	if err := checkSourceRecordGrant(ctx, record); err != nil {
		return err
	}
	found := false
	for _, current := range view.records {
		if current == record {
			found = true
		}
	}
	if !found {
		return extension.ErrActionChanged
	}
	if record.storage == nil {
		if err := record.snapshot.check(ctx); err != nil {
			return err
		}
	}
	owners, err := currentSourceOwners(ctx)
	if err != nil {
		return err
	}
	if !owners[record.grant.descriptor.Agent].matches(record.grant) {
		return extension.ErrActionChanged
	}
	return ctx.Err()
}

func checkSourceGrant(ctx context.Context, grant sourceGrant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkHistoryRead(grant.descriptor.Agent); err != nil {
		return err
	}
	roots, err := sourceStoreRoots(grant.descriptor, grant.store)
	if err != nil {
		return err
	}
	selected := false
	for _, root := range roots {
		if root == grant.requested {
			current, err := makeSourceGrant(grant.plugin, grant.identity, grant.descriptor, grant.store, root)
			if err == nil && current.reference == grant.reference && os.SameFile(current.info, grant.info) &&
				sessionStoreModeEqual(current.info.Mode(), grant.info.Mode()) {
				selected = true
			}
		}
	}
	if !selected {
		return extension.ErrActionChanged
	}
	if nativeSourceOwner(grant.descriptor.Agent) {
		bindings, err := withHistoryClaimsRoot(ctx, readSourceBindingsAt)
		if err != nil {
			return err
		}
		bound := false
		for _, binding := range bindings {
			if binding.Plugin == grant.plugin && binding.Source == grant.descriptor.ID &&
				binding.Agent == grant.descriptor.Agent && binding.Identity == grant.identity {
				bound = true
			}
		}
		if !bound {
			return extension.ErrActionChanged
		}
	}
	current, err := sessionMigrationSelection(ctx, sourceManager(), grant.plugin)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || !grant.matches(current) {
		return extension.ErrActionChanged
	}
	return nil
}

func sourceResume(record *sessionSourceRecord) string {
	if !record.stale && record.grant.descriptor.Resume == "agent" {
		return sourceAgentResume(record)
	}
	if record.stale || record.grant.descriptor.Resume != "native" {
		return ""
	}
	return resumeFor("", record.grant.descriptor.Agent, sourceRecordID(record), record.summary.Cwd, sourceRecordPath(record))
}

func extensionSourceTranscript(ctx context.Context, record *sessionSourceRecord) (Transcript, error) {
	if record.storage != nil {
		return storageSourceTranscript(ctx, record)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ctx = withSessionSourceAdmission(ctx, func() error { return checkSelectedSource(ctx, record) })
	if err := checkSelectedSource(ctx, record); err != nil {
		return Transcript{}, err
	}
	if !record.grant.descriptor.Transcript {
		return Transcript{}, ErrNoTranscript
	}
	manager := sourceManager()
	selected, err := sessionMigrationSelection(ctx, manager, record.grant.plugin)
	if err != nil {
		return Transcript{}, err
	}
	if !record.grant.matches(selected) {
		return Transcript{}, extension.ErrActionChanged
	}
	lease, err := manager.AcquireOutsideCalls(ctx, selected.entry, selected.configVersion)
	if err != nil {
		return Transcript{}, err
	}
	defer lease.Close()
	request, snapshot, err := captureSourceRequest(ctx, record.grant, record.request.Locator.Path, "transcript")
	if err != nil {
		return Transcript{}, err
	}
	if _, err := collectSourceDocuments(ctx, manager, lease, record.grant, &request, snapshot); err != nil {
		return Transcript{}, err
	}
	if !sameSourceSelection(request, record.request) {
		return Transcript{}, extension.ErrActionChanged
	}
	if err := checkSessionSourceAdmission(ctx); err != nil {
		return Transcript{}, err
	}
	raw, err := lease.Call(ctx, sdk.MethodSessionSourceRead,
		sdk.Scope{Session: request.Locator.SessionID, Operation: sdk.MethodSessionSourceRead}, request, nil)
	if err != nil {
		return Transcript{}, err
	}
	result, err := sdk.DecodeSessionSourceResult(request, raw)
	if err != nil {
		return Transcript{}, err
	}
	if err := checkSelectedSource(ctx, record); err != nil {
		return Transcript{}, err
	}
	transcript := Transcript{Parts: []Part{}, Cut: result.Cut}
	for _, part := range result.Parts {
		if !transcript.add(false, part) {
			break
		}
	}
	return transcript, nil
}
