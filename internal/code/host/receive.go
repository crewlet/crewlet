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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
)

// POST /git/{repo}/git-receive-pack — A PUSH, the handler's half of the
// receive pipeline (steps 0–7, and 9 the answer): its command section read and
// what can be refused without objects refused, the cache synced through the
// barrier (a coding box's at its run's session), the push's scratch made, and
// receive-pack run there — never in the
// cache — with the one hook that relays every ref update to the session
// (session.go), which hands them to the one landing every push takes
// (land.go, step 8) and reports what landed.

// receiveConfig is the RECEIVE CONFIGURATION every push's receive-pack runs
// with, its advertisement's included, in this order:
//
//   - receive.procReceiveRefs=refs/: every command goes to the engine's hook,
//     and git moves no ref itself;
//   - receive.unpackLimit=1: the push is kept as ONE pack, the unit a row
//     names;
//   - receive.fsckObjects=true: a malformed object is refused at the door;
//   - receive.maxInputSize: the epoch's code.limits.max_push_bytes;
//   - receive.advertisePushOptions=true: a push opens a change from its
//     options (`git push -o change`, code.ParseChangeOptions), which
//     receive-pack hands the hook after the commands — what it read, which
//     the relay reads there (session.go) — and which the handler's section
//     bound holds before receive-pack, which keeps every one in its memory,
//     reads any (readCommands);
//   - receive.autogc=false and receive.updateServerInfo=false: a scratch is
//     thrown away;
//   - pack.writeReverseIndex=true and pack.indexVersion=2: the .rev and the
//     version-2 .idx the push's BUNDLE carries, written by receive-pack's own
//     index-pack, so no other process and no other node ever indexes the pack.
func receiveConfig(l code.Limits) []gitproc.KV {
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
		// A DECLARED LENGTH THIS LARGE is a client configured to buffer a
		// gigabyte (git sends one only under its post buffer); draining it
		// to say so is the cost the bound exists to refuse. Judged once the
		// request is admitted, never before it, so the push is recorded as
		// whoever sent it — and a client nobody knows is challenged first,
		// as for every other refusal of a push.
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
	sec, err := readCommands(body, MaxCommandSectionBytes, code.MaxCommandsPerPush)
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
			words = "this push names more than " + strconv.Itoa(code.MaxCommandsPerPush) +
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
		refuseAll(RefusalCommands, refuseWith(sec.Commands, func(parsedCommand) (code.Refusal, string) {
			return "", words
		}))
		return
	case errors.Is(err, errPushCertificate):
		drain(body, drainLimit)
		auth.GitText(w, http.StatusBadRequest, pushCertificateSentence)
		audit.record(pushRecord{outcome: types.AuditRefused, refusal: RefusalCommands, commands: len(sec.Commands)})
		return
	case err != nil:
		// A COMMAND LIST THAT COULD NOT BE READ is a push all the same, and
		// recorded like every other: refused for its commands when its bytes
		// are not pkt-lines — the client's to fix, as git's own unpack error
		// over a pack that is not one is — and failed when the body itself
		// failed (cut short, not gzip, too slow), as it is past the section.
		rec := pushRecord{outcome: types.AuditFailed, commands: len(sec.Commands)}
		if errors.Is(err, errMalformed) {
			rec.outcome, rec.refusal = types.AuditRefused, RefusalCommands
		}
		auth.GitText(w, http.StatusBadRequest, "the push's command list could not be read: "+sentence(err.Error()))
		audit.record(rec)
		return
	case sec.Empty():
		// GIT'S PROBE before a push past its post buffer: a 200 and nothing
		// run, so git sends the real request — the push's end, which records
		// it. This one is none.
		audit.none()
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		return
	}
	// A FAILURE FROM HERE ON is recorded with the commands the push names.
	audit.failed.commands = len(sec.Commands)
	// THE GRANT (ADR-0058), before the repository's status: a principal that
	// may push nothing here — a token bound to nobody — is refused whole, as
	// its advertisement refused it, every command with the sentence its grant
	// gives. One that may push some commands has the rest refused beside the
	// stateless rules, which come first: a reserved ref is reserved_ref to
	// everybody, never a question of who asks. An update is judged here as a
	// fast-forward, since whether it is forced is the landing's verdict, after
	// the sync — which no PERSON or token can tell apart: a person maintains
	// and passes both rows, and a token bound to nobody passes neither. A
	// coding box's seat can — a seat holding write updates and may not force
	// — and its forced update is refused by the decide's grant rule (2b) on
	// the landing's verdict, as an engine write's is.
	grant := adm.grant
	if !grant.MayPush() {
		refuseAll(grantRefusal(adm.principal()), refuseWith(sec.Commands,
			func(c parsedCommand) (code.Refusal, string) {
				return grant.RefRule(code.Command{Ref: c.Ref, Old: c.Old, New: c.New})
			}))
		return
	}
	if s.refusedByStatus(adm.repo, sec, refuseAll) {
		return
	}
	pre := map[string]answered{}
	for _, c := range sec.Commands {
		cmd := code.Command{Ref: c.Ref, Old: c.Old, New: c.New}
		if reason := code.StatelessRefusal(cmd); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason}
		} else if reason, words := grant.RefRule(cmd); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason, Detail: words}
		}
	}
	if s.refusedEvery(sec, pre, refuseAll) {
		return
	}
	// WHAT ONE PUSH MAY CARRY, before any work is spent on it: the commands
	// as they will be proposed, with everything the work may add at its
	// largest (code.Proposal.ValidateCommands). Past it, a push was refused by
	// its committer after receive-pack, the scans, the bundle's put and the
	// estate's round trip — as the engine refusing its own proposal.
	if words := commandsRefusal(adm, s.o.NodeID, sec, pre); words != "" {
		refuseAll(RefusalCommands, refuseWith(sec.Commands, func(parsedCommand) (code.Refusal, string) {
			return "", words
		}))
		return
	}
	if ferr := s.fault(ctx, codefault.HostAfterCommands, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
		return
	}

	// 3 THE SYNC, at the request's refs level (admission.refs): through the
	// barrier for a person, and for a coding box at its run's session — its
	// floor, which its own advertisement raised to what it showed. Either way
	// a cache at or past the advertisement holds every base a thin pack
	// deltas against, and its row is what the status and the protections are
	// judged on — never the stale resolve.
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, adm.refs())
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	defer func() { synced.Release() }()

	// 3–7 OVER THE SYNCED INCARNATION, and once more over a rebuilt one when
	// the pool refused receive-pack for the incarnation's shape (repairing):
	// the scratch's one alternate is the incarnation, so a cache git refuses
	// for its shape would refuse every push to it, and the refusal comes
	// before git starts or a byte of the body is read — the push is received
	// whole on the second run. Everything judged on the row is judged again
	// on the rebuilt one, from the refusals that needed no object.
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

	// 9 THE ANSWER AND THE RECORD. A push whose hook claimed its session
	// is recorded as the session concluded it — applied, pending, unknown,
	// refused — WHATEVER THEN BECAME OF THE CONNECTION: once a push reached
	// its proposal, the record may have landed, and "failed" would say it
	// did not. Only a push no session concluded is recorded failed.
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
		// THE CLIENT HUNG UP, OR THE HOST IS STOPPING, and the kill of
		// receive-pack is that end's: answered as it (s.ended — a 503 a
		// stopping host gives, nothing to a client that is gone), never as
		// the code log failing, and a started response is cut.
		audit.record(rec)
		if out.Started() {
			panic(http.ErrAbortHandler)
		}
		s.ended(w, r, adm.name, runErr)
		return
	case errors.Is(runErr, gitproc.ErrInput):
		// THE CLIENT'S BODY FAILED — cut short, too slow for its pace, or
		// not gzip — and git read no whole push to answer. Past git's first
		// byte its own report (an unpack error) ended the response; before
		// it the client is told 400, as a fetch whose body failed is — a
		// handler returning without writing answers an empty 200, which git
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

// receiving is what a push brings to one run of its steps 3 to 7: its
// admission and command section, the body past the section, the epoch's
// limits, the incarnation it is received over, the refusals made before it
// (its own copy: the run adds what the row's protections refuse), and how a
// refusal of every command is answered.
type receiving struct {
	adm       admission
	sec       section
	body      io.Reader
	limits    code.Limits
	synced    cache.Synced
	pre       map[string]answered
	refuseAll func(AuditRefusal, []answered)
	out       *sink
}

// received is what one run of a push's steps 3 to 7 came to: answered when
// it answered the request itself — a refusal judged on the row, a fault, a
// scratch that could not be made — and otherwise the session its hook was
// given and what receive-pack did.
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

// receiveOver is a push's steps 3 to 7 over p.synced: the status and the
// protections judged on its row, the push's identity, its scratch over the
// incarnation and its session, and receive-pack run there. The scratch is
// removed and the session's work ended before it returns, so a second run
// over a rebuilt incarnation starts from nothing of the first.
func (s *Server) receiveOver(ctx context.Context, w http.ResponseWriter, r *http.Request, p receiving) received {
	adm, sec, synced, pre := p.adm, p.sec, p.synced, p.pre
	if s.refusedByStatus(synced.Repo, sec, p.refuseAll) {
		return received{answered: true}
	}
	rules, err := code.ParseProtections(synced.Repo.Protections)
	if err != nil {
		// A RULE THIS BUILD CANNOT READ — a pattern a newer peer admitted —
		// is a row of the snapshot this build cannot use, answered as one:
		// asking again reads the same rule, and a node of a newer build may
		// judge it.
		s.syncFailed(w, r, adm.name, fmt.Errorf("%w: %w", cache.ErrBadSnapshot, err))
		return received{answered: true}
	}
	judged := make(map[judgedKey]bool, len(sec.Commands))
	for _, c := range sec.Commands {
		if _, refused := pre[c.Ref]; refused {
			continue
		}
		cmd := code.Command{Ref: c.Ref, Old: c.Old, New: c.New}
		judged[judgedKey{c.Ref, c.Old, c.New}] = true
		if reason, rule := code.ProtectionRefusal(cmd, rules, adm.principal().Kind); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason, Detail: code.ProtectionDetail(rule, reason)}
		}
	}
	if s.refusedEvery(sec, pre, p.refuseAll) {
		return received{answered: true}
	}
	if ferr := s.fault(ctx, codefault.HostAfterSync, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
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
	if ferr := s.fault(ctx, codefault.HostAfterScratch, 0); ferr != nil {
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

	// 7 RECEIVE-PACK, in the scratch. No GIT_PROTOCOL: git never asks
	// protocol v2 of a push.
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
	// THE REQUEST'S OWN END — its client gone, or the host stopping — read
	// BEFORE relay.close, which ends work whatever happened: judged after
	// it, every failure would read as a process the engine killed.
	ended := ctx.Err() != nil
	claimed := s.relay.close(sess)
	return received{sess: sess, pushID: pushID, res: res, runErr: runErr, ended: ended, claimed: claimed}
}

// refusedByStatus refuses a push to a repository whose row refuses every push
// (statusRefusal): deleted or purging, or archived.
func (s *Server) refusedByStatus(repo code.RepoRef, sec section,
	refuseAll func(AuditRefusal, []answered)) bool {

	reason, words := s.statusRefusal(repo)
	if reason == "" {
		return false
	}
	refuseAll(AuditRefusal(reason), refuseWith(sec.Commands, func(parsedCommand) (code.Refusal, string) {
		return reason, words
	}))
	return true
}

// refusedEvery refuses a push every command of which pre refuses — or, an
// --atomic push, any of whose commands pre refuses (code.Atomic) — before
// receive-pack runs: there is nothing to upload and nothing to propose.
func (s *Server) refusedEvery(sec section, pre map[string]answered,
	refuseAll func(AuditRefusal, []answered)) bool {

	if len(pre) == 0 || (len(pre) < len(sec.Commands) && !sec.Caps.atomic) {
		return false
	}
	cmds := make([]code.Command, len(sec.Commands))
	for i, c := range sec.Commands {
		cmds[i] = code.Command{Ref: c.Ref, Old: c.Old, New: c.New}
		if a, ok := pre[c.Ref]; ok {
			cmds[i].Result, cmds[i].Reason, cmds[i].Detail = code.ResultNG, a.Reason, a.Detail
		}
	}
	cmds = code.Atomic(cmds)
	var first code.Refusal
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
func refuseWith(cmds []parsedCommand, reason func(parsedCommand) (code.Refusal, string)) []answered {
	out := make([]answered, len(cmds))
	for i, c := range cmds {
		r, words := reason(c)
		out[i] = answered{Ref: c.Ref, Reason: r, Detail: words}
	}
	return out
}

// drain reads what is left of a refused push's body, up to limit and one
// byte: a response written while git is still uploading is lost to a broken
// pipe, and git prints `RPC failed` instead of the reason (measured). A body
// past the limit is not drained further — the report is still written, and a
// client that sent more than its limit sees the transport fail, which is
// what receive-pack's own maxInputSize would show it.
func drain(body io.Reader, limit int64) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, limit+1))
}
