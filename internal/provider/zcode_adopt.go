package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// zcodeCoreMigration records slots already adopted from the old mover. The
// original login and migration backup remain intact; this marker also keeps a
// later sign-out from resurrecting an old credential.
const zcodeCoreMigration = "zcode-core"

var zcodeAdoptionMu sync.Mutex

func init() { plugin.PrepareZCode = adoptZCodeAccounts }

func zcodeAdoptionID(l savedLogin) string {
	h := sha256.New()
	h.Write([]byte(loginID(l)))
	h.Write([]byte{0})
	h.Write(l.Auth)
	return hex.EncodeToString(h.Sum(nil))
}

// Only the former runtime's freeFirst option affected the ZCode protocol.
// The provider switch takes precedence, just as it did before migration.
func zcodeLegacyFreeFirst() (*bool, error) {
	l := plugin.Load()
	var values []bool
	var preferred *bool
	for _, e := range l.Plugins {
		if plugin.PackageName(e.Spec) != "@magpie-community/opencode-zcode-auth" {
			continue
		}
		v, ok := e.Options["freeFirst"].(bool)
		if !ok {
			continue
		}
		values = append(values, v)
		if l.Prefer["zcode"] == e.Spec {
			copy := v
			preferred = &copy
		}
	}
	if preferred != nil {
		return preferred, nil
	}
	if len(values) == 0 {
		return nil, nil
	}
	for _, v := range values[1:] {
		if v != values[0] {
			return nil, errors.New("legacy ZCode plugin options disagree; select the intended source before adoption")
		}
	}
	return &values[0], nil
}

func adoptZCodeAccounts() error {
	if settings.AccountDiscoveryOff() {
		return nil
	}
	zcodeAdoptionMu.Lock()
	defer zcodeAdoptionMu.Unlock()
	m, had := MigrationOf("zcode")
	if had && m.State == zcodeCoreMigration {
		return nil
	}
	// The former built-in discovered the app's own account lazily and
	// could have no saved row yet. Materialize that existing side-login
	// before reading the rows to adopt; its intrinsic slot is still bound
	// by the runtime to the app's actual current identity and selection.
	_ = zcodeLogins()
	ls, err := readLoginsFile()
	if err != nil {
		return err
	}
	var old []savedLogin
	seen := map[string]bool{}
	for _, l := range append(slices.Clone(ls), m.Backup...) {
		if l.Agent != "zcode" {
			continue
		}
		id := zcodeAdoptionID(l)
		if !seen[id] {
			seen[id] = true
			old = append(old, l)
		}
	}
	freeFirst, err := zcodeLegacyFreeFirst()
	if err != nil {
		return err
	}
	if len(old) == 0 && !had && freeFirst == nil {
		return nil
	}
	unlock, err := lockMoves()
	if err != nil {
		return err
	}
	defer unlock()
	// Refuse to turn an unreadable migration file into a fresh state.
	if b, err := os.ReadFile(migrationsPath()); err == nil {
		if !validMigrations(b) {
			return errors.New("ZCode migration state is unreadable")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	m, had = MigrationOf("zcode")
	if had && m.State == zcodeCoreMigration {
		return nil
	}
	if freeFirst != nil {
		if err := settings.EditPluginOptions(func(options map[string]map[string]bool) (map[string]map[string]bool, error) {
			if options["zcode"] == nil {
				options["zcode"] = map[string]bool{}
			}
			if _, set := options["zcode"]["free_first"]; !set {
				options["zcode"]["free_first"] = *freeFirst
			}
			return options, nil
		}); err != nil {
			return err
		}
	}
	type adopted struct {
		login savedLogin
		key   string
		id    string
	}
	var adoptedLogins []adopted
	var records []movedAccount
	err = plugin.EditAuth(func(auth map[string]json.RawMessage) (bool, error) {
		changed := false
		for _, l := range old {
			id := zcodeAdoptionID(l)
			key := ""
			// The old mover explicitly recorded this slot; names are used
			// only to recover that record, never to infer vendor identity.
			matches := 0
			for _, a := range m.Accounts {
				if a.User == l.User && a.Own == l.own() && plugin.ProviderOf(a.Key) == "zcode" {
					if _, ok := auth[a.Key]; ok {
						key, matches = a.Key, matches+1
					}
				}
			}
			if matches > 1 {
				return false, errors.New("ZCode migration has ambiguous recorded account slots")
			}
			if key == "" {
				for slot, raw := range auth {
					var saved struct {
						Adopted string `json:"magpieZCodeAdopted"`
					}
					if plugin.ProviderOf(slot) == "zcode" && json.Unmarshal(raw, &saved) == nil && saved.Adopted == id {
						key = slot
						break
					}
				}
			}
			if key == "" {
				var credential zcodeKey
				if !l.own() {
					var ok bool
					credential, ok = zcodeSaved(l)
					if !ok {
						return false, errors.New("a legacy ZCode account cannot be decoded; its credentials were left unchanged")
					}
				}
				key = "zcode#" + id[:24]
				if _, occupied := auth[key]; occupied {
					return false, errors.New("a ZCode adoption slot is already occupied")
				}
				value := zcodeOut(l.User, l.Plan, credential, l.own())
				value["magpieZCodeAdopted"] = id
				if credential.UID != "" {
					value["uid"] = credential.UID
				}
				raw, err := json.Marshal(value)
				if err != nil {
					return false, err
				}
				auth[key] = raw
				changed = true
			}
			adoptedLogins = append(adoptedLogins, adopted{l, key, id})
			records = append(records, movedAccount{Key: key, User: l.User, Own: l.own(), Sent: id})
		}
		return changed, nil
	})
	if err != nil {
		return err
	}
	loginsMu.Lock()
	ls, err = readLoginsFile()
	if err == nil {
		changed := false
		for _, a := range adoptedLogins {
			if slices.ContainsFunc(ls, func(l savedLogin) bool { return l.Agent == "plugin:zcode" && l.Home == a.key }) {
				continue
			}
			l := a.login
			l.Agent, l.Home, l.Auth = "plugin:zcode", a.key, nil
			ls = append(ls, l)
			changed = true
		}
		if changed {
			err = writeLogins(ls)
		}
	}
	loginsMu.Unlock()
	if err != nil {
		return err
	}
	return setMigration("zcode", func(m *Migration) {
		m.State, m.Package, m.At = zcodeCoreMigration, "", time.Now().UTC().Truncate(time.Second)
		m.Accounts = records
	})
}

// Preserve the original material; an unbound app account is bound only after
// the runtime reads the active ZCode user and selection, never from its label.
func zcodeOut(user, plan string, k zcodeKey, own bool) map[string]any {
	site := "zai"
	if strings.Contains(k.Base, "bigmodel.cn") {
		site = "bigmodel"
	}
	state := map[string]any{"site": site, "device": zcodeDeviceMid(), "base": k.Base}
	if own {
		state["source"] = "zcode"
	}
	for name, v := range map[string]string{"key": k.Key, "jwt": k.JWT, "token": k.Token, "org": k.Org, "project": k.Project, "uid": k.UID, "plan": plan} {
		if v != "" {
			state[name] = v
		}
	}
	return map[string]any{"type": "oauth", "access": firstNonEmpty(k.Key, k.JWT), "refresh": jsonText(state), "expires": 0, "accountId": user}
}
