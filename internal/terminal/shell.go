package terminal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ExecutionShell returns the shell used for command-string execution.
// MCPX command policy and command syntax are Bash-oriented on Unix, so this
// deliberately resolves Bash rather than blindly executing the user's
// interactive shell (which may be zsh, fish, etc.).
func ExecutionShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}

	// Respect an explicitly selected Bash when SHELL already points at one.
	// This is especially useful for Homebrew/MacPorts Bash installations.
	if shell := strings.TrimSpace(os.Getenv("SHELL")); filepath.Base(shell) == "bash" && executableFile(shell) {
		return shell
	}

	// macOS ships /bin/bash 3.2. Prefer common modern Bash installations before
	// falling back to the system Bash. This avoids loading modern login-shell
	// tooling (SDKMAN, etc.) under the obsolete system Bash.
	if runtime.GOOS == "darwin" {
		for _, candidate := range []string{"/opt/homebrew/bin/bash", "/usr/local/bin/bash"} {
			if executableFile(candidate) {
				return candidate
			}
		}
	}

	if candidate, err := exec.LookPath("bash"); err == nil && executableFile(candidate) {
		return candidate
	}
	return "/bin/bash"
}

func commandShell(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, ExecutionShell(), "-lc", command)
}

func executableFile(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
