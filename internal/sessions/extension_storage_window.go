package sessions

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/settings"
)

type WindowStorageOwner struct {
	Module, PackageRevision, ConfigVersion, PageRef, Component string
}

type WindowStorageRoot struct {
	Slot       string `json:"slot"`
	RootRef    string `json:"rootRef,omitempty"`
	ConsentRef string `json:"consentRef,omitempty"`
	Available  bool   `json:"available"`
	Consented  bool   `json:"consented"`
}

type WindowStorageInspection struct {
	Module      string              `json:"module"`
	Source      string              `json:"source"`
	Agent       string              `json:"agent"`
	ViewRef     string              `json:"viewRef"`
	ReadAllowed bool                `json:"readAllowed"`
	OwnOff      bool                `json:"ownOff"`
	Roots       []WindowStorageRoot `json:"roots"`
	Replaces    []string            `json:"replaces"`
}

type WindowStorageAccess struct {
	State      string                   `json:"state"`
	Inspection *WindowStorageInspection `json:"inspection"`
	Error      string                   `json:"error,omitempty"`
}

type windowStorageSnapshot struct {
	owner      WindowStorageOwner
	inspection WindowStorageInspection
	settings   settings.SessionReadsSnapshot
	consent    string
	rows       []StorageSourceConsent
	roots      map[string]storageSourceRecord
	epoch      uint64
}

var windowStorageSecret = func() []byte {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	return secret
}()

var windowStorageBeforeReadGate func()

func windowStorageRef(kind string, values ...any) string {
	body, _ := json.Marshal(values)
	hash := hmac.New(sha256.New, windowStorageSecret)
	hash.Write([]byte(kind + "\x00"))
	hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}

func storageConsentRevision() (string, error) {
	name := storageConsentsPath()
	root, err := os.OpenRoot(filepath.Dir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return windowStorageRef("consent-absent", name), nil
	}
	if err != nil {
		return "", err
	}
	defer root.Close()
	input, _, err := readSessionInput(context.Background(), root, filepath.Base(name), 64<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return windowStorageRef("consent-absent", name), nil
	}
	if err != nil {
		return "", err
	}
	native := reflect.Indirect(reflect.ValueOf(input.info.Sys()))
	identity := ""
	if native.IsValid() && native.Kind() == reflect.Struct {
		for _, field := range []string{"Dev", "Ino", "Ctim", "Ctimespec"} {
			if value := native.FieldByName(field); value.IsValid() && value.CanInterface() {
				identity += fmt.Sprintf("%s=%v;", field, value.Interface())
			}
		}
	}
	return windowStorageRef("consent", name, identity, input.hash, input.info.Size(), input.info.ModTime().UnixNano(), input.info.Mode()), nil
}

func inspectWindowStorage(ctx context.Context, owner WindowStorageOwner, source string) (*windowStorageSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner.PageRef == "" || owner.Component == "" || owner.PackageRevision == "" || owner.ConfigVersion == "" {
		return nil, extension.ErrActionChanged
	}
	manager := sourceManager()
	manifest, _, err := sessionMigrationIdentity(ctx, manager, owner.Module)
	if err != nil {
		return nil, err
	}
	descriptor, found := manifest.ResolveStorageSource(source)
	if !found {
		return nil, errors.New("window did not declare this storage source")
	}
	entries, err := manager.List()
	if err != nil {
		return nil, err
	}
	count := 0
	matched := false
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		if entry.Manifest.ID == owner.Module {
			matched = entry.Revision == owner.PackageRevision
		}
		for _, candidate := range entry.Manifest.StorageSources {
			if candidate.Agent == descriptor.Agent {
				count++
			}
		}
		for _, candidate := range entry.Manifest.SessionSources {
			if candidate.Agent == descriptor.Agent {
				count++
			}
		}
	}
	config, err := manager.ModuleConfig(ctx, owner.Module)
	if err != nil || !matched || config.Version != owner.ConfigVersion || count != 1 {
		return nil, extension.ErrActionChanged
	}
	reads, err := settings.InspectSessionReads()
	if err != nil {
		return nil, err
	}
	revision, err := storageConsentRevision()
	if err != nil {
		return nil, err
	}
	rows, err := readStorageConsents()
	if err != nil {
		return nil, err
	}
	snapshot := &windowStorageSnapshot{owner: owner, settings: reads, consent: revision, rows: rows,
		epoch: sourceEpoch.Load(), roots: map[string]storageSourceRecord{},
		inspection: WindowStorageInspection{Module: owner.Module, Source: source, Agent: descriptor.Agent,
			OwnOff: ownOff(), Roots: []WindowStorageRoot{}, Replaces: []string{}}}
	snapshot.inspection.ReadAllowed = !snapshot.inspection.OwnOff && reads.Reads.Reads(descriptor.Agent)
	for _, row := range rows {
		if row.Agent == descriptor.Agent && (row.Plugin != owner.Module || row.Source != source) {
			label := row.Plugin + " / " + row.Source
			if !slices.Contains(snapshot.inspection.Replaces, label) {
				snapshot.inspection.Replaces = append(snapshot.inspection.Replaces, label)
			}
		}
	}
	for _, store := range descriptor.Roots {
		record, err := selectedStorageRoot(ctx, manager, owner.Module, source, store.Slot)
		if errors.Is(err, fs.ErrNotExist) {
			snapshot.inspection.Roots = append(snapshot.inspection.Roots, WindowStorageRoot{Slot: store.Slot})
			continue
		}
		if err != nil {
			return nil, err
		}
		if record.entry.Revision != owner.PackageRevision {
			return nil, extension.ErrActionChanged
		}
		root := WindowStorageRoot{Slot: store.Slot, RootRef: record.rootRef, Available: true, Consented: storageConsented(rows, record)}
		snapshot.roots[root.RootRef] = record
		snapshot.inspection.Roots = append(snapshot.inspection.Roots, root)
	}
	again, err := storageConsentRevision()
	if err != nil || again != revision {
		return nil, extension.ErrActionChanged
	}
	snapshot.inspection.ViewRef = windowStorageRef("view", owner, source, reads.Revision, revision, snapshot.epoch,
		snapshot.inspection.OwnOff, snapshot.inspection.Roots)
	for index := range snapshot.inspection.Roots {
		root := &snapshot.inspection.Roots[index]
		if root.Consented {
			root.ConsentRef = windowStorageRef("release", snapshot.inspection.ViewRef, root.RootRef)
		}
	}
	return snapshot, nil
}

func InspectWindowStorage(ctx context.Context, owner WindowStorageOwner, source string) (WindowStorageInspection, error) {
	snapshot, err := inspectWindowStorage(ctx, owner, source)
	if err != nil {
		return WindowStorageInspection{}, err
	}
	return snapshot.inspection, nil
}

func selectWindowStorage(ctx context.Context, owner WindowStorageOwner, source, viewRef string) (*windowStorageSnapshot, error) {
	snapshot, err := inspectWindowStorage(ctx, owner, source)
	if err != nil {
		return nil, err
	}
	if viewRef == "" || snapshot.inspection.ViewRef != viewRef {
		return nil, extension.ErrActionChanged
	}
	return snapshot, nil
}

func (snapshot *windowStorageSnapshot) check(ctx context.Context) error {
	_, err := selectWindowStorage(ctx, snapshot.owner, snapshot.inspection.Source, snapshot.inspection.ViewRef)
	return err
}

func windowStorageRow(record storageSourceRecord) StorageSourceConsent {
	return StorageSourceConsent{Plugin: record.entry.Manifest.ID, Source: record.descriptor.ID, Agent: record.descriptor.Agent,
		RootSlot: record.store.Slot, RootRef: record.rootRef, Identity: record.identity}
}

func changeWindowStorageConsent(ctx context.Context, snapshot *windowStorageSnapshot, record storageSourceRecord, release bool) error {
	name := storageConsentsPath()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	unlock, held := filelock.Lock(name+".lock", filelock.Wait)
	if !held {
		return errors.New("storage source consent is busy")
	}
	defer unlock()
	if err := snapshot.check(ctx); err != nil {
		return err
	}
	rows, err := readStorageConsents()
	if err != nil {
		return err
	}
	selected := windowStorageRow(record)
	if release {
		if !slices.Contains(rows, selected) {
			return extension.ErrActionChanged
		}
		rows = slices.DeleteFunc(rows, func(row StorageSourceConsent) bool { return row == selected })
	} else {
		rows = slices.DeleteFunc(rows, func(row StorageSourceConsent) bool {
			return row.Agent == selected.Agent &&
				(row.Plugin != selected.Plugin || row.Source != selected.Source || row.RootSlot == selected.RootSlot)
		})
		rows = append(rows, selected)
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err := snapshot.check(ctx); err != nil {
		return err
	}
	if err := edit.WriteAtomic(name, body); err != nil {
		return err
	}
	invalidateSessionSources()
	return nil
}

func windowStorageAccessResult(ctx context.Context, owner WindowStorageOwner, source, state string, cause error) WindowStorageAccess {
	result := WindowStorageAccess{State: state}
	inspection, err := InspectWindowStorage(ctx, owner, source)
	if err == nil {
		result.Inspection = &inspection
	}
	if cause != nil {
		result.Error = "history access changed or could not be saved"
	}
	if err != nil {
		result.State = "unavailable"
		result.Error = "history authority is unavailable"
	}
	return result
}

func RequestWindowStorage(ctx context.Context, owner WindowStorageOwner, source, viewRef, rootRef string) WindowStorageAccess {
	snapshot, err := selectWindowStorage(ctx, owner, source, viewRef)
	if err != nil {
		return windowStorageAccessResult(ctx, owner, source, "changed", err)
	}
	record, found := snapshot.roots[rootRef]
	if !found || snapshot.inspection.OwnOff {
		return windowStorageAccessResult(ctx, owner, source, "denied", ErrNotRead)
	}
	if !storageConsented(snapshot.rows, record) {
		err = changeWindowStorageConsent(ctx, snapshot, record, false)
		if err != nil {
			return windowStorageAccessResult(ctx, owner, source, "changed", err)
		}
	}
	if windowStorageBeforeReadGate != nil {
		windowStorageBeforeReadGate()
	}
	bound, err := inspectWindowStorage(ctx, owner, source)
	if err != nil {
		return windowStorageAccessResult(ctx, owner, source, "changed", err)
	}
	current, found := bound.roots[rootRef]
	if !found || bound.inspection.OwnOff || !storageConsented(bound.rows, current) ||
		bound.settings.Revision != snapshot.settings.Revision {
		return windowStorageAccessResult(ctx, owner, source, "changed", extension.ErrActionChanged)
	}
	if !snapshot.inspection.ReadAllowed {
		if ownOff() {
			return windowStorageAccessResult(ctx, owner, source, "denied", ErrNotRead)
		}
		check := func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if sourceEpoch.Load() != bound.epoch {
				return extension.ErrActionChanged
			}
			revision, err := storageConsentRevision()
			if err != nil || revision != bound.consent {
				return extension.ErrActionChanged
			}
			current, err := selectedStorageRoot(ctx, sourceManager(), owner.Module, source, record.store.Slot)
			if err != nil || current.rootRef != rootRef || current.entry.Revision != owner.PackageRevision {
				return extension.ErrActionChanged
			}
			return nil
		}
		err = settings.CompareAndSwapSessionRead(snapshot.settings.Revision, record.descriptor.Agent, true, check)
		if err != nil {
			return windowStorageAccessResult(ctx, owner, source, "changed", err)
		}
		forgetSettled()
		Reset()
	}
	result := windowStorageAccessResult(ctx, owner, source, "ready", nil)
	if result.Inspection != nil {
		root := slices.IndexFunc(result.Inspection.Roots, func(root WindowStorageRoot) bool { return root.RootRef == rootRef && root.Consented })
		if !result.Inspection.ReadAllowed || root < 0 {
			result.State = "changed"
		}
	}
	return result
}

func ReleaseWindowStorage(ctx context.Context, owner WindowStorageOwner, source, viewRef, consentRef string) WindowStorageAccess {
	snapshot, err := selectWindowStorage(ctx, owner, source, viewRef)
	if err != nil {
		return windowStorageAccessResult(ctx, owner, source, "changed", err)
	}
	for _, root := range snapshot.inspection.Roots {
		if root.ConsentRef != "" && root.ConsentRef == consentRef {
			err := changeWindowStorageConsent(ctx, snapshot, snapshot.roots[root.RootRef], true)
			if err != nil {
				return windowStorageAccessResult(ctx, owner, source, "changed", err)
			}
			return windowStorageAccessResult(ctx, owner, source, "released", nil)
		}
	}
	return windowStorageAccessResult(ctx, owner, source, "changed", extension.ErrActionChanged)
}
