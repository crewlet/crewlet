package iamdomain_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/store"
)

// THE PUBLISHER HALF OF THE SWEEP, through the real broker.
//
// sweep_test.go holds the applier to "a position, never a clock". What it
// cannot hold is the other half: that the POSITIONS come from somewhere a
// second node does not have to agree with — the one publisher, once — and that
// what the publisher sends is what every node then deletes, whatever its own
// clock reads.

// the two horizons every case here sweeps at, which are the shipped defaults.
var defaultHorizons = iamdomain.Horizons{
	Changes:  400 * 24 * time.Hour,
	Sessions: 90 * 24 * time.Hour,
}

// TWO NODES WITH SKEWED CLOCKS DELETE IDENTICAL ROWS: the position, not the
// clock, decides.
//
// Node A publishes the sweep with its clock ninety-five days ahead of the
// broker's — so the session trail is past its horizon and the change trail is
// not — and applies it on a batch clock of 2023. Node B applies the SAME log on
// a batch clock a year later. They must end byte-for-byte alike, and the
// control is that the sweep deleted something at all: an identical pair of
// untouched estates would pass the first half on a publisher that published
// nothing.
func TestTwoNodesWithSkewedClocksDeleteIdenticalRows(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	peer := newFollower(t, "node-b", brokerAt.Add(365*24*time.Hour))

	person := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "enrol-sarah", Reason: "the joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	wall := time.Now().UTC()
	live := rig.openSession(person, wall.Add(365*24*time.Hour))
	ended := rig.openSession(person, wall.Add(365*24*time.Hour))
	rig.closeSession(person, ended, "logout")
	lapsed := rig.openSession(person, wall.Add(time.Hour))

	sweeper := rig.sweeper(wall.Add(95 * 24 * time.Hour))
	var report iamdomain.SweepReport
	if err := rig.during(func() error {
		var err error
		report, err = sweeper.Sweep(t.Context(), defaultHorizons)
		return err
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	peer.follow(rig)

	// THE CONTROL FIRST: something was due, published and deleted.
	if len(report.Published) == 0 {
		t.Fatalf("the sweep published nothing (plan %+v) — the session trail "+
			"is ninety-five days old against a ninety-day horizon", report.Plan)
	}
	for _, node := range []struct {
		name string
		db   *store.DB
	}{{"node-a", rig.db}, {"node-b", peer.db}} {
		if n := countRows(t, node.db, `SELECT COUNT(*) FROM iam_history
			WHERE class = 'session'`); n != 0 {
			t.Errorf("%s kept %d session-trail rows past their horizon", node.name, n)
		}
		if n := countRows(t, node.db, `SELECT COUNT(*) FROM iam_history
			WHERE class = 'change'`); n == 0 {
			t.Errorf("%s swept the change trail, which is a year inside its "+
				"four-hundred-day horizon", node.name)
		}
		lineages := strings.Join(columnOf(t, node.db,
			`SELECT lineage FROM iam_sessions ORDER BY lineage`), ",")
		if lineages != live {
			t.Errorf("%s holds sessions %q, want only the live one %q — the "+
				"ended session (%s) and the one past its deadline (%s) are a week "+
				"past presentable", node.name, lineages, live, ended, lapsed)
		}
	}

	// AND THE TWO NODES AGREE, which is the whole of the claim.
	if a, b := estateOf(t, rig.db), estateOf(t, peer.db); a != b {
		t.Fatalf("two nodes a year apart on their batch clocks hold different "+
			"estates after one sweep:\n node-a: %s\n node-b: %s", a, b)
	}
}

// A BUCKET IS SWEPT ONCE SOMETHING IN IT IS A SLACK PAST ITS HORIZON, and not
// before — which is what bounds this publisher at one record per bucket per day
// however busy the company is.
//
// Twelve hours past the horizon the tick publishes NOTHING: the rows are old
// enough to delete and not old enough to be worth a record. A day and an hour
// past, the one bucket holding them is due and gets exactly one record — and
// planning again at the same instant finds nothing due, because that record
// took everything past the horizon rather than only what was a day past it.
func TestABucketIsSweptOnlyOnceItIsASlackPastItsHorizon(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "enrol-sarah", Reason: "the joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	wall := time.Now().UTC()
	rig.openSession(person, wall.Add(365*24*time.Hour))
	rig.drain()
	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatalf("End: %v", err)
	}

	early := rig.sweeper(wall.Add(defaultHorizons.Sessions + 12*time.Hour))
	var report iamdomain.SweepReport
	if err := rig.during(func() error {
		report, err = early.Sweep(t.Context(), defaultHorizons)
		return err
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.Plan.Sessions == 0 {
		t.Fatal("the plan found no session row past its horizon, so this case " +
			"is not exercising the slack at all")
	}
	if len(report.Plan.Due) != 0 || len(report.Published) != 0 {
		t.Fatalf("twelve hours past the horizon the tick published %v (due %v) — "+
			"a bucket is swept a slack past its horizon, or this publisher "+
			"writes a record per bucket per tick", report.Published, report.Plan.Due)
	}
	if after, _ := rig.log.End(t.Context()); after != end {
		t.Fatalf("the log moved from %d to %d on a tick with nothing due", end, after)
	}

	late := rig.sweeper(wall.Add(defaultHorizons.Sessions + 25*time.Hour))
	if err := rig.during(func() error {
		report, err = late.Sweep(t.Context(), defaultHorizons)
		return err
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want := iamdomain.BucketOf(person)
	if len(report.Published) != 1 || report.Published[0] != want {
		t.Fatalf("a day and an hour past the horizon the tick published %v, "+
			"want exactly the one bucket holding the rows (%s)",
			report.Published, want)
	}
	again, err := late.PlanSweep(t.Context(), defaultHorizons)
	if err != nil {
		t.Fatalf("PlanSweep: %v", err)
	}
	if len(again.Due) != 0 {
		t.Errorf("the bucket is still due (%v) at the instant it was swept — the "+
			"record must take everything past the horizon, or the next tick "+
			"publishes again", again.Due)
	}
}

// THE SWEEP TAKES THE DEPLOYMENT'S GRANT, and administering people is not it.
//
// What a sweep decides is how long the authentication trail is kept, which is
// Tier A's `api.audit` — a fact about the deployment. A party holding only
// people:manage is refused before anything is read; the control is the node's
// own grant, which is what the duty publishes with.
func TestTheSweepTakesTheDeploymentsGrant(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	// NARROWED to people:manage alone: the rig's own party holds every
	// grant, so the gate is exercised by saying which one is missing.
	administrator := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantPeopleManage}))
	_, err := administrator.Sweep(t.Context(), defaultHorizons)
	if !errors.Is(err, iamdomain.ErrRefused) {
		t.Fatalf("a party holding only %s swept the trail (err %v) — whoever "+
			"administers people would decide how long their own changes are "+
			"kept", iam.GrantPeopleManage, err)
	}
	if _, err := rig.sweeper(time.Now()).Sweep(t.Context(), defaultHorizons); err != nil {
		t.Fatalf("the deployment's own writer was refused: %v", err)
	}
}

// A ZERO HORIZON IS REFUSED, never read as "keep nothing".
//
// Config fills both defaults, so a zero here is a caller that built the value
// by hand — and the reading that would follow is that every row is past its
// horizon, which empties the trail on the first tick.
func TestAZeroHorizonIsRefused(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	for _, h := range []iamdomain.Horizons{
		{Changes: defaultHorizons.Changes},
		{Sessions: defaultHorizons.Sessions},
	} {
		if _, err := rig.sweeper(time.Now()).PlanSweep(t.Context(), h); err == nil {
			t.Errorf("horizons %+v planned a sweep", h)
		}
	}
}

// --- helpers ---------------------------------------------------------------- //

// sweeper is this rig's writer acting as the node, with its clock at `now`.
func (r *writeRig) sweeper(now time.Time) *iamdomain.Writer {
	w := r.writer.As(principalNamed("node-a", iam.KindMachine, []iam.Grant{iam.GrantFleetOperate}))
	w.Now = func() time.Time { return now }
	return w
}

// openSession opens one session for a person and applies it.
func (r *writeRig) openSession(person string, expires time.Time) string {
	r.t.Helper()
	lineage := uuid.Must(uuid.NewV7()).String()
	if err := r.during(func() error {
		_, err := r.writer.OpenSession(r.t.Context(), iamdomain.SessionStart{
			Lineage: lineage, Person: person, AbsoluteExpiresAt: expires,
			OpID: "session:" + lineage,
		})
		return err
	}); err != nil {
		r.t.Fatalf("open a session: %v", err)
	}
	return lineage
}

// closeSession ends one of person's sessions and applies it.
func (r *writeRig) closeSession(person, lineage, reason string) {
	r.t.Helper()
	if err := r.during(func() error {
		_, err := r.writer.CloseSession(r.t.Context(), lineage, person, reason,
			"close:"+lineage)
		return err
	}); err != nil {
		r.t.Fatalf("close a session: %v", err)
	}
}

// follower is a second node: its own store and applier, fed the log another
// node writes, on a batch clock of its own.
type follower struct {
	t        *testing.T
	db       *store.DB
	applier  *iamdomain.Applier
	now      time.Time
	consumed uint64
}

func newFollower(t *testing.T, nodeID string, now time.Time) *follower {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open the second node's store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the second node's store: %v", err)
		}
	})
	return &follower{t: t, db: db, applier: iamdomain.NewApplier(nodeID, nil, nil), now: now}
}

// follow applies whatever the rig's log holds past what this node has.
func (f *follower) follow(rig *writeRig) {
	f.t.Helper()
	consumed, err := applyLog(f.t, rig.log, rig.verifier, f.db, f.applier,
		f.consumed, f.now, nil)
	f.consumed = consumed
	if err != nil {
		f.t.Fatalf("the second node could not follow the log: %v", err)
	}
}

// estateOf renders the replicated rows a sweep touches, in key order, as one
// string two nodes either agree on or do not.
func estateOf(t *testing.T, db *store.DB) string {
	t.Helper()
	var out []string
	for _, query := range []string{
		`SELECT id || '|' || class || '|' || bucket || '|' || version
		   FROM iam_history ORDER BY id`,
		`SELECT lineage || '|' || person_id || '|' || ended_at || '|' ||
		        absolute_expires_at || '|' || version
		   FROM iam_sessions ORDER BY lineage`,
		`SELECT id || '|' || stage || '|' || login || '|' || version
		   FROM iam_people ORDER BY id`,
		`SELECT id || '|' || redeemed_at || '|' || expires_at
		   FROM iam_invites ORDER BY id`,
	} {
		out = append(out, strings.Join(columnOf(t, db, query), ";"))
	}
	return strings.Join(out, "\n")
}

// columnOf reads one text column out of a node's replicated estate.
func columnOf(t *testing.T, db *store.DB, query string) []string {
	t.Helper()
	var out []string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), query)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return err
			}
			out = append(out, value)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return out
}

// countRows reads one count out of a node's replicated estate.
func countRows(t *testing.T, db *store.DB, query string) int {
	t.Helper()
	var n int
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), query).Scan(&n)
	}); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}
