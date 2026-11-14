package extension

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func searchManifest() Manifest {
	return Manifest{Schema: ManifestSchema, ID: "search.fixture", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract:      Contract{Version: APIVersion{Major: 1, Minor: 8}, Capabilities: []Capability{ModuleRPC}},
		Actions:       []ActionDescriptor{{ID: "search-read", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		SearchSources: []SearchSourceDescriptor{{ID: "specstream", Name: "SpecStream fixture", Action: "search-read"}}}
}

func TestSearchSourceDeclaredReadAuthority(test *testing.T) {
	root := searchManifest()
	root.Compatibility = &Compatibility{HostAPI: "^1.8"}
	if err := root.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*Manifest){
		func(value *Manifest) { value.Contract.Version.Minor = 7 },
		func(value *Manifest) { value.Contract.Capabilities = []Capability{ToolProvider} },
		func(value *Manifest) { value.SearchSources[0].ID = "source/foreign" },
		func(value *Manifest) { value.SearchSources[0].Name = "\n" },
		func(value *Manifest) { value.SearchSources[0].Action = "web_search" },
		func(value *Manifest) { value.Actions[0].Effect = "write" },
		func(value *Manifest) { value.SearchSources[0].Provider = "foreign" },
		func(value *Manifest) { value.SearchSources = append(value.SearchSources, value.SearchSources[0]) },
		func(value *Manifest) { value.SearchSources = make([]SearchSourceDescriptor, MaxSearchSources+1) },
		func(value *Manifest) {
			value.Actions[0].Component = "backend"
			value.Services = []ServiceDescriptor{{ID: "backend", Runtime: "exec", Entry: "worker",
				Contract: ServiceContract{ID: "search.backend", Version: 1}, Methods: []string{"search-read"}, Start: "lazy"}}
		},
	} {
		value := root.Clone()
		mutate(&value)
		if err := value.Validate(); err == nil {
			test.Fatal("invalid search registration acquired ordinary search authority")
		}
	}
	public := root.Public()
	root.SearchSources[0].Name = "changed"
	if public.SearchSources[0].Name != "SpecStream fixture" {
		test.Fatal("search metadata retained mutable root storage")
	}
	root = searchManifest()
	root.Contract.Capabilities = append(root.Contract.Capabilities, AccountAuth, OutboundExecutor)
	root.Providers = []ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{"chat"}}}
	root.SearchSources[0].Provider = "vendor"
	if err := root.Validate(); err != nil {
		test.Fatal("declared account authority rejected:", err)
	}
	root.Services = []ServiceDescriptor{{ID: "backend", Runtime: "exec", Entry: "worker",
		Contract: ServiceContract{ID: "search.backend", Version: 1}, Start: "lazy"}}
	service, err := ServiceManifest(root, "backend")
	if err != nil || len(service.SearchSources) != 0 || len(service.Providers) != 0 || service.Contract.Has(AccountAuth) {
		test.Fatal("service inherited root search or account authority:", err)
	}
}

func TestSearchDTOBounds(test *testing.T) {
	request := SearchRequest{Source: "specstream", Query: "ordinary search", Count: 2}
	if err := request.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*SearchRequest){
		func(value *SearchRequest) { value.Source = "foreign/source" },
		func(value *SearchRequest) { value.Query = " \t" },
		func(value *SearchRequest) { value.Query = strings.Repeat("q", 16385) },
		func(value *SearchRequest) { value.Query = "query\x00secret" },
		func(value *SearchRequest) { value.Count = 0 },
		func(value *SearchRequest) { value.Count = 21 },
		func(value *SearchRequest) { value.Proxy = "direct\n" },
	} {
		value := request
		mutate(&value)
		if value.Validate() == nil {
			test.Fatal("unbounded search request accepted")
		}
	}
	result := SearchResult{Pages: []SearchPage{{Title: "Found", URL: "https://example.invalid/page", Text: "one\npage"}}}
	if err := result.Validate(2); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*SearchResult){
		func(value *SearchResult) { value.Pages = nil },
		func(value *SearchResult) { value.Pages = append(value.Pages, value.Pages[0], value.Pages[0]) },
		func(value *SearchResult) { value.Pages[0].URL = "file:///private/account" },
		func(value *SearchResult) { value.Pages[0].URL = "https://secret:password@example.invalid" },
		func(value *SearchResult) { value.Pages[0].Text = strings.Repeat("x", 65537) },
		func(value *SearchResult) { value.Pages[0].Title = "title\nhidden" },
		func(value *SearchResult) { value.Status = 429; value.Error = "refused" },
		func(value *SearchResult) { value.Error = "false success" },
		func(value *SearchResult) { value.RetryAfter = "30" },
	} {
		value := result
		value.Pages = append([]SearchPage(nil), result.Pages...)
		mutate(&value)
		if value.Validate(2) == nil {
			test.Fatal("invalid search result accepted")
		}
	}
	if err := (SearchResult{Pages: []SearchPage{}, Status: 429, Error: "rate limited", RetryAfter: "30"}).Validate(2); err != nil {
		test.Fatal("bounded refusal rejected:", err)
	}
}
