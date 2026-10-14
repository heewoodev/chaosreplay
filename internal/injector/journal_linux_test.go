//go:build linux

package injector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"chaosreplay/internal/scenario"
)

func TestJournalRecoveryClearsTerminalProcessFault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active.json")
	b, err := json.Marshal(journal{Active: map[string]scenario.Fault{
		"terminal": {Type: "process_kill", Params: map[string]any{"pid": float64(1234)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLinuxWithState(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal was not cleared: %v", err)
	}
}

func TestResolveCurrentProcessCgroup(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("cgroup v2 unavailable")
	}
	path, err := ResolvePodCgroup(int64(os.Getpid()))
	if err != nil {
		// A process in the cgroup root is intentionally rejected: the root is
		// not a safe Pod target for disk throttling.
		return
	}
	if path == "" || path == "/sys/fs/cgroup" {
		t.Fatalf("unsafe cgroup path: %q", path)
	}
}

func TestDesiredIOPreservesOtherDevices(t *testing.T) {
	old := []byte("8:0 rbps=100\n8:16 wbps=200\n")
	got, err := desiredIOValues(old, 8, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := "8:0 rbps=50\n8:16 wbps=200\n"
	if string(got) != want {
		t.Fatalf("desired io.max=%q, want %q", got, want)
	}
}

func TestRestoreIORefusesExternalChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.max")
	if err := os.WriteFile(path, []byte("external\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreIO(path, []byte("original\n"), []byte("chaosreplay\n")); err == nil {
		t.Fatal("external io.max change was overwritten")
	}
}

func TestDiskIOApplyAndRevertPreservesRules(t *testing.T) {
	dir := t.TempDir()
	ioPath := filepath.Join(dir, "io.max")
	original := []byte("8:0 rbps=100\n8:16 wbps=200\n")
	if err := os.WriteFile(ioPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	l := NewLinux()
	fault := scenario.Fault{Type: "disk_io_limit", Params: map[string]any{
		"cgroupPath":     dir,
		"resolvedTarget": true,
		"major":          8,
		"minor":          0,
		"readBps":        1,
	}}
	if _, err := l.Apply(t.Context(), "disk", fault); err != nil {
		t.Fatal(err)
	}
	applied, err := os.ReadFile(ioPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(applied) != "8:0 rbps=1\n8:16 wbps=200\n" {
		t.Fatalf("applied io.max=%q", applied)
	}
	if err := l.Revert(t.Context(), "disk"); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(ioPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("restored io.max=%q, want %q", restored, original)
	}
}
