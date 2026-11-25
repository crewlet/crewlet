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
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// "Retry now" and "clear", fleet-wide. A sit-out is each node's own memory of a
// vendor's limit ([harness.Plan]), so the request is a coordination record per
// entry ([coord.ModelRetry]) every node watches, as seat pauses (seatpause.go).
// On a record stamped later than the last it applied for that entry, a node
// clears windows from reports dated BEFORE the stamp ([harness.Plan.Clear]; a
// later report is the vendor's word after the ask, and stands) and fires every
// provider-park probe whose chain names the entry ([Engine.rearmLimitProbesNaming]);
// a still-limited chain re-parks. Order-free and idempotent: a record heard
// late, twice or after a restart clears what it would have on time. The
// record goes with its entry ([Engine.clearRemovedModelRetries]).

// ModelRetryProbeBudget bounds the one call "retry now" makes before writing
// the record: past a harness's cold start and a one-word answer (3-10 s), and
// within the minute a person waits on the button. A vendor silent by then has
// not shown the limit is over. Exported because the Models screen's wait
// (contract/models.ts MODEL_RETRY_BUDGET_SECONDS) is checked against it.
const ModelRetryProbeBudget = 60 * time.Second

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
	// Cleared is whether the record was written, so every node clears the
	// entry's sit-out.
	Cleared bool
	// Mode is how it was asked for, and Node the node that took it.
	Mode coord.ModelRetryMode
	Node string
	// Still is the window a "retry now" probe met again: nothing was written,
	// and the fresh report sits the entry out again.
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

// watchRecords watches one record class until ctx ends, handing every record
// delivered (first answer and changes alike) to apply, and re-opens a lost
// watch with the seat pauses' doubling backoff. apply must be order-free and
// idempotent: a re-watch delivers every record again.
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
	if agent, ok := e.harnessEntry(r.Key); ok {
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

// harnessEntry is the active revision's harness entry under key.
func (e *Engine) harnessEntry(key string) (*harness.Provider, bool) {
	c := e.Company()
	if c == nil || c.Models == nil {
		return nil, false
	}
	provider, ok := c.Models.Provider(key)
	if !ok {
		return nil, false
	}
	agent, isHarness := provider.(*harness.Provider)
	return agent, isHarness
}

// RetryModel stops every node sitting an entry out. [coord.RetryClear] writes
// the record at once. [coord.RetryProbe] needs a live node sitting it out
// ([ErrNotSittingOut]) and asks the vendor: by the plan read where the entry has
// one ([Engine.checkPlan], no model call), else by ONE call within
// [ModelRetryProbeBudget], charged as a provider probe; only room or an answer
// writes the record, and every node, this one included, clears through it. A
// limit still in force answers the window ([RetryResult.Still]), held on this
// node's plan; any other failure is [RetryResult.Failed].
//
// The probe goes through [Engine.probeSibling], never the entry, so a failure
// leaves the window as it was: clearing first left this node alone, with no
// record, sending seats into a limit every other node still sits out.
// [ErrNoModel] for an entry the active revision does not have.
func (e *Engine) RetryModel(ctx context.Context, key string, mode coord.ModelRetryMode, by iam.Actor) (RetryResult, error) {
	if !mode.Valid() {
		return RetryResult{}, fmt.Errorf("engine: a retry is %q or %q, not %q", coord.RetryProbe, coord.RetryClear, mode)
	}
	agent, ok := e.harnessEntry(key)
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
		if agent.ReadsPlan() {
			if done := e.checkPlan(ctx, agent, &out); done {
				return out, nil
			}
		} else if done, err := e.probeOnce(ctx, key, agent, &out); err != nil {
			return RetryResult{}, err
		} else if done {
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

// checkPlan is a person's Check now on an entry whose plan is read without a
// call: the read, then done with the window still in force or why it failed,
// or not done where the plan has room and every node should clear.
func (e *Engine) checkPlan(ctx context.Context, agent *harness.Provider, out *RetryResult) (done bool) {
	readings, err := agent.ReadPlan(ctx)
	if err != nil {
		out.Failed = err.Error()
		return true
	}
	for _, r := range readings {
		if (r.Window == llm.LimitSession || r.Window == llm.LimitWeekly) &&
			(r.State == llm.LimitReached || (r.Used != nil && *r.Used >= 1)) {
			out.Still = sitOutFor(agent.Plan().State(e.now()), agent.ModelArgument())
			return true
		}
	}
	return false
}

// probeOnce is [Engine.RetryModel]'s one call on an entry whose plan cannot be
// read: done with the window still in force or why it failed, or not done
// where it answered.
func (e *Engine) probeOnce(ctx context.Context, key string, agent *harness.Provider,
	out *RetryResult) (done bool, err error) {

	sibling, err := e.probeSibling(key)
	if err != nil {
		return true, err
	}
	if err := e.probeEntry(ctx, key, sibling); err != nil {
		if _, limited := chain.Limited(err); limited {
			// The vendor's fresh word is the entry's: hold the sibling's
			// window on the entry's plan (Hold never shortens one in
			// force), or this node retries at the old window's end into a
			// limit already met.
			now := e.now()
			for _, window := range sibling.Plan().State(now).SittingOut {
				agent.Plan().Hold(window)
			}
			out.Still = sitOutFor(agent.Plan().State(now), agent.ModelArgument())
			return true, nil
		}
		out.Failed = err.Error()
		return true, nil
	}
	return false, nil
}

// probeSibling is the entry under key as the active revision builds it, on the
// same sign-in and state directory but with its own record, so its call meets
// the vendor's word, never the entry's window.
func (e *Engine) probeSibling(key string) (*harness.Provider, error) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	spec, ok := c.Config.Providers.LLM[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	return BuildHarness(key, spec, e.resolver(), nil)
}

// probeEntry makes one call on provider, the entry under key, as the provider
// park's probe does, charged to the company, not a seat: a person asked for it.
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
	ctx, cancel := context.WithTimeout(ctx, ModelRetryProbeBudget)
	defer cancel()
	_, err = probe.Complete(ctx, providerProbe)
	return err
}

// fleetSitsOut reports whether any live node's status record says the entry
// sits out now. An unreadable lease table or record store is an error, never
// "nobody does".
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
// whose status record is not counted is absent. With no coordination store,
// this node's own rows.
func (e *Engine) liveModelRows(ctx context.Context) (map[string][]coord.ModelStatus, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return map[string][]coord.ModelStatus{e.selfNode(): e.modelStatus(ctx)}, nil
	}
	records, err := e.liveStatusRecords(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]coord.ModelStatus, len(records))
	for node, r := range records {
		out[node] = r.Models
	}
	return out, nil
}

// liveStatusRecords is every live node's status record with its model rows
// alone, by node id: presence leases joined to records by epoch
// ([coord.LiveNodeStatuses]). A live node
// whose record is not counted is absent, never one with nothing to report.
// With no coordination store it is empty; a reader needing this node's own
// rows reads them.
func (e *Engine) liveStatusRecords(ctx context.Context) (map[string]coord.NodeStatusRecord, error) {
	if e.backends == nil || e.backends.Coord == nil || e.backends.Fleet == nil {
		return map[string]coord.NodeStatusRecord{}, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	records, err := e.backends.Fleet.NodeStatusRecords(ctx, coord.NodeStatusRead{Models: true})
	if err != nil {
		return nil, err
	}
	return coord.LiveNodeStatuses(records, leases), nil
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
	if err := e.fleetCompany(ctx, next); err != nil {
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
