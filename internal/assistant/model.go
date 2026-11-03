package assistant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// WHAT A PERSON IS TOLD WHEN THE MODEL CANNOT ANSWER, in one place. A turn
// whose round failed and a question refused before any round (the chain known
// unusable now, [Service.ModelNotice]) say the same sentence for the same
// failure, because both are [modelProblemOf] reading the backend's own
// classification ([llm.ErrorKind], [llm.ErrNotRunnable]) and never its prose.
//
// THE HINT FOLLOWS WHO IS ASKING: an administrator is told where to fix it,
// and anybody else whom to ask, since only an administrator can sign a model
// in or change the chain.

// modelProblem is a model failure as a person reads it.
type modelProblem struct {
	Code    wire.ErrorCode
	Message string
	Hint    string
	// Retryable is whether asking again can help without anybody acting:
	// not for a signed-out model or a harness that cannot start.
	Retryable bool
	// RetryAt is when a spent plan is tried again, zero where unknown.
	RetryAt time.Time
	// RetryAfter is the wait a generic failure is given, zero for none.
	RetryAfter time.Duration
}

// modelRetrySeconds is how long a person is told to wait after the model chain
// failed for a reason no code names: long enough for a harness's restart.
const modelRetrySeconds = 10

// modelProblemOf is THE mapping from a model failure to what the person reads,
// or false for an error that is not a model failure (no [llm.Error] in it).
// admin is whether the person holds the admin grant.
func modelProblemOf(err error, admin bool) (modelProblem, bool) {
	var failure *llm.Error
	if !errors.As(err, &failure) {
		return modelProblem{}, false
	}
	key := failure.Key
	switch {
	case failure.Kind == llm.KindAuth:
		hint := "Ask an administrator to sign Ask Crewlet's model in again."
		if admin {
			hint = "Sign it in again on the engine's host with " + loginCommand(key) +
				", then ask again."
		}
		return modelProblem{Code: wire.ErrorModelSignedOut,
			Message: wire.ErrorModelSignedOut.Sentence(), Hint: hint}, true
	case failure.Kind == llm.KindRateLimit:
		hint := "It is tried again once the limit resets."
		if admin {
			hint += " To keep Ask Crewlet answering through a limit, add another model to assistant.llm."
		}
		return modelProblem{Code: wire.ErrorModelBusy,
			Message: wire.ErrorModelBusy.Sentence(), Hint: hint,
			Retryable: true, RetryAt: failure.RetryAt}, true
	case failure.Kind == llm.KindTimeout:
		hint := "Ask again, or ask a narrower question."
		if admin && key != "" {
			hint += fmt.Sprintf(" If this keeps happening, raise providers.llm.%s.timeout_seconds.", key)
		}
		return modelProblem{Code: wire.ErrorModelTimeout,
			Message: wire.ErrorModelTimeout.Sentence(), Hint: hint,
			Retryable: true}, true
	case errors.Is(failure, llm.ErrNotRunnable):
		hint := "Ask an administrator to check Ask Crewlet's model."
		if admin {
			hint = "Install its command line tool on the engine's host, then check it with " +
				doctorCommand(key) + "."
		}
		return modelProblem{Code: wire.ErrorModelUnavailable,
			Message: "Ask Crewlet's model cannot be started on this node.", Hint: hint}, true
	}
	var hint string
	if admin {
		hint = doctorCommand(key) + " on the engine's host shows why."
	}
	return modelProblem{Code: wire.ErrorModelUnavailable,
		Message: wire.ErrorModelUnavailable.Sentence(), Hint: hint, Retryable: true,
		RetryAfter: modelRetrySeconds * time.Second}, true
}

// loginCommand is the command that signs an entry in, naming its key where the
// failure carried one.
func loginCommand(key string) string {
	if key == "" {
		return "`crewlet llm login <provider>`"
	}
	return "`crewlet llm login " + key + "`"
}

// doctorCommand is the command that diagnoses an entry.
func doctorCommand(key string) string {
	if key == "" {
		return "`crewlet llm doctor`"
	}
	return "`crewlet llm doctor " + key + "`"
}

// event is the problem as a turn's error frame, its wait counted from now.
func (m modelProblem) event(now time.Time) *wire.ErrorEvent {
	ev := &wire.ErrorEvent{Code: m.Code, Message: m.Message, Hint: m.Hint, Retryable: m.Retryable}
	switch {
	case !m.RetryAt.IsZero():
		ev.RetryAfterSeconds = max(int((m.RetryAt.Sub(now)+time.Second-1)/time.Second), 1)
	case m.RetryAfter > 0:
		ev.RetryAfterSeconds = int(m.RetryAfter / time.Second)
	}
	return ev
}

// notice is the problem as the `/viewer` answer tells it before a question.
func (m modelProblem) notice() *wire.ModelNotice {
	n := &wire.ModelNotice{Code: m.Code, Message: m.Message, Hint: m.Hint}
	if !m.RetryAt.IsZero() {
		at := m.RetryAt.UTC()
		n.RetryAt = &at
	}
	return n
}

// ModelNotReady is a question refused before its first round because every
// model in the chain is known unusable now. It wraps [ErrUnavailable].
type ModelNotReady struct {
	Notice wire.ModelNotice
	// RetryAfter is the wait until a spent plan resets, zero where asking
	// again needs somebody to act first.
	RetryAfter time.Duration
	Cause      error
}

func (e *ModelNotReady) Error() string {
	return fmt.Sprintf("%s: the model is not ready (%s): %v", ErrUnavailable, e.Notice.Code, e.Cause)
}

// Unwrap makes [ModelNotReady] errors.Is-comparable with [ErrUnavailable] and
// with its cause.
func (e *ModelNotReady) Unwrap() []error { return []error{ErrUnavailable, e.Cause} }

// ModelNotice is what p is told before they ask when every model in the
// assistant's chain is known unusable now, or nil when none is known to be.
// Read from what the node already holds ([Models.Unready]): no model is called.
func (s *Service) ModelNotice(ctx context.Context, p iam.Principal) *wire.ModelNotice {
	if problem, _ := s.modelUnready(ctx, p); problem != nil {
		return problem.notice()
	}
	return nil
}

// modelUnready is the chain's known problem for p and the failure it reads,
// or nil for none.
func (s *Service) modelUnready(ctx context.Context, p iam.Principal) (*modelProblem, error) {
	err := s.models.Unready(ctx)
	if err == nil {
		return nil, nil
	}
	problem, ok := modelProblemOf(err, p.Can(iam.GrantAdmin))
	if !ok {
		// Not a classified model failure: the chain could not be read
		// for readiness, which the turn finds out for itself rather than
		// a reason to refuse it.
		s.log.WarnContext(ctx, "assistant_model_readiness_unread", "error", err)
		return nil, nil
	}
	return &problem, err
}

// modelRefusal is the refusal of a question asked while the chain is known
// unusable, or nil.
func (s *Service) modelRefusal(ctx context.Context, p iam.Principal) error {
	problem, cause := s.modelUnready(ctx, p)
	if problem == nil {
		return nil
	}
	refusal := &ModelNotReady{Notice: *problem.notice(), Cause: cause}
	if !problem.RetryAt.IsZero() {
		refusal.RetryAfter = max(problem.RetryAt.Sub(s.now()), time.Second)
	}
	return refusal
}
