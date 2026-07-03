// Package probeconn holds the shared held-connection primitives used by both the
// per-node probe (internal/live) and the concurrent load generator
// (internal/load).
//
// STRICT boundary: this package imports ONLY the standard library and
// internal/model. It has NO knowledge of client-go, the live IO layer, or any
// Pod/Config type, so the id-carry contract and the socket-close classification
// can be unit tested without a cluster (see probeconn_test.go). The two callers
// each supply their own config/dial policy and reuse these leaf primitives.
package probeconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// EmitFunc is the JSON-line ConnEvent sink shape shared by every connection
// loop: given an event kind and the connId it pertains to, record one
// observation. The caller binds Node/timestamp/output when it constructs it.
type EmitFunc func(kind model.ConnEventKind, connID string)

// EnvDurationMS reads an integer-millisecond value from env var key, returning
// def (in ms) when unset or unparimeable.
func EnvDurationMS(key string, def int) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Millisecond
		}
	}
	return time.Duration(def) * time.Millisecond
}

// CloseReason classifies why a held connection ended.
type CloseReason int

const (
	// CloseReasonReset - the socket ended with ECONNRESET (the ztunnel-drain RST).
	CloseReasonReset CloseReason = iota
	// CloseReasonEOF - the socket ended with a graceful FIN (io.EOF).
	CloseReasonEOF
	// CloseReasonCtx - the loop was cancelled via context (a deliberate close).
	CloseReasonCtx
	// CloseReasonOther - anything else.
	CloseReasonOther
)

// Classify maps a socket error to a ConnEvent kind at the syscall level: an
// ECONNRESET is the ztunnel-drain RST the drill measures; io.EOF is a graceful
// FIN. Anything ambiguous is treated as a graceful EOF (never a false RST).
func Classify(err error) model.ConnEventKind {
	switch ClassifyReason(err) {
	case CloseReasonReset:
		return model.ConnReset
	default:
		return model.ConnEOF
	}
}

// ClassifyReason maps a socket error to a CloseReason. ECONNRESET => reset,
// io.EOF => eof, otherwise eof (graceful) so an ambiguous close never surfaces
// as a false upgrade-attributable reset.
func ClassifyReason(err error) CloseReason {
	if err == nil {
		return CloseReasonOther
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return CloseReasonReset
	}
	if errors.Is(err, io.EOF) {
		return CloseReasonEOF
	}
	// A connection reset can surface wrapped in an *net.OpError without a clean
	// errors.Is match on some platforms; fall back to EOF (graceful) otherwise.
	return CloseReasonEOF
}

// HeldConnSeq generates the per-connection ids for a held-connection loop and,
// critically, tracks which PRIOR connection a reconnect recovers. The analyzer
// matches recovery by the reset's OWN connId, so the ConnReconnected that
// recovers connection N's reset MUST carry connection N's id - not the new
// connection's id. Factored out as a pure helper so this id-carry is unit
// testable without a socket (see probeconn_test.go). It is the critical slice-3
// lesson and is reused verbatim by the load generator's long-lived workers.
type HeldConnSeq struct {
	node  string
	gen   uint64
	last  string
	first bool
}

// NewHeldConnSeq returns a HeldConnSeq whose ids are namespaced by node, so
// resets/reconnects from different nodes (or different workers, when the worker
// index is folded into node) never collide in the analyzer.
func NewHeldConnSeq(node string) *HeldConnSeq {
	return &HeldConnSeq{node: node, first: true}
}

// Next allocates the id for a freshly established held connection. It returns
// that id and, when this is a reconnect (every connection after the first), the
// id of the prior connection whose reset this reconnect recovers. Call exactly
// once per successful dial.
func (s *HeldConnSeq) Next() (connID, reconnectID string, isReconnect bool) {
	connID = fmt.Sprintf("%s-held-%d", s.node, atomic.AddUint64(&s.gen, 1))
	if s.first {
		s.first = false
	} else {
		reconnectID = s.last
		isReconnect = true
	}
	s.last = connID
	return connID, reconnectID, isReconnect
}

// ReadinessGate opens a TCP health listener the FIRST time SignalReady is called
// - i.e. only after a held connection has actually been established. A
// tcpSocket readinessProbe on that port therefore reports Ready once the
// measured connection(s) exist, not merely once the process started. The probe
// signals after its single held dial; the load generator signals only after ALL
// its long-lived workers have connected, so the trigger fires under full
// held-conn pressure.
type ReadinessGate struct {
	addr string
	once sync.Once
	mu   sync.Mutex
	ln   net.Listener
}

// NewReadinessGate returns a gate that will listen on addr when first signalled.
func NewReadinessGate(addr string) *ReadinessGate {
	return &ReadinessGate{addr: addr}
}

// SignalReady opens the health listener exactly once. Subsequent calls are
// no-ops, so callers may invoke it per-worker and only the first wins.
func (g *ReadinessGate) SignalReady() {
	g.once.Do(func() {
		ln, err := net.Listen("tcp", g.addr)
		if err != nil {
			log.Printf("probeconn: readiness listener on %s: %v", g.addr, err)
			return
		}
		g.mu.Lock()
		g.ln = ln
		g.mu.Unlock()
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
	})
}

// Close closes the health listener if it was opened.
func (g *ReadinessGate) Close() {
	g.mu.Lock()
	ln := g.ln
	g.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// HoldAndTrickle keeps conn open, writing a small keepalive each keepAlive
// period and reading the echo back. On a successful round-trip it emits
// ConnKeepaliveOK; on a break it emits ConnReset (ECONNRESET) or ConnEOF (FIN)
// via emit and returns the matching CloseReason; on ctx cancellation it returns
// CloseReasonCtx WITHOUT emitting (a deliberate close is not a drop). A read
// deadline timeout with no bytes is not a close and is skipped.
func HoldAndTrickle(ctx context.Context, conn net.Conn, keepAlive time.Duration, connID string, emit EmitFunc) CloseReason {
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	buf := make([]byte, 16)
	for {
		select {
		case <-ctx.Done():
			return CloseReasonCtx
		case <-ticker.C:
		}
		if _, err := conn.Write([]byte("k")); err != nil {
			emit(Classify(err), connID)
			return ClassifyReason(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(keepAlive))
		if _, err := conn.Read(buf); err != nil {
			// A read deadline timeout with no bytes is not a close; only a real
			// FIN/RST ends the held connection.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			emit(Classify(err), connID)
			return ClassifyReason(err)
		}
		emit(model.ConnKeepaliveOK, connID)
	}
}
