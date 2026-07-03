package live

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// orgLiteral fragment-assembles the forbidden org name so this test source does
// not contain it verbatim.
func orgLiteral() string { return "read" + "yon" }

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// --------------------------------------------------------------- case 13 -----
// Exit-code contract: clean tree => 0, no output lines; a planted secret => 1
// with a "file:line" line printed.
func TestRunScanExitCodeContract(t *testing.T) {
	// clean
	clean := t.TempDir()
	writeFile(t, clean, "ok.txt", "app-a talks to app-b\n")
	var out bytes.Buffer
	code, err := runScan(ScanConfig{RepoRoot: clean, Worktree: true}, &out)
	if err != nil {
		t.Fatalf("clean scan err: %v", err)
	}
	if code != 0 {
		t.Fatalf("clean tree: code %d want 0 (out=%q)", code, out.String())
	}
	if !strings.Contains(out.String(), "PASS") {
		t.Fatalf("clean tree: missing PASS summary: %q", out.String())
	}

	// dirty
	dirty := t.TempDir()
	writeFile(t, dirty, "leak.txt", "target "+orgLiteral()+"\n")
	out.Reset()
	code, err = runScan(ScanConfig{RepoRoot: dirty, Worktree: true}, &out)
	if err != nil {
		t.Fatalf("dirty scan err: %v", err)
	}
	if code != 1 {
		t.Fatalf("dirty tree: code %d want 1", code)
	}
	if !strings.Contains(out.String(), "leak.txt:1:") {
		t.Fatalf("dirty tree: missing file:line report: %q", out.String())
	}
}

// --------------------------------------------------------------- case 12 -----
// Fail closed: when the scan cannot run (nonexistent repo root) RunScan returns
// a NON-ZERO code and an error - never a clean 0.
func TestRunScanFailsClosedOnMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	var out bytes.Buffer
	code, err := runScan(ScanConfig{RepoRoot: missing, Worktree: true}, &out)
	if err == nil {
		t.Fatal("expected an error for a missing root")
	}
	if code == 0 {
		t.Fatalf("fail-closed violated: code 0 for an unrunnable scan (%q)", out.String())
	}
}

// A finding must NEVER coexist with exit 0: across a matrix of trees, code==0
// iff there were zero findings.
func TestRunScanFindingNeverExitsZero(t *testing.T) {
	cases := []struct {
		name    string
		content string
		dirty   bool
	}{
		{"clean", "just app-c and pgx\n", false},
		{"org", "see " + orgLiteral() + "\n", true},
		{"account", "id " + "975707" + "452016" + "\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "f.txt", c.content)
			var out bytes.Buffer
			code, err := runScan(ScanConfig{RepoRoot: dir, Worktree: true}, &out)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if c.dirty && code == 0 {
				t.Fatalf("%s: findings present but exit 0 (%q)", c.name, out.String())
			}
			if !c.dirty && code != 0 {
				t.Fatalf("%s: clean but exit %d (%q)", c.name, code, out.String())
			}
		})
	}
}
