package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	openRouterCreditsURL = "https://openrouter.ai/api/v1/credits"
	openRouterKeyURL     = "https://openrouter.ai/api/v1/key"
)

type FreeModelDaily struct {
	Day       string `json:"day"`
	Used      int    `json:"used"`
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
}

func openRouterDailyKey(p Provider) bool {
	if p.BalanceURL != "" || p.Key == "" || p.InsecureTLS || len(p.Resolve) != 0 ||
		p.ClientCert != "" || p.ClientKey != "" || p.ServerCA != "" || p.ServerPin != "" {
		return false
	}
	for name := range p.Headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") {
			return false
		}
	}
	found := false
	for _, endpoint := range []string{p.Chat, p.Responses, p.Anthropic, p.Gemini, p.Decide} {
		if endpoint == "" {
			continue
		}
		target, err := url.Parse(endpoint)
		if err != nil || target.User != nil || !SameOrigin("https", "openrouter.ai", target.Scheme, target.Host) {
			return false
		}
		found = true
	}
	source, _ := balanceSourceOf(p)
	return found && source.url == openRouterCreditsURL
}

func readOpenRouterDaily(ctx context.Context, p Provider) *FreeModelDaily {
	ctx, cancel := context.WithTimeout(p.Via(ctx), 2*time.Second)
	defer cancel()
	started := time.Now().UTC().Format(time.DateOnly)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterKeyURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+p.Key)
	req.Header.Set("Accept", "application/json")
	client := *http.DefaultClient
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	const maxBody = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil || len(raw) > maxBody || started != time.Now().UTC().Format(time.DateOnly) {
		return nil
	}
	var reply struct {
		Data struct {
			Daily struct {
				Used      *int `json:"used"`
				Limit     *int `json:"limit"`
				Remaining *int `json:"remaining"`
			} `json:"free_model_daily_requests"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &reply) != nil {
		return nil
	}
	daily := reply.Data.Daily
	if daily.Used == nil || daily.Limit == nil || daily.Remaining == nil ||
		*daily.Used < 0 || *daily.Limit < 0 || *daily.Remaining < 0 {
		return nil
	}
	return &FreeModelDaily{Day: started, Used: *daily.Used, Limit: *daily.Limit, Remaining: *daily.Remaining}
}
