package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/workspace"
)

type attachOptions struct {
	Name string
	Path string
	JSON bool
}

type ensuredInstance struct {
	State   runtimeinstance.State
	Mode    string
	LogPath string
}

func parseAttachArgs(args []string) (attachOptions, error) {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "logical Workspace name; defaults to directory basename")
	jsonOutput := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return attachOptions{}, err
	}
	if fs.NArg() > 1 {
		return attachOptions{}, fmt.Errorf("attach accepts at most one Workspace path")
	}
	path := ""
	if fs.NArg() == 1 {
		path = strings.TrimSpace(fs.Arg(0))
	}
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return attachOptions{}, err
		}
		path = cwd
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return attachOptions{}, err
	}
	return attachOptions{Name: strings.TrimSpace(*name), Path: filepath.Clean(abs), JSON: *jsonOutput}, nil
}

func resolveDefaultInstance() (runtimeinstance.State, bool, error) {
	state, err := runtimeinstance.ResolveRunning()
	if err == nil {
		return state, true, nil
	}
	if errors.Is(err, runtimeinstance.ErrNotFound) {
		return runtimeinstance.State{}, false, nil
	}
	if errors.Is(err, runtimeinstance.ErrStale) {
		if stale, readErr := runtimeinstance.Read(); readErr == nil {
			if removeErr := runtimeinstance.RemoveIfOwned(stale.InstanceID); removeErr != nil {
				return runtimeinstance.State{}, false, removeErr
			}
		}
		return runtimeinstance.State{}, false, nil
	}
	return runtimeinstance.State{}, false, err
}

func ensureDefaultInstance() (ensuredInstance, error) {
	if state, running, err := resolveDefaultInstance(); err != nil {
		return ensuredInstance{}, err
	} else if running {
		return ensuredInstance{State: state, Mode: "reused"}, nil
	}
	lock, err := runtimeinstance.AcquireStartLock(20 * time.Second)
	if err != nil {
		return ensuredInstance{}, err
	}
	defer lock.Release()
	// Another caller may have started the Instance while this process was
	// waiting on the shared start lock.
	if state, running, resolveErr := resolveDefaultInstance(); resolveErr != nil {
		return ensuredInstance{}, resolveErr
	} else if running {
		return ensuredInstance{State: state, Mode: "reused"}, nil
	}

	pid, logPath, _, err := startBackground(defaultInstanceServeArgs())
	if err != nil {
		return ensuredInstance{}, err
	}
	return waitForBackgroundInstance(pid, logPath, "started")
}

func restartDefaultInstance() (ensuredInstance, []int, error) {
	lock, err := runtimeinstance.AcquireStartLock(20 * time.Second)
	if err != nil {
		return ensuredInstance{}, nil, err
	}
	defer lock.Release()

	state, running, err := resolveDefaultInstance()
	if err != nil {
		return ensuredInstance{}, nil, err
	}
	if running {
		statePath := filepath.Join(state.Home, daemonStateFilename)
		daemon, readErr := readDaemonState(statePath)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return ensuredInstance{}, nil, fmt.Errorf("default Instance pid %d is not a managed background daemon; restart refuses to signal it", state.PID)
			}
			return ensuredInstance{}, nil, fmt.Errorf("read managed daemon state: %w", readErr)
		}
		if daemon.PID != state.PID || strings.TrimSpace(daemon.Executable) == "" {
			return ensuredInstance{}, nil, fmt.Errorf("managed daemon state does not match running Instance pid %d", state.PID)
		}
		stopped, stopErr := terminateBackgroundProcess(daemon.PID, daemon.Executable, 5*time.Second)
		if stopErr != nil {
			return ensuredInstance{}, nil, fmt.Errorf("stop managed MCPX Instance: %w", stopErr)
		}
		if !stopped {
			return ensuredInstance{}, nil, fmt.Errorf("managed MCPX Instance pid %d exited before restart", daemon.PID)
		}
		if removeErr := os.Remove(statePath); removeErr != nil && !os.IsNotExist(removeErr) {
			return ensuredInstance{}, nil, fmt.Errorf("remove previous daemon state: %w", removeErr)
		}
	}

	pid, logPath, stoppedPIDs, err := startBackground(defaultInstanceServeArgs())
	if err != nil {
		return ensuredInstance{}, nil, err
	}
	if running {
		stoppedPIDs = append([]int{state.PID}, stoppedPIDs...)
	}
	ensured, err := waitForBackgroundInstance(pid, logPath, map[bool]string{true: "restarted", false: "started"}[running])
	return ensured, stoppedPIDs, err
}

func waitForBackgroundInstance(pid int, logPath, mode string) (ensuredInstance, error) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state, running, resolveErr := resolveDefaultInstance()
		if resolveErr != nil {
			return ensuredInstance{}, resolveErr
		}
		if running && state.PID == pid {
			return ensuredInstance{State: state, Mode: mode, LogPath: logPath}, nil
		}
		alive, aliveErr := backgroundProcessAlive(pid)
		if aliveErr != nil {
			return ensuredInstance{}, aliveErr
		}
		if !alive {
			return ensuredInstance{}, fmt.Errorf("MCPX background process %d exited before publishing Instance state; log=%s", pid, logPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ensuredInstance{}, fmt.Errorf("MCPX background process %d did not publish Instance state; log=%s", pid, logPath)
}

func defaultInstanceServeArgs() []string {
	if addr := strings.TrimSpace(os.Getenv("MCPX_ADDR")); addr != "" {
		return []string{"-addr", addr}
	}
	if port := strings.TrimSpace(os.Getenv("MCPX_PORT")); port != "" {
		return []string{"-addr", "127.0.0.1:" + port}
	}
	return nil
}

func runEnsure(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "ensure: no arguments are accepted")
		return 2
	}
	ensured, err := ensureDefaultInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ensure: %v\n", err)
		return 1
	}
	printInstanceSummary(os.Stdout, ensured)
	return 0
}

func runRestart(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOutput := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "restart: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "restart: no positional arguments are accepted")
		return 2
	}
	ensured, stoppedPIDs, err := restartDefaultInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "restart: %v\n", err)
		return 1
	}
	if *jsonOutput {
		return writeJSON(os.Stdout, map[string]any{
			"instance": map[string]any{
				"mode": ensured.Mode, "id": ensured.State.InstanceID, "pid": ensured.State.PID,
				"endpoint": ensured.State.Endpoint, "home": ensured.State.Home, "control_socket": ensured.State.ControlSocket,
				"build": ensured.State.Build, "commit": ensured.State.Commit, "started_at": ensured.State.StartedAt,
			},
			"stopped_pids": stoppedPIDs,
		})
	}
	fmt.Print(backgroundStopMessage(stoppedPIDs))
	printInstanceSummary(os.Stdout, ensured)
	return 0
}

func runAttach(args []string) int {
	options, err := parseAttachArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach: %v\n", err)
		return 2
	}
	info, err := os.Stat(options.Path)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "attach: Workspace does not exist or is not a directory: %s\n", options.Path)
		return 1
	}

	// Register before first startup so the default Instance knows this Workspace.
	// Plugin schemas are resolved dynamically only when an active Plugin is used;
	// attach itself never probes or starts Plugin processes.
	registry, err := openWorkspaceRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach: open Workspace registry: %v\n", err)
		return 1
	}
	registered, err := registry.Register(options.Name, options.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach: register Workspace: %v\n", err)
		return 1
	}

	ensured, err := ensureDefaultInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach: ensure MCPX Instance: %v\n", err)
		return 1
	}
	// Cover a concurrent first-start race: the winning Instance may use a
	// different Home than the shell that performed the initial offline write.
	instanceRegistry, err := workspace.NewRegistry(filepath.Join(ensured.State.Home, "config.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach: open running Instance registry: %v\n", err)
		return 1
	}
	registered, err = instanceRegistry.Register(options.Name, options.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach: register Workspace in running Instance: %v\n", err)
		return 1
	}

	if options.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"instance": map[string]any{
				"mode": ensured.Mode, "id": ensured.State.InstanceID, "pid": ensured.State.PID,
				"endpoint": ensured.State.Endpoint, "home": ensured.State.Home, "control_socket": ensured.State.ControlSocket,
				"build": ensured.State.Build, "commit": ensured.State.Commit, "started_at": ensured.State.StartedAt,
			},
			"workspace": map[string]any{"name": registered.Name, "id": registered.ID, "path": registered.Path},
		})
	}
	printInstanceSummary(os.Stdout, ensured)
	fmt.Fprintf(os.Stdout, "Workspace       : %s\n", registered.Path)
	fmt.Fprintf(os.Stdout, "Workspace name  : %s\n", registered.Name)
	fmt.Fprintf(os.Stdout, "Workspace ID    : %s\n", registered.ID)
	return 0
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOutput := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "status: no positional arguments are accepted")
		return 2
	}
	state, running, err := resolveDefaultInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		return 1
	}
	if !running {
		if *jsonOutput {
			return writeJSON(os.Stdout, map[string]any{"running": false})
		}
		fmt.Fprintln(os.Stdout, "MCPX Instance   : not running")
		return 0
	}
	workspaces := 0
	registry, registryErr := workspace.NewRegistry(filepath.Join(state.Home, "config.yaml"))
	if registryErr == nil {
		if items, listErr := registry.ListChecked(); listErr == nil {
			workspaces = len(items)
		}
	}
	if *jsonOutput {
		return writeJSON(os.Stdout, map[string]any{
			"running": true, "instance_id": state.InstanceID, "pid": state.PID,
			"endpoint": state.Endpoint, "home": state.Home, "control_socket": state.ControlSocket, "build": state.Build,
			"commit": state.Commit, "started_at": state.StartedAt, "workspaces": workspaces,
		})
	}
	fmt.Fprintln(os.Stdout, "MCPX Instance   : running")
	fmt.Fprintf(os.Stdout, "Instance ID     : %s\n", state.InstanceID)
	fmt.Fprintf(os.Stdout, "PID             : %d\n", state.PID)
	fmt.Fprintf(os.Stdout, "Endpoint        : %s\n", state.Endpoint)
	fmt.Fprintf(os.Stdout, "Home            : %s\n", state.Home)
	fmt.Fprintf(os.Stdout, "Build           : %s\n", state.Build)
	fmt.Fprintf(os.Stdout, "Commit          : %s\n", state.Commit)
	fmt.Fprintf(os.Stdout, "Workspaces      : %d\n", workspaces)
	return 0
}

func printInstanceSummary(w io.Writer, ensured ensuredInstance) {
	fmt.Fprintf(w, "MCPX Instance   : %s\n", ensured.Mode)
	fmt.Fprintf(w, "Instance ID     : %s\n", ensured.State.InstanceID)
	fmt.Fprintf(w, "PID             : %d\n", ensured.State.PID)
	fmt.Fprintf(w, "Endpoint        : %s\n", ensured.State.Endpoint)
	fmt.Fprintf(w, "Home            : %s\n", ensured.State.Home)
}
