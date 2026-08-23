//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func configureBackgroundProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

func terminateBackgroundProcess(pid int, executable string, timeout time.Duration) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, fmt.Errorf("invalid daemon pid %d", pid)
	}
	alive, matches, err := windowsBackgroundProcessState(pid, executable)
	if err != nil {
		return false, err
	}
	if !alive {
		return false, nil
	}
	if !matches {
		return false, fmt.Errorf("pid %d no longer matches daemon executable %s", pid, executable)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, fmt.Errorf("find daemon pid %d: %w", pid, err)
	}
	if err := process.Kill(); err != nil {
		return false, fmt.Errorf("kill daemon pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, _, err := windowsBackgroundProcessState(pid, executable)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, fmt.Errorf("daemon pid %d did not exit after kill", pid)
}

func managedPluginProcessMatches(pid int, executable, argv0 string) (bool, error) {
	alive, matches, err := windowsBackgroundProcessState(pid, executable)
	if err != nil || !alive {
		return false, err
	}
	return matches, nil
}

func terminateManagedPluginProcess(pid int, executable, argv0 string, timeout time.Duration) (bool, error) {
	return terminateBackgroundProcess(pid, executable, timeout)
}

func discoverBackgroundProcesses(executable string) ([]int, error) {
	return nil, nil
}

func windowsBackgroundProcessState(pid int, executable string) (bool, bool, error) {
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
