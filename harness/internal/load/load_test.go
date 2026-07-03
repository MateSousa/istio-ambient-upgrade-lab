package load

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// testEcho is an in-process TCP echo server for the hermetic load tests. It
// echoes bytes back, tracks the peak number of concurrent connections, records
// each connection's lifetime, and can optionally break connections to simulate a
// ztunnel drain: breakAfter<0 closes right after accept, breakAfter>0 closes
// after that many echoed messages, breakAfter==0 never breaks.
type testEcho struct {
	ln         net.Listener
	breakAfter int

	mu        sync.Mutex
	cur       int
	peak      int
	durations []time.Duration
}

func newTestEcho(t *testing.T, breakAfter int) *testEcho {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	e := &testEcho{ln: ln, breakAfter: breakAfter}
	go e.serve()
	return e
}

func (e *testEcho) addr() string { return e.ln.Addr().String() }
func (e *testEcho) close()       { _ = e.ln.Close() }

func (e *testEcho) peakVal() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.peak
}

func (e *testEcho) maxDuration() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	var max time.Duration
	for _, d := range e.durations {
		if d > max {
			max = d
		}
	}
	return max
}

func (e *testEcho) serve() {
	for {
		conn, err := e.ln.Accept()
		if err != nil {
			return
		}
		go e.handle(conn)
	}
}

func (e *testEcho) handle(conn net.Conn) {
	start := time.Now()
	e.mu.Lock()
	e.cur++
	if e.cur > e.peak {
		e.peak = e.cur
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.cur--
		e.durations = append(e.durations, time.Since(start))
		e.mu.Unlock()
		_ = conn.Close()
	}()
	if e.breakAfter < 0 {
		return // break immediately after accept
	}
	buf := make([]byte, 64)
	msgs := 0
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
			msgs++
			if e.breakAfter > 0 && msgs >= e.breakAfter {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func parseLoadEvents(r io.Reader) []model.ConnEvent {
	var evs []model.ConnEvent
	dec := json.NewDecoder(r)
	for {
		var e model.ConnEvent
		if err := dec.Decode(&e); err != nil {
			break
		}
		evs = append(evs, e)
	}
	return evs
}

// runLoadCollect runs RunLoad to completion (bounded by dur) and returns the
// parsed ConnEvent stream and the diagnostic (stderr) output.
func runLoadCollect(cfg Config, dur time.Duration) ([]model.ConnEvent, string) {
	var out, diag strings.Builder
	cfg.Out = &out
	cfg.Diag = &diag
	if cfg.ReadinessAddr == "" {
		cfg.ReadinessAddr = ":0"
	}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	_ = RunLoad(ctx, cfg)
	return parseLoadEvents(strings.NewReader(out.String())), diag.String()
}

func countKind(evs []model.ConnEvent, k model.ConnEventKind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func workerPrefix(connID string) string {
	for _, sep := range []string{"-held-", "-new-"} {
		if i := strings.Index(connID, sep); i >= 0 {
			return connID[:i]
		}
	}
	return connID
}

// T1: the generator genuinely sustains the requested concurrency - the in-proc
// echo observes a peak of exactly N concurrent connections. Each worker holds at
// most one echo connection at a time, so within the first churn window (before
// any short-lived connection is recycled) all N are open simultaneously and the
// peak equals N, never exceeding it.
func TestT1_SustainsConcurrency(t *testing.T) {
	echo := newTestEcho(t, 0)
	defer echo.close()

	const n = 8
	cfg := Config{
		Concurrency:   n,
		LongFraction:  0.5,
		Hold:          10 * time.Second,
		KeepAlive:     30 * time.Millisecond,
		ShortInterval: 1 * time.Second, // larger than the observation so no recycle race
		Ramp:          100 * time.Millisecond,
		MaxRPS:        0,
		Node:          "n",
		EchoAddr:      echo.addr(),
		ReadinessAddr: ":0",
		Out:           io.Discard,
		Diag:          io.Discard,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = RunLoad(ctx, cfg); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for echo.peakVal() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	peak := echo.peakVal()
	cancel()
	<-done

	if peak != n {
		t.Fatalf("peak concurrent connections = %d, want exactly %d", peak, n)
	}
}

// T2: a long-lived worker holds its connection for >= Hold and trickles
// keepalives (ConnKeepaliveOK) while it does, with no spurious reset/eof.
func TestT2_LongLivedHoldAndKeepalive(t *testing.T) {
	echo := newTestEcho(t, 0)
	defer echo.close()

	const hold = 300 * time.Millisecond
	cfg := Config{
		Concurrency:   1,
		LongFraction:  1.0,
		Hold:          hold,
		KeepAlive:     40 * time.Millisecond,
		ShortInterval: 40 * time.Millisecond,
		Node:          "n",
		EchoAddr:      echo.addr(),
	}
	evs, _ := runLoadCollect(cfg, 800*time.Millisecond)

	if got := echo.maxDuration(); got < hold-60*time.Millisecond {
		t.Fatalf("longest held connection = %v, want >= ~%v", got, hold)
	}
	if got := countKind(evs, model.ConnKeepaliveOK); got < 3 {
		t.Fatalf("ConnKeepaliveOK count = %d, want >= 3", got)
	}
	if r, e := countKind(evs, model.ConnReset), countKind(evs, model.ConnEOF); r != 0 || e != 0 {
		t.Fatalf("clean hold emitted resets/eofs: reset=%d eof=%d", r, e)
	}
}

// T3: splitWorkers is deterministic (round(n*frac), no RNG) and a live run
// honors the mix - exactly nLong workers hold long-lived connections and nShort
// churn short-lived ones, with disjoint worker sets.
func TestT3_SplitWorkersDeterministic(t *testing.T) {
	cases := []struct {
		n              int
		frac           float64
		wantLong, want int
	}{
		{24, 0.4, 10, 14},
		{10, 0.4, 4, 6},
		{100, 0.4, 40, 60},
		{5, 0.5, 3, 2},
		{4, 0.4, 2, 2},
		{1, 0.4, 0, 1},
		{1, 0.6, 1, 0},
		{0, 0.4, 0, 0},
		{4, -1, 0, 4}, // frac clamped to 0
		{4, 2, 4, 0},  // frac clamped to 1
	}
	for _, tc := range cases {
		long, short := splitWorkers(tc.n, tc.frac)
		if long != tc.wantLong || short != tc.want {
			t.Fatalf("splitWorkers(%d,%v) = (%d,%d), want (%d,%d)", tc.n, tc.frac, long, short, tc.wantLong, tc.want)
		}
		if long+short != tc.n {
			t.Fatalf("splitWorkers(%d,%v): long+short=%d != n", tc.n, tc.frac, long+short)
		}
	}

	// Live mix: 6 workers @ 0.5 => 3 long, 3 short, disjoint.
	echo := newTestEcho(t, 0)
	defer echo.close()
	cfg := Config{
		Concurrency:   6,
		LongFraction:  0.5,
		Hold:          10 * time.Second,
		KeepAlive:     20 * time.Millisecond,
		ShortInterval: 20 * time.Millisecond,
		Ramp:          50 * time.Millisecond,
		Node:          "n",
		EchoAddr:      echo.addr(),
	}
	evs, _ := runLoadCollect(cfg, 400*time.Millisecond)

	longSet, shortSet := map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		switch e.Kind {
		case model.ConnKeepaliveOK, model.ConnReset, model.ConnEOF, model.ConnReconnected:
			longSet[workerPrefix(e.ConnID)] = true
		case model.NewConnAttemptOK, model.NewConnAttemptFail:
			shortSet[workerPrefix(e.ConnID)] = true
		}
	}
	if len(longSet) != 3 {
		t.Fatalf("distinct long-lived workers = %d, want 3 (%v)", len(longSet), longSet)
	}
	if len(shortSet) != 3 {
		t.Fatalf("distinct short-lived workers = %d, want 3 (%v)", len(shortSet), shortSet)
	}
	for p := range longSet {
		if shortSet[p] {
			t.Fatalf("worker %q classified as both long and short", p)
		}
	}
}

// T4: emitted events are well-formed, and a broken long-lived connection
// reconnects carrying the RESET connection's own id (the id-carry contract the
// analyzer keys recovery on).
func TestT4_EventsWellFormedAndIDCarry(t *testing.T) {
	echo := newTestEcho(t, 1) // break after the first echoed keepalive
	defer echo.close()

	cfg := Config{
		Concurrency:  1,
		LongFraction: 1.0,
		Hold:         5 * time.Second,
		KeepAlive:    30 * time.Millisecond,
		Node:         "n",
		EchoAddr:     echo.addr(),
	}
	evs, _ := runLoadCollect(cfg, 400*time.Millisecond)

	for _, e := range evs {
		if e.Kind == "" {
			t.Fatalf("event with empty Kind: %+v", e)
		}
		if e.TS.IsZero() {
			t.Fatalf("event with zero TS: %+v", e)
		}
		if e.Node != "n" {
			t.Fatalf("event Node = %q, want n: %+v", e.Node, e)
		}
	}

	var firstReconnect string
	for _, e := range evs {
		if e.Kind == model.ConnReconnected {
			firstReconnect = e.ConnID
			break
		}
	}
	if firstReconnect == "" {
		t.Fatalf("expected at least one ConnReconnected event, got none: %+v", evs)
	}
	if firstReconnect != "n-w0-held-1" {
		t.Fatalf("first ConnReconnected carried %q, want the reset conn id n-w0-held-1 (id-carry)", firstReconnect)
	}
}

// T5: short-lived churn NEVER emits ConnReset/ConnEOF (or any held-conn event),
// even when the echo RSTs every short connection - so it can never trip
// died-before-drain. It emits only the new-conn-safety signal.
func TestT5_ShortLivedChurnNoResetEOF(t *testing.T) {
	echo := newTestEcho(t, -1) // RST every short connection right after accept
	defer echo.close()

	cfg := Config{
		Concurrency:   4,
		LongFraction:  0.0,
		ShortInterval: 30 * time.Millisecond,
		Ramp:          50 * time.Millisecond,
		Node:          "n",
		EchoAddr:      echo.addr(),
	}
	evs, _ := runLoadCollect(cfg, 300*time.Millisecond)

	for _, k := range []model.ConnEventKind{model.ConnReset, model.ConnEOF, model.ConnKeepaliveOK, model.ConnReconnected} {
		if n := countKind(evs, k); n != 0 {
			t.Fatalf("short-lived churn emitted %s x%d (must be 0)", k, n)
		}
	}
	if countKind(evs, model.NewConnAttemptOK) == 0 {
		t.Fatalf("expected NewConnAttemptOK events from short workers, got none")
	}
}

// T6: new-conn classification. (a) dialing a closed port yields
// NewConnAttemptFail. (b) a failing app-a realistic path lands ONLY in the
// diagnostic counter and never as a NewConnAttemptFail in the analyzed stream.
func TestT6_NewConnClassificationAndAppAIsolation(t *testing.T) {
	t.Run("closed-port-fails", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close() // now nothing is listening on addr

		cfg := Config{
			Concurrency:   2,
			LongFraction:  0.0,
			ShortInterval: 30 * time.Millisecond,
			Node:          "n",
			EchoAddr:      addr,
		}
		evs, _ := runLoadCollect(cfg, 200*time.Millisecond)
		if countKind(evs, model.NewConnAttemptFail) == 0 {
			t.Fatalf("closed port: expected NewConnAttemptFail, got none")
		}
		if got := countKind(evs, model.NewConnAttemptOK); got != 0 {
			t.Fatalf("closed port: got %d NewConnAttemptOK, want 0", got)
		}
	})

	t.Run("appa-failure-never-in-verdict", func(t *testing.T) {
		echo := newTestEcho(t, 0) // healthy echo so short workers succeed
		defer echo.close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError) // app-a 5xx
		}))
		defer srv.Close()

		cfg := Config{
			Concurrency:   2,
			LongFraction:  0.0,
			ShortInterval: 30 * time.Millisecond,
			MaxRPS:        50,
			Node:          "n",
			EchoAddr:      echo.addr(),
			AppAURL:       srv.URL,
		}
		evs, diag := runLoadCollect(cfg, 300*time.Millisecond)

		if got := countKind(evs, model.NewConnAttemptFail); got != 0 {
			t.Fatalf("app-a 5xx leaked into verdict stream as NewConnAttemptFail x%d", got)
		}
		if countKind(evs, model.NewConnAttemptOK) == 0 {
			t.Fatalf("expected NewConnAttemptOK from short workers over the healthy echo")
		}
		var ok, fail int
		if _, err := fmt.Sscanf(diag, "loadgen app-a diagnostics (NON-VERDICT): ok=%d fail=%d", &ok, &fail); err != nil {
			t.Fatalf("parse diag %q: %v", diag, err)
		}
		if fail == 0 {
			t.Fatalf("app-a 5xx failures did not reach the diagnostic counter: %q", diag)
		}
	})
}

// T7: a clean ctx-cancel shutdown emits no reset/eof - deliberate closes are
// silent, so shutting the generator down cannot look like a drop.
func TestT7_CleanShutdownNoResetEOF(t *testing.T) {
	echo := newTestEcho(t, 0)
	defer echo.close()

	cfg := Config{
		Concurrency:   4,
		LongFraction:  0.5,
		Hold:          10 * time.Second,
		KeepAlive:     30 * time.Millisecond,
		ShortInterval: 30 * time.Millisecond,
		Ramp:          50 * time.Millisecond,
		Node:          "n",
		EchoAddr:      echo.addr(),
	}
	evs, _ := runLoadCollect(cfg, 200*time.Millisecond)

	if r, e := countKind(evs, model.ConnReset), countKind(evs, model.ConnEOF); r != 0 || e != 0 {
		t.Fatalf("clean shutdown emitted reset=%d eof=%d, want 0/0", r, e)
	}
}

// TestConfigFromEnvHoldFloor pins the HOLD_MS validation: a hold shorter than
// the drain window is rejected so the generator can never be deployed unable to
// observe the reset it exists to measure.
func TestConfigFromEnvHoldFloor(t *testing.T) {
	t.Setenv("ECHO_ADDR", "echo:9000")
	t.Setenv("HOLD_MS", "5000")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatalf("HOLD_MS below the floor must be rejected")
	}
	t.Setenv("HOLD_MS", "130000")
	if _, err := ConfigFromEnv(); err != nil {
		t.Fatalf("HOLD_MS at the floor must be accepted, got %v", err)
	}
}
