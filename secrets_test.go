package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	cases := []struct {
		desc    string
		setup   func(t *testing.T, path string)
		wantMap map[string]string
		wantErr string // substring; empty = no error expected
	}{
		{
			desc: "missing file returns nil map and no error",
			setup: func(t *testing.T, path string) {
				// no-op: file does not exist
			},
			wantMap: nil,
		},
		{
			desc: "valid 0600 file parses KEY=value pairs",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("FOO=bar\nBAZ=qux\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantMap: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			desc: "world-readable file is rejected",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("FOO=bar\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "mode",
		},
		{
			desc: "group-readable file is rejected",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("FOO=bar\n"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "mode",
		},
		{
			desc: "comments and blank lines ignored",
			setup: func(t *testing.T, path string) {
				content := "# a comment\n\nFOO=bar\n# another\nBAZ=qux\n"
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantMap: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".env")
			tc.setup(t, path)

			got, err := loadEnvFile(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !mapsEqual(got, tc.wantMap) {
				t.Fatalf("got %v, want %v", got, tc.wantMap)
			}
		})
	}
}

func TestResolveSecrets(t *testing.T) {
	env := map[string]string{
		"FOO": "bar",
		"BAZ": "qux",
	}

	cases := []struct {
		desc       string
		names      []string
		wantPairs  []string
		wantValues []string
		wantErr    string
	}{
		{
			desc:  "empty names returns nothing",
			names: nil,
		},
		{
			desc:       "resolves declared names in order",
			names:      []string{"FOO", "BAZ"},
			wantPairs:  []string{"FOO=bar", "BAZ=qux"},
			wantValues: []string{"bar", "qux"},
		},
		{
			desc:    "missing name fails loudly",
			names:   []string{"FOO", "NOPE"},
			wantErr: "NOPE",
		},
		{
			desc:    "multiple missing names listed",
			names:   []string{"NOPE", "ALSO_NOPE"},
			wantErr: "ALSO_NOPE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			pairs, values, err := resolveSecrets(env, tc.names)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !stringSlicesEqual(pairs, tc.wantPairs) {
				t.Fatalf("pairs: got %v, want %v", pairs, tc.wantPairs)
			}
			if !stringSlicesEqual(values, tc.wantValues) {
				t.Fatalf("values: got %v, want %v", values, tc.wantValues)
			}
		})
	}
}

func TestRedactingWriter(t *testing.T) {
	cases := []struct {
		desc   string
		values []string
		input  string
		want   string
	}{
		{
			desc:   "replaces single value",
			values: []string{"secret123"},
			input:  "hello secret123 world",
			want:   "hello *** world",
		},
		{
			desc:   "replaces multiple values",
			values: []string{"alpha", "beta"},
			input:  "alpha and beta and alpha",
			want:   "*** and *** and ***",
		},
		{
			desc:   "no values is passthrough",
			values: nil,
			input:  "unchanged secret",
			want:   "unchanged secret",
		},
		{
			desc:   "empty value in slice is skipped",
			values: []string{"", "real"},
			input:  "the real thing",
			want:   "the *** thing",
		},
		{
			desc:   "value not present is unchanged",
			values: []string{"absent"},
			input:  "nothing to redact here",
			want:   "nothing to redact here",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			var buf bytes.Buffer
			w := newRedactingWriter(&buf, tc.values)
			n, err := w.Write([]byte(tc.input))
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if n != len(tc.input) {
				t.Fatalf("wrote %d bytes, want %d", n, len(tc.input))
			}
			if buf.String() != tc.want {
				t.Fatalf("got %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

func TestBuildSecretsPreamble(t *testing.T) {
	cases := []struct {
		desc    string
		names   []string
		wantHas []string // substrings that must appear; nil = expect empty string
	}{
		{
			desc:  "empty list yields empty preamble",
			names: nil,
		},
		{
			desc:    "single name appears in preamble",
			names:   []string{"HA_TOKEN"},
			wantHas: []string{"HA_TOKEN", "Environment variables"},
		},
		{
			desc:    "multiple names joined with commas",
			names:   []string{"HA_TOKEN", "HA_URL"},
			wantHas: []string{"HA_TOKEN, HA_URL"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := buildSecretsPreamble(tc.names)
			if tc.wantHas == nil {
				if got != "" {
					t.Fatalf("expected empty, got %q", got)
				}
				return
			}
			for _, sub := range tc.wantHas {
				if !contains(got, sub) {
					t.Fatalf("preamble missing %q:\n%s", sub, got)
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
