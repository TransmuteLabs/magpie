package extension

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const MethodCatalogRead = "catalog.read"

type CatalogAccessDescriptor struct {
	Scope string `json:"scope"`
}

type CatalogReadRequest struct{}

type CatalogReadResult struct {
	ReadAt   int64                      `json:"readAt"`
	Accounts []CatalogAccountProjection `json:"accounts"`
}

type CatalogAccountProjection struct {
	Ref      string                   `json:"ref"`
	Provider string                   `json:"provider"`
	Kind     string                   `json:"kind"`
	Models   []CatalogModelProjection `json:"models"`
	Quota    CatalogQuotaProjection   `json:"quota"`
}

type CatalogModelProjection struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxOutput     int64  `json:"maxOutput,omitempty"`
}

type CatalogQuotaProjection struct {
	Known   bool                      `json:"known"`
	AsOf    int64                     `json:"asOf,omitempty"`
	Windows []CatalogWindowProjection `json:"windows,omitempty"`
}

type CatalogWindowProjection struct {
	Name      string   `json:"name"`
	Models    []string `json:"models"`
	Used      *float64 `json:"used,omitempty"`
	ResetsAt  int64    `json:"resetsAt,omitempty"`
	Aside     bool     `json:"aside,omitempty"`
	Unlimited bool     `json:"unlimited,omitempty"`
	Credits   bool     `json:"credits,omitempty"`
}

func ValidateCatalogAccess(manifest Manifest) error {
	access := manifest.CatalogAccess
	if access == nil {
		return nil
	}
	if manifest.Contract.Version.Minor < 8 ||
		access.Scope != "selected" && access.Scope != "inventory" ||
		access.Scope == "selected" && !manifest.Contract.Has(AccountAuth) && manifest.Component == nil {
		return errors.New("catalog access requires SDK API 1.8 and an explicit selected or inventory scope")
	}
	return nil
}

func ValidateCatalogProjection(value CatalogReadResult) error {
	if value.ReadAt <= 0 || value.ReadAt > 1<<53-1 || value.Accounts == nil || len(value.Accounts) > 256 {
		return errors.New("invalid catalog projection")
	}
	refs, models := map[string]bool{}, 0
	for _, account := range value.Accounts {
		ref, err := hex.DecodeString(account.Ref)
		if err != nil || len(ref) != 32 || strings.ToLower(account.Ref) != account.Ref ||
			refs[account.Ref] || account.Provider == "" || !validExecutionText(account.Provider, 300) ||
			account.Kind != "account" && account.Kind != "key" && account.Kind != "public" {
			return errors.New("invalid catalog account projection")
		}
		refs[account.Ref] = true
		ids := map[string]bool{}
		models += len(account.Models)
		if account.Models == nil || models > 16384 || len(account.Models) > 4096 || len(account.Quota.Windows) > 1024 ||
			account.Quota.AsOf < 0 || account.Quota.AsOf > 1<<53-1 || !account.Quota.Known && len(account.Quota.Windows) > 0 {
			return errors.New("catalog projection exceeds its budget or invents quota evidence")
		}
		for _, model := range account.Models {
			if model.ID == "" || !validExecutionText(model.ID, 600) || !validExecutionText(model.Name, 300) ||
				ids[model.ID] || model.ContextWindow < 0 || model.MaxOutput < 0 ||
				model.ContextWindow > 1<<53-1 || model.MaxOutput > 1<<53-1 {
				return errors.New("invalid catalog model projection")
			}
			ids[model.ID] = true
		}
		for _, window := range account.Quota.Windows {
			if window.Name == "" || !validExecutionText(window.Name, 300) || window.ResetsAt < 0 || window.ResetsAt > 1<<53-1 ||
				window.Used != nil && (!validExecutionAmount(*window.Used) || *window.Used > 100) ||
				len(window.Models) == 0 || len(window.Models) > len(account.Models) {
				return errors.New("invalid cached catalog quota window")
			}
			windowModels := map[string]bool{}
			for _, model := range window.Models {
				if !ids[model] || windowModels[model] {
					return errors.New("catalog quota escaped the account's visible models")
				}
				windowModels[model] = true
			}
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaxHostBytes {
		return errors.New("catalog projection exceeds its byte budget")
	}
	return nil
}
