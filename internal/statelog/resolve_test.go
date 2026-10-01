package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// peerCopy lands, straight on the log, writer's record of opID on subject a —
// a copy of an operation this node did not publish — and has this node's
// applier pass it without writing its ledger row, which is what applying a
// record a gate dropped looks like. It answers where the copy landed.
func peerCopy(t *testing.T, h *harness, writer, opID string) statelog.Position {
	t.Helper()
	return peerCopyIn(t, h, writer, opID, h.gen.Load())
}

// peerCopyIn is [peerCopy] for a copy its writer stamped with generation gen —
// which need not be the generation this node stamps its own records with.
func peerCopyIn(t *testing.T, h *harness, writer, opID string, gen uint32) statelog.Position {
	t.Helper()
	h.applier.mu.Lock()
	h.applier.auto = false
	h.applier.mu.Unlock()
	seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a", opID, nil,
		probeRecord(statelog.Stamp{Gen: gen, Writer: writer}, opID, "the peer's"))
	if err != nil {
		t.Fatalf("land %s's copy: %v", writer, err)
	}
	at := statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: seq}
	h.applier.advance(at)
	return at
}

// A WRITE COLLAPSED ONTO ANOTHER NODE'S GATED COPY OF ITS OPERATION IS TOLD
// THAT COPY'S GATE — AND WHOSE IT IS.
//
// A node that left a partition while one of its writes was in flight has that
// write on the log above its release, applying nowhere — and holding the
// operation id in the broker's duplicate window. A node that serves the
// partition, handed the same operation inside the window, has its append
// collapsed onto that record. Its resolution finds no ledger row, and whether
// a gate dropped the record is a question about the record's WRITER: asked
// about itself, the serving node found nothing, trusted a ledger that vouched,
// and reported a contract violation for a record that was only ever gated.
//
// And the refusal names that writer, because the reason is then its standing
// and not the refusing node's: read as this node's own `released`, an operator
// was sent away from the one node that can finish the write. A copy this node
// wrote itself, on an earlier attempt, is its own standing, and names nobody.
func TestACollapsedWriteIsJudgedByTheWriterOfTheCopy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, writer, named string
	}{
		{name: "another node's copy", writer: "node-b", named: "node-b"},
		{name: "this node's own earlier copy", writer: "node-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			at := peerCopy(t, h, tc.writer, "op-left")
			h.gates.holdWriter(tc.writer, statelog.ReasonReleased)

			res, err := h.write(probeSubject("a"), "op-left", "mine")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) {
				t.Fatalf("a write collapsed onto %s's released copy = (%+v, %v), "+
					"want a refusal %q", tc.writer, res, err, statelog.ReasonReleased)
			}
			if refusal.Reason != statelog.ReasonReleased || refusal.Position != at ||
				refusal.OpID != "op-left" {
				t.Fatalf("refusal = %+v, want %q at %s under op-left", refusal,
					statelog.ReasonReleased, at)
			}
			if refusal.CopyWriter != tc.named {
				t.Errorf("the refusal names %q as the copy's writer, want %q",
					refusal.CopyWriter, tc.named)
			}
			if !strings.Contains(refusal.Detail, "2m0s") ||
				(tc.named != "" && !strings.Contains(refusal.Detail, tc.named)) {
				t.Errorf("the refusal's detail %q names neither the copy's writer "+
					"nor how long the operation id stays spent", refusal.Detail)
			}
			if asked := h.gates.askedAbout(); !slices.Equal(asked, []string{tc.writer}) {
				t.Errorf("the gates were asked about %v, want the copy's writer alone", asked)
			}
		})
	}
}

// ANOTHER NODE'S COPY DROPPED BY THE DELETION MARKER NAMES NO WRITER.
//
// The marker is a fact about the OBJECT: it holds every writer's record on it
// for ever, so the node that refused would have its own record dropped exactly
// as the copy was. Named as the copy's writer, node-b read as the one the
// refusal was about — and every reader of that field offers the same operation
// again once the duplicate window has passed, which for a purged task meets the
// marker for ever. The detail still says whose copy the record is.
func TestAnotherNodesCopyTheDeletionMarkerDroppedNamesNoWriter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	at := peerCopy(t, h, "node-b", "op-purged")
	// EVERY WRITER, as the marker holds them — no writer named.
	h.gates.gated, h.gates.reason = true, statelog.ReasonDeleted

	res, err := h.write(probeSubject("a"), "op-purged", "mine")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("a write collapsed onto node-b's copy on a purged object = (%+v, %v), "+
			"want a refusal %q", res, err, statelog.ReasonDeleted)
	}
	if refusal.Reason != statelog.ReasonDeleted || refusal.Position != at ||
		refusal.OpID != "op-purged" {
		t.Fatalf("refusal = %+v, want %q at %s under op-purged", refusal,
			statelog.ReasonDeleted, at)
	}
	if refusal.CopyWriter != "" {
		t.Errorf("the refusal names %q as the copy's writer — the deletion marker "+
			"holds this node's own record as surely, so the refusal is not about "+
			"node-b and a retry here meets it again", refusal.CopyWriter)
	}
	if !strings.Contains(refusal.Detail, "node-b") {
		t.Errorf("the refusal's detail %q no longer says whose copy the record is",
			refusal.Detail)
	}
}

// THE REPLICATION GUIDE SAYS WHAT THE WRITE-GATED LINE CARRIES, AND WHAT IT IS.
//
// `statelog_write_gated` is the asking node's only per-record witness of a
// write a gate refused, and its row in the guide read "the same, seen by the
// write that published it" — the same as the applier's `statelog_record_gated`.
// Neither half held. The node that logs it is often not the one that published
// the record: a write collapsed onto another node's copy logs that node as the
// `writer`, and a reader told the line came from the publisher took the asking
// node for the writer, the misreading [statelog.Unavailable.CopyWriter] exists
// to prevent. And the line is a REFUSAL, counted by reason, not a second drop:
// read as "the same" an operator counted one record twice, which is the double
// count the records-gated counter was rid of.
//
// So the row is held to the line in both of those respects: every key the line
// carries is named there, and so are the counter the refusal is counted under
// and the applier's line that is the drop.
func TestTheReplicationGuideSaysWhatTheWriteGatedLineCarries(t *testing.T) {
	t.Parallel()
	logs := &lockedBuffer{}
	h := newHarnessLogging(t, probeDomain{}, slog.New(slog.NewJSONHandler(logs, nil)))
	peerCopy(t, h, "node-b", "op-left")
	h.gates.holdWriter("node-b", statelog.ReasonReleased)
	if _, err := h.write(probeSubject("a"), "op-left", "mine"); !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a write collapsed onto node-b's released copy = %v, want a refusal", err)
	}
	lines := logRecords(t, logs.Bytes(), "statelog_write_gated")
	if len(lines) != 1 {
		t.Fatalf("%d statelog_write_gated lines for one refused write, want one", len(lines))
	}

	row := guideRow(t, "statelog_write_gated")
	for key := range lines[0] {
		switch key {
		case slog.TimeKey, slog.LevelKey, slog.MessageKey:
			continue
		}
		if !strings.Contains(row, "`"+key+"`") {
			t.Errorf("the guide's statelog_write_gated row never names `%s`, a key "+
				"the line carries: %s", key, row)
		}
	}
	for _, name := range []string{metrics.StatelogPublishRefusals, "statelog_record_gated"} {
		if !strings.Contains(row, "`"+name+"`") {
			t.Errorf("the guide's statelog_write_gated row never names `%s` — the "+
				"line is a refusal counted there, not a second drop, and the drop "+
				"is the applier's own line: %s", name, row)
		}
	}
}

// reasonDecision is what one [statelog.Reason] is: a GATE's — a rule under
// which a record the broker accepted applies on no node, and so the reason a
// `statelog_record_gated` line, the records-gated counter and a write refused
// over such a record all carry — and, for a gate, whether it BLAMES the
// record's writer ([statelog.Reason.BlamesWriter]).
type reasonDecision struct{ gate, blames bool }

// reasonDecisions decides both for EVERY reason this build names — not only
// the ones that are true — so a reason added to [statelog.Reasons] fails
// [TestOnlyAGateThatHoldsAWriterBlamesIt] until somebody says which side of
// each it is on, and a gate added that way reaches every surface held to the
// gate set ([TestTheReplicationGuideSaysWhatTheRecordGatedLineCarries]).
var reasonDecisions = map[statelog.Reason]reasonDecision{
	// Gates that drop a record for what its WRITER was or did.
	statelog.ReasonEvicted:        {gate: true, blames: true},
	statelog.ReasonReleased:       {gate: true, blames: true},
	statelog.ReasonAbandoned:      {gate: true, blames: true},
	statelog.ReasonOvertaken:      {gate: true, blames: true},
	statelog.ReasonWrongPartition: {gate: true, blames: true},
	// Gates every writer's record meets alike: the object's marker, a kind
	// no build applies.
	statelog.ReasonDeleted: {gate: true},
	statelog.ReasonRetired: {gate: true},
	// Not a gate's at all: refusals made before or instead of an append,
	// about this node, the log or the operation.
	statelog.ReasonNotHolder:      {},
	statelog.ReasonHoldingUnknown: {},
	statelog.ReasonDeferred:       {},
	statelog.ReasonBehind:         {},
	statelog.ReasonBelowFloor:     {},
	statelog.ReasonFloorUnknown:   {},
	statelog.ReasonLogFull:        {},
	statelog.ReasonSkew:           {},
	statelog.ReasonOpReused:       {},
	statelog.ReasonLogTruncated:   {},
	statelog.ReasonWrongStream:    {},
	statelog.ReasonSuperseded:     {},
}

// ONLY A GATE THAT HOLDS A WRITER BLAMES IT — the five that drop a record for
// what its writer was or did, and none of the rest. The set is what decides
// whether a refusal of another node's copy names that node
// ([statelog.Unavailable.CopyWriter]), so a gate reason moved across it changes
// what every surface tells an operator to do.
//
// EVERY REASON IS DECIDED HERE, NOT ONLY THE FIVE THAT BLAME.
// [statelog.Reason.BlamesWriter] answers false for anything its switch does not
// name, so a test listing only the blaming reasons passed a writer-blaming gate
// added to [statelog.Reasons] and forgotten in the switch — and a collapse onto
// another node's copy under it named no writer, which every surface reads as
// this node's own refusal and answers by sending the write away from the node
// that can finish it. With an explicit answer per reason ([reasonDecisions]), a
// new one fails here until somebody says which side it is on.
func TestOnlyAGateThatHoldsAWriterBlamesIt(t *testing.T) {
	t.Parallel()
	for _, reason := range statelog.Reasons() {
		want, ok := reasonDecisions[reason]
		if !ok {
			t.Errorf("%q is a reason this build names and nothing here decides "+
				"whether it is a gate's and whether its gate blames the writer — "+
				"say which side of each it is on, here and in "+
				"Reason.BlamesWriter", reason)
			continue
		}
		if want.blames && !want.gate {
			t.Errorf("%q is decided to blame a writer and not to be a gate's — only "+
				"a gate drops a record, so only a gate has a writer to blame", reason)
		}
		if got := reason.BlamesWriter(); got != want.blames {
			t.Errorf("%q.BlamesWriter() = %v, want %v", reason, got, want.blames)
		}
	}
	for reason := range reasonDecisions {
		if !slices.Contains(statelog.Reasons(), reason) {
			t.Errorf("%q is decided here and is not a reason this build names", reason)
		}
	}
	if statelog.Reason("from-a-newer-peer").BlamesWriter() {
		t.Error("a reason this build does not name blames the writer — nothing " +
			"here knows what its gate holds")
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

// reanchorRules is a REAL runner whose checkpoint carries the rules a reanchor
// placed on it: it opened generation, abandoned every generation strictly
// between voidAfter and voidBefore, and — for a restored reanchor, a non-zero
// staleAfter — overtook every record after staleAfter in a generation below
// voidBefore. Real rather than a formula typed out again here, so what a
// resolution is told is what the applier's own drop decides.
func reanchorRules(t *testing.T, generation, voidAfter, voidBefore uint32,
	staleAfter uint64) *statelog.Runner {
	t.Helper()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			UPDATE statelog_cursor SET generation = ?, void_after = ?, void_before = ?,
				stale_after = ?`, generation, voidAfter, voidBefore, staleAfter)
		return err
	}); err != nil {
		t.Fatalf("place the reanchor's rules: %v", err)
	}
	h.upgrade(probeDomain{})
	if err := h.boot(0); err != nil {
		t.Fatalf("boot: %v", err)
	}
	return h.runner
}

// land puts a record of another operation on another subject straight on the
// log, so the next append lands past it.
func land(t *testing.T, h *harness, opID string) {
	t.Helper()
	if _, _, err := h.log.Append(t.Context(), probePrefix+".object.filler", opID, nil,
		probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-c"}, opID, "filler")); err != nil {
		t.Fatalf("land %s: %v", opID, err)
	}
}

// A WRITE A REANCHOR'S RULE VOIDED IS REFUSED UNDER THAT RULE, not reported as
// a ledger contract violation — and the rule is asked about THE RECORD'S OWN
// STAMP AND POSITION.
//
// A node whose rows were a restored copy's age goes on writing in the old
// generation until it learns of the move; its record lands after the
// reanchor's own and is overtaken on every node, and writes no ledger row. The
// domain's gates know nothing of that rule — it is the framework's, on the
// checkpoint — so a resolution that asked only them found no gate and a ledger
// that vouched, and called the write a record applied without its row.
//
// WHICH generation and sequence it asks is the whole of the rule, and each is
// the record's: the generation this call stamped its own append with, and the
// one a copy it was collapsed onto carries — another node's, written before
// this node's rows moved on. Asked about generation zero at sequence zero, the
// rules void nothing and the contract violation is back; asked about this
// call's stamp for a copy stamped otherwise, the copy's rule is missed.
func TestAWriteAReanchorVoidedIsRefusedUnderItsRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// rules is the reanchor's: the generation it opened, the ones it
		// abandoned (strictly between after and before) and, when non-zero,
		// the generation record after which it overtook older ones.
		generation, after, before uint32
		staleAfter                uint64
		// nodeGen is the generation this node's rows — and so its own
		// stamp — are in, and copyGen, when non-zero, the generation a copy
		// of the operation already on the log was stamped with by another
		// node, onto which this node's append is collapsed.
		nodeGen, copyGen uint32
		filler           bool
		reason           statelog.Reason
		asked            voidedQuestion
	}{{
		// THIS NODE'S OWN APPEND, stamped in the generation a restored
		// reanchor overtook and landing after its generation record at 1.
		name: "own append overtaken", generation: 2, after: 1, before: 2, staleAfter: 1,
		nodeGen: 1, filler: true,
		reason: statelog.ReasonOvertaken, asked: voidedQuestion{Gen: 1, Seq: 2},
	}, {
		// THIS NODE'S OWN APPEND, stamped in a generation a reanchor
		// abandoned.
		name: "own append abandoned", generation: 2, after: 0, before: 2,
		nodeGen: 1,
		reason:  statelog.ReasonAbandoned, asked: voidedQuestion{Gen: 1, Seq: 1},
	}, {
		// ANOTHER NODE'S COPY, stamped in a generation the reanchor
		// abandoned — while this node's own stamp is in the generation it
		// opened, which the rule voids nothing in.
		name: "copy abandoned", generation: 2, after: 0, before: 2,
		nodeGen: 2, copyGen: 1,
		reason: statelog.ReasonAbandoned, asked: voidedQuestion{Gen: 1, Seq: 1},
	}, {
		// ANOTHER NODE'S COPY, stamped in the generation a restored
		// reanchor overtook and landing after its generation record at 1.
		name: "copy overtaken", generation: 2, after: 1, before: 2, staleAfter: 1,
		nodeGen: 2, copyGen: 1, filler: true,
		reason: statelog.ReasonOvertaken, asked: voidedQuestion{Gen: 1, Seq: 2},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.gen.Store(tc.nodeGen)
			h.applier.mu.Lock()
			h.applier.committed.Generation = tc.nodeGen
			h.applier.rules = reanchorRules(t, tc.generation, tc.after, tc.before, tc.staleAfter)
			h.applier.mu.Unlock()
			if tc.filler {
				land(t, h, "op-filler")
			}
			if tc.copyGen != 0 {
				// ANOTHER NODE'S COPY OF THE OPERATION, which this node's
				// applier passes and drops under the rule.
				peerCopyIn(t, h, "node-b", "op-voided", tc.copyGen)
			} else {
				// THIS NODE'S OWN RECORD LANDS AND ITS APPLIER PASSES IT,
				// dropping it under the rule — so no ledger row is written.
				h.applier.mu.Lock()
				h.applier.auto = false
				h.applier.mu.Unlock()
				h.applier.advance(statelog.Position{Stream: probeStream,
					Generation: tc.nodeGen, Seq: 1_000})
			}

			res, err := h.write(probeSubject("a"), "op-voided", "mine")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
				t.Fatalf("a write the reanchor's rule voided = (%+v, %v), want a "+
					"refusal %q", res, err, tc.reason)
			}
			if refusal.Position.Seq != tc.asked.Seq || refusal.OpID != "op-voided" {
				t.Errorf("the refusal %+v does not name the landing at %d — the "+
					"record is on the log and holds its operation id", refusal, tc.asked.Seq)
			}
			// ANOTHER NODE'S COPY IS NAMED, and this node's own record is
			// not: the reason is the copy's writer's standing only there.
			var named string
			if tc.copyGen != 0 {
				named = "node-b"
			}
			if refusal.CopyWriter != named {
				t.Errorf("the refusal names %q as the copy's writer, want %q",
					refusal.CopyWriter, named)
			}
			if asked := h.applier.voidedQuestions(); !slices.Equal(asked,
				[]voidedQuestion{tc.asked}) {
				t.Errorf("the rules were asked about %+v, want the record's own "+
					"stamp and position %+v", asked, tc.asked)
			}
		})
	}
}

// THE RUNNER VOIDS WHAT ITS CHECKPOINT'S REANCHOR SAID TO — and a resolution
// asks it exactly what the applier asked.
func TestTheRunnerAnswersTheRulesItsCheckpointCarries(t *testing.T) {
	t.Parallel()
	// A RESTORED REANCHOR'S CHECKPOINT: generations 3 and 4 abandoned, and
	// every lower generation's record after sequence 100 overtaken.
	runner := reanchorRules(t, 5, 2, 5, 100)
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
		reason, voided := runner.Voided(tc.gen, tc.seq)
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
