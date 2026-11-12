package version

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	if Get() == "" {
		t.Error("expected Get() / Version to be non-empty by default")
	}
	if GitCommit == "" {
		t.Error("expected GitCommit to be non-empty by default")
	}
	if BuildDate == "" {
		t.Error("expected BuildDate to be non-empty by default")
	}
}

func TestVersionInfo(t *testing.T) {
	origVersion, origCommit, origDate := Version, GitCommit, BuildDate
	defer func() {
		Version, GitCommit, BuildDate = origVersion, origCommit, origDate
	}()

	Version = "v1.2.3"
	GitCommit = "abcdef1"
	BuildDate = "2026-09-20T10:00:00Z"

	info := Info()
	if !strings.Contains(info, "v1.2.3") {
		t.Errorf("expected info to contain version v1.2.3, got %s", info)
	}
	if !strings.Contains(info, "abcdef1") {
		t.Errorf("expected info to contain commit abcdef1, got %s", info)
	}
	if !strings.Contains(info, "2026-09-20T10:00:00Z") {
		t.Errorf("expected info to contain build date 2026-09-20T10:00:00Z, got %s", info)
	}

	if Get() != "v1.2.3" {
		t.Errorf("expected Get() to return v1.2.3, got %s", Get())
	}
}

func TestPrint(t *testing.T) {
	origVersion, origCommit, origDate := Version, GitCommit, BuildDate
	defer func() {
		Version, GitCommit, BuildDate = origVersion, origCommit, origDate
	}()

	Version = "v1.2.3"
	GitCommit = "abcdef1"
	BuildDate = "2026-09-20T10:00:00Z"

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	os.Stdout = w

	Print("test-app")

	_ = w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	if !strings.Contains(output, "test-app") {
		t.Errorf("expected output to contain 'test-app', got %s", output)
	}
	if !strings.Contains(output, "Version:    v1.2.3") {
		t.Errorf("expected output to contain 'Version:    v1.2.3', got %s", output)
	}
	if !strings.Contains(output, "Git Commit: abcdef1") {
		t.Errorf("expected output to contain 'Git Commit: abcdef1', got %s", output)
	}
	if !strings.Contains(output, "Build Date: 2026-09-20T10:00:00Z") {
		t.Errorf("expected output to contain 'Build Date: 2026-09-20T10:00:00Z', got %s", output)
	}
}
