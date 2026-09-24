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
// version 2, so the stamping cases have something to hold it to.
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

func (controlDomain) RecordVersion() int { return 2 }

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
