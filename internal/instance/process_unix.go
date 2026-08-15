//go:build !windows

package instance

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func processMatches(pid int, executable string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return false, err
		}
	}
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false, err
	}
	command := strings.TrimSpace(string(output))
	executable = filepath.Clean(strings.TrimSpace(executable))
	return command == executable || strings.HasPrefix(command, executable+" "), nil
}
