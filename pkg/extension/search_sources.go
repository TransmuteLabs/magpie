package extension

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxSearchSources = 64
	MaxSearchPages   = 20
	MaxSearchBytes   = 1 << 20
)

type SearchSourceDescriptor struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Action   string `json:"action"`
	Provider string `json:"provider,omitempty"`
}

type SearchRequest struct {
	Source string `json:"source"`
	Query  string `json:"query"`
	Count  int    `json:"count"`
	Proxy  string `json:"proxy,omitempty"`
}

type SearchPage struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
	Age   string `json:"age,omitempty"`
}

type SearchResult struct {
	Pages      []SearchPage `json:"pages"`
	Status     int          `json:"status,omitempty"`
	Error      string       `json:"error,omitempty"`
	RetryAfter string       `json:"retryAfter,omitempty"`
}

func (manifest Manifest) validateSearchSources() error {
	if len(manifest.SearchSources) == 0 {
		return nil
	}
	if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(ModuleRPC) ||
		manifest.Component != nil || len(manifest.SearchSources) > MaxSearchSources {
		return fmt.Errorf("search sources require root SDK API 1.8 module-rpc within descriptor limits")
	}
	seen := make(map[string]bool, len(manifest.SearchSources))
	for _, descriptor := range manifest.SearchSources {
		action, found := manifest.ResolveAction(descriptor.Action)
		if !validID(descriptor.ID) || !validName(descriptor.Name) || !validID(descriptor.Action) ||
			seen[descriptor.ID] || !found || action.Effect != "read" || actionOwner(action) != PrimaryComponent {
			return fmt.Errorf("invalid search source %q or undeclared primary read action", descriptor.ID)
		}
		if descriptor.Provider != "" && (!validID(descriptor.Provider) || !manifest.Contract.Has(AccountAuth) ||
			!manifest.Contract.Has(OutboundExecutor) || !slices.ContainsFunc(manifest.Providers, func(provider ProviderDescriptor) bool {
			return provider.ID == descriptor.Provider
		})) {
			return fmt.Errorf("search source %q requires its declared authenticated provider", descriptor.ID)
		}
		seen[descriptor.ID] = true
	}
	return nil
}

func (request SearchRequest) Validate() error {
	if !validID(request.Source) || !utf8.ValidString(request.Query) || strings.TrimSpace(request.Query) == "" ||
		len(request.Query) > 16<<10 || strings.ContainsRune(request.Query, 0) ||
		request.Count < 1 || request.Count > MaxSearchPages || !searchString(request.Proxy, 4096) {
		return fmt.Errorf("invalid search request bounds")
	}
	return nil
}

func (result SearchResult) Validate(count int) error {
	if count < 1 || count > MaxSearchPages || result.Pages == nil || len(result.Pages) > count ||
		!searchString(result.Error, 1024) || !searchString(result.RetryAfter, 128) {
		return fmt.Errorf("invalid search result bounds")
	}
	if result.Status == 0 || result.Status == 200 {
		if result.Error != "" || result.RetryAfter != "" {
			return fmt.Errorf("successful search cannot contain a refusal")
		}
	} else if result.Status < 400 || result.Status > 599 || strings.TrimSpace(result.Error) == "" || len(result.Pages) != 0 {
		return fmt.Errorf("search refusal requires a status and error without pages")
	}
	for _, page := range result.Pages {
		address, err := url.Parse(page.URL)
		if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Host == "" ||
			address.User != nil || !searchString(page.URL, 4096) || !searchString(page.Title, 2048) ||
			!utf8.ValidString(page.Text) || len(page.Text) > 64<<10 || strings.ContainsRune(page.Text, 0) || !searchString(page.Age, 256) {
			return fmt.Errorf("invalid search page")
		}
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > MaxSearchBytes {
		return fmt.Errorf("search result exceeds its byte budget")
	}
	return nil
}

func searchString(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}
