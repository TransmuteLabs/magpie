package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestPreviewKeyCopyOwnsSlotAndSettings(t *testing.T) {
	ref := KeyID("sk-b")
	source := Provider{
		ID: "relay", Key: "sk-a", KeyName: "first",
		Keys:                 []KeyAccount{{Key: "sk-b", Name: "second", Protocol: Chat, Plan: "team", Weight: 3}, {Key: "sk-off", Off: true}},
		AccountModels:        map[string][]string{ref: {"m"}},
		AccountModelsAll:     map[string]bool{ref: true},
		AccountProxies:       map[string]string{ref: "direct"},
		AccountConcurrency:   map[string]int{ref: 4},
		KeySpendCaps:         map[string]SpendCap{ref: {}},
		AccountCaps:          map[string]int{ref: 50},
		AccountWindowCaps:    map[string]map[string]int{ref: {"day": 20}},
		AccountWindowAmounts: map[string]map[string]float64{ref: {"day": 10}},
		KeyOwnBalances:       map[string]OwnBalance{ref: {}},
		KeyPriceRates:        map[string]float64{ref: 1.2},
	}
	before, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	ctx, temporary, now, err := PreviewKey(context.Background(), source, ref, "sk-new")
	if err != nil {
		t.Fatal(err)
	}
	if temporary.Key != source.Key || temporary.Keys[0].Key != "sk-new" || temporary.Keys[0].Name != "second" ||
		temporary.Keys[0].Protocol != Chat || temporary.Keys[0].Plan != "team" || temporary.Keys[0].Weight != 3 || temporary.Keys[0].Off {
		t.Fatal("preview changed another slot or discarded original slot attributes")
	}
	if previewKeyID(ctx, "relay", "sk-new") != ref || previewKeyID(context.Background(), "relay", "sk-new") != now ||
		previewKeyID(ctx, "other", "sk-new") != now || previewKeyID(ctx, "relay", "sk-a") != KeyID("sk-a") {
		t.Fatal("original-slot alias escaped its request, provider or credential")
	}
	for _, name := range []string{"AccountModels", "AccountModelsAll", "AccountProxies", "AccountConcurrency", "KeySpendCaps", "AccountCaps", "AccountWindowCaps", "AccountWindowAmounts", "KeyOwnBalances", "KeyPriceRates"} {
		field := reflect.ValueOf(temporary).FieldByName(name)
		if field.MapIndex(reflect.ValueOf(ref)).IsValid() || !field.MapIndex(reflect.ValueOf(now)).IsValid() {
			t.Errorf("%s did not move only the temporary slot's settings", name)
		}
	}
	temporary.Keys[0].Name = "changed"
	temporary.AccountModels[now][0] = "changed"
	temporary.AccountWindowCaps[now]["day"] = 99
	temporary.AccountWindowAmounts[now]["day"] = 99
	after, err := json.Marshal(source)
	if err != nil || string(before) != string(after) {
		t.Fatal("temporary preview shared mutable settings with the original provider")
	}
}

func TestPreviewKeyAdmissionAndCancellation(t *testing.T) {
	source := Provider{ID: "relay", Key: "sk-a", Keys: []KeyAccount{{Key: "sk-b"}, {Key: "sk-off", Off: true}}}
	for _, row := range []struct {
		name, ref, key string
		refused        bool
	}{
		{"primary", KeyID("sk-a"), "sk-new", false},
		{"secondary", KeyID("sk-b"), "sk-new", false},
		{"same", KeyID("sk-b"), "sk-b", false},
		{"off", KeyID("sk-off"), "sk-new", true},
		{"missing", "missing", "sk-new", true},
		{"empty", KeyID("sk-b"), " ", true},
		{"duplicate", KeyID("sk-b"), "sk-a", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			ctx, _, _, err := PreviewKey(parent, source, row.ref, row.key)
			cancel()
			if (err != nil) != row.refused {
				t.Fatal("preview admission did not preserve key-slot validation")
			}
			if ctx.Err() != context.Canceled {
				t.Fatal("preview alias detached caller cancellation")
			}
		})
	}
	signed := source
	signed.Account = &Account{}
	if _, _, _, err := PreviewKey(context.Background(), signed, KeyID("sk-a"), "sk-new"); err == nil {
		t.Fatal("API-key preview admitted a signed-in account provider")
	}
}
