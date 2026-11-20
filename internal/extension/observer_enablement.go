package extension

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const observerEnablementLimit = 128

func tracksEnablement(manifest sdk.Manifest) bool {
	if manifest.Contract.Has(sdk.LedgerObserver) || manifest.Contract.Has(sdk.RequestObserver) ||
		manifest.Contract.Has(sdk.BudgetPolicy) || manifest.Contract.Has(sdk.MaskingRules) ||
		manifest.Contract.Has(sdk.RequestPolicy) || manifest.Contract.Has(sdk.GatewayHooks) ||
		manifest.Contract.Has(sdk.GatewayMiddleware) || manifest.Contract.Has(sdk.AgentWiring) {
		return true
	}
	for _, tool := range manifest.Tools {
		if tool.MemorySearch {
			return true
		}
	}
	for _, binding := range manifest.AuthBindings {
		if binding.From == sdk.ProviderSignInPart {
			return true
		}
	}
	return false
}

func observerEnablementFile(id string) string {
	return ".observer-" + storeHash(id) + ".json"
}

// An absent marker is the pre-toggle generation. Other failures mean the
// generation is unknown and cannot admit observer calls.
func readObserverEnablement(root *os.Root, id string) (string, error) {
	name := observerEnablementFile(id)
	if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	raw, err := catalogReadFile(root, name, observerEnablementLimit)
	var value string
	if err != nil || json.Unmarshal(raw, &value) != nil || len(value) < 26 || strings.ContainsFunc(value, func(char rune) bool {
		return !(char >= 'A' && char <= 'Z' || char >= '2' && char <= '7')
	}) {
		return "", errors.New("extension observer enablement is unreadable")
	}
	return value, nil
}

// The catalog lock covers both writes. Persist the fence before enabling the
// observer. A failed catalog publication can only invalidate
// queued events conservatively.
func advanceObserverEnablement(root *os.Root, id string) error {
	if _, err := readObserverEnablement(root, id); err != nil {
		return err
	}
	return storeWrite(root, observerEnablementFile(id), rand.Text(), observerEnablementLimit)
}
