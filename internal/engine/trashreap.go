package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// The trash reaper, wired (internal/trashreap).
//
// # What it is for
//
// A domain whose trash has a TERM — the knowledge base's pages and the
// artifacts — promises that what is in it can be taken back for that term and
// is gone after it. Nothing used to keep the second half: a trashed page stayed
// in every data node's copy, and in every backup, for ever. The reaper is the
// one fleet duty that keeps it, for every domain that hands it a
// [trashreap.Source] here.
//
// The work tracker hands it none, and that is the tracker's own promise rather
// than a gap: a removed work item is restorable AT ANY AGE (the work-tracker
// guide), because a removal is how a board is tidied, and the one step that
// destroys an item is a purge somebody asks for. Reaping removed items after a
// term would delete what that promise says is still there.
//
// # Where it runs
//
// On a data node that publishes, beside the native half's other duty
// ([Engine.startNativeDuties]): each source lists the trash from this node's
// own copy of the replicated estate and purges through the domain's publisher,
// and a node in a maintenance mode publishes nothing. Which node of those runs
// it is the `worker:trash-reaper` lease, claimed per turn.
//
// # Adding a source
//
// A domain with a trash appends its adapter to [Engine.trashSources] and
// nothing else: the reaper reads no domain's rows, and the term, the schedule,
// the operation ids and the report are the same for every source.

// trashReaperPoll is how often the reaper's duty is asked for.
//
// A MINUTE, the object collector's figure: the passes are hourly, so the poll
// only decides how soon a node notices it has been handed the duty, and how
// soon a pass that failed or stopped at its cap runs again.
const trashReaperPoll = time.Minute

// trashReaperDutyTTL is how long the duty outlives a holder that stopped
// renewing it.
//
// TEN MINUTES, ten polls rather than three, for the collector's reason: the
// lease is renewed between passes and never during one, and a pass at its cap
// publishes [trashreap.MaxPerPass] purge records a source, each waited for
// until it is applied, which on a log under load is minutes rather than
// seconds. A lease that lapsed mid-pass would hand the duty to a peer that
// purges the same items alongside. That is safe (each purge is decided again
// in its own snapshot, under an operation id both derive alike) and only
// wasteful; ten minutes keeps it from happening on an ordinary pass, and a
// dead holder delays an hourly duty by no more than that.
const trashReaperDutyTTL = 10 * trashReaperPoll

// trashReaper is this node's part in the reaper's duty.
//
// A LOCK AND NOT AN ATOMIC FLAG, for the object collector's reason: the
// native half that starts the duty may be brought up by an apply on its own
// goroutine as late as the teardown, and the check and the start have to be
// one step, or a start that lost the processor to a stop would leave a loop
// nothing ends.
type trashReaper struct {
	mu sync.Mutex
	// loop is the duty's loop, nil until it starts; stopped is set once
	// [Engine.stopTrashReaper] has run, and the loop never starts after it.
	loop    *loop
	stopped bool
}

// startTrashReaper arms the reaper's duty over every trash this node can
// empty. A no-op on a node with none, on a node that runs it already, and once
// it has been stopped.
//
// THE TERM IS artifacts.TrashRetention for every source, the thirty days the
// knowledge base already promised (doclet.StatusTrashed): one term, so a
// person deleting a page and an artifact on the same day can take both back
// until the same day.
func (e *Engine) startTrashReaper(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil {
		return
	}
	r := &e.trashReaper
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.loop != nil {
		return
	}
	sources := e.trashSources()
	if len(sources) == 0 {
		return
	}
	reaper, err := trashreap.New(trashreap.Deps{
		Sources: sources, Term: artifacts.TrashRetention,
		Reports: e.backends.Fleet, Node: e.id,
	})
	if err != nil {
		log.WarnContext(ctx, "trash_reaper_not_started", "error", err,
			"detail", "nothing purges the trash past its term on this node")
		return
	}
	claim := e.workerDuty(trashreap.Name, trashReaperDutyTTL)
	r.loop = startLoop(ctx, sleep, func(ctx context.Context) time.Duration {
		reaper.Turn(ctx, claim)
		return trashReaperPoll
	})
}

// stopTrashReaper ends the reaper's duty for good, waiting out a pass in
// flight. Safe on a node that never started it.
//
// BEFORE [Engine.releaseDuties], with every other loop that claims a duty: a
// loop still running past the release takes the duty straight back. AND
// BEFORE the native half comes down, whose knowledge store the pass purges
// through.
func (e *Engine) stopTrashReaper() {
	r := &e.trashReaper
	r.mu.Lock()
	r.stopped = true
	l := r.loop
	r.loop = nil
	r.mu.Unlock()
	if l != nil {
		l.stop()
	}
}

// trashSources is every trash this node can empty, in the order a pass visits
// them: the knowledge base's where this node writes it, then the artifacts'. A
// domain with a trash appends its source here.
//
// FROM [Engine.writeSide], the one gate every surface that publishes the
// company's records through this node is handed its writer from, so a node in
// a maintenance mode is handed no source and arms no duty.
func (e *Engine) trashSources() []trashreap.Source {
	var out []trashreap.Source
	if kb := e.writeSide().pages; kb != nil {
		out = append(out, pagesTrash{store: kb})
	}
	// THE ARTIFACTS', which every data node runs whatever its company keeps:
	// gated on the node's mode alone, the gate [Engine.writeSide] applies to
	// the native half's writers.
	if c := e.core.Load(); c != nil && e.mode.Publishes() &&
		c.artifactsReader != nil && c.artifactsWriter != nil {
		out = append(out, artifactsTrash{reader: c.artifactsReader, writer: c.artifactsWriter})
	}
	// AND THE ASSISTANT'S CONVERSATIONS, on their own retention, with the
	// erasure of anybody the identity estate removed.
	if src, ok := e.assistantSource(); ok {
		out = append(out, src)
	}
	// AND THE SEATS' EXECUTOR SESSIONS, on the company's conversation
	// retention, never a turn whose run is still live.
	if src, ok := e.executorSource(); ok {
		out = append(out, src)
	}
	return out
}

// artifactsTrash is the artifacts' trash, as the reaper reaches it.
type artifactsTrash struct {
	reader *artifactsdomain.Reader
	writer *artifactsdomain.Writer
}

func (artifactsTrash) Name() string { return artifactsdomain.Domain{}.Name() }

// TrashedBefore nominates the artifacts in the trash since before cutoff, and
// ahead of them every artifact whose purge began and stopped part way, which
// only a purge may finish.
func (a artifactsTrash) TrashedBefore(ctx context.Context, cutoff time.Time,
	limit int) ([]string, error) {

	ids, err := a.reader.TrashedBefore(ctx, cutoff, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out, nil
}

// PurgeTrashed purges one artifact past its term as the engine, dependents
// first, under the operation the reaper derived, which the writer keys the
// gesture on ([artifactsdomain.Call.OpKey]) so a retry in the same slot
// collapses into the first.
func (a artifactsTrash) PurgeTrashed(ctx context.Context, id string, cutoff time.Time,
	opID string) error {

	parsed, err := artifacts.ParseID(id)
	if err != nil {
		return fmt.Errorf("%w: the trash listed %q: %w", trashreap.ErrNotEligible, id, err)
	}
	c := artifactsdomain.Call{
		Authority: artifacts.Authority{
			Actor: iam.Actor{Name: trashreap.Name, Kind: iam.ActorSystem},
			Role:  artifacts.RoleOwner,
		},
		OpKey: opID,
	}
	written, err := a.writer.PurgeTrashed(ctx, c, parsed, cutoff)
	return reapAnswer(written.Result, err, artifactsdomain.ErrNotInTrash,
		artifactsdomain.ErrNotFound, artifactsdomain.ErrPurged)
}

// pagesTrash is the knowledge base's trash, as the reaper reaches it.
type pagesTrash struct{ store *doclet.Store }

func (pagesTrash) Name() string { return "pages" }

func (p pagesTrash) TrashedBefore(ctx context.Context, cutoff time.Time,
	limit int) ([]string, error) {

	return p.store.TrashedBefore(ctx, cutoff, limit)
}

// PurgeTrashed purges one page past its term as the engine, under the
// operation the reaper derived, which the store keys the purge on
// ([doclet.Actor.OpKey]) so a retry in the same slot collapses into the first.
func (p pagesTrash) PurgeTrashed(ctx context.Context, id string, cutoff time.Time,
	opID string) error {

	actor := doclet.SystemActor(trashreap.Name)
	actor.OpKey = opID
	written, err := p.store.PurgeTrashed(ctx, actor, id, cutoff)
	return reapAnswer(written.Outcome, err, doclet.ErrNotInTrash, doclet.ErrNotFound)
}

// reapAnswer turns a domain's answer to one purge into the one a
// [trashreap.Source] gives: nil for a purge that is durable (applied here or
// pending), [trashreap.ErrNotEligible] for a refusal that names one of
// notEligible (the item is no longer in the trash past its term, or is gone
// already), and an error for everything else, the unknown outcome included.
//
// ONE FUNCTION FOR EVERY SOURCE, because the unknown outcome is the trap: the
// state log answers it with a nil error ([statelog.OutcomeUnknown]), and a
// source that read the nil as a purge would report an item purged that may
// still be in the trash, and the next pass would find it there with nothing
// having said why. Counted as a failure instead, it is retried under the same
// operation id, which is the only safe retry the outcome allows.
func reapAnswer(result statelog.Result, err error, notEligible ...error) error {
	if err != nil {
		for _, sentinel := range notEligible {
			if errors.Is(err, sentinel) {
				return fmt.Errorf("%w: %w", trashreap.ErrNotEligible, err)
			}
		}
		return err
	}
	switch result.Outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		return nil
	case statelog.OutcomeUnknown:
		return fmt.Errorf("whether the purge landed is unknown (operation %s); "+
			"the next pass asks again under the same operation", result.OpID)
	}
	return fmt.Errorf("the purge answered an outcome this build does not know: %q",
		result.Outcome)
}
