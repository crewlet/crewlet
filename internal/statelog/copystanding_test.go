package statelog_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// standingDomain is the probe domain with an eviction subject read off its log
// ([statelog.EvictionProbe]), as every identity-claiming domain has: a node's
// standing is the last record on `evict.<node>`, an eviction where its body
// says so and a readmission otherwise.
type standingDomain struct{ probeDomain }

func (standingDomain) EvictionSubject(node string) statelog.Subject {
	return statelog.Subject{Kind: "evict", ID: node}
}

func (standingDomain) Evicts(payload []byte) (bool, error) {
	var record struct{ Body string }
	if err := json.Unmarshal(payload, &record); err != nil {
		return false, err
	}
	return record.Body == "evict", nil
}

// standOn lands, straight on the log, the operator's record of node's standing
// — an eviction where evicts, a readmission otherwise — WITHOUT this node's
// applier reaching it, which is what a standing written above this node's
// rows looks like. It answers the record's sequence.
func standOn(t *testing.T, h *harness, node string, evicts bool) uint64 {
	t.Helper()
	body, opID := "readmit", "op-readmit-"+node
	if evicts {
		body, opID = "evict", "op-evict-"+node
	}
	standing := standingDomain{}.EvictionSubject(node)
	seq, _, err := h.log.Append(t.Context(), probePrefix+".evict."+node, opID+body, nil,
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-ops"},
			standing, statelog.ScopeSet{Paths: []string{standing.String()}}, opID, body)))
	if err != nil {
		t.Fatalf("land %s's standing: %v", node, err)
	}
	return seq
}

// landCopy lands, straight on the log, writer's copy of opID on subject a
// WITHOUT this node's applier reaching it (the copy of an operation another
// node published while this node was behind) and answers where it landed.
//
// THE ANCHOR IS PINNED AT THE COPY, so this node's append is arbitrated
// against it and collapsed onto it at once; left to the rows, the append
// would lose the race and wait for this node to apply the copy, which a case
// about a node that has not applied it must not do.
func landCopy(t *testing.T, h *harness, writer, opID string) statelog.Position {
	t.Helper()
	h.applier.mu.Lock()
	h.applier.auto = false
	h.applier.mu.Unlock()
	seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a", opID, nil,
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: writer},
			probeSubject("a"), statelog.ScopeSet{Paths: []string{probeSubject("a").String()}},
			opID, "the peer's")))
	if err != nil {
		t.Fatalf("land %s's copy: %v", writer, err)
	}
	at := statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: seq}
	h.rows.pin(probeSubject("a"), at)
	return at
}

// A COLLAPSED WRITE IS PENDING ONLY WHERE THE COPY'S WRITER STILL STANDS:
// pending promises every node applies it ([statelog.OutcomePending]), and a
// copy whose writer was evicted in flight applies nowhere. Both answers given
// before this node applied the copy ([statelog.Request.NoWait], a wait run
// out) judge its writer at the copy, by this node's rows where they reach it
// and else by the last record on its eviction subject: a healthy peer's copy
// (router failover) stays pending; a held writer, or an unreadable standing,
// is unknown with no position, and a retry meets the writer's gate. This
// node's own copy is vouched by the fence it cleared first.
//
// Mutations, each red: answer every copy pending; drop the rows' gate or the
// log's standing; read a readmission below as unvouched; judge its own copy.
func TestACollapsedWriteIsPendingOnlyWhereTheCopysWriterStands(t *testing.T) {
	t.Parallel()
	type arm struct {
		name  string
		write func(h *harness, opID string) (statelog.Result, error)
	}
	arms := []arm{
		{name: "nothing waited", write: func(h *harness, opID string) (statelog.Result, error) {
			return h.writeNoWait(probeSubject("a"), opID, "mine")
		}},
		{name: "the wait ran out", write: func(h *harness, opID string) (statelog.Result, error) {
			return h.write(probeSubject("a"), opID, "mine")
		}},
	}
	for _, a := range arms {
		for _, tc := range []struct {
			name    string
			writer  string
			arrange func(t *testing.T, h *harness) // before the copy lands
			after   func(t *testing.T, h *harness) // after it
			pending bool
			asked   []string
		}{
			{
				name: "this node's own earlier copy", writer: "node-a",
				pending: true,
			},
			{
				name: "a healthy peer's copy", writer: "node-b",
				pending: true, asked: []string{"node-b"},
			},
			{
				name: "a peer the rows evict", writer: "node-b",
				arrange: func(_ *testing.T, h *harness) {
					h.gates.holdWriter("node-b", statelog.ReasonEvicted)
				},
				asked: []string{"node-b"},
			},
			{
				name: "a peer the log evicts below the copy, ahead of the rows", writer: "node-b",
				arrange: func(t *testing.T, h *harness) { standOn(t, h, "node-b", true) },
				asked:   []string{"node-b"},
			},
			{
				name: "a peer the log readmits below the copy", writer: "node-b",
				arrange: func(t *testing.T, h *harness) {
					standOn(t, h, "node-b", true)
					standOn(t, h, "node-b", false)
				},
				pending: true, asked: []string{"node-b"},
			},
			{
				name: "a peer whose last standing is above the copy and the rows", writer: "node-b",
				after: func(t *testing.T, h *harness) { standOn(t, h, "node-b", true) },
				asked: []string{"node-b"},
			},
		} {
			t.Run(a.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				h := newHarnessFor(t, standingDomain{})
				if tc.arrange != nil {
					tc.arrange(t, h)
				}
				at := landCopy(t, h, tc.writer, "op-copied")
				if tc.after != nil {
					tc.after(t, h)
				}

				res, err := a.write(h, "op-copied")
				if err != nil {
					t.Fatalf("a write collapsed onto %s's copy = %v, want an outcome", tc.writer, err)
				}
				want := statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op-copied"}
				if tc.pending {
					want = statelog.Result{Outcome: statelog.OutcomePending, Position: at, OpID: "op-copied"}
				}
				if res.Outcome != want.Outcome || res.Position != want.Position || res.OpID != want.OpID {
					t.Fatalf("a write collapsed onto %s's copy at %s = %+v, want %s at %v — "+
						"pending promises every other node applies the record", tc.writer, at,
						res, want.Outcome, want.Position)
				}
				if waited := a.name == "the wait ran out"; res.Waited != waited {
					t.Errorf("the answer says waited=%v, want %v", res.Waited, waited)
				}
				if asked := h.gates.askedAbout(); !slices.Equal(asked, tc.asked) {
					t.Errorf("the gates were asked about %v, want %v", asked, tc.asked)
				}
			})
		}
	}
}

// A STANDING THE ROWS ALREADY HOLD IS THE ROWS' TO ANSWER: the log's last word
// on a writer is read only for what the rows have not applied. A node past a
// peer's later eviction holds it in its rows, and an eviction ABOVE the copy
// holds nothing at the copy, so the copy is pending, not unknown.
//
// Mutation: drop the rows' coverage from the judgement, and the copy is
// answered unknown.
func TestAStandingTheRowsHoldIsAnsweredByTheRows(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, standingDomain{})
	at := landCopy(t, h, "node-b", "op-copied")
	seq := standOn(t, h, "node-b", true)
	h.applier.advance(statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: seq})

	res, err := h.writeNoWait(probeSubject("a"), "op-copied", "mine")
	if err != nil {
		t.Fatalf("a write collapsed onto node-b's copy = %v, want an outcome", err)
	}
	if res.Outcome != statelog.OutcomePending || res.Position != at {
		t.Fatalf("a node whose rows hold node-b's eviction above the copy at %s "+
			"answered %+v, want pending there — the eviction holds nothing below it",
			at, res)
	}
}

// A NODE THAT HAS APPLIED THE COPY ANSWERS IT FROM ITS ROWS. When a read past
// a cleared wait fails (store busy or closed), the copy is judged by what is
// known of it: rows past the copy answer at its position from everything
// below it, and a standing taken ABOVE the copy holds nothing there, so a
// write every node applies stays pending rather than costing a retry.
//
// Mutation: drop the arm that answers from rows at or past the copy, and the
// eviction above it reads as unvouched.
func TestACopyThisNodeHasAppliedIsAnsweredByItsRows(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, standingDomain{})
	at := peerCopy(t, h, "node-b", "op-copied")
	standOn(t, h, "node-b", true)
	// THE LEDGER READ PAST THE WAIT FAILS, once and only once the append has
	// landed: the ledger is read before the decision too.
	var failed atomic.Bool
	h.applier.mu.Lock()
	h.applier.beforeOp = func() error {
		if h.appends.lastSeq.Load() > 0 && failed.CompareAndSwap(false, true) {
			return errors.New("the replicated estate is closed")
		}
		return nil
	}
	h.applier.mu.Unlock()

	res, err := h.write(probeSubject("a"), "op-copied", "mine")
	if err != nil {
		t.Fatalf("a write collapsed onto node-b's applied copy = %v, want an outcome", err)
	}
	if res.Outcome != statelog.OutcomePending || res.Position != at || !res.Waited {
		t.Fatalf("a node past node-b's copy at %s, whose ledger read failed and "+
			"whose eviction of node-b is above the copy, answered %+v, want "+
			"pending there after the wait — the eviction holds nothing below it",
			at, res)
	}
}

// THE REPLICATION GUIDE SAYS WHAT THE UNVOUCHED-COPY LINE CARRIES.
// `statelog_resolve_copy_unvouched` is the only witness of why a collapsed
// write was answered `unknown` rather than `pending`, and the writer it names
// is not the node that logged it, so the guide's row is held to every key the
// line carries.
func TestTheReplicationGuideSaysWhatTheUnvouchedCopyLineCarries(t *testing.T) {
	t.Parallel()
	logs := &lockedBuffer{}
	h := newHarnessLogging(t, standingDomain{}, slog.New(slog.NewJSONHandler(logs, nil)))
	landCopy(t, h, "node-b", "op-copied")
	h.gates.holdWriter("node-b", statelog.ReasonEvicted)
	if res, err := h.writeNoWait(probeSubject("a"), "op-copied", "mine"); err != nil ||
		res.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("a write collapsed onto node-b's evicted copy = (%+v, %v), want unknown", res, err)
	}
	lines := logRecords(t, logs.Bytes(), "statelog_resolve_copy_unvouched")
	if len(lines) != 1 {
		t.Fatalf("%d statelog_resolve_copy_unvouched lines for one unvouched copy, want one", len(lines))
	}
	row := guideRow(t, "statelog_resolve_copy_unvouched")
	for key := range lines[0] {
		switch key {
		case slog.TimeKey, slog.LevelKey, slog.MessageKey:
			continue
		}
		if !strings.Contains(row, "`"+key+"`") {
			t.Errorf("the guide's statelog_resolve_copy_unvouched row never names `%s`, "+
				"a key the line carries: %s", key, row)
		}
	}
}

// A FAILED READ OF A COLLAPSED COPY IS A READ PAST THE ACKNOWLEDGEMENT: the
// broker said the record at the position carries this operation, so the
// failure is answered by what is known of the record (read again and, when
// that answers, judge the copy's writer), never `unknown` on the spot, which
// would make a write every node applies one its caller must retry. On a
// domain with no ledger, a copy this node applied with no gate dropping it
// stays pending at its position.
//
// Mutation: answer the failed read on the spot, as the found record's is, and
// the copy is unknown.
func TestAFailedReadOfACollapsedCopyIsAnsweredByTheCopy(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, ledgerless{})
	at := peerCopy(t, h, "node-b", "op-copied")
	h.records.failOnce(errors.New("no response from stream"))

	res, err := h.write(probeSubject("a"), "op-copied", "mine")
	if err != nil {
		t.Fatalf("a write collapsed onto node-b's copy, whose read failed once = %v, "+
			"want an outcome", err)
	}
	if res.Outcome != statelog.OutcomePending || res.Position != at || !res.Waited {
		t.Fatalf("a write collapsed onto node-b's healthy copy at %s, whose first "+
			"read failed = %+v, want pending there after the wait — the broker said "+
			"the record is this operation's, and the read after the fault names "+
			"its writer", at, res)
	}
}
