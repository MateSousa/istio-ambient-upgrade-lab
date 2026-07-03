package probeconn

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// TestHeldConnSeqIDCarry pins the id-carry contract: the ConnReconnected that
// recovers connection N's reset must carry connection N's OWN id (the id the
// analyzer keys recovery on), NOT the freshly dialed connection's id. The
// earlier bug emitted the reconnect with the new id, so recovery never matched
// and every real run reported pool-not-recovered. This table walks a sequence
// of successful dials and asserts each reconnect recovers the immediately prior
// connection. Moved here from internal/live/probe_test.go when the held-conn
// primitives were extracted, so the shared logic stays protected and the load
// generator's long-lived workers inherit the guarantee.
func TestHeldConnSeqIDCarry(t *testing.T) {
	seq := NewHeldConnSeq("nodeA")

	connID, reconnectID, isReconnect := seq.Next()
	if isReconnect {
		t.Fatalf("first connect must not be a reconnect (reconnectID=%q)", reconnectID)
	}
	if connID != "nodeA-held-1" {
		t.Fatalf("first connID = %q, want nodeA-held-1", connID)
	}

	prev := connID
	for i := 2; i <= 6; i++ {
		connID, reconnectID, isReconnect = seq.Next()
		if !isReconnect {
			t.Fatalf("connect %d must be a reconnect", i)
		}
		if reconnectID != prev {
			t.Fatalf("reconnect at connect %d recovered %q, want prior conn %q", i, reconnectID, prev)
		}
		wantConn := fmt.Sprintf("nodeA-held-%d", i)
		if connID != wantConn {
			t.Fatalf("connect %d connID = %q, want %q", i, connID, wantConn)
		}
		if reconnectID == connID {
			t.Fatalf("connect %d: reconnect id must differ from the new conn id (the id-carry bug)", i)
		}
		prev = connID
	}
}

// TestHeldConnSeqNodeScoping confirms the ids are namespaced by node so resets
// and reconnects from different nodes (or workers) never collide in the
// analyzer.
func TestHeldConnSeqNodeScoping(t *testing.T) {
	a := NewHeldConnSeq("worker1")
	b := NewHeldConnSeq("worker2")
	ida, _, _ := a.Next()
	idb, _, _ := b.Next()
	if ida == idb {
		t.Fatalf("ids from different nodes collided: %q == %q", ida, idb)
	}
	if ida != "worker1-held-1" || idb != "worker2-held-1" {
		t.Fatalf("unexpected ids: %q, %q", ida, idb)
	}
}

// TestClassifySocketErrors pins the socket-close classification the analyzer's
// died-before-drain / reset attribution depends on: ECONNRESET => ConnReset,
// io.EOF => ConnEOF, and anything ambiguous => ConnEOF (never a false RST).
func TestClassifySocketErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantKind   model.ConnEventKind
		wantReason CloseReason
	}{
		{"econnreset", syscall.ECONNRESET, model.ConnReset, CloseReasonReset},
		{"eof", io.EOF, model.ConnEOF, CloseReasonEOF},
		{"wrapped-econnreset", fmt.Errorf("write: %w", syscall.ECONNRESET), model.ConnReset, CloseReasonReset},
		{"other", errors.New("some other error"), model.ConnEOF, CloseReasonEOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.wantKind {
				t.Fatalf("Classify(%v) = %q, want %q", tc.err, got, tc.wantKind)
			}
			if got := ClassifyReason(tc.err); got != tc.wantReason {
				t.Fatalf("ClassifyReason(%v) = %v, want %v", tc.err, got, tc.wantReason)
			}
		})
	}
}
