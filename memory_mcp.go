package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP server name. Tools are exposed to claude as
// `mcp__ccron_memory__memory_summary_view`, etc.
const memoryMCPServerName = "ccron_memory"

// memoryMCPToolNames lists the tools exposed by the ccron_memory server, in the
// `mcp__<server>__<tool>` namespacing claude uses on the wire. Auto-injected
// into --allowedTools when memory is enabled on a job.
var memoryMCPToolNames = []string{
	"mcp__ccron_memory__memory_summary_view",
	"mcp__ccron_memory__memory_summary_write",
	"mcp__ccron_memory__memory_log_list",
	"mcp__ccron_memory__memory_log_write",
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

// buildMemoryMCPServer wires up the four memory tools backed by the given
// store. Exposed for tests.
func buildMemoryMCPServer(store *Store) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: memoryMCPServerName, Version: "1"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_summary_view",
		Description: "View the persistent summary.md for this job. Returns empty string if no summary has been written yet.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
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
		if err := store.SummaryWrite(args.Content); err != nil {
			return nil, nil, err
		}
		return textResult("ok"), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_log_list",
		Description: "List log records for this job, newest first. Use offset to page back through older records.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args logListArgs) (*mcp.CallToolResult, any, error) {
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

// writeMemoryMCPConfig writes the claude --mcp-config file pointing at this
// binary's `memory-mcp` subcommand for the given job. Returns the path.
func writeMemoryMCPConfig(path, selfExe, jobName, memDir string, maxRecords int) error {
	cfg := mcpConfig{
		MCPServers: map[string]mcpServer{
			memoryMCPServerName: {
				Command: selfExe,
				Args: []string{
					"memory-mcp",
					"--job", jobName,
					"--memory-dir", memDir,
					"--max-records", fmt.Sprintf("%d", maxRecords),
				},
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
