package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/memory"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var rankExtensionMemory = memory.Owner().Rank

func withExtensionMemory(request *http.Request) *http.Request {
	if !extension.LocalOwner(request.Context()) || FromRouter(request.Context()) || request.Header.Get(RouterHeader) != "" {
		return request
	}
	var once sync.Once
	var caller memoryCaller
	ctx := extension.WithToolMemorySearch(request.Context(), func(ctx context.Context, query sdk.MemorySearchRequest) (sdk.MemorySearchResult, error) {
		once.Do(func() { caller = memoryCallerOf(request.WithContext(ctx), nil) })
		return searchExtensionMemory(ctx, caller, query)
	})
	return request.WithContext(ctx)
}

func searchExtensionMemory(ctx context.Context, caller memoryCaller, request sdk.MemorySearchRequest) (sdk.MemorySearchResult, error) {
	if err := ctx.Err(); err != nil {
		return sdk.MemorySearchResult{}, err
	}
	if err := sdk.ValidateMemorySearch(request); err != nil {
		return sdk.MemorySearchResult{}, err
	}
	scopes, err := caller.scopes("all")
	if err != nil || len(scopes) == 0 {
		return sdk.MemorySearchResult{}, errors.New("Memory scope could not be established")
	}
	limit := request.Limit
	if limit == 0 {
		limit = sdk.MaxMemorySearchHits
	}
	hits, err := rankExtensionMemory(memory.Query{Text: request.Query, Scopes: scopes, Limit: limit})
	if err != nil {
		return sdk.MemorySearchResult{}, errors.New("scoped Memory could not be read")
	}
	if err := ctx.Err(); err != nil {
		return sdk.MemorySearchResult{}, err
	}
	result := sdk.MemorySearchResult{Hits: make([]sdk.MemorySearchHit, 0, len(hits))}
	remaining := sdk.MaxMemorySearchBytes
	for _, hit := range hits {
		text := strings.TrimSpace(hit.Text)
		remaining -= len(text) + len(hit.Kind)
		if remaining < 0 {
			break
		}
		result.Hits = append(result.Hits, sdk.MemorySearchHit{Kind: hit.Kind, Text: text})
	}
	return result, nil
}
