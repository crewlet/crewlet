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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("code.query")

// Cache is what the reads need of the node's git cache. *cache.Cache
// satisfies it.
type Cache interface {
	// Holds reports whether the cache holds a copy of a repository or is
	// filling one, from memory and starting nothing: what a read that is not
	// its walk's effective home's asks before anything else (step 3).
	Holds(repoID string) bool
	// Sync hands out a repository's directory pinned, holding every row up
	// to a position at least as fresh as fresh asks.
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	// ServeConfig is the configuration every git process in a cache runs
	// with: one value, so no read sees a copy with another idea of it.
	ServeConfig() []gitproc.KV
	// Provenance is the pinned copy's index of which push introduced each
	// commit, held while the copy is; an error is an index the node cannot
	// read yet, which a read answers without (step 9).
	Provenance(s cache.Synced) (cache.Provenance, error)
	// WantHistory marks a repository's history as wanted: what a read
	// answered as the repository's home asks of its index (step 10).
	WantHistory(repoID string)
	// MemoGet is the page the pinned copy's memo keeps under a key, or false
	// (step 6).
	MemoGet(s cache.Synced, k cache.MemoKey) ([]byte, bool)
	// Memo is a batch of the pages a run cuts, to memoize in the pinned copy
	// once its git has answered whole (step 8) — nil where it keeps none.
	Memo(s cache.Synced) *cache.MemoBatch
	// Spool is a file with no name on the cache's volume, gone when it is
	// closed: where a revision's facts write the change's patch for `git
	// patch-id` to read back (facts.go). The caller closes it.
	Spool() (*os.File, error)
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
	// Git is the node's git pool; every process a read runs is a read-lane
	// one.
	Git Pool
	// Share is the node's seat git-work share (code.NewGitShare over the
	// pool's read lane), ONE a node: whatever else of a seat's git work the
	// node runs draws on the same.
	Share *code.GitShare
	// Resyncs is the node's buckets for its seats' tools' re-syncs at the
	// log's head (code.NewResyncs), ONE a node — the code host's engine
	// writes draw on the same (host.Options.Resyncs): a SEAT'S read whose
	// refusal would be judged again at linearizable (step 7) pays its turn's
	// bucket on its seat, and past it is answered from the copy at the seat's
	// floor, saying so — and so does a read made for a CHECK, its attempt on
	// its repository's checks (code.CheckKey.Resyncs), keys no seat spells.
	Resyncs *costlimit.Limiter
	// Roster is the chart a seat's grant is judged by: the serving node's
	// CURRENT epoch's, read per read. A nil answer is a node with no chart
	// yet, which judges no seat (code.UnreadyNoChart).
	Roster func() code.Roster
	// Current reports whether this node serves the company's configuration:
	// false on a node confirmed behind it (shed or stuck), which judges no
	// seat's grant from the chart the fleet replaced
	// (code.UnreadyBehindConfig). nil is current.
	Current func() bool
	// Fetched records one read this node answered of a `visibility: unit`
	// repository (step 11) — filed by the engine in the node's reads ledger
	// (internal/code/readaudit), which publishes every reader's reads of a
	// flush as one record into the node's event log, from which the usage
	// domain derives who read it. It returns at once: no read, and no read
	// waiting on the page one cuts, waits on the broker for a record.
	// REQUIRED, for the host's reason: a read answered and recorded by nobody
	// is an audit short of what the node served.
	Fetched func(ctx context.Context, e types.CodeFetched)
}

// Server answers reads of repositories by object id over the node's cache.
// It holds no goroutine of its own: each read runs on its caller's, and ends
// with its caller's context or with the server's stop ([Server.Stop]).
type Server struct {
	o Options
	// life is the SERVER'S lifetime — the one stored context, for the reason
	// the code host's is: the stop ends it, and every read's context is cut
	// by it (errStopped), so a stop reaches every read in flight. inflight
	// counts the reads begun and not yet returned, which the stop waits for,
	// and stopped, under lifeMu, refuses the reads after it.
	life     context.Context
	end      context.CancelFunc
	lifeMu   sync.Mutex
	stopped  bool
	inflight sync.WaitGroup
	// syncWait bounds a read's wait on its sync (statelog.ReadBudget),
	// gitBudget the run time a read's own git is given, shared by every
	// process of its kind (gitproc.ReadDeadline), pageBytes an answer's
	// JSON (code.MaxGitAnswerBytes), probes a landed-in search's ancestry
	// checks (MaxLandedProbes), touchedPaths and touchedBytes a revision's
	// touched list (code.MaxTouchedPaths, code.MaxTouchedBytes),
	// patchBytes the diff its patch-id is taken of (MaxPatchIDDiffBytes), and
	// classedBlob and classedBytes the Go its classification lexes, a
	// version and in all (code.MaxClassedBlobBytes, code.MaxClassedBytes) —
	// fields so this package's tests can scale them (export_test.go).
	syncWait     time.Duration
	gitBudget    time.Duration
	pageBytes    int
	probes       int
	touchedPaths int
	touchedBytes int
	patchBytes   int64
	classedBlob  int64
	classedBytes int64
	// flights is every page a read of this node is cutting that its copy's
	// memo held none of, by repository and the page's memo key: a read that
	// misses the same page waits for that read's run and asks the memo again,
	// rather than run the same git beside it (read.await). Under flightsMu;
	// waiting counts the reads waiting on one now, which a case reads to know
	// a read waits (export_test.go).
	flightsMu sync.Mutex
	flights   map[flightKey]*flight
	waiting   atomic.Int64
}

// flightKey names one page a read's run is cutting: its repository and its
// memo key, which every argument of the page is in.
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
		return nil, fmt.Errorf("%w: query.Options.%s is required", code.ErrInvalid, missing)
	}
	// THE SERVER'S OWN LIFETIME, ended by its stop alone: no caller's
	// context is the server's, since every read runs on its own.
	life, end := context.WithCancel(context.Background())
	return &Server{o: o, life: life, end: end, syncWait: statelog.ReadBudget, gitBudget: gitproc.ReadDeadline,
		pageBytes: code.MaxGitAnswerBytes, probes: MaxLandedProbes, touchedPaths: code.MaxTouchedPaths,
		touchedBytes: code.MaxTouchedBytes, patchBytes: MaxPatchIDDiffBytes, classedBlob: code.MaxClassedBlobBytes,
		classedBytes: code.MaxClassedBytes}, nil
}

// Read answers q: a page of the tree, file, diff, comparison or interdiff it
// names; a page of a path's history, or of a file's blame; whether a commit
// landed in a target and the push that landed it; a revision's facts; or
// where anchored lines are in a revision.
//
//  1. q is validated (code.GitQuery.Validate), and its cursor read as one its
//     kind issues.
//  2. A SEAT'S read is refused about THIS node — code.ErrGitUnready, which
//     another git node may answer — where the node is confirmed behind the
//     company's configuration (behind_config) or holds no chart (no_chart):
//     judged before the sync, so a node that cannot judge starts nothing. A
//     person's or a token's read reads no chart and skips both.
//  3. A node holding no copy of the repository, nor filling one
//     (Cache.Holds), answers a read that is not its walk's effective home's
//     (code.GitQuery.Home) unready (not_held) at once, having started
//     nothing: ONE node a walk fills a copy (ADR-0059), and every other
//     either holds one already or sends the read on.
//  4. The node's copy is synced to the asker's floor, or read as it stands
//     where the asker holds none, the wait bounded by statelog.ReadBudget: a
//     copy that cannot be brought there in time — a cold one downloading the
//     repository, which goes on after the read stops waiting — or a sync that
//     failed on what every node's copy is filled from (the code log, the
//     object store, the rows, a repair the cache would not repeat) is
//     unready, syncing; one that failed for a reason of the node's own is
//     unready, node_failed, which lets the walk have the next node fill
//     (cache.EveryFillMeets, the rule an engine write's sync is read by too).
//  5. A deleted or purging repository answers code.ErrRepoNotFound with the
//     domain's own sentence for it; the asker's grant is judged from the
//     repository's row and this node's chart, and a read below read is
//     code.ErrNotPermitted — and where the read is made for the seat its
//     asker's turn is acting for (code.GitQuery.CeilingSeat), that seat's
//     grant from the same row and chart, one it could not make itself
//     code.ErrAboveCeiling (code.JudgeRead, the data node's judgment too). A
//     read the ENGINE makes on its own account (code.GitQuery.EnginesOwn: a
//     revision's facts, its repository's home's, or a read made for a check,
//     whose box the attempt's row admitted and which Validate holds to the
//     check's commit) names no asker and is held to no grant.
//  6. A PAGE ALREADY CUT is answered from the copy's memo (memo.go) — of
//     every kind but a landed-in read, a revision's facts and a
//     re-anchoring (memoizable): for a read naming whole ids, as soon as its
//     grant is judged and before any git runs; for one naming an
//     abbreviation, once step 7 has expanded it.
//     A hit holds no unit of the share and goes on through steps 9 and 10
//     as a page cut does. A miss of a page ANOTHER READ OF THIS NODE IS
//     CUTTING waits for that read's run and asks the memo again (await): N
//     seats asking one page a push just made — a review's fan-out — run one
//     git, write the page's files once and hold one unit of the share, not N
//     of each; one whose memo still misses runs its own.
//  7. Every abbreviated id is expanded from the objects alone, and every id
//     and path checked present with one batch (objects.go); an id the copy
//     lacks is code.ErrObjectNotFound, while an anchor's path its revision
//     lacks is an answer — the anchor is gone. No refusal of steps 5 and 7 is
//     concluded from a copy not read at the log's head: it is synced there
//     once, and judged again — for a SEAT, while its re-syncs on this node
//     last (Options.Resyncs; past them it is answered from the copy at its
//     floor, saying so). Each of those short processes runs to its own
//     bound, gitproc.ReadDeadline: one that runs all of it, a few ids' check
//     no healthy node takes a second over, is the node's own failure
//     (node_failed).
//  8. The kind's own git runs under ONE unit of the node's seat git-work
//     share, released the moment it ends — never across a sync or a check,
//     which hold no read-lane slot (code.GitShare) — and within ONE BUDGET
//     of git run time, gitproc.ReadDeadline, which every process of the kind
//     shares: each is given what the ones before it left. A read whose own
//     git runs the whole of it is too large to answer (code.ErrSliceTooLarge,
//     naming the narrowing): another node would take as long. That git cuts
//     every page from the one asked to its answer's end, each memoized once
//     git has answered whole (memo.go).
//  9. Every commit the answer names — its revision, a diff's, a
//     comparison's or an interdiff's commits, a landed-in read's commit and
//     tip, a revision's head and target, a re-anchoring's commits, every
//     commit a history's page lists or a blame's page names — is named with
//     the push that introduced it (code.GitAnswer.PushedBy), from the copy's
//     index of pushes, and the answer carries what that index holds
//     (code.Coverage) — measured into every page's envelope, or for a commit
//     a page itself finds, into that commit's cost, so a page cut against it
//     fits with them. A copy whose index cannot be read yet is answered
//     without either: an index decorates an answer, and no read waits on it.
//  10. A read this node answered as the repository's home (code.GitQuery.Home)
//     marks the repository's history wanted (cache.Cache.WantHistory): the one
//     node a walk asks first reads it back, once per fleet, and only for a
//     repository a seat asks about.
//  11. A read this node answered of a `visibility: unit` repository is
//     recorded (Options.Fetched) — as its asker, by its kind, with the
//     answer's encoded size — the memo's answers as its git's: the memo is
//     the repository's copy's, shared by every asker, and a hit read as no
//     read would name only the first asker of a page. A read of a
//     `visibility: company` repository records nothing, since every seat
//     reads it by declaration, and neither does a refused one, nor the
//     engine's own, which nobody made — a check's read among them, whose
//     attempt's row is its record.
//
// ctx ENDS WHEN THE ANSWER IS DUE (code.AnswerBy): every wait above — the
// sync, the unit of the share, the read lane's queue before each process, and
// each process — takes it, and one that it ends answers code.ErrGitUnready
// naming what the read was waiting on, while the asker still listens
// (code.AnswerDue): syncing where it ended the sync, share where it ended a
// wait for the node's git or a run of it. Never ErrSliceTooLarge — a process
// the answer's due instant ended had less than its read's whole budget, which
// a node with more time would have given it — and never ctx's own error,
// which is the answer only where the caller went away. Time the sum of a
// read's budgets leaves out (the read lane's queue, the short checks: see
// estate.GitReadAttempt) comes out of the asker's attempt, and so, under
// load, out of the read's own git: the read is then answered unready rather
// than late or too large.
//
// A STOPPED SERVER ANSWERS EVERY READ UNREADY, node_failed, having started
// nothing, and its stop CUTS every read in flight, which is answered the same
// ([Server.Stop]): about this node, so the walk asks the next git node and lets
// it fill a copy.
func (s *Server) Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	rctx, end, ok := s.begin(ctx)
	if !ok {
		return code.GitAnswer{}, s.stopping()
	}
	defer end()
	a, err := s.serve(rctx, q)
	if err != nil && errors.Is(context.Cause(rctx), errStopped) {
		// CUT BY THE STOP, whatever the wait or the process it cut answered:
		// what a read ended this way says is about the node, never the
		// repository or the asker.
		return code.GitAnswer{}, s.stopping()
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
// (code.UnreadyReason.PassesFill).
func (s *Server) stopping() error {
	return &code.ErrGitUnready{Reason: code.UnreadyNodeFailed, Detail: fmt.Sprintf("%s is stopping its git reads; "+
		"another git node answers, and fills a copy where none holds one", s.o.NodeID)}
}

// Stop ends the server: every read from now on is refused unready, about this
// node, and every read in flight is CUT — its waits and its git processes end
// — and answered the same, and Stop waits for each to return, or for ctx. So
// once Stop has returned no read this server answered is still being
// answered, nor recorded (Options.Fetched): every record of a read is made
// before the code host's stop ends, which the engine's teardown orders before
// the usage day's last flush. Called by whoever stops the code host, BEFORE
// its cache, which a read in flight still reads. Idempotent: a second Stop
// waits for the same reads.
//
// CUT RATHER THAN WAITED OUT, as the host's own requests are at its stop: a
// read runs up to its asker's whole attempt (estate.GitReadAttempt), longer
// than the stop's budget, and a read cut is answered while its asker still
// listens, which takes it to the runner-up at once.
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
				"is recorded after the code host's stop, behind what the node's teardown flushes of its reads")
	}
}

// serve is Read's work, under the context the server's stop cuts.
func (s *Server) serve(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	if err := q.Validate(); err != nil {
		return code.GitAnswer{}, err
	}
	at, err := cursorOf(q)
	if err != nil {
		return code.GitAnswer{}, err
	}
	r := &read{s: s, q: q}
	if err := r.judgeable(); err != nil {
		return code.GitAnswer{}, err
	}
	if err := r.held(); err != nil {
		return code.GitAnswer{}, err
	}
	// THE PIN IS THE READ'S, released whatever it answers: every Synced a
	// sync or a re-sync handed out goes back through r.synced.
	defer func() { r.synced.Release() }()
	if err := r.sync(ctx, r.fresh()); err != nil {
		return code.GitAnswer{}, err
	}
	// A read that leads a page's flight lets those waiting on it go once it
	// has answered, whatever it answered: its pages are placed by then.
	defer func() {
		if r.landing != nil {
			r.landing()
		}
	}()
	a, ok, err := r.recallWhole(ctx, at)
	if err != nil {
		return code.GitAnswer{}, err
	}
	if !ok {
		if a, err = r.compute(ctx, at); err != nil {
			return code.GitAnswer{}, err
		}
	}
	r.stamp(&a, a.Memo)
	if q.Home {
		s.o.Cache.WantHistory(q.RepoID)
	}
	if err := r.audit(ctx, a); err != nil {
		return code.GitAnswer{}, err
	}
	return a, nil
}

// audit is step 11: a read answered of a `visibility: unit` repository,
// recorded as its asker read it — one read, the answer's encoding its bytes,
// which is what the asker is sent beside its envelope. Before the read
// returns, and before the page flight it leads lets its waiters go, which is
// why the engine's Fetched only files it in the node's reads ledger and never
// waits on the broker.
func (r *read) audit(ctx context.Context, a code.GitAnswer) error {
	repo := r.synced.Repo
	if repo.Visibility != code.VisibilityUnit || r.q.EnginesOwn() {
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
func (r *read) compute(ctx context.Context, at cursor) (code.GitAnswer, error) {
	objs, err := r.objects(ctx)
	if err != nil {
		return code.GitAnswer{}, err
	}
	if !r.recalled && memoizable(r.q) {
		whole := wholeOf(r.q, objs)
		if a, ok := r.recall(whole, at); ok {
			return a, nil
		}
		if a, ok, err := r.await(ctx, whole, at); err != nil || ok {
			return a, err
		}
	}
	r.index(commitsOf(r.q, objs))
	release, err := r.s.o.Share.Take(ctx, code.SeatWorkRead)
	if err != nil {
		if code.AnswerDue(ctx) {
			return code.GitAnswer{}, r.unready(code.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole "+
				"share until the read's answer fell due, its asker's attempt spent; another git node answers "+
				"meanwhile", r.s.o.NodeID))
		}
		return code.GitAnswer{}, err
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
func commitsOf(q code.GitQuery, objs found) []string {
	var out []string
	for _, rev := range revsOf(q) {
		if o := objs.revs[rev]; o.peeledKind == "commit" && !slices.Contains(out, o.peeled) {
			out = append(out, o.peeled)
		}
	}
	return out
}

// index reads the pinned copy's index of pushes — once a read — and names the
// push that introduced each of commits (step 9), in place of whatever an
// earlier call named: a hit the memo answers is decorated by the commits its
// page names, and one it could not use by the read's own.
func (r *read) index(commits []string) {
	// AN UNSTAMPED READ READS NO INDEX: it names no push, so nothing of
	// what the index holds may move its page (code.GitQuery.Unstamped). Its
	// commits are named all the same, by nothing, as a stamped read's are
	// where the index holds none of them.
	if !r.indexRead && !r.q.Unstamped {
		if p, err := r.s.o.Cache.Provenance(r.synced); err == nil && p.Indexed() {
			r.prov = p
			cov := p.Coverage()
			r.coverage = &cov
		}
	}
	r.indexRead = true
	r.pushedBy, r.named = nil, nil
	for _, id := range commits {
		r.decorate(id)
	}
}

// decorate names the push that introduced commit id, where the index holds
// it: before any page is cut, since every page's envelope carries it. Every
// commit decorated is named (read.named), held or not, so a page from the
// memo is decorated by what the index holds when it is read.
func (r *read) decorate(id string) {
	r.named = append(r.named, id)
	if !r.prov.Indexed() {
		return
	}
	by, ok := r.prov.Commit(id)
	if !ok {
		return
	}
	if r.pushedBy == nil {
		r.pushedBy = map[string]code.PushedBy{}
	}
	r.pushedBy[id] = by
}

// read is one Read in progress.
type read struct {
	s *Server
	q code.GitQuery
	// roster is the chart a seat is judged by, read once a read so one read
	// is judged by one chart; nil for a person's or a token's read.
	roster code.Roster
	// synced is the pinned copy the read reads, and linear whether it was
	// read at the log's head — an id it lacks then is absent, with no second
	// look.
	synced cache.Synced
	linear bool
	// prov is the copy's index of pushes, the zero one where it keeps none
	// it can read, and indexRead whether it was asked for; pushedBy the push
	// that introduced each commit the answer names, coverage what the index
	// holds — each carried by every page and so measured into its envelope
	// (room) — and named every commit decorated, held by the index or not.
	prov      cache.Provenance
	indexRead bool
	pushedBy  map[string]code.PushedBy
	coverage  *code.Coverage
	named     []string
	// recalled: the memo was asked before any git ran (recallWhole), so it
	// is not asked again once the read's objects are found. landing ends the
	// flight of the page the read leads (await), nil where it leads none.
	recalled bool
	landing  func()
	// metered is set once the read's own git has begun (step 8), and gitLeft
	// is what is left of its budget of run time: each of its processes is
	// given that as its deadline ([read.budget]) and charged what it ran
	// ([read.failed]) — its run alone, never its wait for a read-lane slot,
	// which is the node's contention and not the read's size, and comes out
	// of the asker's attempt instead.
	metered bool
	gitLeft time.Duration
	// filter is an interdiff's files, made once (read.interdiffFilter).
	filter *pathFilter
}

// judgeable refuses a seat's read on a node that cannot judge a seat (step 2).
func (r *read) judgeable() error {
	if r.q.Asker.Kind != code.PrincipalAgent {
		return nil
	}
	if cur := r.s.o.Current; cur != nil && !cur() {
		return r.unready(code.UnreadyBehindConfig, fmt.Sprintf("%s is behind the company's configuration and "+
			"judges no seat's grant from the chart the fleet replaced; a current git node answers", r.s.o.NodeID))
	}
	if r.roster = r.s.o.Roster(); r.roster == nil {
		return r.unready(code.UnreadyNoChart, fmt.Sprintf("%s holds no chart yet to judge seat %q's grant by; "+
			"a git node holding one answers", r.s.o.NodeID, r.q.Asker.Seat))
	}
	return nil
}

// held refuses a read this node may not fill a copy for (step 3): it holds
// none, and the read is not its walk's effective home's. Asked before the
// sync, which is what would start the download: a cold round runs on under
// the cache's own lifetime after the read stops waiting, so a node that
// started one for a read another node answers would download the whole
// repository for nobody.
func (r *read) held() error {
	if r.q.Home || r.s.o.Cache.Holds(r.q.RepoID) {
		return nil
	}
	return r.unready(code.UnreadyNotHeld, fmt.Sprintf("%s holds no copy of repository %s and the read is "+
		"not its walk's home's, which is the one node that fills a copy; a git node holding one answers "+
		"meanwhile", r.s.o.NodeID, r.q.RepoID))
}

// fresh is what the first sync asks: the asker's floor where it names one —
// at the level it names, session where it names none — and otherwise the copy
// as it stands. An object id names one object at every position, so no level
// buys an answer the copy cannot give; what a floor buys is a copy holding the
// asker's own pushes, which step 7's second look covers for every other id.
//
// A LANDED-IN READ is brought to the position its tip's own move landed at
// (code.LandedInQuery.At), at least: the moves of the target up to that tip are
// what its index must hold to name the push that landed the commit there, and
// no move of the target lies between that one and the read that resolved it.
//
// The asker's floor is on the code log: a floor on any other is refused with
// the request ([code.GitQuery.Validate]).
func (r *read) fresh() statelog.Freshness {
	f := r.q.Fresh
	floor := f.Floor(topics.CodeLogStream)
	if r.q.Kind == code.GitLandedIn {
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
	case errors.Is(err, code.ErrRepoNotFound):
		return fmt.Errorf("query: repository %s: %w", r.q.RepoID, err)
	case errors.Is(err, cache.ErrInvalid):
		// The request's own mistake — a floor on another log — which no
		// other node would take either.
		return fmt.Errorf("%w: fresh: %v", code.ErrInvalid, err)
	case code.AnswerDue(ctx):
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the read's need before its answer fell due; the sync goes on, and another git node answers "+
			"meanwhile", r.s.o.NodeID, r.q.RepoID))
	case ctx.Err() != nil:
		return ctx.Err()
	case sctx.Err() != nil:
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the read's need within %s; the sync goes on, and another git node answers meanwhile",
			r.s.o.NodeID, r.q.RepoID, r.s.syncWait))
	case cache.EveryFillMeets(err):
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s could not sync repository %s: %v — what every "+
			"git node's copy is filled from, so a git node holding a copy answers meanwhile",
			r.s.o.NodeID, r.q.RepoID, err))
	default:
		return r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not sync repository %s: %v; another "+
			"git node answers, and fills a copy where none holds one", r.s.o.NodeID, r.q.RepoID, err))
	}
	r.synced.Release()
	r.synced, r.linear = s, fresh.Level == statelog.ReadLinearizable
	return nil
}

// grant answers a repository the read may not be served from (step 5): one
// deleted or purging, one the asker's grant does not let it read, and one the
// grant of the seat it reads for does not (code.JudgeRead).
func (r *read) grant() error {
	repo := r.synced.Repo
	if repo.Status == code.RepoStatusDeleted || repo.Status == code.RepoStatusPurging {
		gone := code.RepoGone{ID: repo.ID, Name: repo.Name, Status: repo.Status, DeletedAt: repo.DeletedAt,
			ManagedBy: repo.ManagedBy}
		return fmt.Errorf("%w: %s", code.ErrRepoNotFound, gone.Detail())
	}
	if r.q.EnginesOwn() {
		// THE ENGINE'S OWN READ, which names no asker — a revision's facts, or
		// a read made for a check, whose attempt's row admitted its box: there
		// is no grant to judge, and judged as nobody it would be refused as
		// nobody.
		return nil
	}
	_, err := code.JudgeRead(r.q.Asker, r.q.CeilingSeat, repo, r.roster)
	return err
}

// objects is steps 5 and 7 — and their ONE second look at the log's head: NO
// REFUSAL IS CONCLUDED FROM A COPY THAT MAY BE BEHIND THE LOG, as the code
// host never refuses a person's git on a stale read. An id the copy lacks may
// be a push the asker knows of and this node has not synced; a repository the
// copy calls deleted, or a grant its row refuses, may be one an activation
// restored or opened since. So a read that would refuse on a copy not read at
// the log's head syncs there once, judges the grant again on the row it
// brings, and only then answers — a refused read costs one barrier, and a
// served one none.
//
// A SEAT'S SECOND LOOK IS ITS TURN'S TO PAY, from its seat's bucket on this
// node (Options.Resyncs, code.NewResyncs): past it the refusal is answered
// from the copy at the seat's floor — which holds what the turn has read and
// this node's own pushes — saying it was not judged again and when the
// seat's re-syncs come back. A read made for a check is its attempt's to pay,
// at the check's session (resync). A person's and the engine's own are never
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
				if err := r.sync(ctx, statelog.Freshness{Level: statelog.ReadLinearizable}); err != nil {
					return found{}, err
				}
				continue
			}
			_, _, principal, scope := r.floorWords()
			note = " " + code.NotRejudged(spent, principal, scope, time.Now())
		}
		if err != nil {
			return found{}, fmt.Errorf("%w%s", err, note)
		}
		if note != "" {
			shown, instead, _, _ := r.floorWords()
			return found{}, fmt.Errorf("%w: repository %s holds no object %s at the position %s; %s %s",
				code.ErrObjectNotFound, r.synced.Repo.Name, absent, shown, instead, note[1:])
		}
		return found{}, fmt.Errorf("%w: repository %s holds no object %s", code.ErrObjectNotFound,
			r.synced.Repo.Name, absent)
	}
}

// resync draws the read's one second look at the log's head from this node's
// buckets: a SEAT'S read, its turn on its seat; a read made FOR A CHECK — a
// check's box's at /box/code/ — its attempt on its repository's checks
// (code.CheckKey.Resyncs), as the box's git is charged at the code host. Nil
// where it is taken, or for a read charged to nobody: a person's, and the
// engine's own on its own account.
func (r *read) resync() *costlimit.Refusal {
	switch {
	case r.q.Check != nil:
		scope, principal := r.q.Check.Resyncs()
		return r.s.o.Resyncs.Take(scope, principal, 1)
	case r.q.Asker.Kind == code.PrincipalAgent:
		return r.s.o.Resyncs.Take(r.q.Asker.Seat, r.q.Turn, 1)
	}
	return nil
}

// floorWords is how a read answered at its floor names it — what it has read,
// what it may do instead, and whose bucket ran out, the principal's and the
// scope's (code.NotRejudged): a seat's turn's, or a check's.
func (r *read) floorWords() (shown, instead, principal, scope string) {
	if r.q.Check != nil {
		return "this check was queued at", "a check's box reads the one commit it checks", "this check's",
			"its repository's checks'"
	}
	return "this turn has read", "it has not been pushed yet, or ask code_resolve", "this turn's", "this seat's"
}

// answer runs the kind's own git over objs (step 8), memoizing every page it
// cuts once that git has answered whole — and none where it did not.
func (r *read) answer(ctx context.Context, objs found, at cursor) (code.GitAnswer, error) {
	m := r.memoRun(objs)
	defer m.discard()
	switch r.q.Kind {
	case code.GitTree:
		return r.treeRead(ctx, objs, at, m)
	case code.GitBlob:
		return r.blobRead(ctx, objs, at, m)
	case code.GitDiff:
		return r.diffRead(ctx, objs, at, m)
	case code.GitLandedIn:
		return r.landedRead(ctx, objs)
	case code.GitRevisionFacts:
		return r.factsRead(ctx, objs)
	case code.GitReanchor:
		return r.reanchorRead(ctx, objs)
	case code.GitInterdiff:
		return r.interdiffRead(ctx, objs, at, m)
	case code.GitLog:
		return r.logRead(ctx, objs, at, m)
	case code.GitBlame:
		return r.blameRead(ctx, objs, at, m)
	case code.GitChecksFile:
		return r.checksFileRead(ctx, objs)
	}
	return r.compareRead(ctx, objs, m)
}

// unready is a refusal about this node, which another git node may answer.
func (r *read) unready(why code.UnreadyReason, detail string) error {
	return &code.ErrGitUnready{Reason: why, Detail: detail}
}

// budget is the deadline the next process of the read's OWN GIT is given: what
// is left of its budget of run time ([read.gitLeft]) — the processes of a kind
// share one, so a comparison's walks and its stat, or a patch's lookup and its
// diff-pairs, run in the time one listing gets. A budget already run out is the
// read too large to answer, said with the generic narrowing: a process's run is
// charged after it ends (failed), so a later one finds it spent only where the
// one before it ran out its last instant.
func (r *read) budget() (time.Duration, error) {
	if r.gitLeft <= 0 {
		return 0, fmt.Errorf("%w: the read of repository %s ran its git's whole %s: narrow it",
			code.ErrSliceTooLarge, r.q.RepoID, r.s.gitBudget)
	}
	return r.gitLeft, nil
}

// failed is what a git process's end means for the read, once the run is
// charged to the read's own git where that has begun ([read.metered]): nil
// for one that answered; unready, syncing, for one that reported damage —
// failed or not, since git answers some questions over damage it reports and
// exits 0 — and for one the pool refused the copy's shape for (the cache
// repairs both: nothing here names a gitproc.Repairer, so the cache's observer
// acts on a report at once); unready, share, for one the read's answer fell
// due during — waiting for a slot or running (code.AnswerDue) — and for a read
// lane busy past gitproc.SlotWait; slice, for a process of the read's own git
// that ran out the whole of its budget, since another node would take as long;
// and unready, node_failed, for one of the short checks before it that ran its
// whole bound, which no healthy node takes over a few ids, so this node is not
// one to wait for and the walk lets the next node fill a copy.
//
// A RUN THE ANSWER'S DUE INSTANT ENDED IS NEVER TOO LARGE: it had less than
// the read's whole budget — the sync, the checks and the read lane's queue
// took the rest of the asker's attempt — and a node with that time to spare
// would have given it all of it. Refused as too large, a read that only lost
// its time to contention told its seat to narrow a question a quiet node
// answers.
func (r *read) failed(ctx context.Context, what string, res gitproc.Result, err error, slice error) error {
	r.charge(res)
	if res.Damage != nil {
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s met damage the node is "+
			"repairing (%v); another git node answers meanwhile", r.s.o.NodeID, what, r.q.RepoID, res.Damage))
	}
	switch {
	case err == nil:
		return nil
	case code.AnswerDue(ctx):
		return r.unready(code.UnreadyShare, fmt.Sprintf("%s could not finish the %s over repository %s before "+
			"the read's answer fell due — its asker's attempt spent on the waits before it; another git node "+
			"answers meanwhile", r.s.o.NodeID, what, r.q.RepoID))
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, gitproc.ErrBusy):
		return r.unready(code.UnreadyShare, fmt.Sprintf("%s's read lane was busy past %s; another git node "+
			"answers meanwhile", r.s.o.NodeID, gitproc.SlotWait))
	case errors.Is(err, gitproc.ErrDeadline) && r.metered:
		if slice == nil {
			return fmt.Errorf("%w: the %s over repository %s ran the read's whole %s of git: narrow it",
				code.ErrSliceTooLarge, what, r.q.RepoID, r.s.gitBudget)
		}
		return slice
	case errors.Is(err, gitproc.ErrDeadline):
		return r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s's %s over repository %s ran its whole bound, "+
			"which no healthy git node takes over it: %v; another git node answers", r.s.o.NodeID, what,
			r.q.RepoID, err))
	case errors.Is(err, gitproc.ErrRepoShape):
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s did not complete: %v; "+
			"another git node answers meanwhile", r.s.o.NodeID, what, r.q.RepoID, err))
	}
	return fmt.Errorf("query: %s over repository %s: %w", what, r.q.RepoID, err)
}

// charge counts a process's run against the read's budget of git once the
// read's own git has begun ([read.metered]) — its run alone, never its wait
// for a read-lane slot: what [read.failed] charges every process it judges,
// and a run the read ends itself, having read all it needs (errStop), is
// charged here, since it judges none.
func (r *read) charge(res gitproc.Result) {
	if r.metered {
		r.gitLeft -= res.Took
	}
}

// cursorOf is q's cursor, read as one its kind issues.
func cursorOf(q code.GitQuery) (cursor, error) {
	switch q.Kind {
	case code.GitTree:
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'e': 1})
	case code.GitBlob:
		// A file read of a directory answers its listing, whose cursor is
		// an entry's.
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'l': 1, 'b': 2, 'e': 1})
	case code.GitDiff:
		if q.Diff.File != "" || q.Diff.FileB64 != "" {
			return parseCursor(q.Cursor, q.Kind, map[byte]int{'p': 1})
		}
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'f': 1})
	case code.GitInterdiff:
		if q.Interdiff.File != "" || q.Interdiff.FileB64 != "" {
			return parseCursor(q.Cursor, q.Kind, map[byte]int{'p': 1})
		}
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'f': 1})
	case code.GitLog:
		// A history's page begins past the commits of the walk the pages
		// before it covered.
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'s': 1})
	case code.GitBlame:
		return parseCursor(q.Cursor, q.Kind, map[byte]int{'l': 1})
	}
	// A comparison answers its first page of files and no more — a diff of
	// its merge base and head reads the rest — a landed-in read one answer
	// of a few ids, a revision's facts one of its ids and a bounded list, and
	// a re-anchoring one of its bounded anchors.
	return parseCursor(q.Cursor, q.Kind, map[byte]int{})
}
