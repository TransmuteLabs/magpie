package extension

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// NewHTTPError preserves a public inbound codec rejection in the existing RPC
// error code. Only 400–599 are rejection statuses; invalid statuses use 400.
// The message must be safe for the caller and follows the public error limits.
func NewHTTPError(status int, message string) *Error {
	if !validPublicMessage(message) {
		return &Error{Code: "internal", Message: "extension handler failed"}
	}
	if status < 400 || status > 599 || status == 400 {
		return &Error{Code: "invalid_params", Message: message}
	}
	return &Error{Code: "http." + strconv.Itoa(status), Message: message}
}

// HTTPStatus returns the rejection status carried by an RPC error, or zero
// when its code has no HTTP meaning. It never derives a status from a message.
// Existing cancellation, deadline and size errors retain their classifications.
func (err *Error) HTTPStatus() int {
	if err == nil {
		return 0
	}
	switch err.Code {
	case "invalid_params":
		return 400
	case "canceled":
		return 499
	case "deadline":
		return 504
	case "resource_exhausted":
		return 413
	}
	if len(err.Code) != len("http.400") || !strings.HasPrefix(err.Code, "http.") {
		return 0
	}
	status, parseErr := strconv.Atoi(err.Code[len("http."):])
	if parseErr != nil || status < 400 || status > 599 {
		return 0
	}
	return status
}

func validPublicToken(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '_' || char == '.' || char == '-') {
			return false
		}
	}
	return true
}

func validPublicMessage(message string) bool {
	if len(message) > 4096 || !utf8.ValidString(message) || strings.TrimFunc(message, errorWhitespace) == "" {
		return false
	}
	for _, char := range message {
		if char <= 8 || char == 11 || char == 12 || char >= 14 && char <= 31 || char == 127 {
			return false
		}
	}
	return true
}

func errorWhitespace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	default:
		return char >= '\u2000' && char <= '\u200a'
	}
}
