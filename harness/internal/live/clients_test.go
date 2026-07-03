package live

import (
	"strings"
	"testing"
)

// clientDecision is the pure core of the per-client observer. Its load-bearing
// invariant (the fix for the fabricated-reset bug that broke app-b/app-c on
// every run) is: a reset is claimed ONLY on a positive pgbouncer signal
// (recovery != nil). An unavailable or negative /hold check is corroboration and
// must NEVER by itself produce Reset:true.
func TestR20_ClientDecision_NeverFabricatesResetFromHold(t *testing.T) {
	secs := 12.5
	cases := []struct {
		name         string
		recovery     *float64
		hold         holdResult
		wantReset    bool
		wantDistinct int
		wantRecovery bool // whether RecoverySeconds is non-nil
		noteHas      string
	}{
		// No positive pgbouncer signal + /hold UNAVAILABLE: this is the exact case
		// that used to fabricate "reset, not recovered" for app-b (python, no wget)
		// and app-c (distroless, no shell). It must NOT be a reset now.
		{"unavailable-no-signal", nil, holdUnavailable, false, 0, false, "hold confirmation unavailable"},
		// /hold answers "not held" but there is still no pgbouncer signal: also NOT
		// a reset - /hold is corroboration only.
		{"not-held-no-signal", nil, holdNotHeld, false, 0, false, "not attributed"},
		// Clean survival: /hold held, no pgbouncer signal => no reset.
		{"held-no-signal", nil, holdHeld, false, 0, false, "survived the roll"},
		// Positive pgbouncer signal => reset+recovered, regardless of /hold. Even
		// when /hold is unavailable the reset stands (pgbouncer is primary).
		{"signal-hold-unavailable", &secs, holdUnavailable, true, 1, true, "hold confirmation unavailable"},
		{"signal-hold-held", &secs, holdHeld, true, 1, true, "confirms a live pooled connection"},
		{"signal-hold-not-held", &secs, holdNotHeld, true, 1, true, "no live pooled connection"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset, distinct, recovery, note := clientDecision(tc.recovery, tc.hold, "")
			if reset != tc.wantReset {
				t.Fatalf("reset = %v, want %v (note=%q)", reset, tc.wantReset, note)
			}
			if distinct != tc.wantDistinct {
				t.Fatalf("distinctResets = %d, want %d", distinct, tc.wantDistinct)
			}
			if (recovery != nil) != tc.wantRecovery {
				t.Fatalf("recoverySeconds non-nil = %v, want %v", recovery != nil, tc.wantRecovery)
			}
			if !strings.Contains(note, tc.noteHas) {
				t.Fatalf("note = %q, want it to contain %q", note, tc.noteHas)
			}
		})
	}
}

// The co-location caveat must be appended to the note whenever a caveat applies,
// on both the reset and the no-reset paths, so a co-located sibling's reset is
// never silently attributed without the ambiguity being flagged.
func TestR21_ClientDecision_CoLocationCaveat(t *testing.T) {
	secs := 3.0
	caveat := "co-located with app-c on node-x; per-client attribution ambiguous"

	_, _, _, resetNote := clientDecision(&secs, holdHeld, caveat)
	if !strings.Contains(resetNote, caveat) {
		t.Fatalf("reset-path note missing caveat: %q", resetNote)
	}
	_, _, _, survNote := clientDecision(nil, holdHeld, caveat)
	if !strings.Contains(survNote, caveat) {
		t.Fatalf("no-reset-path note missing caveat: %q", survNote)
	}
	// No caveat => note unchanged (no dangling parenthetical).
	_, _, _, plain := clientDecision(nil, holdHeld, "")
	if strings.Contains(plain, "co-located") {
		t.Fatalf("empty caveat leaked into note: %q", plain)
	}
}

func TestR22_CoLocationCaveat(t *testing.T) {
	nodeTargets := map[string][]string{
		"node-shared": {"app-b", "app-c"},
		"node-solo":   {"app-a"},
	}
	cases := []struct {
		name   string
		target clientTarget
		want   string
	}{
		{"co-located names sibling", clientTarget{name: "app-b", node: "node-shared"},
			"co-located with app-c on node-shared; per-client attribution ambiguous"},
		{"co-located from the sibling's side", clientTarget{name: "app-c", node: "node-shared"},
			"co-located with app-b on node-shared; per-client attribution ambiguous"},
		{"alone on node => no caveat", clientTarget{name: "app-a", node: "node-solo"}, ""},
		{"unresolved node => no caveat", clientTarget{name: "app-a", node: ""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coLocationCaveat(tc.target, nodeTargets); got != tc.want {
				t.Fatalf("coLocationCaveat = %q, want %q", got, tc.want)
			}
		})
	}
}
