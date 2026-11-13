package agent

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
)

func extensionLegacyAgentOwned(plugin, id string) (bool, error) {
	stashMu.Lock()
	defer stashMu.Unlock()
	info, err := os.Lstat(stashPath())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > extensionBindingLimit {
		return false, errors.New("previous agent ownership is unreadable")
	}
	file, err := os.Open(stashPath())
	if err != nil {
		return false, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || actual.Size() > extensionBindingLimit {
		return false, errors.New("previous agent ownership changed during read")
	}
	values := map[string]string{}
	decoder := json.NewDecoder(io.LimitReader(file, extensionBindingLimit+1))
	if decoder.Decode(&values) != nil || values == nil || decoder.Decode(new(any)) != io.EOF {
		return false, errors.New("previous agent ownership is invalid")
	}
	return values["sdk.agent."+plugin+":"+id] != "", nil
}
