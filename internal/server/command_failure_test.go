package server

import "testing"

func TestCommandFailureCodeRecognizesExit127RegardlessOfLocale(t *testing.T) {
	for _, stderr := range []string{
		"bash: missing: command not found\n",
		"/opt/homebrew/bin/bash: 行 1: missing: 未找到命令\n",
		"",
	} {
		if got := commandFailureCode(127, stderr); got != "COMMAND_NOT_FOUND" {
			t.Fatalf("commandFailureCode(127, %q) = %q", stderr, got)
		}
	}
	if got := commandFailureCode(126, "command not found"); got != "PROCESS_EXIT" {
		t.Fatalf("commandFailureCode(126, ...) = %q", got)
	}
}
