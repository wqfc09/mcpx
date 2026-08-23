//go:build windows

package server

import "net"

// Launcher V1 is Unix-only. Keep MCPX buildable on Windows without arming idle
// shutdown until a named-pipe lifecycle transport is implemented.
func listenLifecycleControlSocket(string) (net.Listener, error) { return nil, nil }
func cleanupLifecycleControlSocket(string) error                { return nil }
