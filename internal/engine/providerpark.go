package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE PROVIDER PARK: A SEAT WHOSE MODELS ARE ALL LIMITED WAITS FOR THEM, IT
// DOES NOT RETRY INTO THE LIMIT.
//
// A model's usage limit has an end the vendor usually names: Claude Code's
// "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)". The
// backend reads it and sits its entry out until then
// (internal/providers/llm/cliagent), so a call in that window is refused
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
//   - an alarm is armed for the instant the first member is tried again
//     ([chain.Limit.RetryAt]), which PROBES the chain: one short call through
//     the members that refused, outside any delivery;
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
// the alarm asks the models itself ([Engine.probeProviderPark]), and the
// answer decides:
//
//   - the call answers, or fails for a reason that is not a limit: the hold
//     is lifted, `seat_provider_released` says why, and the mail is delivered
//     again in order, once, the held head first, having spent only the one
//     delivery its park did;
//   - every member refuses for a limit again: the alarm is armed for the new
//     instant, `seat_provider_parked` is published again with what the
//     vendors now say, and the inbox is not touched.
//
// The probe is spend like any other auxiliary call, charged and recorded
// through the seam under the background stage (`provider_probe`), and asks
// the members on the seat's own CLI home ([llm.WithSeat]). Members still
// sitting out refuse without a process being started, so a probe costs one
// short call to the member whose window is over.
//
// ONLY A CHAIN THAT IS LIMITED THROUGHOUT. A seat whose head is spent and whose
// fallback failed for a reason nothing says will last keeps the ordinary
// retry, and one whose fallback answers is never parked at all: it is working.
//
// RELEASED BY A NEW SET OF MODELS. Every epoch builds its providers afresh, and
// a fresh provider has sat nothing out, so a park decided under one registry is
// released by an apply that installs another ([Engine.reconcileProviderParks]):
// the redelivery asks the new entries, and a limit that still holds parks the
// seat again at the cost of one delivery.
//
// A RELEASE IS ALSO A SIGNAL to the resumes waiting on the same condition
// ([waitProvider]): a coding run's completion or a person's answer refused
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

// providerProbe is the probe's whole request: a text function whose answer is
// read for nothing but whether it came, so it is as short as a call can be.
var providerProbe = llm.Request{Messages: []llm.Message{
	{Role: llm.RoleSystem, Content: "Reply with the single word: ok"},
	{Role: llm.RoleUser, Content: "ok"},
}}

// providerPark is the dispatcher's provider stage: it parks handle's seat
// until limit's first member is tried again, reporting the deferral reason.
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
	if err := e.providerParks.take(ctx, e, providerParkSpec, seat, handle, basis, limit.RetryAt,
		"its models are tried again", e.probeProviderPark); err != nil {
		return "", false, fmt.Errorf("engine: park %s on its models' limit: %w", handle, err)
	}

	resets := resetsOf(limit)
	reason := fmt.Sprintf("provider limit: %s, tried again %s, resets %s",
		strings.Join(limit.Keys, " -> "), rfc3339(limit.RetryAt), resets)
	log.InfoContext(ctx, "seat_provider_parked", "seat", handle,
		"providers", strings.Join(limit.Keys, ","), "retry_at", rfc3339(limit.RetryAt),
		"resets_at", resets, "said", limit.Said,
		"detail", "this seat's mail waits on its inbox until its models are tried again")
	e.publishProviderPark(ctx, handle, providerParked(limit))
	// THE BASIS AGAIN, after the hold: an apply that installed new models
	// between the turn's failure and the park released nothing, since there
	// was nothing to release yet, and this park would otherwise wait out a
	// limit the new entries may not have.
	if now := e.Company(); now != nil && now.Models != basis.models {
		e.releaseProviderPark(ctx, seat, "the seat's models changed while it parked")
	}
	return reason, true, nil
}

// probeProviderPark is a provider park's alarm: one call through the chain that
// refused, outside any delivery, and the park settled on what it answers (see
// the file's doc). ctx ends if the park is released, given up or stopped while
// the call is out, and whoever did that has already settled it.
func (e *Engine) probeProviderPark(ctx context.Context, seat uuid.UUID, parking *providerParking) {
	// SETTLED ON A CONTEXT THE PARK DOES NOT END: the release cancels the
	// call's context before it lifts the hold.
	settle := context.WithoutCancel(ctx)
	c := e.Company()
	if c == nil || c.Models != parking.basis.models {
		// The apply that installed other models releases this park itself
		// ([Engine.reconcileProviderParks]); one that raced the alarm here
		// is released the same way.
		e.releaseProviderParking(settle, seat, parking, "the seat's models changed")
		return
	}
	err := e.probeModels(ctx, c, parking.handle, parking.basis.keys)
	if ctx.Err() != nil {
		return
	}
	if limit, limited := chain.Limited(err); limited {
		if !e.providerParks.rearm(settle, e, providerParkSpec, seat, parking, limit.RetryAt,
			"its models are tried again") {
			return
		}
		log.InfoContext(settle, "seat_provider_park_renewed", "seat", parking.handle,
			"providers", strings.Join(limit.Keys, ","), "retry_at", rfc3339(limit.RetryAt),
			"resets_at", resetsOf(limit), "said", limit.Said,
			"detail", "the probe met the limit again; this seat's mail stays on its inbox, "+
				"untouched, until its models are tried again")
		e.publishProviderPark(settle, parking.handle, providerParked(limit))
		return
	}
	why := "its models answered"
	if err != nil {
		why = "its models no longer report a usage limit"
		log.InfoContext(settle, "seat_provider_probe_failed", "seat", parking.handle,
			"error", err.Error(),
			"detail", "the probe failed for a reason that is not a usage limit, so the park "+
				"is lifted and the seat's mail meets the failure on the ordinary retry")
	}
	e.releaseProviderParking(settle, seat, parking, why)
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

// providerRefusing reports whether every model of handle's executor chain is
// at a usage limit it already reported, the reason naming it and when the
// first is tried again: the provider stage's question ASKED WITHOUT PARKING,
// for a caller with no mail of its own to hold (a retried resume), which waits
// for the instant rather than parks, as [Engine.budgetRefusing] answers for the
// budget. Asked of what the entries already know ([chain.Unready]), so it
// launches nothing; a chain whose entries know of no limit is not refusing,
// and the resume finds out by running.
func (e *Engine) providerRefusing(ctx context.Context, handle string) (string, time.Time, bool) {
	c := e.Company()
	if c == nil || c.Models == nil {
		return "", time.Time{}, false
	}
	role := e.seatRole(handle)
	if role == nil {
		return "", time.Time{}, false
	}
	members, err := c.Models.Chain(role, phase.Execute)
	if err != nil {
		return "", time.Time{}, false
	}
	providers := make([]llm.Provider, 0, len(members))
	keys := make([]string, 0, len(members))
	for _, m := range members {
		providers = append(providers, m.Provider)
		keys = append(keys, m.Key)
	}
	retry, _, limited := llm.LimitedUntil(chain.Unready(ctx, providers))
	if !limited || retry.IsZero() {
		return "", time.Time{}, false
	}
	return limitReason(keys, retry), retry, true
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
// record holds it; see [Engine.releaseProviderParking].
func (e *Engine) releaseProviderPark(ctx context.Context, seat uuid.UUID, why string) {
	if parking := e.providerParks.parking(seat); parking != nil {
		e.releaseProviderParking(ctx, seat, parking, why)
	}
}

// releaseProviderParking lifts the hold parking took, if it is still the
// seat's park, says so, and signals the resumes waiting on a limit (see the
// file's doc).
func (e *Engine) releaseProviderParking(ctx context.Context, seat uuid.UUID, parking *providerParking, why string) {
	if !e.providerParks.releaseParking(ctx, e, providerParkSpec, seat, parking, why) {
		return
	}
	e.publishProviderPark(ctx, parking.handle, types.SeatProviderReleased{Reason: why})
	e.readmitResumes(waitProvider)
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
	if parking, ok := e.providerParks.forget(seat); ok {
		e.publishProviderPark(ctx, parking.handle,
			types.SeatProviderReleased{Reason: "the seat was released by this node"})
	}
}

// stopProviderParks disarms every alarm, for a node shutting down; see
// [seatParks.stop].
func (e *Engine) stopProviderParks() { e.providerParks.stop() }

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
