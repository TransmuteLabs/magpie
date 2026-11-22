package extension

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
)

const observerEnablementLimit = 128

func observerEnablementFile(id string) string {
	return ".observer-" + storeHash(id) + ".json"
}

// An absent marker is the pre-toggle generation. Other failures mean the
// generation is unknown and cannot admit module calls.
func readObserverEnablement(root *os.Root, id string) (string, error) {
	name := observerEnablementFile(id)
	if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	raw, err := catalogReadFile(root, name, observerEnablementLimit)
	var value string
	if err != nil || json.Unmarshal(raw, &value) != nil || len(value) < 26 || strings.ContainsFunc(value, func(char rune) bool {
		return !(char >= 'A' && char <= 'Z' || char >= '2' && char <= '7')
	}) {
		return "", errors.New("extension observer enablement is unreadable")
	}
	return value, nil
}

// The catalog lock covers both writes. Persist the fence before enabling the
// module. A failed catalog publication can only invalidate
// queued events conservatively.
func advanceObserverEnablement(root *os.Root, id string) error {
	if _, err := readObserverEnablement(root, id); err != nil {
		return err
	}
	return storeWrite(root, observerEnablementFile(id), rand.Text(), observerEnablementLimit)
}
