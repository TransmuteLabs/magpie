package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"unicode"
	"unicode/utf8"
)

func ValidateRichQuotaWindow(manifest Manifest, window UsageWindow, fields map[string]json.RawMessage) error {
	for _, key := range []string{"amount", "balance", "display", "unit", "span"} {
		if fields[key] != nil && manifest.Contract.Version.Minor < 9 {
			return errors.New("rich quota metadata requires SDK API 1.9")
		}
	}
	if manifest.Contract.Version.Minor < 9 {
		return nil
	}
	for _, key := range []string{"amount", "balance", "limit"} {
		raw := bytes.TrimSpace(fields[key])
		if len(raw) == 0 || key != "limit" && bytes.Equal(raw, []byte("null")) {
			continue
		}
		var number float64
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &number) != nil ||
			math.IsNaN(number) || math.IsInf(number, 0) || math.Abs(number) > 9007199254740991 ||
			key != "balance" && number < 0 {
			return errors.New("invalid rich quota amount, balance or limit")
		}
	}
	for _, field := range []struct {
		key   string
		value string
		bytes int
	}{{"display", window.Display, 512}, {"unit", window.Unit, 64}} {
		raw := bytes.TrimSpace(fields[field.key])
		if len(raw) != 0 && (raw[0] != '"' || !utf8.ValidString(field.value) ||
			len(field.value) > field.bytes || bytes.ContainsFunc([]byte(field.value), unicode.IsControl)) {
			return errors.New("invalid rich quota text")
		}
	}
	if raw := bytes.TrimSpace(fields["span"]); len(raw) != 0 {
		var seconds int64
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &seconds) != nil || seconds < 0 || seconds > 9223372036 {
			return errors.New("invalid rich quota duration in seconds")
		}
	}
	return nil
}
