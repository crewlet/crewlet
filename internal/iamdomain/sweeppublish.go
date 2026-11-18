package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PUBLISHER HALF OF THE SWEEP (sweep.go is the half every data node runs),
// run by ONE node under the fleet-singleton duty. It is the only place on the
// retention path that reads a clock, once per tick, to turn a horizon into a
// POSITION: the newest trail row of a class older than the horizon, through
// `iam_history_class_idx`. Only positions travel, so nodes whose clocks disagree
// delete the same rows, and every bucket's record of a tick carries the same
// values rather than sixty-four reads of a moving clock.
//
// A bucket is DUE only when something in it is at least [SweepSlack] past its
// horizon, and its record then deletes everything past the horizon. That bounds
// the publisher at one record per bucket per day however busy the company, with
// no memory of the last run, which a duty's moving lease would lose.

// SessionRowGrace is how long a row nobody can present is kept past the moment
// it stopped being presentable: a session past its absolute deadline or ended,
// an invitation past its expiry or redeemed, a credential past its expiry or
// revoked.
//
// A WEEK (the design's 168 hours): the invitation horizon and the default
// absolute session lifetime. It lets the sessions screen and an investigation
// say what ended or was revoked and why, and keeps a token a pipeline still
// presents from being collected under a clock skew or a paused duty; past it,
// the row's trail entry, kept for its own horizon, is the durable account.
const SessionRowGrace = 7 * 24 * time.Hour

// SweepSlack is how far past its horizon a bucket's oldest row must be before
// the bucket is swept.
//
// A DAY bounds this publisher at one record per bucket per day (this file's
// header), at the cost of keeping a row at most a day past its horizon: about
// one percent of the ninety- and four-hundred-day horizons. Shorter buys tighter
// retention with more records; at a day the log cost is unnoticeable beside the
// domain's own traffic.
const SweepSlack = 24 * time.Hour

// sweepReason is what every sweep record says about itself.
const sweepReason = "retention sweep"

// Horizons is how long the authentication trail keeps each class of row, as
// Tier A's `api.audit` configures it.
type Horizons struct {
	// Changes is how long a grant, status, credential, login, seat or
	// removal row is kept.
	Changes time.Duration

	// Sessions is how long a sign-in and a sign-out are kept.
	Sessions time.Duration
}

// validate refuses a zero horizon rather than read it as "keep nothing": config
// fills both defaults, so a zero is a hand-built value missing a field, and
// reading it would put every row past its horizon and empty the trail.
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
	// At is the instant this plan read the clock (the tick), which every record
	// it publishes carries in its operation id ([statelog.DeriveOpID]), so a
	// retry of the plan reproduces the same ids and the ledger judges them by
	// when the plan was made.
	At time.Time

	// Changes and Sessions are the exclusive POSITIONS below which a trail
	// row of that class is past its horizon. Zero is "nothing old enough",
	// which is the ordinary state of a company younger than its horizon.
	Changes  uint64
	Sessions uint64

	// Expired is the instant before which a session, an invitation or a
	// credential that is over is collected: now less [SessionRowGrace],
	// read once, here.
	Expired time.Time

	// Due are the buckets holding something at least [SweepSlack] past its
	// horizon, in bucket order. A node that is behind resolves lower positions
	// and finds fewer due, so a stale plan deletes less, never more.
	Due []Bucket
}

// SweepReport is what one tick published.
type SweepReport struct {
	Plan SweepPlan

	// Published are the buckets whose record is DURABLE — applied here or
	// pending here, which every data node will apply either way.
	Published []Bucket

	// Unknown are the buckets whose publish established nothing. The next tick
	// re-plans them; a record that did land after all is idempotent, since it
	// deletes by a predicate rather than by a list.
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
	// THE CLOCK, READ ONCE, at the estate's millisecond resolution, so the
	// comparison rounds as the record reads.
	now := w.Now().UTC().Truncate(time.Millisecond)
	plan := SweepPlan{At: now, Expired: now.Add(-SessionRowGrace)}
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if plan.Changes, err = trailBound(ctx, tx, ClassChange,
			now.Add(-horizons.Changes)); err != nil {
			return err
		}
		if plan.Sessions, err = trailBound(ctx, tx, ClassSession,
			now.Add(-horizons.Sessions)); err != nil {
			return err
		}
		// THE DUE BOUNDS ARE THE SAME QUESTION ASKED A SLACK EARLIER, so each
		// probe seeks the index the range delete uses.
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

// trailBoundStatement is the position of a class's newest trail row below an
// instant, sought through (class, broker_at).
const trailBoundStatement = `
	SELECT version FROM iam_history
	WHERE class = ? AND broker_at < ?
	ORDER BY broker_at DESC LIMIT 1`

// trailBound is the exclusive position below which a class's trail rows are
// older than cutoff, or zero when none is: the newest such row's position PLUS
// ONE, since the sweep deletes `version < bound` and that row is past the
// horizon too.
func trailBound(ctx context.Context, tx *sql.Tx, class HistoryClass,
	cutoff time.Time) (uint64, error) {

	var version int64
	err := tx.QueryRowContext(ctx, trailBoundStatement,
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

// dueTrailStatement is [bucketDue]'s probe of one class of one bucket's trail
// below a position: the sweep's own predicate, a seek of the index its delete
// uses ([sweepTrailStatement]).
const dueTrailStatement = `
	SELECT 1 FROM iam_history
	WHERE bucket = ? AND class = ? AND version < ? LIMIT 1`

// The two probes [bucketDue] asks of one bucket's invitations, each the
// apply's own predicate ([sweepOpenInvitationsStatement],
// [sweepSpentInvitationsStatement]) at the slack's bound, and each a seek of
// the partial index that predicate is served by.
const (
	dueOpenInvitationsStatement = `
		SELECT 1 FROM iam_invites
		WHERE bucket = ? AND redeemed_at = 0 AND expires_at < ? LIMIT 1`
	dueSpentInvitationsStatement = `
		SELECT 1 FROM iam_invites
		WHERE bucket = ? AND redeemed_at > 0 AND redeemed_at < ? LIMIT 1`
)

// bucketDue reports whether one bucket holds anything at least a slack past
// its horizon.
//
// EVERY PREDICATE IS THE APPLY'S OWN at the slack's bound, each a seek on the
// index its delete uses, so no probe reads a row it will not report. The
// invitations' probes are named constants because the plan gate reads every
// statement over the invitations.
func bucketDue(ctx context.Context, tx *sql.Tx, b Bucket, changes, sessions uint64,
	over int64) (bool, error) {

	bucket := int64(b)
	probes := []struct {
		what  string
		skip  bool
		query string
		args  []any
	}{
		{"the change trail", changes == 0, dueTrailStatement,
			[]any{bucket, string(ClassChange), int64(changes)}},
		{"the session trail", sessions == 0, dueTrailStatement,
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
		// An invitation with no expiry included, as the apply collects it.
		{"invitations", over <= 0, dueOpenInvitationsStatement,
			[]any{bucket, over}},
		// AND WHAT WAS SPENT, which a sweep collects too, so a bucket holding
		// only a spent invitation or a revoked token is due.
		{"redeemed invitations", over <= 0, dueSpentInvitationsStatement,
			[]any{bucket, over}},
		{"revoked credentials", over <= 0, `
			SELECT 1 FROM iam_credentials
			WHERE bucket = ? AND revoked_at > 0 AND revoked_at < ? LIMIT 1`,
			[]any{bucket, over}},
		{"expired credentials", over <= 0, `
			SELECT 1 FROM iam_credentials
			WHERE bucket = ? AND expires_at > 0 AND expires_at < ? LIMIT 1`,
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
// It takes admin ([Writer.mayOperate]): how long the authentication trail is
// kept is the deployment's decision, as Tier A's `api.audit` is. EVERY BUCKET
// IS ATTEMPTED even when one fails: they are independent, and one unreachable
// round must not stop the other sixty-three.
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
// ITS OPERATION ID IS DERIVED FROM ITS CONTENT at the plan's instant
// ([statelog.DeriveOpID]): a retry of the same plan collapses at the broker and
// in the ledger, a later tick's record is a different operation, and the id
// carries the instant the ledger vouches for it by. An id without one reads as
// minted at the epoch, and once the ledger's monthly sweep has run is answered
// `unknown` without being published.
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
	opID := statelog.DeriveOpID(plan.At, "sweep-"+b.String(),
		"crewlet.iam.sweep", b.String(),
		strconv.FormatUint(plan.Changes, 10),
		strconv.FormatUint(plan.Sessions, 10),
		strconv.FormatInt(plan.Expired.UnixMilli(), 10))
	return w.publish(ctx, w.request(ctx, &rec, opID, statelog.PatternArbitrated, nil))
}

// mayOperate refuses a record only the deployment may publish: a sweep, an
// eviction, a readmission.
//
// ADMIN, AND UNLIKE [Writer.mayAdminister] A MACHINE TOKEN QUALIFIES: these
// records are facts about the deployment (how long the estate keeps its history,
// which nodes may write it), and evicting a node from a pipeline is what an
// automation is for.
func (w *Writer) mayOperate(op OpKind) error {
	if w.Can(iam.GrantAdmin) {
		return nil
	}
	return needs([]iam.Grant{iam.GrantAdmin}, "only an admin can do that").
		about("%s, by a party holding %s", op, grantWords(w.Grants))
}
