package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

func extensionFenceLocalAtomic(a *Agent, paths []string) {
	home, _ := homes()
	if len(paths) == 0 {
		return
	}
	for _, path := range paths {
		relative, err := filepath.Rel(home, path)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			return
		}
	}
	var once sync.Once
	var checkRoot func() error
	extensionFenceAgentAt(home, a, func() error {
		once.Do(func() { checkRoot = extensionAgentRootGuard(home) })
		return checkRoot()
	})
}

func extensionAgentOwnership(home string, ordinary, extensions []*Agent) []*Agent {
	checkRoot := extensionAgentRootGuard(home)
	for _, a := range ordinary {
		extensionFenceAgentAt(home, a, checkRoot)
	}
	all := append(slices.Clone(ordinary), extensions...)
	for _, profile := range extensions {
		metadata := profile.extensionMetadata
		if metadata == nil || metadata.base == nil || metadata.owns == nil || !metadata.owns() {
			continue
		}
		base := metadata.base
		profile.Aliases = append(profile.Aliases, base.ID)
		profile.Aliases = append(profile.Aliases, base.Aliases...)
		all = slices.DeleteFunc(all, func(candidate *Agent) bool { return candidate == base })
	}
	return all
}

func extensionBaseMutation(home, target string, mutate func() error) error {
	release, err := extensionBaseLock(home, target)
	if err != nil {
		return err
	}
	defer release()
	return mutate()
}

func extensionBaseLock(home, target string) (func(), error) {
	current, _ := homes()
	if !extensionSameHome(home, current) {
		return nil, errors.New("agent home changed; rediscover the agent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	unlock, err := extensionBindingLock(ctx, home, target, true)
	if err != nil {
		return nil, err
	}
	release, err := extensionBindingLock(ctx, "", "ledger", true)
	if err != nil {
		unlock()
		return nil, err
	}
	ledger, err := extensionBindingRead()
	release()
	if err != nil {
		unlock()
		return nil, err
	}
	for _, claim := range ledger.Records {
		if extensionSameHome(claim.Home, home) && claim.Phase != "disconnected" && extensionBindingTarget(claim) == target {
			unlock()
			return nil, fmt.Errorf("agent %s is owned by SDK profile %s; disconnect that profile first", target, claim.Agent)
		}
	}
	return unlock, nil
}

func (a *Agent) lockMutation() (func(), error) {
	if a.mutationGuard == nil {
		return func() {}, nil
	}
	return a.mutationGuard()
}

func extensionSameHome(first, second string) bool {
	return extensionBindingKey(first, "", "") == extensionBindingKey(second, "", "")
}

func extensionFenceAgentAt(home string, a *Agent, checkRoot func() error) {
	if a.WSL != "" || a.extensionMetadata != nil {
		return
	}
	root := extensionBindingPath()
	a.mutationGuard = func() (func(), error) {
		if err := checkRoot(); err != nil {
			return nil, err
		}
		if root != extensionBindingPath() {
			return nil, errors.New("agent configuration root changed; rediscover the agent")
		}
		return extensionBaseLock(home, a.ID)
	}
	mutate := func(fn func() error) error {
		release, err := a.mutationGuard()
		if err != nil {
			return err
		}
		defer release()
		return fn()
	}
	fence := func(fn func() error) func() error {
		if fn == nil {
			return nil
		}
		return func() error { return mutate(fn) }
	}
	fenceValue := func(fn func(string) error) func(string) error {
		if fn == nil {
			return nil
		}
		return func(value string) error { return mutate(func() error { return fn(value) }) }
	}
	for index := range a.Fields {
		a.Fields[index].Set = fenceValue(a.Fields[index].Set)
	}
	a.Sync, a.Unwire, a.Restore = fence(a.Sync), fence(a.Unwire), fence(a.Restore)
	a.Follow, a.EndFailover = fence(a.Follow), fence(a.EndFailover)
	a.through = fenceValue(a.through)
	if fn := a.Join; fn != nil {
		a.Join = func() (value bool, err error) {
			err = mutate(func() error { value, err = fn(); return err })
			return
		}
	}
	if fn := a.TakeLeftovers; fn != nil {
		a.TakeLeftovers = func() (value bool, err error) {
			err = mutate(func() error { value, err = fn(); return err })
			return
		}
	}
	if fn := a.RenameRefs; fn != nil {
		a.RenameRefs = func(from, to string) (value bool, err error) {
			err = mutate(func() error { value, err = fn(from, to); return err })
			return
		}
	}
	if native := a.Native; native != nil {
		native.Connect = fence(native.Connect)
		for _, setter := range []*func(string, string) error{&native.Apply, &native.Stage} {
			if fn := *setter; fn != nil {
				*setter = func(key, value string) error {
					return mutate(func() error { return fn(key, value) })
				}
			}
		}
		for _, execute := range []*func(*DisconnectPlan) error{&native.Execute, &native.ExecuteOffline} {
			if fn := *execute; fn != nil {
				*execute = func(plan *DisconnectPlan) error {
					return mutate(func() error { return fn(plan) })
				}
			}
		}
	}
	if start := a.Start; start != nil {
		a.Start = func() Starting {
			var result Starting
			err := mutate(func() error { result = start(); return nil })
			result.Err = errors.Join(result.Err, err)
			return result
		}
	}
}
