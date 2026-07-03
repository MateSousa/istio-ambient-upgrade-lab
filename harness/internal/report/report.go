// Package report renders a hermetic Markdown PASS/FAIL report from a harness
// Result (internal/model). It is the deep, pure rendering module of slice 7.
//
// STRICT boundary: this package imports ONLY internal/model and the standard
// library. No net, no k8s, no os/exec, no clock - the "generated at" stamp is
// passed in via RenderOptions. That is what keeps Render a pure function of its
// inputs and lets report_test.go exercise every verdict/threshold path with
// synthetic Results and never a cluster.
//
// The verdict itself is NOT recomputed here: internal/measure owns the verdict
// logic. Render only PRESENTS a Result - it maps the analyzer's reason strings
// onto the acceptance-criterion checklist and never reads raw counters for a
// PASS/FAIL cell (see the AC checklist below).
package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// ClientsSchemaVersion tags the per-client observations JSON that
// `harness measure --out-clients` produces and `harness report --clients`
// consumes. Bump only on a breaking change to PerClientObservations.
const ClientsSchemaVersion = "clients/v1"

// ClientObservation is one demo client's (app-a Node / app-b Python / app-c Go)
// observed behaviour during the ztunnel roll: whether its held pooled connection
// was reset, how many distinct resets it saw, and how long it took to recover.
type ClientObservation struct {
	Client          string   `json:"client"`          // app-a | app-b | app-c
	Library         string   `json:"library"`         // Node/TypeORM | Python/SQLAlchemy | Go/pgx
	Node            string   `json:"node"`            // node the client pod runs on
	Reset           bool     `json:"reset"`           // held connection was RST during the roll
	DistinctResets  int      `json:"distinctResets"`  // distinct resets attributed to the roll
	RecoverySeconds *float64 `json:"recoverySeconds"` // reconnect latency; nil => never recovered
	Note            string   `json:"note"`            // free-form observation note
}

// PerClientObservations is the optional side-car document produced by the live
// client observer and consumed by the report. It is schema-versioned
// independently of the Result so the two can evolve separately.
type PerClientObservations struct {
	SchemaVersion string              `json:"schemaVersion"`
	Clients       []ClientObservation `json:"clients"`
}

// RenderOptions carries the injected facts a pure renderer must not fetch for
// itself. GeneratedAt is the report timestamp (the caller passes time.Now()).
type RenderOptions struct {
	GeneratedAt time.Time
}

// perNodeFootnote explains why the per-node blast radius is a per-WINDOW bound,
// not a per-connection one - so a run with five connections reset together in
// one node's roll window is a PASS, and only a same-connection reset spanning
// two windows (or windows overlapping across nodes) FAILs. Authored literally.
const perNodeFootnote = "Multiple connections resetting together in one node's roll window is the accepted per-node blast radius (PASS); FAIL fires on the same connection reset across two windows, or reset windows overlapping across different nodes."

// Render turns a Result (plus optional per-client observations) into a Markdown
// report string. It never panics on a nil clients pointer or empty slices.
//
// Trust/AC dominance (REV 4): an ERROR verdict or a non-AC-satisfying trigger
// DOMINATES the presentation - the acceptance-criterion rows only show a real
// PASS/FAIL when the measurement is BOTH trusted (Verdict != ERROR) AND
// AC-satisfying; otherwise every AC row renders N/A and a prominent banner says
// why, even on an otherwise-green run.
func Render(res model.Result, clients *PerClientObservations, opts RenderOptions) string {
	trusted := res.Verdict != model.VerdictError
	acSatisfying := res.Trigger.ACSatisfying
	showAC := trusted && acSatisfying

	var b strings.Builder

	// ---- title + generated-at ------------------------------------------------
	b.WriteString("# Istio ambient ztunnel upgrade - drop-measurement report\n\n")
	fmt.Fprintf(&b, "_Generated at %s._\n\n", mdEscape(opts.GeneratedAt.UTC().Format(time.RFC3339)))

	// ---- trust / verdict banner (dominant) -----------------------------------
	writeBanner(&b, res, trusted, acSatisfying)

	// ---- trigger block -------------------------------------------------------
	writeTrigger(&b, res.Trigger)

	// ---- acceptance-criterion checklist + footnote ---------------------------
	writeACChecklist(&b, res, showAC, trusted, acSatisfying)

	// ---- per-node attribution table ------------------------------------------
	writePerNode(&b, res)

	// ---- per-client table (or pointer) ---------------------------------------
	writePerClient(&b, clients)

	// ---- anomalies -----------------------------------------------------------
	writeAnomalies(&b, res)

	// ---- corroboration -------------------------------------------------------
	writeCorroboration(&b, res.Corroboration)

	// ---- observability pointer -----------------------------------------------
	writeObservability(&b)

	// ---- footer --------------------------------------------------------------
	writeFooter(&b, res, opts)

	return b.String()
}

func writeBanner(b *strings.Builder, res model.Result, trusted, acSatisfying bool) {
	switch {
	case !trusted:
		// ERROR dominates everything: name the reason and refuse to show a verdict.
		fmt.Fprintf(b, "> **NOT TRUSTED - measurement cannot be trusted (%s).** The acceptance criterion is neither PASS nor FAIL for this run; the drill must be re-run. Verdict: **%s**.\n\n",
			mdEscape(reasonOr(res.Reason, "measurement error")), mdEscape(res.Verdict))
	case !acSatisfying:
		// Trusted but the trigger was the rollout-restart escape hatch: the run
		// cannot satisfy the AC even if it is green, so say so prominently.
		warn := res.Trigger.Warning
		if warn == "" {
			warn = "trigger does not satisfy the acceptance criteria (a Git-synced version bump is required)"
		}
		fmt.Fprintf(b, "> **NOT AC-SATISFYING - this run does not satisfy the acceptance criteria.** %s The verdict below is informational only. Underlying measurement verdict: **%s**.\n\n",
			mdEscape(warn), mdEscape(res.Verdict))
	case res.Verdict == model.VerdictPass:
		b.WriteString("> **PASS - the acceptance criterion is satisfied.** No new-connection failures; disruption stayed within the accepted per-node blast radius and recovered within the bound.\n\n")
	default:
		fmt.Fprintf(b, "> **FAIL - the acceptance criterion is violated (%s).** See the checklist and anomalies below.\n\n",
			mdEscape(reasonOr(res.Reason, "see anomalies")))
	}
}

func writeTrigger(b *strings.Builder, t model.TriggerInfo) {
	b.WriteString("## Trigger\n\n")
	b.WriteString("| Field | Value |\n| --- | --- |\n")
	fmt.Fprintf(b, "| Kind | %s |\n", mdEscape(t.Kind))
	fmt.Fprintf(b, "| AC-satisfying | %s |\n", boolWord(t.ACSatisfying))
	if t.ChartVersionFrom != "" || t.ChartVersionTo != "" {
		fmt.Fprintf(b, "| Chart version | %s -> %s |\n", mdEscape(orDash(t.ChartVersionFrom)), mdEscape(orDash(t.ChartVersionTo)))
	}
	if t.ZtunnelTagFrom != "" || t.ZtunnelTagTo != "" {
		fmt.Fprintf(b, "| ztunnel tag | %s -> %s |\n", mdEscape(orDash(t.ZtunnelTagFrom)), mdEscape(orDash(t.ZtunnelTagTo)))
	}
	if t.Warning != "" {
		fmt.Fprintf(b, "| Warning | %s |\n", mdEscape(t.Warning))
	}
	b.WriteString("\n")
}

// acRow is one acceptance-criterion checklist line. failReasons lists the
// analyzer reason strings whose PRESENCE makes this criterion FAIL; the row is
// PASS only when none are present. Cells never read raw counters (REV 1/4).
type acRow struct {
	label       string
	failReasons []string
	footnote    bool
}

func acRows() []acRow {
	return []acRow{
		{label: "Zero new-connection failures", failReasons: []string{model.ReasonNewConnFailure}},
		// REV 1: per-node blast radius is a per-WINDOW bound, mapped ONLY from
		// these two reasons - never from DistinctConnsReset. Footnote clarifies.
		{label: "<=1 disruption window per pre-existing node", failReasons: []string{model.ReasonSameConnTwoWindows, model.ReasonOverlappingNodeWindows}, footnote: true},
		{label: "Drops only on the rolling (pre-existing) node", failReasons: []string{model.ReasonDropOnNonUpgradingNode}},
		{label: "Staggered, not mesh-wide (no cross-node overlap)", failReasons: []string{model.ReasonOverlappingNodeWindows}},
		{label: "Auto-recovery within the configured bound", failReasons: []string{model.ReasonPoolNotRecovered, model.ReasonRecoveryOverBound}},
	}
}

func writeACChecklist(b *strings.Builder, res model.Result, showAC, trusted, acSatisfying bool) {
	b.WriteString("## Acceptance criterion\n\n")
	b.WriteString("| Criterion | Result |\n| --- | --- |\n")

	// The N/A cell text differs by cause so a reader knows WHY the AC was not
	// evaluated. ERROR (untrusted) and non-AC-satisfying are distinct states.
	var naCell string
	switch {
	case !trusted:
		naCell = "N/A - measurement not trusted"
	case !acSatisfying:
		naCell = "N/A - trigger not AC-satisfying"
	}

	haveFootnote := false
	for _, row := range acRows() {
		label := row.label
		if row.footnote {
			label += " [1]"
			haveFootnote = true
		}
		var cell string
		if showAC {
			cell = "PASS"
			for _, r := range row.failReasons {
				if hasReason(res, r) {
					cell = "FAIL"
					break
				}
			}
		} else {
			cell = naCell
		}
		fmt.Fprintf(b, "| %s | %s |\n", label, cell)
	}
	b.WriteString("\n")
	if haveFootnote {
		fmt.Fprintf(b, "[1] %s\n\n", perNodeFootnote)
	}
}

func writePerNode(b *strings.Builder, res model.Result) {
	b.WriteString("## Per-node attribution\n\n")
	if len(res.PerNodeAttribution) == 0 {
		b.WriteString("_No roll windows were observed._\n\n")
		return
	}
	b.WriteString("| Node | Roll window | Distinct conns reset | Raw resets | Recovered | Recovery |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, p := range res.PerNodeAttribution {
		// The " -> " separator is a literal; only the timestamps are data.
		win := mdEscape(p.Window.DrainingAt.UTC().Format(time.RFC3339)) + " -> " +
			mdEscape(p.Window.GraceExpiresAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(b, "| %s | %s | %d | %d | %s | %s |\n",
			mdEscape(p.Node), win, p.DistinctConnsReset, p.RawResets,
			boolWord(p.Recovered), recoveryCell(p.DistinctConnsReset, p.Recovered, p.RecoverySeconds))
	}
	b.WriteString("\n")
}

func writePerClient(b *strings.Builder, clients *PerClientObservations) {
	b.WriteString("## Per-client observations\n\n")
	if clients == nil || len(clients.Clients) == 0 {
		b.WriteString("_No per-client observations were supplied. Run `harness measure --out-clients clients.json` to produce them, then `harness report --clients clients.json`._\n\n")
		return
	}
	b.WriteString("| Client | Library | Node | Reset | Distinct resets | Recovery | Note |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, c := range clients.Clients {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %d | %s | %s |\n",
			mdEscape(c.Client), mdEscape(c.Library), mdEscape(c.Node),
			boolWord(c.Reset), c.DistinctResets,
			recoveryCell(c.DistinctResets, c.RecoverySeconds != nil, c.RecoverySeconds),
			mdEscape(orDash(c.Note)))
	}
	b.WriteString("\n")
}

func writeAnomalies(b *strings.Builder, res model.Result) {
	b.WriteString("## Anomalies\n\n")
	if len(res.Anomalies) == 0 {
		b.WriteString("_None._\n\n")
		return
	}
	b.WriteString("| Reason | Conn | Node | Timestamp |\n| --- | --- | --- | --- |\n")
	for _, a := range res.Anomalies {
		ts := ""
		if !a.TS.IsZero() {
			ts = a.TS.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			mdEscape(a.Reason), mdEscape(orDash(a.ConnID)), mdEscape(orDash(a.Node)), mdEscape(orDash(ts)))
	}
	b.WriteString("\n")
}

func writeCorroboration(b *strings.Builder, c model.Corroboration) {
	b.WriteString("## Corroboration\n\n")
	fmt.Fprintf(b, "- app-a self-reported connection reset: %s\n", boolWord(c.AppAConnReset))
	if c.Note != "" {
		fmt.Fprintf(b, "- Note: %s\n", mdEscape(c.Note))
	}
	b.WriteString("\n")
}

// writeObservability renders ONLY the secondary corroboration pointer (REV 3):
// live mesh-metric enrichment (Prometheus/Loki) is deliberately deferred and is
// NOT done here. A real enrichment step would reuse the query strings already
// encoded in scripts/verify-observability.sh, so the pointer names that script.
func writeObservability(b *strings.Builder) {
	b.WriteString("## Observability\n\n")
	b.WriteString("Live mesh-metric enrichment (Prometheus xDS re-sync, waypoint 5xx, Loki RST logs) is not embedded in this report. For the live operator-facing view, run `scripts/verify-observability.sh`.\n\n")
}

func writeFooter(b *strings.Builder, res model.Result, opts RenderOptions) {
	b.WriteString("---\n\n")
	fmt.Fprintf(b, "schemaVersion: `%s` | recovery bound: %ds | generated at: %s\n",
		mdEscape(res.SchemaVersion), res.RecoveryBoundSeconds, mdEscape(opts.GeneratedAt.UTC().Format(time.RFC3339)))
}

// ---- helpers ---------------------------------------------------------------

// mdEscape neutralizes the characters that could break a Markdown table cell or
// inject markup: the column delimiter, code-span backticks, angle brackets, and
// embedded newlines/carriage returns. Applied to EVERY interpolated data cell;
// static headings and labels are authored literally and never passed through it.
func mdEscape(s string) string {
	return strings.NewReplacer(
		"|", "\\|",
		"`", "\\`",
		"<", "&lt;",
		">", "&gt;",
		"\r", " ",
		"\n", " ",
	).Replace(s)
}

func hasReason(res model.Result, reason string) bool {
	if res.Reason == reason {
		return true
	}
	for _, a := range res.Anomalies {
		if a.Reason == reason {
			return true
		}
	}
	return false
}

func boolWord(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func reasonOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// recoveryCell formats the recovery latency for a per-node or per-client row: a
// dash when nothing was reset, the latency when it recovered, and the literal
// "never" when a reset connection never came back.
func recoveryCell(distinctResets int, recovered bool, secs *float64) string {
	if distinctResets == 0 {
		return "-"
	}
	if recovered && secs != nil {
		return fmt.Sprintf("%.1fs", *secs)
	}
	return "never"
}
