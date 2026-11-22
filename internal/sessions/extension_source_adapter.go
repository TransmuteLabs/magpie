package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sessionSourceRecord struct {
	storage    *storageSourceRecord
	grant      sourceGrant
	request    sdk.SessionSourceReadRequest
	snapshot   *sessionInputSnapshot
	summary    *state
	stale      bool
	usageKnown bool
	parentID   string
	rootID     string
	subs       []string
}

type sessionSourceView struct {
	epoch    uint64
	records  []*sessionSourceRecord
	owners   map[string]bool
	agents   map[string]bool
	problems []string
}

var ErrSessionSourcePending = errors.New("session source preparation is already running")

func PrepareSessionSources(ctx context.Context) error {
	refreshSourceAuthority(ctx)
	select {
	case sourcePreparing <- struct{}{}:
		defer func() { <-sourcePreparing }()
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrSessionSourcePending
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	sourceCancel.Store(&cancel)
	defer func() { sourceCancel.CompareAndSwap(&cancel, nil); cancel() }()
	epoch := sourceEpoch.Load()
	claims, err := readHistoryClaims(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		owners := sourceBoundOwners(sourceView.Load())
		maps.Copy(owners, claims.owners)
		for _, agent := range nativeHistoryOwners() {
			owners[agent] = true
		}
		sourceView.Store(&sessionSourceView{epoch: epoch, owners: owners, problems: []string{err.Error()}})
		return err
	}
	bindings := claims.bindings
	view := &sessionSourceView{epoch: epoch, owners: claims.owners, agents: map[string]bool{}}
	if _, err := os.Stat(extension.CatalogPath()); errors.Is(err, fs.ErrNotExist) {
		sourceView.Store(view)
		return nil
	}
	manager := sourceManager()
	entries, err := manager.ListContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		view.problems = []string{err.Error()}
		sourceView.Store(view)
		return err
	}
	previous := sourceView.Load()
	var reuse sourceRecordReuse
	if previous != nil {
		reuse.records = previous.records
	}
	var problems []error
	owners := enabledSourceOwners(entries)
	for _, entry := range entries {
		for _, source := range entry.Manifest.SessionSources {
			if !nativeSourceOwner(source.Agent) {
				view.agents[source.Agent] = true
			}
		}
	}
	problems = append(problems, prepareStorageSources(ctx, manager, entries, view, owners, claims.consents)...)
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		for _, descriptor := range entry.Manifest.SessionSources {
			if !Reads(descriptor.Agent) || owners[descriptor.Agent].count != 1 {
				continue
			}
			selected, err := sessionMigrationSelection(ctx, manager, entry.Manifest.ID)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if !sameSessionModule(entry, selected.entry) {
				problems = append(problems, extension.ErrActionChanged)
				continue
			}
			if nativeSourceOwner(descriptor.Agent) && !slices.ContainsFunc(bindings, func(binding SessionSourceBinding) bool {
				return binding.Plugin == entry.Manifest.ID && binding.Source == descriptor.ID && binding.Agent == descriptor.Agent && binding.Identity == selected.identity
			}) {
				continue
			}
			for _, store := range descriptor.Stores {
				roots, err := sourceStoreRoots(descriptor, store)
				if err != nil {
					problems = append(problems, err)
					continue
				}
				for _, root := range roots {
					grant, err := makeSourceGrant(entry.Manifest.ID, selected.identity, descriptor, store, root)
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					if err != nil {
						problems = append(problems, err)
						continue
					}
					names, err := listSourceCandidates(ctx, grant)
					if err != nil {
						problems = append(problems, err)
						continue
					}
					for _, name := range names {
						if len(view.records) >= 10000 {
							problems = append(problems, errors.New("session source snapshot record budget exceeded"))
							break
						}
						record, err := prepareSourceRecord(ctx, manager, selected, grant, name, &reuse)
						if record != nil {
							view.records = append(view.records, record)
						}
						if err != nil {
							problems = append(problems, err)
						}
					}
				}
			}
		}
	}
	duplicates := map[string]int{}
	for _, record := range view.records {
		duplicates[record.grant.descriptor.Agent+":"+sourceRecordID(record)]++
		for _, id := range record.subs {
			duplicates[record.grant.descriptor.Agent+":"+id]++
		}
	}
	view.records = slices.DeleteFunc(view.records, func(record *sessionSourceRecord) bool {
		ambiguous := duplicates[record.grant.descriptor.Agent+":"+sourceRecordID(record)] != 1
		for _, id := range record.subs {
			ambiguous = ambiguous || duplicates[record.grant.descriptor.Agent+":"+id] != 1
		}
		if ambiguous {
			problems = append(problems, errors.New("ambiguous session source identity"))
		}
		return ambiguous
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if epoch != sourceEpoch.Load() {
		return extension.ErrActionChanged
	}
	checks, err := checkSourceRecordGrants(ctx, view.records)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if epoch != sourceEpoch.Load() {
		return extension.ErrActionChanged
	}
	if err == nil {
		for _, record := range view.records {
			if err = checks[record.grant.reference]; err != nil {
				break
			}
		}
	}
	if err != nil {
		sourceView.Store(&sessionSourceView{epoch: epoch, owners: view.owners, agents: view.agents})
		return err
	}
	for _, problem := range problems {
		view.problems = append(view.problems, problem.Error())
	}
	sourceView.Store(view)
	return errors.Join(problems...)
}

func sourceBoundOwners(view *sessionSourceView) map[string]bool {
	owners := map[string]bool{}
	if view != nil {
		for agent, owned := range view.owners {
			owners[agent] = owned
		}
	}
	return owners
}

type sourceRecordLocator struct {
	reference, enableGeneration, path string
}

type sourceRecordReuse struct {
	records   []*sessionSourceRecord
	byLocator map[sourceRecordLocator]*sessionSourceRecord
}

func (reuse *sourceRecordReuse) find(grant sourceGrant, name string) *sessionSourceRecord {
	key := sourceRecordLocator{grant.reference, grant.enableGeneration, name}
	if len(reuse.records) > 1 {
		if reuse.byLocator == nil {
			reuse.byLocator = make(map[sourceRecordLocator]*sessionSourceRecord, len(reuse.records))
			for _, record := range reuse.records {
				locator := sourceRecordLocator{record.grant.reference, record.grant.enableGeneration, record.request.Locator.Path}
				if _, duplicate := reuse.byLocator[locator]; duplicate {
					reuse.byLocator[locator] = nil
				} else {
					reuse.byLocator[locator] = record
				}
			}
		}
		record, found := reuse.byLocator[key]
		if !found {
			return nil
		}
		if record != nil {
			if os.SameFile(record.grant.info, grant.info) {
				return record
			}
			return nil
		}
	}
	for _, record := range reuse.records {
		if record.grant.reference == grant.reference && record.grant.enableGeneration == grant.enableGeneration &&
			record.request.Locator.Path == name && os.SameFile(record.grant.info, grant.info) {
			return record
		}
	}
	return nil
}

func prepareSourceRecord(ctx context.Context, manager *extension.Manager, selected sessionModuleSelection, grant sourceGrant, name string, reuse *sourceRecordReuse) (*sessionSourceRecord, error) {
	grant.revision = selected.entry.Revision
	grant.enableGeneration = selected.entry.EnableGeneration
	epoch := sourceEpoch.Load()
	ctx = withSessionSourceAdmission(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !Reads(grant.descriptor.Agent) {
			return ErrNotRead
		}
		if epoch != sourceEpoch.Load() {
			return extension.ErrActionChanged
		}
		return checkSourceGrant(ctx, grant)
	})
	old := reuse.find(grant, name)
	request, snapshot, err := captureSourceRequest(ctx, grant, name, "summary")
	if err == nil && old != nil && sameSourceSelection(request, old.request) && !old.stale && old.snapshot.check(ctx) == nil {
		return old, nil
	}
	if err == nil {
		var lease *extension.Lease
		lease, err = manager.AcquireOutsideCalls(ctx, selected.entry, selected.configVersion)
		if err == nil {
			var raw json.RawMessage
			snapshot, err = collectSourceDocuments(ctx, manager, lease, grant, &request, snapshot)
			if err == nil && old != nil && !old.stale && sameSourceSelection(request, old.request) && old.snapshot.check(ctx) == nil {
				lease.Close()
				return old, nil
			}
			if sourceBeforeRPC != nil && err == nil {
				err = sourceBeforeRPC(ctx)
			}
			if err == nil {
				err = checkSessionSourceAdmission(ctx)
			}
			if err == nil {
				raw, err = lease.Call(ctx, sdk.MethodSessionSourceRead,
					sdk.Scope{Session: request.Locator.SessionID, Operation: sdk.MethodSessionSourceRead}, request, nil)
			}
			lease.Close()
			if err == nil {
				var result sdk.SessionSourceReadResult
				result, err = sdk.DecodeSessionSourceResult(request, raw)
				if err == nil {
					err = snapshot.check(ctx)
					if err == nil {
						err = selected.check(ctx, manager)
						if err == nil && !Reads(grant.descriptor.Agent) {
							err = extension.ErrActionChanged
						}
					}
					if err == nil {
						summary := sourceSummaryState(grant, request, snapshot, result.Summary)
						if summary == nil {
							err = errors.New("session source counters exceed host integer range")
						} else {
							request.Documents = nil
							return &sessionSourceRecord{grant: grant, request: request, snapshot: snapshot,
								summary: summary, usageKnown: result.Summary.Usage != nil,
								parentID: result.Summary.ParentID, rootID: result.Summary.RootID,
								subs: sourceDescendantIDs(result.Summary.Subs)}, nil
						}
					}
				}
			}
		}
	}
	if old != nil && checkSourceGrant(ctx, grant) == nil {
		if old.stale {
			return old, err
		}
		stale := *old
		stale.stale = true
		stale.summary = old.summary.clone()
		return &stale, err
	}
	return nil, err
}

func sourceSummaryState(grant sourceGrant, request sdk.SessionSourceReadRequest, snapshot *sessionInputSnapshot, summary *sdk.SessionSourceSummary) *state {
	file := snapshot.files[0]
	return projectSourceSummary(grant.descriptor.Agent, request.Locator.SessionID, summary, file.info.Size(), file.info.ModTime().UnixNano())
}

func prepareSourcesBeforeRead() {
	prepareSourcesBeforeReadContext(context.Background())
}

func prepareSourcesBeforeReadContext(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = PrepareSessionSources(ctx)
}

func checkSourceRecordGrants(ctx context.Context, records []*sessionSourceRecord) (map[string]error, error) {
	if len(records) == 0 {
		return nil, ctx.Err()
	}
	checks := map[string]error{}
	for _, record := range records {
		reference := record.grant.reference
		if _, checked := checks[reference]; !checked {
			checks[reference] = checkSourceRecordGrant(ctx, record)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	}
	owners, err := currentSourceOwners(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		grant := record.grant
		if checks[grant.reference] == nil && !owners[grant.descriptor.Agent].matches(grant) {
			checks[grant.reference] = extension.ErrActionChanged
		}
	}
	return checks, ctx.Err()
}

func refreshSourceAuthority(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		view := sourceView.Load()
		if view == nil {
			return
		}
		claims, claimErr := readHistoryClaims(ctx)
		if claimErr != nil {
			if claims.owners == nil {
				claims.owners = map[string]bool{}
			}
			for agent := range view.owners {
				claims.owners[agent] = true
			}
			for _, agent := range nativeHistoryOwners() {
				claims.owners[agent] = true
			}
		}
		var checks map[string]error
		var checkErr error
		current := view.epoch == sourceEpoch.Load()
		if current {
			checks, checkErr = checkSourceRecordGrants(ctx, view.records)
		}
		records := slices.DeleteFunc(slices.Clone(view.records), func(record *sessionSourceRecord) bool {
			return !current || checkErr != nil || checks[record.grant.reference] != nil
		})
		if ctx.Err() != nil {
			return
		}
		if len(records) == len(view.records) && maps.Equal(view.owners, claims.owners) {
			return
		}
		next := *view
		next.records = records
		next.owners = claims.owners
		if sourceView.CompareAndSwap(view, &next) {
			return
		}
	}
}

func publishSourceRead[T any](read func() T) T {
	return publishSourceReadContext(context.Background(), read)
}

func publishSourceReadContext[T any](ctx context.Context, read func() T) T {
	prepareSourcesBeforeReadContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
		snapshot, err := inspectHistoryPublication(ctx)
		if err != nil {
			break
		}
		result := read()
		if sourceBeforePublish != nil {
			sourceBeforePublish()
		}
		refreshSourceAuthority(ctx)
		refreshSourceDescendantSnapshots(ctx)
		if snapshot.current(ctx) {
			return result
		}
	}
	var empty T
	return empty
}

func extensionSourceFiles(native []file) []file {
	view := sourceView.Load()
	if view == nil {
		return native
	}
	out := make([]file, 0, len(native)+len(view.records))
	for _, item := range native {
		if !view.owners[item.agent] {
			out = append(out, item)
		}
	}
	if view.epoch != sourceEpoch.Load() {
		return out
	}
	seen := map[string]bool{}
	for _, item := range out {
		seen[item.path] = true
	}
	for _, record := range view.records {
		agent := record.grant.descriptor.Agent
		if !Reads(agent) {
			continue
		}
		path := sourceRecordPath(record)
		if seen[path] {
			continue
		}
		seen[path] = true
		item := file{agent: agent, key: agent + ":" + sourceRecordID(record), path: path,
			size: record.summary.Size, mod: modTime(record.summary.Mod), main: true}
		item.w().source = record
		out = append(out, item)
	}
	return out
}

func extensionSourceAgent(agent string) bool {
	view := sourceView.Load()
	if view == nil {
		return false
	}
	if view.owners[agent] {
		return true
	}
	if view.agents[agent] {
		return true
	}
	if view.epoch != sourceEpoch.Load() {
		return false
	}
	for _, record := range view.records {
		if record.grant.descriptor.Agent == agent {
			return true
		}
	}
	return false
}

func SessionSourceErrors() []string {
	view := sourceView.Load()
	if view == nil {
		return nil
	}
	return slices.Clone(view.problems)
}
