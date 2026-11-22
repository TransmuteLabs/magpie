package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
)

func ParseUsageRevision(value string) (uint64, error) {
	revision, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(revision, 10) != value {
		return 0, errors.New("usage revision must be a canonical unsigned integer string")
	}
	return revision, nil
}

func validUsageTimestamp(milliseconds int64, field json.RawMessage) bool {
	return milliseconds >= 0 && milliseconds <= 253402300799999 &&
		!bytes.Equal(bytes.TrimSpace(field), []byte("null"))
}

func ValidateUsageResult(manifest Manifest, raw json.RawMessage) (UsageResult, bool, error) {
	var result UsageResult
	if len(raw) > MaxFrameBytes || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' ||
		json.Unmarshal(raw, &result) != nil || len(result.Windows) > 1024 {
		return result, false, errors.New("invalid usage result")
	}
	var presence struct {
		Revision json.RawMessage              `json:"revision"`
		AsOf     json.RawMessage              `json:"asOf"`
		Windows  []map[string]json.RawMessage `json:"windows"`
	}
	if json.Unmarshal(raw, &presence) != nil || len(presence.Windows) != len(result.Windows) {
		return result, false, errors.New("invalid usage windows")
	}
	if !validUsageTimestamp(result.AsOf, presence.AsOf) {
		return result, false, errors.New("invalid usage timestamp")
	}
	if manifest.Contract.Version.Minor < 4 && presence.Revision != nil {
		return result, false, errors.New("usage revisions require SDK API 1.4")
	}
	if presence.Revision != nil {
		if _, err := ParseUsageRevision(result.Revision); err != nil {
			return result, false, err
		}
	}
	fresh, ordinary := result.Error == "" && result.AsOf == 0 && !result.ReadDeferred, false
	for index, window := range result.Windows {
		fields := presence.Windows[index]
		if err := ValidateRichQuotaWindow(manifest, window, fields); err != nil {
			return result, false, err
		}
		if manifest.Contract.Version.Minor < 4 && (fields["models"] != nil || fields["notModels"] != nil || fields["aside"] != nil) {
			return result, false, errors.New("usage applicability requires SDK API 1.4")
		}
		if bytes.Equal(bytes.TrimSpace(fields["aside"]), []byte("null")) {
			return result, false, errors.New("invalid quota aside flag")
		}
		if window.Name == "" || !validUsageTimestamp(window.AsOf, fields["asOf"]) ||
			!validUsageTimestamp(window.ResetsAt, fields["resetsAt"]) ||
			len(window.Models) != 0 && len(window.NotModels) != 0 {
			return result, false, errors.New("invalid quota window")
		}
		for _, models := range [][]string{window.Models, window.NotModels} {
			if len(models) > 1024 {
				return result, false, errors.New("too many quota model selectors")
			}
			seen := map[string]bool{}
			for _, model := range models {
				key := strings.ToLower(model)
				if model == "" || len(model) > 300 || strings.ContainsFunc(model, func(char rune) bool { return unicode.IsSpace(char) || unicode.IsControl(char) }) || seen[key] {
					return result, false, errors.New("invalid quota model selector")
				}
				seen[key] = true
			}
		}
		used := bytes.TrimSpace(fields["used"])
		unknown := len(used) == 0 || bytes.Equal(used, []byte("null")) || window.Unknown || window.Error != ""
		if !unknown && (math.IsNaN(window.Used) || math.IsInf(window.Used, 0) || window.Used < 0 || window.Used > 100) {
			return result, false, errors.New("invalid quota percentage")
		}
		if !window.Aside {
			ordinary = true
			fresh = fresh && !unknown && window.AsOf == 0
		}
	}
	return result, fresh && ordinary, nil
}

func BusinessCancel(manifest Manifest, method string) bool {
	return manifest.Contract.Version.Minor >= 4 && method == MethodOperationCancel
}
