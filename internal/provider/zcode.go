package provider

// ZCode is an intrinsic provider. The official vendor transport is embedded
// in internal/plugin/bundled/zcode; zcode_account.go and its coordinators
// own account operations, resets, claims and execution inside magpie.
// This file retains former built-in/app credential readers for adoption,
// plus the Zhipu plan readers a GLM Coding Plan key's card still uses.
// There is no external ZCode serving dependency or subscription mover.

// ZCode's own account is read, never changed, from its credential store,
// ~/.zcode/v2/credentials.json: each value is "enc:v1:" + iv.tag.ciphertext
// (base64url), AES-256-GCM under sha256 of $ZCODE_CREDENTIAL_SECRET, or
// else of "zcode-credential-fallback:<platform>:<home>:<user>". The
// accounts magpie signed in to keep their key in logins.json.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/lazyre"
)

// Where ZCode's coding plan and its sign-in are; vars so tests can point
// them elsewhere.
var (
	ZCodeZaiBase      = "https://api.z.ai/api/anthropic"
	ZCodeBigModelBase = "https://open.bigmodel.cn/api/anthropic"
	zcodeAPI          = "https://zcode.z.ai"
	zcodeZaiAPI       = "https://api.z.ai"
	zcodeBigModelAPI  = "https://bigmodel.cn" // BigModel's business API, where ZCode asks it
)

// zcodeAppVersion is the ZCode whose sign-in magpie makes.
const zcodeAppVersion = "3.14.3"

// zcodeKey is a coding plan's key and where it is served.
type zcodeKey struct {
	Key  string `json:"apiKey,omitempty"`
	Base string `json:"base"`
	JWT  string `json:"jwt,omitempty"` // ZCode's session token, the Start Plan's key (zcode_start.go)
	// A seat on a team's plan (zcode_team.go): the team project it is in,
	// and the account's business sign-in, as its Authorization, for the
	// plan's term and resets. Key is the project's own key, or "" while it
	// is still to be found (ZCode's own account).
	Token   string `json:"token,omitempty"`
	Org     string `json:"org,omitempty"`
	Project string `json:"project,omitempty"`
	// UID is the account's user id, as ZCode's sign-in names it: with the
	// site, which account this is (zcodeSame).
	UID string `json:"uid,omitempty"`
}

// ---- ZCode's own account ------------------------------------------------------

func zcodeCredentialsPath() string {
	home, _ := os.UserHomeDir()
	// under the folder ZCode's data was moved to, when it was
	return filepath.Join(appdir.ZCodeDataIn(appdir.ZCodeHome(home)), ".zcode", "v2", "credentials.json")
}

// zcodeSecret is the key ZCode encrypts its credentials with.
func zcodeSecret() []byte {
	seed := os.Getenv("ZCODE_CREDENTIAL_SECRET")
	if seed == "" {
		home, _ := os.UserHomeDir()
		name := ""
		if u, err := user.Current(); err == nil {
			name = u.Username
			if i := strings.LastIndexByte(name, '\\'); i >= 0 {
				name = name[i+1:] // DOMAIN\user; node's userInfo() has the user alone
			}
		}
		platform := runtime.GOOS
		if platform == "windows" {
			platform = "win32"
		}
		seed = "zcode-credential-fallback:" + platform + ":" + home + ":" + name
	}
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func zcodeDecrypt(secret []byte, v string) (string, bool) {
	rest, ok := strings.CutPrefix(v, "enc:v1:")
	if !ok {
		return "", false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return "", false
	}
	var raw [3][]byte
	for i, p := range parts {
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(p, "="))
		if err != nil {
			return "", false
		}
		raw[i] = b
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(raw[0]))
	if err != nil {
		return "", false
	}
	out, err := gcm.Open(nil, raw[0], append(raw[2], raw[1]...), nil)
	return string(out), err == nil
}

// zcodeOwn is the account ZCode is signed in to, its coding plan's key and
// ZCode's session token (the Start Plan's key); ok is false when it has
// neither.
func zcodeOwn() (who string, k zcodeKey, ok bool) {
	var store map[string]string
	if !readJSON(zcodeCredentialsPath(), &store) {
		return "", zcodeKey{}, false
	}
	secret := zcodeSecret()
	family, active := zcodeActiveFamily(store, secret)
	if family == "" {
		return "", zcodeKey{}, false
	}
	var info struct {
		Email       string `json:"email"`
		Name        string `json:"name"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		ID          string `json:"user_id"`
		AlternateID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(zcodeStoreValue(store, secret, "oauth:"+active+":user_info")), &info)
	uid := strings.TrimSpace(firstNonEmpty(info.ID, info.AlternateID))
	for name, v := range store {
		// account-provider:coding-plan:account:zai-individual-coding-plan:account:<uuid>:api-key
		if !strings.Contains(name, ":"+family+"-individual-coding-plan:") || !strings.HasSuffix(name, ":api-key") ||
			uid == "" || !strings.Contains(name, ":account:"+uid+":") {
			continue
		}
		key, ok := zcodeDecrypt(secret, v)
		if !ok || !strings.Contains(key, ".") {
			continue
		}
		if k.Key != "" && k.Key != key {
			return "", zcodeKey{}, false
		}
		k.Key = key
	}
	k.Base = ZCodeZaiBase
	if family == "bigmodel" {
		k.Base = ZCodeBigModelBase
	}
	// ZCode 3.15 keeps no plan key in its credentials; the one it uses
	// sits in config.json, the provider's options.apiKey (#2130)
	if k.Key == "" {
		k = zcodeConfigKey(secret, family)
		if k.Base == "" {
			k.Base = ZCodeZaiBase
			if family == "bigmodel" {
				k.Base = ZCodeBigModelBase
			}
		}
	}
	if v, ok := store["zcodejwttoken"]; ok {
		if s, ok := zcodeDecrypt(secret, v); ok {
			k.JWT = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "Bearer "))
		}
	}
	// ZCode switched to a team's plan: that project's key, found when asked
	if t, ok := zcodeOwnTeam(store, secret, family, active, uid); ok {
		t.JWT = k.JWT
		k = t
	}
	if k.Key == "" && k.JWT == "" && !k.team() {
		return "", zcodeKey{}, false
	}
	k.UID = uid
	return zcodeWho(info.Email, firstNonEmpty(info.Name, info.Username, info.DisplayName), uid), k, true
}

func zcodeStoreValue(store map[string]string, secret []byte, name string) string {
	v := store[name]
	if strings.HasPrefix(v, "enc:") {
		if !strings.HasPrefix(v, "enc:v1:") {
			return ""
		}
		v, _ = zcodeDecrypt(secret, v)
	}
	return strings.TrimSpace(v)
}

// zcodeActiveFamily follows the SDK's active provider and explicit family.
// Legacy data with no active provider is usable only when one family is
// positively identifiable; a second configured family is ambiguous.
func zcodeActiveFamily(store map[string]string, secret []byte) (family, active string) {
	home, _ := os.UserHomeDir()
	var set struct {
		Domain string `json:"providerFamilyDomain"`
	}
	b, err := readAccountFile(filepath.Join(appdir.ZCodeHome(home), ".zcode", "v2", "setting.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && json.Unmarshal(b, &set) != nil {
		return "", ""
	}
	if _, present := store["oauth:active_provider"]; present {
		active = zcodeStoreValue(store, secret, "oauth:active_provider")
		if active != "zai" && active != "bigmodel" && active != "zai_cn" {
			return "", ""
		}
		if set.Domain == "zai" || set.Domain == "bigmodel" {
			return set.Domain, active
		}
		if active == "zai" {
			return "zai", active
		}
		return "bigmodel", active
	}
	if set.Domain == "zai" || set.Domain == "bigmodel" {
		return set.Domain, set.Domain
	}
	var found []string
	for _, site := range []string{"zai", "bigmodel"} {
		has := zcodeStoreValue(store, secret, "oauth:"+site+":user_info") != "" ||
			zcodeStoreValue(store, secret, "oauth:"+site+":access_token") != "" ||
			zcodeConfigKey(secret, site).Key != ""
		for name := range store {
			if strings.Contains(name, ":"+site+"-individual-coding-plan:") && strings.HasSuffix(name, ":api-key") {
				has = true
			}
		}
		if has {
			found = append(found, site)
		}
	}
	if len(found) != 1 {
		return "", ""
	}
	return found[0], found[0]
}

// zcodeConfigKey reads only the requested family's coding plan key.
func zcodeConfigKey(secret []byte, family string) zcodeKey {
	if family != "zai" && family != "bigmodel" {
		return zcodeKey{}
	}
	var cfg struct {
		Provider map[string]struct {
			Options struct {
				APIKey string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if !readJSON(filepath.Join(filepath.Dir(zcodeCredentialsPath()), "config.json"), &cfg) {
		return zcodeKey{}
	}
	for _, p := range []struct {
		ids  []string
		base string
	}{
		{[]string{"builtin:zai-coding-plan", "account:zai-individual-coding-plan"}, ZCodeZaiBase},
		{[]string{"builtin:bigmodel-coding-plan", "account:bigmodel-individual-coding-plan"}, ZCodeBigModelBase},
	} {
		if family == "zai" && p.base != ZCodeZaiBase || family == "bigmodel" && p.base != ZCodeBigModelBase {
			continue
		}
		for _, id := range p.ids {
			key := strings.TrimSpace(cfg.Provider[id].Options.APIKey)
			if strings.HasPrefix(key, "enc:") {
				key, _ = zcodeDecrypt(secret, key)
				key = strings.TrimSpace(key)
			}
			if strings.Contains(key, ".") && !strings.ContainsAny(key, " {$") {
				return zcodeKey{Key: key, Base: p.base}
			}
		}
	}
	return zcodeKey{}
}

// zcodeWho names a Z.ai account: its email, or the phone number of one
// signed in by phone (its email is then <phone>@phone.local).
func zcodeWho(email, name, id string) string {
	email = strings.TrimSuffix(email, "@phone.local")
	return firstNonEmpty(email, name, id, "ZCode")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// ---- the accounts -------------------------------------------------------------

type zcodeLoginKey struct {
	Login
	key zcodeKey
}

func zcodeSaved(l savedLogin) (zcodeKey, bool) {
	var k zcodeKey
	if json.Unmarshal(l.Auth, &k) != nil || k.Key == "" && k.JWT == "" && !k.team() {
		return zcodeKey{}, false
	}
	if k.Base == "" {
		k.Base = ZCodeZaiBase
	}
	return k, true
}

// zcodeLogins is every ZCode account signed in, the first in use first.
func zcodeLogins() []zcodeLoginKey {
	ownUser, own, hasOwn := zcodeOwn()
	if !hasOwn {
		ownUser = ""
	}
	var out []zcodeLoginKey
	for _, l := range sideLogins("zcode", ownUser, func(l savedLogin) bool {
		_, ok := zcodeSaved(l)
		return ok
	}) {
		k := own
		if !l.saved.own() {
			k, _ = zcodeSaved(l.saved)
		}
		out = append(out, zcodeLoginKey{l.Login, k})
	}
	return out
}

// ---- allowance ----------------------------------------------------------------

// zhipuTerm is how long a GLM Coding plan is paid for, from the
// account's subscriptions as ZCode reads them: the valid one's next
// renewal, a charge when it renews itself, else the end of its time — or
// the last date its "valid" span names.
func zhipuTerm(ctx context.Context, root, key string) (*time.Time, string) {
	var subs []zhipuSubscription
	if zcodeGet(ctx, root+"/api/biz/subscription/list", key, &subs) != nil {
		return nil, ""
	}
	return zhipuTermOf(subs)
}

type zhipuSubscription struct {
	Status    string `json:"status"`
	Valid     string `json:"valid"`
	AutoRenew any    `json:"autoRenew"` // true or 1
	NextRenew string `json:"nextRenewTime"`
}

// zhipuDate finds the dates in a span like "2026-09-18 12:00:00-2026-10-18 12:00:00".
var zhipuDate = lazyre.New(`\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}:\d{2})?`)

// zhipuTime reads the plan's times, "2026-10-18 12:00:00" in Beijing.
func zhipuTime(s string) *time.Time {
	s = strings.Replace(strings.TrimSpace(s), " ", "T", 1)
	cst := time.FixedZone("CST", 8*3600)
	for _, f := range []string{"2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(f, s, cst); err == nil {
			return &t
		}
	}
	return nil
}

func zhipuTermOf(subs []zhipuSubscription) (*time.Time, string) {
	for _, s := range subs {
		if !strings.EqualFold(s.Status, "VALID") {
			continue
		}
		auto := s.AutoRenew == true || s.AutoRenew == float64(1)
		if t := zhipuTime(s.NextRenew); t != nil {
			if auto {
				return t, "auto"
			}
			return t, "off"
		}
		if ds := zhipuDate.FindAllString(s.Valid, -1); len(ds) > 0 && !auto {
			if t := zhipuTime(ds[len(ds)-1]); t != nil {
				return t, "off"
			}
		}
		return nil, ""
	}
	return nil, ""
}

// zcodeRoot is the site a plan's endpoint is on: https://api.z.ai.
func zcodeRoot(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return zcodeZaiAPI
}

// ---- Z.ai's API ---------------------------------------------------------------

// zcodeCall asks one of Z.ai's JSON endpoints, which wrap what they say in
// {code, msg, data}: code 0 or 200 is a success.
func zcodeCall(ctx context.Context, method, u, auth string, body, dst any) error {
	return zcodeCallH(ctx, method, u, auth, nil, body, dst)
}

// zcodeCallH is zcodeCall with more headers: a team's organization and
// project.
func zcodeCallH(ctx context.Context, method, u, auth string, hdr map[string]string, body, dst any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)
	zcodeDeviceHeader(req)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := Keyed(http.DefaultClient).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var env struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if json.Unmarshal(b, &env) == nil && env.Msg != "" {
			if c := strings.Trim(string(env.Code), `"`); c != "" && c != "null" && c != "0" {
				return fmt.Errorf("%s (%d, code %s)", env.Msg, res.StatusCode, c)
			}
			return fmt.Errorf("%s (%d)", env.Msg, res.StatusCode)
		}
		return &accountStatusError{status: res.StatusCode}
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	if c := strings.Trim(string(env.Code), `"`); c != "" && c != "0" && c != "200" && c != "null" {
		return errors.New(firstNonEmpty(env.Msg, "error "+c))
	}
	if dst == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, dst)
}

// zcodeGet asks with a coding plan's key, which goes bare in Authorization.
func zcodeGet(ctx context.Context, u, key string, dst any) error {
	return zcodeCall(ctx, http.MethodGet, u, key, nil, dst)
}

// ---- signing in ---------------------------------------------------------------
