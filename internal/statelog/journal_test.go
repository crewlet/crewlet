package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A WINDOW'S JOURNAL KEEPS ITS UNRESOLVED REQUESTS AND DROPS ITS OLDEST
// RESOLVED ONES: a ninth record pushes out the oldest completed or retired
// record, whatever lies between, and never an issued one, which the seal still
// reads; a ninth request beside eight unresolved ones is not journaled, so it
// is not sent.
//
// Mutation: drop the oldest record whatever its state, or let the journal grow
// past its cap, and this is red.
func TestAJournalDropsItsOldestResolvedRecordsAndNeverAnUnresolvedOne(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	record := func(attempt int, state coord.JournalState) coord.JournalRecord {
		return coord.JournalRecord{Attempt: attempt, By: "node-a:p", State: state, At: at}
	}
	next := record(9, coord.JournalIssued)

	// ISSUED FIRST, then resolved ones: the oldest RESOLVED leaves.
	journal := []coord.JournalRecord{record(1, coord.JournalIssued), record(2, coord.JournalRetired),
		record(3, coord.JournalCompleted)}
	for i := 4; len(journal) < coord.MaxMaintenanceJournal; i++ {
		journal = append(journal, record(i, coord.JournalRetired))
	}
	kept, ok := statelog.Journaled(journal, next)
	if !ok || len(kept) != coord.MaxMaintenanceJournal {
		t.Fatalf("a full journal with resolved records took the next one = %d records, %t; want %d, true",
			len(kept), ok, coord.MaxMaintenanceJournal)
	}
	if kept[0].Attempt != 1 || kept[1].Attempt != 3 || kept[len(kept)-1] != next {
		t.Errorf("the journal kept attempts %v, want 1 (issued) and 3 on, then the new one", attempts(kept))
	}

	// UNDER ITS CAP nothing leaves.
	if short, ok := statelog.Journaled(journal[:3], next); !ok || len(short) != 4 {
		t.Errorf("a journal under its cap kept %d of 4 records (%t)", len(short), ok)
	}

	// EIGHT UNRESOLVED: the ninth is refused, the journal as it was.
	var issued []coord.JournalRecord
	for i := range coord.MaxMaintenanceJournal {
		issued = append(issued, record(i+1, coord.JournalIssued))
	}
	if held, ok := statelog.Journaled(issued, next); ok || len(held) != coord.MaxMaintenanceJournal ||
		held[0].Attempt != 1 {
		t.Errorf("a ninth unresolved request was journaled (%t) as %v", ok, attempts(held))
	}
}

// attempts is each record's attempt, in order.
func attempts(journal []coord.JournalRecord) []int {
	out := make([]int, len(journal))
	for i, r := range journal {
		out[i] = r.Attempt
	}
	return out
}

// A RETIREMENT'S EVIDENCE IS CUT TO ITS RECORD, its ceiling first: a barrier
// over every participant a window names, each at its longest, is recorded
// within coord.MaxJournalEvidenceBytes and the window validates.
//
// Mutation: drop the cut in describeBarrier, and the window is refused.
func TestARetirementsEvidenceIsCutToItsRecordCeilingFirst(t *testing.T) {
	t.Parallel()
	ev := statelog.BarrierEvidence{Incarnations: map[string]string{}, ObservedMaxBytes: 1 << 40,
		At: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}
	op := coord.MaintenanceOperation{Stream: "CREWLET_TRACKER_LOG", OperationID: "capacity:x",
		Phase: coord.PhaseSealing, Attempt: 1, TargetMaxBytes: 1 << 40,
		Journal: []coord.JournalRecord{{Attempt: 1, By: "node-a:p", State: coord.JournalIssued, At: ev.At}}}
	for i := range coord.MaxMaintenanceParticipants {
		node := strings.Repeat("n", coord.MaxNodeIDBytes-2) + string(rune('a'+i/26)) + string(rune('a'+i%26))
		op.Participants = append(op.Participants, node)
		ev.Incarnations[node] = node + ":01938a6e-7a3e-7cd1-9b2a-0f1e2d3c4b5a"
	}
	retired := statelog.Retire(op, ev)
	evidence := retired.Journal[0].Evidence
	if len(evidence) > coord.MaxJournalEvidenceBytes || !strings.HasPrefix(evidence, "ceiling 1099511627776 ") {
		t.Errorf("the evidence is %d bytes, %q: want at most %d, the ceiling first", len(evidence),
			evidence, coord.MaxJournalEvidenceBytes)
	}
	if err := retired.Validate(); err != nil {
		t.Errorf("the retired window is refused: %v", err)
	}
}

// THE SEAL LIFTS A BLOCK THAT HELD A REQUEST BACK, and no other: a window
// blocked because its journal was full or its fleet outgrew it carries no
// stale sentence once the seal has retired its requests, while an operator's
// abandonment and an exhausted window keep theirs.
//
// Mutation: drop the lift from Retire, and the journal's block survives the
// seal.
func TestTheSealLiftsABlockThatHeldARequestBack(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	for block, lifted := range map[coord.MaintenanceBlock]bool{
		coord.BlockJournalFull: true, coord.BlockTooManyParticipants: true, coord.BlockTooManyRecorded: true,
		coord.BlockAbandoned: false, coord.BlockUnsealed: false,
	} {
		op := coord.MaintenanceOperation{Blocked: block, Outgrown: 17,
			Journal: []coord.JournalRecord{{Attempt: 1, By: "node-a:p", State: coord.JournalIssued, At: at}}}
		out := statelog.Retire(op, statelog.BarrierEvidence{At: at})
		if (out.Blocked == "") != lifted {
			t.Errorf("a window blocked %q is %q after the seal retires its requests, want it lifted: %t",
				block, out.Blocked, lifted)
		}
	}
}
