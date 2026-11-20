package pluginrt

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gwbudget"
	"github.com/yetone/magpie/internal/gwmw"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestFeatureGatewayOwnsRegistrations(t *testing.T) {
	t.Cleanup(gwtool.Gate())
	t.Cleanup(provider.GateGroups(nil))
	t.Cleanup(usage.ServeObservers())
	middleware := &struct{ sdk.LinkedAgentMiddleware }{}
	previous := &struct{ sdk.LinkedMasker }{}
	masker := &struct{ sdk.LinkedMasker }{}
	previousBudget := &struct {
		gwbudget.Limiter
		name string
	}{name: "previous"}
	budget := &struct {
		gwbudget.Limiter
		name string
	}{name: "current"}
	who := access.Identity{KeyID: "limited", Limit: &access.Limit{Period: "day", Tokens: 1}}
	t.Cleanup(gwmw.Mask(previous))
	t.Cleanup(gwbudget.Limit(previousBudget))
	observer := &featureGatewayObserver{}
	check := func(t *testing.T, active bool) {
		t.Helper()
		wantMasker := previous
		wantBudget := previousBudget
		wantBodies := usage.NoBodies
		var wantMiddleware []gwmw.Agent
		if active {
			wantMasker = masker
			wantBudget = budget
			wantBodies = usage.WholeBodies
			wantMiddleware = []gwmw.Agent{middleware}
		}
		if got := gwmw.Agents(); !slices.Equal(got, wantMiddleware) {
			t.Errorf("middleware not owned by active feature: %v, active %v", got, active)
		}
		if got, _ := gwmw.Served(); got != wantMasker {
			t.Errorf("masker not owned by active feature: %p, want %p", got, wantMasker)
		}
		if search, plugins := gwtool.Served(gwtool.WebSearch), gwtool.Served(gwtool.PluginTools); search != active || plugins != active {
			t.Errorf("search served %v, plugin tools served %v, want both %v", search, plugins, active)
		}
		if groups := provider.GroupsServed(); groups != active {
			t.Errorf("routing groups served %v, want %v", groups, active)
		}
		if got, err := gwbudget.Limiting(who); got != wantBudget || err != nil {
			t.Errorf("budget limiter %p, %v, want %p", got, err, wantBudget)
		}
		if bodies := usage.ObservedBodies(); bodies != wantBodies || observer.running != active {
			t.Errorf("observer bodies %v, running %v, want %v, %v", bodies, observer.running, wantBodies, active)
		}
	}
	for _, state := range []string{"active", "error", "panic"} {
		t.Run(state, func(t *testing.T) {
			tools := []sdk.LinkedGatewayTool{sdk.LinkedWebSearch, sdk.LinkedPluginTools}
			manifest := gatewayFeatureManifest(GatewayFeature{
				FeatureContribution: sdk.FeatureContribution{
					ID: "gateway-fixture", Middleware: middleware, Masker: masker, Tools: tools, RoutingGroups: true,
					Activate: func(sdk.FeatureLifetime) error {
						check(t, true)
						switch state {
						case "error":
							return errors.New("activation failed")
						case "panic":
							panic("activation failed")
						default:
							return nil
						}
					},
				},
				Budget:   budget,
				Observer: func() usage.Observer { return observer },
			})
			tools[0] = ""
			runtime := New([]Manifest{manifest})
			t.Cleanup(runtime.Stop)
			check(t, false)
			runtime.Reconcile(map[string]bool{"gateway-fixture": true}, nil)
			check(t, state == "active")
			runtime.Reconcile(nil, nil)
			check(t, false)
		})
	}
}

type featureGatewayObserver struct {
	running bool
}

func (observer *featureGatewayObserver) Serve() func() {
	observer.running = true
	return func() { observer.running = false }
}

func (*featureGatewayObserver) Bodies() usage.Bodies            { return usage.WholeBodies }
func (*featureGatewayObserver) Begin(usage.Request) usage.Watch { return nil }
func (*featureGatewayObserver) Ledger(usage.Record)             {}

func TestFeatureGatewayRejectsUnknownAndDuplicateTools(t *testing.T) {
	for _, tools := range [][]sdk.LinkedGatewayTool{{"unknown"}, {sdk.LinkedWebSearch, sdk.LinkedWebSearch}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("accepted invalid gateway tools %v", tools)
				}
			}()
			featureManifest(sdk.FeatureContribution{ID: "gateway-fixture", Tools: tools})
		}()
	}
}

func TestFeatureRoutingOwnsRegistration(t *testing.T) {
	const id = "fixture-order"
	seats := []routes.Seat{{Key: "first"}, {Key: "second"}}
	previous := routes.Register(id, routes.Strategy{Order: func(string, []routes.Seat) []int { return []int{0, 1} }})
	t.Cleanup(previous)
	for _, state := range []string{"active", "error", "panic"} {
		t.Run(state, func(t *testing.T) {
			declared := map[string]sdk.LinkedRoutingStrategy{
				id: {
					ByWindow: true,
					Order: func(pool string, inputs []sdk.LinkedRoutingSeat) []int {
						if pool != "group/fixture" || !slices.EqualFunc(inputs, seats, func(left, right routes.Seat) bool {
							return left.Key == right.Key
						}) {
							t.Errorf("unexpected pool or seats: %q %+v", pool, inputs)
						}
						return []int{1, 0}
					},
				},
			}
			check := func(owned bool) {
				t.Helper()
				strategy, found := routes.Find(id)
				if !found || strategy.ByWindow != owned {
					t.Errorf("route found %v, ByWindow %v, want owned %v", found, strategy.ByWindow, owned)
					return
				}
				want := []int{0, 1}
				if owned {
					want = []int{1, 0}
				}
				if got := routes.Apply(strategy, "group/fixture", seats); !slices.Equal(got, want) {
					t.Errorf("route order %v, want %v", got, want)
				}
			}
			feature := featureManifest(sdk.FeatureContribution{
				ID: "fixture", Routing: declared,
				Activate: func(sdk.FeatureLifetime) error {
					check(true)
					switch state {
					case "error":
						return errors.New("activation failed")
					case "panic":
						panic("activation failed")
					default:
						return nil
					}
				},
			})
			delete(declared, id)
			runtime := New([]Manifest{feature})
			t.Cleanup(runtime.Stop)
			check(false)
			runtime.Reconcile(map[string]bool{"fixture": true}, nil)
			wantState := Active
			if state != "active" {
				wantState = Failed
			}
			if got := runtime.Statuses()[0].State; got != wantState {
				t.Errorf("feature state = %s, want %s", got, wantState)
			}
			check(state == "active")
			runtime.Reconcile(nil, nil)
			check(false)
		})
	}
}

func TestFeatureRoutingRejectsInvalidDeclarations(t *testing.T) {
	for _, id := range []string{"", "installed/module", "missing-order"} {
		t.Run(id, func(t *testing.T) {
			strategy := sdk.LinkedRoutingStrategy{Order: func(string, []sdk.LinkedRoutingSeat) []int { return nil }}
			if id == "missing-order" {
				strategy.Order = nil
			}
			defer func() {
				if recover() == nil {
					t.Fatal("accepted invalid routing declaration")
				}
			}()
			featureManifest(sdk.FeatureContribution{ID: "fixture", Routing: map[string]sdk.LinkedRoutingStrategy{id: strategy}})
		})
	}
}

func TestFeatureActivationDisposesOwnedRegistrations(t *testing.T) {
	for _, state := range []string{"active", "error", "panic"} {
		t.Run(state, func(t *testing.T) {
			var events []string
			feature := featureManifest(sdk.FeatureContribution{
				ID: "fixture", Requires: []string{"gateway"},
				Parts: []sdk.FeaturePart{{Slot: sdk.FeatureNavigationPage, Key: "fixture"}},
				Activate: func(scope sdk.FeatureLifetime) error {
					events = append(events, "start")
					scope.OnDispose(func() { events = append(events, "first cleanup") })
					scope.OnDispose(func() { events = append(events, "second cleanup") })
					switch state {
					case "error":
						return errors.New("activation failed")
					case "panic":
						panic("activation failed")
					default:
						return nil
					}
				},
			})
			runtime := New([]Manifest{feature})
			t.Cleanup(runtime.Stop)
			runtime.Reconcile(map[string]bool{"fixture": true}, nil)
			status := runtime.Statuses()[0]
			if state == "active" {
				if status.State != Active || !slices.Equal(status.Parts, []Part{{"nav.page", "fixture"}}) {
					t.Fatalf("active feature: %+v", status)
				}
				if !slices.Equal(events, []string{"start"}) {
					t.Fatalf("before disable: %v", events)
				}
				runtime.Reconcile(nil, nil)
				status = runtime.Statuses()[0]
				if status.State != Disposed || len(status.Parts) != 0 {
					t.Fatalf("disabled feature: %+v", status)
				}
			} else if status.State != Failed || len(status.Parts) != 0 {
				t.Fatalf("failed feature: %+v", status)
			}
			runtime.Stop()
			if !slices.Equal(events, []string{"start", "second cleanup", "first cleanup"}) {
				t.Fatalf("activation and disposal: %v", events)
			}
		})
	}
}

func TestFeaturePartsRejectAmbiguousDeclarations(t *testing.T) {
	for name, declarations := range map[string][]sdk.FeaturePart{
		"unknown slot": {{Slot: "unknown", Key: "page"}},
		"empty key":    {{Slot: sdk.FeatureInlinePart}},
		"repeated part": {
			{Slot: sdk.FeatureInlinePart, Key: "tools"},
			{Slot: sdk.FeatureInlinePart, Key: "tools"},
		},
		"multiple pages": {
			{Slot: sdk.FeatureNavigationPage, Key: "alpha"},
			{Slot: sdk.FeatureNavigationPage, Key: "beta"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if parts, err := featureParts(declarations); err == nil || len(parts) != 0 {
				t.Fatalf("accepted parts %v with error %v", parts, err)
			}
		})
	}
}

func TestFeatureSchemaValidatesConfigAndOwnsItsValues(t *testing.T) {
	minimum := 1.0
	settings := []sdk.FeatureSetting{
		{Name: "page", Kind: sdk.FeatureSettingInteger, Title: "Page size", Default: 20, Enum: []any{10, 20}},
		{Name: "enabled", Kind: sdk.FeatureSettingBoolean, Title: "Enabled", Default: true},
		{Name: "interval", Kind: sdk.FeatureSettingNumber, Title: "Interval", Default: 1.5, Minimum: &minimum},
		{Name: "label", Kind: sdk.FeatureSettingString, Title: "Label", Default: ""},
	}
	schema, err := SchemaOf(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings[0].Enum[0] = 50
	minimum = 2.0
	if !slices.Equal(schema.Order, []string{"page", "enabled", "interval", "label"}) ||
		!slices.Equal(schema.Properties["page"].Enum, []any{int64(10), int64(20)}) ||
		schema.Properties["page"].Default != int64(20) {
		t.Fatalf("projected settings: %+v, page: %+v", schema, schema.Properties["page"])
	}
	got, err := schema.Validate(map[string]any{"page": 10.0, "enabled": false, "interval": 1.25, "label": "local"})
	want := map[string]any{"page": int64(10), "enabled": false, "interval": 1.25, "label": "local"}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("config = %v, %v, want %v", got, err, want)
	}
}

func TestFeatureSchemaRejectsMalformedDeclarations(t *testing.T) {
	valid := sdk.FeatureSetting{Name: "page", Kind: sdk.FeatureSettingInteger, Title: "Page size", Default: 20, Enum: []any{10, 20}}
	for name, mutate := range map[string]func(*sdk.FeatureSetting){
		"missing title": func(setting *sdk.FeatureSetting) { setting.Title = "" },
		"unknown kind":  func(setting *sdk.FeatureSetting) { setting.Kind = "object" },
		"invalid enum":  func(setting *sdk.FeatureSetting) { setting.Enum = []any{map[string]any{}} },
		"default":       func(setting *sdk.FeatureSetting) { setting.Default = 30 },
		"inverted bounds": func(setting *sdk.FeatureSetting) {
			minimum, maximum := 20.0, 10.0
			setting.Minimum, setting.Maximum = &minimum, &maximum
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			if _, err := SchemaOf([]sdk.FeatureSetting{invalid}); err == nil {
				t.Fatalf("accepted malformed setting: %+v", invalid)
			}
		})
	}
	if _, err := SchemaOf([]sdk.FeatureSetting{valid, valid}); err == nil {
		t.Fatal("accepted duplicate setting")
	}
}

func TestFeatureIntegerSettingsKeepExactValues(t *testing.T) {
	setting := sdk.FeatureSetting{
		Name: "limit", Title: "Limit", Kind: sdk.FeatureSettingInteger,
		Default: int64(9007199254740991),
	}
	schema, err := SchemaOf([]sdk.FeatureSetting{setting})
	if err != nil || schema.Properties["limit"].Default != setting.Default {
		t.Fatalf("exact default: %+v, %v", schema, err)
	}
	for _, value := range []any{int64(9007199254740993), int64(-9007199254740993), float64(9007199254740992)} {
		if got, err := schema.Validate(map[string]any{"limit": value}); err == nil {
			t.Errorf("accepted unsafe saved integer %v as %v", value, got)
		}
		invalid := setting
		invalid.Default = value
		if _, err := SchemaOf([]sdk.FeatureSetting{invalid}); err == nil {
			t.Errorf("accepted unsafe default %v", value)
		}
		invalid = setting
		invalid.Enum = []any{setting.Default, value}
		if _, err := SchemaOf([]sdk.FeatureSetting{invalid}); err == nil {
			t.Errorf("accepted unsafe enum %v", value)
		}
	}
}
