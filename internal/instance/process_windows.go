//go:build windows

package instance

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func processMatches(pid int, executable string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	output, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false, err
	}
	line := strings.TrimSpace(string(output))
	if line == "" || strings.HasPrefix(line, "INFO:") {
		return false, nil
	}
	first := line
	if index := strings.Index(first, ","); index >= 0 {
		first = first[:index]
	}
	image := strings.Trim(first, "\" ")
	return strings.EqualFold(image, filepath.Base(executable)), nil
}
