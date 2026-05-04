//go:build windows

package procutil

import (
	"os/exec"
	"syscall"
)

// HideWindow suppresses command prompt flashes for child processes on Windows.
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
