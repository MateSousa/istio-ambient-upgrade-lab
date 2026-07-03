package live

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/measure"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// MeasureConfig configures the orchestrator: which trigger to fire, where the
// repo lives (for git-bump), the observation deadline, and the output sink.
type MeasureConfig struct {
	TriggerKind    string        // "git-bump" (default) | "rollout-restart"
	RepoRoot       string        // lab repo working copy (git-bump)
	ZtunnelFrom    string        // git-bump: current ztunnel version
	ZtunnelTo      string        // git-bump: target ztunnel version
	ChartVersionTo string        // git-bump: umbrella chart version to publish
	Acknowledged   bool          // rollout-restart escape-hatch acknowledgement
	Deadline       time.Duration // observation window (default 5m)
	ProbeNamespace string        // namespace holding the probe Pods (demo-app)
	OutPath        string        // Result JSON destination ("" or "-" => stdout)
	Grace          time.Duration // ztunnel terminationGracePeriodSeconds (120s)
	RecoveryBound  int           // recovery bound seconds (30)
	JitterEps      float64       // jitter tolerance seconds (2)
	OutClientsPath string        // per-client observations JSON dest ("" => producer disabled)
}

// DefaultMeasureConfig returns the standard measure settings.
func DefaultMeasureConfig() MeasureConfig {
	return MeasureConfig{
		TriggerKind:    "git-bump",
		Deadline:       5 * time.Minute,
		ProbeNamespace: "demo-app",
		Grace:          120 * time.Second,
		RecoveryBound:  30,
		JitterEps:      2,
	}
}

// RunMeasure orchestrates the whole measurement:
//  1. validate trigger prerequisites,
//  2. start the ztunnel roll-window informer,
//  3. wait for the per-node probe Pods to be up (held conns established),
//  4. fire the upgrade trigger,
//  5. observe for the bounded deadline,
//  6. collect ConnEvents from the probe Pod logs,
//  7. call the pure analyzer, print/persist the Result,
//  8. restore any mutated state, and exit 0 on PASS / non-zero on FAIL|ERROR.
//
// It returns the Result and a process exit code.
func RunMeasure(ctx context.Context, cfg MeasureConfig) (model.Result, int, error) {
	cs, dyn, restCfg, err := NewClientsAndConfig()
	if err != nil {
		return model.Result{}, 3, fmt.Errorf("kube clients: %w", err)
	}

	// Anchor log collection to this run so a re-run against the long-lived probe
	// Pods never ingests a PREVIOUS run's ConnEvents (which predate this run's
	// drain and would surface as false died-before-drain ERRORs).
	runStart := time.Now()

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watcher := NewRollWatcher(cfg.Grace)
	go func() { _ = watcher.Start(watchCtx, cs) }()

	// Give probes a moment to establish their held connections before the roll.
	if err := waitProbesReady(ctx, cs, cfg.ProbeNamespace, 60*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "warning: probe readiness wait: %v\n", err)
	}

	trigger, prereq := fireTrigger(ctx, cfg, dyn)

	// Observe for the bounded deadline (or until ctx is done).
	select {
	case <-ctx.Done():
	case <-time.After(cfg.Deadline):
	}

	events, err := collectEvents(ctx, cs, cfg.ProbeNamespace, runStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: collecting probe events: %v\n", err)
	}

	// Optional per-client observer (slice 7): only when --out-clients is set.
	// It observes how app-a/b/c weathered the same roll and writes a side-car
	// PerClientObservations JSON that `harness report --clients` renders. A
	// producer error never fails the run - it is a secondary, non-verdict signal.
	if cfg.OutClientsPath != "" {
		if _, perr := ProduceClientObservations(ctx, cs, restCfg, dyn, watcher.Windows(), cfg); perr != nil {
			fmt.Fprintf(os.Stderr, "warning: per-client observations: %v\n", perr)
		}
	}

	acfg := model.DefaultConfig()
	acfg.RecoveryBoundSeconds = cfg.RecoveryBound
	acfg.GraceSeconds = int(cfg.Grace.Seconds())
	acfg.JitterToleranceSeconds = cfg.JitterEps
	acfg.TriggerFired = prereq == ""
	acfg.TriggerPrereqError = prereq

	res := measure.Analyze(model.Input{
		Trigger: trigger,
		Events:  events,
		Windows: watcher.Windows(),
		Config:  acfg,
	})

	if err := writeResult(res, cfg.OutPath); err != nil {
		return res, 3, err
	}

	switch res.Verdict {
	case model.VerdictPass:
		return res, 0, nil
	case model.VerdictFail:
		return res, 1, nil
	default:
		return res, 2, nil
	}
}

func fireTrigger(ctx context.Context, cfg MeasureConfig, dyn dynamic.Interface) (model.TriggerInfo, string) {
	switch cfg.TriggerKind {
	case "rollout-restart":
		info, err := RunRolloutRestart(ctx, dyn, RolloutConfig{Acknowledged: cfg.Acknowledged})
		if err != nil {
			return info, err.Error()
		}
		return info, ""
	default: // git-bump
		out, err := RunGitBump(ctx, GitBumpConfig{
			RepoRoot:       cfg.RepoRoot,
			ZtunnelFrom:    cfg.ZtunnelFrom,
			ZtunnelTo:      cfg.ZtunnelTo,
			ChartVersionTo: cfg.ChartVersionTo,
		})
		if out.Prereq != "" {
			return out.Info, out.Prereq
		}
		if err != nil {
			return out.Info, err.Error()
		}
		return out.Info, ""
	}
}

// probeLoadSelector selects BOTH the per-node probe Pods and the concurrent
// load-generator Pods. Both emit the analyzed JSON-line ConnEvent stream and
// both gate readiness on their held connection(s), so readiness-wait and
// event-collection must cover the union - never just app=probe, or the load
// pods' events (and a crashlooping load pod's absence) would be silently
// ignored. echo Pods (app=echo) are deliberately excluded.
const probeLoadSelector = "app in (probe,load)"

// containerForPod returns the name of the ConnEvent-emitting container in a
// probe-or-load Pod: the load Pods run container "load", the probe Pods run
// container "probe". Keyed off the app label so log collection always reads the
// right container even as both kinds coexist in the namespace.
func containerForPod(p *corev1.Pod) string {
	if p.Labels["app"] == "load" {
		return "load"
	}
	return "probe"
}

// waitProbesReady blocks until all Pods matching probeLoadSelector in ns are
// Ready, or the timeout elapses.
func waitProbesReady(ctx context.Context, cs kubernetes.Interface, ns string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: probeLoadSelector})
		if err == nil && len(pods.Items) > 0 {
			allReady := true
			for i := range pods.Items {
				if !podReady(&pods.Items[i]) {
					allReady = false
					break
				}
			}
			if allReady {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("probe Pods not all Ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// collectEvents reads the logs of every probe-and-load Pod and parses the
// JSON-line ConnEvents they emitted during the observation window. It bounds log
// collection with SinceTime=since so only THIS run's events are ingested; older
// events from a prior run against the same long-lived Pods are skipped. Each
// Pod's log is read from its ConnEvent-emitting container (containerForPod), and
// a per-Pod GetLogs error (e.g. a crashlooping load Pod with no logs yet) is
// logged and skipped so it never drops the OTHER Pods' events - in particular
// never the probe's new-conn-safety signal.
func collectEvents(ctx context.Context, cs kubernetes.Interface, ns string, since time.Time) ([]model.ConnEvent, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: probeLoadSelector})
	if err != nil {
		return nil, err
	}
	sinceTime := metav1.NewTime(since)
	var events []model.ConnEvent
	for i := range pods.Items {
		p := &pods.Items[i]
		stream, err := cs.CoreV1().Pods(ns).GetLogs(p.Name, &corev1.PodLogOptions{Container: containerForPod(p), SinceTime: &sinceTime}).Stream(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: logs %s: %v\n", p.Name, err)
			continue
		}
		evs := parseEvents(stream)
		_ = stream.Close()
		events = append(events, evs...)
	}
	return events, nil
}

func parseEvents(r io.Reader) []model.ConnEvent {
	var out []model.ConnEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev model.ConnEvent
		if err := json.Unmarshal(line, &ev); err == nil && ev.Kind != "" {
			out = append(out, ev)
		}
	}
	return out
}

// writeResult renders the Result as indented JSON to OutPath (stdout for "" or
// "-").
func writeResult(res model.Result, outPath string) error {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if outPath == "" || outPath == "-" {
		_, err = os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(outPath, b, 0o644)
}
