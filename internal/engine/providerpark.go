package engine

import (
	"context"
	"errors"
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
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The provider park: a seat whose models are all limited waits for them, it
// does not retry into the limit. The backend sits a limited entry out until the
// vendor's reset (internal/providers/llm/harness), but a turn whose every chain
// member refused for a rate limit ([chain.Limited]) used to fail and be
// redelivered a second later, so every seat failed a turn about once a second.
// It now parks its seat with the budget park's machinery (seatpark.go): a hold
// under [pauseReasonProvider] on its inbox, its delivery DEFERRED to keep its
// place at the head, and the seat joined to the probe of the chain that
// refused. `seat_provider_parked` says when the vendors said the limit lifts, so
// every screen draws one steady condition. Only a chain limited throughout
// parks: a spent head with a fallback failing for an unlasting reason keeps the
// ordinary retry, and a fallback that answers is working.

// The probe is not the held mail. Lifting the hold to let the head message run
// into the limit spent a delivery per probe and walked it to the dead-letter
// boundary (24 probes for a limit named 40 hours out). So one alarm per limited
// chain, armed for [chain.Limit.RetryAt], makes one short call through the
// refusing members outside any delivery ([Engine.probeLimit], [limitProbes]),
// and its answer settles every seat parked on the same entries, order and
// registry; per-seat alarms made five full harness runs to learn one fact. It
// answers or fails for a non-limit reason: each hold is lifted with
// `seat_provider_released` and the mail redelivered once, priority lane first.
// Limited again: the alarm is re-armed, the parked line republished, no inbox
// touched.

// What a probe costs: a full harness run, charged under the background stage
// (`provider_probe`) to the parked seat that sorts first, on whose harness home
// ([llm.WithSeat]) the call is made. Members still sitting out refuse without
// a process, so a retry instant costs one run for every seat at once.
//
// A resume that meets the limit parks its seat too ([Engine.parkResumeOnLimit]):
// a seat's plan usually runs out inside a run, the segment collecting it benches
// the entry, and its review or relaunch is refused, yet no delivery ran, so
// nothing parked the seat and each held resume ran into the vendor's window at
// every instant the entry was benched until. So does a completion whose review's
// models already know the limit ([Engine.collectedRefusal]), refused after
// collection, not at the turn gate before its claim.

// A parked seat starts no turn by any road: the turn gate refuses every resume
// on it until release ([Engine.providerParkRefusal]); a completion refused
// before its claim is one already collected ([Engine.seatCannotRun]).
// Released by a new set of models: an apply installing another registry
// releases the parks ([Engine.reconcileProviderParks]); the redelivery asks the
// new entries and re-parks at the cost of one delivery and no process, since
// the same account's sit-out lives on the node's registry ([harness.Plans]). A
// park is not carried across: whether the chain still refuses is the new
// company's to say. A seat whose mail its run holds has no delivery to come, so
// the release re-fires its held completion ([Engine.readmitResumes]), refused on
// the carried limit and parking the seat again ([Engine.collectedRefusal]).

// A park outlives no seat. A seat's release detaches its inbox, dropping every
// hold, and forgets its parks ([Engine.forgetProviderPark]), waiting for none of
// the turns that can park it, so a park can land during or after the release. One
// taken while the seat was held asks again once it is whole and undoes itself
// where the release took it ([Engine.unparkReleased]); one asked after the
// detach is refused by the queue and records nothing (seatinbox.go). Left
// standing, its probe kept publishing the seat parked while a peer ran it.
//
// A release also signals the resumes waiting on the same condition
// ([waitProvider]) on any seat, since the entries are the node's, rather than
// leaving them to their own clock. Who has to agree: this node alone, like the
// budget park; a restart or a move asks the models again on the first delivery.

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
	// fleet is the LLM fleet whose members these are, "" for a pinned
	// chain; with no keys, the seat waits on a fleet with no member
	// ([Engine.fleetEmptyPark]), which no probe can answer.
	fleet string
}

// emptyFleet reports whether b holds a seat on a fleet with no member.
func (b providerBasis) emptyFleet() bool { return b.fleet != "" && len(b.keys) == 0 }

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
	// THE SEAT'S, NOT A PROBE'S: an apply that brings new models re-parks
	// the seat at once under a new probe, and dated by the probe the wait
	// restarted at every apply. A wait ends when the chain answers (or stops
	// reporting a limit) or the seat leaves this node; a park that follows a
	// release within [parkCarryWindow] is the same wait, and so is one taken
	// in a release's own window ([Engine.unlessParked]). The package doc's
	// "A provider park's release and its wait" says more.
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
	basis := providerBasis{keys: slices.Clone(limit.Keys), fleet: limit.Fleet}
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
	if basis.fleet != "" {
		e.publishFleetFull(ctx, basis, limit)
	}

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

// fleetEmptyPark is the dispatcher's stage for a seat job on an LLM fleet with
// no member: its inbox is held as a provider park holds it, with no probe,
// since nothing but a member joining ends the wait, and the apply that adds
// one releases it ([Engine.reconcileProviderParks]). It publishes no park:
// the seat reads the fleet's own "No members yet".
func (e *Engine) fleetEmptyPark(ctx context.Context, handle, fleet string) (string, bool, error) {
	seat, err := e.seatID(handle)
	if err != nil {
		return "", false, err
	}
	c := e.Company()
	basis := providerBasis{fleet: fleet}
	if c != nil {
		basis.models = c.Models
	}
	if old := e.providerParks.parking(seat); old != nil && probeKeyOf(old.basis) != probeKeyOf(basis) {
		e.leaveLimitProbe(seat, old.basis)
	}
	if err := e.providerParks.take(ctx, e, providerParkSpec, seat, handle, basis, time.Time{},
		"its fleet has a member"); err != nil {
		return "", false, fmt.Errorf("engine: hold %s until fleet %s has a member: %w", handle, fleet, err)
	}
	reason := emptyFleetReason(fleet)
	log.InfoContext(ctx, "seat_fleet_empty_held", "seat", handle, "fleet", fleet,
		"detail", "this seat's mail waits on its inbox until a model joins its fleet")
	if _, held := e.launchFence(handle); !held {
		e.unparkReleased(ctx, seat, handle, basis)
		return reason, true, nil
	}
	if now := e.Company(); now != nil && now.Models != basis.models {
		e.releaseProviderPark(ctx, seat, "the seat's models changed while it waited")
	}
	return reason, true, nil
}

// emptyFleetReason is what a seat held on an empty fleet is deferred for.
func emptyFleetReason(fleet string) string {
	return fmt.Sprintf("fleet %s has no members yet: the seat waits until one joins", fleet)
}

// unparkReleased undoes a provider park taken on a seat this node no longer
// holds ([Engine.providerPark]): its hold lifted, its place on the chain's
// probe left, the seat said released and its wait forgotten, as the release
// does with a park it forgets ([Engine.forgetProviderPark]).
//
// WHETHER OR NOT ITS RECORD IS LEFT, and the wait either way: the release's
// forget can land between the park's take and this, dropping the record and
// the wait before the park joined the probe, said the seat parked and began
// the wait again, and left, the probe called the models as the seat and the
// wait outlived it here. The package doc's "A provider park's release and its
// wait" says why.
func (e *Engine) unparkReleased(ctx context.Context, seat uuid.UUID, handle string, basis providerBasis) {
	if !e.releaseProviderPark(ctx, seat, seatReleasedHere) {
		e.leaveLimitProbe(seat, basis)
		if !basis.emptyFleet() {
			e.publishProviderPark(ctx, handle, types.SeatProviderReleased{Reason: seatReleasedHere})
		}
	}
	e.forgetParkWait(seat)
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
		return
	}
	// A FLEET WITH NO MEMBER holds the seat as a dispatch would.
	var empty *phase.EmptyFleetError
	if errors.As(err, &empty) {
		if _, _, err := e.fleetEmptyPark(ctx, handle, empty.Fleet); err != nil {
			log.WarnContext(ctx, "seat_fleet_empty_not_held", "seat", handle, "fleet", empty.Fleet,
				"error", err.Error())
		}
	}
}

// parkOnLimit parks handle's seat on limit for a resume, which no delivery
// carries: the limit its segment met ([Engine.parkResumeOnLimit]), or the one
// its collected completion's review would meet ([Engine.collectedRefusal]).
// Nothing for a limit that names no instant, which no probe could be armed for.
//
// THE PARK IS NO DELIVERY'S, so a hold it could not take is said and
// otherwise left: the resume is reverted and waits on its refusal either way.
// A seat this node has let go of under the resume is refused its park, which
// the queue refuses for the ended attachment its hold names; that is said at
// debug, since the node that takes the seat asks its own models (the package
// doc's "A provider park's release and its wait").
func (e *Engine) parkOnLimit(ctx context.Context, handle string, limit chain.Limit) {
	_, _, err := e.providerPark(ctx, handle, limit)
	switch {
	case errors.Is(err, queue.ErrAttachmentGone):
		log.DebugContext(ctx, "seat_provider_park_attachment_ended", "seat", handle,
			"providers", strings.Join(limit.Keys, ","),
			"detail", "the seat was let go of here under its resume; the node that takes "+
				"it asks its own models on its first delivery")
	case err != nil:
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
// Every resume is refused whichever chain its first call takes: a completion
// whose reviewer is ready would otherwise be admitted by its own chain
// ([Engine.providerRefusing]) into a window the probe found still spent. Until
// is an instant still to come, or none: while the probe's call is out the
// instant is past and the park stands, and naming it made every waiting answer
// re-check thousands of times a second. A due probe names nothing
// ([Engine.limitProbeNext]); its answer, the release, ends the wait.
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
	if parking.basis.emptyFleet() {
		reason = emptyFleetReason(parking.basis.fleet)
	}
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
	// THE PARKED SEAT THAT SORTS FIRST runs the call, on its harness home and
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
	answered, err := e.probeModels(ctx, c, handle, probe.basis.keys)
	if ctx.Err() != nil {
		return
	}
	if limit, limited := chain.Limited(err); limited {
		// THE NEXT CALL IS STILL TO COME. A chain that refused while naming
		// an instant already past cannot say when its members are tried
		// again, and a probe armed for that instant is due at once: it
		// asked, was refused and asked again as fast as the call returned,
		// each pass renewing every seat's park. That is what a host that
		// slept inside a window did, its entries refusing on the monotonic
		// clock while naming an end already past on the wall clock
		// (harness's onTheWall). So such a probe waits the shortest window
		// any entry sits out, and says so once per renewal.
		if now := e.now(); !limit.RetryAt.After(now) {
			log.WarnContext(settle, "seat_provider_probe_retry_past",
				"providers", strings.Join(limit.Keys, ","), "retry_at", rfc3339(limit.RetryAt),
				"retry_in", harness.ShortestSitOut.String(),
				"detail", "the probe met the limit again and its models named an instant "+
					"already past for their next try; it asks again after the shortest "+
					"window an entry sits out, rather than at once")
			limit.RetryAt = now.Add(harness.ShortestSitOut)
		}
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
	if err == nil && probe.basis.fleet != "" {
		e.publishFleetRoom(settle, probe, answered)
	}
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
		// limit, unless the seat met it again in the release's window and
		// stands parked on it ([Engine.unlessParked]). After the release,
		// whose park, standing, would keep it.
		e.endParkWait(seat)
	}
}

// parkReleased marks seat's wait as released at this instant, without ending
// it: a park within [parkCarryWindow] continues it. A park of the seat that
// stands keeps it unmarked ([Engine.unlessParked]).
func (e *Engine) parkReleased(seat uuid.UUID) {
	e.unlessParked(seat, func(p *limitProbes) {
		if wait, known := p.waits[seat]; known {
			wait.releasedAt = e.now()
			p.waits[seat] = wait
		}
	})
}

// endParkWait forgets when seat began waiting, its wait over, unless a park of
// the seat stands, which keeps it ([Engine.unlessParked]).
func (e *Engine) endParkWait(seat uuid.UUID) {
	e.unlessParked(seat, func(p *limitProbes) { delete(p.waits, seat) })
}

// unlessParked settles seat's wait record under the probes' lock, unless a
// provider park of the seat stands, which owns the record.
//
// A release marks the wait released, or a probe's answer ends it, only after
// its resume, and a turn that meets the limit again in between parks the seat
// on the record first: landed on that park's record, the late mark or end made
// its next re-park begin a new wait, though the seat had waited throughout.
// UNDER THE PARKS' LOCK, which a park records itself under before it joins a
// probe, taken before the probes' one here as in [seatParks.releaseParking],
// and nowhere the other way round. The package doc's "A provider park's
// release and its wait" says why.
func (e *Engine) unlessParked(seat uuid.UUID, settle func(p *limitProbes)) {
	parks := &e.providerParks
	parks.mu.Lock()
	defer parks.mu.Unlock()
	if parks.seats[seat] != nil {
		return
	}
	p := &e.limitProbes
	p.mu.Lock()
	defer p.mu.Unlock()
	settle(p)
}

// forgetParkWait forgets when seat began waiting, whatever park stands: the
// seat has left this node ([Engine.forgetProviderPark],
// [Engine.unparkReleased]).
func (e *Engine) forgetParkWait(seat uuid.UUID) {
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
// handle's own harness home, and names the member that answered. A key the
// registry no longer has is an error that is not a limit, which lifts the park.
func (e *Engine) probeModels(ctx context.Context, c *Company, handle string, keys []string) (string, error) {
	members := make([]chain.Member, 0, len(keys))
	for _, key := range keys {
		provider, ok := c.Models.Provider(key)
		if !ok {
			return "", fmt.Errorf("engine: provider %q is no longer configured", key)
		}
		members = append(members, chain.Member{Key: key, Provider: provider})
	}
	seam, ok := e.seamFor(c)
	if !ok {
		return "", fmt.Errorf("engine: the company has no models to probe")
	}
	members, err := seam.recordedMembers(e.seatRole(handle), members,
		auxspend.Use{Stage: types.AuxStageBackground, Purpose: types.AuxProviderProbe})
	if err != nil {
		return "", err
	}
	probe, err := chain.New(members, chain.Options{})
	if err != nil {
		return "", err
	}
	answer, err := probe.Complete(llm.WithSeat(ctx, handle), providerProbe)
	if err != nil {
		return "", err
	}
	return answer.ProviderKey, nil
}

// providerRefusing reports whether every model the first call of route's turn
// on handle's seat could take is at a usage limit it already reported, refusing
// on [waitProvider] with the reason naming them and when the first is tried
// again: the provider stage's question without a delivery, for a resume with no
// mail to hold ([Engine.budgetHold]'s twin), asking only what the entries know
// ([chain.Unready]). It takes the chain of the route's first call
// ([firstCallMembers]), which differs where the seat names its own reviewer: a
// seat whose executor sits out a spent plan while its `llm_review` answers has
// a run to review, and held on the executor's limit it waited out the vendor's
// window. A later call meets its own limit mid-turn ([resumeRefusal]). The
// returned [chain.KnownLimit] is what a collected resume parks on
// ([Engine.collectedRefusal]), keyless where a member refuses otherwise.
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
// job is collected, before its turn runs: a usage limit every model its review
// could take already reported ([Engine.providerRefusing],
// [sandbox.ResumeCompletion]), on which the seat is parked at once
// ([Engine.parkOnLimit]). The coordinator holds the job until the refusal's
// instant ([sandbox.Coordinator.OnCompleted]), then the park refuses it at the
// turn gate. After the collection, never at the gate: the claim collects the
// finished job first (charged, box paused, run recorded), else every seat on a
// shared plan left later completions uncollected with boxes awake at every poll
// and seats unparked, in no probe. A seat a person stopped is refused nothing
// ([Engine.seatCannotRun]).
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
		Harness: limit.Harness, Subscription: limit.Subscription,
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
//
// THE SEAT TAKEN OFF ITS PROBE AND ITS RELEASE NOTED AS THE RECORD IS DROPPED,
// under the parks' lock ([seatParks.releaseParking]), and the notice published
// once the release has let go: a park taken in the release's window joins the
// probe and notes the seat parked after the drop, where after the queue's
// resume the release would take it off that probe and draw it released. The
// probes' and the notices' locks are taken under the parks' one here and
// nowhere the other way round (the package doc's "A provider park's release
// and its wait").
func (e *Engine) releaseProviderParking(ctx context.Context, seat uuid.UUID, parking *providerParking, why string) bool {
	released := e.providerParks.releaseParking(ctx, e, providerParkSpec, seat, parking, why, func() {
		e.leaveLimitProbe(seat, parking.basis)
		if !parking.basis.emptyFleet() {
			e.noteProviderPark(ctx, parking.handle, types.SeatProviderReleased{Reason: why})
		}
	})
	e.parkNotices.flush(ctx, e.publishParkNotice)
	if !released {
		return false
	}
	// RELEASED, NOT ANSWERED: a park soon after continues this wait
	// ([limitProbes.waits]); one that ends it forgets it after this. A park
	// taken in this release's window stands on the wait, and is left it
	// ([Engine.unlessParked]).
	e.parkReleased(seat)
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
	e.forgetParkWait(seat)
	if parking, ok := e.providerParks.forget(seat); ok {
		e.leaveLimitProbe(seat, parking.basis)
		if !parking.basis.emptyFleet() {
			e.publishProviderPark(ctx, parking.handle, types.SeatProviderReleased{Reason: seatReleasedHere})
		}
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
	// AFTER THE PROBES, whose settling posts notices: what they held is
	// dropped with the node's seats.
	e.parkNotices.stop()
}

// publishProviderPark announces a provider park or its release for handle's
// seat through [parkNotices]: once per change, and held and retried on a
// backoff while the broker refuses. The park holds whether or not a screen
// hears of it.
func (e *Engine) publishProviderPark(ctx context.Context, handle string, payload events.Payload) {
	e.noteProviderPark(ctx, handle, payload)
	e.parkNotices.flush(ctx, e.publishParkNotice)
}

// noteProviderPark hands handle's seat's notice to [parkNotices] without
// publishing it, for a release that decides it under the parks' lock and
// publishes it once it has let go ([Engine.releaseProviderParking]).
func (e *Engine) noteProviderPark(ctx context.Context, handle string, payload events.Payload) {
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
	e.parkNotices.note(ctx, handle, ev)
}

// publishParkNotice hands one park notice to the broker.
func (e *Engine) publishParkNotice(ctx context.Context, ev *events.Event) error {
	return e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev)
}

// parkedByEntry is every seat this node holds parked on a limited chain, under
// each entry the chain names, ordered by seat id: what the status record's model
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
