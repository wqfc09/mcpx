package terminal

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExecutionShellPrefersBashFromShellEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell resolution test")
	}
	dir := t.TempDir()
	bash := filepath.Join(dir, "bash")
	if err := os.WriteFile(bash, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bash, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", bash)

	if got := ExecutionShell(); got != bash {
		t.Fatalf("ExecutionShell() = %q, want %q", got, bash)
	}
	cmd := commandShell(context.Background(), "printf ok")
	if cmd.Path != bash {
		t.Fatalf("command shell path = %q, want %q", cmd.Path, bash)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-lc" || cmd.Args[2] != "printf ok" {
		t.Fatalf("command shell args = %#v", cmd.Args)
	}
}
