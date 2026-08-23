//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureBackgroundProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func terminateBackgroundProcess(pid int, executable string, timeout time.Duration) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, fmt.Errorf("invalid daemon pid %d", pid)
	}
	alive, err := backgroundProcessAlive(pid)
	if err != nil {
		return false, err
	}
	if !alive {
		return false, nil
	}
	matches, err := backgroundProcessMatches(pid, executable)
	if err != nil {
		return false, fmt.Errorf("verify daemon pid %d: %w", pid, err)
	}
	if !matches {
		return false, fmt.Errorf("pid %d no longer matches daemon executable %s", pid, executable)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("terminate daemon pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, err := backgroundProcessAlive(pid)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("kill daemon pid %d: %w", pid, err)
	}
	return true, nil
}

func backgroundProcessAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, fmt.Errorf("check daemon pid %d: %w", pid, err)
	}
}

func backgroundProcessMatches(pid int, executable string) (bool, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false, err
	}
	return backgroundCommandMatches(strings.TrimSpace(string(output)), executable), nil
}

func managedPluginProcessMatches(pid int, executable, argv0 string) (bool, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false, err
	}
	return managedPluginCommandMatches(strings.TrimSpace(string(output)), executable, argv0), nil
}

func managedPluginCommandMatches(command, executable, argv0 string) bool {
	command = strings.TrimSpace(command)
	for _, identity := range []string{strings.TrimSpace(argv0), strings.TrimSpace(executable)} {
		if identity == "" {
			continue
		}
		if backgroundCommandMatches(command, identity) || commandContainsExactArgument(command, identity) {
			return true
		}
	}
	return false
}

func commandContainsExactArgument(command, argument string) bool {
	command = strings.TrimSpace(command)
	argument = strings.TrimSpace(argument)
	if command == "" || argument == "" {
		return false
	}
	return command == argument || strings.HasPrefix(command, argument+" ") || strings.HasSuffix(command, " "+argument) || strings.Contains(command, " "+argument+" ")
}

func terminateManagedPluginProcess(pid int, executable, argv0 string, timeout time.Duration) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, fmt.Errorf("invalid Plugin pid %d", pid)
	}
	alive, err := backgroundProcessAlive(pid)
	if err != nil {
		return false, err
	}
	if !alive {
		return false, nil
	}
	matches, err := managedPluginProcessMatches(pid, executable, argv0)
	if err != nil {
		return false, fmt.Errorf("verify Plugin pid %d: %w", pid, err)
	}
	if !matches {
		return false, fmt.Errorf("pid %d no longer matches managed Plugin process", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("terminate Plugin pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, err := backgroundProcessAlive(pid)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("kill Plugin pid %d: %w", pid, err)
	}
	return true, nil
}

func discoverBackgroundProcesses(executable string) ([]int, error) {
	output, err := exec.Command("ps", "-axo", "pid=,sess=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parseDetachedBackgroundProcesses(string(output), executable), nil
}

func parseDetachedBackgroundProcesses(output, executable string) []int {
	pids := make([]int, 0, 1)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		sessionID, err := strconv.Atoi(fields[1])
		if err != nil || sessionID != pid {
			continue
		}
		commandStart := strings.Index(line, fields[2])
		if commandStart < 0 || !backgroundCommandMatches(strings.TrimSpace(line[commandStart:]), executable) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

func backgroundCommandMatches(command, executable string) bool {
	executable = filepath.Clean(strings.TrimSpace(executable))
	command = strings.TrimSpace(command)
	if command == "" || executable == "" {
		return false
	}
	return command == executable || strings.HasPrefix(command, executable+" ")
}
