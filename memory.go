package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// MemoryConfig is the per-job memory configuration. nil on Job means disabled.
type MemoryConfig struct {
	MaxRecords     int
	InitialRecords int
}

// Record is one entry in log.jsonl.
type Record struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Content   string    `json:"content"`
}

// Store wraps a per-job memory directory containing summary.md and log.jsonl.
type Store struct {
	Dir string
	Cap int
}

const (
	memorySummaryFile = "summary.md"
	memoryLogFile     = "log.jsonl"
)

// SummaryView returns the contents of summary.md, or empty string if absent.
func (s *Store) SummaryView() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, memorySummaryFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read summary: %w", err)
	}
	return string(data), nil
}

// SummaryWrite atomically rewrites summary.md. Empty content removes the file.
func (s *Store) SummaryWrite(content string) error {
	path := filepath.Join(s.Dir, memorySummaryFile)
	if content == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove summary: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, "summary-*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename tmp: %w", err)
	}
	cleanup = false
	return nil
}

// LogList returns log records newest-first. limit<=0 returns all available
// (capped implicitly by Cap). offset skips that many from the newest end.
func (s *Store) LogList(limit, offset int) ([]Record, error) {
	if offset < 0 {
		offset = 0
	}
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	// Reverse to newest-first.
	out := make([]Record, len(all))
	for i, r := range all {
		out[len(all)-1-i] = r
	}
	if offset >= len(out) {
		return nil, nil
	}
	out = out[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LogWrite appends a record to log.jsonl, evicting oldest entries if the file
// would exceed Cap. Holds flock for the read-modify-write to keep concurrent
// writers from interleaving.
func (s *Store) LogWrite(content string) (Record, error) {
	if s.Cap <= 0 {
		return Record{}, fmt.Errorf("memory disabled (cap=0)")
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return Record{}, fmt.Errorf("create memory dir: %w", err)
	}
	path := filepath.Join(s.Dir, memoryLogFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return Record{}, fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return Record{}, fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	existing, err := readRecords(f, path)
	if err != nil {
		return Record{}, err
	}

	now := time.Now().UTC()
	rec := Record{
		ID:        strconv.FormatInt(now.UnixNano(), 16),
		CreatedAt: now,
		Content:   content,
	}
	existing = append(existing, rec)
	if len(existing) > s.Cap {
		existing = existing[len(existing)-s.Cap:]
	}

	if _, err := f.Seek(0, 0); err != nil {
		return Record{}, err
	}
	if err := f.Truncate(0); err != nil {
		return Record{}, err
	}
	enc := json.NewEncoder(f)
	for _, r := range existing {
		if err := enc.Encode(r); err != nil {
			return Record{}, fmt.Errorf("write log: %w", err)
		}
	}
	return rec, nil
}

// readAll opens log.jsonl read-only and returns all records oldest-first.
func (s *Store) readAll() ([]Record, error) {
	path := filepath.Join(s.Dir, memoryLogFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer f.Close()
	return readRecords(f, path)
}

// readRecords parses a log.jsonl stream, skipping malformed lines with a log
// warning. Position-independent: caller is responsible for seeking if needed.
func readRecords(f *os.File, path string) ([]Record, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			log.Printf("memory: skipping malformed line in %s: %v", path, err)
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan log: %w", err)
	}
	return out, nil
}
