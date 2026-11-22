package sessions

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
)

type historyClaims struct {
	bindings []SessionSourceBinding
	consents []StorageSourceConsent
	owners   map[string]bool
}

func withHistoryClaimsRoot[T any](ctx context.Context, read func(context.Context, *os.Root) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	root, err := os.OpenRoot(appdir.Config())
	if errors.Is(err, fs.ErrNotExist) {
		return zero, nil
	}
	if err != nil {
		return zero, err
	}
	defer root.Close()
	return read(ctx, root)
}

func readHistoryClaims(ctx context.Context) (historyClaims, error) {
	return withHistoryClaimsRoot(ctx, func(ctx context.Context, root *os.Root) (historyClaims, error) {
		bindings, bindingErr := readSourceBindingsAt(ctx, root)
		consents, consentErr := readStorageConsentsAt(ctx, root)
		claims := historyClaims{bindings: bindings, consents: consents}
		if len(bindings)+len(consents) != 0 {
			claims.owners = make(map[string]bool, len(bindings)+len(consents))
			for _, binding := range bindings {
				claims.owners[binding.Agent] = true
			}
			for _, consent := range consents {
				claims.owners[consent.Agent] = true
			}
		}
		return claims, errors.Join(bindingErr, consentErr)
	})
}

type historyOwnerMode uint8

const (
	historyUnavailable historyOwnerMode = iota
	historyNative
	historyDelegated
)

type sessionHistoryOwner struct {
	mode           historyOwnerMode
	session        Session
	reader         historyPartReader
	readGeneration string
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
	if err := ctx.Err(); err != nil {
		return sessionHistoryOwner{}, err
	}
	generation, err := historyReadGeneration(session.Agent)
	if err != nil {
		return sessionHistoryOwner{}, err
	}
	owner := sessionHistoryOwner{mode: historyNative, session: session, readGeneration: generation}
	if session.source != nil {
		owner.mode = historyDelegated
	}
	if err := owner.checkSelection(ctx); err != nil {
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
	return owner.walkParts(ctx, read, add)
}

func (owner sessionHistoryOwner) walkParts(ctx context.Context, read historyRead, add func(bool, Part) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	generation, err := historyReadGeneration(owner.session.Agent)
	if err != nil {
		return err
	}
	if generation != owner.readGeneration {
		return ErrNotRead
	}
	return owner.checkSelection(ctx)
}

func (owner sessionHistoryOwner) checkSelection(ctx context.Context) error {
	if owner.mode == historyDelegated {
		if owner.session.source == nil || owner.session.source.grant.descriptor.Agent != owner.session.Agent {
			return extension.ErrActionChanged
		}
		return checkSelectedSource(ctx, owner.session.source)
	}
	if err := checkNativeHistoryClaim(ctx, owner.session.Agent); err != nil {
		return err
	}
	return ctx.Err()
}

func checkNativeHistoryOwner(ctx context.Context, agent string) error {
	if err := checkHistoryRead(agent); err != nil {
		return err
	}
	if err := checkNativeHistoryClaim(ctx, agent); err != nil {
		return err
	}
	return ctx.Err()
}

func checkNativeHistoryClaim(ctx context.Context, agent string) error {
	claims, err := readHistoryClaims(ctx)
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
	if err := checkNativeHistoryClaim(context.Background(), agent); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}
