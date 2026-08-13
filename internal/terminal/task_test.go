package terminal

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcpx/internal/auth"
	"mcpx/internal/remotesession"
	"mcpx/internal/state"
)

func TestTaskStartLogsKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	m := NewTaskManager()
	task, err := m.StartRemote(context.Background(), "rs1", "", t.TempDir(), "echo hello-task; sleep 2")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var chunk string
	for time.Now().Before(deadline) {
		chunk, _ = task.Logs(0)
		if strings.Contains(chunk, "hello-task") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(chunk, "hello-task") {
		t.Fatalf("logs %q", chunk)
	}
	if err := task.Kill(); err != nil {
		t.Fatal(err)
	}
	st := task.StatusView()
	if st["status"] != TaskKilled {
		t.Fatalf("%v", st)
	}
}

func TestPersistentTaskSurvivesManagerRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell assertion is Unix-specific")
	}
	databasePath := filepath.Join(t.TempDir(), "mcpx.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	principal := auth.Principal{ID: "task-principal", Kind: "test", SubjectHash: "task-subject"}
	created, err := remotesession.NewService(store.DB()).Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "project", WorkspacePath: t.TempDir(), Label: "task test",
	})
	if err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(t.TempDir(), "tasks")
	manager, err := NewPersistentTaskManager(store.DB(), logDir)
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.StartRemote(context.Background(), created.Session.ID, "project", created.Session.WorkspacePath, "printf persistent-log")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if task.StatusView()["status"] != TaskRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	manager.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := NewPersistentTaskManager(reopened.DB(), logDir)
	if err != nil {
		t.Fatal(err)
	}
	items, err := restored.List(created.Session.ID, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("restored list=%+v err=%v", items, err)
	}
	loaded, err := restored.Get(created.Session.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	chunk, next := loaded.Logs(0)
	if chunk != "persistent-log" || next != len(chunk) {
		t.Fatalf("chunk=%q next=%d", chunk, next)
	}
}

func TestTaskSeparatesOutputStreamsAndSupportsStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell assertion is Unix-specific")
	}
	manager := NewTaskManager()
	task, err := manager.StartRemote(context.Background(), "rs1", "project", t.TempDir(), "read value; printf 'out:%s' \"$value\"; printf 'err:%s' \"$value\" >&2")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.WriteStdin("typed-value\n"); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !task.Wait(waitCtx) {
		t.Fatal("task did not exit")
	}
	stdout, stdoutNext := task.LogsFor("stdout", 0)
	stderr, stderrNext := task.LogsFor("stderr", 0)
	if stdout != "out:typed-value" || stderr != "err:typed-value" || stdoutNext != len(stdout) || stderrNext != len(stderr) {
		t.Fatalf("stdout=%q/%d stderr=%q/%d", stdout, stdoutNext, stderr, stderrNext)
	}
}

func TestTaskDrainsLargeOutputWithoutObserverPanic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell assertion is Unix-specific")
	}
	manager := NewTaskManager()
	manager.SetOutputSink(func(OutputChunk) {})
	task, err := manager.StartRemote(context.Background(), "rs_large_output", "project", t.TempDir(), "head -c 1048576 /dev/zero | tr '\\000' x")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !task.Wait(waitCtx) {
		t.Fatal("large-output task did not exit")
	}
	status := task.StatusView()
	if status["status"] != TaskExited || status["exit_code"] != 0 {
		t.Fatalf("large-output status=%+v", status)
	}
	if task.LogStreamSize("stdout") != 1<<20 {
		t.Fatalf("stdout log size=%d, want %d", task.LogStreamSize("stdout"), 1<<20)
	}
}

func TestDirectProcessUsesFiniteStdinAndEOF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("direct process fixture is Unix-specific")
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat unavailable")
	}
	manager := NewTaskManager()
	task, err := manager.StartRemoteProcessWithObservationContext(
		"req_direct", "call_direct", "execute", "rs_direct", "project", t.TempDir(), "cat",
		ProcessSpec{Executable: cat, Stdin: "finite-stdin"},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !task.Wait(waitCtx) {
		t.Fatal("direct process did not receive EOF")
	}
	stdout, _ := task.LogsFor("stdout", 0)
	if stdout != "finite-stdin" || task.StatusView()["status"] != TaskExited {
		t.Fatalf("direct process stdout=%q status=%+v", stdout, task.StatusView())
	}
}

func TestDirectProcessWallLimitTerminatesTask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("direct process fixture is Unix-specific")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	manager := NewTaskManager()
	task, err := manager.StartRemoteProcessWithObservationContext(
		"req_limit", "call_limit", "execute", "rs_limit", "project", t.TempDir(), "sh -c sleep",
		ProcessSpec{Executable: sh, Args: []string{"-c", "sleep 2"}, WallLimit: 30 * time.Millisecond},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !task.Wait(waitCtx) {
		t.Fatal("wall-limited process did not terminate")
	}
	status := task.StatusView()
	if status["limit_reason"] != "wall_time_limit" {
		t.Fatalf("wall-limited task status=%+v", status)
	}
}

func TestParseLsofPorts(t *testing.T) {
	ports := parseLsof("p123\nn*:3000\nn127.0.0.1:8080\nn[::1]:3000\n")
	if len(ports) != 3 || ports[0].Port != 3000 || ports[2].Port != 8080 {
		t.Fatalf("ports=%+v", ports)
	}
}
