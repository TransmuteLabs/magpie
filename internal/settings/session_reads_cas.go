package settings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
)

var ErrSessionReadsChanged = errors.New("session read consent changed")

type SessionReadsSnapshot struct {
	Reads    *SessionReads
	Revision string
}

func sessionReadsFile() ([]byte, string, error) {
	before, err := os.Lstat(Path())
	if errors.Is(err, os.ErrNotExist) {
		hash := sha256.Sum256([]byte(Path() + "\x00absent"))
		return nil, hex.EncodeToString(hash[:]), nil
	}
	if err != nil {
		return nil, "", err
	}
	if !before.Mode().IsRegular() || before.Size() > 32<<20 {
		return nil, "", errors.New("settings consent requires a bounded regular file")
	}
	file, err := os.Open(Path())
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, "", ErrSessionReadsChanged
	}
	body, err := io.ReadAll(io.LimitReader(file, (32<<20)+1))
	if err != nil || len(body) > 32<<20 {
		return nil, "", errors.New("settings consent could not be read")
	}
	after, err := os.Lstat(Path())
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() ||
		!opened.ModTime().Equal(after.ModTime()) {
		return nil, "", ErrSessionReadsChanged
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
	fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%d\x00%d\x00", Path(), identity, after.Size(), after.ModTime().UnixNano(), after.Mode())
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
