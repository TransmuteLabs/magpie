package sessionformat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf16"
)

// EachLine reads complete lines and the final unterminated line.
func EachLine(ctx context.Context, source io.Reader, fn func([]byte)) error {
	reader := bufio.NewReaderSize(source, 1<<16)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ResultText returns a tool's text or blocks.
func ResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, b.Text)
		case "image":
			out = append(out, "[image]")
		}
	}
	return strings.Join(out, "\n")
}

// Pretty indents short JSON for reading.
func Pretty(raw json.RawMessage) string {
	if len(raw) == 0 || len(raw) > 4000 {
		return string(raw)
	}
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return string(raw)
	}
	return b.String()
}

// CodexOutput extracts the text of a Codex tool's result.
func CodexOutput(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Content string `json:"content"`
		Output  string `json:"output"`
	}
	if json.Unmarshal(raw, &o) == nil && (o.Content != "" || o.Output != "") {
		if o.Content != "" {
			return o.Content
		}
		return o.Output
	}
	return string(raw)
}

// ProjectName computes the name of an unhashed Claude Code project directory.
func ProjectName(p string) (string, error) {
	n := ProjectSlug(p)
	if len(n) > 200 {
		return "", errors.New("long hashed Claude project paths are not supported yet")
	}
	return n, nil
}

// ProjectSlug replaces each non-alphanumeric UTF-16 unit with a hyphen.
func ProjectSlug(p string) string {
	var b strings.Builder
	for _, c := range utf16.Encode([]rune(p)) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteByte(byte(c))
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
