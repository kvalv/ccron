package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// RenderEvents reads newline-delimited claude stream-json events from r and
// writes a human-readable summary to w. Unparseable lines are skipped silently
// (they might be partial JSON while the process is still writing, or the
// non-JSON "starting job ..." preamble the runner emits).
//
// Colors are emitted automatically when w is a terminal and NO_COLOR is not
// set (https://no-color.org/). Piping into a file or another process drops
// to plain ASCII so logs and jq pipelines stay clean.
//
// Returns nil on normal EOF. Any write error aborts.
func RenderEvents(r io.Reader, w io.Writer) error {
	return renderEventsAt(r, w, time.Now, pickPalette(w))
}

// renderEventsAt is the testable core — accepts an injectable clock and an
// explicit palette so tests can assert both plain and colored output.
func renderEventsAt(r io.Reader, w io.Writer, now func() time.Time, p palette) error {
	sc := bufio.NewScanner(r)
	// Default Scanner buffer (64 KiB) is too small for init events that
	// enumerate every tool name. 4 MiB is plenty.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev envelope
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out := renderEvent(ev, now(), p)
		if out == "" {
			continue
		}
		if _, err := fmt.Fprintln(w, out); err != nil {
			return err
		}
	}
	return sc.Err()
}

type envelope struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`

	// system init
	Model          string `json:"model,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`

	// result
	Result       string  `json:"result,omitempty"`
	DurationMs   int64   `json:"duration_ms,omitempty"`
	NumTurns     int     `json:"num_turns,omitempty"`
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`

	// rate_limit_event
	RateLimitInfo *struct {
		Status string `json:"status"`
	} `json:"rate_limit_info,omitempty"`
}

type msgBody struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	// tool_use
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// text / thinking
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func renderEvent(ev envelope, t time.Time, p palette) string {
	ts := p.gray + t.Format("15:04:05") + p.reset
	switch ev.Type {
	case "system":
		if ev.Subtype != "init" {
			return ""
		}
		sid := ev.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		mode := ev.PermissionMode
		if mode == "" {
			mode = "default"
		}
		return fmt.Sprintf("%s %s●%s session %s · %s · permission=%s",
			ts, p.cyan, p.reset, sid, ev.Model, mode)

	case "rate_limit_event":
		if ev.RateLimitInfo == nil || ev.RateLimitInfo.Status == "allowed" {
			return ""
		}
		return fmt.Sprintf("%s %s⚠ rate limit: %s%s",
			ts, p.yellow, ev.RateLimitInfo.Status, p.reset)

	case "assistant":
		return renderAssistant(ev.Message, ts, p)

	case "user":
		return renderUser(ev.Message, ts, p)

	case "result":
		mark, color := "✓", p.green
		if ev.IsError {
			mark, color = "✗", p.red
		}
		dur := time.Duration(ev.DurationMs) * time.Millisecond
		return fmt.Sprintf("%s %s%s%s done in %s · %d turns · $%.2f",
			ts, color+p.bold, mark, p.reset,
			formatDuration(dur), ev.NumTurns, ev.TotalCostUSD)
	}
	return ""
}

func renderAssistant(raw json.RawMessage, ts string, p palette) string {
	var m msgBody
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	var lines []string
	for _, b := range m.Content {
		switch b.Type {
		case "thinking":
			// Skip empty-text thinking blocks (signature-only placeholders).
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s %s🧠 %s%s",
				ts, p.dim, truncOneLine(b.Thinking, 120), p.reset))
		case "tool_use":
			lines = append(lines, fmt.Sprintf("%s %s▶%s %s%s%s   %s",
				ts, p.blue, p.reset,
				p.cyan, padRight(b.Name, 6), p.reset,
				truncOneLine(toolSummary(b.Name, b.Input), 120)))
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s 💬 %s", ts, truncOneLine(b.Text, 300)))
		}
	}
	return strings.Join(lines, "\n")
}

func renderUser(raw json.RawMessage, ts string, p palette) string {
	var m msgBody
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	var lines []string
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			continue
		}
		mark, color := "✓", p.green
		if b.IsError {
			mark, color = "✗", p.red
		}
		body := toolResultText(b.Content)
		lines = append(lines, fmt.Sprintf("%s   %s%s%s %s",
			ts, color, mark, p.reset, truncOneLine(body, 160)))
	}
	return strings.Join(lines, "\n")
}

// toolSummary returns a compact human-readable summary of a tool's input
// dictionary. Known tools get field-specific treatment; unknown tools fall
// back to the raw JSON (already compact, one line).
func toolSummary(name string, input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return string(input)
	}
	switch name {
	case "Bash":
		if s, ok := m["command"].(string); ok {
			return s
		}
	case "Read", "Edit", "Write", "NotebookEdit":
		if s, ok := m["file_path"].(string); ok {
			return s
		}
	case "Glob":
		if s, ok := m["pattern"].(string); ok {
			return s
		}
	case "Grep":
		pat, _ := m["pattern"].(string)
		if p, ok := m["path"].(string); ok && p != "" {
			return pat + "  in  " + p
		}
		return pat
	case "WebFetch":
		if s, ok := m["url"].(string); ok {
			return s
		}
	case "WebSearch":
		if s, ok := m["query"].(string); ok {
			return s
		}
	case "TodoWrite":
		return fmt.Sprintf("%d todos", len(asSlice(m["todos"])))
	case "Task":
		if s, ok := m["description"].(string); ok {
			return s
		}
	}
	return string(input)
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// toolResultText extracts a plain string from a tool_result's content field,
// which may be (a) a plain string, or (b) an array of {type,text} blocks per
// the Anthropic API spec. Unknown shapes return the raw JSON.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var buf []string
		for _, p := range parts {
			if p.Type == "text" {
				buf = append(buf, p.Text)
			}
		}
		return strings.Join(buf, "\n")
	}
	return string(raw)
}

// truncOneLine collapses newlines to " / " and truncates to n runes with an
// ellipsis. Used for single-line summaries of potentially multi-line content.
func truncOneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " / ")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// palette holds ANSI escape codes used by the renderer. A zero-value palette
// has empty strings for every field, producing plain ASCII output. The
// `ansi` palette has the real codes; pickPalette picks between them based on
// the output destination.
type palette struct {
	reset, dim, bold   string
	red, green, yellow string
	blue, cyan, gray   string
}

var ansi = palette{
	reset:  "\x1b[0m",
	dim:    "\x1b[2m",
	bold:   "\x1b[1m",
	red:    "\x1b[31m",
	green:  "\x1b[32m",
	yellow: "\x1b[33m",
	blue:   "\x1b[34m",
	cyan:   "\x1b[36m",
	gray:   "\x1b[90m",
}

// pickPalette returns the ansi palette when w is a terminal and NO_COLOR
// is not set; otherwise a zero palette that renders plain ASCII. The check
// only recognises *os.File writers — other writer shapes (io.Pipe, buffer,
// etc.) are assumed non-terminal and get no color. That matches how
// callers currently hook up the renderer.
func pickPalette(w io.Writer) palette {
	if os.Getenv("NO_COLOR") != "" {
		return palette{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return palette{}
	}
	if !term.IsTerminal(int(f.Fd())) {
		return palette{}
	}
	return ansi
}
