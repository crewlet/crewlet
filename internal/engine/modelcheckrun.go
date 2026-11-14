package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A MODEL CHECK RUNS ON THE NODE'S OWN TIME. `POST /llm/providers/{key}/check`
// asks a node to diagnose a model entry ([Engine.CheckModel]): with its smoke
// calls that is up to four harness runs, each measured at 3 to 20 s, inside a
// budget of three minutes, past the two minutes of silence the deployment
// guide tells a proxy in front of the API to allow. So the check is the
// node's, never the request's: the node that takes the request runs it and
// records it in the coordination store ([coord.ModelCheck]), the request is
// answered with the check running, and the person's page reads how it ended
// from whichever node its poll reaches ([Engine.ModelCheck]).
//
// # One check of an entry at a time
//
// The record is filed under the entry, and a start creates it only where no
// check runs. A second ask of the entry as it runs, with the same smoke, is
// handed the running check, which is the check it would make; any other ask —
// a draft, or the other smoke setting — is refused naming the running one
// ([ModelCheckInProgressError]), since that check's answer is not the one it
// asked for. A record whose check has ended, lapsed or been lost is replaced
// at the version read.
//
// # A check outlives nothing it should not
//
// It runs under the node's lifetime and its own budget. A node that stops ends
// its checks with the failure that says so, and one that dies leaves a record
// every reader reads as lost past its deadline ([coord.ModelCheck.Lost]). The
// record of an entry the company removed goes with the apply that removes it
// ([Engine.clearRemovedModelChecks]).

// modelCheckHold is how long a start waits for its check to end before it
// answers with the check running: TWO SECONDS. A check that makes no real call
// reads the harness's version and the node's own files, which takes well under
// that, so it is answered where it was asked; one that makes calls takes three
// seconds at the least, so the hold is never most of what the person waits on.
const modelCheckHold = 2 * time.Second

// modelCheckWriteBudget bounds the write that records how a check ended, made
// after its own context may be gone: TEN SECONDS, a few store round trips, the
// device sign-in's final write's. It is part of the deadline a record carries,
// so no reader reads as lost a check whose end is still being written.
const modelCheckWriteBudget = 10 * time.Second

// maxModelCheckClaims bounds a start's compare-and-set against the record
// another ask, or a lost check, left: EIGHT, the handful of people who may ask
// for one entry's check at once, as a model retry's write is bounded.
const maxModelCheckClaims = 8

var (
	// ErrModelCheckInProgress is a running check of the entry that a start
	// cannot hand back ([ModelCheckInProgressError]).
	ErrModelCheckInProgress = errors.New("engine: a check of the model is running")
	// ErrModelChecksStopped is a node that has stopped running checks, or has
	// not started to.
	ErrModelChecksStopped = errors.New("engine: this node is not running model checks")

	// errModelCheckNodeStopping and errModelCheckBudget are why a check's
	// context ended.
	errModelCheckNodeStopping = errors.New("the node checking the model stopped before the check " +
		"ended; check again")
	errModelCheckBudget = errors.New("the check took its whole budget")
)

// ModelCheckInProgressError is [ErrModelCheckInProgress] with the running
// check.
type ModelCheckInProgressError struct{ Running coord.ModelCheck }

func (e *ModelCheckInProgressError) Error() string {
	return fmt.Sprintf("%s: check %s on node %s, started %s", ErrModelCheckInProgress, e.Running.ID,
		e.Running.Node, e.Running.StartedAt.Format(time.RFC3339))
}

func (e *ModelCheckInProgressError) Unwrap() error { return ErrModelCheckInProgress }

// ModelCheckAsk is what a start asks for: which entry, whether of a draft of
// it, whether with the smoke calls, who asks, and the budget past which this
// node gives the check up.
type ModelCheckAsk struct {
	Entry  string
	Draft  bool
	Smoke  bool
	By     iam.Actor
	Budget time.Duration
}

// modelChecks is this node's running checks.
type modelChecks struct {
	mu sync.Mutex

	// base is the node's lifetime every check runs under, cancelled with
	// [errModelCheckNodeStopping] when the node stops; running counts the
	// checks so a stopping node waits for their last writes.
	base    context.Context
	cancel  context.CancelCauseFunc
	running sync.WaitGroup

	// stopping refuses new checks once the node has begun ending them: set
	// under mu before [Engine.stopModelChecks] waits on running, and read
	// under mu where a start adds to it, so no check is added to a wait that
	// has begun.
	stopping bool

	// hold and writeBudget are [modelCheckHold] and [modelCheckWriteBudget]
	// outside a test.
	hold, writeBudget time.Duration
}

// startModelChecks lets this node run checks until [Engine.stopModelChecks].
func (e *Engine) startModelChecks(ctx context.Context) {
	c := &e.modelChecks
	base, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.base, c.cancel = base, cancel
	c.mu.Unlock()
}

// stopModelChecks ends every check this node runs, each recording that the
// node stopped, and waits for their last writes.
func (e *Engine) stopModelChecks() {
	c := &e.modelChecks
	c.mu.Lock()
	c.stopping = true
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel(errModelCheckNodeStopping)
	}
	c.running.Wait()
}

// StartModelCheck runs a check of ask's entry on this node, run being the
// check itself, and answers its record: ended, where it ended within
// [modelCheckHold]; running otherwise, or where the caller's context ended
// first, the check going on regardless. run is handed the check's own context
// — the node's lifetime under ask's budget — and answers what the check found,
// as the API renders it, within [coord.MaxModelCheckAnswerBytes].
//
// Refused with a [ModelCheckInProgressError] naming a running check of the
// entry this ask would not make, and [ErrModelChecksStopped] on a node that is
// stopping.
//
//nolint:contextcheck // the check runs on the node's lifetime (modelChecks.base), never the request's: it outlasts what a proxy lets the request wait
func (e *Engine) StartModelCheck(ctx context.Context, ask ModelCheckAsk,
	run func(context.Context) (json.RawMessage, error)) (coord.ModelCheck, error) {

	if e.backends == nil || e.backends.Fleet == nil {
		return coord.ModelCheck{}, errors.New("engine: this node has no coordination store to record the check in")
	}
	c := &e.modelChecks
	c.mu.Lock()
	if c.base == nil || c.stopping {
		c.mu.Unlock()
		return coord.ModelCheck{}, ErrModelChecksStopped
	}
	base := c.base
	c.running.Add(1)
	c.mu.Unlock()

	now := e.now().UTC()
	record := coord.ModelCheck{
		ID: uuid.New(), Entry: ask.Entry, Node: e.selfNode(), Draft: ask.Draft, Smoke: ask.Smoke,
		StartedAt: now, Deadline: now.Add(ask.Budget + c.write()),
		By: ask.By.Name, ByKind: ask.By.Kind, OperatorID: ask.By.OperatorID,
	}
	stored, handed, err := e.claimModelCheck(ctx, record, ask)
	if err != nil || handed {
		c.running.Done()
		return stored, err
	}
	ended := make(chan coord.ModelCheck, 1)
	go func() {
		defer c.running.Done()
		checkCtx, cancel := context.WithTimeoutCause(base, ask.Budget, errModelCheckBudget)
		defer cancel()
		answer, runErr := run(checkCtx)
		ended <- e.endModelCheck(checkCtx, stored, answer, runErr)
	}()
	hold := time.NewTimer(c.holdFor())
	defer hold.Stop()
	select {
	case final := <-ended:
		return final, nil
	case <-hold.C:
		return stored, nil
	case <-ctx.Done():
		return stored, ctx.Err()
	}
}

// claimModelCheck files record under its entry: created where no record is,
// the running check handed back where it is the check ask would make (true),
// refused naming it where it is not, and a record whose check ended, lapsed or
// was lost replaced at the version read.
func (e *Engine) claimModelCheck(ctx context.Context, record coord.ModelCheck,
	ask ModelCheckAsk) (coord.ModelCheck, bool, error) {

	fleet := e.backends.Fleet
	for range maxModelCheckClaims {
		record.Version = 0
		stored, created, err := fleet.CreateModelCheck(ctx, record)
		if err != nil {
			return coord.ModelCheck{}, false, fmt.Errorf("engine: record the check of %q: %w", record.Entry, err)
		}
		if created {
			return stored, false, nil
		}
		held, found, err := fleet.ModelCheck(ctx, record.Entry)
		if err != nil {
			return coord.ModelCheck{}, false, fmt.Errorf("engine: read the check of %q: %w", record.Entry, err)
		}
		if !found {
			continue
		}
		if held.Running() && !held.Lost(e.now()) {
			if !ask.Draft && !held.Draft && held.Smoke == ask.Smoke {
				return held, true, nil
			}
			return coord.ModelCheck{}, false, &ModelCheckInProgressError{Running: held}
		}
		record.Version = held.Version
		stored, replaced, err := fleet.UpdateModelCheck(ctx, record)
		if err != nil {
			return coord.ModelCheck{}, false, fmt.Errorf("engine: record the check of %q: %w", record.Entry, err)
		}
		if replaced {
			return stored, false, nil
		}
	}
	return coord.ModelCheck{}, false, fmt.Errorf("engine: the check of %q was not recorded: its record "+
		"changed under each of %d tries; check again", record.Entry, maxModelCheckClaims)
}

// endModelCheck records how the check stored ended, ctx being the check's own:
// what run answered, or why it answered nothing. A check the node's stop cut
// off records the stop, whatever its harness runs made of the cancellation,
// which would read as the vendor failing to answer. The write gets a budget of
// its own, detached from ctx, which may be gone.
func (e *Engine) endModelCheck(ctx context.Context, stored coord.ModelCheck, answer json.RawMessage,
	runErr error) coord.ModelCheck {

	cause := context.Cause(ctx)
	final := stored
	final.EndedAt = e.now().UTC()
	if !final.EndedAt.After(final.StartedAt) {
		final.EndedAt = final.StartedAt
	}
	switch {
	case errors.Is(cause, errModelCheckNodeStopping):
		final.Failure = errModelCheckNodeStopping.Error()
	case runErr != nil:
		final.Failure = textcut.Within(runErr.Error(), coord.MaxModelCheckFailureBytes)
	case len(answer) == 0:
		final.Failure = "the check answered nothing"
	case len(answer) > coord.MaxModelCheckAnswerBytes:
		final.Failure = fmt.Sprintf("the check's answer was %d bytes, past the %d a check keeps",
			len(answer), coord.MaxModelCheckAnswerBytes)
	default:
		final.Answer = answer
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.modelChecks.write())
	defer cancel()
	written, ok, err := e.backends.Fleet.UpdateModelCheck(ctx, final)
	switch {
	case err != nil:
		log.WarnContext(ctx, "model_check_end_not_recorded", "provider", stored.Entry, "check", stored.ID,
			"error", err, "detail", "the check's page reads it as lost once its deadline passes")
		return final
	case !ok:
		log.WarnContext(ctx, "model_check_end_superseded", "provider", stored.Entry, "check", stored.ID,
			"detail", "another check of the model replaced this one's record before it ended")
		return final
	}
	return written
}

// ModelCheck reads an entry's latest check from the coordination store, as
// any node answers it: false where it has none, or none still answering
// ([coord.ModelCheck.Lapsed]). Whether a running check is lost is the reader's
// to judge ([coord.ModelCheck.Lost]).
func (e *Engine) ModelCheck(ctx context.Context, key string) (coord.ModelCheck, bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return coord.ModelCheck{}, false, nil
	}
	check, found, err := e.backends.Fleet.ModelCheck(ctx, key)
	if err != nil || !found || check.Lapsed(e.now()) {
		return coord.ModelCheck{}, false, err
	}
	return check, true, nil
}

// clearRemovedModelChecks deletes the check record of every model entry the
// next revision removed: a check goes with what it checked, as an MCP server's
// latest test does ([Engine.clearRemovedMCPChecks]). A record of an entry
// still declared stays until its own lifetime ends it.
func (e *Engine) clearRemovedModelChecks(ctx context.Context, next *Company) {
	if next == nil || next.Config == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if err := e.fleetCompany(ctx, next); err != nil {
		log.DebugContext(ctx, "model_checks_not_swept", "reason", err.Error(),
			"detail", "a removed model's check is cleared by the company the fleet is pointed at; "+
				"this one is not it yet")
		return
	}
	checks, err := e.backends.Fleet.ListModelChecks(ctx)
	if err != nil {
		log.WarnContext(ctx, "model_checks_not_swept", "error", err,
			"detail", "a removed model's check stays until the next apply clears it")
		return
	}
	for _, check := range checks {
		if _, declared := next.Config.Providers.LLM[check.Entry]; declared {
			continue
		}
		if _, err := e.backends.Fleet.DeleteModelCheck(ctx, check.Entry, check.Version); err != nil {
			log.WarnContext(ctx, "model_check_not_cleared", "provider", check.Entry, "error", err)
		}
	}
}

func (c *modelChecks) holdFor() time.Duration {
	if c.hold > 0 {
		return c.hold
	}
	return modelCheckHold
}

func (c *modelChecks) write() time.Duration {
	if c.writeBudget > 0 {
		return c.writeBudget
	}
	return modelCheckWriteBudget
}
