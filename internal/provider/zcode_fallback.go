package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/plugin"
)

// ZCodeFallbackRequest describes a proposed implicit move to the ordinary
// model on the very same plugin account. It contains no credentials.
type ZCodeFallbackRequest struct {
	Provider       string
	Account        string
	RequestedModel string
	Model          string
}

type zcodeFallbackKey struct{}

const (
	zcodeFallbackHeader      = "X-Magpie-ZCode-Fallback"
	zcodeFallbackModelHeader = "X-Magpie-ZCode-Fallback-Model"
)

type zcodeFallbackState struct {
	mu           sync.Mutex
	authorize    func(ZCodeFallbackRequest) error
	attempted    bool
	provider     string
	account      string
	model        string
	bundleSpec   string
	bundleTarget string
	expectation  zcodeAccountExpectation
	rejected     error
}

// WithZCodeFallback installs the request-scoped policy for an implicit
// Highspeed-to-ordinary transition. Without it, a transition is refused.
func WithZCodeFallback(ctx context.Context, authorize func(ZCodeFallbackRequest) error) context.Context {
	return context.WithValue(ctx, zcodeFallbackKey{}, &zcodeFallbackState{authorize: authorize})
}

// ZCodeFallbackResult reports the accepted ordinary model or a local
// rejection, so the gateway can account and classify the actual try.
func ZCodeFallbackResult(ctx context.Context) (string, error) {
	s, _ := ctx.Value(zcodeFallbackKey{}).(*zcodeFallbackState)
	if s == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model, s.rejected
}

func zcodeFallbackReject(ctx context.Context, err error) error {
	if s, ok := ctx.Value(zcodeFallbackKey{}).(*zcodeFallbackState); ok {
		s.mu.Lock()
		s.rejected = err
		s.mu.Unlock()
	}
	return err
}

func zcodeFallbackAuthorize(ctx context.Context, request ZCodeFallbackRequest, bundleSpec, bundleTarget string, expectation zcodeAccountExpectation) error {
	s, _ := ctx.Value(zcodeFallbackKey{}).(*zcodeFallbackState)
	if s == nil {
		return errors.New("ordinary ZCode fallback requires gateway authorization")
	}
	s.mu.Lock()
	if s.attempted || s.authorize == nil {
		s.mu.Unlock()
		return zcodeFallbackReject(ctx, errors.New("ordinary ZCode fallback is not authorized"))
	}
	s.attempted = true
	guard := s.authorize
	s.mu.Unlock()
	if err := guard(request); err != nil {
		return zcodeFallbackReject(ctx, err)
	}
	s.mu.Lock()
	s.provider, s.account, s.model = request.Provider, request.Account, request.Model
	s.bundleSpec, s.bundleTarget = bundleSpec, bundleTarget
	s.expectation = expectation
	s.mu.Unlock()
	return nil
}

func zcodeFallbackExpectation(ctx context.Context) zcodeAccountExpectation {
	s, _ := ctx.Value(zcodeFallbackKey{}).(*zcodeFallbackState)
	if s == nil {
		return zcodeAccountExpectation{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expectation
}

// zcodeFallbackModel keeps subsequent retries in the same attempt on the
// authorized ordinary model and refuses a change of account.
func zcodeFallbackModel(ctx context.Context, provider, account string) (string, string, string, error) {
	s, _ := ctx.Value(zcodeFallbackKey{}).(*zcodeFallbackState)
	if s == nil {
		return "", "", "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.model == "" {
		return "", "", "", s.rejected
	}
	if s.provider != provider || s.account != account {
		s.rejected = errors.New("ordinary ZCode fallback cannot change account")
		return "", "", "", s.rejected
	}
	return s.model, s.bundleSpec, s.bundleTarget, nil
}

// zcodeAlias uses the same @size convention as account usage applicability.
// The returned alias is the actual bundled model, not a caller's local id.
func zcodeAlias(model string) (alias, raw, lane string, ok bool) {
	base, _, _ := strings.Cut(model, "@")
	switch strings.ToLower(base) {
	case "glm-5.3-offpeak":
		return "GLM-5.3-OffPeak", "GLM-5.3", "offpeak", true
	case "glm-5.3-flash-offpeak":
		return "GLM-5.3-Flash-OffPeak", "GLM-5.3-Flash", "offpeak", true
	case "glm-5.3-highspeed":
		return "GLM-5.3-Highspeed", "GLM-5.3", "highspeed", true
	case "glm-5.3-flash-highspeed":
		return "GLM-5.3-Flash-Highspeed", "GLM-5.3-Flash", "highspeed", true
	default:
		return "", "", "", false
	}
}

// zcodeUnbundledNative detects lane intent before an explicit source's
// API-id rewrite can turn it into an ordinary paid request.
func zcodeUnbundledNative(pp plugin.Provider, model string, matched plugin.Model, found bool) bool {
	if pp.ID != "zcode" || plugin.IsBundledZCode(pp.Spec) {
		return false
	}
	isLane := func(id string) bool {
		_, _, _, ok := zcodeAlias(id)
		return ok
	}
	if isLane(model) {
		return true
	}
	base, _, _ := strings.Cut(model, "@")
	if isLane(modelIDsOf(PluginID(pp.ID)).ownOf(base)) {
		return true
	}
	if found && (isLane(matched.ID) || isLane(matched.APIID)) {
		return true
	}
	if base != model {
		if m, ok := pluginModel(pp, base); ok && (isLane(m.ID) || isLane(m.APIID)) {
			return true
		}
	}
	return false
}

// IsBundledZCodeHighspeed reports the actual listed native lane even when
// the user renamed it or requests a context-size variant.
func (p Provider) IsBundledZCodeHighspeed(model string) bool {
	if !p.IsPlugin() || p.Account.plugin.ID != "zcode" || !plugin.IsBundledZCode(p.Account.plugin.Spec) {
		return false
	}
	own := p.OwnModelID(model)
	if _, _, lane, ok := zcodeAlias(own); ok {
		return lane == "highspeed"
	}
	if base, _, sized := strings.Cut(model, "@"); sized {
		_, _, lane, ok := zcodeAlias(p.OwnModelID(base))
		return ok && lane == "highspeed"
	}
	return false
}

func zcodeFallbackInvalid(why string) error {
	return fmt.Errorf("invalid internal ZCode fallback: %s", why)
}
