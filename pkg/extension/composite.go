package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	ModuleRPC Capability = "module-rpc"

	MaxModuleServices = 16

	HeaderComponentInstance = "X-Magpie-Component-Instance"
	HeaderComponent         = "X-Magpie-Component"
	HeaderComponentConfig   = "X-Magpie-Component-Config"

	MethodModuleCall         = "module.call"
	MethodComponentConfigure = "component.configure"
	MethodComponentReady     = "component.ready"
	MethodComponentStop      = "component.stop"
	MethodComponentCall      = "component.call"
	PrimaryComponent         = "primary"
)

type ActionDescriptor struct {
	ID          string          `json:"id"`
	Component   string          `json:"component,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Effect      string          `json:"effect"`
}

type ServiceContract struct {
	ID      string `json:"id"`
	Version uint32 `json:"version"`
}

type ServiceReady struct {
	TimeoutMs     int      `json:"timeoutMs,omitempty"`
	HTTPEndpoints []string `json:"httpEndpoints,omitempty"`
}

type ServiceDescriptor struct {
	ID        string          `json:"id"`
	Runtime   string          `json:"runtime"`
	Entry     string          `json:"entry"`
	Args      []string        `json:"args,omitempty"`
	Contract  ServiceContract `json:"contract"`
	Methods   []string        `json:"methods"`
	Start     string          `json:"start"`
	DependsOn []string        `json:"dependsOn,omitempty"`
	Ready     ServiceReady    `json:"ready,omitempty"`
}

type Compatibility struct {
	HostAPI   string   `json:"hostAPI"`
	NativeABI uint32   `json:"nativeABI,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
}

type WindowDescriptor struct {
	Metadata string `json:"metadata"`
}

type BindingDescriptor struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Action   string `json:"action,omitempty"`
	Service  string `json:"service,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

type ComponentIdentity struct {
	ID       string          `json:"id"`
	Contract ServiceContract `json:"contract"`
}

type ModuleCallRequest struct {
	Action string          `json:"action"`
	Input  json.RawMessage `json:"input"`
}

type ModuleCallResult = json.RawMessage

type ComponentCallRequest struct {
	Binding string          `json:"binding"`
	Input   json.RawMessage `json:"input"`
}

type ConfigSnapshot struct {
	Version string          `json:"version"`
	Values  json.RawMessage `json:"values"`
}

type ComponentConfigureRequest struct {
	Component string                          `json:"component"`
	Contract  ServiceContract                 `json:"contract"`
	Instance  string                          `json:"instance"`
	Config    ConfigSnapshot                  `json:"config"`
	Services  map[string]ComponentReadyResult `json:"services,omitempty"`
}

type ComponentReadyResult struct {
	Component     string            `json:"component"`
	Contract      ServiceContract   `json:"contract"`
	Instance      string            `json:"instance"`
	ConfigVersion string            `json:"configVersion"`
	Endpoints     map[string]string `json:"endpoints"`
}

type ModuleContext struct {
	Config    ConfigSnapshot
	Component string
	Instance  string
	Services  map[string]ComponentReadyResult
	State     any `json:"-"`
}

// HTTPHeaders returns fresh identity headers for this component's local HTTP
// responses. Configuration values, dependency records and State stay local.
func (value ModuleContext) HTTPHeaders() http.Header {
	return http.Header{
		HeaderComponentInstance: {value.Instance},
		HeaderComponent:         {value.Component},
		HeaderComponentConfig:   {value.Config.Version},
	}
}

type ModuleReady struct {
	Endpoints map[string]string
	State     any `json:"-"`
}

type StartFunc func(context.Context, ModuleContext) (ModuleReady, func(), error)

func componentID(manifest Manifest) string {
	if manifest.Component != nil {
		return manifest.Component.ID
	}
	return PrimaryComponent
}

func actionOwner(action ActionDescriptor) string {
	if action.Component == "" {
		return PrimaryComponent
	}
	return action.Component
}

func (manifest Manifest) ResolveAction(id string) (ActionDescriptor, bool) {
	for _, action := range manifest.Actions {
		if action.ID == id {
			action.InputSchema = slices.Clone(action.InputSchema)
			return action, true
		}
	}
	return ActionDescriptor{}, false
}

func (manifest Manifest) ResolveBinding(id string) (BindingDescriptor, bool) {
	for _, binding := range manifest.Bindings {
		if binding.ID == id {
			return binding, true
		}
	}
	return BindingDescriptor{}, false
}

func ServiceManifest(root Manifest, id string) (Manifest, error) {
	if err := root.Validate(); err != nil {
		return Manifest{}, err
	}
	if root.Component != nil {
		return Manifest{}, fmt.Errorf("a service manifest requires the root module")
	}
	for _, service := range root.Services {
		if service.ID != id {
			continue
		}
		view := Manifest{
			Schema: root.Schema, ID: root.ID, Version: root.Version,
			Runtime: service.Runtime, Entry: service.Entry, Args: slices.Clone(service.Args),
			Contract:  Contract{Version: APIVersion{Major: 1, Minor: 1}, Capabilities: []Capability{ModuleRPC}},
			Component: &ComponentIdentity{ID: id, Contract: service.Contract},
		}
		for _, action := range root.Actions {
			if actionOwner(action) == id {
				action.InputSchema = slices.Clone(action.InputSchema)
				view.Actions = append(view.Actions, action)
			}
		}
		return view, nil
	}
	return Manifest{}, fmt.Errorf("unknown module service %q", id)
}

func validPackagePath(value string) bool {
	return value != "" && len(value) <= 1024 && value != "." && value != ".." &&
		!strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "../") &&
		path.Clean(value) == value && !strings.ContainsAny(value, "\\:\x00?#%") &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func validServiceContract(contract ServiceContract) bool {
	return validID(contract.ID) && contract.Version != 0
}

func (manifest Manifest) validateComposite() error {
	composite := manifest.Contract.Has(ModuleRPC) || manifest.Actions != nil || manifest.Services != nil ||
		manifest.Compatibility != nil || manifest.Window != nil || manifest.Bindings != nil || manifest.Component != nil
	if !composite {
		return nil
	}
	if manifest.Contract.Version.Minor < 1 {
		return fmt.Errorf("module composition requires SDK API 1.1")
	}
	if len(manifest.Services) > MaxModuleServices {
		return fmt.Errorf("module declares %d services; maximum is %d", len(manifest.Services), MaxModuleServices)
	}
	if len(manifest.Actions)+len(manifest.Services)+len(manifest.Bindings) > 1024 {
		return fmt.Errorf("too many module composition descriptors")
	}
	own := componentID(manifest)
	if manifest.Component != nil {
		if !validID(own) || own == PrimaryComponent || !validServiceContract(manifest.Component.Contract) ||
			len(manifest.Services) != 0 || manifest.Window != nil || len(manifest.Bindings) != 0 || manifest.Compatibility != nil ||
			len(manifest.Contract.Capabilities) != 1 || !manifest.Contract.Has(ModuleRPC) {
			return fmt.Errorf("invalid service component identity or delegated capability")
		}
	}
	services := make(map[string]ServiceDescriptor, len(manifest.Services))
	for _, service := range manifest.Services {
		if !validID(service.ID) || service.ID == PrimaryComponent || services[service.ID].ID != "" ||
			(service.Runtime != "exec" && service.Runtime != "bun") || !validPackagePath(service.Entry) ||
			!validServiceContract(service.Contract) || (service.Start != "lazy" && service.Start != "eager") ||
			len(service.Args) > 128 || len(service.DependsOn) > 128 || len(service.Methods) > 1024 ||
			service.Ready.TimeoutMs < 0 || service.Ready.TimeoutMs > 300_000 || len(service.Ready.HTTPEndpoints) > 128 {
			return fmt.Errorf("invalid or duplicate module service %q", service.ID)
		}
		for _, arg := range service.Args {
			if len(arg) > MaxChunkBytes || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("invalid module service argument")
			}
		}
		methods := map[string]bool{}
		for _, method := range service.Methods {
			if !validID(method) || methods[method] {
				return fmt.Errorf("invalid or duplicate service action method")
			}
			methods[method] = true
		}
		endpoints := map[string]bool{}
		for _, endpoint := range service.Ready.HTTPEndpoints {
			if !validID(endpoint) || endpoints[endpoint] {
				return fmt.Errorf("invalid or duplicate service HTTP endpoint")
			}
			endpoints[endpoint] = true
		}
		services[service.ID] = service
	}
	colors := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if colors[id] == 1 {
			return fmt.Errorf("module service dependency cycle")
		}
		if colors[id] == 2 {
			return nil
		}
		colors[id] = 1
		seen := map[string]bool{}
		for _, dependency := range services[id].DependsOn {
			if services[dependency].ID == "" || dependency == id || seen[dependency] {
				return fmt.Errorf("unknown, duplicate or self service dependency")
			}
			seen[dependency] = true
			if err := visit(dependency); err != nil {
				return err
			}
		}
		colors[id] = 2
		return nil
	}
	for id := range services {
		if err := visit(id); err != nil {
			return err
		}
	}
	actions := map[string]bool{}
	for _, action := range manifest.Actions {
		owner := actionOwner(action)
		var schema any
		if !manifest.Contract.Has(ModuleRPC) || !validID(action.ID) || actions[action.ID] ||
			(action.Effect != "read" && action.Effect != "write") || len(action.InputSchema) > MaxChunkBytes ||
			json.Unmarshal(action.InputSchema, &schema) != nil {
			return fmt.Errorf("invalid or duplicate module action %q", action.ID)
		}
		switch schema.(type) {
		case map[string]any, bool:
		default:
			return fmt.Errorf("action input schema must be a JSON schema object or boolean")
		}
		if manifest.Component != nil {
			if owner != own {
				return fmt.Errorf("action belongs to another service component")
			}
		} else if owner != PrimaryComponent {
			service, found := services[owner]
			if !found || !slices.Contains(service.Methods, action.ID) {
				return fmt.Errorf("action owner requires a service declaring its action method")
			}
		}
		actions[action.ID] = true
	}
	for _, service := range manifest.Services {
		for _, method := range service.Methods {
			action, found := manifest.ResolveAction(method)
			if !found || actionOwner(action) != service.ID {
				return fmt.Errorf("service method does not name its own declared action")
			}
		}
	}
	if manifest.Window != nil && (!validPackagePath(manifest.Window.Metadata) || path.Base(manifest.Window.Metadata) != "package.json") {
		return fmt.Errorf("window metadata must name a relative package.json")
	}
	bindings := map[string]bool{}
	for _, binding := range manifest.Bindings {
		if !validID(binding.ID) || bindings[binding.ID] || !validBindingFrom(binding.From, manifest.Window != nil) {
			return fmt.Errorf("invalid or duplicate module binding %q", binding.ID)
		}
		if binding.Action != "" {
			if !actions[binding.Action] || binding.Service != "" || binding.Endpoint != "" {
				return fmt.Errorf("binding requires one declared action or service endpoint")
			}
		} else if service, found := services[binding.Service]; !found || !slices.Contains(service.Ready.HTTPEndpoints, binding.Endpoint) {
			return fmt.Errorf("binding references an undeclared service endpoint")
		}
		bindings[binding.ID] = true
	}
	if compatibility := manifest.Compatibility; compatibility != nil {
		if compatibility.HostAPI != "^1" && compatibility.HostAPI != "^1.1" && compatibility.HostAPI != "^1.2" ||
			compatibility.HostAPI == "^1.2" && manifest.Contract.Version.Minor < 2 {
			return fmt.Errorf("unsupported host API requirement %q", compatibility.HostAPI)
		}
		if len(compatibility.Platforms) > 128 {
			return fmt.Errorf("too many compatibility platforms")
		}
		platforms := map[string]bool{}
		for _, platform := range compatibility.Platforms {
			if !validPlatform(platform) || platforms[platform] {
				return fmt.Errorf("invalid or duplicate compatibility platform %q", platform)
			}
			platforms[platform] = true
		}
	}
	return nil
}

func validBindingFrom(value string, window bool) bool {
	if value == PrimaryComponent {
		return true
	}
	if !window {
		return false
	}
	for _, kind := range []string{"window:page", "window:panel"} {
		if value == kind || strings.HasPrefix(value, kind+":") && validID(strings.TrimPrefix(value, kind+":")) {
			return true
		}
	}
	return false
}

func validPlatform(value string) bool {
	os, arch, found := strings.Cut(value, "/")
	if !found || !validID(os) || !validID(arch) {
		return false
	}
	architectures := map[string]string{
		"aix": "ppc64", "android": "386 amd64 arm arm64", "darwin": "amd64 arm64", "dragonfly": "amd64",
		"freebsd": "386 amd64 arm arm64 riscv64", "illumos": "amd64", "ios": "amd64 arm64", "js": "wasm",
		"linux":  "386 amd64 arm arm64 loong64 mips mipsle mips64 mips64le ppc64 ppc64le riscv64 s390x",
		"netbsd": "386 amd64 arm arm64", "openbsd": "386 amd64 arm arm64 ppc64 riscv64",
		"plan9": "386 amd64 arm", "solaris": "amd64", "wasip1": "wasm", "windows": "386 amd64 arm64",
	}
	return architectures[os] != "" && strings.Contains(" "+architectures[os]+" ", " "+arch+" ")
}

// CheckCompatibility checks declarations against the running SDK host.
func CheckCompatibility(manifest Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if compatibility := manifest.Compatibility; compatibility != nil {
		if compatibility.NativeABI != 0 && compatibility.NativeABI != 1 {
			return fmt.Errorf("module requires native ABI %d; host supports 1", compatibility.NativeABI)
		}
		platform := runtime.GOOS + "/" + runtime.GOARCH
		if len(compatibility.Platforms) != 0 && !slices.Contains(compatibility.Platforms, platform) {
			return fmt.Errorf("module does not support host platform %s", platform)
		}
	}
	return nil
}

func validReadyURL(value string) bool {
	if len(value) > 4096 || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	address, err := url.Parse(value)
	if err != nil {
		return false
	}
	port, err := strconv.ParseUint(address.Port(), 10, 16)
	host := address.Hostname()
	ip := net.ParseIP(host)
	return err == nil && port != 0 && (address.Scheme == "http" || address.Scheme == "https") &&
		address.Host != "" && address.User == nil && address.Fragment == "" && address.Port() != "" &&
		(host == "localhost" || ip != nil && ip.IsLoopback())
}
