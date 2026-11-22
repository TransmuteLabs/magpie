package sessions

import (
	"context"
	"errors"
	"maps"
	"reflect"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
)

// A publication binds both native and delegated history to durable authority.
// It needs no catalog, worker or session-cache lock, so direct title readers
// and background callers use the same boundary as ordinary listings.
type historyPublication struct {
	historyClaims
	view  *sessionSourceView
	epoch uint64
	reads *settings.SessionReads
}

func inspectHistoryReads() (*settings.SessionReads, error) {
	// Preserve the one-time legacy/new-install decision before taking the
	// checked snapshot. Unknown settings never inherit the test/legacy default.
	fallback := currentReads()
	snapshot, err := settings.InspectSessionReads()
	if err != nil {
		return nil, err
	}
	if ownOff() {
		return readsNone, nil
	}
	if snapshot.Reads != nil {
		return snapshot.Reads, nil
	}
	return fallback, nil
}

func checkHistoryRead(agent string) error {
	_, err := historyReadGeneration(agent)
	return err
}

func historyReadGeneration(agent string) (string, error) {
	reads, err := inspectHistoryReads()
	if err != nil {
		return "", errors.Join(extension.ErrActionChanged, err)
	}
	if !reads.Reads(agent) {
		return "", ErrNotRead
	}
	return reads.Generation, nil
}

func inspectHistoryPublication(ctx context.Context) (historyPublication, error) {
	if err := ctx.Err(); err != nil {
		return historyPublication{}, err
	}
	view, epoch := sourceView.Load(), sourceEpoch.Load()
	reads, err := inspectHistoryReads()
	if err != nil {
		return historyPublication{}, err
	}
	claims, err := readHistoryClaims(ctx)
	if err != nil {
		return historyPublication{}, err
	}
	return historyPublication{view: view, epoch: epoch, reads: reads, historyClaims: claims}, ctx.Err()
}

// JSONL owns discovery and the transcript. A rooted store can prevent native
// access altogether: even a read-only SQLite connection can create sidecars.
func (snapshot historyPublication) jsonlAccounting(ctx context.Context) map[string]bool {
	if len(snapshot.bindings) == 0 {
		return nil
	}
	manager := sourceManager()
	entries, err := manager.ListContext(ctx)
	if err != nil {
		return nil
	}
	sources := enabledSourceOwners(entries)
	rooted := map[string]bool{}
	for _, consent := range snapshot.consents {
		rooted[consent.Agent] = true
	}
	var allowed map[string]bool
	for _, binding := range snapshot.bindings {
		agent := binding.Agent
		if !snapshot.reads.Reads(agent) || rooted[agent] || sources[agent].count != 1 ||
			snapshot.view != nil && snapshot.view.agents[agent] {
			continue
		}
		manifest, identity, err := sessionMigrationIdentity(ctx, manager, binding.Plugin)
		if err != nil || identity != binding.Identity {
			continue
		}
		descriptor, ok := manifest.ResolveSessionSource(binding.Source)
		if ok && descriptor.Agent == agent && descriptor.Container == "jsonl" {
			if allowed == nil {
				allowed = map[string]bool{}
			}
			allowed[agent] = true
		}
	}
	return allowed
}

func (before historyPublication) current(ctx context.Context) bool {
	after, err := inspectHistoryPublication(ctx)
	return err == nil && before.same(after)
}

func (before historyPublication) same(after historyPublication) bool {
	return before.view == after.view && before.samePermissions(after)
}

func (before historyPublication) sameNative(after historyPublication) bool {
	if !before.samePermissions(after) {
		return false
	}
	if before.view == after.view {
		return true
	}
	beforeOwners, beforeAgents := before.view.nativeExclusions()
	afterOwners, afterAgents := after.view.nativeExclusions()
	return maps.Equal(beforeOwners, afterOwners) && maps.Equal(beforeAgents, afterAgents)
}

func (before historyPublication) samePermissions(after historyPublication) bool {
	return before.epoch == after.epoch &&
		reflect.DeepEqual(before.reads, after.reads) && maps.Equal(before.owners, after.owners)
}

func (view *sessionSourceView) nativeExclusions() (owners, agents map[string]bool) {
	if view == nil {
		return nil, nil
	}
	return view.owners, view.agents
}

func (snapshot historyPublication) native(agent string) bool {
	return snapshot.reads.Reads(agent) && !snapshot.owners[agent] &&
		(snapshot.view == nil || !snapshot.view.owners[agent] && !snapshot.view.agents[agent])
}

func publishNativeTitles(agent string, read func() map[string]string) map[string]string {
	ctx := context.Background()
	snapshot, err := inspectHistoryPublication(ctx)
	if err == nil && snapshot.native(agent) {
		result := read()
		if snapshot.current(ctx) {
			return result
		}
	}
	return map[string]string{}
}
