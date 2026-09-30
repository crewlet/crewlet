package statelog_test

import (
	"context"
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
		Waiter: struct{ statelog.Waiter }{}, Identity: struct{ statelog.Identity }{},
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
