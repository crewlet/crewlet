package mcpbridge

// A SESSION ITS HARNESS MINTS. A minting harness names its session only once it
// runs, and its script reports the id (internal/sandbox/executor's
// sessionMinter): until then the session is keyed by its launch, and the
// report is the box's word, so the bridge binds it under three rules. ONCE: a
// later report is refused. FRESH: an id bound to another run, turn or seat is
// refused ([Session.Foreign]). A RESUME REPORTS NOTHING NEW: a box that took up
// [Session.Continues] binds it alone. Any refusal fails the run closed
// ([binding.stop]). The run's single token is kept; once bound it resolves only
// to the bound session, and the job's other token is refused ([binding.begin]).

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// ErrBindRefused is a minted session's report the bridge would not bind: the
// run's tools are closed to it from then on.
var ErrBindRefused = errors.New("mcpbridge: the session the run reported is refused")

// Bind binds s's live opening to id, the session its box reported, or refuses
// it with [ErrBindRefused] and closes the run's tools: a run whose session the
// engine cannot trust makes no further call as its seat. ctx bounds the
// freshness check ([Session.Foreign]); a check that cannot answer refuses.
func (b *Bridge) Bind(ctx context.Context, s *Session, id string) error {
	if b == nil || s == nil {
		return errors.New("mcpbridge: no session to bind a reported id to")
	}
	s.mu.Lock()
	opened := s.live
	s.mu.Unlock()
	if opened == nil {
		return fmt.Errorf("mcpbridge: run %s has no open session to bind %q to", s.RunID, id)
	}
	if err := opened.bind(ctx, id); err != nil {
		opened.stop(err)
		return err
	}
	return nil
}

// bind is [Bridge.Bind] on the opening, under startMu: every request waits
// behind it, so none meets the guard of the launch once the id is bound. A
// bind ahead of the box's first request settles which session the box started
// as [binding.begin] would, the continued one or the launch's own, so the
// job's other token is refused from then on. A box that fell back to a session
// of its own says so on its other token first: its harness dials its servers
// as it starts, before its prompt, and so before its report.
func (o *binding) bind(ctx context.Context, id string) error {
	o.startMu.Lock()
	defer o.startMu.Unlock()
	s := o.session
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrBindRefused}, args...)...)
	}
	switch {
	case !s.Mints:
		return refuse("run %s's harness names its session as it starts, and reports none", s.RunID)
	case o.bound != "":
		return refuse("run %s already bound session %q, and a run binds one, once", s.RunID, o.bound)
	case !sandbox.ValidSessionID(id):
		return refuse("%q is no session id a harness names", id)
	}
	fellBack := o.started && o.startedFresh
	if s.Continues != "" && !fellBack {
		if id != s.Continues {
			return refuse("run %s continued session %q and reported %q", s.RunID, s.Continues, id)
		}
	} else if err := o.fresh(ctx, id); err != nil {
		return err
	}
	if !o.started {
		o.started, o.startedFresh = true, false
		if hook := s.Started; hook != nil {
			hook(o.ctx, false) //nolint:contextcheck // the run's context, as [binding.begin] tells it: it holds for the run
		}
	}
	o.bound = id
	if hook := s.Bound; hook != nil {
		hook(o.ctx, id) //nolint:contextcheck // the run's context, as above, never the caller's
	}
	return nil
}

// fresh is why id cannot be a new session's, or nil: the session the launch
// continued is an earlier run's, and any other is asked of [Session.Foreign],
// whose silence or failure refuses.
func (o *binding) fresh(ctx context.Context, id string) error {
	s := o.session
	switch {
	case s.Continues != "" && id == s.Continues:
		return fmt.Errorf("%w: run %s started a session of its own and reported the one it continued, %q",
			ErrBindRefused, s.RunID, id)
	case s.Foreign == nil:
		return fmt.Errorf("%w: nothing can tell whether session %q is bound to another run", ErrBindRefused, id)
	}
	foreign, err := s.Foreign(ctx, id)
	switch {
	case err != nil:
		return fmt.Errorf("%w: whether session %q is bound to another run could not be told: %w",
			ErrBindRefused, id, err)
	case foreign:
		return fmt.Errorf("%w: session %q is bound to another run, turn or seat", ErrBindRefused, id)
	}
	return nil
}
