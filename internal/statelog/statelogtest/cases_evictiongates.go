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

// EvictionGateCandidate is a domain whose ONLY gate that makes a durable
// record apply nowhere is a node's EVICTION WINDOW — it keeps no deletion
// marker on any object — together with the publisher-side reader,
// [statelog.Gates], that has to report it.
//
// A FAMILY OF ITS OWN rather than a [GateCandidate] with its Purge left nil,
// for the reason that type is one: a hook a domain may leave out is a hook a
// domain that HAS a marker can forget with nothing failing. A domain is one
// shape or the other, and its test file names the family it is.
//
// A domain of this shape may still install OTHER apply gates — records that
// stop an applier rather than be deferred when a build cannot read them,
// because deferring one is a node serving what every other node removed — and
// those are what this candidate's ApplyGates name: every one of them must
// answer false to [statelog.Domain.NodeGate], for the reason [NodeGates]
// gives, and only its own records can show that.
type EvictionGateCandidate struct {
	Candidate

	// Reader is the publisher's side of the gate, over the estate the cases
	// applied into.
	Reader func(db store.ReplicatedReader) statelog.Gates

	// Kind is the subject kind Write publishes on: the one every question
	// about one object is asked under.
	Kind string

	// SubjectKinds is EVERY subject kind the domain's records carry, and it
	// holds Kind. The window is a fact about a WRITER, so a reader has to
	// report it on every kind the applier drops it on — and a reader that
	// asked about Kind alone would pass every question here that names
	// Kind, so the coverage probe asks the applier about all of them.
	SubjectKinds []string

	// The records the cases publish, each for the object (or node) id, the
	// writer and the operation id given. Create brings an object into
	// existence; Write is an ordinary change to one, published on Kind;
	// Evict and Readmit are the inverse pair on a node.
	Create  func(id, writer, opID string) ([]byte, error)
	Write   func(id, writer, opID string) ([]byte, error)
	Evict   func(node, writer, opID string) ([]byte, error)
	Readmit func(node, writer, opID string) ([]byte, error)

	// ApplyGates are the domain's other gate-installing records, by name,
	// each built for an object id. EMPTY IS AN ANSWER and nil is not: a
	// domain whose only gate is the eviction says so with an empty map,
	// and one that left this out has not said whether its other gates
	// are node gates.
	ApplyGates map[string]func(id, writer, opID string) ([]byte, error)
}

// EvictionGateFactory builds a fresh eviction-gate candidate for one case.
type EvictionGateFactory func(t *testing.T) EvictionGateCandidate

// RunEvictionGates certifies the gate reader of a domain whose only
// apply-nowhere gate is the eviction window, against the half of the rule
// [statelog.Gates] states that such a reader keeps — the window, half-open at
// both ends, reported as the eviction it is — on rows the domain's own applier
// wrote.
//
// IT ASKS THE APPLIER, for every subject kind the domain's records carry,
// whether an evicted writer's record of that kind is dropped inside the window
// and applied outside it, and holds the reader to the same answer — because
// the reader is only right where it agrees with what every applier did.
//
// It then bends the candidate's own reader and applier each way they could
// plausibly be written wrong, and requires every bend reported, on
// [RunGates]' terms: every bend is of the candidate itself, so the candidate
// is its own control. And it holds the domain's NODE-GATE answer to its
// eviction, its readmission and every other gate it installs.
//
// The reader is handed each probe's own body, as the publisher hands it its
// own append's, though nothing a window reads is in one: the window is a fact
// about the WRITER, which every envelope names. So the reader that ignores the
// body is a bend of the deletion family's alone.
func RunEvictionGates(t *testing.T, new EvictionGateFactory) {
	t.Helper()
	t.Run("the reader reports the window the rule names", func(t *testing.T) {
		if err := EvictionGateAgreement(t, new); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the cases catch a reader that breaks the rule", func(t *testing.T) {
		for name, bend := range windowLiars {
			t.Run(name, func(t *testing.T) {
				err := EvictionGateAgreement(t, func(t *testing.T) EvictionGateCandidate {
					c := new(t)
					honest := c.Reader
					c.Reader = func(db store.ReplicatedReader) statelog.Gates {
						return bend(honest(db))
					}
					return c
				})
				if err == nil {
					t.Errorf("a reader that %s passed every eviction-gate case "+
						"— the family cannot tell it from one that keeps the "+
						"rule", name)
				}
			})
		}
	})
	t.Run("the cases catch a reader that asks about its object's own kind alone", func(t *testing.T) {
		err := EvictionGateAgreement(t, func(t *testing.T) EvictionGateCandidate {
			c := new(t)
			own, honest := c.Kind, c.Reader
			c.Reader = func(db store.ReplicatedReader) statelog.Gates {
				g := honest(db)
				return liar{g, func(ctx context.Context, subj statelog.Subject, writer,
					opID string, p statelog.Position, body []byte) (statelog.Reason, bool, error) {
					if subj.Kind != own {
						return "", false, nil
					}
					return g.GatedAt(ctx, subj, writer, opID, p, body)
				}}
			}
			return c
		})
		if err == nil {
			t.Error("a reader that reports the window on its object's own kind " +
				"alone passed every eviction-gate case — the coverage probe " +
				"reached no other kind the applier drops")
		}
	})
	t.Run("the cases catch an applier that breaks the rule", func(t *testing.T) {
		for name, bend := range windowApplierLiars {
			t.Run(name, func(t *testing.T) {
				err := EvictionGateAgreement(t, func(t *testing.T) EvictionGateCandidate {
					c := new(t)
					c.Applier = bend(c.Kind, c.Applier)
					return c
				})
				if err == nil {
					t.Errorf("an applier that %s passed every eviction-gate "+
						"case — the family cannot tell it from one that keeps "+
						"the rule", name)
				}
			})
		}
	})
	t.Run("only a node's eviction and readmission are node gates", func(t *testing.T) {
		if err := EvictionGateNodeGates(new(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the cases catch a domain that calls the wrong record a node gate", func(t *testing.T) {
		// THE APPLY-GATE BEND IS SEEN ONLY THROUGH ANOTHER GATE: where the
		// eviction is the domain's only one, its apply gate and its node
		// gate are the same answer. So which bends apply is decided BEFORE
		// any subtest exists — a subtest that returned without asserting
		// would report a pass for a case nothing ran, which is a skip no
		// gate can see.
		others := len(new(t).ApplyGates) > 0
		for name, bend := range nodeGateLiars {
			if name == applyGateNodeGateLiar && !others {
				continue
			}
			t.Run(name, func(t *testing.T) {
				c := new(t)
				c.Domain = bend(c.Domain)
				if EvictionGateNodeGates(c) == nil {
					t.Errorf("a domain that %s passed the node-gate case — the "+
						"family cannot tell it from one that keeps the rule", name)
				}
			})
		}
	})
	// NIL IS NOT AN ANSWER. A candidate that left its other apply gates out
	// has not said whether they are node gates, and reading the silence as
	// "none" would certify a domain whose erase is flagged one.
	t.Run("the cases catch a candidate that leaves its other gates unsaid", func(t *testing.T) {
		c := new(t)
		c.ApplyGates = nil
		if EvictionGateNodeGates(c) == nil {
			t.Error("a candidate that names none of its other apply gates, not " +
				"even with an empty map, passed the node-gate case — the family " +
				"cannot tell it from one whose eviction is its only gate")
		}
	})
}

// The ids the eviction-gate cases use: obj-1 is written inside the window by
// both writers, obj-2 by the evicted one only after it is readmitted.
const (
	windowObj1 = "window-obj-1"
	windowObj2 = "window-obj-2"
)

// windowHistory is the one sequence every eviction-gate case reads, at
// positions 1..7. The window opens at 3 and closes at 6, with a record of each
// writer strictly inside it and records on both of its edges' far sides.
var windowHistory = []struct {
	label string
	body  func(c EvictionGateCandidate) ([]byte, error)
	drops bool
}{
	{label: "create obj-1", body: func(c EvictionGateCandidate) ([]byte, error) { // 1
		return c.Create(windowObj1, gateCounted, "window-create-1")
	}},
	{label: "create obj-2", body: func(c EvictionGateCandidate) ([]byte, error) { // 2
		return c.Create(windowObj2, gateCounted, "window-create-2")
	}},
	{label: "evict node-b", body: func(c EvictionGateCandidate) ([]byte, error) { // 3
		return c.Evict(gateEvicted, gateCounted, "window-evict")
	}},
	{label: "node-b writes obj-1", drops: true, // 4
		body: func(c EvictionGateCandidate) ([]byte, error) {
			return c.Write(windowObj1, gateEvicted, "window-evicted")
		}},
	{label: "node-a writes obj-1", // 5
		body: func(c EvictionGateCandidate) ([]byte, error) {
			return c.Write(windowObj1, gateCounted, "window-counted")
		}},
	{label: "readmit node-b", body: func(c EvictionGateCandidate) ([]byte, error) { // 6
		return c.Readmit(gateEvicted, gateCounted, "window-readmit")
	}},
	{label: "readmitted node-b writes obj-2", // 7
		body: func(c EvictionGateCandidate) ([]byte, error) {
			return c.Write(windowObj2, gateEvicted, "window-back")
		}},
}

// windowQuestions is the window half of the rule [statelog.Gates] states, as
// answers. A reader with no deletion marker never answers `deleted`.
var windowQuestions = []gateQuestion{
	{name: "a record the eviction drops",
		id: windowObj1, writer: gateEvicted, opID: "window-evicted", seq: 4,
		want: statelog.ReasonEvicted},
	// THE WINDOW IS A FACT ABOUT THE WRITER, whatever the record is about.
	{name: "another kind's record by the evicted writer inside the window",
		kind: ungatedKind, id: windowObj1, writer: gateEvicted,
		opID: "window-other-kind", seq: 5, want: statelog.ReasonEvicted},
	// THE WINDOW IS HALF-OPEN AT BOTH ENDS, as the applier's is.
	{name: "a record at the eviction's own position",
		id: windowObj1, writer: gateEvicted, opID: "window-at-eviction", seq: 3},
	{name: "a record at the readmission's own position",
		id: windowObj2, writer: gateEvicted, opID: "window-at-readmission", seq: 6},
	{name: "a readmitted writer's record",
		id: windowObj2, writer: gateEvicted, opID: "window-back", seq: 7},
	{name: "a counted writer inside the window",
		id: windowObj1, writer: gateCounted, opID: "window-counted", seq: 5},
	{name: "a record that names no writer",
		id: windowObj1, opID: "window-no-writer", seq: 4},
}

// EvictionGateAgreement applies the window history through the candidate's
// own applier and asks its reader the rule's questions, reporting every answer
// that breaks it.
//
// EXPORTED AND RETURNING THE VERDICT, for the reason [GateAgreement] is.
func EvictionGateAgreement(t *testing.T, new EvictionGateFactory) error {
	t.Helper()
	c := new(t)
	if c.Reader == nil || c.Create == nil || c.Write == nil || c.Evict == nil ||
		c.Readmit == nil || c.Kind == "" || c.Encode == nil ||
		!slices.Contains(c.SubjectKinds, c.Kind) {
		t.Fatal("the eviction-gate candidate leaves a hook, its Kind, its " +
			"Encode or its SubjectKinds (which must hold Kind) out, so the " +
			"cases cannot build the records they read — FATAL rather than a " +
			"skip, for the reason requireKinds gives")
	}
	db := openEstate(t, c.Candidate)
	stream := c.spec().Name
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: stream, Generation: 1, Seq: seq}
	}

	problems := applyWindowHistory(t, c, db, at)
	gates := c.Reader(db.Reader())
	for _, q := range windowQuestions {
		kind := q.kind
		if kind == "" {
			kind = c.Kind
		}
		reason, gated, err := gates.GatedAt(t.Context(),
			statelog.Subject{Kind: kind, ID: q.id}, q.writer, q.opID, at(q.seq), nil)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: GatedAt: %v", q.name, err))
		case gated != (reason != ""):
			problems = append(problems, fmt.Sprintf("%s: GatedAt = (%q, %v) — "+
				"a gate with no reason, or a reason with no gate", q.name, reason, gated))
		case reason != q.want:
			problems = append(problems, fmt.Sprintf("%s: GatedAt reports %q, "+
				"want %q", q.name, reason, q.want))
		}
	}
	problems = append(problems, windowCoverage(t, c, db, gates, at)...)
	if len(problems) == 0 {
		return nil
	}
	return errors.New("the gate reader breaks the window statelog.Gates states:\n  " +
		strings.Join(problems, "\n  "))
}

// windowCoverage asks the APPLIER, for every subject kind the domain's records
// carry, whether an evicted writer's record of that kind is dropped inside the
// window and applied after the readmission — and requires the reader to answer
// every one exactly as the applier did. It refuses a probe in which the
// applier dropped nothing of the candidate's own kind, because agreement there
// is agreement with any reader.
//
// ASKED AND NEVER APPLIED, at positions the history already holds: what is
// certified is the gate, read from the eviction row the history wrote, and an
// apply would only be the domain's own business.
func windowCoverage(t *testing.T, c EvictionGateCandidate, db store.ReplicatedHandle,
	gates statelog.Gates, at func(uint64) statelog.Position) []string {

	t.Helper()
	w, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()
	stored := time.Unix(1_700_000_000, 0).UTC()
	var problems []string
	ownDropped := false
	for _, kind := range c.SubjectKinds {
		for _, probe := range []struct {
			seq    uint64
			inside bool
		}{{5, true}, {7, false}} {
			opID := fmt.Sprintf("window-cover-%s-%d", kind, probe.seq)
			body, err := c.Encode(kind, windowObj1, opID, c.Domain.RecordVersion())
			if err != nil {
				t.Fatalf("encode a %s record: %v", kind, err)
			}
			env, err := c.Domain.Envelope(body)
			if err != nil {
				t.Fatalf("envelope a %s record: %v", kind, err)
			}
			env.Writer = gateEvicted
			rec := statelog.Record{
				Envelope: env, Position: at(probe.seq), Payload: body, StoredAt: stored,
			}
			var dropped statelog.Reason
			var gated bool
			if err = w.Tx(t.Context(), func(tx *sql.Tx) error {
				var gErr error
				dropped, gated, gErr = c.Applier.Gated(t.Context(), tx, rec)
				return gErr
			}); err != nil {
				t.Fatalf("ask the applier about a %s record at %s: %v", kind,
					rec.Position, err)
			}
			label := fmt.Sprintf("an evicted writer's %s record at %s", kind, rec.Position)
			if probe.inside && gated && kind == c.Kind {
				ownDropped = true
			}
			if !probe.inside && gated {
				problems = append(problems, fmt.Sprintf("%s: the applier drops "+
					"it as %q after the readmission — the window closed at 6",
					label, dropped))
			}
			reason, reported, err := gates.GatedAt(t.Context(), env.Subject,
				env.Writer, env.OpID, rec.Position, body)
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
	if !ownDropped {
		problems = append(problems, fmt.Sprintf("the applier drops no %s record "+
			"an evicted writer published inside its window, so the probe of "+
			"every other kind proves nothing", c.Kind))
	}
	return problems
}

// applyWindowHistory runs the history through the candidate's own gate and
// state machine, as the framework's loop does, and reports every record the
// applier dropped or applied other than the history says.
func applyWindowHistory(t *testing.T, c EvictionGateCandidate, db store.ReplicatedHandle,
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
	for i, step := range windowHistory {
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
		case !step.drops && gated:
			problems = append(problems, fmt.Sprintf("the applier dropped %s "+
				"(%s) as %q — nothing gates it", step.label, rec.Position, reason))
		case step.drops && !gated:
			problems = append(problems, fmt.Sprintf("the applier applied %s "+
				"(%s) — its writer is evicted", step.label, rec.Position))
		case gated && reason != statelog.ReasonEvicted:
			problems = append(problems, fmt.Sprintf("the applier dropped %s "+
				"(%s) as %q, and the only gate this domain has is the eviction",
				step.label, rec.Position, reason))
		}
	}
	return problems
}

// windowApplierLiars are the appliers the eviction-gate cases must catch, each
// built from the candidate's honest one and given the object's own kind.
var windowApplierLiars = map[string]func(own string, a statelog.Applier) statelog.Applier{
	"drops nothing an evicted writer publishes": func(_ string, a statelog.Applier) statelog.Applier {
		return applierLiar{a, func(context.Context, *sql.Tx, statelog.Record) (
			statelog.Reason, bool, error) {
			return "", false, nil
		}}
	},
	// THE WINDOW NARROWED TO ONE KIND, on the applier's side: the history
	// writes only the object's own kind, so only the coverage probe of
	// every other kind can see it.
	"drops an evicted writer's records of its object's own kind alone": func(own string, a statelog.Applier) statelog.Applier {
		return applierLiar{a, func(ctx context.Context, tx *sql.Tx,
			rec statelog.Record) (statelog.Reason, bool, error) {
			if rec.Kind != own {
				return "", false, nil
			}
			return a.Gated(ctx, tx, rec)
		}}
	},
}

// EvictionGateNodeGates certifies, over an eviction-gated domain's own
// records, that exactly its eviction and readmission are node gates — and that
// its create, its ordinary write and every other gate it installs are not.
//
// EXPORTED AND RETURNING THE VERDICT, so [RunEvictionGates] can bend the
// candidate's own domain and require the bend to be reported.
func EvictionGateNodeGates(c EvictionGateCandidate) error {
	var errs []error
	if c.ApplyGates == nil {
		errs = append(errs, fmt.Errorf("%s states none of its other apply gates — "+
			"an empty map says the eviction is the only one, and a nil one says "+
			"nothing about whether the others are node gates", c.Domain.Name()))
	}
	records := []judgedRecord{
		{label: "its create", body: func() ([]byte, error) {
			return c.Create("node-gates-obj", gateCounted, "node-gates-create")
		}},
		{label: "its ordinary write", body: func() ([]byte, error) {
			return c.Write("node-gates-obj", gateCounted, "node-gates-write")
		}},
		{label: "its eviction of a node", nodeGate: true, installs: true,
			body: func() ([]byte, error) {
				return c.Evict(gateEvicted, gateCounted, "node-gates-evict")
			}},
		{label: "its readmission of a node", nodeGate: true, installs: true,
			body: func() ([]byte, error) {
				return c.Readmit(gateEvicted, gateCounted, "node-gates-readmit")
			}},
	}
	for label, build := range c.ApplyGates {
		records = append(records, judgedRecord{label: "its " + label, installs: true,
			body: func() ([]byte, error) {
				return build("node-gates-obj", gateCounted, "node-gates-"+label)
			}})
	}
	return errors.Join(append(errs, judgeNodeGates(c.Domain, records)...)...)
}
