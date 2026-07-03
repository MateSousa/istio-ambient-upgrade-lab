// Package model holds the pure data types shared between the measurement
// analyzer (internal/measure) and the live IO layer (internal/live + cmd).
//
// STRICT boundary: this package imports only the standard library, and the
// analyzer imports only this package. That is what keeps the analyzer verdict
// logic hermetic - it can be exercised with synthetic connection-event streams
// and never needs a cluster, a socket, or a clock.
package model

import "time"

// SchemaVersion tags the Result JSON. Slice 7's report generator keys off this;
// bump only on a breaking change to the Result contract.
const SchemaVersion = "harness/v1"

// ConnEventKind classifies a single observed connection-lifecycle event.
type ConnEventKind string

const (
	// NewConnAttemptOK - a freshly dialed connection succeeded (new-conn safety).
	NewConnAttemptOK ConnEventKind = "NewConnAttemptOK"
	// NewConnAttemptFail - a freshly dialed connection failed. Any of these fail
	// the run: new connections must never fail during a ztunnel roll.
	NewConnAttemptFail ConnEventKind = "NewConnAttemptFail"
	// ConnReset - a held connection was RST (ECONNRESET) at the socket level.
	// This is the unit of harm the drill measures.
	ConnReset ConnEventKind = "ConnReset"
	// ConnEOF - a held connection was closed with a graceful FIN. ztunnel RSTs
	// rather than FINs on drain, so an EOF is not an upgrade-attributable reset;
	// it is tracked only to catch a connection dying before the drain begins.
	ConnEOF ConnEventKind = "ConnEOF"
	// ConnKeepaliveOK - the held connection answered a keepalive round-trip.
	ConnKeepaliveOK ConnEventKind = "ConnKeepaliveOK"
	// ConnReconnected - a previously reset held connection was re-established.
	// The gap between its ConnReset and this event is the recovery time.
	ConnReconnected ConnEventKind = "ConnReconnected"
)

// ConnEvent is one observation from a probe. Node is where the probe physically
// runs; attribution of a reset, however, is by which roll window the timestamp
// falls in, NOT by this field (see internal/measure).
type ConnEvent struct {
	Kind   ConnEventKind `json:"kind"`
	ConnID string        `json:"connId"`
	Node   string        `json:"node"`
	TS     time.Time     `json:"ts"`
}

// RollWindow is the attribution window for one old ztunnel pod on one node.
//
// [LOAD-BEARING] The window is [DrainingAt, GraceExpiresAt] where
// GraceExpiresAt = DrainingAt + grace (120s here). It is NOT closed on the new
// pod's ReadyAt: with the ztunnel DaemonSet's maxSurge:1/maxUnavailable:0 the
// new pod goes Ready BEFORE the old pod drains, and the RST fires at
// ~DrainingAt+115s - inside [DrainingAt, GraceExpiresAt]. ReadyAt is retained
// only as a recovery/progress marker and must never be used as the window end.
type RollWindow struct {
	Node           string     `json:"node"`
	DrainingAt     time.Time  `json:"drainingAt"`
	GraceExpiresAt time.Time  `json:"graceExpiresAt"`
	ReadyAt        *time.Time `json:"readyAt,omitempty"`
}

// TriggerInfo describes how the ztunnel roll was provoked. It is copied
// verbatim into Result.Trigger, so it is part of the stable JSON contract.
type TriggerInfo struct {
	// Kind is "git-bump" (the AC-satisfying primary path) or "rollout-restart"
	// (a dev escape hatch that does not satisfy the acceptance criteria).
	Kind string `json:"kind"`
	// ACSatisfying is true only for git-bump; false marks rollout-restart runs.
	ACSatisfying     bool   `json:"acSatisfying"`
	ChartVersionFrom string `json:"chartVersionFrom,omitempty"`
	ChartVersionTo   string `json:"chartVersionTo,omitempty"`
	ZtunnelTagFrom   string `json:"ztunnelTagFrom,omitempty"`
	ZtunnelTagTo     string `json:"ztunnelTagTo,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

// Corroboration is a secondary, non-verdict signal from app-a's own logs that
// its pooled connection was reset, backing up the probe measurement.
type Corroboration struct {
	AppAConnReset bool   `json:"appAConnReset"`
	Note          string `json:"note"`
}

// Anomaly is a single reason the run is not a clean PASS, with the offending
// connection/node/timestamp where applicable.
type Anomaly struct {
	Reason string    `json:"reason"`
	ConnID string    `json:"connId,omitempty"`
	Node   string    `json:"node,omitempty"`
	TS     time.Time `json:"ts,omitempty"`
}

// Anomaly / verdict reason constants. These strings are part of the contract.
const (
	// ERROR-class reasons: the measurement itself cannot be trusted.
	ReasonNoRolloutObserved    = "no-rollout-observed"
	ReasonDiedBeforeDrain      = "died-before-drain"
	ReasonHalfOpenWindow       = "half-open-window"
	ReasonTriggerPrereqMissing = "trigger-prereq-missing"

	// FAIL-class reasons: a real drop violated the accepted-harm bound.
	ReasonNewConnFailure         = "new-conn-failure"
	ReasonDropOnNonUpgradingNode = "drop-on-non-upgrading-node"
	ReasonOverlappingNodeWindows = "overlapping-node-windows"
	ReasonSameConnTwoWindows     = "same-conn-reset-in-2-windows"
	ReasonPoolNotRecovered       = "pool-not-recovered"
	ReasonRecoveryOverBound      = "recovery-over-bound"
)

// Verdict values.
const (
	VerdictPass  = "PASS"
	VerdictFail  = "FAIL"
	VerdictError = "ERROR"
)

// WindowJSON is the window shape embedded per node in the Result.
type WindowJSON struct {
	DrainingAt     time.Time  `json:"drainingAt"`
	GraceExpiresAt time.Time  `json:"graceExpiresAt"`
	ReadyAt        *time.Time `json:"readyAt"`
}

// PerNodeAttribution is the per-(node,window) breakdown of resets.
type PerNodeAttribution struct {
	Node               string     `json:"node"`
	Window             WindowJSON `json:"window"`
	RawResets          int        `json:"rawResets"`
	DistinctConnsReset int        `json:"distinctConnsReset"`
	Recovered          bool       `json:"recovered"`
	RecoverySeconds    *float64   `json:"recoverySeconds"`
}

// Result is the stable machine-readable output the harness prints/persists and
// slice 7 consumes. RecoverySeconds is the max across affected connections and
// is null (nil) if ANY affected connection never recovered.
type Result struct {
	SchemaVersion         string               `json:"schemaVersion"`
	Verdict               string               `json:"verdict"`
	Reason                string               `json:"reason,omitempty"`
	Trigger               TriggerInfo          `json:"trigger"`
	NewConnFailures       int                  `json:"newConnFailures"`
	ExistingConnRSTs      int                  `json:"existingConnRSTs"`
	AffectedNodeWindows   int                  `json:"affectedNodeWindows"`
	DistinctNodesAffected int                  `json:"distinctNodesAffected"`
	RecoverySeconds       *float64             `json:"recoverySeconds"`
	RecoveryBoundSeconds  int                  `json:"recoveryBoundSeconds"`
	PerNodeAttribution    []PerNodeAttribution `json:"perNodeAttribution"`
	Corroboration         Corroboration        `json:"corroboration"`
	Anomalies             []Anomaly            `json:"anomalies"`
}

// Config carries the analyzer's tunables and the facts the live layer resolves
// (whether the trigger fired, any prerequisite error, app-a corroboration).
type Config struct {
	// RecoveryBoundSeconds - an affected connection must reconnect within this
	// many seconds or the run FAILs. Default 30.
	RecoveryBoundSeconds int
	// GraceSeconds - ztunnel terminationGracePeriodSeconds (120 here). Used only
	// to derive GraceExpiresAt when the live layer builds windows; the analyzer
	// reads GraceExpiresAt off the window directly.
	GraceSeconds int
	// JitterToleranceSeconds - epsilon (default ~2s) absorbing kind's per-node
	// scheduling jitter so adjacent staggered windows that merely touch are not
	// falsely flagged as overlapping, and a reset landing a hair outside a
	// window boundary is still attributed.
	JitterToleranceSeconds float64
	// TriggerFired - did the upgrade trigger actually run? If true with zero
	// windows the analyzer returns ERROR no-rollout-observed.
	TriggerFired bool
	// TriggerPrereqError - non-empty when a trigger precondition was missing
	// (e.g. no GHCR_TOKEN for git-bump); yields ERROR trigger-prereq-missing.
	TriggerPrereqError string
	// AppACorroboration - optional secondary signal copied into the Result.
	AppACorroboration *Corroboration
}

// Input is the complete, self-contained argument to measure.Analyze. Everything
// the verdict depends on lives here; the analyzer reads nothing else.
type Input struct {
	Trigger TriggerInfo
	Events  []ConnEvent
	Windows []RollWindow
	Config  Config
}

// DefaultConfig returns the analyzer defaults (30s recovery bound, 120s grace,
// 2s jitter tolerance, trigger assumed fired).
func DefaultConfig() Config {
	return Config{
		RecoveryBoundSeconds:   30,
		GraceSeconds:           120,
		JitterToleranceSeconds: 2,
		TriggerFired:           true,
	}
}
