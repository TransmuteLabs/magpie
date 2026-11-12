package extension

import (
	"strings"
	"unicode/utf8"
)

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
