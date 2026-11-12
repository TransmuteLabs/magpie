package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/settings"
)

// AuthEntries is plugin-auth.json's sign-ins by key, each as written, for
// a backup that carries them. A file that can't be read is an error,
// never no sign-ins: carried as none, it would read as every sign-in
// signed out.
func AuthEntries() (map[string]json.RawMessage, error) {
	b, err := readAuthFile()
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// AuthWho is who a sign-in entry is signed in as (its accountId, or the
// email it carries), "" when it doesn't say.
func AuthWho(raw json.RawMessage) string {
	var a storedAuth
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	return firstNonEmpty(a.AccountID, a.Metadata.Email, a.Email)
}

// AuthZCodeScope validates the intrinsic account's portable, nonsecret
// selection scope against the same family and selection used by its runtime.
func AuthZCodeScope(raw json.RawMessage) (string, bool) {
	var a storedAuth
	if json.Unmarshal(raw, &a) != nil {
		return "", false
	}
	return zcodeAuthScope(a)
}

func authString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var value string
	err := json.Unmarshal(raw, &value)
	return value, err == nil
}

func zcodeAuthScope(a storedAuth) (string, bool) {
	var state struct {
		Version int    `json:"version"`
		Site    string `json:"site"`
		UID     string `json:"uid"`
		Kind    string `json:"kind"`
		Key     string `json:"key"`
		Org     string `json:"org"`
		Project string `json:"project"`
		Product string `json:"product"`
	}
	uidField, uidOK := authString(a.UID)
	scopeField, scopeOK := authString(a.Scope)
	siteField, siteOK := authString(a.Metadata.Site)
	if !uidOK || !scopeOK || !siteOK {
		return "", false
	}
	if a.Type == "api" {
		state.Site, state.Kind, state.Key = siteField, "legacy-key", a.Key
		if state.Site == "" {
			state.Site = "zai"
		}
	} else {
		var refresh string
		if a.Type != "oauth" || json.Unmarshal(a.Refresh, &refresh) != nil ||
			json.Unmarshal([]byte(refresh), &state) != nil || state.Version != 0 && state.Version != 1 {
			return "", false
		}
	}
	state.UID, uidField = strings.TrimSpace(state.UID), strings.TrimSpace(uidField)
	if state.UID != "" && uidField != "" && state.UID != uidField {
		return "", false
	}
	uid := firstNonEmpty(state.UID, uidField)
	if state.Site != "zai" && state.Site != "bigmodel" {
		return "", false
	}
	if state.Kind != "individual" && state.Kind != "start" && state.Kind != "team" && state.Kind != "legacy-key" {
		return "", false
	}
	if state.Kind == "team" && (strings.TrimSpace(state.Org) == "" || strings.TrimSpace(state.Project) == "" || strings.TrimSpace(state.Product) == "") {
		return "", false
	}
	if state.Kind == "legacy-key" && (strings.TrimSpace(state.Org) == "") != (strings.TrimSpace(state.Project) == "") {
		return "", false
	}
	if uid == "" && state.Kind == "legacy-key" && strings.TrimSpace(state.Key) != "" {
		sum := sha256.Sum256([]byte(strings.TrimSpace(state.Key)))
		uid = "unidentified:" + hex.EncodeToString(sum[:])
	}
	if uid == "" {
		return "", false
	}
	business := "https://api.z.ai"
	if state.Site == "bigmodel" {
		business = "https://bigmodel.cn"
	}
	org, project, product := "", "", ""
	if state.Kind == "team" || state.Kind == "legacy-key" && (state.Org != "" || state.Project != "") {
		org, project, product = strings.TrimSpace(state.Org), strings.TrimSpace(state.Project), strings.TrimSpace(state.Product)
	}
	parts := []string{business, "https://zcode.z.ai", state.Site, uid, state.Kind, org, project, product}
	var encoded []byte
	encoded = append(encoded, '[')
	for i, part := range parts {
		if i != 0 {
			encoded = append(encoded, ',')
		}
		encoded = appendJSString(encoded, part)
	}
	encoded = append(encoded, ']')
	sum := sha256.Sum256(encoded)
	scope := hex.EncodeToString(sum[:])
	if scopeField != "" && scopeField != scope {
		return "", false
	}
	return scope, true
}

// JSON.stringify leaves UTF-8 and HTML characters unescaped; quote the
// strings in its scope tuple without exposing the credential used to form it.
func appendJSString(dst []byte, value string) []byte {
	const hexDigits = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(value); i++ {
		b := value[i]
		switch b {
		case '"', '\\':
			dst = append(dst, '\\', b)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if b < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&15])
			} else {
				dst = append(dst, b)
			}
		}
	}
	return append(dst, '"')
}

// AuthExpires is when an OAuth sign-in entry's access token runs out, in
// Unix milliseconds: a later one is a later refresh. 0 when it doesn't say.
func AuthExpires(raw json.RawMessage) int64 {
	var a struct {
		Expires int64 `json:"expires"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return 0
	}
	return a.Expires
}

// EditAuth changes plugin-auth.json under the shared OS auth lock, read
// afresh there: f gets the sign-ins by key and
// says whether it changed them. A file that can't be read is not written
// over; the one there is kept as its last good generation first, and the
// new one is renamed in whole.
func EditAuth(f func(m map[string]json.RawMessage) (bool, error)) error {
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		return err
	}
	unlock, err := lockAuth()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := AuthEntries()
	if err != nil {
		return err
	}
	ch, err := f(m)
	if err != nil || !ch {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := lastgood.Keep(AuthPath(), validAuth, 0o600); err != nil {
		return err
	}
	if err := writeWhole(AuthPath(), append(b, '\n')); err != nil {
		return err
	}
	changed()
	return nil
}
