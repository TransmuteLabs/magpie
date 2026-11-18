package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionAuthMark = "extension-auth:"

func (binding *extensionAccount) authMark() string {
	return extensionAuthMark + extensionModelHash([]byte(binding.cacheKey())) + ":"
}

func validExtensionAuthMark(value string) bool {
	value, found := strings.CutPrefix(value, extensionAuthMark)
	if !found {
		return false
	}
	identity, token, found := strings.Cut(value, ":")
	return found && validExtensionModelHash(identity) && len(token) >= 16 && len(token) <= 128 && sdk.ValidID(token)
}

func extensionAuthLapse(binding *extensionAccount, saved savedLogin) string {
	if validExtensionAuthMark(saved.Refused) && !strings.HasPrefix(saved.Refused, binding.authMark()) {
		return ""
	}
	return saved.Lapsed
}

func (binding *extensionAccount) beginUsageAuth(ctx context.Context) (string, error) {
	if binding.scope.Account == "" || !binding.manifest.Contract.Has(sdk.AccountAuth) {
		return "", nil
	}
	if !loginsMu.LockChecked() {
		return "", errors.New("account authentication update deferred: login lock is unavailable")
	}
	defer loginsMu.Unlock()
	if err := binding.check(ctx); err != nil {
		return "", err
	}
	if err := binding.checkModelRevision(ctx); err != nil {
		return "", err
	}
	logins, err := readLoginsFile()
	if err != nil {
		return "", err
	}
	for index := range logins {
		saved := &logins[index]
		if saved.Agent != ExtensionProviderID(binding.scope.Plugin, binding.scope.Provider) || saved.User != binding.scope.Account {
			continue
		}
		before := saved.Lapsed
		saved.Lapsed = extensionAuthLapse(binding, *saved)
		saved.Refused = binding.authMark() + strings.ToLower(rand.Text())
		if err := writeLoginsTold(logins, saved.Lapsed != before); err != nil {
			return "", err
		}
		return saved.Refused, nil
	}
	return "", errors.New("extension login metadata is unavailable")
}

func (binding *extensionAccount) finishUsageAuth(ctx context.Context, token string, refused bool) error {
	if token == "" {
		return nil
	}
	if !loginsMu.LockChecked() {
		return errors.New("account authentication update deferred: login lock is unavailable")
	}
	defer loginsMu.Unlock()
	if err := binding.check(ctx); err != nil {
		return err
	}
	if err := binding.checkModelRevision(ctx); err != nil {
		return err
	}
	logins, err := readLoginsFile()
	if err != nil {
		return err
	}
	for index := range logins {
		saved := &logins[index]
		if saved.Agent != ExtensionProviderID(binding.scope.Plugin, binding.scope.Provider) || saved.User != binding.scope.Account || saved.Refused != token {
			continue
		}
		lapsed := ""
		if refused {
			lapsed = "Account sign-in has expired; sign in again"
		}
		if saved.Lapsed != lapsed {
			saved.Lapsed = lapsed
			return writeLogins(logins)
		}
		return nil
	}
	return nil
}

func extensionUsageUnauthenticated(err error) bool {
	var refusal *sdk.Error
	return errors.As(err, &refusal) && refusal.Code == "unauthenticated"
}
