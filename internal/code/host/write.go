package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/toolname"
)

// AN ENGINE WRITE IS BUILT HERE AND LANDS AS A PUSH (ADR-0085). A seat's or a
// person's change made through a tool — files committed, a commit reverted or
// picked, a branch created or deleted — is built on the repository's git node,
// in a scratch beside its cache, by git's own plumbing over exactly the trees
// the change touches (build.go), and handed to the SAME landing every push
// takes (land.go): its status check, its protections, its blob limit, its size,
// the bundle stored with the listing, the proposal, the wait for its row and
// the adoption serve the write as they serve the relay. Its push id is an
// unnamed derivation of the caller's operation (code.WritePushID), and its
// commit's author and dates are pure functions of the operation and its parent
// (Signer, commitDates), so a retry — on this node, on the runner-up after
// silence, after a crash — is the same push and the same commit, and collapses
// at the committer.
//
// A REFUSAL IS DECIDED BEFORE THE BYTES AND NEVER ON A STALE READ (the host's
// rule since M2): every refusal a write would answer from its copy — the
// repository's status, the grant, a protection, a stale head — is judged
// again once at the log's head (a linearizable re-sync) before it is answered,
// so a seat's session floor, which holds its own writes and not a protection a
// person just removed, refuses nothing a current copy would let through —
// while the seat's re-syncs on this node last (code.NewResyncs): past them a
// seat's refusal is answered from its session, saying so, rather than paying
// the code log a barrier the census never gave it.

// Signer names an engine-built commit's author and committer from the serving
// node's epoch (the engine's codeSigner): the author is the acting principal —
// a seat's role name and role.email, or the bound person's, else
// `<handle>@<code.identity_domain>` — and the committer the engine,
// `Crewlet <engine@<code.identity_domain>>`, a CONVENTION that marks the
// commits the host built in a cooperating client's history and authenticates
// nothing: any client can write it, and only the push row (`via engine`, its
// principal) says who built a commit (ADR-0085). A coding box's push whose
// pack holds a commit naming it that the repository does not already hold is
// refused (forged_committer, landing.EngineCommitter); a person's may carry
// one.
type Signer interface {
	// Sign is the signature of a commit p makes. An error is a node that
	// cannot sign — it holds no epoch, or its chart holds no seat p names —
	// and the write runs nothing.
	Sign(p code.Principal) (Signature, error)
	// Committer is the address Sign names every commit's committer by on
	// the serving epoch, `engine@<code.identity_domain>` — what a coding
	// box's pack may hold no commit of that the repository does not already
	// hold (forged_committer). An error is a node that holds no epoch, which
	// judges no box's push.
	Committer() (string, error)
	// Author is the name and address a commit names the seat handle by as
	// its AUTHOR where the engine did not act as that seat (U43 a): a
	// squash's, authored by its change's owner, and the `Co-authored-by:`
	// trailer of each of its other authors — the chart's role name and
	// address as Sign names them, or, for a seat the chart no longer holds,
	// the handle at `<handle>@<code.identity_domain>`: a seat that left
	// still wrote what it wrote. An error is a node that holds no epoch.
	Author(handle string) (name, email string, err error)
}

// Signature is an engine-built commit's names and addresses, each one git
// writes as it is given (gitproc.IdentityRefusal): the Signer's to ensure.
type Signature struct {
	AuthorName, AuthorEmail       string
	CommitterName, CommitterEmail string
}

// The outcomes of a write, as crewlet.code.host.writes counts them.
const (
	writeApplied = "applied"
	writePending = "pending"
	writeUnknown = "unknown"
	writeRefused = "refused"
	writeUnready = "unready"
	writeFailed  = "failed"
)

// The stages of a write, as crewlet.code.host.write.duration times them.
const (
	stageSync  = "sync"
	stageBuild = "build"
	stageLand  = "land"
	stageTotal = "total"
)

// inflightWrite is one write of a push id running on this node, which a second
// Write of the same push id waits for (Server.Write).
type inflightWrite struct {
	done chan struct{}
	res  code.EngineWriteResult
	err  error
	// cut: the write ended because its OWN caller left or its answer fell due
	// — before it had an answer of its own — so a caller still waiting runs
	// the write itself rather than take that.
	cut bool
	// joined counts the writes that joined this one (under writesMu): the
	// suite's window onto the join (export_test.go).
	joined int
}

// errWritesStopping is the cause a write's context ends with when the host
// stops (Server.enter): never the answer, which is ErrNotServing about this
// node where the write ran nothing.
var errWritesStopping = errors.New("host: the code host is stopping")

// Write builds the change w asks for in a scratch beside this node's cache and
// lands it as one push (land, O15.1). It answers a refusal of the change
// (EngineWriteResult.Refusal), a refusal of the push (its Command), or the
// push's outcome, applied, pending or unknown.
//
//  0. w is held to code.EngineWrite.Validate.
//  1. A stopping host answers code.ErrNotServing, about this node. A SEAT'S
//     write on a node confirmed behind the company's configuration (Current
//     false) is code.ErrGitUnready{behind_config}, having started nothing, so
//     the walk asks the runner-up; a person's never consults it.
//  2. A second write of the same push id on this node waits for the first
//     (the IN-FLIGHT JOIN) and is answered what it is answered — a push id
//     names one scratch, once — unless the first ended only because ITS
//     caller left, when the second runs the write itself.
//  3. A node holding no copy of the repository, nor filling one, answers a
//     write that is not its walk's effective home's (code.EngineWrite.Home)
//     code.ErrGitUnready{not_held} at once; the copy is synced to the
//     caller's floor, within statelog.ReadBudget, the sync's failure read as
//     a read's is (cache.EveryFillMeets).
//  4. The repository's status, the stateless rules, the grant
//     (code.GrantOf on this node's chart, held to the write's ceiling —
//     code.EngineWrite.Ceiling) and the protections of the one
//     command the write makes are judged on the synced row; then the copy is
//     checked (build.go's check): where the branch is, by its exact name, and
//     what the objects the write names by id are — short reads, each one
//     lookup.
//  5. A head that is not the one the caller read is first looked up as this
//     write's own push (Estate.PushOutcome): FOUND, the write already landed
//     and is answered from its row, nothing built.
//  6. Every refusal of 3–5 — and an object the copy lacks — is judged again
//     once at the log's head (a linearizable re-sync) before it is answered:
//     a SEAT'S re-sync drawn from its seat's bucket on this node
//     (Options.Resyncs, its turn the principal — code.NewResyncs), and past
//     it the refusal is answered from the seat's session, saying it was not
//     judged again and when the seat's re-syncs come back.
//  7. The build (build.go), in the write's scratch (cache.NewScratch, named
//     the push id), under ONE unit of the node's seat git-work share, given
//     back before the landing, within ONE budget of git run time,
//     code.WriteBuildDeadline — a commit_files' read of what every path it
//     names and every directory above one is at its parent among it, whose
//     cost grows with the square of a path's depth (building.paths): a
//     refusal of the change, a conflict or nothing to change is answered
//     here, nothing stored. Then codefault.WriteAfterBuild.
//  8. THE DEADLINE RULE: a caller whose deadline cannot hold the bundle's
//     upload (code.UploadDeadline, at most code.WriteUploadAllowance) and the
//     proposal after it (code.MinProposeAttempt) is answered busy with
//     nothing uploaded — a slow bucket costs a refusal, never a PUT nobody
//     proposes.
//  9. land, with code.ViaEngine and the write's principal; the answer is the
//     landing's, its outcome from the push's row only.
//
// ctx ENDS WHEN THE ANSWER IS DUE (code.AnswerBy, the estate's git arm and the
// router's own in-process run): every wait before the landing that it ends is
// answered code.ErrGitUnready while the asker still listens; the landing's
// own are its own. A write cut by the host's stop before its landing ran
// nothing, and is answered code.ErrNotServing.
//
// Every write is counted (crewlet.code.host.writes) and timed
// (crewlet.code.host.write.duration), and logged code_write at info. It
// publishes NO operator_acted: that record is the relay's of a git push, which
// nothing else records; a person's tool call is already one from the act
// catalogue's dispatch, and a seat's write is its push row and its turn.
func (s *Server) Write(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	if err := w.Validate(); err != nil {
		return code.EngineWriteResult{}, err
	}
	pushID := code.WritePushID(w.OpID)
	ctx, end, ok := s.enter(ctx)
	if !ok {
		return code.EngineWriteResult{}, s.writesStopping()
	}
	defer end()
	if w.Principal.Kind == code.PrincipalAgent && s.o.Current != nil && !s.o.Current() {
		return code.EngineWriteResult{}, &code.ErrGitUnready{Reason: code.UnreadyBehindConfig, Detail: fmt.Sprintf(
			"%s is behind the company's configuration and judges no seat's write from the chart the fleet "+
				"replaced; a current git node builds it", s.o.NodeID)}
	}
	for {
		f, leads := s.joinWrite(pushID)
		if leads {
			res, err := s.write(ctx, w, pushID)
			s.landWrite(pushID, f, res, err, ctx.Err() != nil)
			return res, err
		}
		select {
		case <-f.done:
			if f.cut && ctx.Err() == nil {
				continue
			}
			return f.res, f.err
		case <-ctx.Done():
			if code.AnswerDue(ctx) {
				return code.EngineWriteResult{}, &code.ErrGitUnready{Reason: code.UnreadyShare, Detail: fmt.Sprintf(
					"%s is still building and landing this write (push %s) under an earlier ask, and its "+
						"answer fell due first; asked again, it is answered from the push's row", s.o.NodeID, pushID)}
			}
			return code.EngineWriteResult{}, s.cutOrGone(ctx)
		}
	}
}

// enter admits one write to a running host — the in-process twin of begin,
// which answers a request over HTTP — and answers the write's context: the
// caller's, cut by the host's stop with errWritesStopping as its cause, so a
// stop reaches every git process of a write in flight.
func (s *Server) enter(parent context.Context) (context.Context, func(), bool) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, nil, false
	}
	s.inflight.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancelCause(parent)
	//nolint:contextcheck // The host's lifetime cuts the write's work too: Stop must reach every git process in flight.
	stop := context.AfterFunc(s.life, func() { cancel(errWritesStopping) })
	return ctx, func() {
		stop()
		cancel(nil)
		s.inflight.Done()
	}, true
}

// writesStopping is what a write the stopping host started nothing for is
// answered: about this node, so the walk asks the runner-up.
func (s *Server) writesStopping() error {
	return fmt.Errorf("%w: %s's code host is stopping; another git node builds the write", code.ErrNotServing,
		s.o.NodeID)
}

// cutOrGone is a write's answer once its context ended outside its landing:
// the host's stop, about this node; the answer's due instant, unready; or
// the caller's own end, which nobody waits to hear.
func (s *Server) cutOrGone(ctx context.Context) error {
	switch {
	case errors.Is(context.Cause(ctx), errWritesStopping):
		return s.writesStopping()
	case code.AnswerDue(ctx):
		return &code.ErrGitUnready{Reason: code.UnreadyShare, Detail: fmt.Sprintf("%s could not build the write "+
			"before its answer fell due, its asker's attempt spent; another git node builds it", s.o.NodeID)}
	}
	return context.Cause(ctx)
}

// joinWrite is the in-flight write of pushID: one already running, and false;
// or a new one the caller leads, and true.
func (s *Server) joinWrite(pushID string) (*inflightWrite, bool) {
	s.writesMu.Lock()
	defer s.writesMu.Unlock()
	if f, ok := s.writes[pushID]; ok {
		f.joined++
		return f, false
	}
	if s.writes == nil {
		s.writes = map[string]*inflightWrite{}
	}
	f := &inflightWrite{done: make(chan struct{})}
	s.writes[pushID] = f
	return f, true
}

// landWrite ends the in-flight write of pushID with what it came to, letting
// every write waiting on it go. A FINISHED WRITE LEAVES NO ENTRY: a later
// retry runs the pipeline again and is answered from the push's row.
func (s *Server) landWrite(pushID string, f *inflightWrite, res code.EngineWriteResult, err error, cut bool) {
	s.writesMu.Lock()
	delete(s.writes, pushID)
	s.writesMu.Unlock()
	f.res, f.err, f.cut = res, err, cut && err != nil
	close(f.done)
}

// writing is one write in progress.
type writing struct {
	s      *Server
	w      code.EngineWrite
	pushID string
	cmd    code.Command // the one command the write makes: Ref, Old, and New once built

	// synced is the pinned copy the write is judged and built over, and
	// linear whether it was read at the log's head.
	synced cache.Synced
	linear bool
	// sig is the commit's signature, for a kind that commits.
	sig Signature
	// metered is set once the build has begun, and left is what remains of
	// its budget of git run time (build.go's budget and charge).
	metered bool
	left    time.Duration

	started             time.Time
	syncTook, buildTook time.Duration
	landTook            time.Duration
	landed              bool // the landing was reached: what follows is the push's
}

// write is one write's pipeline, counted, timed and logged however it ends.
func (s *Server) write(ctx context.Context, w code.EngineWrite, pushID string) (res code.EngineWriteResult,
	err error) {

	wr := &writing{s: s, w: w, pushID: pushID, started: s.now(), cmd: code.Command{Ref: w.Ref,
		Old: code.ZeroOID}}
	if w.ExpectedHead != "" {
		wr.cmd.Old = w.ExpectedHead
	}
	defer func() { wr.synced.Release() }()
	defer func() { wr.record(ctx, res, err) }()
	res, err = wr.run(ctx)
	if err != nil && !wr.landed && ctx.Err() != nil {
		// CUT BEFORE ITS LANDING, by the stop or the answer's due instant:
		// whatever the wait or the process it cut answered, the write ran
		// nothing, and what it says is about this node.
		err = s.cutOrGone(ctx)
	}
	res.PushID = pushID
	return res, err
}

// run is steps 3 to 9.
func (wr *writing) run(ctx context.Context) (code.EngineWriteResult, error) {
	s, w := wr.s, wr.w
	if !w.Home && !s.o.Cache.Holds(w.RepoID) {
		return code.EngineWriteResult{}, &code.ErrGitUnready{Reason: code.UnreadyNotHeld, Detail: fmt.Sprintf(
			"%s holds no copy of repository %s and the write is not its walk's home's, which is the one node "+
				"that fills a copy; a git node holding one builds it meanwhile", s.o.NodeID, w.RepoID)}
	}
	if w.Kind == code.WriteMergeChange {
		// A MERGE IS ITS OWN PIPELINE from here (merge.go): judged by its
		// change and its gate rather than the push rules of one command, and
		// landed as a merge record.
		return wr.runMerge(ctx)
	}
	syncStart := s.now()
	j, res, done, err := wr.judge(ctx)
	wr.syncTook = s.now().Sub(syncStart)
	if done || err != nil {
		return res, err
	}
	sc, err := s.o.Cache.NewScratch(ctx, wr.synced, wr.pushID)
	if err != nil {
		if errors.Is(err, cache.ErrStopped) {
			return code.EngineWriteResult{}, s.writesStopping()
		}
		return code.EngineWriteResult{}, fmt.Errorf("host: a scratch for write %s: %w", wr.pushID, err)
	}
	defer sc.Remove()

	buildStart := s.now()
	built, res, done, err := wr.buildShared(ctx, sc, j)
	wr.buildTook = s.now().Sub(buildStart)
	if done || err != nil {
		return res, err
	}
	if err := s.fault(ctx, codefault.WriteAfterBuild, 0); err != nil {
		return code.EngineWriteResult{}, fmt.Errorf("host: write %s after its build: %w", wr.pushID, err)
	}
	wr.cmd.New = built.commit
	if res, refused, err := wr.deadlineRule(ctx, sc); refused || err != nil {
		return res, err
	}
	return wr.land(ctx, sc)
}

// fresh is what the first sync asks: the caller's floor where it names one — at
// the level it names, session where it names none — and otherwise the copy as
// it stands, every refusal of which is judged again at the log's head.
func (wr *writing) fresh() statelog.Freshness {
	f := wr.w.Fresh
	floor := f.Floor(topics.CodeLogStream)
	switch {
	case f.Level == statelog.ReadLinearizable:
		return f
	case floor.IsZero():
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	f.Floors = []statelog.Position{floor}
	if f.Level == "" {
		f.Level = statelog.ReadSession
	}
	return f
}

// sync pins the copy at fresh, in place of any the write holds, its wait
// bounded by statelog.ReadBudget and by ctx — answered as a read's sync is
// (query's sync): a copy that cannot be brought there in time, or that failed
// on what every node's copy is filled from, is unready, syncing; one that
// failed for a reason of the node's own is unready, node_failed. Gone is the
// repository the rows hold no live row of.
func (wr *writing) sync(ctx context.Context, fresh statelog.Freshness) (gone bool, err error) {
	s := wr.s
	sctx, cancel := context.WithTimeout(ctx, s.syncWait)
	defer cancel()
	got, err := s.o.Cache.Sync(sctx, wr.w.RepoID, fresh)
	switch {
	case err == nil:
	case errors.Is(err, code.ErrRepoNotFound):
		wr.linear = fresh.Level == statelog.ReadLinearizable
		return true, nil
	case errors.Is(err, cache.ErrInvalid):
		return false, fmt.Errorf("%w: fresh: %v", code.ErrInvalid, err)
	case code.AnswerDue(ctx):
		return false, wr.unready(code.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not "+
			"reach the write's need before its answer fell due; the sync goes on, and another git node builds "+
			"it meanwhile", s.o.NodeID, wr.w.RepoID))
	case ctx.Err() != nil:
		return false, ctx.Err()
	case sctx.Err() != nil:
		return false, wr.unready(code.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not "+
			"reach the write's need within %s; the sync goes on, and another git node builds it meanwhile",
			s.o.NodeID, wr.w.RepoID, s.syncWait))
	case errors.Is(err, cache.ErrStopped):
		return false, s.writesStopping()
	case cache.EveryFillMeets(err):
		return false, wr.unready(code.UnreadySyncing, fmt.Sprintf("%s could not sync repository %s: %v — what "+
			"every git node's copy is filled from, so a git node holding a copy builds it meanwhile",
			s.o.NodeID, wr.w.RepoID, err))
	default:
		return false, wr.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not sync repository %s: %v; "+
			"another git node builds it, and fills a copy where none holds one", s.o.NodeID, wr.w.RepoID, err))
	}
	wr.synced.Release()
	wr.synced, wr.linear = got, fresh.Level == statelog.ReadLinearizable
	return false, nil
}

// unready is a refusal about this node, which another git node may answer.
func (wr *writing) unready(why code.UnreadyReason, detail string) error {
	return &code.ErrGitUnready{Reason: why, Detail: detail}
}

// writeVerdict is a refusal of the push a write's judgement reached: its reason and
// sentence, and whether it is the head's — a stale read the write's own
// landing may explain.
type writeVerdict struct {
	reason code.Refusal
	detail string
	stale  bool
}

// judge is steps 3 to 6: the copy synced to the caller's floor, the write
// judged on it, and — before any refusal is answered — once more at the log's
// head. It answers what the build reads (judgement) or, done, the write's
// answer: a refusal, or the push's own row where the write already landed.
func (wr *writing) judge(ctx context.Context) (judgement, code.EngineWriteResult, bool, error) {
	s, w := wr.s, wr.w
	asked, askedAt := false, ""
	fresh := wr.fresh()
	for {
		gone, err := wr.sync(ctx, fresh)
		if err != nil {
			return judgement{}, code.EngineWriteResult{}, true, err
		}
		var (
			v writeVerdict
			j judgement
		)
		if gone {
			v = writeVerdict{reason: code.RefusalRepoDeleted,
				detail: code.RepoGone{ID: w.RepoID, Retention: s.o.DeletedRetention()}.Detail()}
		} else {
			if v, err = wr.rules(); err != nil {
				return judgement{}, code.EngineWriteResult{}, true, err
			}
			if v.reason == "" {
				if j, v, err = wr.check(ctx); err != nil {
					return judgement{}, code.EngineWriteResult{}, true, err
				}
			}
		}
		switch {
		case v.reason == "" && j.absent == "":
			return j, code.EngineWriteResult{}, false, nil
		case v.stale && (!asked || j.head != askedAt):
			// A HEAD THAT MOVED may be this write's own push, landed by an
			// earlier ask of it — on this node, on another after silence —
			// and is looked up as one before it is called stale: found, the
			// write is answered from its row with nothing built. Asked again
			// at the log's head only where the head moved between the two.
			asked, askedAt = true, j.head
			if res, found, err := wr.outcome(ctx); err != nil || found {
				return judgement{}, res, true, err
			}
		}
		note := ""
		if !wr.linear {
			spent := wr.resync()
			if spent == nil {
				fresh = statelog.Freshness{Level: statelog.ReadLinearizable}
				continue
			}
			// THE SEAT'S RE-SYNCS ARE SPENT: the refusal is answered from
			// its session, saying it was not judged again.
			note = " " + code.NotRejudged(spent, "this turn's", "this seat's", s.now())
		}
		if j.absent != "" && v.reason == "" {
			return judgement{}, code.EngineWriteResult{}, true, fmt.Errorf("%w: repository %s holds no object %s%s",
				code.ErrObjectNotFound, wr.repoName(), j.absent, note)
		}
		return judgement{}, wr.refused(v.reason, v.detail+note), true, nil
	}
}

// resync draws the write's one re-judgement at linearizable from its seat's
// bucket on this node (Options.Resyncs, the query server's own): a SEAT'S
// write, its turn the principal, nil where it is taken; a person's is never
// charged — its floor is the log's head, which it reads by right.
func (wr *writing) resync() *costlimit.Refusal {
	p := wr.w.Principal
	if p.Kind != code.PrincipalAgent {
		return nil
	}
	return wr.s.o.Resyncs.Turn(p.Seat, p.Run.TurnID)
}

// rules judges the write's one command on the synced row, before any git
// runs: the repository's status, the stateless rules, the principal's grant
// and the protections — and signs a commit the write will make.
func (wr *writing) rules() (writeVerdict, error) {
	s, w := wr.s, wr.w
	repo := wr.synced.Repo
	if reason, words := s.statusRefusal(repo); reason != "" {
		return writeVerdict{reason: reason, detail: words}, nil
	}
	judged := wr.judged()
	if reason := code.StatelessRefusal(judged); reason != "" {
		return writeVerdict{reason: reason, detail: reason.Sentence()}, nil
	}
	var roster code.Roster
	if w.Principal.Kind == code.PrincipalAgent {
		if roster = s.o.Roster(); roster == nil {
			return writeVerdict{}, wr.unready(code.UnreadyNoChart, fmt.Sprintf("%s holds no chart yet to judge seat "+
				"%q's grant by; a git node holding one builds the write", s.o.NodeID, w.Principal.Seat))
		}
	}
	// THE SEAT'S GRANT, HELD TO WHOEVER ITS TURN WORKS FOR (code.WriteCeiling):
	// judged here, from this node's chart and the synced row, as the seat's
	// own is — never on the node the turn runs on.
	grant := code.GrantOf(w.Principal, repo, roster)
	if w.Ceiling != nil {
		grant = grant.Under(w.Ceiling.Grant(repo, roster))
	}
	if reason, words := grant.RefRule(judged); reason != "" {
		return writeVerdict{reason: reason, detail: words}, nil
	}
	rules, err := code.ParseProtections(repo.Protections)
	if err != nil {
		// A RULE THIS BUILD CANNOT READ is a row of the snapshot this build
		// cannot use: asking again reads the same rule, and a node of a newer
		// build may judge it.
		return writeVerdict{}, wr.unready(code.UnreadySyncing, fmt.Sprintf("%s cannot read a protection of "+
			"repository %s (%v); a node of a newer build judges it", s.o.NodeID, repo.Name, err))
	}
	if reason, rule := code.ProtectionRefusal(judged, rules, w.Principal.Kind); reason != "" {
		return writeVerdict{reason: reason, detail: code.ProtectionDetail(rule, reason)}, nil
	}
	if w.Kind.Commits() && wr.sig == (Signature{}) {
		sig, err := s.o.Signer.Sign(w.Principal)
		if err != nil {
			return writeVerdict{}, err
		}
		// A SIGNATURE GIT WOULD NOT WRITE AS GIVEN is the signer's bug: told
		// before the build rather than by commit-tree after it.
		for _, part := range []struct{ name, value string }{
			{"author name", sig.AuthorName}, {"author email", sig.AuthorEmail},
			{"committer name", sig.CommitterName}, {"committer email", sig.CommitterEmail},
		} {
			if why := gitproc.IdentityRefusal(part.value); why != "" {
				return writeVerdict{}, fmt.Errorf("host: the signer's %s %q for %s %s", part.name, part.value,
					w.Principal.Kind, why)
			}
		}
		wr.sig = sig
	}
	return writeVerdict{}, nil
}

// placeholder is the new id a write's command is judged with before its
// commit exists: the grant and the protections read only which ref moves and
// whether it is created, updated or deleted, never to what — and a write never
// forces, since its commit sits on the head it read.
const placeholder = "1111111111111111111111111111111111111111"

// judged is the write's one command as the rules judge it.
func (wr *writing) judged() code.Command {
	c := code.Command{Ref: wr.w.Ref, Old: wr.cmd.Old, New: placeholder}
	switch wr.w.Kind {
	case code.WriteDeleteBranch:
		c.New = code.ZeroOID
	case code.WriteCreateBranch:
		c.New = wr.w.Commit
	}
	return c
}

// refused is the write's answer to a refusal of its push, nothing built: the
// command as the write would have made it, its New the target of a create and
// empty where nothing was built.
func (wr *writing) refused(reason code.Refusal, detail string) code.EngineWriteResult {
	c := wr.cmd
	switch wr.w.Kind {
	case code.WriteCreateBranch:
		c.New = wr.w.Commit
	case code.WriteDeleteBranch:
		c.New = code.ZeroOID
	}
	c.Result, c.Reason, c.Detail = code.ResultNG, reason, detail
	return code.EngineWriteResult{Command: c}
}

// outcome is step 5: this write's own push looked up at the copy's position,
// within statelog.ReadBudget. Found, it is the write's answer, from its row. An
// outcome that cannot be learned is answered busy — never stale, which a write
// that may have landed must not be told.
func (wr *writing) outcome(ctx context.Context) (code.EngineWriteResult, bool, error) {
	s := wr.s
	octx, cancel := context.WithTimeout(ctx, s.syncWait)
	defer cancel()
	out, err := s.o.Estate.PushOutcome(octx, code.OutcomeQuery{RepoID: wr.w.RepoID, PushID: wr.pushID,
		At: wr.synced.Position})
	switch {
	case err == nil && out.Found && len(out.Push.Commands) > 0:
		c := out.Push.Commands[0]
		res := code.EngineWriteResult{Outcome: statelog.OutcomeApplied, Command: c, Position: out.Push.Position}
		if c.New != code.ZeroOID {
			res.Commit = c.New
		}
		return res, true, nil
	case err == nil:
		return code.EngineWriteResult{}, false, nil
	case code.AnswerDue(ctx):
		return code.EngineWriteResult{}, true, wr.unready(code.UnreadyShare, fmt.Sprintf("%s could not learn "+
			"whether this write already landed before its answer fell due; another git node is asked",
			s.o.NodeID))
	case ctx.Err() != nil:
		return code.EngineWriteResult{}, true, ctx.Err()
	}
	s.log.Warn("code_write_outcome_unread", "repo", s.repoNameOf(wr.synced.Repo), "push_id", wr.pushID,
		"error", err)
	return wr.refused(code.RefusalBusy, "could not learn whether this write already landed; make the same "+
		"call again"), true, nil
}

// repoName is the synced repository's name, or its id before any sync.
func (s *Server) repoNameOf(repo code.RepoRef) string {
	if repo.Name != "" {
		return repo.Name
	}
	return repo.ID
}

// repoName is the write's repository by name where its copy says one.
func (wr *writing) repoName() string {
	if wr.synced.Repo.Name != "" {
		return wr.synced.Repo.Name
	}
	return wr.w.RepoID
}

// buildShared is step 7: the build under one unit of the node's seat git-work
// share, taken for the build's git alone and given back before the landing,
// and within ONE budget of git run time its processes share
// (code.WriteBuildDeadline). Past code.GitShareWait the share is unready,
// about this node; a build whose own git runs the whole budget is too_large,
// since another node would take as long. A create or a delete of a branch
// builds nothing and takes no unit.
func (wr *writing) buildShared(ctx context.Context, sc *cache.Scratch, j judgement) (built,
	code.EngineWriteResult, bool, error) {

	s, w := wr.s, wr.w
	switch w.Kind {
	case code.WriteCreateBranch:
		return built{commit: w.Commit}, code.EngineWriteResult{}, false, nil
	case code.WriteDeleteBranch:
		return built{commit: code.ZeroOID}, code.EngineWriteResult{}, false, nil
	}
	release, err := s.o.Share.Take(ctx, code.SeatWorkWrite)
	if err != nil {
		if code.AnswerDue(ctx) {
			err = wr.unready(code.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole share until the "+
				"write's answer fell due, its asker's attempt spent; another git node builds it", s.o.NodeID))
		}
		return built{}, code.EngineWriteResult{}, true, err
	}
	defer release()
	// THE BUILD'S OWN GIT BEGINS: from here every process is given what is
	// left of one budget of run time, and charged what it ran.
	wr.metered, wr.left = true, s.buildBudget(w.Kind)
	b, res, done, err := wr.build(ctx, sc, j)
	wr.metered = false
	if errors.Is(err, errBuildSpent) {
		return built{}, wr.spent(), true, nil
	}
	return b, res, done, err
}

// spent is the answer to a build whose git ran the whole of its kind's budget:
// too_large, a change no node builds faster.
func (wr *writing) spent() code.EngineWriteResult {
	return wr.changeRefused(code.WriteTooLarge, fmt.Sprintf("building this change ran the whole %s of git one "+
		"%s is given on %s; make it in a checkout of the repository ("+toolname.CodeCheckout+")", wr.s.buildBudget(wr.w.Kind), wr.w.Kind,
		wr.s.o.NodeID))
}

// errBuildSpent is a build whose git ran the whole of its budget (its kind's,
// code.EngineWriteKind.BuildDeadline): answered too_large.
var errBuildSpent = errors.New("host: the write's build ran its whole budget of git")

// changeRefused is the write's answer to a refusal of its change: nothing
// built, uploaded or proposed.
func (wr *writing) changeRefused(r code.WriteRefusal, detail string) code.EngineWriteResult {
	return code.EngineWriteResult{Refusal: r, Detail: detail}
}

// deadlineRule is step 8: a write whose caller's deadline cannot hold its
// bundle's upload — code.UploadDeadline of its size, at most
// code.WriteUploadAllowance — and the proposal after it
// (code.MinProposeAttempt) is refused busy, with nothing uploaded: a slow
// bucket costs a refusal, never a PUT nobody proposes. ctx ends when the
// answer is due (code.AnswerBy), a reply's margin short of the caller's own
// deadline, so what is left of it is what the landing has. A caller with no
// deadline holds any.
func (wr *writing) deadlineRule(ctx context.Context, sc *cache.Scratch) (code.EngineWriteResult, bool, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return code.EngineWriteResult{}, false, nil
	}
	need := code.MinProposeAttempt
	packPath, checksum, held, err := sc.Pack()
	if err != nil {
		return code.EngineWriteResult{}, true, err
	}
	if held {
		ref, err := code.LayBundle(wr.pushID, packPath, checksum)
		if err != nil {
			return code.EngineWriteResult{}, true, err
		}
		need += min(code.UploadDeadline(ref.Size), code.WriteUploadAllowance)
	}
	if left := time.Until(deadline); left < need {
		return wr.refused(code.RefusalBusy, fmt.Sprintf("this write's caller left %s for its upload and "+
			"proposal, under the %s they take; ask again", left.Round(time.Millisecond), need)), true, nil
	}
	return code.EngineWriteResult{}, false, nil
}

// land is step 9: the landing every push takes, and the write's answer from
// what it came to.
func (wr *writing) land(ctx context.Context, sc *cache.Scratch) (code.EngineWriteResult, error) {
	s, w := wr.s, wr.w
	wr.landed = true
	landStart := s.now()
	l, err := s.land(ctx, landing{Synced: wr.synced, Scratch: sc, Commands: []code.Command{wr.cmd},
		Principal: w.Principal, Via: code.ViaEngine, PushID: wr.pushID, ReceivedAt: s.now()})
	wr.landTook = s.now().Sub(landStart)
	if err != nil {
		return code.EngineWriteResult{}, err
	}
	res := code.EngineWriteResult{Command: l.Commands[0], Position: l.Result.position}
	switch l.Result.outcome {
	case types.AuditApplied:
		res.Outcome = statelog.OutcomeApplied
	case types.AuditPending:
		res.Outcome = statelog.OutcomePending
	case types.AuditUnknown:
		res.Outcome = statelog.OutcomeUnknown
	}
	if res.Outcome == statelog.OutcomeApplied || res.Outcome == statelog.OutcomePending {
		res.Commit = wr.cmd.New
		if w.Kind == code.WriteDeleteBranch {
			res.Commit = ""
		}
	}
	return res, nil
}

// record counts, times and logs one write as it ended.
func (wr *writing) record(ctx context.Context, res code.EngineWriteResult, err error) {
	s := wr.s
	outcome := writeOutcome(res, err)
	total := s.now().Sub(wr.started)
	if s.o.Metrics != nil {
		kind := metrics.Attrs{"kind": string(wr.w.Kind), "outcome": outcome}
		s.o.Metrics.Add(metrics.CodeHostWrites, 1, kind)
		for stage, took := range map[string]time.Duration{stageSync: wr.syncTook, stageBuild: wr.buildTook,
			stageLand: wr.landTook, stageTotal: total} {
			if took > 0 || stage == stageTotal {
				s.o.Metrics.Observe(metrics.CodeHostWriteDuration, took,
					metrics.Attrs{"kind": string(wr.w.Kind), "stage": stage})
			}
		}
	}
	refusal := string(res.Refusal)
	switch {
	case refusal != "":
	case res.Merge != nil && res.Merge.Refusal != "":
		refusal = string(res.Merge.Refusal)
	default:
		refusal = string(res.Command.Reason)
	}
	// WHO WROTE, as code_push names a pusher: the seat the write is the
	// commit's author for — a person's human seat or the agent's own — and
	// the credential a person came through beside it (iam.ActorFor: one
	// person, many credentials), or the turn behind a seat's.
	p := wr.w.Principal
	attrs := []any{"repo", wr.repoName(), "kind", wr.w.Kind, "principal_kind", p.Kind, "seat", p.Seat,
		"operator_id", p.OperatorID, "turn_id", p.Run.TurnID, "push_id", wr.pushID, "outcome", outcome,
		"refusal", refusal, "sync_ms", wr.syncTook.Milliseconds(), "build_ms", wr.buildTook.Milliseconds(),
		"land_ms", wr.landTook.Milliseconds(), "total_ms", total.Milliseconds()}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	s.log.InfoContext(ctx, "code_write", attrs...)
}

// writeOutcome is how crewlet.code.host.writes counts a write that ended so.
func writeOutcome(res code.EngineWriteResult, err error) string {
	var unready *code.ErrGitUnready
	switch {
	case errors.As(err, &unready):
		return writeUnready
	case errors.Is(err, code.ErrInvalid), errors.Is(err, code.ErrObjectNotFound):
		return writeRefused
	case err != nil:
		return writeFailed
	case res.Outcome == statelog.OutcomeApplied:
		return writeApplied
	case res.Outcome == statelog.OutcomePending:
		return writePending
	case res.Outcome == statelog.OutcomeUnknown:
		return writeUnknown
	}
	return writeRefused
}

// short is an id's first seven digits, as a sentence names a commit.
func shortID(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

// headSentence is the stale verdict's sentence for a head that is not the one
// the write expects.
func (wr *writing) headSentence(head string) string {
	w := wr.w
	switch {
	case w.ExpectedHead == "" && head != "":
		return fmt.Sprintf("%s already exists, at %s; read it with %s and write onto its head", w.Ref,
			shortID(head), toolname.CodeResolve)
	case head == "":
		return fmt.Sprintf("%s does not exist; read it again with %s", w.Ref, toolname.CodeResolve)
	}
	return fmt.Sprintf("%s is at %s, not the %s you read; read it again with %s", w.Ref, shortID(head),
		shortID(w.ExpectedHead), toolname.CodeResolve)
}
