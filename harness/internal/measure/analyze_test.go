package measure

import (
	"testing"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// T0 is an arbitrary fixed base time; every event/window is expressed relative
// to it so the tests are deterministic and clock-free.
var T0 = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return T0.Add(time.Duration(sec) * time.Second) }

func ptr(t time.Time) *time.Time { return &t }

// window builds a RollWindow [drain, drain+120] with an optional readyAt marker.
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

// baseInput wires the default config with the trigger already fired.
func baseInput(windows []model.RollWindow, events []model.ConnEvent) model.Input {
	cfg := model.DefaultConfig()
	return model.Input{
		Trigger: model.TriggerInfo{Kind: "git-bump", ACSatisfying: true},
		Windows: windows,
		Events:  events,
		Config:  cfg,
	}
}

// perNode returns the attribution entry for node n (or a zero value + false).
func perNode(res model.Result, n string) (model.PerNodeAttribution, bool) {
	for _, p := range res.PerNodeAttribution {
		if p.Node == n {
			return p, true
		}
	}
	return model.PerNodeAttribution{}, false
}

func hasAnomaly(res model.Result, reason string) bool {
	for _, a := range res.Anomalies {
		if a.Reason == reason {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ 1 --------
func TestNewConnOK_Pass(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			ev(model.NewConnAttemptOK, "n1", "w1", 5),
			ev(model.NewConnAttemptOK, "n2", "w1", 60),
			ev(model.ConnKeepaliveOK, "c1", "w1", 100),
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	if res.NewConnFailures != 0 || res.AffectedNodeWindows != 0 {
		t.Fatalf("newConnFailures=%d affectedNodeWindows=%d, want 0/0", res.NewConnFailures, res.AffectedNodeWindows)
	}
}

// ------------------------------------------------------------------ 2 --------
func TestSingleRSTAfterDrain_Pass(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115), // drainingAt+115 ∈ [0,120]
			ev(model.ConnReconnected, "c1", "w1", 118),
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	if res.AffectedNodeWindows != 1 {
		t.Fatalf("affectedNodeWindows = %d, want 1", res.AffectedNodeWindows)
	}
	if res.ExistingConnRSTs != 1 {
		t.Fatalf("existingConnRSTs = %d, want 1", res.ExistingConnRSTs)
	}
	if res.DistinctNodesAffected != 1 {
		t.Fatalf("distinctNodesAffected = %d, want 1", res.DistinctNodesAffected)
	}
}

// ------------------------------------------------------------------ 3 --------
func TestRecovery6s(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115),
			ev(model.ConnReconnected, "c1", "w1", 121), // +6s
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	if res.RecoverySeconds == nil || *res.RecoverySeconds != 6 {
		t.Fatalf("recoverySeconds = %v, want 6", res.RecoverySeconds)
	}
}

// ------------------------------------------------------------------ 4 --------
func TestRSTOnNodeWithNoWindow_Fail(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			// ts well after the only window and after minDrain => not
			// died-before-drain, attributable to no upgrading node.
			ev(model.ConnReset, "c9", "w2", 300),
			ev(model.ConnReconnected, "c9", "w2", 305),
		},
	))
	if res.Verdict != model.VerdictFail {
		t.Fatalf("verdict = %s (%s), want FAIL", res.Verdict, res.Reason)
	}
	if !hasAnomaly(res, model.ReasonDropOnNonUpgradingNode) {
		t.Fatalf("missing drop-on-non-upgrading-node anomaly: %+v", res.Anomalies)
	}
}

// ------------------------------------------------------------------ 5 --------
func TestNewConnFailure_Fail(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			ev(model.NewConnAttemptFail, "n1", "w1", 60),
		},
	))
	if res.Verdict != model.VerdictFail {
		t.Fatalf("verdict = %s (%s), want FAIL", res.Verdict, res.Reason)
	}
	if res.NewConnFailures != 1 || !hasAnomaly(res, model.ReasonNewConnFailure) {
		t.Fatalf("newConnFailures=%d anomalies=%+v", res.NewConnFailures, res.Anomalies)
	}
}

// ------------------------------------------------------------------ 6 --------
func TestFiveConnsResetTogetherOneNode_Pass(t *testing.T) {
	events := []model.ConnEvent{}
	for _, c := range []string{"c1", "c2", "c3", "c4", "c5"} {
		events = append(events, ev(model.ConnReset, c, "w1", 115))
		events = append(events, ev(model.ConnReconnected, c, "w1", 118))
	}
	res := Analyze(baseInput([]model.RollWindow{window("w1", 0, ptr(at(-10)))}, events))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	if res.AffectedNodeWindows != 1 {
		t.Fatalf("affectedNodeWindows = %d, want 1", res.AffectedNodeWindows)
	}
	p, _ := perNode(res, "w1")
	if p.DistinctConnsReset != 5 {
		t.Fatalf("distinctConnsReset = %d, want 5", p.DistinctConnsReset)
	}
}

// ------------------------------------------------------------------ 7 --------
func TestStaggeredTwoNodesNonOverlapping_Pass(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{
			window("w1", 0, ptr(at(-10))),
			window("w2", 120, ptr(at(110))), // touches w1 end at 120 (overlap 0)
		},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115),
			ev(model.ConnReconnected, "c1", "w1", 118),
			ev(model.ConnReset, "c2", "w2", 235),
			ev(model.ConnReconnected, "c2", "w2", 238),
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	if res.AffectedNodeWindows != 2 || res.DistinctNodesAffected != 2 {
		t.Fatalf("affectedNodeWindows=%d distinctNodesAffected=%d, want 2/2", res.AffectedNodeWindows, res.DistinctNodesAffected)
	}
}

// ------------------------------------------------------------------ 8 --------
func TestOverlappingTwoNodesBeyondEps_Fail(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{
			window("w1", 0, ptr(at(-10))),
			window("w2", 60, ptr(at(50))), // overlap [60,120] = 60s > eps
		},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 30),
			ev(model.ConnReconnected, "c1", "w1", 33),
		},
	))
	if res.Verdict != model.VerdictFail {
		t.Fatalf("verdict = %s (%s), want FAIL", res.Verdict, res.Reason)
	}
	if !hasAnomaly(res, model.ReasonOverlappingNodeWindows) {
		t.Fatalf("missing overlapping-node-windows anomaly: %+v", res.Anomalies)
	}
}

// ------------------------------------------------------------------ 9 --------
func TestSameConnResetInTwoWindows_Fail(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{
			window("w1", 0, ptr(at(-10))),
			window("w2", 120, ptr(at(110))),
		},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115),
			ev(model.ConnReconnected, "c1", "w1", 118),
			ev(model.ConnReset, "c1", "w2", 235), // same conn, second window
			ev(model.ConnReconnected, "c1", "w2", 238),
		},
	))
	if res.Verdict != model.VerdictFail {
		t.Fatalf("verdict = %s (%s), want FAIL", res.Verdict, res.Reason)
	}
	if !hasAnomaly(res, model.ReasonSameConnTwoWindows) {
		t.Fatalf("missing same-conn-reset-in-2-windows anomaly: %+v", res.Anomalies)
	}
}

// ------------------------------------------------------------------ 10 -------
func TestNonRecovering_Fail(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115), // never reconnects
		},
	))
	if res.Verdict != model.VerdictFail {
		t.Fatalf("verdict = %s (%s), want FAIL", res.Verdict, res.Reason)
	}
	if !hasAnomaly(res, model.ReasonPoolNotRecovered) {
		t.Fatalf("missing pool-not-recovered anomaly: %+v", res.Anomalies)
	}
	if res.RecoverySeconds != nil {
		t.Fatalf("recoverySeconds = %v, want nil", *res.RecoverySeconds)
	}
}

// ------------------------------------------------------------------ 11 -------
func TestRecovery45OverBound30_Fail(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115),
			ev(model.ConnReconnected, "c1", "w1", 160), // +45s > 30s bound
		},
	))
	if res.Verdict != model.VerdictFail {
		t.Fatalf("verdict = %s (%s), want FAIL", res.Verdict, res.Reason)
	}
	if !hasAnomaly(res, model.ReasonRecoveryOverBound) {
		t.Fatalf("missing recovery-over-bound anomaly: %+v", res.Anomalies)
	}
}

// ------------------------------------------------------------------ 12 -------
func TestThreeNodeStaggeredCanonical_Pass(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{
			window("w1", 0, ptr(at(-10))),
			window("w2", 120, ptr(at(110))),
			window("w3", 240, ptr(at(230))),
		},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115),
			ev(model.ConnReconnected, "c1", "w1", 118),
			ev(model.ConnReset, "c2", "w2", 235),
			ev(model.ConnReconnected, "c2", "w2", 238),
			ev(model.ConnReset, "c3", "w3", 355),
			ev(model.ConnReconnected, "c3", "w3", 358),
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	if res.AffectedNodeWindows != 3 || res.DistinctNodesAffected != 3 {
		t.Fatalf("affectedNodeWindows=%d distinctNodesAffected=%d, want 3/3", res.AffectedNodeWindows, res.DistinctNodesAffected)
	}
}

// ------------------------------------------------------------------ 13 -------
// Attribution is by WINDOW, not by the prober's node: the probe physically runs
// on node A, but the RST timestamp falls inside node B's window, so it must be
// attributed to B.
func TestAttributionByWindowNotProberNode_Pass(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{
			window("A", 0, ptr(at(-10))),
			window("B", 120, ptr(at(110))),
		},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "A", 130), // prober on A, ts ∈ B window
			ev(model.ConnReconnected, "c1", "A", 133),
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	pa, _ := perNode(res, "A")
	pb, _ := perNode(res, "B")
	if pa.DistinctConnsReset != 0 {
		t.Fatalf("node A distinctConnsReset = %d, want 0", pa.DistinctConnsReset)
	}
	if pb.DistinctConnsReset != 1 {
		t.Fatalf("node B distinctConnsReset = %d, want 1", pb.DistinctConnsReset)
	}
	if res.DistinctNodesAffected != 1 {
		t.Fatalf("distinctNodesAffected = %d, want 1 (B)", res.DistinctNodesAffected)
	}
}

// ------------------------------------------------------------------ 14 -------
func TestNoRolloutObserved_Error(t *testing.T) {
	in := baseInput(nil, []model.ConnEvent{
		ev(model.ConnKeepaliveOK, "c1", "w1", 30),
	})
	in.Config.TriggerFired = true
	res := Analyze(in)
	if res.Verdict == model.VerdictPass || res.Verdict == model.VerdictFail {
		t.Fatalf("verdict = %s, want ERROR (not PASS/FAIL)", res.Verdict)
	}
	if res.Verdict != model.VerdictError || !hasAnomaly(res, model.ReasonNoRolloutObserved) {
		t.Fatalf("verdict=%s anomalies=%+v, want ERROR no-rollout-observed", res.Verdict, res.Anomalies)
	}
}

// ------------------------------------------------------------------ 15 -------
// A window that started draining but whose new pod never reached Ready is
// half-open at the deadline: ERROR, yet the RST is still attributed to the open
// [drainingAt, graceExpiresAt] window.
func TestHalfOpenWindow_Error(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, nil)}, // readyAt nil => half-open
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115),
		},
	))
	if res.Verdict != model.VerdictError {
		t.Fatalf("verdict = %s (%s), want ERROR", res.Verdict, res.Reason)
	}
	if !hasAnomaly(res, model.ReasonHalfOpenWindow) {
		t.Fatalf("missing half-open-window anomaly: %+v", res.Anomalies)
	}
	p, _ := perNode(res, "w1")
	if p.DistinctConnsReset != 1 {
		t.Fatalf("reset not attributed to open window: distinctConnsReset=%d, want 1", p.DistinctConnsReset)
	}
}

// ------------------------------------------------------------------ 16 -------
func TestDuplicateRSTSameConnWindow_Dedup_Pass(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 115),
			ev(model.ConnReset, "c1", "w1", 116), // duplicate, same conn+window
			ev(model.ConnReconnected, "c1", "w1", 119),
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	p, _ := perNode(res, "w1")
	if p.DistinctConnsReset != 1 {
		t.Fatalf("distinctConnsReset = %d, want 1 (dedup)", p.DistinctConnsReset)
	}
	if p.RawResets != 2 {
		t.Fatalf("rawResets = %d, want 2", p.RawResets)
	}
}

// ------------------------------------------------------------------ 17 -------
func TestRSTRecoversAfterReadyAt(t *testing.T) {
	// ready marker at drainingAt-10; reconnect happens AFTER readyAt yet the
	// recovery is measured reset->reconnect, and is within bound => PASS.
	t.Run("within-bound", func(t *testing.T) {
		res := Analyze(baseInput(
			[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
			[]model.ConnEvent{
				ev(model.ConnReset, "c1", "w1", 115),
				ev(model.ConnReconnected, "c1", "w1", 125), // +10s, after readyAt
			},
		))
		if res.Verdict != model.VerdictPass {
			t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
		}
		if res.RecoverySeconds == nil || *res.RecoverySeconds != 10 {
			t.Fatalf("recoverySeconds = %v, want 10", res.RecoverySeconds)
		}
	})
	t.Run("never-reconnect", func(t *testing.T) {
		res := Analyze(baseInput(
			[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
			[]model.ConnEvent{
				ev(model.ConnReset, "c1", "w1", 115),
			},
		))
		if res.Verdict != model.VerdictFail {
			t.Fatalf("verdict = %s (%s), want FAIL", res.Verdict, res.Reason)
		}
		if !hasAnomaly(res, model.ReasonPoolNotRecovered) {
			t.Fatalf("missing pool-not-recovered anomaly: %+v", res.Anomalies)
		}
	})
}

// ------------------------------------------------------------------ 18 -------
func TestDiedBeforeDrain_Error(t *testing.T) {
	// Close before min(drainingAt) => measurement contaminated => ERROR, never a
	// silent PASS. Both the RST and the EOF variant must behave the same.
	t.Run("rst-variant", func(t *testing.T) {
		res := Analyze(baseInput(
			[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
			[]model.ConnEvent{
				ev(model.ConnReset, "c1", "w1", -30), // before drain
			},
		))
		if res.Verdict != model.VerdictError {
			t.Fatalf("verdict = %s (%s), want ERROR", res.Verdict, res.Reason)
		}
		if res.Verdict == model.VerdictPass {
			t.Fatalf("died-before-drain must not be PASS")
		}
		if !hasAnomaly(res, model.ReasonDiedBeforeDrain) {
			t.Fatalf("missing died-before-drain anomaly: %+v", res.Anomalies)
		}
	})
	t.Run("eof-variant", func(t *testing.T) {
		res := Analyze(baseInput(
			[]model.RollWindow{window("w1", 0, ptr(at(-10)))},
			[]model.ConnEvent{
				ev(model.ConnEOF, "c1", "w1", -30),
			},
		))
		if res.Verdict != model.VerdictError {
			t.Fatalf("verdict = %s (%s), want ERROR", res.Verdict, res.Reason)
		}
		if !hasAnomaly(res, model.ReasonDiedBeforeDrain) {
			t.Fatalf("missing died-before-drain anomaly: %+v", res.Anomalies)
		}
	})
}

// ------------------------------------------------------- JITTER edge case ----
// Two adjacent per-node windows whose boundaries touch within eps must NOT be
// flagged as overlapping (kind's per-node scheduling jitter), so the run PASSes.
func TestAdjacentWindowsWithinEps_NotOverlapping_Pass(t *testing.T) {
	res := Analyze(baseInput(
		[]model.RollWindow{
			window("w1", 0, ptr(at(-10))),   // [0,120]
			window("w2", 119, ptr(at(109))), // [119,239]; overlap 1s <= eps(2s)
		},
		[]model.ConnEvent{
			ev(model.ConnReset, "c1", "w1", 50), // clearly inside w1 only
			ev(model.ConnReconnected, "c1", "w1", 53),
			ev(model.ConnReset, "c2", "w2", 200), // clearly inside w2 only
			ev(model.ConnReconnected, "c2", "w2", 203),
		},
	))
	if res.Verdict != model.VerdictPass {
		t.Fatalf("verdict = %s (%s), want PASS", res.Verdict, res.Reason)
	}
	if hasAnomaly(res, model.ReasonOverlappingNodeWindows) {
		t.Fatalf("adjacent-within-eps windows wrongly flagged overlapping: %+v", res.Anomalies)
	}
	if res.AffectedNodeWindows != 2 {
		t.Fatalf("affectedNodeWindows = %d, want 2", res.AffectedNodeWindows)
	}
}

// -------------------------------------------------- trigger prereq ERROR -----
func TestTriggerPrereqMissing_Error(t *testing.T) {
	in := baseInput(nil, nil)
	in.Config.TriggerPrereqError = "GHCR_TOKEN not set"
	res := Analyze(in)
	if res.Verdict != model.VerdictError {
		t.Fatalf("verdict = %s, want ERROR", res.Verdict)
	}
	if !hasAnomaly(res, model.ReasonTriggerPrereqMissing) {
		t.Fatalf("missing trigger-prereq-missing anomaly: %+v", res.Anomalies)
	}
}
