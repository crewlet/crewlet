package statelogtest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
		c := control()
		c.Domain = compactedControl{}
		c.Rows = rowsOver(c.Domain)
		return c
	})
}

// rowsOver is the framework's own read seam over one control domain, which is
// what a real domain's constructor returns too.
func rowsOver(d statelog.Domain) func(*store.DB) (statelog.Rows, error) {
	return func(db *store.DB) (statelog.Rows, error) {
		return statelog.NewRows(db, d, nil)
	}
}

// controlWrite is the control domain's write path: one widget, through the
// publisher, stamped with what stampOf makes of the stamp it is handed — the
// identity for a domain that does its job, anything else for one that lies.
func controlWrite(stampOf func(statelog.Stamp) statelog.Stamp) func(context.Context,
	*statelog.Publisher, *store.DB) error {

	return func(ctx context.Context, pub *statelog.Publisher, _ *store.DB) error {
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
		Domain:     controlDomain{},
		Applier:    controlApplier{},
		Kinds:      []string{"widget"},
		Rows:       rowsOver(controlDomain{}),
		Write:      controlWrite(func(s statelog.Stamp) statelog.Stamp { return s }),
		EncodeGate: encodeControlGate,
		Migrate: func(ctx context.Context, db *store.DB) error {
			return db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, controlDDL)
				return err
			})
		},
		Encode: func(kind, id, opID string, version int) ([]byte, error) {
			return json.Marshal(statelog.Envelope{
				V:       version,
				Kind:    kind,
				Subject: statelog.Subject{Kind: kind, ID: id},
				OpID:    opID,
				Gen:     1,
				Scope:   statelog.ScopeSet{Paths: []string{kind + "/" + id}},
				Writer:  "control",
			})
		},
	}
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
    applied_at INTEGER NOT NULL
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

type controlBase struct{}

func (controlBase) Name() string { return "control" }

func (controlBase) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            "CREWLET_CONTROL_LOG",
		Subjects:        []string{"crewlet.control.log.>"},
		SubjectPrefix:   "crewlet.control.log",
		ArbitratedKinds: []string{"widget"},
		MaxBytes:        16 << 20,
		Duplicates:      2 * time.Minute,
		Replay:          statelog.ReplayStrict,
	}
}

func (controlBase) RecordVersion() int { return 1 }

func (controlBase) Envelope(payload []byte) (statelog.Envelope, error) {
	var env statelog.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return statelog.Envelope{}, err
	}
	return env, nil
}

func (controlBase) InstallsGate(statelog.Envelope) bool { return false }

func (controlBase) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		"control_widgets":        statelog.Replicated,
		"control_ops":            statelog.Local,
		"control_log_deferred":   statelog.Local,
		"control_deferred_scope": statelog.Local,
	}
}

func (controlBase) DeferredTable() string { return "control_log_deferred" }
func (controlBase) ScopeIndex() string    { return "control_deferred_scope" }
func (controlBase) OpsTable() string      { return "control_ops" }
func (controlBase) ReadinessInput() bool  { return true }
func (controlBase) ClaimsIdentity() bool  { return true }
func (controlBase) FeedGroup() string     { return "" }

// controlDomain is the identity-claiming control: the base, plus the eviction
// record and table every domain the trim counts nodes on has to have.
type controlDomain struct{ controlBase }

func (controlDomain) InstallsGate(env statelog.Envelope) bool {
	return env.Kind == controlGateKind
}

func (controlDomain) Tables() map[string]statelog.TableClass {
	tables := controlBase{}.Tables()
	tables["control_evictions"] = statelog.Replicated
	return tables
}

// Evictions answers from this log's own rows, as a real domain does.
func (controlDomain) Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
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

func (compactedControl) OpsTable() string     { return "" }
func (compactedControl) ReadinessInput() bool { return false }
func (compactedControl) ClaimsIdentity() bool { return false }

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
	if rec.Kind == controlGateKind {
		return applyControlGate(ctx, tx, rec, false)
	}
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
			c.Applier = appendOnly{}
			return c
		})
		if err == nil {
			t.Fatal("the suite passed an applier that writes a new row per delivery")
		}
	})
}

type brokenStream struct{ controlDomain }

func (b brokenStream) Stream() statelog.StreamSpec {
	s := b.controlDomain.Stream()
	s.MaxPerSubject = 1 // a strict log that keeps one message per subject
	return s
}

type ledgerless struct{ controlDomain }

func (ledgerless) OpsTable() string { return "" }

type nothingReplicated struct{ controlDomain }

func (nothingReplicated) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{"control_widgets": statelog.Local}
}

// appendOnly writes a new row per delivery, so a redelivery is visible.
type appendOnly struct{ controlApplier }

func (appendOnly) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, _ statelog.ApplyOptions) (int, error) {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO control_widgets (id, version) VALUES (?, ?)`,
		fmt.Sprintf("%s-%d", rec.Subject.ID, rec.Position.Seq), rec.Position.Packed())
	return 1, err
}

// unlisted claims identity and has no answer for who is evicted on its log.
type unlisted struct{ controlBase }

// blindLister answers from somewhere other than its own log's rows — the
// shape the trim's old read had, asking the tracker's table on the pages
// log's behalf and finding nothing.
type blindLister struct{ controlDomain }

func (blindLister) Evictions(context.Context, *store.DB) ([]statelog.EvictionRow, error) {
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

func (listingCompacted) Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error) {
	return controlDomain{}.Evictions(ctx, db)
}
