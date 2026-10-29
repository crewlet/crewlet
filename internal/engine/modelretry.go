package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// "RETRY NOW" AND "CLEAR", FLEET-WIDE (M10 of the Models design).
//
// A sit-out is each node's own memory of a vendor's limit, kept on the
// entry's sign-in ([cliagent.Plan]): a request a person makes through one node
// changes nothing on another unless every node hears it. So the request is a
// coordination record, one per entry ([coord.ModelRetry]), and every node
// keeps a watched copy of the class, as it does of seat pauses
// (seatpause.go). On a record stamped later than the last one it applied for
// that entry, a node:
//
//  1. clears the entry's sign-in of every window opened by a report dated
//     BEFORE the stamp ([cliagent.Plan.Clear]); a report after it stands, since
//     the vendor said it after the person asked; and
//  2. fires now every provider-park probe whose chain names the entry, so a
//     seat parked on it is tried at once ([Engine.rearmLimitProbesNaming]); a
//     chain that is still limited re-parks with the vendor's fresh word.
//
// Order-free and idempotent: a node that hears a record late, twice, or after a
// restart clears exactly what it would have cleared on time.
//
// The record goes with its entry: the apply that removes an entry deletes it
// ([Engine.clearRemovedModelRetries]).

// modelRetryProbeBudget bounds the one call "retry now" makes before it writes
// the record: SIXTY SECONDS, past a coding CLI's cold start and a one-word
// answer (3 to 10 s measured), and short enough that a person waiting on the
// button is told within a minute. A vendor that has not answered by then has
// not shown the limit is over.
const modelRetryProbeBudget = 60 * time.Second

// modelRetryRewatchBase and modelRetryRewatchCeiling pace a lost watch's
// re-watch, as the seat pauses' do.
const (
	modelRetryRewatchBase    = seatPauseRewatchBase
	modelRetryRewatchCeiling = seatPauseRewatchCeiling
)

// ErrNotSittingOut is a "retry now" for an entry no live node reports sitting
// out: there is nothing to retry, and a test is the gesture instead.
var ErrNotSittingOut = errors.New("engine: no live node reports the model sitting out")

// RetryResult is what a retry did.
type RetryResult struct {
	// Cleared is whether the record was written, and so whether every node
	// is clearing the entry's sit-out.
	Cleared bool
	// Mode is how it was asked for, and Node the node that took it.
	Mode coord.ModelRetryMode
	Node string
	// Still is the window the probe met again, for a "retry now" the vendor
	// refused for a limit: nothing was written, and the fresh report sits the
	// entry out again.
	Still *coord.ModelSitOut
	// Failed is why the probe failed for any other reason, empty otherwise.
	Failed string
}

// modelRetries is this node's watched copy of the retry records: the last stamp
// it applied per entry, and the watch's lifetime.
type modelRetries struct {
	mu      sync.Mutex
	applied map[string]time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// startModelRetries opens the retry watch and keeps it open until
// stopModelRetries.
func (e *Engine) startModelRetries(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil {
		return
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.retries.cancel, e.retries.done = cancel, make(chan struct{})
	go func() {
		defer close(e.retries.done)
		watchRecords(loopCtx, "model_retry", e.backends.Fleet.WatchModelRetries,
			func(u coord.ModelRetryUpdate) bool { return u.Current },
			func(ctx context.Context, u coord.ModelRetryUpdate) {
				if u.Retry != nil {
					e.applyModelRetry(ctx, *u.Retry)
					return
				}
				e.retries.mu.Lock()
				delete(e.retries.applied, u.Key)
				e.retries.mu.Unlock()
			})
	}()
}

// stopModelRetries ends the watch and waits for it.
func (e *Engine) stopModelRetries() {
	if e.retries.cancel == nil {
		return
	}
	e.retries.cancel()
	<-e.retries.done
}

// watchRecords keeps watching one record class until ctx ends: every record a
// watch delivers, the first answer and each change alike, goes to apply, and a
// watch that stops hearing is opened again with the seat pauses' doubling
// backoff. apply must be order-free and idempotent, since a re-watch delivers
// every record again.
func watchRecords[U any](ctx context.Context, name string,
	watch func(context.Context) (<-chan U, error), marker func(U) bool,
	apply func(context.Context, U)) {

	for attempt := 0; ; attempt++ {
		updates, err := watch(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.WarnContext(ctx, name+"_watch_failed", "error", err,
					"detail", "this node keeps what it last applied and watches again")
			}
		} else {
			for u := range updates {
				if marker(u) {
					attempt = 0
					continue
				}
				apply(ctx, u)
			}
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff.Doubling(attempt, modelRetryRewatchBase, modelRetryRewatchCeiling)):
		}
	}
}

// applyModelRetry clears what a retry record asks this node to clear, once per
// stamp.
func (e *Engine) applyModelRetry(ctx context.Context, r coord.ModelRetry) {
	e.retries.mu.Lock()
	if last, seen := e.retries.applied[r.Key]; seen && !r.At.After(last) {
		e.retries.mu.Unlock()
		return
	}
	if e.retries.applied == nil {
		e.retries.applied = map[string]time.Time{}
	}
	e.retries.applied[r.Key] = r.At
	e.retries.mu.Unlock()

	cleared := false
	if agent, ok := e.cliEntry(r.Key); ok {
		cleared = agent.Plan().Clear(r.At)
	}
	armed := e.rearmLimitProbesNaming(ctx, r.Key)
	log.InfoContext(ctx, "model_retry_applied", "provider", r.Key, "mode", string(r.Mode),
		"at", r.At.UTC().Format(time.RFC3339), "by", r.By, "by_kind", string(r.ByKind),
		"cleared", cleared, "probes_rearmed", armed,
		"detail", "a person asked every node to stop sitting this model out: windows "+
			"reported before the request are cleared here, and seats parked on it are "+
			"tried again now")
}

// cliEntry is the active revision's coding-CLI entry under key.
func (e *Engine) cliEntry(key string) (*cliagent.Provider, bool) {
	c := e.Company()
	if c == nil || c.Models == nil {
		return nil, false
	}
	provider, ok := c.Models.Provider(key)
	if !ok {
		return nil, false
	}
	agent, isCLI := provider.(*cliagent.Provider)
	return agent, isCLI
}

// RetryModel stops every node sitting an entry out.
//
// [coord.RetryClear] writes the record at once and makes no call.
// [coord.RetryProbe] first asks whether any live node sits the entry out
// ([ErrNotSittingOut] if none does) and makes ONE probe call within
// [modelRetryProbeBudget], charged as a provider probe to the company: the
// record is written only if the call answered, and every node, this one
// included, clears through it. A probe refused for a limit writes nothing and
// answers the fresh window ([RetryResult.Still]); one that failed otherwise
// writes nothing either ([RetryResult.Failed]).
//
// THE PROBE NEVER TOUCHES THE ENTRY'S WINDOW. It is made through a sibling of
// the entry built on the same sign-in and state directory with a record of its
// own, so it reaches the vendor however the entry's window stands, and a
// probe that does not answer leaves that window exactly as it was. Clearing
// the window first and probing the entry itself left this node's sit-out
// cleared on a probe that failed for any other reason, with no record written
// to say anybody had cleared it, so this node alone sent seats into a limit
// every other node still sat out.
//
// [ErrNoModel] for an entry the active revision does not have.
func (e *Engine) RetryModel(ctx context.Context, key string, mode coord.ModelRetryMode, by iam.Actor) (RetryResult, error) {
	if !mode.Valid() {
		return RetryResult{}, fmt.Errorf("engine: a retry is %q or %q, not %q", coord.RetryProbe, coord.RetryClear, mode)
	}
	agent, ok := e.cliEntry(key)
	if !ok {
		return RetryResult{}, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	out := RetryResult{Mode: mode, Node: e.selfNode()}
	at := e.now()
	if mode == coord.RetryProbe {
		sitting, err := e.fleetSitsOut(ctx, key)
		if err != nil {
			return RetryResult{}, err
		}
		if !sitting {
			return RetryResult{}, fmt.Errorf("%w: %q", ErrNotSittingOut, key)
		}
		sibling, err := e.probeSibling(key)
		if err != nil {
			return RetryResult{}, err
		}
		if err := e.probeEntry(ctx, key, sibling); err != nil {
			if _, limited := chain.Limited(err); limited {
				out.Still = sitOutFor(sibling.Plan().State(e.now()), agent.ModelArgument())
				return out, nil
			}
			out.Failed = err.Error()
			return out, nil
		}
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return RetryResult{}, errors.New("engine: this node has no coordination store to tell the fleet")
	}
	if _, err := e.backends.Fleet.PutModelRetry(ctx, coord.ModelRetry{
		Key: key, Mode: mode, At: at, By: by.Name, ByKind: by.Kind, OperatorID: by.OperatorID,
	}); err != nil {
		return RetryResult{}, err
	}
	out.Cleared = true
	return out, nil
}

// probeSibling is the entry under key as the active revision builds it, on
// the same sign-in and state directory, with a record of its own rather than
// the node's: what its one call meets is the vendor's word, never the entry's
// window.
func (e *Engine) probeSibling(key string) (*cliagent.Provider, error) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	spec, ok := c.Config.Providers.LLM[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	return BuildCLIAgent(key, spec, e.resolver(), nil)
}

// probeEntry makes one call on provider, the entry under key, as the provider
// park's probe makes it, charged as a provider probe to the company rather
// than a seat: a person asked for it, not a seat's work.
func (e *Engine) probeEntry(ctx context.Context, key string, provider llm.Provider) error {
	c := e.Company()
	if c == nil || c.Models == nil {
		return fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	members := []chain.Member{{Key: key, Provider: provider}}
	if seam, ok := e.seamFor(c); ok {
		recorded, err := seam.recordedMembers(nil, members,
			auxspend.Use{Stage: types.AuxStageOperator, Purpose: types.AuxProviderProbe})
		if err != nil {
			return err
		}
		members = recorded
	}
	probe, err := chain.New(members, chain.Options{})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, modelRetryProbeBudget)
	defer cancel()
	_, err = probe.Complete(ctx, providerProbe)
	return err
}

// fleetSitsOut reports whether any live node's heartbeat says the entry sits
// out now. An unreadable lease table is an error, never "nobody does".
func (e *Engine) fleetSitsOut(ctx context.Context, key string) (bool, error) {
	nodes, err := e.liveModelRows(ctx)
	if err != nil {
		return false, err
	}
	now := e.now()
	for _, rows := range nodes {
		for _, row := range rows {
			if row.Key == key && row.SittingOut != nil && now.Before(row.SittingOut.Until) {
				return true, nil
			}
		}
	}
	return false, nil
}

// liveModelRows is every live node's published model rows, by node id; a node
// whose heartbeat carried no status is absent.
func (e *Engine) liveModelRows(ctx context.Context) (map[string][]coord.ModelStatus, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return map[string][]coord.ModelStatus{e.selfNode(): e.modelStatus(ctx)}, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]coord.ModelStatus, len(leases))
	for _, lease := range leases {
		status, ok := coord.StatusFromMeta(lease.Meta)
		if !ok {
			continue
		}
		node, _ := coord.ClassNode.Name(lease.Resource)
		out[node] = status.Models
	}
	return out, nil
}

// selfNode is this node's id: its node's, or the one a test names.
func (e *Engine) selfNode() string {
	if e.node == nil {
		return e.testNode
	}
	return e.node.ID()
}

// clearRemovedModelRetries deletes the retry of every entry the next revision
// removed: a retry goes with its entry.
func (e *Engine) clearRemovedModelRetries(ctx context.Context, next *Company) {
	if next == nil || next.Config == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if _, err := e.fleetCompany(ctx, next); err != nil {
		log.DebugContext(ctx, "model_retries_not_swept", "reason", err.Error(),
			"detail", "a removed model's retry is cleared by the company the fleet is pointed "+
				"at; this one is not it yet")
		return
	}
	retries, err := e.backends.Fleet.ListModelRetries(ctx)
	if err != nil {
		log.WarnContext(ctx, "model_retries_not_swept", "error", err,
			"detail", "a removed model's retry record stays until the next apply clears it")
		return
	}
	for _, r := range retries {
		if _, still := next.Config.Providers.LLM[r.Key]; still {
			continue
		}
		if _, err := e.backends.Fleet.DeleteModelRetry(ctx, r.Key, r.Version); err != nil {
			log.WarnContext(ctx, "model_retry_not_cleared", "provider", r.Key, "error", err)
		}
	}
}
