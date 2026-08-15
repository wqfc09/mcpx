package server

import (
	"fmt"
	"net"
	"os"
	"strings"

	runtimeinstance "mcpx/internal/instance"
)

func (r *Runtime) publishInstance(listener net.Listener) error {
	if r == nil || listener == nil {
		return fmt.Errorf("Runtime listener is required to publish MCPX instance")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve MCPX executable: %w", err)
	}
	addr := listener.Addr().String()
	state := runtimeinstance.State{
		Version: runtimeinstance.StateVersion, InstanceID: r.instanceID, PID: os.Getpid(),
		Executable: executable, Home: r.homeDir, Addr: addr, Endpoint: localMCPEndpoint(addr),
		Build: r.build.Version, Commit: r.build.Commit, StartedAt: runtimeinstance.StartedAtNow(),
	}
	if err := runtimeinstance.Write(state); err != nil {
		return fmt.Errorf("publish MCPX instance: %w", err)
	}
	return nil
}

func localMCPEndpoint(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "http://" + strings.TrimSpace(addr) + "/mcp"
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/mcp"
}
