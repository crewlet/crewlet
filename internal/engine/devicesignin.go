package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A DEVICE SIGN-IN, the engine's half (M11 of the Models design): a person
// signs a CLI in from the Models page without a terminal on the node. The node
// that takes the request runs the CLI's own device-code login
// ([cliagent.DeviceLogin]), relays its verification address and one-time code
// through a coordination record ([coord.DeviceSignIn]), and waits for the
// person to approve it in their browser. Nothing is pasted back through
// Crewlet.
//
// # One session per entry, across the fleet
//
// The record is filed under the entry, and a start creates it only where
// none is running: a second start is refused with the running session
// ([ErrSignInInProgress]). A record whose session has ended, lapsed, or whose
// node is gone is replaced.
//
// # On approval the login reaches every node
//
// The CLI wrote its login into this node's credential directory. The node
// exports it as the entry's credential bundle, seals it in the secret store
// ([secrets.SourceModels]), and re-activates the revision so every node
// rebuilds the entry and restores the bundle, the newest login winning over an
// older one it holds (cliagent's bundle restore). A failed re-activation leaves
// the login sealed and says so on the record: the next apply carries it.
//
// # A session outlives nothing it should not
//
// A cancel reaches the running node through the record (status
// cancel_requested), which every node watches. A node that stops ends its
// sessions as failed ("the node signing in stopped; start again"), and every
// node's sweep marks failed a session whose node is gone and removes a record
// past its lifetime ([coord.DeviceSignIn.Lapsed]).

// deviceCodeWait is how long a start waits for the CLI to print its address and
// code before it answers that the sign-in is unavailable: TWENTY SECONDS, past a
// CLI's cold start and its first round trip to the vendor, and the bound the
// API promises a person waiting on the dialog.
const deviceCodeWait = 20 * time.Second

// deviceCodeLifetime is how long a session is held for before the CLI has said
// how long its code lives: OpenAI's documented fifteen minutes for a device
// code, plus the thirty-second grace cliagent's device login allows past it.
const deviceCodeLifetime = 15*time.Minute + 30*time.Second

// deviceSweepInterval is how often a node sweeps the sessions: the coordination
// heartbeat, which is the cadence a node that died stops renewing its presence
// at, so its sessions are failed within a beat of its lease lapsing.
const deviceSweepInterval = coord.ReconcileInterval

// deviceFinalWriteBudget bounds the write that records how a session ended,
// made after the session's own context may be gone: ten seconds, a few store
// round trips.
const deviceFinalWriteBudget = 10 * time.Second

var (
	// ErrSignInInProgress is a start for an entry whose session is running.
	ErrSignInInProgress = errors.New("engine: a device sign-in is already running for this model")
	// ErrNoDeviceSignIn is an entry whose CLI has no device sign-in.
	ErrNoDeviceSignIn = errors.New("engine: this model's CLI has no device sign-in")
	// ErrSignsInWithoutLogin is an entry whose auth mode reads no login, so
	// a device sign-in would store one none of its calls reads.
	ErrSignsInWithoutLogin = errors.New("engine: this model does not sign in with a CLI login")
	// ErrDeviceSignInUnavailable is a CLI that did not print its code in time,
	// or exited before it did.
	ErrDeviceSignInUnavailable = errors.New("engine: the CLI did not start a device sign-in")
	// ErrNoSuchSignIn is a session id no live record names.
	ErrNoSuchSignIn = errors.New("engine: no such device sign-in")
)

// SignInInProgressError is [ErrSignInInProgress] with the running session.
type SignInInProgressError struct{ Running coord.DeviceSignIn }

func (e *SignInInProgressError) Error() string {
	return fmt.Sprintf("%s: session %s on node %s, %s", ErrSignInInProgress,
		e.Running.ID, e.Running.Node, e.Running.Status)
}

func (e *SignInInProgressError) Unwrap() error { return ErrSignInInProgress }

// deviceSignIns is this node's running sessions and the watch that hears
// cancels and sweeps.
type deviceSignIns struct {
	mu      sync.Mutex
	running map[string]*deviceSession

	// base is the node's lifetime context every session runs under, and
	// sessions counts them so a stopping node waits for their last write.
	base     context.Context
	sessions sync.WaitGroup

	cancel context.CancelFunc
	done   chan struct{}

	// login is cliagent.DeviceLogin outside a test.
	login func(ctx context.Context, p *cliagent.Provider, out func(cliagent.DeviceCode)) error
}

// deviceSession is one session this node runs.
type deviceSession struct {
	id     uuid.UUID
	cancel context.CancelCauseFunc
}

// errSessionCancelled and errNodeStopping are why a session's context ended.
var (
	errSessionCancelled = errors.New("a person cancelled the sign-in")
	errNodeStopping     = errors.New("the node signing in stopped; start again")
)

// startDeviceSignIns opens the watch and the sweep.
func (e *Engine) startDeviceSignIns(ctx context.Context) {
	d := &e.signIns
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d.mu.Lock()
	d.base, d.cancel, d.done = base, cancel, make(chan struct{})
	d.mu.Unlock()
	if e.backends == nil || e.backends.Fleet == nil {
		close(d.done)
		return
	}
	go func() {
		defer close(d.done)
		var watching sync.WaitGroup
		watching.Go(func() {
			watchRecords(base, "device_sign_in", e.backends.Fleet.WatchDeviceSignIns,
				func(u coord.DeviceSignInUpdate) bool { return u.Current },
				func(ctx context.Context, u coord.DeviceSignInUpdate) {
					if u.SignIn != nil {
						e.heardDeviceSignIn(ctx, *u.SignIn)
					}
				})
		})
		ticker := time.NewTicker(deviceSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-base.Done():
				watching.Wait()
				return
			case <-ticker.C:
				e.sweepDeviceSignIns(base)
			}
		}
	}()
}

// stopDeviceSignIns ends every session this node runs as failed, waits for
// their last writes, and ends the watch.
func (e *Engine) stopDeviceSignIns() {
	d := &e.signIns
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	for _, s := range d.running {
		s.cancel(errNodeStopping)
	}
	d.mu.Unlock()
	d.sessions.Wait()
	if cancel != nil {
		cancel()
		<-done
	}
}

// StartDeviceSignIn starts a device sign-in for an entry on this node and
// answers its record once the CLI has printed its address and code.
//
// Refused with [ErrNoModel], [ErrNoDeviceSignIn], [ErrSignsInWithoutLogin], a
// [SignInInProgressError] naming the running session, or
// [ErrDeviceSignInUnavailable] when the CLI did not print its code within
// [deviceCodeWait] (its first line said why).
//
//nolint:contextcheck // the session runs on the node's lifetime (signIns.base), never the request's: the person approves minutes after the request returns
func (e *Engine) StartDeviceSignIn(ctx context.Context, key string, by iam.Actor) (coord.DeviceSignIn, error) {
	agent, err := e.deviceEntry(key)
	if err != nil {
		return coord.DeviceSignIn{}, err
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return coord.DeviceSignIn{}, errors.New("engine: this node has no coordination store to hold the session")
	}
	now := e.now().UTC()
	record := coord.DeviceSignIn{
		ID: uuid.New(), Entry: key, Node: e.selfNode(), Status: coord.DeviceStarting,
		StartedAt: now, ExpiresAt: now.Add(deviceCodeWait + deviceCodeLifetime),
		By: by.Name, ByKind: by.Kind, OperatorID: by.OperatorID,
	}
	stored, err := e.claimDeviceSignIn(ctx, record)
	if err != nil {
		return coord.DeviceSignIn{}, err
	}

	d := &e.signIns
	d.mu.Lock()
	if d.base == nil {
		d.mu.Unlock()
		return coord.DeviceSignIn{}, errors.New("engine: this node runs no device sign-ins: it has not started")
	}
	// THE NODE'S LIFETIME, NOT THE REQUEST'S: the session outlives the
	// request that started it (the person approves minutes later), and ends
	// with the node, which fails it ([Engine.stopDeviceSignIns]).
	sessionCtx, cancel := context.WithCancelCause(d.base)
	if d.running == nil {
		d.running = map[string]*deviceSession{}
	}
	d.running[key] = &deviceSession{id: stored.ID, cancel: cancel}
	login := d.login
	if login == nil {
		login = cliagent.DeviceLogin
	}
	d.sessions.Add(1)
	d.mu.Unlock()

	coded := make(chan coord.DeviceSignIn, 1)
	ended := make(chan coord.DeviceSignIn, 1)
	go func() {
		defer d.sessions.Done()
		defer cancel(nil)
		err := login(sessionCtx, agent, func(code cliagent.DeviceCode) {
			pending, werr := e.updateDeviceSignIn(sessionCtx, key, stored.ID, func(s *coord.DeviceSignIn) bool {
				if s.Status.Ended() {
					return false
				}
				s.URL, s.Code = code.URL, code.Code
				if !code.ExpiresAt.IsZero() {
					s.ExpiresAt = code.ExpiresAt.UTC()
				}
				if s.Status == coord.DeviceStarting {
					s.Status = coord.DevicePending
				}
				return true
			})
			if werr != nil {
				log.WarnContext(sessionCtx, "device_sign_in_not_recorded", "provider", key, "error", werr)
			}
			select {
			case coded <- pending:
			default:
			}
		})
		final := e.endDeviceSignIn(sessionCtx, key, stored.ID, agent, by, err)
		d.mu.Lock()
		if s := d.running[key]; s != nil && s.id == stored.ID {
			delete(d.running, key)
		}
		d.mu.Unlock()
		ended <- final
	}()

	wait := time.NewTimer(deviceCodeWait)
	defer wait.Stop()
	select {
	case pending := <-coded:
		return pending, nil
	case final := <-ended:
		if final.Status == coord.DevicePending || final.Status == coord.DeviceApproved {
			return final, nil
		}
		return final, fmt.Errorf("%w: %s", ErrDeviceSignInUnavailable, final.Detail)
	case <-wait.C:
		cancel(fmt.Errorf("the CLI printed no device code within %s", deviceCodeWait))
		final := <-ended
		return final, fmt.Errorf("%w: %s", ErrDeviceSignInUnavailable, final.Detail)
	case <-ctx.Done():
		// The person's request ended; the session goes on, and its record
		// says how it is doing.
		return stored, ctx.Err()
	}
}

// deviceEntry is an entry a device sign-in can sign in, with its config.
func (e *Engine) deviceEntry(key string) (*cliagent.Provider, error) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	spec, declared := c.Config.Providers.LLM[key]
	agent, ok := e.cliEntry(key)
	if !declared || !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	profile := agent.Profile()
	if !profile.HasDeviceLogin() {
		return nil, fmt.Errorf("%w: %s", ErrNoDeviceSignIn, agent.Agent())
	}
	if mode := spec.ResolvedAuthMode(); mode != config.AuthSubscription {
		return nil, fmt.Errorf("%w: %s signs in with auth.mode %s; change "+
			"how it signs in first", ErrSignsInWithoutLogin, key, mode)
	}
	return agent, nil
}

// claimDeviceSignIn writes a new session's record: a create, or a replacement
// of a record whose session is over. A running session refuses it.
func (e *Engine) claimDeviceSignIn(ctx context.Context, record coord.DeviceSignIn) (coord.DeviceSignIn, error) {
	fleet := e.backends.Fleet
	for range 4 {
		stored, created, err := fleet.CreateDeviceSignIn(ctx, record)
		if err != nil {
			return coord.DeviceSignIn{}, err
		}
		if created {
			return stored, nil
		}
		held, found, err := fleet.DeviceSignIn(ctx, record.Entry)
		if err != nil {
			return coord.DeviceSignIn{}, err
		}
		if !found {
			continue
		}
		live, err := e.liveNodeSet(ctx)
		if err != nil {
			return coord.DeviceSignIn{}, err
		}
		if !e.sessionOver(held, live) {
			return coord.DeviceSignIn{}, &SignInInProgressError{Running: held}
		}
		record.Version = held.Version
		stored, replaced, err := fleet.UpdateDeviceSignIn(ctx, record)
		if err != nil {
			return coord.DeviceSignIn{}, err
		}
		if replaced {
			return stored, nil
		}
	}
	return coord.DeviceSignIn{}, fmt.Errorf("%w: the session record of %s kept moving", coord.ErrUnavailable, record.Entry)
}

// sessionOver reports whether a record's session is over: ended, lapsed, or
// held by a node that is gone (or, for this node, one it does not run). With
// no lease view (live nil) another node's session is never judged gone:
// UNKNOWN IS NOT DEAD, and judged so, a start would replace a session a person
// is typing the code of.
func (e *Engine) sessionOver(s coord.DeviceSignIn, live map[string]bool) bool {
	if s.Status.Ended() || s.Lapsed(e.now()) {
		return true
	}
	if s.Node == e.selfNode() {
		e.signIns.mu.Lock()
		defer e.signIns.mu.Unlock()
		running := e.signIns.running[s.Entry]
		return running == nil || running.id != s.ID
	}
	return live != nil && !live[s.Node]
}

// updateDeviceSignIn applies change to the entry's record while it still names
// session id, at the version read, retrying a lost race. change reports
// whether there is anything to write.
func (e *Engine) updateDeviceSignIn(ctx context.Context, key string, id uuid.UUID,
	change func(*coord.DeviceSignIn) bool) (coord.DeviceSignIn, error) {

	fleet := e.backends.Fleet
	for range 8 {
		held, found, err := fleet.DeviceSignIn(ctx, key)
		if err != nil {
			return coord.DeviceSignIn{}, err
		}
		if !found || held.ID != id {
			return coord.DeviceSignIn{}, fmt.Errorf("%w: session %s of %s", ErrNoSuchSignIn, id, key)
		}
		next := held
		if !change(&next) {
			return held, nil
		}
		stored, ok, err := fleet.UpdateDeviceSignIn(ctx, next)
		if err != nil {
			return coord.DeviceSignIn{}, err
		}
		if ok {
			return stored, nil
		}
	}
	return coord.DeviceSignIn{}, fmt.Errorf("%w: the session record of %s kept moving", coord.ErrUnavailable, key)
}

// endDeviceSignIn records how a session ended: approved (its login sealed and
// the revision re-activated), expired, cancelled or failed.
func (e *Engine) endDeviceSignIn(sessionCtx context.Context, key string, id uuid.UUID,
	agent *cliagent.Provider, by iam.Actor, loginErr error) coord.DeviceSignIn {

	ctx, cancel := context.WithTimeout(context.WithoutCancel(sessionCtx), deviceFinalWriteBudget)
	defer cancel()
	status, detail := coord.DeviceApproved, ""
	var revision string
	var epoch uint64
	switch cause := context.Cause(sessionCtx); {
	case loginErr == nil:
		var err error
		revision, epoch, err = e.sealDeviceLogin(ctx, key, agent, by)
		if err != nil {
			// THE CLI SIGNED IN, so the session is approved; where the
			// login could not be stored or carried to the fleet, the
			// record says what is left to do.
			detail = err.Error()
		}
	case errors.Is(loginErr, cliagent.ErrDeviceCodeExpired):
		status, detail = coord.DeviceExpired, "the code expired before it was approved"
	case errors.Is(cause, errSessionCancelled):
		status = coord.DeviceCancelled
	case cause != nil:
		status, detail = coord.DeviceFailed, cause.Error()
	default:
		status, detail = coord.DeviceFailed, loginErr.Error()
	}
	final, err := e.updateDeviceSignIn(ctx, key, id, func(s *coord.DeviceSignIn) bool {
		s.Status, s.Detail, s.RevisionID, s.Epoch = status, detail, revision, epoch
		return true
	})
	if err != nil {
		log.WarnContext(ctx, "device_sign_in_end_not_recorded", "provider", key,
			"status", string(status), "error", err,
			"detail", "every node's sweep marks the session failed once its record lapses")
		final = coord.DeviceSignIn{ID: id, Entry: key, Status: status, Detail: detail}
	}
	log.InfoContext(ctx, "device_sign_in_ended", "provider", key, "session", id.String(),
		"status", string(status), "detail", detail)
	return final
}

// sealDeviceLogin exports the login a device sign-in made as the entry's
// bundle, seals it, and re-activates the revision so every node restores it,
// answering the activation it produced.
func (e *Engine) sealDeviceLogin(ctx context.Context, key string, agent *cliagent.Provider,
	by iam.Actor) (string, uint64, error) {

	blob, err := agent.ExportBundle(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("the CLI signed in, and its login could not be exported: %w", err)
	}
	name := e.bundleVariable(key)
	store := fleetsecrets.New(e.backends.Fleet, e.cipher)
	if err = store.Set(ctx, name, blob, secrets.Author{Name: by.Name, Kind: string(by.Kind),
		OperatorID: by.OperatorID}, secrets.SourceModels, e.now()); err != nil {
		return "", 0, fmt.Errorf("the CLI signed in on %s, and its login could not be stored as %s: %w",
			e.selfNode(), name, err)
	}
	writer := e.configWriterOrNil()
	if writer == nil {
		return "", 0, fmt.Errorf("the login is stored as %s, and this node cannot re-activate "+
			"the revision; other nodes take it at the next apply", name)
	}
	if err = writer.Reload(ctx, "reload: device sign-in for "+key, by); err != nil {
		return "", 0, fmt.Errorf("the login is stored as %s, and the re-activation that carries "+
			"it to every node failed: %w", name, err)
	}
	target, found, err := e.backends.Fleet.Target(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("the login is stored as %s and the revision re-activated, and "+
			"the activation could not be read back: %w", name, err)
	}
	if !found {
		return "", 0, nil
	}
	return target.RevisionID, uint64(max(target.Epoch, 0)), nil
}

// bundleVariable is the secret an entry's credential bundle is read from: the
// ${VAR} its auth.credential_bundle names, else the convention
// ([cliprofile.BundleVarName]).
func (e *Engine) bundleVariable(key string) string {
	if c := e.Company(); c != nil && c.Config != nil {
		if spec, ok := c.Config.Providers.LLM[key]; ok {
			source := spec.CredentialSources(key).Bundle
			if name, whole := envref.Whole(source.Value); whole {
				return name
			}
			if source.Fallback != "" {
				return source.Fallback
			}
		}
	}
	return cliprofile.BundleVarName(key)
}

// CancelDeviceSignIn cancels a running session by id: at once where this node
// runs it, and through the record where another does. An ended session
// answers as it ended; [ErrNoSuchSignIn] for an id no live record names.
func (e *Engine) CancelDeviceSignIn(ctx context.Context, id uuid.UUID, by iam.Actor) (coord.DeviceSignIn, error) {
	held, err := e.DeviceSignIn(ctx, id)
	if err != nil {
		return coord.DeviceSignIn{}, err
	}
	if held.Status.Ended() {
		return held, nil
	}
	d := &e.signIns
	d.mu.Lock()
	running := d.running[held.Entry]
	d.mu.Unlock()
	if running != nil && running.id == id {
		running.cancel(errSessionCancelled)
		log.InfoContext(ctx, "device_sign_in_cancelled", "provider", held.Entry,
			"session", id.String(), "by", by.Name)
		deadline := time.Now().Add(deviceFinalWriteBudget)
		for time.Now().Before(deadline) {
			if now, err := e.DeviceSignIn(ctx, id); err == nil && now.Status.Ended() {
				return now, nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return e.DeviceSignIn(ctx, id)
	}
	return e.updateDeviceSignIn(ctx, held.Entry, id, func(s *coord.DeviceSignIn) bool {
		if s.Status.Ended() || s.Status == coord.DeviceCancelRequested {
			return false
		}
		s.Status = coord.DeviceCancelRequested
		return true
	})
}

// DeviceSignIn reads a session by id, [ErrNoSuchSignIn] where no live record
// names it.
func (e *Engine) DeviceSignIn(ctx context.Context, id uuid.UUID) (coord.DeviceSignIn, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return coord.DeviceSignIn{}, fmt.Errorf("%w: %s", ErrNoSuchSignIn, id)
	}
	all, err := e.backends.Fleet.ListDeviceSignIns(ctx)
	if err != nil {
		return coord.DeviceSignIn{}, err
	}
	for _, s := range all {
		if s.ID == id && !s.Lapsed(e.now()) {
			return s, nil
		}
	}
	return coord.DeviceSignIn{}, fmt.Errorf("%w: %s", ErrNoSuchSignIn, id)
}

// heardDeviceSignIn acts on a record this node heard: a cancel asked of a
// session it runs.
func (e *Engine) heardDeviceSignIn(_ context.Context, s coord.DeviceSignIn) {
	if s.Status != coord.DeviceCancelRequested || s.Node != e.selfNode() {
		return
	}
	d := &e.signIns
	d.mu.Lock()
	defer d.mu.Unlock()
	if running := d.running[s.Entry]; running != nil && running.id == s.ID {
		running.cancel(errSessionCancelled)
	}
}

// sweepDeviceSignIns removes every lapsed record and fails every session whose
// node is gone or no longer runs it.
func (e *Engine) sweepDeviceSignIns(ctx context.Context) {
	fleet := e.backends.Fleet
	all, err := fleet.ListDeviceSignIns(ctx)
	if err != nil || len(all) == 0 {
		return
	}
	live, err := e.liveNodeSet(ctx)
	if err != nil {
		return
	}
	for _, s := range all {
		switch {
		case s.Lapsed(e.now()):
			_, _ = fleet.DeleteDeviceSignIn(ctx, s.Entry, s.Version)
		case !s.Status.Ended() && e.sessionOver(s, live):
			failed := s
			failed.Status, failed.Detail = coord.DeviceFailed, errNodeStopping.Error()
			if _, ok, err := fleet.UpdateDeviceSignIn(ctx, failed); err == nil && ok {
				log.InfoContext(ctx, "device_sign_in_orphaned", "provider", s.Entry,
					"session", s.ID.String(), "node", s.Node,
					"detail", "the node running the sign-in is gone; the session is failed so "+
						"a new one can start")
			}
		}
	}
}

// liveNodeSet is the nodes holding a presence lease ([Engine.liveNodes]), this
// one always among them; nil for a node with no lease view, which knows of no
// other node and so cannot say one is gone.
func (e *Engine) liveNodeSet(ctx context.Context) (map[string]bool, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return nil, nil
	}
	nodes, err := e.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{e.selfNode(): true}
	for _, node := range nodes {
		out[node] = true
	}
	return out, nil
}
