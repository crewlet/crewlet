package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A turn failure that will happen again until a person changes something
// pauses its seat on its second time, rather than being redelivered into the
// same refusal until its deliveries run out (ADR-0117).

// failureKind is a turn failure a retry cannot cure.
type failureKind string

// The failures that will repeat, each the seat's own: its sandbox or its models.
const (
	failureSandboxSettings failureKind = "sandbox_configuration"
	failureSandboxRefused  failureKind = "sandbox_refused"
	failureSandboxBilling  failureKind = "sandbox_billing"
	failureModelsRefused   failureKind = "models_refused"
)

// failureRule is what a seat's pause for a kind says, and whether the failure
// depends on the seat's models rather than its sandbox.
type failureRule struct {
	says   string
	models bool
}

// failureRules is the closed table of failures that pause their seat.
var failureRules = map[failureKind]failureRule{
	failureSandboxSettings: {says: "its sandbox could not start as configured, twice in a row"},
	failureSandboxRefused:  {says: "the sandbox provider refused the engine's credential, twice in a row"},
	failureSandboxBilling:  {says: "the sandbox provider refused for billing, twice in a row"},
	failureModelsRefused: {says: "every model it can use refused its credential, twice in a row; " +
		"resume it once the credential is fixed", models: true},
}

// whyFailures classifies every [sandbox.Why]: an empty kind is a failure that
// waits on something outside the company, which a retry may outlast.
var whyFailures = map[sandbox.Why]failureKind{
	sandbox.WhyConfiguration:       failureSandboxSettings,
	sandbox.WhyProviderRefused:     failureSandboxRefused,
	sandbox.WhyProviderBilling:     failureSandboxBilling,
	sandbox.WhySetupUnavailable:    "",
	sandbox.WhyProviderLimit:       "",
	sandbox.WhyProviderUnavailable: "",
}

// repeatingFailure is the failure err is when a retry cannot cure it.
func repeatingFailure(err error) (failureKind, bool) {
	if why, ok := sandbox.WhyOf(err); ok {
		kind := whyFailures[why]
		return kind, kind != ""
	}
	if modelsRefused(err) {
		return failureModelsRefused, true
	}
	return "", false
}

// modelsRefused is whether every member of the turn's chain refused its credential.
func modelsRefused(err error) bool {
	var refused *chain.Error
	if !errors.As(err, &refused) || len(refused.Failures) == 0 {
		return false
	}
	for _, f := range refused.Failures {
		if llm.KindOf(f) != llm.KindAuth {
			return false
		}
	}
	return true
}

// boundToModels is whether kind's failure depends on the seat's models rather
// than its sandbox; a context that does not fit, which holds only its
// conversation and so is not in the table, always does, and work that does not
// fit its box's memory never does.
func boundToModels(kind failureKind) bool {
	return kind == failureContextExhausted || failureRules[kind].models
}

// failureBasis fingerprints the settings kind's failure depends on for handle
// in c: the seat's models and the company's, or its sandbox and the company's.
func failureBasis(c *Company, handle string, kind failureKind) (string, bool) {
	if c == nil || c.Config == nil {
		return "", false
	}
	for role := range c.Config.EachRole() {
		if role.Seat().Handle() != handle {
			continue
		}
		settings := []any{role.Sandbox, c.Config.Providers.Sandbox}
		if boundToModels(kind) {
			settings = []any{role.LLM, c.Config.Providers.LLM}
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			return "", false
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:]), true
	}
	return "", false
}

// lastFailures is each seat's latest failure that will repeat, on this node.
type lastFailures struct {
	mu   sync.Mutex
	last map[uuid.UUID]coord.SeatFailure
}

// repeat records f as seat's latest failure and reports whether it is the same
// failure on the same settings as the one before it.
func (l *lastFailures) repeat(seat uuid.UUID, f coord.SeatFailure) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[uuid.UUID]coord.SeatFailure{}
	}
	before, seen := l.last[seat]
	l.last[seat] = f
	return seen && before == f
}

// forget ends seat's run of failures: its turn worked, or failed otherwise.
func (l *lastFailures) forget(seat uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, seat)
}

// failureCleared is the dispatcher's [Dispatcher.FailureCleared]: a handle
// that names no seat has no run of failures to end.
func (e *Engine) failureCleared(handle string) {
	if seat, err := e.seatID(handle); err == nil {
		e.failures.forget(seat)
	}
}

// failureHold is the dispatcher's [Dispatcher.Failure]: a failure that will
// repeat pauses handle's seat on its second time in a row.
func (e *Engine) failureHold(ctx context.Context, handle string, failed error) (string, bool, error) {
	seat, err := e.seatID(handle)
	if err != nil {
		return "", false, err
	}
	kind, repeats := repeatingFailure(failed)
	if !repeats {
		e.failures.forget(seat)
		return "", false, nil
	}
	basis, known := failureBasis(e.Company(), handle, kind)
	if !known {
		e.failures.forget(seat)
		return "", false, nil
	}
	failure := coord.SeatFailure{Kind: string(kind), Basis: basis}
	if !e.failures.repeat(seat, failure) {
		return "", false, nil
	}
	return e.pauseForFailure(ctx, seat, handle, failure)
}

// pauseForFailure pauses seat for failure, as a person's pause holds it: the
// record is the fleet's, so a restart or another node holding the seat keeps it.
func (e *Engine) pauseForFailure(ctx context.Context, seat uuid.UUID, handle string,
	failure coord.SeatFailure) (string, bool, error) {

	if e.backends == nil || e.backends.Fleet == nil {
		return "", false, nil
	}
	reason := "Paused by the engine: " + failureRules[failureKind(failure.Kind)].says + "."
	p, created, err := e.backends.Fleet.CreateSeatPause(ctx, coord.SeatPause{
		Seat: seat, By: engineActor, ByKind: iam.ActorSystem, Reason: reason, At: time.Now().UTC(),
		Failure: &failure,
	})
	if err != nil {
		return "", false, fmt.Errorf("engine: pause %s for %s: %w", handle, failure.Kind, err)
	}
	e.failures.forget(seat)
	if created {
		log.InfoContext(ctx, "seat_failure_paused", "seat", handle, "failure", failure.Kind,
			"detail", "the seat takes no work until its settings change or a person resumes it")
		e.announceSeatPause(ctx, events.New(types.SeatPaused{Agent: seat.String(), AgentHandle: handle,
			RoleName: e.roleName(seat, handle), PausedBy: p.By, PausedByKind: string(p.ByKind),
			Reason: p.Reason, PausedAt: p.At}, events.TraceContext{}))
	}
	if err := e.holdPausedInbox(ctx, handle); err != nil {
		return "", false, fmt.Errorf("engine: hold %s's inbox for its pause: %w", handle, err)
	}
	return reason, true, nil
}

// engineActor is who a pause the engine takes or lifts names.
const engineActor = "engine"

// liftChangedFailurePauses lifts every failure's pause whose settings next
// changes: the change is what the failure waited on, and a failure that
// still holds pauses the seat again on its second time.
func (e *Engine) liftChangedFailurePauses(ctx context.Context, next *Company) {
	if next == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if err := e.fleetCompany(ctx, next); err != nil {
		return
	}
	e.pauses.mu.Lock()
	var changed []coord.SeatPause
	for seat, p := range e.pauses.pauses {
		if p.Failure == nil || next.Org == nil || next.Org.AgentSeatByID(seat) == nil {
			continue
		}
		handle := next.Org.AgentSeatByID(seat).Handle()
		if basis, ok := failureBasis(next, handle, failureKind(p.Failure.Kind)); ok && basis != p.Failure.Basis {
			changed = append(changed, p)
		}
	}
	e.pauses.mu.Unlock()
	for _, p := range changed {
		lifted, err := e.backends.Fleet.DeleteSeatPause(ctx, p.Seat, p.Version)
		if err != nil {
			log.WarnContext(ctx, "seat_failure_pause_not_lifted", "agent_id", p.Seat.String(),
				"error", err.Error(), "detail", "the seat stays paused until the next apply or a person resumes it")
			continue
		}
		if !lifted {
			continue
		}
		handle := next.Org.AgentSeatByID(p.Seat).Handle()
		log.InfoContext(ctx, "seat_failure_pause_lifted", "seat", handle, "failure", p.Failure.Kind,
			"detail", "the settings the failure depended on changed")
		e.announceSeatPause(ctx, events.New(types.SeatResumed{Agent: p.Seat.String(), AgentHandle: handle,
			RoleName: e.roleName(p.Seat, handle), ResumedBy: engineActor,
			ResumedByKind: string(iam.ActorSystem), PausedBy: p.By, PausedByKind: string(p.ByKind),
			PausedAt: p.At}, events.TraceContext{}))
	}
}

// roleName is what screens call seat: its role's name, or its handle.
func (e *Engine) roleName(seat uuid.UUID, handle string) string {
	if c := e.Company(); c != nil && c.Org != nil {
		if role := c.Org.AgentSeatByID(seat); role != nil {
			return role.Name
		}
	}
	return handle
}

// announceSeatPause publishes a pause or its lift the engine made, best effort:
// the pause is the coordination record, and a missing feed row is not a lost pause.
func (e *Engine) announceSeatPause(ctx context.Context, ev *events.Event) {
	ev.Source = engineActor
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "seat_pause_not_announced", "type", ev.Type, "error", err.Error(),
			"detail", "the pause itself is recorded; the feed has no row for it")
	}
}
