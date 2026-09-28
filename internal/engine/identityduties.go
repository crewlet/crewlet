package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/schedule"
)

// THE IDENTITY DUTIES: two fleet singletons that keep the identity estate
// honest, each on its own lease and its own interval.
//
//   - THE SWEEP PUBLISHER turns `api.audit`'s two horizons into positions and
//     publishes a retention record for every bucket that is due
//     ([iamdomain.Writer.Sweep]). The applier on every node deletes the same
//     rows; nothing else ever deletes from this estate.
//   - THE CLAIM REPORT names a duplicate a restore produced and a reservation
//     an enrolment left behind ([iamdomain.Reader.Claims]). It reports and
//     never repairs: which of two people keeps an address is a decision.
//
// # Two leases rather than one duty with two jobs
//
// They share nothing but the domain. Their costs land on different systems —
// one publishes to the log, the other only reads the local database — and a
// lease flap on one should cost that one a skipped interval rather than both. So each claims its own `worker:` lease through
// [Engine.workerDuty], which also gives each the roles gate and the release on
// a graceful stop that every singleton has.
//
// # Every loop ticks once at start
//
// Unlike the maintenance sweep, which waits a full interval so a rolling
// deploy does not run it once per replica. Here the first tick is the useful
// one: the claim report after a boot is how a restore's duplicate is named the
// moment the restored node is back rather than an hour later; and the one that
// writes — the sweep — is gated on something being due, so a tick that finds
// nothing costs reads and no records.

// identityLog is the duties' own voice.
var identityLog = logging.Get("iam.duty")

// The two duties' lease names.
const (
	identitySweepDuty  = "iam_sweep"
	identityClaimsDuty = "iam_claims"
)

// IdentitySweepInterval is how often the sweep publisher plans.
//
// AN HOUR. What bounds the records it writes is [iamdomain.SweepSlack] — a
// bucket is swept once something in it is a day past its horizon — so the
// interval only decides how soon after that a bucket is swept and how fast a
// backlog converges: an hour past the slack, and four thousand rows a bucket an
// hour, which drains a weekend's lapse at the largest company this estate is
// sized for inside a tick or two. A plan with nothing due is 640 indexed probes
// and no records.
const IdentitySweepInterval = time.Hour

// IdentityClaimsInterval is how often the claim report runs.
//
// AN HOUR. A duplicate arises only from a restore or a reanchor, both of which
// restart the node that performed them — and the first tick at boot is what
// names it then. The interval is the cadence of the WARNING that follows: loud
// enough that a standing duplicate is not forgotten, rare enough that it is
// not the log.
const IdentityClaimsInterval = time.Hour

// identityDutyTTL is a duty's lease: three of its intervals, the ratio every
// singleton here takes, so one slow claim does not hand the duty to a peer and
// a dead holder's duty moves within about three ticks.
//
// EVERY INTERVAL HERE IS AN HOUR OR LESS, so the lease stays inside
// [coord.MaxDutyTTL]'s three hours and the duty claims once per tick.
func identityDutyTTL(every time.Duration) time.Duration { return 3 * every }

// identityDuty is one loop's declaration.
type identityDuty struct {
	name     string
	interval time.Duration
	claim    schedule.DutyFunc
	pass     func(context.Context)
}

// identityDuties is the running loops.
type identityDuties struct {
	duties []identityDuty
	stop   context.CancelFunc
	done   sync.WaitGroup
}

// startIdentityDuties arms every identity duty this node can run.
//
// A NODE THAT RUNS NO WORKERS ARMS NONE, although it runs the identity domain
// as every node does: every duty here is a worker singleton, whose claim that
// node's roles gate refuses on every tick ([Engine.workerDuty]), so arming them
// there would be loops that never run, reported by [Engine.IdentityDuties] as
// duties that do.
func (e *Engine) startIdentityDuties(ctx context.Context, boot *config.Bootstrap) {
	duties := e.identityDutiesFor(boot)
	if len(duties) == 0 {
		return
	}
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	d := &identityDuties{duties: duties, stop: stop}
	for _, duty := range duties {
		d.done.Add(1)
		go func() {
			defer d.done.Done()
			runIdentityDuty(loop, duty)
		}()
	}
	e.identity.Store(d)
	names := make([]string, 0, len(duties))
	for _, duty := range duties {
		names = append(names, duty.name)
	}
	identityLog.InfoContext(ctx, "iam_duties_armed", "duties", names)
}

// IdentityDuties names the identity duties this node armed, with the interval
// each runs at, and nil on a node that armed none.
//
// SERVED ON `GET /health` as `identity_duty_seconds`, because "is the identity
// estate being kept here, and how often" is an operator's question, and a duty
// that was never armed looks from every other vantage point exactly like one
// quietly finding nothing to do. It says what this node WILL run when it holds
// the lease; which node holds each lease is the coordination store's answer.
func (e *Engine) IdentityDuties() map[string]time.Duration {
	if e == nil {
		return nil
	}
	armed := e.identity.Load()
	if armed == nil {
		return nil
	}
	out := make(map[string]time.Duration, len(armed.duties))
	for _, duty := range armed.duties {
		out[duty.name] = duty.interval
	}
	return out
}

// stopIdentityDuties ends every loop, waiting for a tick in flight.
//
// THE ROSTER IS TAKEN DOWN FIRST, so `/health` stops naming duties that are
// ending, and it is a Swap so two stops never both wait on one set of loops.
func (e *Engine) stopIdentityDuties() {
	armed := e.identity.Swap(nil)
	if armed == nil {
		return
	}
	armed.stop()
	armed.done.Wait()
}

// identityDutiesFor declares the duties this node runs, without starting them.
//
// SEPARATE FROM THE START so a case can hold the arming decision — which
// duties, at which interval — without running a loop.
func (e *Engine) identityDutiesFor(boot *config.Bootstrap) []identityDuty {
	if e.native == nil || boot == nil || !e.profile.RunsWorkers() {
		return nil
	}
	reader, writer := e.native.iamReader, e.native.iamWriter
	horizons := iamdomain.Horizons{
		Changes:  boot.API.Auth.Audit.Changes(),
		Sessions: boot.API.Auth.Audit.Sessions(),
	}
	duty := func(name string, interval time.Duration, pass func(context.Context)) identityDuty {
		return identityDuty{name: name, interval: interval,
			claim: e.workerDuty(name, identityDutyTTL(interval)), pass: pass}
	}

	return []identityDuty{
		duty(identitySweepDuty, IdentitySweepInterval, func(ctx context.Context) {
			sweepPass(ctx, writer, horizons)
		}),
		duty(identityClaimsDuty, IdentityClaimsInterval, func(ctx context.Context) {
			claimsPass(ctx, reader)
		}),
	}
}

// runIdentityDuty ticks one duty until the context ends: a claim of its lease
// every interval, and the pass on every claim this node holds.
func runIdentityDuty(ctx context.Context, duty identityDuty) {
	ticker := time.NewTicker(duty.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if mine(ctx, duty) {
			duty.pass(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// mine claims a duty for one tick. Nil is the single-node answer: nobody to
// be a singleton among, so always this node's.
func mine(ctx context.Context, duty identityDuty) bool {
	if duty.claim == nil {
		return true
	}
	held, err := duty.claim(ctx)
	if err != nil {
		identityLog.WarnContext(ctx, "iam_duty_unclaimed", "duty", duty.name,
			"error", err.Error())
		return false
	}
	return held
}

// sweepPass publishes one tick of the retention sweep.
func sweepPass(ctx context.Context, writer *iamdomain.Writer, horizons iamdomain.Horizons) {
	report, err := writer.Sweep(ctx, horizons)
	if err != nil {
		identityLog.WarnContext(ctx, "iam_sweep_failed", "error", err.Error(),
			"published", len(report.Published), "unknown", len(report.Unknown))
		return
	}
	if len(report.Published) > 0 || len(report.Unknown) > 0 {
		identityLog.InfoContext(ctx, "iam_sweep_published",
			"buckets", len(report.Published), "unknown", len(report.Unknown),
			"changes_below", report.Plan.Changes,
			"sessions_below", report.Plan.Sessions,
			"expired_before", report.Plan.Expired)
	}
}

// claimsPass says every duplicate and orphan this node's rows hold.
//
// A WARNING PER FINDING, every tick it stands: each is something an operator
// has to decide, and a report that said it once would be forgotten by the
// person who was not looking at the log that hour.
func claimsPass(ctx context.Context, reader *iamdomain.Reader) {
	report, err := reader.Claims(ctx, time.Now())
	if err != nil {
		identityLog.WarnContext(ctx, "iam_claims_unreadable", "error", err.Error())
		return
	}
	for _, dup := range report.Duplicates {
		identityLog.WarnContext(ctx, "iam_claim_duplicated",
			duplicateAttrs(dup, report.At.String())...)
	}
	for _, orphan := range report.Orphans {
		identityLog.WarnContext(ctx, "iam_claim_orphaned",
			"person", orphan.Person, "holds", orphan.Holds, "since", orphan.Since,
			"detail", "an enrolment stopped after taking these claims; "+
				"`crewlet iam remove` on this id releases them")
	}
}

// duplicateAttrs is what one duplicated claim's warning says.
//
// THE TOKEN ONLY FOR A CLAIM THAT IS IN THE CLEAR ANYWAY — a login and a seat,
// which the dashboard prints on every row. An address's token is its keyed
// BLIND, and a log line is the one copy of it that leaves the estate: it is
// shipped to whatever aggregates the logs, kept on that system's retention
// rather than this one's, and it is the same value for the same address for the
// life of the company — a stable pseudonym anybody holding the log can join
// across every line and every system that ever wrote it, and one guess away from
// the address for anybody who ever holds the blind key. The holders' ids are
// what an operator acts on, so the address is named by its KIND alone, which is
// the directory report's rule too. A WHITELIST rather than a refusal of the
// address kind, so a claim kind added later is not logged by default.
func duplicateAttrs(dup iamdomain.DuplicateClaim, at string) []any {
	attrs := []any{"claim", string(dup.Kind), "people", dup.People, "at", at}
	switch dup.Kind {
	case iamdomain.KindLogin, iamdomain.KindSeat:
		attrs = append(attrs, "token", dup.Token)
	}
	return append(attrs, "detail", "more than one person holds this claim, "+
		"which the broker cannot produce and a restore or a reanchor can; "+
		"decide who keeps it and release it from the others — nothing here picks")
}
