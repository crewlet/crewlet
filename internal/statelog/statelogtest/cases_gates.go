package statelogtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// GateCandidate is a domain whose applier installs the two gates a strict
// domain has — a permanent DELETION MARKER on one object kind, and a node's
// EVICTION WINDOW, opened by an operator's eviction or by the node's own
// release of the log — together with the publisher-side reader,
// [statelog.Gates], that has to report them.
//
// A CANDIDATE OF ITS OWN rather than more fields on [Candidate], because not
// every domain has the gates: the vectors gate nothing. A field a domain may
// leave nil is a field a domain that DOES gate can forget with nothing
// failing, while a separate entry point is one a gating domain's own test
// file calls by name, beside [Run].
type GateCandidate struct {
	Candidate

	// Reader is the publisher's side of the gates, over the estate the
	// cases applied into.
	Reader func(db store.PartitionReader) statelog.Gates

	// Kind is the OBJECT's own subject kind: the one Create, Write and
	// Purge publish on, and the one whose marker the history purges.
	Kind string

	// SubjectKinds is EVERY subject kind the domain's records carry —
	// arbitrated or not, its gates' own included — and it holds Kind.
	//
	// The cases build a record of each kind about a purged object and
	// about one nobody purged, through Encode, ask the APPLIER which of
	// them the object's marker drops, and hold the reader to the same
	// answer for every one. A marker may cover more than its object's own
	// kind — a tracker turn is a record about its task — and a reader that
	// asked about Kind alone passed every other case here while every
	// applier dropped the rest, so a turn racing its task's purge resolved
	// as a record applied without its ledger row. ALL the kinds rather
	// than the ones the domain believes its marker covers, because that
	// belief is the thing that drifted: the applier is asked, never told.
	SubjectKinds []string

	// The records the cases publish, each for the object (or node) id, the
	// writer and the operation id given. Create brings an object of Kind
	// into existence; Write is an ordinary change to one; Purge destroys
	// one and writes its marker; Evict and Readmit are the inverse pair on
	// a node; Release is the node's own release of the log, which the node
	// itself writes.
	Create  func(id, writer, opID string) ([]byte, error)
	Write   func(id, writer, opID string) ([]byte, error)
	Purge   func(id, writer, opID string) ([]byte, error)
	Evict   func(node, writer, opID string) ([]byte, error)
	Readmit func(node, writer, opID string) ([]byte, error)
	Release func(node, opID string) ([]byte, error)
}

// GateFactory builds a fresh gate candidate for one case.
type GateFactory func(t *testing.T) GateCandidate

// RunGates certifies a domain's gate reader against the rule
// [statelog.Gates] states, on rows the domain's own applier wrote.
//
// ONE FAMILY FOR EVERY READER, because the rule is shared and the readers are
// not: each domain spells its own two queries, and the two readers agreed
// only because their code looked alike — which is how one of them came to
// answer the eviction first while the other answered the deletion.
//
// And it asks the APPLIER, for every subject kind the domain's records carry,
// which records about a purged object its marker drops, and holds the reader
// to the same answer for each — because the marker covers whatever subjects
// the applier says it does, and the tracker's reader once covered the task's
// own while its applier dropped the task's turns too.
//
// It then turns the same cases on the candidate's own reader bent each way a
// reader has been, or could plausibly be, written wrong, and requires every
// one of them to be reported. This package has no control domain that
// installs gates, so the candidate is its own control: a family that passes
// a reader answering the eviction first certifies nothing about precedence.
// And it bends the candidate's own APPLIER the way the release gate could be
// dropped — the writer's records after its release applied as though it had
// never left — and requires that reported too.
//
// And it holds the domain's NODE-GATE answer to the same records
// ([GateNodeGates]): its eviction and readmission are node gates, and its
// purge — the other gate, which [NodeGates] has no record of — is not. That
// case is bent the same way, through the domain rather than the reader.
func RunGates(t *testing.T, new GateFactory) {
	t.Helper()
	t.Run("the reader reports the gate the rule names", func(t *testing.T) {
		if err := GateAgreement(t, new); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the cases catch a reader that asks the marker about the wrong kinds", func(t *testing.T) {
		for name, bend := range kindLiars {
			t.Run(name, func(t *testing.T) {
				covered, err := gateAgreement(t, func(t *testing.T) GateCandidate {
					c := new(t)
					own, honest := c.Kind, c.Reader
					c.Reader = func(db store.PartitionReader) statelog.Gates {
						return bend.bend(own, honest(db))
					}
					return c
				})
				if err == nil && (!bend.needsAnotherKind || len(covered) > 1) {
					t.Errorf("a reader that %s passed every gate case — the "+
						"family cannot tell it from one that keeps the rule "+
						"(the marker covers %q)", name, covered)
				}
			})
		}
	})
	t.Run("the cases catch a reader that breaks the rule", func(t *testing.T) {
		for name, bend := range gateLiars {
			t.Run(name, func(t *testing.T) {
				err := GateAgreement(t, func(t *testing.T) GateCandidate {
					c := new(t)
					honest := c.Reader
					c.Reader = func(db store.PartitionReader) statelog.Gates {
						return bend(honest(db))
					}
					return c
				})
				if err == nil {
					t.Errorf("a reader that %s passed every gate case — the "+
						"family cannot tell it from one that keeps the rule", name)
				}
			})
		}
	})
	// A PROBE THAT REACHES NO MARKER agrees with any reader at all, so the
	// family refuses one rather than reporting the agreement: here every
	// record the probe encodes is about an object nobody purged.
	t.Run("the cases catch a coverage probe that reaches no marker", func(t *testing.T) {
		err := GateAgreement(t, func(t *testing.T) GateCandidate {
			c := new(t)
			encode := c.Encode
			c.Encode = func(kind, id, opID string, version int) ([]byte, error) {
				return encode(kind, id+"-elsewhere", opID, version)
			}
			return c
		})
		if err == nil {
			t.Error("a coverage probe whose records are about no purged object " +
				"passed every gate case — its agreement certifies nothing")
		}
	})
	t.Run("the cases catch an applier that breaks the rule", func(t *testing.T) {
		for name, bend := range applierLiars {
			t.Run(name, func(t *testing.T) {
				err := GateAgreement(t, func(t *testing.T) GateCandidate {
					c := new(t)
					c.Applier = bend(c.Kind, c.Applier)
					return c
				})
				if err == nil {
					t.Errorf("an applier that %s passed every gate case — the "+
						"family cannot tell it from one that keeps the rule", name)
				}
			})
		}
	})
	t.Run("only a node's eviction, release and readmission are node gates", func(t *testing.T) {
		if err := GateNodeGates(new(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the cases catch a domain that calls the wrong record a node gate", func(t *testing.T) {
		for name, bend := range nodeGateLiars {
			t.Run(name, func(t *testing.T) {
				c := new(t)
				c.Domain = bend(c.Domain)
				if GateNodeGates(c) == nil {
					t.Errorf("a domain that %s passed the node-gate case — the "+
						"family cannot tell it from one that keeps the rule", name)
				}
			})
		}
	})
}

// gateRecord is one record the gate cases apply, in order.
type gateRecord struct {
	label string
	body  func(c GateCandidate) ([]byte, error)

	// drops is every reason the APPLIER may drop the record under, and
	// empty for a record it applies. A record both gates hold lists both:
	// which one the applier counts a double drop under is its own
	// business, and only the reader's answer is the rule's.
	drops []statelog.Reason
}

// The ids the gate cases use. obj-1 is purged AFTER an evicted writer's
// record on it, obj-3 BEFORE one, and obj-2 never. node-c is the node that
// releases the log.
const (
	gateObj1     = "gates-obj-1"
	gateObj2     = "gates-obj-2"
	gateObj3     = "gates-obj-3"
	gateCounted  = "gates-node-a"
	gateEvicted  = "gates-node-b"
	gateReleased = "gates-node-c"

	// ungatedKind is a subject kind no domain declares, so no deletion
	// marker covers it: a record of it is asked about its writer only.
	ungatedKind = "statelogtest-no-deletion-gate"
)

var (
	dropsEvicted        = []statelog.Reason{statelog.ReasonEvicted}
	dropsDeleted        = []statelog.Reason{statelog.ReasonDeleted}
	dropsBoth           = []statelog.Reason{statelog.ReasonEvicted, statelog.ReasonDeleted}
	dropsReleased       = []statelog.Reason{statelog.ReasonReleased}
	dropsReleasedPurged = []statelog.Reason{statelog.ReasonReleased, statelog.ReasonDeleted}
)

// gateHistory is the one sequence every gate case reads, at positions 1..17.
//
// Each gate is held ALONE on some record and BOTH on another, the purge that
// makes obj-1's marker lands ABOVE a record the eviction already dropped,
// and the eviction window has records on both of its edges. Then node-c
// RELEASES the log — the node's own gate record — and writes after it, a
// write it had in flight while it left: dropped, alone and beside the
// deletion marker, until a readmission takes it back.
var gateHistory = []gateRecord{
	{label: "create obj-1", body: func(c GateCandidate) ([]byte, error) {
		return c.Create(gateObj1, gateCounted, "gates-create-1")
	}},
	{label: "create obj-2", body: func(c GateCandidate) ([]byte, error) {
		return c.Create(gateObj2, gateCounted, "gates-create-2")
	}},
	{label: "create obj-3", body: func(c GateCandidate) ([]byte, error) {
		return c.Create(gateObj3, gateCounted, "gates-create-3")
	}},
	{label: "evict node-b", body: func(c GateCandidate) ([]byte, error) { // 4
		return c.Evict(gateEvicted, gateCounted, "gates-evict")
	}},
	{label: "node-b writes obj-2", drops: dropsEvicted, // 5
		body: func(c GateCandidate) ([]byte, error) {
			return c.Write(gateObj2, gateEvicted, "gates-evicted")
		}},
	{label: "node-b writes obj-1", drops: dropsEvicted, // 6
		body: func(c GateCandidate) ([]byte, error) {
			return c.Write(gateObj1, gateEvicted, "gates-evicted-then-purged")
		}},
	{label: "purge obj-3", body: func(c GateCandidate) ([]byte, error) { // 7
		return c.Purge(gateObj3, gateCounted, "gates-purge-3")
	}},
	{label: "node-b writes the purged obj-3", drops: dropsBoth, // 8
		body: func(c GateCandidate) ([]byte, error) {
			return c.Write(gateObj3, gateEvicted, "gates-both")
		}},
	{label: "purge obj-1", body: func(c GateCandidate) ([]byte, error) { // 9
		return c.Purge(gateObj1, gateCounted, "gates-purge-1")
	}},
	{label: "node-a writes the purged obj-3", drops: dropsDeleted, // 10
		body: func(c GateCandidate) ([]byte, error) {
			return c.Write(gateObj3, gateCounted, "gates-deleted")
		}},
	{label: "readmit node-b", body: func(c GateCandidate) ([]byte, error) { // 11
		return c.Readmit(gateEvicted, gateCounted, "gates-readmit")
	}},
	{label: "readmitted node-b writes obj-2", body: func(c GateCandidate) ([]byte, error) { // 12
		return c.Write(gateObj2, gateEvicted, "gates-back")
	}},
	{label: "node-c releases the log", body: func(c GateCandidate) ([]byte, error) { // 13
		return c.Release(gateReleased, "gates-release")
	}},
	{label: "node-c writes obj-2 after its release", drops: dropsReleased, // 14
		body: func(c GateCandidate) ([]byte, error) {
			return c.Write(gateObj2, gateReleased, "gates-released")
		}},
	{label: "node-c writes the purged obj-3 after its release", drops: dropsReleasedPurged, // 15
		body: func(c GateCandidate) ([]byte, error) {
			return c.Write(gateObj3, gateReleased, "gates-released-purged")
		}},
	{label: "readmit node-c", body: func(c GateCandidate) ([]byte, error) { // 16
		return c.Readmit(gateReleased, gateCounted, "gates-readmit-released")
	}},
	{label: "readmitted node-c writes obj-2", body: func(c GateCandidate) ([]byte, error) { // 17
		return c.Write(gateObj2, gateReleased, "gates-rejoined")
	}},
}

// gateQuestion is one question the reader is asked about the history above.
type gateQuestion struct {
	name   string
	kind   string // empty is the candidate's own Kind
	id     string
	writer string
	opID   string
	seq    uint64
	want   statelog.Reason
}

// gateQuestions is the rule [statelog.Gates] states, as answers.
var gateQuestions = []gateQuestion{
	{name: "a record only the eviction drops",
		id: gateObj2, writer: gateEvicted, opID: "gates-evicted", seq: 5,
		want: statelog.ReasonEvicted},
	{name: "a record only the deletion drops",
		id: gateObj3, writer: gateCounted, opID: "gates-deleted", seq: 10,
		want: statelog.ReasonDeleted},
	// THE PRECEDENCE: the marker is a fact about the object, which no
	// readmission lifts, where the eviction is a fact about one writer.
	{name: "a record both gates drop",
		id: gateObj3, writer: gateEvicted, opID: "gates-both", seq: 8,
		want: statelog.ReasonDeleted},
	// THE MARKER HAS NO POSITION, as the applier's does not: it gates
	// every record on its object that did not apply, so a record the
	// eviction dropped is reported by the state its object is in NOW.
	{name: "an evicted writer's record on an object purged after it",
		id: gateObj1, writer: gateEvicted, opID: "gates-evicted-then-purged", seq: 6,
		want: statelog.ReasonDeleted},
	{name: "the purge's own record",
		id: gateObj1, writer: gateCounted, opID: "gates-purge-1", seq: 9},
	// THE FALL-THROUGH: the exemption says the MARKER did not drop the
	// record, which is no answer about its writer.
	{name: "the purge's own id under an evicted writer",
		id: gateObj3, writer: gateEvicted, opID: "gates-purge-3", seq: 7,
		want: statelog.ReasonEvicted},
	{name: "a second purge of a purged object",
		id: gateObj3, writer: gateCounted, opID: "gates-purge-3-again", seq: 12,
		want: statelog.ReasonDeleted},
	// THE WINDOW IS HALF-OPEN AT BOTH ENDS, as the applier's is.
	{name: "a record at the eviction's own position",
		id: gateObj2, writer: gateEvicted, opID: "gates-at-eviction", seq: 4},
	{name: "a record at the readmission's own position",
		id: gateObj2, writer: gateEvicted, opID: "gates-at-readmission", seq: 11},
	{name: "a readmitted writer's record",
		id: gateObj2, writer: gateEvicted, opID: "gates-back", seq: 12},
	{name: "a counted writer on an object nobody purged",
		id: gateObj2, writer: gateCounted, opID: "gates-counted", seq: 5},
	{name: "a kind the deletion gate does not cover",
		kind: ungatedKind, id: gateObj3, writer: gateCounted, opID: "gates-other-kind", seq: 10},
	// A RELEASE IS THE SAME WINDOW, reported as the node's own gate: the
	// write it had in flight when it left is dropped, and a caller is told
	// the node left rather than that the fleet put it out.
	{name: "a record only the writer's release drops",
		id: gateObj2, writer: gateReleased, opID: "gates-released", seq: 14,
		want: statelog.ReasonReleased},
	{name: "a released writer's record under a kind no marker covers",
		kind: ungatedKind, id: gateObj2, writer: gateReleased, opID: "gates-released-other", seq: 14,
		want: statelog.ReasonReleased},
	{name: "a record the release and the deletion both drop",
		id: gateObj3, writer: gateReleased, opID: "gates-released-purged", seq: 15,
		want: statelog.ReasonDeleted},
	{name: "a record at the release's own position",
		id: gateObj2, writer: gateReleased, opID: "gates-at-release", seq: 13},
	{name: "a record at the released writer's readmission",
		id: gateObj2, writer: gateReleased, opID: "gates-at-rejoin", seq: 16},
	{name: "a readmitted released writer's record",
		id: gateObj2, writer: gateReleased, opID: "gates-rejoined", seq: 17},
}

// GateAgreement applies the gate history through the candidate's own applier
// and asks its reader the rule's questions, reporting every answer that
// breaks it.
//
// EXPORTED AND RETURNING THE VERDICT, for the reason [Declaration] is: a case
// that cannot be shown to fail is a claim rather than a check. The
// *testing.T is for the infrastructure — a store that would not open, a
// record that would not encode — and those still stop the test.
func GateAgreement(t *testing.T, new GateFactory) error {
	t.Helper()
	_, err := gateAgreement(t, new)
	return err
}

// gateAgreement is [GateAgreement], also returning the subject kinds the
// applier's marker on a purged object covers — which the kind liars need,
// because a reader that asks about the object's own kind alone keeps the rule
// exactly wherever that is the only kind the marker covers.
func gateAgreement(t *testing.T, new GateFactory) ([]string, error) {
	t.Helper()
	c := new(t)
	if c.Reader == nil || c.Create == nil || c.Write == nil || c.Purge == nil ||
		c.Evict == nil || c.Readmit == nil || c.Release == nil || c.Kind == "" ||
		c.Encode == nil || !slices.Contains(c.SubjectKinds, c.Kind) {
		t.Fatal("the gate candidate leaves a hook, its Kind, its Encode or its " +
			"SubjectKinds (which must hold Kind) out, so the gate cases cannot " +
			"build the records they read — FATAL rather than a skip, for the " +
			"reason requireKinds gives")
	}
	db := openEstate(t, c.Candidate)
	stream := c.spec().Name
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: stream, Generation: 1, Seq: seq}
	}

	problems := applyGateHistory(t, c, db, at)

	gates := c.Reader(db.Reader())
	for _, q := range gateQuestions {
		kind := q.kind
		if kind == "" {
			kind = c.Kind
		}
		reason, gated, err := gates.GatedAt(t.Context(),
			statelog.Subject{Kind: kind, ID: q.id}, q.writer, q.opID, at(q.seq))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: GatedAt: %v", q.name, err))
			continue
		}
		if gated != (reason != "") {
			problems = append(problems, fmt.Sprintf("%s: GatedAt = (%q, %v) — "+
				"a gate with no reason, or a reason with no gate", q.name, reason, gated))
			continue
		}
		if reason != q.want {
			problems = append(problems, fmt.Sprintf("%s: GatedAt reports %q, "+
				"want %q", q.name, reason, q.want))
		}
	}
	covered, uncovered := gateCoverage(t, c, db, gates, at)
	problems = append(problems, uncovered...)
	if len(problems) == 0 {
		return covered, nil
	}
	return covered, errors.New("the gate reader breaks the rule statelog.Gates states:\n  " +
		strings.Join(problems, "\n  "))
}

// gateCoverage asks the APPLIER, for every subject kind the domain's records
// carry, whether the marker of the object purged at position 7 drops a record
// of that kind about it — and about an object nobody purged — and requires the
// reader to answer every one of those records exactly as the applier did. It
// returns the kinds the marker covers, and every disagreement, and refuses a
// probe that reached no marker at all: agreement there is agreement with any
// reader.
//
// AFTER the history, at positions above it, so the marker is in place and
// nothing a probe does is a record the questions read: a probe is asked
// about and never applied, since what is certified is the gate and an apply
// would only be the domain's own business.
func gateCoverage(t *testing.T, c GateCandidate, db store.PartitionHandle, gates statelog.Gates,
	at func(uint64) statelog.Position) ([]string, []string) {

	t.Helper()
	w, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()
	stored := time.Unix(1_700_000_000, 0).UTC()
	seq := uint64(len(gateHistory))
	var covered, problems []string
	for _, kind := range c.SubjectKinds {
		for _, id := range []string{gateObj3, gateObj2} {
			seq++
			opID := "gates-cover-" + kind + "-" + id
			body, err := c.Encode(kind, id, opID, c.Domain.RecordVersion())
			if err != nil {
				t.Fatalf("encode a %s record about %s: %v", kind, id, err)
			}
			env, err := c.Domain.Envelope(body)
			if err != nil {
				t.Fatalf("envelope a %s record about %s: %v", kind, id, err)
			}
			// THE MARKER ALONE: a writer no gate in the history holds, so
			// whatever either side answers is the deletion gate's.
			env.Writer = gateCounted
			rec := statelog.Record{
				Envelope: env, Position: at(seq), Payload: body, StoredAt: stored,
			}
			var dropped statelog.Reason
			var gated bool
			if err = w.Tx(t.Context(), func(tx *sql.Tx) error {
				var gErr error
				dropped, gated, gErr = c.Applier.Gated(t.Context(), tx, rec)
				return gErr
			}); err != nil {
				t.Fatalf("ask the applier about a %s record about %s: %v", kind, id, err)
			}
			label := fmt.Sprintf("a %s record on %s (%s)", kind, env.Subject.ID, rec.Position)
			if gated && dropped == statelog.ReasonDeleted && id == gateObj3 {
				covered = append(covered, kind)
			}
			reason, reported, err := gates.GatedAt(t.Context(), env.Subject,
				env.Writer, env.OpID, rec.Position)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("%s: GatedAt: %v", label, err))
			case reason != dropped || reported != gated:
				problems = append(problems, fmt.Sprintf("%s: the applier "+
					"answers (%q, %v) and the reader (%q, %v) — a reader that "+
					"misses a drop resolves the write as a record applied "+
					"without its ledger row, and one that reports a drop the "+
					"applier never made refuses a write every node applied",
					label, dropped, gated, reason, reported))
			}
		}
	}
	if !slices.Contains(covered, c.Kind) {
		problems = append(problems, fmt.Sprintf("the applier's marker on a "+
			"purged %s drops no record of that kind about it, so the probe "+
			"of every other kind proves nothing", c.Kind))
	}
	return covered, problems
}

// applyGateHistory runs the history through the candidate's own gate and
// state machine, as the framework's loop does, and reports every record the
// applier dropped or applied other than the history says.
//
// THE APPLIER IS WHAT THE READER HAS TO AGREE WITH, so the history is only
// the history the questions assume if the applier dropped exactly what it
// says it did.
func applyGateHistory(t *testing.T, c GateCandidate, db store.PartitionHandle,
	at func(uint64) statelog.Position) []string {

	t.Helper()
	w, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()
	opts := statelog.ApplyOptions{
		Now:             time.Unix(1_700_000_000, 0).UTC(),
		StoredAt:        time.Unix(1_700_000_000, 0).UTC(),
		ArbitratedKinds: c.spec().ArbitratedKinds,
		MaxVariables:    db.Caps().MaxVariables,
	}
	var problems []string
	for i, step := range gateHistory {
		body, err := step.body(c)
		if err != nil {
			t.Fatalf("encode %s: %v", step.label, err)
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			t.Fatalf("envelope %s: %v", step.label, err)
		}
		rec := statelog.Record{
			Envelope: env, Position: at(uint64(i + 1)),
			Payload: body, StoredAt: opts.StoredAt,
		}
		var reason statelog.Reason
		var gated bool
		if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
			var gErr error
			reason, gated, gErr = c.Applier.Gated(t.Context(), tx, rec)
			if gErr != nil || gated {
				return gErr
			}
			_, aErr := c.Applier.Apply(t.Context(), tx, rec, opts)
			return aErr
		}); err != nil {
			t.Fatalf("apply %s at %s: %v", step.label, rec.Position, err)
		}
		switch {
		case len(step.drops) == 0 && gated:
			problems = append(problems, fmt.Sprintf("the applier dropped %s "+
				"(%s) as %q — nothing gates it", step.label, rec.Position, reason))
		case len(step.drops) != 0 && !gated:
			problems = append(problems, fmt.Sprintf("the applier applied %s "+
				"(%s) — it is gated", step.label, rec.Position))
		case gated && !slices.Contains(step.drops, reason):
			problems = append(problems, fmt.Sprintf("the applier dropped %s "+
				"(%s) as %q, want one of %q", step.label, rec.Position,
				reason, step.drops))
		}
	}
	return problems
}

// gateLiars are the readers the gate cases must catch, each built from an
// honest one so every domain gets the same controls for free.
//
// Each asks the honest reader a question that isolates one gate: under a
// subject kind no deletion marker covers, it answers from the eviction
// window alone, and with no writer, from the marker alone.
var gateLiars = map[string]func(statelog.Gates) statelog.Gates{
	"answers the eviction before the deletion": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			if r, ok, err := evictionOnly(ctx, g, subj, writer, opID, p); err != nil || ok {
				return r, ok, err
			}
			return g.GatedAt(ctx, subj, writer, opID, p)
		}}
	},
	"returns early on the purge's own id": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			// With no writer the honest reader answers from the marker
			// alone, so an op id the marker exempts where another id on
			// the same object is deleted is the purge's own.
			_, markerGates, err := g.GatedAt(ctx, subj, "", opID, p)
			if err != nil {
				return "", false, err
			}
			other, _, err := g.GatedAt(ctx, subj, "", opID+"-not-the-purge", p)
			if err != nil {
				return "", false, err
			}
			if !markerGates && other == statelog.ReasonDeleted {
				return "", false, nil // the marker's own record: stop here
			}
			return g.GatedAt(ctx, subj, writer, opID, p)
		}}
	},
	"never reads the deletion gate": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			return evictionOnly(ctx, g, subj, writer, opID, p)
		}}
	},
	"never reads the eviction gate": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, _, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			return g.GatedAt(ctx, subj, "", opID, p)
		}}
	},
	"opens the window at the eviction's own position": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			r, ok, err := g.GatedAt(ctx, subj, writer, opID, p)
			if err != nil || ok {
				return r, ok, err
			}
			next := p
			next.Seq++
			return evictionOnly(ctx, g, subj, writer, opID, next)
		}}
	},
	"reports a release as an eviction": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			r, ok, err := g.GatedAt(ctx, subj, writer, opID, p)
			if r == statelog.ReasonReleased {
				r = statelog.ReasonEvicted
			}
			return r, ok, err
		}}
	},
	"reads no release": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			r, ok, err := g.GatedAt(ctx, subj, writer, opID, p)
			if r == statelog.ReasonReleased {
				return "", false, err
			}
			return r, ok, err
		}}
	},
	"keeps the window open at the readmission's position": func(g statelog.Gates) statelog.Gates {
		return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
			p statelog.Position) (statelog.Reason, bool, error) {
			r, ok, err := g.GatedAt(ctx, subj, writer, opID, p)
			if err != nil || ok || p.Seq == 0 {
				return r, ok, err
			}
			prev := p
			prev.Seq--
			return evictionOnly(ctx, g, subj, writer, opID, prev)
		}}
	},
}

// kindLiar bends a reader in which subject kinds it asks the marker about,
// given the object's own kind.
type kindLiar struct {
	bend func(own string, g statelog.Gates) statelog.Gates

	// needsAnotherKind is a liar indistinguishable from an honest reader
	// wherever the marker covers the object's own kind alone — the
	// knowledge base's — so it is caught only where there is another.
	needsAnotherKind bool
}

// kindLiars are the readers the coverage probe must catch.
var kindLiars = map[string]kindLiar{
	// THE TRACKER'S OWN BUG: a reader that read a task's marker for the
	// task's subject alone, while the applier dropped its turns too.
	"asks the marker about its object's own kind alone": {needsAnotherKind: true,
		bend: func(own string, g statelog.Gates) statelog.Gates {
			return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
				p statelog.Position) (statelog.Reason, bool, error) {
				if subj.Kind != own {
					return evictionOnly(ctx, g, subj, writer, opID, p)
				}
				return g.GatedAt(ctx, subj, writer, opID, p)
			}}
		}},
	"asks the marker about every kind": {
		bend: func(own string, g statelog.Gates) statelog.Gates {
			return liar{g, func(ctx context.Context, subj statelog.Subject, writer, opID string,
				p statelog.Position) (statelog.Reason, bool, error) {
				r, ok, err := g.GatedAt(ctx, subj, writer, opID, p)
				if err != nil || ok {
					return r, ok, err
				}
				return g.GatedAt(ctx, statelog.Subject{Kind: own, ID: subj.ID}, writer, opID, p)
			}}
		}},
}

// evictionOnly asks the honest reader about the writer's eviction alone.
func evictionOnly(ctx context.Context, g statelog.Gates, subj statelog.Subject,
	writer, opID string, p statelog.Position) (statelog.Reason, bool, error) {
	return g.GatedAt(ctx, statelog.Subject{Kind: ungatedKind, ID: subj.ID}, writer, opID, p)
}

// applierLiars are the appliers the gate cases must catch, each built from the
// candidate's honest one and given the object's own kind.
var applierLiars = map[string]func(own string, a statelog.Applier) statelog.Applier{
	// A MARKER WIDER THAN THE READER'S, on the applier's side: a record of
	// any kind about a purged object dropped as though it were the
	// object's own. The history never puts another kind on a purged
	// object's id, so only the coverage probe can see it — and a probe
	// that asked about the object's own kind alone would certify this
	// applier against a reader that covers nothing of it.
	"drops every kind's record about a purged object": func(own string, a statelog.Applier) statelog.Applier {
		return applierLiar{a, func(ctx context.Context, tx *sql.Tx,
			rec statelog.Record) (statelog.Reason, bool, error) {
			r, ok, err := a.Gated(ctx, tx, rec)
			if err != nil || ok {
				return r, ok, err
			}
			as := rec
			as.Kind, as.Subject.Kind = own, own
			return a.Gated(ctx, tx, as)
		}}
	},
	// THE RELEASE GATE DROPPED: a leaving node's in-flight write that lands
	// after its release applies as though the node had never left — on
	// every holder alike, so nothing diverges and nothing looks wrong, and
	// the write the node was told was refused is in every copy's rows.
	//
	// Every other gate is kept: where the release answered, the record is
	// asked again with no writer, which is the deletion marker alone — so
	// what the family sees is the release, and only the release, missing.
	"drops no record for its writer's release": func(_ string, a statelog.Applier) statelog.Applier {
		return applierLiar{a, func(ctx context.Context, tx *sql.Tx,
			rec statelog.Record) (statelog.Reason, bool, error) {
			r, ok, err := a.Gated(ctx, tx, rec)
			if err != nil || r != statelog.ReasonReleased {
				return r, ok, err
			}
			anonymous := rec
			anonymous.Writer = ""
			return a.Gated(ctx, tx, anonymous)
		}}
	},
}

// applierLiar is an applier whose Gated is replaced.
type applierLiar struct {
	statelog.Applier
	gated func(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error)
}

func (l applierLiar) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	return l.gated(ctx, tx, rec)
}

// liar is a reader whose GatedAt is replaced.
type liar struct {
	statelog.Gates
	gatedAt func(ctx context.Context, subj statelog.Subject, writer, opID string,
		p statelog.Position) (statelog.Reason, bool, error)
}

func (l liar) GatedAt(ctx context.Context, subj statelog.Subject, writer, opID string,
	p statelog.Position) (statelog.Reason, bool, error) {
	return l.gatedAt(ctx, subj, writer, opID, p)
}
