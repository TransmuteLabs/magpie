package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
)

const SessionRelatedInputs Capability = "session-related-inputs"
const CodexRolloutLineage = "codex-rollout-lineage"

func sessionRelatedHandler(manifest Manifest, handler Handler) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		var request SessionRequest
		if decodeParams(call.Params, &request) != nil {
			return nil, &Error{Code: "invalid_params", Message: "session.convert requires a valid session request"}
		}
		if err := ValidateSessionRelatedInputs(manifest, request); err != nil {
			return nil, &Error{Code: "invalid_params", Message: err.Error()}
		}
		return handler(ctx, call, stream)
	}
}

type SessionRelatedInputSet struct {
	Version   int                    `json:"version"`
	Kind      string                 `json:"kind"`
	Selected  SessionInputProvenance `json:"selected"`
	Ancestors []SessionRelatedInput  `json:"ancestors"`
}

type SessionRelatedInput struct {
	Body   []byte                 `json:"body"`
	Source SessionInputProvenance `json:"source"`
}

type SessionInputProvenance struct {
	Path                string `json:"path"`
	RolloutID           string `json:"rolloutId"`
	Encoding            string `json:"encoding"`
	RawBytes            string `json:"rawBytes"`
	RawSHA256           string `json:"rawSha256"`
	DecodedBytes        string `json:"decodedBytes"`
	DecodedSHA256       string `json:"decodedSha256"`
	ByteStart           string `json:"byteStart"`
	ByteEndExclusive    string `json:"byteEndExclusive"`
	OrdinalStart        string `json:"ordinalStart"`
	OrdinalEndExclusive string `json:"ordinalEndExclusive"`
	PrefixSHA256        string `json:"prefixSha256"`
}

func (related *SessionRelatedInputSet) Clone() *SessionRelatedInputSet {
	if related == nil {
		return nil
	}
	cloned := *related
	cloned.Ancestors = slices.Clone(related.Ancestors)
	for index := range cloned.Ancestors {
		cloned.Ancestors[index].Body = bytes.Clone(cloned.Ancestors[index].Body)
	}
	return &cloned
}

func ValidateSessionRelatedContract(manifest Manifest) error {
	if !manifest.Contract.Has(SessionRelatedInputs) {
		return nil
	}
	if manifest.Contract.Version.Major != 1 || manifest.Contract.Version.Minor < 10 ||
		!manifest.Contract.Has(SessionMigration) {
		return errors.New("session-related-inputs requires API 1.10 and session-migration")
	}
	return nil
}

func ValidateSessionRelatedInputs(manifest Manifest, request SessionRequest) error {
	if request.Related == nil {
		return nil
	}
	if err := ValidateSessionRelatedContract(manifest); err != nil {
		return err
	}
	if !manifest.Contract.Has(SessionRelatedInputs) {
		return errors.New("session-related-inputs capability is required")
	}
	related := request.Related
	if related.Version != 1 || related.Kind != CodexRolloutLineage || request.From != "codex" ||
		len(request.Files) != 1 || len(related.Ancestors) == 0 || len(related.Ancestors) >= 32 {
		return errors.New("invalid finite codex-rollout-lineage carrier")
	}
	if err := related.Selected.validate(request.Files[0].Body, true); err != nil {
		return err
	}
	seen := map[string]bool{related.Selected.RolloutID: true}
	paths := map[string]bool{related.Selected.Path: true}
	total := len(request.Files[0].Body)
	rawTotal, _ := strconv.ParseUint(related.Selected.RawBytes, 10, 64)
	decodedTotal, _ := strconv.ParseUint(related.Selected.DecodedBytes, 10, 64)
	for _, ancestor := range related.Ancestors {
		if seen[ancestor.Source.RolloutID] || paths[ancestor.Source.Path] {
			return errors.New("duplicate related rollout identity or capture path")
		}
		seen[ancestor.Source.RolloutID], paths[ancestor.Source.Path] = true, true
		if err := ancestor.Source.validate(ancestor.Body, false); err != nil {
			return err
		}
		total += len(ancestor.Body)
		rawSize, _ := strconv.ParseUint(ancestor.Source.RawBytes, 10, 64)
		decodedSize, _ := strconv.ParseUint(ancestor.Source.DecodedBytes, 10, 64)
		rawTotal, decodedTotal = rawTotal+rawSize, decodedTotal+decodedSize
		if total > 16<<20 || rawTotal > 16<<20 || decodedTotal > 16<<20 {
			return errors.New("related session inputs exceed their byte budget")
		}
	}
	return nil
}

func (source SessionInputProvenance) validate(body []byte, selected bool) error {
	if !SessionSourcePath(source.Path) ||
		!strings.HasPrefix(source.Path, "sessions/") && !strings.HasPrefix(source.Path, "archived_sessions/") ||
		!relatedRolloutID(source.RolloutID) || source.Encoding != "jsonl" && source.Encoding != "jsonl.zst" ||
		!strings.HasSuffix(source.Path, "."+source.Encoding) {
		return errors.New("invalid related input capture identity")
	}
	numbers := []string{source.RawBytes, source.DecodedBytes, source.ByteStart, source.ByteEndExclusive,
		source.OrdinalStart, source.OrdinalEndExclusive}
	values := make([]uint64, len(numbers))
	for index, number := range numbers {
		value, err := strconv.ParseUint(number, 10, 64)
		if err != nil || strconv.FormatUint(value, 10) != number {
			return errors.New("related input ranges require canonical decimal uint64 strings")
		}
		values[index] = value
	}
	rawBytes, decodedBytes, start, end, first, last := values[0], values[1], values[2], values[3], values[4], values[5]
	if rawBytes == 0 || rawBytes > 16<<20 || decodedBytes == 0 || decodedBytes > 16<<20 ||
		start != 0 || end == 0 || end > decodedBytes || end != uint64(len(body)) || first >= last ||
		selected && end != decodedBytes {
		return errors.New("invalid related input decoded prefix or ordinal range")
	}
	for _, hash := range []string{source.RawSHA256, source.DecodedSHA256, source.PrefixSHA256} {
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != hash {
			return errors.New("related input requires lowercase SHA256 capture digests")
		}
	}
	digest := sha256.Sum256(body)
	if source.PrefixSHA256 != hex.EncodeToString(digest[:]) ||
		end == decodedBytes && source.PrefixSHA256 != source.DecodedSHA256 ||
		source.Encoding == "jsonl" && (rawBytes != decodedBytes || source.RawSHA256 != source.DecodedSHA256) {
		return errors.New("related input body differs from its captured digest")
	}
	return nil
}

func relatedRolloutID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	raw := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == raw
}
