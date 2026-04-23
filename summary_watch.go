package main

import (
	"bufio"
	"encoding/json"
	"io"
)

// summaryToolName is the fully-qualified tool name claude emits on the wire
// when the agent calls run_summary_write.
const summaryToolName = "mcp__ccron__run_summary_write"

// summaryMaxBytes caps the stored summary length. Byte-aware, not rune-aware
// (rune-aware is a parked non-goal).
const summaryMaxBytes = 80

// watchSummary scans claude's stream-json NDJSON output from r for tool_use
// events calling run_summary_write and returns the last observed content
// (truncated to summaryMaxBytes) when r reaches EOF. Returns "" if the tool
// was never called. Malformed lines and non-matching events are silently
// skipped.
func watchSummary(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var last string
	for sc.Scan() {
		var ev struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || ev.Type != "assistant" {
			continue
		}
		var m struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal(ev.Message, &m); err != nil {
			continue
		}
		for _, b := range m.Content {
			if b.Type != "tool_use" || b.Name != summaryToolName {
				continue
			}
			var args struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(b.Input, &args); err != nil {
				continue
			}
			if len(args.Content) > summaryMaxBytes {
				args.Content = args.Content[:summaryMaxBytes]
			}
			last = args.Content
		}
	}
	return last
}
