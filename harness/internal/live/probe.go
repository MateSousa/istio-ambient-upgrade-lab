package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/probeconn"
)

// ProbeConfig configures a single probe process. One probe runs per worker
// node, dialing the co-located echo Pod so exactly one ztunnel mediates.
type ProbeConfig struct {
	EchoAddr    string        // same-node echo Pod IP:port
	Node        string        // node this probe is pinned to (for event.Node)
	KeepAlive   time.Duration // trickle/keepalive period on the held conn (~20s)
	NewConnRate time.Duration // how often to dial a fresh probe connection
	HealthAddr  string        // readiness health listen addr (opened after first held connect)
	Out         io.Writer     // JSON-line ConnEvent sink (stdout in the Pod)
}

// ProbeConfigFromEnv builds a ProbeConfig from the probe Pod's environment:
// ECHO_ADDR (required), NODE_NAME (downward API), KEEPALIVE_MS (default 20000),
// NEWCONN_MS (default 2000), READINESS_ADDR (default ":8080").
func ProbeConfigFromEnv() (ProbeConfig, error) {
	addr := os.Getenv("ECHO_ADDR")
	if addr == "" {
		return ProbeConfig{}, errors.New("ECHO_ADDR must be set (same-node echo Pod IP:port)")
	}
	keep := probeconn.EnvDurationMS("KEEPALIVE_MS", 20000)
	newc := probeconn.EnvDurationMS("NEWCONN_MS", 2000)
	health := os.Getenv("READINESS_ADDR")
	if health == "" {
		health = ":8080"
	}
	return ProbeConfig{
		EchoAddr:    addr,
		Node:        os.Getenv("NODE_NAME"),
		KeepAlive:   keep,
		NewConnRate: newc,
		HealthAddr:  health,
		Out:         os.Stdout,
	}, nil
}

// RunProbe runs the two probe loops until ctx is cancelled:
//   - a HELD connection that mirrors app-a's pooled client: it stays open and
//     trickles a keepalive byte every KeepAlive; when it breaks it classifies
//     the close as ConnReset (ECONNRESET) vs ConnEOF (FIN) at the socket level,
//     then reconnects (emitting ConnReconnected).
//   - a NEW-connection loop that dials a fresh connection every NewConnRate and
//     records ok/fail (the new-connection-safety signal).
//
// Every observation is emitted as a JSON-line model.ConnEvent to cfg.Out. The
// held-connection primitives (id-carry, trickle, socket classification,
// readiness gate) live in internal/probeconn and are shared with the load
// generator.
func RunProbe(ctx context.Context, cfg ProbeConfig) error {
	enc := json.NewEncoder(cfg.Out)
	emit := func(kind model.ConnEventKind, connID string) {
		_ = enc.Encode(model.ConnEvent{Kind: kind, ConnID: connID, Node: cfg.Node, TS: time.Now().UTC()})
	}

	gate := probeconn.NewReadinessGate(cfg.HealthAddr)
	defer gate.Close()

	go newConnLoop(ctx, cfg, emit)
	heldConnLoop(ctx, cfg, emit, gate.SignalReady)
	return ctx.Err()
}

// heldConnLoop keeps a single long-lived connection open, trickling a keepalive
// on it, reconnecting after a break. The connId is stable across the life of
// one connection instance and regenerated on reconnect, so the analyzer can
// dedup resets per (connId, window) and measure recovery reset->reconnect. The
// ConnReconnected recovering connection N's reset is emitted with connection N's
// id (carried forward via probeconn.HeldConnSeq), which is the id the analyzer
// keys recovery on. onReady is invoked once, after the first successful dial.
func heldConnLoop(ctx context.Context, cfg ProbeConfig, emit probeconn.EmitFunc, onReady func()) {
	seq := probeconn.NewHeldConnSeq(cfg.Node)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := net.DialTimeout("tcp", cfg.EchoAddr, 5*time.Second)
		if err != nil {
			// Cannot establish the held connection at all; wait and retry.
			time.Sleep(time.Second)
			continue
		}
		connID, reconnectID, isReconnect := seq.Next()
		if isReconnect {
			emit(model.ConnReconnected, reconnectID)
		} else if onReady != nil {
			onReady()
		}
		reason := probeconn.HoldAndTrickle(ctx, conn, cfg.KeepAlive, connID, emit)
		conn.Close()
		if reason == probeconn.CloseReasonCtx {
			return
		}
	}
}

// newConnLoop dials a fresh connection on an interval and records success or
// failure - the new-connection-safety signal (expected: always ok).
func newConnLoop(ctx context.Context, cfg ProbeConfig, emit probeconn.EmitFunc) {
	var gen uint64
	ticker := time.NewTicker(cfg.NewConnRate)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		connID := fmt.Sprintf("%s-new-%d", cfg.Node, atomic.AddUint64(&gen, 1))
		conn, err := net.DialTimeout("tcp", cfg.EchoAddr, 3*time.Second)
		if err != nil {
			emit(model.NewConnAttemptFail, connID)
			continue
		}
		emit(model.NewConnAttemptOK, connID)
		conn.Close()
	}
}
