package advisor

import "testing"

// A PREMISE IS GROUNDED AS A FACT IS, AND MUST HOLD WHEN SUBMITTED: it cites a
// number at a path into a read the run made, which compares as it says now,
// and the finding keeps the read and its parameters to ask it again. One that
// cites no read, or that does not hold now, drops the finding. Mutation: keep
// a premise that does not hold, and the next hourly pass resolves a finding
// that was never true.
func TestAPremiseIsGroundedAndMustHoldWhenSubmitted(t *testing.T) {
	t.Parallel()
	served := NewServed()
	answer := map[string]any{"entries": []any{map[string]any{"hits": 4}}}
	if err := served.ServeRead("read_1", ReadSource{Read: "advisor_capacity", Params: map[string]any{"range": "7d"}},
		answer, nil); err != nil {
		t.Fatal(err)
	}
	if err := served.Serve("snapshot", map[string]any{"hits": 4}, nil); err != nil {
		t.Fatal(err)
	}
	p, ok := premiseOf(SubmittedPremise{From: "read_1.entries[0].hits", Op: PremiseGe, Value: 3}, served)
	if !ok || p.Read != "advisor_capacity" || p.Params["range"] != "7d" || p.Path != "entries[0].hits" {
		t.Errorf("premise = %+v (%v), want the capacity read's hits at 3 or more", p, ok)
	}
	for _, bad := range []SubmittedPremise{
		{From: "snapshot.hits", Op: PremiseGe, Value: 3},
		{From: "read_1.entries[0].hits", Op: PremiseGt, Value: 4},
		{From: "read_1.entries[1].hits", Op: PremiseGe, Value: 1},
		{From: "read_1.entries[0].hits", Op: "about", Value: 4},
	} {
		if _, ok := premiseOf(bad, served); ok {
			t.Errorf("premise %+v was kept", bad)
		}
	}
	if n, ok := ReadPath(map[string]any{"a": []any{map[string]any{"b": 2.5}}}, "a[0].b"); !ok || n != 2.5 {
		t.Errorf("ReadPath = %v, %v", n, ok)
	}
}
