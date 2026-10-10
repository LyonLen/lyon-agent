package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lyon-agent/pkg/tools"
)

// newTestServer wires an in-memory server with two tools (echo + fail) and
// returns a client Connection against it plus a gauge of the maximum
// concurrent handler invocations observed.
func newTestServer(t *testing.T) (*Connection, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "testsrv", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "echo",
		Description: "Echo the text back.",
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, args struct {
		Text string `json:"text" jsonschema:"text to echo"`
	}) (*mcpsdk.CallToolResult, any, error) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		// Hold briefly so overlapping calls would be observable.
		time.Sleep(20 * time.Millisecond)
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + args.Text}},
		}, nil, nil
	})
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "fail",
		Description: "Always fails.",
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, args struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			IsError: true,
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "boom"}},
		}, nil, nil
	})

	srvTransport, cliTransport := mcpsdk.NewInMemoryTransports()
	if _, err := server.Connect(ctx, srvTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	conn, err := ConnectTransport(ctx, "test-srv", cliTransport)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, &maxInFlight
}

func findTool(t *testing.T, list []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tool := range list {
		if tool.Name() == name {
			return tool
		}
	}
	names := make([]string, 0, len(list))
	for _, tool := range list {
		names = append(names, tool.Name())
	}
	t.Fatalf("tool %q not found in %v", name, names)
	return nil
}

func TestListAndCallTools(t *testing.T) {
	conn, _ := newTestServer(t)
	ctx := context.Background()

	list, err := conn.Tools(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d tools, want 2", len(list))
	}

	// Deterministic prefixed names keep the prompt cache prefix stable.
	echoTool := findTool(t, list, "mcp_test_srv_echo")
	findTool(t, list, "mcp_test_srv_fail")

	props, ok := echoTool.InputSchema().Properties.(map[string]any)
	if !ok || len(props) != 1 {
		t.Fatalf("echo schema properties = %#v, want one 'text' property", echoTool.InputSchema().Properties)
	}
	if !strings.Contains(strings.ToLower(echoTool.Description()), "echo") {
		t.Errorf("description not forwarded: %q", echoTool.Description())
	}
	if !echoTool.ParallelSafe() {
		t.Error("MCP adapters must be parallel-safe (serialization happens per connection)")
	}

	out, err := echoTool.Execute(ctx, []byte(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("echo execute: %v", err)
	}
	if len(out) != 1 || out[0].OfText == nil || out[0].OfText.Text != "echo: hi" {
		t.Fatalf("echo result: %+v", out)
	}
}

func TestToolErrorBecomesGoError(t *testing.T) {
	conn, _ := newTestServer(t)
	list, err := conn.Tools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	failTool := findTool(t, list, "mcp_test_srv_fail")
	if _, err = failTool.Execute(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("isError result must surface as a Go error")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should carry server text, got: %v", err)
	}
}

func TestInvalidArgumentsRejected(t *testing.T) {
	conn, _ := newTestServer(t)
	if _, err := conn.Call(context.Background(), "echo", []byte(`{not json`)); err == nil {
		t.Fatal("malformed JSON must be rejected locally")
	}
}

func TestPerConnectionSerialization(t *testing.T) {
	conn, maxInFlight := newTestServer(t)
	list, err := conn.Tools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	echoTool := findTool(t, list, "mcp_test_srv_echo")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := echoTool.Execute(context.Background(), []byte(fmt.Sprintf(`{"text":"t%d"}`, i)))
			if err != nil {
				t.Errorf("exec %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if got := maxInFlight.Load(); got > 1 {
		t.Fatalf("max concurrent handler invocations = %d, want 1 (per-connection mutex)", got)
	}
}

func TestManagerToolsAndClose(t *testing.T) {
	conn, _ := newTestServer(t)
	m := NewManager()
	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()

	if got := m.Names(); len(got) != 1 || got[0] != "test-srv" {
		t.Fatalf("names: %v", got)
	}
	list, err := m.Tools(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("manager tools: err=%v n=%d", err, len(list))
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(m.Names()) != 0 {
		t.Fatal("close must clear the manager")
	}
	// Double close is safe.
	if err := m.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := Connect(context.Background(), Config{Name: "x"}); err == nil {
		t.Error("missing transport must error")
	}
	if _, err := Connect(context.Background(), Config{Command: []string{"echo"}}); err == nil {
		t.Error("missing name must error")
	}
	if _, err := Connect(context.Background(), Config{
		Name: "x", Command: []string{"echo"}, URL: "http://localhost",
	}); err == nil {
		t.Error("command+url must be mutually exclusive")
	}
}
