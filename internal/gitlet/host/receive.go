package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// POST /git/{repo}/git-receive-pack — A PUSH, the handler's half of the
// receive pipeline (steps 0–7, and 9 the answer): its command section read and
// what needs no object refused, the cache synced through the barrier (a coding
// box's at its run's session), the push's scratch made, and receive-pack run
// there, never in the cache, with the one hook relaying every ref update to
// the session (session.go), which hands them to the one landing (land.go,
// step 8) and reports what landed.

// receiveConfig is every push's receive-pack configuration, its advertisement's included:
//
//   - receive.procReceiveRefs=refs/: every command goes to the hook; git moves no ref;
//   - receive.unpackLimit=1: the push stays ONE pack, the unit a row names;
//   - receive.fsckObjects=true: a malformed object is refused at the door;
//   - receive.maxInputSize: the epoch's code.limits.max_push_bytes;
//   - receive.advertisePushOptions=true: `git push -o change` opens a change
//     (gitlet.ParseChangeOptions); receive-pack holds each in memory, handing
//     them to the hook after the commands (session.go): readCommands bounds them;
//   - receive.autogc=false, receive.updateServerInfo=false: a scratch is thrown away;
//   - pack.writeReverseIndex=true, pack.indexVersion=2: the .rev and v2 .idx the
//     push's BUNDLE carries, so no other process or node indexes the pack.
func receiveConfig(l gitlet.Limits) []gitproc.KV {
	return []gitproc.KV{
		{Key: "receive.procReceiveRefs", Value: "refs/"},
		{Key: "receive.unpackLimit", Value: "1"},
		{Key: "receive.fsckObjects", Value: "true"},
		{Key: "receive.maxInputSize", Value: strconv.FormatInt(l.MaxPushBytes, 10)},
		{Key: "receive.advertisePushOptions", Value: "true"},
		{Key: "receive.autogc", Value: "false"},
		{Key: "receive.updateServerInfo", Value: "false"},
		{Key: "pack.writeReverseIndex", Value: "true"},
		{Key: "pack.indexVersion", Value: "2"},
	}
}

// refusedNote is what a client asking for side-band sees beside a push the
// engine refused before receive-pack ran.
const refusedNote = "refused before anything was stored"

// pushCertificateSentence answers a push that carries a push certificate
// (errPushCertificate) — sent only by a client built by hand, since git's own
// stops `git push --signed` itself when the server advertises no push-cert.
const pushCertificateSentence = "this push carries a push certificate (a signed push), which this engine " +
	"never advertises and does not take: push without --signed"

func (s *Server) receivePack(w http.ResponseWriter, r *http.Request, admit admitter) {
	start := s.now()
	if r.Header.Get("Content-Type") != ReceivePackRequestType {
		auth.GitText(w, http.StatusUnsupportedMediaType,
			"a git-receive-pack request must be "+ReceivePackRequestType)
		return
	}
	ctx, end, ok := s.begin(r.Context(), w)
	if !ok {
		return
	}
	defer end()
	adm, ok := admit(ctx, w, r, auth.GitPush)
	if !ok {
		return
	}
	// THE PUSH'S ONE RECORD from here (pushAudit), settled however this
	// request ends: refused, concluded by its run, or — every return below
	// that records nothing: a body this node cannot decode, a fault, the
	// sync, a rule this build cannot read, the scratch, a panic — failed.
	audit := s.newPushAudit(ctx, adm, start)
	defer audit.settle()
	limits := s.limits()
	drainLimit := limits.MaxPushBytes + MaxCommandSectionBytes
	if r.ContentLength > drainLimit {
		// A DECLARED LENGTH THIS LARGE (git sends one only within its post
		// buffer): draining it to say so is the cost the bound refuses. Judged
		// after admission, so the push is recorded as whoever sent it and an
		// unknown client is challenged first, as for every refusal of a push.
		auth.GitText(w, http.StatusRequestEntityTooLarge, "this push is "+humanBytes(r.ContentLength)+
			", past code.limits.max_push_bytes ("+humanBytes(limits.MaxPushBytes)+")")
		audit.record(pushRecord{outcome: types.AuditRefused, refusal: RefusalPushTooLarge})
		return
	}
	in, ok := decodedBody(w, r, s)
	if !ok {
		return
	}
	body := bufio.NewReaderSize(in, 64<<10)

	// 2 THE COMMANDS, and every refusal that needs no object.
	sec, err := readCommands(body, MaxCommandSectionBytes, gitlet.MaxCommandsPerPush)
	refuseAll := func(reason AuditRefusal, answers []answered) {
		drain(body, drainLimit)
		writeRefusalReport(w, sec.Caps, answers, refusedNote)
		audit.record(pushRecord{outcome: types.AuditRefused, refusal: reason, commands: len(sec.Commands)})
	}
	switch {
	case errors.Is(err, errTooManyCommands), errors.Is(err, errSectionTooLarge), errors.Is(err, errOptionsTooLarge):
		words := "this push's command list is over 1 MiB"
		switch {
		case errors.Is(err, errTooManyCommands):
			words = "this push names more than " + strconv.Itoa(gitlet.MaxCommandsPerPush) +
				" refs; push them in parts (git push origin <some refs>)"
		case errors.Is(err, errOptionsTooLarge):
			// THE COMMANDS WERE READ WHOLE, so every one is answered: the
			// options came after them.
			words = "this push's options take its command list past 1 MiB; push with fewer or shorter " +
				"options (git push -o)"
		}
		if len(sec.Commands) == 0 {
			auth.GitText(w, http.StatusRequestEntityTooLarge, words)
			audit.record(pushRecord{outcome: types.AuditRefused, refusal: RefusalCommands})
			return
		}
		refuseAll(RefusalCommands, refuseWith(sec.Commands, func(parsedCommand) (gitlet.Refusal, string) {
			return "", words
		}))
		return
	case errors.Is(err, errPushCertificate):
		drain(body, drainLimit)
		auth.GitText(w, http.StatusBadRequest, pushCertificateSentence)
		audit.record(pushRecord{outcome: types.AuditRefused, refusal: RefusalCommands, commands: len(sec.Commands)})
		return
	case err != nil:
		// A COMMAND LIST THAT COULD NOT BE READ is still a recorded push:
		// refused for its commands when its bytes are not pkt-lines (the
		// client's to fix), failed when the body itself failed (cut short,
		// not gzip, too slow), as past the section.
		rec := pushRecord{outcome: types.AuditFailed, commands: len(sec.Commands)}
		if errors.Is(err, errMalformed) {
			rec.outcome, rec.refusal = types.AuditRefused, RefusalCommands
		}
		auth.GitText(w, http.StatusBadRequest, "the push's command list could not be read: "+sentence(err.Error()))
		audit.record(rec)
		return
	case sec.Empty():
		// GIT'S PROBE before a push past its post buffer: a 200 and nothing
		// run, so git sends the real request, which records the push.
		audit.none()
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		return
	}
	// A FAILURE FROM HERE ON is recorded with the commands the push names.
	audit.failed.commands = len(sec.Commands)
	// THE GRANT (ADR-0088), before the repository's status: a principal that
	// may push nothing here (a token bound to nobody) is refused whole, as its
	// advertisement was. Otherwise the stateless rules come first (a reserved
	// ref is reserved_ref to everybody), then the grant, judging an update as a
	// fast-forward: whether it is forced is the landing's verdict after the
	// sync, and changes nothing for a person (passes both) or a token bound to
	// nobody (passes neither). A coding box's seat holding write may not force;
	// its forced update is refused by the decide's grant rule (2b).
	grant := adm.grant
	if !grant.MayPush() {
		refuseAll(grantRefusal(adm.principal()), refuseWith(sec.Commands,
			func(c parsedCommand) (gitlet.Refusal, string) {
				return grant.RefRule(gitlet.Command{Ref: c.Ref, Old: c.Old, New: c.New})
			}))
		return
	}
	if s.refusedByStatus(adm.repo, sec, refuseAll) {
		return
	}
	pre := map[string]answered{}
	for _, c := range sec.Commands {
		cmd := gitlet.Command{Ref: c.Ref, Old: c.Old, New: c.New}
		if reason := gitlet.StatelessRefusal(cmd); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason}
		} else if reason, words := grant.RefRule(cmd); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason, Detail: words}
		}
	}
	if s.refusedEvery(sec, pre, refuseAll) {
		return
	}
	// WHAT ONE PUSH MAY CARRY, before any work is spent on it: the commands as
	// they will be proposed, with all the work may add at its largest
	// (gitlet.Proposal.ValidateCommands); otherwise the committer refuses it
	// only after receive-pack, the scans, the bundle's put and the round trip.
	if words := commandsRefusal(adm, s.o.NodeID, sec, pre); words != "" {
		refuseAll(RefusalCommands, refuseWith(sec.Commands, func(parsedCommand) (gitlet.Refusal, string) {
			return "", words
		}))
		return
	}
	if ferr := s.fault(ctx, gitletfault.HostAfterCommands, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
		return
	}

	// 3 THE SYNC, at the request's refs level (admission.refs): through the
	// barrier for a person, at its run's session for a coding box (a floor its
	// advertisement raised to what it showed). A cache at or past the
	// advertisement holds every base a thin pack deltas against, and its row,
	// never the stale resolve, is what status and protections are judged on.
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, adm.refs())
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	defer func() { synced.Release() }()

	// 3–7 OVER THE SYNCED INCARNATION, and once more over a rebuilt one when
	// the pool refused receive-pack for the incarnation's shape (repairing):
	// the scratch's alternate is the incarnation, so that shape would refuse
	// every push, and the refusal comes before git reads a byte of the body,
	// so the second run receives the push whole. The row's judgments are made
	// again on the rebuilt one, from the refusals that needed no object.
	out := newSink(w, "application/x-git-receive-pack-result", s.now)
	defer out.End()
	// ITS BYTES, however the answer ends (Server.served): the push itself is
	// counted where it is recorded (audit).
	defer func() { s.served(adm, out, cache.Use{}) }()
	var rcv received
	runErr := s.repairing(ctx, r, adm.name, &synced, asAlternate,
		func() bool { return rcv.answered || rcv.claimed || rcv.ended || out.Started() },
		func(in cache.Synced) error {
			rcv = s.receiveOver(ctx, w, r, receiving{adm: adm, sec: sec, body: body, limits: limits,
				synced: in, pre: maps.Clone(pre), refuseAll: refuseAll, out: out})
			return rcv.runErr
		})
	if rcv.answered {
		return
	}
	sess, pushID, res, ended, claimed := rcv.sess, rcv.pushID, rcv.res, rcv.ended, rcv.claimed

	// 9 THE ANSWER AND THE RECORD. A push whose hook claimed its session is
	// recorded as the session concluded it WHATEVER THEN BECAME OF THE
	// CONNECTION: once proposed, the record may have landed, and "failed"
	// would deny it. Only a push no session concluded is recorded failed.
	rec := pushRecord{pushID: pushID, commands: len(sec.Commands), outcome: types.AuditFailed}
	if claimed {
		result := sess.outcome()
		if result.outcome != "" {
			rec.outcome = result.outcome
		}
		rec.refusal, rec.position, rec.ok, rec.bytes = result.refusal, result.position, result.ok, result.packBytes
	}
	switch {
	case runErr == nil && !claimed:
		// GIT REFUSED THE PUSH ITSELF — an unpack, fsck or connectivity
		// failure, a shallow update — and wrote its own report.
		rec.outcome, rec.refusal, rec.git = types.AuditRefused, RefusalGit, sentence(res.LastLine())
	case runErr == nil:
	case ended:
		// THE CLIENT HUNG UP, OR THE HOST IS STOPPING, and receive-pack's
		// kill is that end's: answered as it (s.ended), never as the code
		// log failing, and a started response is cut.
		audit.record(rec)
		if out.Started() {
			panic(http.ErrAbortHandler)
		}
		s.ended(w, r, adm.name, runErr)
		return
	case errors.Is(runErr, gitproc.ErrInput):
		// THE CLIENT'S BODY FAILED (cut short, too slow, not gzip). Past
		// git's first byte its unpack error ended the response; before it,
		// a 400, since returning unwritten answers an empty 200, which git
		// cannot tell from a push that did nothing.
		if !out.Started() {
			auth.GitText(w, http.StatusBadRequest, "the push's body could not be read: "+sentence(runErr.Error()))
		}
	case !out.Started():
		// GIT FAILED BEFORE ITS FIRST BYTE, or never started (every
		// transfer slot busy): answered with a status.
		audit.record(rec)
		s.syncFailed(w, r, adm.name, runErr, "push_id", pushID)
		return
	default:
		// PAST THE FIRST BYTE: git that exited by itself has written its
		// report, which ends the response as git ended it; a process the
		// engine killed (a deadline, a stall, its output failing) is cut.
		audit.record(rec)
		s.failedStarted(r, adm.name, runErr, "push_id", pushID)
		return
	}
	audit.record(rec)
}

// receiving is what a push brings to one run of its steps 3 to 7. pre is the
// run's own copy of the refusals made before it: the run adds what the row's
// protections refuse.
type receiving struct {
	adm       admission
	sec       section
	body      io.Reader
	limits    gitlet.Limits
	synced    cache.Synced
	pre       map[string]answered
	refuseAll func(AuditRefusal, []answered)
	out       *sink
}

// received is what one run of a push's steps 3 to 7 came to: answered when it
// answered the request itself (a refusal judged on the row, a fault, a scratch
// that could not be made), otherwise its hook's session and what receive-pack
// did.
type received struct {
	answered bool
	sess     *session
	pushID   string
	res      gitproc.Result
	runErr   error
	// ended: the request's own context ended — its client gone, or the host
	// stopping — by the time receive-pack returned. claimed: its hook
	// claimed the session.
	ended, claimed bool
}

// receiveOver is a push's steps 3 to 7 over p.synced: status and protections
// judged on its row, the push's identity, its scratch and session, and
// receive-pack run there. The scratch is removed and the session's work ended
// before it returns, so a second run over a rebuilt incarnation starts from
// nothing of the first.
func (s *Server) receiveOver(ctx context.Context, w http.ResponseWriter, r *http.Request, p receiving) received {
	adm, sec, synced, pre := p.adm, p.sec, p.synced, p.pre
	if s.refusedByStatus(synced.Repo, sec, p.refuseAll) {
		return received{answered: true}
	}
	rules, err := gitlet.ParseProtections(synced.Repo.Protections)
	if err != nil {
		// A RULE THIS BUILD CANNOT READ (a newer peer's pattern) is a snapshot
		// this build cannot use: asking again reads the same rule, and a newer
		// node may judge it.
		s.syncFailed(w, r, adm.name, fmt.Errorf("%w: %w", cache.ErrBadSnapshot, err))
		return received{answered: true}
	}
	judged := make(map[judgedKey]bool, len(sec.Commands))
	for _, c := range sec.Commands {
		if _, refused := pre[c.Ref]; refused {
			continue
		}
		cmd := gitlet.Command{Ref: c.Ref, Old: c.Old, New: c.New}
		judged[judgedKey{c.Ref, c.Old, c.New}] = true
		if reason, rule := gitlet.ProtectionRefusal(cmd, rules, adm.principal().Kind); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason, Detail: gitlet.ProtectionDetail(rule, reason)}
		}
	}
	if s.refusedEvery(sec, pre, p.refuseAll) {
		return received{answered: true}
	}
	if ferr := s.fault(ctx, gitletfault.HostAfterSync, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
		return received{answered: true}
	}
	// A BOX'S RUN HOLDS EVERY SEAT BRANCH IT PUSHES, on the run's row, before
	// anything of the push is received, stored or proposed (holdRefs).
	if err = adm.holdRefs(ctx, s.o.Boxes.Runs, sec.Commands, pre); err != nil {
		s.boxHoldFailed(w, r, adm, err)
		return received{answered: true}
	}

	// 4–5 THE PUSH'S IDENTITY AND ITS SCRATCH.
	pushID := uuid.Must(uuid.NewV7()).String()
	scratch, err := s.o.Cache.NewScratch(ctx, synced, pushID)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return received{answered: true}
	}
	defer scratch.Remove()
	if ferr := s.fault(ctx, gitletfault.HostAfterScratch, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
		return received{answered: true}
	}

	// 6 THE SESSION its hook will name.
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	sess := newSession(work, cancel)
	sess.synced, sess.scratch, sess.pushID = synced, scratch, pushID
	sess.principal, sess.adm = adm.principal(), adm
	sess.receivedAt, sess.preRefused, sess.judged = s.now(), pre, judged
	s.relay.open(sess)

	// 7 RECEIVE-PACK, in the scratch. No GIT_PROTOCOL: a push is never v2.
	res, runErr := s.runGit(work, gitproc.Cmd{
		Dir: scratch.Dir, Alternate: scratch.Alternate, Args: []string{"receive-pack", "--stateless-rpc", scratch.Dir},
		Config: receiveConfig(p.limits), Hooks: s.o.Cache.Hooks(),
		Env: []gitproc.KV{
			{Key: gitproc.EnvHookSocket, Value: s.relay.path},
			{Key: gitproc.EnvHookToken, Value: sess.token},
		},
		Stdin: io.MultiReader(bytes.NewReader(sec.Raw), p.body), Stdout: p.out,
		Class: gitproc.ClassTransfer, Share: adm.share(),
		Deadline: receivePackDeadline(p.limits, synced.Repo, sec.Commands, s.o.Timing),
		Stall:    s.o.Timing.Stall, Indexed: sess.indexed,
	})
	// THE REQUEST'S OWN END, read BEFORE relay.close, which ends work whatever
	// happened: read after it, every failure would look engine-killed.
	ended := ctx.Err() != nil
	claimed := s.relay.close(sess)
	return received{sess: sess, pushID: pushID, res: res, runErr: runErr, ended: ended, claimed: claimed}
}

// refusedByStatus refuses a push to a repository whose row refuses every push
// (statusRefusal): deleted or purging, or archived.
func (s *Server) refusedByStatus(repo gitlet.RepoRef, sec section,
	refuseAll func(AuditRefusal, []answered)) bool {

	reason, words := s.statusRefusal(repo)
	if reason == "" {
		return false
	}
	refuseAll(AuditRefusal(reason), refuseWith(sec.Commands, func(parsedCommand) (gitlet.Refusal, string) {
		return reason, words
	}))
	return true
}

// refusedEvery refuses, before receive-pack runs, a push every command of
// which pre refuses (any one, for an --atomic push: gitlet.Atomic): nothing is
// left to upload or propose. An attempt (gitlet.Attempted) is received
// instead, its refusals added to pre, so its landing records it with nothing
// stored.
func (s *Server) refusedEvery(sec section, pre map[string]answered,
	refuseAll func(AuditRefusal, []answered)) bool {

	if len(pre) == 0 || (len(pre) < len(sec.Commands) && !sec.Caps.atomic) {
		return false
	}
	cmds := make([]gitlet.Command, len(sec.Commands))
	for i, c := range sec.Commands {
		cmds[i] = gitlet.Command{Ref: c.Ref, Old: c.Old, New: c.New}
		if a, ok := pre[c.Ref]; ok {
			cmds[i].Result, cmds[i].Reason, cmds[i].Detail = gitlet.ResultNG, a.Reason, a.Detail
		}
	}
	cmds = gitlet.Atomic(cmds)
	if gitlet.Attempted(cmds) {
		for _, c := range cmds {
			if _, refused := pre[c.Ref]; !refused {
				pre[c.Ref] = answered{Ref: c.Ref, Reason: c.Reason, Detail: c.Detail}
			}
		}
		return false
	}
	var first gitlet.Refusal
	for _, c := range cmds {
		if pre[c.Ref].Reason != "" {
			first = pre[c.Ref].Reason
			break
		}
	}
	refuseAll(AuditRefusal(first), answersFor(cmds))
	return true
}

// refuseWith is every command refused as reason says, with its sentence.
func refuseWith(cmds []parsedCommand, reason func(parsedCommand) (gitlet.Refusal, string)) []answered {
	out := make([]answered, len(cmds))
	for i, c := range cmds {
		r, words := reason(c)
		out[i] = answered{Ref: c.Ref, Reason: r, Detail: words}
	}
	return out
}

// drain reads what is left of a refused push's body, up to limit and one
// byte: a response written while git still uploads is lost to a broken pipe,
// and git prints `RPC failed` instead of the reason. Past the limit the report
// is still written, and the client sees the transport fail, as receive-pack's
// own maxInputSize would show it.
func drain(body io.Reader, limit int64) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, limit+1))
}
