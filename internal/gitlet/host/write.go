package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/toolname"
)

// Signer names an engine-built commit's author and committer from the serving
// node's epoch (the engine's codeSigner): the author is the acting principal (a
// seat's role name and role.email, or the bound person's, else
// `<handle>@<code.identity_domain>`), the committer
// `Crewlet <engine@<code.identity_domain>>`. That committer is a convention and
// authenticates nothing: only the push row (`via engine`) says who built a
// commit (ADR-0092). A coding box's pack holding a commit naming it that the
// repository lacks is refused (forged_committer, landing.EngineCommitter); a
// person's may carry one.
type Signer interface {
	// Sign is the signature of a commit p makes. An error (no epoch, or no
	// seat p names on the chart) means the write runs nothing.
	Sign(p gitlet.Principal) (Signature, error)
	// Committer is the address Sign names every committer by on the serving
	// epoch, `engine@<code.identity_domain>` (what forged_committer judges a
	// box's pack by). An error is a node that holds no epoch.
	Committer() (string, error)
	// Author is the name and address a commit authors the seat handle by
	// where the engine did not act as that seat (U43 a): a squash's owner and
	// each `Co-authored-by:` trailer. It is the chart's role name and address
	// as Sign gives them or, for a seat the chart no longer holds,
	// `<handle>@<code.identity_domain>`: a seat that left still wrote what it
	// wrote. An error is a node that holds no epoch.
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
	res  gitlet.EngineWriteResult
	err  error
	// cut: the write ended without an answer because its OWN caller left or
	// its answer fell due, so a caller still waiting runs the write itself.
	cut bool
	// joined counts the writes that joined this one (under writesMu), for the
	// suite (export_test.go).
	joined int
}

// errWritesStopping is the cause a write's context ends with when the host
// stops (Server.enter): never the answer, which is ErrNotServing about this
// node where the write ran nothing.
var errWritesStopping = errors.New("host: Gitlet is stopping")

// Write builds the change w asks for in a scratch beside this node's cache and
// lands it as one push (package doc, "An engine write is built beside the copy
// and lands as a push"). It answers a refusal of the change
// (EngineWriteResult.Refusal), of the push (its Command), or the push's outcome.
// A stopping host answers gitlet.ErrNotServing; a seat's write on a node behind
// the company's configuration (Current false) is ErrGitUnready{behind_config},
// so the walk asks the runner-up; writtenForOn judges whom it is made for. A
// second write of a push id on this node joins the first (a push id names one
// scratch) and takes its answer, unless the first was cut only by its own
// caller leaving. The rest is run's. ctx ends when the answer is due
// (gitlet.AnswerBy): a wait before the landing it ends is answered unready, and
// a write cut there ran nothing (cutOrGone). record counts, times and logs it.
func (s *Server) Write(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	if err := w.Validate(); err != nil {
		return gitlet.EngineWriteResult{}, err
	}
	pushID := gitlet.WritePushID(w.OpID)
	ctx, end, ok := s.enter(ctx)
	if !ok {
		return gitlet.EngineWriteResult{}, s.writesStopping()
	}
	defer end()
	if w.Principal.Kind == gitlet.PrincipalAgent && s.o.Current != nil && !s.o.Current() {
		return gitlet.EngineWriteResult{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyBehindConfig, Detail: fmt.Sprintf(
			"%s is behind the company's configuration and judges no seat's write from the chart the fleet "+
				"replaced; a current Gitlet node builds it", s.o.NodeID)}
	}
	if err := s.writtenForOn(w); err != nil {
		return gitlet.EngineWriteResult{}, err
	}
	for {
		f, leads := s.joinWrite(pushID)
		if leads {
			res, err := s.write(ctx, w, pushID)
			s.landWrite(pushID, f, res, err, ctx.Err() != nil)
			return res, err
		}
		if err := s.fault(ctx, gitletfault.WriteJoined, 0); err != nil {
			return gitlet.EngineWriteResult{}, err
		}
		select {
		case <-f.done:
			if f.cut && ctx.Err() == nil {
				continue
			}
			return f.res, f.err
		case <-ctx.Done():
			if gitlet.AnswerDue(ctx) {
				return gitlet.EngineWriteResult{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyShare, Detail: fmt.Sprintf(
					"%s is still building and landing this write (push %s) under an earlier ask, and its "+
						"answer fell due first; asked again, it is answered from the push's row", s.o.NodeID, pushID)}
			}
			return gitlet.EngineWriteResult{}, s.cutOrGone(ctx)
		}
	}
}

// writtenForOn is why a seat's write w may not be made for whom it states on
// this node's chart (gitlet.EngineWrite.WrittenForOn), or nil: only the chart
// tells a person's seat, which bounds nothing, from a colleague's, whose
// ceiling the write carries. It judges the request, so it runs before anything
// is read or built; a node holding no chart yet answers unready, about itself,
// so a node holding one builds it. A write carrying nobody, and a person's,
// need no chart.
func (s *Server) writtenForOn(w gitlet.EngineWrite) error {
	if w.Principal.Kind != gitlet.PrincipalAgent || w.WrittenFor.Carrier() == nil {
		return nil
	}
	roster := s.o.Roster()
	if roster == nil {
		return &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNoChart, Detail: fmt.Sprintf("%s holds no chart yet "+
			"to judge whom seat %q's write is made for; a Gitlet node holding one builds it", s.o.NodeID,
			w.Principal.Seat)}
	}
	return w.WrittenForOn(roster)
}

// enter admits one write to a running host (the in-process twin of begin) and
// answers its context: the caller's, cut by the host's stop with cause
// errWritesStopping, so a stop reaches every git process in flight.
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
	return fmt.Errorf("%w: %s's Gitlet is stopping; another Gitlet node builds the write", gitlet.ErrNotServing,
		s.o.NodeID)
}

// cutOrGone is a write's answer once its context ended outside its landing:
// the host's stop, about this node; the answer's due instant, unready; or
// the caller's own end, which nobody waits to hear.
func (s *Server) cutOrGone(ctx context.Context) error {
	switch {
	case errors.Is(context.Cause(ctx), errWritesStopping):
		return s.writesStopping()
	case gitlet.AnswerDue(ctx):
		return &gitlet.ErrGitUnready{Reason: gitlet.UnreadyShare, Detail: fmt.Sprintf("%s could not build the write "+
			"before its answer fell due, its asker's attempt spent; another Gitlet node builds it", s.o.NodeID)}
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

// landWrite ends the in-flight write of pushID with what it came to, releasing
// its waiters. It leaves no entry: a later retry runs the pipeline again and is
// answered from the push's row.
func (s *Server) landWrite(pushID string, f *inflightWrite, res gitlet.EngineWriteResult, err error, cut bool) {
	s.writesMu.Lock()
	delete(s.writes, pushID)
	s.writesMu.Unlock()
	f.res, f.err, f.cut = res, err, cut && err != nil
	close(f.done)
}

// writing is one write in progress.
type writing struct {
	s      *Server
	w      gitlet.EngineWrite
	pushID string
	cmd    gitlet.Command // the one command the write makes: Ref, Old, and New once built

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
	// landNeed is what the last deadlineRule held the caller's deadline to,
	// 0 before one or with no deadline: merge.go's roomToRebuild measures a
	// rebuild's landing by it.
	landNeed time.Duration
	// opens is the change the push opens as its build formed it (a
	// revert_change's "Revert <repo>!<n>"), nil for every other kind.
	opens *gitlet.ChangeOption

	started             time.Time
	syncTook, buildTook time.Duration
	landTook            time.Duration
	landed              bool // the landing was reached: what follows is the push's
}

// write is one write's pipeline, counted, timed and logged however it ends.
func (s *Server) write(ctx context.Context, w gitlet.EngineWrite, pushID string) (res gitlet.EngineWriteResult,
	err error) {

	wr := &writing{s: s, w: w, pushID: pushID, started: s.now(), cmd: gitlet.Command{Ref: w.Ref,
		Old: gitlet.ZeroOID}}
	if w.ExpectedHead != "" {
		wr.cmd.Old = w.ExpectedHead
	}
	defer func() { wr.synced.Release() }()
	defer func() { wr.record(ctx, res, err) }()
	res, err = wr.run(ctx)
	if err != nil && !wr.landed && ctx.Err() != nil {
		// Cut before its landing: whatever the cut wait or process said, the
		// write ran nothing, and the answer is about this node.
		err = s.cutOrGone(ctx)
	}
	res.PushID = pushID
	return res, err
}

// run is the write after its join: judge, buildShared, deadlineRule, land. A
// node holding no copy answers unready, not_held, unless it is the walk's home
// (gitlet.EngineWrite.Home), the one node that fills a copy.
func (wr *writing) run(ctx context.Context) (gitlet.EngineWriteResult, error) {
	s, w := wr.s, wr.w
	if !w.Home && !s.o.Cache.Holds(w.RepoID) {
		return gitlet.EngineWriteResult{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNotHeld, Detail: fmt.Sprintf(
			"%s holds no copy of repository %s and the write is not its walk's home's, which is the one node "+
				"that fills a copy; a Gitlet node holding one builds it meanwhile", s.o.NodeID, w.RepoID)}
	}
	if w.Kind == gitlet.WriteMergeChange {
		// A merge is its own pipeline (merge.go): judged by its change and
		// gate, not one command's push rules, and landed as a merge record.
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
			return gitlet.EngineWriteResult{}, s.writesStopping()
		}
		return gitlet.EngineWriteResult{}, fmt.Errorf("host: a scratch for write %s: %w", wr.pushID, err)
	}
	defer sc.Remove()

	buildStart := s.now()
	made, res, done, err := wr.buildShared(ctx, sc, j)
	wr.buildTook = s.now().Sub(buildStart)
	if done || err != nil {
		return res, err
	}
	if err := s.fault(ctx, gitletfault.WriteAfterBuild, 0); err != nil {
		return gitlet.EngineWriteResult{}, fmt.Errorf("host: write %s after its build: %w", wr.pushID, err)
	}
	wr.cmd.New = made.commit
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

// sync pins the copy at fresh in place of any the write holds, within
// statelog.ReadBudget and ctx, its failures answered as query's sync answers
// them: late, or failed on what every node's copy is filled from, is unready,
// syncing; failed for the node's own reason, node_failed. gone is a repository
// with no live row.
func (wr *writing) sync(ctx context.Context, fresh statelog.Freshness) (gone bool, err error) {
	s := wr.s
	sctx, cancel := context.WithTimeout(ctx, s.syncWait)
	defer cancel()
	got, err := s.o.Cache.Sync(sctx, wr.w.RepoID, fresh)
	switch {
	case err == nil:
	case errors.Is(err, gitlet.ErrRepoNotFound):
		wr.linear = fresh.Level == statelog.ReadLinearizable
		return true, nil
	case errors.Is(err, cache.ErrInvalid):
		return false, fmt.Errorf("%w: fresh: %w", gitlet.ErrInvalid, err)
	case gitlet.AnswerDue(ctx):
		return false, wr.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not "+
			"reach the write's need before its answer fell due; the sync goes on, and another Gitlet node builds "+
			"it meanwhile", s.o.NodeID, wr.w.RepoID))
	case ctx.Err() != nil:
		return false, ctx.Err()
	case sctx.Err() != nil:
		return false, wr.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not "+
			"reach the write's need within %s; the sync goes on, and another Gitlet node builds it meanwhile",
			s.o.NodeID, wr.w.RepoID, s.syncWait))
	case errors.Is(err, cache.ErrStopped):
		return false, s.writesStopping()
	case cache.EveryFillMeets(err):
		return false, wr.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s could not sync repository %s: %v — what "+
			"every Gitlet node's copy is filled from, so a Gitlet node holding a copy builds it meanwhile",
			s.o.NodeID, wr.w.RepoID, err))
	default:
		return false, wr.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not sync repository %s: %v; "+
			"another Gitlet node builds it, and fills a copy where none holds one", s.o.NodeID, wr.w.RepoID, err))
	}
	wr.synced.Release()
	wr.synced, wr.linear = got, fresh.Level == statelog.ReadLinearizable
	return false, nil
}

// unready is a refusal about this node, which another Gitlet node may answer.
func (wr *writing) unready(why gitlet.UnreadyReason, detail string) error {
	return &gitlet.ErrGitUnready{Reason: why, Detail: detail}
}

// writeVerdict is a refusal of the push a write's judgement reached: its reason,
// its sentence, and whether it is a stale head, which the write's own earlier
// landing may explain.
type writeVerdict struct {
	reason gitlet.Refusal
	detail string
	stale  bool
}

// judge syncs the copy to the caller's floor (fresh) and judges the write on it
// (rules, then build.go's check). A refusal, or an object the copy lacks, is
// judged once more at the log's head before it is answered, so a session floor
// refuses nothing a current copy allows; a seat's re-sync comes from its bucket
// (resync), and once spent the refusal is answered from its session, saying so
// and when re-syncs return. It answers what the build reads or, done, the
// write's answer: a refusal, or its own push's row where it already landed.
func (wr *writing) judge(ctx context.Context) (judgement, gitlet.EngineWriteResult, bool, error) {
	s, w := wr.s, wr.w
	asked, askedAt := false, ""
	fresh := wr.fresh()
	for {
		gone, err := wr.sync(ctx, fresh)
		if err != nil {
			return judgement{}, gitlet.EngineWriteResult{}, true, err
		}
		var (
			v writeVerdict
			j judgement
		)
		if gone {
			v = writeVerdict{reason: gitlet.RefusalRepoDeleted,
				detail: gitlet.RepoGone{ID: w.RepoID, Retention: s.o.DeletedRetention()}.Detail()}
		} else {
			if v, err = wr.rules(); err != nil {
				return judgement{}, gitlet.EngineWriteResult{}, true, err
			}
			if v.reason == "" {
				if j, v, err = wr.check(ctx); err != nil {
					return judgement{}, gitlet.EngineWriteResult{}, true, err
				}
			}
		}
		switch {
		case v.reason == "" && j.absent == "":
			return j, gitlet.EngineWriteResult{}, false, nil
		case v.stale && (!asked || j.head != askedAt):
			// A moved head may be this write's own push, landed by an earlier
			// ask on any node: looked up (outcome) before it is called stale,
			// and again at the log's head only where the head moved since.
			asked, askedAt = true, j.head
			if res, _, found, err := wr.outcome(ctx); err != nil || found {
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
			note = " " + gitlet.NotRejudged(spent, "this turn's", "this seat's", s.now())
		}
		if j.absent != "" && v.reason == "" {
			return judgement{}, gitlet.EngineWriteResult{}, true, fmt.Errorf("%w: repository %s holds no object %s%s",
				gitlet.ErrObjectNotFound, wr.repoName(), j.absent, note)
		}
		return judgement{}, wr.refused(v.reason, v.detail+note), true, nil
	}
}

// resync draws a seat's write's one linearizable re-judgement from its seat's
// bucket on this node (Options.Resyncs, the query server's own; its turn the
// principal), nil where taken. A person's is never charged: it reads the log's
// head by right.
func (wr *writing) resync() *costlimit.Refusal {
	p := wr.w.Principal
	if p.Kind != gitlet.PrincipalAgent {
		return nil
	}
	return wr.s.o.Resyncs.Turn(p.Seat, p.Run.TurnID)
}

// rules judges the write's one command on the synced row, before any git
// runs: the repository's status, the stateless rules, the principal's grant,
// the protections and the default branch no write deletes; and signs a
// commit the write will make.
func (wr *writing) rules() (writeVerdict, error) {
	s, w := wr.s, wr.w
	repo := wr.synced.Repo
	if reason, words := s.statusRefusal(repo); reason != "" {
		return writeVerdict{reason: reason, detail: words}, nil
	}
	judged := wr.judged()
	if reason := gitlet.StatelessRefusal(judged); reason != "" {
		return writeVerdict{reason: reason, detail: reason.Sentence()}, nil
	}
	var roster gitlet.Roster
	if w.Principal.Kind == gitlet.PrincipalAgent {
		if roster = s.o.Roster(); roster == nil {
			return writeVerdict{}, wr.unready(gitlet.UnreadyNoChart, fmt.Sprintf("%s holds no chart yet to judge seat "+
				"%q's grant by; a Gitlet node holding one builds the write", s.o.NodeID, w.Principal.Seat))
		}
	}
	// The grant, held to whoever the turn works for (gitlet.WriteCeiling), is
	// judged here on this node's chart and the synced row, never on the
	// node the turn runs on.
	grant := gitlet.GrantOf(w.Principal, repo, roster)
	if w.Ceiling != nil {
		grant = grant.Under(w.Ceiling.Grant(repo, roster))
	}
	if reason, words := grant.RefRule(judged); reason != "" {
		return writeVerdict{reason: reason, detail: words}, nil
	}
	rules, err := gitlet.ParseProtections(repo.Protections)
	if err != nil {
		// A rule this build cannot read: asking again reads the same, and a
		// newer build's node may judge it.
		return writeVerdict{}, wr.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s cannot read a protection of "+
			"repository %s (%v); a node of a newer build judges it", s.o.NodeID, repo.Name, err))
	}
	if reason, rule := gitlet.ProtectionRefusal(judged, rules, w.Principal.Kind); reason != "" {
		return writeVerdict{reason: reason, detail: gitlet.ProtectionDetail(rule, reason)}, nil
	}
	if reason, words := gitlet.DefaultBranchRefusal(judged, repo.Name, repo.DefaultBranch); reason != "" {
		return writeVerdict{reason: reason, detail: words}, nil
	}
	if (w.Kind.Commits() || w.Annotates()) && wr.sig == (Signature{}) {
		sig, err := s.o.Signer.Sign(w.Principal)
		if err != nil {
			return writeVerdict{}, err
		}
		// A signature git would not write as given is the signer's bug, told
		// before the build rather than by commit-tree.
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

// placeholder is the new id a write's command is judged with before its commit
// exists: the grant and the protections read only which ref moves and how
// (create, update, delete), never to what, and a write never forces, since its
// commit sits on the head it read.
const placeholder = "1111111111111111111111111111111111111111"

// judged is the write's one command as the rules judge it.
func (wr *writing) judged() gitlet.Command {
	c := gitlet.Command{Ref: wr.w.Ref, Old: wr.cmd.Old, New: placeholder}
	switch wr.w.Kind {
	case gitlet.WriteDeleteBranch, gitlet.WriteDeleteTag:
		c.New = gitlet.ZeroOID
	case gitlet.WriteCreateBranch, gitlet.WriteCreateTag:
		c.New = wr.w.Commit
	}
	return c
}

// refused is the write's answer to a refusal of its push, nothing built: the
// command as the write would have made it, its New the target of a create and
// empty where nothing was built.
func (wr *writing) refused(reason gitlet.Refusal, detail string) gitlet.EngineWriteResult {
	c := wr.cmd
	switch wr.w.Kind {
	case gitlet.WriteCreateBranch, gitlet.WriteCreateTag:
		c.New = wr.w.Commit
	case gitlet.WriteDeleteBranch, gitlet.WriteDeleteTag:
		c.New = gitlet.ZeroOID
	}
	c.Result, c.Reason, c.Detail = gitlet.ResultNG, reason, detail
	return gitlet.EngineWriteResult{Command: c}
}

// outcome looks this write's own push up at the copy's position, within
// statelog.ReadBudget. Found, it is the write's answer from its row, beside
// every ref update the row names (a merge's source deletion, which the answer's
// one Command cannot say). An outcome that cannot be learned is answered busy,
// never stale: a write that may have landed must not be told that.
func (wr *writing) outcome(ctx context.Context) (gitlet.EngineWriteResult, []gitlet.Command, bool, error) {
	s := wr.s
	octx, cancel := context.WithTimeout(ctx, s.syncWait)
	defer cancel()
	out, err := s.o.Estate.PushOutcome(octx, gitlet.OutcomeQuery{RepoID: wr.w.RepoID, PushID: wr.pushID,
		At: wr.synced.Position})
	switch {
	case err == nil && out.Found && len(out.Push.Commands) > 0:
		c := out.Push.Commands[0]
		res := gitlet.EngineWriteResult{Outcome: statelog.OutcomeApplied, Command: c, Position: out.Push.Position}
		if c.New != gitlet.ZeroOID {
			res.Commit = c.New
		}
		return res, out.Push.Commands, true, nil
	case err == nil:
		return gitlet.EngineWriteResult{}, nil, false, nil
	case gitlet.AnswerDue(ctx):
		return gitlet.EngineWriteResult{}, nil, true, wr.unready(gitlet.UnreadyShare, fmt.Sprintf("%s could not "+
			"learn whether this write already landed before its answer fell due; another Gitlet node is asked",
			s.o.NodeID))
	case ctx.Err() != nil:
		return gitlet.EngineWriteResult{}, nil, true, ctx.Err()
	}
	s.log.Warn("code_write_outcome_unread", "repo", s.repoNameOf(wr.synced.Repo), "push_id", wr.pushID,
		"error", err)
	return wr.refused(gitlet.RefusalBusy, "could not learn whether this write already landed; make the same "+
		"call again"), nil, true, nil
}

// repoNameOf is the repository's name, or its id before any sync.
func (s *Server) repoNameOf(repo gitlet.RepoRef) string {
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

// buildShared builds under one unit of the node's seat git-work share, held for
// the build's git alone and given back before the landing, within one budget of
// git run time its processes share (gitlet.WriteBuildDeadline). Past
// gitlet.GitShareWait the share is unready, about this node; a build whose git
// runs the whole budget is too_large, since another node would take as long. A
// commit_files' read of its paths (building.paths), costing the square of a
// path's depth, is the build's, so it runs under the unit and the budget, never
// in the check. A branch's or a lightweight tag's create or delete builds
// nothing and takes no unit; an annotated tag builds its object.
func (wr *writing) buildShared(ctx context.Context, sc *cache.Scratch, j judgement) (built,
	gitlet.EngineWriteResult, bool, error) {

	s, w := wr.s, wr.w
	switch {
	case w.Kind == gitlet.WriteCreateBranch, w.Kind == gitlet.WriteCreateTag && !w.Annotates():
		return built{commit: w.Commit}, gitlet.EngineWriteResult{}, false, nil
	case w.Kind == gitlet.WriteDeleteBranch, w.Kind == gitlet.WriteDeleteTag:
		return built{commit: gitlet.ZeroOID}, gitlet.EngineWriteResult{}, false, nil
	}
	release, err := s.o.Share.Take(ctx, gitlet.SeatWorkWrite)
	if err != nil {
		if gitlet.AnswerDue(ctx) {
			err = wr.unready(gitlet.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole share until the "+
				"write's answer fell due, its asker's attempt spent; another Gitlet node builds it", s.o.NodeID))
		}
		return built{}, gitlet.EngineWriteResult{}, true, err
	}
	defer release()
	// From here every git process gets what is left of the budget and is
	// charged what it ran.
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
func (wr *writing) spent() gitlet.EngineWriteResult {
	return wr.changeRefused(gitlet.WriteTooLarge, fmt.Sprintf("building this change ran the whole %s of git one "+
		"%s is given on %s; make it in a checkout of the repository ("+toolname.CodeCheckout+")", wr.s.buildBudget(wr.w.Kind), wr.w.Kind,
		wr.s.o.NodeID))
}

// errBuildSpent is a build whose git ran the whole of its budget (its kind's,
// gitlet.EngineWriteKind.BuildDeadline): answered too_large.
var errBuildSpent = errors.New("host: the write's build ran its whole budget of git")

// changeRefused is the write's answer to a refusal of its change: nothing
// built, uploaded or proposed.
func (wr *writing) changeRefused(r gitlet.WriteRefusal, detail string) gitlet.EngineWriteResult {
	return gitlet.EngineWriteResult{Refusal: r, Detail: detail}
}

// deadlineRule refuses busy, with nothing uploaded, a write whose ctx cannot
// hold its bundle's upload (gitlet.UploadDeadline of its size, at most
// gitlet.WriteUploadAllowance) and the proposal after it
// (gitlet.MinProposeAttempt): a slow bucket costs a refusal, never a PUT nobody
// proposes. ctx ends when the answer is due (gitlet.AnswerBy), so what is left
// of it is what the landing has. A caller with no deadline holds any.
func (wr *writing) deadlineRule(ctx context.Context, sc *cache.Scratch) (gitlet.EngineWriteResult, bool, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return gitlet.EngineWriteResult{}, false, nil
	}
	need := gitlet.MinProposeAttempt
	packPath, checksum, held, err := sc.Pack()
	if err != nil {
		return gitlet.EngineWriteResult{}, true, err
	}
	if held {
		ref, err := gitlet.LayBundle(wr.pushID, packPath, checksum)
		if err != nil {
			return gitlet.EngineWriteResult{}, true, err
		}
		need += min(gitlet.UploadDeadline(ref.Size), gitlet.WriteUploadAllowance)
	}
	wr.landNeed = need
	if left := time.Until(deadline); left < need {
		return wr.refused(gitlet.RefusalBusy, fmt.Sprintf("this write's caller left %s for its upload and "+
			"proposal, under the %s they take; ask again", left.Round(time.Millisecond), need)), true, nil
	}
	return gitlet.EngineWriteResult{}, false, nil
}

// land hands the write to the landing every push takes (with gitlet.ViaEngine)
// and answers from what it came to, its outcome from the push's row only.
func (wr *writing) land(ctx context.Context, sc *cache.Scratch) (gitlet.EngineWriteResult, error) {
	s, w := wr.s, wr.w
	wr.landed = true
	landStart := s.now()
	l, err := s.land(ctx, landing{Synced: wr.synced, Scratch: sc, Commands: []gitlet.Command{wr.cmd},
		Change: wr.opens, Principal: w.Principal, WrittenFor: w.WrittenFor, Via: gitlet.ViaEngine, PushID: wr.pushID,
		ReceivedAt: s.now()})
	wr.landTook = s.now().Sub(landStart)
	if err != nil {
		return gitlet.EngineWriteResult{}, err
	}
	res := gitlet.EngineWriteResult{Command: l.Commands[0], Position: l.Result.position}
	if wr.opens != nil {
		res.Change = l.Change.answer
	}
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
		if w.Kind == gitlet.WriteDeleteBranch || w.Kind == gitlet.WriteDeleteTag {
			res.Commit = ""
		}
	}
	return res, nil
}

// record counts, times and logs (code_write) one write as it ended. It publishes
// no operator_acted: that record is the relay's, for a git push nothing else
// records; a person's tool call already has one from the act catalogue's
// dispatch, and a seat's write is its push row and its turn.
func (wr *writing) record(ctx context.Context, res gitlet.EngineWriteResult, err error) {
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
	// Who wrote, as code_push names a pusher: the seat the commit is authored
	// for, beside the credential a person came through (iam.ActorFor: one
	// person, many credentials) or the turn behind a seat's.
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
func writeOutcome(res gitlet.EngineWriteResult, err error) string {
	var unready *gitlet.ErrGitUnready
	switch {
	case errors.As(err, &unready):
		return writeUnready
	case errors.Is(err, gitlet.ErrInvalid), errors.Is(err, gitlet.ErrObjectNotFound):
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

// shortID is an id's first seven digits, as a sentence names a commit.
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
	case w.ExpectedHead == "" && head != "" && w.Kind.Tags():
		return fmt.Sprintf("%s already exists, at %s, and a tag never moves; name another tag, or delete it "+
			"first", w.Ref, shortID(head))
	case w.ExpectedHead == "" && head != "":
		return fmt.Sprintf("%s already exists, at %s; read it with %s and write onto its head", w.Ref,
			shortID(head), toolname.CodeResolve)
	case head == "":
		return fmt.Sprintf("%s does not exist; read it again with %s", w.Ref, toolname.CodeResolve)
	}
	return fmt.Sprintf("%s is at %s, not the %s you read; read it again with %s", w.Ref, shortID(head),
		shortID(w.ExpectedHead), toolname.CodeResolve)
}
