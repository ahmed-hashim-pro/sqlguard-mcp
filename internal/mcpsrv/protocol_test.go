package mcpsrv

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect wires a client to the server over in-memory transports, so these
// tests go through real JSON-RPC and the generated tool schemas rather than
// calling the handlers directly.
func connect(t *testing.T, h *harness) *mcp.ClientSession {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "sqlguard", Version: "test"}, nil)
	h.Register(server)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestAdvertisedTools(t *testing.T) {
	session := connect(t, newHarness(t))

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var got []string
	for _, tool := range result.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)

	want := []string{
		"describe_table", "execute_approved_write", "list_tables",
		"query", "request_write_approval",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// The property the whole design rests on: an agent can ask for approval but has
// no tool that grants it. If someone ever exposes one, this fails.
func TestNoToolCanGrantApproval(t *testing.T) {
	session := connect(t, newHarness(t))

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name == "approve" || strings.HasPrefix(tool.Name, "approve_") {
			t.Errorf("tool %q would let the agent approve its own request", tool.Name)
		}
	}
	// request_write_approval must describe the boundary rather than imply it
	// can grant anything.
	for _, tool := range result.Tools {
		if tool.Name == "request_write_approval" &&
			!strings.Contains(tool.Description, "cannot approve your own") {
			t.Errorf("request_write_approval description does not state the boundary: %q", tool.Description)
		}
	}
}

func TestToolSchemasAreGenerated(t *testing.T) {
	session := connect(t, newHarness(t))

	result, _ := session.ListTools(context.Background(), nil)
	for _, tool := range result.Tools {
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}
}

func TestCallQueryOverTheProtocol(t *testing.T) {
	session := connect(t, newHarness(t))

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"sql": "SELECT id, status FROM orders ORDER BY id"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool reported an error: %+v", res.Content)
	}

	var out QueryOutput
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if out.Decision != "allowed" {
		t.Fatalf("Decision = %q, want allowed", out.Decision)
	}
	if out.RowCount != 3 { // harness caps at 3
		t.Errorf("RowCount = %d, want 3", out.RowCount)
	}
}

func TestRefusalTravelsOverTheProtocol(t *testing.T) {
	session := connect(t, newHarness(t))

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"sql": "DELETE FROM orders"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	var out QueryOutput
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if out.Decision != "refused" {
		t.Errorf("Decision = %q, want refused", out.Decision)
	}
	if !strings.Contains(out.NextStep, "request_write_approval") {
		t.Errorf("NextStep = %q, want it to name the approval tool", out.NextStep)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
