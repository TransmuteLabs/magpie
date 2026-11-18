//go:build !unix

package extension

import "os/exec"

// proc.CommandContext already supplies the Windows child-tree owner.
func prepareWorker(cmd *exec.Cmd) {}
