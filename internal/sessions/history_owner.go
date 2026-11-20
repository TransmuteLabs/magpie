package sessions

import (
	"context"
	"errors"
	"math"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
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
	reader  historyPartReader
}

type historyPartReader func(context.Context, Session, historyRead, func(bool, Part) bool) error

type historyRead struct {
	keep       func([]byte) bool
	spokenOnly bool
	span       *historySpan
}

type historySpan struct {
	from, to int64
	message  string
}

func (read historyRead) call(session Session) Call {
	call := Call{Agent: session.Agent, File: session.Path, To: math.MaxInt64}
	if read.span != nil {
		call.From, call.To, call.Msg = read.span.from, read.span.to, read.span.message
	}
	return call
}

func selectSessionHistoryOwner(ctx context.Context, session Session) (sessionHistoryOwner, error) {
	owner := sessionHistoryOwner{mode: historyNative, session: session}
	if session.source != nil {
		owner.mode = historyDelegated
	}
	if err := owner.check(ctx); err != nil {
		return sessionHistoryOwner{}, err
	}
	switch {
	case owner.mode == historyDelegated:
		owner.reader = readDelegatedHistoryParts
	case session.Agent == "codex":
		owner.reader = readCodexHistoryParts
	case ccCallAgent(session.Agent):
		owner.reader = readClaudeHistoryParts
	default:
		owner.reader = readNativeHistoryParts
	}
	return owner, nil
}

func (owner sessionHistoryOwner) readParts(ctx context.Context, read historyRead, add func(bool, Part) bool) (err error) {
	if err := owner.check(ctx); err != nil {
		return err
	}
	defer func() {
		if changed := owner.check(ctx); changed != nil {
			err = changed
		}
	}()
	stopped := false
	return owner.reader(ctx, owner.session, read, func(user bool, part Part) bool {
		if stopped || ctx.Err() != nil {
			return false
		}
		stopped = !add(user, part)
		return !stopped
	})
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
	if err := checkNativeHistoryClaim(agent); err != nil {
		return err
	}
	return ctx.Err()
}

func checkNativeHistoryClaim(agent string) error {
	claims, err := readHistoryClaims()
	if err != nil {
		return errors.Join(extension.ErrActionChanged, err)
	}
	if claims.owners[agent] || extensionSourceAgent(agent) {
		return extension.ErrActionChanged
	}
	return nil
}

func lockNativeHistoryMutation(agent string) (func(), error) {
	unlockBinding, held := filelock.Lock(sourceBindingsPath()+".lock", 0)
	if !held {
		return nil, extension.ErrActionChanged
	}
	unlockStorage, held := filelock.Lock(storageConsentsPath()+".lock", 0)
	if !held {
		unlockBinding()
		return nil, extension.ErrActionChanged
	}
	unlock := func() {
		unlockStorage()
		unlockBinding()
	}
	if err := checkNativeHistoryClaim(agent); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}
