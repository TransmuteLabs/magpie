package provider

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// A plugin's provider is signed in to as many times as the user likes, as
// a built-in subscription is: plugin-auth.json keeps each account's
// sign-in (under the provider's id, then id#slot), logins.json which is
// first and which are on, as it does for the built-ins, the account's key
// in its Home.

// pluginAgent is what logins.json lists a plugin provider's accounts as.
func pluginAgent(pp plugin.Provider) string { return "plugin:" + pp.ID }

// publicPlugin is whether every way to sign in to the plugin's provider is
// keyless, its credential one the plugin gives whatever is typed: OpenCode
// Zen Free's shared "public". Such an account is nobody's, so with
// subscription accounts off (#1535, discovery.go) it is still listed and
// served. Before, Zen Free signed in all the same with them off, the
// Plugins page said so, and magpie listed it nowhere (lileo0668 on
// Discord: "signed in, but magpie can't list it").
func publicPlugin(pp plugin.Provider) bool {
	if len(pp.Methods) == 0 {
		return false
	}
	for _, m := range pp.Methods {
		if m.Type != "api" || !m.Keyless {
			return false
		}
	}
	return true
}

// publicLogins are a public plugin's accounts while subscription accounts
// are off, read from the plugin alone: logins.json and plugin-auth.json
// are left unread then (discovery.go), so the first is in use and every
// one is on.
func publicLogins(pp plugin.Provider) []pluginLogin {
	agent, labels := pluginAgent(pp), pluginLabels(pp, nil)
	var out []pluginLogin
	for _, a := range pp.Accounts {
		saved := savedLogin{Agent: agent, User: labels[a.Key], Home: a.Key, On: true}
		out = append(out, pluginLogin{sideLogin{Login{Agent: agent, User: saved.User, On: true}, saved}, a})
	}
	if len(out) > 0 {
		out[0].Active = true
	}
	return out
}

// publicAccount is whether p is a public plugin's provider (publicPlugin),
// served while subscription accounts are off.
func (p Provider) publicAccount() bool {
	if p.IsExtension() {
		return !p.Account.extension.manifest.Contract.Has("account-auth")
	}
	return p.IsPlugin() && publicPlugin(*p.Account.plugin)
}

// pluginLabels names each account: by named (the agent's own, as the
// agent is signed in now), else its id, else the end of its key, else what
// it signed in with; a name two share gets the account's slot. The slot
// goes on after the own name, so the own account never reads as another
// signed in to the same one (dev on Discord: ZCode's, its id "ZCode",
// named as one magpie signed in, and Remove on that one took the own).
func pluginLabels(pp plugin.Provider, named map[string]string) map[string]string {
	out := map[string]string{}
	seen := map[string]int{}
	for _, a := range pp.Accounts {
		l := named[a.Key]
		if l == "" {
			l = a.AccountID
		}
		if l == "" && a.Hint != "" {
			l = "API key …" + a.Hint
		}
		if l == "" {
			l = map[string]string{"api": "API key", "oauth": "Signed in"}[a.Type]
		}
		if l == "" {
			l = "Signed in"
		}
		out[a.Key] = l
		seen[strings.ToLower(l)]++
	}
	for _, a := range pp.Accounts {
		if seen[strings.ToLower(out[a.Key])] > 1 {
			if _, slot, ok := strings.Cut(a.Key, "#"); ok {
				out[a.Key] += " (" + slot + ")"
			}
		}
	}
	return out
}

// pluginNames is pluginLabels as the accounts list names them, the
// agent's own as the agent is signed in now.
func pluginNames(pp plugin.Provider) map[string]string {
	_, named := pluginOwnNames(pp)
	return pluginLabels(pp, named)
}

// pluginOwnNames tells the provider's accounts that are the agent's own,
// and names them as the agent is signed in now, where that can be read.
func pluginOwnNames(pp plugin.Provider) (own map[string]bool, named map[string]string) {
	own, named = map[string]bool{}, map[string]string{}
	listed := map[string]bool{}
	for _, a := range pp.Accounts {
		listed[a.Key] = true
	}
	for k, a := range plugin.Auths(pp.ID) {
		if listed[k] && pluginOwn(pp.ID, a) {
			own[k] = true
		}
	}
	if len(own) > 0 {
		if u := pluginOwnUser(pp.ID); u != "" {
			for k := range own {
				named[k] = u
			}
		}
	}
	return own, named
}

type pluginLogin struct {
	sideLogin
	acct plugin.Account
}

// cliMarked says a sign-in carries the "cli" mark of the agent's own
// sign-in: true as a mover wrote it before, "true" as the Devin plugin
// writes it since 0.1.24 (plugins #396).
func cliMarked(md map[string]any) bool {
	return md["cli"] == true || md["cli"] == "true"
}

// pluginOwn says a plugin sign-in is the agent's own (its CLI's, its
// app's), which the plugin reads where the agent keeps it. The movers mark
// it so (migrate_side.go, migrate_kiro.go, migrate_workbuddy.go,
// migrate_zcode.go), as the plugins' own "CLI's sign-in" ways do.
func pluginOwn(id string, auth map[string]any) bool {
	md, _ := auth["metadata"].(map[string]any)
	switch id {
	case "devin", CommandCodePlanID:
		return cliMarked(md)
	case "grok":
		return samePath(str(auth["refresh"]), GrokHome())
	case "cursor":
		return str(auth["refresh"]) == cursorCLIMark
	case "kiro", "workbuddy", WorkBuddyAIID:
		return str(auth["source"]) != ""
	case "zcode":
		var s struct{ Source string }
		_ = json.Unmarshal([]byte(str(auth["refresh"])), &s)
		return s.Source == "zcode"
	}
	return false
}

// pluginOwnUser is who the agent itself is signed in to now, "" when it
// can't be told: the own account is named after it, as the built-in's
// was, so the name follows a switch made in the agent.
var pluginOwnUser = func(id string) string {
	switch id {
	case "devin":
		if u, _, ok := devinIdentity(); ok {
			return u
		}
	case "grok":
		if c, ok := readGrokCredential(GrokHome()); ok {
			return c.Email
		}
	case CommandCodePlanID:
		if u, _, ok := cmdOwn(); ok {
			return u
		}
	case "cursor":
		if CursorExecutable() != "" && !cursorSignedOut() {
			if u, _, ok := cursorIdentity(); ok {
				return u
			}
		}
	case "kiro":
		return kiroOwnUser()
	case "zcode":
		if u, _, ok := zcodeOwn(); ok {
			return u
		}
	case "workbuddy", WorkBuddyAIID:
		if u, _, ok := wbOwn(wbSiteOf(id)); ok {
			return u
		}
	}
	return ""
}

// pluginLoginLocks counts the times pluginLogins took loginsMu (a test
// reads it).
var pluginLoginLocks atomic.Int64

// pluginLogins are the provider's accounts, the first in use first. What
// plugin-auth.json keeps is the truth: logins.json follows it. The agent's
// own account is named as the agent is signed in now; removed in magpie,
// it is only hidden, and shows again once the agent signs in anew.
func pluginLogins(pp plugin.Provider) []pluginLogin {
	return pluginLoginsContext(context.Background(), pp)
}

func pluginLoginsContext(ctx context.Context, pp plugin.Provider) []pluginLogin {
	return pluginLoginsCheckedContext(ctx, pp, ctx.Err)
}

func pluginLoginsCheckedContext(ctx context.Context, pp plugin.Provider, check func() error) []pluginLogin {
	if check() != nil {
		return nil
	}
	if settings.AccountDiscoveryOff() {
		if publicPlugin(pp) {
			return publicLogins(pp)
		}
		return nil
	}
	agent := pluginAgent(pp)
	byKey := map[string]plugin.Account{}
	for _, a := range pp.Accounts {
		byKey[a.Key] = a
	}
	own, named := pluginOwnNames(pp)
	mark := ""
	if len(own) > 0 {
		mark = ownMark(pp.ID)
	}
	labels := pluginLabels(pp, named)
	// looked at as kept, and copied only at the first change: nearly
	// every list changes nothing, and a page's state lists each plugin's
	settle := func(ls []savedLogin, saving bool) ([]savedLogin, bool) {
		changed := false
		have := map[string]bool{}
		var keep []savedLogin
		for i, l := range ls {
			drop, edit := false, false
			if l.Agent == agent {
				if _, ok := byKey[l.Home]; !ok || have[l.Home] {
					drop = true // signed out, or listed twice
				} else {
					have[l.Home] = true
					renamed := l.User != labels[l.Home]
					if renamed {
						l.User, edit = labels[l.Home], true
					}
					// the agent's own, hidden, shows again once the agent signs
					// in anew: told by its sign-in's mark, else by another account
					if l.Hidden != "" && own[l.Home] && (mark != "" && mark != l.Hidden || mark == "" && l.Hidden == hiddenNoMark && renamed) {
						l.Hidden, edit = "", true
					}
				}
			}
			if (drop || edit) && !changed {
				keep, changed = slices.Clone(ls[:i:i]), true
			}
			if changed && !drop {
				keep = append(keep, l)
			}
		}
		if changed {
			ls = keep
		}
		for _, a := range pp.Accounts {
			if !have[a.Key] {
				if !changed {
					ls, changed = slices.Clone(ls), true
				}
				login := savedLogin{Agent: agent, User: labels[a.Key], Home: a.Key, On: true, Seen: time.Now().UTC().Truncate(time.Second)}
				if saving {
					login.PluginObservations.Epoch = rand.Text()
				}
				ls = append(ls, login)
			}
		}
		return ls, changed
	}
	// what nothing changes takes no lock: taken, it opens logins.json's
	// lock file, across magpies, and /api/providers lists every plugin
	// (a fifth of its CPU). A change is made from a read under it.
	if _, changed := settle(sharedLogins(), false); changed {
		pluginLoginLocks.Add(1)
		withPluginLogins(ctx, pp.ID, func() {
			if check() != nil {
				return
			}
			if ls, changed := settle(sharedLogins(), true); changed {
				_ = writeLoginsChecked(ls, true, check)
			}
		})
	}
	var out []pluginLogin
	for _, l := range sideLogins(agent, "", func(l savedLogin) bool { _, ok := byKey[l.Home]; return ok && l.Hidden == "" }) {
		l.Lapsed = l.saved.Lapsed // a refused sign-in shows on the account
		l.Own = own[l.saved.Home]
		out = append(out, pluginLogin{l, byKey[l.saved.Home]})
	}
	return out
}

func pluginSide(ctx context.Context, pp plugin.Provider) []sideLogin {
	var out []sideLogin
	for _, l := range pluginLoginsContext(ctx, pp) {
		out = append(out, l.sideLogin)
	}
	return out
}

// pluginOfAgent is the plugin provider the accounts page names by its
// magpie id.
func pluginOfAgent(agent string) (plugin.Provider, bool) {
	return pluginOfAgentContext(context.Background(), agent)
}

func pluginOfAgentContext(ctx context.Context, agent string) (plugin.Provider, bool) {
	if strings.HasPrefix(agent, "plugin:") {
		id := strings.TrimPrefix(agent, "plugin:")
		for _, pp := range heldPluginsContext(ctx) {
			if pp.ID == id {
				return pp, true
			}
		}
		return plugin.Provider{}, false
	}
	return pluginOfContext(ctx, agent)
}

func pluginLoginList(ctx context.Context, pp plugin.Provider) []Login {
	return loginsOf(pluginSide(ctx, pp))
}

func switchPluginLogin(pp plugin.Provider, user string) error {
	return switchSideLogin(pluginAgent(pp), user, pluginSide(context.Background(), pp))
}

func setPluginLoginOn(pp plugin.Provider, user string, on bool) error {
	return setSideLoginOn(pluginAgent(pp), user, on, pluginSide(context.Background(), pp))
}

// forgetPluginLogin signs the account out: its sign-in is magpie's own.
// The agent's own is only hidden, as the built-in hid it: the agent stays
// signed in, and the account shows again once it signs in anew. Qoder's
// first goes as the built-in's did, the next put first.
func forgetPluginLogin(pp plugin.Provider, user string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := plugin.GuardCommunitySubscription(ctx, pp.ID)
	if err != nil {
		return err
	}
	defer release()
	agent := pluginAgent(pp)
	ls := pluginSide(ctx, pp)
	i := slices.IndexFunc(ls, func(l sideLogin) bool { return strings.EqualFold(l.User, user) })
	if i < 0 {
		return fmt.Errorf("no %s account %q", pp.Name, user)
	}
	own, first := ls[i].Own, ls[i].Active
	if !own && !(first && firstGoes(pp.ID)) {
		var err error
		ferr := forgetSideLogin(agent, user, ls, func(l savedLogin) { err = signOutPlugin(pp, l.Home) })
		if ferr != nil {
			return ferr
		}
		return err
	}
	next := ""
	if first {
		for _, l := range ls {
			if !strings.EqualFold(l.User, user) {
				next = l.User
				break
			}
		}
	}
	key := ls[i].saved.Home
	isNext := listedMatch(agent, next, ls)
	err = editListedLogin(agent, user, ls, func(saved []savedLogin, j int) ([]savedLogin, error) {
		n := -1
		if next != "" {
			n = pickSaved(saved, isNext)
		}
		for k := range saved {
			if saved[k].Agent == agent && (first || k == j) {
				saved[k].First = k == n
			}
		}
		if own {
			saved[j].Hidden = firstNonEmpty(ownMark(pp.ID), hiddenNoMark)
			return saved, nil
		}
		return append(saved[:j], saved[j+1:]...), nil
	})
	if err != nil || own {
		return err
	}
	return signOutPlugin(pp, key)
}

// firstGoes says the account in use first can be removed, the next taking
// its place, as the built-in let it (Qoder's).
func firstGoes(id string) bool { return id == "qoder" || id == QoderCNID }

func signOutPlugin(pp plugin.Provider, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return plugin.SignOut(ctx, pp.ID, key)
}

// pluginAlsoOn is the provider's accounts in use behind the first.
func pluginAlsoOn(pp plugin.Provider) []Provider {
	var out []Provider
	for _, l := range pluginLogins(pp) {
		if !l.Active && l.On {
			out = append(out, pluginProvider(pp, l))
		}
	}
	return out
}

// withPluginLogins holds community ownership through a login edit and
// checks the SDK store before taking loginsMu.
func withPluginLogins(ctx context.Context, provider string, edit func()) {
	release, err := plugin.GuardCommunitySubscription(ctx, provider)
	if err != nil {
		return
	}
	defer release()
	if !loginsMu.LockCheckedContext(ctx) {
		return
	}
	defer loginsMu.Unlock()
	if ctx.Err() == nil {
		edit()
	}
}

// PluginAccountUser is who the plugin id's account at key is (an email, a
// login), as the Routing trace and the request log name it: "" when no
// account is kept there.
func PluginAccountUser(id, key string) string {
	for _, l := range readLogins() {
		if l.Agent == "plugin:"+id && l.Home == key {
			return l.User
		}
	}
	return ""
}

// notePluginLapse marks the account at key lapsed when its vendor refused
// a request (401), as a built-in's refused sign-in is marked, and clears
// the mark once one goes through.
func notePluginLapse(ctx context.Context, pp plugin.Provider, auth plugin.FetchAuth, status int, explicitStatus bool) {
	if ctx.Err() != nil || !pluginLoginStatus(status) {
		return
	}
	key := auth.Account
	agent := pluginAgent(pp)
	for _, login := range sharedLogins() {
		if login.Agent != agent || login.Home != key {
			continue
		}
		ref := pluginLoginReadKey{loginsPath(), agent, key}
		pluginLoginReads.Lock()
		active := pluginLoginReads.active[ref]
		pluginLoginReads.Unlock()
		lapse := ""
		if status == http.StatusUnauthorized {
			lapse = lapsedText(pp, login.User)
		}
		if !explicitStatus && active == nil && login.Lapsed == lapse {
			return
		}
		notePluginLogin(ctx, pp.ID, key, pluginLoginObservation{
			status: status, explicitStatus: explicitStatus, loginID: loginID(login), epoch: login.PluginObservations.Epoch,
			authEpoch: auth.Epoch, active: active,
			text: func(user string) string { return lapsedText(pp, user) },
		})
		return
	}
}

type pluginLoginOrder struct {
	Revision uint64    `json:"revision,omitempty"`
	Started  time.Time `json:"started,omitzero"`
}

func (order pluginLoginOrder) after(saved pluginLoginOrder) bool {
	return order.Revision > saved.Revision || order.Revision == saved.Revision && order.Started.After(saved.Started)
}

type pluginLoginObservations struct {
	Epoch    string           `json:"epoch,omitempty"`
	Revision uint64           `json:"revision,omitempty"`
	SignIn   pluginLoginOrder `json:"signIn,omitzero"`
	Plan     pluginLoginOrder `json:"plan,omitzero"`
}

type pluginLoginReadKey struct {
	path, agent, account string
}

type pluginLoginReadSet struct {
	count    int
	answered time.Time
}

var pluginLoginReads struct {
	sync.Mutex
	active map[pluginLoginReadKey]*pluginLoginReadSet
}

type pluginLoginObservation struct {
	order          pluginLoginOrder
	status         int
	plan           string
	loginID        string
	epoch          string
	authEpoch      *string
	signedIn       bool
	explicitStatus bool
	active         *pluginLoginReadSet
	text           func(user string) string
}

func beginPluginLoginRead(login savedLogin) (pluginLoginObservation, func()) {
	key := pluginLoginReadKey{loginsPath(), login.Agent, login.Home}
	pluginLoginReads.Lock()
	if pluginLoginReads.active == nil {
		pluginLoginReads.active = map[pluginLoginReadKey]*pluginLoginReadSet{}
	}
	active := pluginLoginReads.active[key]
	if active == nil {
		active = &pluginLoginReadSet{}
		pluginLoginReads.active[key] = active
	}
	active.count++
	began := time.Now()
	pluginLoginReads.Unlock()
	observation := pluginLoginObservation{
		order:   pluginLoginOrder{Revision: login.PluginObservations.Revision, Started: began},
		loginID: loginID(login),
		epoch:   login.PluginObservations.Epoch,
		active:  active,
	}
	return observation, func() {
		pluginLoginReads.Lock()
		active.count--
		if active.count == 0 {
			delete(pluginLoginReads.active, key)
		}
		pluginLoginReads.Unlock()
	}
}

func pluginLoginStatus(status int) bool {
	return status == http.StatusUnauthorized || status >= 200 && status <= 299
}

func (observation pluginLoginObservation) recordsLogin() bool {
	return pluginLoginStatus(observation.status) || observation.plan != "" || observation.signedIn
}

func notePluginLogin(ctx context.Context, provider, key string, observation pluginLoginObservation) {
	if !observation.recordsLogin() || settings.AccountDiscoveryOff() {
		return
	}
	withPluginLogins(ctx, provider, func() {
		check := func() error {
			off, err := currentUsageDiscovery(ctx)
			if err == nil && off {
				err = errAccountDiscoveryOff
			}
			return err
		}
		if check() != nil {
			return
		}
		agent := "plugin:" + provider
		logins, err := readCurrentLogins()
		if err != nil {
			return
		}
		for index, login := range logins {
			if login.Agent != agent || login.Home != key {
				continue
			}
			if observation.loginID != "" && (loginID(login) != observation.loginID || login.PluginObservations.Epoch != observation.epoch) {
				continue
			}
			publish := func() error { return recordPluginLogin(logins, index, observation, check) }
			if observation.authEpoch != nil {
				_ = plugin.WithAuthEpoch(ctx, key, *observation.authEpoch, publish)
			} else {
				_ = publish()
			}
			return
		}
	})
}

func recordPluginLogin(logins []savedLogin, index int, observation pluginLoginObservation, check func() error) error {
	login := logins[index]
	current := login
	orders := &current.PluginObservations
	order := observation.order
	immediate := order.Started.IsZero()
	if immediate {
		order.Revision = orders.Revision + 1
	} else if order.Revision > orders.Revision {
		return check()
	}
	if pluginLoginStatus(observation.status) && order.after(orders.SignIn) &&
		(immediate || !order.Started.Before(observation.active.answered)) {
		current.Lapsed = ""
		if observation.status == http.StatusUnauthorized {
			current.Lapsed = observation.text(login.User)
		}
		if !immediate || observation.signedIn || observation.explicitStatus || current.Lapsed != login.Lapsed {
			orders.SignIn = order
		}
	}
	if observation.plan != "" && order.after(orders.Plan) {
		orders.Plan = order
		current.Plan = observation.plan
	}
	if observation.signedIn {
		orders.Plan = order
		orders.Epoch = rand.Text()
		current.Hidden = ""
	}
	if *orders != login.PluginObservations {
		orders.Revision++
		changed := current.Lapsed != login.Lapsed || current.Plan != login.Plan || current.Hidden != login.Hidden ||
			orders.Epoch != login.PluginObservations.Epoch
		next := slices.Clone(logins)
		next[index] = current
		if err := writeLoginsChecked(next, changed, check); err != nil {
			return err
		}
	} else if err := check(); err != nil {
		return err
	}
	if immediate && observation.active != nil && check() == nil {
		observation.active.answered = time.Now()
	}
	return nil
}

// lapsedText is what a plugin account the vendor refused says.
func lapsedText(pp plugin.Provider, user string) string {
	name, _ := pluginCard(pp)
	return user + "'s " + name + " sign-in has expired — sign in again"
}

// clearPluginLapse takes the mark off an account signed in again, and
// brings back the agent's own account removed in magpie, as signing in to
// the built-in's did (#320). The account's kept usage reading goes too: it
// was read under the old sign-in, and for a minute it would answer the
// next reading with the old sign-in's "expired" without asking the plugin,
// and so without marking or clearing anything. The mark goes by the
// account alone: the providers' list is empty while another magpie's
// change to plugins.json is read again, and a sign-in finished then left
// the account saying "sign in again" (#2748).
func clearPluginLapse(saved plugin.Saved) {
	observation := pluginLoginObservation{status: http.StatusOK, signedIn: true}
	if saved.AuthEpoch != "" {
		observation.authEpoch = &saved.AuthEpoch
	}
	notePluginLogin(context.Background(), saved.Provider, saved.Account, observation)
	for _, pp := range plugin.Cached() {
		if pp.ID == saved.Provider {
			if user := pluginNames(pp)[saved.Account]; user != "" {
				StaleAllowance(pluginAgent(pp), user)
			}
		}
	}
}
