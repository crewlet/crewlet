package sandbox

// A SESSION ITS HARNESS MINTS is named only once it runs: its script reports
// the id (internal/sandbox/executor's sessionMinter), and the coordinator binds
// the report to the job's record ([PendingStore.BindSession]) where the id is
// first used: before a running job's save is packed, as the job is collected,
// and before any take of its box (a lost run's read after a recovery). A bind
// is ONCE, of a FRESH id, A RESUME BINDING THE ONE IT CONTINUED: the store
// claims the id fleet-wide, then the executor log is asked ([SessionHolders],
// one seek), then the record written. The claim holds the id until the log
// does (the capture landed) or the record goes. A refusal fails the run
// closed; a log that cannot answer leaves it undecided, asked again.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SessionHolders answers whether any turn's row of the executor log names a
// session id (executordomain.Reader.SessionHeld, one seek over its index,
// through the estate router's reads): what a minted session's bind asks
// before it binds an id its box reported as fresh.
type SessionHolders interface {
	SessionHeld(ctx context.Context, sessionID string) (bool, error)
}

// sessionReporter is a [Runner] whose harness mints its session's id and whose
// script reports it (internal/sandbox/executor's Runner): ReportedSession
// reads the report against the box's record of its session.
type sessionReporter interface {
	MintsSession() bool
	ReportedSession(ctx context.Context, box Sandbox, handle RunHandle) (string, error)
}

// reporterOf is runner as a minting runner, false for one whose harness names
// its session as its run starts.
func reporterOf(runner Runner) (sessionReporter, bool) {
	r, ok := runner.(sessionReporter)
	return r, ok && r.MintsSession()
}

// errNoSessionReported is a minting job whose box has reported no session
// yet: nothing is bound, and nothing that uses the session goes on.
var errNoSessionReported = errors.New("sandbox: no session reported yet")

// bindMinted reads the session run's box reported and binds it to the job's
// record ([Coordinator.bindReported]), answering the record as bound. Nothing
// for a runner that does not mint; [errNoSessionReported] for a box that
// reported none; [ErrSessionBindRefused] for a report the box's runner refused
// or the bind would not take.
func (c *Coordinator) bindMinted(ctx context.Context, run PendingRun, box Sandbox, runner Runner) (PendingRun,
	error) {

	reporter, mints := reporterOf(runner)
	if !mints {
		return run, nil
	}
	id, err := reporter.ReportedSession(ctx, box, run.Handle())
	switch {
	case errors.Is(err, ErrSessionBindRefused):
		return run, err
	case err != nil:
		return run, fmt.Errorf("sandbox: reading the session run %s reported: %w", run.TurnID, err)
	case id == "" && run.Launch.Minted != "":
		return run, fmt.Errorf("%w: job %s bound session %q and its box reports none now",
			ErrSessionBindRefused, run.LaunchID, run.Launch.Minted)
	case id == "":
		return run, errNoSessionReported
	}
	return c.bindReported(ctx, run, id)
}

// bindReported binds id, the session run's box reported, to the job's record.
// WHETHER THE LAUNCH CONTINUED is the engine's word, the token its box dialled
// ([LaunchRecord.SessionStart]): a launch that continued a session reads as
// continuing it unless its box said it started its own, as [PendingRun.SessionRan]
// reads it, and binds that session alone. A fresh id is asked of the executor
// log ([Coordinator.freshSession]) once the store has claimed it. AN ID THE
// RECORD BINDS ALREADY still goes to the store, which makes its claim again
// where none stands ([CoordStore.BindSession]): a claim a failed write or a
// sweep lost is back by the job's next save, take or collection.
func (c *Coordinator) bindReported(ctx context.Context, run PendingRun, id string) (PendingRun, error) {
	continued := run.SessionID != "" && run.Launch.SessionStart != SessionStartFresh
	bind := SessionBind{Launch: run.LaunchID, ID: id, Continued: continued}
	if !continued {
		bind.Fresh = func(_ context.Context, id string) error { return c.freshSession(ctx, id) }
	}
	write, cancel := c.rowContext(ctx, c.recordBudget)
	defer cancel()
	return c.pending.BindSession(write, run.TurnID, bind)
}

// freshSession is why id cannot be bound as a new session, nil where no turn's
// row names it: a coordinator built with no log refuses every fresh id, and a
// log that cannot answer leaves the bind UNDECIDED, an error that is no
// refusal, so the read that asked is made again and nothing is dropped.
func (c *Coordinator) freshSession(ctx context.Context, id string) error {
	if c.holders == nil {
		return fmt.Errorf("%w: nothing can tell whether session %q is another turn's", ErrSessionBindRefused, id)
	}
	held, err := c.holders.SessionHeld(ctx, id)
	switch {
	case err != nil:
		return fmt.Errorf("sandbox: whether session %q is another turn's could not be told: %w", id, err)
	case held:
		return fmt.Errorf("%w: session %q is another turn's", ErrSessionBindRefused, id)
	}
	return nil
}

// undecidedBind reports whether err is a bind neither made nor refused: a
// store or the executor log that could not answer, which the read that asked
// makes again.
func undecidedBind(err error) bool {
	return err != nil && !errors.Is(err, ErrSessionBindRefused) && !errors.Is(err, errNoSessionReported)
}

// bindCollected binds the session a minting job's collection read
// ([Result.SessionID], the report as its runner read it) and answers the
// result: as it was where the job reported none and binds none, and FAILED
// CLOSED where the bind is refused, naming no session and no capture, its
// error saying why first. A store that cannot answer is an error: the read is
// made again.
func (c *Coordinator) bindCollected(ctx context.Context, run PendingRun, runner Runner,
	result Result) (PendingRun, Result, error) {

	if _, mints := reporterOf(runner); !mints {
		return run, result, nil
	}
	if result.SessionID == "" && run.Launch.Minted == "" {
		return run, result, nil
	}
	bound, err := run, fmt.Errorf("%w: job %s bound session %q and its collection read none",
		ErrSessionBindRefused, run.LaunchID, run.Launch.Minted)
	if result.SessionID != "" {
		bound, err = c.bindReported(ctx, run, result.SessionID)
	}
	switch {
	case errors.Is(err, ErrSessionBindRefused):
		result.SessionID, result.Captured, result.Success = "", nil, false
		result.Error = strings.TrimSpace(err.Error() + "\n" + result.Error)
		return run, result, nil
	case err != nil:
		return run, Result{}, err
	}
	return bound, result, nil
}
