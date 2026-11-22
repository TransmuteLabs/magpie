package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	AccountCheckinRateLimitAPIMinor = 20
	AccountCheckinRateLimited       = "rate-limited"
)

type AccountCheckinDescriptor struct {
	Action  string `json:"action"`
	Recheck bool   `json:"recheck,omitempty"`
}

type AccountCheckinRequest struct {
	Day   string `json:"day"`
	Proxy string `json:"proxy,omitempty"`
}

type AccountCheckinResult struct {
	Outcome string   `json:"outcome"`
	Credit  *float64 `json:"credit,omitempty"`
	Streak  *int     `json:"streak,omitempty"`
	Message string   `json:"message,omitempty"`
	Until   string   `json:"until,omitempty"`
}

func (manifest Manifest) validateAccountCheckins() error {
	for _, provider := range manifest.Providers {
		if provider.Checkin == nil {
			continue
		}
		action, found := manifest.ResolveAction(provider.Checkin.Action)
		if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(AccountAuth) ||
			!manifest.Contract.Has(ModuleRPC) || !found || action.Effect != "write" ||
			manifest.Component != nil {
			return fmt.Errorf("account check-in requires SDK 1.8, account auth and a declared module write")
		}
	}
	return nil
}

func (manifest Manifest) AccountCheckin(provider string) (AccountCheckinDescriptor, bool) {
	for _, descriptor := range manifest.Providers {
		if descriptor.ID == provider && descriptor.Checkin != nil {
			return *descriptor.Checkin, true
		}
	}
	return AccountCheckinDescriptor{}, false
}

func (request AccountCheckinRequest) Validate() error {
	day, err := time.Parse("2006-01-02", request.Day)
	if err != nil || day.Format("2006-01-02") != request.Day ||
		!checkinText(request.Proxy, 4096, false) {
		return fmt.Errorf("check-in requires a canonical day and bounded selected proxy")
	}
	return nil
}

func (result AccountCheckinResult) Validate() error {
	switch result.Outcome {
	case "claimed", "done", "inactive", "ineligible", "failed", "captcha", "own-app":
	case AccountCheckinRateLimited:
		if result.Until == "" {
			return fmt.Errorf("rate-limited check-in requires an until boundary")
		}
	default:
		return fmt.Errorf("unknown account check-in outcome")
	}
	if result.Credit != nil && (math.IsNaN(*result.Credit) || math.IsInf(*result.Credit, 0) || *result.Credit < 0) ||
		result.Streak != nil && (*result.Streak < 0 || int64(*result.Streak) > 2147483647) ||
		!checkinText(result.Message, 4096, true) {
		return fmt.Errorf("invalid account check-in result")
	}
	if result.Until != "" {
		if !validSourceTimestamp(result.Until) {
			return fmt.Errorf("invalid account check-in boundary")
		}
	}
	return nil
}

func (result AccountCheckinResult) Clone() AccountCheckinResult {
	if result.Credit != nil {
		value := *result.Credit
		result.Credit = &value
	}
	if result.Streak != nil {
		value := *result.Streak
		result.Streak = &value
	}
	return result
}

func DecodeAccountCheckinResult(raw json.RawMessage) (AccountCheckinResult, error) {
	var result AccountCheckinResult
	if len(raw) > MaxChunkBytes || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return result, fmt.Errorf("account check-in result must be a bounded object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return result, fmt.Errorf("invalid account check-in result")
	}
	return result, result.Validate()
}

func checkinText(value string, limit int, multiline bool) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsFunc(value, func(char rune) bool {
		return unicode.IsControl(char) && !(multiline && (char == '\n' || char == '\t'))
	})
}
