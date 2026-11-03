package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: gen, Writer: writer}, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, opID, "the peer's")))
	if err != nil {
		t.Fatalf("land %s's copy: %v", writer, err)
	}
	at := statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: seq}
	h.applier.advance(at)
	return at
}

// signedByPeer is body as a peer publishes it: a frame signed under the fleet
// keyring, which is all a state log holds. A peer signs with the same keyring
// this node verifies under, so a frame signed here is one a peer would have
// written — and a body appended raw is one the publisher's reader of its log
// refuses as not this fleet's.
func signedByPeer(t *testing.T, h *harness, body []byte) []byte {
	t.Helper()
	return testSigner(t, h.deps.Domain).Seal(body)
}

// A WRITE COLLAPSED ONTO ANOTHER NODE'S GATED COPY OF ITS OPERATION IS TOLD
// THAT COPY'S GATE — AND WHOSE IT IS.
//
// A node evicted while one of its writes was in flight has that write on the
// log above its eviction, applying nowhere — and holding the operation id in
// the broker's duplicate window. Another node, handed the same operation
// inside the window, has its append collapsed onto that record. Its resolution
// finds no ledger row, and whether a gate dropped the record is a question
// about the record's WRITER: asked about itself, the other node found nothing,
// trusted a ledger that vouched, and reported a contract violation for a
// record that was only ever gated.
//
// And the refusal names that writer, because the reason is then its standing
// and not the refusing node's: read as this node's own `evicted`, an operator
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
			at := peerCopy(t, h, tc.writer, "op-evicted")
			h.gates.holdWriter(tc.writer, statelog.ReasonEvicted)

			res, err := h.write(probeSubject("a"), "op-evicted", "mine")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) {
				t.Fatalf("a write collapsed onto %s's evicted copy = (%+v, %v), "+
					"want a refusal %q", tc.writer, res, err, statelog.ReasonEvicted)
			}
			if refusal.Reason != statelog.ReasonEvicted || refusal.Position != at ||
				refusal.OpID != "op-evicted" {
				t.Fatalf("refusal = %+v, want %q at %s under op-evicted", refusal,
					statelog.ReasonEvicted, at)
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
			// WHEN TO COME BACK AND WHAT A PERSON IS TOLD: another node's
			// copy clears once the duplicate window lets go of the id, so
			// it carries the window as its hint and asks the person to send
			// it again then, never to readmit a node. This node's own copy
			// is its own eviction, which no wait clears.
			wait := statelog.RetryAfter(err, time.Second)
			said, _ := statelog.ConditionSentence(err)
			if tc.named != "" {
				if wait != 2*time.Minute || !strings.HasSuffix(said,
					statelog.TryAgainLater) || strings.Contains(said, "readmit") {
					t.Errorf("another node's evicted copy waits %s and is told %q, "+
						"want the 2m0s window and %q", wait, said,
						statelog.TryAgainLater)
				}
			} else if wait != 0 || !strings.Contains(said, "readmit") {
				t.Errorf("this node's own evicted copy waits %s and is told %q, "+
					"want no wait and a readmission", wait, said)
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
	peerCopy(t, h, "node-b", "op-evicted")
	h.gates.holdWriter("node-b", statelog.ReasonEvicted)
	if _, err := h.write(probeSubject("a"), "op-evicted", "mine"); !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a write collapsed onto node-b's evicted copy = %v, want a refusal", err)
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
	statelog.ReasonEvicted:   {gate: true, blames: true},
	statelog.ReasonAbandoned: {gate: true, blames: true},
	statelog.ReasonOvertaken: {gate: true, blames: true},
	// A gate every writer's record meets alike: the object's marker.
	statelog.ReasonDeleted: {gate: true},
	// Not a gate's at all: refusals made before or instead of an append,
	// about this node, the log or the operation.
	statelog.ReasonEvictionUnknown: {},
	statelog.ReasonDeferred:        {},
	statelog.ReasonBehind:          {},
	statelog.ReasonBelowFloor:      {},
	statelog.ReasonFloorUnknown:    {},
	statelog.ReasonLogFull:         {},
	statelog.ReasonRecordTooLarge:  {},
	statelog.ReasonBrokerRefused:   {},
	statelog.ReasonSkew:            {},
	statelog.ReasonOpReused:        {},
	statelog.ReasonLogTruncated:    {},
	statelog.ReasonWrongStream:     {},
	statelog.ReasonSuperseded:      {},
}

// ONLY A GATE THAT HOLDS A WRITER BLAMES IT — the three that drop a record for
// what its writer was or did, and none of the rest. The set is what decides
// whether a refusal of another node's copy names that node
// ([statelog.Unavailable.CopyWriter]), so a gate reason moved across it changes
// what every surface tells an operator to do.
//
// EVERY REASON IS DECIDED HERE, NOT ONLY THE THREE THAT BLAME.
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
		if got := reason.FromGate(); got != want.gate {
			t.Errorf("%q.FromGate() = %v, want %v", reason, got, want.gate)
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
	if statelog.Reason("from-a-newer-peer").FromGate() {
		t.Error("a reason this build does not name is a gate's — nothing here " +
			"knows that a record landed under it")
	}
}

// ONLY A GATE'S REFUSAL OF A RECORD OF ITS OWN OPERATION LANDED. Every reason,
// with and without a position, with and without [statelog.Unavailable.Dropped]:
// a refusal made before the append carries positions too — the point a
// `behind` write needs, a `deferred` record, a `skew`'s last sequence, the
// record an `op_reused` id names — and so does a gate's refusal at ANOTHER
// operation's record, at which the gate holds this node; a caller that read
// any of those as a record that landed and applies nowhere told its client a
// write was durable and lost when nothing of it was known to be on the log and
// another node could make it.
func TestOnlyAGateRefusalOfItsOwnRecordLanded(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 42}
	for _, reason := range statelog.Reasons() {
		for _, dropped := range []bool{false, true} {
			if (&statelog.Unavailable{Reason: reason, Dropped: dropped}).Landed() {
				t.Errorf("a %q refusal with no position (dropped %v) landed — nothing "+
					"names a record", reason, dropped)
			}
		}
		if (&statelog.Unavailable{Reason: reason, Position: at}).Landed() {
			t.Errorf("a %q refusal at %s that names no record of its own operation "+
				"landed — the record there may be anybody's", reason, at)
		}
		want := reasonDecisions[reason].gate
		if got := (&statelog.Unavailable{Reason: reason, Position: at, Dropped: true}).Landed(); got != want {
			t.Errorf("a %q refusal of its own record at %s: Landed() = %v, want %v",
				reason, at, got, want)
		}
	}
}

// A GATED WRITE'S REFUSAL SAYS WHETHER THE RECORD IT NAMES IS THE WRITE'S OWN,
// on both arms of the resolution.
//
// Where the record at the refusal's position carries this operation — this
// write's own acknowledged append, the copy the broker collapsed it onto, or
// the record a lost answer found newest carrying its id — a gate dropped THAT
// record, and the refusal is [statelog.Unavailable.Landed]. Where a lost answer
// found ANOTHER operation's record newest on the subject and the gates hold
// THIS NODE there, the record named is only where the gate was judged: it may
// be a healthy peer's that applied on every node, and this write's own append
// may never have landed at all. Read as landed, a caller told its client the
// write was durable at that position and dropped everywhere — neither known —
// and answered as lost a write the runner-up could have made.
func TestAGatedWriteSaysWhetherItsRecordIsItsOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// settle lands what the case is about and holds what the gate holds;
		// it answers, once the write has run, the position the refusal must
		// name.
		settle  func(t *testing.T, h *harness) func() statelog.Position
		op      string
		reason  statelog.Reason
		dropped bool
	}{{
		name: "this write's own acknowledged record", op: "op-own",
		reason: statelog.ReasonEvicted, dropped: true,
		settle: func(t *testing.T, h *harness) func() statelog.Position {
			h.applier.mu.Lock()
			h.applier.auto = false
			h.applier.mu.Unlock()
			h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})
			h.gates.holdWriter("node-a", statelog.ReasonEvicted)
			return h.lastAppended
		},
	}, {
		name: "a copy the broker collapsed the append onto", op: "op-copied",
		reason: statelog.ReasonEvicted, dropped: true,
		settle: func(t *testing.T, h *harness) func() statelog.Position {
			at := peerCopy(t, h, "node-b", "op-copied")
			h.gates.holdWriter("node-b", statelog.ReasonEvicted)
			return func() statelog.Position { return at }
		},
	}, {
		name: "this write's own record a lost answer found", op: "op-found",
		reason: statelog.ReasonEvicted, dropped: true,
		settle: func(t *testing.T, h *harness) func() statelog.Position {
			return heldAfterALostAnswer(t, h, false, statelog.ReasonEvicted)
		},
	}, {
		name: "another operation's record a lost answer found, evicted", op: "op-held",
		reason: statelog.ReasonEvicted,
		settle: func(t *testing.T, h *harness) func() statelog.Position {
			return heldAfterALostAnswer(t, h, true, statelog.ReasonEvicted)
		},
	}, {
		name: "another operation's record a lost answer found, deleted", op: "op-purged",
		reason: statelog.ReasonDeleted,
		settle: func(t *testing.T, h *harness) func() statelog.Position {
			return heldAfterALostAnswer(t, h, true, statelog.ReasonDeleted)
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			at := tc.settle(t, h)

			res, err := h.write(probeSubject("a"), tc.op, "mine")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
				t.Fatalf("the write = (%+v, %v), want a refusal %q", res, err, tc.reason)
			}
			if want := at(); refusal.Position != want || refusal.OpID != tc.op {
				t.Errorf("the refusal names %s under %s, want %s under %s",
					refusal.Position, refusal.OpID, want, tc.op)
			}
			if refusal.Dropped != tc.dropped || refusal.Landed() != tc.dropped {
				t.Errorf("the refusal at %s: Dropped = %v, Landed() = %v, want %v — "+
					"the record there is this operation's, and a gate dropped it, in "+
					"exactly the cases that say so", refusal.Position, refusal.Dropped,
					refusal.Landed(), tc.dropped)
			}
			if !tc.dropped && refusal.CopyWriter != "" {
				t.Errorf("a refusal held at another operation's record names %q as a "+
					"copy's writer — it is about this node", refusal.CopyWriter)
			}
		})
	}
}

// lastAppended is where this harness's appender last put a record.
func (h *harness) lastAppended() statelog.Position {
	return statelog.Position{Stream: probeStream, Generation: h.gen.Load(),
		Seq: uint64(h.appends.lastSeq.Load())}
}

// heldAfterALostAnswer has the next write's append on subject a lose its
// answer — after landing, or (swallowed) without landing at all, in which case
// a PEER's record of another operation lands on the subject before the
// publisher asks the broker what the subject holds. The subject is
// established first, so the write takes the anchored path; this node's
// applier passes everything without a ledger row for the write; and the gate
// holds this node under reason. It answers, once the write has run, the
// newest record on the subject: the peer's, or the write's own.
func heldAfterALostAnswer(t *testing.T, h *harness, swallowed bool,
	reason statelog.Reason) func() statelog.Position {

	t.Helper()
	first, err := h.write(probeSubject("a"), "op-0", "one")
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	h.anchorAt(probeSubject("a"), first.Position.Seq)
	h.applier.mu.Lock()
	h.applier.auto = false
	h.applier.mu.Unlock()
	h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})
	if reason == statelog.ReasonDeleted {
		// EVERY WRITER, as the marker holds them.
		h.gates.mu.Lock()
		h.gates.gated, h.gates.reason = true, reason
		h.gates.mu.Unlock()
	} else {
		h.gates.holdWriter("node-a", reason)
	}
	h.appends.fail(errors.New("no response from stream"), swallowed)
	if !swallowed {
		return h.lastAppended
	}
	var peer atomic.Uint64
	h.appends.mu.Lock()
	h.appends.beforeLastSeq = func() {
		// SIGNED, as a peer's record is: the publisher reads it through the
		// verifier to learn it is another operation's.
		seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
			signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"},
				probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}},
				"peer-op", "the peer's")))
		if err != nil {
			t.Errorf("land the peer's record: %v", err)
		}
		peer.Store(seq)
	}
	h.appends.mu.Unlock()
	return func() statelog.Position {
		return statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: peer.Load()}
	}
}

// A COPY THIS NODE CANNOT READ IS ONE WHOSE WRITER NOBODY HERE CAN NAME — so
// whether a gate dropped it is unknown, and the answer is `unknown` rather than
// either guess.
func TestACollapsedWriteWhoseCopyCannotBeReadIsUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	peerCopy(t, h, "node-b", "op-unread")
	h.gates.holdWriter("node-b", statelog.ReasonEvicted)
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
		Log: struct{ statelog.Appender }{}, Rows: struct{ statelog.Rows }{},
		Fence: struct{ statelog.Fence }{}, Gates: struct{ statelog.Gates }{},
		Waiter: struct{ statelog.Waiter }{}, Voids: struct{ statelog.Voids }{},
		Identity:  struct{ statelog.Identity }{},
		Signer:    testSigner(t, probeDomain{}),
		Admission: noCeiling(t), NodeID: "node-a",
		Generation: func() uint32 { return 1 },
	}
	if _, err := statelog.NewPublisher(deps); err == nil ||
		!strings.Contains(err.Error(), "reader of its log") {
		t.Fatalf("a publisher with no reader of its log was built (%v)", err)
	}
	deps.Records = statelog.VerifiedLog{
		Log:      struct{ statelog.LogReader }{},
		Verifier: testVerifier(t, probeDomain{}),
	}
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
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-c"}, probeSubject("filler"), statelog.ScopeSet{Paths: []string{"object.filler"}},
			opID, "filler"))); err != nil {
		t.Fatalf("land %s: %v", opID, err)
	}
}

// A WRITE A REANCHOR'S RULE VOIDED IS REFUSED UNDER THAT RULE, not reported as
// a ledger contract violation — and the rule is asked about THE RECORD'S OWN
// STAMP AND POSITION.
//
// A node whose rows were a restored copy's age goes on writing in the old
// generation until it learns of the move; its record lands after the
// reanchor's own and is overtaken on every data node, and writes no ledger
// row. The domain's gates know nothing of that rule — it is the framework's,
// on the checkpoint — so a resolution that asked only them found no gate and a
// ledger that vouched, and called the write a record applied without its row.
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
		Log: struct{ statelog.Appender }{},
		Records: statelog.VerifiedLog{
			Log: struct{ statelog.LogReader }{}, Verifier: testVerifier(t, probeDomain{}),
		},
		Rows: struct{ statelog.Rows }{}, Fence: struct{ statelog.Fence }{},
		Gates: struct{ statelog.Gates }{}, Waiter: struct{ statelog.Waiter }{},
		Identity:  struct{ statelog.Identity }{},
		Signer:    testSigner(t, probeDomain{}),
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
