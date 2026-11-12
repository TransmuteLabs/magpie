package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
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
	agent, labels := pluginAgent(pp), pluginLabels(pp, nil, nil)
	var out []pluginLogin
	for _, a := range pp.Accounts {
		saved := savedLogin{Agent: agent, User: labels[a.Key], Home: a.Key, On: true}
		l := sideLogin{Login{Agent: agent, User: saved.User, On: true}, saved}
		setPluginLoginIdentity(&l.Login, a)
		out = append(out, pluginLogin{l, a})
	}
	if len(out) > 0 {
		out[0].Active = true
	}
	return out
}

// publicAccount is whether p is a public plugin's provider (publicPlugin),
// served while subscription accounts are off.
func (p Provider) publicAccount() bool {
	return p.IsPlugin() && publicPlugin(*p.Account.plugin)
}

// pluginLabels names each account: by named (the agent's own, as the
// agent is signed in now), else its id, else the end of its key, else what
// it signed in with; a name two visible accounts share gets the slot. The slot
// goes on after the own name, so the own account never reads as another
// signed in to the same one (dev on Discord: ZCode's, its id "ZCode",
// named as one magpie signed in, and Remove on that one took the own).
func pluginLabels(pp plugin.Provider, named map[string]string, hidden map[string]bool) map[string]string {
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
		if !hidden[a.Key] {
			seen[strings.ToLower(l)]++
		}
	}
	for _, a := range pp.Accounts {
		if !hidden[a.Key] && seen[strings.ToLower(out[a.Key])] > 1 {
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
	if pp.ID != "zcode" {
		own, named := pluginOwnNames(pp)
		mark := ""
		if len(own) > 0 {
			mark = ownMark(pp.ID)
		}
		labels, _ := pluginAccountLabels(pp, own, named, mark, sharedLogins())
		return labels
	}
	labels := map[string]string{}
	for _, l := range pluginLogins(pp) {
		labels[l.acct.Key] = l.User
	}
	return labels
}

// Hidden accounts do not rename the only visible account beside them.
// Decide which own accounts reappear before disambiguating the visible names,
// using the same sign-in mark as the account reconciliation below.
func pluginAccountLabels(pp plugin.Provider, own map[string]bool, named map[string]string, mark string, ls []savedLogin) (map[string]string, map[string]bool) {
	labels := pluginLabels(pp, named, nil)
	agent := pluginAgent(pp)
	var hidden map[string]bool
	for _, l := range ls {
		if l.Agent != agent || l.Hidden == "" {
			continue
		}
		if own[l.Home] && (mark != "" && mark != l.Hidden || mark == "" && l.Hidden == hiddenNoMark && l.User != labels[l.Home]) {
			continue
		}
		if hidden == nil {
			hidden = map[string]bool{}
		}
		hidden[l.Home] = true
	}
	if len(hidden) > 0 {
		labels = pluginLabels(pp, named, hidden)
	}
	if pp.ID != "zcode" {
		return labels, hidden
	}
	for _, l := range ls {
		if l.Agent != agent || l.Hidden != "" {
			continue
		}
		base := labels[l.Home]
		_, slot, hasSlot := strings.Cut(l.Home, "#")
		if hasSlot && strings.EqualFold(l.User, base+" ("+slot+")") {
			labels[l.Home] = l.User
		} else if l.PluginScope != "" && strings.EqualFold(l.User, scopedPluginLabel(base, l.Home, l.PluginScope)) {
			labels[l.Home] = l.User
		} else if hasSlot && l.PluginScope != "" && strings.EqualFold(l.User, scopedPluginLabel(base+" ("+slot+")", l.Home, l.PluginScope)) {
			labels[l.Home] = l.User
		}
	}
	return labels, hidden
}

func scopedPluginLabel(base, slot, scope string) string {
	if _, suffix, ok := strings.Cut(slot, "#"); ok {
		slot = suffix
	}
	if len(scope) > 12 {
		scope = scope[:12]
	}
	return base + " (" + slot + "-" + scope + ")"
}

func pluginPolicyScope(id string, a plugin.Account) string {
	if id != "zcode" {
		return ""
	}
	if a.Scope != "" {
		return a.Scope
	}
	if a.Generation != "" {
		generation, _, _ := strings.Cut(a.Generation, "/app:")
		return "material:" + generation
	}
	return ""
}

func pluginPolicyFile() (file, error) {
	b, source, err := readProviderSource(Path())
	if err != nil {
		return file{}, errors.New("account settings are unreadable")
	}
	if !source.exists {
		return file{source: source}, nil
	}
	if !validProviders(b) {
		return file{}, errors.New("account settings are unreadable")
	}
	var f file
	if err := json.Unmarshal(bytes.TrimPrefix(b, utf8BOM), &f); err != nil {
		return file{}, err
	}
	f.source = source
	return f, nil
}

func pluginPolicyTargetFree(id, user string) (bool, error) {
	f, err := pluginPolicyFile()
	if err != nil {
		return false, err
	}
	key := accountKey(user)
	for _, p := range f.Providers {
		if p.ID != id {
			continue
		}
		if _, ok := p.AccountProxies[key]; ok {
			return false, nil
		}
		if _, ok := p.AccountModels[key]; ok {
			return false, nil
		}
		if _, ok := p.AccountCaps[key]; ok {
			return false, nil
		}
		if _, ok := p.AccountWindowCaps[key]; ok {
			return false, nil
		}
		if _, ok := p.AccountWindowAmounts[key]; ok {
			return false, nil
		}
		if _, ok := p.AccountConcurrency[key]; ok {
			return false, nil
		}
		if _, ok := p.AccountWeights[key]; ok {
			return false, nil
		}
		if _, ok := p.AccountNames[key]; ok {
			return false, nil
		}
	}
	return true, nil
}

func pluginLabelAvailable(id, agent, slot, label string, ls []savedLogin) (bool, error) {
	for _, l := range ls {
		if l.Agent == agent && l.Home != slot &&
			(strings.EqualFold(l.User, label) || l.PluginRename != nil && strings.EqualFold(l.PluginRename.To, label)) {
			return false, nil
		}
	}
	free, err := pluginPolicyTargetFree(id, label)
	if err != nil || !free {
		return free, err
	}
	err = settings.EditTrayUsages(func(ids []string) ([]string, error) {
		for _, ref := range ids {
			if strings.EqualFold(ref, id+"|"+label) {
				free = false
				break
			}
		}
		return ids, nil
	})
	return free, err
}

func pluginPolicyMark[V any](m map[string]V, key string) string {
	value, ok := m[key]
	if !ok {
		return ""
	}
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func preparePluginIntent(id string, intent *pluginRenameIntent) error {
	f, err := pluginPolicyFile()
	if err != nil {
		return err
	}
	free, err := pluginPolicyTargetFree(id, intent.To)
	if err != nil || !free {
		return errors.New("destination account policy is unavailable")
	}
	if intent.MovePolicy {
		from := accountKey(intent.From)
		for _, p := range f.Providers {
			if p.ID != id {
				continue
			}
			intent.Policy = map[string]string{
				"proxy":       pluginPolicyMark(p.AccountProxies, from),
				"models":      pluginPolicyMark(p.AccountModels, from),
				"cap":         pluginPolicyMark(p.AccountCaps, from),
				"windows":     pluginPolicyMark(p.AccountWindowCaps, from),
				"amounts":     pluginPolicyMark(p.AccountWindowAmounts, from),
				"concurrency": pluginPolicyMark(p.AccountConcurrency, from),
				"weight":      pluginPolicyMark(p.AccountWeights, from),
				"name":        pluginPolicyMark(p.AccountNames, from),
			}
			break
		}
	}
	return settings.EditTrayUsages(func(ids []string) ([]string, error) {
		for _, ref := range ids {
			if strings.EqualFold(ref, id+"|"+intent.To) {
				return nil, errors.New("destination tray selection is already owned")
			}
			if intent.MovePolicy && strings.EqualFold(ref, id+"|"+intent.From) {
				intent.Tray = true
			}
		}
		return ids, nil
	})
}

func stagePluginPolicyMap[V any](m map[string]V, from, to, expected string, cleanup bool) (bool, error) {
	source, hasSource := m[from]
	target, hasTarget := m[to]
	if expected == "" {
		if hasSource || hasTarget {
			return false, errors.New("account policy changed during rename")
		}
		return false, nil
	}
	if !hasSource {
		if cleanup && hasTarget && pluginPolicyMark(m, to) == expected {
			return false, nil
		}
		return false, errors.New("source policy changed during account rename")
	}
	if pluginPolicyMark(m, from) != expected || hasTarget && !reflect.DeepEqual(source, target) {
		return false, errors.New("destination policy conflicts with account rename")
	}
	if cleanup {
		if !hasTarget {
			return false, errors.New("staged account policy is missing")
		}
		delete(m, from)
		return true, nil
	}
	if !hasTarget {
		m[to] = source
		return true, nil
	}
	return false, nil
}

// stagePluginPolicies copies before publishing the new login name, then
// removes the old copy only after publication. Both phases are idempotent.
func stagePluginPolicies(id string, intent *pluginRenameIntent, cleanup bool) error {
	if !intent.MovePolicy {
		free, err := pluginPolicyTargetFree(id, intent.To)
		if err != nil {
			return err
		}
		if !free {
			return errors.New("destination account policy is already owned")
		}
		return nil
	}
	f, err := pluginPolicyFile()
	if err != nil {
		return err
	}
	from, to := accountKey(intent.From), accountKey(intent.To)
	for i := range f.Providers {
		p := &f.Providers[i]
		if p.ID != id {
			continue
		}
		changed := false
		record := func(moved bool, err error) error {
			changed = changed || moved
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountProxies, from, to, intent.Policy["proxy"], cleanup)); err != nil {
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountModels, from, to, intent.Policy["models"], cleanup)); err != nil {
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountCaps, from, to, intent.Policy["cap"], cleanup)); err != nil {
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountWindowCaps, from, to, intent.Policy["windows"], cleanup)); err != nil {
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountWindowAmounts, from, to, intent.Policy["amounts"], cleanup)); err != nil {
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountConcurrency, from, to, intent.Policy["concurrency"], cleanup)); err != nil {
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountWeights, from, to, intent.Policy["weight"], cleanup)); err != nil {
			return err
		}
		if err := record(stagePluginPolicyMap(p.AccountNames, from, to, intent.Policy["name"], cleanup)); err != nil {
			return err
		}
		if changed {
			return store(f)
		}
		return nil
	}
	for _, expected := range intent.Policy {
		if expected != "" {
			return errors.New("source account policy is unavailable")
		}
	}
	return nil
}

func stagePluginTray(id string, intent *pluginRenameIntent, cleanup bool) error {
	return settings.EditTrayUsages(func(ids []string) ([]string, error) {
		old, next := -1, -1
		for i, ref := range ids {
			if strings.EqualFold(ref, id+"|"+intent.From) {
				old = i
			}
			if strings.EqualFold(ref, id+"|"+intent.To) {
				next = i
			}
		}
		if !intent.MovePolicy || !intent.Tray {
			if next >= 0 || intent.MovePolicy && old >= 0 {
				return nil, errors.New("destination tray selection is already owned")
			}
			return ids, nil
		}
		if cleanup {
			if old >= 0 && next < 0 {
				return nil, errors.New("staged tray selection is missing")
			}
			if old >= 0 {
				ids = slices.Delete(ids, old, old+1)
			}
			return ids, nil
		}
		if old < 0 {
			return nil, errors.New("destination tray selection changed during account rename")
		}
		if old >= 0 && next < 0 {
			ids = slices.Insert(ids, old+1, id+"|"+intent.To)
		}
		return ids, nil
	})
}

func pendingPluginAccountError(agent, slot string) error {
	for _, l := range sharedLogins() {
		if l.Agent == agent && l.Home == slot && l.PluginRename != nil {
			return errors.New("account rename is pending; repair the account settings and retry")
		}
	}
	return nil
}

func currentPluginAccount(id, slot string) (plugin.Account, bool) {
	providers, available := plugin.PassiveProviders()
	if !available {
		return plugin.Account{}, false
	}
	for _, pp := range providers {
		if pp.ID == id {
			for _, a := range pp.Accounts {
				if a.Key == slot {
					return a, true
				}
			}
		}
	}
	return plugin.Account{}, false
}

// reconcilePluginPending resumes each durable intent without routing the
// account. The source copies remain until the new name has been published.
func reconcilePluginPending(pp plugin.Provider) {
	if pp.ID != "zcode" {
		return
	}
	agent, id := pluginAgent(pp), PluginID(pp.ID)
	if !slices.ContainsFunc(sharedLogins(), func(l savedLogin) bool { return l.Agent == agent && l.PluginRename != nil }) {
		return
	}
	loginsMu.Lock()
	defer loginsMu.Unlock()
	b, err := steady.ReadFile(loginsPath())
	if err != nil || !validLogins(b) {
		log.Printf("%s: account rename pending: login state is unreadable", id)
		return
	}
	ls, err := parseLogins(b)
	if err != nil {
		log.Printf("%s: account rename pending: %v", id, err)
		return
	}
	blocked := map[string]bool{}
	for {
		i := slices.IndexFunc(ls, func(l savedLogin) bool { return l.Agent == agent && l.PluginRename != nil && !blocked[l.Home] })
		if i < 0 {
			return
		}
		l := &ls[i]
		intent := l.PluginRename
		a, ok := currentPluginAccount(pp.ID, l.Home)
		valid := ok && intent.Slot == l.Home && intent.From != "" && intent.To != "" &&
			pluginPolicyScope(pp.ID, a) == intent.Scope
		if !intent.Published {
			valid = valid && l.User == intent.From && l.PluginScope == intent.PreviousScope
		} else {
			valid = valid && l.User == intent.To && l.PluginScope == intent.Scope
		}
		for _, other := range ls {
			if other.Agent == agent && other.Home != l.Home &&
				(strings.EqualFold(other.User, intent.To) || other.PluginRename != nil && strings.EqualFold(other.PluginRename.To, intent.To) ||
					intent.MovePolicy && strings.EqualFold(other.User, intent.From)) {
				valid = false
			}
		}
		if !valid {
			log.Printf("%s: account rename pending: identity or name ownership changed", id)
			blocked[l.Home] = true
			continue
		}
		if !intent.Prepared {
			if err := preparePluginIntent(id, intent); err != nil {
				log.Printf("%s: account rename pending: %v", id, err)
				blocked[l.Home] = true
				continue
			}
			intent.Prepared = true
			if err := writeLogins(ls); err != nil {
				log.Printf("%s: account rename pending: %v", id, err)
				return
			}
			continue
		}
		if !intent.Published {
			if err := stagePluginPolicies(id, intent, false); err != nil {
				log.Printf("%s: account rename pending: %v", id, err)
				blocked[l.Home] = true
				continue
			}
			if err := stagePluginTray(id, intent, false); err != nil {
				log.Printf("%s: account rename pending: %v", id, err)
				blocked[l.Home] = true
				continue
			}
			l.User, l.PluginScope = intent.To, intent.Scope
			if intent.PreviousScope != intent.Scope {
				l.ID = ""
			}
			intent.Published = true
			if err := writeLogins(ls); err != nil {
				log.Printf("%s: account rename pending: %v", id, err)
				return
			}
			continue
		}
		if err := stagePluginPolicies(id, intent, true); err != nil {
			log.Printf("%s: account rename pending: %v", id, err)
			blocked[l.Home] = true
			continue
		}
		if err := stagePluginTray(id, intent, true); err != nil {
			log.Printf("%s: account rename pending: %v", id, err)
			blocked[l.Home] = true
			continue
		}
		l.PluginRename = nil
		if err := writeLogins(ls); err != nil {
			log.Printf("%s: account rename pending: %v", id, err)
			return
		}
	}
}

// pluginOwnNames tells the provider's accounts that are the agent's own,
// and names them as the agent is signed in now, where that can be read.
func pluginOwnNames(pp plugin.Provider) (own map[string]bool, named map[string]string) {
	own, named = map[string]bool{}, map[string]string{}
	listed := map[string]bool{}
	for _, a := range pp.Accounts {
		listed[a.Key] = true
	}
	auths := plugin.Auths(pp.ID)
	for k, a := range auths {
		if listed[k] && pluginOwn(pp.ID, a) {
			own[k] = true
		}
	}
	if pp.ID == "zcode" {
		app := zcodeAppLabel()
		for k := range own {
			var saved struct {
				Site    string `json:"site"`
				UID     string `json:"uid"`
				Kind    string `json:"kind"`
				Org     string `json:"org"`
				Project string `json:"project"`
				Product string `json:"product"`
			}
			if json.Unmarshal([]byte(str(auths[k]["refresh"])), &saved) != nil {
				continue
			}
			saved.UID = firstNonEmpty(saved.UID, str(auths[k]["uid"]))
			if app.family != "" && app.uid != "" && app.user != "" &&
				saved.Site == app.family && saved.UID == app.uid && saved.Kind == app.kind &&
				(saved.Kind != "team" || saved.Org == app.org && saved.Project == app.project && saved.Product == app.product) {
				named[k] = app.user
			}
		}
		return own, named
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

type zcodeAppIdentity struct {
	family, uid, user, kind, org, project, product string
	generation                                     string
}

// zcodeAppLabel follows the SDK's active provider, selected family and
// full selection, without borrowing a stale profile from another family.
var zcodeAppLabel = func() zcodeAppIdentity {
	var store map[string]string
	if !readJSON(zcodeCredentialsPath(), &store) {
		return zcodeAppIdentity{}
	}
	secret := zcodeSecret()
	value := func(key string) string {
		v := store[key]
		if strings.HasPrefix(v, "enc:") {
			if !strings.HasPrefix(v, "enc:v1:") {
				return ""
			}
			s, ok := zcodeDecrypt(secret, v)
			if !ok {
				return ""
			}
			return s
		}
		return v
	}
	family, active := zcodeActiveFamily(store, secret)
	if family == "" || active == "" {
		return zcodeAppIdentity{}
	}
	var profile struct {
		UserID      string `json:"user_id"`
		ID          string `json:"id"`
		Email       string `json:"email"`
		Name        string `json:"name"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	}
	if json.Unmarshal([]byte(value("oauth:"+active+":user_info")), &profile) != nil {
		return zcodeAppIdentity{}
	}
	uid := strings.TrimSpace(firstNonEmpty(profile.UserID, profile.ID))
	if uid == "" {
		return zcodeAppIdentity{}
	}
	home, _ := os.UserHomeDir()
	var set struct {
		Domain     string `json:"providerFamilyDomain"`
		Selections map[string]struct {
			Kind    string `json:"kind"`
			Org     string `json:"organizationId"`
			Project string `json:"projectId"`
			Product string `json:"productId"`
		} `json:"providerFamilyConnectionSelections"`
		Legacy map[string]string `json:"modelProviderFamilySelectedKeys"`
	}
	b, err := readAccountFile(filepath.Join(appdir.ZCodeHome(home), ".zcode", "v2", "setting.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && json.Unmarshal(b, &set) != nil {
		return zcodeAppIdentity{}
	}
	if (set.Domain == "zai" || set.Domain == "bigmodel") && set.Domain != family {
		return zcodeAppIdentity{}
	}
	app := zcodeAppIdentity{family: family, uid: uid, user: zcodeWho(profile.Email, firstNonEmpty(profile.Name, profile.Username, profile.DisplayName), uid)}
	if selected, ok := set.Selections[family]; ok {
		switch selected.Kind {
		case "start-plan":
			app.kind = "start"
		case "individual-coding-plan":
			app.kind = "individual"
		case "team-coding-plan":
			app.kind, app.org, app.project, app.product = "team", strings.TrimSpace(selected.Org), strings.TrimSpace(selected.Project), strings.TrimSpace(selected.Product)
		}
	}
	if app.kind == "" {
		prefix := "team-plan:builtin:" + family + "-coding-plan:"
		if legacy, ok := strings.CutPrefix(strings.TrimSpace(set.Legacy[family]), prefix); ok {
			parts := strings.Split(legacy, ":")
			if len(parts) != 3 {
				return zcodeAppIdentity{}
			}
			decoded := make([]string, len(parts))
			for i, part := range parts {
				decoded[i], err = url.PathUnescape(part)
				if err != nil {
					return zcodeAppIdentity{}
				}
			}
			app.kind, app.product, app.org, app.project = "team", decoded[0], decoded[1], decoded[2]
		}
	}
	token := ""
	if active == family {
		token = strings.TrimSpace(value("oauth:" + active + ":access_token"))
	}
	accountIdentity := uid
	if family == "bigmodel" {
		var linked struct {
			UserID      string `json:"userId"`
			CustomerID  string `json:"customerId"`
			AccessToken string `json:"accessToken"`
		}
		verifiedUID := strings.TrimSpace(profile.UserID)
		if verifiedUID == "" && profile.Username != "" && profile.DisplayName != "" && profile.ID != "unknown" {
			verifiedUID = strings.TrimSpace(profile.ID)
		}
		if json.Unmarshal([]byte(value("oauth:linked_bigmodel")), &linked) == nil &&
			verifiedUID != "" && strings.TrimSpace(linked.UserID) == verifiedUID &&
			strings.TrimSpace(linked.CustomerID) != "" && strings.TrimSpace(linked.AccessToken) != "" {
			accountIdentity, token = strings.TrimSpace(linked.CustomerID), strings.TrimSpace(linked.AccessToken)
		}
	}
	unbear := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 7 && strings.EqualFold(s[:7], "Bearer ") {
			s = strings.TrimSpace(s[7:])
		}
		return s
	}
	jwt := unbear(value("zcodejwttoken"))
	token = unbear(token)
	if token == jwt {
		token = ""
	}
	var legacy zcodeKey
	if token == "" && app.kind != "start" && app.kind != "team" {
		legacy = zcodeConfigKey(secret, family)
	}
	if app.kind == "" {
		switch {
		case token != "":
			app.kind = "individual"
		case legacy.Key != "":
			app.kind = "legacy-key"
		default:
			app.kind = "start"
		}
	} else if app.kind == "individual" && token == "" && legacy.Key != "" {
		app.kind = "legacy-key"
	}
	if app.kind == "team" && (app.org == "" || app.project == "" || app.product == "") || token == "" && jwt == "" && legacy.Key == "" {
		return zcodeAppIdentity{}
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{accountIdentity, value("oauth:session_revision"), token, jwt, value("oauth:" + active + ":refresh_token"), legacy.Key}, "\x00")))
	app.generation = hex.EncodeToString(sum[:])
	return app
}

type pluginLogin struct {
	sideLogin
	acct plugin.Account
}

func setPluginLoginIdentity(l *Login, a plugin.Account) {
	l.pluginSlot, l.pluginGeneration = a.Key, a.Generation
	if a.Key == "" || a.Generation == "" {
		return
	}
	scope := a.Scope
	if scope == "" {
		// Unscoped plugins have no verified durable selection. Bind even
		// their historical identity to the nonsecret material generation.
		scope = "material:" + a.Generation
	}
	sum := sha256.Sum256([]byte(a.Key + "\x00" + scope))
	l.pluginIdentity = hex.EncodeToString(sum[:])
}

func pluginLoginByUser(agent, user string) (Login, bool) {
	id := strings.TrimPrefix(agent, "plugin:")
	if id == agent {
		pp, ok := pluginOfAgent(agent)
		if !ok {
			return Login{}, false
		}
		id = pp.ID
	}
	providers, available := plugin.PassiveProviders()
	if !available {
		return Login{}, false
	}
	var pp plugin.Provider
	for _, candidate := range providers {
		if candidate.ID == id {
			pp = candidate
			break
		}
	}
	if pp.ID == "" {
		return Login{}, false
	}
	var found Login
	for _, l := range pluginLogins(pp) {
		if strings.EqualFold(l.User, user) {
			if found.pluginSlot != "" {
				return Login{}, false
			}
			found = l.Login
		}
	}
	return found, found.pluginSlot != ""
}

func pluginUsageBindings(agent string) (map[string]string, bool) {
	id := strings.TrimPrefix(agent, "plugin:")
	if id == agent {
		pp, ok := pluginOfAgent(agent)
		if !ok {
			return nil, false
		}
		id = pp.ID
	}
	out := map[string]string{}
	providers, available := plugin.PassiveProviders()
	if !available {
		return out, true
	}
	for _, pp := range providers {
		if pp.ID != id {
			continue
		}
		for _, l := range pluginLogins(pp) {
			out[strings.ToLower(l.User)] = loginReadingKey(l.Login)
		}
		break
	}
	return out, true
}

func pluginLoginCurrent(l Login) bool {
	current, ok := pluginLoginByUser(l.Agent, l.User)
	return ok && l.pluginSlot != "" && current.pluginSlot == l.pluginSlot &&
		l.pluginIdentity != "" && current.pluginIdentity == l.pluginIdentity &&
		l.pluginGeneration != "" && current.pluginGeneration == l.pluginGeneration
}

func currentPluginQuota(q SubscriptionQuota) bool {
	if q.From != "" || q.AccountIdentity == "" && !q.accountUnverified {
		return true
	}
	if q.accountUnverified {
		return false
	}
	pp, ok := pluginOfAgent(q.Provider)
	if !ok {
		return false
	}
	l, ok := pluginLoginByUser("plugin:"+pp.ID, q.User)
	return ok && l.pluginIdentity == q.AccountIdentity && l.pluginGeneration == q.AccountGeneration
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
	if pp.ID == "zcode" && len(own) > 0 {
		app := zcodeAppLabel()
		for key := range own {
			if named[key] == "" || app.generation == "" {
				continue
			}
			a := byKey[key]
			a.Generation += "/app:" + app.generation
			byKey[key] = a
		}
	}
	mark := ""
	if len(own) > 0 {
		mark = ownMark(pp.ID)
	}
	reconcilePluginPending(pp)
	// looked at as kept, and copied only at the first change: nearly
	// every list changes nothing, and a page's state lists each plugin's
	settle := func(ls []savedLogin) ([]savedLogin, bool, error) {
		labels, hidden := pluginAccountLabels(pp, own, named, mark, ls)
		changed := false
		have := map[string]bool{}
		var keep []savedLogin
		for i, l := range ls {
			drop, edit := false, false
			if l.Agent == agent {
				if l.PluginRename != nil {
					have[l.Home] = true
					if changed {
						keep = append(keep, l)
					}
					continue
				}
				a, ok := byKey[l.Home]
				if !ok || have[l.Home] {
					drop = true // signed out, or listed twice
				} else {
					have[l.Home] = true
					if pp.ID != "zcode" {
						if l.User != labels[l.Home] {
							l.User, edit = labels[l.Home], true
						}
					} else {
						desired := labels[l.Home]
						scope := pluginPolicyScope(pp.ID, a)
						scopeChanged := l.PluginScope != "" && l.PluginScope != scope
						scopeLabel := scopeChanged || l.PluginScope == "" && !strings.EqualFold(l.User, desired)
						if scopeChanged && (scope == "" || l.Hidden != "") {
							// An unproved or hidden old selection is never
							// rebound to whatever now occupies its slot.
							if changed {
								keep = append(keep, l)
							}
							continue
						}
						if scope != "" && l.PluginScope == "" && strings.EqualFold(l.User, desired) {
							free, err := pluginLabelAvailable(PluginID(pp.ID), agent, l.Home, desired, ls)
							if err != nil {
								// Preparation stops until the source files can be read.
								free = false
							}
							if !free {
								scopeLabel = true
							}
						}
						if scope != "" && scopeLabel {
							desired = scopedPluginLabel(desired, l.Home, scope)
							if scopeChanged && strings.EqualFold(desired, l.User) {
								desired += "-" + scope
							}
						}
						if !strings.EqualFold(l.User, desired) {
							free, err := pluginLabelAvailable(PluginID(pp.ID), agent, l.Home, desired, ls)
							if err != nil {
								free = true // the durable intent checks again
							}
							if !free && scope != "" {
								desired = scopedPluginLabel(desired, l.Home, scope)
								free, err = pluginLabelAvailable(PluginID(pp.ID), agent, l.Home, desired, ls)
								if err != nil {
									free = true
								}
							}
							if !free {
								if changed {
									keep = append(keep, l)
								}
								continue
							}
							// Only a positively bound native scope owns the
							// old name's settings. A shared name owns none.
							owned := a.Scope != "" && l.PluginScope == a.Scope
							for _, other := range ls {
								if other.Agent == agent && other.Home != l.Home && strings.EqualFold(other.User, l.User) {
									owned = false
								}
							}
							l.PluginRename = &pluginRenameIntent{Slot: l.Home, From: l.User, To: desired,
								PreviousScope: l.PluginScope, Scope: scope, Generation: a.Generation, MovePolicy: owned}
							edit = true
						}
						if scope != "" && l.PluginScope != scope && l.PluginRename == nil {
							l.PluginRename = &pluginRenameIntent{Slot: l.Home, From: l.User, To: l.User,
								PreviousScope: l.PluginScope, Scope: scope, Generation: a.Generation}
							edit = true
						}
					}
					// the agent's own, hidden, shows again once the agent signs
					// in anew: told by its sign-in's mark, else by another account
					if l.Hidden != "" && own[l.Home] && !hidden[l.Home] {
						if l.PluginRename == nil {
							l.Hidden, edit = "", true
						}
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
				label := labels[a.Key]
				scope := pluginPolicyScope(pp.ID, a)
				if pp.ID == "zcode" && scope != "" {
					free, err := pluginLabelAvailable(PluginID(pp.ID), agent, a.Key, label, ls)
					if err != nil {
						free = true // the durable intent checks again
					}
					if !free {
						label = scopedPluginLabel(label, a.Key, scope)
						free, err = pluginLabelAvailable(PluginID(pp.ID), agent, a.Key, label, ls)
						if err != nil {
							free = true
						}
						if !free {
							return ls, false, errors.New("the new account's label is unavailable")
						}
					}
				}
				row := savedLogin{Agent: agent, User: label, Home: a.Key, On: true, Seen: time.Now().UTC().Truncate(time.Second)}
				if pp.ID == "zcode" && scope != "" {
					row.PluginRename = &pluginRenameIntent{Slot: a.Key, From: label, To: label, Scope: scope, Generation: a.Generation}
				}
				ls = append(ls, row)
			}
		}
		return ls, changed, nil
	}
	// what nothing changes takes no lock: taken, it opens logins.json's
	// lock file, across magpies, and /api/providers lists every plugin
	// (a fifth of its CPU). A change is made from a read under it.
	if _, changed, err := settle(sharedLogins()); changed && err == nil {
		pluginLoginLocks.Add(1)
		loginsMu.Lock()
		if ls, changed, err := settle(sharedLogins()); changed && err == nil {
			if err = writeLogins(ls); err != nil {
				log.Printf("%s: account reconciliation failed: %v", PluginID(pp.ID), err)
			}
		}
		loginsMu.Unlock()
	}
	reconcilePluginPending(pp)
	var out []pluginLogin
	for _, l := range sideLogins(agent, "", func(l savedLogin) bool {
		a, ok := byKey[l.Home]
		return ok && l.Hidden == "" && l.PluginRename == nil && (l.PluginScope == "" || l.PluginScope == pluginPolicyScope(pp.ID, a))
	}) {
		l.Lapsed = l.saved.Lapsed // a refused sign-in shows on the account
		l.Own = own[l.saved.Home]
		a := byKey[l.saved.Home]
		setPluginLoginIdentity(&l.Login, a)
		out = append(out, pluginLogin{l, a})
	}
	return out
}

func pluginSide(pp plugin.Provider) []sideLogin {
	var out []sideLogin
	for _, l := range pluginLogins(pp) {
		out = append(out, l.sideLogin)
	}
	return out
}

// pluginOfAgent is the plugin provider the accounts page names by its
// magpie id.
func pluginOfAgent(agent string) (plugin.Provider, bool) {
	if strings.HasPrefix(agent, "plugin:") {
		id := strings.TrimPrefix(agent, "plugin:")
		for _, pp := range plugin.Cached() {
			if pp.ID == id {
				return pp, true
			}
		}
		return plugin.Provider{}, false
	}
	return PluginOf(agent)
}

func pluginLoginList(pp plugin.Provider) []Login { return loginsOf(pluginSide(pp)) }

func switchPluginLogin(pp plugin.Provider, user string) error {
	return switchSideLogin(pluginAgent(pp), user, pluginSide(pp))
}

func setPluginLoginOn(pp plugin.Provider, user string, on bool) error {
	return setSideLoginOn(pluginAgent(pp), user, on, pluginSide(pp))
}

// forgetPluginLogin signs the account out: its sign-in is magpie's own.
// The agent's own is only hidden, as the built-in hid it: the agent stays
// signed in, and the account shows again once it signs in anew. Qoder's
// first goes as the built-in's did, the next put first.
func forgetPluginLogin(pp plugin.Provider, user string) error {
	agent := pluginAgent(pp)
	ls := pluginSide(pp)
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
	err := editListedLogin(agent, user, ls, func(saved []savedLogin, j int) ([]savedLogin, error) {
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

// keepPluginPlan keeps the plan an account's allowance told on its row,
// for the accounts list to show it as a built-in's.
func keepPluginPlan(pp plugin.Provider, key, plan string) {
	if plan == "" {
		return
	}
	loginsMu.Lock()
	defer loginsMu.Unlock()
	ls := readLogins()
	for i, l := range ls {
		if l.Agent == pluginAgent(pp) && l.Home == key {
			if l.Plan != plan {
				ls[i].Plan = plan
				_ = writeLogins(ls)
			}
			return
		}
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
func notePluginLapse(pp plugin.Provider, key string, status int) {
	notePluginLapseSince(pp, key, status, toldNow)
}

// What plugin accounts' sign-ins were told of (a request's answer, a usage
// reading, a sign-in) is counted, under loginsMu: pluginSignInTold
// counts it all, and pluginSignInSaid has the count each account was last
// told of at. A count, not a time: Windows' clock moves in ticks of up to
// 15.6ms, and a reading begun just after an answer would read as begun
// with it.
var (
	pluginSignInTold uint64
	pluginSignInSaid = map[string]uint64{}
)

// toldNow is the began of what is told now, not read from earlier.
const toldNow = ^uint64(0)

// pluginSignInAt is the count a usage reading beginning now goes by.
func pluginSignInAt() uint64 {
	loginsMu.Lock()
	defer loginsMu.Unlock()
	return pluginSignInTold
}

// notePluginLapseSince is notePluginLapse for what a usage reading begun
// at began (pluginSignInAt) found, toldNow for what is told now: what was
// told of the sign-in since began stands over it. A reading begun before
// a request's 401 that ended clean took the mark off an account the
// vendor had just refused.
func notePluginLapseSince(pp plugin.Provider, key string, status int, began uint64) {
	notePluginLapseOf(pluginAgent(pp), key, status, began, func(user string) string { return lapsedText(pp, user) })
}

// notePluginLapseOf is notePluginLapseSince by the account's agent, so a
// mark can be cleared without the plugin's provider at hand.
func notePluginLapseOf(agent, key string, status int, began uint64, text func(user string) string) {
	refused := status == http.StatusUnauthorized
	if !refused && (status < 200 || status > 299) {
		return
	}
	loginsMu.Lock()
	defer loginsMu.Unlock()
	said := agent + "/" + key
	if pluginSignInSaid[said] > began {
		return
	}
	if began == toldNow {
		pluginSignInTold++
		began = pluginSignInTold
	}
	pluginSignInSaid[said] = began
	ls := readLogins()
	for i, l := range ls {
		if l.Agent == agent && l.Home == key {
			want := ""
			if refused {
				want = text(l.User)
			}
			if l.Lapsed != want {
				ls[i].Lapsed = want
				_ = writeLogins(ls)
			}
			return
		}
	}
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
	notePluginLapseOf("plugin:"+saved.Provider, saved.Account, http.StatusOK, toldNow, nil)
	for _, pp := range plugin.Cached() {
		if pp.ID == saved.Provider {
			if user := pluginNames(pp)[saved.Account]; user != "" {
				StaleAllowance(pluginAgent(pp), user)
			}
		}
	}
	loginsMu.Lock()
	defer loginsMu.Unlock()
	ls := readLogins()
	for i, l := range ls {
		if l.Agent == "plugin:"+saved.Provider && l.Home == saved.Account && l.Hidden != "" {
			ls[i].Hidden = ""
			_ = writeLogins(ls)
			return
		}
	}
}
