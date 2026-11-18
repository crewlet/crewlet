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

// The trash reaper, wired (internal/trashreap): it purges what sat past its
// term in every trash that has one (the knowledge base's pages, the
// artifacts), or a trashed item stays in every data node's copy and backup for
// ever. The work tracker hands it no source by design: a removed work item is
// restorable at any age (the Worklet guide), and only a purge somebody asks
// for destroys one.
//
// It runs on a data node that publishes, beside the native half's other duty
// ([Engine.startNativeDuties]), listing from this node's copy of the estate and
// purging through each domain's publisher; the `worker:trash-reaper` lease,
// claimed per turn, picks the node. A new trash appends its adapter to
// [Engine.trashSources] and nothing else: term, schedule and op ids are shared.

// trashReaperPoll is how often the reaper's duty is asked for: a minute, the
// object collector's figure. The passes are hourly, so it only decides how soon
// a node notices it holds the duty and how soon a failed or capped pass reruns.
const trashReaperPoll = time.Minute

// trashReaperDutyTTL is how long the duty outlives a holder that stopped
// renewing it: ten polls, for the object collector's reason. The lease is
// renewed only between passes, and a capped pass waits on
// [trashreap.MaxPerPass] applied purges a source, minutes on a loaded log. A
// lapse mid-pass lets a peer purge the same items alongside: safe (each purge
// is decided in its own snapshot under an operation id both derive alike) but
// wasteful. A dead holder delays an hourly duty by at most this.
const trashReaperDutyTTL = 10 * trashReaperPoll

// trashReaper is this node's part in the reaper's duty. A lock, not an atomic
// flag: an apply's goroutine may start it as late as the teardown, and the
// check and the start must be one step, or a start racing a stop leaves a loop
// nothing ends.
type trashReaper struct {
	mu sync.Mutex
	// loop is nil until the duty starts; once stopped is set
	// ([Engine.stopTrashReaper]) it never starts.
	loop    *loop
	stopped bool
}

// startTrashReaper arms the reaper's duty over every trash this node can
// empty; a no-op on a node with none, on one that runs it already, and once
// stopped. Every source's term is artifacts.TrashRetention, the thirty days
// the knowledge base promised (doclet.StatusTrashed), so a page and an
// artifact deleted the same day come back until the same day.
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

// stopTrashReaper ends the duty for good, waiting out a pass in flight; safe on
// a node that never started it. It must run before [Engine.releaseDuties] (a
// loop still running past the release takes the duty back) and before the
// native half, whose knowledge store a pass purges through, comes down.
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

// reaperActor authors every record the trash reaper publishes (a retention
// delete, a trash purge, an erasure, a forgetting), under the reaper's own name
// so a log line leads to the duty that made it.
var reaperActor = iam.Actor{Name: trashreap.Name, Kind: iam.ActorSystem}

// trashSources is every trash this node can empty, in the order a pass visits
// them. The knowledge base's comes from [Engine.writeSide], the one gate that
// hands every publishing surface its writer, so a node in a maintenance mode
// gets no source and arms no duty.
func (e *Engine) trashSources() []trashreap.Source {
	var out []trashreap.Source
	if kb := e.writeSide().pages; kb != nil {
		out = append(out, pagesTrash{store: kb})
	}
	// The artifacts', which every data node runs: gated on the node's mode
	// alone, as [Engine.writeSide] gates the native half's writers.
	if c := e.core.Load(); c != nil && e.mode.Publishes() &&
		c.artifactsReader != nil && c.artifactsWriter != nil {
		out = append(out, artifactsTrash{reader: c.artifactsReader, writer: c.artifactsWriter})
	}
	// The assistant's conversations, on their own retention, erasing anybody
	// the identity estate removed.
	if src, ok := e.assistantSource(); ok {
		out = append(out, src)
	}
	// The seats' executor sessions, on the conversation retention, never a
	// turn whose run is still live.
	if src, ok := e.executorSource(); ok {
		out = append(out, src)
	}
	// The access log, on its retention, forgetting a removed seat's told
	// records.
	if src, ok := e.accessSource(); ok {
		out = append(out, src)
	}
	// The atlas's history, on its retention.
	if src, ok := e.atlasSource(); ok {
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
			Actor: reaperActor,
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

// reapAnswer turns a domain's answer to one purge into a [trashreap.Source]'s:
// nil for a durable purge (applied or pending), [trashreap.ErrNotEligible] for
// a refusal naming one of notEligible (no longer in the trash past its term, or
// gone), and an error otherwise, the unknown outcome included.
//
// One function for every source because of that outcome: the state log answers
// it with a nil error ([statelog.OutcomeUnknown]), and read as a purge it
// reports purged an item that may still be in the trash. As a failure it is
// retried under the same operation id, the only safe retry.
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
