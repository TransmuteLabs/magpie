package provider

// A plugin's own daily check-in (Lemon on Discord: provider 每日签到的方法能
// 放到插件里面去做吗): a plugin whose auth hook has checkin presses its
// vendor's 签到 itself, and magpie runs it as it runs its own check-ins
// (checkin.go): each account in use once a Beijing day while the user has
// it on, a failure tried again later that day, what came of it on the
// account's Usage card and in Settings. A plugin that checks in takes over
// from magpie's own check-in through its fetch (WorkBuddy, Trae CN,
// MiniMax Code, Qoder), so an account is never checked in twice.
//
// What came of it is kept in plugin-checkin.json by provider and sign-in
// (pluginCheckinKey).

import (
	"context"
	"log"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// pluginCheckinBy is the WorkBuddyCheckin.By of a plugin's check-in.
func pluginCheckinBy(id string) string { return "plugin:" + id }

func pluginCheckinPath() string { return filepath.Join(filepath.Dir(Path()), "plugin-checkin.json") }

var pluginCheckinMu sync.Mutex

// pluginCheckinAcct is a plugin's account checked in.
type pluginCheckinAcct struct {
	User string
	On   bool
	key  string // pluginCheckinKey
	// legacy is the account's entry before plugins#254,
	// pluginCheckinLegacyKey
	legacy string
	card   string
	do     func(context.Context) WorkBuddyCheckin
}

// pluginCheckinKey is a plugin sign-in's entry in plugin-checkin.json and
// settings.CheckinOff: the provider and the key plugin-auth.json keeps the
// sign-in under ("autoclaw", "autoclaw#x1y2z3"). Two sign-ins never share
// it, and it holds while the sign-in does: a refresh writes the same key,
// and signing in to an account already signed in goes back to its key
// (host.js settle). Its display name (AccountID) is not the account: two
// AutoClaw sign-ins share a default name the user can't change, and keyed
// on it only the first of them was ever checked in (plugins#254).
func pluginCheckinKey(id, login string) string { return id + "|" + login }

// pluginCheckinLegacyKey is what pluginCheckinKey was before plugins#254:
// the provider and the account's display name, else its sign-in key.
func pluginCheckinLegacyKey(id string, a plugin.Account) string {
	if a.AccountID != "" {
		return id + "|" + a.AccountID
	}
	return pluginCheckinKey(id, a.Key)
}

// pluginCheckinProvider is a plugin's provider that checks in, and its
// accounts.
type pluginCheckinProvider struct {
	ID, Name string
	accounts []pluginCheckinAcct
	recheck  bool
}

// pluginCheckinProviders are the plugins' providers whose plugin checks
// in, those signed in to.
func pluginCheckinProviders(ctx context.Context) []pluginCheckinProvider {
	var out []pluginCheckinProvider
	owners := sync.OnceValue(func() extensionSubscriptionOwners { return currentSubscriptionOwners(ctx) })
	for _, pp := range plugin.Cached() {
		if ctx.Err() != nil {
			return nil
		}
		if !pp.Checkin || movingNow(pp.ID) || owners().replaces(pp.ID) {
			continue
		}
		p := pluginCheckinProvider{ID: pp.ID, Name: pp.Name}
		card := PluginID(pp.ID)
		for _, l := range pluginLoginsContext(ctx, pp) {
			acct := l.acct.Key
			id := pp.ID
			p.accounts = append(p.accounts, pluginCheckinAcct{User: l.User, On: l.On, key: pluginCheckinKey(id, acct), legacy: pluginCheckinLegacyKey(id, l.acct), card: card,
				do: func(ctx context.Context) WorkBuddyCheckin {
					c, err := plugin.AccountCheckin(ctx, id, acct)
					if err != nil {
						return checkinRequestFailed(ctx, err)
					}
					r := WorkBuddyCheckin{Outcome: c.Outcome, Credit: c.Credit, Streak: c.Streak, Msg: c.Message}
					// the vendor's own day turns then (Qoder's 10:00
					// UTC+8); a time that doesn't parse is not told
					if t, err := time.Parse(time.RFC3339, c.Until); err == nil {
						r.ValidUntil = t
					}
					return r
				}})
		}
		if len(p.accounts) > 0 {
			out = append(out, p)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return out
}

type legacyCheckinKeys map[string][]string

// legacyPluginCheckinKeys maps what was kept under each account's
// entry before plugins#254 (its provider and display name) over to its
// entry now (its sign-in), so an upgrade neither checks an account in
// twice in a day nor loses the user's own Off. Kept under a name two
// sign-ins share, it is:
//
//   - a check-in, in plugin-checkin.json: the first of them in use, in
//     the order the check-in lists them, the one the old key's check-in
//     asked (it skipped the rest), and only that one: the others were
//     never asked, so they are asked now. The entry goes once carried.
//   - an account left out, in settings.CheckinOff: each of them. The user
//     switched that name off, and none of its sign-ins was checked in
//     since; one switched on only by the upgrade would be checked in
//     against the user's choice.
func legacyPluginCheckinKeys(ps []pluginCheckinProvider) legacyCheckinKeys {
	// an entry a sign-in keeps now is never taken for an old one
	now := map[string]bool{}
	for _, p := range ps {
		for _, a := range p.accounts {
			now[a.key] = true
		}
	}
	// each legacy entry's heirs, those in use first: the old check-in
	// skipped an account not in use before it took the key
	heirs := legacyCheckinKeys{}
	for _, inUse := range []bool{true, false} {
		for _, p := range ps {
			for _, a := range p.accounts {
				if a.On != inUse || a.legacy == "" || now[a.legacy] {
					continue
				}
				heirs[a.legacy] = append(heirs[a.legacy], a.key)
			}
		}
	}
	return heirs
}

func (heirs legacyCheckinKeys) moveHistory(st map[string]WorkBuddyCheckin) bool {
	changed := false
	for legacy, keys := range heirs {
		result, ok := st[legacy]
		if !ok {
			continue
		}
		moveCheckinAlias(st, legacy, keys[0], result)
		changed = true
	}
	return changed
}

func moveCheckinAlias(st map[string]WorkBuddyCheckin, alias, key string, result WorkBuddyCheckin) {
	if canonical, exists := st[key]; exists {
		if !result.At.After(canonical.At) {
			result, canonical = canonical, result
		}
		result = keepCheckinClaim(result, canonical)
	}
	st[key] = result
	delete(st, alias)
}

func (heirs legacyCheckinKeys) expandOff(off []string) []string {
	if len(heirs) == 0 || !slices.ContainsFunc(off, func(key string) bool { return heirs[key] != nil }) {
		return off
	}
	var out []string
	for _, key := range off {
		keys := heirs[key]
		if len(keys) == 0 {
			keys = []string{key}
		}
		for _, key := range keys {
			if !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
	}
	return out
}

func (heirs legacyCheckinKeys) adoptOff(ctx context.Context) {
	if len(heirs) == 0 || ctx.Err() != nil {
		return
	}
	s, err := settings.LoadChecked()
	if err != nil || slices.Equal(s.CheckinOff, heirs.expandOff(s.CheckinOff)) || ctx.Err() != nil {
		return
	}
	if err := settings.EditCheckinOff(heirs.expandOff); err != nil {
		log.Printf("plugin check-in: %v", err)
	}
}

func SetCheckinAccountOn(ctx context.Context, key string, on bool) error {
	heirs := legacyPluginCheckinKeys(pluginCheckinProviders(ctx))
	if err := ctx.Err(); err != nil {
		return err
	}
	return settings.EditCheckinOff(func(off []string) []string {
		off = slices.DeleteFunc(heirs.expandOff(off), func(saved string) bool { return saved == key })
		if !on {
			off = append(off, key)
		}
		return off
	})
}

// pluginChecksIn says whether pp's plugin presses the check-in itself:
// magpie's own check-in through its fetch leaves its accounts to it.
func pluginChecksIn(pp plugin.Provider) bool { return pp.Checkin }

// PluginCheckinOn says whether a plugin provider's accounts are checked in
// each day: as the user set it for the provider, else as the built-in
// check-in of its vendor is set, which the plugin took over.
func PluginCheckinOn(s settings.Settings, id string) bool {
	if on, ok := s.PluginCheckins[id]; ok {
		return on
	}
	switch id {
	case wbCN.id:
		return s.WorkBuddyCheckin
	case TraeCNID:
		return s.TraeCheckin
	case MiniMaxCodeID, MiniMaxCodeGlobalID:
		return s.MiniMaxCheckin
	case "qoder", QoderCNID:
		return s.QoderCheckin
	}
	return false
}

// pluginRechecks says whether a plugin provider's check-in is asked again
// within the Beijing day, as magpie's own Qoder check-in is (recheck):
// a campaign can open mid-day (Qoder CN's ran 10:00 to 09:59 the next
// day), so a done or inactive read before then said nothing about the
// day's own and was kept all day, a press too (#2696). ZCode's gift
// plans open as campaigns too, some for under a day (plugins#503). Every
// other plugin is settled for the day.
func pluginRechecks(id string) bool { return id == "qoder" || id == QoderCNID || id == "zcode" }

func (p pluginCheckinProvider) checkiner(now func() time.Time) checkiner {
	return checkiner{path: pluginCheckinPath(), now: now, mu: &pluginCheckinMu, by: pluginCheckinBy(p.ID), label: p.ID, recheck: p.recheck || pluginRechecks(p.ID), accounts: func() []checkinAcct {
		var out []checkinAcct
		for _, a := range p.accounts {
			out = append(out, checkinAcct{User: a.User, On: a.On, key: a.key, provider: a.card, do: a.do})
		}
		return out
	}}
}

// pluginCheckiner checks in the plugins' accounts: of those whose
// provider only says, all of them when only is nil.
type pluginCheckiner struct {
	now  func() time.Time
	only func(id string) bool
}

func (c pluginCheckiner) clock() time.Time { return c.now() }

func (c pluginCheckiner) checkinNow(ctx context.Context, soon bool) []WorkBuddyCheckin {
	if discoveryOff() {
		return nil
	}
	var out []WorkBuddyCheckin
	providers := allPluginCheckinProviders(ctx)
	if ctx.Err() != nil {
		return nil
	}
	heirs := legacyPluginCheckinKeys(providers)
	for _, p := range providers {
		if c.only != nil && !c.only(p.ID) {
			continue
		}
		runner := p.checkiner(c.now)
		runner.legacy = heirs
		for _, r := range runner.checkinNow(ctx, soon) {
			r.Vendor = p.Name
			out = append(out, r)
		}
	}
	return out
}

// CheckInPlugins checks the accounts of each plugin that checks in in for
// today now, those not in yet; with ids, only those providers'. It says
// how each stands.
func CheckInPlugins(ctx context.Context, ids ...string) []WorkBuddyCheckin {
	c := pluginCheckiner{now: time.Now}
	if len(ids) > 0 {
		c.only = func(id string) bool {
			for _, x := range ids {
				if x == id {
					return true
				}
			}
			return false
		}
	}
	return c.checkinNow(ctx, true)
}

// PluginCheckin is a plugin's provider that checks in, for Settings: its
// switch and how each account's last check-in went.
type PluginCheckin struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	On       bool               `json:"on"`
	Checkins []WorkBuddyCheckin `json:"checkins,omitempty"`
}

// PluginCheckins are the plugins' providers that check in, each with its
// accounts' last check-ins as kept; asks nothing.
func PluginCheckins() []PluginCheckin {
	ps := allPluginCheckinProviders(context.Background())
	return pluginCheckins(ps, legacyPluginCheckinKeys(ps))
}

func pluginCheckins(ps []pluginCheckinProvider, legacy legacyCheckinKeys) []PluginCheckin {
	st := shownCheckins(pluginCheckinPath())
	legacy.moveHistory(st)
	s := settings.Load()
	var out []PluginCheckin
	for _, p := range ps {
		pc := PluginCheckin{ID: p.ID, Name: p.Name, On: PluginCheckinOn(s, p.ID)}
		listed := map[string]bool{}
		for _, a := range p.accounts {
			if r, ok := st[a.key]; ok && a.On && !listed[a.key] {
				listed[a.key] = true
				r.User, r.By, r.Vendor = a.User, pluginCheckinBy(p.ID), p.Name
				pc.Checkins = append(pc.Checkins, r)
			}
		}
		sort.SliceStable(pc.Checkins, func(i, j int) bool { return pc.Checkins[i].User < pc.Checkins[j].User })
		out = append(out, pc)
	}
	return out
}

// HasPluginCheckin says whether a plugin that checks in is signed in.
func HasPluginCheckin() bool { return len(allPluginCheckinProviders(context.Background())) > 0 }

// pluginCheckinMarks is qs with the cards of the plugins' accounts that
// check in marked, as WithCheckins marks the built-ins'.
func pluginCheckinMarks(ctx context.Context, qs []SubscriptionQuota) []SubscriptionQuota {
	ps := allPluginCheckinProviders(ctx)
	if len(ps) == 0 || ctx.Err() != nil {
		return qs
	}
	heirs := legacyPluginCheckinKeys(ps)
	st := shownCheckins(pluginCheckinPath())
	heirs.moveHistory(st)
	off := checkinOff(heirs)
	if ctx.Err() != nil {
		return qs
	}
	for _, p := range ps {
		var on []checkinCard
		for _, a := range p.accounts {
			if a.On {
				on = append(on, checkinCard{User: a.User, card: a.card, key: a.key})
			}
		}
		qs = markCheckinsWithOff(qs, on, st, pluginCheckinBy(p.ID), off)
	}
	return qs
}

// KeepPluginsCheckedIn checks the plugins' accounts in each day, each
// provider's while settings say to, as KeepWorkBuddyCheckedIn does
// WorkBuddy's.
func KeepPluginsCheckedIn(ctx context.Context) {
	c := pluginCheckiner{now: time.Now, only: func(id string) bool { return PluginCheckinOn(settings.Load(), id) }}
	keepCheckedIn(ctx, "plugin", c, func() bool { return true })
}
