package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type windowAdmissionKey struct{}

func checkWindowAdmission(ctx context.Context) error {
	if check, found := ctx.Value(windowAdmissionKey{}).(func(context.Context) error); found {
		return check(ctx)
	}
	return nil
}

func windowAccountGrant(manifest sdk.Manifest, binding sdk.BindingDescriptor) error {
	if manifest.Contract.Version.Minor < 5 || !manifest.Contract.Has(sdk.AccountAuth) ||
		binding.Provider == "" || binding.Action == "" || !strings.HasPrefix(binding.From, "window:") {
		return errors.New("window binding does not grant a selected account")
	}
	for _, provider := range manifest.Providers {
		if provider.ID == binding.Provider {
			return nil
		}
	}
	return errors.New("window account provider is not declared")
}

func (m *Manager) WindowAccounts(ctx context.Context, id, revision, part, bindingID string) ([]sdk.AccountSummary, error) {
	entry, _, err := m.windowEntry(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	binding, err := windowBinding(entry.Manifest, part, bindingID)
	if err != nil || windowAccountGrant(entry.Manifest, binding) != nil {
		return nil, errors.New("window binding does not grant account discovery")
	}
	accounts, err := m.store.accountMetadataForProvider(ctx, id, binding.Provider, 1024)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, errAccountListBudget) {
		return nil, errors.New("window account list exceeds its budget")
	}
	if err != nil {
		return nil, errors.New("window accounts are unavailable")
	}
	summaries := make([]sdk.AccountSummary, len(accounts))
	for index, account := range accounts {
		summaries[index] = sdk.AccountSummary{
			ID: account.ID, Generation: strconv.FormatUint(account.Generation, 10), ExpiresAt: account.ExpiresAt, Name: account.Name,
		}
	}
	if _, _, err := m.windowEntry(ctx, id, revision); err != nil {
		return nil, err
	}
	return summaries, nil
}

func (m *Manager) selectedWindowScope(ctx context.Context, manifest sdk.Manifest, binding sdk.BindingDescriptor, selection *sdk.AccountSelection) (sdk.Scope, error) {
	scope := sdk.Scope{Plugin: manifest.ID}
	if binding.Provider == "" {
		if selection != nil {
			return scope, errors.New("window binding does not grant account selection")
		}
		return scope, nil
	}
	if windowAccountGrant(manifest, binding) != nil || selection == nil {
		return scope, errors.New("window action requires a declared selected account")
	}
	generation, err := selection.Validate()
	if err != nil {
		return scope, err
	}
	account, err := m.store.GetAccountContext(ctx, manifest.ID, binding.Provider, selection.ID)
	if ctx.Err() != nil {
		return scope, ctx.Err()
	}
	if err != nil || account.Generation != generation {
		return scope, errors.New("window account selection changed or is unavailable")
	}
	scope.Provider, scope.Account, scope.Generation = binding.Provider, account.ID, generation
	return scope, nil
}

type windowIntentRecord struct {
	Schema      int       `json:"schema"`
	Scope       sdk.Scope `json:"scope"`
	Binding     string    `json:"binding"`
	Fingerprint string    `json:"fingerprint"`
}

func (s *Store) bindWindowIntent(ctx context.Context, scope sdk.Scope, binding string, input json.RawMessage) error {
	if !storeValidScope(scope) || scope.Account == "" || !storeToken(scope.Operation) || !sdk.ValidID(binding) || !json.Valid(input) {
		return errors.New("window intent requires its selected account and bounded input")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, input) != nil {
		return errors.New("window intent input is invalid")
	}
	digest := sha256.Sum256(compact.Bytes())
	record := windowIntentRecord{Schema: 1, Scope: scope, Binding: binding, Fingerprint: hex.EncodeToString(digest[:])}
	return s.withStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		if err := storeCheckGeneration(root, scope); err != nil {
			return err
		}
		directory, err := storeDirectory(root, "window-intents")
		if err != nil {
			return err
		}
		defer directory.Close()
		name := storeHash(scope.Operation) + ".json"
		var previous windowIntentRecord
		found, err := storeRead(directory, name, storeFileLimit, &previous)
		if err != nil {
			return err
		}
		if found {
			if !reflect.DeepEqual(previous, record) {
				return errors.New("window request intent already names different account authority or input")
			}
			return nil
		}
		return storeWrite(directory, name, record, storeFileLimit)
	})
}
