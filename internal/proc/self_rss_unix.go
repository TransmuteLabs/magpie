//go:build !windows && (!darwin || !cgo)

package proc

import "os"

// SelfRSS returns this process's resident memory in bytes.
func SelfRSS() (int64, bool) { return pidRSS(os.Getpid()) }
