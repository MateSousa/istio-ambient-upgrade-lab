package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/report"
)

// pgbAdminPassword mirrors scripts/verify-data.sh: the pgbouncer admin-console
// password used to run SHOW CLIENTS from the out-of-mesh Postgres pod (which
// ships psql). This is a LAB credential, never a real secret.
const pgbAdminPassword = "pgbouncer_admin_pw"

// clientTarget is one demo client (app-a/b/c) the observer watches: its app
// label, the port its /hold endpoint listens on, and the pool library it uses.
type clientTarget struct {
	name     string // app-a
	library  string // Node/TypeORM
	appLabel string // app label selector value
	holdPort int    // container port serving /hold
	node     string // resolved node the pod runs on
	podName  string // resolved pod name
}

// attributeClientReset reports whether a client reset observed at resetTS is
// UPGRADE-ATTRIBUTABLE to the ztunnel roll captured by window w. It is
// attributable only when resetTS falls within [DrainingAt, GraceExpiresAt]
// widened by eps on each end - the same jitter tolerance the analyzer uses so a
// reset landing a hair outside the boundary is still counted. A reset outside
// that window is Karpenter/consolidation churn, NOT the upgrade, and is excluded
// (the "upgrade-attributable / pre-existing node" qualifier from the AC).
//
// PURE: this is the hermetically tested core of the live producer's attribution.
func attributeClientReset(resetTS time.Time, w model.RollWindow, eps time.Duration) bool {
	if resetTS.Before(w.DrainingAt.Add(-eps)) {
		return false
	}
	if resetTS.After(w.GraceExpiresAt.Add(eps)) {
		return false
	}
	return true
}

// ProduceClientObservations observes how each demo client (app-a Node, app-b
// Python, app-c Go) weathered the ztunnel roll and writes a
// report.PerClientObservations document to cfg.OutClientsPath.
//
// Mechanism (the verify-data.sh SHOW CLIENTS technique, via client-go SPDY
// pod-exec rather than kubectl):
//   - resolve the pgbouncer-writer pod IPs and the app-a/b/c pods (node, name);
//   - snapshot demo_app client connect_times per writer through a pgbouncer
//     admin-console SHOW CLIENTS exec'd into the out-of-mesh Postgres pod;
//   - cross-check each app's /hold endpoint to confirm it currently holds a live
//     pooled connection;
//   - attribute a client reset to a node's roll window ONLY when the reconnect
//     connect_time is attributable to that node's window (attributeClientReset),
//     matching primarily on user=demo_app + connect_time so it stays robust if
//     ambient translates the client source address.
//
// This is a live, best-effort producer (no hermetic test beyond the pure
// attributeClientReset helper): a demo_app connection whose connect_time is
// newer than its node's drain and lands inside the window was reset and
// recovered during the roll; an app whose /hold no longer answers and has no
// demo_app row was reset and never recovered.
func ProduceClientObservations(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, dyn dynamic.Interface, windows []model.RollWindow, cfg MeasureConfig) (*report.PerClientObservations, error) {
	_ = dyn // reserved for future waypoint/telemetry cross-checks
	eps := time.Duration(cfg.JitterEps * float64(time.Second))
	ns := cfg.ProbeNamespace

	pgPod, err := firstPodName(ctx, cs, "demo-data", "app=postgres")
	if err != nil {
		return nil, fmt.Errorf("resolve postgres pod: %w", err)
	}
	writerIPs, err := podIPs(ctx, cs, ns, "app=pgbouncer,pgbouncer-role=writer")
	if err != nil {
		return nil, fmt.Errorf("resolve pgbouncer-writer IPs: %w", err)
	}

	targets := []clientTarget{
		{name: "app-a", library: "Node/TypeORM", appLabel: "app-a", holdPort: 3000},
		{name: "app-b", library: "Python/SQLAlchemy", appLabel: "app-b", holdPort: 8000},
		{name: "app-c", library: "Go/pgx", appLabel: "app-c", holdPort: 8080},
	}
	for i := range targets {
		name, node, err := firstPodNameNode(ctx, cs, ns, "app="+targets[i].appLabel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: resolve %s pod: %v\n", targets[i].name, err)
			continue
		}
		targets[i].podName, targets[i].node = name, node
	}

	// The connect_times demo_app clients currently show across all writers.
	connectTimes := demoClientConnectTimes(ctx, cs, restCfg, pgPod, writerIPs)

	obs := &report.PerClientObservations{SchemaVersion: report.ClientsSchemaVersion}
	for _, t := range targets {
		obs.Clients = append(obs.Clients, observeClient(ctx, cs, restCfg, ns, t, windows, connectTimes, eps))
	}
	sort.SliceStable(obs.Clients, func(i, j int) bool { return obs.Clients[i].Client < obs.Clients[j].Client })

	if cfg.OutClientsPath != "" {
		if err := writeClientObservations(obs, cfg.OutClientsPath); err != nil {
			return obs, err
		}
	}
	return obs, nil
}

// observeClient derives one client's observation from its node's roll window and
// the observed demo_app connect_times, cross-checked with its /hold endpoint.
func observeClient(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, ns string, t clientTarget, windows []model.RollWindow, connectTimes []time.Time, eps time.Duration) report.ClientObservation {
	o := report.ClientObservation{Client: t.name, Library: t.library, Node: t.node}
	if t.podName == "" {
		o.Note = "pod not found"
		return o
	}

	w, ok := windowForNode(windows, t.node)
	if !ok {
		o.Note = "node did not roll (no upgrade-attributable window)"
		return o
	}

	holdOK := hitHold(ctx, cs, restCfg, ns, t.podName, t.holdPort)

	// A reconnect whose connect_time is attributable to this node's window means
	// the held pooled connection was reset and re-established during the roll.
	var reconnect *time.Time
	for i := range connectTimes {
		ct := connectTimes[i]
		if ct.After(w.DrainingAt.Add(-eps)) && attributeClientReset(ct, w, eps) {
			if reconnect == nil || ct.After(*reconnect) {
				c := ct
				reconnect = &c
			}
		}
	}

	switch {
	case reconnect != nil:
		o.Reset = true
		o.DistinctResets = 1
		secs := reconnect.Sub(w.DrainingAt).Seconds()
		if secs < 0 {
			secs = 0
		}
		o.RecoverySeconds = &secs
		o.Note = "held connection reset during the roll and re-established"
	case !holdOK:
		// /hold no longer answers and no attributable reconnect was seen: the
		// connection was reset and never came back.
		o.Reset = true
		o.DistinctResets = 1
		o.Note = "held connection reset during the roll; not recovered"
	default:
		o.Note = "held connection survived the roll"
	}
	return o
}

// ---- kube helpers ----------------------------------------------------------

func firstPodName(ctx context.Context, cs kubernetes.Interface, ns, selector string) (string, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod matches %q in %s", selector, ns)
	}
	return pods.Items[0].Name, nil
}

func firstPodNameNode(ctx context.Context, cs kubernetes.Interface, ns, selector string) (string, string, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", "", err
	}
	if len(pods.Items) == 0 {
		return "", "", fmt.Errorf("no pod matches %q in %s", selector, ns)
	}
	return pods.Items[0].Name, pods.Items[0].Spec.NodeName, nil
}

func podIPs(ctx context.Context, cs kubernetes.Interface, ns, selector string) ([]string, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var ips []string
	for i := range pods.Items {
		if ip := pods.Items[i].Status.PodIP; ip != "" {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

func windowForNode(windows []model.RollWindow, node string) (model.RollWindow, bool) {
	for _, w := range windows {
		if w.Node == node {
			return w, true
		}
	}
	return model.RollWindow{}, false
}

// demoClientConnectTimes runs SHOW CLIENTS against every writer (via psql exec'd
// into the out-of-mesh Postgres pod) and returns the parsed connect_time of
// every demo_app client row. Parse failures are skipped, not fatal.
func demoClientConnectTimes(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, pgPod string, writerIPs []string) []time.Time {
	var out []time.Time
	for _, ip := range writerIPs {
		stdout, _, err := podExec(ctx, cs, restCfg, "demo-data", pgPod, "",
			[]string{"env", "PGPASSWORD=" + pgbAdminPassword, "psql", "-h", ip, "-p", "5432",
				"-U", "pgbouncer", "-d", "pgbouncer", "-tAc", "SHOW CLIENTS;"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: SHOW CLIENTS on writer %s: %v\n", ip, err)
			continue
		}
		out = append(out, parseDemoConnectTimes(stdout)...)
	}
	return out
}

// parseDemoConnectTimes extracts the connect_time of each demo_app row from a
// `psql -tA` (unaligned, tuples-only, '|'-separated) SHOW CLIENTS dump. Columns:
// type|user|database|state|addr|port|local_addr|local_port|connect_time|...
// so user is field 2 and connect_time is field 9 (1-indexed).
func parseDemoConnectTimes(dump string) []time.Time {
	var out []time.Time
	for _, line := range strings.Split(dump, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 9 || fields[1] != "demo_app" {
			continue
		}
		if ts, ok := parsePgTime(fields[8]); ok {
			out = append(out, ts)
		}
	}
	return out
}

// parsePgTime parses a pgbouncer connect_time. pgbouncer prints it in a handful
// of shapes depending on build/timezone; try the common layouts and give up
// gracefully if none match.
func parsePgTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999 MST",
		"2006-01-02 15:04:05",
		time.RFC3339,
	} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
}

// hitHold exec's a wget against the app pod's own /hold endpoint to confirm it
// currently holds a live pooled connection. Best-effort: a non-nil error (e.g.
// the container lacks wget) yields false and is treated as "not confirmed".
func hitHold(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, ns, pod string, port int) bool {
	url := fmt.Sprintf("http://localhost:%d/hold", port)
	stdout, _, err := podExec(ctx, cs, restCfg, ns, pod, "",
		[]string{"wget", "-q", "-O", "-", "-T", "5", url})
	if err != nil {
		return false
	}
	return strings.Contains(stdout, "\"held\":true") || strings.Contains(stdout, "\"held\": true")
}

// podExec opens a SPDY exec stream into a pod container and returns its stdout
// and stderr. It is the client-go analogue of `kubectl exec`.
func podExec(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, ns, pod, container string, command []string) (string, string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

func writeClientObservations(obs *report.PerClientObservations, outPath string) error {
	b, err := json.MarshalIndent(obs, "", "  ")
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
