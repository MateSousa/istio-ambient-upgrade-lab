package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// ProbeConfig configures a single probe process. One probe runs per worker
// node, dialing the co-located echo Pod so exactly one ztunnel mediates.
type ProbeConfig struct {
	EchoAddr    string        // same-node echo Pod IP:port
	Node        string        // node this probe is pinned to (for event.Node)
	KeepAlive   time.Duration // trickle/keepalive period on the held conn (~20s)
	NewConnRate time.Duration // how often to dial a fresh probe connection
	Out         io.Writer     // JSON-line ConnEvent sink (stdout in the Pod)
}

// ProbeConfigFromEnv builds a ProbeConfig from the probe Pod's environment:
// ECHO_ADDR (required), NODE_NAME (downward API), KEEPALIVE_MS (default 20000),
// NEWCONN_MS (default 2000).
func ProbeConfigFromEnv() (ProbeConfig, error) {
	addr := os.Getenv("ECHO_ADDR")
	if addr == "" {
		return ProbeConfig{}, errors.New("ECHO_ADDR must be set (same-node echo Pod IP:port)")
	}
	keep := envDurationMS("KEEPALIVE_MS", 20000)
	newc := envDurationMS("NEWCONN_MS", 2000)
	return ProbeConfig{
		EchoAddr:    addr,
		Node:        os.Getenv("NODE_NAME"),
		KeepAlive:   keep,
		NewConnRate: newc,
		Out:         os.Stdout,
	}, nil
}

func envDurationMS(key string, def int) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Millisecond
		}
	}
	return time.Duration(def) * time.Millisecond
}

// RunProbe runs the two probe loops until ctx is cancelled:
//   - a HELD connection that mirrors app-a's pooled client: it stays open and
//     trickles a keepalive byte every KeepAlive; when it breaks it classifies
//     the close as ConnReset (ECONNRESET) vs ConnEOF (FIN) at the socket level,
//     then reconnects (emitting ConnReconnected).
//   - a NEW-connection loop that dials a fresh connection every NewConnRate and
//     records ok/fail (the new-connection-safety signal).
//
// Every observation is emitted as a JSON-line model.ConnEvent to cfg.Out.
func RunProbe(ctx context.Context, cfg ProbeConfig) error {
	enc := json.NewEncoder(cfg.Out)
	emit := func(kind model.ConnEventKind, connID string) {
		_ = enc.Encode(model.ConnEvent{Kind: kind, ConnID: connID, Node: cfg.Node, TS: time.Now().UTC()})
	}

	go newConnLoop(ctx, cfg, emit)
	heldConnLoop(ctx, cfg, emit)
	return ctx.Err()
}

type emitFunc func(kind model.ConnEventKind, connID string)

// heldConnLoop keeps a single long-lived connection open, trickling a keepalive
// on it, reconnecting after a break. The connId is stable across the life of
// one connection instance and regenerated on reconnect, so the analyzer can
// dedup resets per (connId, window) and measure recovery reset->reconnect.
func heldConnLoop(ctx context.Context, cfg ProbeConfig, emit emitFunc) {
	var gen uint64
	firstConnect := true
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		connID := fmt.Sprintf("%s-held-%d", cfg.Node, atomic.AddUint64(&gen, 1))
		conn, err := net.DialTimeout("tcp", cfg.EchoAddr, 5*time.Second)
		if err != nil {
			// Cannot establish the held connection at all; wait and retry.
			time.Sleep(time.Second)
			continue
		}
		if firstConnect {
			firstConnect = false
		} else {
			emit(model.ConnReconnected, connID)
		}
		reason := holdAndTrickle(ctx, conn, cfg, connID, emit)
		conn.Close()
		if reason == closeReasonCtx {
			return
		}
	}
}

type closeReason int

const (
	closeReasonReset closeReason = iota
	closeReasonEOF
	closeReasonCtx
	closeReasonOther
)

// holdAndTrickle keeps conn open, writing a small keepalive each period and
// reading the echo back. It returns why the connection ended.
func holdAndTrickle(ctx context.Context, conn net.Conn, cfg ProbeConfig, connID string, emit emitFunc) closeReason {
	ticker := time.NewTicker(cfg.KeepAlive)
	defer ticker.Stop()
	buf := make([]byte, 16)
	for {
		select {
		case <-ctx.Done():
			return closeReasonCtx
		case <-ticker.C:
		}
		if _, err := conn.Write([]byte("k")); err != nil {
			emit(classify(err), connID)
			return classifyReason(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(cfg.KeepAlive))
		if _, err := conn.Read(buf); err != nil {
			// A read deadline timeout with no bytes is not a close; only a real
			// FIN/RST ends the held connection.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			emit(classify(err), connID)
			return classifyReason(err)
		}
		emit(model.ConnKeepaliveOK, connID)
	}
}

// newConnLoop dials a fresh connection on an interval and records success or
// failure - the new-connection-safety signal (expected: always ok).
func newConnLoop(ctx context.Context, cfg ProbeConfig, emit emitFunc) {
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

// classify maps a socket error to a ConnEvent kind at the syscall level: an
// ECONNRESET is the ztunnel-drain RST we measure; io.EOF is a graceful FIN.
func classify(err error) model.ConnEventKind {
	switch classifyReason(err) {
	case closeReasonReset:
		return model.ConnReset
	default:
		return model.ConnEOF
	}
}

func classifyReason(err error) closeReason {
	if err == nil {
		return closeReasonOther
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return closeReasonReset
	}
	if errors.Is(err, io.EOF) {
		return closeReasonEOF
	}
	// A connection reset can surface wrapped in an *net.OpError without a clean
	// errors.Is match on some platforms; fall back to EOF (graceful) otherwise.
	return closeReasonEOF
}
