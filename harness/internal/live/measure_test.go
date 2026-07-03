package live

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func pod(app string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}}}
}

// TestT8_SelectorWideningCoversProbeAndLoad pins the invariant that widening
// event collection to app in (probe,load) actually selects BOTH probe and load
// Pods (so a load Deployment's events are never silently ignored) while still
// excluding the echo Pods. If a crashlooping/absent load Pod were dropped from
// the selector, the harness would silently lose the load signal; if the selector
// were still app=probe only, load events would never be ingested at all.
func TestT8_SelectorWideningCoversProbeAndLoad(t *testing.T) {
	sel, err := labels.Parse(probeLoadSelector)
	if err != nil {
		t.Fatalf("probeLoadSelector %q does not parse: %v", probeLoadSelector, err)
	}
	cases := []struct {
		app  string
		want bool
	}{
		{"probe", true},
		{"load", true},
		{"echo", false},
		{"app-a", false},
	}
	for _, tc := range cases {
		got := sel.Matches(labels.Set{"app": tc.app})
		if got != tc.want {
			t.Fatalf("selector.Matches(app=%s) = %v, want %v", tc.app, got, tc.want)
		}
	}
}

// TestT8_ContainerForPod pins the per-Pod container selection collectEvents uses
// to read the correct log stream: load Pods emit from container "load", probe
// Pods (and any non-load match) from container "probe". Reading the wrong
// container would yield no events and silently drop that Pod's signal.
func TestT8_ContainerForPod(t *testing.T) {
	if got := containerForPod(pod("load")); got != "load" {
		t.Fatalf("containerForPod(load) = %q, want load", got)
	}
	if got := containerForPod(pod("probe")); got != "probe" {
		t.Fatalf("containerForPod(probe) = %q, want probe", got)
	}
	// Defensive default: an unexpected/missing app label falls back to the probe
	// container rather than dropping the Pod.
	if got := containerForPod(pod("")); got != "probe" {
		t.Fatalf("containerForPod(unset) = %q, want probe (default)", got)
	}
}
