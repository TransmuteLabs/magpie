package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const codexLineageMaxDepth = 32

type codexHistoryPosition struct {
	RolloutID string  `json:"thread_id"`
	Ordinal   *uint64 `json:"end_ordinal_exclusive"`
	Offset    *uint64 `json:"end_byte_offset"`
}

type codexLineageSegment struct {
	store        int
	path         string
	rolloutID    string
	header       map[string]json.RawMessage
	metadata     map[string]json.RawMessage
	base         *codexHistoryPosition
	end          *codexHistoryPosition
	decodedHash  [sha256.Size]byte
	prefixHash   [sha256.Size]byte
	rawHash      [sha256.Size]byte
	rawBytes     int
	decodedBytes int
	ordinalStart uint64
	ordinalEnd   uint64
	prefix       []byte
}

type codexLineageSnapshot struct {
	home         string
	stores       []codexLineageStore
	segments     []codexLineageSegment
	rawBytes     int
	decodedBytes int
	files        int
}

func collectCodexLineage(ctx context.Context, host extension.ActionRuntime, plugin string, source Session, request *sdk.SessionRequest, snapshot *sessionSourceSnapshot) error {
	if source.Agent != "codex" || len(request.Files) == 0 {
		return nil
	}
	selected, err := codexLineageHeader(request.Files[0].Body)
	if err != nil {
		return err
	}
	if selected.base == nil {
		return nil
	}
	if len(request.Files) != 1 {
		return errors.New("Codex lineage acquisition requires one selected journal")
	}
	manifest, err := host.Manifest(plugin)
	if err != nil {
		return err
	}
	if !manifest.Contract.Has(sdk.SessionRelatedInputs) {
		return errors.New("Codex sparse rollout requires session-related-inputs API 1.10")
	}
	if err := sdk.ValidateSessionRelatedContract(manifest); err != nil {
		return err
	}
	if err := checkCodexLineageAdmission(ctx); err != nil {
		return err
	}
	lineage, selectedStore, relative, err := openCodexLineageStores(ctx, source)
	if err != nil {
		return err
	}
	captured, err := lineage.capture(ctx, selectedStore, relative, nil)
	if err != nil {
		return err
	}
	original := lineage.stores[selectedStore].inputs.files[0]
	if !os.SameFile(snapshot.info, original.info) || snapshot.hash != original.hash ||
		captured.decodedHash != sha256.Sum256(request.Files[0].Body) {
		return extension.ErrActionChanged
	}
	var owner string
	if json.Unmarshal(captured.metadata["id"], &owner) != nil || owner != source.ID {
		return errors.New("Codex selected rollout belongs to another thread")
	}
	lineage.segments = append(lineage.segments, captured)
	seen := map[string]bool{captured.rolloutID: true}
	for base := captured.base; base != nil; {
		if len(lineage.segments) >= codexLineageMaxDepth {
			return errors.New("Codex lineage recursion budget exceeded")
		}
		if seen[base.RolloutID] {
			return errors.New("Codex lineage contains a rollout cycle")
		}
		seen[base.RolloutID] = true
		ancestor, err := lineage.ancestor(ctx, base)
		if err != nil {
			return err
		}
		lineage.segments = append(lineage.segments, ancestor)
		base = ancestor.base
	}
	slices.Reverse(lineage.segments)
	related := &sdk.SessionRelatedInputSet{Version: 1, Kind: sdk.CodexRolloutLineage}
	for index, segment := range lineage.segments {
		provenance := lineage.provenance(segment)
		if index == len(lineage.segments)-1 {
			related.Selected = provenance
		} else {
			related.Ancestors = append(related.Ancestors, sdk.SessionRelatedInput{Source: provenance, Body: segment.prefix})
		}
	}
	request.Related = related
	if err := sdk.ValidateSessionRelatedInputs(manifest, *request); err != nil {
		return err
	}
	if snapshot.inputs == nil {
		root, err := os.OpenRoot(filepath.Dir(source.Path))
		if err != nil {
			return err
		}
		info, err := root.Stat(".")
		root.Close()
		if err != nil {
			return err
		}
		snapshot.inputs = &sessionInputSnapshot{root: filepath.Dir(source.Path), info: info}
	}
	snapshot.inputs.lineage = lineage
	if err := snapshot.check(ctx); err != nil {
		return err
	}
	for index := range lineage.segments {
		lineage.segments[index].prefix = nil
	}
	return nil
}

func checkCodexLineageAdmission(ctx context.Context) error {
	if err := checkSessionSourceAdmission(ctx); err != nil {
		return err
	}
	if !Reads("codex") {
		return ErrNotRead
	}
	return nil
}

func (lineage *codexLineageSnapshot) ancestor(ctx context.Context, end *codexHistoryPosition) (codexLineageSegment, error) {
	var selected codexLineageSegment
	found := false
	for storeIndex := range lineage.stores {
		store := &lineage.stores[storeIndex]
		names := store.candidates[end.RolloutID]
		for _, name := range names {
			if strings.HasSuffix(name, zstSuffix) && slices.Contains(names, strings.TrimSuffix(name, zstSuffix)) {
				continue
			}
			segment, err := lineage.capture(ctx, storeIndex, name, end)
			if err != nil {
				if !strings.HasSuffix(name, zstSuffix) && slices.Contains(names, name+zstSuffix) {
					segment, err = lineage.capture(ctx, storeIndex, name+zstSuffix, end)
				}
				if err != nil {
					return codexLineageSegment{}, err
				}
			}
			if found && (selected.prefixHash != segment.prefixHash || !bytes.Equal(selected.prefix, segment.prefix)) {
				return codexLineageSegment{}, errors.New("Codex lineage has divergent copies of an inherited prefix")
			}
			if !found {
				selected, found = segment, true
			}
		}
	}
	if !found {
		return codexLineageSegment{}, errors.New("Codex lineage is missing its captured ancestor rollout")
	}
	return selected, nil
}

func (lineage *codexLineageSnapshot) provenance(segment codexLineageSegment) sdk.SessionInputProvenance {
	encoding := "jsonl"
	if packed(segment.path) {
		encoding = "jsonl.zst"
	}
	end := segment.decodedBytes
	if segment.end != nil {
		end = int(*segment.end.Offset)
	}
	return sdk.SessionInputProvenance{
		Path:      filepath.ToSlash(filepath.Join(filepath.Base(lineage.stores[segment.store].requested), segment.path)),
		RolloutID: segment.rolloutID, Encoding: encoding,
		RawBytes: strconv.Itoa(segment.rawBytes), RawSHA256: hex.EncodeToString(segment.rawHash[:]),
		DecodedBytes: strconv.Itoa(segment.decodedBytes), DecodedSHA256: hex.EncodeToString(segment.decodedHash[:]),
		ByteStart: "0", ByteEndExclusive: strconv.Itoa(end), PrefixSHA256: hex.EncodeToString(segment.prefixHash[:]),
		OrdinalStart: strconv.FormatUint(segment.ordinalStart, 10), OrdinalEndExclusive: strconv.FormatUint(segment.ordinalEnd, 10),
	}
}

func (lineage *codexLineageSnapshot) check(ctx context.Context) error {
	if err := checkCodexLineageAdmission(ctx); err != nil {
		return err
	}
	if !slices.Contains(CodexDirs(), lineage.home) {
		return extension.ErrActionChanged
	}
	entries := 0
	for index := range lineage.stores {
		store := &lineage.stores[index]
		if err := store.check(ctx, &entries); err != nil {
			return err
		}
	}
	return checkCodexLineageAdmission(ctx)
}
