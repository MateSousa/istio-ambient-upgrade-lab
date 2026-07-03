package live

import (
	"context"
	"encoding/json"
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
	keep := envDurationMS("KEEPALIVE_MS", 20000)
	newc := envDurationMS("NEWCONN_MS", 2000)
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

	gate := newReadinessGate(cfg.HealthAddr)
	defer gate.close()

	go newConnLoop(ctx, cfg, emit)
	heldConnLoop(ctx, cfg, emit, gate.signalReady)
	return ctx.Err()
}

type emitFunc func(kind model.ConnEventKind, connID string)

// readinessGate opens a TCP health listener the FIRST time signalReady is
// called - i.e. only after the probe's held connection has actually been
// established. A tcpSocket readinessProbe on that port therefore reports Ready
// once the measured connection exists, not merely once the process started,
// which is what waitProbesReady claims to confirm before firing the trigger.
type readinessGate struct {
	addr string
	once sync.Once
	mu   sync.Mutex
	ln   net.Listener
}

func newReadinessGate(addr string) *readinessGate {
	return &readinessGate{addr: addr}
}

func (g *readinessGate) signalReady() {
	g.once.Do(func() {
		ln, err := net.Listen("tcp", g.addr)
		if err != nil {
			log.Printf("probe: readiness listener on %s: %v", g.addr, err)
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

func (g *readinessGate) close() {
	g.mu.Lock()
	ln := g.ln
	g.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// heldConnSeq generates the per-connection ids for the held-connection loop and,
// critically, tracks which PRIOR connection a reconnect recovers. The analyzer
// matches recovery by the reset's OWN connId, so the ConnReconnected that
// recovers connection N's reset MUST carry connection N's id - not the new
// connection's id. Factored out as a pure helper so this id-carry is unit
// testable without a socket (see probe_test.go).
type heldConnSeq struct {
	node  string
	gen   uint64
	last  string
	first bool
}

func newHeldConnSeq(node string) *heldConnSeq {
	return &heldConnSeq{node: node, first: true}
}

// next allocates the id for a freshly established held connection. It returns
// that id and, when this is a reconnect (every connection after the first), the
// id of the prior connection whose reset this reconnect recovers. Call exactly
// once per successful dial.
func (s *heldConnSeq) next() (connID, reconnectID string, isReconnect bool) {
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

// heldConnLoop keeps a single long-lived connection open, trickling a keepalive
// on it, reconnecting after a break. The connId is stable across the life of
// one connection instance and regenerated on reconnect, so the analyzer can
// dedup resets per (connId, window) and measure recovery reset->reconnect. The
// ConnReconnected recovering connection N's reset is emitted with connection N's
// id (carried forward via heldConnSeq), which is the id the analyzer keys
// recovery on. onReady is invoked once, after the first successful dial.
func heldConnLoop(ctx context.Context, cfg ProbeConfig, emit emitFunc, onReady func()) {
	seq := newHeldConnSeq(cfg.Node)
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
		connID, reconnectID, isReconnect := seq.next()
		if isReconnect {
			emit(model.ConnReconnected, reconnectID)
		} else if onReady != nil {
			onReady()
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
