//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the sign-in helper in its own session so that killing the
// bridge (or its process group — the Claude desktop app kills the whole group)
// does not kill the browser sign-in in progress.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
