package statelogtest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CONTROL, expected to come back clean.
//
// A suite that reports a problem in everything it touches is a suite nobody
// reads, and this is what says the cases can pass at all. It is also the only
// thing exercising them until the first real domain arrives — so a case that
// cannot pass here is a case that was written against a domain nobody has
// built yet.
func TestTheControlDomainPasses(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return control()
	})
}

// A COMPACTED CONTROL, because the two protocols are the two shapes the suite
// has to be able to certify — and a case that assumes arbitration, an
// operation ledger or contiguity passes against one and fails against the
// other for the wrong reason.
func TestTheCompactedControlPasses(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return compacted()
	})
}

// compacted is the control under the compacted protocol: no arbitration, so no
// anchor and no trimmed record — and, like the engine's compacted logs, no
// read index, so no barrier.
func compacted() statelogtest.Candidate {
	c := control()
	c.Domain = compactedControl{}
	c.Rows = rowsOver(c.Domain)
	c.EncodeTrimmed = nil
	c.EncodeBarrier, c.NoBarrier = nil, true
	return c
}

// rowsOver is the framework's own read seam over one control domain, which is
// what a real domain's constructor returns too.
func rowsOver(d statelog.Domain) func(store.ReplicatedReader, statelog.StreamSpec) (statelog.Rows, error) {
	return func(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
		return statelog.NewRows(db, d, spec, nil)
	}
}

// controlWrite is the control domain's write path: one widget, through the
// publisher, stamped with what stampOf makes of the stamp it is handed — the
// identity for a domain that does its job, anything else for one that lies.
func controlWrite(stampOf func(statelog.Stamp) statelog.Stamp) func(context.Context,
	*statelog.Publisher, store.ReplicatedReader) error {

	return func(ctx context.Context, pub *statelog.Publisher, _ store.ReplicatedReader) error {
		const opID = "control-write"
		subject := statelog.Subject{Kind: "widget", ID: "w-1"}
		scope := statelog.ScopeSet{Paths: []string{"widget/w-1"}}
		_, err := pub.Publish(ctx, statelog.Request{
			Subject: subject, Scope: scope, OpID: opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				stamp = stampOf(stamp)
				payload, err := json.Marshal(statelog.Envelope{
					V: 1, Kind: "widget", Subject: subject, OpID: opID,
					Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
				})
				return statelog.Decision{Payload: payload}, err
			},
		})
		return err
	}
}

func control() statelogtest.Candidate {
	return statelogtest.Candidate{
		Domain:        controlDomain{},
		Applier:       controlApplier{},
		Kinds:         []string{"widget"},
		Rows:          rowsOver(controlDomain{}),
		Write:         controlWrite(func(s statelog.Stamp) statelog.Stamp { return s }),
		EncodeGate:    encodeControlGate,
		EncodeTrimmed: encodeControlTrimmed,
		EncodeBarrier: encodeControlBarrier,
		Migrate: func(ctx context.Context, db store.ReplicatedHandle) error {
			return db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, controlDDL)
				return err
			})
		},
		Encode: func(kind, id, opID string, version int) ([]byte, error) {
			return encodeControl(controlRecord{Envelope: controlEnvelope(kind, id, opID, version)},
				stampByContent)
		},
		Fields: controlFields,
		Carrying: func(field statelog.VersionedField) ([]byte, error) {
			if field.Name != "widget.Colour" {
				return nil, fmt.Errorf("the control has no field %s", field.Name)
			}
			return encodeControl(controlRecord{
				Envelope: controlEnvelope("widget", "carrying", "suite-carrying", 0),
				Colour:   "violet",
			}, stampByContent)
		},
	}
}

// controlFields is the control's versioned-field table: ONE field, added at
// version 2, so the version-stamping cases ([statelogtest.Stamping]) have
// something to hold it to.
var controlFields = statelog.RecordFields{
	{Name: "widget.Colour", Since: 2, Path: []string{"colour"}},
}

// controlRecord is the control's wire shape: the framework's envelope plus
// the one field a later build added.
type controlRecord struct {
	statelog.Envelope
	Colour string `json:"colour,omitempty"`
}

func controlEnvelope(kind, id, opID string, version int) statelog.Envelope {
	return statelog.Envelope{
		V:       version,
		Kind:    kind,
		Subject: statelog.Subject{Kind: kind, ID: id},
		OpID:    opID,
		Gen:     1,
		Scope:   statelog.ScopeSet{Paths: []string{kind + "/" + id}},
		Writer:  "control",
	}
}

// stamper decides the version a record whose caller left it unset goes out
// at — the one thing the meta-tests below vary.
type stamper func(rec controlRecord) (int, error)

// stampByContent is the rule every domain follows: the lowest version that
// reads what the record carries.
func stampByContent(rec controlRecord) (int, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	return controlFields.Minimum(rec.Kind, body)
}

func encodeControl(rec controlRecord, stamp stamper) ([]byte, error) {
	if rec.V == 0 {
		v, err := stamp(rec)
		if err != nil {
			return nil, err
		}
		rec.V = v
	}
	return json.Marshal(rec)
}

const controlDDL = `
CREATE TABLE control_widgets (
    id      TEXT    NOT NULL PRIMARY KEY,
    version INTEGER NOT NULL
);
CREATE TABLE control_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX control_ops_swept_idx ON control_ops (applied_at);
CREATE TABLE control_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX control_log_deferred_subject_idx
    ON control_log_deferred (subject_id, subject_kind);
CREATE TABLE control_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX control_deferred_scope_path_idx ON control_deferred_scope (path);
CREATE TABLE control_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER
);
`

// controlGateKind is the control domain's eviction record, and controlGate its
// shape: the envelope every build reads, plus the node and the direction.
const controlGateKind = "eviction"

type controlGate struct {
	statelog.Envelope
	Node    string
	Readmit bool
}

// encodeControlGate is the control domain's own eviction record.
func encodeControlGate(node string, readmit bool) ([]byte, error) {
	op := "gate-evict-" + node
	if readmit {
		op = "gate-readmit-" + node
	}
	return json.Marshal(controlGate{
		Envelope: statelog.Envelope{
			V: 1, Kind: controlGateKind,
			Subject: statelog.Subject{Kind: controlGateKind, ID: node},
			OpID:    op, Gen: 1, Writer: "control",
			Scope: statelog.ScopeSet{Paths: []string{controlGateKind}},
		},
		Node: node, Readmit: readmit,
	})
}

// encodeControlTrimmed is the control's encoder of the trim's trimmed record:
// its wire shape is the framework's envelope, so the record is the envelope.
func encodeControlTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("control: a %q envelope is not a trimmed record", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("control: a trimmed record carries no op id")
	}
	return json.Marshal(env)
}

// encodeControlBarrier is the control's encoder of its read index's barrier:
// the framework's envelope, as the trimmed record is.
func encodeControlBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("control: a %q envelope is not a barrier", env.Kind)
	}
	return json.Marshal(env)
}

type controlBase struct{}

func (controlBase) Name() string { return "control" }

// controlStream is the control's own stream — named here rather than taken
// from a production domain's topics constants, because a fake that took one
// would certify the framework under a name a real domain owns.
const (
	controlStream = "CREWLET_CONTROL_LOG"
	controlPrefix = "crewlet.control.log"
)

func (controlBase) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            controlStream,
		Subjects:        []string{controlPrefix + ".>"},
		SubjectPrefix:   controlPrefix,
		ArbitratedKinds: []string{"widget"},
		MaxBytes:        16 << 20,
		MaxRecordBytes:  64 << 10,
		Duplicates:      2 * time.Minute,
		Replay:          statelog.ReplayStrict,
	}
}

// RecordVersion is 2 because the control's field table reaches 2 — the one
// field a later build added ([controlFields]) — and a build reads exactly the
// highest version its table introduces.
func (controlBase) RecordVersion() int { return 2 }

func (controlBase) Envelope(payload []byte) (statelog.Envelope, error) {
	var env statelog.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return statelog.Envelope{}, err
	}
	return env, nil
}

func (controlBase) InstallsGate(statelog.Envelope) bool { return false }
func (controlBase) NodeGate(statelog.Envelope) bool     { return false }

func (controlBase) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		"control_widgets":        statelog.Replicated,
		"control_ops":            statelog.Divergent,
		"control_log_deferred":   statelog.Local,
		"control_deferred_scope": statelog.Local,
	}
}

func (controlBase) DeferredTable() string    { return "control_log_deferred" }
func (controlBase) ScopeIndex() string       { return "control_deferred_scope" }
func (controlBase) OpsTable() string         { return "control_ops" }
func (controlBase) GatesSeatAdmission() bool { return true }
func (controlBase) ClaimsIdentity() bool     { return true }
func (controlBase) FeedGroup() string        { return "" }

// controlDomain is the identity-claiming control: the base, plus the eviction
// record and table every domain the trim counts nodes on has to have.
type controlDomain struct{ controlBase }

func (controlDomain) InstallsGate(env statelog.Envelope) bool {
	return env.Kind == controlGateKind
}

func (controlDomain) NodeGate(env statelog.Envelope) bool {
	return env.Kind == controlGateKind
}

func (controlDomain) Tables() map[string]statelog.TableClass {
	tables := controlBase{}.Tables()
	tables["control_evictions"] = statelog.Replicated
	return tables
}

// EvictionSubject and Evicts read the control's node gates off its
// log, as every identity-claiming domain must be able to.
func (controlDomain) EvictionSubject(node string) statelog.Subject {
	return statelog.Subject{Kind: controlGateKind, ID: node}
}

func (controlDomain) Evicts(payload []byte) (bool, error) {
	var gate controlGate
	if err := json.Unmarshal(payload, &gate); err != nil {
		return false, err
	}
	return !gate.Readmit, nil
}

// Evictions answers from this log's own rows, as a real domain does.
func (controlDomain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM control_evictions ORDER BY node_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				e          statelog.EvictionRow
				at, from   int64
				readmitted sql.NullInt64
			)
			if err := rows.Scan(&e.NodeID, &at, &e.By, &from, &readmitted); err != nil {
				return err
			}
			e.At, e.From = store.DecodeTime(at), uint64(from)
			e.Readmitted = uint64(readmitted.Int64)
			e.Back = readmitted.Valid && readmitted.Int64 > from
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// compactedControl is the same domain under the other replay protocol: no
// arbitration, no operation ledger, and a gap that is a coverage number rather
// than a fault.
type compactedControl struct{ controlBase }

func (c compactedControl) Stream() statelog.StreamSpec {
	s := c.controlBase.Stream()
	s.Replay = statelog.ReplayCompacted
	s.MaxPerSubject = 1
	s.MaxAge = time.Hour
	s.ArbitratedKinds = nil
	return s
}

func (compactedControl) OpsTable() string         { return "" }
func (compactedControl) GatesSeatAdmission() bool { return false }
func (compactedControl) ClaimsIdentity() bool     { return false }

func (compactedControl) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		"control_widgets":        statelog.Divergent,
		"control_log_deferred":   statelog.Local,
		"control_deferred_scope": statelog.Local,
	}
}

// controlApplier writes one row per record, guarded by the version so a
// redelivery is a no-op.
type controlApplier struct{}

func (controlApplier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, _ statelog.ApplyOptions) (int, error) {
	switch rec.Kind {
	case controlGateKind:
		return applyControlGate(ctx, tx, rec, false)
	case statelog.TrimmedKind:
		// THE FRAMEWORK REMOVES THE ANCHORS; the control writes nothing.
		return 0, nil
	case statelog.BarrierKind:
		// A BARRIER WRITES NO ROW: its reads are served before it commits.
		return 0, nil
	}
	return applyWidget(ctx, tx, rec)
}

// applyWidget is the control's one row per record.
func applyWidget(ctx context.Context, tx *sql.Tx, rec statelog.Record) (int, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO control_widgets (id, version) VALUES (?, ?)
		ON CONFLICT (id) DO UPDATE SET version = excluded.version
		WHERE excluded.version > control_widgets.version`,
		rec.Subject.ID, rec.Position.Packed())
	if err != nil {
		return 0, fmt.Errorf("control: apply %s: %w", rec.Subject, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (controlApplier) Gated(context.Context, *sql.Tx, statelog.Record) (statelog.Reason, bool, error) {
	return "", false, nil
}

func (controlApplier) Committed(context.Context) {}

// applyControlGate records an eviction or its readmission, as an inverse
// commit — or, when deleting, the way a domain that got it wrong would.
func applyControlGate(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	deleting bool) (int, error) {

	var gate controlGate
	if err := json.Unmarshal(rec.Payload, &gate); err != nil {
		return 0, fmt.Errorf("control: decode the gate at %s: %w", rec.Position, err)
	}
	at := rec.Position.Packed()
	var res sql.Result
	var err error
	switch {
	case gate.Readmit && deleting:
		res, err = tx.ExecContext(ctx,
			`DELETE FROM control_evictions WHERE node_id = ?`, gate.Node)
	case gate.Readmit:
		res, err = tx.ExecContext(ctx, `
			UPDATE control_evictions SET readmitted_position = ?
			WHERE node_id = ? AND from_position < ?`, at, gate.Node, at)
	default:
		res, err = tx.ExecContext(ctx, `
			INSERT INTO control_evictions
				(node_id, at, by, from_position, readmitted_position)
			VALUES (?, ?, ?, ?, NULL)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position, readmitted_position = NULL
			WHERE excluded.from_position > control_evictions.from_position`,
			gate.Node, store.EncodeTime(rec.StoredAt), "control", at)
	}
	if err != nil {
		return 0, fmt.Errorf("control: apply the gate at %s: %w", rec.Position, err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// A DOMAIN THAT LIES ABOUT ITSELF IS CAUGHT, which is what says the cases can
// fail as well as pass.
//
// Every arm here is a real defect that ships silently: an unclassed table is a
// snapshot that carries what it should scrub, a mismatched protocol is a loop
// that stalls or a hole it steps over, and an arbitrated kind with no ledger is
// a write that cannot answer for itself.
func TestTheSuiteCatchesADomainThatMisdeclaresItself(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		domain statelog.Domain
		names  string
	}{
		"a stream whose protocol its own settings refuse": {
			domain: brokenStream{controlDomain{}}, names: "per subject",
		},
		"an arbitrated kind with no operation ledger": {
			domain: ledgerless{controlDomain{}}, names: "operation ledger",
		},
		"an identity claim over no replicated table": {
			domain: nothingReplicated{controlDomain{}}, names: "empty set",
		},
		"an operation ledger scrubbed out of every snapshot": {
			domain: scrubbedLedger{controlDomain{}}, names: "must be divergent",
		},
		"a compacted domain that gates seat admission": {
			domain: gatingCompacted{compactedControl{}},
			names:  "gates seat admission and replays compacted",
		},
		"a domain with no operation ledger that gates seat admission": {
			domain: ledgerlessGate{controlDomain{}},
			names:  "keeps no operation ledger and gates seat admission",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := control()
			c.Domain = tc.domain
			errs := statelogtest.Declaration(c)
			if len(errs) == 0 {
				t.Fatalf("the suite passed a domain that declares %s", name)
			}
			var found bool
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.names) {
					found = true
				}
			}
			if !found {
				t.Errorf("the suite objected, but to something else: %v", errs)
			}
		})
	}

	// AND A CANDIDATE THAT PUBLISHES NOTHING, which is the one shape the
	// suite cannot certify at all.
	//
	// Kinds is what these cases publish records OF, so a candidate declaring
	// none leaves the apply path — the rolling-upgrade envelope contract, the
	// op-id round trip, the gated reprocess — with nothing to exercise. It was
	// three t.Skips, which reported a pass over a domain nothing had touched;
	// they are fatal now, and this is what pins that. Without it the guard
	// could be reverted to a skip and every case here would still be green,
	// because every other candidate in this file declares a kind.
	t.Run("a candidate that declares no kinds", func(t *testing.T) {
		t.Parallel()
		c := control()
		c.Kinds = nil
		errs := statelogtest.Declaration(c)
		if len(errs) == 0 {
			t.Fatal("the suite passed a candidate that publishes no kind, so its " +
				"arbitrated kind is an anchor nothing ever writes")
		}
		var found bool
		for _, err := range errs {
			if strings.Contains(err.Error(), "publishes no record of that kind") {
				found = true
			}
		}
		if !found {
			t.Errorf("the suite objected, but to something else: %v", errs)
		}
	})

	// AND A WRITE PATH THAT DROPS THE STAMP IT IS HANDED — the defect every
	// domain in this tree shipped with. Each arm is a way to get it wrong:
	// the stamp ignored outright, a writer that is some other node's, a
	// generation that is not the snapshot's. Its verdict comes back from a
	// write, so it needs a store.
	for name, lie := range map[string]func(statelog.Stamp) statelog.Stamp{
		"a write path that stamps nothing": func(statelog.Stamp) statelog.Stamp {
			return statelog.Stamp{}
		},
		"a write path that names another node": func(s statelog.Stamp) statelog.Stamp {
			s.Writer = "somebody-else"
			return s
		},
		"a write path that stamps another generation": func(s statelog.Stamp) statelog.Stamp {
			s.Gen = 1
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := statelogtest.Stamped(t, func(*testing.T) statelogtest.Candidate {
				c := control()
				c.Write = controlWrite(lie)
				return c
			})
			if err == nil {
				t.Fatalf("the suite passed %s — every applier's eviction gate "+
					"reads that stamp", name)
			}
		})
	}
	// AND ONE THAT SUPPLIES NO WRITE PATH AT ALL, which certifies nothing.
	t.Run("a candidate with no write path", func(t *testing.T) {
		t.Parallel()
		err := statelogtest.Stamped(t, func(*testing.T) statelogtest.Candidate {
			c := control()
			c.Write = nil
			return c
		})
		if err == nil {
			t.Fatal("the suite passed a candidate whose write path it never ran")
		}
	})

	// AND A DOMAIN THAT CANNOT SAY WHO IS EVICTED ON ITS OWN LOG, which is
	// the defect the pages log shipped with: an applier, a fence and a table
	// for an eviction, and no answer the trim could read — so an evicted
	// node was counted on that log for ever. Each arm is a way to get the
	// answer wrong; every verdict comes back from applied records.
	for name, mutate := range map[string]func(*statelogtest.Candidate){
		"an identity-claiming domain that lists no evictions": func(c *statelogtest.Candidate) {
			c.Domain = unlisted{}
		},
		"a domain whose list reads some other log's rows": func(c *statelogtest.Candidate) {
			c.Domain = blindLister{}
		},
		"an applier that deletes a readmitted node's row": func(c *statelogtest.Candidate) {
			c.Applier = deletingApplier{}
		},
		"a domain that claims no identity and lists evictions": func(c *statelogtest.Candidate) {
			c.Domain = listingCompacted{}
		},
		"an identity-claiming candidate with no eviction record": func(c *statelogtest.Candidate) {
			c.EncodeGate = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := statelogtest.Evictions(t, func(*testing.T) statelogtest.Candidate {
				c := control()
				mutate(&c)
				return c
			})
			if err == nil {
				t.Fatalf("the suite passed %s — the trim reads exactly this to stop "+
					"counting an evicted node", name)
			}
		})
	}

	// AND AN APPLIER THAT IS NOT IDEMPOTENT AT A POSITION. This one needs
	// a store, so its verdict comes back from the apply case rather than
	// from the declaration.
	t.Run("an applier that is not idempotent at a position", func(t *testing.T) {
		t.Parallel()
		err := statelogtest.Idempotency(t, func(*testing.T) statelogtest.Candidate {
			c := control()
			c.Applier = countingApplier{}
			return c
		})
		if err == nil {
			t.Fatal("the suite passed an applier that counts its deliveries into a row")
		}
	})
}

// A KIND A ROW INTRODUCES IS STAMPED AT THAT ROW'S VERSION, and the suite holds
// a domain to it in both directions: its ordinary record of that kind carries
// the kind's own row and nothing else, so the version it is owed is the row's
// rather than one — demanding one would demand that builds with no applier for
// the kind apply it — and a record of it stamped one is the fault the row
// exists to prevent.
func TestTheSuiteHoldsAKindARowIntroducedToThatRowsVersion(t *testing.T) {
	t.Parallel()
	fields := append(statelog.RecordFields{}, controlFields...)
	fields = append(fields, statelog.VersionedField{
		Name: "Kind=gadget", Since: 2, Path: []string{"Kind"}, Equals: "gadget",
	})
	byTable := func(rec controlRecord) (int, error) {
		body, err := json.Marshal(rec)
		if err != nil {
			return 0, err
		}
		return fields.Minimum(rec.Op, body)
	}
	withGadgets := func(stamp stamper) statelogtest.Candidate {
		c := control()
		c.Kinds = []string{"widget", "gadget"}
		c.Fields = fields
		c.Encode = func(kind, id, opID string, version int) ([]byte, error) {
			return encodeControl(controlRecord{Envelope: controlEnvelope(kind, id, opID, version)}, stamp)
		}
		c.Carrying = func(field statelog.VersionedField) ([]byte, error) {
			rec := controlRecord{Envelope: controlEnvelope("widget", "carrying", "suite-carrying", 0)}
			switch field.Name {
			case "widget.Colour":
				rec.Colour = "violet"
			case "Kind=gadget":
				rec.Envelope = controlEnvelope("gadget", "carrying", "suite-carrying", 0)
			default:
				return nil, fmt.Errorf("the control has no field %s", field.Name)
			}
			return encodeControl(rec, stamp)
		}
		return c
	}

	if errs := statelogtest.Stamping(withGadgets(byTable)); len(errs) != 0 {
		t.Errorf("a domain stamping a gadget at the version its row gives it was "+
			"refused: %v", errs)
	}
	errs := statelogtest.Stamping(withGadgets(func(controlRecord) (int, error) { return 1, nil }))
	var found bool
	for _, err := range errs {
		if strings.Contains(err.Error(), "the only versioned field it carries is its kind's own") {
			found = true
		}
	}
	if !found {
		t.Errorf("a gadget stamped at version one passed the base-record case: %v", errs)
	}
}

// A GATE IS STAMPED AT EXACTLY WHAT ITS OWN CHANGED APPLY NEEDS — the base
// version until a field the table marks [statelog.VersionedField.Gate] raises
// it, and never higher for any other field.
//
// The suite once refused every gate above version 1, which is the rule for a
// domain whose gates never change: the first change to an eviction's apply,
// stamped above the base exactly as the framework's field table intends (an
// older build HALTS at a gate it cannot read rather than applying it the old
// way), would have failed the shared suite with a sentence saying that build
// defers it. Each arm is one way the write path can stamp a gate, and the
// verdict comes back from the bytes it published.
func TestTheSuiteHoldsAGateToItsOwnChangedApplysVersion(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		gate   changedGate
		refuse string // empty when the suite must accept
	}{
		"a gate carrying nothing new, at the base version": {
			gate: changedGate{v: 1},
		},
		"a gate carrying its changed apply's field, at that field's version": {
			gate: changedGate{v: 3, Grace: "drain"},
		},
		"a gate carrying its changed apply's field and an ordinary one beneath it": {
			gate: changedGate{v: 3, Grace: "drain", Colour: "violet"},
		},
		"a gate carrying its changed apply's field, stamped at the base version": {
			gate: changedGate{v: 1, Grace: "drain"}, refuse: "at record version 1, want 3",
		},
		"a gate carrying nothing new, stamped above the base": {
			gate: changedGate{v: 3}, refuse: "at record version 3, want 1",
		},
		"a gate carrying an ordinary versioned field and no changed apply": {
			gate: changedGate{v: 2, Colour: "violet"}, refuse: "above its own changed apply",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := statelogtest.Stamped(t, func(*testing.T) statelogtest.Candidate {
				c := control()
				c.Domain = gateChangedDomain{}
				c.Rows = rowsOver(c.Domain)
				c.Fields = gateFields
				c.Write = gateWrite(tc.gate)
				return c
			})
			switch {
			case tc.refuse == "" && err != nil:
				t.Fatalf("the suite refused %s: %v", name, err)
			case tc.refuse != "" && err == nil:
				t.Fatalf("the suite passed %s", name)
			case tc.refuse != "" && !strings.Contains(err.Error(), tc.refuse):
				t.Errorf("the suite refused %s, but for something else: %v", name, err)
			}
		})
	}
}

// gateFields is the control's table once its eviction's apply has changed: the
// widget's colour at 2, as ever, and a field only a gate carries at 3, marked
// as the change to what the gate's apply does.
var gateFields = statelog.RecordFields{
	controlFields[0],
	{Name: "eviction.Grace", Since: 3, Path: []string{"grace"}, Gate: true},
}

// gateChangedDomain is the control at the build that reads its changed gate.
type gateChangedDomain struct{ controlDomain }

func (gateChangedDomain) RecordVersion() int { return 3 }

// changedGate is an eviction as the write path publishes it, stamped at v
// whatever it carries — the arms above choose both.
type changedGate struct {
	controlGate
	v      int
	Grace  string `json:"grace,omitempty"`
	Colour string `json:"colour,omitempty"`
}

// gateWrite is a write path that publishes one eviction, as a node gate.
func gateWrite(gate changedGate) func(context.Context, *statelog.Publisher, store.ReplicatedReader) error {
	return func(ctx context.Context, pub *statelog.Publisher, _ store.ReplicatedReader) error {
		const node = "evicted-node"
		opID := "gate-evict-" + node
		subject := statelog.Subject{Kind: controlGateKind, ID: node}
		scope := statelog.ScopeSet{Paths: []string{controlGateKind}}
		_, err := pub.Publish(ctx, statelog.Request{
			Subject: subject, Scope: scope, OpID: opID,
			Pattern: statelog.PatternArbitrated, NodeGate: true,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				gate.controlGate = controlGate{
					Envelope: statelog.Envelope{
						V: gate.v, Kind: controlGateKind, Subject: subject, OpID: opID,
						Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
					},
					Node: node,
				}
				payload, err := json.Marshal(gate)
				return statelog.Decision{Payload: payload}, err
			},
		})
		return err
	}
}

// A DOMAIN THAT STAMPS THE WRONG VERSION IS CAUGHT, in both directions and
// in both halves of the table-and-build agreement.
//
// Each arm is the shape the stamping rule was written against: a writer that
// stamps its BUILD's version holds every record back from every older node,
// one that stamps the base version always lets an older node apply a field
// away, and a table and a build that disagree are one of those two waiting
// for the day the other half moves.
func TestTheSuiteCatchesADomainThatStampsTheWrongVersion(t *testing.T) {
	t.Parallel()
	buildVersion := func(controlRecord) (int, error) { return controlDomain{}.RecordVersion(), nil }
	baseVersion := func(controlRecord) (int, error) { return 1, nil }
	for name, tc := range map[string]struct {
		mutate func(*statelogtest.Candidate)
		names  string
	}{
		"a writer that stamps its build's version on everything": {
			mutate: func(c *statelogtest.Candidate) {
				c.Encode = func(kind, id, opID string, version int) ([]byte, error) {
					return encodeControl(controlRecord{Envelope: controlEnvelope(kind, id, opID, version)}, buildVersion)
				}
			},
			names: "carrying no versioned field at version 2",
		},
		"a writer that stamps version one on everything": {
			mutate: func(c *statelogtest.Candidate) {
				c.Carrying = func(statelog.VersionedField) ([]byte, error) {
					return encodeControl(controlRecord{
						Envelope: controlEnvelope("widget", "carrying", "suite-carrying", 0),
						Colour:   "violet",
					}, baseVersion)
				}
			},
			names: "would decode it, drop the field",
		},
		"a field table whose path misses where the field is written": {
			mutate: func(c *statelogtest.Candidate) {
				// THE ENCODER READS THE SAME TABLE the candidate
				// declares, as every domain's does, so a path typo is
				// a field nothing ever stamps.
				misspelt := statelog.RecordFields{
					{Name: "widget.Colour", Since: 2, Path: []string{"color"}},
				}
				c.Fields = misspelt
				c.Carrying = func(statelog.VersionedField) ([]byte, error) {
					rec := controlRecord{
						Envelope: controlEnvelope("widget", "carrying", "suite-carrying", 0),
						Colour:   "violet",
					}
					return encodeControl(rec, func(rec controlRecord) (int, error) {
						body, err := json.Marshal(rec)
						if err != nil {
							return 0, err
						}
						return misspelt.Minimum(rec.Kind, body)
					})
				}
			},
			names: "would decode it, drop the field",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := control()
			tc.mutate(&c)
			errs := statelogtest.Stamping(c)
			if len(errs) == 0 {
				t.Fatalf("the suite passed %s", name)
			}
			var found bool
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.names) {
					found = true
				}
			}
			if !found {
				t.Errorf("the suite objected, but to something else: %v", errs)
			}
		})
	}

	for name, tc := range map[string]struct {
		domain statelog.Domain
		fields statelog.RecordFields
		names  string
	}{
		"a build that reads a version no field introduced": {
			domain: readsAhead{controlDomain{}}, fields: controlFields,
			names: "no field introduced",
		},
		"a field at a version the build does not read": {
			domain: controlDomain{},
			fields: statelog.RecordFields{{Name: "widget.Colour", Since: 3, Path: []string{"colour"}}},
			names:  "would be retained by the build that wrote them",
		},
		"a field table with no record carrying its field": {
			domain: controlDomain{}, fields: controlFields,
			names: "no record carrying one",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := control()
			c.Domain, c.Fields = tc.domain, tc.fields
			if tc.names == "no record carrying one" {
				c.Carrying = nil
			}
			var found bool
			for _, err := range statelogtest.Declaration(c) {
				if strings.Contains(err.Error(), tc.names) {
					found = true
				}
			}
			if !found {
				t.Errorf("the declaration passed %s", name)
			}
		})
	}
}

// readsAhead claims a record version its field table never reaches.
type readsAhead struct{ controlDomain }

func (readsAhead) RecordVersion() int { return 3 }

type brokenStream struct{ controlDomain }

func (b brokenStream) Stream() statelog.StreamSpec {
	s := b.controlDomain.Stream()
	s.MaxPerSubject = 1 // a strict log that keeps one message per subject
	return s
}

type ledgerless struct{ controlDomain }

func (ledgerless) OpsTable() string { return "" }

// scrubbedLedger classes its operation ledger as this node's own, which is
// what every domain here did until an adopter's first attempts started coming
// back `unknown`.
type scrubbedLedger struct{ controlDomain }

func (scrubbedLedger) Tables() map[string]statelog.TableClass {
	tables := controlDomain{}.Tables()
	tables["control_ops"] = statelog.Local
	return tables
}

// gatingCompacted is the compacted control claiming that a seat depends on
// it — with an operation ledger, so the only rule it breaks is that one: a
// compacted domain's gap is a coverage number, and admission waiting on it
// withholds every seat claim on a node over one.
type gatingCompacted struct{ compactedControl }

func (gatingCompacted) GatesSeatAdmission() bool { return true }
func (gatingCompacted) OpsTable() string         { return "control_ops" }

func (gatingCompacted) Tables() map[string]statelog.TableClass {
	tables := compactedControl{}.Tables()
	tables["control_ops"] = statelog.Divergent
	return tables
}

// ledgerlessGate is the strict control with no operation ledger and nothing
// arbitrated, still claiming that a seat depends on it — the pairing the
// ledger rule refuses, with every other rule satisfied.
type ledgerlessGate struct{ controlDomain }

func (ledgerlessGate) OpsTable() string { return "" }

func (ledgerlessGate) Stream() statelog.StreamSpec {
	s := controlDomain{}.Stream()
	s.ArbitratedKinds = nil
	return s
}

func (ledgerlessGate) Tables() map[string]statelog.TableClass {
	tables := controlDomain{}.Tables()
	delete(tables, "control_ops")
	return tables
}

type nothingReplicated struct{ controlDomain }

func (nothingReplicated) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{"control_widgets": statelog.Local}
}

// appendOnly writes a new row per delivery, so a redelivery is visible.
// countingApplier is the liar: it writes the right ROW for every delivery and
// the wrong VALUE, incrementing a column each time it sees one.
//
// THAT IS THE SHAPE A REAL NON-IDEMPOTENT APPLIER TAKES. An applier that
// inserted a new row per delivery would be caught by counting rows, which the
// suite did until this commit; the failure a content comparison exists to
// catch is the one where the shape is right and the values are not — a
// redelivery that increments, a clock that is read, a node id that is stamped.
// Two nodes then hold the same number of rows and disagree about what is in
// them, which is precisely what nobody notices.
type countingApplier struct{ controlApplier }

func (countingApplier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, _ statelog.ApplyOptions) (int, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO control_widgets (id, version) VALUES (?, 1)
		ON CONFLICT (id) DO UPDATE SET version = version + 1`,
		rec.Subject.ID)
	return 1, err
}

// unlisted claims identity and has no answer for who is evicted on its log.
type unlisted struct{ controlBase }

// blindLister answers from somewhere other than its own log's rows — the
// shape the trim's old read had, asking the tracker's table on the pages
// log's behalf and finding nothing.
type blindLister struct{ controlDomain }

func (blindLister) Evictions(context.Context, store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	return nil, nil
}

// deletingApplier removes a readmitted node's row instead of marking it back,
// which loses the eviction's history on every replay.
type deletingApplier struct{ controlApplier }

func (d deletingApplier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	if rec.Kind == controlGateKind {
		return applyControlGate(ctx, tx, rec, true)
	}
	return d.controlApplier.Apply(ctx, tx, rec, opts)
}

// listingCompacted claims no identity and answers for evictions anyway.
type listingCompacted struct{ compactedControl }

func (listingCompacted) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	return controlDomain{}.Evictions(ctx, db)
}

// A DOMAIN THAT MISNAMES ITS NODE GATE IS CAUGHT.
//
// The publisher holds a write flagged a node gate to this answer, and the flag
// excuses the passed-generation fence, a peer's truncated rows and the gate
// reserve — so each arm is a defect that ships silently until the day it
// matters: an eviction the publisher refuses on the log it was meant to
// unpin, a node gate a lagging build defers instead of stopping on, or a log
// that counts no node naming a record that may pass every fence.
func TestTheSuiteCatchesADomainThatMisnamesItsNodeGate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*statelogtest.Candidate)
		names  string
	}{
		"an identity-claiming domain whose eviction is no node gate": {
			mutate: func(c *statelogtest.Candidate) { c.Domain = evictionNotANodeGate{} },
			names:  "does not call its eviction of a node a node gate",
		},
		"a node gate that installs no apply gate": {
			mutate: func(c *statelogtest.Candidate) { c.Domain = nodeGateWithoutGate{} },
			names:  "installs no apply gate",
		},
		"a domain that claims no identity and names a node gate": {
			mutate: func(c *statelogtest.Candidate) {
				c.Domain = compactedNodeGate{}
				c.Rows = rowsOver(c.Domain)
			},
			names: "claims no identity",
		},
		"an identity-claiming candidate with no eviction record": {
			mutate: func(c *statelogtest.Candidate) { c.EncodeGate = nil },
			names:  "supplies no eviction record",
		},
		"an identity-claiming domain that reads no node gate off its log": {
			mutate: func(c *statelogtest.Candidate) { c.Domain = unprobed{controlBase{}} },
			names:  "cannot read its node gates off its log",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := control()
			tc.mutate(&c)
			err := statelogtest.NodeGates(c)
			if err == nil {
				t.Fatalf("the suite passed %s", name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the suite objected, but to something else: %v", err)
			}
		})
	}
}

// unprobed claims identity, gates its evictions, and cannot read them off its
// log — the base with the control's gate answers and none of its probe.
type unprobed struct{ controlBase }

func (unprobed) InstallsGate(env statelog.Envelope) bool { return env.Kind == controlGateKind }
func (unprobed) NodeGate(env statelog.Envelope) bool     { return env.Kind == controlGateKind }

// evictionNotANodeGate answers no to its own eviction, so the publisher would
// refuse the gesture that unpins its log.
type evictionNotANodeGate struct{ controlDomain }

func (evictionNotANodeGate) NodeGate(statelog.Envelope) bool { return false }

// nodeGateWithoutGate names its eviction a node gate and says it installs no
// apply gate, so a build that cannot decode it would defer it.
type nodeGateWithoutGate struct{ controlDomain }

func (nodeGateWithoutGate) InstallsGate(statelog.Envelope) bool { return false }

// compactedNodeGate counts no node and calls every record a node gate — an
// apply gate as well, so what is caught is the identity half alone.
type compactedNodeGate struct{ compactedControl }

func (compactedNodeGate) InstallsGate(statelog.Envelope) bool { return true }
func (compactedNodeGate) NodeGate(statelog.Envelope) bool     { return true }

// A DOMAIN WHOSE TRIMMED RECORD THE FRAMEWORK CANNOT USE IS CAUGHT — and each
// arm is a way one ships silently: no record, and the anchors grow with every
// subject ever written; one the framework does not recognise, and it removes
// nothing; one an older build defers, a gate, or arbitrated, and every node
// pays for it on every tick; an applier that writes a row for it, or has no
// case for it, and the first trim changes or stops every node's copy.
func TestTheSuiteCatchesADomainWhoseTrimmedRecordTheFrameworkCannotUse(t *testing.T) {
	t.Parallel()
	encoding := func(mutate func(*statelog.Envelope)) func(statelog.Envelope) ([]byte, error) {
		return func(env statelog.Envelope) ([]byte, error) {
			mutate(&env)
			return json.Marshal(env)
		}
	}
	for name, tc := range map[string]struct {
		candidate func() statelogtest.Candidate
		names     string
	}{
		"a log that writes anchors and supplies no trimmed record": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.EncodeTrimmed = nil
				return c
			},
			names: "supplies no trimmed record",
		},
		"a log that writes no anchor and supplies one": {
			candidate: func() statelogtest.Candidate {
				c := compacted()
				c.EncodeTrimmed = encodeControlTrimmed
				return c
			},
			names: "writes no anchor",
		},
		"an encoder that writes another kind": {
			candidate: withTrimmed(encoding(func(e *statelog.Envelope) {
				e.Kind, e.Subject.Kind = "widget", "widget"
			})),
			names: "would not recognise it",
		},
		"an encoder that drops the floor": {
			candidate: withTrimmed(encoding(func(e *statelog.Envelope) { e.Subject.ID = "" })),
			names:     "would not recognise it",
		},
		"an encoder that stamps an operation": {
			candidate: withTrimmed(encoding(func(e *statelog.Envelope) { e.OpID = "op-1" })),
			names:     "carries operation",
		},
		"an encoder that stamps a version an older build defers": {
			candidate: withTrimmed(encoding(func(e *statelog.Envelope) { e.V = 2 })),
			names:     "an older build would defer it",
		},
		"an encoder that drops the writer": {
			candidate: withTrimmed(encoding(func(e *statelog.Envelope) { e.Writer = "" })),
			names:     "eviction gate reads the writer",
		},
		"an encoder that scopes it to an object": {
			candidate: withTrimmed(encoding(func(e *statelog.Envelope) {
				e.Scope = statelog.ScopeSet{Paths: []string{"widget/w-1"}}
			})),
			names: "not the framework's",
		},
		"a domain that calls it a gate": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.Domain = trimmedGate{controlDomain{}}
				c.Rows = rowsOver(c.Domain)
				return c
			},
			names: "declares its trimmed record a gate",
		},
		"a log that arbitrates it": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.Domain = trimmedArbitrated{controlDomain{}}
				c.Rows = rowsOver(c.Domain)
				return c
			},
			names: "arbitrates the trimmed kind",
		},
		"an applier that writes a row for it": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.Applier = widgetsForAll{}
				return c
			},
			names: "wrote rows of its own",
		},
		"an applier with no case for it": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.Applier = noTrimmedCase{}
				return c
			},
			names: "did not apply the trimmed record",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := statelogtest.TrimmedAnchorsGoAndAZeroWriteIsFenced(t,
				func(*testing.T) statelogtest.Candidate { return tc.candidate() })
			if err == nil {
				t.Fatalf("the suite passed %s", name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the suite objected, but to something else: %v", err)
			}
		})
	}
}

// withTrimmed is the control with its trimmed-record encoder replaced.
func withTrimmed(encode func(statelog.Envelope) ([]byte, error)) func() statelogtest.Candidate {
	return func() statelogtest.Candidate {
		c := control()
		c.EncodeTrimmed = encode
		return c
	}
}

// trimmedGate calls its trimmed record an apply gate.
type trimmedGate struct{ controlDomain }

func (trimmedGate) InstallsGate(env statelog.Envelope) bool {
	return env.Kind == statelog.TrimmedKind || controlDomain{}.InstallsGate(env)
}

// trimmedArbitrated arbitrates the trimmed kind on its log.
type trimmedArbitrated struct{ controlDomain }

func (d trimmedArbitrated) Stream() statelog.StreamSpec {
	s := d.controlDomain.Stream()
	s.ArbitratedKinds = append(slices.Clone(s.ArbitratedKinds), statelog.TrimmedKind)
	return s
}

// widgetsForAll writes the control's row for every record it has no other case
// for — the trimmed record included.
type widgetsForAll struct{ controlApplier }

func (widgetsForAll) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, _ statelog.ApplyOptions) (int, error) {
	if rec.Kind == controlGateKind {
		return applyControlGate(ctx, tx, rec, false)
	}
	return applyWidget(ctx, tx, rec)
}

// THE SUITE CATCHES A BARRIER THAT WRITES A ROW, and a candidate that never says
// whether its log has one.
func TestTheSuiteCatchesABarrierThatWritesARow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		candidate func() statelogtest.Candidate
		names     string
	}{
		"a candidate that states neither a barrier nor none": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.EncodeBarrier = nil
				return c
			},
			names: "does not say its log has no barrier",
		},
		"a candidate that states both": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.NoBarrier = true
				return c
			},
			names: "state one",
		},
		"an applier that writes a row for it": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.Applier = widgetsForAll{}
				return c
			},
			names: "changed rows for a barrier",
		},
		"an applier with no case for it": {
			candidate: func() statelogtest.Candidate {
				c := control()
				c.Applier = noBarrierCase{}
				return c
			},
			names: "did not apply its barrier",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := statelogtest.ABarrierWritesNoRow(t,
				func(*testing.T) statelogtest.Candidate { return tc.candidate() })
			if err == nil {
				t.Fatalf("the suite passed %s", name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the suite objected, but to something else: %v", err)
			}
		})
	}
}

// noBarrierCase faults on a barrier, as a domain's exhaustive switch does on a
// kind it has no case for.
type noBarrierCase struct{ controlApplier }

func (noBarrierCase) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, opts statelog.ApplyOptions) (int, error) {
	if rec.Kind == statelog.BarrierKind {
		return 0, fmt.Errorf("control: no case for a %q record", rec.Kind)
	}
	return controlApplier{}.Apply(ctx, tx, rec, opts)
}

// noTrimmedCase faults on the trimmed record, as a domain's exhaustive switch
// does on a kind it has no case for.
type noTrimmedCase struct{ controlApplier }

func (noTrimmedCase) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, opts statelog.ApplyOptions) (int, error) {
	if rec.Kind == statelog.TrimmedKind {
		return 0, fmt.Errorf("control: no case for a %q record", rec.Kind)
	}
	return controlApplier{}.Apply(ctx, tx, rec, opts)
}
