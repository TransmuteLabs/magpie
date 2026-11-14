//go:build cgo && (darwin || freebsd || windows)

package extension

import "errors"

func nativeDependencyIdentity(filename string) (string, error) {
	return "", errors.New("native dependency equivalence is not established on this platform")
}
