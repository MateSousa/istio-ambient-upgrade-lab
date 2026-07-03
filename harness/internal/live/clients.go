package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"

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
//     admin-console SHOW CLIENTS exec'd into the out-of-mesh Postgres pod - the
//     PRIMARY per-client signal (all three apps are demo_app clients);
//   - CORROBORATE with each app's /hold endpoint, reached by port-forwarding to
//     the pod's HTTP port through the API server (kubelet SPDY tunnel) and GETing
//     /hold from the harness. Port-forward needs NO binary inside the container,
//     so it works uniformly for app-a (node), app-b (python) and app-c
//     (distroless) - unlike the old in-pod `wget`, which existed only in app-a;
//   - attribute a client reset to a node's roll window ONLY when a demo_app
//     connect_time is attributable to that node's window (attributeClientReset),
//     matching primarily on user=demo_app + connect_time so it stays robust if
//     ambient translates the client source address.
//
// This is a live, best-effort producer (hermetically tested only through the
// pure attributeClientReset and clientDecision helpers). A demo_app connection
// whose connect_time lands inside the window was reset and recovered during the
// roll. Critically, a reset is claimed ONLY on that positive pgbouncer signal:
// an unavailable or negative /hold check is corroboration and can NEVER by
// itself fabricate a reset (a missing signal is reported as "not observed" /
// "unavailable", never a confident reset). Because all three apps share the
// demo_app role and app-b/app-c may co-locate on one node (soft anti-affinity),
// when two target pods resolve to the SAME node their windows and connect_time
// list are indistinguishable, so an explicit co-location caveat is attached to
// each affected client's Note rather than silently misattributing.
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

	// Which resolved target pods share a node: app-b/app-c have only SOFT
	// anti-affinity, so two of them can land on the same node. When they do,
	// windowForNode returns the same window and the flat demo_app connect_time
	// list is shared, so per-client attribution is ambiguous - flag it rather
	// than confidently attributing one client's reset to a co-located sibling.
	nodeTargets := map[string][]string{}
	for _, t := range targets {
		if t.podName != "" && t.node != "" {
			nodeTargets[t.node] = append(nodeTargets[t.node], t.name)
		}
	}

	obs := &report.PerClientObservations{SchemaVersion: report.ClientsSchemaVersion}
	for _, t := range targets {
		obs.Clients = append(obs.Clients, observeClient(ctx, cs, restCfg, ns, t, windows, connectTimes, eps, coLocationCaveat(t, nodeTargets)))
	}
	sort.SliceStable(obs.Clients, func(i, j int) bool { return obs.Clients[i].Client < obs.Clients[j].Client })

	if cfg.OutClientsPath != "" {
		if err := writeClientObservations(obs, cfg.OutClientsPath); err != nil {
			return obs, err
		}
	}
	return obs, nil
}

// holdResult is the outcome of the /hold corroboration check. It is deliberately
// three-valued: an UNAVAILABLE check (port-forward or GET failed) is distinct
// from a pod that answered "not held", and neither may be turned into a reset.
type holdResult int

const (
	holdUnavailable holdResult = iota // check could not be performed / errored
	holdHeld                          // /hold answered held:true
	holdNotHeld                       // /hold answered but not held:true
)

// observeClient derives one client's observation from its node's roll window and
// the observed demo_app connect_times, corroborated with its /hold endpoint. The
// actual reset/recovery/note decision is delegated to the pure clientDecision so
// the "check-unavailable must NOT become a reset" rule is unit-tested.
func observeClient(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, ns string, t clientTarget, windows []model.RollWindow, connectTimes []time.Time, eps time.Duration, coLocatedNote string) report.ClientObservation {
	o := report.ClientObservation{Client: t.name, Library: t.library, Node: t.node}
	if t.podName == "" {
		o.Note = "pod not found"
		return o
	}

	w, ok := windowForNode(windows, t.node)
	if !ok {
		o.Note = withCoLoc("node did not roll (no upgrade-attributable window)", coLocatedNote)
		return o
	}

	hold := hitHold(ctx, cs, restCfg, ns, t.podName, t.holdPort)

	// The PRIMARY per-client signal: a demo_app connect_time attributable to this
	// node's window means the held pooled connection was reset and re-established
	// during the roll. Newest attributable connect_time wins.
	var reconnect *time.Time
	for i := range connectTimes {
		ct := connectTimes[i]
		if attributeClientReset(ct, w, eps) {
			if reconnect == nil || ct.After(*reconnect) {
				c := ct
				reconnect = &c
			}
		}
	}
	var recovery *float64
	if reconnect != nil {
		secs := reconnect.Sub(w.DrainingAt).Seconds()
		if secs < 0 {
			secs = 0
		}
		recovery = &secs
	}

	o.Reset, o.DistinctResets, o.RecoverySeconds, o.Note = clientDecision(recovery, hold, coLocatedNote)
	return o
}

// clientDecision is the pure, hermetically-tested core of observeClient. Given
// the recovery latency of a POSITIVE pgbouncer reconnect signal (nil => no such
// signal), the /hold corroboration result, and any co-location caveat, it
// decides the reset/recovery fields and the note.
//
// INVARIANT (the fix for the fabricated-reset bug): a reset is reported ONLY
// when there is a positive pgbouncer signal (recovery != nil). An unavailable or
// negative /hold check is corroboration only and can NEVER by itself produce
// Reset:true - a missing signal becomes "not observed" / "unavailable", never a
// confident reset claim.
func clientDecision(recovery *float64, hold holdResult, coLocatedNote string) (bool, int, *float64, string) {
	if recovery != nil {
		note := "held connection reset during the roll and re-established (pgbouncer connect_time advanced within the node window)"
		switch hold {
		case holdHeld:
			note += "; /hold confirms a live pooled connection"
		case holdNotHeld:
			note += "; /hold reports no live pooled connection"
		case holdUnavailable:
			note += "; hold confirmation unavailable"
		}
		return true, 1, recovery, withCoLoc(note, coLocatedNote)
	}

	// No positive pgbouncer signal: never claim a reset.
	var note string
	switch hold {
	case holdHeld:
		note = "no pgbouncer reset signal in the node window; /hold confirms the held connection survived the roll"
	case holdNotHeld:
		note = "no pgbouncer reset signal in the node window; /hold reports no live pooled connection (reset not observed via pgbouncer - not attributed)"
	default: // holdUnavailable
		note = "no pgbouncer reset signal in the node window; hold confirmation unavailable"
	}
	return false, 0, nil, withCoLoc(note, coLocatedNote)
}

// coLocationCaveat returns a per-client caveat when another target app pod
// resolved to the SAME node as t (so windowForNode and the flat demo_app
// connect_time list cannot tell them apart), or "" when t is alone on its node.
func coLocationCaveat(t clientTarget, nodeTargets map[string][]string) string {
	if t.node == "" {
		return ""
	}
	var others []string
	for _, name := range nodeTargets[t.node] {
		if name != t.name {
			others = append(others, name)
		}
	}
	if len(others) == 0 {
		return ""
	}
	return fmt.Sprintf("co-located with %s on %s; per-client attribution ambiguous", strings.Join(others, ", "), t.node)
}

// withCoLoc appends a co-location caveat to a note when one applies.
func withCoLoc(note, coLoc string) string {
	if coLoc == "" {
		return note
	}
	return note + " (" + coLoc + ")"
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

// hitHold corroborates whether the app pod currently holds a live pooled
// connection by GETing its /hold endpoint through a port-forward. This is
// runtime-agnostic: it tunnels via the API server (kubelet SPDY) and needs NO
// binary inside the container, so it works for app-a (node), app-b (python) and
// app-c (distroless) alike - the old in-pod `wget` existed only in app-a and
// silently failed (holdUnavailable) for the other two. A failed forward/GET
// returns holdUnavailable, NOT a reset - callers must never turn it into one.
func hitHold(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, ns, pod string, port int) holdResult {
	body, err := httpGetViaPortForward(ctx, cs, restCfg, ns, pod, port, "/hold")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: /hold via port-forward for %s: %v\n", pod, err)
		return holdUnavailable
	}
	if strings.Contains(body, "\"held\":true") || strings.Contains(body, "\"held\": true") {
		return holdHeld
	}
	return holdNotHeld
}

// httpGetViaPortForward port-forwards to the pod's HTTP port through the API
// server (the same SPDY tunnel `kubectl port-forward` uses) and issues an HTTP
// GET for path against the pod's own loopback via an ephemeral local port. It
// requires no in-pod binary, so it is uniform across every container image.
func httpGetViaPortForward(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, ns, pod string, port int, path string) (string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(pod).SubResource("portforward")
	roundTripper, upgrader, err := spdy.RoundTripperFor(restCfg)
	if err != nil {
		return "", err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, "POST", req.URL())

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	defer close(stopCh)

	// Local port 0 => the OS picks a free ephemeral port; GetPorts reports it.
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", port)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return "", err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-errCh:
		return "", fmt.Errorf("port-forward: %w", err)
	case <-readyCh:
	}

	ports, err := fw.GetPorts()
	if err != nil {
		return "", fmt.Errorf("port-forward local port: %w", err)
	}
	if len(ports) == 0 {
		return "", fmt.Errorf("port-forward reported no local port")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", ports[0].Local, path)
	hreq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("hold endpoint status %d", resp.StatusCode)
	}
	return string(body), nil
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
