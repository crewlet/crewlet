package statelog_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A POSITION IS A TRIPLE AND ORDERS BY GENERATION FIRST: ordering sequences
// first would rank a large sequence from before a reanchor above a small
// current one and hand a writer an expectation the broker refuses for ever.
func TestAPositionOrdersByGenerationBeforeSequence(t *testing.T) {
	t.Parallel()
	const s = "CREWLET_PROBE_LOG"
	for name, tc := range map[string]struct {
		a, b statelog.Position
		want bool
	}{
		"a huge old sequence is below a small new one": {
			a:    statelog.Position{Stream: s, Generation: 0, Seq: 4_000_000},
			b:    statelog.Position{Stream: s, Generation: 1, Seq: 1},
			want: true,
		},
		"and never the other way round": {
			a:    statelog.Position{Stream: s, Generation: 1, Seq: 1},
			b:    statelog.Position{Stream: s, Generation: 0, Seq: 4_000_000},
			want: false,
		},
		"within one generation it is the sequence": {
			a:    statelog.Position{Stream: s, Generation: 3, Seq: 10},
			b:    statelog.Position{Stream: s, Generation: 3, Seq: 11},
			want: true,
		},
		"equal is not before": {
			a:    statelog.Position{Stream: s, Generation: 3, Seq: 10},
			b:    statelog.Position{Stream: s, Generation: 3, Seq: 10},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tc.a.Before(tc.b)
			if err != nil {
				t.Fatalf("Before: %v", err)
			}
			if got != tc.want {
				t.Errorf("%s.Before(%s) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// The packed form must give the SAME order: every durable
			// column is compared by it, in SQL that knows no generation.
			if packed := tc.a.Packed() < tc.b.Packed(); packed != tc.want {
				t.Errorf("packed order = %v, want %v — the packed form and "+
					"Before must not be able to disagree", packed, tc.want)
			}
		})
	}
}

// TWO STREAMS ARE NOT COMPARABLE, and that is an ERROR rather than a false
// ([statelog.ErrWrongStream] says why).
func TestComparingAcrossStreamsIsAnErrorAndNotAFalse(t *testing.T) {
	t.Parallel()
	a := statelog.Position{Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 1}
	b := statelog.Position{Stream: "CREWLET_OTHER_LOG", Generation: 1, Seq: 9}
	got, err := a.Before(b)
	if !errors.Is(err, statelog.ErrWrongStream) {
		t.Fatalf("Before across streams = (%v, %v), want ErrWrongStream", got, err)
	}
	if got {
		t.Error("a refused comparison must not also answer true")
	}
}

// A POSITION A CALLER NAMED IS ON THE LOG IT IS ASKED OF, OR NAMES NOTHING
// ([statelog.Position.On]): one on another log is
// [statelog.ErrForeignPosition], the request's mistake, never
// [statelog.ErrWrongStream]. A zero position and one with no stream are the
// controls.
func TestAPositionACallerNamedIsHeldToItsOwnLog(t *testing.T) {
	t.Parallel()
	const here = "CREWLET_PROBE_LOG"
	for _, c := range []struct {
		name    string
		at      statelog.Position
		foreign bool
	}{
		{"on this log", statelog.Position{Stream: here, Generation: 1, Seq: 5}, false},
		{"the zero position", statelog.Position{}, false},
		{"a zero position on another log", statelog.Position{Stream: "OTHER"}, false},
		{"no stream", statelog.Position{Generation: 1, Seq: 5}, false},
		{"on another log", statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 5}, true},
	} {
		err := c.at.On(here)
		switch {
		case c.foreign && !errors.Is(err, statelog.ErrForeignPosition):
			t.Errorf("%s: On = %v, want %v", c.name, err, statelog.ErrForeignPosition)
		case c.foreign && errors.Is(err, statelog.ErrWrongStream):
			t.Errorf("%s: On = %v, which is the engine's own comparison "+
				"fault rather than the caller's request", c.name, err)
		case !c.foreign && err != nil:
			t.Errorf("%s: On = %v, want nil", c.name, err)
		}
	}
}

// THE PACKED FORM'S BOUNDS ARE CHECKED WHERE A POSITION IS MINTED, so Packed()
// stays a plain conversion: a value past either bound would go negative in a
// durable column with nothing saying so.
func TestAPositionOutsideThePackedRangeIsRefusedWhereItIsMinted(t *testing.T) {
	t.Parallel()
	const s = "CREWLET_PROBE_LOG"
	for name, tc := range map[string]struct {
		p    statelog.Position
		want bool
	}{
		"the highest generation fits": {
			p:    statelog.Position{Stream: s, Generation: statelog.MaxGeneration, Seq: 1},
			want: true,
		},
		"one generation past it does not": {
			p:    statelog.Position{Stream: s, Generation: statelog.MaxGeneration + 1, Seq: 1},
			want: false,
		},
		"the highest sequence fits": {
			p:    statelog.Position{Stream: s, Generation: 1, Seq: statelog.MaxSeq},
			want: true,
		},
		"one sequence past it does not": {
			p:    statelog.Position{Stream: s, Generation: 1, Seq: statelog.MaxSeq + 1},
			want: false,
		},
		"and a position with no stream names no number space": {
			p:    statelog.Position{Generation: 1, Seq: 1},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.p.Valid()
			if tc.want && err != nil {
				t.Fatalf("Valid() = %v, want nil", err)
			}
			if !tc.want && err == nil {
				t.Fatalf("Valid() accepted %v", tc.p)
			}
			if !tc.want && err != nil && !errors.Is(err, statelog.ErrPositionRange) &&
				tc.p.Stream != "" {
				t.Errorf("error = %v, want ErrPositionRange", err)
			}
		})
	}

	// AND THE HIGHEST LEGAL POSITION IS STILL POSITIVE: SQL compares packed
	// positions with a plain `>`, and a negative one sorts below every row
	// it is meant to be above.
	top := statelog.Position{Stream: s, Generation: statelog.MaxGeneration, Seq: statelog.MaxSeq}
	if top.Packed() <= 0 {
		t.Fatalf("the highest legal position packs to %d, which is not positive",
			top.Packed())
	}
}

// A POSITION RENDERS WITH THE KEYS ITS READERS INDEX: the dashboard's protocol
// types declare `{stream, generation, seq}`, and a Go round trip alone would
// pass on Go's own field names while the dashboard read `undefined`.
func TestAPositionRendersTheKeysTheDashboardReads(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 2, Seq: 4711}
	raw, err := json.Marshal(at)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"stream":"CREWLET_TRACKER_LOG","generation":2,"seq":4711}`
	if string(raw) != want {
		t.Fatalf("a position rendered as %s, want %s", raw, want)
	}
	var back statelog.Position
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != at {
		t.Fatalf("round-tripped to %+v, want %+v", back, at)
	}
}

// A POSITION'S TOKEN READS BACK AS THE POSITION IT NAMES, and nothing else
// does: a reader that accepted a bare sequence, or rounded a generation it
// could not hold, would hand a node a position in the wrong number space.
func TestAPositionTokenReadsBackAsThePositionItNames(t *testing.T) {
	t.Parallel()
	for _, at := range []statelog.Position{
		{Stream: "CREWLET_TRACKER_LOG", Generation: 0, Seq: 1},
		{Stream: "CREWLET_TRACKER_LOG", Generation: 7, Seq: 4_000_000},
		{Stream: "CREWLET_TRACKER_VECTORS", Generation: statelog.MaxGeneration,
			Seq: statelog.MaxSeq},
	} {
		got, err := statelog.ParsePosition(at.String())
		if err != nil || got != at {
			t.Errorf("ParsePosition(%q) = (%v, %v), want %v", at.String(), got, err, at)
		}
	}
	for _, bad := range []string{
		"", "42", "@1:2", "CREWLET_TRACKER_LOG@1", "CREWLET_TRACKER_LOG@x:2",
		"CREWLET_TRACKER_LOG@1:y", "CREWLET_TRACKER_LOG@-1:2",
		// One generation past what the packed form can carry.
		"CREWLET_TRACKER_LOG@8388608:1",
		// One sequence past one generation's whole space.
		"CREWLET_TRACKER_LOG@0:1099511627776",
	} {
		if got, err := statelog.ParsePosition(bad); err == nil {
			t.Errorf("ParsePosition(%q) = %v, want a refusal", bad, got)
		}
	}
}
