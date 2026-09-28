package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/schedule"
)

// THE IDENTITY DUTIES: three fleet singletons that keep the identity estate
// honest, each on its own lease and its own interval.
//
//   - THE SWEEP PUBLISHER turns `api.audit`'s two horizons into positions and
//     publishes a retention record for every bucket that is due
//     ([iamdomain.Writer.Sweep]). The applier on every node deletes the same
//     rows; nothing else ever deletes from this estate.
//   - THE KEY DUTY destroys a removed person's key when the removal's own
//     post-commit shred failed, and retries until it lands; destroys a key
//     NOBODY owns — minted for an enrolment or an invitation refused after
//     the mint, or left by an invitation the sweep collected — once it is past
//     the grace on a node whose rows have APPLIED the whole log, not merely
//     consumed it ([iamdomain.ShredKeys]). Until each lands, what the key
//     sealed is readable from every backup.
//   - THE CLAIM REPORT names a duplicate a restore produced and a reservation
//     an enrolment left behind ([iamdomain.Reader.Claims]). It reports and
//     never repairs: which of two people keeps an address is a decision.
//
// # Three leases rather than one duty with three jobs
//
// They share nothing but the domain. Their intervals differ, their costs land
// on different systems — the log, the coordination store, the local database —
// and a lease flap on one should cost that one a skipped interval rather than
// all three. So each claims its own `worker:` lease through
// [Engine.workerDuty], which also gives each the roles gate and the release on
// a graceful stop that every singleton has.
//
// # Every loop ticks once at start
//
// Unlike the maintenance sweep, which waits a full interval so a rolling
// deploy does not run it once per replica. Here the first tick is the useful
// one: the claim report after a boot is how a restore's duplicate is named the
// moment the restored node is back rather than an hour later; the key duty
// after a boot is how a shred that failed during the outage lands; and the one
// that writes — the sweep — is gated on something being due, so a tick that
// finds nothing costs reads and no records.

// identityLog is the duties' own voice.
var identityLog = logging.Get("iam.duty")

// The three duties' lease names.
const (
	identitySweepDuty  = "iam_sweep"
	identityKeysDuty   = "iam_key_shred"
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

// IdentityKeysInterval is how often the key duty looks for a key that outlived
// its owner.
//
// THE MAINTENANCE SWEEP'S FIFTEEN MINUTES. A removal's pending key exists only
// because a coordination write failed at the instant of a removal, so the
// useful retry cadence is "soon after the store is back" — and every minute of
// delay is a minute a removed person's name is readable from a backup. A key
// nobody owns waits out [iamdomain.OrphanKeyGrace]'s hour first, so fifteen
// minutes collects it within a quarter of that past it. A pass that finds
// nothing is two secret-store listings and two local reads, so a shorter
// interval would buy almost nothing but load on the coordination store.
const IdentityKeysInterval = maintenance.Interval

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
// duties that do. Within that, each duty is armed only where it has what it
// needs: the key duty needs the company's secret store.
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

	out := []identityDuty{
		duty(identitySweepDuty, IdentitySweepInterval, func(ctx context.Context) {
			sweepPass(ctx, writer, horizons)
		}),
		duty(identityClaimsDuty, IdentityClaimsInterval, func(ctx context.Context) {
			claimsPass(ctx, reader)
		}),
	}
	if e.backends != nil && e.backends.Fleet != nil {
		// NO DECRYPTION NEEDED: finding a key and deleting it open
		// nothing, so an off-boarding is never stuck behind a keyring that
		// no longer holds the key a row was sealed under.
		store := fleetsecrets.New(e.backends.Fleet, e.cipher).Estate()
		sealer, err := iamdomain.NewSealer(store)
		if err != nil {
			// SAID, because a key duty that is not armed looks exactly
			// like one finding nothing to destroy.
			identityLog.Error("iam_duty_unarmed", "duty", identityKeysDuty,
				"error", err.Error())
		} else {
			out = append(out, duty(identityKeysDuty, IdentityKeysInterval,
				func(ctx context.Context) {
					keysPass(ctx, reader, store, sealer, e.IdentityLogEnd)
				}))
		}
	}
	return out
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

// keysPass destroys every key a removal left behind and every key nobody owns.
//
// NO PERSON'S ID REACHES A WARNING about a key nobody owns: such a key is by
// definition not a person's, and the count is what an operator acts on.
func keysPass(ctx context.Context, reader *iamdomain.Reader, keys iamdomain.KeyIndex,
	shredder iamdomain.KeyDestroyer, logEnd func(context.Context) (uint64, error)) {

	report, err := iamdomain.ShredKeys(ctx, reader, keys, shredder, time.Now(),
		logEnd)
	if len(report.Destroyed) > 0 {
		identityLog.InfoContext(ctx, "iam_keys_shredded",
			"people", report.Destroyed,
			"detail", "each was removed with a key that outlived the removal; "+
				"their names and addresses are now unrecoverable everywhere")
	}
	if len(report.Collected) > 0 {
		identityLog.InfoContext(ctx, "iam_keys_collected",
			"keys", len(report.Collected),
			"detail", "each was minted for an enrolment that was refused before "+
				"it claimed anything, or for an invitation the sweep collected, "+
				"and no row owned it")
	}
	if report.Moved > 0 {
		// A KEY WRITTEN BETWEEN THE CENSUS AND THE DESTROY was spared:
		// a retried gesture re-used it, or a rekey moved it. Said, because
		// a count that fell short of what was judged would otherwise read
		// as a delete that failed.
		identityLog.InfoContext(ctx, "iam_keys_spared",
			"keys", report.Moved,
			"detail", "each was judged nobody's and written again before it "+
				"was destroyed — a gesture is using it — so the next pass judges "+
				"it afresh")
	}
	if report.Unjudged != nil && report.Unproven > 0 {
		// INFO AND NOT A WARNING: a node behind the log is the ordinary
		// state of one that has just booted, and one holding a record it
		// retained is the ordinary state of a rolling upgrade or a keyring
		// rotation half done — the next pass on a node that can judges.
		identityLog.InfoContext(ctx, "iam_keys_unjudged",
			"unowned", report.Unproven, "reason", report.Unjudged.Error())
	}
	if err != nil {
		identityLog.WarnContext(ctx, "iam_keys_pending", "error", err.Error(),
			"pending", len(report.Pending)-len(report.Destroyed),
			"detail", "a key could not be destroyed, so a name it seals is "+
				"still readable from backups taken before its removal; the "+
				"duty retries every interval")
	}
}

// PersonKeyIndex is the company's secret store as the identity duties and the
// directory report list it — which person keys exist — or nil on a node with
// no fleet store.
//
// NO DECRYPTION NEEDED, which is why this is not [Engine.PersonSealer]: listing
// a name and deleting it open nothing, so a node whose keyring no longer holds
// the key a row was sealed under can still say whose key outlived their
// removal, and destroy it. Returned as
// the interface with an explicit nil, never a typed one, so a caller's nil
// check means what it says.
func (e *Engine) PersonKeyIndex() iamdomain.KeyIndex {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return fleetsecrets.New(e.backends.Fleet, e.cipher).Estate()
}
