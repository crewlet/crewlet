package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// servePollInterval is the wait between one answer of a serve lifecycle's poll
// and the next poll ([harnessprofile.ServePoll]). Half a second because a poll is a
// whole harness launch — about 1.3s each for OpenCode 2.1's `api` — and the crewlet
// server connected 1.3 to 3s after the server was listening (measured): a
// shorter wait adds launches and no earlier start, a longer one adds up to its
// own length to every run's start.
const servePollInterval = 500 * time.Millisecond

// Why a serve lifecycle's start ended without the server up, as the cause of
// the context it is waited for under.
var (
	errServerExited = errors.New("the harness's server exited")
	errServerSlow   = errors.New("the harness's server was not up within timeout_seconds")
)

// harnessServer is a serve-lifecycle harness's own server for one run — see
// [harnessprofile.Serve]: started in its own process group, read on stdout for the
// values it prints once it is up, and stopped with its whole group.
//
// ITS STDOUT IS READ FOR THE READY VALUES AND KEPT NOWHERE: a server prints the
// password it minted there, and its log lines name what it runs. Its stderr is
// kept, capped, for a failure's detail, as a harness's is.
type harnessServer struct {
	pgid  int
	grace time.Duration
	want  []harnessprofile.ReadyValue

	mu     sync.Mutex
	ready  map[string]string
	stderr lockedBuffer

	// up is closed once every ready value has been printed.
	up     chan struct{}
	upOnce sync.Once

	// marker is closed once a failure marker was written on stderr, the
	// line in markerLine.
	marker     chan struct{}
	markerLine atomic.Pointer[string]

	// exited is closed once the server has exited and its streams were read
	// to their end; exitCode is its status then.
	exited   chan struct{}
	exitCode int

	stopping atomic.Bool
	stopOnce sync.Once
}

// lockedBuffer is a capped buffer read while it is still written: a server's
// stderr is read for a failure that the server outlives.
type lockedBuffer struct {
	mu  sync.Mutex
	buf cappedBuffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServer starts the server: the profile's binary with args, in dir, with
// env — never nil, as [run] refuses a nil one — its stdin closed. An error
// means it did not start at all.
func startServer(
	binary string, args []string, dir string, env []string, grace time.Duration,
	want []harnessprofile.ReadyValue, failOn []string,
) (*harnessServer, error) {
	if env == nil {
		return nil, fmt.Errorf("harness: %s's server was about to run with no environment of "+
			"its own, which os/exec would fill with the engine's; it was not started", binary)
	}
	s := &harnessServer{
		grace: grace, want: want, ready: map[string]string{},
		up: make(chan struct{}), marker: make(chan struct{}), exited: make(chan struct{}),
	}
	s.stderr.buf.limit = maxOutput
	// NOT UNDER A CONTEXT: the server lives for the run it serves and is ended
	// by [harnessServer.stop] — its whole group signalled, a grace, then killed —
	// which the run's end calls. A context's cancel would kill the leader
	// alone, leaving what it started (the relay among it) running.
	cmd := exec.Command(binary, args...) //nolint:gosec,noctx // a validated profile's binary and args; ended by stop
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout = &lineTee{sink: io.Discard, on: s.line, limit: maxOutput, every: true}
	cmd.Stderr = &s.stderr
	if len(failOn) > 0 {
		cmd.Stderr = stderrWatch(&s.stderr, failOn, func(line string) {
			s.markerLine.Store(&line)
			close(s.marker)
		})
	}
	procgroup.Set(cmd)
	// Bounds how long a stream something the server started still holds
	// keeps Wait from returning once the server itself has exited.
	cmd.WaitDelay = grace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("harness: starting %q's server: %w", binary, err)
	}
	s.pgid = cmd.Process.Pid
	if len(want) == 0 {
		s.upOnce.Do(func() { close(s.up) })
	}
	go func() {
		_ = cmd.Wait()
		s.exitCode = cmd.ProcessState.ExitCode()
		close(s.exited)
	}()
	return s, nil
}

// line reads one line of the server's stdout for the ready values it carries.
func (s *harnessServer) line(l string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.want {
		if _, seen := s.ready[r.As]; seen || !strings.HasPrefix(l, r.Prefix) {
			continue
		}
		s.ready[r.As] = strings.TrimSpace(strings.TrimPrefix(l, r.Prefix))
	}
	if len(s.ready) == len(s.want) {
		s.upOnce.Do(func() { close(s.up) })
	}
}

// values is the ready values printed so far, by their names.
func (s *harnessServer) values() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.ready)
}

// stop ends the server — its group signalled, a grace, then killed — and waits
// for it. The group is killed whatever the server did, because what it started
// (the relay among them) is in it. Safe to call more than once.
func (s *harnessServer) stop() {
	s.stopOnce.Do(func() {
		s.stopping.Store(true)
		select {
		case <-s.exited:
		default:
			_ = procgroup.Terminate(s.pgid)
			select {
			case <-s.exited:
			case <-time.After(s.grace):
			}
		}
		if err := procgroup.Kill(s.pgid); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			log.Warn("harness_server_kill_failed", "pgid", s.pgid, "error", err)
		}
		<-s.exited
	})
}

// lost reports a server that exited on its own, without being stopped.
func (s *harnessServer) lost() bool {
	select {
	case <-s.exited:
		return !s.stopping.Load()
	default:
		return false
	}
}

// alive reports a server not yet exited or stopped.
func (s *harnessServer) alive() bool {
	select {
	case <-s.exited:
		return false
	default:
		return !s.stopping.Load()
	}
}

// markerSeen is the stderr line that carried a failure marker, or "".
func (s *harnessServer) markerSeen() string {
	if line := s.markerLine.Load(); line != nil {
		return *line
	}
	return ""
}

// up starts a serve lifecycle's server, waits until it is up — every ready
// value printed, the poll showing the crewlet server started — and renders what
// the run is handed from its ready values. Nothing to do on the run lifecycle.
//
// Called after everything the run writes into its working directory — the
// route's seeds, the system prompt's file — because the server reads its
// configuration as it starts, and its loop is the one the system prompt is for.
//
// It returns the context the run is made under — one the server's end aborts —
// and FALSE WITH NO ERROR when the host ended the run before the server was up:
// nothing ran, so nothing is charged and the run's answer is empty.
func (t *toolRun) up(ctx context.Context, p *Provider, in *invocation) (context.Context, bool, error) {
	m := p.profile.MCP
	if !m.Serves() {
		return ctx, true, nil
	}
	args := make([]string, 0, len(m.Serve.Args))
	for _, arg := range m.Serve.Args {
		args = append(args, m.Render(arg, t.values))
	}
	srv, err := startServer(p.profile.Binary, args, in.dir, slices.Clone(in.env), in.grace,
		m.Serve.Ready, in.failOn)
	if err != nil {
		// No server ran: the binary is missing or the host would give it no
		// process — the one exec failure the next member is no answer to.
		return nil, false, p.fail(llm.KindFatal, err)
	}
	t.server = srv

	// ONE STRETCH FOR THE START, of timeout_seconds: a server's start is
	// the harness's start, which the first stretch of a run lifecycle's harness
	// covers too.
	waitCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	slow := time.AfterFunc(p.timeout, func() { cancel(errServerSlow) })
	defer slow.Stop()
	go func() {
		select {
		case <-t.host.Done():
			cancel(errHostStopped)
		case <-srv.exited:
			cancel(errServerExited)
		case <-srv.marker:
			cancel(&failureMarkerError{line: srv.markerSeen()})
		case <-waitCtx.Done():
		}
	}()
	select {
	case <-srv.up:
	case <-waitCtx.Done():
		return t.notUp(ctx, waitCtx, p, "")
	}
	t.values.Ready = srv.values()
	readyEnv := make([]string, 0, len(t.readyEnv))
	for _, name := range t.readyEnv {
		readyEnv = append(readyEnv, name+"="+m.Render(m.Env[name], t.values))
	}
	if poll := m.Serve.Poll; poll != nil {
		pollArgs := make([]string, 0, len(poll.Args))
		for _, arg := range poll.Args {
			pollArgs = append(pollArgs, m.Render(arg, t.values))
		}
		pollEnv := slices.Concat(in.env, readyEnv)
		last := ""
		for {
			out, pollErr := p.runAuxiliary(waitCtx, pollArgs, in.dir, pollEnv, "")
			if pollErr == nil && started(out, poll) {
				break
			}
			// Asked again: a server that has printed its address may not
			// answer yet. The last answer is what a server never up says —
			// not a poll the start's own end cut short.
			switch {
			case waitCtx.Err() != nil:
			case pollErr != nil:
				last = pollErr.Error()
			default:
				last = "it did not list the crewlet server as started"
			}
			select {
			case <-time.After(servePollInterval):
				continue
			case <-waitCtx.Done():
				return t.notUp(ctx, waitCtx, p, last)
			}
		}
	}
	for i, tmpl := range t.readyArgs {
		in.args[i] = m.Render(tmpl, t.values)
	}
	in.env = append(in.env, readyEnv...)
	t.routeEnv = append(t.routeEnv, readyEnv...)

	// THE RUN IS THE SERVER'S CLIENT, AND THE SERVER'S END IS ITS END: the
	// run is aborted the moment the server exits under it or writes a failure
	// marker; and the server is stopped the moment the run is cut short —
	// its loop is the server's, and a server left to it would keep its model
	// working for a run nobody reads.
	runCtx, abort := context.WithCancelCause(ctx)
	t.abort = abort
	t.watchStop = make(chan struct{})
	t.watchDone = make(chan struct{})
	go func() {
		defer close(t.watchDone)
		select {
		case <-srv.exited:
			t.abort(errServerExited)
		case <-srv.marker:
			t.abort(&failureMarkerError{line: srv.markerSeen()})
			srv.stop()
		case <-t.host.Done():
			srv.stop()
		case <-t.clock.expired():
			srv.stop()
		case <-t.watchStop:
		}
	}()
	return runCtx, true, nil
}

// notUp ends a start that ended without the server up, the server stopped:
// its failure by its cause, or not started and no failure for a start the host
// ended.
func (t *toolRun) notUp(
	ctx, waitCtx context.Context, p *Provider, last string,
) (context.Context, bool, error) {
	t.server.stop()
	cause := context.Cause(waitCtx)
	detail := failureDetail(t.server.stderr.String(), "")
	var marker *failureMarkerError
	switch {
	case errors.Is(cause, errHostStopped):
		return nil, false, nil
	case errors.As(cause, &marker):
		return nil, false, p.confinementFailed(marker.line, t.server.stderr.String())
	case errors.Is(cause, errServerExited):
		return nil, false, p.fail(llm.KindServer, fmt.Errorf(
			"the %s harness's server exited %d before it was up%s", p.harness, t.server.exitCode, detail))
	case errors.Is(cause, errServerSlow):
		why := "it did not print every value the profile's mcp.serve.ready waits for"
		if last != "" {
			why = "its poll last said: " + last
		}
		return nil, false, p.fail(llm.KindTimeout, fmt.Errorf(
			"the %s harness's server was not up within %s — %s%s", p.harness, p.timeout, why, detail))
	default:
		return nil, false, p.fail(llm.KindTimeout, fmt.Errorf(
			"the call ended while the %s harness's server was starting: %w", p.harness, context.Cause(ctx)))
	}
}

// started reports whether a poll's answer lists the crewlet server as started:
// its entry there, and not starting.
func started(out string, poll *harnessprofile.ServePoll) bool {
	doc, ok := decodeObject(strings.TrimSpace(out))
	if !ok {
		return false
	}
	items, ok := lookup(doc, poll.Items)
	if !ok {
		return false
	}
	list, ok := items.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok || !matches(entry, poll.Match) {
			continue
		}
		return len(poll.Starting) == 0 || !matches(entry, poll.Starting)
	}
	return false
}

// ran settles the server once the run is over: stopped now when the run was
// cut short — kept for the usage command otherwise — and its stderr put before
// the run's, so a failure's detail says what the loop said.
func (t *toolRun) ran(ctx context.Context, res *rawResult) {
	srv := t.server
	if srv == nil {
		return
	}
	t.lost = srv.lost()
	if res.timedOut || res.stopped || res.failureMarker != "" || ctx.Err() != nil || t.lost {
		srv.stop()
	}
	if line := srv.markerSeen(); line != "" && res.failureMarker == "" {
		res.failureMarker = line
	}
	if said := srv.stderr.String(); strings.TrimSpace(said) != "" {
		res.stderr = "[its server]\n" + said + "\n[the run]\n" + res.stderr
	}
}

// usageCommand is the usage command of a run with tools, rendered, and the
// environment it runs in — false when there is none to ask, or nothing left to
// ask it of: a serve lifecycle's server gone with a run cut short.
func (t *toolRun) usageCommand(p *Provider, session string, base []string) ([]string, []string, bool) {
	m := p.profile.MCP
	if len(m.UsageArgs) == 0 {
		return nil, nil, false
	}
	if t.server != nil && !t.server.alive() {
		return nil, nil, false
	}
	args := make([]string, 0, len(m.UsageArgs))
	for _, arg := range m.UsageArgs {
		args = append(args, strings.ReplaceAll(m.Render(arg, t.values), "{session}", session))
	}
	return args, slices.Concat(base, t.routeEnv), true
}
