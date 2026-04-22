package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dialMemoryMCP wires an in-memory client to a server backed by store, returns
// the connected client session. Caller defers session.Close().
func dialMemoryMCP(t *testing.T, store *Store) *mcp.ClientSession {
	t.Helper()
	clientT, serverT := mcp.NewInMemoryTransports()
	server := buildMemoryMCPServer(store)
	if _, err := server.Connect(t.Context(), serverT, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	sess, err := client.Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	return sess
}

func callTool(t *testing.T, sess *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tool %s returned error: %+v", name, res.Content)
	}
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

func TestMemoryMCP_ListsFourTools(t *testing.T) {
	store := newStore(t, 10)
	sess := dialMemoryMCP(t, store)
	defer sess.Close()

	res, err := sess.ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	want := []string{
		"memory_summary_view",
		"memory_summary_write",
		"memory_log_list",
		"memory_log_write",
	}
	if len(got) != len(want) {
		t.Errorf("expected %d tools, got %d (%v)", len(want), len(got), got)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing tool: %s", w)
		}
	}
}

func TestMemoryMCP_SummaryRoundtrip(t *testing.T) {
	store := newStore(t, 10)
	sess := dialMemoryMCP(t, store)
	defer sess.Close()

	if got := callTool(t, sess, "memory_summary_view", emptyArgs{}); got != "" {
		t.Errorf("expected empty summary, got %q", got)
	}
	callTool(t, sess, "memory_summary_write", summaryWriteArgs{Content: "the digest"})
	if got := callTool(t, sess, "memory_summary_view", emptyArgs{}); got != "the digest" {
		t.Errorf("got %q, want %q", got, "the digest")
	}
}

func TestMemoryMCP_LogWriteAndList(t *testing.T) {
	store := newStore(t, 10)
	sess := dialMemoryMCP(t, store)
	defer sess.Close()

	for _, c := range []string{"one", "two", "three"} {
		callTool(t, sess, "memory_log_write", logWriteArgs{Content: c})
	}

	body := callTool(t, sess, "memory_log_list", logListArgs{})
	var recs []Record
	if err := json.Unmarshal([]byte(body), &recs); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}
	// Newest-first.
	want := []string{"three", "two", "one"}
	for i, w := range want {
		if recs[i].Content != w {
			t.Errorf("recs[%d] = %q, want %q", i, recs[i].Content, w)
		}
	}

	// Limit + offset.
	body = callTool(t, sess, "memory_log_list", logListArgs{Limit: 1, Offset: 1})
	recs = nil
	if err := json.Unmarshal([]byte(body), &recs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(recs) != 1 || recs[0].Content != "two" {
		t.Fatalf("limit/offset: got %v, want one record %q", recs, "two")
	}
}

func TestMemoryMCP_LogWriteRespectsCap(t *testing.T) {
	store := newStore(t, 2)
	sess := dialMemoryMCP(t, store)
	defer sess.Close()

	for _, c := range []string{"a", "b", "c", "d"} {
		callTool(t, sess, "memory_log_write", logWriteArgs{Content: c})
	}

	body := callTool(t, sess, "memory_log_list", logListArgs{})
	var recs []Record
	if err := json.Unmarshal([]byte(body), &recs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records (cap), got %d", len(recs))
	}
	if recs[0].Content != "d" || recs[1].Content != "c" {
		t.Errorf("expected [d, c], got %v", recs)
	}
}

func TestMemoryMCP_SummaryWriteEmptyDeletes(t *testing.T) {
	store := newStore(t, 10)
	sess := dialMemoryMCP(t, store)
	defer sess.Close()

	callTool(t, sess, "memory_summary_write", summaryWriteArgs{Content: "to be deleted"})
	callTool(t, sess, "memory_summary_write", summaryWriteArgs{Content: ""})
	if got := callTool(t, sess, "memory_summary_view", emptyArgs{}); got != "" {
		t.Errorf("expected empty after delete, got %q", got)
	}
}

func TestMemoryMCP_LogWriteReturnsRecord(t *testing.T) {
	store := newStore(t, 10)
	sess := dialMemoryMCP(t, store)
	defer sess.Close()

	body := callTool(t, sess, "memory_log_write", logWriteArgs{Content: "hello"})
	var rec Record
	if err := json.Unmarshal([]byte(body), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if rec.Content != "hello" || rec.ID == "" || rec.CreatedAt.IsZero() {
		t.Fatalf("bad record: %+v", rec)
	}
	// IDs are hex.
	if strings.TrimLeft(rec.ID, "0123456789abcdef") != "" {
		t.Errorf("ID not hex: %q", rec.ID)
	}
}
