package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCmd runs the top-level CLI against args (starting after the program
// name) with --jobs-dir pointed at jobsDir. Stdout and stderr are redirected
// into the returned strings for inspection.
func runCmd(t *testing.T, jobsDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	// Redirect stdout/stderr through pipes.
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW

	done := make(chan struct{})
	var outBuf, errBuf strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := outR.Read(buf)
			if n > 0 {
				outBuf.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := errR.Read(buf)
			if n > 0 {
				errBuf.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	app := buildApp()
	full := append([]string{"ccron", "--jobs-dir", jobsDir, "--log-dir", filepath.Join(t.TempDir(), "logs")}, args...)
	err = app.Run(context.Background(), full)

	outW.Close()
	errW.Close()
	<-done
	<-done
	os.Stdout, os.Stderr = oldOut, oldErr
	return outBuf.String(), errBuf.String(), err
}

func TestCmdValidate_AllValid(t *testing.T) {
	dir := t.TempDir()
	writeJobFile(t, dir, "a", "* * * * *", "body-a")
	writeJobFile(t, dir, "b", "0 9 * * *", "body-b")

	stdout, _, err := runCmd(t, dir, "validate")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.Contains(stdout, "2 valid, 0 invalid") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestCmdValidate_MixedFailsNonZero(t *testing.T) {
	dir := t.TempDir()
	writeJobFile(t, dir, "good", "* * * * *", "ok")
	// Two bad jobs — missing workdir.
	if err := os.WriteFile(filepath.Join(dir, "bad1.md"), []byte("---\nschedule: \"* * * * *\"\nallowed_tools: [Read]\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad2.md"), []byte("---\nschedule: bogus\nworkdir: /tmp\nallowed_tools: [Read]\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCmd(t, dir, "validate")
	if err == nil {
		t.Fatal("expected error exit, got nil")
	}
	for _, want := range []string{"bad1.md", "bad2.md"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestCmdStatus_Default(t *testing.T) {
	dir := t.TempDir()
	writeJobFile(t, dir, "alpha", "0 9 * * *", "a")
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\nschedule: nope\nworkdir: /tmp\nallowed_tools: [Read]\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(t, dir)
	if err != nil {
		t.Fatalf("status returned error: %v", err)
	}
	for _, want := range []string{"alpha", "NEXT RUN", "never run", "broken", "parse error"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status missing %q:\n%s", want, stdout)
		}
	}
}
