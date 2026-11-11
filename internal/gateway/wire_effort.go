package gateway

import (
	"context"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// thinkingBackOn is req with the thinking off it asked for taken back, for
// a model that refused it: at the least the model has, as a request asking
// none was sent before thinking could be turned off (#1636).
func thinkingBackOn(p provider.Provider, model string, req *Request) *Request {
	r := *req
	r.ThinkOff = false
	if r.Effort == "" || offEffort(r.Effort) {
		r.Effort = onEffort(p, model)
	}
	return &r
}

// wireAt is where a translated request leaves the reasoning it was built
// at, for the log (wireEffortOf); set says it was.
type wireAt struct {
	effort string
	set    bool
	// tier: the service tier the try's request went out at, whichever
	// path sent it (noteWireTier)
	tier string
}

type wireAtKey struct{}

// withWireAt is ctx carrying w, which forwardTranslated fills.
func withWireAt(ctx context.Context, w *wireAt) context.Context {
	return context.WithValue(ctx, wireAtKey{}, w)
}

// noteWireEffort leaves, where the request's context has a place for it,
// the reasoning req goes to to at.
func noteWireEffort(ctx context.Context, to provider.Protocol, req *Request) {
	w, _ := ctx.Value(wireAtKey{}).(*wireAt)
	if w == nil {
		return
	}
	if e, ok := wireEffort(to, req); ok {
		w.effort, w.set = e, true
	}
}

// noteNativeWireEffort records the final native body after extra fields and
// normalization, including on retries and passthroughs.
func noteNativeWireEffort(ctx context.Context, body []byte) {
	w, _ := ctx.Value(wireAtKey{}).(*wireAt)
	if w == nil {
		return
	}
	fields := gjson.GetManyBytes(body, "thinking.type", "output_config.effort")
	effort := ""
	switch fields[0].String() {
	case "disabled", "between_tools":
		effort = "none"
	case "enabled", "adaptive":
		effort = fields[1].String()
	}
	w.effort, w.set = effort, true
}

// wireEffort is the reasoning a request built from req asks the model for,
// in the words the log uses: what the client asked can go as another
// level once translated — effort none and minimal read as low where the
// model's levels don't say it can stop (effortOf), thinking turned off
// goes as off to Anthropic's API — and the log said the one asked while
// another was sent (#1636). false for an API whose builder says it its own
// way (Gemini's levels and budgets), which the log leaves as asked.
func wireEffort(to provider.Protocol, req *Request) (string, bool) {
	switch {
	case req.GeminiCompat:
		return "", false
	case to == provider.Anthropic:
		if req.ThinkOff {
			return "none", true
		}
		return req.Effort, true
	case to == provider.Chat || to == provider.Responses:
		return req.Effort, true
	}
	return "", false
}
