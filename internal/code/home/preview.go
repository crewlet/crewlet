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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// PreviewOptions build a [Previews]: exactly what a read by object id is
// served from (internal/code/query's Options), since a preview is one. Every
// field but Current, Metrics, Logger and Now is required, and NewPreviews
// refuses a missing one by name.
type PreviewOptions struct {
	// NodeID is this node, which a refusal about the node names.
	NodeID string
	// Cache is the node's git cache and Git its pool, as the home's own
	// merges use them — and the cache's memo, which a decided preview is
	// kept in, and the pool's git version, which keys it.
	Cache PreviewCache
	Git   PreviewPool
	// Share is the node's ONE seat git-work share (code.NewGitShare over the
	// pool's read lane), a unit of which every preview's merge holds, as a
	// read by object id's git does: the node's seat reads, its engine
	// writes' builds and its previews between them never hold more of the
	// read lane than the share.
	Share *code.GitShare
	// Roster is the chart a seat's grant is judged by, the serving node's
	// CURRENT epoch's, read per preview — nil for a node holding none, which
	// judges no seat — and Current whether the node serves the company's
	// configuration, nil being current.
	Roster  func() code.Roster
	Current func() bool
	// Fetched records a preview this node answered of a `visibility: unit`
	// repository, as the git reads record theirs: the merge names the files
	// two commits conflict in, which is a read of the repository. It returns
	// at once (the engine files it in the node's reads ledger).
	Fetched func(ctx context.Context, e types.CodeFetched)
	// Metrics nil records nothing; Logger nil is the package's own; Now nil
	// is time.Now.
	Metrics *metrics.Recorder
	Logger  *slog.Logger
	Now     func() time.Time
}

// PreviewCache is the node's git cache as a preview reaches it —
// internal/code/cache's *Cache satisfies it: what the home's merges use
// ([Cache]), and the pinned copy's MEMO, which a decided preview is answered
// from the next time the same two commits are previewed (MemoGet), and kept
// in once it is merged (Memo).
type PreviewCache interface {
	Cache
	MemoGet(s cache.Synced, k cache.MemoKey) ([]byte, bool)
	Memo(s cache.Synced) *cache.MemoBatch
}

// PreviewPool runs a preview's git — internal/code/gitproc's *Pool satisfies
// it — and says which git it runs, which a memoized preview is keyed by.
type PreviewPool interface {
	Pool
	Version() gitproc.Version
}

// Previews answers a git node's MERGE PREVIEWS — what merging one commit into
// another would do, a comparison's preview ([code.MergePreviewQuery]) — by
// THE SAME MERGE the home loop records a change's mergeability by
// ([mergeIn]), so a preview and the change it would open say one thing: the
// state, and the conflicted paths a record would hold.
//
// A SERVER OF READS BESIDE THE LOOP, NEVER PART OF IT: a preview is a READ a
// principal makes, served from exactly what a read by object id is served
// from — the cache, the pool, the node's one seat git-work share, the chart
// and the reads ledger (internal/code/query) — on every git node a walk asks,
// whether or not the node runs home work. The loop's estate, placement and
// nudges answer nothing a preview asks, so every git node's composition
// builds a Previews beside its query server, and the loop and the previews
// share the merge alone.
//
// A preview is answered as a read by object id is (query's Server.Read,
// whose steps it takes):
//
//  1. q is validated (code.MergePreviewQuery.Validate).
//  2. A SEAT'S preview is refused about this node — code.ErrGitUnready —
//     where the node is confirmed behind the company's configuration
//     (behind_config) or holds no chart (no_chart), before anything starts.
//  3. A node holding no copy of the repository, nor filling one, answers a
//     preview that is not its walk's effective home's unready (not_held) at
//     once: one node a walk fills a copy (ADR-0059).
//  4. The copy is synced to the asker's floor, or read as it stands where
//     the asker names none, the wait bounded by statelog.ReadBudget, and a
//     sync that cannot be answered in time is unready as a read's is.
//  5. A deleted or purging repository is code.ErrRepoNotFound, and the
//     asker's grant — and the grant of the seat it reads for — is judged
//     from the copy's row and this node's chart (code.JudgeRead). A preview
//     of two commits this copy has merged before is then answered from the
//     copy's MEMO with no git run — no scratch, no cat-file, no unit of the
//     share — as a read by object id's page is (internal/code/cache's
//     memo.go): a merge is a function of the two ids and the git that ran
//     it, which key it ([previewMemoKey]). Step 8 records it all the same.
//  6. Both commits are found in the copy with ONE `cat-file --batch-check`,
//     a commit it lacks being code.ErrObjectNotFound and an id of another
//     kind code.ErrInvalid. No refusal of steps 5 and 6 is concluded from a
//     copy not read at the log's head: it is synced there once, and judged
//     again.
//  7. The merge runs in a SCRATCH of its own, never the copy, in the READ
//     lane under ONE unit of the node's seat git-work share and within one
//     read's git (gitproc.ReadDeadline) — so a preview is answered inside a
//     git read's attempt (estate.GitReadAttempt) — a merge that runs out of
//     it answering `unknown`, as the home records an undecided merge. A
//     DECIDED merge is kept in the copy's memo for step 5's next look; an
//     `unknown` one never, since asked again it may finish.
//  8. A preview answered of a `visibility: unit` repository is recorded as
//     its asker's read ([PreviewReadKind]).
//
// It holds no goroutine of its own: each preview runs on its caller's, and
// ends with its caller's context or with [Previews.Stop].
type Previews struct {
	o   PreviewOptions
	log *slog.Logger
	now func() time.Time
	// syncWait bounds a preview's wait on its sync, and gitBudget its
	// merge's git: statelog.ReadBudget and gitproc.ReadDeadline, a read's
	// own.
	syncWait, gitBudget time.Duration

	// life is the previews' lifetime, ended by Stop, which cuts every
	// preview in flight; inflight counts them, and stopped refuses the
	// previews after it.
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
		p.log = logging.Get("code.home")
	}
	if p.now == nil {
		p.now = time.Now
	}
	// THE PREVIEWS' OWN LIFETIME, ended by their stop alone: no caller's
	// context is theirs, since every preview runs on its own.
	p.life, p.end = context.WithCancel(context.Background())
	return p, nil
}

// errPreviewsStopped is the cause a preview's context ends with when Stop
// cuts it: never a preview's answer, which is [Previews.stopping] instead.
var errPreviewsStopped = errors.New("home: this node's merge previews are stopping")

// Stop refuses every preview from now on — unready, about this node — and
// CUTS every preview in flight, each answered the same, and waits for them,
// or for ctx. Called by whoever stops the code host, with the git reads and
// BEFORE the cache a preview reads. Idempotent.
//
// CUT RATHER THAN WAITED OUT, as the git reads are at their stop: a preview
// runs up to its asker's whole attempt, longer than the stop's budget, and a
// preview cut is answered while its asker still listens, which takes it to
// the runner-up at once.
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

// stopping is what a preview the stop refused or cut is answered: unready
// about this node, node_failed, so the walk asks the next git node and lets
// it fill a copy (code.UnreadyReason.PassesFill).
func (p *Previews) stopping() error {
	return &code.ErrGitUnready{Reason: code.UnreadyNodeFailed, Detail: fmt.Sprintf("%s is stopping its merge "+
		"previews; another git node answers, and fills a copy where none holds one", p.o.NodeID)}
}

// MergePreview answers what merging q.Head into q.Base would do — see
// [Previews]. ctx ENDS WHEN THE ANSWER IS DUE (code.AnswerBy): a wait or a
// merge it ends answers code.ErrGitUnready while the asker still listens,
// never ctx's own error, which the walk would take for the repository's
// answer.
func (p *Previews) MergePreview(ctx context.Context, q code.MergePreviewQuery) (code.MergePreviewAnswer, error) {
	if err := q.Validate(); err != nil {
		return code.MergePreviewAnswer{}, err
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return code.MergePreviewAnswer{}, p.stopping()
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
		return code.MergePreviewAnswer{}, p.stopping()
	}
	return a, err
}

// previewing is one preview in progress: the chart a seat is judged by, read
// once, and the copy it reads, pinned — linear where it was read at the log's
// head.
type previewing struct {
	p      *Previews
	q      code.MergePreviewQuery
	roster code.Roster
	synced cache.Synced
	linear bool
}

// preview is MergePreview's work, under the context Stop cuts.
func (p *Previews) preview(ctx context.Context, q code.MergePreviewQuery) (code.MergePreviewAnswer, error) {
	pv := &previewing{p: p, q: q}
	if err := pv.judgeable(); err != nil {
		return code.MergePreviewAnswer{}, err
	}
	if !q.Home && !p.o.Cache.Holds(q.RepoID) {
		return code.MergePreviewAnswer{}, pv.unready(code.UnreadyNotHeld, fmt.Sprintf("%s holds no copy of "+
			"repository %s and the preview is not its walk's home's, which is the one node that fills a copy; "+
			"a git node holding one answers meanwhile", p.o.NodeID, q.RepoID))
	}
	// THE PIN IS THE PREVIEW'S, released whatever it answers: every Synced a
	// sync or a re-sync handed out goes back through pv.synced.
	defer func() { pv.synced.Release() }()
	if err := pv.sync(ctx, pv.fresh()); err != nil {
		return code.MergePreviewAnswer{}, err
	}
	m, err := pv.merge(ctx)
	if err != nil {
		return code.MergePreviewAnswer{}, err
	}
	a := code.MergePreviewAnswer{RepoID: q.RepoID, Base: q.Base, Head: q.Head, State: m.state,
		Conflicts: m.conflicts.Paths, ConflictsOmitted: m.conflicts.Omitted, Position: pv.synced.Position}
	if err := pv.audit(ctx, a); err != nil {
		return code.MergePreviewAnswer{}, err
	}
	return a, nil
}

// judgeable is step 2: a seat's preview refused on a node that cannot judge
// a seat, its chart read once where it can.
func (pv *previewing) judgeable() error {
	if pv.q.Asker.Kind != code.PrincipalAgent {
		return nil
	}
	p := pv.p
	if cur := p.o.Current; cur != nil && !cur() {
		return pv.unready(code.UnreadyBehindConfig, fmt.Sprintf("%s is behind the company's configuration and "+
			"judges no seat's grant from the chart the fleet replaced; a current git node answers", p.o.NodeID))
	}
	if pv.roster = p.o.Roster(); pv.roster == nil {
		return pv.unready(code.UnreadyNoChart, fmt.Sprintf("%s holds no chart yet to judge seat %q's grant by; "+
			"a git node holding one answers", p.o.NodeID, pv.q.Asker.Seat))
	}
	return nil
}

// fresh is what the first sync asks: the asker's floor where it names one —
// at the level it names, session where it names none — and otherwise the copy
// as it stands: two ids name two commits at every position, and what a floor
// buys is a copy holding the asker's own pushes, which step 6's second look
// covers for every other id.
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

// sync is step 4: the copy pinned at fresh in place of any the preview holds,
// its wait bounded by statelog.ReadBudget and by ctx — answered as a read's
// sync is.
func (pv *previewing) sync(ctx context.Context, fresh statelog.Freshness) error {
	p := pv.p
	sctx, cancel := context.WithTimeout(ctx, p.syncWait)
	defer cancel()
	s, err := p.o.Cache.Sync(sctx, pv.q.RepoID, fresh)
	switch {
	case err == nil:
	case errors.Is(err, code.ErrRepoNotFound):
		return fmt.Errorf("home: repository %s: %w", pv.q.RepoID, err)
	case errors.Is(err, cache.ErrInvalid):
		return fmt.Errorf("%w: fresh: %v", code.ErrInvalid, err)
	case code.AnswerDue(ctx):
		return pv.unready(code.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the preview's need before its answer fell due; the sync goes on, and another git node answers "+
			"meanwhile", p.o.NodeID, pv.q.RepoID))
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case sctx.Err() != nil:
		return pv.unready(code.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the preview's need within %s; the sync goes on, and another git node answers meanwhile",
			p.o.NodeID, pv.q.RepoID, p.syncWait))
	case cache.EveryFillMeets(err):
		return pv.unready(code.UnreadySyncing, fmt.Sprintf("%s could not sync repository %s: %v — what every "+
			"git node's copy is filled from, so a git node holding a copy answers meanwhile", p.o.NodeID,
			pv.q.RepoID, err))
	default:
		return pv.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not sync repository %s: %v; another "+
			"git node answers, and fills a copy where none holds one", p.o.NodeID, pv.q.RepoID, err))
	}
	pv.synced.Release()
	pv.synced, pv.linear = s, fresh.Level == statelog.ReadLinearizable
	return nil
}

// merge is steps 5 to 7 over the pinned copy and, where the copy may be behind
// the log, ONE second look at the log's head before a refusal: a repository a
// later activation restored, a grant it opened, or a commit the asker knows of
// and this node has not synced.
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

// mergeOnce is one look at the pinned copy: its answer, done where it is the
// preview's answer whatever the copy's position — the merge, or an error about
// this node — and not done for a refusal a copy not read at the log's head
// must not conclude: the repository gone, the grant refused, a commit absent.
func (pv *previewing) mergeOnce(ctx context.Context) (merged, bool, error) {
	p, q := pv.p, pv.q
	repo := pv.synced.Repo
	if repo.Status == code.RepoStatusDeleted || repo.Status == code.RepoStatusPurging {
		gone := code.RepoGone{ID: repo.ID, Name: repo.Name, Status: repo.Status, DeletedAt: repo.DeletedAt,
			ManagedBy: repo.ManagedBy}
		return merged{}, false, fmt.Errorf("%w: %s", code.ErrRepoNotFound, gone.Detail())
	}
	if _, err := code.JudgeRead(q.Asker, q.CeilingSeat, repo, pv.roster); err != nil {
		return merged{}, false, err
	}
	// THE MEMO FIRST, once the grant has judged the preview and before any
	// git runs ([previewMemoKey]).
	if m, ok := pv.recall(); ok {
		return m, true, nil
	}
	// ITS OWN SCRATCH, a fresh id a preview: what the merge writes leaves
	// with it, and no two previews — nor a preview and the home's own merge
	// of the same two commits — share one.
	sc, err := p.o.Cache.NewScratch(ctx, pv.synced, statelog.NewOpID(p.now(), ""))
	if err != nil {
		return merged{}, true, pv.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not make a scratch to "+
			"merge in: %v; another git node answers", p.o.NodeID, err))
	}
	defer sc.Remove()
	if absent, err := pv.commits(ctx, sc); err != nil || absent != "" {
		if err == nil {
			err = fmt.Errorf("%w: repository %s holds no commit %s", code.ErrObjectNotFound, repo.Name, absent)
		}
		return merged{}, !errors.Is(err, code.ErrObjectNotFound), err
	}
	release, err := p.o.Share.Take(ctx, code.SeatWorkRead)
	if err != nil {
		if code.AnswerDue(ctx) {
			return merged{}, true, pv.unready(code.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole "+
				"share until the preview's answer fell due, its asker's attempt spent; another git node answers "+
				"meanwhile", p.o.NodeID))
		}
		return merged{}, true, err
	}
	defer release()
	m, err := pv.mergeTimed(ctx, sc)
	if err != nil && code.AnswerDue(ctx) {
		return merged{}, true, pv.unready(code.UnreadyShare, fmt.Sprintf("%s could not merge before the "+
			"preview's answer fell due, its asker's attempt spent; another git node answers meanwhile",
			p.o.NodeID))
	}
	if err == nil {
		pv.memoize(m)
	}
	return m, true, err
}

// previewMemoFormat names what a memoized preview holds — a previewMemo as
// JSON — and is the first part of its key, so an entry of another shape, a
// read's page among them (internal/code/query's own format), is never read
// as one.
const previewMemoFormat = "merge_preview/1"

// previewMemo is a decided preview as the memo keeps it: what [mergeIn]
// answered, and nothing the node or the moment stamps on the answer — its
// repository and the copy's position are the preview's own.
type previewMemo struct {
	State            code.MergeState `json:"state"`
	Conflicts        []string        `json:"conflicts,omitempty"`
	ConflictsOmitted int64           `json:"conflicts_omitted,omitempty"`
}

// previewMemoKey is the key a preview of the same two commits is memoized
// under on the pinned copy (cache.NewMemoKey): its format, the git the merge
// ran — a merge is git's algorithm over the two commits, which a git build may
// decide otherwise — and the two whole ids, base then head.
//
// A FUNCTION OF THE TWO IDS ALONE, so never stale, as every answer the memo
// keeps is (internal/code/cache's memo.go): an object id names one object for
// ever, the merge base is git's every one, and the memo is the copy's —
// kept only where the merge found both commits in this incarnation, which
// loses an object only with its history, and that drops the memo whole. So a
// hit is answered with no git run, the copy not asked again whether it holds
// the two.
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
	return merged{state: e.State, conflicts: code.ConflictsHeld{Paths: e.Conflicts,
		Omitted: e.ConflictsOmitted}}, true
}

// memoize keeps m, the merge of the two commits, in the pinned copy's memo —
// a DECIDED merge alone: one that ran out of its time (`unknown`) is the
// node's moment, never the commits', and asked again may finish. A memo that
// cannot be written is a slower preview next time, never a failed one.
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

// mergeTimed is step 7's merge ([mergeIn]) of the preview's two commits in sc,
// in the read lane within one read's git, observed on
// crewlet.code.home.merge.duration under `work` preview — ITS MERGE-TREE
// ALONE, since only a preview that reached it merged anything: one refused
// before it (behind its configuration, holding no chart, not held, not
// permitted, a commit absent, an id of no commit) is a read's refusal and no
// merge, and its sync and its wait for a unit of the share are the cache's
// and the share's. `failed` is a merge-tree that failed, or that its asker's
// attempt ended first.
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

// commits is step 6: the preview's two ids found in the copy — ONE `cat-file
// --batch-check` in the scratch, which reads the copy through its alternate —
// answering the first the copy does not hold, "" where it holds both as
// commits: an id of a tree, a blob or a tag is no commit to merge, and refused
// code.ErrInvalid naming it.
func (pv *previewing) commits(ctx context.Context, sc *cache.Scratch) (string, error) {
	q, p := pv.q, pv.p
	var out bytes.Buffer
	_, err := p.o.Git.Run(ctx, gitproc.Cmd{Dir: sc.Dir, Alternate: sc.Alternate,
		Args:  []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"},
		Stdin: strings.NewReader(q.Base + "\n" + q.Head + "\n"), Stdout: &out,
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil {
		if code.AnswerDue(ctx) {
			return "", pv.unready(code.UnreadyShare, fmt.Sprintf("%s could not look up the preview's commits "+
				"before its answer fell due; another git node answers meanwhile", p.o.NodeID))
		}
		return "", pv.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not look up the preview's commits: "+
			"%v; another git node answers", p.o.NodeID, err))
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
			return "", fmt.Errorf("%w: %s: %s is a %s, and a preview merges two commits", code.ErrInvalid,
				id.field, id.id, kind)
		}
	}
	return "", nil
}

// audit is step 8: a preview answered of a `visibility: unit` repository,
// recorded as its asker's read — one read, the answer's encoding its bytes —
// before it is answered.
func (pv *previewing) audit(ctx context.Context, a code.MergePreviewAnswer) error {
	repo := pv.synced.Repo
	if repo.Visibility != code.VisibilityUnit {
		return nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("home: measure the preview of repository %s: %w", repo.Name, err)
	}
	reader := pv.q.Asker.OperatorID
	if pv.q.Asker.Kind == code.PrincipalAgent {
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

// unready is a refusal about this node, which another git node may answer.
func (pv *previewing) unready(why code.UnreadyReason, detail string) error {
	return &code.ErrGitUnready{Reason: why, Detail: detail}
}
