package settings

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

func EditFoldReferences(apply func(current *Settings, check func() error) (bool, error)) error {
	defer lockFile()()
	path := Path()
	body, info, err := readFoldSettings(path)
	if err != nil {
		return err
	}
	current := Defaults()
	if len(bytes.TrimSpace(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")))) != 0 {
		parsed, err := parseCurrentSettings(body)
		if err != nil {
			return fmt.Errorf("could not read settings at %s: %w", path, err)
		}
		current = parsed.clone().normal()
	}
	check := func() error {
		next, fresh, err := readFoldSettings(path)
		if err != nil {
			return err
		}
		sameFile := info == nil && fresh == nil || info != nil && fresh != nil && os.SameFile(info, fresh)
		if !sameFile || !bytes.Equal(body, next) {
			return fmt.Errorf("%s: %w", path, errSettingsFileChanged)
		}
		return nil
	}
	changed, err := apply(&current, check)
	if err != nil || !changed {
		return err
	}
	if err := current.RequestPolicy.Validate(); err != nil {
		return err
	}
	return saveOwnChecked(current, false, false, check)
}

func readFoldSettings(path string) ([]byte, os.FileInfo, error) {
	body, info, err := readSettingsSnapshot(path, os.Stat)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("could not read settings at %s: %w", path, err)
	}
	return body, info, nil
}
