package live

import (
	"regexp"
	"strings"
	"testing"
)

// fixtureChart mirrors charts/istio/Chart.yaml's shape: an umbrella `version:`,
// a top-level quoted `appVersion:`, and four identically-versioned dependency
// blocks. The tests assert each bump edits ONLY the intended line(s), which is
// the whole point of the scoped regexes in trigger_gitbump.go.
const fixtureChart = `apiVersion: v2
name: istio
description: Istio ambient mesh umbrella
type: application
version: 1.0.0
appVersion: "1.29.2"

dependencies:
  - name: base
    version: 1.29.2
    repository: https://istio-release.storage.googleapis.com/charts
  - name: cni
    version: 1.29.2
    repository: https://istio-release.storage.googleapis.com/charts
  - name: istiod
    version: 1.29.2
    repository: https://istio-release.storage.googleapis.com/charts
  - name: ztunnel
    version: 1.29.2
    repository: https://istio-release.storage.googleapis.com/charts
`

// depVersionOf extracts the `version:` under a named dependency block.
func depVersionOf(t *testing.T, chart, dep string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)- name: ` + regexp.QuoteMeta(dep) + `\n\s+version:\s*(\S+)`)
	m := re.FindStringSubmatch(chart)
	if m == nil {
		t.Fatalf("could not find version under `- name: %s`", dep)
	}
	return m[1]
}

// appVersionOf extracts the top-level appVersion value (quotes stripped).
func appVersionOf(t *testing.T, chart string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^appVersion:\s*"?([^"\s]+)"?\s*$`)
	m := re.FindStringSubmatch(chart)
	if m == nil {
		t.Fatalf("could not find appVersion")
	}
	return m[1]
}

// umbrellaVersionOf extracts the top-level umbrella version via the same regex
// the production code uses.
func umbrellaVersionOf(t *testing.T, chart string) string {
	t.Helper()
	v, err := chartVersion(chart)
	if err != nil {
		t.Fatalf("chartVersion: %v", err)
	}
	return v
}

// assertUntouchedExcept checks that every dep NOT in changed is still at want,
// and every dep IN changed is at its mapped value.
func assertDeps(t *testing.T, chart string, want map[string]string) {
	t.Helper()
	for dep, v := range want {
		if got := depVersionOf(t, chart, dep); got != v {
			t.Errorf("dep %s version = %q, want %q", dep, got, v)
		}
	}
}

// (1-4) Each dep-block rewrite changes ONLY that block; siblings, appVersion and
// the umbrella version are untouched.
func TestBumpDepVersion_ScopedPerDep(t *testing.T) {
	for _, dep := range istioDeps {
		t.Run(dep, func(t *testing.T) {
			out, err := bumpDepVersion(fixtureChart, dep, "1.29.2", "1.30.0")
			if err != nil {
				t.Fatalf("bumpDepVersion(%s): %v", dep, err)
			}
			want := map[string]string{"base": "1.29.2", "cni": "1.29.2", "istiod": "1.29.2", "ztunnel": "1.29.2"}
			want[dep] = "1.30.0"
			assertDeps(t, out, want)
			if got := appVersionOf(t, out); got != "1.29.2" {
				t.Errorf("appVersion changed to %q bumping dep %s (must be untouched)", got, dep)
			}
			if got := umbrellaVersionOf(t, out); got != "1.0.0" {
				t.Errorf("umbrella version changed to %q bumping dep %s (must be untouched)", got, dep)
			}
		})
	}
}

// (5) Each dep's no-op guard fires with a dep-named error.
func TestBumpDepVersion_NoOpGuardNamesDep(t *testing.T) {
	for _, dep := range istioDeps {
		t.Run(dep, func(t *testing.T) {
			_, err := bumpDepVersion(fixtureChart, dep, "9.9.9", "1.30.0")
			if err == nil {
				t.Fatalf("expected a no-op error for dep %s with a non-matching from", dep)
			}
			if !strings.Contains(err.Error(), dep) {
				t.Errorf("no-op error %q does not name the dep %q", err.Error(), dep)
			}
		})
	}
}

// (6) appVersion bump moves only appVersion (quotes preserved, deps + umbrella
// untouched) and its no-op guard fires.
func TestBumpAppVersion(t *testing.T) {
	out, err := bumpAppVersion(fixtureChart, "1.29.2", "1.30.0")
	if err != nil {
		t.Fatalf("bumpAppVersion: %v", err)
	}
	if got := appVersionOf(t, out); got != "1.30.0" {
		t.Errorf("appVersion = %q, want 1.30.0", got)
	}
	if !strings.Contains(out, `appVersion: "1.30.0"`) {
		t.Errorf("appVersion quoting not preserved; got:\n%s", out)
	}
	assertDeps(t, out, map[string]string{"base": "1.29.2", "cni": "1.29.2", "istiod": "1.29.2", "ztunnel": "1.29.2"})
	if got := umbrellaVersionOf(t, out); got != "1.0.0" {
		t.Errorf("umbrella version changed to %q bumping appVersion (must be untouched)", got)
	}

	if _, err := bumpAppVersion(fixtureChart, "9.9.9", "1.30.0"); err == nil {
		t.Fatalf("expected a no-op error for a non-matching appVersion from")
	}
}

// (7) An umbrella `version:` bump touches neither appVersion nor any dep version
// (no cross-contamination among the three version-ish lines). Exercised through
// the same chartVersionRe the production umbrella replace uses.
func TestUmbrellaVersionBump_NoCrossContamination(t *testing.T) {
	out := chartVersionRe.ReplaceAllString(fixtureChart, "version: 1.0.1")
	if got := umbrellaVersionOf(t, out); got != "1.0.1" {
		t.Fatalf("umbrella version = %q, want 1.0.1", got)
	}
	if got := appVersionOf(t, out); got != "1.29.2" {
		t.Errorf("appVersion changed to %q on an umbrella bump (must be untouched)", got)
	}
	assertDeps(t, out, map[string]string{"base": "1.29.2", "cni": "1.29.2", "istiod": "1.29.2", "ztunnel": "1.29.2"})
}

// (8) NextChartVersion arithmetic: patch/minor increments, mandatory -devN
// prerelease, the <2.0.0 bound, and monotonic/invalid rejection.
func TestNextChartVersion(t *testing.T) {
	cases := []struct {
		name    string
		current string
		hop     string
		runTag  string
		want    string
		wantErr bool
	}{
		{"patch increments patch", "1.0.0", "patch", "7", "1.0.1-dev7", false},
		{"minor increments minor and zeros patch", "1.0.5", "minor", "9", "1.1.0-dev9", false},
		{"patch off a prerelease keeps monotonic", "1.0.1-dev1", "patch", "2", "1.0.2-dev2", false},
		{"minor near the ceiling stays <2.0.0", "1.9.3", "minor", "3", "1.10.0-dev3", false},
		{"patch that would reach major 2 is rejected", "2.0.0", "patch", "3", "", true},
		{"minor that would reach major 2 is rejected", "2.3.0", "minor", "3", "", true},
		{"unknown hop is rejected", "1.0.0", "bogus", "3", "", true},
		{"missing runTag is rejected", "1.0.0", "patch", "", "", true},
		{"malformed current is rejected", "1.0", "patch", "3", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NextChartVersion(tc.current, tc.hop, tc.runTag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("NextChartVersion(%q,%q,%q) = %q, want %q", tc.current, tc.hop, tc.runTag, got, tc.want)
			}
			if !strings.Contains(got, "-dev"+tc.runTag) {
				t.Errorf("result %q missing the mandatory -dev%s prerelease", got, tc.runTag)
			}
		})
	}
}

// (9a) A full minor bumpChart moves all four deps + appVersion + the umbrella
// version together.
func TestBumpChart_MinorMovesEverything(t *testing.T) {
	out, err := bumpChart(fixtureChart, chartBump{
		hop:     "minor",
		depFrom: "1.29.2",
		depTo:   "1.30.0",
		verTo:   "1.1.0-dev42",
	})
	if err != nil {
		t.Fatalf("bumpChart minor: %v", err)
	}
	assertDeps(t, out, map[string]string{"base": "1.30.0", "cni": "1.30.0", "istiod": "1.30.0", "ztunnel": "1.30.0"})
	if got := appVersionOf(t, out); got != "1.30.0" {
		t.Errorf("appVersion = %q, want 1.30.0", got)
	}
	if got := umbrellaVersionOf(t, out); got != "1.1.0-dev42" {
		t.Errorf("umbrella version = %q, want 1.1.0-dev42", got)
	}
}

// (9b) A patch bumpChart moves ONLY ztunnel + the umbrella version;
// base/cni/istiod and appVersion are untouched.
func TestBumpChart_PatchMovesOnlyZtunnel(t *testing.T) {
	out, err := bumpChart(fixtureChart, chartBump{
		hop:    "patch",
		ztFrom: "1.29.2",
		ztTo:   "1.29.5",
		verTo:  "1.0.1-dev42",
	})
	if err != nil {
		t.Fatalf("bumpChart patch: %v", err)
	}
	assertDeps(t, out, map[string]string{"base": "1.29.2", "cni": "1.29.2", "istiod": "1.29.2", "ztunnel": "1.29.5"})
	if got := appVersionOf(t, out); got != "1.29.2" {
		t.Errorf("appVersion changed to %q on a patch hop (must be untouched)", got)
	}
	if got := umbrellaVersionOf(t, out); got != "1.0.1-dev42" {
		t.Errorf("umbrella version = %q, want 1.0.1-dev42", got)
	}
}

// (9c) A patch bumpChart whose ztunnel from does not match still errors (the
// no-op guard survives being routed through bumpChart).
func TestBumpChart_PatchNoOpGuard(t *testing.T) {
	_, err := bumpChart(fixtureChart, chartBump{hop: "patch", ztFrom: "9.9.9", ztTo: "1.29.5", verTo: "1.0.1-dev1"})
	if err == nil {
		t.Fatalf("expected a no-op error when the ztunnel from does not match")
	}
	if !strings.Contains(err.Error(), "ztunnel") {
		t.Errorf("no-op error %q does not name ztunnel", err.Error())
	}
}
