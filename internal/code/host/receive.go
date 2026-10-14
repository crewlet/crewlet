package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// POST /git/{repo}/git-receive-pack — A PUSH, the handler's half of the
// receive pipeline (steps 0–7 and the answer): its command section read and
// what can be refused without objects refused, the cache synced through the
// barrier, the push's scratch made, and receive-pack run there — never in the
// cache — with the one hook that relays every ref update to the session
// (session.go), which decides, uploads, proposes and reports.

// receiveConfig is the RECEIVE CONFIGURATION every push's receive-pack runs
// with, its advertisement's included, in this order:
//
//   - receive.procReceiveRefs=refs/: every command goes to the engine's hook,
//     and git moves no ref itself;
//   - receive.unpackLimit=1: the push is kept as ONE pack, the unit a row
//     names;
//   - receive.fsckObjects=true: a malformed object is refused at the door;
//   - receive.maxInputSize: the epoch's code.limits.max_push_bytes;
//   - receive.advertisePushOptions=false: nothing reads them;
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
		{Key: "receive.advertisePushOptions", Value: "false"},
		{Key: "receive.autogc", Value: "false"},
		{Key: "receive.updateServerInfo", Value: "false"},
		{Key: "pack.writeReverseIndex", Value: "true"},
		{Key: "pack.indexVersion", Value: "2"},
	}
}

// refusedNote is what a client asking for side-band sees beside a push the
// engine refused before receive-pack ran.
const refusedNote = "refused before anything was stored"

func (s *Server) receivePack(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	if r.Header.Get("Content-Type") != "application/x-git-receive-pack-request" {
		auth.GitText(w, http.StatusUnsupportedMediaType,
			"a git-receive-pack request must be application/x-git-receive-pack-request")
		return
	}
	limits := s.limits()
	drainLimit := limits.MaxPushBytes + MaxCommandSectionBytes
	if r.ContentLength > drainLimit {
		// A DECLARED LENGTH THIS LARGE is a client configured to buffer a
		// gigabyte (git sends one only under its post buffer); draining it
		// to say so is the cost the bound exists to refuse.
		auth.GitText(w, http.StatusRequestEntityTooLarge, "this push is "+humanBytes(r.ContentLength)+
			", past code.limits.max_push_bytes ("+humanBytes(limits.MaxPushBytes)+")")
		return
	}
	ctx, end, ok := s.begin(r.Context(), w)
	if !ok {
		return
	}
	defer end()
	adm, ok := s.admit(ctx, w, r, auth.GitPush)
	if !ok {
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
		s.audit(ctx, adm, pushRecord{outcome: types.AuditRefused, refusal: reason, commands: len(sec.Commands),
			started: start})
	}
	switch {
	case errors.Is(err, errTooManyCommands), errors.Is(err, errSectionTooLarge):
		words := "this push's command list is over 1 MiB"
		if errors.Is(err, errTooManyCommands) {
			words = "this push names more than " + strconv.Itoa(code.MaxCommandsPerPush) +
				" refs; push them in parts (git push origin <some refs>)"
		}
		if len(sec.Commands) == 0 {
			auth.GitText(w, http.StatusRequestEntityTooLarge, words)
			s.audit(ctx, adm, pushRecord{outcome: types.AuditRefused, refusal: RefusalCommands, started: start})
			return
		}
		refuseAll(RefusalCommands, refuseWith(sec.Commands, func(parsedCommand) (code.Refusal, string) {
			return "", words
		}))
		return
	case err != nil:
		auth.GitText(w, http.StatusBadRequest, "the push's command list could not be read: "+sentence(err.Error()))
		return
	case sec.Empty():
		// GIT'S PROBE before a push past its post buffer: a 200 and nothing
		// run, so git sends the real request.
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		return
	}
	if adm.seat == "" {
		refuseAll(RefusalUnbound, refuseWith(sec.Commands, func(parsedCommand) (code.Refusal, string) {
			return "", unboundSentence
		}))
		return
	}
	if s.refusedByStatus(adm.repo, sec, refuseAll) {
		return
	}
	pre := map[string]answered{}
	for _, c := range sec.Commands {
		if reason := code.StatelessRefusal(code.Command{Ref: c.Ref, Old: c.Old, New: c.New}); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason}
		}
	}
	if s.refusedEvery(sec, pre, refuseAll) {
		return
	}
	if ferr := s.fault(ctx, codefault.HostAfterCommands, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
		return
	}

	// 3 THE SYNC, through the barrier: a cache at or past the advertisement
	// holds every base a thin pack deltas against, and its row is what the
	// status and the protections are judged on — never the stale resolve.
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	defer synced.Release()
	if s.refusedByStatus(synced.Repo, sec, refuseAll) {
		return
	}
	rules, err := code.ParseProtections(synced.Repo.Protections)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	judged := make(map[judgedKey]bool, len(sec.Commands))
	for _, c := range sec.Commands {
		if _, refused := pre[c.Ref]; refused {
			continue
		}
		cmd := code.Command{Ref: c.Ref, Old: c.Old, New: c.New}
		judged[judgedKey{c.Ref, c.Old, c.New}] = true
		if reason, rule := code.ProtectionRefusal(cmd, rules); reason != "" {
			pre[c.Ref] = answered{Ref: c.Ref, Reason: reason, Detail: code.ProtectionDetail(rule, reason)}
		}
	}
	if s.refusedEvery(sec, pre, refuseAll) {
		return
	}
	if ferr := s.fault(ctx, codefault.HostAfterSync, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
		return
	}

	// 4–5 THE PUSH'S IDENTITY AND ITS SCRATCH.
	pushID := uuid.Must(uuid.NewV7()).String()
	scratch, err := s.o.Cache.NewScratch(ctx, synced, pushID)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	defer scratch.Remove()
	if ferr := s.fault(ctx, codefault.HostAfterScratch, 0); ferr != nil {
		s.syncFailed(w, r, adm.name, ferr)
		return
	}

	// 6 THE SESSION its hook will name.
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	sess := newSession(work, cancel)
	sess.repo, sess.synced, sess.scratch, sess.pushID = synced.Repo, synced, scratch, pushID
	sess.principal = code.Principal{Kind: code.PrincipalOperator, OperatorID: adm.operatorID, Seat: adm.seat}
	sess.receivedAt, sess.preRefused, sess.caps = s.now(), pre, sec.Caps
	sess.rules, sess.judged = rules, judged
	s.relay.open(sess)

	// 7 RECEIVE-PACK, in the scratch. No GIT_PROTOCOL: git never asks
	// protocol v2 of a push.
	out := newSink(w, "application/x-git-receive-pack-result", nil, s.now)
	defer out.End()
	res, runErr := s.o.Git.Run(work, gitproc.Cmd{
		Dir: scratch.Dir, Args: []string{"receive-pack", "--stateless-rpc", scratch.Dir},
		Config: receiveConfig(limits), Hooks: s.o.Cache.Hooks(),
		Env: []gitproc.KV{
			{Key: gitproc.EnvHookSocket, Value: s.relay.path},
			{Key: gitproc.EnvHookToken, Value: sess.token},
		},
		Stdin: io.MultiReader(bytes.NewReader(sec.Raw), body), Stdout: out, Class: gitproc.ClassTransfer,
		Deadline: gitproc.ReceivePackDeadline(limits.MaxPushBytes, HookBudget(s.o.Timing)),
		Stall:    s.o.Timing.Stall, Indexed: sess.indexed,
	})
	// THE REQUEST'S OWN END — its client gone, or the host stopping — read
	// BEFORE relay.close, which ends work whatever happened: judged after
	// it, every failure would read as a process the engine killed.
	ended := ctx.Err() != nil
	claimed := s.relay.close(sess)

	// 13 THE ANSWER AND THE RECORD. A push whose hook claimed its session
	// is recorded as the session concluded it — applied, pending, unknown,
	// refused — WHATEVER THEN BECAME OF THE CONNECTION: once a push reached
	// its proposal, the record may have landed, and "failed" would say it
	// did not. Only a push no session concluded is recorded failed.
	rec := pushRecord{pushID: pushID, commands: len(sec.Commands), started: start, outcome: types.AuditFailed}
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
		rec.outcome, rec.refusal, rec.git = types.AuditRefused, RefusalGit, lastLine(res.Stderr)
	case runErr == nil:
	case ended:
		// THE CLIENT HUNG UP, OR THE HOST IS STOPPING, and the kill of
		// receive-pack is that end's: answered as it (s.ended — a 503 a
		// stopping host gives, nothing to a client that is gone), never as
		// the code log failing, and a started response is cut.
		s.audit(ctx, adm, rec)
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
		s.audit(ctx, adm, rec)
		s.syncFailed(w, r, adm.name, runErr, "push_id", pushID)
		return
	default:
		// PAST THE FIRST BYTE: git that exited by itself has written its
		// report, which ends the response as git ended it; a process the
		// engine killed (a deadline, a stall, its output failing) is cut.
		s.audit(ctx, adm, rec)
		s.failedStarted(r, adm.name, runErr, "push_id", pushID)
		return
	}
	s.audit(ctx, adm, rec)
}

// refusedByStatus refuses a push to a repository whose row refuses every push:
// deleted or purging, or archived.
func (s *Server) refusedByStatus(repo code.RepoRef, sec section,
	refuseAll func(AuditRefusal, []answered)) bool {

	var (
		reason code.Refusal
		words  string
	)
	switch {
	case gone(repo.Status):
		reason, words = code.RefusalRepoDeleted, s.goneSentence(repo)
	case repo.Status == code.RepoStatusArchived:
		reason, words = code.RefusalRepoArchived, code.ArchivedDetail(repo.Name)
	default:
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
