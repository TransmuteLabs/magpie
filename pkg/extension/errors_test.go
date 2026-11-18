package extension

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPublicErrorsFollowSharedWireLimits(t *testing.T) {
	for _, public := range []*Error{
		{Code: strings.Repeat("A", 64), Message: strings.Repeat("m", 4096), Outcome: strings.Repeat("x", 64)},
		{Code: "Auth.DENIED_1-2", Message: "\tmessage\n\r"},
		{Code: "public", Message: strings.Repeat("界", 1365) + "x"},
		{Code: "public", Message: "\u0085"},
	} {
		if got := publicError(public); *got != *public {
			t.Fatalf("valid public error changed: got=%+v want=%+v", got, public)
		}
	}
	for _, public := range []*Error{
		{Code: "", Message: "message"},
		{Code: strings.Repeat("x", 65), Message: "message"},
		{Code: "bad code", Message: "message"},
		{Code: "invalid/namespace", Message: "message"},
		{Code: "invalid\u0085", Message: "message"},
		{Code: "valid", Message: ""},
		{Code: "valid", Message: " \t\r\n"},
		{Code: "valid", Message: "\ufeff"},
		{Code: "valid", Message: strings.Repeat("x", 4097)},
		{Code: "valid", Message: strings.Repeat("界", 1366)},
		{Code: "valid", Message: "message", Outcome: "not/allowed"},
		{Code: "valid", Message: "message", Outcome: strings.Repeat("x", 65)},
	} {
		if got := publicError(public); got.Code != "internal" || got.Message != "extension handler failed" || got.Outcome != "" {
			t.Fatalf("invalid public error escaped: %+v", got)
		}
	}
	for char := rune(0); char <= 127; char++ {
		if char > 31 && char != 127 || char == '\t' || char == '\n' || char == '\r' {
			continue
		}
		got := publicError(&Error{Code: "valid", Message: "message" + string(char)})
		if got.Code != "internal" {
			t.Fatalf("public error retained forbidden control U+%04X", char)
		}
	}
	for _, test := range []struct {
		err  error
		code string
	}{
		{err: context.Canceled, code: "canceled"},
		{err: context.DeadlineExceeded, code: "deadline"},
		{err: errors.New("private credential"), code: "internal"},
	} {
		if got := publicError(test.err); got.Code != test.code || strings.Contains(got.Message, "credential") {
			t.Fatal("context/private error mapping changed", got)
		}
	}
}
