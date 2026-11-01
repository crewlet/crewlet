package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE PROVIDER PARK: A SEAT WHOSE MODELS ARE ALL LIMITED WAITS FOR THEM, IT
// DOES NOT RETRY INTO THE LIMIT.
//
// A model's usage limit has an end the vendor usually names: Claude Code's
// "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)". The
// backend reads it and sits its entry out until then
// (internal/providers/llm/harness), so a call in that window is refused
// without a process being started. Before this park, nothing above the backend
// used it. A turn every member of whose chain was refused failed, the
// dispatcher NAKed its delivery, and the redelivery a second later ran a turn
// that was refused the same way: with ten task partitions per seat and new
// notifications joining them, every seat started and failed a turn about once
// a second for the whole afternoon. Every one moved the seat between working,
// failed and stopped on every screen that draws it.
//
// So a turn that failed because EVERY member of its chain refused for a rate
// limit ([chain.Limited]) parks its seat, with the machinery the budget park
// uses (seatpark.go):
//
//   - a pause hold under [pauseReasonProvider] is taken on its inbox, so
//     nothing more is delivered to it, and the delivery is DEFERRED, which
//     keeps its place at the head of the inbox;
//   - the seat joins the PROBE of the chain that refused: one alarm, armed
//     for the instant the first member is tried again
//     ([chain.Limit.RetryAt]), which makes one short call through the members
//     that refused, outside any delivery, for every seat parked on that chain
//     at once ([limitProbes]);
//   - and `seat_provider_parked` says so, with when the vendors said the
//     limit lifts, so every screen draws one steady condition rather than a
//     seat that blinks.
//
// THE PROBE IS NOT THE HELD MAIL. It used to be: the alarm lifted the hold,
// the head of the inbox ran a turn into the limit, and that turn parked the
// seat again by deferring its delivery. A deferral spends one of the
// message's deliveries, so a limit that outlasted the backend's hourly retry
// (a weekly plan, a day's quota) walked the held message to the dead-letter
// boundary one probe at a time: 24 probes, a day, for a limit named 40 hours
// out, and about five and a half hours for one whose reset nobody named. So
// the alarm asks the models itself ([Engine.probeLimit]), and the answer
// decides for every seat parked on the chain:
//
//   - the call answers, or fails for a reason that is not a limit: each hold
//     is lifted, `seat_provider_released` says why, and each seat's mail is
//     delivered again in order, once, the held head first, having spent only
//     the one delivery its park did;
//   - every member refuses for a limit again: the alarm is armed for the new
//     instant, `seat_provider_parked` is published again for each seat with
//     what the vendors now say, and no inbox is touched.
//
// ONE PROBE PER LIMITED CHAIN, NOT PER SEAT. The limit is the entries', and so
// is its end: five seats on one spent subscription each arming its own alarm
// made five calls at every retry instant, each a full run of the CLI, to learn
// one fact. Seats parked on the same entries, in the same order, of the same
// registry share one probe, and its answer settles them all; a seat on another
// chain has its own.
//
// WHAT A PROBE COSTS. It is a full run of the entry's CLI: a process launched,
// and a request carrying the CLI's own fixed overhead (its harness, which a
// one-word prompt does not shrink), far more than the prompt itself. It is
// spend like any other auxiliary call, charged and recorded through the seam
// under the background stage (`provider_probe`), where the Spend screen
// shows it, to the seat the probe runs as: the parked seat that sorts first,
// on whose CLI home ([llm.WithSeat]) the call is made. Members still sitting
// out refuse without a process being started, so each retry instant costs
// one such run, to the member whose window is over, for every seat at once.
//
// ONLY A CHAIN THAT IS LIMITED THROUGHOUT. A seat whose head is spent and whose
// fallback failed for a reason nothing says will last keeps the ordinary
// retry, and one whose fallback answers is never parked at all: it is working.
//
// RELEASED BY A NEW SET OF MODELS. Every epoch builds its providers afresh, so a
// park decided under one registry is released by an apply that installs
// another ([Engine.reconcileProviderParks]): the redelivery asks the new
// entries, and a limit that still holds parks the seat again at the cost of one
// delivery. An entry that still calls as the same account finds the sit-out
// its sign-in is serving on the node's registry ([harness.Plans]), so that
// re-park costs the delivery and no process: the entry refuses before any CLI
// is started. The park is not
// carried across instead, because whether the seat's chain is still the one
// that refused is the new company's to say (its `llm`, the entries it names),
// and its first turn is what asks. A SEAT WHOSE MAIL ITS RUN HOLDS has no
// delivery to come: the release re-fires its run's held completion
// ([Engine.readmitResumes]), which is collected again and refused on the
// carried limit before its turn runs, and that parks the seat again
// ([Engine.collectedRefusal]), at the cost of one collection and no process.
// Parked by nothing until then, it read released, left the probe, and its job
// was collected and reviewed into the vendor's window once the sit-out ended.
//
// A RESUME THAT MEETS THE LIMIT PARKS ITS SEAT TOO ([Engine.parkResumeOnLimit]).
// A seat's executor is its coding CLI's own run in a box, so the usual way a
// plan runs out is inside a run: the run ends on its vendor's limit report, the
// segment that collects it sits the entry out, and the review or the relaunch
// that segment makes is refused, every member sitting out. That segment is no
// delivery, and the dispatcher never ran for the seat, whose mail the run
// holds, so nothing parked it: the seat read idle with a failed turn, never the
// one steady condition above, and joined no probe, so each held resume ran its
// review or launched a box into the vendor's window at every instant the
// backend benched the entry until, an hour at a time. The resume parks the seat
// as the dispatcher would, and its job waits for the park.
//
// SO DOES A COMPLETION WHOSE REVIEW'S MODELS ALREADY KNOW THE LIMIT, once its job
// is collected ([Engine.collectedRefusal]): the entry one seat's run spent is
// benched for every seat on it, and each of their completions is collected,
// charged in its window and its box paused, then refused before its turn runs
// and its seat parked, rather than refused at the turn gate before its claim,
// uncollected, its box kept awake, its seat parked by nothing.
//
// A PARKED SEAT STARTS NO TURN, whatever road the turn comes by: its mail waits
// on its inbox, and the turn gate refuses every resume on it until the park is
// released ([Engine.providerParkRefusal]), whichever chain the resume's first
// call would take. A probe that meets the limit again keeps it so; the
// instant a completion's own refusal named passes under a park that still
// stands. A completion it refuses before its claim is one already collected:
// a parked seat holds no job no collection has read (see
// [Engine.seatCannotRun]).
//
// A PARK OUTLIVES NO SEAT. A seat's release takes the seat out of this node's
// hands, detaches its inbox, which drops every hold on it, and forgets its
// parks ([Engine.forgetProviderPark]); it waits for none of the turns that can
// park the seat: a lost lease detaches the inbox under a running delivery, and
// a resume runs in no delivery at all, its review minutes long. So a park can
// land while the seat is being released, or after. One taken after the
// release forgot the seat's parks asks for the seat again once it is whole,
// and undoes itself where it is gone ([Engine.unparkReleased]); one taken
// between the detach and the forget has its hold lifted by the forget
// ([seatParks.forget]). Left standing, such a park outlived the seat here: its
// probe went on calling the models and publishing the seat parked while a peer
// ran it, and its hold kept the seat's inbox shut on this node with no park
// left to lift it.
//
// A RELEASE IS ALSO A SIGNAL to the resumes waiting on the same condition
// ([waitProvider]): a launch's completion or a person's answer refused
// because the models were limited is re-checked the moment a park lifts, on
// any seat, since the entries are the node's and not the seat's, rather than
// at its own clock or the waiter's next announcement.
//
// WHO HAS TO AGREE ON IT: this node alone, like the budget park. A restart, or
// the seat moving to a peer, asks the models again on the first delivery.

// pauseReasonProvider is the hold name the provider park takes on a seat's
// inbox; see [parkSpec.hold].
const pauseReasonProvider = "provider_limit"

// providerParkSpec is the provider park's hold and log lines.
var providerParkSpec = parkSpec{
	hold:        pauseReasonProvider,
	released:    "seat_provider_park_released",
	notReleased: "seat_provider_park_not_released",
}

// providerBasis is what a provider park was decided under: the registry whose
// entries refused, and the keys of the chain that refused, in its order, which
// the probe asks. A park whose registry is no longer current was decided by
// entries that no longer serve the seat.
type providerBasis struct {
	models *phase.Registry
	keys   []string
}

// providerParks is every seat inbox this node holds because every model of the
// seat's chain is limited, by the seat's id.
type providerParks = seatParks[providerBasis]

// providerParking is one seat's provider park.
type providerParking = seatParking[providerBasis]

// limitProbes is every limited chain this node probes, one probe for all the
// seats parked on it (see the file's doc).
type limitProbes struct {
	mu     sync.Mutex
	probes map[probeKey]*limitProbe

	// stopped refuses new alarms once the engine is tearing down.
	stopped bool

	// asking counts the probes making their call, which
	// [Engine.stopProviderParks] ends and waits for: the call is this
	// node's, and a stopping node must not leave one running.
	asking sync.WaitGroup

	// waits is when each seat this node parked on a limit began waiting,
	// which the Models page and the `llm_limit_waiting` alarm measure from
	// ([Engine.parkedByEntry]).
	//
	// THE SEAT'S, NOT A PROBE'S: an apply that brings new models releases
	// every park decided under the old ones, and the seat's next delivery
	// parks it again at once on the same limit, under a new probe. Dated by
	// the probe, the wait restarted at every apply, so a seat stuck for an
	// hour read as parked for seconds and the alarm that turns red at
	// fifteen minutes went out and came back with every config change. A wait
	// ends when the chain answers (or stops reporting a limit) or the seat
	// leaves this node; a park that follows a release for any other reason
	// within [parkCarryWindow] is the same wait.
	waits map[uuid.UUID]parkWait
}

// parkWait is one seat's wait: when it began, when its last park was released
// for a reason that does not end the wait (zero while it is parked), and the
// entries of the chain it last parked on.
type parkWait struct {
	since, releasedAt time.Time
	keys              []string
}

// parkCarryWindow is how soon after a release that does not end the wait a
// park still continues it: ONE MINUTE. The park that follows an apply's
// release is the seat's next delivery meeting the same limit, which the
// entry's sit-out refuses before any process starts, so it lands within the
// delivery's own queue latency, seconds; a minute covers a slow redelivery and
// is far below the fifteen minutes the wait is judged at, so a seat that
// truly got through and later met a new limit starts a new wait.
const parkCarryWindow = time.Minute

// probeKey names one limited chain: the registry it was decided under and its
// keys in order.
type probeKey struct {
	models *phase.Registry
	keys   string
}

func probeKeyOf(b providerBasis) probeKey {
	return probeKey{models: b.models, keys: strings.Join(b.keys, "\x00")}
}

// limitProbe is one limited chain's probe: what it asks, when, and the seats
// its answer settles, by id with their handles.
type limitProbe struct {
	basis providerBasis
	until time.Time
	seats map[uuid.UUID]string
	// since is when each seat joined the probe: the instant it parked on
	// this chain, which the Models page and the `llm_limit_waiting` alarm
	// measure a wait from ([Engine.parkedByEntry]).
	since map[uuid.UUID]time.Time
	alarm alarm
	// call is the call the probe is making, nil when none is.
	call *probeCall
}

// probeCall is one call of a probe: its own, so the call that ends clears
// only itself. A limit that still holds re-arms the alarm before its call has
// returned, and an alarm armed for an instant already past fires at once, so
// a newer call can be under way when an older one's deferred cleanup runs.
type probeCall struct {
	cancel context.CancelFunc
}

// providerProbe is the probe's whole request: a text function whose answer is
// read for nothing but whether it came, so it is as short as a call can be.
var providerProbe = llm.Request{Messages: []llm.Message{
	{Role: llm.RoleSystem, Content: "Reply with the single word: ok"},
	{Role: llm.RoleUser, Content: "ok"},
}}

// providerPark is the dispatcher's provider stage: it parks handle's seat
// until limit's first member is tried again, reporting the deferral reason. A
// resumed segment that meets the limit parks its seat through it too
// ([Engine.parkResumeOnLimit]).
//
// The error it returns is a hold that could not be taken, which the
// dispatcher DEFERS ([Dispatcher.parkOn]): the refusal is the node's, and a
// deferral keeps the delivery's place at the head of the seat's inbox.
func (e *Engine) providerPark(ctx context.Context, handle string, limit chain.Limit) (string, bool, error) {
	if limit.RetryAt.IsZero() {
		return "", false, nil
	}
	seat, err := e.seatID(handle)
	if err != nil {
		return "", false, err
	}
	c := e.Company()
	basis := providerBasis{keys: slices.Clone(limit.Keys)}
	if c != nil {
		basis.models = c.Models
	}
	// A SEAT PARKED AGAIN ON ANOTHER CHAIN leaves the probe of the one it
	// was parked on, which no longer settles it.
	if old := e.providerParks.parking(seat); old != nil && probeKeyOf(old.basis) != probeKeyOf(basis) {
		e.leaveLimitProbe(seat, old.basis)
	}
	// NO INSTANT OF ITS OWN: the chain's probe releases it.
	if err := e.providerParks.take(ctx, e, providerParkSpec, seat, handle, basis, time.Time{},
		"its models answered"); err != nil {
		return "", false, fmt.Errorf("engine: park %s on its models' limit: %w", handle, err)
	}
	e.joinLimitProbe(ctx, basis, seat, handle, limit.RetryAt)

	resets := resetsOf(limit)
	reason := fmt.Sprintf("provider limit: %s, tried again %s, resets %s",
		strings.Join(limit.Keys, " -> "), rfc3339(limit.RetryAt), resets)
	log.InfoContext(ctx, "seat_provider_parked", "seat", handle,
		"providers", strings.Join(limit.Keys, ","), "retry_at", rfc3339(limit.RetryAt),
		"resets_at", resets, "said", limit.Said,
		"detail", "this seat's mail waits on its inbox until its models are tried again")
	e.publishProviderPark(ctx, handle, providerParked(limit))
	// THE SEAT AGAIN, once the park is whole: the seat's release waits for
	// none of the turns that park it (see the file's doc). The release takes
	// the seat out of this node's hands before it detaches the inbox and
	// forgets the parks (seat.Host), so a park that finds the seat held here
	// is one that release will forget, and one that finds it gone is undone
	// now, whatever of it the release got to first.
	if _, held := e.launchFence(handle); !held {
		e.unparkReleased(ctx, seat, handle, basis)
		return reason, true, nil
	}
	// AND THE BASIS, after the hold: an apply that installed new models
	// between the turn's failure and the park released nothing, since there
	// was nothing to release yet, and this park would otherwise wait out a
	// limit the new entries may not have.
	if now := e.Company(); now != nil && now.Models != basis.models {
		e.releaseProviderPark(ctx, seat, "the seat's models changed while it parked")
	}
	return reason, true, nil
}

// unparkReleased undoes a provider park taken on a seat this node no longer
// holds ([Engine.providerPark]): its hold lifted, its place on the chain's
// probe left, and the seat said released, as the release says of a park it
// forgets ([Engine.forgetProviderPark]).
//
// WHETHER OR NOT ITS RECORD IS LEFT. The release's forget can land between the
// park's take and this, dropping the record and lifting the hold, and still
// come before the park joined the probe or said the seat parked: with no
// record to release, the place on the probe stayed, its alarm went on calling
// the models as the seat, and the seat read parked on every screen while a
// peer ran it. So the probe is left and the release said either way.
func (e *Engine) unparkReleased(ctx context.Context, seat uuid.UUID, handle string, basis providerBasis) {
	if e.releaseProviderPark(ctx, seat, seatReleasedHere) {
		return
	}
	e.leaveLimitProbe(seat, basis)
	e.publishProviderPark(ctx, handle, types.SeatProviderReleased{Reason: seatReleasedHere})
}

// parkResumeOnLimit parks handle's seat on the limit a resumed segment met
// (err, a chain every member of which refused for a limit, [chain.Limited]):
// the review of a run that spent its plan, or a relaunch whose every runner
// member sits out. See the file's doc for why the resume parks the seat as a
// dispatch does, and [Engine.providerParkRefusal] for what the park then holds.
// Nothing for any other error.
func (e *Engine) parkResumeOnLimit(ctx context.Context, handle string, err error) {
	if limit, limited := chain.Limited(err); limited {
		e.parkOnLimit(ctx, handle, limit)
	}
}

// parkOnLimit parks handle's seat on limit for a resume, which no delivery
// carries: the limit its segment met ([Engine.parkResumeOnLimit]), or the one
// its collected completion's review would meet ([Engine.collectedRefusal]).
// Nothing for a limit that names no instant, which no probe could be armed for.
//
// THE PARK IS NO DELIVERY'S, so a hold it could not take is said and
// otherwise left: the resume is reverted and waits on its refusal either way,
// as it did before it parked anything.
func (e *Engine) parkOnLimit(ctx context.Context, handle string, limit chain.Limit) {
	if _, _, err := e.providerPark(ctx, handle, limit); err != nil {
		log.WarnContext(ctx, "seat_provider_park_not_taken", "seat", handle,
			"providers", strings.Join(limit.Keys, ","), "error", err.Error(),
			"detail", "the resume met its models' limit and the seat could not be parked "+
				"on it; the resume waits for the limit's instant instead")
	}
}

// providerParkRefusal is what a resume on handle's seat is refused for while
// this node has the seat parked on its models' limit: [waitProvider], which
// the park's release signals, until the instant the chain's probe makes its
// next call where that is still to come. False for a seat not parked here.
//
// EVERY RESUME, WHICHEVER CHAIN ITS FIRST CALL TAKES. A park is this node's
// word that the seat waits for its models, and a resume is a turn. The case it
// decides is a completion whose review sends its run back to runners every one
// of which sits out: its reviewer is ready, so asking the review's chain
// ([Engine.providerRefusing]) admits it the moment its own hold's instant
// passes, and it was collected and reviewed again, and its box launched again,
// into a window the probe had just found still spent.
//
// AN INSTANT STILL TO COME, OR NONE. A refusal's Until is when it lifts on the
// clock alone, and an answer's retry re-checks at it
// ([sandbox.Coordinator.waitOwed]). From the instant the probe's alarm fires
// until its call comes back, a full run of the entry's CLI, that instant is
// past and the park still stands: named, it told every answer waiting on the
// seat to re-check at once, and each one did, thousands of times a second for
// as long as the call was out, every pass re-reading its run, opening its
// answer's seal, taking and giving back the node's admission and saying so in
// the log. A probe that is due or asking names nothing ([Engine.limitProbeNext]),
// because what ends that wait is its answer: the park's release, which signals
// [waitProvider], or a renewal, whose instant the next check is told.
func (e *Engine) providerParkRefusal(handle string) (sandbox.Refusal, bool) {
	seat, err := e.seatID(handle)
	if err != nil {
		return sandbox.Refusal{}, false
	}
	parking := e.providerParks.parking(seat)
	if parking == nil {
		return sandbox.Refusal{}, false
	}
	next := e.limitProbeNext(parking.basis)
	reason := "provider limit: " + strings.Join(parking.basis.keys, " -> ") +
		", the seat parked until its models answer"
	if !next.IsZero() {
		reason = limitReason(parking.basis.keys, next)
	}
	return sandbox.Refusal{Condition: waitProvider, Reason: reason, Until: next}, true
}

// limitProbeNext is when the probe of the chain basis names makes its next
// call, where that is still to come: zero where it has no probe, or where its
// call is out or due now, whose answer releases the parks (a signal) or renews
// them (a new instant) — see [Engine.providerParkRefusal].
func (e *Engine) limitProbeNext(basis providerBasis) time.Time {
	p := &e.limitProbes
	p.mu.Lock()
	defer p.mu.Unlock()
	probe := p.probes[probeKeyOf(basis)]
	if probe == nil || probe.call != nil || !probe.until.After(e.now()) {
		return time.Time{}
	}
	return probe.until
}

// joinLimitProbe puts seat on the probe of the chain basis names, creating the
// probe and arming it for retry if it has none, and arming it sooner if retry
// is earlier than the instant it waits for. A probe whose call is out already
// settles a seat that joins meanwhile with its answer.
func (e *Engine) joinLimitProbe(ctx context.Context, basis providerBasis, seat uuid.UUID,
	handle string, retry time.Time) {

	p := &e.limitProbes
	key := probeKeyOf(basis)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.probes == nil {
		p.probes = map[probeKey]*limitProbe{}
	}
	probe := p.probes[key]
	if probe == nil {
		probe = &limitProbe{basis: basis, seats: map[uuid.UUID]string{},
			since: map[uuid.UUID]time.Time{}}
		p.probes[key] = probe
	}
	probe.seats[seat] = handle
	if _, waiting := probe.since[seat]; !waiting {
		// A seat that joins again while it waits keeps its first instant:
		// a renewed park is the same wait, and so is a park soon after a
		// release that did not end it ([limitProbes.waits]).
		now := e.now()
		if p.waits == nil {
			p.waits = map[uuid.UUID]parkWait{}
		}
		wait, known := p.waits[seat]
		if !known || (!wait.releasedAt.IsZero() && now.Sub(wait.releasedAt) > parkCarryWindow) {
			wait = parkWait{since: now}
		}
		wait.releasedAt, wait.keys = time.Time{}, basis.keys
		p.waits[seat] = wait
		probe.since[seat] = wait.since
	}
	if probe.call == nil && (probe.alarm == nil || retry.Before(probe.until)) {
		probe.until = retry
		e.armLimitProbeLocked(ctx, key, probe)
	}
}

// armLimitProbeLocked arms probe's alarm for its instant, on
// context.WithoutCancel(ctx) for the reason [seatParks.armLocked] gives. The
// caller holds the probes' lock.
func (e *Engine) armLimitProbeLocked(ctx context.Context, key probeKey, probe *limitProbe) {
	if e.limitProbes.stopped {
		return
	}
	if probe.alarm != nil {
		probe.alarm.Stop()
	}
	wait := max(probe.until.Sub(e.now()), 0)
	fired := context.WithoutCancel(ctx)
	probe.alarm = e.providerParks.arm(wait, func() { e.fireLimitProbe(fired, key, probe) })
}

// fireLimitProbe is a probe's alarm: its one call, under a context ended if the
// probe's last seat leaves it or the node stops meanwhile.
func (e *Engine) fireLimitProbe(ctx context.Context, key probeKey, probe *limitProbe) {
	p := &e.limitProbes
	p.mu.Lock()
	if p.stopped || p.probes[key] != probe || len(probe.seats) == 0 {
		p.mu.Unlock()
		return
	}
	asking, cancel := context.WithCancel(ctx)
	call := &probeCall{cancel: cancel}
	probe.call = call
	// THE PARKED SEAT THAT SORTS FIRST runs the call, on its CLI home and
	// its counter: a stable choice, so one probe after another runs as
	// the same seat while the set is the same.
	handle := probe.seats[slices.MinFunc(slices.Collect(maps.Keys(probe.seats)),
		func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })]
	p.asking.Add(1)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if probe.call == call {
			probe.call = nil
		}
		p.mu.Unlock()
		cancel()
		p.asking.Done()
	}()
	e.probeLimit(asking, key, probe, handle)
}

// probeLimit makes a probe's one call through the chain that refused and
// settles every seat parked on it by the answer (see the file's doc). ctx ends
// if the probe's last seat leaves it or the node stops while the call is out,
// and whoever did that has already settled them.
func (e *Engine) probeLimit(ctx context.Context, key probeKey, probe *limitProbe, handle string) {
	// SETTLED ON A CONTEXT THE PROBE DOES NOT END: a release cancels the
	// call's context before it lifts the holds.
	settle := context.WithoutCancel(ctx)
	c := e.Company()
	if c == nil || c.Models != probe.basis.models {
		// The apply that installed other models releases these parks
		// itself ([Engine.reconcileProviderParks]); one that raced the
		// alarm here is released the same way.
		e.endLimitProbe(settle, key, probe, "the seat's models changed")
		return
	}
	err := e.probeModels(ctx, c, handle, probe.basis.keys)
	if ctx.Err() != nil {
		return
	}
	if limit, limited := chain.Limited(err); limited {
		p := &e.limitProbes
		p.mu.Lock()
		current := !p.stopped && p.probes[key] == probe
		var handles []string
		if current {
			probe.until = limit.RetryAt
			e.armLimitProbeLocked(settle, key, probe)
			handles = slices.Sorted(maps.Values(probe.seats))
		}
		p.mu.Unlock()
		if !current {
			return
		}
		log.InfoContext(settle, "seat_provider_park_renewed", "seats", strings.Join(handles, ","),
			"providers", strings.Join(limit.Keys, ","), "retry_at", rfc3339(limit.RetryAt),
			"resets_at", resetsOf(limit), "said", limit.Said,
			"detail", "the probe met the limit again; these seats' mail stays on their "+
				"inboxes, untouched, until their models are tried again")
		for _, h := range handles {
			e.publishProviderPark(settle, h, providerParked(limit))
		}
		return
	}
	why := "its models answered"
	if err != nil {
		why = "its models no longer report a usage limit"
		log.InfoContext(settle, "seat_provider_probe_failed", "seat", handle,
			"error", err.Error(),
			"detail", "the probe failed for a reason that is not a usage limit, so the parks "+
				"are lifted and the seats' mail meets the failure on the ordinary retry")
	}
	e.endLimitProbe(settle, key, probe, why)
}

// endLimitProbe retires probe and releases every seat it still settles.
func (e *Engine) endLimitProbe(ctx context.Context, key probeKey, probe *limitProbe, why string) {
	p := &e.limitProbes
	p.mu.Lock()
	if p.probes[key] == probe {
		delete(p.probes, key)
	}
	if probe.alarm != nil {
		probe.alarm.Stop()
		probe.alarm = nil
	}
	seats := sortedSeats(maps.Keys(probe.seats))
	p.mu.Unlock()
	for _, seat := range seats {
		if parking := e.providerParks.parking(seat); parking != nil && probeKeyOf(parking.basis) == key {
			e.releaseProviderParking(ctx, seat, parking, why)
		}
		// THE WAIT IS OVER: the chain answered, or stopped reporting a
		// limit.
		e.endParkWait(seat)
	}
}

// parkReleased marks seat's wait as released at this instant, without ending
// it: a park within [parkCarryWindow] continues it.
func (e *Engine) parkReleased(seat uuid.UUID) {
	p := &e.limitProbes
	p.mu.Lock()
	defer p.mu.Unlock()
	if wait, known := p.waits[seat]; known {
		wait.releasedAt = e.now()
		p.waits[seat] = wait
	}
}

// endParkWait forgets when seat began waiting: its wait is over.
func (e *Engine) endParkWait(seat uuid.UUID) {
	p := &e.limitProbes
	p.mu.Lock()
	delete(p.waits, seat)
	p.mu.Unlock()
}

// leaveLimitProbe takes seat off the probe of the chain basis names, and
// retires the probe, its alarm stopped and its call ended, once no seat is on
// it.
func (e *Engine) leaveLimitProbe(seat uuid.UUID, basis providerBasis) {
	p := &e.limitProbes
	key := probeKeyOf(basis)
	p.mu.Lock()
	defer p.mu.Unlock()
	probe := p.probes[key]
	if probe == nil {
		return
	}
	delete(probe.seats, seat)
	delete(probe.since, seat)
	if len(probe.seats) > 0 {
		return
	}
	delete(p.probes, key)
	if probe.alarm != nil {
		probe.alarm.Stop()
		probe.alarm = nil
	}
	if probe.call != nil {
		probe.call.cancel()
	}
}

// probeModels makes the probe's one call through the members of c's registry
// keys names, in order, charged and recorded through the auxiliary seam and on
// handle's own CLI home. A key the registry no longer has is an error that is
// not a limit, which lifts the park.
func (e *Engine) probeModels(ctx context.Context, c *Company, handle string, keys []string) error {
	members := make([]chain.Member, 0, len(keys))
	for _, key := range keys {
		provider, ok := c.Models.Provider(key)
		if !ok {
			return fmt.Errorf("engine: provider %q is no longer configured", key)
		}
		members = append(members, chain.Member{Key: key, Provider: provider})
	}
	seam, ok := e.seamFor(c)
	if !ok {
		return fmt.Errorf("engine: the company has no models to probe")
	}
	members, err := seam.recordedMembers(e.seatRole(handle), members,
		auxspend.Use{Stage: types.AuxStageBackground, Purpose: types.AuxProviderProbe})
	if err != nil {
		return err
	}
	probe, err := chain.New(members, chain.Options{})
	if err != nil {
		return err
	}
	_, err = probe.Complete(llm.WithSeat(ctx, handle), providerProbe)
	return err
}

// providerRefusing reports whether every model the first call of route's turn
// on handle's seat could take is at a usage limit it already reported, refusing
// on [waitProvider] with the reason naming them and when the first is tried
// again: the provider stage's question ASKED WITHOUT A DELIVERY, for a resume,
// which has no mail of its own to hold, as [Engine.budgetHold] answers for the
// budget. Asked of what the entries already know ([chain.Unready]), so it
// launches nothing; a chain whose entries know of no limit is not refusing, and
// the resume finds out by running.
//
// THE CHAIN OF THE ROUTE'S FIRST CALL ([firstCallMembers]), because the two
// differ wherever the seat names a reviewer of its own: a seat whose executor
// sits out a spent plan while its `llm_review` answers has a finished run to
// review and deliver, and held on its executor's limit it waited out the
// vendor's window, an hour at a time, with its mail behind it. A call the turn
// makes later, a review's send-back to a spent runner, meets its own limit
// mid-turn and waits there ([resumeRefusal]).
//
// AND THE CHAIN'S LIMIT, where every member sits one out with an instant it is
// tried again ([chain.KnownLimit]): the limit a call through the chain would
// have met, which a completion's collected resume parks its seat on
// ([Engine.collectedRefusal]). No keys where a member refuses for another
// reason, a login somebody has to mend, which no park waits out: the refusal
// stands, until the instant the spent member is tried again.
func (e *Engine) providerRefusing(ctx context.Context, c *Company, handle string,
	route sandbox.ResumeRoute,
) (sandbox.Refusal, chain.Limit, bool) {
	if c == nil || c.Models == nil || c.Org == nil {
		return sandbox.Refusal{}, chain.Limit{}, false
	}
	role := c.Org.AgentSeatByHandle(handle)
	if role == nil {
		return sandbox.Refusal{}, chain.Limit{}, false
	}
	members, err := firstCallMembers(c, role, route)
	if err != nil {
		return sandbox.Refusal{}, chain.Limit{}, false
	}
	providers := make([]llm.Provider, 0, len(members))
	keys := make([]string, 0, len(members))
	for _, m := range members {
		providers = append(providers, m.Provider)
		keys = append(keys, m.Key)
	}
	retry, _, limited := llm.LimitedUntil(chain.Unready(ctx, providers))
	if !limited || retry.IsZero() {
		return sandbox.Refusal{}, chain.Limit{}, false
	}
	limit, _ := chain.KnownLimit(ctx, members)
	return sandbox.Refusal{Condition: waitProvider, Reason: limitReason(keys, retry), Until: retry},
		limit, true
}

// collectedRefusal is what a launch's completion is refused for once its
// job is collected, and before its turn runs: a usage limit every model its
// review could take already reported ([Engine.providerRefusing], for
// [sandbox.ResumeCompletion]), on which the seat is parked at once, as the turn
// that called through the chain would have parked it ([Engine.parkOnLimit]).
// The coordinator holds the job on this node until the instant the refusal
// names ([sandbox.Coordinator.OnCompleted]), and from then the seat's park
// refuses it at the turn gate, before its claim, until the park's release.
//
// AFTER THE COLLECTION, AND NEVER AT THE GATE. The job is over and its tokens are
// spent, so the claim collects it first: charged in the window it ran in, its
// box paused and left asleep by the waiter from then on, its run on the
// record as read back. Refused at the gate instead, before the claim, the
// usual case was every seat on a shared plan: the plan is spent inside runs,
// so every run on it ends on the vendor's limit, the first completion's resume
// sits the entry out, and every later one whose review takes that entry was
// refused there and left uncollected, uncharged, its box kept awake by the
// waiter at every poll, for as long as the entry sat out: up to the hour the
// backend benches it, and another hour each time a probe re-benched it. Its
// seat was never parked either, so it read idle with no steady condition and
// joined no probe, and an apply that released a park carrying the entry's
// sit-out left the seat whose mail its run held parked by nothing.
//
// A SEAT A PERSON STOPPED is refused nothing here: its turn is closed as
// stopped at its first round, before any model call, so no limit can refuse
// it (see [Engine.seatCannotRun]).
func (e *Engine) collectedRefusal(ctx context.Context, c *Company, handle string) (sandbox.Refusal, bool) {
	if e.seatStopping(handle) {
		return sandbox.Refusal{}, false
	}
	refusal, limit, limited := e.providerRefusing(ctx, c, handle, sandbox.ResumeCompletion)
	if !limited {
		return sandbox.Refusal{}, false
	}
	if len(limit.Keys) > 0 {
		e.parkOnLimit(ctx, handle, limit)
	}
	return refusal, true
}

// firstCallMembers is the chain the first model call of route's turn on
// role's seat is made on: a completion's is its reviewer's
// ([phase.Registry.Chain] for [phase.Review], which is the seat's `llm` where
// it names no `llm_review`), and an answer's is the relaunch that carries the
// run on, over the executor members a launch can take ([executorRunners],
// against the epoch's own catalogue, as the launch picks from it). An error
// where the chain cannot be told, which no gate refuses on: the turn finds
// out by running.
func firstCallMembers(c *Company, role *org.Role, route sandbox.ResumeRoute) ([]chain.Member, error) {
	switch route {
	case sandbox.ResumeCompletion:
		return c.Models.Chain(role, phase.Review)
	case sandbox.ResumeAnswer:
		if c.catalogue == nil {
			return nil, fmt.Errorf("engine: seat %q's epoch has no sandbox catalogue to "+
				"launch its executor from", role.Handle())
		}
		return executorRunners(c, role, c.catalogue)
	default:
		return nil, fmt.Errorf("engine: a resume on route %q", route)
	}
}

// limitReason is a resume's refusal for a chain at its limit.
func limitReason(keys []string, retry time.Time) string {
	return fmt.Sprintf("provider limit: %s, tried again %s", strings.Join(keys, " -> "),
		rfc3339(retry))
}

// resetsOf is when limit's vendors said it lifts, for a log line.
func resetsOf(limit chain.Limit) string {
	if limit.ResetsAt.IsZero() {
		return "unknown"
	}
	return rfc3339(limit.ResetsAt)
}

// providerParked is the event a park publishes, its seat fields filled by
// [Engine.publishProviderPark].
func providerParked(limit chain.Limit) types.SeatProviderParked {
	parked := types.SeatProviderParked{
		ProviderKeys: limit.Keys, RetryAt: limit.RetryAt.UTC(), Said: limit.Said,
		CLI: limit.Agent, Subscription: limit.Subscription,
	}
	if !limit.ResetsAt.IsZero() {
		resets := limit.ResetsAt.UTC()
		parked.ResetsAt = &resets
	}
	return parked
}

// releaseProviderPark lifts the provider hold from one seat's inbox, whatever
// record holds it, reporting whether it did; see
// [Engine.releaseProviderParking].
func (e *Engine) releaseProviderPark(ctx context.Context, seat uuid.UUID, why string) bool {
	if parking := e.providerParks.parking(seat); parking != nil {
		return e.releaseProviderParking(ctx, seat, parking, why)
	}
	return false
}

// releaseProviderParking lifts the hold parking took, if it is still the
// seat's park, says so, and signals the resumes waiting on a limit (see the
// file's doc), reporting whether it did.
func (e *Engine) releaseProviderParking(ctx context.Context, seat uuid.UUID, parking *providerParking, why string) bool {
	if !e.providerParks.releaseParking(ctx, e, providerParkSpec, seat, parking, why) {
		return false
	}
	e.leaveLimitProbe(seat, parking.basis)
	// RELEASED, NOT ANSWERED: a park soon after continues this wait
	// ([limitProbes.waits]); one that ends it forgets it after this.
	e.parkReleased(seat)
	e.publishProviderPark(ctx, parking.handle, types.SeatProviderReleased{Reason: why})
	e.readmitResumes(waitProvider)
	return true
}

// reconcileProviderParks releases every park decided under models next does
// not have (see the file's doc). Run on every published company, after next is
// current, beside [Engine.reconcileBudgetParks].
func (e *Engine) reconcileProviderParks(ctx context.Context, next *Company) {
	var models *phase.Registry
	if next != nil {
		models = next.Models
	}
	for _, seat := range e.providerParks.changed(func(_ uuid.UUID, basis providerBasis) bool {
		return basis.models == models
	}) {
		e.releaseProviderPark(ctx, seat, "the seat's models changed")
	}
}

// forgetProviderPark drops a released seat's park without resuming anything
// (see [seatParks.forget]), and says the seat no longer waits here: the node
// that takes it asks its own models on the first delivery.
func (e *Engine) forgetProviderPark(ctx context.Context, seat uuid.UUID) {
	e.endParkWait(seat)
	if parking, ok := e.providerParks.forget(ctx, e, providerParkSpec, seat); ok {
		e.leaveLimitProbe(seat, parking.basis)
		e.publishProviderPark(ctx, parking.handle, types.SeatProviderReleased{Reason: seatReleasedHere})
	}
}

// stopProviderParks disarms every probe's alarm and ends every probe's call,
// for a node shutting down, and returns once those calls have (see
// [seatParks.stop] for the holds). The wait is outside the lock, which a
// probe's own settling takes.
func (e *Engine) stopProviderParks() {
	e.providerParks.stop()
	p := &e.limitProbes
	p.mu.Lock()
	p.stopped = true
	for _, probe := range p.probes {
		if probe.alarm != nil {
			probe.alarm.Stop()
			probe.alarm = nil
		}
		if probe.call != nil {
			probe.call.cancel()
		}
	}
	p.mu.Unlock()
	p.asking.Wait()
}

// publishProviderPark announces a provider park or its release for handle's
// seat, best effort: the park holds whether or not a screen hears of it.
func (e *Engine) publishProviderPark(ctx context.Context, handle string, payload events.Payload) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	role := e.seatRole(handle)
	agentID := e.seatAgentID(role)
	if role == nil || agentID == "" {
		return
	}
	switch p := payload.(type) {
	case types.SeatProviderParked:
		p.Agent, p.AgentHandle, p.RoleName = agentID, role.Handle(), role.Name
		payload = p
	case types.SeatProviderReleased:
		p.Agent, p.AgentHandle, p.RoleName = agentID, role.Handle(), role.Name
		payload = p
	default:
		return
	}
	ev := events.NewFrom(payload, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	ev.Source = role.Name
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "seat_provider_park_not_published", "type", ev.Type,
			"seat", handle, "error", err)
	}
}

// parkedByEntry is every seat this node holds parked on a limited chain, under
// each entry the chain names, ordered by seat id: what the heartbeat's model
// rows publish ([coord.ModelStatus.Parked]).
//
// UNDER EVERY ENTRY OF THE CHAIN, because a seat parks only once every member
// it could fall back to is limited: each of them is what the seat waits on.
func (e *Engine) parkedByEntry() map[string][]coord.ParkedSeat {
	p := &e.limitProbes
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string][]coord.ParkedSeat{}
	for _, probe := range p.probes {
		for seat, since := range probe.since {
			for _, key := range probe.basis.keys {
				out[key] = append(out[key], coord.ParkedSeat{Seat: seat, Since: since})
			}
		}
	}
	// A WAIT BETWEEN A RELEASE AND ITS RE-PARK is still a wait: the seat is
	// reported on the chain it last parked on, from when it began, for as
	// long as a park would continue it. Left out, a heartbeat in that gap
	// cleared the waiting alarm the re-park then raised again.
	now := e.now()
	for seat, wait := range p.waits {
		if wait.releasedAt.IsZero() || now.Sub(wait.releasedAt) > parkCarryWindow {
			continue
		}
		for _, key := range wait.keys {
			out[key] = append(out[key], coord.ParkedSeat{Seat: seat, Since: wait.since})
		}
	}
	for key := range out {
		slices.SortFunc(out[key], func(a, b coord.ParkedSeat) int {
			return strings.Compare(a.Seat.String(), b.Seat.String())
		})
		out[key] = slices.CompactFunc(out[key], func(a, b coord.ParkedSeat) bool { return a.Seat == b.Seat })
	}
	return out
}

// rearmLimitProbesNaming makes every probe whose chain names entry fire now,
// rather than at the instant it waits for: a person cleared the entry's
// sit-out ([Engine.applyModelRetry]), so a seat parked on it may have a member
// that answers. A chain that is still limited re-parks with the vendor's fresh
// word. A probe whose call is out already is left to it.
func (e *Engine) rearmLimitProbesNaming(ctx context.Context, entry string) int {
	p := &e.limitProbes
	p.mu.Lock()
	defer p.mu.Unlock()
	armed := 0
	for key, probe := range p.probes {
		if probe.call != nil || !slices.Contains(probe.basis.keys, entry) {
			continue
		}
		probe.until = e.now()
		e.armLimitProbeLocked(ctx, key, probe)
		armed++
	}
	return armed
}
