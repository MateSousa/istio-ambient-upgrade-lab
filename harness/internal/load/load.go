// Package load is the concurrent load generator (slice 4). It drives the mesh
// under configurable concurrency, holding a realistic mix of long-lived and
// short-lived connections open so the ztunnel upgrade is observed under pressure
// rather than with a single idle probe connection.
//
// It runs THREE distinct traffic paths, deliberately kept apart so app failures
// can never contaminate the drop verdict:
//
//   - Long-lived workers: a raw TCP connection to the SAME-NODE echo, held for
//     >= Hold, trickled every KeepAlive (emitting ConnKeepaliveOK), classified
//     on break into ConnReset (RST) / ConnEOF (FIN) and reconnected carrying the
//     RESET connection's id (probeconn.HeldConnSeq - the slice-3 lesson). These
//     are the measured held connections at risk during the drain.
//   - Short-lived verdict workers: raw TCP to the SAME-NODE echo, churned every
//     ShortInterval. They emit ONLY NewConnAttemptOK / NewConnAttemptFail - the
//     new-connection-safety signal. A fail can therefore ONLY mean a ztunnel
//     new-conn drop; a deliberate close emits nothing.
//   - Realistic app-a traffic (NON-VERDICT): GET AppAURL with keep-alives
//     DISABLED (a fresh TCP per request), rate-limited by an aggregate MaxRPS
//     token bucket. This exercises the real app -> pgbouncer -> postgres path
//     under load, but its failures (pool exhaustion, unready, 5xx) go to a
//     STDERR diagnostic counter ONLY - they are NEVER emitted as
//     NewConnAttemptFail and can never reach the analyzed verdict stream.
//
// The verdict-bearing IO shape (ConnEvent JSON lines) and the held-connection
// primitives are shared with the per-node probe via internal/probeconn.
package load

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/probeconn"
)

// minHoldMS is the validated floor for HOLD_MS: the held connections must
// outlive the ~115s ztunnel drain (grace 120s), so a hold shorter than the
// drain window could never observe the reset it exists to measure. 130s gives
// margin over the ~115s internal drain.
const minHoldMS = 130000

// Config carries the load generator's tunables. It is env-first (ConfigFromEnv)
// but every field is settable directly, which is what lets the hermetic tests
// use sub-second Hold/KeepAlive values that ConfigFromEnv would reject.
type Config struct {
	Concurrency   int           // total concurrent workers (CONCURRENCY, default 24)
	LongFraction  float64       // fraction held long-lived (LONG_FRACTION, default 0.4)
	Hold          time.Duration // long-lived hold duration (HOLD_MS, >= 130000)
	KeepAlive     time.Duration // held-conn trickle period (KEEPALIVE_MS, default 20000)
	ShortInterval time.Duration // short-lived churn period (SHORT_INTERVAL_MS, default 250)
	Ramp          time.Duration // staggered worker start window (RAMP_MS, default 10000)
	MaxRPS        int           // aggregate app-a request cap (MAX_RPS, default 40)
	Duration      time.Duration // run duration; 0 = until ctx cancel (DURATION_MS)
	Node          string        // node this pod runs on (NODE_NAME, downward API)
	ReadinessAddr string        // readiness gate listen addr (READINESS_ADDR, :8080)
	EchoAddr      string        // same-node echo IP:port (ECHO_ADDR, required)
	AppAURL       string        // app-a query URL (APPA_URL); empty disables app-a traffic
	Out           io.Writer     // JSON-line ConnEvent sink (stdout in the Pod)
	Diag          io.Writer     // non-verdict diagnostic sink (stderr in the Pod)
}

// ConfigFromEnv builds a Config from the load Pod's environment, validating the
// HOLD_MS floor. ECHO_ADDR is required. Out is stdout (the analyzed stream);
// Diag is stderr (never analyzed).
func ConfigFromEnv() (Config, error) {
	addr := os.Getenv("ECHO_ADDR")
	if addr == "" {
		return Config{}, errors.New("ECHO_ADDR must be set (same-node echo Pod IP:port)")
	}
	holdMS := envInt("HOLD_MS", minHoldMS)
	if holdMS < minHoldMS {
		return Config{}, fmt.Errorf("HOLD_MS=%d is below the %d ms floor (held conns must outlive the ~115s ztunnel drain)", holdMS, minHoldMS)
	}
	readiness := os.Getenv("READINESS_ADDR")
	if readiness == "" {
		readiness = ":8080"
	}
	return Config{
		Concurrency:   envInt("CONCURRENCY", 24),
		LongFraction:  envFloat("LONG_FRACTION", 0.4),
		Hold:          time.Duration(holdMS) * time.Millisecond,
		KeepAlive:     probeconn.EnvDurationMS("KEEPALIVE_MS", 20000),
		ShortInterval: probeconn.EnvDurationMS("SHORT_INTERVAL_MS", 250),
		Ramp:          probeconn.EnvDurationMS("RAMP_MS", 10000),
		MaxRPS:        envInt("MAX_RPS", 40),
		Duration:      probeconn.EnvDurationMS("DURATION_MS", 0),
		Node:          os.Getenv("NODE_NAME"),
		ReadinessAddr: readiness,
		EchoAddr:      addr,
		AppAURL:       os.Getenv("APPA_URL"),
		Out:           os.Stdout,
		Diag:          os.Stderr,
	}, nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// splitWorkers deterministically partitions n workers into long-lived and
// short-lived counts using long = round(n * frac) (no RNG), so a given
// (n, frac) always yields the same mix across pods and test runs. frac is
// clamped to [0,1] and long to [0,n].
func splitWorkers(n int, frac float64) (long, short int) {
	if n <= 0 {
		return 0, 0
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	long = int(math.Round(float64(n) * frac))
	if long < 0 {
		long = 0
	}
	if long > n {
		long = n
	}
	return long, n - long
}

// RunLoad runs the load generator until ctx is cancelled (or Duration elapses).
// It opens the readiness gate only after ALL long-lived workers have established
// their first connection, so the trigger fires under full held-conn pressure.
func RunLoad(ctx context.Context, cfg Config) error {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.Diag == nil {
		cfg.Diag = os.Stderr
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}

	var encMu sync.Mutex
	enc := json.NewEncoder(cfg.Out)
	emit := func(kind model.ConnEventKind, connID string) {
		encMu.Lock()
		_ = enc.Encode(model.ConnEvent{Kind: kind, ConnID: connID, Node: cfg.Node, TS: time.Now().UTC()})
		encMu.Unlock()
	}

	nLong, nShort := splitWorkers(cfg.Concurrency, cfg.LongFraction)

	gate := probeconn.NewReadinessGate(cfg.ReadinessAddr)
	defer gate.Close()

	// [BOARD FIX 2] Honest readiness: open the gate only once EVERY long-lived
	// worker has connected, so a startup flake on any held connection cannot
	// later masquerade as died-before-drain and the trigger fires under the full
	// held-conn load. With no long workers there is nothing to hold, so signal
	// immediately.
	var longRemaining int64 = int64(nLong)
	onLongConnected := func() {
		if atomic.AddInt64(&longRemaining, -1) == 0 {
			gate.SignalReady()
		}
	}
	if nLong == 0 {
		gate.SignalReady()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	diag := &diagCounter{}

	var wg sync.WaitGroup

	// Staggered start over Ramp: worker i begins after i*(Ramp/N).
	stagger := time.Duration(0)
	if cfg.Concurrency > 0 {
		stagger = cfg.Ramp / time.Duration(cfg.Concurrency)
	}
	for i := 0; i < cfg.Concurrency; i++ {
		i := i
		isLong := i < nLong
		delay := time.Duration(i) * stagger
		wg.Add(1)
		go func() {
			defer wg.Done()
			if delay > 0 {
				select {
				case <-runCtx.Done():
					return
				case <-time.After(delay):
				}
			}
			workerNode := fmt.Sprintf("%s-w%d", cfg.Node, i)
			if isLong {
				longWorker(runCtx, cfg, workerNode, emit, onLongConnected)
			} else {
				shortWorker(runCtx, cfg, workerNode, emit)
			}
		}()
	}

	// Realistic app-a traffic: a separate pool bounded by the aggregate MaxRPS
	// token bucket. Failures land in diag only (NON-VERDICT).
	if cfg.AppAURL != "" && cfg.MaxRPS > 0 {
		tokens := tokenBucket(runCtx, cfg.MaxRPS)
		appAWorkers := nShort
		if appAWorkers < 1 {
			appAWorkers = 1
		}
		for i := 0; i < appAWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				appATraffic(runCtx, cfg, tokens, diag)
			}()
		}
	}

	// Run until Duration elapses (if set) or ctx is cancelled.
	if cfg.Duration > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(cfg.Duration):
		}
	} else {
		<-ctx.Done()
	}
	cancel()
	wg.Wait()

	diag.flush(cfg.Diag)
	return ctx.Err()
}

// longWorker holds one raw-TCP connection to the same-node echo for up to Hold,
// then closes it silently and re-establishes a fresh held session. Within a
// session a real break (RST/EOF) is classified and the connection reconnected
// carrying the reset connection's id. onFirstConn fires exactly once, on this
// worker's very first successful dial, to drive the honest readiness gate.
func longWorker(ctx context.Context, cfg Config, node string, emit probeconn.EmitFunc, onFirstConn func()) {
	firstDone := false
	for {
		if ctx.Err() != nil {
			return
		}
		// Bound one held session by Hold; a session ends either on a deliberate
		// Hold/ctx close (silent) or keeps recovering in-place across real breaks.
		sessCtx, sessCancel := context.WithTimeout(ctx, cfg.Hold)
		heldSession(sessCtx, cfg, node, emit, &firstDone, onFirstConn)
		sessCancel()
		if ctx.Err() != nil {
			return
		}
		// Hold elapsed: the deliberate close already happened silently inside the
		// session; start a fresh session (new id lineage) to sustain concurrency.
	}
}

// heldSession runs one held-connection lifetime bounded by sessCtx. It mirrors
// the probe's held loop: dial, on reconnect emit ConnReconnected carrying the
// prior (reset) id, trickle keepalives, and on a deliberate sessCtx close return
// silently. A real break (RST/EOF) is emitted by HoldAndTrickle and the loop
// reconnects within the same session.
func heldSession(sessCtx context.Context, cfg Config, node string, emit probeconn.EmitFunc, firstDone *bool, onFirstConn func()) {
	seq := probeconn.NewHeldConnSeq(node)
	for {
		if sessCtx.Err() != nil {
			return
		}
		conn, err := net.DialTimeout("tcp", cfg.EchoAddr, 5*time.Second)
		if err != nil {
			select {
			case <-sessCtx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		connID, reconnectID, isReconnect := seq.Next()
		if isReconnect {
			emit(model.ConnReconnected, reconnectID)
		} else if !*firstDone {
			*firstDone = true
			if onFirstConn != nil {
				onFirstConn()
			}
		}
		reason := probeconn.HoldAndTrickle(sessCtx, conn, cfg.KeepAlive, connID, emit)
		conn.Close()
		if reason == probeconn.CloseReasonCtx {
			// Deliberate Hold/ctx close: no reconnect, end the session silently.
			return
		}
		// Real break already emitted; loop dials again -> seq.Next marks a
		// reconnect and emits ConnReconnected carrying this conn's id.
	}
}

// shortWorker churns short-lived raw-TCP connections to the same-node echo,
// emitting ONLY NewConnAttemptOK / NewConnAttemptFail. Each connection is held
// for one ShortInterval then closed silently (a deliberate close is not a drop),
// so a short-lived connection RST never becomes a ConnReset/ConnEOF and can
// never trip died-before-drain. A dial failure is the pure new-conn-safety fail
// signal.
func shortWorker(ctx context.Context, cfg Config, node string, emit probeconn.EmitFunc) {
	var gen uint64
	for {
		if ctx.Err() != nil {
			return
		}
		connID := fmt.Sprintf("%s-new-%d", node, atomic.AddUint64(&gen, 1))
		conn, err := net.DialTimeout("tcp", cfg.EchoAddr, 3*time.Second)
		if err != nil {
			emit(model.NewConnAttemptFail, connID)
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.ShortInterval):
			}
			continue
		}
		emit(model.NewConnAttemptOK, connID)
		// Hold this short-lived connection for one interval, then close it
		// silently and churn a fresh one. Each worker holds at most one echo
		// connection at a time (so aggregate concurrency == N).
		select {
		case <-ctx.Done():
			conn.Close()
			return
		case <-time.After(cfg.ShortInterval):
		}
		conn.Close()
	}
}

// diagCounter accumulates the NON-VERDICT app-a traffic outcomes. It is written
// to Diag (stderr) only and never influences the analyzed ConnEvent stream.
type diagCounter struct {
	appAOK   atomic.Int64
	appAFail atomic.Int64
}

func (d *diagCounter) flush(w io.Writer) {
	fmt.Fprintf(w, "loadgen app-a diagnostics (NON-VERDICT): ok=%d fail=%d\n",
		d.appAOK.Load(), d.appAFail.Load())
}

// appATraffic drives realistic app-a requests with keep-alives disabled (fresh
// TCP per request), gated by the shared MaxRPS token bucket. Every outcome is
// recorded in diag ONLY; nothing here ever emits a ConnEvent, so app-a's own
// pool/unready/5xx failures can never reach the drop verdict.
func appATraffic(ctx context.Context, cfg Config, tokens <-chan struct{}, diag *diagCounter) {
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tokens:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.AppAURL, nil)
		if err != nil {
			diag.appAFail.Add(1)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			diag.appAFail.Add(1)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			diag.appAFail.Add(1)
		} else {
			diag.appAOK.Add(1)
		}
	}
}

// tokenBucket emits a token at rps/second (small burst buffer), stopping on ctx.
func tokenBucket(ctx context.Context, rps int) <-chan struct{} {
	ch := make(chan struct{}, rps)
	if rps <= 0 {
		return ch
	}
	go func() {
		interval := time.Second / time.Duration(rps)
		if interval <= 0 {
			interval = time.Millisecond
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	return ch
}
