//go:build windows

package terminal

import (
	"os/exec"
	"time"
)

func configureProcess(_ *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func processCPUTime(int) (time.Duration, bool) {
	return 0, false
}
