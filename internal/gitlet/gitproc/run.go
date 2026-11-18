package gitproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Run runs one git process — and everything it starts — to its end.
//
// Safe for concurrent use. Every check on c happens before any slot is taken.
// The process leads its own group, which ctx, the deadline or a stall ends.
// Run returns only once the group is swept and every byte git wrote has
// reached the caller: no goroutine it started is running and no pipe end it
// made is open.
//
// A non-zero exit that is not one of c.Answers is an [*ExitError] whose Kind
// is classified from its process tree's trace2 error events (trace.go). The
// Result is returned beside every error once the process started.
func (p *Pool) Run(ctx context.Context, c Cmd) (Result, error) {
	prep, err := p.prepare(c)
	if err != nil {
		return Result{}, err
	}
	release, waited, err := p.admit(ctx, c.Class, c.Share)
	if p.metrics != nil {
		p.metrics.Observe(metrics.CodeGitWait, waited, metrics.Attrs{"class": string(c.Class)})
		if c.Class == ClassTransfer {
			p.metrics.Observe(metrics.CodeGitShareWait, waited, metrics.Attrs{"share": string(c.Share.Class)})
		}
	}
	if err != nil {
		return Result{Waited: waited}, err
	}
	var res Result
	// Deferred first, so it runs last: the slot is released, and a transfer's
	// charge settled from this Result's CPU, only once the process is reaped
	// and its group swept. A run that never started settles occupancy alone.
	defer release(&res)
	lent := p.lend(c)
	if lent != nil {
		// Repaid here at the latest, however the run ends; earlier when the
		// call site closes Cmd.Indexed (the watcher).
		defer lent.repay()
		prep.env[prep.threads] = threadsValue(1 + lent.cores)
	}
	res, err = p.run(ctx, c, prep, lent)
	res.Waited = waited
	p.report(c, res)
	return res, err
}

// admit takes a slot in c's lane: a FIFO wait of at most SlotWait for reads
// and transfers, a free slot or none for maintenance. A caller never waits
// for a slot while holding one in the same lane, or lanes deadlock (package
// doc, "Slots: three lanes, each sized by what it spends").
//
// A TRANSFER waits for the slot its share grants (share.go), and its release
// charges the CPU in the Result it is handed (none for nil). The other lanes
// ignore the share and the Result.
func (p *Pool) admit(ctx context.Context, c Class, s Share) (release func(*Result), waited time.Duration, err error) {
	start := time.Now()
	l := p.lane(c)
	if ctx.Err() != nil {
		return nil, 0, fmt.Errorf("gitproc: git was not started: its caller had already gone: %w", context.Cause(ctx))
	}
	if c == ClassMaintenance {
		free, refused := p.tryMaintenance()
		if refused != nil {
			return nil, time.Since(start), refused
		}
		return func(*Result) { free() }, time.Since(start), nil
	}
	wait, cancel := context.WithTimeout(ctx, p.slotWait)
	defer cancel()
	r, err := l.acquire(wait, s)
	if err != nil {
		waited = time.Since(start)
		if ctx.Err() != nil {
			return nil, waited, fmt.Errorf("gitproc: waiting for a %s slot: its caller went away: %w", c, context.Cause(ctx))
		}
		return nil, waited, fmt.Errorf("%w: all %d %s slots on this node stayed busy for %v", ErrBusy, l.size, c, p.slotWait)
	}
	return func(res *Result) {
		var cpu time.Duration
		if res != nil {
			cpu = res.CPU
		}
		l.release(r, cpu)
	}, time.Since(start), nil
}

// run starts the process and sees it, its group and its pipes to their end.
// lent is the run's loan, or nil.
func (p *Pool) run(ctx context.Context, c Cmd, prep prepared, lent *loan) (Result, error) {
	cmd := exec.Command(p.git, c.Args...) //nolint:noctx // not CommandContext: its Cancel ends the leader only, and the watcher below ends the GROUP
	cmd.Dir = prep.wd
	cmd.Env = prep.env
	procgroup.Set(cmd)

	// RUN OWNS THE FOUR PIPES. Every stdio is an *os.File, so os/exec starts
	// no copier and needs no WaitDelay, whose timer would cut a slow client's
	// clone short (see drain). The fourth is the trace2 event stream, the
	// child's fd 3: ExtraFiles[0], which GIT_TRACE2_EVENT=3 in the base
	// environment names, so a later extra file takes index 1 and up.
	outR, outW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("gitproc: making a pipe for git %s: %w", c.Args[0], err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return Result{}, fmt.Errorf("gitproc: making a pipe for git %s: %w", c.Args[0], err)
	}
	traceR, traceW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW, errR, errW)
		return Result{}, fmt.Errorf("gitproc: making a pipe for git %s: %w", c.Args[0], err)
	}
	var inR, inW *os.File
	if c.Stdin != nil {
		if inR, inW, err = os.Pipe(); err != nil {
			closeAll(outR, outW, errR, errW, traceR, traceW)
			return Result{}, fmt.Errorf("gitproc: making a pipe for git %s: %w", c.Args[0], err)
		}
		cmd.Stdin = inR
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.ExtraFiles = []*os.File{traceW}

	start := p.now()
	if err = cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW, traceR, traceW, inR, inW)
		return Result{}, fmt.Errorf("gitproc: starting git %s: %w", c.Args[0], err)
	}
	// The CHILD's ends close at once, or no copier would ever see end of file.
	// os.Pipe sets close-on-exec, so no other child holds them.
	closeAll(outW, errW, traceW, inR)

	leader, err := procgroup.Identify(cmd.Process.Pid)
	if err != nil {
		// Unreaped, the pid is still ours, so its group may be signalled
		// without a recorded identity — and must be, as nothing later could
		// be checked against one.
		_ = procgroup.Kill(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		closeAll(outR, errR, traceR, inW)
		return Result{}, fmt.Errorf("gitproc: reading git %s's process identity: %w — without it no later signal can be checked", c.Args[0], err)
	}
	p.track(c.Class, +1)

	errs := &stderrSink{}
	var known *strings.Replacer
	if c.Remote != nil {
		known = c.Remote.known()
		errs.known = known
	}
	trace := newTraceSink(c.Dir)
	var capt *capture
	dst := c.Stdout
	if dst == nil {
		capt = &capture{max: p.captureMax}
		dst = capt
	}
	// A remote with a secret gets stdout a line at a time, secrets taken out,
	// the last line flushed inside the copier so Run still waits for it.
	var redacting *lineRedactor
	if known != nil {
		redacting = &lineRedactor{w: dst, known: known}
		dst = redacting
	}
	out := startCopier("stdout", outR, func(cp *copier) {
		cp.pump(dst)
		if redacting != nil && cp.callerErr == nil {
			cp.state.Store(inCaller)
			cp.callerErr = redacting.flush()
		}
	})
	errc := startCopier("stderr", errR, func(cp *copier) { cp.pump(errs) })
	// The trace counts toward no stall (moved): a pack-objects writing region
	// events must not keep a dead client's transfer alive.
	tracec := startCopier("trace", traceR, func(cp *copier) { cp.pump(trace) })
	copiers := []*copier{out, errc, tracec}
	var in *copier
	if inW != nil {
		in = startCopier("stdin", inW, func(cp *copier) { cp.feed(c.Stdin) })
		copiers = append(copiers, in)
	}

	moved := func() int64 {
		n := out.n.Load() + errs.n.Load()
		if in != nil {
			n += in.n.Load()
		}
		return n
	}
	w := p.watch(ctx, c, leader, cmd.Process, moved, lent)
	waitErr := cmd.Wait()
	reaped := p.now()
	close(w.reaped)
	<-w.done

	// THE SWEEP, after the reap and before the drain: a pipe ends only when
	// every holder of its write end has gone, and a group member that
	// outlived its leader is one. Leader.Current answers false at once when
	// the leader was the last member, so the common case costs two system
	// calls. A straggler's CPU is in no reap record, so it is read first.
	var stragglers time.Duration
	if p.signal(c, leader, "KILL", p.counted(c, &stragglers, procgroup.Kill)) {
		p.log.Warn("code_git_straggler_killed", "class", c.Class, "command", c.Args[0], "pid", leader.PID)
	}
	p.track(c.Class, -1)
	p.drain(c, copiers)
	errs.finish()
	trace.finish()

	res := Result{
		Stderr: errs.tail(),
		Damage: trace.damage(start),
		Trace:  trace.summary(),
		Exit:   cmd.ProcessState.ExitCode(),
		Took:   reaped.Sub(start),
		Began:  start,
		CPU:    max(cpuOf(cmd.ProcessState)+stragglers, w.spent),
	}
	if capt != nil {
		res.Stdout = capt.buf
	}
	if c.Remote != nil {
		// A REMOTE RUN'S LINES ARE REDACTED BEFORE ANYTHING READS THEM (its
		// ExitError, the observer, a caller's log): stderr lost the remote's
		// secrets in the sink, the damage loses them here, and both then go
		// through internal/redact, as a remote's text is from outside. Stdout
		// lost them in lineRedactor and no more: it is the run's ANSWER (an
		// ls-remote's refs, a push's statuses), which its caller parses, so no
		// guess at a credential's shape may rewrite it.
		res.Stderr = redact.Secrets(res.Stderr)
		res.Damage = redactDamage(res.Damage, known)
	}
	// A CRASH IS A SIGNAL NOBODY HERE SENT. The pool's own kill (w.ended)
	// and a caller's failed input or output (git's SIGPIPE) say nothing about
	// what the process read.
	res.Crashed = w.ended == endNone && (in == nil || in.callerErr == nil) && out.callerErr == nil &&
		(diedOfSignal(cmd.ProcessState) || res.Trace.Crashes > 0)
	if p.metrics != nil {
		p.metrics.Observe(metrics.CodeGitCPU, res.CPU, metrics.Attrs{"class": string(c.Class)})
	}
	if res.Trace.Unread > 0 {
		// A request can cause it (an echoed pkt-line past MaxLine), so never
		// a warning; no damage event comes near that length.
		p.log.Debug("code_git_trace_unread", "class", c.Class, "command", c.Args[0], "lines", res.Trace.Unread)
	}

	// THE ANSWER, in precedence order. A kill wins whatever the exit status:
	// the caller cannot know the output it received was whole.
	if w.ended != endNone {
		return res, p.killed(c, res, w)
	}
	if in != nil && in.callerErr != nil {
		return res, fmt.Errorf("%w: git %s: %w", ErrInput, c.Args[0], in.callerErr)
	}
	if out.callerErr != nil {
		return res, fmt.Errorf("%w: git %s: %w", ErrOutput, c.Args[0], out.callerErr)
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return res, fmt.Errorf("gitproc: waiting for git %s: %w", c.Args[0], waitErr)
	}
	if res.Exit == 0 || slices.Contains(c.Answers, res.Exit) {
		return res, nil
	}
	return res, &ExitError{Command: c.Args[0], Result: res, Kind: trace.kind()}
}

// diedOfSignal reports whether the reaped leader died of a signal other than
// SIGPIPE, git's own rule (run-command's wait_or_whine): a SIGPIPE is its
// reader hanging up, the consumer's doing, never the repository's.
func diedOfSignal(s *os.ProcessState) bool {
	if s == nil {
		return false
	}
	ws, ok := s.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() != syscall.SIGPIPE
}

// cpuOf is the user and system time wait4 reported at the reap, which the
// kernel sums over the process and every descendant it waited for: the whole
// tree's for a run that ended by itself (git waits for every child it
// starts), nothing of a member nobody reaped
// ([Pool.counted]). Never getrusage's RUSAGE_CHILDREN, which sums every git
// this process reaped and cannot say which run spent what. Zero for a
// process whose wait reported no usage.
func cpuOf(s *os.ProcessState) time.Duration {
	if s == nil {
		return 0
	}
	return s.UserTime() + s.SystemTime()
}

// counted is send preceded by a reading of the CPU the group's members have
// spent so far, kept in *spent if it is the most a reading found.
//
// A RUN THE NODE ENDS IS COUNTED BEFORE IT IS SIGNALLED: the group dies as
// one and its leader reaps none of the rest, so the reap's figure is the
// leader alone (TestARunTheNodeEndsCountsWhatItsGroupSpent; package doc,
// "What every process cost is counted when it ends"). The run takes the
// GREATER of this reading and the reap's, never the sum: a leader may still
// reap a child after the signal, putting its time in both. A sweep's
// straggler is outside the reap's figure, so it is added instead (see run).
// Off Linux the reap's figure stands; a record that fails to read is logged,
// and what was read is kept as a floor.
func (p *Pool) counted(c Cmd, spent *time.Duration, send func(int) error) func(int) error {
	return func(pid int) error {
		cpu, err := procgroup.GroupCPU(pid)
		if err != nil && !errors.Is(err, errors.ErrUnsupported) {
			p.log.Warn("code_git_cpu_unread", "class", c.Class, "command", c.Args[0], "pid", pid, "error", err)
		}
		*spent = max(*spent, cpu)
		return send(pid)
	}
}

// killed is the answer for a run the watcher ended, logged where it is
// decided: a deadline or a stall is the pool's own judgement and worth a
// warning; a caller that went away is ordinary.
func (p *Pool) killed(c Cmd, res Result, w *watcher) error {
	switch w.ended {
	case endDeadline:
		p.log.Warn("code_git_killed", "class", c.Class, "command", c.Args[0], "reason", "deadline",
			"after", res.Took, "deadline", c.Deadline)
		return fmt.Errorf("gitproc: git %s ran past its %v deadline and was killed: %w", c.Args[0], c.Deadline, ErrDeadline)
	case endStall:
		p.log.Warn("code_git_killed", "class", c.Class, "command", c.Args[0], "reason", "stall",
			"after", res.Took, "stall", c.Stall)
		return fmt.Errorf("gitproc: git %s read and wrote nothing for %v and was killed: %w", c.Args[0], c.Stall, ErrStalled)
	default:
		p.log.Debug("code_git_killed", "class", c.Class, "command", c.Args[0], "reason", "caller",
			"after", res.Took, "error", w.cause)
		return fmt.Errorf("gitproc: git %s was ended by its caller after %v: %w", c.Args[0], res.Took, w.cause)
	}
}

// end is why the watcher ended a run.
type end int

const (
	endNone end = iota
	endCaller
	endDeadline
	endStall
)

// watcher ends a run: on its caller's context, its deadline or a stall.
type watcher struct {
	reaped chan struct{} // closed by Run once the leader is reaped
	done   chan struct{} // closed by the watcher as it returns
	ended  end           // read after done
	cause  error         // the caller's, when ended is endCaller
	// spent is the most CPU the group's members had spent at a signal the
	// watcher sent them (Pool.counted); read after done.
	spent time.Duration
}

// watch starts the run's one watcher goroutine. Run closes reaped once the
// leader is reaped and waits for done, so no signal is sent after the reap
// except the sweep's — which checks the group's identity itself.
//
// It is also where a loan ends early: the call site's Cmd.Indexed closing
// repays it while the process runs on (lent nil: nothing to repay).
func (p *Pool) watch(ctx context.Context, c Cmd, leader procgroup.Leader, proc *os.Process, moved func() int64, lent *loan) *watcher {
	w := &watcher{reaped: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		deadline := time.NewTimer(c.Deadline)
		defer deadline.Stop()
		var tick <-chan time.Time
		if c.Stall > 0 {
			ticker := time.NewTicker(max(c.Stall/4, time.Millisecond))
			defer ticker.Stop()
			tick = ticker.C
		}
		var indexed <-chan struct{}
		if lent != nil {
			indexed = c.Indexed
		}
		last, lastMove := moved(), time.Now()
		for w.ended == endNone {
			select {
			case <-w.reaped:
				return
			case <-indexed:
				lent.repay()
				indexed = nil // a closed channel would answer every select
			case <-ctx.Done():
				w.ended, w.cause = endCaller, context.Cause(ctx)
			case <-deadline.C:
				w.ended = endDeadline
			case now := <-tick:
				if cur := moved(); cur != last {
					last, lastMove = cur, now
				} else if now.Sub(lastMove) >= c.Stall {
					w.ended = endStall
				}
			}
		}
		p.kill(c, leader, proc, w.reaped, &w.spent)
	}()
	return w
}

// kill ends the group: SIGTERM, then SIGKILL KillGrace later unless the leader
// was reaped first — and the leader's own handle after it, so a leader that
// somehow left its group still ends. Before each signal the group's CPU is
// read into *spent (Pool.counted): the second reading counts what a member
// that ignored the first spent through the grace.
func (p *Pool) kill(c Cmd, leader procgroup.Leader, proc *os.Process, reaped <-chan struct{}, spent *time.Duration) {
	p.signal(c, leader, "TERM", p.counted(c, spent, procgroup.Terminate))
	grace, stop := p.grace(p.killGrace)
	defer stop()
	select {
	case <-reaped:
		return
	case <-grace:
	}
	p.signal(c, leader, "KILL", p.counted(c, spent, procgroup.Kill))
	// The handle, not the pid: os.Process knows when it was waited on, so
	// this can never reach a recycled pid.
	_ = proc.Kill()
}

// timer is the wait a pool arms in production (Pool.grace): a time.Timer's
// channel, and its Stop.
func timer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// signal sends to the group only once procgroup has confirmed it is still the
// group this run started: an unreadable kernel record grants no permission
// to signal (procgroup's rule), so it is withheld and logged. It reports
// whether the group was still there.
//
// Current and the kill are two system calls, and the reap may land between
// them. The kernel reissues a pid only after cycling through pid_max
// (4 194 304 on 64-bit Linux, in order), so no stranger leads a group under
// it in that window, and the sweep, after the reap, tells a reused pid by its
// start time. A pidfd names one process, never a GROUP, so nothing closes it
// further.
func (p *Pool) signal(c Cmd, leader procgroup.Leader, name string, send func(int) error) bool {
	live, err := leader.Current()
	if err != nil {
		p.log.Warn("code_git_signal_withheld", "command", c.Args[0], "pid", leader.PID, "signal", name, "error", err)
		return false
	}
	if !live {
		return false
	}
	if err := send(leader.PID); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		p.log.Warn("code_git_signal_failed", "command", c.Args[0], "pid", leader.PID, "signal", name, "error", err)
	}
	return true
}

// track counts running processes per class into the gauge, under one mutex
// so a later Set never carries an older count.
func (p *Pool) track(c Class, delta int64) {
	if p.metrics == nil {
		return
	}
	p.gaugeMu.Lock()
	defer p.gaugeMu.Unlock()
	p.running[c] += delta
	p.metrics.Set(metrics.CodeGitProcesses, float64(p.running[c]), metrics.Attrs{"class": string(c)})
}

// THE DRAIN. A copier ends at END OF FILE (every byte git wrote delivered,
// however slowly the caller takes it) or at the caller's own error; stdin's
// at the caller's end of input or when git stops reading. After the sweep
// only a descendant that ESCAPED the group (its own setsid) can still hold a
// pipe's write end, perhaps for ever.
//
// So a copier blocked ON THE PIPE for KillGrace has its end closed and
// code_git_pipe_held logged: lossless, since a pipe read blocks only on an
// EMPTY pipe. A copier inside the CALLER's Read or Write is never cut: its
// bound is the caller's (Cmd's contract), and cutting it is os/exec
// WaitDelay's defect (package doc, "Every process ends, and so does
// everything it started").
func (p *Pool) drain(c Cmd, copiers []*copier) {
	tick := time.NewTicker(max(p.killGrace/4, time.Millisecond))
	defer tick.Stop()
	for _, cp := range copiers {
		if p.draining != nil {
			p.draining(cp.stream)
		}
		for waiting := true; waiting; {
			select {
			case <-cp.done:
				waiting = false
			case now := <-tick.C:
				for _, held := range copiers {
					if held.heldFor(now) >= p.killGrace && held.release() {
						p.log.Warn("code_git_pipe_held", "class", c.Class, "command", c.Args[0], "stream", held.stream)
					}
				}
			}
		}
	}
}

// A copier's state, for the drain watch.
const (
	inCaller int32 = iota // inside the caller's Read or Write
	inPipe                // blocked on the pipe
	finished
)

// copyBuffers keeps the copiers' 64 KiB buffers, so a node serving thousands
// of runs an hour allocates them once.
var copyBuffers = sync.Pool{New: func() any {
	buf := make([]byte, CopyBuffer)
	return &buf
}}

// copier moves one stream between a pipe and the caller.
type copier struct {
	stream string
	pipe   *os.File
	n      atomic.Int64 // bytes moved, counted when the CALLER's call returns
	state  atomic.Int32
	since  atomic.Int64 // when it last blocked on the pipe, in unix nanoseconds

	callerErr error // the caller's own failure; read after done
	done      chan struct{}
	closing   sync.Once
}

// startCopier runs body on a goroutine of its own, closing the pipe end and
// done when it returns.
func startCopier(stream string, pipe *os.File, body func(*copier)) *copier {
	cp := &copier{stream: stream, pipe: pipe, done: make(chan struct{})}
	go func() {
		defer close(cp.done)
		defer cp.closePipe()
		defer cp.state.Store(finished)
		body(cp)
	}()
	return cp
}

func (cp *copier) closePipe() { cp.closing.Do(func() { _ = cp.pipe.Close() }) }

func (cp *copier) blocked() {
	cp.since.Store(time.Now().UnixNano())
	cp.state.Store(inPipe)
}

// heldFor is how long the copier has been blocked on its pipe as of now, or 0
// when it is not.
func (cp *copier) heldFor(now time.Time) time.Duration {
	if cp.state.Load() != inPipe {
		return 0
	}
	return now.Sub(time.Unix(0, cp.since.Load()))
}

// release closes the pipe under a copier the drain watch found blocked on it,
// reporting whether it did (once).
func (cp *copier) release() bool {
	released := false
	cp.closing.Do(func() {
		_ = cp.pipe.Close()
		released = true
	})
	return released
}

// pump copies git's side of a pipe into dst: stdout into the caller, stderr
// into the sink. A failed Write closes the pipe (the deferred close), so git's
// next write fails and it exits rather than blocking on a full pipe until the
// watchdog notices.
func (cp *copier) pump(dst io.Writer) {
	bufp, _ := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bufp)
	buf := *bufp
	for {
		cp.blocked()
		n, rerr := cp.pipe.Read(buf)
		if n > 0 {
			cp.state.Store(inCaller)
			m, werr := dst.Write(buf[:n])
			cp.n.Add(int64(m))
			if werr == nil && m != n {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				cp.callerErr = werr
				return
			}
		}
		if rerr != nil {
			// End of file, or the drain watch closed an empty pipe:
			// either way nothing more is coming.
			return
		}
	}
}

// feed copies the caller's Stdin into git, closing the pipe at its end so git
// sees end of input.
//
// THE END IS io.EOF ITSELF, compared by equality (objstore.Fill's rule): a
// body whose client went away answers io.ErrUnexpectedEOF, and a wrapped
// io.EOF says more than io.EOF does; either is the caller's failure, never
// the end of git's input. A reader answering nothing maxEmptyReads times
// running is stuck (io.ErrNoProgress) rather than spun on.
func (cp *copier) feed(src io.Reader) {
	bufp, _ := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bufp)
	buf := *bufp
	for empty := 0; ; {
		cp.state.Store(inCaller)
		n, rerr := src.Read(buf)
		cp.n.Add(int64(n))
		if n > 0 {
			empty = 0
			cp.blocked()
			if _, werr := cp.pipe.Write(buf[:n]); werr != nil {
				// git stopped reading: it has exited or is exiting, and
				// its exit status is what says why.
				return
			}
		}
		switch {
		case rerr == io.EOF:
			return
		case rerr != nil:
			cp.callerErr = rerr
			return
		case n == 0:
			if empty++; empty >= maxEmptyReads {
				cp.callerErr = io.ErrNoProgress
				return
			}
		}
	}
}

// maxEmptyReads is how many reads in a row may answer nothing and no error
// before a Stdin is called stuck: a hundred, bufio's figure (and
// objstore.Fill's) for the same judgement.
const maxEmptyReads = 100

// capture is the Stdout of a run whose caller gave none: up to max bytes,
// and a write that would cross it is refused WHOLE, so no capture is ever
// returned cut short as if it were complete.
type capture struct {
	max int
	buf []byte
}

func (c *capture) Write(p []byte) (int, error) {
	if len(c.buf)+len(p) > c.max {
		return 0, fmt.Errorf("%w: git wrote more than %d bytes into a capture — stream it through Cmd.Stdout", ErrCaptureFull, c.max)
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// closeAll closes every non-nil file.
func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
