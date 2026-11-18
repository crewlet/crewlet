package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

var log = logging.Get("gitlet.query")

// Cache is what the reads need of the node's git cache. *cache.Cache
// satisfies it.
type Cache interface {
	// Holds reports whether the cache holds or is filling a copy of a
	// repository, from memory and starting nothing (step 3).
	Holds(repoID string) bool
	// Sync hands out a repository's directory pinned, holding every row up
	// to a position at least as fresh as fresh asks.
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	// ServeConfig is the configuration every git process in a cache runs
	// with: one value, so no read sees a copy with another idea of it.
	ServeConfig() []gitproc.KV
	// Provenance is the pinned copy's index of pushes, held while the copy
	// is; an error is an index not readable yet, which a read answers
	// without (step 9).
	Provenance(s cache.Synced) (cache.Provenance, error)
	// WantHistory marks a repository's history as wanted (step 10).
	WantHistory(repoID string)
	// MemoGet is the page the pinned copy's memo keeps under a key, or false
	// (step 6).
	MemoGet(s cache.Synced, k cache.MemoKey) ([]byte, bool)
	// MemoProbe is MemoGet for a part of an answer looked for under several
	// keys, counted apart (cache's MemoProbe).
	MemoProbe(s cache.Synced, k cache.MemoKey) ([]byte, bool)
	// Memo is a batch of the pages a run cuts, memoized once its git has
	// answered whole (step 8); nil where the copy keeps none.
	Memo(s cache.Synced) *cache.MemoBatch
	// Spool is an unnamed file on the cache's volume, gone when closed, where
	// a revision's facts write the patch `git patch-id` reads (facts.go). The
	// caller closes it.
	Spool() (*os.File, error)
	// IndexTip is the commit the pinned copy's code index answers for, and
	// whether this node is one of the repository's index holders (search.go).
	IndexTip(s cache.Synced) (cache.IndexedTip, bool)
	// CodeIndex is the pinned copy's code index, valid while s is held, or
	// false, which a search answers with git grep; one it cannot read it
	// drops, under ctx's values.
	CodeIndex(ctx context.Context, s cache.Synced) (*cache.CodeIndexView, bool)
	// Tags is the pinned copy's tag index, open for one read, which closes
	// it; false where it keeps none a read can trust (tags.go).
	Tags(s cache.Synced) (*cache.TagIndex, bool)
}

// Pool runs git. *gitproc.Pool satisfies it.
type Pool interface {
	Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error)
}

// Options build a [Server].
type Options struct {
	// NodeID is this node's id, which a refusal about the node names.
	NodeID string
	// Cache is the node's git cache.
	Cache Cache
	// Git is the node's git pool; every process a read runs is in its read
	// lane.
	Git Pool
	// Share is the node's ONE seat git-work share (gitlet.NewGitShare over
	// the pool's read lane), which all of a seat's git work on the node
	// draws on.
	Share *gitlet.GitShare
	// Resyncs is the node's ONE set of re-sync buckets at the log's head
	// (gitlet.NewResyncs), shared with the host (host.Options.Resyncs): a
	// seat's second look at the head (step 7) pays its turn's bucket on its
	// seat, and a check's read its attempt's on its repository's checks
	// (gitlet.CheckKey.Resyncs); past it the read is answered from the copy at
	// its floor, saying so (read.objects).
	Resyncs *gitlet.Resyncs
	// Roster is the chart a seat's grant is judged by: the serving node's
	// CURRENT epoch's, read per read. A nil answer is a node with no chart
	// yet, which judges no seat (gitlet.UnreadyNoChart).
	Roster func() gitlet.Roster
	// Current reports whether this node serves the company's configuration:
	// false on a node confirmed behind it (shed or stuck), which judges no
	// seat's grant from the chart the fleet replaced
	// (gitlet.UnreadyBehindConfig). nil is current.
	Current func() bool
	// Fetched records one read this node answered of a `visibility: unit`
	// repository (step 11; package doc, "Who read it"). It is called inside
	// the read, on the read's context, which [Server.Stop] cuts and waits
	// for, and returns at once: no read waits on the broker for a record.
	// REQUIRED: a read answered and recorded by nobody is an audit short of
	// what the node served.
	Fetched func(ctx context.Context, e types.CodeFetched)
}

// Server answers reads of repositories by object id over the node's cache.
// It holds no goroutine of its own: each read runs on its caller's, and ends
// with its caller's context or with the server's stop ([Server.Stop]).
type Server struct {
	o Options
	// life is the SERVER'S lifetime, the one stored context: the stop ends
	// it, and it cuts every read's context (errStopped), so a stop reaches
	// every read in flight. inflight counts the reads begun and not yet
	// returned, which the stop waits for; stopped, under lifeMu, refuses
	// the reads after it.
	life     context.Context
	end      context.CancelFunc
	lifeMu   sync.Mutex
	stopped  bool
	inflight sync.WaitGroup
	// The bounds a read runs to, as New sets them; fields so this package's
	// tests can scale them (export_test.go). gitBudget is shared by every
	// process of a read's kind.
	syncWait     time.Duration
	searchBudget time.Duration
	gitBudget    time.Duration
	pageBytes    int
	probes       int
	touchedPaths int
	touchedBytes int
	patchBytes   int64
	classedBlob  int64
	classedBytes int64
	treeWalk     treeWalkBounds
	// flights is every page a read of this node is cutting that the memo
	// missed: a read missing the same page waits for that run and asks the
	// memo again rather than run the same git beside it (read.await). Under
	// flightsMu; waiting counts the reads waiting now, for the tests
	// (export_test.go).
	flightsMu sync.Mutex
	flights   map[flightKey]*flight
	waiting   atomic.Int64
	// patchSpools holds a token for each patch spooled on the cache's volume
	// now, MaxPatchSpools at most (read.takePatchSpool); patchSpoolWait is how
	// long a read waits for one (gitlet.GitShareWait), a field for the tests.
	patchSpools    chan struct{}
	patchSpoolWait time.Duration
}

// flightKey names one page a read's run is cutting: its repository and its
// memo key, which holds every argument of the page.
type flightKey struct {
	repo string
	key  cache.MemoKey
}

// flight is one page being cut, closed once the read that cuts it has
// answered — its pages placed in the memo, or given back.
type flight struct{ done chan struct{} }

// join is the flight of the page k: one already flying, and false; or a new
// one the caller leads, and true.
func (s *Server) join(k flightKey) (*flight, bool) {
	s.flightsMu.Lock()
	defer s.flightsMu.Unlock()
	if f, ok := s.flights[k]; ok {
		return f, false
	}
	if s.flights == nil {
		s.flights = map[flightKey]*flight{}
	}
	f := &flight{done: make(chan struct{})}
	s.flights[k] = f
	return f, true
}

// land ends the flight f of the page k, letting every read waiting on it go.
func (s *Server) land(k flightKey, f *flight) {
	s.flightsMu.Lock()
	delete(s.flights, k)
	s.flightsMu.Unlock()
	close(f.done)
}

// New is a server over o, refusing a field it needs by name.
func New(o Options) (*Server, error) {
	missing := ""
	switch {
	case o.NodeID == "":
		missing = "NodeID"
	case o.Cache == nil:
		missing = "Cache"
	case o.Git == nil:
		missing = "Git"
	case o.Share == nil:
		missing = "Share"
	case o.Resyncs == nil:
		missing = "Resyncs"
	case o.Roster == nil:
		missing = "Roster"
	case o.Fetched == nil:
		missing = "Fetched"
	}
	if missing != "" {
		return nil, fmt.Errorf("%w: query.Options.%s is required", gitlet.ErrInvalid, missing)
	}
	// THE SERVER'S OWN LIFETIME, ended by its stop alone: every read runs on
	// its own caller's context, none of which is the server's.
	life, end := context.WithCancel(context.Background())
	return &Server{o: o, life: life, end: end, syncWait: statelog.ReadBudget, gitBudget: gitproc.ReadDeadline,
		pageBytes: gitlet.MaxGitAnswerBytes, probes: MaxLandedProbes, touchedPaths: gitlet.MaxTouchedPaths,
		touchedBytes: gitlet.MaxTouchedBytes, patchBytes: MaxPatchIDDiffBytes, classedBlob: gitlet.MaxClassedBlobBytes,
		classedBytes: gitlet.MaxClassedBytes, searchBudget: gitlet.SearchBudget, treeWalk: defaultTreeWalk,
		patchSpools: make(chan struct{}, MaxPatchSpools), patchSpoolWait: gitlet.GitShareWait}, nil
}

// Read answers q with one page of its kind, in steps the package's files
// cite by number: (1) validate q and its cursor; (2) refuse a seat's read on
// a node that cannot judge it (judgeable); (3) refuse a non-home read on a
// node holding no copy, one node a walk filling it (held, ADR-0089); (4) sync
// to the asker's floor (sync); (5) judge the grant (grant); (6) answer from
// the memo, before any git for whole ids, after 7 for an abbreviation;
// (7) expand and find every id and path, refusing only once read at the log's
// head (objects); (8) run the kind's git under one share unit and one budget
// (compute); (9) name each commit's push and tags (index); (10) mark a home's
// history wanted; (11) record a unit repository's read (audit). ctx ends when
// the answer is due; a stopped server answers unready, node_failed (package
// doc, "Time: one budget of git, and an answer while the asker listens").
func (s *Server) Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	rctx, end, ok := s.begin(ctx)
	if !ok {
		return gitlet.GitAnswer{}, s.stopping()
	}
	defer end()
	a, err := s.serve(rctx, q)
	if err != nil && errors.Is(context.Cause(rctx), errStopped) {
		// CUT BY THE STOP, whatever the wait or the process it cut answered:
		// what a read ended this way says is about the node, never the
		// repository or the asker.
		return gitlet.GitAnswer{}, s.stopping()
	}
	return a, err
}

// errStopped is the cause a read's context ends with when its server stops
// ([Server.Stop]): never a read's answer, which is [Server.stopping] instead.
var errStopped = errors.New("query: this node's git reads are stopping")

// begin admits one read to a running server, answering the context it runs
// under — the caller's, cut by the server's stop — and what ends it; or false
// once the stop has begun, where the read starts nothing.
func (s *Server) begin(parent context.Context) (context.Context, func(), bool) {
	s.lifeMu.Lock()
	if s.stopped {
		s.lifeMu.Unlock()
		return nil, nil, false
	}
	s.inflight.Add(1)
	s.lifeMu.Unlock()
	ctx, cancel := context.WithCancelCause(parent)
	//nolint:contextcheck // The server's lifetime cuts the read's work too: Stop must reach every read in flight.
	cut := context.AfterFunc(s.life, func() { cancel(errStopped) })
	return ctx, func() {
		cut()
		cancel(nil)
		s.inflight.Done()
	}, true
}

// stopping is what a read the stop refused or cut is answered: unready about
// this node, node_failed, which lets the walk have the next node fill a copy
// (gitlet.UnreadyReason.PassesFill).
func (s *Server) stopping() error {
	return &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNodeFailed, Detail: fmt.Sprintf("%s is stopping its git reads; "+
		"another Gitlet node answers, and fills a copy where none holds one", s.o.NodeID)}
}

// Stop ends the server: every later read is refused unready, node_failed,
// and every read in flight is CUT (its waits and git end) and answered the
// same; Stop waits for each to return, or for ctx. So once it returns no read
// is still being answered or recorded (Options.Fetched), which the engine's
// teardown orders before the usage day's last flush. Called BEFORE the cache
// is stopped, which a read in flight still reads. Idempotent. Cut rather than
// waited out: a read may run its asker's whole attempt
// (estate.GitReadAttempt), longer than the stop's budget, and a cut read is
// answered while its asker listens (package doc, "The stop").
func (s *Server) Stop(ctx context.Context) {
	s.lifeMu.Lock()
	s.stopped = true
	s.lifeMu.Unlock()
	s.end()
	waited := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		log.WarnContext(ctx, "code_git_reads_stop_unfinished", "node", s.o.NodeID, "error", context.Cause(ctx),
			"detail", "git reads were still answering when the stop's wait ended; one that answers after it "+
				"is recorded after Gitlet's stop, behind what the node's teardown flushes of its reads")
	}
}

// serve is Read's work, under the context the server's stop cuts.
func (s *Server) serve(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	if err := q.Validate(); err != nil {
		return gitlet.GitAnswer{}, err
	}
	at, err := cursorOf(q)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	r := &read{s: s, q: q}
	if err = r.judgeable(); err != nil {
		return gitlet.GitAnswer{}, err
	}
	if err = r.held(); err != nil {
		return gitlet.GitAnswer{}, err
	}
	// THE PIN IS THE READ'S, released whatever it answers: every Synced a
	// sync or a re-sync handed out goes back through r.synced.
	defer func() { r.synced.Release() }()
	defer r.closeTags()
	if err = r.sync(ctx, r.fresh()); err != nil {
		return gitlet.GitAnswer{}, err
	}
	// A read that leads a page's flight lets those waiting on it go once it
	// has answered, whatever it answered: its pages are placed by then.
	defer func() {
		if r.landing != nil {
			r.landing()
		}
	}()
	var a gitlet.GitAnswer
	var ok bool
	if q.Kind == gitlet.GitSearch {
		// A SEARCH NAMES A REF, not an id: it resolves its commit before
		// the memo can be asked, and reads by its own paths (search.go).
		if a, err = r.searchRead(ctx); err != nil {
			return gitlet.GitAnswer{}, err
		}
		ok = true
	} else if q.Kind == gitlet.GitTips {
		// MANY TIPS ARE KEPT ONE BY ONE, never as a page (tips.go): the memo
		// is asked for each before any git runs.
		if a, ok, err = r.recallTips(ctx, at); err != nil {
			return gitlet.GitAnswer{}, err
		}
	} else if q.Kind == gitlet.GitTags {
		// THE COPY'S TAGS, from its tag index with no git (tags.go).
		if a, err = r.tagsRead(ctx); err != nil {
			return gitlet.GitAnswer{}, err
		}
		ok = true
	} else if a, ok, err = r.recallWhole(ctx, at); err != nil {
		return gitlet.GitAnswer{}, err
	}
	if !ok {
		if a, err = r.compute(ctx, at); err != nil {
			return gitlet.GitAnswer{}, err
		}
	}
	r.stamp(&a, a.Memo)
	if q.Home {
		s.o.Cache.WantHistory(q.RepoID)
	}
	if err := r.audit(ctx, a); err != nil {
		return gitlet.GitAnswer{}, err
	}
	return a, nil
}

// audit is step 11: a read of a `visibility: unit` repository recorded as its
// asker read it, one read of the answer's encoded bytes (what the asker is
// sent beside its envelope). It runs before the read returns and before its
// page flight lets its waiters go, which is why Fetched never waits on the
// broker.
func (r *read) audit(ctx context.Context, a gitlet.GitAnswer) error {
	repo := r.synced.Repo
	if repo.Visibility != gitlet.VisibilityUnit || r.q.EnginesOwn() {
		return nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("query: measure the answer to a read of repository %s: %w", repo.Name, err)
	}
	r.s.o.Fetched(ctx, types.CodeFetched{RepoID: repo.ID, RepoName: repo.Name,
		PrincipalKind: types.CodeReaderKind(r.q.Asker.Kind), Principal: r.q.Asker.Actor(),
		Seat: r.q.Asker.Seat, Via: types.FetchedViaTool, Kind: string(r.q.Kind), Reads: 1,
		Bytes: int64(len(encoded))})
	return nil
}

// compute is steps 7 and 8: the answer of a read the memo did not answer
// before any git ran — found, then asked of the memo once more where an
// abbreviation's expansion made its key, and cut by the kind's own git.
func (r *read) compute(ctx context.Context, at cursor) (gitlet.GitAnswer, error) {
	objs, err := r.objects(ctx)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	if !r.recalled && memoizable(r.q) {
		whole := wholeOf(r.q, objs)
		if a, ok := r.recall(whole, at); ok {
			return a, nil
		}
		if a, ok, err := r.await(ctx, whole, at); err != nil || ok { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			return a, err
		}
	}
	r.index(commitsOf(r.q, objs))
	// A FIND TAKES ITS OWN UNIT, and only for git: its pages are the tree
	// listing's, read from the memo with none held (find.go).
	if r.q.Kind == gitlet.GitFind {
		return r.findRead(ctx, objs)
	}
	release, err := r.s.o.Share.Take(ctx, r.q.SeatWork())
	if err != nil {
		if gitlet.AnswerDue(ctx) {
			return gitlet.GitAnswer{}, r.unready(gitlet.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole "+
				"share until the read's answer fell due, its asker's attempt spent; another Gitlet node answers "+
				"meanwhile", r.s.o.NodeID))
		}
		return gitlet.GitAnswer{}, err
	}
	defer release()
	// THE READ'S OWN GIT BEGINS: from here every process is given what is
	// left of one budget of run time, and charged what it ran.
	r.metered, r.gitLeft = true, r.s.gitBudget
	a, err := r.answer(ctx, objs, at)
	release()
	return a, err
}

// commitsOf is every commit q's revisions name once each tag is peeled, in
// the order q names them: what an answer of q is decorated with.
func commitsOf(q gitlet.GitQuery, objs found) []string {
	var out []string
	for _, rev := range revsOf(q) {
		if o := objs.revs[rev]; o.peeledKind == "commit" && !slices.Contains(out, o.peeled) {
			out = append(out, o.peeled)
		}
	}
	return out
}

// index reads the pinned copy's index of pushes, once a read, and names the
// push that introduced each of commits (step 9), replacing what an earlier
// call named: a memo hit is decorated by the commits its page names, one it
// could not use by the read's own.
func (r *read) index(commits []string) {
	// AN UNSTAMPED READ READS NO INDEX: it names no push, so nothing the
	// index holds may move its page (gitlet.GitQuery.Unstamped). Its commits
	// are still named, by no push.
	if !r.indexRead && !r.q.Unstamped {
		if p, err := r.s.o.Cache.Provenance(r.synced); err == nil && p.Indexed() {
			r.prov = p
			cov := p.Coverage()
			r.coverage = &cov
		}
	}
	r.indexRead = true
	r.openTags()
	r.pushedBy, r.named = nil, nil
	r.tagged = nil
	if r.tagIndex != nil {
		r.tagged = &gitlet.TagMarks{}
	}
	for _, id := range commits {
		r.decorate(id)
	}
}

// decorate names the push that introduced commit id, where the index holds
// it, before any page is cut, since every page's envelope carries it. Every
// commit decorated is named (read.named), held or not, so a memo page is
// decorated by what the index holds when it is read.
func (r *read) decorate(id string) {
	r.named = append(r.named, id)
	r.markTags(id)
	if !r.prov.Indexed() {
		return
	}
	by, ok := r.prov.Commit(id)
	if !ok {
		return
	}
	if r.pushedBy == nil {
		r.pushedBy = map[string]gitlet.PushedBy{}
	}
	r.pushedBy[id] = by
}

// read is one Read in progress.
type read struct {
	s *Server
	q gitlet.GitQuery
	// roster is the chart a seat is judged by, read once a read so one read
	// is judged by one chart; nil for a person's or a token's read.
	roster gitlet.Roster
	// synced is the pinned copy the read reads, and linear whether it was
	// read at the log's head — an id it lacks then is absent, with no second
	// look.
	synced cache.Synced
	linear bool
	// prov is the copy's index of pushes (zero where none is readable),
	// indexRead whether it was asked for; pushedBy and coverage ride every
	// page and so are measured into its envelope (room); named is every
	// commit decorated, held by the index or not.
	prov      cache.Provenance
	indexRead bool
	pushedBy  map[string]gitlet.PushedBy
	coverage  *gitlet.Coverage
	named     []string
	// recalled: the memo was asked before any git ran (recallWhole), so it
	// is not asked again once the read's objects are found. landing ends the
	// flight of the page the read leads (await), nil where it leads none.
	recalled bool
	landing  func()
	// metered is set once the read's own git has begun (step 8); gitLeft is
	// what is left of its budget of run time, each process's deadline
	// ([read.budget]), charged its run alone ([read.charge]), never its wait
	// for a read-lane slot: that is the node's contention, not the read's size.
	metered bool
	gitLeft time.Duration
	// filter is an interdiff's files, made once (read.interdiffFilter).
	filter *pathFilter
	// tips is what a tips read holds of its facts and counts (recallTips).
	tips *tipsWarm
	// tagIndex is the tag index a history's read opened under the pin tagsDir
	// and tagsAt name (openTags); tagged the tags naming each commit decorated.
	tagIndex  *cache.TagIndex
	tagsDir   string
	tagsAt    statelog.Position
	tagsTried bool
	tagged    *gitlet.TagMarks
	// tagsOf is each commit's lookup, kept between its cost and its naming.
	tagsOf map[string]gitlet.CommitTags
}

// judgeable refuses a seat's read on a node that cannot judge a seat (step 2).
func (r *read) judgeable() error {
	if r.q.Asker.Kind != gitlet.PrincipalAgent {
		return nil
	}
	if cur := r.s.o.Current; cur != nil && !cur() {
		return r.unready(gitlet.UnreadyBehindConfig, fmt.Sprintf("%s is behind the company's configuration and "+
			"judges no seat's grant from the chart the fleet replaced; a current Gitlet node answers", r.s.o.NodeID))
	}
	if r.roster = r.s.o.Roster(); r.roster == nil {
		return r.unready(gitlet.UnreadyNoChart, fmt.Sprintf("%s holds no chart yet to judge seat %q's grant by; "+
			"a Gitlet node holding one answers", r.s.o.NodeID, r.q.Asker.Seat))
	}
	return nil
}

// held refuses a read this node may not fill a copy for (step 3): it holds
// none, and the read is not its walk's effective home's. Asked before the
// sync, which would start the download: a cold round outlives the read's
// wait, so a node starting one for a read another node answers would download
// the whole repository for nobody.
func (r *read) held() error {
	if r.q.Home || r.s.o.Cache.Holds(r.q.RepoID) {
		return nil
	}
	return r.unready(gitlet.UnreadyNotHeld, fmt.Sprintf("%s holds no copy of repository %s and the read is "+
		"not its walk's home's, which is the one node that fills a copy; a Gitlet node holding one answers "+
		"meanwhile", r.s.o.NodeID, r.q.RepoID))
}

// fresh is what the first sync asks: the asker's floor on the code log, at
// the level it names (session where none), or else the copy as it stands. An
// id names one object at every position, so a floor buys only a copy holding
// the asker's own pushes; step 7's second look covers every other id. A
// LANDED-IN READ is brought at least to where its tip's move landed
// (gitlet.LandedInQuery.At): its index must hold the target's moves up to
// that tip to name the push that landed the commit, and no move lies between
// that one and the read that resolved it. A floor on another log is refused
// with the request ([gitlet.GitQuery.Validate]).
func (r *read) fresh() statelog.Freshness {
	f := r.q.Fresh
	floor := f.Floor(topics.CodeLogStream)
	if r.q.Kind == gitlet.GitLandedIn {
		if at := r.q.LandedIn.At; floor.IsZero() || reachedFloor(at, floor) {
			floor = at
		}
	}
	if floor.IsZero() {
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	f.Floors = []statelog.Position{floor}
	if f.Level == "" {
		f.Level = statelog.ReadSession
	}
	return f
}

// reachedFloor reports whether a is past b, both on the code log.
func reachedFloor(a, b statelog.Position) bool {
	before, err := b.Before(a)
	return err == nil && before
}

// sync pins the copy at fresh, in place of any copy the read holds, its wait
// bounded by the read's sync budget and by ctx, which ends when the read's
// answer is due.
func (r *read) sync(ctx context.Context, fresh statelog.Freshness) error {
	sctx, cancel := context.WithTimeout(ctx, r.s.syncWait)
	defer cancel()
	s, err := r.s.o.Cache.Sync(sctx, r.q.RepoID, fresh)
	switch {
	case err == nil:
	case errors.Is(err, gitlet.ErrRepoNotFound):
		return fmt.Errorf("query: repository %s: %w", r.q.RepoID, err)
	case errors.Is(err, cache.ErrInvalid):
		// The request's own mistake (a floor on another log), which no
		// other node would take either.
		return fmt.Errorf("%w: fresh: %w", gitlet.ErrInvalid, err)
	case gitlet.AnswerDue(ctx):
		return r.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the read's need before its answer fell due; the sync goes on, and another Gitlet node answers "+
			"meanwhile", r.s.o.NodeID, r.q.RepoID))
	case ctx.Err() != nil:
		return ctx.Err()
	case sctx.Err() != nil:
		return r.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the read's need within %s; the sync goes on, and another Gitlet node answers meanwhile",
			r.s.o.NodeID, r.q.RepoID, r.s.syncWait))
	case cache.EveryFillMeets(err):
		return r.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s could not sync repository %s: %v — what every "+
			"Gitlet node's copy is filled from, so a Gitlet node holding a copy answers meanwhile",
			r.s.o.NodeID, r.q.RepoID, err))
	default:
		return r.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not sync repository %s: %v; another "+
			"Gitlet node answers, and fills a copy where none holds one", r.s.o.NodeID, r.q.RepoID, err))
	}
	r.synced.Release()
	r.synced, r.linear = s, fresh.Level == statelog.ReadLinearizable
	return nil
}

// grant answers a repository the read may not be served from (step 5): one
// deleted or purging, one the asker's grant does not let it read, and one the
// grant of the seat it reads for does not (gitlet.JudgeRead).
func (r *read) grant() error {
	repo := r.synced.Repo
	if repo.Status == gitlet.RepoStatusDeleted || repo.Status == gitlet.RepoStatusPurging {
		gone := gitlet.RepoGone{ID: repo.ID, Name: repo.Name, Status: repo.Status, DeletedAt: repo.DeletedAt,
			ManagedBy: repo.ManagedBy}
		return fmt.Errorf("%w: %s", gitlet.ErrRepoNotFound, gone.Detail())
	}
	if r.q.EnginesOwn() {
		// THE ENGINE'S OWN READ names no asker (a revision's facts, or a read
		// for a check, whose attempt's row admitted its box): judged as nobody
		// it would be refused.
		return nil
	}
	_, err := gitlet.JudgeRead(r.q.Asker, r.q.CeilingSeat, repo, r.roster)
	return err
}

// objects is steps 5 and 7, with ONE second look at the log's head: no
// refusal is concluded from a copy that may be behind the log (package doc,
// "By object id, never by ref"). A read that would refuse on a copy not read
// at the head syncs there once (lookAtHead), judges again on the row it
// brings, and only then answers. A SEAT'S second look is paid from its turn's
// bucket on this node, a check's read from its attempt's (resync); past it
// the refusal is answered from the copy at the floor, which holds what the
// turn has read and this node's own pushes, saying it was not judged again
// (gitlet.NotRejudged). A person's and the engine's own reads are never
// charged.
func (r *read) objects(ctx context.Context) (found, error) {
	for {
		err := r.grant()
		var (
			objs   found
			absent string
		)
		if err == nil {
			var ferr error
			objs, absent, ferr = r.find(ctx)
			switch {
			case ferr != nil:
				return found{}, ferr
			case absent == "":
				return objs, nil
			}
		}
		note := ""
		if !r.linear {
			spent := r.resync()
			if spent == nil {
				if err = r.lookAtHead(ctx); err != nil {
					return found{}, err
				}
				continue
			}
			_, _, principal, scope := r.floorWords()
			note = " " + gitlet.NotRejudged(spent, principal, scope, time.Now())
		}
		if err != nil {
			return found{}, fmt.Errorf("%w%s", err, note)
		}
		if note != "" {
			shown, instead, _, _ := r.floorWords()
			return found{}, fmt.Errorf("%w: repository %s holds no object %s at the position %s; %s %s",
				gitlet.ErrObjectNotFound, r.synced.Repo.Name, absent, shown, instead, note[1:])
		}
		return found{}, fmt.Errorf("%w: repository %s holds no object %s", gitlet.ErrObjectNotFound,
			r.synced.Repo.Name, absent)
	}
}

// lookAtHead is the read's ONE SECOND LOOK at the log's head: the copy synced
// at session to the head its asker read (gitlet.GitQuery.Head), else to a
// barrier of its own at linearizable; either way no write the asker could
// have seen is past it, so the read counts as read at the head. The asker's
// head costs no barrier here: one for a whole search, not one a repository.
func (r *read) lookAtHead(ctx context.Context) error {
	fresh := statelog.Freshness{Level: statelog.ReadLinearizable}
	if !r.q.Head.IsZero() {
		fresh = statelog.FloorAt(statelog.ReadSession, r.q.Head)
	}
	if err := r.sync(ctx, fresh); err != nil {
		return err
	}
	r.linear = true
	return nil
}

// resync draws the read's second look at the log's head from this node's
// buckets: a SEAT'S read from its turn on its seat; a read FOR A CHECK (a
// box's, at /box/gitlet/) from its attempt on its repository's checks
// (gitlet.CheckKey.Resyncs), the bucket the box's transfers pay, counted with
// them (crewlet.code.box.resyncs). Nil where it is taken, or for a read
// charged to nobody: a person's, or the engine's own.
func (r *read) resync() *costlimit.Refusal {
	switch {
	case r.q.Check != nil:
		return r.s.o.Resyncs.Box(r.q.Check.Resyncs())
	case r.q.Asker.Kind == gitlet.PrincipalAgent:
		return r.s.o.Resyncs.Turn(r.q.Asker.Seat, r.q.Turn)
	}
	return nil
}

// floorWords is how a read answered at its floor names it: what it has read,
// what it may do instead, and whose bucket ran out (gitlet.NotRejudged), a
// seat's turn's or a check's.
func (r *read) floorWords() (shown, instead, principal, scope string) {
	if r.q.Check != nil {
		return "this check was queued at", "a check's box reads the one commit it checks", "this check's",
			"its repository's checks'"
	}
	return "this turn has read", "it has not been pushed yet, or ask " + toolname.CodeResolve, "this turn's",
		"this seat's"
}

// answer runs the kind's own git over objs (step 8), memoizing every page it
// cuts once that git has answered whole, and none where it did not.
func (r *read) answer(ctx context.Context, objs found, at cursor) (gitlet.GitAnswer, error) {
	m := r.memoRun(objs)
	defer m.discard()
	switch r.q.Kind {
	case gitlet.GitTree:
		return r.treeRead(ctx, objs, at, m)
	case gitlet.GitBlob:
		return r.blobRead(ctx, objs, at, m)
	case gitlet.GitDiff:
		return r.diffRead(ctx, objs, at, m)
	case gitlet.GitTips:
		return r.tipsRead(ctx, objs, at)
	case gitlet.GitLandedIn:
		return r.landedRead(ctx, objs)
	case gitlet.GitRevisionFacts:
		return r.factsRead(ctx, objs)
	case gitlet.GitReanchor:
		return r.reanchorRead(ctx, objs)
	case gitlet.GitInterdiff:
		return r.interdiffRead(ctx, objs, at, m)
	case gitlet.GitLog:
		return r.logRead(ctx, objs, at, m)
	case gitlet.GitBlame:
		return r.blameRead(ctx, objs, at, m)
	case gitlet.GitTreeCommits:
		return r.treeCommitsRead(ctx, objs, at, m)
	case gitlet.GitChecksFile:
		return r.checksFileRead(ctx, objs)
	case gitlet.GitCodeFiles:
		return r.codeFilesRead(ctx, objs)
	case gitlet.GitExcerpts:
		return r.excerptsRead(ctx, objs, at, m)
	}
	return r.compareRead(ctx, objs, m)
}

// unready is a refusal about this node, which another Gitlet node may answer.
func (r *read) unready(why gitlet.UnreadyReason, detail string) error {
	return &gitlet.ErrGitUnready{Reason: why, Detail: detail}
}

// budget is the deadline the next process of the read's OWN GIT is given:
// what is left of the one budget its kind's processes share ([read.gitLeft]).
// One already run out is the read too large to answer, with the generic
// narrowing: a run is charged after it ends, so a later process finds it spent
// only where the one before ran out its last instant.
func (r *read) budget() (time.Duration, error) {
	if r.gitLeft <= 0 {
		return 0, fmt.Errorf("%w: the read of repository %s ran its git's whole %s: narrow it",
			gitlet.ErrSliceTooLarge, r.q.RepoID, r.s.gitBudget)
	}
	return r.gitLeft, nil
}

// failed is what a git process's end means for the read, once its run is
// charged ([read.charge]): nil where it answered; unready, syncing, where it
// reported damage (failed or not: git answers some questions over damage and
// exits 0) or the pool refused the copy's shape, both of which the cache
// repairs; unready, share, where the answer fell due (gitlet.AnswerDue) or the
// lane was busy past gitproc.SlotWait; slice (ErrSliceTooLarge where nil)
// where the read's own git ran out its budget, as it would on another node;
// unready, node_failed, where a short check before it ran its whole bound,
// which no healthy node takes. A RUN THE DUE INSTANT ENDED IS NEVER TOO LARGE:
// it had less than its whole budget, which a quiet node gives (package doc,
// "Time: one budget of git, and an answer while the asker listens").
func (r *read) failed(ctx context.Context, what string, res gitproc.Result, err error, slice error) error {
	r.charge(res)
	if res.Damage != nil {
		return r.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s met damage the node is "+
			"repairing (%v); another Gitlet node answers meanwhile", r.s.o.NodeID, what, r.q.RepoID, res.Damage))
	}
	switch {
	case err == nil:
		return nil
	case gitlet.AnswerDue(ctx):
		return r.unready(gitlet.UnreadyShare, fmt.Sprintf("%s could not finish the %s over repository %s before "+
			"the read's answer fell due — its asker's attempt spent on the waits before it; another Gitlet node "+
			"answers meanwhile", r.s.o.NodeID, what, r.q.RepoID))
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, gitproc.ErrBusy):
		return r.unready(gitlet.UnreadyShare, fmt.Sprintf("%s's read lane was busy past %s; another Gitlet node "+
			"answers meanwhile", r.s.o.NodeID, gitproc.SlotWait))
	case errors.Is(err, gitproc.ErrDeadline) && r.metered:
		if slice == nil {
			return fmt.Errorf("%w: the %s over repository %s ran the read's whole %s of git: narrow it",
				gitlet.ErrSliceTooLarge, what, r.q.RepoID, r.s.gitBudget)
		}
		return slice
	case errors.Is(err, gitproc.ErrDeadline):
		return r.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s's %s over repository %s ran its whole bound, "+
			"which no healthy Gitlet node takes over it: %v; another Gitlet node answers", r.s.o.NodeID, what,
			r.q.RepoID, err))
	case errors.Is(err, gitproc.ErrRepoShape):
		return r.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s did not complete: %v; "+
			"another Gitlet node answers meanwhile", r.s.o.NodeID, what, r.q.RepoID, err))
	}
	return fmt.Errorf("query: %s over repository %s: %w", what, r.q.RepoID, err)
}

// charge counts a process's run, never its wait for a read-lane slot,
// against the read's budget once its own git has begun ([read.metered]).
// [read.failed] calls it for every process it judges; a run the read ends
// itself, having read all it needs (errStop), is charged here directly.
func (r *read) charge(res gitproc.Result) {
	if r.metered {
		r.gitLeft -= res.Took
	}
}

// cursorOf is q's cursor, read as one its kind issues.
func cursorOf(q gitlet.GitQuery) (cursor, error) {
	switch q.Kind {
	case gitlet.GitTree:
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'e': 1})
	case gitlet.GitBlob:
		// A file read of a directory answers its listing, whose cursor is
		// an entry's.
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'l': 1, 'b': 2, 'e': 1})
	case gitlet.GitDiff:
		if q.Diff.Patches {
			return parseCursor(q.Cursor, q.Kind, map[byte]int{'w': 2})
		}
		if q.Diff.File != "" || q.Diff.FileB64 != "" {
			return parseCursor(q.Cursor, q.Kind, map[byte]int{'p': 1})
		}
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'f': 1})
	case gitlet.GitInterdiff:
		if q.Interdiff.Patches {
			return parseCursor(q.Cursor, q.Kind, map[byte]int{'w': 2})
		}
		if q.Interdiff.File != "" || q.Interdiff.FileB64 != "" {
			return parseCursor(q.Cursor, q.Kind, map[byte]int{'p': 1})
		}
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'f': 1})
	case gitlet.GitLog:
		// A history's page begins past the commits of the walk the pages
		// before it covered.
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'s': 1})
	case gitlet.GitTips:
		// A page of tips begins at the n-th tip the query names.
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'t': 1})
	case gitlet.GitTags:
		// A tags page begins past the key the last page ended at.
		_, err := parseTagsCursor(q.Cursor)
		return cursor{}, err
	case gitlet.GitBlame:
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'l': 1})
	case gitlet.GitTreeCommits:
		// A directory's last commits begin at its listing's n-th entry.
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'e': 1})
	case gitlet.GitExcerpts:
		// A page of excerpts begins at the n-th anchor, one the query names.
		c, err := parseCursor(q.Cursor, q.Kind, map[byte]int{'x': 1})
		if err == nil && c.a >= len(q.Excerpts.Anchors) {
			return cursor{}, errBadCursor(q.Cursor, q.Kind, "an anchor of the read")
		}
		return c, err
	}
	// One page and no cursor: a comparison's first page of files (a diff of
	// its merge base and head reads the rest), or a landed-in read's, a
	// revision's facts' or a re-anchoring's single bounded answer.
	return parseCursor(q.Cursor, q.Kind, map[byte]int{})
}
