package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"sigs.k8s.io/yaml"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// GitBumpConfig configures the PRIMARY, acceptance-criteria-satisfying trigger:
// a ztunnel version bump expressed as a Git change that ArgoCD then syncs (the
// real "bump and sync" flow), NOT a manual kubectl/helm edit.
type GitBumpConfig struct {
	RepoRoot       string // path to the lab repo working copy
	ZtunnelFrom    string // e.g. "1.29.2"
	ZtunnelTo      string // e.g. "1.29.5"
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
	info := model.TriggerInfo{
		Kind:           "git-bump",
		ACSatisfying:   true,
		ChartVersionTo: cfg.ChartVersionTo,
		ZtunnelTagFrom: cfg.ZtunnelFrom,
		ZtunnelTagTo:   cfg.ZtunnelTo,
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

	updated, err := bumpChart(string(chart), cfg.ZtunnelFrom, cfg.ZtunnelTo, fromVer, cfg.ChartVersionTo)
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
	msg := fmt.Sprintf("chore: bump ztunnel %s -> %s (chart %s)", cfg.ZtunnelFrom, cfg.ZtunnelTo, cfg.ChartVersionTo)
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

// bumpChart rewrites the umbrella version and the ztunnel dependency version. It
// errors if the ztunnel-dependency substitution is a no-op (e.g. a reformatted
// Chart.yaml the regex no longer matches): otherwise the umbrella version would
// still bump and a chart with an UNCHANGED ztunnel dep would be published,
// wasting a cycle that only surfaces later as a no-rollout ERROR.
func bumpChart(chart, ztFrom, ztTo, verFrom, verTo string) (string, error) {
	// umbrella version: only the top-level `version:` line.
	out := chartVersionRe.ReplaceAllString(chart, "version: "+verTo)
	// ztunnel dependency version: the `version: <ztFrom>` under the ztunnel dep.
	// The subchart deps are all pinned to the same version, so scope the replace
	// to the ztunnel block by matching the name line first.
	ztBlock := regexp.MustCompile(`(?m)(- name: ztunnel\n\s+version:\s*)` + regexp.QuoteMeta(ztFrom))
	bumped := ztBlock.ReplaceAllString(out, "${1}"+ztTo)
	if bumped == out {
		return "", fmt.Errorf("ztunnel dependency version %q not found under `- name: ztunnel` in Chart.yaml; substitution would no-op and publish a chart with an UNCHANGED ztunnel dep", ztFrom)
	}
	_ = verFrom
	return bumped, nil
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
