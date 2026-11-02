package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// THE LOGIN WATCHER keeps, for every model entry that signs in with the CLI's
// own login on this node's disk, what the CLI says of that login (M12 of the
// Models design): signed in, signed out, or unknown, and as whom. The Models
// page shows it per node, with the command to run there; it is never a
// sign-in. The engine does not log a CLI in; a person runs `crewlet llm login`
// on the node.
//
// # When it asks
//
// The CLI's status command is local and never calls the network, but it is a
// process, so it is not run per read. It runs for an entry at boot and at
// every apply, when the login's files change (a stat each [hostLoginBeat]),
// and otherwise every [hostLoginRecheck]. A person's "check now" runs it at
// once ([Engine.CheckHostLogin]).
//
// # Who has to agree on it
//
// This node alone, in memory: the login is on this node's disk.

// hostLoginBeat is how often the watcher stats each login's files: the
// coordination heartbeat, which is the cadence the answer is published at, so
// a login made on the node is on the page by the next beat.
const hostLoginBeat = coord.ReconcileInterval

// hostLoginRecheck is how long an answer stands with nothing changed on disk:
// TEN MINUTES, the readiness memory's own horizon (cliagent's readinessMemory),
// so the page and a call's readiness verdict go stale on the same clock. A
// login that expires on the vendor's side without a file changing is said
// within it.
const hostLoginRecheck = 10 * time.Minute

// ErrNoModel is a model entry the active revision does not have.
var ErrNoModel = errors.New("engine: no such model entry")

// ErrNoHostLogin is an entry that does not sign in with a CLI login on this
// node: it signs in with a token, a key or its env, or its CLI has no status
// command.
var ErrNoHostLogin = errors.New("engine: the model does not sign in with a CLI login on this node")

// hostLogins is the watcher's state and its loop.
type hostLogins struct {
	mu   sync.Mutex
	rows map[string]hostLoginRow

	once sync.Once
	kick chan struct{}

	cancel context.CancelFunc
	done   chan struct{}
}

// hostLoginRow is one entry's latest answer and the login files' stamp it was
// read under.
type hostLoginRow struct {
	login coord.HostLogin
	stamp string
}

func (h *hostLogins) kicks() chan struct{} {
	h.once.Do(func() { h.kick = make(chan struct{}, 1) })
	return h.kick
}

// startHostLogins checks every login now and keeps them checked.
func (e *Engine) startHostLogins(ctx context.Context) {
	h := &e.hostLogins
	kick := h.kicks()
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(hostLoginBeat)
		defer ticker.Stop()
		force := true
		for {
			e.checkHostLogins(loopCtx, force)
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				force = false
			case <-kick:
				force = true
			}
		}
	}()
}

// stopHostLogins ends the loop and waits for it.
func (e *Engine) stopHostLogins() {
	if e.hostLogins.cancel == nil {
		return
	}
	e.hostLogins.cancel()
	<-e.hostLogins.done
}

// kickHostLogins asks for every login to be checked now (an apply).
func (e *Engine) kickHostLogins() {
	select {
	case e.hostLogins.kicks() <- struct{}{}:
	default:
	}
}

// loginEntries is every entry of the active revision that signs in with a CLI
// login here, by key.
func (e *Engine) loginEntries() map[string]*cliagent.Provider {
	c := e.Company()
	if c == nil || c.Config == nil || c.Models == nil {
		return nil
	}
	out := map[string]*cliagent.Provider{}
	for key := range c.Config.Providers.LLM {
		provider, ok := c.Models.Provider(key)
		if !ok {
			continue
		}
		if agent, isCLI := provider.(*cliagent.Provider); isCLI && agent.SignsInWithLogin() {
			out[key] = agent
		}
	}
	return out
}

// checkHostLogins runs the status command for every login entry whose answer is
// missing, stale or read under other files, or for all of them when forced, and
// forgets the entries that no longer sign in with a login.
func (e *Engine) checkHostLogins(ctx context.Context, force bool) {
	entries := e.loginEntries()
	h := &e.hostLogins
	now := e.now()
	h.mu.Lock()
	for key := range h.rows {
		if _, still := entries[key]; !still {
			delete(h.rows, key)
		}
	}
	due := map[string]*cliagent.Provider{}
	for key, agent := range entries {
		row, known := h.rows[key]
		if force || !known || row.stamp != agent.LoginStamp() || now.Sub(row.login.CheckedAt) >= hostLoginRecheck {
			due[key] = agent
		}
	}
	h.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(due)) {
		if ctx.Err() != nil {
			return
		}
		e.recordHostLogin(ctx, key, due[key])
	}
}

// recordHostLogin runs one entry's status command and keeps its answer.
func (e *Engine) recordHostLogin(ctx context.Context, key string, agent *cliagent.Provider) coord.HostLogin {
	stamp := agent.LoginStamp()
	status, err := agent.LoginStatus(ctx)
	login := coord.HostLogin{
		State:     coord.HostLoginState(status.State),
		Method:    status.Method,
		Account:   status.Account,
		Detail:    status.Detail,
		CheckedAt: status.CheckedAt,
	}
	if err != nil {
		login.State, login.Detail = coord.HostLoginUnknown, err.Error()
	}
	if !login.State.Valid() {
		login.State = coord.HostLoginUnknown
	}
	if login.CheckedAt.IsZero() {
		login.CheckedAt = e.now()
	}
	h := &e.hostLogins
	h.mu.Lock()
	if h.rows == nil {
		h.rows = map[string]hostLoginRow{}
	}
	h.rows[key] = hostLoginRow{login: login, stamp: stamp}
	h.mu.Unlock()
	return login
}

// hostLoginOf is an entry's latest answer, nil when it has none.
func (e *Engine) hostLoginOf(key string) *coord.HostLogin {
	h := &e.hostLogins
	h.mu.Lock()
	defer h.mu.Unlock()
	row, ok := h.rows[key]
	if !ok {
		return nil
	}
	login := row.login
	return &login
}

// CheckHostLogin runs an entry's status command on this node now and answers
// what it said, which the heartbeat publishes from then on. [ErrNoModel] for an
// entry the active revision does not have, and [ErrNoHostLogin] for one that
// does not sign in with a CLI login here.
func (e *Engine) CheckHostLogin(ctx context.Context, key string) (coord.HostLogin, error) {
	c := e.Company()
	if c == nil || c.Config == nil || c.Models == nil {
		return coord.HostLogin{}, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	if _, declared := c.Config.Providers.LLM[key]; !declared {
		return coord.HostLogin{}, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	agent, ok := e.loginEntries()[key]
	if !ok {
		return coord.HostLogin{}, fmt.Errorf("%w: %q", ErrNoHostLogin, key)
	}
	return e.recordHostLogin(ctx, key, agent), nil
}
