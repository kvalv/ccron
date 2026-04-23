package main

import (
	"strings"
	"testing"
	"time"
)

func TestRenderEvents(t *testing.T) {
	// Fixed clock so output is deterministic.
	fixed := time.Date(2026, 4, 23, 13, 29, 30, 0, time.UTC)
	now := func() time.Time { return fixed }

	cases := []struct {
		desc string
		in   string
		want []string // substrings that must all appear in the rendered output
	}{
		{
			desc: "system init renders model, short session id, permission mode",
			in:   `{"type":"system","subtype":"init","session_id":"a12e0dea-6ded-4873","model":"claude-opus-4-7","permissionMode":"default"}`,
			want: []string{"13:29:30", "● session a12e0dea", "claude-opus-4-7", "permission=default"},
		},
		{
			desc: "system non-init is skipped",
			in:   `{"type":"system","subtype":"info"}`,
			want: []string{}, // nothing
		},
		{
			desc: "assistant tool_use Bash shows command",
			in:   `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}`,
			want: []string{"▶ Bash", "echo hi"},
		},
		{
			desc: "assistant tool_use Read shows file_path",
			in:   `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/etc/hosts"}}]}}`,
			want: []string{"▶ Read", "/etc/hosts"},
		},
		{
			desc: "assistant tool_use Grep shows pattern and path",
			in:   `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"TODO","path":"src/"}}]}}`,
			want: []string{"▶ Grep", "TODO", "in", "src/"},
		},
		{
			desc: "assistant tool_use unknown tool falls back to raw input JSON",
			in:   `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__custom__thing","input":{"foo":"bar"}}]}}`,
			want: []string{"▶ mcp__custom__thing", `"foo":"bar"`},
		},
		{
			desc: "empty thinking block is skipped entirely",
			in:   `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"xyz"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
			want: []string{"▶ Bash", "ls"},
		},
		{
			desc: "non-empty thinking block is rendered",
			in:   `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"let me consider..."}]}}`,
			want: []string{"🧠", "let me consider"},
		},
		{
			desc: "assistant text message",
			in:   `{"type":"assistant","message":{"content":[{"type":"text","text":"All done."}]}}`,
			want: []string{"💬", "All done."},
		},
		{
			desc: "user tool_result string content, first line shown",
			in:   `{"type":"user","message":{"content":[{"type":"tool_result","content":"line one\nline two","is_error":false}]}}`,
			want: []string{"✓", "line one / line two"},
		},
		{
			desc: "user tool_result array content collapsed to text",
			in:   `{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"hello"}]}]}}`,
			want: []string{"✓", "hello"},
		},
		{
			desc: "user tool_result with is_error shows cross",
			in:   `{"type":"user","message":{"content":[{"type":"tool_result","content":"boom","is_error":true}]}}`,
			want: []string{"✗", "boom"},
		},
		{
			desc: "result success",
			in:   `{"type":"result","subtype":"success","is_error":false,"duration_ms":12345,"num_turns":4,"total_cost_usd":0.15,"result":"OK"}`,
			want: []string{"✓ done in", "12.3s", "4 turns", "$0.15"},
		},
		{
			desc: "result error",
			in:   `{"type":"result","is_error":true,"duration_ms":500,"num_turns":1,"total_cost_usd":0.01}`,
			want: []string{"✗ done in", "500ms", "1 turns", "$0.01"},
		},
		{
			desc: "rate_limit_event allowed is silent",
			in:   `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
			want: []string{},
		},
		{
			desc: "rate_limit_event non-allowed surfaces",
			in:   `{"type":"rate_limit_event","rate_limit_info":{"status":"throttled"}}`,
			want: []string{"⚠ rate limit", "throttled"},
		},
		{
			desc: "garbage lines are dropped",
			in:   "not json\n{bad\n" + `{"type":"result","duration_ms":100,"num_turns":1}`,
			want: []string{"done in", "100ms"},
		},
		{
			desc: "very long tool output truncated with ellipsis",
			in: `{"type":"user","message":{"content":[{"type":"tool_result","content":"` +
				strings.Repeat("x", 500) + `"}]}}`,
			want: []string{"✓", "…"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			var buf strings.Builder
			if err := renderEventsAt(strings.NewReader(tc.in+"\n"), &buf, now); err != nil {
				t.Fatalf("RenderEvents: %v", err)
			}
			got := buf.String()
			if len(tc.want) == 0 {
				if strings.TrimSpace(got) != "" {
					t.Errorf("expected no output, got:\n%s", got)
				}
				return
			}
			for _, needle := range tc.want {
				if !strings.Contains(got, needle) {
					t.Errorf("missing %q in output:\n%s", needle, got)
				}
			}
		})
	}
}

func TestRenderEvents_realSampleLog(t *testing.T) {
	// Derived from a real ccron-demo/heartbeat run — three Bash tool calls
	// with results, then a text answer and a success result.
	sample := `{"type":"system","subtype":"init","session_id":"e5ca629c-90ab","model":"claude-opus-4-7","permissionMode":"default"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"sig"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo \"step 1\" && whoami"}}]}}
{"type":"user","message":{"content":[{"tool_use_id":"t1","type":"tool_result","content":"step 1\nmikael","is_error":false}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"pwd"}}]}}
{"type":"user","message":{"content":[{"tool_use_id":"t2","type":"tool_result","content":"/tmp","is_error":false}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":8000,"num_turns":3,"total_cost_usd":0.05,"result":"Done."}
`
	fixed := time.Date(2026, 4, 23, 13, 29, 30, 0, time.UTC)
	var buf strings.Builder
	if err := renderEventsAt(strings.NewReader(sample), &buf, func() time.Time { return fixed }); err != nil {
		t.Fatalf("RenderEvents: %v", err)
	}
	got := buf.String()

	// Lines we expect to see, in order.
	wantInOrder := []string{
		"● session e5ca629c",
		"▶ Bash",
		"whoami",
		"✓ step 1 / mikael",
		"▶ Bash",
		"pwd",
		"✓ /tmp",
		"💬 Done.",
		"✓ done in 8.0s · 3 turns · $0.05",
	}
	idx := 0
	for _, needle := range wantInOrder {
		i := strings.Index(got[idx:], needle)
		if i < 0 {
			t.Fatalf("missing (or out of order) %q in output:\n%s", needle, got)
		}
		idx += i + len(needle)
	}

	// Rate-limit-allowed line must be silent.
	if strings.Contains(got, "rate limit") {
		t.Errorf("allowed rate_limit_event leaked into output:\n%s", got)
	}
	// Empty thinking block must not produce a 🧠 line.
	if strings.Contains(got, "🧠") {
		t.Errorf("empty thinking block leaked into output:\n%s", got)
	}
}
