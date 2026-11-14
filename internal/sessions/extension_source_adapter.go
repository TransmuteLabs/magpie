package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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
	bindings, err := readSourceBindings()
	if err != nil {
		owners := sourceBoundOwners(sourceView.Load())
		for _, agent := range []string{"claude", "codex", "omp", "pi", "raft", "cindy", "opencode", "zcode", "dsh", "cline",
			"qoder", "qoder-cn", "grok", "workbuddy", "workbuddy-ai", "codebuddy", "droid", "cursor", "hermes", "alma", "kimi", "agy", "antigravity"} {
			owners[agent] = true
		}
		sourceView.Store(&sessionSourceView{epoch: epoch, owners: owners, problems: []string{err.Error()}})
		return err
	}
	view := &sessionSourceView{epoch: epoch, owners: map[string]bool{}, agents: map[string]bool{}}
	for _, binding := range bindings {
		view.owners[binding.Agent] = true
	}
	if _, err := os.Stat(extension.CatalogPath()); errors.Is(err, fs.ErrNotExist) {
		sourceView.Store(view)
		return nil
	}
	manager := sourceManager()
	entries, err := manager.List()
	if err != nil {
		view.problems = []string{err.Error()}
		sourceView.Store(view)
		return err
	}
	previous := sourceView.Load()
	var problems []error
	counts := map[string]int{}
	for _, entry := range entries {
		if !entry.Disabled {
			for _, source := range entry.Manifest.StorageSources {
				counts[source.Agent]++
			}
		}
		for _, source := range entry.Manifest.SessionSources {
			if !nativeSourceOwner(source.Agent) {
				view.agents[source.Agent] = true
			}
		}
		if !entry.Disabled {
			for _, source := range entry.Manifest.SessionSources {
				counts[source.Agent]++
			}
		}
	}
	problems = append(problems, prepareStorageSources(ctx, manager, entries, view, counts)...)
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		for _, descriptor := range entry.Manifest.SessionSources {
			if !Reads(descriptor.Agent) || counts[descriptor.Agent] != 1 {
				continue
			}
			_, identity, err := sessionMigrationIdentity(ctx, manager, entry.Manifest.ID)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if nativeSourceOwner(descriptor.Agent) && !slices.ContainsFunc(bindings, func(binding SessionSourceBinding) bool {
				return binding.Plugin == entry.Manifest.ID && binding.Source == descriptor.ID && binding.Agent == descriptor.Agent && binding.Identity == identity
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
					grant, err := makeSourceGrant(entry.Manifest.ID, identity, descriptor, store, root)
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
						record, err := prepareSourceRecord(ctx, manager, entry, grant, name, previous)
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
	for _, record := range view.records {
		if err := checkSourceRecordGrant(ctx, record); err != nil {
			sourceView.Store(&sessionSourceView{epoch: epoch, owners: view.owners, agents: view.agents})
			return err
		}
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

func prepareSourceRecord(ctx context.Context, manager *extension.Manager, entry extension.Installed, grant sourceGrant, name string, previous *sessionSourceView) (*sessionSourceRecord, error) {
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
	var old *sessionSourceRecord
	if previous != nil {
		for _, record := range previous.records {
			if record.grant.reference == grant.reference && record.request.Locator.Path == name &&
				os.SameFile(record.grant.info, grant.info) {
				old = record
				break
			}
		}
	}
	request, snapshot, err := captureSourceRequest(ctx, grant, name, "summary")
	if err == nil && old != nil && sameSourceSelection(request, old.request) && !old.stale && old.snapshot.check(ctx) == nil {
		return old, nil
	}
	if err == nil {
		config, configErr := manager.ModuleConfig(ctx, entry.Manifest.ID)
		if configErr != nil {
			err = configErr
		} else {
			var lease *extension.Lease
			lease, err = manager.AcquireRevision(ctx, entry.Manifest.ID, entry.Revision, config.Version)
			if err == nil {
				var raw json.RawMessage
				snapshot, err = collectSourceDocuments(ctx, manager, lease, grant, &request, snapshot)
				if err == nil && old != nil && !old.stale && sameSourceSelection(request, old.request) && old.snapshot.check(ctx) == nil {
					lease.Close()
					return old, nil
				}
				if sourceBeforeRPC != nil {
					if err == nil {
						err = sourceBeforeRPC(ctx)
					}
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
							_, identity, identityErr := sessionMigrationIdentity(ctx, manager, entry.Manifest.ID)
							if identityErr != nil || identity != grant.identity || !Reads(grant.descriptor.Agent) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = PrepareSessionSources(ctx)
}

func refreshSourceAuthority(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		view := sourceView.Load()
		if view == nil || len(view.records) == 0 {
			return
		}
		allowed := map[string]bool{}
		for _, record := range view.records {
			if _, checked := allowed[record.grant.reference]; !checked {
				allowed[record.grant.reference] = view.epoch == sourceEpoch.Load() && checkSourceRecordGrant(ctx, record) == nil
			}
		}
		records := slices.DeleteFunc(slices.Clone(view.records), func(record *sessionSourceRecord) bool {
			return !allowed[record.grant.reference]
		})
		if ctx.Err() != nil {
			return
		}
		if len(records) == len(view.records) {
			return
		}
		next := *view
		next.records = records
		if sourceView.CompareAndSwap(view, &next) {
			return
		}
	}
}

func publishSourceRead[T any](read func() T) T {
	prepareSourcesBeforeRead()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
		view, epoch := sourceView.Load(), sourceEpoch.Load()
		result := read()
		if sourceBeforePublish != nil {
			sourceBeforePublish()
		}
		refreshSourceAuthority(ctx)
		refreshSourceDescendantSnapshots(ctx)
		if view == sourceView.Load() && epoch == sourceEpoch.Load() && ctx.Err() == nil {
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
