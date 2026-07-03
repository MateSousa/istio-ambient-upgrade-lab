package live

import (
	"fmt"
	"testing"
)

// TestHeldConnSeqIDCarry pins the id-carry contract: the ConnReconnected that
// recovers connection N's reset must carry connection N's OWN id (the id the
// analyzer keys recovery on), NOT the freshly dialed connection's id. The
// earlier bug emitted the reconnect with the new id, so recovery never matched
// and every real run reported pool-not-recovered. This table walks a sequence
// of successful dials and asserts each reconnect recovers the immediately prior
// connection.
func TestHeldConnSeqIDCarry(t *testing.T) {
	seq := newHeldConnSeq("nodeA")

	connID, reconnectID, isReconnect := seq.next()
	if isReconnect {
		t.Fatalf("first connect must not be a reconnect (reconnectID=%q)", reconnectID)
	}
	if connID != "nodeA-held-1" {
		t.Fatalf("first connID = %q, want nodeA-held-1", connID)
	}

	prev := connID
	for i := 2; i <= 6; i++ {
		connID, reconnectID, isReconnect = seq.next()
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
// and reconnects from different nodes never collide in the analyzer.
func TestHeldConnSeqNodeScoping(t *testing.T) {
	a := newHeldConnSeq("worker1")
	b := newHeldConnSeq("worker2")
	ida, _, _ := a.next()
	idb, _, _ := b.next()
	if ida == idb {
		t.Fatalf("ids from different nodes collided: %q == %q", ida, idb)
	}
	if ida != "worker1-held-1" || idb != "worker2-held-1" {
		t.Fatalf("unexpected ids: %q, %q", ida, idb)
	}
}
