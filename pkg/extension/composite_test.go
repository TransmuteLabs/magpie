package extension

import (
	"encoding/json"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func compositeManifest() Manifest {
	return Manifest{
		Schema: 1, ID: "fixture", Version: "1.0.0", Runtime: "native", Entry: "lib/primary.dylib",
		Contract:      Contract{Version: APIVersion{Major: 1, Minor: 1}, Capabilities: []Capability{ModuleRPC}},
		Compatibility: &Compatibility{HostAPI: "^1.1", NativeABI: 1, Platforms: []string{runtime.GOOS + "/" + runtime.GOARCH}},
		Actions: []ActionDescriptor{
			{ID: "refresh", InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "read"},
			{ID: "backend.run", Component: "backend", InputSchema: json.RawMessage(`true`), Effect: "write"},
		},
		Services: []ServiceDescriptor{{
			ID: "backend", Runtime: "exec", Entry: "bin/backend", Args: []string{"--serve"},
			Contract: ServiceContract{ID: "our-backend", Version: 1}, Methods: []string{"backend.run"}, Start: "lazy",
			Ready: ServiceReady{TimeoutMs: 10_000, HTTPEndpoints: []string{"ui"}},
		}},
		Window: &WindowDescriptor{Metadata: "package.json"},
		Bindings: []BindingDescriptor{
			{ID: "page.refresh", From: "window:page", Action: "refresh"},
			{ID: "controller.run", From: "primary", Action: "backend.run"},
			{ID: "page.service", From: "window:page", Service: "backend", Endpoint: "ui"},
		},
	}
}

func TestCompositeManifestPreservesLegacyAndNegotiatesAPI11(t *testing.T) {
	legacy := completeManifest()
	if err := legacy.Validate(); err != nil || legacy.Contract.Version.Minor != 0 {
		t.Fatal("the nine legacy capability paths changed:", err)
	}
	current := compositeManifest()
	if err := current.Validate(); err != nil {
		t.Fatal(err)
	}
	if capability, ok := MethodCapability(MethodModuleCall); !ok || capability != ModuleRPC {
		t.Fatal("module.call lost its negotiated capability")
	}
	for _, control := range []string{MethodDescribe, MethodComponentConfigure, MethodComponentReady, MethodComponentStop, MethodComponentCall} {
		if _, found := MethodCapability(control); found {
			t.Fatal("SDK or host control acquired a worker capability:", control)
		}
	}
	for _, add := range []func(*Manifest){
		func(m *Manifest) { m.Contract.Capabilities = append(m.Contract.Capabilities, ModuleRPC) },
		func(m *Manifest) { m.Compatibility = current.Compatibility },
		func(m *Manifest) { m.Window = current.Window },
		func(m *Manifest) { m.Services = current.Services },
		func(m *Manifest) { m.Actions = current.Actions },
		func(m *Manifest) { m.Bindings = current.Bindings },
	} {
		manifest := legacy.Clone()
		add(&manifest)
		if err := manifest.Validate(); err == nil {
			t.Fatal("API 1.0 accepted an API 1.1 addition")
		}
	}
	wire, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"hostAPI"`, `"nativeABI"`, `"inputSchema"`, `"dependsOn"`, `"httpEndpoints"`, `"timeoutMs"`} {
		if field == `"dependsOn"` {
			continue
		}
		if !strings.Contains(string(wire), field) {
			t.Fatal("ADR field spelling changed:", field)
		}
	}
}

func TestCompositeManifestRejectsAmbiguousOwnershipAndMalformedMetadata(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Manifest)
	}{
		{"missing capability", func(m *Manifest) { m.Contract.Capabilities = []Capability{OperationControl} }},
		{"action id", func(m *Manifest) { m.Actions[0].ID = "../bad" }},
		{"duplicate action", func(m *Manifest) { m.Actions = append(m.Actions, m.Actions[0]) }},
		{"effect", func(m *Manifest) { m.Actions[0].Effect = "stream" }},
		{"schema scalar", func(m *Manifest) { m.Actions[0].InputSchema = json.RawMessage(`42`) }},
		{"schema oversized", func(m *Manifest) { m.Actions[0].InputSchema = json.RawMessage(strings.Repeat(" ", MaxChunkBytes+1)) }},
		{"unknown owner", func(m *Manifest) { m.Actions[0].Component = "missing" }},
		{"service omits action", func(m *Manifest) { m.Services[0].Methods = nil }},
		{"service claims primary action", func(m *Manifest) { m.Services[0].Methods = append(m.Services[0].Methods, "refresh") }},
		{"service claims legacy method", func(m *Manifest) { m.Services[0].Methods = append(m.Services[0].Methods, MethodProviderExecute) }},
		{"service duplicate method", func(m *Manifest) { m.Services[0].Methods = append(m.Services[0].Methods, "backend.run") }},
		{"service id", func(m *Manifest) { m.Services[0].ID = "primary" }},
		{"duplicate service", func(m *Manifest) { m.Services = append(m.Services, m.Services[0]) }},
		{"service native", func(m *Manifest) { m.Services[0].Runtime = "native" }},
		{"service traversal", func(m *Manifest) { m.Services[0].Entry = "../backend" }},
		{"service absolute", func(m *Manifest) { m.Services[0].Entry = "/backend" }},
		{"service windows", func(m *Manifest) { m.Services[0].Entry = `C:\backend.exe` }},
		{"service argument", func(m *Manifest) { m.Services[0].Args = []string{"bad\x00arg"} }},
		{"service contract id", func(m *Manifest) { m.Services[0].Contract.ID = "" }},
		{"service contract version", func(m *Manifest) { m.Services[0].Contract.Version = 0 }},
		{"startup policy", func(m *Manifest) { m.Services[0].Start = "background" }},
		{"negative timeout", func(m *Manifest) { m.Services[0].Ready.TimeoutMs = -1 }},
		{"oversized timeout", func(m *Manifest) { m.Services[0].Ready.TimeoutMs = 300_001 }},
		{"duplicate endpoint", func(m *Manifest) { m.Services[0].Ready.HTTPEndpoints = []string{"ui", "ui"} }},
		{"endpoint URL", func(m *Manifest) { m.Services[0].Ready.HTTPEndpoints = []string{"http://localhost"} }},
		{"unknown dependency", func(m *Manifest) { m.Services[0].DependsOn = []string{"missing"} }},
		{"self dependency", func(m *Manifest) { m.Services[0].DependsOn = []string{"backend"} }},
		{"missing window", func(m *Manifest) { m.Window = nil }},
		{"window traversal", func(m *Manifest) { m.Window.Metadata = "../package.json" }},
		{"window wrong file", func(m *Manifest) { m.Window.Metadata = "ui.json" }},
		{"binding id", func(m *Manifest) { m.Bindings[0].ID = "" }},
		{"binding duplicate", func(m *Manifest) { m.Bindings = append(m.Bindings, m.Bindings[0]) }},
		{"binding source", func(m *Manifest) { m.Bindings[0].From = "backend" }},
		{"binding action unknown", func(m *Manifest) { m.Bindings[0].Action = "unknown" }},
		{"binding two targets", func(m *Manifest) { m.Bindings[0].Service, m.Bindings[0].Endpoint = "backend", "ui" }},
		{"binding endpoint unknown", func(m *Manifest) { m.Bindings[2].Endpoint = "other" }},
		{"binding service unknown", func(m *Manifest) { m.Bindings[2].Service = "other" }},
		{"compatibility range", func(m *Manifest) { m.Compatibility.HostAPI = "^2" }},
		{"compatibility platform", func(m *Manifest) { m.Compatibility.Platforms = []string{"darwin-arm64"} }},
		{"compatibility pair", func(m *Manifest) { m.Compatibility.Platforms = []string{"windows/mips"} }},
		{"duplicate platform", func(m *Manifest) { m.Compatibility.Platforms = []string{"linux/amd64", "linux/amd64"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := compositeManifest()
			test.edit(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("malformed composite descriptor accepted")
			}
		})
	}
}

func TestCompositeServiceDAGAndCompatibility(t *testing.T) {
	manifest := compositeManifest()
	manifest.Services = append(manifest.Services, ServiceDescriptor{
		ID: "storage", Runtime: "bun", Entry: "services/storage.ts", Start: "eager",
		Contract: ServiceContract{ID: "storage", Version: 2},
	})
	manifest.Services[0].DependsOn = []string{"storage"}
	if err := manifest.Validate(); err != nil {
		t.Fatal("valid dependency graph rejected:", err)
	}
	manifest.Services[0].DependsOn = []string{"storage", "storage"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("duplicate dependency accepted")
	}
	manifest.Services[0].DependsOn = []string{"storage"}
	manifest.Services[1].DependsOn = []string{"backend"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("dependency cycle accepted")
	}
	manifest = compositeManifest()
	if err := CheckCompatibility(manifest); err != nil {
		t.Fatal("current platform rejected:", err)
	}
	manifest.Compatibility.NativeABI = 2
	if err := CheckCompatibility(manifest); err == nil {
		t.Fatal("unsupported native ABI accepted")
	}
	manifest.Compatibility.NativeABI = 1
	manifest.Compatibility.Platforms = []string{"js/wasm"}
	if err := CheckCompatibility(manifest); err == nil && runtime.GOOS != "js" {
		t.Fatal("foreign platform accepted")
	}
}

func TestCompositeManifestBoundsServiceInstancesSeparatelyFromMetadata(t *testing.T) {
	for _, count := range []int{16, 17} {
		t.Run(strconv.Itoa(count)+" eager services sharing one binary", func(t *testing.T) {
			manifest := compositeManifest()
			manifest.Services[0].Start = "eager"
			for index := 1; index < count; index++ {
				manifest.Services = append(manifest.Services, ServiceDescriptor{
					ID: "service-" + strconv.Itoa(index), Runtime: "exec", Entry: manifest.Services[0].Entry,
					Contract: ServiceContract{ID: "shared-backend", Version: 1}, Start: "eager",
				})
			}
			err := manifest.Validate()
			if count == 16 && err != nil {
				t.Fatal("sixteen service instances rejected:", err)
			}
			if count == 17 && err == nil {
				t.Fatal("seventeen eager service instances sharing one binary accepted")
			}
		})
	}
	t.Run("metadata retains 1024 descriptor budget", func(t *testing.T) {
		manifest := testManifest(ModuleRPC)
		manifest.Contract.Version.Minor = 1
		manifest.Actions = []ActionDescriptor{{ID: "read", Effect: "read", InputSchema: json.RawMessage(`true`)}}
		for index := range 1023 {
			manifest.Bindings = append(manifest.Bindings, BindingDescriptor{
				ID: "binding-" + strconv.Itoa(index), From: PrimaryComponent, Action: "read",
			})
		}
		if err := manifest.Validate(); err != nil {
			t.Fatal("1024 metadata descriptors rejected by the service instance budget:", err)
		}
		manifest.Bindings = append(manifest.Bindings, BindingDescriptor{ID: "overflow", From: PrimaryComponent, Action: "read"})
		if err := manifest.Validate(); err == nil {
			t.Fatal("1025 metadata descriptors exceeded their original budget")
		}
	})
	if MaxModuleServices != 16 {
		t.Fatal("module service execution budget changed:", MaxModuleServices)
	}
}

func TestCompositeProjectionsKeepRootAuthorityAndFreezeNestedFields(t *testing.T) {
	root := compositeManifest()
	root.Services[0].DependsOn = []string{}
	before := root.Clone()
	public := root.Public()
	if public.Runtime != "" || public.Entry != "" || public.Args != nil || len(public.Services) != 0 ||
		public.Window != nil || public.Compatibility != nil || len(public.Bindings) != 0 ||
		len(public.Actions) != 1 || public.Actions[0].ID != "refresh" {
		t.Fatalf("primary handshake disclosed hidden components: %+v", public)
	}
	service, err := ServiceManifest(root, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Validate(); err != nil {
		t.Fatal("derived service manifest is invalid:", err)
	}
	if service.ID != root.ID || service.Version != root.Version || service.Runtime != "exec" ||
		service.Entry != "bin/backend" || len(service.Contract.Capabilities) != 1 ||
		service.Contract.Capabilities[0] != ModuleRPC || service.Component.ID != "backend" ||
		len(service.Actions) != 1 || service.Actions[0].ID != "backend.run" ||
		service.Component.Contract != root.Services[0].Contract {
		t.Fatalf("service projection changed its authority: %+v", service)
	}
	if len(service.Public().Actions) != 1 || service.Public().Component == nil {
		t.Fatal("service handshake lost component identity or owned actions")
	}
	if _, err := ServiceManifest(root, "unknown"); err == nil {
		t.Fatal("unknown service projected")
	}
	if !reflect.DeepEqual(root, before) {
		t.Fatal("projection modified root authority")
	}
	clone := root.Clone()
	clone.Services[0].Args[0] = "--changed"
	clone.Services[0].Methods[0] = "changed"
	clone.Services[0].Ready.HTTPEndpoints[0] = "changed"
	clone.Compatibility.Platforms[0] = "changed"
	clone.Window.Metadata = "changed"
	clone.Bindings[0].Action = "changed"
	clone.Actions[0].InputSchema[0] = '!'
	action, ok := root.ResolveAction("refresh")
	if !ok {
		t.Fatal("declared action not resolved")
	}
	action.InputSchema[0] = '!'
	if binding, ok := root.ResolveBinding("controller.run"); !ok || binding.Action != "backend.run" {
		t.Fatal("binding did not resolve from full root")
	}
	if _, ok := public.ResolveBinding("controller.run"); ok {
		t.Fatal("public projection retained a hidden binding")
	}
	if !reflect.DeepEqual(root, before) {
		t.Fatal("nested projection or resolution retained mutable fields")
	}
	service.Contract.Capabilities = append(service.Contract.Capabilities, OperationControl)
	if err := service.Validate(); err == nil {
		t.Fatal("helper delegated one of the nine primary capability paths")
	}
}
