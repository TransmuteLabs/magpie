package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionAuthMark = "extension-auth:"

func (binding *extensionAccount) authMark() string {
	key := binding.cacheKey()
	if binding.enableGeneration != "" {
		key += "\x00enable:" + binding.enableGeneration
	}
	return extensionAuthMark + extensionModelHash([]byte(key)) + ":"
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
	lapsed, refused := extensionAuthStatus(binding, &saved)
	if validExtensionAuthMark(*refused) && !strings.HasPrefix(*refused, binding.authMark()) {
		return ""
	}
	return *lapsed
}

func extensionAuthStatus(binding *extensionAccount, saved *savedLogin) (lapsed, refused *string) {
	if binding.identity().revision != "" {
		return &saved.SDKLapsed, &saved.SDKRefused
	}
	return &saved.Lapsed, &saved.Refused
}

func (binding *extensionAccount) beginUsageAuth(ctx context.Context) (string, error) {
	if binding.scope.Account == "" || !binding.manifest.Contract.Has(sdk.AccountAuth) {
		return "", nil
	}
	if !loginsMu.LockCheckedContext(ctx) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", errors.New("account authentication update deferred: login lock is unavailable")
	}
	defer loginsMu.Unlock()
	if err := requireExtensionAccountDiscovery(ctx); err != nil {
		return "", err
	}
	if err := binding.check(ctx); err != nil {
		return "", err
	}
	if err := binding.checkModelRevision(ctx); err != nil {
		return "", err
	}
	logins, err := readCurrentLogins()
	if err != nil {
		return "", err
	}
	beforeCount := len(logins)
	logins, index := ensureExtensionLoginPolicy(slices.Clone(logins), binding.identity())
	saved := &logins[index]
	lapsed, refused := extensionAuthStatus(binding, saved)
	before := *lapsed
	*lapsed = extensionAuthLapse(binding, *saved)
	*refused = binding.authMark() + strings.ToLower(rand.Text())
	token := *refused
	if err := writeLoginsChecked(logins, *lapsed != before || len(logins) != beforeCount,
		func() error { return requireExtensionAccountDiscovery(ctx) }); err != nil {
		return "", err
	}
	return token, nil
}

func (binding *extensionAccount) finishUsageAuth(ctx context.Context, token string, refused bool) error {
	if token == "" {
		return nil
	}
	if !loginsMu.LockCheckedContext(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("account authentication update deferred: login lock is unavailable")
	}
	defer loginsMu.Unlock()
	if err := requireExtensionAccountDiscovery(ctx); err != nil {
		return err
	}
	if err := binding.check(ctx); err != nil {
		return err
	}
	if err := binding.checkModelRevision(ctx); err != nil {
		return err
	}
	logins, err := readCurrentLogins()
	if err != nil {
		return err
	}
	identity := binding.identity()
	for index := range logins {
		saved := logins[index]
		lapsed, refusedMark := extensionAuthStatus(binding, &saved)
		if !identity.hasPolicy(saved) || *refusedMark != token {
			continue
		}
		message := ""
		if refused {
			message = "Account sign-in has expired; sign in again"
		}
		if *lapsed != message {
			*lapsed = message
			next := slices.Clone(logins)
			next[index] = saved
			return writeLoginsChecked(next, true, func() error { return requireExtensionAccountDiscovery(ctx) })
		}
		return nil
	}
	return nil
}

func extensionUsageUnauthenticated(err error) bool {
	var refusal *sdk.Error
	return errors.As(err, &refusal) && refusal.Code == "unauthenticated"
}
