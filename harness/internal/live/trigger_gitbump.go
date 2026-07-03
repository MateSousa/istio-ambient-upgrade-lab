package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// GitBumpConfig configures the PRIMARY, acceptance-criteria-satisfying trigger:
// a ztunnel version bump expressed as a Git change that ArgoCD then syncs (the
// real "bump and sync" flow), NOT a manual kubectl/helm edit.
type GitBumpConfig struct {
	RepoRoot string // path to the lab repo working copy
	// Hop selects how much of the Chart.yaml moves:
	//   "patch" (default) - bump ONLY the ztunnel dep + the umbrella version
	//                       (a same-minor ztunnel hop, e.g. 1.29.2 -> 1.29.5);
	//   "minor"           - bump ALL FOUR deps (base/cni/istiod/ztunnel) + the
	//                       top-level appVersion + the umbrella version (a
	//                       cross-minor hop, e.g. 1.29.2 -> 1.30.0).
	Hop            string
	ZtunnelFrom    string // patch hop: current ztunnel version, e.g. "1.29.2"
	ZtunnelTo      string // patch hop: target ztunnel version, e.g. "1.29.5"
	VersionFrom    string // minor hop: current istio version for all deps + appVersion
	VersionTo      string // minor hop: target istio version
	ChartVersionTo string // umbrella chart version to publish, e.g. "1.0.1"
}

// GitBumpResult reports what the trigger changed, for Result.trigger.
type GitBumpResult struct {
	Info    model.TriggerInfo
	Prereq  string // non-empty when a prerequisite was missing (=> ERROR)
	Applied bool
}

// RunGitBump performs the version-source-of-truth bump for ztunnel and lets
// ArgoCD deliver it:
//
//  1. bump the ztunnel dependency version AND the umbrella chart version in
//     charts/istio/Chart.yaml,
//  2. helm dependency update (re-vendor the bumped ztunnel subchart),
//  3. scripts/publish-chart.sh (push the new umbrella chart to GHCR),
//  4. bump targetRevision in apps/mesh/mesh.yaml to the new chart version,
//  5. git commit + push -> ArgoCD's floating/pinned source syncs the roll.
//
// [IMAGE-RESOLUTION ASSERTION] This lab deliberately leaves global.hub/global.tag
// UNSET in charts/istio/values.yaml, so each subchart resolves its image from
// its own appVersion. Bumping the ztunnel DEPENDENCY version therefore changes
// the resolved ztunnel image and DOES roll the DaemonSet. If global.tag were
// ever set it would override the subchart tag and the dep bump would no-op (no
// roll) - which the analyzer would surface as ERROR no-rollout-observed (test
// 14). We assert values.yaml has no global.tag before firing, and error out
// loudly if it does, rather than silently producing a no-op run.
//
// Gated on GHCR_TOKEN: without it the chart cannot be published, so the trigger
// returns a Prereq error that the caller maps to ERROR trigger-prereq-missing.
func RunGitBump(ctx context.Context, cfg GitBumpConfig) (GitBumpResult, error) {
	hop := cfg.Hop
	if hop == "" {
		hop = "patch"
	}
	// The from/to reported in the Result: patch tracks the ztunnel dep, minor
	// tracks the shared istio version that moves all four deps + appVersion.
	fromTag, toTag := cfg.ZtunnelFrom, cfg.ZtunnelTo
	if hop == "minor" {
		fromTag, toTag = cfg.VersionFrom, cfg.VersionTo
	}
	info := model.TriggerInfo{
		Kind:           "git-bump",
		ACSatisfying:   true,
		ChartVersionTo: cfg.ChartVersionTo,
		ZtunnelTagFrom: fromTag,
		ZtunnelTagTo:   toTag,
	}
	if hop == "minor" {
		info.Warning = "minor hop (all four deps + appVersion): crosses a minor, touches CRDs, and is governed by the skew rule; recovery is roll-forward, not downgrade"
	}
	if os.Getenv("GHCR_TOKEN") == "" {
		return GitBumpResult{Info: info, Prereq: "GHCR_TOKEN not set (needed to publish the bumped chart)"}, nil
	}

	chartPath := filepath.Join(cfg.RepoRoot, "charts", "istio", "Chart.yaml")
	valuesPath := filepath.Join(cfg.RepoRoot, "charts", "istio", "values.yaml")
	meshPath := filepath.Join(cfg.RepoRoot, "apps", "mesh", "mesh.yaml")

	// Guard: refuse to fire a bump that would no-op because global.tag pins the
	// image and overrides the subchart appVersion.
	if err := assertNoGlobalTag(valuesPath); err != nil {
		return GitBumpResult{Info: info}, err
	}

	chart, err := os.ReadFile(chartPath)
	if err != nil {
		return GitBumpResult{Info: info}, err
	}
	fromVer, err := chartVersion(string(chart))
	if err != nil {
		return GitBumpResult{Info: info}, err
	}
	info.ChartVersionFrom = fromVer

	updated, err := bumpChart(string(chart), chartBump{
		hop:     hop,
		ztFrom:  cfg.ZtunnelFrom,
		ztTo:    cfg.ZtunnelTo,
		depFrom: cfg.VersionFrom,
		depTo:   cfg.VersionTo,
		verTo:   cfg.ChartVersionTo,
	})
	if err != nil {
		return GitBumpResult{Info: info}, err
	}
	if err := os.WriteFile(chartPath, []byte(updated), 0o644); err != nil {
		return GitBumpResult{Info: info}, err
	}

	// Re-vendor the bumped ztunnel subchart, then publish the umbrella chart.
	if err := runCmd(ctx, cfg.RepoRoot, "helm", "dependency", "update", "charts/istio"); err != nil {
		return GitBumpResult{Info: info}, fmt.Errorf("helm dependency update: %w", err)
	}
	if err := runCmd(ctx, cfg.RepoRoot, "scripts/publish-chart.sh"); err != nil {
		return GitBumpResult{Info: info}, fmt.Errorf("publish-chart: %w", err)
	}

	// Bump the mesh Application targetRevision to the new chart version so the
	// (pinned) source moves and ArgoCD pulls the new chart.
	mesh, err := os.ReadFile(meshPath)
	if err != nil {
		return GitBumpResult{Info: info}, err
	}
	newMesh := bumpTargetRevision(string(mesh), cfg.ChartVersionTo)
	if err := os.WriteFile(meshPath, []byte(newMesh), 0o644); err != nil {
		return GitBumpResult{Info: info}, err
	}

	// Commit + push: this is the Git change ArgoCD reconciles.
	if err := runCmd(ctx, cfg.RepoRoot, "git", "add", "charts/istio/Chart.yaml", "apps/mesh/mesh.yaml"); err != nil {
		return GitBumpResult{Info: info}, err
	}
	msg := fmt.Sprintf("chore: %s hop istio %s -> %s (chart %s)", hop, fromTag, toTag, cfg.ChartVersionTo)
	if err := runCmd(ctx, cfg.RepoRoot, "git", "commit", "-m", msg); err != nil {
		return GitBumpResult{Info: info}, err
	}
	if err := runCmd(ctx, cfg.RepoRoot, "git", "push"); err != nil {
		return GitBumpResult{Info: info}, err
	}
	return GitBumpResult{Info: info, Applied: true}, nil
}

var (
	chartVersionRe = regexp.MustCompile(`(?m)^version:\s*(\S+)\s*$`)
	targetRevRe    = regexp.MustCompile(`(?m)^(\s*targetRevision:\s*)\S+\s*$`)
)

func chartVersion(chart string) (string, error) {
	m := chartVersionRe.FindStringSubmatch(chart)
	if m == nil {
		return "", fmt.Errorf("could not find version: in Chart.yaml")
	}
	return m[1], nil
}

// istioDeps is the ordered list of the four umbrella subchart dependencies. A
// minor hop moves all four in lockstep; a patch hop moves only ztunnel.
var istioDeps = []string{"base", "cni", "istiod", "ztunnel"}

// chartBump is the hop-aware argument to bumpChart. For a patch hop only ztFrom/
// ztTo/verTo are read; for a minor hop only depFrom/depTo/verTo are read.
type chartBump struct {
	hop     string // "patch" | "minor"
	ztFrom  string // patch hop: current ztunnel dep version
	ztTo    string // patch hop: target ztunnel dep version
	depFrom string // minor hop: current version for all four deps + appVersion
	depTo   string // minor hop: target version
	verTo   string // umbrella chart version to publish
}

// bumpChart rewrites Chart.yaml for a version hop and is the single place the
// three version-ish lines (umbrella `version:`, `appVersion:`, and the per-dep
// `version:` blocks) are edited.
//
//   - patch hop: bump ONLY the ztunnel dependency + the umbrella version
//     (base/cni/istiod and appVersion are left untouched, preserving the
//     original same-minor ztunnel behaviour);
//   - minor hop: bump ALL FOUR dep versions + the appVersion + the umbrella
//     version.
//
// Every dep/appVersion substitution is no-op-guarded (via bumpDepVersion/
// bumpAppVersion) and names the specific line it could not find: a silent no-op
// would otherwise publish a chart whose deps DID NOT actually move, wasting a
// cycle that only surfaces later as a no-rollout ERROR.
func bumpChart(chart string, b chartBump) (string, error) {
	// umbrella version first: only the top-level `version:` line (anchored at
	// column 0, so the indented per-dep `version:` lines and the `appVersion:`
	// line are never matched by this replace).
	out := chartVersionRe.ReplaceAllString(chart, "version: "+b.verTo)

	switch b.hop {
	case "minor":
		for _, dep := range istioDeps {
			var err error
			if out, err = bumpDepVersion(out, dep, b.depFrom, b.depTo); err != nil {
				return "", err
			}
		}
		var err error
		if out, err = bumpAppVersion(out, b.depFrom, b.depTo); err != nil {
			return "", err
		}
	default: // patch
		var err error
		if out, err = bumpDepVersion(out, "ztunnel", b.ztFrom, b.ztTo); err != nil {
			return "", err
		}
	}
	return out, nil
}

// bumpDepVersion rewrites the `version: <from>` line inside a single named
// dependency block (`- name: <dep>`) to <to>, scoping the substitution to that
// dep so the identically-versioned sibling deps are never touched. It errors,
// naming the specific dep, if the substitution would be a no-op.
func bumpDepVersion(chart, dep, from, to string) (string, error) {
	re := regexp.MustCompile(`(?m)(- name: ` + regexp.QuoteMeta(dep) + `\n\s+version:\s*)` + regexp.QuoteMeta(from))
	out := re.ReplaceAllString(chart, "${1}"+to)
	if out == chart {
		return "", fmt.Errorf("dependency %q version %q not found under `- name: %s` in Chart.yaml; substitution would no-op and publish a chart with an UNCHANGED %s dep", dep, from, dep, dep)
	}
	return out, nil
}

// bumpAppVersion rewrites the top-level `appVersion: <from>` line to <to>,
// preserving the original quoting, and errors if the substitution is a no-op.
func bumpAppVersion(chart, from, to string) (string, error) {
	re := regexp.MustCompile(`(?m)^(appVersion:\s*"?)` + regexp.QuoteMeta(from) + `("?)(\s*)$`)
	out := re.ReplaceAllString(chart, "${1}"+to+"${2}${3}")
	if out == chart {
		return "", fmt.Errorf("appVersion %q not found in Chart.yaml; substitution would no-op", from)
	}
	return out, nil
}

// NextChartVersion is the single authority for the fresh umbrella version a
// scenario publishes. It increments current by the hop (patch or minor), ALWAYS
// appends a `-dev<runTag>` prerelease (so GHCR immutability is satisfied and a
// re-pull is forced even for an otherwise-identical chart), refuses to cross the
// <2.0.0 umbrella bound, and rejects a non-monotonic (equal-or-lower) result.
func NextChartVersion(current, hop, runTag string) (string, error) {
	maj, min, pat, _, err := parseSemver(current)
	if err != nil {
		return "", fmt.Errorf("parse current version %q: %w", current, err)
	}
	switch hop {
	case "patch":
		pat++
	case "minor":
		min++
		pat = 0
	default:
		return "", fmt.Errorf("unknown hop %q (want patch|minor)", hop)
	}
	if maj >= 2 {
		return "", fmt.Errorf("next version %d.%d.%d would break the <2.0.0 umbrella bound", maj, min, pat)
	}
	if runTag == "" {
		return "", fmt.Errorf("runTag is required (fresh versions always carry a -dev<runTag> prerelease)")
	}
	next := fmt.Sprintf("%d.%d.%d-dev%s", maj, min, pat, runTag)
	if !semverLess(current, next) {
		return "", fmt.Errorf("next version %q is not strictly greater than current %q", next, current)
	}
	return next, nil
}

// parseSemver splits a major.minor.patch[-prerelease] string into its parts.
func parseSemver(v string) (maj, min, pat int, pre string, err error) {
	core := v
	if i := strings.IndexByte(core, '-'); i >= 0 {
		pre = core[i+1:]
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, 0, 0, "", fmt.Errorf("not major.minor.patch: %q", v)
	}
	if maj, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, 0, "", fmt.Errorf("bad major in %q: %w", v, err)
	}
	if min, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, 0, "", fmt.Errorf("bad minor in %q: %w", v, err)
	}
	if pat, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, "", fmt.Errorf("bad patch in %q: %w", v, err)
	}
	return maj, min, pat, pre, nil
}

// semverLess reports whether a sorts strictly before b. Cores compare
// numerically; on an equal core a version WITH a prerelease sorts before the
// same version WITHOUT one, and two prereleases compare lexically. Inputs are
// assumed already-validated (NextChartVersion validates before constructing).
func semverLess(a, b string) bool {
	amaj, amin, apat, apre, _ := parseSemver(a)
	bmaj, bmin, bpat, bpre, _ := parseSemver(b)
	switch {
	case amaj != bmaj:
		return amaj < bmaj
	case amin != bmin:
		return amin < bmin
	case apat != bpat:
		return apat < bpat
	case apre == bpre:
		return false
	case apre == "":
		return false // a is a release, b a prerelease of the same core => a is greater
	case bpre == "":
		return true // a is a prerelease, b the release of the same core => a is lesser
	default:
		return apre < bpre
	}
}

func bumpTargetRevision(mesh, to string) string {
	return targetRevRe.ReplaceAllString(mesh, "${1}"+to)
}

// assertNoGlobalTag fails if values.yaml sets global.tag/global.hub, which would
// override the subchart image and make the dep bump a no-op (no roll). It parses
// the YAML and inspects global.tag/global.hub specifically, so an unrelated
// `tag:`/`hub:` under another key does not false-positive and a genuine one
// under a differently-indented global block is not missed.
func assertNoGlobalTag(valuesPath string) error {
	b, err := os.ReadFile(valuesPath)
	if err != nil {
		return err
	}
	var values struct {
		Global struct {
			Tag string `json:"tag"`
			Hub string `json:"hub"`
		} `json:"global"`
	}
	if err := yaml.Unmarshal(b, &values); err != nil {
		return fmt.Errorf("parse %s: %w", valuesPath, err)
	}
	if values.Global.Tag != "" || values.Global.Hub != "" {
		return fmt.Errorf("values.yaml sets global.tag/global.hub (tag=%q hub=%q); a set global.tag/hub overrides the subchart image and no-ops the ztunnel dep bump", values.Global.Tag, values.Global.Hub)
	}
	return nil
}

func runCmd(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr // keep stdout clean for the Result JSON
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}
