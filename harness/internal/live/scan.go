package live

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/scan"
)

// ScanConfig configures the `harness scan` subcommand: which working copy to
// scan and whether to force a filesystem walk instead of the git-tracked listing.
type ScanConfig struct {
	RepoRoot string // path to the lab repo working copy to scan
	Worktree bool   // force WalkLister (scan the working tree, incl. untracked files)
}

// RunScan resolves the repo root, runs the hygiene scan, prints each finding as
// "file:line: [rule] match" plus a PASS/FAIL summary, and returns a process exit
// code:
//
//	0  - clean tree (no findings).
//	1  - one or more proprietary identifiers found (each printed).
//	2  - the scan could not run (repo-root/IO error). FAIL CLOSED: a non-zero
//	     code here still blocks a push; we never return 0 without having scanned.
//
// Exit-code contract invariant: code 0 is returned only when there are ZERO
// findings and no error - a finding never coexists with a clean exit.
func RunScan(cfg ScanConfig) (int, error) {
	return runScan(cfg, os.Stdout)
}

func runScan(cfg ScanConfig, w io.Writer) (int, error) {
	root, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return 2, fmt.Errorf("resolve repo-root %q: %w", cfg.RepoRoot, err)
	}
	sc := scan.DefaultConfig()
	if cfg.Worktree {
		sc.Lister = scan.WalkLister
	}
	findings, err := scan.Scan(root, sc)
	if err != nil {
		return 2, fmt.Errorf("scan %s: %w", root, err)
	}
	for _, f := range findings {
		fmt.Fprintf(w, "%s:%d: [%s] %s\n", f.File, f.Line, f.Rule, f.Match)
	}
	if len(findings) > 0 {
		fmt.Fprintf(w, "FAIL: %d proprietary identifier(s) found. Remove before pushing.\n", len(findings))
		return 1, nil
	}
	fmt.Fprintln(w, "PASS: no proprietary identifiers found.")
	return 0, nil
}
