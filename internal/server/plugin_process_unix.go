//go:build !windows

package server

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

func terminateManagedPluginProcess(pid int, executable, argv0 string, expectedArgs []string, timeout time.Duration) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, fmt.Errorf("invalid Plugin pid %d", pid)
	}
	alive, err := managedPluginProcessAlive(pid)
	if err != nil {
		return false, err
	}
	if !alive {
		return false, nil
	}
	matches, err := managedPluginProcessMatches(pid, executable, argv0, expectedArgs)
	if err != nil {
		return false, fmt.Errorf("verify Plugin pid %d: %w", pid, err)
	}
	if !matches {
		return false, fmt.Errorf("pid %d no longer matches persisted managed Plugin identity", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("terminate Plugin pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, err := managedPluginProcessAlive(pid)
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

func managedPluginProcessAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, fmt.Errorf("check Plugin pid %d: %w", pid, err)
	}
}

func managedPluginProcessMatches(pid int, executable, argv0 string, expectedArgs []string) (bool, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false, err
	}
	return managedPluginCommandMatches(strings.TrimSpace(string(output)), executable, argv0, expectedArgs), nil
}

func managedPluginCommandMatches(command, executable, argv0 string, expectedArgs []string) bool {
	identityMatched := false
	for _, identity := range []string{strings.TrimSpace(executable), strings.TrimSpace(argv0)} {
		if identity != "" && commandContainsExactPluginArgument(command, identity) {
			identityMatched = true
			break
		}
	}
	pathArgumentMatched := false
	for _, argument := range expectedArgs {
		argument = strings.TrimSpace(argument)
		if argument == "" {
			continue
		}
		if !commandContainsExactPluginArgument(command, argument) {
			return false
		}
		// Some interpreters re-exec into a different executable image after
		// launch (notably macOS python3 -> Python.app). An exact Plugin script
		// path remains a strong secondary identity anchor; generic flags/words do
		// not qualify on their own.
		if filepath.IsAbs(argument) || strings.Contains(argument, "/") {
			pathArgumentMatched = true
		}
	}
	return identityMatched || pathArgumentMatched
}

func commandContainsExactPluginArgument(command, argument string) bool {
	command = strings.TrimSpace(command)
	argument = strings.TrimSpace(argument)
	if command == "" || argument == "" {
		return false
	}
	return command == argument || strings.HasPrefix(command, argument+" ") || strings.HasSuffix(command, " "+argument) || strings.Contains(command, " "+argument+" ")
}
