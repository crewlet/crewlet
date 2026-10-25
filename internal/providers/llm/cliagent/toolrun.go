package cliagent

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// toolRun is one CLI run that is handed a phase's tools: the host's server for
// the run, the reader of the CLI's record, the clock its model's work is
// bounded by, and the carry of a login it refreshes on the way.
//
// # The shape of a run
//
// The host serves its tools for this run alone ([llm.ToolHost.Serve]); the
// profile's tool route tells the CLI how to start the relay to them
// ([cliprofile.MCP]); the CLI runs its own loop, calling them as its model
// asks; and the run ends when the model stops or when the host needs it to
// ([llm.ToolHost.Done]), which stops the CLI the way a timeout does — its
// group signalled, a grace, then killed. Then the host's server for the run is
// released, which waits for every call still running on it.
type toolRun struct {
	host   llm.ToolHost
	served llm.Served
	reader *recordReader
	clock  *stretch
	carry  *carrier

	// tailing is a file record's follower, stopped once the CLI has exited.
	stopTail chan struct{}
	waitTail func()

	releaseOnce sync.Once
	connected   bool
}

// serveTools opens the host to this run and hands the CLI its server: the
// profile's tool route rendered into the run's argv, environment and working
// directory, the clock the model's work is bounded by, the host's word that
// the run must end, and the reader of the CLI's record.
//
// Called once the run holds its slot and its checkout, before the system
// prompt and the prompt are put on the argv — the route's arguments precede
// them, as the model's do.
func (p *Provider) serveTools(
	ctx context.Context, req llm.Request, checkout *Checkout, s *slot, in *invocation, session, sent string,
) (*toolRun, error) {
	m := p.profile.MCP
	t := &toolRun{
		host:   req.Tools,
		reader: newRecordReader(p.profile, req, sent),
		clock:  newStretch(p.timeout),
		carry:  startCarrier(ctx, p, checkout),
	}
	// THE RUN'S SLOT RIDES THE CONTEXT ITS CALLS RUN UNDER, so a model call
	// one of them makes on this entry runs on it rather than waiting for a
	// slot only this run can free — see slots.go.
	served, err := req.Tools.Serve(withLease(ctx, s.lend()), llm.ServeOptions{
		Relists:          m.Relists,
		ToolTimeout:      m.ToolTimeout(),
		ObservesRequests: t.reader.observes(),
		OnCall:           t.onCall,
	})
	if err != nil {
		t.carry.stop()
		// The engine could not open its own end — a socket it could not
		// bind, a phase with no session host. Nothing about the CLI or
		// its vendor is at fault, and the next member would fail the same.
		return nil, p.fail(llm.KindFatal, fmt.Errorf("serving the seat's tools to the CLI: %w", err))
	}
	t.served = served
	values := cliprofile.MCPValues{
		RelayCommand: served.Command, RelayArgs: served.Args, Work: checkout.Work,
	}
	if n := len(served.Args); n > 0 {
		// The relay's socket is its last argument (toolsession's Command).
		values.Socket = served.Args[n-1]
	}
	for _, f := range m.Seed {
		if err := writeSeed(checkout.Work, f.Path, m.Render(f.Content, values)); err != nil {
			t.close()
			return nil, p.fail(llm.KindFatal, err)
		}
	}
	for _, arg := range append(append([]string(nil), m.Args...), m.AllowArgs...) {
		in.args = append(in.args, m.Render(arg, values))
	}
	// APPENDED, so they win over every layer below: the tool timeout the host
	// was told of must be the one the CLI applies.
	for _, name := range slices.Sorted(maps.Keys(m.Env)) {
		in.env = append(in.env, name+"="+m.Render(m.Env[name], values))
	}
	in.clock = t.clock
	in.stop = req.Tools.Done()
	in.onLine, in.everyLine = t.reader.stdoutLine, true
	if r := p.profile.Requests; r != nil && r.Source == cliprofile.SourceFile {
		root := checkout.Work
		if r.In == cliprofile.SeedHome {
			root = checkout.Home
		}
		pattern := filepath.Join(root, strings.ReplaceAll(r.File, "{session}", session))
		t.stopTail = make(chan struct{})
		t.waitTail = tailRecord(pattern, t.reader.fileLine, t.stopTail)
	}
	return t, nil
}

// onCall is the host's word that the first of its tools in flight started, or
// the last one returned. Called under the host's own lock, so it only flips
// the clock and nudges the carry — neither waits on anything.
func (t *toolRun) onCall(busy bool) {
	if busy {
		t.clock.pause()
		t.carry.nudge()
		return
	}
	t.clock.resume()
}

// finish reads the rest of the record once the CLI has exited, tells the host
// every request it showed, and releases the host's server for the run —
// reporting whether the CLI ever reached its tools.
func (t *toolRun) finish() (connected bool) {
	if t.waitTail != nil {
		close(t.stopTail)
		t.waitTail()
		t.waitTail = nil
	}
	t.reader.finish()
	return t.release()
}

// release ends the run's connection to the host once: no new call, every call
// running on it returned.
func (t *toolRun) release() bool {
	t.releaseOnce.Do(func() {
		if t.served.Release != nil {
			t.connected = t.served.Release()
		}
	})
	return t.connected
}

// close is the run's teardown on every way out: the tail stopped, the server
// released, the carry stopped and waited for — before the checkout is
// released, which the carry works in.
func (t *toolRun) close() {
	if t.waitTail != nil {
		close(t.stopTail)
		t.waitTail()
		t.waitTail = nil
	}
	t.release()
	t.clock.stop()
	t.carry.stop()
}

// writeSeed writes one of the tool route's files into the call's working
// directory, 0600 — it names the run's socket, which is this run's alone.
func writeSeed(work, rel, content string) error {
	path, err := cliprofile.UnderRoot(work, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("cli-agent: writing the tool route's %q: %w", rel, err)
	}
	return nil
}

// carrier carries a refreshed login out of a running call's home at each of
// its tool calls ([Checkout.carryRefreshed]), one carry at a time, on a
// goroutine of its own: the host tells a run a call started under its own
// lock, and a carry waits on the shared login's lock and may run the CLI's
// export, neither of which it may wait for.
type carrier struct {
	poke    chan struct{}
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// startCarrier starts the carry for a checkout that has a login to carry: not
// a key home, and a CLI that keeps a login at all. Nil otherwise, which every
// method takes as nothing to do.
func startCarrier(ctx context.Context, p *Provider, c *Checkout) *carrier {
	if c == nil || c.keyed || !p.profile.KeepsLogin() {
		return nil
	}
	k := &carrier{poke: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(k.stopped)
		for {
			select {
			case <-k.done:
				return
			case <-k.poke:
				// Under the call's own context: a carry the call's end cuts
				// short is made at the release instead, which runs whatever
				// cancelled the call.
				if err := c.carryRefreshed(ctx); err != nil && ctx.Err() == nil {
					log.WarnContext(ctx, "cli_agent_carry_failed", "provider", p.key,
						"seat", llm.SeatOf(ctx), "error", err)
				}
			}
		}
	}()
	return k
}

// nudge asks for a carry; one already asked for and not yet made covers it.
func (k *carrier) nudge() {
	if k == nil {
		return
	}
	select {
	case k.poke <- struct{}{}:
	default:
	}
}

// stop ends the carry and waits for one in progress.
func (k *carrier) stop() {
	if k == nil {
		return
	}
	k.once.Do(func() {
		close(k.done)
		<-k.stopped
	})
}
