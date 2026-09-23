package iamdomain_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SWEEP DELETES BY POSITION RANGE, NEVER BY A CLOCK.
//
// THE case this whole mechanism exists for. These rows are identity-claimed:
// N nodes assert their replicated tables are byte-identical. A node sweeping
// on its own clock would hold different bytes from its peers, and the claim
// would quietly become a claim about how synchronised their clocks were —
// checkable by nothing, false whenever an NTP correction landed, and
// indistinguishable from an applier that had diverged.
//
// So the publisher resolves each horizon to a POSITION, once, and every node
// deletes exactly the same rows.
//
// THE CLOCK THAT COULD DIFFER IS THE BATCH INSTANT. Every other instant the
// applier can reach is the BROKER's, stamped on the record and identical on
// every node — which is a structural guarantee rather than a rule somebody
// follows: [applyContext] carries the broker's instant and does not carry
// ApplyOptions.Now at all, so there is nothing per-node for an arm here to
// read. This case varies the one that IS per-node and asserts it changes
// nothing, and then asserts the POSITION is what decided.
func TestTheSweepDeletesByPositionRangeNeverByAClock(t *testing.T) {
	t.Parallel()

	// TWO NODES, TWO BATCH CLOCKS, ONE RECORD. The skew is a year, which
	// is past every horizon this domain keeps — so a sweep that read the
	// batch instant at all would delete a different set on each.
	early := brokerAt.Add(-365 * 24 * time.Hour)
	late := brokerAt.Add(365 * 24 * time.Hour)

	first := sweepRig(t, early)
	second := sweepRig(t, late)
	if got, want := first.trail(), second.trail(); !equalRows(got, want) {
		t.Fatalf("the two nodes started from different rows: %v vs %v", got, want)
	}

	// The record's own horizon: everything below position 3, which is the
	// first two trail rows and not the third.
	record := sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
		Changes: 3,
	})
	first.apply(record, early)
	second.apply(record, late)

	got, want := first.trail(), second.trail()
	if !equalRows(got, want) {
		t.Fatalf("two nodes a year apart on their batch clocks deleted "+
			"different rows:\n node with the early clock kept %v\n node with "+
			"the late clock kept %v\n— the sweep is reading a per-node clock, "+
			"so 'N byte-identical copies' is a claim about how synchronised "+
			"they are", got, want)
	}
	if len(got) != 1 {
		t.Errorf("the sweep kept %d rows, want the one at or above the "+
			"horizon: %v", len(got), got)
	}
}

// A ZERO HORIZON DELETES NOTHING, which is the correct reading of "this
// company has no rows old enough yet" and is NOT the same as "everything".
//
// A zero is below every row's version, so the arithmetic already says so —
// this case exists because the OPPOSITE reading is the one somebody writes by
// accident, and it would empty the authentication trail of a company on its
// first tick.
func TestAZeroHorizonSweepsNothing(t *testing.T) {
	t.Parallel()
	rig := sweepRig(t, brokerAt)
	before := rig.trail()
	rig.apply(sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
	}), brokerAt)
	if after := rig.trail(); !equalRows(before, after) {
		t.Errorf("a sweep with no horizon deleted rows: %v became %v — a zero "+
			"position is below every row's version, and reading it as "+
			"'everything' empties a new company's trail on its first tick",
			before, after)
	}
}

// AND A SWEEP NEVER EXCEEDS ONE TRANSACTION'S ROW BUDGET.
//
// The budget is per TRANSACTION and the sweep is per BUCKET, and the two are
// not the same bound: a bucket unswept for a month is not a sixty-fourth of a
// tick, it is a sixty-fourth of a month. Without the cap, one record's apply
// holds this store's only writer for as long as that takes, with every other
// domain's apply waiting behind it.
//
// THE BUCKET CONVERGES ANYWAY, which is what makes the cap cheap: what is left
// is swept on the next tick.
func TestTheSweepNeverExceedsOneTransactionsBudget(t *testing.T) {
	t.Parallel()
	rig := sweepRig(t, brokerAt)

	// More trail rows than one transaction may delete, in one bucket.
	const over = iamdomain.MaxSweepRows + 25
	rig.fillTrail(over)

	rows := rig.apply(sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
		Changes: 1 << 40,
	}), brokerAt)
	if rows > iamdomain.MaxSweepRows {
		t.Errorf("one sweep record deleted %d rows and the budget is %d — this "+
			"transaction holds the store's only writer, and every other "+
			"domain's apply is waiting behind it", rows, iamdomain.MaxSweepRows)
	}
	if rows == 0 {
		t.Fatal("the sweep deleted nothing, so the cap is not what bounded it")
	}
	// AND WHAT IS LEFT IS SWEPT NEXT TICK, which is what makes the cap a
	// bound on one transaction rather than on the retention itself.
	remaining := len(rig.trail())
	if remaining == 0 {
		t.Fatal("one record swept everything, so the cap did not apply and " +
			"this case is asserting nothing")
	}
	for range 3 {
		rig.apply(sweepRecord(t, iamdomain.Sweep{
			V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
			Changes: 1 << 40,
		}), brokerAt)
	}
	if after := len(rig.trail()); after >= remaining {
		t.Errorf("three more sweeps left %d rows of %d — a bucket past the cap "+
			"must converge, or its retention never takes effect at all",
			after, remaining)
	}
}

// AND THE BUDGET IS THE RECORD'S, NOT EACH STATEMENT'S.
//
// The case above fills ONE table and so cannot tell a budget per record from a
// budget per statement. This one puts just over half a budget behind each of
// three predicates in one bucket — the change trail, the session trail and the
// ended sessions — which a per-statement LIMIT deletes in full, one and a half
// budgets in a transaction sized for one.
func TestOneSweepRecordSharesItsBudgetAcrossEveryTable(t *testing.T) {
	t.Parallel()
	rig := sweepRig(t, brokerAt)
	bucket := iamdomain.BucketOf(sweptPerson)
	const each = iamdomain.MaxSweepRows/2 + 10
	if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range each {
			for _, class := range []iamdomain.HistoryClass{
				iamdomain.ClassChange, iamdomain.ClassSession,
			} {
				if _, err := tx.ExecContext(t.Context(), `
					INSERT INTO iam_history
						(id, class, object_kind, object_id, person_id, op,
						 created_at, broker_at, bucket, version, document)
					VALUES (?, ?, 'person', ?, ?, 'status', 1, 1, ?, ?, x'')`,
					fmt.Sprintf("%s-%d", class, i), string(class), sweptPerson,
					sweptPerson, int64(bucket), i+1); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO iam_sessions
					(lineage, person_id, ended_at, ended_reason, bucket,
					 created_at, version, document)
				VALUES (?, ?, 1, 'logout', ?, 1, ?, x'')`,
				fmt.Sprintf("lineage-%d", i), sweptPerson, int64(bucket),
				i+1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("fill the bucket: %v", err)
	}

	rows := rig.apply(sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: bucket,
		Changes: 1 << 40, Sessions: 1 << 40, Expired: brokerAt,
	}), brokerAt)
	if rows > iamdomain.MaxSweepRows {
		t.Fatalf("one sweep record deleted %d rows across its statements and the "+
			"budget is %d — a LIMIT per statement is a budget per statement, "+
			"and this transaction holds the store's only writer", rows,
			iamdomain.MaxSweepRows)
	}
	if rows < iamdomain.MaxSweepRows {
		t.Errorf("one sweep record deleted %d rows with %d due — it stopped "+
			"short of the budget, so a backlog converges slower than it has to",
			rows, 3*each)
	}
}

// A SWEEP NAMING A BUCKET THIS ESTATE DOES NOT HAVE IS REFUSED.
//
// It would otherwise delete nothing, silently, for ever — a retention that
// reports itself running and collects nothing, which is the failure mode every
// horizon in this tree is written to avoid.
func TestASweepPastTheBucketCountIsRefused(t *testing.T) {
	t.Parallel()
	rig := sweepRig(t, brokerAt)
	record := sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.Buckets + 1, Changes: 1 << 40,
	})
	err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := rig.applier.Apply(t.Context(), tx, record,
			statelog.ApplyOptions{Now: brokerAt, StoredAt: brokerAt})
		return err
	})
	if err == nil {
		t.Error("a sweep naming a bucket outside the estate applied — it would " +
			"delete nothing, for ever, while reporting itself as retention " +
			"that runs")
	}
}

// --- the rig ---------------------------------------------------------------- //

const sweptPerson = "018f3a9c-0000-7000-8000-00000000beef"

// sweepRigT is one node's applier and store, with a trail already in it.
type sweepRigT struct {
	*writeRig
	written int
}

// sweepRig builds a node whose clock is `now`, with three trail rows at
// positions 1, 2 and 3.
func sweepRig(t *testing.T, now time.Time) *sweepRigT {
	t.Helper()
	rig := &sweepRigT{writeRig: newWriteRig(t)}
	for seq := range 3 {
		rig.write(uint64(seq+1), now)
	}
	return rig
}

// write puts one trail row in, through the APPLIER rather than through SQL, so
// the rows are the ones a record actually produces.
func (r *sweepRigT) write(seq uint64, now time.Time) {
	r.t.Helper()
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.RecordVersion, OpID: opIDFor(seq),
			Subject: iamdomain.PersonSubject(sweptPerson),
			Op:      iamdomain.OpStatus, Gen: 0, Writer: "node-a",
			Scope: iamdomain.PeopleScope(sweptPerson),
		},
		Person: sweptPerson, Actor: "suite", ActorKind: iam.KindMachine,
		Mutation: mustJSON(r.t, iamdomain.StatusChange{
			V: iamdomain.DocumentVersion, Stage: iam.StageActive,
		}),
	})
	if err != nil {
		r.t.Fatalf("encode: %v", err)
	}
	r.apply(statelog.Record{
		Envelope: statelog.Envelope{
			V: iamdomain.RecordVersion, Kind: string(iamdomain.KindPerson),
			Subject: statelog.Subject{
				Kind: string(iamdomain.KindPerson), ID: sweptPerson,
			},
			Op: string(iamdomain.OpStatus), OpID: opIDFor(seq),
		},
		Position: statelog.Position{
			Stream: iamdomain.Domain{}.Stream().Name, Seq: seq,
		},
		Payload: payload, StoredAt: brokerAt,
	}, now)
	r.written++
}

// fillTrail puts n more trail rows in, at ascending positions.
func (r *sweepRigT) fillTrail(n int) {
	r.t.Helper()
	for i := range n {
		r.write(uint64(r.written+i+1), brokerAt)
	}
}

// apply runs one record through this node's applier and returns the rows it
// wrote or deleted.
//
// `now` IS THE BATCH INSTANT AND NOTHING ELSE. StoredAt stays the broker's own
// stamp, identical on every node, because that is what it is: a record carries
// one instant from the broker and every node applying it sees that one. The
// split is the whole point of the skew case — varying both would prove only
// that two different records produce two different states.
func (r *sweepRigT) apply(rec statelog.Record, now time.Time) int {
	r.t.Helper()
	var rows int
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		var err error
		rows, err = r.applier.Apply(r.t.Context(), tx, rec,
			statelog.ApplyOptions{Now: now, StoredAt: rec.StoredAt})
		return err
	}); err != nil {
		r.t.Fatalf("apply: %v", err)
	}
	return rows
}

// trail is every trail row's id, in order — the thing two nodes must agree on.
func (r *sweepRigT) trail() []string {
	return r.column(`SELECT id FROM iam_history ORDER BY id`)
}

// sweepRecord wraps one sweep payload as the framework delivers it, at the
// version the publisher writes.
func sweepRecord(t *testing.T, sweep iamdomain.Sweep) statelog.Record {
	t.Helper()
	return sweepRecordAt(t, iamdomain.SweepRecordVersion, sweep)
}

// sweepRecordAt is [sweepRecord] at a record version of the case's choosing —
// the version a sweep already on the log may have been written at.
func sweepRecordAt(t *testing.T, version int, sweep iamdomain.Sweep) statelog.Record {
	t.Helper()
	subject := iamdomain.SweepSubject(sweep.Bucket)
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: version, OpID: "sweep-" + subject.ID,
			Subject: subject, Op: iamdomain.OpSweep, Writer: "node-a",
			Scope: iamdomain.BucketScope(sweep.Bucket % iamdomain.Buckets),
		},
		Actor: "engine", ActorKind: iam.KindEngine,
		Mutation: mustJSON(t, sweep),
	})
	if err != nil {
		t.Fatalf("encode a sweep: %v", err)
	}
	return statelog.Record{
		Envelope: statelog.Envelope{
			V: version, Kind: string(iamdomain.KindSweep),
			Subject: statelog.Subject{
				Kind: string(iamdomain.KindSweep), ID: subject.ID,
			},
			Op: string(iamdomain.OpSweep), OpID: "sweep-" + subject.ID,
		},
		Position: statelog.Position{
			Stream: iamdomain.Domain{}.Stream().Name, Seq: 1 << 30,
		},
		Payload: payload, StoredAt: brokerAt,
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func opIDFor(seq uint64) string {
	return "op-" + string(rune('a'+int(seq%26))) + itoa(int(seq))
}

func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = context.Background

// A VERSION-2 SWEEP COLLECTS WHAT WAS SPENT, from the rows AND the document.
//
// Version 1 kept every redeemed invitation — with the sealed address it was
// for — every redeemed bootstrap code, and every revoked or expired credential,
// verifier and all, for the life of the company. Version 2 collects each a week
// past the moment it stopped being presentable, which the record's own instant
// says; anything that stopped more recently, and anything still presentable,
// stays.
//
// A CREDENTIAL IS COLLECTED FROM THE PERSON'S DOCUMENT TOO, and the last step
// is why: credentials travel whole on the document and the rows are derived
// from it, so the next change to somebody's credentials — which forms the new
// set from the document — would republish a credential a sweep had deleted only
// from the rows.
//
// And the CONTROL is the same record at version 1, which is how every sweep
// already on the log was written: replayed, it must collect exactly what
// version 1 said, or a node rebuilding from the log holds different rows from a
// node that applied it live.
func TestAVersionTwoSweepCollectsWhatWasSpent(t *testing.T) {
	t.Parallel()
	// THE PUBLISHER'S OWN CLOCK IS brokerAt, so its collection instant is a
	// week before it; "long ago" is past that by more than the slack, which
	// is what makes the bucket due at all.
	cutoff := brokerAt.Add(-iamdomain.SessionRowGrace)
	long, recent := cutoff.Add(-2*iamdomain.SweepSlack), cutoff.Add(time.Hour)
	credentials := []iamdomain.Credential{
		{V: iamdomain.DocumentVersion, ID: "revoked-long-ago",
			Method: iamdomain.MethodToken, Verifier: "h1", RevokedAt: long},
		{V: iamdomain.DocumentVersion, ID: "expired-long-ago",
			Method: iamdomain.MethodToken, Verifier: "h2", ExpiresAt: long},
		{V: iamdomain.DocumentVersion, ID: "revoked-recently",
			Method: iamdomain.MethodToken, Verifier: "h3", RevokedAt: recent},
		{V: iamdomain.DocumentVersion, ID: "live-token",
			Method: iamdomain.MethodToken, Verifier: "h4",
			ExpiresAt: brokerAt.Add(30 * 24 * time.Hour)},
		{V: iamdomain.DocumentVersion, ID: "password",
			Method: iamdomain.MethodPassword, Verifier: "argon"},
	}
	spent := func(t *testing.T) (*sweepRigT, string) {
		t.Helper()
		rig := &sweepRigT{writeRig: newWriteRig(t)}
		person := uuid.Must(uuid.NewV7()).String()
		if err := rig.enrol(iamdomain.Enrolment{
			PersonID: person, Kind: iam.KindMachine, Stage: iam.StageActive,
			Name: "Release pipeline", Login: "release:pipeline",
			OpID: "enrol-pipeline", Reason: "a pipeline",
		}); err != nil {
			t.Fatalf("enrol: %v", err)
		}
		if err := rig.during(func() error {
			_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
				PersonID: person, OpID: "tokens",
				Apply: func([]iamdomain.Credential) []iamdomain.Credential {
					return credentials
				},
			})
			return err
		}); err != nil {
			t.Fatalf("SetCredentials: %v", err)
		}
		bucket := int64(iamdomain.BucketOf(person))
		if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			for id, redeemed := range map[string]time.Time{
				"spent-long-ago": long, "spent-recently": recent,
			} {
				if _, err := tx.ExecContext(t.Context(), `
					INSERT INTO iam_invites
						(id, email_blind, redeemed_at, bucket, created_at,
						 version, document)
					VALUES (?, ?, ?, ?, 1, 1, x'')`, "invite-"+id, "blind-"+id,
					redeemed.UnixMilli(), bucket); err != nil {
					return err
				}
				if _, err := tx.ExecContext(t.Context(), `
					INSERT INTO iam_bootstrap_codes
						(id, verifier, redeemed_at, bucket, created_at,
						 version, document)
					VALUES (?, x'00', ?, ?, 1, 1, x'')`, "code-"+id,
					redeemed.UnixMilli(), bucket); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("plant the spent rows: %v", err)
		}
		return rig, person
	}
	sweep := func(person string) iamdomain.Sweep {
		return iamdomain.Sweep{V: iamdomain.DocumentVersion,
			Bucket: iamdomain.BucketOf(person), Expired: cutoff}
	}
	held := func(rig *sweepRigT, person string) (rows, document []string) {
		rows = rig.column(`SELECT id FROM iam_credentials WHERE person_id = ?
			ORDER BY id`, person)
		raw := rig.column(`SELECT document FROM iam_people WHERE id = ?`, person)
		if len(raw) != 1 {
			rig.t.Fatalf("person %s has %d rows", person, len(raw))
		}
		doc, err := iamdomain.DecodePerson([]byte(raw[0]))
		if err != nil {
			rig.t.Fatalf("decode the person: %v", err)
		}
		for _, c := range doc.Credentials {
			document = append(document, c.ID)
		}
		slices.Sort(document)
		return rows, document
	}
	everything := []string{"expired-long-ago", "live-token", "password",
		"revoked-long-ago", "revoked-recently"}
	kept := []string{"live-token", "password", "revoked-recently"}

	t.Run("version 1 collects none of it", func(t *testing.T) {
		t.Parallel()
		rig, person := spent(t)
		rig.apply(sweepRecordAt(t, iamdomain.BaseRecordVersion, sweep(person)),
			brokerAt)
		rows, document := held(rig, person)
		if !slices.Equal(rows, everything) || !slices.Equal(document, everything) {
			t.Errorf("a version-1 sweep left rows %v and document %v, want every "+
				"credential in both — replay would diverge from live", rows, document)
		}
		if got := rig.column(`SELECT id FROM iam_invites ORDER BY id`); len(got) != 2 {
			t.Errorf("a version-1 sweep collected a redeemed invitation: %v", got)
		}
	})

	t.Run("version 2 collects what was spent a week ago", func(t *testing.T) {
		t.Parallel()
		rig, person := spent(t)
		// THROUGH THE PUBLISHER, so the record is the one a node writes and
		// sits at a real position on the log the next write follows.
		var report iamdomain.SweepReport
		if err := rig.during(func() error {
			var err error
			report, err = rig.sweeper(brokerAt).Sweep(t.Context(), defaultHorizons)
			return err
		}); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		rig.drain()
		if !slices.Equal(report.Published, []iamdomain.Bucket{iamdomain.BucketOf(person)}) {
			t.Fatalf("the tick published %v, want the one bucket holding what was "+
				"spent", report.Published)
		}
		at, err := rig.log.End(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		rows, document := held(rig, person)
		if !slices.Equal(rows, kept) {
			t.Errorf("the credential rows are %v, want %v", rows, kept)
		}
		if !slices.Equal(document, kept) {
			t.Errorf("the person's document lists %v, want %v — the next change "+
				"to their credentials would republish what the sweep deleted",
				document, kept)
		}
		for table, want := range map[string][]string{
			"iam_invites":         {"invite-spent-recently"},
			"iam_bootstrap_codes": {"code-spent-recently"},
		} {
			if got := rig.column(`SELECT id FROM ` + table + ` ORDER BY id`); !slices.Equal(got, want) {
				t.Errorf("%s holds %v, want %v", table, got, want)
			}
		}
		// SCOPED, NOT VERSIONED: the person subject's own expectation is
		// its last record, which a sweep on the bucket's subject is not.
		stamped := rig.column(`SELECT scoped_through FROM iam_people WHERE id = ?`,
			person)
		if len(stamped) != 1 || stamped[0] != strconv.FormatUint(at, 10) {
			t.Errorf("the person's scoped_through is %v, want the sweep's position %d",
				stamped, at)
		}

		// AND THE NEXT CHANGE TO THEIR CREDENTIALS resurrects nothing.
		if err := rig.during(func() error {
			_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
				PersonID: person, OpID: "one-more",
				Apply: func(current []iamdomain.Credential) []iamdomain.Credential {
					return append(current, iamdomain.Credential{
						V: iamdomain.DocumentVersion, ID: "new-token",
						Method: iamdomain.MethodToken, Verifier: "h5",
					})
				},
			})
			return err
		}); err != nil {
			t.Fatalf("SetCredentials after the sweep: %v", err)
		}
		rows, _ = held(rig, person)
		want := append(slices.Clone(kept), "new-token")
		slices.Sort(want)
		if !slices.Equal(rows, want) {
			t.Errorf("after the next credential change the rows are %v, want "+
				"%v — a swept credential came back", rows, want)
		}
	})
}

// THE PUBLISHER WRITES A SWEEP AT THE VERSION THAT STATES ITS PREDICATE, and
// finds a bucket due for what only that version collects.
//
// Written at the base version, a sweep's new clauses would be evaluated by the
// nodes that know them and skipped by the ones that do not — the same record
// deleting different rows on different nodes. And a bucket whose only due row
// is a credential revoked a week and a day ago is due: were the publisher's
// probes version 1's, it would never publish the record that collects it.
func TestTheSweepPublisherWritesTheVersionThatStatesItsPredicate(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindMachine, Stage: iam.StageActive,
		Name: "Release pipeline", Login: "release:pipeline",
		OpID: "enrol-pipeline", Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	wall := time.Now().UTC()
	if err := rig.during(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: person, OpID: "revoke",
			Apply: func([]iamdomain.Credential) []iamdomain.Credential {
				return []iamdomain.Credential{{V: iamdomain.DocumentVersion,
					ID: "revoked", Method: iamdomain.MethodToken, Verifier: "h",
					RevokedAt: wall}}
			},
		})
		return err
	}); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	later := rig.sweeper(wall.Add(iamdomain.SessionRowGrace + iamdomain.SweepSlack +
		time.Hour))
	var report iamdomain.SweepReport
	if err := rig.during(func() error {
		var err error
		report, err = later.Sweep(t.Context(), defaultHorizons)
		return err
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want := iamdomain.BucketOf(person)
	if !slices.Equal(report.Published, []iamdomain.Bucket{want}) {
		t.Fatalf("a week and a day past a revocation the tick published %v, "+
			"want the revoked token's bucket %s", report.Published, want)
	}
	rig.drain()
	if env := rig.lastEnvelope(); env.Op != iamdomain.OpSweep ||
		env.V != iamdomain.SweepRecordVersion {
		t.Errorf("the sweep was written as op %s at version %d, want a sweep at "+
			"%d", env.Op, env.V, iamdomain.SweepRecordVersion)
	}
	if got := rig.column(`SELECT id FROM iam_credentials WHERE person_id = ?`,
		person); len(got) != 0 {
		t.Errorf("the revoked token outlived the sweep that was due for it: %v", got)
	}
}

// lastEnvelope is the newest record on the rig's log, as every build reads it.
func (r *writeRig) lastEnvelope() iamdomain.RecordEnvelope {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v (present %v)", last, err, ok)
	}
	body, verdict := r.verifier.Open(payload)
	if verdict != statelog.Verified {
		r.t.Fatalf("record %d did not verify: %s", last, verdict)
	}
	env, err := iamdomain.DecodeEnvelope(body)
	if err != nil {
		r.t.Fatalf("decode the envelope: %v", err)
	}
	return env
}
