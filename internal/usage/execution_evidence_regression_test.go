package usage

import (
	"context"
	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
	"testing"
	"time"
)

func TestAstraContractsUnknownCurrencyLedger(t *testing.T) {
	for _, currency := range []string{"USD", ""} {
		t.Run("currency="+currency, func(t *testing.T) {
			manifest := sdk.Manifest{Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.UsageReporter}}}
			call := sdk.OwnCall{Kind: "title", Model: "model", Status: 200, Usage: sdk.Usage{InputTokens: 10, Cost: 0.12, Currency: currency}}
			if err := sdk.ValidateOwnCall(manifest, call); err != nil {
				t.Fatal("fixture is not a valid SDK own call", err)
			}
			record := extensionCall(sdk.Scope{Plugin: "fixture", Provider: "vendor", Account: "account"}, call)
			record.Time = time.Unix(1800000000, 0)
			serving := &extensionLedgerServing{ctx: context.Background(), queue: make(chan extensionLedgerDelivery, 1)}
			targets := []extensionhost.LedgerTarget{{Module: "observer"}}
			serving.targets.Store(&targets)
			observer := &extensionLedgerObserver{active: serving}
			observer.Ledger(record)
			select {
			case got := <-serving.queue:
				if got.row.Input != 10 {
					t.Fatal(got.row)
				}
				if got.row.Time != record.Time.UnixMilli() || got.row.Status != 200 || got.row.ReportedCost == nil || got.row.ReportedCost.Amount != 0.12 || got.row.ReportedCost.Currency != currency {
					t.Fatal("ledger lost valid time, status or reported cost", got.row)
				}
				if currency == "" && got.row.CostUSD != nil {
					t.Fatal("unspecified currency became USD", got.row)
				}
			default:
				t.Fatalf("valid SDK call vanished from ledger observation: failed=%d validation=%v record=%+v", observer.failed.Load(), sdk.ValidateLedgerObservation(extensionLedgerRow(record)), record)
			}
		})
	}
}

func TestAstraContractsResponseIdentitySettledKey(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	makeChunk := func(id, source, upstream string) *rowChunk {
		c := &rowChunk{}
		c.add(Row{Record: Record{Time: now, Agent: "codex", Model: "model", Session: "session", Input: 80, Output: 8, Status: 200, ResponseID: id, UpstreamResponseID: upstream}, Source: source}, "", 1, false)
		return c
	}
	local := makeChunk("response-a", "log", "")
	first := makeChunk("response-a", "", "upstream-id")
	second := makeChunk("response-b", "", "upstream-id")
	a, _ := codexChunkResponsePairs([]*rowChunk{first}, []*rowChunk{local}, nil)
	b, _ := codexChunkResponsePairs([]*rowChunk{second}, []*rowChunk{local}, nil)
	if len(a) != 1 || len(b) != 0 {
		t.Fatalf("pairing fixture failed: %d %d", len(a), len(b))
	}
	key := func(c *rowChunk) string {
		k, ok := heatSettledKey(heatDiskRef{root: "fixture", meta: "same-prices"}, now.Add(48*time.Hour), now.Add(-48*time.Hour), Filter{}, nil, [3][]*rowChunk{{c}, {local}}, heatSum{})
		if !ok {
			t.Fatal("fixture missing settled key")
		}
		return k
	}
	if key(first) == key(second) {
		t.Fatalf("same settled key despite exact response pairs changing %d -> %d; tagged response id is absent from hashRows", len(a), len(b))
	}
}
