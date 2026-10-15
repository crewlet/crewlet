package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// PROC-RECEIVE, THE ENGINE'S SIDE: what a push's hook is served once it has
// named its session — git's ref updates read off the hook, handed to the one
// landing every push takes (land.go), and git told what landed. Everything
// here is the RELAY's — the protocol, the keepalive, the report — and what a
// push IS once receive-pack has written its pack is the landing's.

// session is one push's hook session: the relay's own state, and what the
// receive handler hands the landing.
type session struct {
	token string
	// ctx is the request's work context, cancel ends it — what a receive-pack
	// that ended, and a stopping host, end the session's work with.
	ctx    context.Context
	cancel context.CancelFunc

	// synced is the incarnation the push's sync pinned — its Repo the
	// linearizable row the push is judged on — and scratch the repository
	// its receive-pack runs in, leaning on it.
	synced     cache.Synced
	scratch    *cache.Scratch
	pushID     string
	principal  code.Principal
	receivedAt time.Time
	// preRefused is what the receive handler refused before receive-pack
	// ran, by ref.
	preRefused map[string]answered
	// judged is every command the handler asked synced.Repo's protections
	// about — with Forced unknown, so false — as it asked
	// (protectAfterForce).
	judged map[judgedKey]bool

	// indexed is the receive-pack's Cmd.Indexed: closed when the hook first
	// reaches the relay, which receive-pack starts only once its index-pack
	// has exited — so the cores it was lent are repaid before the upload and
	// the record wait (gitproc.MaxLentCores).
	indexed     chan struct{}
	indexedOnce sync.Once

	// claimed is set, under the relay's lock, when a hook connection named
	// the session's token; done closes when that connection's session ends.
	claimed bool
	done    chan struct{}

	mu sync.Mutex
	// current is the step the keepalive names.
	current string
	// result is what the push came to, written before done closes.
	result pushResult
}

// pushResult is one push's outcome as the session concluded it.
type pushResult struct {
	outcome   types.AuditOutcome
	refusal   AuditRefusal
	position  statelog.Position
	commands  int
	ok        int
	packBytes int64
}

func newSession(ctx context.Context, cancel context.CancelFunc) *session {
	return &session{ctx: ctx, cancel: cancel, indexed: make(chan struct{}), done: make(chan struct{}),
		current: "checking the push"}
}

// reachedRelay closes indexed, once.
func (sess *session) reachedRelay() { sess.indexedOnce.Do(func() { close(sess.indexed) }) }

// step names the session's current step, for the keepalive.
func (sess *session) step(name string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.current = name
}

// fail records a session that ended without an outcome git could be told.
func (sess *session) fail(_ string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.result.outcome == "" {
		sess.result.outcome = types.AuditFailed
	}
}

// conclude records the push's outcome.
func (sess *session) conclude(r pushResult) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.result = r
}

// outcome is the push's result as concluded.
func (sess *session) outcome() pushResult {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.result
}

// errProtocol is git's proc-receive protocol broken: what the hook relayed is
// not what git sends.
var errProtocol = errors.New("host: git handed the hook a command it cannot read")

// serveSession is one push's session, on its hook's connection: git's version
// block and commands read, the handler's own refusals folded into them, the
// push landed (land) and git told what landed. Every step's failure ends it
// with a sentence on the hook's stderr and exit 1 — git then fails every ref
// of the push — and the push recorded failed.
func (s *Server) serveSession(sess *session, br *bufio.Reader, fw *frameWriter) {
	sess.reachedRelay()
	ctx := sess.ctx
	fail := func(words string, err error) {
		s.log.Warn("code_hook_protocol", "repo", sess.synced.Repo.Name, "push_id", sess.pushID, "step", words,
			"error", err)
		sess.fail(words)
		_ = fw.write(frameStderr, []byte("crewlet: "+sentence(words)+"\n"))
		_ = fw.write(frameExit, []byte{1})
	}
	if err := s.fault(ctx, codefault.HostHookAdmitted, 0); err != nil {
		fail("the engine stopped while admitting this push's hook", err)
		return
	}
	stop := s.keepalive(sess, fw)
	defer stop()

	atomic, err := s.versionBlock(br, fw)
	if err != nil {
		fail("proc-receive did not speak version 1, the one this engine speaks", err)
		return
	}
	cmds, err := readHookCommands(br)
	if err != nil {
		fail("git handed the hook a command it cannot read", err)
		return
	}
	for i, c := range cmds {
		if a, ok := sess.preRefused[c.Ref]; ok {
			cmds[i].Result, cmds[i].Reason, cmds[i].Detail = code.ResultNG, a.Reason, a.Detail
		}
	}
	ld, err := s.land(ctx, landing{
		Synced: sess.synced, Scratch: sess.scratch, Commands: cmds, Atomic: atomic, Principal: sess.principal,
		PushID: sess.pushID, ReceivedAt: sess.receivedAt, Judged: sess.judged, Progress: sess.step,
	})
	if err != nil {
		cause := err
		if le, ok := errors.AsType[*landingError](err); ok {
			cause = le.err
		}
		fail(err.Error(), cause)
		return
	}
	stop()
	sess.conclude(ld.Result)
	if err := s.fault(ctx, codefault.HostHookClose, 0); err != nil {
		// THE CONNECTION DROPS BEFORE THE REPORT: git fails the push with
		// the hook's death, whatever the record did.
		s.log.Warn("code_hook_dropped", "repo", sess.synced.Repo.Name, "push_id", sess.pushID, "error", err)
		return
	}
	var report bytes.Buffer
	for _, a := range answersFor(ld.Commands) {
		report.Write(pktString(a.line()))
	}
	report.WriteString(flushPkt)
	_ = fw.write(frameStdout, report.Bytes())
	if ld.Result.outcome == types.AuditApplied && !ld.Result.position.IsZero() {
		_ = fw.write(frameStderr, []byte(fmt.Sprintf("crewlet: %s at %s\n", sess.synced.Repo.Name,
			ld.Result.position)))
	}
	_ = fw.write(frameExit, []byte{0})
}

// versionBlock reads git's version block — pkt-lines to a flush, the first
// `version=1` with its NUL-separated features — answers `version=1` with no
// feature of the engine's own (so git sends no push options), and reports
// whether git is running the push --atomic, which the report must agree with.
func (s *Server) versionBlock(br *bufio.Reader, fw *frameWriter) (atomic bool, err error) {
	first := true
	for {
		p, err := readPkt(br)
		if err != nil {
			return false, err
		}
		if p.flush {
			break
		}
		if !first {
			continue
		}
		first = false
		version, features, _ := strings.Cut(strings.TrimSuffix(string(p.payload), "\n"), "\x00")
		if version != "version=1" {
			return false, fmt.Errorf("%w: proc-receive spoke %q", errProtocol, version)
		}
		for f := range strings.SplitSeq(features, "\x00") {
			for word := range strings.FieldsSeq(f) {
				atomic = atomic || word == "atomic"
			}
		}
	}
	if first {
		return false, fmt.Errorf("%w: no version line", errProtocol)
	}
	return atomic, fw.write(frameStdout, append(pktString("version=1\x00"), flushPkt...))
}

// readHookCommands reads git's `<old> <new> <ref>` lines to a flush. These are
// AUTHORITATIVE — what git received and accepted; commands git refused itself
// never reach the hook. A line that does not parse, or more than
// code.MaxCommandsPerPush of them, is a protocol violation.
func readHookCommands(br *bufio.Reader) ([]code.Command, error) {
	var cmds []code.Command
	for {
		p, err := readPkt(br)
		if err != nil {
			return nil, err
		}
		if p.flush {
			return cmds, nil
		}
		if p.delim || p.end {
			return nil, fmt.Errorf("%w: a special packet among the commands", errProtocol)
		}
		fields := strings.SplitN(strings.TrimSuffix(string(p.payload), "\n"), " ", 3)
		if len(fields) != 3 || !gitref.ValidOID(fields[0]) || !gitref.ValidOID(fields[1]) || fields[2] == "" {
			return nil, fmt.Errorf("%w: %q", errProtocol, p.payload)
		}
		if len(cmds) == code.MaxCommandsPerPush {
			return nil, fmt.Errorf("%w: more than %d commands", errProtocol, code.MaxCommandsPerPush)
		}
		cmds = append(cmds, code.Command{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
}

// keepalive writes the session's step on the hook's stderr every
// Timing.Keepalive after the first, until the stop it answers is called:
// without side-band git keeps no connection alive while the hook is silent, and
// the engine's line on receive-pack's stderr is then what feeds the stall
// watchdog; with side-band git keeps it alive itself, and the line is for the
// person watching. `\r`, so a terminal redraws one line.
func (s *Server) keepalive(sess *session, fw *frameWriter) (stop func()) {
	done := make(chan struct{})
	ended := make(chan struct{})
	start := s.now()
	go func() {
		defer close(ended)
		ticker := time.NewTicker(s.o.Timing.Keepalive)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sess.mu.Lock()
				step := sess.current
				sess.mu.Unlock()
				_ = fw.write(frameStderr, []byte(fmt.Sprintf("crewlet: %s (%ds)\r", step,
					int(s.now().Sub(start)/time.Second))))
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-ended
		})
	}
}

// short is an object id's first seven digits, as git prints one.
func short(oid string) string { return oid[:min(len(oid), 7)] }

// humanBytes is a size as a person reads it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
