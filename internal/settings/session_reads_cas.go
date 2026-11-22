package settings

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
)

var ErrSessionReadsChanged = errors.New("session read consent changed")

type SessionReadsSnapshot struct {
	Reads    *SessionReads
	Revision string
}

func nextSessionReads(before, after *SessionReads) *SessionReads {
	if after == nil {
		return nil
	}
	next := *after
	if before != nil && before.Asked == next.Asked && before.All == next.All && maps.Equal(before.Agents, next.Agents) {
		next.Generation = before.Generation
	} else {
		next.Generation = rand.Text()
	}
	return &next
}

func sessionReadsFile() ([]byte, string, error) {
	path := Path()
	body, after, err := readSettingsSnapshot(path, os.Lstat)
	if errors.Is(err, os.ErrNotExist) {
		hash := sha256.Sum256([]byte(path + "\x00absent"))
		return nil, hex.EncodeToString(hash[:]), nil
	}
	if errors.Is(err, errSettingsFileChanged) {
		return nil, "", ErrSessionReadsChanged
	}
	if err != nil {
		return nil, "", err
	}
	if len(bytes.TrimSpace(body)) != 0 && !validSettings(body) {
		return nil, "", errors.New("settings consent is malformed")
	}
	native := reflect.Indirect(reflect.ValueOf(after.Sys()))
	identity := ""
	if native.IsValid() && native.Kind() == reflect.Struct {
		for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "VolumeSerialNumber", "FileIndexHigh", "FileIndexLow"} {
			if field := native.FieldByName(name); field.IsValid() && field.CanInterface() {
				identity += fmt.Sprintf("%s=%v;", name, field.Interface())
			}
		}
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%d\x00%d\x00", path, identity, after.Size(), after.ModTime().UnixNano(), after.Mode())
	hash.Write(body)
	return body, hex.EncodeToString(hash.Sum(nil)), nil
}

// Decode the bytes whose revision was verified. A metadata memo can still hold
// an older decision after a foreign same-size edit with a restored timestamp.
func inspectSessionReads() (SessionReadsSnapshot, error) {
	body, revision, err := sessionReadsFile()
	if err != nil {
		return SessionReadsSnapshot{}, err
	}
	var current struct {
		Reads *SessionReads `json:"sessionReads"`
	}
	if len(bytes.TrimSpace(body)) != 0 {
		if err := json.Unmarshal(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")), &current); err != nil {
			return SessionReadsSnapshot{}, err
		}
	}
	return SessionReadsSnapshot{Reads: current.Reads, Revision: revision}, nil
}

func InspectSessionReads() (SessionReadsSnapshot, error) {
	fileMu.RLock()
	defer fileMu.RUnlock()
	return inspectSessionReads()
}

func CompareAndSwapSessionRead(expected, agent string, allowed bool, checks ...func() error) error {
	if expected == "" || agent == "" {
		return ErrSessionReadsChanged
	}
	defer lockFile()()
	snapshot, err := inspectSessionReads()
	if err != nil {
		return err
	}
	if snapshot.Revision != expected {
		return ErrSessionReadsChanged
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	body, revision, err := sessionReadsFile()
	if err != nil {
		return err
	}
	if revision != expected {
		return ErrSessionReadsChanged
	}
	current := Defaults()
	if body != nil {
		current, err = parse(body)
		if err != nil {
			return err
		}
	}
	reads := current.clone().SessionReads
	if reads == nil {
		reads = &SessionReads{}
	}
	reads.Asked = true
	if reads.Agents == nil {
		reads.Agents = map[string]bool{}
	}
	reads.Agents[agent] = allowed
	current.SessionReads = reads
	return saveOwn(current, false, true)
}
