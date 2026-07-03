// Package measure holds the pure, hermetic drop-measurement analyzer.
//
// STRICT boundary: it imports ONLY internal/model (and the standard library).
// No net, no k8s, no os/exec, no clock. Every verdict is a pure function of the
// synthetic-or-real event stream handed to Analyze, which is what makes the
// table tests in analyze_test.go run without a cluster.
package measure

import (
	"sort"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// Analyze turns an observed connection-event stream plus the per-node roll
// windows into the stable Result. It is the whole verdict engine.
//
// Verdict precedence is ERROR > FAIL > PASS: an ERROR means the measurement
// cannot be trusted (so it is never silently downgraded to PASS), a FAIL means
// a real drop violated the accepted-harm bound, and PASS is the clean outcome.
func Analyze(in model.Input) model.Result {
	cfg := in.Config
	eps := time.Duration(cfg.JitterToleranceSeconds * float64(time.Second))

	res := model.Result{
		SchemaVersion:        model.SchemaVersion,
		Trigger:              in.Trigger,
		RecoveryBoundSeconds: cfg.RecoveryBoundSeconds,
		Corroboration:        model.Corroboration{Note: "no app-a corroboration available"},
		Anomalies:            []model.Anomaly{},
		PerNodeAttribution:   []model.PerNodeAttribution{},
	}
	if cfg.AppACorroboration != nil {
		res.Corroboration = *cfg.AppACorroboration
	}

	var errors, fails []model.Anomaly
	addErr := func(a model.Anomaly) { errors = append(errors, a); res.Anomalies = append(res.Anomalies, a) }
	addFail := func(a model.Anomaly) { fails = append(fails, a); res.Anomalies = append(res.Anomalies, a) }

	// ---- raw counters (existingConnRSTs is reported but NOT a verdict input) --
	for _, ev := range in.Events {
		switch ev.Kind {
		case model.NewConnAttemptFail:
			res.NewConnFailures++
		case model.ConnReset:
			res.ExistingConnRSTs++
		}
	}

	// ---- trigger prerequisite missing: hard ERROR before anything else -------
	if cfg.TriggerPrereqError != "" {
		addErr(model.Anomaly{Reason: model.ReasonTriggerPrereqMissing})
	}

	// ---- no rollout observed: trigger fired but zero windows -----------------
	if cfg.TriggerFired && len(in.Windows) == 0 {
		addErr(model.Anomaly{Reason: model.ReasonNoRolloutObserved})
	}

	// ---- half-open window: new pod never went Ready (marker absent) ----------
	// The window is still usable for attribution ([drainingAt, graceExpiresAt]
	// needs no readyAt), but an unresolved roll means we cannot trust recovery.
	for _, w := range in.Windows {
		if w.ReadyAt == nil {
			addErr(model.Anomaly{Reason: model.ReasonHalfOpenWindow, Node: w.Node, TS: w.DrainingAt})
		}
	}

	// ---- overlapping windows across DIFFERENT nodes (beyond jitter eps) ------
	for i := 0; i < len(in.Windows); i++ {
		for j := i + 1; j < len(in.Windows); j++ {
			a, b := in.Windows[i], in.Windows[j]
			if a.Node == b.Node {
				continue
			}
			// overlap = min(ends) - max(starts); >eps means a genuine cross-node
			// overlap, i.e. two nodes draining at once = mesh-wide, not per-node.
			latestStart := maxTime(a.DrainingAt, b.DrainingAt)
			earliestEnd := minTime(a.GraceExpiresAt, b.GraceExpiresAt)
			if earliestEnd.Sub(latestStart) > eps {
				addFail(model.Anomaly{Reason: model.ReasonOverlappingNodeWindows, Node: a.Node, TS: latestStart})
			}
		}
	}

	// ---- earliest drain, for the died-before-drain guard ---------------------
	var minDrain time.Time
	haveMinDrain := false
	for _, w := range in.Windows {
		if !haveMinDrain || w.DrainingAt.Before(minDrain) {
			minDrain = w.DrainingAt
			haveMinDrain = true
		}
	}

	// ---- attribute each reset to at most one window --------------------------
	// perWindow is keyed by window index; a nil entry means "no window".
	type windowAgg struct {
		raw         int
		connFirst   map[string]time.Time // connId -> first attributed reset ts
		connResetTS map[string]time.Time // connId -> latest attributed reset ts
	}
	aggs := make([]windowAgg, len(in.Windows))
	for i := range aggs {
		aggs[i] = windowAgg{connFirst: map[string]time.Time{}, connResetTS: map[string]time.Time{}}
	}
	// connWindows tracks, per connId, the distinct window indices it was reset in
	// (for the same-conn-in-2-windows guard).
	connWindows := map[string]map[int]bool{}

	for _, ev := range in.Events {
		if ev.Kind != model.ConnReset && ev.Kind != model.ConnEOF {
			continue
		}
		// died-before-drain: any close strictly before the first drain begins
		// means the connection died for a non-upgrade reason - measurement is
		// contaminated, so ERROR (covers both RST and EOF variants).
		if haveMinDrain && ev.TS.Before(minDrain.Add(-eps)) {
			addErr(model.Anomaly{Reason: model.ReasonDiedBeforeDrain, ConnID: ev.ConnID, Node: ev.Node, TS: ev.TS})
			continue
		}
		if ev.Kind != model.ConnReset {
			// A non-early EOF is graceful and not an upgrade-attributable reset.
			continue
		}
		idx := attributeReset(ev.TS, in.Windows, eps)
		if idx < 0 {
			// A reset that lands in no window (and not before drain) is a drop
			// pinned to a node that is not upgrading - FAIL.
			addFail(model.Anomaly{Reason: model.ReasonDropOnNonUpgradingNode, ConnID: ev.ConnID, Node: ev.Node, TS: ev.TS})
			continue
		}
		ag := &aggs[idx]
		ag.raw++
		if _, seen := ag.connFirst[ev.ConnID]; !seen {
			ag.connFirst[ev.ConnID] = ev.TS
		}
		if prev, ok := ag.connResetTS[ev.ConnID]; !ok || ev.TS.After(prev) {
			ag.connResetTS[ev.ConnID] = ev.TS
		}
		if connWindows[ev.ConnID] == nil {
			connWindows[ev.ConnID] = map[int]bool{}
		}
		connWindows[ev.ConnID][idx] = true
	}

	// ---- same connId reset in two or more distinct windows -------------------
	// Deterministic: iterate connIds in sorted order.
	for _, connID := range sortedKeys(connWindows) {
		if len(connWindows[connID]) >= 2 {
			addFail(model.Anomaly{Reason: model.ReasonSameConnTwoWindows, ConnID: connID})
		}
	}

	// ---- recovery: match each reset conn to a later ConnReconnected -----------
	// reconnectByConn[connId] = earliest reconnect ts.
	reconnectByConn := map[string]time.Time{}
	for _, ev := range in.Events {
		if ev.Kind != model.ConnReconnected {
			continue
		}
		if prev, ok := reconnectByConn[ev.ConnID]; !ok || ev.TS.Before(prev) {
			reconnectByConn[ev.ConnID] = ev.TS
		}
	}

	anyUnrecovered := false
	var maxRecovery float64
	haveRecovery := false

	// ---- build per-node attribution (one entry per window) -------------------
	for i, w := range in.Windows {
		ag := aggs[i]
		pna := model.PerNodeAttribution{
			Node: w.Node,
			Window: model.WindowJSON{
				DrainingAt:     w.DrainingAt,
				GraceExpiresAt: w.GraceExpiresAt,
				ReadyAt:        w.ReadyAt,
			},
			RawResets:          ag.raw,
			DistinctConnsReset: len(ag.connResetTS),
			Recovered:          true,
		}
		var winMax float64
		winHaveRecovery := false
		for _, connID := range sortedKeys(ag.connResetTS) {
			resetTS := ag.connResetTS[connID]
			rc, ok := reconnectByConn[connID]
			if !ok || rc.Before(resetTS) {
				pna.Recovered = false
				anyUnrecovered = true
				continue
			}
			secs := rc.Sub(resetTS).Seconds()
			if !winHaveRecovery || secs > winMax {
				winMax = secs
				winHaveRecovery = true
			}
			if !haveRecovery || secs > maxRecovery {
				maxRecovery = secs
				haveRecovery = true
			}
		}
		if pna.DistinctConnsReset == 0 {
			pna.Recovered = false // no affected conns => nothing recovered
		}
		if winHaveRecovery && pna.Recovered {
			v := winMax
			pna.RecoverySeconds = &v
		}
		res.PerNodeAttribution = append(res.PerNodeAttribution, pna)

		if pna.DistinctConnsReset >= 1 {
			res.AffectedNodeWindows++
		}
	}
	sort.SliceStable(res.PerNodeAttribution, func(i, j int) bool {
		if res.PerNodeAttribution[i].Node != res.PerNodeAttribution[j].Node {
			return res.PerNodeAttribution[i].Node < res.PerNodeAttribution[j].Node
		}
		return res.PerNodeAttribution[i].Window.DrainingAt.Before(res.PerNodeAttribution[j].Window.DrainingAt)
	})

	// distinct nodes among affected windows
	affectedNodes := map[string]bool{}
	for _, pna := range res.PerNodeAttribution {
		if pna.DistinctConnsReset >= 1 {
			affectedNodes[pna.Node] = true
		}
	}
	res.DistinctNodesAffected = len(affectedNodes)

	// ---- recovery verdict inputs ---------------------------------------------
	if anyUnrecovered {
		res.RecoverySeconds = nil
		addFail(model.Anomaly{Reason: model.ReasonPoolNotRecovered})
	} else if haveRecovery {
		v := maxRecovery
		res.RecoverySeconds = &v
		if maxRecovery > float64(cfg.RecoveryBoundSeconds) {
			addFail(model.Anomaly{Reason: model.ReasonRecoveryOverBound})
		}
	}

	// ---- new connection failures --------------------------------------------
	if res.NewConnFailures > 0 {
		addFail(model.Anomaly{Reason: model.ReasonNewConnFailure})
	}

	// ---- final verdict -------------------------------------------------------
	switch {
	case len(errors) > 0:
		res.Verdict = model.VerdictError
		res.Reason = errors[0].Reason
	case len(fails) > 0:
		res.Verdict = model.VerdictFail
		res.Reason = fails[0].Reason
	default:
		res.Verdict = model.VerdictPass
	}
	return res
}

// attributeReset returns the index of the single window a reset at ts belongs
// to, or -1 if none contains it. When more than one window contains ts (only
// possible inside the small jitter-tolerance overlap of adjacent staggered
// windows), the window with the LATEST DrainingAt wins, giving each reset a
// single deterministic owner.
func attributeReset(ts time.Time, windows []model.RollWindow, eps time.Duration) int {
	best := -1
	for i, w := range windows {
		if ts.Before(w.DrainingAt.Add(-eps)) || ts.After(w.GraceExpiresAt.Add(eps)) {
			continue
		}
		if best < 0 || w.DrainingAt.After(windows[best].DrainingAt) {
			best = i
		}
	}
	return best
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
