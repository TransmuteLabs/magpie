package provider

import (
	"encoding/json"
)

const (
	quotaAmountPresent uint8 = iota + 1
	quotaAmountAbsent
)

func quotaAmount(amount, limit float64, presence uint8) *float64 {
	if presence == quotaAmountAbsent || presence == 0 && amount == 0 && limit <= 0 {
		return nil
	}
	return &amount
}

func (window QuotaWindow) MarshalJSON() ([]byte, error) {
	type plain QuotaWindow
	return json.Marshal(struct {
		plain
		Amount               *float64 `json:"amount,omitempty"`
		BalanceInformational bool     `json:"balanceInformational,omitempty"`
	}{plain: plain(window), Amount: quotaAmount(window.Amount, window.Limit, window.amountPresence),
		BalanceInformational: window.balanceInformational})
}

func (window *QuotaWindow) UnmarshalJSON(raw []byte) error {
	type plain QuotaWindow
	var value plain
	var fields struct {
		Amount               *float64 `json:"amount"`
		BalanceInformational bool     `json:"balanceInformational"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	value.amountPresence = quotaAmountAbsent
	if fields.Amount != nil {
		value.amountPresence = quotaAmountPresent
	}
	// Older cards omit this field: their wallet balance remains authoritative.
	value.balanceInformational = fields.BalanceInformational
	*window = QuotaWindow(value)
	return nil
}

func (window QuotaSpan) MarshalJSON() ([]byte, error) {
	type plain QuotaSpan
	return json.Marshal(struct {
		plain
		Amount *float64 `json:"amount,omitempty"`
	}{plain: plain(window), Amount: quotaAmount(window.Amount, window.Limit, window.amountPresence)})
}

func (window *QuotaSpan) UnmarshalJSON(raw []byte) error {
	type plain QuotaSpan
	var value plain
	var fields struct {
		Amount *float64 `json:"amount"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	value.amountPresence = quotaAmountAbsent
	if fields.Amount != nil {
		value.amountPresence = quotaAmountPresent
	}
	*window = QuotaSpan(value)
	return nil
}
