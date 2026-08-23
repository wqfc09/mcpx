package mcpproxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/logging"
	buildversion "mcpx/internal/version"
)

const (
	mcpConnectTimeout      = 30 * time.Second
	mcpListTimeout         = 30 * time.Second
	mcpCallTimeout         = 2 * time.Minute
	clientStartedAtMetaKey = "mcpx/started_at_ms"
)

var (
	clientProgressHeartbeatInterval = 20 * time.Second
	nextProgressToken               atomic.Uint64
)

// ToolProgress is a client-side progress update for one upstream tools/call.
// Synthetic updates are emitted by the MCPX client watchdog when the upstream
// server has not sent a progress notification within the heartbeat interval.
type ToolProgress struct {
	Message   string
	Progress  float64
	Total     float64
	Elapsed   time.Duration
	Synthetic bool
}

// ProgressHandler receives progress for an upstream tools/call.
type ProgressHandler func(ToolProgress)

// ClientSession owns one upstream stdio MCP process. ListTools and CallTool on
// the same value observe and execute against the same upstream instance.
type ClientSession struct {
	srv               config.MCPServer
	session           *mcp.ClientSession
	cancel            context.CancelFunc
	processPID        int
	processExecutable string
	processArgv0      string
	onProgress        ProgressHandler
	multiplexed       bool
	opMu              sync.Mutex
	callbackMu        sync.Mutex
	progressMu        sync.Mutex
	progressToken     string
	progressReset     chan struct{}
	callStarted       time.Time
	closed            chan struct{}
}

// OpenClientSession connects one upstream MCP process using the ordinary MCP
// execution model: operations on the session are serialized so one session-level
// progress callback can safely represent the active call.
func OpenClientSession(ctx context.Context, srv config.MCPServer, onProgress ProgressHandler) (*ClientSession, error) {
	return openClientSession(ctx, srv, onProgress, false)
}

// OpenMultiplexedClientSession connects one long-lived MCP process whose JSON-RPC
// requests may be concurrently in flight. MCPX uses this for Package V2 MCP
// Plugin runtimes, where Inbox waits, dependency watches and business Tool calls
// must not block each other on one shared stdio connection.
func OpenMultiplexedClientSession(ctx context.Context, srv config.MCPServer) (*ClientSession, error) {
	return openClientSession(ctx, srv, nil, true)
}

func openClientSession(ctx context.Context, srv config.MCPServer, onProgress ProgressHandler, multiplexed bool) (*ClientSession, error) {
	client := &ClientSession{srv: srv, onProgress: onProgress, multiplexed: multiplexed}
	var options *mcp.ClientOptions
	if onProgress != nil {
		options = &mcp.ClientOptions{ProgressNotificationHandler: client.handleProgress}
	}
	session, cancel, processPID, processExecutable, err := connect(ctx, srv, mcpConnectTimeout, options)
	if err != nil {
		return nil, err
	}
	client.session = session
	client.cancel = cancel
	client.processPID = processPID
	client.processExecutable = processExecutable
	client.processArgv0 = strings.TrimSpace(srv.Command)
	client.closed = make(chan struct{})
	go func() {
		_ = session.Wait()
		close(client.closed)
	}()
	return client, nil
}

// ProcessID returns the PID of the stdio MCP process owned by this ClientSession.
func (c *ClientSession) ProcessID() int {
	if c == nil {
		return 0
	}
	return c.processPID
}

// ProcessExecutable returns the executable path resolved by exec.Command for
// the stdio MCP process owned by this ClientSession.
func (c *ClientSession) ProcessExecutable() string {
	if c == nil {
		return ""
	}
	return c.processExecutable
}

// ProcessArgv0 returns argv[0] used for the owned stdio MCP process.
func (c *ClientSession) ProcessArgv0() string {
	if c == nil {
		return ""
	}
	return c.processArgv0
}

// IsClosed reports whether the upstream MCP connection has actually closed.
// Caller cancellation does not affect this signal after initialize succeeds.
func (c *ClientSession) IsClosed() bool {
	if c == nil || c.session == nil {
		return true
	}
	if c.closed == nil {
		return false
	}
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *ClientSession) handleProgress(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	c.progressMu.Lock()
	token := c.progressToken
	reset := c.progressReset
	started := c.callStarted
	c.progressMu.Unlock()
	if token == "" || fmt.Sprint(req.Params.ProgressToken) != token {
		return
	}
	c.deliver(ToolProgress{
		Message: req.Params.Message, Progress: req.Params.Progress, Total: req.Params.Total,
		Elapsed: time.Since(started),
	})
	if reset != nil {
		select {
		case reset <- struct{}{}:
		default:
		}
	}
}

func (c *ClientSession) deliver(update ToolProgress) {
	if c == nil || c.onProgress == nil {
		return
	}
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.onProgress(update)
}

// Close terminates the owned upstream MCP process.
func (c *ClientSession) Close() {
	if c == nil {
		return
	}
	if c.session != nil {
		_ = c.session.Close()
	}
	if c.cancel != nil {
		c.cancel()
	}
}

// ListTools reads tools/list on this exact upstream instance.
func (c *ClientSession) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("upstream mcp session is not connected")
	}
	if !c.multiplexed {
		c.opMu.Lock()
		defer c.opMu.Unlock()
	}
	listCtx, cancel := context.WithTimeout(ctx, mcpListTimeout)
	defer cancel()
	listed, err := c.session.ListTools(listCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	if listed == nil {
		return nil, nil
	}
	return listed.Tools, nil
}

// Instructions returns initialize.instructions captured from this exact
// upstream connection.
func (c *ClientSession) Instructions() string {
	if c == nil || c.session == nil || c.session.InitializeResult() == nil {
		return ""
	}
	return c.session.InitializeResult().Instructions
}

// CallTool calls a tool on this exact upstream instance.
func (c *ClientSession) CallTool(ctx context.Context, toolName string, arguments map[string]any, meta mcp.Meta) (*mcp.CallToolResult, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("upstream mcp session is not connected")
	}
	if !c.multiplexed {
		c.opMu.Lock()
		defer c.opMu.Unlock()
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	callCtx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	started := time.Now()
	progressToken := ""
	stopHeartbeat := func() {}
	if c.onProgress != nil {
		progressToken = fmt.Sprintf("mcpx-progress-%d", nextProgressToken.Add(1))
		progressReset := make(chan struct{}, 1)
		c.progressMu.Lock()
		c.progressToken = progressToken
		c.progressReset = progressReset
		c.callStarted = started
		c.progressMu.Unlock()
		defer func() {
			c.progressMu.Lock()
			c.progressToken = ""
			c.progressReset = nil
			c.progressMu.Unlock()
		}()
		stopHeartbeat = startClientProgressHeartbeat(callCtx, toolName, started, progressReset, c.deliver)
	}
	params := newCallToolParams(toolName, arguments, meta, progressToken, started)
	defer stopHeartbeat()
	res, err := c.session.CallTool(callCtx, params)
	if err != nil {
		return nil, err
	}
	logging.Debug("mcp call ok", "tool", toolName, "cmd", DescribeCommand(c.srv))
	return res, nil
}

// CallTool starts one stdio MCP client, calls tool, and closes the session.
func CallTool(ctx context.Context, srv config.MCPServer, toolName string, arguments map[string]any, meta mcp.Meta) (*mcp.CallToolResult, error) {
	return CallToolWithProgress(ctx, srv, toolName, arguments, meta, nil)
}

// CallToolWithProgress is the one-shot convenience wrapper around ClientSession.
func CallToolWithProgress(ctx context.Context, srv config.MCPServer, toolName string, arguments map[string]any, meta mcp.Meta, onProgress ProgressHandler) (*mcp.CallToolResult, error) {
	client, err := OpenClientSession(ctx, srv, onProgress)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.CallTool(ctx, toolName, arguments, meta)
}

func newCallToolParams(toolName string, arguments map[string]any, meta mcp.Meta, progressToken string, started time.Time) *mcp.CallToolParams {
	requestMeta := make(mcp.Meta, len(meta)+1)
	for key, value := range meta {
		requestMeta[key] = value
	}
	// The client owns the actual call start time. A caller-provided value must
	// never override it, even when the rest of the metadata is forwarded.
	requestMeta[clientStartedAtMetaKey] = started.UnixMilli()
	params := &mcp.CallToolParams{
		Meta:      requestMeta,
		Name:      toolName,
		Arguments: arguments,
	}
	if progressToken != "" {
		params.SetProgressToken(progressToken)
	}
	return params
}

func startClientProgressHeartbeat(ctx context.Context, toolName string, started time.Time, reset <-chan struct{}, deliver ProgressHandler) func() {
	if deliver == nil || clientProgressHeartbeatInterval <= 0 {
		return func() {}
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(clientProgressHeartbeatInterval)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				// Native progress may have arrived before the timer deadline while
				// this goroutine was not scheduled. Prefer that real progress over a
				// synthetic heartbeat when both signals are ready.
				select {
				case <-reset:
					timer.Reset(clientProgressHeartbeatInterval)
					continue
				default:
				}
				deliver(ToolProgress{
					Message:   fmt.Sprintf("%s is still running", toolName),
					Progress:  time.Since(started).Seconds(),
					Elapsed:   time.Since(started),
					Synthetic: true,
				})
				timer.Reset(clientProgressHeartbeatInterval)
			case <-reset:
				resetProgressTimer(timer, clientProgressHeartbeatInterval)
			case <-heartbeatCtx.Done():
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func resetProgressTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

// ListTools starts one upstream stdio server and returns its tools/list items.
func ListTools(ctx context.Context, srv config.MCPServer) ([]*mcp.Tool, error) {
	client, err := OpenClientSession(ctx, srv, nil)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.ListTools(ctx)
}

// InitializeInstructions starts one upstream server and returns the
// initialize.instructions value from its handshake.
func InitializeInstructions(ctx context.Context, srv config.MCPServer) (string, error) {
	client, err := OpenClientSession(ctx, srv, nil)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return client.Instructions(), nil
}

func connect(ctx context.Context, srv config.MCPServer, timeout time.Duration, options *mcp.ClientOptions) (*mcp.ClientSession, context.CancelFunc, int, string, error) {
	if strings.TrimSpace(srv.Command) == "" {
		return nil, func() {}, 0, "", fmt.Errorf("empty command")
	}

	// The process context must outlive the initialize handshake so Plugin
	// Runtime leases can keep one stdio MCP process for many calls. A timer and
	// the caller context guard only the connect phase; successful connections
	// remain alive until ClientSession.Close.
	processCtx, cancel := context.WithCancel(context.Background())
	stopParent := context.AfterFunc(ctx, cancel)
	timer := time.AfterFunc(timeout, cancel)

	command := ExpandValue(srv.Command, srv.RuntimeEnv)
	args := make([]string, len(srv.Args))
	for i, arg := range srv.Args {
		args[i] = ExpandValue(arg, srv.RuntimeEnv)
	}
	cmd := exec.CommandContext(processCtx, command, args...)
	if strings.TrimSpace(srv.WorkDir) != "" {
		cmd.Dir = srv.WorkDir
	}
	registrationEnv := srv.Env
	if len(srv.RuntimeEnv) > 0 {
		registrationEnv = make(map[string]string, len(srv.Env))
		for key, value := range srv.Env {
			if strings.HasPrefix(key, "MCPX_") {
				continue
			}
			registrationEnv[key] = value
		}
	}
	cmd.Env = append(os.Environ(), ExpandEnvWith(registrationEnv, srv.RuntimeEnv)...)
	// MCPX_* is a Runtime-owned namespace for managed Plugin launches.
	cmd.Env = append(cmd.Env, ExpandEnvWith(srv.RuntimeEnv, srv.RuntimeEnv)...)
	client := mcp.NewClient(&mcp.Implementation{Name: "mcpx", Version: buildversion.Current}, options)
	session, err := client.Connect(processCtx, &mcp.CommandTransport{Command: cmd}, nil)
	timer.Stop()
	stopParent()
	if err != nil {
		cancel()
		return nil, func() {}, 0, "", fmt.Errorf("connect upstream mcp: %w", err)
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	return session, cancel, pid, cmd.Path, nil
}
