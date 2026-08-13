//go:build !windows

package terminal

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// processCPUTime is a best-effort CPU-time probe for the primary runtime
// process. Process-tree termination is still enforced when the budget is hit.
func processCPUTime(pid int) (time.Duration, bool) {
	if pid <= 0 {
		return 0, false
	}
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	return parseProcessCPUTime(strings.TrimSpace(string(out)))
}

func parseProcessCPUTime(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	var days int64
	if dash := strings.IndexByte(value, '-'); dash >= 0 {
		parsed, err := strconv.ParseInt(strings.TrimSpace(value[:dash]), 10, 64)
		if err != nil || parsed < 0 {
			return 0, false
		}
		days = parsed
		value = value[dash+1:]
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	minutes, err := strconv.ParseInt(parts[len(parts)-2], 10, 64)
	if err != nil || minutes < 0 {
		return 0, false
	}
	var hours int64
	if len(parts) == 3 {
		hours, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || hours < 0 {
			return 0, false
		}
	}
	totalSeconds := float64(days*24*60*60+hours*60*60+minutes*60) + seconds
	return time.Duration(totalSeconds * float64(time.Second)), true
}
