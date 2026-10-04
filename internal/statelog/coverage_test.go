package statelog_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A NON-EMPTY MISSING IS RENDERED AS THE ONE SENTENCE, never as nothing.
//
// A gathered answer short of a partition is a shorter answer, and a shorter
// answer reads exactly like a company with less in it unless something says
// so — so the notice counts what is missing against what was addressed, and an
// answer with nothing missing (or no coverage at all) says nothing.
func TestAnAnswerMissingAPartitionSaysSo(t *testing.T) {
	t.Parallel()
	if got := (statelog.Coverage{}).Notice(); got != "" {
		t.Errorf("an answer that states no coverage renders %q, want nothing", got)
	}
	whole := statelog.Coverage{Addressed: 2, Answered: []string{"tracker.000", "tracker.001"}}
	if got := whole.Notice(); got != "" || !whole.Complete() {
		t.Errorf("a whole answer renders %q (complete %v), want nothing and complete",
			got, whole.Complete())
	}
	short := statelog.Coverage{
		Addressed: 4,
		Answered:  []string{"tracker.000", "tracker.003"},
		Missing: []statelog.MissingPartition{
			{Partition: "tracker.001", Reason: statelog.MissingUnreachable},
			{Partition: "tracker.002", Reason: "a reason a newer build named"},
		},
	}
	const want = "2 of 4 partitions did not answer; this list may be incomplete"
	if got := short.Notice(); got != want || short.Complete() {
		t.Errorf("an answer short of two partitions renders %q (complete %v), want %q",
			got, short.Complete(), want)
	}
}

// EVERY REASON IS ONE THIS BUILD NAMES, and an unknown one off the wire is a
// value rather than a refusal: the partition is missing whatever the reason
// is called, and a newer build may name one this build has not learned.
func TestAMissingReasonOffTheWireIsAValue(t *testing.T) {
	t.Parallel()
	for _, r := range statelog.MissingReasons {
		if !r.Valid() {
			t.Errorf("%q is listed and not valid", r)
		}
	}
	if statelog.MissingReason("").Valid() {
		t.Error("the empty reason is valid")
	}
	var c statelog.Coverage
	raw := `{"addressed":1,"answered":[],"missing":[{"partition":"tracker.001","reason":"evicted"}]}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("a coverage naming a reason this build does not know was refused: %v", err)
	}
	if c.Missing[0].Reason.Valid() || c.Notice() == "" {
		t.Errorf("decoded %+v: the unknown reason must decode as a value and still count", c)
	}
}

// A STATED COVERAGE NEVER SENDS A NULL. A read that addressed partitions and
// heard from none of them carries no answered list, and a client reading
// `.length` on a null throws where it would read an absent list as empty.
func TestACoverageSendsNoNull(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(statelog.Coverage{
		Addressed: 1,
		Missing:   []statelog.MissingPartition{{Partition: "tracker.000", Reason: statelog.MissingUnserved}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "null") {
		t.Errorf("a coverage with nothing answered marshals a null: %s", raw)
	}
}
