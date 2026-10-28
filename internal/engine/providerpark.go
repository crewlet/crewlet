package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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
//     ([chain.Limit.RetryAt]), which lifts the hold for a probe: the mail is
//     delivered again in order, once, and a turn that meets the same limit
//     parks the seat again until the next instant;
//   - and `seat_provider_parked` says so, with when the vendors said the
//     limit lifts, so every screen draws one steady condition rather than a
//     seat that blinks.
//
// A PROBE PUBLISHES NOTHING when it lifts the hold. The seat is still limited
// as far as anybody knows until a call through its chain answers, which the
// seat's own phase completing says; a release event at each probe would make
// the seat blink at every probe instead of at every second.
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
// entries refused. A park whose registry is no longer current was decided by
// entries that no longer serve the seat.
type providerBasis struct {
	models *phase.Registry
}

// providerParks is every seat inbox this node holds because every model of the
// seat's chain is limited, by the seat's id.
type providerParks = seatParks[providerBasis]

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
	var basis providerBasis
	if c != nil {
		basis.models = c.Models
	}
	if err := e.providerParks.take(ctx, e, providerParkSpec, seat, handle, basis, limit.RetryAt,
		"its models are tried again"); err != nil {
		return "", false, fmt.Errorf("engine: park %s on its models' limit: %w", handle, err)
	}

	resets := "unknown"
	if !limit.ResetsAt.IsZero() {
		resets = rfc3339(limit.ResetsAt)
	}
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

// providerParked is the event a park publishes, its seat fields filled by
// [Engine.publishProviderPark].
func providerParked(limit chain.Limit) types.SeatProviderParked {
	parked := types.SeatProviderParked{
		ProviderKeys: limit.Keys, RetryAt: limit.RetryAt.UTC(), Said: limit.Said,
	}
	if !limit.ResetsAt.IsZero() {
		resets := limit.ResetsAt.UTC()
		parked.ResetsAt = &resets
	}
	return parked
}

// releaseProviderPark lifts the provider hold from one seat's inbox for a
// reason other than its probe, and says so.
func (e *Engine) releaseProviderPark(ctx context.Context, seat uuid.UUID, why string) {
	parking := e.providerParks.parking(seat)
	if parking == nil {
		return
	}
	if e.providerParks.releaseParking(ctx, e, providerParkSpec, seat, parking, why) {
		e.publishProviderPark(ctx, parking.handle, types.SeatProviderReleased{Reason: why})
	}
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
