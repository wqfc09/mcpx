//go:build !windows

package server

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func shortLifecycleSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcpx-lc-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "control.sock")
}

func TestLifecycleSocketStaleEvidenceIsConservative(t *testing.T) {
	if !lifecycleSocketClearlyStale(syscall.ECONNREFUSED) {
		t.Fatal("ECONNREFUSED must be accepted as explicit stale evidence")
	}
	if !lifecycleSocketClearlyStale(syscall.ENOENT) {
		t.Fatal("ENOENT must be accepted as explicit stale evidence")
	}
	for _, err := range []error{syscall.ETIMEDOUT, syscall.EACCES, errors.New("temporary dial failure")} {
		if lifecycleSocketClearlyStale(err) {
			t.Fatalf("ambiguous dial error incorrectly authorized unlink: %v", err)
		}
	}
}

func TestLifecycleSocketNeverUnlinksActiveListener(t *testing.T) {
	path := shortLifecycleSocketPath(t)
	listener, err := listenLifecycleControlSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := listenLifecycleControlSocket(path); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second listener error=%v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("active lifecycle socket was removed: %v", err)
	}
}

func TestLifecycleSocketRecoversExplicitlyStaleSocket(t *testing.T) {
	path := shortLifecycleSocketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type=%T", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket fixture missing: %v", err)
	}
	recovered, err := listenLifecycleControlSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("recovered lifecycle socket is not live: %v", err)
	}
	_ = conn.Close()
}
