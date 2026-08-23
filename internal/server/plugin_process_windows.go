//go:build windows

package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func terminateManagedPluginProcess(pid int, executable, _ string, _ []string, timeout time.Duration) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, fmt.Errorf("invalid Plugin pid %d", pid)
	}
	alive, matches, err := windowsManagedPluginProcessState(pid, executable)
	if err != nil {
		return false, err
	}
	if !alive {
		return false, nil
	}
	if !matches {
		return false, fmt.Errorf("pid %d no longer matches persisted managed Plugin identity", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, fmt.Errorf("find Plugin pid %d: %w", pid, err)
	}
	if err := process.Kill(); err != nil {
		return false, fmt.Errorf("kill Plugin pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, _, err := windowsManagedPluginProcessState(pid, executable)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, fmt.Errorf("Plugin pid %d did not exit after kill", pid)
}

func windowsManagedPluginProcessState(pid int, executable string) (bool, bool, error) {
	output, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false, false, err
	}
	line := strings.TrimSpace(string(output))
	if line == "" || strings.HasPrefix(line, "INFO:") {
		return false, false, nil
	}
	first := line
	if index := strings.Index(first, ","); index >= 0 {
		first = first[:index]
	}
	image := strings.Trim(first, "\" ")
	return true, strings.EqualFold(image, filepath.Base(executable)), nil
}
