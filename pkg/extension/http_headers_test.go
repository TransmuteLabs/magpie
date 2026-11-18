package extension

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestModuleHTTPHeadersProveInstanceComponentAndConfigWithoutLeakingState(t *testing.T) {
	if HeaderComponentInstance != "X-Magpie-Component-Instance" ||
		HeaderComponent != "X-Magpie-Component" || HeaderComponentConfig != "X-Magpie-Component-Config" {
		t.Fatal("component HTTP proof header names changed")
	}
	first := ModuleContext{
		Instance: "backend-instance", Component: "backend",
		Config: ConfigSnapshot{Version: "18446744073709551615", Values: json.RawMessage(`{"private":"config"}`)},
		Services: map[string]ComponentReadyResult{"private": {
			Component: "private", Instance: "dependency-instance",
			Endpoints: map[string]string{"http": "http://127.0.0.1:12345/private"},
		}},
		State: make(chan struct{}),
	}
	second := first
	second.Instance, second.Component, second.Config.Version = "other-instance", "other", "0"
	for _, value := range []ModuleContext{first, second} {
		expected := http.Header{
			"X-Magpie-Component-Instance": {value.Instance},
			"X-Magpie-Component":          {value.Component},
			"X-Magpie-Component-Config":   {value.Config.Version},
		}
		actual, independent := value.HTTPHeaders(), value.HTTPHeaders()
		if !reflect.DeepEqual(actual, expected) || !reflect.DeepEqual(independent, expected) {
			t.Fatal("HTTP proof omitted identity or exposed local data:", actual)
		}
		actual[HeaderComponentInstance][0] = "changed"
		actual.Set(HeaderComponentConfig, "changed")
		delete(actual, HeaderComponent)
		if !reflect.DeepEqual(independent, expected) || !reflect.DeepEqual(value.HTTPHeaders(), expected) {
			t.Fatal("HTTP proof maps or header slices were shared between calls")
		}
	}
	if first.HTTPHeaders().Get(HeaderComponentInstance) == second.HTTPHeaders().Get(HeaderComponentInstance) {
		t.Fatal("different component instances received the same HTTP proof")
	}
}
