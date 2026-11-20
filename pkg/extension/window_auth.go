package extension

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

type WindowAuthMethod struct {
	ID     string           `json:"id"`
	Mode   string           `json:"mode"`
	Label  string           `json:"label,omitempty"`
	Fields []AuthInputField `json:"fields,omitempty"`
}

const ProviderSignInPart = "provider:signin"

type AuthInputField struct {
	Key      string            `json:"key"`
	Label    string            `json:"label"`
	Type     string            `json:"type"`
	Optional bool              `json:"optional,omitempty"`
	Choices  []AuthInputChoice `json:"choices,omitempty"`
}

type AuthInputChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type WindowAuthBindingDescriptor struct {
	ID          string             `json:"id"`
	From        string             `json:"from"`
	Provider    string             `json:"provider"`
	Methods     []WindowAuthMethod `json:"methods"`
	Reauthorize bool               `json:"reauthorize,omitempty"`
}

type WindowAuthStartOptions struct {
	Method  string            `json:"method"`
	Input   json.RawMessage   `json:"input,omitempty"`
	Account *AccountSelection `json:"account,omitempty"`
}

type WindowAuthFlowView struct {
	Flow         string          `json:"flow"`
	State        string          `json:"state"`
	URL          string          `json:"url,omitempty"`
	UserCode     string          `json:"userCode,omitempty"`
	RetryAfterMs int64           `json:"retryAfterMs,omitempty"`
	Account      *AccountSummary `json:"account,omitempty"`
}

func (method WindowAuthMethod) Validate() error {
	if !validID(method.ID) || method.Mode != "poll" || method.Label != "" && !validName(method.Label) || len(method.Fields) > 16 {
		return fmt.Errorf("window authentication requires a declared polling method")
	}
	keys := map[string]bool{}
	for _, field := range method.Fields {
		if !providerOptionKey(field.Key) || !validName(field.Label) || keys[field.Key] {
			return fmt.Errorf("invalid or duplicate provider sign-in field")
		}
		keys[field.Key] = true
		switch field.Type {
		case "text", "secret":
			if len(field.Choices) != 0 {
				return fmt.Errorf("only select fields accept choices")
			}
		case "select":
			if len(field.Choices) == 0 || len(field.Choices) > 32 {
				return fmt.Errorf("provider sign-in select requires bounded choices")
			}
			seen := map[string]bool{}
			for _, choice := range field.Choices {
				if !validName(choice.Value) || !validName(choice.Label) || seen[choice.Value] {
					return fmt.Errorf("invalid or duplicate provider sign-in choice")
				}
				seen[choice.Value] = true
			}
		default:
			return fmt.Errorf("unsupported provider sign-in field")
		}
	}
	return nil
}

func (binding WindowAuthBindingDescriptor) Validate() error {
	validPart := binding.From == "window:page" || binding.From == ProviderSignInPart
	if index, found := strings.CutPrefix(binding.From, "window:panel:"); found {
		number, err := strconv.ParseUint(index, 10, 32)
		validPart = err == nil && strconv.FormatUint(number, 10) == index
	}
	if !validID(binding.ID) || !validID(binding.Provider) || !validPart || len(binding.Methods) == 0 || len(binding.Methods) > 16 {
		return fmt.Errorf("invalid window authentication binding")
	}
	seen := map[string]bool{}
	for _, method := range binding.Methods {
		if seen[method.ID] || method.Validate() != nil {
			return fmt.Errorf("invalid or duplicate window authentication method")
		}
		seen[method.ID] = true
	}
	return nil
}

func (options WindowAuthStartOptions) Validate() error {
	if !validID(options.Method) || len(options.Input) > MaxHostBytes ||
		len(options.Input) != 0 && !json.Valid(options.Input) {
		return fmt.Errorf("window authentication requires a declared method and bounded JSON input")
	}
	if options.Account != nil {
		_, err := options.Account.Validate()
		return err
	}
	return nil
}

func (view WindowAuthFlowView) Validate() error {
	if len(view.Flow) != 64 || strings.Trim(view.Flow, "0123456789abcdef") != "" {
		return fmt.Errorf("invalid window authentication flow")
	}
	if view.RetryAfterMs < 0 || view.RetryAfterMs > 300_000 {
		return fmt.Errorf("invalid window authentication retry interval")
	}
	if len(view.UserCode) > 256 || strings.ContainsFunc(view.UserCode, unicode.IsControl) {
		return fmt.Errorf("invalid window authentication user code")
	}
	if view.URL != "" {
		address, err := url.Parse(view.URL)
		if err != nil || len(view.URL) > 8192 || address.Scheme != "https" && address.Scheme != "http" || address.Host == "" ||
			address.User != nil || address.Opaque != "" || strings.ContainsFunc(view.URL, unicode.IsControl) {
			return fmt.Errorf("invalid window authentication verification URL")
		}
	}
	switch view.State {
	case "waiting", "pending":
		if view.Account != nil {
			return fmt.Errorf("unfinished window authentication cannot publish an account")
		}
	case "complete":
		if view.Account == nil || view.URL != "" || view.UserCode != "" || view.RetryAfterMs != 0 {
			return fmt.Errorf("completed window authentication requires only a sanitized account")
		}
	case "canceled", "expired", "uncertain":
		if view.Account != nil || view.URL != "" || view.UserCode != "" || view.RetryAfterMs != 0 {
			return fmt.Errorf("ended window authentication cannot publish authorization data")
		}
	default:
		return fmt.Errorf("invalid window authentication state")
	}
	if view.Account != nil {
		if view.Account.ExpiresAt < 0 {
			return fmt.Errorf("invalid window authentication account expiry")
		}
		_, err := (AccountSelection{ID: view.Account.ID, ExpectedGeneration: view.Account.Generation}).Validate()
		return err
	}
	return nil
}

func (manifest Manifest) ResolveAuthBinding(id string) (WindowAuthBindingDescriptor, bool) {
	for _, binding := range manifest.AuthBindings {
		if binding.ID == id {
			binding.Methods = cloneAuthMethods(binding.Methods)
			return binding, true
		}
	}
	return WindowAuthBindingDescriptor{}, false
}

func cloneAuthMethods(methods []WindowAuthMethod) []WindowAuthMethod {
	methods = slices.Clone(methods)
	for index := range methods {
		fields := slices.Clone(methods[index].Fields)
		for index := range fields {
			fields[index].Choices = slices.Clone(fields[index].Choices)
		}
		methods[index].Fields = fields
	}
	return methods
}

func (manifest Manifest) validateWindowAuthBindings() error {
	if manifest.Contract.Has(ProviderSignIn) && (manifest.Contract.Version.Minor < 5 || !manifest.Contract.Has(AccountAuth)) {
		return fmt.Errorf("provider sign-in requires SDK API 1.5 and account authentication")
	}
	if len(manifest.AuthBindings) == 0 {
		return nil
	}
	if len(manifest.AuthBindings) > 64 || manifest.Contract.Version.Minor < 5 ||
		!manifest.Contract.Has(AccountAuth) || manifest.Component != nil {
		return fmt.Errorf("authentication bindings require SDK API 1.5 and root account authentication")
	}
	seen, providers := map[string]bool{}, map[string]bool{}
	for _, binding := range manifest.AuthBindings {
		if binding.Validate() != nil || seen[binding.ID] ||
			!slices.ContainsFunc(manifest.Providers, func(provider ProviderDescriptor) bool { return provider.ID == binding.Provider }) {
			return fmt.Errorf("invalid or duplicate window authentication binding")
		}
		if binding.From == ProviderSignInPart {
			if !manifest.Contract.Has(ProviderSignIn) || providers[binding.Provider] ||
				slices.ContainsFunc(binding.Methods, func(method WindowAuthMethod) bool { return method.Label == "" }) {
				return fmt.Errorf("provider sign-in requires its capability and one binding per provider with labeled methods")
			}
			providers[binding.Provider] = true
		} else if manifest.Window == nil || slices.ContainsFunc(binding.Methods, func(method WindowAuthMethod) bool {
			return method.Label != "" || len(method.Fields) != 0
		}) {
			return fmt.Errorf("window authentication requires a window and uses its own form")
		}
		seen[binding.ID] = true
	}
	return nil
}
