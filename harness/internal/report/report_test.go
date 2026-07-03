package report

import (
	"strings"
	"testing"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/measure"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// T0 anchors every synthetic event/window so the tests are clock-free.
var T0 = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time       { return T0.Add(time.Duration(sec) * time.Second) }
func ptr(t time.Time) *time.Time { return &t }

func window(node string, drainSec int, ready *time.Time) model.RollWindow {
	return model.RollWindow{
		Node:           node,
		DrainingAt:     at(drainSec),
		GraceExpiresAt: at(drainSec + 120),
		ReadyAt:        ready,
	}
}

func ev(kind model.ConnEventKind, conn, node string, sec int) model.ConnEvent {
	return model.ConnEvent{Kind: kind, ConnID: conn, Node: node, TS: at(sec)}
}

// analyze runs the REAL analyzer so the report renders genuine Result contracts,
// not hand-built ones. trigger defaults to the AC-satisfying git-bump.
func analyze(windows []model.RollWindow, events []model.ConnEvent) model.Result {
	return analyzeWith(model.TriggerInfo{Kind: "git-bump", ACSatisfying: true}, model.DefaultConfig(), windows, events)
}

func analyzeWith(trigger model.TriggerInfo, cfg model.Config, windows []model.RollWindow, events []model.ConnEvent) model.Result {
	return measure.Analyze(model.Input{Trigger: trigger, Windows: windows, Events: events, Config: cfg})
}

func render(res model.Result, clients *PerClientObservations) string {
	return Render(res, clients, RenderOptions{GeneratedAt: T0})
}

// acCell returns the "Result" cell of the AC-checklist row whose label contains
// labelSubstr. Fails the test if the row is absent.
func acCell(t *testing.T, md, labelSubstr string) string {
	t.Helper()
	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") || !strings.Contains(line, labelSubstr) {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 3 {
			return strings.TrimSpace(parts[2])
		}
	}
	t.Fatalf("AC row %q not found in report:\n%s", labelSubstr, md)
	return ""
}

const disruptionRow = "disruption window per pre-existing node"

// perNodeRow returns the per-node attribution table row for node.
func perNodeRow(t *testing.T, md, node string) string {
	t.Helper()
	inTable := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "## Per-node attribution") {
			inTable = true
			continue
		}
		if inTable && strings.HasPrefix(line, "## ") {
			break
		}
		if inTable && strings.HasPrefix(line, "| "+node+" ") {
			return line
		}
	}
	t.Fatalf("per-node row for %q not found in:\n%s", node, md)
	return ""
}

// ------------------------------------------------------------------ R1 -------
func TestR1_CleanPass(t *testing.T) {
	res := analyze(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115), ev(model.ConnReconnected, "c1", "w1", 118)},
	)
	md := render(res, nil)
	if res.Verdict != model.VerdictPass {
		t.Fatalf("precondition: verdict=%s want PASS", res.Verdict)
	}
	if !strings.Contains(md, "**PASS") {
		t.Fatalf("missing PASS banner:\n%s", md)
	}
	if strings.Contains(md, "N/A") {
		t.Fatalf("clean PASS should have no N/A AC cells:\n%s", md)
	}
	for _, label := range []string{"Zero new-connection failures", disruptionRow, "Auto-recovery"} {
		if got := acCell(t, md, label); got != "PASS" {
			t.Fatalf("AC row %q = %q, want PASS", label, got)
		}
	}
}

// ------------------------------------------------------------------ R2 -------
// Five connections reset together in ONE node window is a PASS: the
// disruption-window AC row is PASS, the footnote is present, and the per-node
// table still descriptively shows DistinctConnsReset=5 - no contradiction.
func TestR2_FiveConnsOneWindow_NoContradiction(t *testing.T) {
	var events []model.ConnEvent
	for _, c := range []string{"c1", "c2", "c3", "c4", "c5"} {
		events = append(events, ev(model.ConnReset, c, "w1", 115), ev(model.ConnReconnected, c, "w1", 118))
	}
	res := analyze([]model.RollWindow{window("w1", 0, ptr(at(-10)))}, events)
	md := render(res, nil)
	if res.Verdict != model.VerdictPass {
		t.Fatalf("precondition: verdict=%s want PASS", res.Verdict)
	}
	if got := acCell(t, md, disruptionRow); got != "PASS" {
		t.Fatalf("disruption-window AC row = %q, want PASS", got)
	}
	if !strings.Contains(md, perNodeFootnote) {
		t.Fatalf("footnote missing:\n%s", md)
	}
	row := perNodeRow(t, md, "w1")
	if !strings.Contains(row, "| 5 |") {
		t.Fatalf("per-node table should show DistinctConnsReset=5, got row: %q", row)
	}
}

// ------------------------------------------------------------------ R3 -------
func TestR3_SameConnTwoWindows_Fail(t *testing.T) {
	res := analyze(
		[]model.RollWindow{window("w1", 0, ptr(at(-10))), window("w2", 120, ptr(at(110)))},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115), ev(model.ConnReconnected, "c1", "w1", 118),
			ev(model.ConnReset, "c1", "w2", 235), ev(model.ConnReconnected, "c1", "w2", 238),
		},
	)
	md := render(res, nil)
	if res.Verdict != model.VerdictFail {
		t.Fatalf("precondition: verdict=%s want FAIL", res.Verdict)
	}
	if got := acCell(t, md, disruptionRow); got != "FAIL" {
		t.Fatalf("disruption-window AC row = %q, want FAIL", got)
	}
	if !strings.Contains(md, "**FAIL") {
		t.Fatalf("missing FAIL banner:\n%s", md)
	}
}

// ------------------------------------------------------------------ R4 -------
func TestR4_Overlapping_Fail(t *testing.T) {
	res := analyze(
		[]model.RollWindow{window("w1", 0, ptr(at(-10))), window("w2", 60, ptr(at(50)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 30), ev(model.ConnReconnected, "c1", "w1", 33)},
	)
	md := render(res, nil)
	if res.Verdict != model.VerdictFail {
		t.Fatalf("precondition: verdict=%s want FAIL", res.Verdict)
	}
	if got := acCell(t, md, disruptionRow); got != "FAIL" {
		t.Fatalf("disruption-window AC row = %q, want FAIL", got)
	}
	if got := acCell(t, md, "Staggered, not mesh-wide"); got != "FAIL" {
		t.Fatalf("mesh-wide AC row = %q, want FAIL", got)
	}
}

// ------------------------------------------------------------------ R5 -------
func TestR5_DropOnNonUpgradingNode_Fail(t *testing.T) {
	res := analyze(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c9", "w2", 300), ev(model.ConnReconnected, "c9", "w2", 305)},
	)
	md := render(res, nil)
	if res.Verdict != model.VerdictFail {
		t.Fatalf("precondition: verdict=%s want FAIL", res.Verdict)
	}
	if got := acCell(t, md, "Drops only on the rolling"); got != "FAIL" {
		t.Fatalf("drop-on-non-upgrading AC row = %q, want FAIL", got)
	}
}

// ------------------------------------------------------------------ R6 -------
func TestR6_NewConnFailure_Fail(t *testing.T) {
	res := analyze(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.NewConnAttemptFail, "n1", "w1", 60)},
	)
	md := render(res, nil)
	if res.Verdict != model.VerdictFail {
		t.Fatalf("precondition: verdict=%s want FAIL", res.Verdict)
	}
	if got := acCell(t, md, "Zero new-connection failures"); got != "FAIL" {
		t.Fatalf("new-conn AC row = %q, want FAIL", got)
	}
}

// ------------------------------------------------------------------ R7 -------
func TestR7_RecoveryOverBound_Fail(t *testing.T) {
	res := analyze(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115), ev(model.ConnReconnected, "c1", "w1", 160)},
	)
	md := render(res, nil)
	if res.Verdict != model.VerdictFail {
		t.Fatalf("precondition: verdict=%s want FAIL", res.Verdict)
	}
	if got := acCell(t, md, "Auto-recovery"); got != "FAIL" {
		t.Fatalf("recovery AC row = %q, want FAIL", got)
	}
}

// ------------------------------------------------------------------ R8 -------
func TestR8_PoolNotRecovered_Fail_NeverCell(t *testing.T) {
	res := analyze(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115)}, // never reconnects
	)
	md := render(res, nil)
	if res.Verdict != model.VerdictFail {
		t.Fatalf("precondition: verdict=%s want FAIL", res.Verdict)
	}
	if got := acCell(t, md, "Auto-recovery"); got != "FAIL" {
		t.Fatalf("recovery AC row = %q, want FAIL", got)
	}
	row := perNodeRow(t, md, "w1")
	if !strings.Contains(row, "never") {
		t.Fatalf("per-node recovery cell should be 'never', got row: %q", row)
	}
}

// ------------------------------------------------------------------ R9 -------
// Every ERROR variant: ALL AC rows render "N/A - measurement not trusted" and a
// NOT-TRUSTED banner naming the reason - never a green cell.
func TestR9_Error_AllACRowsNotTrusted(t *testing.T) {
	halfOpen := analyze([]model.RollWindow{window("w1", 0, nil)}, []model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115)})

	diedCfg := model.DefaultConfig()
	died := analyzeWith(model.TriggerInfo{Kind: "git-bump", ACSatisfying: true}, diedCfg,
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))}, []model.ConnEvent{ev(model.ConnReset, "c1", "w1", -30)})

	noRollCfg := model.DefaultConfig()
	noRollCfg.TriggerFired = true
	noRoll := analyzeWith(model.TriggerInfo{Kind: "git-bump", ACSatisfying: true}, noRollCfg, nil,
		[]model.ConnEvent{ev(model.ConnKeepaliveOK, "c1", "w1", 30)})

	prereqCfg := model.DefaultConfig()
	prereqCfg.TriggerPrereqError = "GHCR_TOKEN not set"
	prereq := analyzeWith(model.TriggerInfo{Kind: "git-bump", ACSatisfying: true}, prereqCfg, nil, nil)

	cases := map[string]model.Result{
		"half-open": halfOpen, "died-before-drain": died, "no-rollout": noRoll, "trigger-prereq": prereq,
	}
	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			if res.Verdict != model.VerdictError {
				t.Fatalf("precondition: verdict=%s want ERROR", res.Verdict)
			}
			md := render(res, nil)
			if !strings.Contains(md, "NOT TRUSTED") {
				t.Fatalf("missing NOT TRUSTED banner:\n%s", md)
			}
			if res.Reason != "" && !strings.Contains(md, res.Reason) {
				t.Fatalf("banner should name reason %q:\n%s", res.Reason, md)
			}
			for _, label := range []string{"Zero new-connection failures", disruptionRow, "Drops only on the rolling", "Staggered, not mesh-wide", "Auto-recovery"} {
				if got := acCell(t, md, label); got != "N/A - measurement not trusted" {
					t.Fatalf("AC row %q = %q, want 'N/A - measurement not trusted'", label, got)
				}
			}
			if strings.Contains(md, "| PASS |") {
				t.Fatalf("ERROR run must not render any green AC cell:\n%s", md)
			}
		})
	}
}

// ------------------------------------------------------------------ R10 ------
// A rollout-restart run is NOT AC-satisfying: even a green (PASS) measurement
// must show a NOT-AC-SATISFYING banner and N/A AC rows.
func TestR10_RolloutRestart_NotAC_EvenOnGreen(t *testing.T) {
	trigger := model.TriggerInfo{Kind: "rollout-restart", ACSatisfying: false, Warning: "rollout-restart does NOT satisfy the acceptance criteria"}
	res := analyzeWith(trigger, model.DefaultConfig(),
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115), ev(model.ConnReconnected, "c1", "w1", 118)},
	)
	if res.Verdict != model.VerdictPass {
		t.Fatalf("precondition: verdict=%s want PASS (green run)", res.Verdict)
	}
	md := render(res, nil)
	if !strings.Contains(md, "NOT AC-SATISFYING") {
		t.Fatalf("missing NOT AC-SATISFYING banner on green run:\n%s", md)
	}
	for _, label := range []string{"Zero new-connection failures", disruptionRow, "Auto-recovery"} {
		if got := acCell(t, md, label); !strings.HasPrefix(got, "N/A") {
			t.Fatalf("AC row %q = %q, want N/A even on green non-AC run", label, got)
		}
	}
}

// ------------------------------------------------------------------ R11 ------
func TestR11_PerClientTable(t *testing.T) {
	res := analyze([]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115), ev(model.ConnReconnected, "c1", "w1", 118)})
	five := 5.0
	clients := &PerClientObservations{
		SchemaVersion: ClientsSchemaVersion,
		Clients: []ClientObservation{
			{Client: "app-a", Library: "Node/TypeORM", Node: "na", Reset: true, DistinctResets: 1, RecoverySeconds: &five, Note: "reset+recovered"},
			{Client: "app-b", Library: "Python/SQLAlchemy", Node: "nb", Reset: false, DistinctResets: 0, Note: "survived"},
			{Client: "app-c", Library: "Go/pgx", Node: "nc", Reset: true, DistinctResets: 1, RecoverySeconds: nil, Note: "reset+never"},
		},
	}
	md := render(res, clients)
	for _, want := range []string{"app-a", "Node/TypeORM", "5.0s", "app-b", "Python/SQLAlchemy", "app-c", "Go/pgx", "never"} {
		if !strings.Contains(md, want) {
			t.Fatalf("per-client table missing %q:\n%s", want, md)
		}
	}
	// app-b (no reset) recovery cell must be a dash, not "never".
	inTable := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "## Per-client") {
			inTable = true
			continue
		}
		if inTable && strings.HasPrefix(line, "| app-b ") {
			if strings.Contains(line, "never") {
				t.Fatalf("app-b (no reset) should not show 'never': %q", line)
			}
		}
	}
}

// ------------------------------------------------------------------ R12 ------
func TestR12_NilClients_Pointer_NoPanic(t *testing.T) {
	res := analyze([]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115), ev(model.ConnReconnected, "c1", "w1", 118)})
	md := render(res, nil)
	if !strings.Contains(md, "--out-clients") {
		t.Fatalf("nil clients should render the producer pointer:\n%s", md)
	}
}

// ------------------------------------------------------------------ R13 ------
func TestR13_MdEscape(t *testing.T) {
	// Direct unit of the escaper.
	if got := mdEscape("a|b`c<d>e\nf\rg"); got != "a\\|b\\`c&lt;d&gt;e f g" {
		t.Fatalf("mdEscape = %q", got)
	}
	// Integration: hostile data must not break the table structure.
	res := analyze([]model.RollWindow{window("w|1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w|1", 115)})
	res.Trigger.Warning = "danger|`pipe`\nbreak"
	md := render(res, nil)
	if !strings.Contains(md, "w\\|1") {
		t.Fatalf("node name pipe not escaped:\n%s", md)
	}
	if strings.Contains(md, "danger|`pipe`") {
		t.Fatalf("trigger warning was not escaped:\n%s", md)
	}
}

// ------------------------------------------------------------------ R14 ------
func TestR14_ObservabilityIsCorroborationPlusPointerOnly(t *testing.T) {
	res := analyze([]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "w1", 115), ev(model.ConnReconnected, "c1", "w1", 118)})
	res.Corroboration = model.Corroboration{AppAConnReset: true, Note: "app-a logged ECONNRESET"}
	md := render(res, nil)
	if !strings.Contains(md, "verify-observability.sh") {
		t.Fatalf("observability section must point at verify-observability.sh:\n%s", md)
	}
	if !strings.Contains(md, "not embedded in this report") {
		t.Fatalf("observability section must state enrichment is not embedded:\n%s", md)
	}
	if !strings.Contains(md, "app-a logged ECONNRESET") {
		t.Fatalf("corroboration note must be rendered:\n%s", md)
	}
	// No live enrichment: none of the mesh metric series names should appear.
	for _, forbidden := range []string{"pilot_xds", "istio_requests_total", "istio_tcp_connections"} {
		if strings.Contains(md, forbidden) {
			t.Fatalf("report must not embed live metric %q (enrichment is deferred):\n%s", forbidden, md)
		}
	}
}

// ------------------------------------------------------------------ R19 ------
// The rendered report must never leak a proprietary identifier. The forbidden
// tokens are assembled by concatenation so the literals never appear in this
// source (which would itself trip scripts/no-identity-scan.sh).
func TestR19_NoProprietaryIdentifiers(t *testing.T) {
	res := analyze([]model.RollWindow{window("prod-node-1", 0, ptr(at(-10)))},
		[]model.ConnEvent{ev(model.ConnReset, "c1", "prod-node-1", 115)})
	clients := &PerClientObservations{SchemaVersion: ClientsSchemaVersion, Clients: []ClientObservation{{Client: "app-a", Library: "Node/TypeORM", Node: "prod-node-1", Reset: true, DistinctResets: 1}}}
	md := strings.ToLower(render(res, clients))
	forbidden := []string{
		"ready" + "on",
		"onready" + ".dev",
		"9757074" + "52016",
		"1161535" + "46408",
		"9511139" + "16427",
	}
	for _, id := range forbidden {
		if strings.Contains(md, id) {
			t.Fatalf("rendered report leaked a proprietary identifier")
		}
	}
}
