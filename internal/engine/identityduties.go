package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/schedule"
)

// THE IDENTITY DUTIES: four fleet singletons that keep the identity estate
// honest, each on its own lease and its own interval.
//
//   - THE SWEEP PUBLISHER turns `api.audit`'s two horizons into positions and
//     publishes a retention record for every bucket that is due
//     ([iamdomain.Writer.Sweep]). The applier on every node deletes the same
//     rows; nothing else ever deletes from this estate.
//   - THE DEACTIVATION PROBE asks the identity provider about every live OIDC
//     session with its refresh token ([oidc.Prober] over
//     [iamdomain.ProbeSessions]). A provider tells nobody when somebody is
//     disabled, so this is the only thing that ends their session before its
//     absolute deadline.
//   - THE KEY DUTY destroys a removed person's key when the removal's own
//     post-commit shred failed, and retries until it lands; destroys a key
//     NOBODY owns — minted for an enrolment or an invitation refused after
//     the mint, or left by an invitation the sweep collected — once it is past
//     the grace on a node that has applied the whole log
//     ([iamdomain.ShredKeys]); and collects the refresh token of every OIDC
//     session that is over ([iamdomain.Refreshes.CollectEnded]). Until each
//     lands, what the key sealed is readable from every backup, and a grant
//     is a live credential at somebody else's provider.
//   - THE CLAIM REPORT names a duplicate a restore produced and a reservation
//     an enrolment left behind ([iamdomain.Reader.Claims]). It reports and
//     never repairs: which of two people keeps an address is a decision.
//
// # Four leases rather than one duty with four jobs
//
// They share nothing but the domain. Their intervals differ by an order of
// magnitude and one of them is an operator's setting, their costs land on
// different systems — the log, somebody else's identity provider, the
// coordination store, the local database — and a lease flap on one should
// cost that one a skipped interval rather than all four. So each claims its
// own `worker:` lease through [Engine.workerDuty], which also gives each the
// roles gate and the release on a graceful stop that every singleton has.
//
// # Every loop ticks once at start
//
// Unlike the maintenance sweep, which waits a full interval so a rolling
// deploy does not run it once per replica. Here the first tick is the useful
// one: the claim report after a boot is how a restore's duplicate is named the
// moment the restored node is back rather than an hour later; the key duty
// after a boot is how a shred that failed during the outage lands; and the two
// that write — the sweep and the probe — are gated on something being due, so
// a tick that finds nothing costs reads and no records.

// identityLog is the duties' own voice.
var identityLog = logging.Get("iam.duty")

// The four duties' lease names.
const (
	identitySweepDuty  = "iam_sweep"
	identityProbeDuty  = "iam_deactivation_probe"
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
// its owner and a refresh grant that outlived its session.
//
// THE MAINTENANCE SWEEP'S FIFTEEN MINUTES. A removal's pending key exists only
// because a coordination write failed at the instant of a removal, so the
// useful retry cadence is "soon after the store is back" — and every minute of
// delay is a minute a removed person's name is readable from a backup. A key
// nobody owns waits out [iamdomain.OrphanKeyGrace]'s hour first, so fifteen
// minutes collects it within a quarter of that past it. A pass that finds
// nothing is two secret-store listings, one read of each held refresh grant and
// two local reads, so a shorter interval would buy almost nothing but load on
// the coordination store.
const IdentityKeysInterval = maintenance.Interval

// IdentityClaimsInterval is how often the claim report runs.
//
// AN HOUR. A duplicate arises only from a restore or a reanchor, both of which
// restart the node that performed them — and the first tick at boot is what
// names it then. The interval is the cadence of the WARNING that follows: loud
// enough that a standing duplicate is not forgotten, rare enough that it is
// not the log.
const IdentityClaimsInterval = time.Hour

// identityClaimCeiling is the longest a duty goes between two claims of its
// lease, whatever its own interval.
//
// AN HOUR, because a lease may live no longer than [coord.MaxDutyTTL] (three
// hours) and every singleton here keeps the three-claims ratio. The probe's
// interval is an operator's setting that may be a day, and a lease claimed
// once a day would have to live three: every backend refuses that claim, and a
// refused duty never runs at all. So a long interval is claimed hourly and run
// every so many claims.
const identityClaimCeiling = time.Hour

// claimCadence is how often a duty that runs every `every` claims its lease,
// and how many claims make one run.
//
// THE CADENCE DIVIDES THE INTERVAL EXACTLY — ninety minutes is two claims of
// forty-five rather than a claim an hour and a run every hour or two — so the
// operator's interval is the interval the duty keeps.
func claimCadence(every time.Duration) (time.Duration, int) {
	claims := int((every + identityClaimCeiling - 1) / identityClaimCeiling)
	if claims < 1 {
		claims = 1
	}
	return every / time.Duration(claims), claims
}

// identityDutyTTL is a duty's lease: three of its claims, the ratio every
// singleton here takes, so one slow claim does not hand the duty to a peer and
// a dead holder's duty moves within about three claims.
func identityDutyTTL(every time.Duration) time.Duration {
	cadence, _ := claimCadence(every)
	return 3 * cadence
}

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
// A NODE THAT RUNS NO IDENTITY DOMAIN ARMS NONE — a seats-only satellite
// applies no identity records, so it has no rows to report on and no estate
// to publish into — and NOR DOES ONE THAT RUNS NO WORKERS: every duty here is a
// worker singleton, whose claim that node's roles gate refuses on every tick
// ([Engine.workerDuty]), so arming them there would be loops that never run,
// reported by [Engine.IdentityDuties] as duties that do. Within that, each duty
// is armed only where it has what it needs: the probe needs a provider and
// custody, the key duty the company's secret store.
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
	e.identity = d
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
	if e == nil || e.identity == nil {
		return nil
	}
	out := make(map[string]time.Duration, len(e.identity.duties))
	for _, duty := range e.identity.duties {
		out[duty.name] = duty.interval
	}
	return out
}

// stopIdentityDuties ends every loop, waiting for a tick in flight.
func (e *Engine) stopIdentityDuties() {
	if e.identity == nil {
		return
	}
	e.identity.stop()
	e.identity.done.Wait()
	e.identity = nil
}

// identityDutiesFor declares the duties this node runs, without starting them.
//
// SEPARATE FROM THE START so a case can hold the arming decision — which
// duties, at which interval — without running a loop.
func (e *Engine) identityDutiesFor(boot *config.Bootstrap) []identityDuty {
	if e.native == nil || e.native.iamReader == nil || e.native.iamWriter == nil ||
		boot == nil || !e.profile.RunsWorkers() {
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
		// NO KEYRING NEEDED: finding a key and deleting it both work on a
		// node that cannot decrypt anything, so an off-boarding is never
		// stuck behind a missing keyring.
		store := fleetsecrets.New(e.backends.Fleet, e.cipher).Estate()
		sealer, err := iamdomain.NewSealer(store)
		if err != nil {
			// SAID, because a key duty that is not armed looks exactly
			// like one finding nothing to destroy.
			identityLog.Error("iam_duty_unarmed", "duty", identityKeysDuty,
				"error", err.Error())
		} else {
			// AND REFRESH CUSTODY WHERE THERE IS A KEYRING: collecting a
			// grant whose session is over is this duty's, because this
			// duty is armed wherever a grant can exist and the probe only
			// while a provider is configured. Nil on a node with no
			// keyring, which holds no grant to collect.
			custody := e.RefreshCustody()
			out = append(out, duty(identityKeysDuty, IdentityKeysInterval,
				func(ctx context.Context) {
					keysPass(ctx, reader, store, sealer, e.IdentityCaughtUp, custody)
				}))
		}
	}
	if provider, custody := e.identityProvider, e.RefreshCustody(); provider != nil &&
		custody != nil {
		sessions, err := iamdomain.NewProbeSessions(reader, writer, custody,
			provider.Config().Issuer, nil)
		if err != nil {
			identityLog.Error("iam_duty_unarmed", "duty", identityProbeDuty,
				"error", err.Error())
		} else {
			prober := e.newProber(provider, sessions)
			out = append(out, duty(identityProbeDuty, prober.Interval(),
				func(ctx context.Context) { probePass(ctx, prober) }))
		}
	}
	return out
}

// runIdentityDuty ticks one duty until the context ends.
//
// IT CLAIMS ON THE CADENCE AND RUNS ON THE INTERVAL — see [claimCadence]. The
// count of claims since the last run is this node's own and does not travel
// with the lease: a duty that moves runs at its new holder's first claim, which
// costs at most one early pass per move and never a skipped one.
func runIdentityDuty(ctx context.Context, duty identityDuty) {
	cadence, claims := claimCadence(duty.interval)
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	plan := newPassSchedule(claims)
	for {
		if ctx.Err() != nil {
			return
		}
		tickIdentityDuty(ctx, duty, plan)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tickIdentityDuty is one claim of one duty, reporting whether it ran the pass.
//
// THE CLAIM FIRST, AND THE SCHEDULE ASKED ONLY ON A HELD ONE. [passSchedule]
// counts the claims this node WON, so asking it on a claim a peer won would
// count that claim here: a node that lost the lease for a while would run
// early the moment it won it back, or spend its first held claim's run on a
// tick it never held.
func tickIdentityDuty(ctx context.Context, duty identityDuty, plan *passSchedule) bool {
	if !mine(ctx, duty) || !plan.held() {
		return false
	}
	duty.pass(ctx)
	return true
}

// passSchedule decides, claim by claim, when a duty that claims more often than
// it runs should run: on its first held claim, and then every `every` held
// claims. A claim a peer won does not count — the pass ran over there.
type passSchedule struct{ every, since int }

func newPassSchedule(every int) *passSchedule {
	return &passSchedule{every: every, since: every}
}

// held records one held claim and reports whether this one runs the pass.
func (p *passSchedule) held() bool {
	run := p.since >= p.every
	if run {
		p.since = 0
	}
	p.since++
	return run
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

// keysPass destroys every key a removal left behind and every key nobody owns,
// and collects every refresh grant whose session is over.
//
// NO PERSON'S ID REACHES A WARNING about a key nobody owns: such a key is by
// definition not a person's, and the count is what an operator acts on.
func keysPass(ctx context.Context, reader *iamdomain.Reader, keys iamdomain.KeyIndex,
	shredder iamdomain.Shredder, caughtUp func(context.Context) error,
	custody *iamdomain.Refreshes) {

	report, err := iamdomain.ShredKeys(ctx, reader, keys, shredder, time.Now(),
		caughtUp)
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
	if report.Unjudged != nil && report.Waiting > 0 {
		// INFO AND NOT A WARNING: a node behind the log is the ordinary
		// state of one that has just booted, and the next pass judges.
		identityLog.InfoContext(ctx, "iam_keys_unjudged",
			"unowned", report.Waiting, "reason", report.Unjudged.Error())
	}
	if err != nil {
		identityLog.WarnContext(ctx, "iam_keys_pending", "error", err.Error(),
			"pending", len(report.Pending)-len(report.Destroyed),
			"detail", "a key could not be destroyed, so a name it seals is "+
				"still readable from backups taken before its removal; the "+
				"duty retries every interval")
	}
	if custody == nil {
		return
	}
	collected, skipped, err := custody.CollectEnded(ctx, reader, time.Now())
	if len(collected) > 0 {
		identityLog.InfoContext(ctx, "iam_refresh_grants_collected",
			"grants", len(collected))
	}
	for _, reason := range skipped {
		identityLog.WarnContext(ctx, "iam_refresh_grant_kept",
			"error", reason.Error(),
			"detail", "this grant was not collected this pass; every other "+
				"one was judged, and the next pass tries it again")
	}
	if err != nil {
		identityLog.WarnContext(ctx, "iam_refresh_grants_unjudged",
			"error", err.Error())
	}
}

// probePass asks the provider about every live session once.
//
// A PASS THAT COULD NOT ASK ABOUT EVERYBODY IS A WARNING, not a quiet success:
// each skipped or failed session is already named on its own line, and this is
// the one line that says how much of the company the pass did not reach.
func probePass(ctx context.Context, prober *oidc.Prober) {
	pass, err := prober.Run(ctx)
	if err != nil {
		identityLog.WarnContext(ctx, "iam_probe_failed", "error", err.Error(),
			"checked", pass.Checked, "ended", pass.Ended)
		return
	}
	switch {
	case pass.Skipped > 0 || pass.Failed > 0:
		identityLog.WarnContext(ctx, "iam_probe_pass_partial",
			"checked", pass.Checked, "ended", pass.Ended,
			"skipped", pass.Skipped, "failed", pass.Failed,
			"detail", "every other session was asked about; the ones "+
				"named above are asked again next pass")
	case pass.Ended > 0:
		identityLog.InfoContext(ctx, "iam_probe_pass", "checked", pass.Checked,
			"ended", pass.Ended)
	}
}

// identityProvider is the OIDC provider Tier A names, or nil on a deployment
// that signs in another way.
//
// EVERY FIELD THE FLOW READS comes from the block here and nowhere else — the
// scopes and the probe interval were once dropped at this step, so the request
// asked for the package's scopes and the probe ran at the package's hour
// whatever the file said.
func identityProvider(boot *config.Bootstrap) *oidc.Provider {
	if boot == nil {
		return nil
	}
	block := boot.API.Auth.OIDC
	if block == nil || block.Issuer == "" {
		return nil
	}
	return oidc.NewProvider(oidc.Config{
		Issuer:            block.Issuer,
		ClientID:          block.ClientID,
		ClientSecret:      block.ClientSecret,
		RedirectURI:       boot.API.ExternalBase() + auth.PathAuthOIDCCallback,
		GroupsClaim:       block.GroupsClaim,
		RequireACR:        block.RequireACR,
		Scopes:            block.RequestedScopes(),
		DeactivationProbe: block.DeactivationProbe(),
	}, nil, nil)
}

// IdentityProvider is this process's OIDC provider, or nil.
func (e *Engine) IdentityProvider() *oidc.Provider {
	if e == nil {
		return nil
	}
	return e.identityProvider
}

// PersonKeyIndex is the company's secret store as the identity duties and the
// directory report list it — which person keys exist — or nil on a node with
// no fleet store.
//
// NO KEYRING NEEDED, which is why this is not [Engine.PersonSealer]: listing a
// name and deleting it both work without one, so a node that cannot decrypt
// can still say whose key outlived their removal, and destroy it. Returned as
// the interface with an explicit nil, never a typed one, so a caller's nil
// check means what it says.
func (e *Engine) PersonKeyIndex() iamdomain.KeyIndex {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return fleetsecrets.New(e.backends.Fleet, e.cipher).Estate()
}

// RefreshCustody is where an OIDC session's refresh token is kept, or nil on
// a node with no company secret store or no keyring — which cannot keep one,
// because custody seals the token and reads it back.
func (e *Engine) RefreshCustody() *iamdomain.Refreshes {
	if e == nil || e.backends == nil || e.backends.Fleet == nil || e.cipher == nil ||
		e.native == nil {
		return nil
	}
	custody, err := iamdomain.NewRefreshes(
		fleetsecrets.New(e.backends.Fleet, e.cipher).Estate(), e.native.nodeID)
	if err != nil {
		return nil
	}
	return custody
}
