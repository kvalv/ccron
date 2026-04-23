package main

import (
	"strings"
	"testing"
)

// assistantToolUse builds one stream-json assistant event with a single
// tool_use content block for the given tool name and content arg.
func assistantToolUse(name, content string) string {
	return `{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"` + name + `","input":{"content":` + jsonString(content) + `}}` +
		`]}}`
}

func jsonString(s string) string {
	// Minimal JSON string encoder for test fixtures (no embedded quotes/newlines needed).
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestWatchSummary(t *testing.T) {
	longContent := strings.Repeat("x", 120)
	cases := []struct {
		desc string
		in   string
		want string
	}{
		{
			desc: "single summary call",
			in:   assistantToolUse(summaryToolName, "ran fine"),
			want: "ran fine",
		},
		{
			desc: "multiple calls — last wins",
			in: assistantToolUse(summaryToolName, "first") + "\n" +
				assistantToolUse(summaryToolName, "second") + "\n" +
				assistantToolUse(summaryToolName, "third"),
			want: "third",
		},
		{
			desc: "no summary call",
			in:   assistantToolUse("mcp__ccron__memory_log_write", "some log entry"),
			want: "",
		},
		{
			desc: "over-80-byte content truncated",
			in:   assistantToolUse(summaryToolName, longContent),
			want: strings.Repeat("x", 80),
		},
		{
			desc: "unrelated tool_use ignored",
			in: assistantToolUse("mcp__ccron__memory_log_write", "log") + "\n" +
				assistantToolUse(summaryToolName, "real"),
			want: "real",
		},
		{
			desc: "malformed line skipped",
			in: "not valid json\n" +
				`{"type":"assistant","message":"not an object"}` + "\n" +
				assistantToolUse(summaryToolName, "survived"),
			want: "survived",
		},
		{
			desc: "non-assistant events interleaved",
			in: `{"type":"system","subtype":"init","tools":["x"]}` + "\n" +
				`{"type":"result","result":"ok"}` + "\n" +
				assistantToolUse(summaryToolName, "after noise"),
			want: "after noise",
		},
		{
			desc: "empty input",
			in:   "",
			want: "",
		},
		{
			desc: "empty content explicit clear",
			in: assistantToolUse(summaryToolName, "initial") + "\n" +
				assistantToolUse(summaryToolName, ""),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := watchSummary(strings.NewReader(tc.in))
			if got != tc.want {
				t.Fatalf("watchSummary:\ngot  %q\nwant %q", got, tc.want)
			}
		})
	}
}
