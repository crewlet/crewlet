package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// A FLEET DUTY DECLINED BY A NODE WHOSE COPY IT DECIDES FROM IS OUT OF SERVICE
// — ADR-0072.
//
// A fleet-singleton duty that decides from a domain's rows decides for the
// whole fleet. The embedding duty publishes the vectors every node applies,
// and it withdraws the vector of every source its own copy no longer shows:
// on a holder whose tracker copy stopped applying while its vectors copy kept
// up, every task created since has a vector and no row, and the duty withdrew
// those vectors fleet-wide. The trim counts the evictions this node's own rows
// hold, and a holder whose copy applied an eviction and not the readmission
// after it uncounted a node that was back and purged past its position. The
// object collector pins its reads through the tracker's read authority, which
// refuses a copy out of service, so its holder renewed the duty and collected
// nothing for as long as the copy stayed wrong.
//
// So such a duty asks the verdict of every domain it decides from BEFORE it
// claims ([Engine.copyOutOfService]), and a node whose copy of any of them is
// out of service DECLINES: it does not claim, it GIVES THE DUTY'S LEASE BACK
// if it holds it ([Engine.workerGiveBack]), and a node whose copies serve
// takes the duty at its own next tick. Not claiming is the conservative
// direction for every one of them — nothing withdrawn, nothing purged, nothing
// deleted. Left to lapse instead, the lease kept the duty from every node for
// its whole TTL — the trim's 45 minutes, plus a 15-minute tick, with no log
// trimmed — over a fault that does not heal on its own. A single data node
// whose copy is wrong runs none of them until it recovers, which that copy's
// own `estate_copy_not_served` line and alarms already name.
//
// # A second reason, for a duty whose judgement against the revision has no undo
//
// The code-retention duty purges by the revision's code.deleted_retention_days,
// and a purge cannot be undone. So it also declines, in the same way and on
// the same two lines, while this node does not serve the revision the fleet's
// activation pointer names ([Engine.servedFleetRevision]) — the rule the
// mailbox retirement already followed. It is not the posture's shed folded
// into every duty: a duty that only decides from rows a later apply corrects
// keeps running on a node that is merely behind.
//
// # A decline that is no fault
//
// Not every reason is a fault, and the lines say which. A node is behind the
// fleet's revision for a moment at EVERY activation and at EVERY boot — the
// purge duty's first turn runs before `crewlet run` has built the reconciler
// that says which epoch it serves — and a warning for that would be logged at
// every deploy, which teaches whoever reads the log to ignore the one a node
// that failed an apply raises. So a reason that clears by itself in seconds is
// logged at DEBUG, and the duty ASKS AGAIN at the reconcile cadence rather than
// a whole tick later ([declineReason.again], [dutyDecline.again]); a reason that
// says something is wrong is a WARN. And a decline is ONE EPISODE however many
// reasons it passes through: its `_resumed` line is logged at the loudest
// level any of them asked for, so a node that warned and then went quiet on
// its way back still says, at INFO, that it is back.

// declinable is one duty as its decline is announced.
type declinable struct {
	// event prefixes the duty's two lines, `<event>_declined` and
	// `<event>_resumed`.
	event string
	// duty is the duty as a sentence names it.
	duty string
	// hazard is what deciding from a wrong copy would do — why it declines.
	hazard string
	// staleHazard is what judging by a revision the fleet has replaced would
	// do, for a duty that waits for this node to serve the fleet's revision
	// ([revisionDeclineReason]); empty for a duty that does not.
	staleHazard string
}

// The duties that decide from a domain's rows.
var (
	embedDecline = declinable{event: "embed_duty", duty: "the embedding duty",
		hazard: "deciding from it would withdraw or re-embed the fleet's vectors from " +
			"rows the node has stopped serving"}
	retentionDecline = declinable{event: "retention_duty", duty: "the trim",
		hazard: "deciding from it would count the evictions in rows the node has " +
			"stopped serving, and could purge past the position of a node that is back"}
	collectorDecline = declinable{event: "object_collector", duty: "the object collector",
		hazard: "its passes read the rows that name the files through a read the " +
			"copy refuses, so it would hold the duty and collect nothing"}
	codeRetentionDecline = declinable{event: "code_retention_duty", duty: "the code-retention duty",
		hazard: "deciding from it would purge repositories by rows the node has stopped " +
			"serving, and a purge has no undo",
		staleHazard: "a purge judged by the code.deleted_retention_days of a revision the " +
			"fleet has replaced could remove a repository the current one keeps restorable, " +
			"and a purge has no undo"}
)

// declineReason is why a duty declines on one turn. The zero value is no
// reason: the duty runs.
type declineReason struct {
	// key tells one reason from another, so a turn that declines for the
	// reason the last one did says nothing new; "" is no reason.
	key string
	// attrs ride both of the reason's lines.
	attrs []any
	// level is the level of the `_declined` line, and resumed of the
	// `_resumed` line.
	level, resumed slog.Level
	// detail and resumedDetail are the two lines' details.
	detail, resumedDetail string
	// again is how soon a turn declined for this reason asks again, for a
	// reason that clears by itself sooner than the duty's own tick; zero is
	// the duty's interval ([dutyDecline.again]).
	again time.Duration
}

// copyDeclineReason is the reason a duty deciding from domains declines while
// copies — [Engine.copyOutOfService] — finds one of them out of service on
// this node, and that domain; no reason for a nil copies, a node that judges
// no copy of its own.
func copyDeclineReason(ctx context.Context, of declinable,
	copies func(context.Context, ...string) (string, bool), domains []string) (declineReason, string) {

	if copies == nil {
		return declineReason{}, ""
	}
	domain, out := copies(ctx, domains...)
	if !out {
		return declineReason{}, ""
	}
	return declineReason{
		key: "copy:" + domain, attrs: []any{"domain", domain},
		level: slog.LevelWarn, resumed: slog.LevelInfo,
		detail: fmt.Sprintf("this node's copy of %s is out of service, so it does "+
			"not hold %s — %s; it gives the duty back if it holds it, a node whose "+
			"copies serve takes it at its next tick, and this node takes it again "+
			"once its copy is back in service", domain, of.duty, of.hazard),
		resumedDetail: fmt.Sprintf("this node's copy of %s is back in service, so it "+
			"claims %s again", domain, of.duty),
	}, domain
}

// revisionDeclineReason is the reason a duty whose judgement against the active
// revision has no undo declines while this node does not serve the revision
// the fleet's activation pointer names — served is [Engine.servedFleetRevision]
// — and no reason where it does, or for a nil served, a node that judges no
// revision.
//
// Every answer of the helper but "serves it" is a reason, told apart by
// whether it is a FAULT:
//
//   - BEHIND, a WARN naming both epochs: this node's lag on the fleet's epoch
//     is CONFIRMED by the posture's own rule ([configplane.FleetView.LagConfirmed])
//     — an attempt of its own at it failed, or it has been behind on it longer
//     than propagation explains. A node that failed the fleet's apply is what
//     this exists for, and a fleet whose only worker data node is stuck must
//     say why its purges wait;
//   - PROPAGATING, at DEBUG, asked again at the reconcile cadence: behind, but
//     no more than every node is for the seconds an activation takes to apply,
//     or a node booting is until its reconciler has been built and has read
//     the pointer — which the posture reads as waiting too, by the same rule;
//   - UNKNOWN, a WARN naming why: the pointer could not be read;
//   - NOTHING ACTIVATED, at DEBUG and asked again at the reconcile cadence, as
//     the mailbox sweep logs `seat_mailboxes_not_judged`: a fresh deployment
//     meets it for the seconds before the seed's first activation.
func revisionDeclineReason(ctx context.Context, of declinable,
	served func(context.Context) (*Company, error)) declineReason {

	if served == nil {
		return declineReason{}
	}
	_, err := served(ctx)
	var behind *errBehindFleet
	switch {
	case err == nil:
		return declineReason{}
	case errors.Is(err, maintenance.ErrNoActiveRevision):
		return declineReason{
			key: "not_activated", attrs: []any{"reason", "not_activated"},
			level: slog.LevelDebug, resumed: slog.LevelDebug, again: configplane.ReconcileInterval,
			detail: fmt.Sprintf("nothing has been activated yet, so this node does not hold "+
				"%s — it judges only against a revision the fleet has activated; the first "+
				"activation brings it in", of.duty),
			resumedDetail: fmt.Sprintf("a revision has been activated and this node serves "+
				"it, so it claims %s", of.duty),
		}
	case errors.As(err, &behind) && behind.confirmed:
		return declineReason{
			key: "behind", attrs: []any{"reason", "behind",
				"applied_epoch", behind.applied, "fleet_epoch", behind.target},
			level: slog.LevelWarn, resumed: slog.LevelInfo,
			detail: fmt.Sprintf("this node serves activation epoch %d and the fleet is on "+
				"%d, which it has failed to apply or been applying for longer than "+
				"propagation explains, so it does not hold %s — %s; it gives the duty back "+
				"if it holds it, a node serving the fleet's revision takes it at its next "+
				"tick, and this node takes it again once it has applied the fleet's revision",
				behind.applied, behind.target, of.duty, of.staleHazard),
			resumedDetail: fmt.Sprintf("this node serves the fleet's revision again, so it "+
				"claims %s again", of.duty),
		}
	case errors.As(err, &behind):
		attrs := []any{"reason", "propagating", "fleet_epoch", behind.target}
		state := "it is starting, and has not yet reconciled with the activation pointer"
		if behind.reconciled() {
			attrs = append(attrs, "applied_epoch", behind.applied)
			state = fmt.Sprintf("it serves activation epoch %d and is still within "+
				"propagation of the fleet's", behind.applied)
		}
		return declineReason{
			key: "propagating", attrs: attrs,
			level: slog.LevelDebug, resumed: slog.LevelDebug, again: configplane.ReconcileInterval,
			detail: fmt.Sprintf("this node has not yet applied activation epoch %d, the "+
				"fleet's — %s — so it does not hold %s yet; it asks again at the reconcile "+
				"cadence, and warns once the lag is confirmed", behind.target, state, of.duty),
			resumedDetail: fmt.Sprintf("this node serves the fleet's revision, so it claims %s",
				of.duty),
		}
	default:
		return declineReason{
			key: "unknown", attrs: []any{"reason", "unknown", "error", err.Error()},
			level: slog.LevelWarn, resumed: slog.LevelInfo,
			detail: fmt.Sprintf("this node cannot tell whether it serves the revision the "+
				"fleet is on, so it does not hold %s — %s; it gives the duty back if it "+
				"holds it, and takes it again once it can tell that it does",
				of.duty, of.staleHazard),
			resumedDetail: fmt.Sprintf("this node serves the fleet's revision, so it "+
				"claims %s again", of.duty),
		}
	}
}

// dutyDecline is what one duty remembers of its declining: the reason it is
// declining for, none while it is not. The zero value has declined nothing and
// logs to the engine's logger.
type dutyDecline struct {
	// log is where the two lines go; nil is the engine's.
	log *slog.Logger

	mu  sync.Mutex
	out declineReason
}

// check asks copies — [Engine.copyOutOfService] — whether any of domains is
// out of service on this node, and reports the first that is, to decline
// for ([dutyDecline.record]). A nil copies declines nothing — a node that
// judges no copy of its own.
func (d *dutyDecline) check(ctx context.Context, of declinable,
	copies func(context.Context, ...string) (string, bool), domains []string) (string, bool) {

	reason, domain := copyDeclineReason(ctx, of, copies, domains)
	return domain, d.record(ctx, of, reason)
}

// record keeps reason as the duty's on this turn, and reports whether it
// declines. It SAYS SO ON A CHANGE ONLY: a line when the duty starts declining
// or declines for another reason, and one when it stops, naming the reason it
// waited on — so a copy out of service for an hour is one line rather than one
// a tick.
func (d *dutyDecline) record(ctx context.Context, of declinable, reason declineReason) bool {
	d.mu.Lock()
	was := d.out
	if reason.key != "" && was.key != "" {
		// ONE EPISODE: the `_resumed` line that ends it is logged at the
		// loudest level any of its reasons asked for, so a decline that
		// began with a WARN and ends on a reason logged at DEBUG still
		// says, where its WARN was read, that it ended.
		reason.resumed = max(reason.resumed, was.resumed)
	}
	d.out = reason
	d.mu.Unlock()
	logger := cmp.Or(d.log, log)
	switch {
	case reason.key != "" && reason.key != was.key:
		logger.Log(ctx, reason.level, of.event+"_declined",
			slices.Concat(reason.attrs, []any{"detail", reason.detail})...)
	case reason.key == "" && was.key != "":
		logger.Log(ctx, was.resumed, of.event+"_resumed",
			slices.Concat(was.attrs, []any{"detail", was.resumedDetail})...)
	}
	return reason.key != ""
}

// again is how soon the duty asks again when the turn just declined for a
// reason that clears sooner than its own cadence and says when
// ([declineReason.again]) — so a node that was only applying the fleet's
// revision, or only starting, takes the duty a reconcile interval after it
// can rather than a whole tick. Zero is no such reason: the duty waits its
// own cadence, which is the duty's to name ([codeRetentionDuty.wait]) rather
// than a parameter here, since only the duty knows it.
func (d *dutyDecline) again() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.again
}

// giveBack gives the duty back where this node holds it ([Engine.workerGiveBack])
// — on every tick that declines, so a give-back the store did not answer is
// tried again a tick later rather than left to the lease's TTL. Nil release
// gives back nothing: a node with no fleet holds no lease.
//
// It reports whether the store ANSWERED that this node no longer holds the
// duty — it gave the lease back, or held none to give — so a duty that keeps
// state as its holder (the collector's gauges and alarm) can let it go as a
// claim that answered "not held" lets it go. False is no such answer: a
// give-back the store did not answer says nothing about who holds the duty,
// and neither does a node with no lease to give, which runs the duty alone.
func (d *dutyDecline) giveBack(ctx context.Context, of declinable,
	release func(context.Context) (bool, error)) (notHeld bool) {

	if release == nil {
		return false
	}
	logger := cmp.Or(d.log, log)
	released, err := release(ctx)
	switch {
	case err != nil:
		logger.WarnContext(ctx, of.event+"_not_given_back", "err", err,
			"detail", "this node declines "+of.duty+" and could not give its lease "+
				"back, so the next tick tries again — or a node that does not decline it "+
				"takes it once the lease lapses")
		return false
	case released:
		logger.InfoContext(ctx, of.event+"_given_back",
			"detail", "this node declined "+of.duty+" and gave its lease back, so a "+
				"node that does not decline it takes it at its next tick")
	}
	return true
}
