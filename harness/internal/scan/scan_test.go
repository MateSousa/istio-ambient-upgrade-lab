package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// scanDir is the absolute path of this package's source directory
// (harness/internal/scan), resolved from the compiled test's own file so it is
// independent of the working directory `go test` runs in.
func scanDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

// repoRoot is three levels up from the package dir (scan -> internal -> harness
// -> repo root).
func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(scanDir(t), "..", "..", "..")
}

// buildPlantedCorpus materialises the positive fixture corpus into a fresh temp
// dir at TEST TIME. Every fixture is fragment-assembled with frag() (the same
// primitive rules.go uses), so no forbidden literal is ever written to a
// committed file - the corpus exists only on disk under t.TempDir() while the
// test runs. Collectively the files fire every rule in DefaultRules(). The
// example account id 123456789012 is intentionally used for the ecr/irsa shapes
// (it is a shape match, not one of the real account ids), and the IRSA role name
// is generic (no internal codename).
func buildPlantedCorpus(t *testing.T) string {
	t.Helper()
	lower := frag("read", "yon")
	upper := frag("READ", "YON")
	mixed := frag("Ready", "On")
	domain := frag("onr", "eady") + "." + frag("d", "ev")
	ns := frag("opentelemetry-operator", "-system")
	ecrHost := "123456789012" + frag(".dkr", ".ecr", ".us-east-2", ".amazonaws", ".com")
	irsaARN := frag("arn:aws:iam::", "123456789012:") + "role/example-irsa-external-secrets"

	return writeTree(t, map[string]string{
		"org.txt": "deploy target: " + lower + " primary cluster\n" +
			"mixed case must also trip: " + upper + " and " + mixed + "\n",
		"domain.txt": "ArgoCD lives at argocd." + domain + " behind the platform account.\n",
		"account.txt": "prod account: " + frag("975707", "452016") + "\n" +
			"nonprod account: " + frag("116153", "546408") + "\n" +
			"platform account: " + frag("951113", "916427") + "\n" +
			"management account: " + frag("835975", "842700") + "\n",
		"ecr.txt":  "image: " + ecrHost + "/mesh/ztunnel:1.29.2\n",
		"irsa.txt": "roleArn: " + irsaARN + "\n",
		"otel.txt": "endpoint: http://otel-collector." + ns + "." + frag("svc", ".cluster.local") + ":4317\n" +
			"bare namespace reference: " + ns + "\n",
	})
}

func hasGit() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// writeTree materialises files (relative path -> content) under a fresh temp dir
// and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// ruleNames returns the sorted set of rule names present in findings.
func ruleNames(findings []Finding) []string {
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.Rule] = true
	}
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func defaultRuleNames() []string {
	var out []string
	for _, r := range DefaultRules() {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// allRulesConfig scans with the full rule set, WalkLister, and no exclusions.
func allRulesConfig() Config {
	return Config{Rules: DefaultRules(), Lister: WalkLister, SkipBinary: true}
}

// assertMatchAnchored verifies every finding actually sits at the reported
// line/column of its file - i.e. rule/file/line/col/match are internally
// consistent - without the test source ever naming a forbidden literal.
func assertMatchAnchored(t *testing.T, root string, findings []Finding) {
	t.Helper()
	for _, f := range findings {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.File)))
		if err != nil {
			t.Fatalf("read %s: %v", f.File, err)
		}
		lines := strings.Split(string(data), "\n")
		if f.Line < 1 || f.Line > len(lines) {
			t.Fatalf("%s: line %d out of range (%d lines)", f.File, f.Line, len(lines))
		}
		line := lines[f.Line-1]
		start := f.Col - 1
		if start < 0 || start+len(f.Match) > len(line) {
			t.Fatalf("%s:%d col %d + match %q overflows line %q", f.File, f.Line, f.Col, f.Match, line)
		}
		if got := line[start : start+len(f.Match)]; got != f.Match {
			t.Fatalf("%s:%d:%d reported match %q but line has %q there", f.File, f.Line, f.Col, f.Match, got)
		}
	}
}

// ---------------------------------------------------------------- case 1 -----
// Every rule matches its positive planted fixture, and every finding's
// rule/file/line/col/match is internally consistent (also covers case 6).
func TestEachRuleMatchesPlantedFixture(t *testing.T) {
	root := buildPlantedCorpus(t)
	findings, err := Scan(root, allRulesConfig())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("no findings over the planted fixtures")
	}
	assertMatchAnchored(t, root, findings)

	got := ruleNames(findings)
	want := defaultRuleNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted fixtures fired rules %v; want every rule %v", got, want)
	}
}

// ---------------------------------------------------------------- case 2 -----
func TestCleanTreeIsZero(t *testing.T) {
	root := writeTree(t, map[string]string{
		"README.md":         "a perfectly ordinary lab with app-a, app-b, app-c\n",
		"deploy/values.txt": "registry: ghcr.io/example/mesh\naccount: 123456789012\n",
	})
	findings, err := Scan(root, allRulesConfig())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("clean tree produced findings: %v", findings)
	}
}

// ---------------------------------------------------------------- case 3 -----
// The org-name rule is case-insensitive. Variants are fragment-assembled so the
// forbidden literal never appears verbatim in this test source.
func TestOrgNameCaseInsensitive(t *testing.T) {
	upper := frag("READ", "YON")
	mixed := frag("Ready", "On")
	root := writeTree(t, map[string]string{
		"a.txt": upper + " here\nand " + mixed + " there\n",
	})
	findings, err := Scan(root, allRulesConfig())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var n int
	for _, f := range findings {
		if f.Rule == "org-name" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("case-insensitive org-name: got %d hits, want 2 (%v)", n, findings)
	}
}

// ---------------------------------------------------------------- case 4 -----
// The account-id rule matches the EXACT ids only: a benign 12-digit number does
// not trip it, while a real id (fragment-assembled) does.
func TestAccountIDExactMatchNotBareDigits(t *testing.T) {
	real := frag("975707", "452016")
	root := writeTree(t, map[string]string{
		"fp.txt":  "unrelated 12-digit token 123456789012 must not match\n",
		"hit.txt": "id " + real + "\n",
	})
	findings, err := Scan(root, allRulesConfig())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var hitFiles []string
	for _, f := range findings {
		if f.Rule == "account-id" {
			hitFiles = append(hitFiles, f.File)
		}
	}
	if len(hitFiles) != 1 || hitFiles[0] != "hit.txt" {
		t.Fatalf("account-id fired on %v; want only hit.txt", hitFiles)
	}
}

// ---------------------------------------------------------------- case 5 -----
// ecr-host matches the FULL shape only. Benign prose and a bare "dkr.ecr"
// fragment (the dropped loose fallback) must NOT match.
func TestECRFullShapeOnly(t *testing.T) {
	full := "123456789012" + frag(".dkr", ".ecr", ".eu-west-1", ".amazonaws", ".com") + "/mesh/app:1.0"
	bare := frag("dkr", ".ecr") + " is just a service abbreviation here"
	root := writeTree(t, map[string]string{
		"full.txt":  "image: " + full + "\n",
		"prose.txt": "we push images to our container registry nightly\n",
		"bare.txt":  bare + "\n",
	})
	findings, err := Scan(root, allRulesConfig())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var ecr []Finding
	for _, f := range findings {
		if f.Rule == "ecr-host" {
			ecr = append(ecr, f)
		}
	}
	if len(ecr) != 1 || ecr[0].File != "full.txt" {
		t.Fatalf("ecr-host fired %v; want exactly one on full.txt", ecr)
	}
}

// ---------------------------------------------------------------- case 6 -----
// Explicit line/col reporting on a multi-line file.
func TestLineAndColReporting(t *testing.T) {
	org := frag("read", "yon")
	root := writeTree(t, map[string]string{
		"m.txt": "line one clean\nprefix " + org + " suffix\n",
	})
	findings, err := Scan(root, allRulesConfig())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %v", findings)
	}
	f := findings[0]
	// "prefix " is 7 bytes, so the match starts at column 8 on line 2.
	if f.Line != 2 || f.Col != 8 {
		t.Fatalf("got line %d col %d; want line 2 col 8", f.Line, f.Col)
	}
	assertMatchAnchored(t, root, findings)
}

// ---------------------------------------------------------------- case 7 -----
func TestGlobExclusionSkipsTgz(t *testing.T) {
	org := frag("read", "yon")
	root := writeTree(t, map[string]string{
		"vendor/chart.tgz": "buried secret " + org + "\n",
		"control.txt":      "visible secret " + org + "\n",
	})
	cfg := allRulesConfig()
	cfg.ExcludeGlobs = []string{"*.tgz"}
	findings, err := Scan(root, cfg)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 || findings[0].File != "control.txt" {
		t.Fatalf("glob exclusion: got %v; want one finding on control.txt", findings)
	}
}

// ---------------------------------------------------------------- case 8 -----
func TestBinarySkip(t *testing.T) {
	org := frag("read", "yon")
	root := writeTree(t, map[string]string{
		"blob.bin": "head\x00" + org + "\n", // NUL => binary
		"text.txt": org + "\n",
	})
	cfg := allRulesConfig()
	cfg.SkipBinary = true
	findings, err := Scan(root, cfg)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 || findings[0].File != "text.txt" {
		t.Fatalf("binary skip: got %v; want one finding on text.txt", findings)
	}
}

// ---------------------------------------------------------------- case 9 -----
// The self-reference PAIR. (a) The real repo scans clean under DefaultConfig
// because rules.go AND testdata/ are excluded. (b) The SAME planted fixtures,
// scanned WITHOUT the testdata exclusion, produce every planted finding - so it
// is the exclusion suppressing them, not their absence.
func TestSelfReferencePair(t *testing.T) {
	if hasGit() {
		root := repoRoot(t)
		findings, err := Scan(root, DefaultConfig())
		if err != nil {
			t.Fatalf("self-scan of repo root: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("self-scan of the real repo must be clean; got %v", findings)
		}
	} else {
		t.Log("git unavailable; skipping the real-repo self-scan half")
	}

	planted := buildPlantedCorpus(t)
	cfg := Config{Rules: DefaultRules(), Lister: WalkLister, SkipBinary: true} // no testdata exclusion
	findings, err := Scan(planted, cfg)
	if err != nil {
		t.Fatalf("scan planted without exclusion: %v", err)
	}
	got := ruleNames(findings)
	want := defaultRuleNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("without exclusion the fixtures must fire every rule; got %v want %v", got, want)
	}
}

// TestSelfExclusionPathsMatchInBothModes proves the exclude path forms match
// the paths the listers actually emit - git-tracked and worktree paths are both
// repo-root-relative slash paths, so the exact rules.go path and the testdata/
// prefix suppress the same files in either mode.
func TestSelfExclusionPathsMatchInBothModes(t *testing.T) {
	if !excluded(selfRulesPath, nil, []string{selfRulesPath, selfTestdataPrefix}) {
		t.Fatalf("rules.go path %q not excluded", selfRulesPath)
	}
	testdataFile := selfTestdataPrefix + "planted/org.txt"
	if !excluded(testdataFile, nil, []string{selfRulesPath, selfTestdataPrefix}) {
		t.Fatalf("testdata file %q not excluded", testdataFile)
	}
	// A secret in some OTHER testdata dir must still be caught (narrow scope).
	other := "harness/internal/live/testdata/leak.txt"
	if excluded(other, nil, []string{selfRulesPath, selfTestdataPrefix}) {
		t.Fatalf("exclusion is too broad: %q should not be excluded", other)
	}
}

// --------------------------------------------------------------- case 10 -----
// git-tracked scoping: only tracked files are scanned; an untracked (e.g.
// gitignored) file with the same secret is invisible to the default lister.
func TestGitTrackedScoping(t *testing.T) {
	if !hasGit() {
		t.Skip("git unavailable")
	}
	org := frag("read", "yon")
	dir := t.TempDir()
	mustGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	mustGit("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("secret "+org+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit("add", "tracked.txt")
	mustGit("commit", "-q", "-m", "x")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("secret "+org+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Rules: DefaultRules(), Lister: GitTrackedLister, SkipBinary: true}
	findings, err := Scan(dir, cfg)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 || findings[0].File != "tracked.txt" {
		t.Fatalf("git-tracked scoping: got %v; want only tracked.txt", findings)
	}
}

// --------------------------------------------------------------- case 11 -----
// Auto-worktree fallback: pointed at a non-git directory, GitTrackedLister must
// fall back to WalkLister rather than scanning nothing.
func TestAutoWorktreeFallbackOnNonGitDir(t *testing.T) {
	org := frag("read", "yon")
	dir := writeTree(t, map[string]string{"leak.txt": "secret " + org + "\n"})
	// dir is a fresh temp dir, not a git repository.
	cfg := Config{Rules: DefaultRules(), Lister: GitTrackedLister, SkipBinary: true}
	findings, err := Scan(dir, cfg)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 || findings[0].File != "leak.txt" {
		t.Fatalf("auto fallback: got %v; want one finding on leak.txt", findings)
	}
}

// --------------------------------------------------------------- extra -------
// matchLine orders multiple hits on one line by column deterministically.
func TestMatchLineDeterministicOrder(t *testing.T) {
	org := frag("read", "yon")
	line := org + " and later " + org
	hits := matchLine(DefaultRules(), line)
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	if hits[0].Col >= hits[1].Col {
		t.Fatalf("hits not ordered by column: %v", hits)
	}
}
