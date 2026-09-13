package store

import "testing"

// A CAPABILITY WITH TWO SPELLINGS IS GATED ONLY IF NEITHER IS USABLE.
//
// `probeVectorIndex` asks for `USING vector` and then `USING diskann`. Marking
// as it went, a first spelling that turned out to be gated put the capability
// on [Capabilities.Gated] and a second that then worked on the live pool
// returned true over the top of it — a reading that claimed the same
// capability was reachable AND unreachable, which is the one thing the two
// fields exist to tell apart.
func TestAGatedAlternativeDoesNotSurviveAUsableOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		got    gateOutcome
		usable bool
		gated  bool
	}{
		{name: "usable is neither gated nor absent", got: gateUsable, usable: true},
		{name: "behind the flag is gated, not usable", got: gateBehind, gated: true},
		{name: "absent is neither", got: gateAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var caps Capabilities
			if got := record(&caps, capVectorIndex, tc.got); got != tc.usable {
				t.Errorf("usable = %v, want %v", got, tc.usable)
			}
			marked := len(caps.Gated) == 1 && caps.Gated[0] == capVectorIndex.Name
			if marked != tc.gated {
				t.Errorf("gated = %v (%v), want %v", marked, caps.Gated, tc.gated)
			}
		})
	}

	// The ordering the loop actually resolves: gated first, usable second.
	// `max` over the outcomes is what makes the better answer win whichever
	// spelling produced it.
	if got := max(gateBehind, gateUsable); got != gateUsable {
		t.Errorf("best of {behind, usable} = %v, want usable", got)
	}
	if got := max(gateAbsent, gateBehind); got != gateBehind {
		t.Errorf("best of {absent, behind} = %v, want behind", got)
	}
}

// A REPEAT IS ORDINARY: both index methods answer for one capability, so the
// name must not land twice.
func TestOneCapabilityIsNamedOnceHoweverManySpellingsAskedFor(t *testing.T) {
	t.Parallel()
	var caps Capabilities
	markGated(&caps, capVectorIndex.Name)
	markGated(&caps, capVectorIndex.Name)
	markGated(&caps, capFullText.Name)
	if len(caps.Gated) != 2 {
		t.Fatalf("gated = %v, want each capability once", caps.Gated)
	}
	if caps.Gated[0] > caps.Gated[1] {
		t.Errorf("gated = %v, want it sorted so a test and a log line can "+
			"compare against it", caps.Gated)
	}
}
