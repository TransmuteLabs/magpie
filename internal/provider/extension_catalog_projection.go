package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func init() {
	extensionhost.SetCatalogProjectionReader(extensionCatalogProjection)
}

func extensionCatalogProjection(ctx context.Context, scope sdk.Scope, access sdk.CatalogAccessDescriptor) (sdk.CatalogReadResult, error) {
	if err := ctx.Err(); err != nil {
		return sdk.CatalogReadResult{}, err
	}
	defer Hold()()
	stamp := StampNow()
	if _, err := read(); err != nil {
		return sdk.CatalogReadResult{}, err
	}
	if _, err := readLoginsFile(); err != nil {
		return sdk.CatalogReadResult{}, err
	}
	entries := ListedProviderEntries(ctx)
	if err := ctx.Err(); err != nil {
		return sdk.CatalogReadResult{}, err
	}
	byProvider := map[string][]Entry{}
	for _, entry := range entries {
		if entry.Group == "" && entry.Provider != nil {
			byProvider[entry.Provider.ID] = append(byProvider[entry.Provider.ID], entry)
		}
	}
	now := time.Now()
	cards := CachedCardsContext(ctx, now)
	if err := ctx.Err(); err != nil {
		return sdk.CatalogReadResult{}, err
	}
	result := sdk.CatalogReadResult{ReadAt: now.UnixMilli(), Accounts: []sdk.CatalogAccountProjection{}}
	selected := access.Scope == "selected"
	var selectedID string
	if selected {
		selectedID = heldSubscriptionOwnersContext(ctx).identity(scope).provider
	}
	for _, configured := range AllContext(ctx) {
		if err := ctx.Err(); err != nil {
			return sdk.CatalogReadResult{}, err
		}
		if !configured.On() || configured.Unlisted || len(byProvider[configured.ID]) == 0 {
			continue
		}
		if selected && configured.ID != selectedID {
			continue
		}
		if configured.Account != nil {
			for _, account := range configured.AccountsOnContext(ctx) {
				binding := account.Account.extension
				if selected {
					if binding == nil {
						continue
					}
					current := binding.currentScope()
					if current.Plugin != scope.Plugin || current.Provider != scope.Provider ||
						current.Account != scope.Account || current.Generation != scope.Generation {
						continue
					}
				}
				projection := projectedCatalogAccount(scope.Plugin, configured.ID, "account", account.Account.User)
				projection.Models = projectedCatalogAccountModels(ctx, configured, account.Account, byProvider[configured.ID])
				for _, card := range cards {
					if card.Provider == configured.ID && strings.EqualFold(card.User, account.Account.User) {
						projection.Quota = projectedCatalogQuota(configured, currentExtensionQuotaContext(ctx, card), projection.Models)
						break
					}
				}
				if len(projection.Models) > 0 {
					result.Accounts = append(result.Accounts, projection)
				}
			}
			continue
		}
		if selected {
			continue
		}
		keys := configured.KeysOn()
		if len(keys) == 0 {
			projection := projectedCatalogAccount(scope.Plugin, configured.ID, "public", "")
			for _, entry := range byProvider[configured.ID] {
				projection.Models = append(projection.Models, projectedCatalogModel(entry))
			}
			result.Accounts = append(result.Accounts, projection)
		}
		for _, key := range keys {
			ref := KeyID(key.Key)
			projection := projectedCatalogAccount(scope.Plugin, configured.ID, "key", ref)
			for _, entry := range byProvider[configured.ID] {
				if configured.AccountServes(ref, entry.Model) && configured.Serves(key, entry.Model) {
					projection.Models = append(projection.Models, projectedCatalogModel(entry))
				}
			}
			projection.Quota = projectedCatalogKeyQuota(configured.WithKey(key), projection.Models)
			if len(projection.Models) > 0 {
				result.Accounts = append(result.Accounts, projection)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return sdk.CatalogReadResult{}, err
	}
	if StampNow() != stamp {
		return sdk.CatalogReadResult{}, errors.New("catalog changed while its projection was read")
	}
	if selected && len(result.Accounts) != 1 {
		return sdk.CatalogReadResult{}, errors.New("selected catalog account is unavailable or has no visible models")
	}
	if err := sdk.ValidateCatalogProjection(result); err != nil {
		return sdk.CatalogReadResult{}, err
	}
	return result, nil
}

func projectedCatalogAccountModels(ctx context.Context, configured Provider, account *Account, entries []Entry) []sdk.CatalogModelProjection {
	var models []sdk.CatalogModelProjection
	var allowed map[string]struct{}
	loaded := false
	binding := account.extension
	for _, entry := range entries {
		if !configured.AccountServes(account.User, entry.Model) {
			continue
		}
		own := configured.OwnModelID(entry.Model)
		if binding != nil {
			if !loaded {
				list, known, err := binding.selectedModels(ctx)
				if err != nil || known && len(list) == 0 {
					return nil
				}
				if len(list) > 0 {
					allowed = make(map[string]struct{}, len(list))
					for _, model := range list {
						allowed[model.ID] = struct{}{}
					}
				}
				loaded = true
			}
			if _, ok := allowed[own]; allowed != nil && !ok {
				continue
			}
		} else if !account.listsContext(ctx, own) {
			continue
		}
		models = append(models, projectedCatalogModel(entry))
	}
	return models
}

func projectedCatalogAccount(module, provider, kind, account string) sdk.CatalogAccountProjection {
	digest := sha256.Sum256([]byte(module + "\x00" + provider + "\x00" + kind + "\x00" + account))
	return sdk.CatalogAccountProjection{Ref: hex.EncodeToString(digest[:]), Provider: provider, Kind: kind,
		Models: []sdk.CatalogModelProjection{}}
}

func projectedCatalogModel(entry Entry) sdk.CatalogModelProjection {
	return sdk.CatalogModelProjection{ID: entry.ID, Name: entry.Name, ContextWindow: int64(entry.Context), MaxOutput: int64(entry.Output)}
}

func projectedCatalogKeyQuota(configured Provider, models []sdk.CatalogModelProjection) sdk.CatalogQuotaProjection {
	tags := []string{keyTag("plan", configured.Key), keyTag("balance", configured.Key)}
	currentTag := keyCardTag(configured)
	if !slices.Contains(tags, currentTag) {
		tags = append(tags, currentTag)
	}
	latest := keyRead(keyAllowanceID(configured))
	var result sdk.CatalogQuotaProjection
	for _, tag := range tags {
		card, known := lastReading(configured.ID + "/" + strings.ToLower(tag))
		if tag == currentTag && latest.seq > card.readSeq {
			at := latest.at
			card.Windows, card.ReadAt, card.AsOf, card.Error = latest.ws, &at, nil, ""
			known = true
		}
		if !known {
			continue
		}
		projected := projectedCatalogQuota(configured, card, models)
		if !projected.Known {
			continue
		}
		if !result.Known {
			result.AsOf = projected.AsOf
		} else {
			result.AsOf = min(result.AsOf, projected.AsOf)
		}
		result.Known = true
		result.Windows = append(result.Windows, projected.Windows...)
	}
	return result
}

func projectedCatalogQuota(configured Provider, card SubscriptionQuota, models []sdk.CatalogModelProjection) sdk.CatalogQuotaProjection {
	if card.Error != "" || len(card.Windows) == 0 {
		return sdk.CatalogQuotaProjection{}
	}
	projection := sdk.CatalogQuotaProjection{Known: true}
	if card.ReadAt != nil {
		projection.AsOf = card.ReadAt.UnixMilli()
	} else if card.AsOf != nil {
		projection.AsOf = card.AsOf.UnixMilli()
	}
	for _, window := range card.Windows {
		value := sdk.CatalogWindowProjection{Name: window.Name, Aside: window.Aside, Unlimited: window.Unlimited,
			Credits: window.Credits, Models: []string{}}
		applicability := Limit{Model: window.Model, matches: window.matches}
		for _, model := range models {
			_, native, found := strings.Cut(model.ID, "/")
			if found && applicability.applies(configured.OwnModelID(native)) {
				value.Models = append(value.Models, model.ID)
			}
		}
		if len(value.Models) == 0 {
			continue
		}
		if !window.Unknown && !window.Text && window.Error == "" {
			used := window.Used
			value.Used = &used
		}
		if window.ResetsAt != nil {
			value.ResetsAt = window.ResetsAt.UnixMilli()
		}
		projection.Windows = append(projection.Windows, value)
	}
	return projection
}
