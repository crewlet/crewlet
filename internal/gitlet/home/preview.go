package home

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// PreviewOptions build a [Previews] from what a read by object id is served
// from (internal/gitlet/query's Options), since a preview is one. NewPreviews
// refuses any field but Current, Metrics, Logger and Now missing, by name.
type PreviewOptions struct {
	// NodeID is this node, which a refusal about the node names.
	NodeID string
	// Cache is the node's git cache and Git its pool, as the home's merges use
	// them; the cache's memo keeps a decided preview, keyed by the pool's git
	// version.
	Cache PreviewCache
	Git   PreviewPool
	// Share is the node's ONE seat git-work share (gitlet.NewGitShare over the
	// pool's read lane). Each preview's merge holds a unit, as a read by object
	// id's git does, so seat reads, engine writes' builds and previews together
	// never hold more of the read lane than the share.
	Share *gitlet.GitShare
	// Roster is the serving node's CURRENT epoch's chart, which a seat's grant
	// is judged by, read per preview; nil holds none and judges no seat.
	// Current reports whether the node serves the company's configuration; nil
	// is current.
	Roster  func() gitlet.Roster
	Current func() bool
	// Fetched records a preview of a `visibility: unit` repository, as git
	// reads record theirs: the merge names the files two commits conflict in,
	// a read of the repository. It returns at once (the engine files it in the
	// node's reads ledger).
	Fetched func(ctx context.Context, e types.CodeFetched)
	// Metrics nil records nothing; Logger nil is the package's own; Now nil
	// is time.Now.
	Metrics *metrics.Recorder
	Logger  *slog.Logger
	Now     func() time.Time
}

// PreviewCache is the node's git cache as a preview uses it (internal/gitlet/
// cache's *Cache satisfies it): the home merges' [Cache], plus the pinned
// copy's MEMO, which answers a repeat preview of the same two commits
// (MemoGet) and keeps a decided one (Memo).
type PreviewCache interface {
	Cache
	MemoGet(s cache.Synced, k cache.MemoKey) ([]byte, bool)
	Memo(s cache.Synced) *cache.MemoBatch
}

// PreviewPool runs a preview's git — internal/gitlet/gitproc's *Pool satisfies
// it — and says which git it runs, which a memoized preview is keyed by.
type PreviewPool interface {
	Pool
	Version() gitproc.Version
}

// Previews answers a Gitlet node's MERGE PREVIEWS ([gitlet.MergePreviewQuery])
// by THE SAME MERGE the home loop records mergeability by ([mergeIn]), served
// beside the loop as a read by object id is (package doc, "Merge previews,
// beside the loop"), taking query's Server.Read steps: validate; refuse a
// seat's preview on a node behind its configuration or holding no chart;
// answer not_held where it neither holds nor fills a copy (ADR-0089); sync to
// the asker's floor; judge deletion and grant (gitlet.JudgeRead); answer from
// the copy's memo ([previewing.previewMemoKey]); find both commits; merge in
// a scratch ([previewing.mergeTimed]); record a `visibility: unit` read
// ([PreviewReadKind]). No refusal is concluded from a copy behind the log's
// head ([previewing.merge]). It runs no goroutine: each preview runs on its
// caller's, ending with the caller's context or [Previews.Stop].
type Previews struct {
	o   PreviewOptions
	log *slog.Logger
	now func() time.Time
	// syncWait bounds a preview's sync and gitBudget its merge's git: a read's
	// own statelog.ReadBudget and gitproc.ReadDeadline.
	syncWait, gitBudget time.Duration

	// life ends at Stop, cutting every preview in flight; inflight counts
	// them, and stopped refuses later ones.
	life     context.Context
	end      context.CancelFunc
	mu       sync.Mutex
	stopped  bool
	inflight sync.WaitGroup
}

// NewPreviews builds a Previews over o, refusing every required field
// missing, by name, in one error.
func NewPreviews(o PreviewOptions) (*Previews, error) {
	var missing []string
	for _, f := range []struct {
		absent bool
		name   string
	}{
		{o.NodeID == "", "NodeID"}, {o.Cache == nil, "Cache"}, {o.Git == nil, "Git"}, {o.Share == nil, "Share"},
		{o.Roster == nil, "Roster"}, {o.Fetched == nil, "Fetched"},
	} {
		if f.absent {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s missing", ErrOptions, strings.Join(missing, ", "))
	}
	p := &Previews{o: o, log: o.Logger, now: o.Now, syncWait: statelog.ReadBudget, gitBudget: gitproc.ReadDeadline}
	if p.log == nil {
		p.log = logging.Get("gitlet.home")
	}
	if p.now == nil {
		p.now = time.Now
	}
	// Ended by Stop alone: each preview runs on its own caller's context.
	p.life, p.end = context.WithCancel(context.Background())
	return p, nil
}

// errPreviewsStopped is the cause a preview's context ends with when Stop
// cuts it: never a preview's answer, which is [Previews.stopping] instead.
var errPreviewsStopped = errors.New("home: this node's merge previews are stopping")

// Stop refuses every later preview and CUTS every one in flight, each answered
// unready about this node, then waits for them or for ctx. Whoever stops
// Gitlet calls it with the git reads and BEFORE the cache. Idempotent.
//
// Cut rather than waited out, as the git reads are: a preview may run its
// asker's whole attempt, past the stop's budget, and one cut while its asker
// listens sends the asker to the runner-up at once.
func (p *Previews) Stop(ctx context.Context) {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.end()
	waited := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		p.log.WarnContext(ctx, "code_merge_previews_stop_unfinished", "node", p.o.NodeID, "error",
			context.Cause(ctx), "detail", "merge previews were still answering when the stop's wait ended")
	}
}

// stopping answers a preview the stop refused or cut: node_failed, so the walk
// asks the next Gitlet node and lets it fill a copy
// (gitlet.UnreadyReason.PassesFill).
func (p *Previews) stopping() error {
	return &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNodeFailed, Detail: fmt.Sprintf("%s is stopping its merge "+
		"previews; another Gitlet node answers, and fills a copy where none holds one", p.o.NodeID)}
}

// MergePreview answers what merging q.Head into q.Base would do; see
// [Previews]. ctx ENDS WHEN THE ANSWER IS DUE (gitlet.AnswerBy): a wait or
// merge it ends answers gitlet.ErrGitUnready while the asker still listens,
// never ctx's error, which the walk would take for the repository's answer.
func (p *Previews) MergePreview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	if err := q.Validate(); err != nil {
		return gitlet.MergePreviewAnswer{}, err
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return gitlet.MergePreviewAnswer{}, p.stopping()
	}
	p.inflight.Add(1)
	p.mu.Unlock()
	defer p.inflight.Done()
	pctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	//nolint:contextcheck // The previews' lifetime cuts a preview's work too: Stop must reach every merge in flight.
	cut := context.AfterFunc(p.life, func() { cancel(errPreviewsStopped) })
	defer cut()
	a, err := p.preview(pctx, q)
	if err != nil && errors.Is(context.Cause(pctx), errPreviewsStopped) {
		return gitlet.MergePreviewAnswer{}, p.stopping()
	}
	return a, err
}

// previewing is one preview in progress: the chart a seat is judged by, read
// once, and the copy it reads, pinned — linear where it was read at the log's
// head.
type previewing struct {
	p      *Previews
	q      gitlet.MergePreviewQuery
	roster gitlet.Roster
	synced cache.Synced
	linear bool
}

// preview is MergePreview's work, under the context Stop cuts.
func (p *Previews) preview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	pv := &previewing{p: p, q: q}
	if err := pv.judgeable(); err != nil {
		return gitlet.MergePreviewAnswer{}, err
	}
	if !q.Home && !p.o.Cache.Holds(q.RepoID) {
		return gitlet.MergePreviewAnswer{}, pv.unready(gitlet.UnreadyNotHeld, fmt.Sprintf("%s holds no copy of "+
			"repository %s and the preview is not its walk's home's, which is the one node that fills a copy; "+
			"a Gitlet node holding one answers meanwhile", p.o.NodeID, q.RepoID))
	}
	// The pin is released whatever the preview answers: every Synced a sync
	// or re-sync hands out goes back through pv.synced.
	defer func() { pv.synced.Release() }()
	if err := pv.sync(ctx, pv.fresh()); err != nil {
		return gitlet.MergePreviewAnswer{}, err
	}
	m, err := pv.merge(ctx)
	if err != nil {
		return gitlet.MergePreviewAnswer{}, err
	}
	a := gitlet.MergePreviewAnswer{RepoID: q.RepoID, Base: q.Base, Head: q.Head, State: m.state,
		Conflicts: m.conflicts.Paths, ConflictsOmitted: m.conflicts.Omitted, Position: pv.synced.Position}
	if err := pv.audit(ctx, a); err != nil {
		return gitlet.MergePreviewAnswer{}, err
	}
	return a, nil
}

// judgeable refuses a seat's preview, before anything starts, on a node that
// cannot judge a seat, and reads the chart once where it can.
func (pv *previewing) judgeable() error {
	if pv.q.Asker.Kind != gitlet.PrincipalAgent {
		return nil
	}
	p := pv.p
	if cur := p.o.Current; cur != nil && !cur() {
		return pv.unready(gitlet.UnreadyBehindConfig, fmt.Sprintf("%s is behind the company's configuration and "+
			"judges no seat's grant from the chart the fleet replaced; a current Gitlet node answers", p.o.NodeID))
	}
	if pv.roster = p.o.Roster(); pv.roster == nil {
		return pv.unready(gitlet.UnreadyNoChart, fmt.Sprintf("%s holds no chart yet to judge seat %q's grant by; "+
			"a Gitlet node holding one answers", p.o.NodeID, pv.q.Asker.Seat))
	}
	return nil
}

// fresh is what the first sync asks: the asker's floor where it names one, at
// its level (session where it names none), else the copy as it stands. Two ids
// name the same commits at every position; a floor buys only the asker's own
// pushes, and [previewing.merge]'s second look covers every other id.
func (pv *previewing) fresh() statelog.Freshness {
	f := pv.q.Fresh
	floor := f.Floor(topics.CodeLogStream)
	if floor.IsZero() {
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	f.Floors = []statelog.Position{floor}
	if f.Level == "" {
		f.Level = statelog.ReadSession
	}
	return f
}

// sync pins the copy at fresh in place of any the preview holds, waiting at
// most statelog.ReadBudget and ctx, and answers as a read's sync does.
func (pv *previewing) sync(ctx context.Context, fresh statelog.Freshness) error {
	p := pv.p
	sctx, cancel := context.WithTimeout(ctx, p.syncWait)
	defer cancel()
	s, err := p.o.Cache.Sync(sctx, pv.q.RepoID, fresh)
	switch {
	case err == nil:
	case errors.Is(err, gitlet.ErrRepoNotFound):
		return fmt.Errorf("home: repository %s: %w", pv.q.RepoID, err)
	case errors.Is(err, cache.ErrInvalid):
		return fmt.Errorf("%w: fresh: %w", gitlet.ErrInvalid, err)
	case gitlet.AnswerDue(ctx):
		return pv.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the preview's need before its answer fell due; the sync goes on, and another Gitlet node answers "+
			"meanwhile", p.o.NodeID, pv.q.RepoID))
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case sctx.Err() != nil:
		return pv.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the preview's need within %s; the sync goes on, and another Gitlet node answers meanwhile",
			p.o.NodeID, pv.q.RepoID, p.syncWait))
	case cache.EveryFillMeets(err):
		return pv.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s could not sync repository %s: %v — what every "+
			"Gitlet node's copy is filled from, so a Gitlet node holding a copy answers meanwhile", p.o.NodeID,
			pv.q.RepoID, err))
	default:
		return pv.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not sync repository %s: %v; another "+
			"Gitlet node answers, and fills a copy where none holds one", p.o.NodeID, pv.q.RepoID, err))
	}
	pv.synced.Release()
	pv.synced, pv.linear = s, fresh.Level == statelog.ReadLinearizable
	return nil
}

// merge is mergeOnce over the pinned copy and, where the copy may be behind the
// log, ONE second look at the log's head before a refusal: a later activation
// may have restored the repository or opened the grant, and the asker may know
// a commit this node has not synced.
func (pv *previewing) merge(ctx context.Context) (merged, error) {
	for {
		m, done, err := pv.mergeOnce(ctx)
		if done || pv.linear || err == nil {
			return m, err
		}
		if err := pv.sync(ctx, statelog.Freshness{Level: statelog.ReadLinearizable}); err != nil {
			return merged{}, err
		}
	}
}

// mergeOnce is one look at the pinned copy: done where its answer stands at any
// copy position (the merge, or an error about this node), not done for a
// refusal a copy behind the log's head must not conclude: the repository gone,
// the grant refused, a commit absent.
func (pv *previewing) mergeOnce(ctx context.Context) (merged, bool, error) {
	p, q := pv.p, pv.q
	repo := pv.synced.Repo
	if repo.Status == gitlet.RepoStatusDeleted || repo.Status == gitlet.RepoStatusPurging {
		gone := gitlet.RepoGone{ID: repo.ID, Name: repo.Name, Status: repo.Status, DeletedAt: repo.DeletedAt,
			ManagedBy: repo.ManagedBy}
		return merged{}, false, fmt.Errorf("%w: %s", gitlet.ErrRepoNotFound, gone.Detail())
	}
	if _, err := gitlet.JudgeRead(q.Asker, q.CeilingSeat, repo, pv.roster); err != nil {
		return merged{}, false, err
	}
	// The memo only after the grant has judged the preview, and before any
	// git runs ([previewing.previewMemoKey]).
	if m, ok := pv.recall(); ok {
		return m, true, nil
	}
	// A scratch of its own under a fresh id, never the copy: the merge's
	// writes leave with it, and no two previews, nor a preview and the home's
	// own merge of the same commits, share one.
	sc, err := p.o.Cache.NewScratch(ctx, pv.synced, statelog.NewOpID(p.now(), ""))
	if err != nil {
		return merged{}, true, pv.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not make a scratch to "+
			"merge in: %v; another Gitlet node answers", p.o.NodeID, err))
	}
	defer sc.Remove()
	if absent, err := pv.commits(ctx, sc); err != nil || absent != "" { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err == nil {
			err = fmt.Errorf("%w: repository %s holds no commit %s", gitlet.ErrObjectNotFound, repo.Name, absent)
		}
		return merged{}, !errors.Is(err, gitlet.ErrObjectNotFound), err
	}
	release, err := p.o.Share.Take(ctx, gitlet.SeatWorkRead)
	if err != nil {
		if gitlet.AnswerDue(ctx) {
			return merged{}, true, pv.unready(gitlet.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole "+
				"share until the preview's answer fell due, its asker's attempt spent; another Gitlet node answers "+
				"meanwhile", p.o.NodeID))
		}
		return merged{}, true, err
	}
	defer release()
	m, err := pv.mergeTimed(ctx, sc)
	if err != nil && gitlet.AnswerDue(ctx) {
		return merged{}, true, pv.unready(gitlet.UnreadyShare, fmt.Sprintf("%s could not merge before the "+
			"preview's answer fell due, its asker's attempt spent; another Gitlet node answers meanwhile",
			p.o.NodeID))
	}
	if err == nil {
		pv.memoize(m)
	}
	return m, true, err
}

// previewMemoFormat names a memoized preview's shape (a previewMemo as JSON)
// and leads its key, so an entry of another shape, such as a read's page
// (internal/gitlet/query's format), is never read as one.
const previewMemoFormat = "merge_preview/1"

// previewMemo is a decided preview as the memo keeps it: what [mergeIn]
// answered, nothing the node or the moment stamps on it; the repository and
// the copy's position are the preview's own.
type previewMemo struct {
	State            gitlet.MergeState `json:"state"`
	Conflicts        []string          `json:"conflicts,omitempty"`
	ConflictsOmitted int64             `json:"conflicts_omitted,omitempty"`
}

// previewMemoKey keys a preview on the pinned copy's memo (cache.NewMemoKey):
// its format, the git version that merged (another git build may merge
// otherwise), and the two whole ids, base then head.
//
// A FUNCTION OF THE TWO IDS ALONE, so never stale (internal/gitlet/cache's
// memo.go): an id names one object for ever, which fixes the merge base, and
// the memo is kept only where the merge found both commits in this copy's
// incarnation, which loses an object only with its history, dropping the memo
// whole. So a hit runs no git, nor asks the copy again whether it holds them.
func (pv *previewing) previewMemoKey() cache.MemoKey {
	return cache.NewMemoKey(previewMemoFormat, pv.p.o.Git.Version().String(), pv.q.Base, pv.q.Head)
}

// recall is the memo's preview of the two commits on the pinned copy, or
// none: no entry, or one this build cannot read, which the merge then answers
// and memoizes over.
func (pv *previewing) recall() (merged, bool) {
	body, ok := pv.p.o.Cache.MemoGet(pv.synced, pv.previewMemoKey())
	if !ok {
		return merged{}, false
	}
	var e previewMemo
	if err := json.Unmarshal(body, &e); err != nil || !e.State.Decided() {
		return merged{}, false
	}
	return merged{state: e.State, conflicts: gitlet.ConflictsHeld{Paths: e.Conflicts,
		Omitted: e.ConflictsOmitted}}, true
}

// memoize keeps m in the pinned copy's memo, a DECIDED merge only: `unknown`
// (out of time) is the node's moment, not the commits', and may finish when
// asked again. A memo that cannot be written slows the next preview, never
// fails it.
func (pv *previewing) memoize(m merged) {
	if !m.state.Decided() {
		return
	}
	body, err := json.Marshal(previewMemo{State: m.state, Conflicts: m.conflicts.Paths,
		ConflictsOmitted: m.conflicts.Omitted})
	if err != nil {
		return
	}
	b := pv.p.o.Cache.Memo(pv.synced)
	if b == nil {
		return
	}
	b.Put(pv.previewMemoKey(), body)
	b.Place(func(int) []byte { return nil })
}

// mergeTimed merges ([mergeIn]) the preview's two commits in sc, in the read
// lane within one read's git (gitproc.ReadDeadline), so it answers inside a git
// read's attempt (estate.GitReadAttempt); out of time it answers `unknown`, as
// the home records an undecided merge. crewlet.code.home.merge.duration under
// `work` preview observes the merge-tree ALONE: a preview refused before it is
// a read's refusal, no merge, and its sync and share wait are the cache's and
// the share's. `failed` is a merge-tree that failed or whose asker's attempt
// ended first.
func (pv *previewing) mergeTimed(ctx context.Context, sc *cache.Scratch) (merged, error) {
	p := pv.p
	began := p.now()
	m, err := mergeIn(ctx, p.o.Git, sc, pv.q.Base, pv.q.Head, gitproc.ClassRead, p.gitBudget)
	if p.o.Metrics != nil {
		outcome := mergeFailed
		if err == nil {
			outcome = string(m.state)
		}
		p.o.Metrics.Observe(metrics.CodeHomeMergeDuration, p.now().Sub(began),
			metrics.Attrs{"work": workPreview, "outcome": outcome})
	}
	return m, err
}

// commits looks up the preview's two ids with ONE `cat-file --batch-check` in
// the scratch (which reads the copy through its alternate), answering the
// first the copy lacks, or "" where it holds both as commits; an id of a tree,
// blob or tag is refused gitlet.ErrInvalid, naming it.
func (pv *previewing) commits(ctx context.Context, sc *cache.Scratch) (string, error) {
	q, p := pv.q, pv.p
	var out bytes.Buffer
	_, err := p.o.Git.Run(ctx, gitproc.Cmd{Dir: sc.Dir, Alternate: sc.Alternate,
		Args:  []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"},
		Stdin: strings.NewReader(q.Base + "\n" + q.Head + "\n"), Stdout: &out,
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil {
		if gitlet.AnswerDue(ctx) {
			return "", pv.unready(gitlet.UnreadyShare, fmt.Sprintf("%s could not look up the preview's commits "+
				"before its answer fell due; another Gitlet node answers meanwhile", p.o.NodeID))
		}
		return "", pv.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not look up the preview's commits: "+
			"%v; another Gitlet node answers", p.o.NodeID, err))
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	for i, id := range []struct{ field, id string }{{"base", q.Base}, {"head", q.Head}} {
		if i >= len(lines) {
			return "", fmt.Errorf("home: cat-file answered %d of the preview's 2 ids", len(lines))
		}
		name, kind, _ := strings.Cut(lines[i], " ")
		switch {
		case name != id.id:
			return "", fmt.Errorf("home: cat-file answered %q for %s", lines[i], id.id)
		case kind == "missing":
			return id.id, nil
		case kind != "commit":
			return "", fmt.Errorf("%w: %s: %s is a %s, and a preview merges two commits", gitlet.ErrInvalid,
				id.field, id.id, kind)
		}
	}
	return "", nil
}

// audit records a preview of a `visibility: unit` repository as its asker's
// read (one read, the answer's encoding its bytes) before it is answered.
func (pv *previewing) audit(ctx context.Context, a gitlet.MergePreviewAnswer) error {
	repo := pv.synced.Repo
	if repo.Visibility != gitlet.VisibilityUnit {
		return nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("home: measure the preview of repository %s: %w", repo.Name, err)
	}
	reader := pv.q.Asker.OperatorID
	if pv.q.Asker.Kind == gitlet.PrincipalAgent {
		reader = pv.q.Asker.Seat
	}
	pv.p.o.Fetched(ctx, types.CodeFetched{RepoID: repo.ID, RepoName: repo.Name,
		PrincipalKind: types.CodeReaderKind(pv.q.Asker.Kind), Principal: reader, Seat: pv.q.Asker.Seat,
		Via: types.FetchedViaTool, Kind: PreviewReadKind, Reads: 1, Bytes: int64(len(encoded))})
	return nil
}

// PreviewReadKind is the kind a merge preview of a `visibility: unit`
// repository is recorded under among its readers' reads (types.CodeFetched),
// beside the git reads' own kinds.
const PreviewReadKind = "merge_preview"

// unready is a refusal about this node, which another Gitlet node may answer.
func (pv *previewing) unready(why gitlet.UnreadyReason, detail string) error {
	return &gitlet.ErrGitUnready{Reason: why, Detail: detail}
}
