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
	view   *sessionSourceView
	epoch  uint64
	reads  *settings.SessionReads
	owners map[string]bool
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
	reads, err := inspectHistoryReads()
	if err != nil {
		return errors.Join(extension.ErrActionChanged, err)
	}
	if !reads.Reads(agent) {
		return ErrNotRead
	}
	return nil
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
	claims, err := readHistoryClaims()
	if err != nil {
		return historyPublication{}, err
	}
	return historyPublication{view: view, epoch: epoch, reads: reads, owners: claims.owners}, ctx.Err()
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
