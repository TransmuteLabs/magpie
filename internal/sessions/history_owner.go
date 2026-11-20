package sessions

import (
	"context"
	"errors"

	"github.com/yetone/magpie/internal/extension"
)

type historyClaims struct {
	bindings []SessionSourceBinding
	consents []StorageSourceConsent
	owners   map[string]bool
}

func readHistoryClaims() (historyClaims, error) {
	bindings, bindingErr := readSourceBindings()
	consents, consentErr := readStorageConsents()
	claims := historyClaims{bindings: bindings, consents: consents, owners: map[string]bool{}}
	for _, binding := range bindings {
		claims.owners[binding.Agent] = true
	}
	for _, consent := range consents {
		claims.owners[consent.Agent] = true
	}
	return claims, errors.Join(bindingErr, consentErr)
}

type historyOwnerMode uint8

const (
	historyUnavailable historyOwnerMode = iota
	historyNative
	historyDelegated
)

type sessionHistoryOwner struct {
	mode    historyOwnerMode
	session Session
}

func selectSessionHistoryOwner(ctx context.Context, session Session) (sessionHistoryOwner, error) {
	owner := sessionHistoryOwner{mode: historyNative, session: session}
	if session.source != nil {
		owner.mode = historyDelegated
	}
	if err := owner.check(ctx); err != nil {
		return sessionHistoryOwner{}, err
	}
	return owner, nil
}

func (owner sessionHistoryOwner) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if owner.mode == historyUnavailable {
		return extension.ErrActionChanged
	}
	if owner.mode == historyDelegated {
		if err := checkHistoryRead(owner.session.Agent); err != nil {
			return err
		}
		if owner.session.source == nil || owner.session.source.grant.descriptor.Agent != owner.session.Agent {
			return extension.ErrActionChanged
		}
		return checkSelectedSource(ctx, owner.session.source)
	}
	return checkNativeHistoryOwner(ctx, owner.session.Agent)
}

func checkNativeHistoryOwner(ctx context.Context, agent string) error {
	if err := checkHistoryRead(agent); err != nil {
		return err
	}
	claims, err := readHistoryClaims()
	if err != nil {
		return errors.Join(extension.ErrActionChanged, err)
	}
	if claims.owners[agent] || extensionSourceAgent(agent) {
		return extension.ErrActionChanged
	}
	return ctx.Err()
}
