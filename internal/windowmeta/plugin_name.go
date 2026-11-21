package windowmeta

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func PluginDisplayName(value string) string {
	name := strings.TrimSpace(value)
	if len(name) > 80 || !utf8.ValidString(name) || strings.ContainsFunc(name, func(char rune) bool {
		return unicode.IsControl(char) || unicode.Is(unicode.Cf, char)
	}) {
		return ""
	}
	return name
}
