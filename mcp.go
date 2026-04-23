package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP server name. Tools are exposed to claude as
// `mcp__ccron__<tool>`.
const mcpServerName = "ccron"

// errMemoryNotEnabled is returned by memory handlers when the server was
// constructed without a store. Defence in depth — allowed_tools is the real
// gate, but the handler shouldn't panic if it leaks.
var errMemoryNotEnabled = errors.New("memory not enabled for this job")

// runMCPToolNames is the always-on tool subset. Auto-injected into
// --allowedTools on every run.
var runMCPToolNames = []string{
	"mcp__ccron__run_summary_write",
}

// memoryMCPToolNames lists the memory tools. Auto-injected into
// --allowedTools only when memory is enabled on a job.
var memoryMCPToolNames = []string{
	"mcp__ccron__memory_summary_view",
	"mcp__ccron__memory_summary_write",
	"mcp__ccron__memory_log_list",
	"mcp__ccron__memory_log_write",
}

type emptyArgs struct{}

type summaryWriteArgs struct {
	Content string `json:"content" mcp:"summary content; passing empty string deletes the summary"`
}

type logListArgs struct {
	Limit  int `json:"limit,omitempty" mcp:"max records to return; 0 or omitted returns all"`
	Offset int `json:"offset,omitempty" mcp:"skip this many records from the newest end"`
}

type logWriteArgs struct {
	Content string `json:"content" mcp:"the record content to append"`
}

type runSummaryWriteArgs struct {
	Content string `json:"content" mcp:"≤80-char summary of what this run did. Shown in the ccron status table. Last call wins; empty clears."`
}

// buildMCPServer wires up the run and memory tools on a single server. store
// may be nil — memory tools then return errMemoryNotEnabled on call, though
// allowed_tools should prevent claude from reaching them.
func buildMCPServer(store *Store) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: "1"}, nil)

	// run_summary_write — stateless stub. The runner extracts the argument
	// from claude's stream-json stdout; this handler just acknowledges so
	// claude sees a successful tool call.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "run_summary_write",
		Description: "Write a ≤80-char summary of what this run did (or \"no-op\" if nothing happened). Shown in the ccron status table. Last call wins; empty clears.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ runSummaryWriteArgs) (*mcp.CallToolResult, any, error) {
		return textResult("ok"), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_summary_view",
		Description: "View the persistent summary.md for this job. Returns empty string if no summary has been written yet.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		if store == nil {
			return nil, nil, errMemoryNotEnabled
		}
		content, err := store.SummaryView()
		if err != nil {
			return nil, nil, err
		}
		return textResult(content), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_summary_write",
		Description: "Atomically rewrite the summary.md for this job. Empty content deletes the file.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args summaryWriteArgs) (*mcp.CallToolResult, any, error) {
		if store == nil {
			return nil, nil, errMemoryNotEnabled
		}
		if err := store.SummaryWrite(args.Content); err != nil {
			return nil, nil, err
		}
		return textResult("ok"), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_log_list",
		Description: "List log records for this job, newest first. Use offset to page back through older records.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args logListArgs) (*mcp.CallToolResult, any, error) {
		if store == nil {
			return nil, nil, errMemoryNotEnabled
		}
		recs, err := store.LogList(args.Limit, args.Offset)
		if err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(recs)
		if err != nil {
			return nil, nil, err
		}
		return textResult(string(body)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_log_write",
		Description: "Append a record to the job's log. Oldest records are evicted when the log exceeds the configured cap.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args logWriteArgs) (*mcp.CallToolResult, any, error) {
		if store == nil {
			return nil, nil, errMemoryNotEnabled
		}
		rec, err := store.LogWrite(args.Content)
		if err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(rec)
		if err != nil {
			return nil, nil, err
		}
		return textResult(string(body)), nil, nil
	})

	return server
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

// mcpConfig is the shape of a claude --mcp-config JSON file. Single-server
// flat shape — no env, no transport config; `claude` defaults to stdio.
type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// writeMCPConfig writes the claude --mcp-config file pointing at this binary's
// `mcp` subcommand. memDir/maxRecords are optional: pass empty dir / 0 when
// memory is disabled for the job — the resulting server will have a nil store.
func writeMCPConfig(path, selfExe, memDir string, maxRecords int) error {
	args := []string{"mcp"}
	if memDir != "" {
		args = append(args,
			"--memory-dir", memDir,
			"--max-records", fmt.Sprintf("%d", maxRecords),
		)
	}
	cfg := mcpConfig{
		MCPServers: map[string]mcpServer{
			mcpServerName: {
				Command: selfExe,
				Args:    args,
			},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
