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

	// values are the run's substitutions into the profile's tool route; a
	// serve lifecycle's ready values join them once its server is up.
	values cliprofile.MCPValues
	// readyArgs are the run's arguments, by position, that carry a ready
	// value, as their templates; readyEnv the names of the route's variables
	// that do. Rendered once the server is up ([toolRun.up]).
	readyArgs map[int]string
	readyEnv  []string
	// routeEnv is the route's environment, rendered, for the usage command.
	routeEnv []string

	// server is a serve lifecycle's own server; nil on the run lifecycle.
	// abort ends the context the run is made under — the server exiting
	// under it, or failing closed — and the watcher that calls it is
	// stopped by watchStop and closes watchDone. lost reports a server that
	// exited on its own while the run went on.
	server    *cliServer
	abort     context.CancelCauseFunc
	watchStop chan struct{}
	watchDone chan struct{}
	lost      bool
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
		host: req.Tools,
		// AS A RUN WITH TOOLS PRINTS: its output may be the route's own
		// ([cliprofile.MCP.Output]).
		reader: newRecordReader(p.profile.Routed(), req, sent),
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
		Concurrency:      callConcurrency(m.Concurrency),
		CallIDMeta:       callIDMeta(p.profile.Requests),
		RecordsCallsLate: p.profile.Requests != nil && p.profile.Requests.LateCalls,
	})
	if err != nil {
		t.carry.stop()
		// The engine could not open its own end — a socket it could not
		// bind, a phase with no session host. Nothing about the CLI or
		// its vendor is at fault, and the next member would fail the same.
		return nil, p.fail(llm.KindFatal, fmt.Errorf("serving the seat's tools to the CLI: %w", err))
	}
	t.served = served
	t.values = cliprofile.MCPValues{
		RelayCommand: served.Command, RelayArgs: served.Args, Work: checkout.Work,
	}
	if n := len(served.Args); n > 0 {
		// The relay's socket is its last argument (toolsession's Command).
		t.values.Socket = served.Args[n-1]
	}
	for _, f := range m.Seed {
		if err := writeSeed(checkout.Work, f.Path, m.Render(f.Content, t.values)); err != nil {
			t.close()
			return nil, p.fail(llm.KindFatal, err)
		}
	}
	// AN ARGUMENT CARRYING A VALUE THE CLI'S SERVER PRINTS waits for it, as
	// its template, and is rendered once the server is up — from the template,
	// so nothing substituted into it is read for a placeholder twice.
	t.readyArgs = map[int]string{}
	place := func(i int, tmpl string) {
		if m.ReadyPlaceholder(tmpl) {
			t.readyArgs[i] = tmpl
			return
		}
		in.args[i] = m.Render(tmpl, t.values)
	}
	// The route's complete_args lead the argv ([Provider.argv]).
	for i, tmpl := range m.CompleteArgs {
		place(i, tmpl)
	}
	for _, tmpl := range slices.Concat(m.Args, m.AllowArgs) {
		in.args = append(in.args, tmpl)
		place(len(in.args)-1, tmpl)
	}
	// APPENDED, so they win over every layer below: the tool timeout the host
	// was told of must be the one the CLI applies.
	for _, name := range slices.Sorted(maps.Keys(m.Env)) {
		if m.ReadyPlaceholder(m.Env[name]) {
			t.readyEnv = append(t.readyEnv, name)
			continue
		}
		pair := name + "=" + m.Render(m.Env[name], t.values)
		in.env = append(in.env, pair)
		t.routeEnv = append(t.routeEnv, pair)
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
		t.waitTail = tailRecord(pattern, recordOwner(r.Owner, session, checkout.Work), t.reader.fileLine, t.stopTail)
	}
	return t, nil
}

// recordOwner is the test a file record's head must pass to be this run's
// ([cliprofile.RecordOwner]), or nil where the glob alone names it: handed the
// file's lines from its first, it says whether the line decides — the first
// line, or the first matching the owner's Match — and whether the file is this
// run's.
//
// The working directory is compared as the CLI reports it, which is the path
// the kernel resolved — so the call's directory is accepted as written and with
// its links resolved, as a state_dir under a symlinked /tmp reads either way.
func recordOwner(o *cliprofile.RecordOwner, session, work string) func(line string) (decided, owned bool) {
	if o == nil {
		return nil
	}
	var want []string
	switch o.Equals {
	case "{session}":
		want = []string{session}
	case "{work}":
		want = []string{filepath.Clean(work)}
		if real, err := filepath.EvalSymlinks(work); err == nil && real != want[0] {
			want = append(want, real)
		}
	}
	return func(line string) (bool, bool) {
		doc, ok := decodeRecordLine(line)
		if len(o.Match) > 0 && (!ok || !matches(doc, o.Match)) {
			// Not the line that names the run: the next one may be.
			return false, false
		}
		if !ok {
			return true, false
		}
		got, ok := stringAt(doc, o.Path)
		if !ok || got == "" {
			return true, false
		}
		if o.Equals == "{work}" {
			got = filepath.Clean(got)
		}
		return true, slices.Contains(want, got)
	}
}

// callConcurrency is the profile's word for which of one response's calls the
// CLI makes at once, in the host's. The profile's is required and validated,
// so at_once is the only value the default reaches.
func callConcurrency(c cliprofile.Concurrency) llm.CallConcurrency {
	switch c {
	case cliprofile.ConcurrencyReadOnly:
		return llm.CallsReadOnlyAtOnce
	case cliprofile.ConcurrencyInTurn:
		return llm.CallsInTurn
	default:
		return llm.CallsAtOnce
	}
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

// close is the run's teardown on every way out: the tail stopped, the host's
// server released, a serve lifecycle's own server stopped, the carry stopped
// and waited for — before the checkout is released, which the carry works in
// and the CLI's server runs in.
func (t *toolRun) close() {
	if t.waitTail != nil {
		close(t.stopTail)
		t.waitTail()
		t.waitTail = nil
	}
	if t.watchStop != nil {
		close(t.watchStop)
		<-t.watchDone
		t.watchStop = nil
	}
	if t.server != nil {
		t.server.stop()
	}
	if t.abort != nil {
		t.abort(nil)
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

// callIDMeta is the `_meta` key the CLI sends its call ids under, where its
// profile reads its requests and names one.
func callIDMeta(r *cliprofile.Requests) string {
	if r == nil {
		return ""
	}
	return r.CallIDMeta
}
