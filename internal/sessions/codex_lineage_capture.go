package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/extension"
)

func codexLineageHeader(body []byte) (codexLineageSegment, error) {
	segment := codexLineageSegment{}
	line, _, _ := bytes.Cut(bytes.TrimLeft(body, " \r\n\t"), []byte{'\n'})
	if json.Unmarshal(line, &segment.header) != nil || string(segment.header["type"]) != `"session_meta"` {
		return segment, nil
	}
	if json.Unmarshal(segment.header["payload"], &segment.metadata) != nil || segment.metadata == nil {
		return segment, errors.New("invalid Codex lineage session metadata")
	}
	base := segment.metadata["history_base"]
	if len(base) == 0 || bytes.Equal(bytes.TrimSpace(base), []byte("null")) {
		return segment, nil
	}
	if json.Unmarshal(base, &segment.base) != nil || segment.base == nil || segment.base.Ordinal == nil ||
		segment.base.Offset == nil || *segment.base.Ordinal == 0 || *segment.base.Ordinal == ^uint64(0) ||
		*segment.base.Offset == 0 || *segment.base.Offset > extension.MaxActionBytes {
		return segment, errors.New("invalid Codex lineage history_base range")
	}
	id, err := uuid.Parse(segment.base.RolloutID)
	if err != nil || segment.base.RolloutID != id.String() {
		return segment, errors.New("invalid Codex lineage immutable rollout identity")
	}
	return segment, nil
}

func (lineage *codexLineageSnapshot) capture(ctx context.Context, storeIndex int, name string, end *codexHistoryPosition) (codexLineageSegment, error) {
	if err := checkCodexLineageAdmission(ctx); err != nil {
		return codexLineageSegment{}, err
	}
	if lineage.files >= extension.MaxActionFiles {
		return codexLineageSegment{}, errors.New("Codex lineage source file budget exceeded")
	}
	store := &lineage.stores[storeIndex]
	root, err := store.open()
	if err != nil {
		return codexLineageSegment{}, err
	}
	file, raw, err := readSessionInput(ctx, root, name, extension.MaxActionBytes-lineage.rawBytes)
	root.Close()
	if err != nil {
		return codexLineageSegment{}, err
	}
	store.inputs.files = append(store.inputs.files, file)
	lineage.files++
	lineage.rawBytes += len(raw)
	body := raw
	if packed(name) {
		decoder, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(extension.MaxActionBytes))
		if err != nil {
			return codexLineageSegment{}, err
		}
		body, err = io.ReadAll(io.LimitReader(decoder, int64(extension.MaxActionBytes-lineage.decodedBytes)+1))
		decoder.Close()
		if err != nil {
			return codexLineageSegment{}, err
		}
	}
	lineage.decodedBytes += len(body)
	if lineage.decodedBytes > extension.MaxActionBytes {
		return codexLineageSegment{}, errors.New("Codex lineage decoded byte budget exceeded")
	}
	segment, err := codexLineageHeader(body)
	if err != nil {
		return codexLineageSegment{}, err
	}
	segment.store, segment.path, segment.end = storeIndex, name, end
	segment.rolloutID = codexRolloutID(filepath.Base(name))
	segment.decodedHash = sha256.Sum256(body)
	segment.rawHash, segment.rawBytes, segment.decodedBytes = file.hash, len(raw), len(body)
	if segment.header == nil || segment.metadata == nil || segment.rolloutID == "" {
		return segment, errors.New("Codex lineage requires a native rollout header and filename")
	}
	var owner, mode string
	if json.Unmarshal(segment.metadata["id"], &owner) != nil ||
		json.Unmarshal(segment.metadata["history_mode"], &mode) != nil || mode != "paginated" ||
		codexThreadOf(filepath.Base(name)) != owner {
		return segment, errors.New("Codex lineage rollout metadata does not match its native identity")
	}
	if end != nil {
		if segment.rolloutID != end.RolloutID || *end.Offset > uint64(len(body)) {
			return segment, errors.New("Codex lineage cutoff exceeds its decoded ancestor")
		}
		body = body[:int(*end.Offset)]
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return segment, errors.New("Codex lineage cutoff must end at a complete JSONL record")
	}
	segment.prefixHash = sha256.Sum256(body)
	prefix := body
	ordinal := uint64(0)
	if segment.base != nil {
		ordinal = *segment.base.Ordinal
	}
	segment.ordinalStart = ordinal
	first := true
	for len(body) != 0 {
		if err := ctx.Err(); err != nil {
			return segment, err
		}
		line, rest, _ := bytes.Cut(body, []byte{'\n'})
		body = rest
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type    string  `json:"type"`
			Ordinal *uint64 `json:"ordinal"`
		}
		if json.Unmarshal(line, &record) != nil || record.Ordinal == nil || *record.Ordinal != ordinal ||
			record.Type == "" || first && record.Type != "session_meta" || !first && record.Type == "session_meta" {
			return segment, errors.New("Codex lineage has inconsistent record ordinals or metadata")
		}
		if first {
			first = false
		}
		if ordinal == ^uint64(0) {
			return segment, errors.New("Codex lineage record ordinal overflow")
		}
		ordinal++
	}
	if first || end != nil && ordinal != *end.Ordinal {
		return segment, errors.New("Codex lineage decoded cutoff and exclusive ordinal disagree")
	}
	segment.ordinalEnd, segment.prefix = ordinal, prefix
	return segment, nil
}
