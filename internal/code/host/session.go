package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// PROC-RECEIVE, THE ENGINE'S SIDE: what a push's hook is served once it has
// named its session — git's ref updates read, checked, the pack uploaded, the
// push proposed to its committer and git told what landed.

// session is one push's hook session.
type session struct {
	token string
	// ctx is the request's work context, cancel ends it — what a receive-pack
	// that ended, and a stopping host, end the session's work with.
	ctx    context.Context
	cancel context.CancelFunc

	// repo is the repository's linearizable row, read by the push's sync.
	repo       code.RepoRef
	synced     cache.Synced
	scratch    *cache.Scratch
	pushID     string
	principal  code.Principal
	receivedAt time.Time
	// preRefused is what the receive handler refused before receive-pack
	// ran, by ref.
	preRefused map[string]answered
	// rules are the repository's protections as the handler parsed them
	// from repo, and judged every command it asked them about — with
	// Forced unknown, so false — as it asked (protectAfterForce).
	rules  code.Protections
	judged map[judgedKey]bool
	// caps are the push's capabilities.
	caps capabilities

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

// serveSession is one push's session, on its hook's connection. Every step's
// failure ends it with a sentence on the hook's stderr and exit 1 — git then
// fails every ref of the push — and the push recorded failed.
func (s *Server) serveSession(sess *session, br *bufio.Reader, fw *frameWriter) {
	sess.reachedRelay()
	ctx := sess.ctx
	fail := func(words string, err error) {
		s.log.Warn("code_hook_protocol", "repo", sess.repo.Name, "push_id", sess.pushID, "step", words,
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
	v, err := s.decide(ctx, sess, cmds, atomic)
	if err != nil {
		fail("the engine could not check this push: "+err.Error(), err)
		return
	}
	if v.applied && v.adopt != nil {
		if err := s.fault(ctx, codefault.HostBeforeAdopt, 0); err != nil {
			fail("the engine stopped before it took this push's pack into its cache", err)
			return
		}
		if err := s.o.Cache.Adopt(ctx, sess.repo.ID, sess.scratch, *v.adopt); err != nil {
			s.log.Warn("code_pack_adopt_failed", "repo", sess.repo.Name, "push_id", sess.pushID, "error", err)
		}
	}
	stop()
	sess.conclude(v.result)
	if err := s.fault(ctx, codefault.HostHookClose, 0); err != nil {
		// THE CONNECTION DROPS BEFORE THE REPORT: git fails the push with
		// the hook's death, whatever the record did.
		s.log.Warn("code_hook_dropped", "repo", sess.repo.Name, "push_id", sess.pushID, "error", err)
		return
	}
	var report bytes.Buffer
	for _, a := range answersFor(v.commands) {
		report.Write(pktString(a.line()))
	}
	report.WriteString(flushPkt)
	_ = fw.write(frameStdout, report.Bytes())
	if v.result.outcome == types.AuditApplied && !v.result.position.IsZero() {
		_ = fw.write(frameStderr, []byte(fmt.Sprintf("crewlet: %s at %s\n", sess.repo.Name, v.result.position)))
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

// decision is what the session decided: the commands as git is told them, the
// record's result, and the pack to adopt once its row applied.
type decision struct {
	commands []code.Command
	result   pushResult
	applied  bool
	adopt    *code.PackRef
}

// decide is the receive pipeline's steps 8.1–11: the push's own checks, its
// bundle stored, its proposal and its outcome.
func (s *Server) decide(ctx context.Context, sess *session, cmds []code.Command, atomic bool) (decision, error) {
	limits := s.limits()
	refuse := func(i int, r code.Refusal, detail string) {
		cmds[i].Result, cmds[i].Reason, cmds[i].Detail = code.ResultNG, r, detail
	}
	for i, c := range cmds {
		if a, ok := sess.preRefused[c.Ref]; ok {
			refuse(i, a.Reason, a.Detail)
		}
	}

	// 8.1 FORCE AND STALE: one merge-base a branch update, with bounded
	// parallelism; then the stateless rules, and the protections of what
	// Forced can change (protectAfterForce).
	var todo []int
	for i, c := range cmds {
		if c.Result == "" && c.Old != code.ZeroOID && c.New != code.ZeroOID &&
			strings.HasPrefix(c.Ref, "refs/heads/") && code.StatelessRefusal(c) == "" {
			todo = append(todo, i)
		}
	}
	forced, stale, err := s.forceChecks(ctx, sess.scratch.Dir, cmds, todo)
	if err != nil {
		return decision{}, err
	}
	for i := range cmds {
		c := &cmds[i]
		if c.Result != "" {
			continue
		}
		if stale[i] {
			refuse(i, code.RefusalStale, code.UnheldDetail(c.Old))
			continue
		}
		c.Forced = forced[i]
		if r := code.StatelessRefusal(*c); r != "" {
			refuse(i, r, "")
		}
	}
	protectAfterForce(cmds, sess.rules, sess.judged)

	// 8.2 BLOBS, over the push's own pack.
	packPath, checksum, hasPack, err := sess.scratch.Pack()
	if err != nil {
		return decision{}, err
	}
	var ref *code.PackRef
	var news []string
	for _, c := range cmds {
		if c.Result == "" && c.New != code.ZeroOID {
			news = append(news, c.New)
		}
	}
	if hasPack {
		if ref, err = bundleRef(sess.pushID, packPath, checksum); err != nil {
			return decision{}, err
		}
		if s.o.Metrics != nil {
			s.o.Metrics.ObserveValue(metrics.CodePushBytes, float64(ref.Size), nil)
		}
		idx := strings.TrimSuffix(packPath, ".pack") + ".idx"
		oid, size, found, berr := s.largestBlob(ctx, sess.scratch.Dir, idx, ref.PackBytes(), limits.MaxBlobBytes)
		if berr != nil {
			return decision{}, berr
		}
		if found {
			detail := fmt.Sprintf("blob %s (%s) is %s, past code.limits.max_blob_bytes (%s)", short(oid),
				s.pathOf(ctx, sess.scratch.Dir, news, oid, ref.PackBytes()), humanBytes(size),
				humanBytes(limits.MaxBlobBytes))
			for i := range cmds {
				refuse(i, code.RefusalBlobTooLarge, detail)
			}
		}
	}

	// 7 ATOMIC: an --atomic push refused in part is refused whole, before
	// anything is uploaded.
	if atomic {
		cmds = code.Atomic(cmds)
	}
	result := pushResult{commands: len(cmds)}
	if ref != nil {
		result.packBytes = ref.Size
	}

	// 8.4 NOTHING LEFT: every command refused, so nothing is uploaded and
	// nothing proposed.
	if !undecided(cmds) {
		v := settled(cmds, statelog.Position{}, false, false)
		result.outcome, result.refusal = v.outcome, v.refusal
		return decision{commands: cmds, result: result}, nil
	}
	if ref != nil && ref.Size > code.MaxPackBytes {
		detail := fmt.Sprintf("the push stores %s, past the %s one push may store; push fewer commits at "+
			"once, or without thin packs (git push --no-thin)", humanBytes(ref.Size), humanBytes(code.MaxPackBytes))
		cmds = refuseRest(cmds, code.RefusalPackTooLarge, detail)
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(code.RefusalPackTooLarge)
		return decision{commands: cmds, result: result}, nil
	}

	// 8.3 AND 8.5 TOGETHER: the commits every update still standing
	// introduces, and the bundle stored before anything names it.
	news = news[:0]
	for _, c := range cmds {
		if c.Result == "" && c.New != code.ZeroOID {
			news = append(news, c.New)
		}
	}
	introduced, count, stored, err := s.introduceAndStore(ctx, sess, news, packPath, ref)
	switch {
	case err != nil:
		return decision{}, err
	case stored != nil:
		var bad *badBundle
		detail := "object store unavailable: " + sentence(stored.Error())
		if errors.As(stored, &bad) {
			detail = "this node stored a pack the code host cannot serve: " + sentence(bad.err.Error())
		}
		cmds = refuseRest(cmds, code.RefusalUnavailable, detail)
		result.outcome = types.AuditFailed
		return decision{commands: cmds, result: result}, nil
	}
	if ref != nil {
		if err := s.fault(ctx, codefault.HostAfterUpload, 0); err != nil {
			return decision{}, err
		}
	}

	// 10 PROPOSE, to the repository's committer, under the request's
	// context: cancellation and no deadline of its own — each data node gets
	// the router's write attempt.
	sess.step("committing the push")
	proposed := cmds
	res, perr := s.o.Estate.Propose(ctx, code.Proposal{
		PushID: sess.pushID, RepoID: sess.repo.ID, Principal: sess.principal, Node: s.o.NodeID,
		Atomic: atomic, Commands: proposed, Pack: ref, Introduced: introduced, IntroducedCount: count,
		ReceivedAt: sess.receivedAt, Seen: sess.synced.Position,
	})
	if err := s.fault(ctx, codefault.HostAfterPropose, 0); err != nil {
		return decision{}, err
	}
	v := s.decided(ctx, sess, proposed, res, perr)
	result.outcome, result.refusal, result.position = v.outcome, v.refusal, v.position
	for _, c := range v.commands {
		if c.Result == code.ResultOK {
			result.ok++
		}
	}
	d := decision{commands: v.commands, result: result, applied: v.applied}
	if v.applied && ref != nil && result.ok > 0 {
		d.adopt = ref
	}
	return d, nil
}

// judgedKey is one command as the receive handler judged its protection.
type judgedKey struct{ ref, old, new string }

// protectAfterForce asks the protections again about the commands whose
// verdict the receive handler's could not settle, refusing them in place, and
// reports how many it asked: a FORCED update — protected_force turns on
// Forced, which only the hook's check of the pushed objects knows — and a
// command the handler did not judge as it stands. Every other command was
// asked the same rules, about the same ids, by the handler, and refused there
// if any rule refuses it; asked again, a push walked every rule once more for
// every command for answers already known — at the costliest rules a
// repository may carry, 1.8 s for three thousand commands (internal/code's
// measured section), about 0.6 s of a maximal push's own.
func protectAfterForce(cmds []code.Command, rules code.Protections, judged map[judgedKey]bool) (asked int) {
	for i := range cmds {
		c := &cmds[i]
		if c.Result != "" || (!c.Forced && judged[judgedKey{c.Ref, c.Old, c.New}]) {
			continue
		}
		asked++
		if r, rule := code.ProtectionRefusal(*c, rules); r != "" {
			c.Result, c.Reason, c.Detail = code.ResultNG, r, code.ProtectionDetail(rule, r)
		}
	}
	return asked
}

// introduceAndStore lists the commits the push's updates still standing
// introduce (news) and stores its bundle (ref, nil for a push with no pack) AT
// ONCE: the listing cannot refuse anything — it only fills the proposal's
// Introduced — so nothing waits for it before the upload, and the two longest
// steps of a push's own work (a rev-list that reads every ref tip of the
// repository, and the object store's first byte and transfer) overlap rather
// than add. Either failing ends the other, whose work is then for nothing: err
// is the listing's failure, which fails the session; stored the upload's,
// which refuses the push unavailable. A bundle stored under a listing that
// then failed is named by no row, and the collector removes it after its
// grace, as it does a refused push's.
func (s *Server) introduceAndStore(ctx context.Context, sess *session, news []string, packPath string,
	ref *code.PackRef) (introduced []string, count int, stored, err error) {

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var listing sync.WaitGroup
	if len(news) > 0 && ref != nil {
		// Read before the upload starts: the upload writes ref's object
		// into it, and PackBytes reads the row whole.
		packBytes := ref.PackBytes()
		listing.Go(func() {
			if introduced, count, err = s.introduced(ctx, sess.scratch.Dir, news, packBytes); err != nil {
				cancel(errListingFailed)
			}
		})
	}
	if ref != nil {
		sess.step("storing the pack")
		if stored = s.storeBundle(ctx, sess, packPath, ref); stored != nil {
			cancel(errUploadFailed)
		}
	}
	listing.Wait()
	switch cause := context.Cause(ctx); {
	case err != nil && errors.Is(cause, errUploadFailed):
		// Ended because the upload failed: the upload's failure is the
		// push's.
		err = nil
	case stored != nil && errors.Is(cause, errListingFailed):
		// Ended because the listing failed: the listing's failure is.
		stored = nil
	}
	return introduced, count, stored, err
}

// The two causes introduceAndStore ends one step's work with.
var (
	errListingFailed = errors.New("host: listing the commits the push introduces failed")
	errUploadFailed  = errors.New("host: storing the push's bundle failed")
)

// undecided reports whether any command is still to be decided.
func undecided(cmds []code.Command) bool {
	for _, c := range cmds {
		if c.Result == "" {
			return true
		}
	}
	return false
}

// bundleRef is the push's ONE stored object, laid out from its scratch's
// three files: the pack, its .idx (version 2) and its .rev (version 1),
// back to back. A missing .rev fails the session: the receive configuration
// pins it.
func bundleRef(pushID, packPath, checksum string) (*code.PackRef, error) {
	base := strings.TrimSuffix(packPath, ".pack")
	sizes := make([]int64, 3)
	for i, ext := range []string{".pack", ".idx", ".rev"} {
		info, err := os.Stat(base + ext)
		if err != nil {
			return nil, fmt.Errorf("host: the push's %s: %w", ext, err)
		}
		sizes[i] = info.Size()
	}
	count, ids, err := idxOIDs(base + ".idx")
	if err != nil {
		return nil, err
	}
	_ = ids.Close()
	return &code.PackRef{PackID: pushID, Size: sizes[0] + sizes[1] + sizes[2], IndexBytes: sizes[1],
		ReverseBytes: sizes[2], GitChecksum: checksum, Objects: count}, nil
}

// badBundle is a stored bundle its own seal refuses.
type badBundle struct{ err error }

func (b *badBundle) Error() string { return b.err.Error() }

// storeBundle uploads the push's bundle — the three files back to back,
// exactly ref.Size bytes, limited at that size — sealing it on the way, and
// holds the seal to ref (code.CheckBundle), the one rule every installing
// cache holds it to. On success ref names the stored object. Nothing is read
// twice: the seal is gathered from the bytes on their way to the store.
func (s *Server) storeBundle(ctx context.Context, sess *session, packPath string, ref *code.PackRef) error {
	base := strings.TrimSuffix(packPath, ".pack")
	var files []io.Reader
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		f, err := os.Open(base + ext)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		files = append(files, f)
	}
	ctx, cancel := context.WithTimeout(ctx, uploadDeadline(ref.Size))
	defer cancel()
	sw := code.NewSealWriter(*ref)
	in := &faultReader{ctx: ctx, s: s, r: io.MultiReader(files...)}
	obj, err := s.o.Objects.Put(ctx, io.TeeReader(in, sw), ref.Size,
		objstore.PutMeta{ContentType: code.BundleContentType})
	if err != nil {
		return err
	}
	ref.Object, ref.Hash = obj.Key, obj.Hash
	seal, err := sw.Seal()
	if err == nil {
		err = code.CheckBundle(*ref, seal)
	}
	if err != nil {
		s.log.Error("code_pack_rejected", "repo", sess.repo.Name, "push_id", sess.pushID,
			"object", obj.Key.String(), "error", err)
		return &badBundle{err: err}
	}
	return nil
}

// faultReader hits HostUpload before each read the object store makes of the
// bundle, at the offset of the bytes handed over so far.
type faultReader struct {
	ctx context.Context
	s   *Server
	r   io.Reader
	at  int64
}

func (f *faultReader) Read(p []byte) (int, error) {
	if err := f.s.fault(f.ctx, codefault.HostUpload, f.at); err != nil {
		return 0, err
	}
	n, err := f.r.Read(p)
	f.at += int64(n)
	return n, err
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
