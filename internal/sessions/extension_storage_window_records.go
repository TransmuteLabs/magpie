package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type WindowStorageSummary struct {
	SessionRef string `json:"sessionRef"`
	Title      string `json:"title"`
	Start      string `json:"start,omitempty"`
	Last       string `json:"last,omitempty"`
}

type WindowStoragePage struct {
	Entries    []WindowStorageSummary `json:"entries"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}

type WindowStorageMedia struct {
	MediaRef string `json:"mediaRef"`
	Name     string `json:"name"`
	MIME     string `json:"mime"`
	Size     int    `json:"size"`
}

type WindowStorageTranscript struct {
	SessionRef string               `json:"sessionRef"`
	ReadRef    string               `json:"readRef"`
	Parts      []Part               `json:"parts"`
	Cut        bool                 `json:"cut"`
	Media      []WindowStorageMedia `json:"media"`
}

func windowStorageMediaName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, name)
	if name == "." || name == "/" {
		return ""
	}
	if len(name) > 256 {
		name = name[:256]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	return name
}

type windowStorageCatalog struct {
	snapshot *windowStorageSnapshot
	rootRef  string
	ref      string
	created  time.Time
	records  []*sessionSourceRecord
}

var windowStorageCatalogs = struct {
	sync.Mutex
	entries map[string]*windowStorageCatalog
}{entries: map[string]*windowStorageCatalog{}}

func windowStorageCatalogKey(snapshot *windowStorageSnapshot, rootRef string) string {
	return windowStorageRef("catalog-key", snapshot.inspection.ViewRef, rootRef)
}

func storeWindowStorageCatalog(catalog *windowStorageCatalog) {
	windowStorageCatalogs.Lock()
	defer windowStorageCatalogs.Unlock()
	for key, current := range windowStorageCatalogs.entries {
		if time.Since(current.created) > 5*time.Minute {
			delete(windowStorageCatalogs.entries, key)
		}
	}
	key := windowStorageCatalogKey(catalog.snapshot, catalog.rootRef)
	if len(windowStorageCatalogs.entries) >= 16 && windowStorageCatalogs.entries[key] == nil {
		var oldestKey string
		var oldest time.Time
		for key, current := range windowStorageCatalogs.entries {
			if oldestKey == "" || current.created.Before(oldest) {
				oldestKey, oldest = key, current.created
			}
		}
		delete(windowStorageCatalogs.entries, oldestKey)
	}
	windowStorageCatalogs.entries[key] = catalog
}

func (catalog *windowStorageCatalog) sessionRef(index int) string {
	return windowStorageRef("session", catalog.ref, index, catalog.records[index].storage.locator)
}

func (catalog *windowStorageCatalog) check(ctx context.Context, record *sessionSourceRecord) error {
	if time.Since(catalog.created) > 5*time.Minute {
		return extension.ErrActionChanged
	}
	if err := catalog.snapshot.check(ctx); err != nil {
		return err
	}
	windowStorageCatalogs.Lock()
	current := windowStorageCatalogs.entries[windowStorageCatalogKey(catalog.snapshot, catalog.rootRef)]
	windowStorageCatalogs.Unlock()
	if current != catalog || !slices.Contains(catalog.records, record) || record.storage == nil {
		return extension.ErrActionChanged
	}
	return record.storage.check(ctx)
}

func ListWindowStorage(ctx context.Context, owner WindowStorageOwner, source, viewRef, rootRef, cursor string, limit int) (WindowStoragePage, error) {
	snapshot, err := selectWindowStorage(ctx, owner, source, viewRef)
	if err != nil {
		return WindowStoragePage{}, err
	}
	root, found := snapshot.roots[rootRef]
	if !found || !snapshot.inspection.ReadAllowed || !storageConsented(snapshot.rows, root) {
		return WindowStoragePage{}, ErrNotRead
	}
	if limit == 0 {
		limit = 64
	}
	if limit < 1 || limit > 256 || len(cursor) > 256 {
		return WindowStoragePage{}, errors.New("invalid history page bounds")
	}
	key := windowStorageCatalogKey(snapshot, rootRef)
	var catalog *windowStorageCatalog
	offset := 0
	if cursor != "" {
		windowStorageCatalogs.Lock()
		catalog = windowStorageCatalogs.entries[key]
		windowStorageCatalogs.Unlock()
		raw, signature, valid := strings.Cut(cursor, ".")
		offset, err = strconv.Atoi(raw)
		if !valid || err != nil || catalog == nil || offset < 0 || offset >= len(catalog.records) ||
			signature != windowStorageRef("cursor", catalog.ref, offset) {
			return WindowStoragePage{}, extension.ErrActionChanged
		}
	} else {
		select {
		case sourcePreparing <- struct{}{}:
			defer func() { <-sourcePreparing }()
		default:
			return WindowStoragePage{}, ErrSessionSourcePending
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ctx = withSessionSourceAdmission(ctx, func() error { return snapshot.check(ctx) })
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return WindowStoragePage{}, err
		}
		catalog = &windowStorageCatalog{snapshot: snapshot, rootRef: rootRef, ref: hex.EncodeToString(random),
			created: time.Now(), records: []*sessionSourceRecord{}}
		seen := map[string]bool{}
		for _, family := range root.descriptor.Families {
			selected := root
			selected.family = family.ID
			records, err := selected.enumerate(ctx, sourceManager())
			if err != nil {
				return WindowStoragePage{}, err
			}
			for _, record := range records {
				id := record.storage.locator.SessionID
				if seen[id] || len(catalog.records) >= sdk.DefaultStorageLimits().MaxEntries {
					return WindowStoragePage{}, errors.New("ambiguous or oversized selected history")
				}
				seen[id] = true
				catalog.records = append(catalog.records, record)
			}
		}
		if err := snapshot.check(ctx); err != nil {
			return WindowStoragePage{}, err
		}
		storeWindowStorageCatalog(catalog)
	}
	if time.Since(catalog.created) > 5*time.Minute {
		return WindowStoragePage{}, extension.ErrActionChanged
	}
	out := WindowStoragePage{Entries: []WindowStorageSummary{}}
	bytes := 0
	index := offset
	for index < len(catalog.records) && len(out.Entries) < limit {
		record := catalog.records[index]
		if err := catalog.check(ctx, record); err != nil {
			return WindowStoragePage{}, err
		}
		title := record.summary.Title
		if len(title) > 4096 {
			title = title[:4096]
			for !utf8.ValidString(title) {
				title = title[:len(title)-1]
			}
		}
		entry := WindowStorageSummary{SessionRef: catalog.sessionRef(index), Title: title}
		if !record.summary.Start.IsZero() {
			entry.Start = record.summary.Start.Format(time.RFC3339Nano)
		}
		if !record.summary.Last.IsZero() {
			entry.Last = record.summary.Last.Format(time.RFC3339Nano)
		}
		encoded, _ := json.Marshal(entry)
		if bytes+len(encoded) > 256<<10 && len(out.Entries) > 0 {
			break
		}
		bytes += len(encoded)
		out.Entries = append(out.Entries, entry)
		index++
	}
	if index < len(catalog.records) {
		out.NextCursor = fmt.Sprintf("%d.%s", index, windowStorageRef("cursor", catalog.ref, index))
	}
	if err := snapshot.check(ctx); err != nil {
		return WindowStoragePage{}, err
	}
	return out, nil
}

func selectWindowStorageRecord(ctx context.Context, owner WindowStorageOwner, source, viewRef, sessionRef string) (*windowStorageCatalog, *sessionSourceRecord, error) {
	if len(sessionRef) != 64 {
		return nil, nil, extension.ErrActionChanged
	}
	snapshot, err := selectWindowStorage(ctx, owner, source, viewRef)
	if err != nil {
		return nil, nil, err
	}
	windowStorageCatalogs.Lock()
	catalogs := []*windowStorageCatalog{}
	for _, catalog := range windowStorageCatalogs.entries {
		if catalog.snapshot.inspection.ViewRef == snapshot.inspection.ViewRef {
			catalogs = append(catalogs, catalog)
		}
	}
	windowStorageCatalogs.Unlock()
	for _, catalog := range catalogs {
		for index, record := range catalog.records {
			if catalog.sessionRef(index) == sessionRef {
				if err := catalog.check(ctx, record); err != nil {
					return nil, nil, err
				}
				return catalog, record, nil
			}
		}
	}
	return nil, nil, extension.ErrActionChanged
}

func projectWindowStorage(result sdk.StorageResult, viewRef, sessionRef string) WindowStorageTranscript {
	transcript := Transcript{Parts: []Part{}, Cut: result.Cut}
	bytes := 0
	for _, part := range result.Parts {
		text := part.Text
		remaining := transcriptMax - bytes
		if len(text) > remaining {
			text = text[:remaining]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			transcript.Cut = true
		}
		if !transcript.add(false, Part{Role: part.Role, Kind: part.Kind, Name: part.Name, Text: text, Model: part.Model, Cut: part.Cut + len(part.Text) - len(text)}) {
			break
		}
		bytes += len(text)
		if transcript.Cut && bytes >= transcriptMax {
			break
		}
	}
	out := WindowStorageTranscript{SessionRef: sessionRef, Parts: transcript.Parts, Cut: transcript.Cut, Media: []WindowStorageMedia{}}
	type mediaRevision struct {
		ID, Name, MIME, Digest string
		Size                   int
	}
	revisions := []mediaRevision{}
	for _, media := range result.Media {
		digest := sha256.Sum256(media.Bytes)
		revisions = append(revisions, mediaRevision{ID: media.ID, Name: windowStorageMediaName(media.Name), MIME: media.MIME,
			Size: len(media.Bytes), Digest: hex.EncodeToString(digest[:])})
	}
	out.ReadRef = windowStorageRef("read", viewRef, sessionRef, out.Parts, out.Cut, revisions)
	for _, media := range result.Media {
		out.Media = append(out.Media, WindowStorageMedia{MediaRef: windowStorageRef("media", out.ReadRef, media.ID),
			Name: windowStorageMediaName(media.Name), MIME: media.MIME, Size: len(media.Bytes)})
	}
	for index := range out.Parts {
		if out.Parts[index].Kind == "image" {
			id := out.Parts[index].Name
			out.Parts[index].Name = ""
			for mediaIndex, media := range result.Media {
				if media.ID == id {
					out.Parts[index].Name = out.Media[mediaIndex].MediaRef
				}
			}
		}
	}
	return out
}

func ReadWindowStorage(ctx context.Context, owner WindowStorageOwner, source, viewRef, sessionRef string) (WindowStorageTranscript, error) {
	catalog, record, err := selectWindowStorageRecord(ctx, owner, source, viewRef, sessionRef)
	if err != nil {
		return WindowStorageTranscript{}, err
	}
	ctx = withSessionSourceAdmission(ctx, func() error { return catalog.snapshot.check(ctx) })
	result, err := storageSourceReadChecked(ctx, record, catalog.check)
	if err != nil {
		return WindowStorageTranscript{}, err
	}
	out := projectWindowStorage(result, viewRef, sessionRef)
	if err := catalog.check(ctx, record); err != nil {
		return WindowStorageTranscript{}, err
	}
	return out, nil
}

func MediaWindowStorage(ctx context.Context, owner WindowStorageOwner, source, viewRef, sessionRef, readRef, mediaRef string) (sdk.StorageMedia, error) {
	catalog, record, err := selectWindowStorageRecord(ctx, owner, source, viewRef, sessionRef)
	if err != nil {
		return sdk.StorageMedia{}, err
	}
	ctx = withSessionSourceAdmission(ctx, func() error { return catalog.snapshot.check(ctx) })
	result, err := storageSourceReadChecked(ctx, record, catalog.check)
	if err != nil {
		return sdk.StorageMedia{}, err
	}
	projected := projectWindowStorage(result, viewRef, sessionRef)
	if readRef == "" || projected.ReadRef != readRef {
		return sdk.StorageMedia{}, extension.ErrActionChanged
	}
	for index, descriptor := range projected.Media {
		if descriptor.MediaRef == mediaRef {
			if err := catalog.check(ctx, record); err != nil {
				return sdk.StorageMedia{}, err
			}
			return result.Media[index], nil
		}
	}
	return sdk.StorageMedia{}, extension.ErrActionChanged
}
