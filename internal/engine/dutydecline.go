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
// (ADR-0079). Such a duty decides for the whole fleet, so it asks the verdict
// of every domain it decides from BEFORE it claims ([Engine.copyOutOfService]);
// on a wrong copy it does not claim (nothing withdrawn, purged or deleted) and
// GIVES THE LEASE BACK ([Engine.workerGiveBack]) for a serving node's next tick.
//
// A duty with no undo (the code-retention purge) also declines while this node
// does not serve the fleet's revision ([Engine.servedFleetRevision]); one whose
// rows a later apply corrects keeps running on a node merely behind. A reason
// that clears by itself in seconds (every activation, every boot) is DEBUG and
// asked again at the reconcile cadence ([declineReason.again]); a fault is a
// WARN. A decline is ONE EPISODE: `_resumed` logs at its loudest reason's level.

// declinable is one duty as its decline is announced.
type declinable struct {
	// event prefixes the duty's two lines, `<event>_declined` and
	// `<event>_resumed`.
	event string
	// duty is the duty as a sentence names it.
	duty string
	// hazard is what deciding from a wrong copy would do — why it declines.
	hazard string
	// staleHazard is what judging by a replaced revision would do, for a duty
	// that waits on the fleet's revision ([revisionDeclineReason]); else empty.
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
	// key tells reasons apart, so a repeat says nothing new; "" is none.
	key string
	// attrs ride both of the reason's lines.
	attrs []any
	// level is the level of the `_declined` line, and resumed of the
	// `_resumed` line.
	level, resumed slog.Level
	// detail and resumedDetail are the two lines' details.
	detail, resumedDetail string
	// again is how soon a turn declined for a reason that clears by itself
	// asks again; zero is the duty's own interval ([dutyDecline.again]).
	again time.Duration
}

// copyDeclineReason is the reason a duty declines while copies
// ([Engine.copyOutOfService]) finds one of domains out of service, and that
// domain; none for a nil copies, a node that judges no copy of its own.
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

// revisionDeclineReason is the reason a duty with no undo declines while this
// node does not serve the revision the fleet's activation pointer names
// (served is [Engine.servedFleetRevision]); none for a nil served. Told apart
// by whether it is a FAULT:
//
//   - BEHIND, a WARN naming both epochs: the lag is CONFIRMED
//     ([configplane.FleetView.LagConfirmed]), the case this exists for;
//   - PROPAGATING, DEBUG, asked again at the reconcile cadence: behind only as
//     every node is while an activation applies, or while booting;
//   - UNKNOWN, a WARN naming why: the pointer could not be read;
//   - NOTHING ACTIVATED, DEBUG, asked again at the reconcile cadence (as
//     `seat_mailboxes_not_judged`): a fresh deployment before its first one.
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

// dutyDecline is the reason one duty is declining for, none while it is not.
// The zero value has declined nothing and logs to the engine's logger.
type dutyDecline struct {
	// log is where the two lines go; nil is the engine's.
	log *slog.Logger

	mu  sync.Mutex
	out declineReason
}

// check reports the first of domains copies ([Engine.copyOutOfService]) finds
// out of service, and whether the duty declines ([dutyDecline.record]).
func (d *dutyDecline) check(ctx context.Context, of declinable,
	copies func(context.Context, ...string) (string, bool), domains []string) (string, bool) {

	reason, domain := copyDeclineReason(ctx, of, copies, domains)
	return domain, d.record(ctx, of, reason)
}

// record keeps reason as the duty's on this turn, and reports whether it
// declines. It logs ON A CHANGE ONLY: when the duty starts declining or its
// reason changes, and when it stops, naming the reason it waited on.
func (d *dutyDecline) record(ctx context.Context, of declinable, reason declineReason) bool {
	d.mu.Lock()
	was := d.out
	if reason.key != "" && was.key != "" {
		// ONE EPISODE: `_resumed` takes the loudest level any reason
		// asked for, so a WARN's reader also sees the decline end.
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

// again is how soon the duty asks again after a turn declined for a reason
// that clears sooner than its cadence ([declineReason.again]). Zero is no such
// reason: the duty waits its own cadence ([codeRetentionDuty.wait]).
func (d *dutyDecline) again() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.again
}

// giveBack gives the duty back where this node holds it ([Engine.workerGiveBack]),
// on every declining tick, so an unanswered give-back is retried a tick later
// rather than left to the TTL. Nil release gives back nothing (no fleet).
//
// It reports whether the store ANSWERED that this node no longer holds the
// duty, so a duty keeping holder state (the collector's gauges and alarm) can
// drop it as on a "not held" claim. False is no such answer: an error, or a
// node with no lease to give, which runs the duty alone.
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
