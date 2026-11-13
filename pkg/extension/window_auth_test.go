package extension

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWindowAuthPublicViewHasNoCredentialFields(t *testing.T) {
	view := WindowAuthFlowView{
		Flow: strings.Repeat("a", 64), State: "complete",
		Account: &AccountSummary{ID: "chosen", Generation: "9007199254740993"},
	}
	if err := view.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || len(fields["flow"]) == 0 || len(fields["state"]) == 0 || len(fields["account"]) == 0 {
		t.Fatalf("unexpected public authentication fields: %s", body)
	}
	for _, forbidden := range []string{"credentials", "continuation", "auth", "input", "provider", "redirectUri"} {
		if _, found := fields[forbidden]; found {
			t.Fatalf("authentication result exposed %s", forbidden)
		}
	}
}

func TestWindowAuthContractRejectsUnpublishedAuthority(t *testing.T) {
	valid := WindowAuthFlowView{Flow: strings.Repeat("a", 64), State: "waiting",
		URL: "https://login.example/verify?state=public", UserCode: "TEST-CODE", RetryAfterMs: 1000}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*WindowAuthFlowView){
		"short flow":       func(view *WindowAuthFlowView) { view.Flow = "guess" },
		"negative retry":   func(view *WindowAuthFlowView) { view.RetryAfterMs = -1 },
		"large retry":      func(view *WindowAuthFlowView) { view.RetryAfterMs = 300_001 },
		"script URL":       func(view *WindowAuthFlowView) { view.URL = "javascript:alert(1)" },
		"relative URL":     func(view *WindowAuthFlowView) { view.URL = "/verify" },
		"userinfo URL":     func(view *WindowAuthFlowView) { view.URL = "https://private@login.example" },
		"usercode control": func(view *WindowAuthFlowView) { view.UserCode = "code\nsecret" },
		"unknown state":    func(view *WindowAuthFlowView) { view.State = "almost-complete" },
		"early account": func(view *WindowAuthFlowView) {
			view.Account = &AccountSummary{ID: "chosen", Generation: "1"}
		},
		"terminal URL": func(view *WindowAuthFlowView) { view.State = "expired" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			view := valid
			change(&view)
			if err := view.Validate(); err == nil {
				t.Fatalf("invalid authentication view accepted: %+v", view)
			}
		})
	}
}

func TestWindowAuthTransportModeIsNotMethodName(t *testing.T) {
	for _, id := range []string{"browser", "device", "vendor-method"} {
		if err := (WindowAuthMethod{ID: id, Mode: "poll"}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"", "callback", "shell", "browser"} {
		if err := (WindowAuthMethod{ID: "browser", Mode: mode}).Validate(); err == nil {
			t.Fatalf("unimplemented mode accepted: %q", mode)
		}
	}
	if err := (WindowAuthStartOptions{Method: "device", Input: json.RawMessage(`{"site":"fixture"}`),
		Account: &AccountSelection{ID: "chosen", ExpectedGeneration: "18446744073709551615"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, options := range []WindowAuthStartOptions{
		{Method: "not a method"},
		{Method: "device", Input: json.RawMessage(`{"broken"`)},
		{Method: "device", Account: &AccountSelection{ID: "chosen", ExpectedGeneration: "01"}},
	} {
		if err := options.Validate(); err == nil {
			t.Fatalf("invalid start options accepted: %+v", options)
		}
	}
}

func TestWindowAuthBindingsDeclareProviderMethodAndExactPart(t *testing.T) {
	source := accountBindingManifest()
	source.AuthBindings = []WindowAuthBindingDescriptor{{
		ID: "signin", From: "window:page", Provider: "vendor",
		Methods: []WindowAuthMethod{{ID: "browser", Mode: "poll"}},
	}}
	if err := source.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Manifest){
		func(value *Manifest) { value.Contract.Version.Minor = 4 },
		func(value *Manifest) { value.AuthBindings[0].Provider = "foreign" },
		func(value *Manifest) { value.AuthBindings[0].From = "primary" },
		func(value *Manifest) { value.AuthBindings[0].Methods[0].Mode = "callback" },
		func(value *Manifest) { value.AuthBindings[0].Methods = nil },
		func(value *Manifest) {
			value.AuthBindings[0].Methods = append(value.AuthBindings[0].Methods, value.AuthBindings[0].Methods[0])
		},
		func(value *Manifest) { value.AuthBindings = append(value.AuthBindings, value.AuthBindings[0]) },
		func(value *Manifest) { value.Window = nil },
		func(value *Manifest) { value.Contract.Capabilities = []Capability{ModuleRPC, ModelDiscovery} },
	} {
		value := source.Clone()
		mutate(&value)
		if err := value.Validate(); err == nil {
			t.Fatal("invalid authentication grant accepted")
		}
	}
	clone := source.Clone()
	clone.AuthBindings[0].Methods[0].ID = "changed"
	resolved, found := source.ResolveAuthBinding("signin")
	if !found || resolved.Methods[0].ID != "browser" {
		t.Fatal("cloning changed installed authentication authority")
	}
	resolved.Methods[0].ID = "changed"
	if source.AuthBindings[0].Methods[0].ID != "browser" {
		t.Fatal("resolved authentication binding changed installed authority")
	}
	if source.Public().AuthBindings != nil {
		t.Fatal("worker handshake retained root window authority")
	}
}
