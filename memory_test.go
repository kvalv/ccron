package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newStore(t *testing.T, cap int) *Store {
	t.Helper()
	return &Store{Dir: t.TempDir(), Cap: cap}
}

func TestMemory_LogWrite_Empty(t *testing.T) {
	s := newStore(t, 10)
	rec, err := s.LogWrite("hello")
	if err != nil {
		t.Fatalf("LogWrite: %v", err)
	}
	if rec.Content != "hello" || rec.ID == "" || rec.CreatedAt.IsZero() {
		t.Fatalf("bad record: %+v", rec)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, memoryLogFile))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := splitNonEmpty(string(data))
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %q", len(lines), data)
	}
}

func TestMemory_LogWrite_PastCap_DropsOldest(t *testing.T) {
	s := newStore(t, 3)
	for _, c := range []string{"a", "b", "c", "d", "e"} {
		if _, err := s.LogWrite(c); err != nil {
			t.Fatalf("LogWrite(%s): %v", c, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, memoryLogFile))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := splitNonEmpty(string(data))
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	recs, err := s.LogList(0, 0)
	if err != nil {
		t.Fatalf("LogList: %v", err)
	}
	// Newest-first, oldest evicted.
	want := []string{"e", "d", "c"}
	if len(recs) != len(want) {
		t.Fatalf("got %d records, want %d", len(recs), len(want))
	}
	for i, w := range want {
		if recs[i].Content != w {
			t.Errorf("recs[%d].Content = %q, want %q", i, recs[i].Content, w)
		}
	}
}

func TestMemory_LogList_NewestFirst(t *testing.T) {
	s := newStore(t, 10)
	for _, c := range []string{"first", "second", "third"} {
		if _, err := s.LogWrite(c); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := s.LogList(0, 0)
	if err != nil {
		t.Fatalf("LogList: %v", err)
	}
	want := []string{"third", "second", "first"}
	for i, w := range want {
		if recs[i].Content != w {
			t.Errorf("recs[%d] = %q, want %q", i, recs[i].Content, w)
		}
	}
}

func TestMemory_LogList_LimitOffset(t *testing.T) {
	s := newStore(t, 20)
	for i := 0; i < 10; i++ {
		if _, err := s.LogWrite(fmt.Sprintf("r%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		desc         string
		limit        int
		offset       int
		wantContents []string
	}{
		{
			desc:         "limit 3 newest",
			limit:        3,
			offset:       0,
			wantContents: []string{"r9", "r8", "r7"},
		},
		{
			desc:         "limit 3 offset 5",
			limit:        3,
			offset:       5,
			wantContents: []string{"r4", "r3", "r2"},
		},
		{
			desc:         "offset past end",
			limit:        5,
			offset:       100,
			wantContents: nil,
		},
		{
			desc:         "limit 0 returns all",
			limit:        0,
			offset:       0,
			wantContents: []string{"r9", "r8", "r7", "r6", "r5", "r4", "r3", "r2", "r1", "r0"},
		},
		{
			desc:         "limit larger than available",
			limit:        50,
			offset:       8,
			wantContents: []string{"r1", "r0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			recs, err := s.LogList(tc.limit, tc.offset)
			if err != nil {
				t.Fatalf("LogList: %v", err)
			}
			if len(recs) != len(tc.wantContents) {
				t.Fatalf("got %d records, want %d (%v)", len(recs), len(tc.wantContents), recs)
			}
			for i, w := range tc.wantContents {
				if recs[i].Content != w {
					t.Errorf("recs[%d].Content = %q, want %q", i, recs[i].Content, w)
				}
			}
		})
	}
}

func TestMemory_LogList_CorruptLineSkipped(t *testing.T) {
	s := newStore(t, 10)
	if _, err := s.LogWrite("good1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LogWrite("good2"); err != nil {
		t.Fatal(err)
	}
	// Inject a corrupt line by appending to the file directly.
	path := filepath.Join(s.Dir, memoryLogFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Insert garbage between the two valid lines.
	corrupted := "{not json at all\n" + string(data)
	if err := os.WriteFile(path, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}

	recs, err := s.LogList(0, 0)
	if err != nil {
		t.Fatalf("LogList: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records (corrupt skipped), got %d", len(recs))
	}
}

func TestMemory_LogWrite_Concurrent(t *testing.T) {
	s := newStore(t, 100)
	var wg sync.WaitGroup
	const writers = 10
	const perWriter = 10
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := s.LogWrite(fmt.Sprintf("w%d-%d", w, i)); err != nil {
					t.Errorf("LogWrite: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	recs, err := s.LogList(0, 0)
	if err != nil {
		t.Fatalf("LogList: %v", err)
	}
	if len(recs) != writers*perWriter {
		t.Fatalf("expected %d records, got %d", writers*perWriter, len(recs))
	}
}

func TestMemory_SummaryView_Missing(t *testing.T) {
	s := newStore(t, 10)
	got, err := s.SummaryView()
	if err != nil {
		t.Fatalf("SummaryView: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestMemory_SummaryWrite_Roundtrip(t *testing.T) {
	s := newStore(t, 10)
	const want = "hello\nworld\n"
	if err := s.SummaryWrite(want); err != nil {
		t.Fatalf("SummaryWrite: %v", err)
	}
	got, err := s.SummaryView()
	if err != nil {
		t.Fatalf("SummaryView: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMemory_SummaryWrite_EmptyRemoves(t *testing.T) {
	s := newStore(t, 10)
	if err := s.SummaryWrite("something"); err != nil {
		t.Fatal(err)
	}
	if err := s.SummaryWrite(""); err != nil {
		t.Fatalf("SummaryWrite empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, memorySummaryFile)); !os.IsNotExist(err) {
		t.Fatalf("expected summary.md removed, stat err: %v", err)
	}
	// Removing again is a no-op.
	if err := s.SummaryWrite(""); err != nil {
		t.Fatalf("second SummaryWrite empty: %v", err)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range splitLines(s) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
