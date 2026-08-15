package instance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartLockSerializesCallersAcrossSameRuntimeDir(t *testing.T) {
	t.Setenv("MCPX_RUNTIME_DIR", t.TempDir())
	first, err := AcquireStartLock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		second, acquireErr := AcquireStartLock(time.Second)
		if acquireErr == nil {
			acquireErr = second.Release()
		}
		done <- acquireErr
	}()
	time.Sleep(100 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Instance start lock did not acquire after first release")
	}
}

func TestStateRoundTripUsesRuntimeDirNotMCPXHome(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("MCPX_RUNTIME_DIR", runtimeDir)
	t.Setenv("MCPX_HOME", filepath.Join(t.TempDir(), "other-home"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := State{
		Version: StateVersion, InstanceID: "mcpx_test", PID: os.Getpid(), Executable: executable,
		Home: filepath.Join(t.TempDir(), "runtime-home"), Addr: "127.0.0.1:9090", Endpoint: "http://127.0.0.1:9090/mcp", StartedAt: StartedAtNow(),
	}
	if err := Write(state); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRunning()
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != state.InstanceID || got.Home != state.Home {
		t.Fatalf("instance state=%+v", got)
	}
	path, _ := StatePath()
	if filepath.Dir(path) != runtimeDir {
		t.Fatalf("state path=%q, want runtime dir %q", path, runtimeDir)
	}
	if err := RemoveIfOwned("different"); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(); err != nil {
		t.Fatalf("foreign owner removed state: %v", err)
	}
	if err := RemoveIfOwned(state.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owned state not removed: %v", err)
	}
}
