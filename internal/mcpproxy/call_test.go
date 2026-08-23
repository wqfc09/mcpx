package mcpproxy

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewCallToolParamsCarriesClientTimestampAndProgressToken(t *testing.T) {
	started := time.UnixMilli(1786365000123)
	params := newCallToolParams("demo", map[string]any{"value": "ok"}, mcp.Meta{"provider": "caller", clientStartedAtMetaKey: int64(1)}, "progress-1", started)
	if got := params.Meta[clientStartedAtMetaKey]; got != started.UnixMilli() {
		t.Fatalf("client timestamp = %v, want %d", got, started.UnixMilli())
	}
	if got := params.Meta["provider"]; got != "caller" {
		t.Fatalf("caller metadata was not preserved: %v", got)
	}
	if got := params.GetProgressToken(); got != "progress-1" {
		t.Fatalf("progress token = %v", got)
	}
}

func TestMultiplexedClientSessionAllowsConcurrentToolCalls(t *testing.T) {
	protocol := mcp.NewServer(&mcp.Implementation{Name: "concurrent-server", Version: "0.1.0"}, nil)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	protocol.AddTool(&mcp.Tool{Name: "wait", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	clientProtocol := mcp.NewClient(&mcp.Implementation{Name: "concurrent-client", Version: "0.1.0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	serverSession, err := protocol.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	upstream, err := clientProtocol.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	client := &ClientSession{session: upstream, multiplexed: true}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, callErr := client.CallTool(ctx, "wait", nil, nil)
			errs <- callErr
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(300 * time.Millisecond):
			close(release)
			t.Fatal("multiplexed ClientSession serialized concurrent tool calls")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		if callErr := <-errs; callErr != nil {
			t.Fatal(callErr)
		}
	}
}

func TestClientProgressHeartbeatEmitsSyntheticUpdate(t *testing.T) {
	old := clientProgressHeartbeatInterval
	clientProgressHeartbeatInterval = 5 * time.Millisecond
	defer func() { clientProgressHeartbeatInterval = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan ToolProgress, 1)
	stop := startClientProgressHeartbeat(ctx, "slow_tool", time.Now(), make(chan struct{}), func(update ToolProgress) {
		updates <- update
	})
	defer stop()

	select {
	case update := <-updates:
		if !update.Synthetic || update.Message != "slow_tool is still running" || update.Elapsed <= 0 {
			t.Fatalf("unexpected heartbeat: %+v", update)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("client progress heartbeat did not fire")
	}
}

func TestClientProgressHeartbeatResetsAfterNativeProgress(t *testing.T) {
	old := clientProgressHeartbeatInterval
	clientProgressHeartbeatInterval = 20 * time.Millisecond
	defer func() { clientProgressHeartbeatInterval = old }()

	reset := make(chan struct{}, 1)
	updates := make(chan ToolProgress, 1)
	stop := startClientProgressHeartbeat(context.Background(), "slow_tool", time.Now(), reset, func(update ToolProgress) {
		updates <- update
	})
	defer stop()

	time.Sleep(10 * time.Millisecond)
	reset <- struct{}{}
	select {
	case update := <-updates:
		t.Fatalf("heartbeat fired before reset interval elapsed: %+v", update)
	case <-time.After(15 * time.Millisecond):
	}
	select {
	case <-updates:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("heartbeat did not fire after reset interval")
	}
}
