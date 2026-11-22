//go:build !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package extension

func newActivationVnodeWatch() activationVnodeWatch { return nil }
