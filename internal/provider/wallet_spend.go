package provider

// A wallet's window (QuotaWindow.Balance: AStudio's 通用积分 and 星火积分,
// SuperMagic's points) tells only what is left: no total, no share used.
// A usage cap had nothing to hold it at. It read 0% used until the wallet
// was empty, so a cap by share never tripped and one by amount, which
// needs the window's total, left it aside; an account capped at 4 spent
// over 1,000 credits past it (#5770, 9won_25273 on Discord).
//
// So magpie counts what such a wallet spends itself. Each reading's
// balance is set against the one before: a fall is spent, a rise (a
// top-up, a daily grant) is not. The count runs over a period, the
// window's own when it tells a reset, else the local day (a wallet never
// renews, and a cap that, once reached, held the account for good would
// turn it off with nothing to say when it comes back; the ask was a
// daily one). A period's count starts at 0 at its first reading. What
// went before that reading, or between the last reading of one period
// and the first of the next, isn't known and isn't counted.
//
// On an account with a usage cap, the window then reads as Amount spent
// of Limit, what the period had (spent plus what is left), and Used that
// share: a cap by share or by amount holds it as it holds any window, in
// routing (allowanceOf takes them as they are) and on the GUI's row.
// Without a cap the window keeps its balance alone, as before, and
// routing goes by that (Allowance.Left).
//
// The counts are kept in wallet-spend.json beside providers.json, by
// provider, account and window, so a restart doesn't start a period's
// count again.

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

// walletSpend is one wallet window's count for its period.
type walletSpend struct {
	Ends        time.Time         `json:"ends"`  // when the period ends
	Since       time.Time         `json:"since"` // its first reading
	Spent       float64           `json:"spent"`
	Last        float64           `json:"last"` // the balance last read
	Observation walletObservation `json:"observation,omitzero"`
}

type walletObservation struct {
	pluginLoginOrder
	LoginID   string `json:"loginId"`
	Epoch     string `json:"epoch,omitempty"`
	AuthEpoch string `json:"authEpoch,omitempty"`
}

var errWalletReplySuperseded = errors.New("a newer Usage reply already observed this wallet")

var walletSpends = filelock.Mutex{
	Path: func() string { return walletSpendPath() + ".lock" },
}

func walletSpendPath() string { return filepath.Join(filepath.Dir(Path()), "wallet-spend.json") }

// walletPeriodEnd is when the period a reading of w at now starts ends:
// the window's reset when it tells one, else the next local midnight.
func walletPeriodEnd(w QuotaWindow, now time.Time) time.Time {
	switch {
	case w.ResetsAt != nil && w.ResetsAt.After(now):
		return *w.ResetsAt
	case w.ResetSecs > 0:
		return now.Add(time.Duration(w.ResetSecs) * time.Second)
	}
	y, m, d := now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
}

func noteWalletSpend(ctx context.Context, prefix string, windows []QuotaWindow, now time.Time, observation walletObservation, check func() error) ([]walletSpend, error) {
	if !walletSpends.LockCheckedContext(ctx) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("the wallet spend lock is unavailable")
	}
	defer walletSpends.Unlock()
	if err := check(); err != nil {
		return nil, err
	}
	path := walletSpendPath()
	current := map[string]walletSpend{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(raw, &current) != nil || current == nil {
			return nil, errors.New("the wallet spend file is unreadable")
		}
		superseded := false
		for key, spend := range current {
			if spend.Since.IsZero() || !spend.Ends.After(spend.Since) || spend.Spent < 0 || spend.Last < 0 {
				return nil, errors.New("the wallet spend file has invalid counters")
			}
			saved := spend.Observation
			if saved == (walletObservation{}) {
				continue
			}
			if saved.LoginID == "" || saved.Started.IsZero() {
				return nil, errors.New("the wallet spend file has an invalid observation")
			}
			if saved.LoginID == observation.LoginID && saved.Epoch == observation.Epoch && saved.AuthEpoch == observation.AuthEpoch &&
				strings.HasPrefix(key, prefix) && !observation.after(saved.pluginLoginOrder) {
				superseded = true
			}
		}
		if superseded {
			return nil, errWalletReplySuperseded
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	counts := make([]walletSpend, len(windows))
	changed := false
	for index, window := range windows {
		if !walletSpendWindow(window) {
			continue
		}
		key := prefix + WindowCapID(window.Name)
		previous, ok := current[key]
		balance := *window.Balance
		next := previous
		if !ok || !now.Before(previous.Ends) || now.Before(previous.Since) {
			next = walletSpend{Ends: walletPeriodEnd(window, now), Since: now, Last: balance}
		} else {
			next.Last = balance
			next.Spent += max(previous.Last-balance, 0)
		}
		next.Observation = observation
		counts[index] = next
		if next != previous {
			current[key] = next
			changed = true
		}
	}
	if !changed {
		return counts, check()
	}
	maps.DeleteFunc(current, func(_ string, entry walletSpend) bool { return now.Sub(entry.Ends) > 7*24*time.Hour })
	raw, err = json.Marshal(current)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomicChecked(path, raw, check); err != nil {
		return nil, err
	}
	return counts, nil
}

func walletSpendWindow(window QuotaWindow) bool {
	return window.Balance != nil && !window.Text
}

func countsWalletSpend(q SubscriptionQuota, user string) bool {
	return q.Error == "" && q.Provider != "" && user != "" && slices.ContainsFunc(q.Windows, walletSpendWindow)
}

// withWalletSpend is a fresh reading q of account user with each wallet
// window's spend counted (Spent, SpentSince) and, on an account with a
// usage cap, read as that much used of what its period had. q's windows
// are its own, changed in place.
func withWalletSpend(ctx context.Context, q SubscriptionQuota, user string, now time.Time, observation walletObservation, check func() error) SubscriptionQuota {
	if !countsWalletSpend(q, user) {
		return q
	}
	counts, err := noteWalletSpend(ctx, q.Provider+"/"+accountKey(user)+"/", q.Windows, now, observation, check)
	if errors.Is(err, errWalletReplySuperseded) || errors.Is(err, errAccountDiscoveryOff) || errors.Is(err, errPluginUsageDiscovery) {
		return deferPluginQuota(q, user, err)
	}
	if err != nil {
		q.Error = err.Error()
		return q
	}
	caps := AccountCapsOf(q.Provider, user)
	for i := range q.Windows {
		w := &q.Windows[i]
		if !walletSpendWindow(*w) {
			continue
		}
		s := counts[i]
		spent, since := s.Spent, s.Since
		w.Spent, w.SpentSince = &spent, &since
		of := spent + max(*w.Balance, 0)
		if !caps.Holds() || of <= 0 {
			continue
		}
		w.Amount, w.Limit = spent, of
		w.Used = max(w.Used, spent/of*100)
	}
	return q
}
