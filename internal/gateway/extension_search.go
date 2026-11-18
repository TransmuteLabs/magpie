package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

func (s *Server) askExtensionSearch(ctx context.Context, vendor provider.SearchVendor, selected *provider.Provider, query string) ([]foundPage, error) {
	count := searchCount(ctx)
	result, err := provider.RunExtensionSearch(ctx, vendor, selected, query, count)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("no answer in time")
		}
		return nil, err
	}
	if result.Status != 0 && result.Status != 200 {
		message := fmt.Sprintf("extension search refused (%d)", result.Status)
		if keyRefusal(result.Status) {
			return nil, searchRefused{result.Status, message, result.RetryAfter}
		}
		return nil, errors.New(message)
	}
	pages := make([]plugin.SearchPage, len(result.Pages))
	for index, page := range result.Pages {
		pages[index] = plugin.SearchPage{Title: page.Title, URL: page.URL, Text: page.Text, Age: page.Age}
	}
	return pluginPages(pages, count), nil
}
