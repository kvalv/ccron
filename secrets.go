package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/joho/godotenv"
)

// loadEnvFile reads the .env file at path and returns its contents as a map.
// The file must be mode 0600 — any wider and we refuse to load it.
// A non-existent file returns (nil, nil): jobs without `secrets:` won't care,
// and jobs that declare secrets will fail loudly when resolve can't find them.
func loadEnvFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		return nil, fmt.Errorf("%s has mode %04o, want 0600", path, mode)
	}
	env, err := godotenv.Read(path)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return env, nil
}

// resolveSecrets looks up each declared name in env and returns the matching
// "NAME=value" strings in declaration order. Missing names produce an error
// listing all of them, so the caller sees the full picture on the first try.
func resolveSecrets(env map[string]string, names []string) ([]string, []string, error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	var missing []string
	pairs := make([]string, 0, len(names))
	values := make([]string, 0, len(names))
	for _, n := range names {
		v, ok := env[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		pairs = append(pairs, n+"="+v)
		values = append(values, v)
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("secrets missing from .env: %v", missing)
	}
	return pairs, values, nil
}

// redactingWriter replaces every occurrence of each value in values with
// "***" before writing to the underlying writer. Empty values are skipped so
// we don't eat every byte of output by matching the empty string.
//
// Writes are assumed to arrive on logical boundaries (NDJSON lines, log lines)
// so we don't buffer across Write calls. If a secret straddles two writes it
// won't be caught — acceptable for v1 given how claude streams output.
type redactingWriter struct {
	w      io.Writer
	values [][]byte
}

func newRedactingWriter(w io.Writer, values []string) io.Writer {
	if len(values) == 0 {
		return w
	}
	vs := make([][]byte, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		vs = append(vs, []byte(v))
	}
	if len(vs) == 0 {
		return w
	}
	return &redactingWriter{w: w, values: vs}
}

var redactedMask = []byte("***")

func (r *redactingWriter) Write(p []byte) (int, error) {
	out := p
	for _, v := range r.values {
		if bytes.Contains(out, v) {
			out = bytes.ReplaceAll(out, v, redactedMask)
		}
	}
	if _, err := r.w.Write(out); err != nil {
		return 0, err
	}
	// Report the original byte count so callers (io.Copy, etc.) don't loop.
	return len(p), nil
}
