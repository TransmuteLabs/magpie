package provider

// A daily check-in, whichever vendor's: WorkBuddy's (workbuddy_checkin.go)
// and Trae CN's (trae_checkin.go). Each account in use is checked in once
// a Beijing day, except Qoder's campaigns and ZCode's gift plans,
// rechecked every 30 minutes but for a captcha or own-app answer, which
// waits on the user, and an answer that may be the vendor's day before
// (provisional, #3326), asked again every 30 minutes until a claim or an answer from noon on.
// The loop asks for a day from the time of day the user set on
// (settings.CheckinAt, checkinGate), else as the day turns.
// A press asks the vendor again for a done, inactive or ineligible one.
// What came of it is kept in the vendor's file by its key for reuse
// (see workbuddy_checkin.go for when a failure is tried again).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/settings"
)

// checkinAcct is an account checked in each day.
type checkinAcct struct {
	User     string
	On       bool
	key      string // its entry in the file; "" for one that can't be told apart
	provider string // the provider its card is under, which the user can switch off
	do       func(context.Context) WorkBuddyCheckin
}

// checkiner checks the accounts it lists in, keeping what came of it at
// path; mu keeps two check-ins (the loop and a press) from asking for one
// account at once.
type checkiner struct {
	path     string
	now      func() time.Time
	mu       *sync.Mutex
	by       string // the vendor, WorkBuddyCheckin.By; "" for WorkBuddy
	label    string // the vendor in the log
	recheck  bool   // its campaigns can change before the Beijing day ends
	accounts func() []checkinAcct
	legacy   legacyCheckinKeys
	// only, of a press for one account (CheckInAccount), is its key: the
	// others are neither asked nor said, and it is asked even while left
	// out of the daily check-in, whose switch it leaves as it is
	only string
}

// checkinRunner is a check-in the daily loop runs.
type checkinRunner interface {
	checkinNow(ctx context.Context, soon bool) []WorkBuddyCheckin
	clock() time.Time
}

func (c checkiner) clock() time.Time { return c.now() }

// checkinNow checks each account in use in for today that isn't yet, and
// keeps what came of it; soon, a failure is tried again without waiting
// out wbCheckinRetry. It says how every account stands, those it didn't
// ask as last kept. An empty outcome represents a call with no observation.
func (c checkiner) checkinNow(ctx context.Context, soon bool) []WorkBuddyCheckin {
	if discoveryOff() || filelock.LockMutex(ctx, c.mu) != nil {
		return nil
	}
	defer c.mu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	now := c.now()
	day := wbCheckinDay(now)
	snapshot, err := readCheckins(c.path)
	if err != nil {
		// unknown, never empty: nothing is asked or written, the file
		// is left as it is, and each account says why
		return c.unread(now, err)
	}
	var out []WorkBuddyCheckin
	previous := snapshot
	migrated := false
	if len(c.legacy) > 0 {
		previous = maps.Clone(snapshot)
		migrated = c.legacy.moveHistory(previous)
	}
	var observations map[string]WorkBuddyCheckin
	// one account signed in twice (the built-in and the plugin) is
	// checked in once
	asked := map[string]bool{}
	// and an account the user left out is not asked at all, by the loop
	// or a press (#1902)
	off := checkinOff(c.legacy)
	gate := checkinGate(now)
	for _, a := range c.accounts() {
		if ctx.Err() != nil {
			break
		}
		if !a.On || a.key == "" || asked[a.key] || !c.reaches(a.key, off) {
			continue
		}
		// nor, by the loop, one whose provider is switched off or removed
		// (leftAlone); a press still checks it in
		if !soon && leftAlone(a.provider) {
			continue
		}
		asked[a.key] = true
		prev, seen := previous[a.key]
		// the loop asks for a day not yet asked from the user's time of
		// day on (checkinGate); a press asks at once
		if !soon && (!seen || prev.Day != day) && now.Before(gate) {
			if seen {
				prev.User, prev.By = a.User, c.by
				out = append(out, prev.shown())
			}
			continue
		}
		settled := prev.settled(day, now, soon)
		// a press asks the vendor again for a done, inactive or
		// ineligible magpie saw no claim for: answered from the record,
		// a press never reached the vendor (#3326). A claim, and an
		// answer the user acts on (own-app, captcha), stand.
		if soon && prev.unclaimedAnswer() {
			settled = false
		}
		// and the loop asks again an answer that may be the vendor's
		// day before (#3326), unless the vendor said when its day turns
		// (ValidUntil, held to in settled)
		if settled && prev.provisional() && prev.ValidUntil.IsZero() {
			settled = now.Sub(prev.At) < wbCheckinEvery
		}
		if c.recheck {
			// a captcha or own-app answer waits on the user, in the
			// vendor's app, so the loop leaves it for the day: asked
			// again every 30 minutes, ZCode's claim only answered 3007
			// again, and the reporter's accounts went on to 3012
			// (plugins#503). A press still asks.
			// A failure held to its until waits it out (heldUntil).
			waits := prev.Outcome == CheckinCaptcha || prev.Outcome == CheckinOwnApp || now.Before(prev.heldUntil())
			settled = settled && !soon && (waits || now.Sub(prev.At) < wbCheckinEvery)
			if prev.Checked() && !prev.ValidUntil.IsZero() {
				settled = settled && now.Before(prev.ValidUntil)
			}
		}
		if seen && settled {
			prev.User, prev.By = a.User, c.by
			out = append(out, prev.shown())
			continue
		}
		// through the account's own proxy, else its provider's, as its
		// usage is read (loginQuota), never the global one past them: a
		// provider set to direct whose check-in went out through the
		// global proxy was refused there (#2777)
		r := a.do(withCheckinDay(ViaLogin(ctx, a.provider, a.User), day))
		if r.Outcome == "" {
			continue
		}
		r.Day, r.At = day, now
		if r.Offline && seen && prev.Day == day && prev.Offline {
			r.Tries = prev.Tries + 1
		}
		if r.Outcome == CheckinClaimed {
			r.Claimed = now
		}
		r = keepCheckinClaim(r, prev)
		if observations == nil {
			observations = map[string]WorkBuddyCheckin{}
		}
		observations[a.key] = r
		r.User, r.By, r.Asked = a.User, c.by, true
		out = append(out, r.shown())
	}
	if len(observations) > 0 || migrated && ctx.Err() == nil {
		c.publishCheckins(ctx, snapshot, previous, observations)
	}
	c.legacy.adoptOff(ctx)
	return out
}

func (c checkiner) publishCheckins(ctx context.Context, snapshot, previous, observations map[string]WorkBuddyCheckin) {
	publication := ctx
	if len(observations) > 0 {
		publication = context.WithoutCancel(ctx)
	}
	lock := filelock.Mutex{Path: func() string { return c.path + ".lock" }}
	if !lock.LockCheckedContext(publication) {
		if publication.Err() == nil {
			log.Printf("%s check-in: %s lock unavailable", c.label, c.path)
		}
		return
	}
	defer lock.Unlock()
	current, err := readCheckins(c.path)
	if err != nil {
		return
	}
	changed := false
	migrate := ctx.Err() == nil
	for alias, keys := range c.legacy {
		aliased, exists := current[alias]
		if !exists {
			continue
		}
		key := keys[0]
		if original, seen := snapshot[alias]; !seen || !reflect.DeepEqual(original, aliased) {
			if observed, asked := observations[key]; asked && !observed.At.After(aliased.At) {
				delete(observations, key)
			}
			continue
		}
		if migrate {
			moveCheckinAlias(current, alias, key, aliased)
			changed = true
		}
	}
	for key, observed := range observations {
		if latest, exists := current[key]; exists {
			if latest.At.After(observed.At) || latest.At.Equal(observed.At) && !reflect.DeepEqual(latest, previous[key]) {
				withClaim := keepCheckinClaim(latest, observed)
				if !withClaim.Claimed.Equal(latest.Claimed) {
					current[key] = withClaim
					changed = true
				}
				continue
			}
			observed = keepCheckinClaim(observed, latest)
		}
		current[key] = observed
		changed = true
	}
	if !changed {
		return
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err == nil {
		data = append(data, '\n')
		if publication.Done() == nil {
			err = writePrivate(c.path, data)
		} else {
			err = writePrivateQuietChecked(c.path, data, publication.Err)
			if err == nil {
				filememo.Forget()
			}
		}
	}
	if err != nil {
		if publication.Err() == nil {
			log.Printf("%s check-in: %v", c.label, err)
		}
		return
	}
	wroteCheckins(c.path, current)
}

func keepCheckinClaim(result, previous WorkBuddyCheckin) WorkBuddyCheckin {
	if previous.claimedOn(result.Day) {
		claimed := cmp.Or(previous.Claimed, previous.At)
		kept := result.Claimed
		if result.Outcome == CheckinClaimed {
			kept = cmp.Or(kept, result.At)
		}
		if kept.Before(claimed) {
			result.Claimed = claimed
		}
	}
	return result
}

func checkinReply(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, []byte, error) {
	if err := req.Context().Err(); err != nil {
		return nil, nil, err
	}
	res, err := send(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res, body, err
}

func checkinRequestFailed(ctx context.Context, err error) WorkBuddyCheckin {
	if errors.Is(err, ctx.Err()) {
		return WorkBuddyCheckin{}
	}
	return wbCheckinFailed(err)
}

// checkinGate is when the loop first asks for the Beijing day now is on:
// the first time of day settings.CheckinAt names, on now's clock (the
// computer's zone), at or after the day's start; the start when none is
// set (as before: as the day turns). Every Beijing day holds one, so a
// time set never skips one; on a day a clock change leaves without it,
// the start.
func checkinGate(now time.Time) time.Time {
	t := now.In(beijing)
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, beijing)
	h, m, ok := settings.Clock(settings.Shared().CheckinAt)
	if !ok {
		return start
	}
	l := start.In(now.Location())
	at := time.Date(l.Year(), l.Month(), l.Day(), h, m, 0, 0, l.Location())
	if at.Before(start) {
		at = time.Date(l.Year(), l.Month(), l.Day()+1, h, m, 0, 0, l.Location())
	}
	if !at.Before(start.AddDate(0, 0, 1)) {
		return start
	}
	return at
}

// unread is how the accounts checkinNow would look at stand while its file
// can't be read (err, logged by readCheckins): each failed with err, at
// now, so the loop tries again after wbCheckinRetry and a press says why;
// none was asked.
func (c checkiner) unread(now time.Time, err error) []WorkBuddyCheckin {
	var out []WorkBuddyCheckin
	listed := map[string]bool{}
	off := checkinOff(c.legacy)
	for _, a := range c.accounts() {
		if !a.On || a.key == "" || listed[a.key] || !c.reaches(a.key, off) {
			continue
		}
		listed[a.key] = true
		out = append(out, WorkBuddyCheckin{User: a.User, By: c.by, Day: wbCheckinDay(now), At: now, Outcome: CheckinFailed, Msg: err.Error()})
	}
	return out
}

// reaches says whether a run of c asks the account at key: one the user
// left out (off) only by a press for it alone (only).
func (c checkiner) reaches(key string, off map[string]bool) bool {
	if c.only != "" {
		return key == c.only
	}
	return !off[key]
}

// checkinOff are the accounts left out of their vendor's daily check-in
// (settings.CheckinOff), by their entry in its file.
func checkinOff(legacy legacyCheckinKeys) map[string]bool {
	off := map[string]bool{}
	for _, k := range legacy.expandOff(settings.Load().CheckinOff) {
		off[k] = true
	}
	return off
}

// CheckinAccount is an account a daily check-in reaches, for Settings: its
// entry in its vendor's file (Key, which settings.CheckinOff names), the
// vendor (By, as WorkBuddyCheckin's), and whether it is checked in while
// its vendor's check-in is on (#1902, magicLaLa: 根据每个账号设置是否开启
// 自动签到).
type CheckinAccount struct {
	Key  string `json:"key"`
	By   string `json:"by"`
	User string `json:"user"`
	On   bool   `json:"on"`
}

// CheckinAccounts are the accounts in use that each vendor's check-in
// reaches, WorkBuddy's, Trae CN's, MiniMax Code's, Qoder's and the
// plugins' that check in themselves, each once; asks nothing.
func CheckinAccounts() []CheckinAccount {
	cs, legacy := checkiners()
	return checkinAccounts(cs, legacy)
}

// CheckinSettingsContext reads the providers once for the plugin and account
// rows of the same Settings response.
func CheckinSettingsContext(ctx context.Context) ([]PluginCheckin, []CheckinAccount) {
	providers := allPluginCheckinProviders(ctx)
	if ctx.Err() != nil {
		return nil, nil
	}
	legacy := legacyPluginCheckinKeys(providers)
	accounts := checkinAccounts(checkinersFor(ctx, providers, legacy), legacy)
	if ctx.Err() != nil {
		return nil, nil
	}
	return pluginCheckins(providers, legacy), accounts
}

func checkinAccounts(cs []checkiner, legacy legacyCheckinKeys) []CheckinAccount {
	off := checkinOff(legacy)
	var out []CheckinAccount
	seen := map[string]bool{}
	for _, c := range cs {
		var mine []CheckinAccount
		for _, a := range c.accounts() {
			if !a.On || a.key == "" || seen[c.by+"\x00"+a.key] {
				continue
			}
			seen[c.by+"\x00"+a.key] = true
			mine = append(mine, CheckinAccount{Key: a.key, By: c.by, User: a.User, On: !off[a.key]})
		}
		sort.SliceStable(mine, func(i, j int) bool { return mine[i].User < mine[j].User })
		out = append(out, mine...)
	}
	return out
}

// checkiners are every vendor's check-in: WorkBuddy's, Trae CN's, MiniMax
// Code's, Qoder's and each plugin's that checks in itself, with its name
// (a plugin's provider's, for WorkBuddyCheckin.Vendor; "" for a built-in).
func checkiners() ([]checkiner, legacyCheckinKeys) {
	cs, _, legacy := namedCheckiners(context.Background())
	return cs, legacy
}

func namedCheckiners(ctx context.Context) ([]checkiner, []string, legacyCheckinKeys) {
	providers := allPluginCheckinProviders(ctx)
	legacy := legacyPluginCheckinKeys(providers)
	cs := checkinersFor(ctx, providers, legacy)
	names := make([]string, len(cs)-len(providers), len(cs))
	for _, provider := range providers {
		names = append(names, provider.Name)
	}
	return cs, names, legacy
}

func checkinersFor(ctx context.Context, providers []pluginCheckinProvider, legacy legacyCheckinKeys) []checkiner {
	workbuddy := newWBCheckiner()
	workbuddy.accounts = func() []wbAccount { return wbCheckinAccountsContext(ctx) }
	cs := []checkiner{
		workbuddy.checkiner(),
		newTraeCheckiner(func() []traeAccount { return traeCheckinAccountsContext(ctx) }),
		newMiniMaxCheckiner(func() []miniMaxAccount { return miniMaxCheckinAccountsContext(ctx) }),
		newQoderCheckiner(func() []qoderCheckinAcct { return qoderCheckinAccountsContext(ctx) }, time.Now),
	}
	for _, p := range providers {
		runner := p.checkiner(time.Now)
		runner.legacy = legacy
		cs = append(cs, runner)
	}
	return cs
}

// CheckInAccount checks one account in for today now, as a press of Check
// in now does for its vendor's (CheckinAccount's By and Key): asked again
// unless claimed today, even while left out of the daily check-in, and
// no switch changed (xiaowu_95660: 手动签到一次). It says how the account
// stands, or nothing for an account no check-in reaches.
func CheckInAccount(ctx context.Context, by, key string) []WorkBuddyCheckin {
	if key == "" || ctx.Err() != nil {
		return nil
	}
	cs, names, _ := namedCheckiners(ctx)
	if ctx.Err() != nil {
		return nil
	}
	for i, c := range cs {
		if c.by != by {
			continue
		}
		c.only = key
		rs := c.checkinNow(ctx, true)
		for j := range rs {
			if names[i] != "" {
				rs[j].Vendor = names[i]
			}
		}
		return rs
	}
	return nil
}
