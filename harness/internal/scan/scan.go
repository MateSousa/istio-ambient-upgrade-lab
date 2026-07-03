// Package scan is the hygiene scanner: it walks the lab repo tree and reports
// any occurrence of a proprietary fingerprint (private org name, AWS account
// IDs, internal registry hosts, internal FQDNs, IRSA ARN shapes) so nothing
// proprietary leaks into this public personal repo. It is the deep, pure
// module of slice 9; all process/IO wiring (flags, stdout, exit codes) lives
// in internal/live and cmd/harness.
//
// Design boundary: this package imports only the standard library. The forbidden
// literals live in exactly one sibling file (rules.go), fragment-assembled so no
// forbidden string appears verbatim even there; every other file in the module -
// including the tests - is free of them. The core (matchLine, scanReader, Scan)
// is a pure function of its inputs: the file list comes from a pluggable
// FileLister and file contents are read through it, so scan_test.go exercises
// every rule, exclusion and edge case over a t.TempDir and never a real secret.
package scan

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Rule is one named forbidden pattern. Re is the compiled matcher; Desc is an
// abstract, leak-free description (it never spells out the literal it catches).
type Rule struct {
	Name string
	Re   *regexp.Regexp
	Desc string
}

// Finding is one match: which rule fired, in which file (repo-root-relative,
// slash-separated), at which 1-based line and column, and the exact matched
// text. Findings are returned in deterministic order (file, line, col, rule).
type Finding struct {
	Rule  string
	File  string
	Line  int
	Col   int
	Match string
}

// hit is a per-line match before it is anchored to a file/line.
type hit struct {
	Rule  string
	Col   int
	Match string
}

// FileLister enumerates the candidate files under root, returning each as a
// root-relative path. Two are provided: GitTrackedLister (default) and
// WalkLister (forced by --worktree, or fallen back to automatically).
type FileLister func(root string) ([]string, error)

// Config controls a scan. A zero Config is not useful; start from DefaultConfig.
type Config struct {
	Rules        []Rule
	Lister       FileLister
	ExcludeGlobs []string // filepath.Match patterns tested against the base name (e.g. "*.tgz")
	ExcludePaths []string // repo-relative, slash-separated: exact file paths, or dir prefixes ending in "/"
	SkipBinary   bool     // skip files containing a NUL byte (grep -I semantics)
}

// selfRulesPath and selfTestdataPrefix are the ONLY two paths the scanner
// excludes from itself (FIX 1, narrow): the rule table (which necessarily
// encodes the fingerprints) and this module's own test fixtures (which plant
// them on purpose). The exclusion is deliberately NOT a blanket "testdata/"
// anywhere - a real secret dropped into any other testdata dir must still fail.
var (
	selfRulesPath      = path.Join("harness", "internal", "scan", "rules.go")
	selfTestdataPrefix = path.Join("harness", "internal", "scan", "testdata") + "/"
)

// DefaultConfig is the production scan configuration used by `harness scan`.
func DefaultConfig() Config {
	return Config{
		Rules:        DefaultRules(),
		Lister:       GitTrackedLister,
		ExcludeGlobs: []string{"*.tgz"},
		ExcludePaths: []string{selfRulesPath, selfTestdataPrefix},
		SkipBinary:   true,
	}
}

// matchLine returns every rule match on a single line, ordered by column then
// rule name so a line that trips several rules yields a stable sequence.
func matchLine(rules []Rule, line string) []hit {
	var hits []hit
	for _, r := range rules {
		for _, loc := range r.Re.FindAllStringIndex(line, -1) {
			hits = append(hits, hit{Rule: r.Name, Col: loc[0] + 1, Match: line[loc[0]:loc[1]]})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Col != hits[j].Col {
			return hits[i].Col < hits[j].Col
		}
		return hits[i].Rule < hits[j].Rule
	})
	return hits
}

// scanReader applies the rules line by line to r, tagging each hit with name as
// the file field. It is pure: no filesystem, no process, just the reader.
func scanReader(rules []Rule, name string, r io.Reader) ([]Finding, error) {
	var findings []Finding
	sc := bufio.NewScanner(r)
	// Allow long minified lines (default 64KiB token would error on them).
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		for _, h := range matchLine(rules, line) {
			findings = append(findings, Finding{Rule: h.Rule, File: name, Line: lineNo, Col: h.Col, Match: h.Match})
		}
	}
	return findings, sc.Err()
}

// excluded reports whether a repo-relative slash path is excluded by a glob
// (base-name match) or a path rule (exact file, or a "dir/" prefix).
func excluded(rel string, globs, paths []string) bool {
	base := path.Base(rel)
	for _, g := range globs {
		if ok, err := path.Match(g, base); err == nil && ok {
			return true
		}
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(rel, p) {
				return true
			}
			continue
		}
		if rel == p {
			return true
		}
	}
	return false
}

// Scan lists the files under root via cfg.Lister, applies the rules to each
// non-excluded, non-binary file, and returns every finding in deterministic
// order. A nil error with a non-empty slice means "clean tree failed"; a
// non-nil error means the scan could not complete (the caller must treat that
// as a failure, never as a clean pass).
func Scan(root string, cfg Config) ([]Finding, error) {
	lister := cfg.Lister
	if lister == nil {
		lister = GitTrackedLister
	}
	files, err := lister(root)
	if err != nil {
		return nil, err
	}
	var findings []Finding
	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if excluded(rel, cfg.ExcludeGlobs, cfg.ExcludePaths) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		if cfg.SkipBinary && bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		fs, err := scanReader(cfg.Rules, rel, bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", rel, err)
		}
		findings = append(findings, fs...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		if findings[i].Col != findings[j].Col {
			return findings[i].Col < findings[j].Col
		}
		return findings[i].Rule < findings[j].Rule
	})
	return findings, nil
}

// GitTrackedLister lists the files git tracks under root (`git ls-files -z`),
// which is exactly what a push publishes and excludes gitignored build
// artifacts. It is the DEFAULT lister. If git is absent, root is not a git
// repository, or ls-files fails for any reason, it AUTOMATICALLY falls back to
// WalkLister so a tarball checkout or a CI image without git still scans (fail
// safe: scanning a superset is fine; silently scanning nothing is not).
func GitTrackedLister(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		return WalkLister(root)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// WalkLister lists every regular file under root (skipping the .git directory),
// returning root-relative slash paths. It is forced by `--worktree` and is the
// automatic fallback when git is unavailable. Glob/path exclusions are applied
// by Scan over whatever the lister returns, so the two listers are
// interchangeable and the exclude rules behave identically in both modes.
func WalkLister(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
