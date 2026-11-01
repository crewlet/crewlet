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
// named its session — git's ref updates and the push's options read off the
// hook, handed to the one landing every push takes (land.go), and git told
// what landed, and what became of the change the options asked for
// (change.go). Everything here is the RELAY's — the protocol, the keepalive,
// the report — and what a push IS once receive-pack has written its pack is
// the landing's.

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
	// adm is the push's admission: its grant, judged again on a command the
	// landing finds forced (landing.ForcedGrant), and — for a coding box —
	// the run whose floor an applied push raises before `ok` is written
	// (raiseFloor).
	adm admission
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
//
// Its log line says WHOSE step failed, since the two send an operator to
// different places: code_hook_protocol is the RELAY's — git's version block or
// commands not what git sends, before anything is stored — and
// code_push_unchecked the push's own work, a landing that could not finish (a
// git process over the push's objects failing or past its deadline on this
// node, its pack unreadable), after which a bundle may already be stored and
// named by no row, which the object collector removes after its grace.
func (s *Server) serveSession(sess *session, br *bufio.Reader, fw *frameWriter) {
	sess.reachedRelay()
	ctx := sess.ctx
	end := func(words string) {
		sess.fail(words)
		_ = fw.write(frameStderr, []byte("crewlet: "+sentence(words)+"\n"))
		_ = fw.write(frameExit, []byte{1})
	}
	protocol := func(words string, err error) {
		s.log.Warn("code_hook_protocol", "repo", sess.synced.Repo.Name, "push_id", sess.pushID, "step", words,
			"error", err)
		end(words)
	}
	// unchecked is the push's own work ended with no outcome: at DEBUG when
	// the push's request ended under it — its client gone, the host stopping,
	// receive-pack ended — which the handler logs for what it was
	// (code_git_abandoned, code_push_failed), so a client that hangs up mid
	// check is no WARN.
	unchecked := func(words string, err error) {
		attrs := []any{"repo", sess.synced.Repo.Name, "push_id", sess.pushID, "words", words, "error", err}
		if sess.ctx.Err() != nil {
			s.log.Debug("code_push_unchecked", attrs...)
		} else {
			s.log.Warn("code_push_unchecked", attrs...)
		}
		end(words)
	}
	if err := s.fault(ctx, codefault.HostHookAdmitted, 0); err != nil {
		unchecked("the engine stopped while admitting this push's hook", err)
		return
	}
	stop := s.keepalive(sess, fw)
	defer stop()

	features, err := s.versionBlock(br, fw)
	if err != nil {
		protocol("proc-receive did not speak version 1, the one this engine speaks", err)
		return
	}
	cmds, err := readHookCommands(br)
	if err != nil {
		protocol("git handed the hook a command it cannot read", err)
		return
	}
	// THE PUSH'S OPTIONS, which git sends after the commands only because
	// the relay answered its version block that it reads them.
	var options []string
	if features.pushOptions {
		if options, err = readHookPushOptions(br); err != nil {
			protocol("git handed the hook a push option it cannot read", err)
			return
		}
	}
	for i, c := range cmds {
		if a, ok := sess.preRefused[c.Ref]; ok {
			cmds[i].Result, cmds[i].Reason, cmds[i].Detail = code.ResultNG, a.Reason, a.Detail
		}
	}
	// A CODING BOX COMMITS NOTHING AS THE ENGINE that the repository does not
	// already hold: the address is read off the epoch now, as the engine's
	// own writes are signed, so a revision that moves the identity domain
	// holds a box to the new one from its activation.
	committer, err := sess.adm.engineCommitter(s.o.Signer)
	if err != nil {
		unchecked("the engine could not name the committer its own commits carry, which a coding box's "+
			"push is checked against", err)
		return
	}
	ld, err := s.land(ctx, landing{
		Synced: sess.synced, Scratch: sess.scratch, Commands: cmds, Atomic: features.atomic,
		PushOptions: options, Principal: sess.principal, Via: code.ViaGit, PushID: sess.pushID,
		ReceivedAt: sess.receivedAt, Judged: sess.judged, ForcedGrant: sess.adm.grant.RefRule,
		EngineCommitter: committer, Progress: sess.step,
	})
	if err != nil {
		cause := err
		if le, ok := errors.AsType[*landingError](err); ok {
			cause = le.err
		}
		unchecked(err.Error(), cause)
		return
	}
	stop()
	sess.conclude(ld.Result)
	// A CODING BOX'S APPLIED PUSH RAISES ITS RUN'S FLOOR to the push's row
	// before `ok` is written, so whatever node the box reads next holds what
	// it pushed. A raise that fails answers `ok` all the same: the push is
	// durable, and what the box may then read behind is its own push, whose
	// objects it holds.
	if ld.Result.outcome == types.AuditApplied {
		if rerr := s.raiseFloor(ctx, sess.adm, ld.Result.position); rerr != nil {
			s.log.Warn("code_box_floor_unraised", "repo", sess.synced.Repo.Name, "push_id", sess.pushID,
				"position", ld.Result.position.String(), "error", rerr)
		}
	}
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
	if line := changeLine(ld.Change, ld.Result, sess.synced.Repo.Name, s.o.ExternalBase); line != "" {
		_ = fw.write(frameStderr, []byte("crewlet: "+line+"\n"))
	}
	_ = fw.write(frameExit, []byte{0})
}

// procFeatures are the features of git's proc-receive version block the relay
// answers: --atomic, which the report must agree with, and push-options —
// offered only for a push that sent options — which the relay answers it
// reads, so git sends them after the commands, and only then.
type procFeatures struct{ atomic, pushOptions bool }

// versionBlock reads git's version block — pkt-lines to a flush, the first
// `version=1` with its NUL-separated features — and answers `version=1` with
// push-options where git offered them (so git hands the hook the push's
// options after its commands, readHookPushOptions) and no feature otherwise:
// the protocol's negotiation, in which the hook answers what it reads of what
// git offered. It reports both features, the options the ones the relay must
// read now that it said it would.
func (s *Server) versionBlock(br *bufio.Reader, fw *frameWriter) (procFeatures, error) {
	var f procFeatures
	first := true
	for {
		p, err := readPkt(br)
		if err != nil {
			return procFeatures{}, err
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
			return procFeatures{}, fmt.Errorf("%w: proc-receive spoke %q", errProtocol, version)
		}
		for list := range strings.SplitSeq(features, "\x00") {
			for word := range strings.FieldsSeq(list) {
				f.atomic = f.atomic || word == "atomic"
				f.pushOptions = f.pushOptions || word == "push-options"
			}
		}
	}
	if first {
		return procFeatures{}, fmt.Errorf("%w: no version line", errProtocol)
	}
	answer := "version=1\x00"
	if f.pushOptions {
		answer += "push-options"
	}
	return f, fw.write(frameStdout, append(pktString(answer), flushPkt...))
}

// readHookPushOptions reads the push options git hands the hook after the
// commands — one pkt-line each, to a flush, sent because the relay answered
// git's version block that it reads them (versionBlock). They are
// AUTHORITATIVE as the commands are: what receive-pack read from the client
// and accepted, which the client's own bytes beside it are not — a special
// packet among them is a protocol violation, and each option is its line, its
// one trailing line feed dropped as git's packet reader drops it. How many and
// how long the handler held before receive-pack read any (readCommands), so
// they are nothing a push could not already make the receiver hold.
func readHookPushOptions(br *bufio.Reader) ([]string, error) {
	var options []string
	for {
		p, err := readPkt(br)
		if err != nil {
			return nil, err
		}
		if p.flush {
			return options, nil
		}
		if p.delim || p.end {
			return nil, fmt.Errorf("%w: a special packet among the push options", errProtocol)
		}
		options = append(options, strings.TrimSuffix(string(p.payload), "\n"))
	}
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
