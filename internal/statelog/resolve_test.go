package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// peerCopy lands, straight on the log, writer's record of opID on subject a —
// a copy of an operation this node did not publish — and has this node's
// applier pass it without writing its ledger row, which is what applying a
// record a gate dropped looks like. It answers where the copy landed.
func peerCopy(t *testing.T, h *harness, writer, opID string) statelog.Position {
	t.Helper()
	h.applier.mu.Lock()
	h.applier.auto = false
	h.applier.mu.Unlock()
	seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a", opID, nil,
		probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: writer}, opID, "the peer's"))
	if err != nil {
		t.Fatalf("land %s's copy: %v", writer, err)
	}
	at := statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: seq}
	h.applier.advance(at)
	return at
}

// A WRITE COLLAPSED ONTO ANOTHER NODE'S GATED COPY OF ITS OPERATION IS TOLD
// THAT COPY'S GATE.
//
// A node that left a partition while one of its writes was in flight has that
// write on the log above its release, applying nowhere — and holding the
// operation id in the broker's duplicate window. A node that serves the
// partition, handed the same operation inside the window, has its append
// collapsed onto that record. Its resolution finds no ledger row, and whether
// a gate dropped the record is a question about the record's WRITER: asked
// about itself, the serving node found nothing, trusted a ledger that vouched,
// and reported a contract violation for a record that was only ever gated.
func TestACollapsedWriteIsJudgedByTheWriterOfTheCopy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	at := peerCopy(t, h, "node-b", "op-left")
	h.gates.holdWriter("node-b", statelog.ReasonReleased)

	res, err := h.write(probeSubject("a"), "op-left", "mine")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("a write collapsed onto node-b's released copy = (%+v, %v), want "+
			"a refusal %q", res, err, statelog.ReasonReleased)
	}
	if refusal.Reason != statelog.ReasonReleased || refusal.Position != at ||
		refusal.OpID != "op-left" {
		t.Fatalf("refusal = %+v, want %q at %s under op-left", refusal,
			statelog.ReasonReleased, at)
	}
	if !strings.Contains(refusal.Detail, "node-b") || !strings.Contains(refusal.Detail, "2m0s") {
		t.Errorf("the refusal's detail %q names neither the copy's writer nor how "+
			"long the operation id stays spent", refusal.Detail)
	}
	if asked := h.gates.askedAbout(); !slices.Equal(asked, []string{"node-b"}) {
		t.Errorf("the gates were asked about %v, want the copy's writer alone", asked)
	}
}

// A COPY THIS NODE CANNOT READ IS ONE WHOSE WRITER NOBODY HERE CAN NAME — so
// whether a gate dropped it is unknown, and the answer is `unknown` rather than
// either guess.
func TestACollapsedWriteWhoseCopyCannotBeReadIsUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	peerCopy(t, h, "node-b", "op-unread")
	h.gates.holdWriter("node-b", statelog.ReasonReleased)
	h.records.fail(errors.New("no response from stream"))

	res, err := h.write(probeSubject("a"), "op-unread", "mine")
	if err != nil || res.Outcome != statelog.OutcomeUnknown || res.OpID != "op-unread" ||
		res.Position != (statelog.Position{}) {
		t.Fatalf("a write collapsed onto a copy nobody could read = (%+v, %v), want "+
			"unknown under op-unread with no position", res, err)
	}
}

// collapsing answers every append as the broker's duplicate of the record at
// seq, publishing nothing — a collapse the duplicate window itself would never
// make onto another operation's record, which is what a resolution must not
// take on trust.
type collapsing struct {
	statelog.Appender
	seq uint64
}

func (c collapsing) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return c.seq, true, nil
}

// A COLLAPSE ONTO ANOTHER OPERATION'S RECORD IS NOT RESOLVED AS THIS ONE'S.
//
// The broker's duplicate window is keyed on the operation id, so the record an
// append is collapsed onto carries it. One that does not is a log whose
// acknowledgements say something it does not hold, and judging that record's
// gate — or its ledger row's absence — as this operation's would report
// another write's fate as this one's.
func TestACollapseOntoAnotherOperationsRecordIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	at := peerCopy(t, h, "node-b", "op-other")
	h.appends.inner = collapsing{Appender: h.log, seq: at.Seq}

	res, err := h.write(probeSubject("a"), "op-mine", "mine")
	if err == nil || !strings.Contains(err.Error(), "op-other") {
		t.Fatalf("a write collapsed onto op-other's record = (%+v, %v), want an "+
			"error naming the operation the record carries", res, err)
	}
	var refusal *statelog.Unavailable
	if errors.As(err, &refusal) {
		t.Fatalf("the collapse was answered as a refusal %q, which judges op-other's "+
			"record as op-mine's", refusal.Reason)
	}
}

// A PUBLISHER THAT CANNOT READ ITS OWN LOG IS NEVER BUILT: a write collapsed
// onto another node's copy of its operation is judged by that copy's writer,
// and only the record on the log names it.
func TestAPublisherIsNotBuiltWithoutAReaderOfItsLog(t *testing.T) {
	t.Parallel()
	deps := statelog.Deps{
		Domain: probeDomain{}, Spec: specOf(probeDomain{}),
		Layout: layoutOf(probeDomain{}), LogID: logOf(probeDomain{}),
		Holding: statelog.ServesOnly(logOf(probeDomain{}).Partition),
		Log:     struct{ statelog.Appender }{}, Rows: struct{ statelog.Rows }{},
		Fence: struct{ statelog.Fence }{}, Gates: struct{ statelog.Gates }{},
		Waiter: struct{ statelog.Waiter }{}, Voids: struct{ statelog.Voids }{},
		Identity:  struct{ statelog.Identity }{},
		Admission: noCeiling(t), NodeID: "node-a",
		Generation: func() uint32 { return 1 },
	}
	if _, err := statelog.NewPublisher(deps); err == nil ||
		!strings.Contains(err.Error(), "reader of its log") {
		t.Fatalf("a publisher with no reader of its log was built (%v)", err)
	}
	deps.Records = struct{ statelog.LogReader }{}
	if _, err := statelog.NewPublisher(deps); err != nil {
		t.Fatalf("a publisher with every dependency was refused: %v", err)
	}
}

// A WRITE A REANCHOR'S RULE VOIDED IS REFUSED UNDER THAT RULE, not reported as
// a ledger contract violation.
//
// A node whose rows were a restored copy's age goes on writing in the old
// generation until it learns of the move; its record lands after the
// reanchor's own and is overtaken on every node, and writes no ledger row. The
// domain's gates know nothing of that rule — it is the framework's, on the
// checkpoint — so a resolution that asked only them found no gate and a ledger
// that vouched, and called the write a record applied without its row.
func TestAWriteAReanchorVoidedIsRefusedUnderItsRule(t *testing.T) {
	t.Parallel()
	for _, reason := range []statelog.Reason{statelog.ReasonOvertaken, statelog.ReasonAbandoned} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			// THE RECORD LANDS AND THIS NODE'S APPLIER PASSES IT, dropping
			// it under the rule — so no ledger row is written for it.
			h.applier.mu.Lock()
			h.applier.auto, h.applier.voids = false, reason
			h.applier.mu.Unlock()
			h.applier.advance(statelog.Position{Stream: probeStream,
				Generation: h.gen.Load(), Seq: 1_000})

			res, err := h.write(probeSubject("a"), "op-voided", "mine")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != reason {
				t.Fatalf("a write the reanchor's rule voided = (%+v, %v), want a "+
					"refusal %q", res, err, reason)
			}
			if refusal.Position.Seq == 0 || refusal.OpID != "op-voided" {
				t.Errorf("the refusal %+v names no landing — the record is on the "+
					"log and holds its operation id", refusal)
			}
		})
	}
}

// THE RUNNER VOIDS WHAT ITS CHECKPOINT'S REANCHOR SAID TO — and a resolution
// asks it exactly what the applier asked.
func TestTheRunnerAnswersTheRulesItsCheckpointCarries(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	// A RESTORED REANCHOR'S CHECKPOINT: generations 3 and 4 abandoned, and
	// every lower generation's record after sequence 100 overtaken.
	if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			UPDATE statelog_cursor SET generation = 5, void_after = 2, void_before = 5,
				stale_after = 100`)
		return err
	}); err != nil {
		t.Fatalf("place the reanchor's rules: %v", err)
	}
	h.upgrade(probeDomain{})
	if err := h.boot(0); err != nil {
		t.Fatalf("boot: %v", err)
	}
	for _, tc := range []struct {
		gen    uint32
		seq    uint64
		reason statelog.Reason
	}{
		{gen: 3, seq: 50, reason: statelog.ReasonAbandoned},
		{gen: 4, seq: 150, reason: statelog.ReasonAbandoned},
		{gen: 1, seq: 150, reason: statelog.ReasonOvertaken},
		{gen: 1, seq: 50},
		{gen: 5, seq: 150},
	} {
		reason, voided := h.runner.Voided(tc.gen, tc.seq)
		if voided != (tc.reason != "") || reason != tc.reason {
			t.Errorf("a record of generation %d at %d is voided %v (%q), want %q",
				tc.gen, tc.seq, voided, reason, tc.reason)
		}
	}
}

// A PUBLISHER THAT CANNOT ASK ITS APPLIER'S REANCHOR RULES IS NEVER BUILT.
func TestAPublisherIsNotBuiltWithoutItsReanchorRules(t *testing.T) {
	t.Parallel()
	deps := statelog.Deps{
		Domain: probeDomain{}, Spec: specOf(probeDomain{}),
		Layout: layoutOf(probeDomain{}), LogID: logOf(probeDomain{}),
		Holding: statelog.ServesOnly(logOf(probeDomain{}).Partition),
		Log:     struct{ statelog.Appender }{}, Records: struct{ statelog.LogReader }{},
		Rows: struct{ statelog.Rows }{}, Fence: struct{ statelog.Fence }{},
		Gates: struct{ statelog.Gates }{}, Waiter: struct{ statelog.Waiter }{},
		Identity:  struct{ statelog.Identity }{},
		Admission: noCeiling(t), NodeID: "node-a",
		Generation: func() uint32 { return 1 },
	}
	if _, err := statelog.NewPublisher(deps); err == nil ||
		!strings.Contains(err.Error(), "reanchor rules") {
		t.Fatalf("a publisher with no reanchor rules was built (%v)", err)
	}
	deps.Voids = struct{ statelog.Voids }{}
	if _, err := statelog.NewPublisher(deps); err != nil {
		t.Fatalf("a publisher with every dependency was refused: %v", err)
	}
}
