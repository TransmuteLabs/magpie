package usage

import (
	"encoding/json"
	"math"
	"strings"
)

type ReportedCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency,omitempty"`
}

func reportedCostText(cost *ReportedCost) string {
	if cost == nil {
		return ""
	}
	encoded, err := json.Marshal(cost)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func reportedCostFromText(encoded string) *ReportedCost {
	if encoded == "" {
		return nil
	}
	var cost ReportedCost
	if json.Unmarshal([]byte(encoded), &cost) != nil {
		return nil
	}
	return &cost
}

func (cost *ReportedCost) USD() *float64 {
	if cost == nil || !strings.EqualFold(cost.Currency, "USD") ||
		cost.Amount < 0 || math.IsNaN(cost.Amount) || math.IsInf(cost.Amount, 0) {
		return nil
	}
	amount := cost.Amount
	return &amount
}
