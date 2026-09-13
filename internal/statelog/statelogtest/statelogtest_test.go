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
		c.Domain = compactedControl{c.Domain.(controlDomain)}
		return c
	})
}

func control() statelogtest.Candidate {
	return statelogtest.Candidate{
		Domain:  controlDomain{},
		Applier: controlApplier{},
		Kinds:   []string{"widget"},
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
`

type controlDomain struct{}

func (controlDomain) Name() string { return "control" }

func (controlDomain) Stream() statelog.StreamSpec {
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

func (controlDomain) RecordVersion() int { return 1 }

func (controlDomain) Envelope(payload []byte) (statelog.Envelope, error) {
	var env statelog.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return statelog.Envelope{}, err
	}
	return env, nil
}

func (controlDomain) InstallsGate(statelog.Envelope) bool { return false }

func (controlDomain) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		"control_widgets":        statelog.Replicated,
		"control_ops":            statelog.Local,
		"control_log_deferred":   statelog.Local,
		"control_deferred_scope": statelog.Local,
	}
}

func (controlDomain) DeferredTable() string { return "control_log_deferred" }
func (controlDomain) ScopeIndex() string    { return "control_deferred_scope" }
func (controlDomain) OpsTable() string      { return "control_ops" }
func (controlDomain) ReadinessInput() bool  { return true }
func (controlDomain) ClaimsIdentity() bool  { return true }

// compactedControl is the same domain under the other replay protocol: no
// arbitration, no operation ledger, and a gap that is a coverage number rather
// than a fault.
type compactedControl struct{ controlDomain }

func (c compactedControl) Stream() statelog.StreamSpec {
	s := c.controlDomain.Stream()
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
