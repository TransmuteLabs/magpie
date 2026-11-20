package sessioncarry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func FromClaude(ctx context.Context, input sdk.SessionConversionInput) (sdk.SessionConversionOutput, error) {
	items, err := claudeCarryItems(ctx, input.Source)
	if err != nil {
		return sdk.SessionConversionOutput{}, err
	}
	if !saysSomething(items) {
		return sdk.SessionConversionOutput{}, errors.New("nothing was said in the session")
	}
	return codexOutputDocument(input, pairCarry(items))
}

func FromCodex(ctx context.Context, input sdk.SessionConversionInput) (sdk.SessionConversionOutput, error) {
	items, err := codexCarryItems(ctx, input.Source)
	if err != nil {
		return sdk.SessionConversionOutput{}, err
	}
	if !saysSomething(items) {
		return sdk.SessionConversionOutput{}, errors.New("nothing was said in the session")
	}
	return claudeOutput(input, pairCarry(items))
}

type lineWriter struct {
	ctx    context.Context
	output io.Writer
	err    error
}

func (w *lineWriter) line(value any) {
	if w.err != nil {
		return
	}
	if w.err = w.ctx.Err(); w.err != nil {
		return
	}
	data, err := marshalPlain(value)
	if err != nil {
		w.err = err
		return
	}
	_, w.err = w.output.Write(append(data, '\n'))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// carryItem is one thing said in a conversation, in neither agent's form.
type carryItem struct {
	Kind   string   // user | assistant | call | result
	Text   string   // what was said; a result's output
	Images []string // a user's images, as data: URLs
	// a call's: its tool and input, the shell line when it ran one, and
	// the folder it ran in
	Name    string
	Input   string
	Shell   string
	Workdir string
	ID      string // a call's, and its result's
	IsError bool
	// a call's result, once paired
	Result string
	Done   bool
	Time   time.Time
}

// carryResultMax is the most of one tool's output carried over: the rest is
// cut, as a model's context would have cut it.
const carryResultMax = 64 << 10

func saysSomething(items []carryItem) bool {
	for _, it := range items {
		if it.Kind == "user" {
			return true
		}
	}
	return false
}

// pairCarry puts each result on its call, in the call's place.
func pairCarry(items []carryItem) []carryItem {
	at := map[string]int{}
	out := items[:0:0]
	for _, it := range items {
		switch it.Kind {
		case "call":
			if it.ID != "" {
				at[it.ID] = len(out)
			}
			out = append(out, it)
		case "result":
			if i, ok := at[it.ID]; ok && !out[i].Done {
				out[i].Result, out[i].IsError, out[i].Done = cutResult(it.Text), it.IsError, true
			}
		default:
			out = append(out, it)
		}
	}
	return out
}

func cutResult(s string) string {
	if len(s) <= carryResultMax {
		return s
	}
	cut := carryResultMax
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "\n[…]"
}

// note is a call the target has no tool for, as words the agent said.
func (it carryItem) note() string {
	var b strings.Builder
	b.WriteString("[" + it.Name + "]\n")
	if strings.TrimSpace(it.Input) != "" {
		b.WriteString(fence(it.Input))
	}
	if it.Done {
		if it.IsError {
			b.WriteString("\n[error]\n")
		} else {
			b.WriteString("\n[result]\n")
		}
		b.WriteString(fence(it.Result))
	}
	return b.String()
}

// fence is text in a code block no line of it closes.
func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f + "\n" + strings.TrimRight(s, "\n") + "\n" + f + "\n"
}

func carryStamp(at, now time.Time) string {
	if at.IsZero() {
		at = now
	}
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

// jsonLine is v as one line of JSON, <, > and & as they are, as the
// agents write them.
func jsonLine(v any) []byte {
	b, _ := marshalPlain(v)
	return b
}

func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ord is a JSON object whose fields are written in the order given, as the
// agent's own files have them (some readers look for a line's type near
// its start).
type ord []kv

type kv struct {
	k string
	v any
}

// o is an ord of key, value pairs.
func o(pairs ...any) ord {
	out := make(ord, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, kv{pairs[i].(string), pairs[i+1]})
	}
	return out
}

func (m ord) MarshalJSON() ([]byte, error) {
	b := []byte{'{'}
	for i, f := range m {
		if i > 0 {
			b = append(b, ',')
		}
		k, _ := marshalPlain(f.k)
		v, err := marshalPlain(f.v)
		if err != nil {
			return nil, err
		}
		b = append(append(append(b, k...), ':'), v...)
	}
	return append(b, '}'), nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// splitDataURL is a data: URL's media type and base64 data.
func splitDataURL(u string) (string, string, bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", "", false
	}
	head, data, ok := strings.Cut(rest, ",")
	mt, b64 := strings.CutSuffix(head, ";base64")
	if !ok || !b64 || mt == "" {
		return "", "", false
	}
	return mt, data, true
}
