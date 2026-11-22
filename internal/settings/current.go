package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/steady"
)

const settingsFileLimit = 32 << 20

var errSettingsFileChanged = errors.New("settings file changed while reading")

// Publication and strict reads share this lock. A writer can run its checks
// under fileMu, so strict reads must not wait for that broader lock.
var settingsPublicationMu sync.RWMutex

// LoadCurrent returns an owned copy of the settings in the primary file.
// An absent file gives the defaults. A present file must contain a readable object.
func LoadCurrent() (Settings, error) {
	current, err := readCurrentSettings()
	if err != nil {
		return Settings{}, err
	}
	if current == nil {
		return Defaults(), nil
	}
	return current.clone().normal(), nil
}

// PluginOptionsNow reads provider switches and discovery policy together.
// The switch map is shared and must only be read.
func PluginOptionsNow() (options map[string]map[string]bool, discoveryOff bool, err error) {
	current, err := readCurrentSettings()
	if err != nil || current == nil {
		return nil, noAccountsEnv(), err
	}
	return current.PluginOptions, AccountDiscoveryOffIn(*current), nil
}

func readCurrentSettings() (*Settings, error) {
	settingsPublicationMu.RLock()
	defer settingsPublicationMu.RUnlock()
	return readCurrentSettingsWith(readCurrentSettingsFile)
}

func readCurrentSettingsWith(snapshot func(string, os.FileInfo, []byte) ([]byte, error)) (*Settings, error) {
	path := pathNow()
	var err error
	for attempt := range 3 {
		var current *Settings
		current, err = filememo.ReadNowWithSnapshot("settings:current", path, parseCurrentObject, snapshot)
		if err == nil {
			return current, nil
		}
		if errors.Is(err, errSettingsFileChanged) {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			break
		}
		if _, entryErr := os.Lstat(path); errors.Is(entryErr, os.ErrNotExist) {
			if attempt == 0 {
				return nil, nil
			}
			err = errSettingsFileChanged
			break
		}
		if _, statErr := os.Stat(path); statErr != nil {
			err = statErr
			break
		}
		err = errSettingsFileChanged
	}
	return nil, fmt.Errorf("could not read current settings at %s: %w", path, err)
}

func parseCurrentObject(body []byte) (*Settings, error) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("settings must contain an object")
	}
	s, err := parseCurrentSettings(body)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func readCurrentSettingsFile(path string, before os.FileInfo, room []byte) ([]byte, error) {
	body, _, err := readSettingsFile(path, before, room, steady.Stat)
	return body, err
}

func readSettingsSnapshot(path string, stat func(string) (os.FileInfo, error)) ([]byte, os.FileInfo, error) {
	before, err := stat(path)
	if err != nil {
		return nil, nil, err
	}
	return readSettingsFile(path, before, nil, stat)
}

func readSettingsFile(path string, before os.FileInfo, body []byte, stat func(string) (os.FileInfo, error)) ([]byte, os.FileInfo, error) {
	if !before.Mode().IsRegular() || before.Size() > settingsFileLimit {
		return nil, nil, errors.New("settings require a bounded regular file")
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, errSettingsFileChanged
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, nil, errSettingsFileChanged
	}
	if !opened.Mode().IsRegular() || opened.Size() > settingsFileLimit {
		return nil, nil, errors.New("settings require a bounded regular file")
	}
	size := int(opened.Size()) + 1
	if cap(body) < size {
		body = make([]byte, size)
	}
	count, err := io.ReadFull(file, body[:size])
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		if err == nil {
			return nil, nil, errSettingsFileChanged
		}
		return nil, nil, errors.New("settings file could not be read")
	}
	after, err := stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, errSettingsFileChanged
	}
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(opened, after) || opened.Size() != after.Size() ||
		int64(count) != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, nil, errSettingsFileChanged
	}
	return body[:count], after, nil
}
