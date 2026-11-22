package plugin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
)

// AuthEntries is plugin-auth.json's credentials by key for a backup
// that carries them. A file that can't be read is an error,
// never no sign-ins: carried as none, it would read as every sign-in
// signed out.
func AuthEntries() (map[string]json.RawMessage, error) {
	records, _, err := readAuthEntries()
	if err != nil {
		return nil, err
	}
	return authEntryCredentials(records)
}

func authEntryCredentials(records map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(records))
	for key, raw := range records {
		record, err := authCredentials(raw)
		if err != nil {
			return nil, err
		}
		out[key] = record
	}
	return out, nil
}

func readAuthEntries() (map[string]json.RawMessage, []byte, error) {
	b, err := readAuthFile()
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, []byte("{}"), nil
	}
	if err != nil {
		return nil, nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, b, nil
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

// EditAuth changes plugin-auth.json under the shared writer lock. f gets
// the sign-ins by key and says whether it changed them. A file that can't
// be read is left as it is.
func EditAuth(f func(m map[string]json.RawMessage) (bool, error)) error {
	path := AuthPath()
	unlock, err := lockAuth(context.Background(), path)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		unlock()
		if published {
			changed()
		}
	}()
	records, before, err := readAuthEntries()
	if err != nil {
		return err
	}
	credentials, err := authEntryCredentials(records)
	if err != nil {
		return err
	}
	m := maps.Clone(credentials)
	ch, err := f(m)
	if err != nil || !ch {
		return err
	}
	for key, raw := range m {
		if sameAuthRecord(credentials[key], raw) {
			m[key] = records[key]
			continue
		}
		var record map[string]json.RawMessage
		if json.Unmarshal(raw, &record) != nil || record == nil {
			continue
		}
		record[authEpochField], _ = json.Marshal(rand.Text())
		m[key], err = json.Marshal(record)
		if err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := publishAuth(context.Background(), path, before, b, validAuth); err != nil {
		return err
	}
	published = true
	return nil
}
