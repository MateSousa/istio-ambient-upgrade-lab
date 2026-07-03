package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// RolloutConfig configures the DEV ESCAPE HATCH trigger: a plain
// `kubectl rollout restart`-style bump of the ztunnel DaemonSet. It does NOT
// satisfy the acceptance criteria (the AC requires a Git-synced version bump),
// so it is gated behind an explicit acknowledgement flag and marks the Result.
type RolloutConfig struct {
	Acknowledged bool // must be set via --i-know-this-is-not-ac
}

var meshAppGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// RunRolloutRestart restarts the ztunnel DaemonSet directly, without a Git bump.
//
// Because ArgoCD would otherwise fight the restart (selfHeal reverts the
// restartedAt annotation, and a re-sync could double-roll), it first patches the
// mesh Application's syncPolicy.automated.selfHeal to false for the observation
// window. That patch MUST be restored, so restoration is CRASH-SAFE: a deferred
// restore runs on normal return AND a signal handler restores on SIGINT/SIGTERM.
// If restoration cannot be confirmed the run is forced to ERROR, so we never
// leave the Application in a mutated state that would cause a later double-roll.
func RunRolloutRestart(ctx context.Context, dyn dynamic.Interface, cfg RolloutConfig) (model.TriggerInfo, error) {
	info := model.TriggerInfo{
		Kind:         "rollout-restart",
		ACSatisfying: false,
		Warning:      "rollout-restart does NOT satisfy the acceptance criteria (requires --i-know-this-is-not-ac); the AC needs a Git-synced version bump",
	}
	if !cfg.Acknowledged {
		return info, fmt.Errorf("rollout-restart requires --i-know-this-is-not-ac (it is not the AC-satisfying path)")
	}

	// Snapshot the current selfHeal value so restore returns it verbatim.
	prev, err := getSelfHeal(ctx, dyn)
	if err != nil {
		return info, fmt.Errorf("read mesh Application selfHeal: %w", err)
	}

	restored := false
	restore := func() error {
		if restored {
			return nil
		}
		if err := setSelfHeal(ctx, dyn, prev); err != nil {
			return err
		}
		restored = true
		return nil
	}

	// Signal-safe restore: if we are killed mid-window, put selfHeal back.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = restore()
		os.Exit(2)
	}()
	defer signal.Stop(sigCh)
	defer func() { _ = restore() }()

	if err := setSelfHeal(ctx, dyn, false); err != nil {
		return info, fmt.Errorf("disable selfHeal: %w", err)
	}

	// Restart the ztunnel DaemonSet via a restartedAt annotation patch.
	if err := restartZtunnel(ctx); err != nil {
		return info, fmt.Errorf("restart ztunnel: %w", err)
	}

	// Restore selfHeal now that the roll is under way; confirm it, else ERROR.
	if err := restore(); err != nil {
		info.Warning += "; FAILED to restore selfHeal (manual repair needed)"
		return info, fmt.Errorf("restore selfHeal unconfirmed: %w", err)
	}
	return info, nil
}

func getSelfHeal(ctx context.Context, dyn dynamic.Interface) (bool, error) {
	obj, err := dyn.Resource(meshAppGVR).Namespace("argocd").Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	automated, found, err := unstructuredNestedMap(obj.Object, "spec", "syncPolicy", "automated")
	if err != nil || !found {
		return false, err
	}
	v, _ := automated["selfHeal"].(bool)
	return v, nil
}

func setSelfHeal(ctx context.Context, dyn dynamic.Interface, val bool) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"syncPolicy":{"automated":{"selfHeal":%t}}}}`, val))
	_, err := dyn.Resource(meshAppGVR).Namespace("argocd").Patch(
		ctx, "mesh", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func unstructuredNestedMap(obj map[string]any, fields ...string) (map[string]any, bool, error) {
	cur := obj
	for i, f := range fields {
		v, ok := cur[f]
		if !ok {
			return nil, false, nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("field %q is not a map", f)
		}
		if i == len(fields)-1 {
			return m, true, nil
		}
		cur = m
	}
	return cur, true, nil
}

// restartZtunnel patches the ztunnel DaemonSet's pod template with a restartedAt
// annotation (the same mechanism as `kubectl rollout restart`).
func restartZtunnel(ctx context.Context) error {
	stamp := time.Now().UTC().Format(time.RFC3339)
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"kubectl.kubernetes.io/restartedAt": stamp,
					},
				},
			},
		},
	})
	return runCmd(ctx, ".", "kubectl", "-n", "istio-system", "patch", "daemonset", "ztunnel",
		"--type", "strategic", "-p", string(patch))
}
