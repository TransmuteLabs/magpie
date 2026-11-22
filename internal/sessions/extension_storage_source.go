package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type StorageSourceConsent struct {
	Plugin   string `json:"plugin"`
	Source   string `json:"source"`
	Agent    string `json:"agent"`
	RootSlot string `json:"rootSlot"`
	RootRef  string `json:"rootRef"`
	Identity string `json:"identity"`
}

type StorageSourceRoot struct {
	Plugin    string `json:"plugin"`
	Source    string `json:"source"`
	Agent     string `json:"agent"`
	RootSlot  string `json:"rootSlot"`
	RootRef   string `json:"rootRef"`
	Revision  string `json:"revision"`
	Consented bool   `json:"consented"`
}

type storageSourceRecord struct {
	entry         extension.Installed
	descriptor    sdk.StorageSourceDescriptor
	store         sdk.SessionSourceStore
	root          string
	rootRef       string
	identity      string
	configVersion string
	family        string
	locator       sdk.StorageSessionLocator
	epoch         uint64
}

const storageConsentsFile = "storage-source-consents.json"

var storageConsentsLock = filelock.Mutex{Path: func() string { return storageConsentsPath() + ".lock" }}

func storageConsentsPath() string {
	return filepath.Join(appdir.Config(), storageConsentsFile)
}

func readStorageConsents() ([]StorageSourceConsent, error) {
	return withHistoryClaimsRoot(context.Background(), readStorageConsentsAt)
}

func readStorageConsentsAt(ctx context.Context, root *os.Root) ([]StorageSourceConsent, error) {
	_, body, err := readSessionInput(ctx, root, storageConsentsFile, 64<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var consents []StorageSourceConsent
	if err := decodeSourceJSON(body, &consents); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, consent := range consents {
		key := consent.Plugin + ":" + consent.Source + ":" + consent.RootSlot
		if !sdk.ValidID(consent.Plugin) || !sdk.ValidID(consent.Source) || !sdk.ValidID(consent.Agent) ||
			!sdk.ValidID(consent.RootSlot) || len(consent.Identity) != 64 || len(consent.RootRef) != 64 || seen[key] {
			return nil, errors.New("invalid storage source consent")
		}
		for _, digest := range []string{consent.Identity, consent.RootRef} {
			decoded, err := hex.DecodeString(digest)
			if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest {
				return nil, errors.New("invalid storage source consent identity")
			}
		}
		seen[key] = true
	}
	return consents, nil
}

func selectedStorageRoot(ctx context.Context, manager *extension.Manager, plugin, source, slot string) (storageSourceRecord, error) {
	selected, err := sessionMigrationSelection(ctx, manager, plugin)
	if err != nil {
		return storageSourceRecord{}, err
	}
	entry, identity := selected.entry, selected.identity
	descriptor, found := entry.Manifest.ResolveStorageSource(source)
	if !found {
		return storageSourceRecord{}, errors.New("unknown storage source")
	}
	index := slices.IndexFunc(descriptor.Roots, func(store sdk.SessionSourceStore) bool { return store.Slot == slot })
	if index < 0 {
		return storageSourceRecord{}, errors.New("unknown storage source root")
	}
	store := descriptor.Roots[index]
	roots, err := sourceStoreRoots(sdk.SessionSourceDescriptor{Agent: descriptor.Agent}, store)
	if err != nil || len(roots) != 1 {
		return storageSourceRecord{}, errors.New("storage source requires one selected finite root")
	}
	opened, err := os.OpenRoot(roots[0])
	if err != nil {
		return storageSourceRecord{}, err
	}
	info, err := opened.Stat(".")
	opened.Close()
	if err != nil {
		return storageSourceRecord{}, err
	}
	fileIdentity, err := storageRootFileIdentity(info)
	if err != nil {
		return storageSourceRecord{}, err
	}
	body, err := json.Marshal([]string{appdir.Config(), plugin, identity, source, slot, roots[0], fileIdentity})
	if err != nil {
		return storageSourceRecord{}, err
	}
	hash := sha256.Sum256(body)
	if err := ctx.Err(); err != nil {
		return storageSourceRecord{}, err
	}
	return storageSourceRecord{entry: entry, descriptor: descriptor, store: store,
		root: roots[0], rootRef: hex.EncodeToString(hash[:]), identity: identity,
		configVersion: selected.configVersion, epoch: sourceEpoch.Load()}, nil
}

func BindStorageSource(ctx context.Context, plugin, source, slot, rootRef, expectedRevision string) error {
	manager := sourceManager()
	record, err := selectedStorageRoot(ctx, manager, plugin, source, slot)
	if err != nil {
		return err
	}
	if record.entry.Revision != expectedRevision || expectedRevision == "" || record.rootRef != rootRef {
		return extension.ErrActionChanged
	}
	name := storageConsentsPath()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	if !storageConsentsLock.LockCheckedContext(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("storage source consent is busy")
	}
	defer storageConsentsLock.Unlock()
	consents, err := withHistoryClaimsRoot(ctx, readStorageConsentsAt)
	if err != nil {
		return err
	}
	consents = slices.DeleteFunc(consents, func(consent StorageSourceConsent) bool {
		return consent.Agent == record.descriptor.Agent &&
			(consent.Plugin != plugin || consent.Source != source || consent.RootSlot == slot)
	})
	consents = append(consents, StorageSourceConsent{Plugin: plugin, Source: source, Agent: record.descriptor.Agent,
		RootSlot: slot, RootRef: rootRef, Identity: record.identity})
	current, err := selectedStorageRoot(ctx, manager, plugin, source, slot)
	if err != nil || current.rootRef != record.rootRef || current.entry.Revision != expectedRevision {
		return extension.ErrActionChanged
	}
	body, err := json.Marshal(consents)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := edit.WriteAtomic(name, body); err != nil {
		return err
	}
	invalidateSessionSources()
	return nil
}

func bindSingleStorageSource(ctx context.Context, manifest sdk.Manifest, source, expectedRevision string) error {
	descriptor, found := manifest.ResolveStorageSource(source)
	if !found || len(descriptor.Roots) != 1 {
		return errors.New("storage consent requires an explicit root selection")
	}
	record, err := selectedStorageRoot(ctx, sourceManager(), manifest.ID, source, descriptor.Roots[0].Slot)
	if err != nil {
		return err
	}
	return BindStorageSource(ctx, manifest.ID, source, descriptor.Roots[0].Slot, record.rootRef, expectedRevision)
}

func UnbindStorageSource(ctx context.Context, agent, expectedIdentity string) error {
	name := storageConsentsPath()
	if !storageConsentsLock.LockCheckedContext(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("storage source consent is busy")
	}
	defer storageConsentsLock.Unlock()
	consents, err := withHistoryClaimsRoot(ctx, readStorageConsentsAt)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(consents, func(consent StorageSourceConsent) bool {
		return consent.Agent == agent && consent.Identity == expectedIdentity
	}) {
		return extension.ErrActionChanged
	}
	consents = slices.DeleteFunc(consents, func(consent StorageSourceConsent) bool {
		return consent.Agent == agent && consent.Identity == expectedIdentity
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(consents)
	if err == nil {
		err = edit.WriteAtomic(name, body)
	}
	if err == nil {
		invalidateSessionSources()
	}
	return err
}

func InspectStorageSources(ctx context.Context) ([]StorageSourceRoot, []StorageSourceConsent, error) {
	manager := sourceManager()
	entries, err := manager.ListContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	consents, err := readStorageConsents()
	if err != nil {
		return nil, nil, err
	}
	roots := []StorageSourceRoot{}
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		for _, source := range entry.Manifest.StorageSources {
			for _, store := range source.Roots {
				record, err := selectedStorageRoot(ctx, manager, entry.Manifest.ID, source.ID, store.Slot)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return nil, nil, err
				}
				roots = append(roots, StorageSourceRoot{Plugin: entry.Manifest.ID, Source: source.ID, Agent: source.Agent,
					RootSlot: store.Slot, RootRef: record.rootRef, Revision: entry.Revision,
					Consented: storageConsented(consents, record)})
			}
		}
	}
	if consents == nil {
		consents = []StorageSourceConsent{}
	}
	return roots, consents, nil
}

func storageConsented(consents []StorageSourceConsent, record storageSourceRecord) bool {
	return slices.ContainsFunc(consents, func(consent StorageSourceConsent) bool {
		return consent.Plugin == record.entry.Manifest.ID && consent.Source == record.descriptor.ID &&
			consent.Agent == record.descriptor.Agent && consent.RootSlot == record.store.Slot &&
			consent.RootRef == record.rootRef && consent.Identity == record.identity
	})
}

func (record *storageSourceRecord) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkSessionSourceAdmission(ctx); err != nil {
		return err
	}
	if record.epoch != sourceEpoch.Load() {
		return ErrNotRead
	}
	if err := checkHistoryRead(record.descriptor.Agent); err != nil {
		return err
	}
	current, err := selectedStorageRoot(ctx, sourceManager(), record.entry.Manifest.ID, record.descriptor.ID, record.store.Slot)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || current.rootRef != record.rootRef || current.identity != record.identity ||
		!sameSessionModule(record.entry, current.entry) {
		return extension.ErrActionChanged
	}
	consents, err := withHistoryClaimsRoot(ctx, readStorageConsentsAt)
	if err != nil {
		return err
	}
	if !storageConsented(consents, *record) {
		return extension.ErrActionChanged
	}
	return nil
}

func prepareStorageSources(ctx context.Context, manager *extension.Manager, entries []extension.Installed, view *sessionSourceView, owners map[string]sourceOwner, consents []StorageSourceConsent) []error {
	previous := sourceView.Load()
	previousRecords := map[sdk.StorageSessionLocator]*sessionSourceRecord{}
	if previous != nil && previous.epoch == view.epoch {
		for _, record := range previous.records {
			if record.storage != nil {
				previousRecords[record.storage.locator] = record
			}
		}
	}
	var problems []error
	for _, entry := range entries {
		for _, source := range entry.Manifest.StorageSources {
			if !nativeSourceOwner(source.Agent) {
				view.agents[source.Agent] = true
			}
			if entry.Disabled || !Reads(source.Agent) || owners[source.Agent].count != 1 {
				continue
			}
			for _, store := range source.Roots {
				record, err := selectedStorageRoot(ctx, manager, entry.Manifest.ID, source.ID, store.Slot)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					problems = append(problems, err)
					continue
				}
				if !sameSessionModule(entry, record.entry) {
					problems = append(problems, extension.ErrActionChanged)
					continue
				}
				if !storageConsented(consents, record) {
					continue
				}
				for _, family := range source.Families {
					selected := record
					selected.family = family.ID
					records, err := selected.enumerate(ctx, manager)
					if err != nil {
						problems = append(problems, err)
						continue
					}
					if len(view.records)+len(records) > sdk.DefaultStorageLimits().MaxEntries {
						problems = append(problems, errors.New("storage source snapshot record budget exceeded"))
						continue
					}
					for index, fresh := range records {
						if old := previousRecords[fresh.storage.locator]; old != nil && sameStorageSourceRecord(old, fresh) {
							records[index] = old
						}
					}
					view.records = append(view.records, records...)
				}
			}
		}
	}
	return problems
}

func sameStorageSourceRecord(previous, current *sessionSourceRecord) bool {
	if previous.storage == nil || current.storage == nil || previous.stale || current.stale {
		return false
	}
	before, after := previous.storage, current.storage
	return before.rootRef == after.rootRef && before.identity == after.identity && before.family == after.family &&
		before.epoch == after.epoch && sameSessionModule(before.entry, after.entry) &&
		before.locator == after.locator && previous.usageKnown == current.usageKnown &&
		previous.parentID == current.parentID && previous.rootID == current.rootID &&
		reflect.DeepEqual(previous.summary, current.summary)
}

func (record storageSourceRecord) enumerate(ctx context.Context, manager *extension.Manager) ([]*sessionSourceRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := manager.AcquireOutsideCalls(ctx, record.entry, record.configVersion)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	grant, err := extension.NewStorageGrant(ctx, lease, record.descriptor.ID, record.family, record.rootRef, record.root, record.check)
	if err != nil {
		return nil, err
	}
	defer grant.Close()
	ctx = grant.Context(ctx)
	request := sdk.StorageRequest{Lease: grant.RequestLease(), Limit: 256}
	records, seen, cursors := []*sessionSourceRecord{}, map[string]bool{}, map[string]bool{}
	for {
		if sourceBeforeRPC != nil {
			if err := sourceBeforeRPC(ctx); err != nil {
				return nil, err
			}
		}
		if err := record.check(ctx); err != nil {
			return nil, err
		}
		raw, err := lease.Call(ctx, sdk.MethodStorageList, sdk.Scope{Operation: sdk.MethodStorageList}, request, nil)
		if err != nil {
			return nil, err
		}
		var result sdk.StorageResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		if err := sdk.ValidateStorageResult(request, result); err != nil {
			return nil, err
		}
		if result.Cut {
			return nil, errors.New("storage source catalog exceeded its bounded snapshot")
		}
		for _, entry := range result.Entries {
			if seen[entry.Locator.SessionID] || len(records) >= request.Lease.Limits.MaxEntries {
				return nil, errors.New("duplicate or oversized storage catalog")
			}
			seen[entry.Locator.SessionID] = true
			selected := record
			selected.locator = entry.Locator
			summary := projectSourceSummary(record.descriptor.Agent, entry.Locator.SessionID, &entry.Summary, 0, 0)
			if summary == nil {
				return nil, errors.New("storage summary counter overflow")
			}
			if !summary.Last.IsZero() {
				summary.Mod = summary.Last.UnixNano()
			}
			records = append(records, &sessionSourceRecord{storage: &selected, summary: summary,
				grant: sourceGrant{plugin: record.entry.Manifest.ID, revision: record.entry.Revision, identity: record.identity,
					enableGeneration: record.entry.EnableGeneration, reference: record.rootRef,
					descriptor: sdk.SessionSourceDescriptor{ID: record.descriptor.ID, Agent: record.descriptor.Agent,
						Transcript: record.descriptor.Transcript, Resume: record.descriptor.Resume,
						AgentResume: record.descriptor.AgentResume.Clone()}},
				usageKnown: entry.Summary.Usage != nil, parentID: entry.Summary.ParentID, rootID: entry.Summary.RootID})
		}
		if result.NextCursor == "" {
			break
		}
		if cursors[result.NextCursor] {
			return nil, errors.New("storage catalog cursor did not advance")
		}
		cursors[result.NextCursor] = true
		request.Cursor = result.NextCursor
	}
	if err := grant.Recheck(ctx); err != nil {
		return nil, err
	}
	return records, nil
}

func sourceRecordID(record *sessionSourceRecord) string {
	if record.storage != nil {
		return record.storage.locator.SessionID
	}
	return record.request.Locator.SessionID
}

func sourceRecordPath(record *sessionSourceRecord) string {
	if record.storage != nil {
		return "storage:" + record.storage.rootRef + "/" + record.storage.locator.SessionID
	}
	return sourcePhysicalPath(record.grant, record.request.Locator.Path)
}

func checkSourceRecordGrant(ctx context.Context, record *sessionSourceRecord) error {
	if record.storage != nil {
		return record.storage.check(ctx)
	}
	return checkSourceGrant(ctx, record.grant)
}

func storageSourceRead(ctx context.Context, record *sessionSourceRecord) (sdk.StorageResult, error) {
	return storageSourceReadChecked(ctx, record, checkSelectedSource)
}

func storageSourceReadChecked(ctx context.Context, record *sessionSourceRecord, check func(context.Context, *sessionSourceRecord) error) (sdk.StorageResult, error) {
	selected := record.storage
	if selected == nil {
		return sdk.StorageResult{}, ErrNoTranscript
	}
	if err := check(ctx, record); err != nil {
		return sdk.StorageResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	manager := sourceManager()
	lease, err := manager.AcquireOutsideCalls(ctx, selected.entry, selected.configVersion)
	if err != nil {
		return sdk.StorageResult{}, err
	}
	defer lease.Close()
	grant, err := extension.NewStorageGrant(ctx, lease, selected.descriptor.ID, selected.family, selected.rootRef, selected.root, selected.check)
	if err != nil {
		return sdk.StorageResult{}, err
	}
	defer grant.Close()
	ctx = grant.Context(ctx)
	request := sdk.StorageRequest{Lease: grant.RequestLease(), Locator: &selected.locator, Mode: "transcript"}
	raw, err := lease.Call(ctx, sdk.MethodStorageRead, sdk.Scope{Session: selected.locator.SessionID, Operation: sdk.MethodStorageRead}, request, nil)
	if err != nil {
		return sdk.StorageResult{}, err
	}
	var result sdk.StorageResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return sdk.StorageResult{}, err
	}
	if err := sdk.ValidateStorageResult(request, result); err != nil {
		return sdk.StorageResult{}, err
	}
	if sourceBeforePublish != nil {
		sourceBeforePublish()
	}
	if err := check(ctx, record); err != nil {
		return sdk.StorageResult{}, err
	}
	if err := grant.Recheck(ctx); err != nil {
		return sdk.StorageResult{}, err
	}
	return result, nil
}

func storageSourceTranscript(ctx context.Context, record *sessionSourceRecord) (Transcript, error) {
	result, err := storageSourceRead(ctx, record)
	if err != nil {
		return Transcript{}, err
	}
	transcript := Transcript{Parts: []Part{}, Cut: result.Cut}
	for _, part := range result.Parts {
		if !transcript.add(false, Part{Role: part.Role, Kind: part.Kind, Name: part.Name, Text: part.Text, Model: part.Model, Cut: part.Cut}) {
			break
		}
	}
	if err := checkSelectedSource(ctx, record); err != nil {
		return Transcript{}, err
	}
	return transcript, nil
}

func StorageSourceMedia(ctx context.Context, agent, session, mediaID string) (sdk.StorageMedia, error) {
	selected, found := Find(agent, session)
	if !found || selected.source == nil || selected.source.storage == nil {
		return sdk.StorageMedia{}, ErrNoTranscript
	}
	result, err := storageSourceRead(ctx, selected.source)
	if err != nil {
		return sdk.StorageMedia{}, err
	}
	for _, media := range result.Media {
		if media.ID == mediaID {
			if err := checkSelectedSource(ctx, selected.source); err != nil {
				return sdk.StorageMedia{}, err
			}
			return media, nil
		}
	}
	return sdk.StorageMedia{}, fs.ErrNotExist
}

func projectSourceSummary(agent, id string, summary *sdk.SessionSourceSummary, size, mod int64) *state {
	value := &state{Agent: agent, ID: id, Title: summary.Title, Cwd: summary.Cwd, Size: size, Mod: mod, sourceOwned: true}
	value.Start, _ = time.Parse(time.RFC3339Nano, summary.Start)
	value.Last, _ = time.Parse(time.RFC3339Nano, summary.Last)
	var total uint64
	for _, point := range summary.Usage {
		for _, count := range []uint64{point.Input, point.Output, point.CacheRead, point.CacheWrite, point.CacheWrite1h} {
			if count > uint64(^uint(0)>>1)-total {
				return nil
			}
			total += count
		}
		at, _ := time.Parse(time.RFC3339Nano, point.At)
		value.use(at.In(time.Local).Format("2006-01-02"), point.Model, Tokens{Input: int(point.Input),
			Output: int(point.Output), CacheRead: int(point.CacheRead), CacheWrite: int(point.CacheWrite), CacheWrite1h: int(point.CacheWrite1h)})
	}
	return value
}
