package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PUBLISHER HALF OF THE SWEEP: what a sweep record says, and when one is
// published at all.
//
// sweep.go is the half every node runs. This is the half ONE node runs, under
// the fleet-singleton duty the engine arms, and it is the only place in the
// whole retention path that reads a clock — once per tick, here, and never at
// apply. What it reads the clock FOR is to turn "ninety days" into a POSITION:
// the newest trail row of a class whose broker instant is older than the
// horizon, read off this node's own rows through the index the migration ships
// for exactly that (`iam_history_class_idx`). From then on only positions
// travel, so two nodes whose clocks disagree by a year delete the same rows.
//
// # Resolved once, published per bucket
//
// The positions and the collection instant are resolved ONCE for the tick and
// every bucket's record carries the same three values. Resolving per bucket
// would be sixty-four reads of a clock that moves between them, and the
// records of one tick would disagree about what "ninety days ago" was.
//
// # A bucket with nothing due publishes nothing, and "due" has SLACK
//
// A sweep record is a record on the log every node applies, the snapshot
// carries and the trim has to wait for. Published unconditionally it is
// sixty-four records a tick — six thousand a day at an hourly tick, which is
// more than the rest of this domain writes at the reference company and would
// fill the log's ceiling with housekeeping. Published whenever ANY row was past
// its horizon it is nearly as bad on a busy company, because some row in most
// buckets crosses its horizon every hour.
//
// So a bucket is DUE only when something in it is at least [SweepSlack] past
// its horizon, and the record it then gets deletes EVERYTHING past the horizon.
// After it lands the bucket's oldest remaining row is younger than the horizon,
// so the bucket cannot be due again for at least the slack — which bounds this
// publisher at sixty-four records a day however busy the company is, and needs
// no memory of when it last ran. That last property is the one a duty needs:
// the lease moves between nodes, and nothing the previous holder remembered
// moves with it.

// SessionRowGrace is how long a row nobody can present is kept past the moment
// it stopped being presentable: a session past its absolute deadline or ended,
// an invitation or a bootstrap code past its expiry.
//
// A WEEK, which is the invitation horizon and the default absolute session
// lifetime. The row is kept for the sessions screen and an investigation to say
// what ended and why ("ended by reuse detection" is the sentence somebody is
// looking for); past a week the session's own trail row — kept for the session
// horizon, ninety days by default — is the durable account of it, and the row
// itself is a thing nobody can present answering a question nobody is asking.
const SessionRowGrace = 7 * 24 * time.Hour

// SweepSlack is how far past its horizon a bucket's oldest row must be before
// the bucket is swept.
//
// A DAY. It is what bounds this publisher's own volume at one record per
// bucket per day — see the header — and what it costs is precision: a row is
// kept at most a day longer than its horizon says. Against horizons of ninety
// and four hundred days that is about one percent at the worst, and against the
// week of [SessionRowGrace] it is a day on a row nobody can present. A shorter
// slack buys a tighter retention with more records on the log; a day is where
// the log cost stops being noticeable beside the domain's own traffic.
const SweepSlack = 24 * time.Hour

// sweepReason is what every sweep record says about itself.
const sweepReason = "retention sweep"

// Horizons is how long the authentication trail keeps each class of row, as
// Tier A's `api.audit` configures it.
type Horizons struct {
	// Changes is how long a grant, status, credential, claim or removal
	// row is kept.
	Changes time.Duration

	// Sessions is how long a sign-in and a sign-out are kept.
	Sessions time.Duration
}

// validate refuses a horizon that would delete the trail it describes.
//
// A ZERO IS REFUSED rather than read as "keep nothing": config validates both
// against their floors and fills the defaults, so a zero here is a caller that
// built the value by hand and forgot a field — and the reading that would
// silently follow is that every row is past its horizon.
func (h Horizons) validate() error {
	if h.Changes <= 0 || h.Sessions <= 0 {
		return fmt.Errorf("iamdomain: a sweep needs both trail horizons, and "+
			"was handed changes=%s sessions=%s — a zero horizon would read "+
			"every row as past it and empty the authentication trail",
			h.Changes, h.Sessions)
	}
	return nil
}

// SweepPlan is one tick's resolution: the three values every due bucket's
// record carries, and which buckets are due.
type SweepPlan struct {
	// Changes and Sessions are the exclusive POSITIONS below which a trail
	// row of that class is past its horizon. Zero is "nothing old enough",
	// which is the ordinary state of a company younger than its horizon.
	Changes  uint64
	Sessions uint64

	// Expired is the instant before which a session, an invitation or a
	// bootstrap code that is over is collected: now less
	// [SessionRowGrace], read once, here.
	Expired time.Time

	// Due are the buckets holding something at least [SweepSlack] past its
	// horizon, in bucket order.
	//
	// A plan is only as current as the rows it was read from, and that is
	// safe in the one direction it can be wrong: a node that is behind
	// resolves LOWER positions and finds fewer buckets due, which deletes
	// less and never more.
	Due []Bucket
}

// SweepReport is what one tick published.
type SweepReport struct {
	Plan SweepPlan

	// Published are the buckets whose record is DURABLE — applied here or
	// pending here, which every node will apply either way.
	Published []Bucket

	// Unknown are the buckets whose publish established nothing. They are
	// not retried under a fresh id here: the next tick re-plans them, and a
	// record that did land after all is idempotent, since it deletes by a
	// predicate rather than by a list.
	Unknown []Bucket
}

// PlanSweep resolves one tick's horizons against this node's rows.
//
// ONE READ TRANSACTION, so the positions, the instant and the due set describe
// one state rather than three.
func (w *Writer) PlanSweep(ctx context.Context, horizons Horizons) (SweepPlan, error) {
	if err := horizons.validate(); err != nil {
		return SweepPlan{}, err
	}
	// THE CLOCK, READ ONCE. Milliseconds are the estate's own resolution,
	// so an instant carried at finer grain would round differently in the
	// comparison than it reads in the record.
	now := w.Now().UTC().Truncate(time.Millisecond)
	plan := SweepPlan{Expired: now.Add(-SessionRowGrace)}
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		if plan.Changes, err = trailBound(ctx, tx, ClassChange,
			now.Add(-horizons.Changes)); err != nil {
			return err
		}
		if plan.Sessions, err = trailBound(ctx, tx, ClassSession,
			now.Add(-horizons.Sessions)); err != nil {
			return err
		}
		// THE DUE BOUNDS ARE THE SAME QUESTION ASKED A SLACK EARLIER,
		// resolved to positions the same way, so "is anything in this
		// bucket a day past its horizon" is a seek on the index the
		// range delete itself uses.
		dueChanges, err := trailBound(ctx, tx, ClassChange,
			now.Add(-horizons.Changes-SweepSlack))
		if err != nil {
			return err
		}
		dueSessions, err := trailBound(ctx, tx, ClassSession,
			now.Add(-horizons.Sessions-SweepSlack))
		if err != nil {
			return err
		}
		dueOver := millis(plan.Expired.Add(-SweepSlack))
		for b := range Bucket(Buckets) {
			due, err := bucketDue(ctx, tx, b, dueChanges, dueSessions, dueOver)
			if err != nil {
				return err
			}
			if due {
				plan.Due = append(plan.Due, b)
			}
		}
		return nil
	})
	if err != nil {
		return SweepPlan{}, fmt.Errorf("iamdomain: plan the retention sweep: %w", err)
	}
	return plan, nil
}

// trailBound is the exclusive position below which a class's trail rows are
// older than cutoff, or zero when none is.
//
// THE NEWEST ROW BELOW THE AGE, read through (class, broker_at) — a seek and a
// step rather than a scan of everything old. Its position PLUS ONE is the
// bound, because the sweep deletes `version < bound` and the row that defined
// it is itself past the horizon.
func trailBound(ctx context.Context, tx *sql.Tx, class HistoryClass,
	cutoff time.Time) (uint64, error) {

	var version int64
	err := tx.QueryRowContext(ctx, `
		SELECT version FROM iam_history
		WHERE class = ? AND broker_at < ?
		ORDER BY broker_at DESC LIMIT 1`,
		string(class), cutoff.UnixMilli()).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("iamdomain: resolve the %s horizon to a position: %w",
			class, err)
	}
	return uint64(version) + 1, nil
}

// bucketDue reports whether one bucket holds anything at least a slack past
// its horizon.
//
// EVERY PREDICATE IS THE APPLY'S OWN, at the slack's bound rather than the
// horizon's, and each is a seek on the index its delete uses — so this costs
// six probes a bucket and never reads a row it will not report.
func bucketDue(ctx context.Context, tx *sql.Tx, b Bucket, changes, sessions uint64,
	over int64) (bool, error) {

	bucket := int64(b)
	probes := []struct {
		what  string
		skip  bool
		query string
		args  []any
	}{
		{"the change trail", changes == 0, `
			SELECT 1 FROM iam_history
			WHERE bucket = ? AND class = ? AND version < ? LIMIT 1`,
			[]any{bucket, string(ClassChange), int64(changes)}},
		{"the session trail", sessions == 0, `
			SELECT 1 FROM iam_history
			WHERE bucket = ? AND class = ? AND version < ? LIMIT 1`,
			[]any{bucket, string(ClassSession), int64(sessions)}},
		{"ended sessions", over <= 0, `
			SELECT 1 FROM iam_sessions
			WHERE bucket = ? AND ended_at > 0 AND ended_at < ? LIMIT 1`,
			[]any{bucket, over}},
		{"expired sessions", over <= 0, `
			SELECT 1 FROM iam_sessions
			WHERE bucket = ? AND ended_at = 0
			  AND absolute_expires_at > 0 AND absolute_expires_at < ? LIMIT 1`,
			[]any{bucket, over}},
		{"invitations", over <= 0, `
			SELECT 1 FROM iam_invites
			WHERE bucket = ? AND redeemed_at = 0
			  AND expires_at > 0 AND expires_at < ? LIMIT 1`,
			[]any{bucket, over}},
		{"bootstrap codes", over <= 0, `
			SELECT 1 FROM iam_bootstrap_codes
			WHERE bucket = ? AND redeemed_at = 0
			  AND expires_at > 0 AND expires_at < ? LIMIT 1`,
			[]any{bucket, over}},
	}
	for _, probe := range probes {
		if probe.skip {
			continue
		}
		var one int
		err := tx.QueryRowContext(ctx, probe.query, probe.args...).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return false, fmt.Errorf("iamdomain: is %s in bucket %s due: %w",
				probe.what, b, err)
		}
		return true, nil
	}
	return false, nil
}

// Sweep plans one tick and publishes a record for every bucket that is due.
//
// THE DEPLOYMENT'S GESTURE, and it takes the deployment's grant: a sweep
// deletes the authentication trail, and the party that decides how long that
// trail is kept is whoever runs the deployment — `api.audit` is Tier A for the
// same reason. The node's own writer holds it; no person's does unless they
// were given the whole of `fleet:operate`.
//
// EVERY BUCKET IS ATTEMPTED even when one fails, for the maintenance sweep's
// reason: the buckets are independent, and one unreachable round must not stop
// the other sixty-three.
func (w *Writer) Sweep(ctx context.Context, horizons Horizons) (SweepReport, error) {
	if err := w.mayOperate(OpSweep); err != nil {
		return SweepReport{}, err
	}
	plan, err := w.PlanSweep(ctx, horizons)
	if err != nil {
		return SweepReport{}, err
	}
	report := SweepReport{Plan: plan}
	var errs []error
	for _, b := range plan.Due {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		result, err := w.sweepBucket(ctx, plan, b)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("bucket %s: %w", b, err))
		case result.Outcome == statelog.OutcomeUnknown:
			report.Unknown = append(report.Unknown, b)
		default:
			report.Published = append(report.Published, b)
		}
	}
	return report, errors.Join(errs...)
}

// sweepBucket publishes one bucket's record.
//
// ITS OPERATION ID IS ITS CONTENT — the bucket and the three values — so a
// retry of the same plan collapses at the broker and in the ledger, and a later
// tick's record, which says something different, is a different operation.
func (w *Writer) sweepBucket(ctx context.Context, plan SweepPlan, b Bucket) (
	statelog.Result, error) {

	mutation, err := EncodeSweep(Sweep{
		V: DocumentVersion, Bucket: b,
		Changes: plan.Changes, Sessions: plan.Sessions, Expired: plan.Expired,
	})
	if err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.record(SweepSubject(b), OpSweep, "", BucketScope(b),
		mutation, sweepReason)
	if err != nil {
		return statelog.Result{}, err
	}
	opID := fmt.Sprintf("sweep:%s:%d:%d:%d", b, plan.Changes, plan.Sessions,
		plan.Expired.UnixMilli())
	return w.publish(ctx, w.request(&rec, opID, statelog.PatternArbitrated, nil))
}

// mayOperate refuses a record only the deployment may publish.
//
// `fleet:operate` AND NOT [AdminGrant], because what these records decide is
// how long the estate keeps its own history — a fact about the deployment, set
// in Tier A, which administering people confers no say over.
func (w *Writer) mayOperate(op OpKind) error {
	if w.Can(iam.GrantFleetOperate) {
		return nil
	}
	return fmt.Errorf("%w: %s requires %s, and this party holds %v", ErrRefused,
		op, iam.GrantFleetOperate, w.Grants)
}
