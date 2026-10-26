package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

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
	// Roster is the chart a seat's grant is judged by: the serving node's
	// CURRENT epoch's, read per read. A nil answer is a node with no chart
	// yet, which judges no seat (code.UnreadyNoChart).
	Roster func() code.Roster
	// Current reports whether this node serves the company's configuration:
	// false on a node confirmed behind it (shed or stuck), which judges no
	// seat's grant from the chart the fleet replaced
	// (code.UnreadyBehindConfig). nil is current.
	Current func() bool
}

// Server answers reads of repositories by object id over the node's cache.
// It holds no goroutine of its own: each read runs on its caller's, and ends
// with its caller's context.
type Server struct {
	o Options
	// syncWait bounds a read's wait on its sync (statelog.ReadBudget), and
	// pageBytes an answer's JSON (code.MaxGitAnswerBytes) — fields so this
	// package's tests can scale them (export_test.go).
	syncWait  time.Duration
	pageBytes int
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
	case o.Roster == nil:
		missing = "Roster"
	}
	if missing != "" {
		return nil, fmt.Errorf("%w: query.Options.%s is required", code.ErrInvalid, missing)
	}
	return &Server{o: o, syncWait: statelog.ReadBudget, pageBytes: code.MaxGitAnswerBytes}, nil
}

// Read answers q: a page of the tree, file, diff or comparison it names.
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
//     failed here is unready (syncing).
//  5. A deleted or purging repository answers code.ErrRepoNotFound with the
//     domain's own sentence for it; the asker's grant is judged from the
//     repository's row and this node's chart (code.GrantOf), and a read below
//     read is code.ErrNotPermitted.
//  6. Every abbreviated id is expanded from the objects alone, and every id
//     and path checked present with one batch (objects.go); an id the copy
//     lacks is code.ErrObjectNotFound. No refusal of steps 5 and 6 is
//     concluded from a copy not read at the log's head: it is synced there
//     once, and judged again.
//  7. The kind's own git runs under ONE unit of the node's seat git-work
//     share, released the moment it ends — never across a sync or a check,
//     which hold no read-lane slot (code.GitShare).
func (s *Server) Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
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
	objs, err := r.objects(ctx)
	if err != nil {
		return code.GitAnswer{}, err
	}
	release, err := s.o.Share.Take(ctx, code.SeatWorkRead)
	if err != nil {
		return code.GitAnswer{}, err
	}
	defer release()
	a, err := r.answer(ctx, objs, at)
	release()
	if err != nil {
		return code.GitAnswer{}, err
	}
	a.RepoID, a.Position = q.RepoID, r.synced.Position
	return a, nil
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
// asker's own pushes, which step 5's second look covers for every other id.
func (r *read) fresh() statelog.Freshness {
	f := r.q.Fresh
	if f.MinPosition.IsZero() {
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	if f.Level == "" {
		f.Level = statelog.ReadSession
	}
	return f
}

// sync pins the copy at fresh, in place of any copy the read holds, its wait
// bounded by the read's sync budget and the request's own deadline less the
// reply's margin.
func (r *read) sync(ctx context.Context, fresh statelog.Freshness) error {
	wait, err := r.bound(ctx, r.s.syncWait)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	s, err := r.s.o.Cache.Sync(sctx, r.q.RepoID, fresh)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, code.ErrRepoNotFound):
		return fmt.Errorf("query: repository %s: %w", r.q.RepoID, err)
	case errors.Is(err, cache.ErrInvalid):
		// The request's own mistake — a floor on another log — which no
		// other node would take either.
		return fmt.Errorf("%w: fresh: %v", code.ErrInvalid, err)
	case sctx.Err() != nil:
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s is syncing repository %s, which did not reach "+
			"the read's need within %s; the sync goes on, and another git node answers meanwhile",
			r.s.o.NodeID, r.q.RepoID, wait))
	default:
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s could not sync repository %s: %v; another git "+
			"node answers meanwhile", r.s.o.NodeID, r.q.RepoID, err))
	}
	r.synced.Release()
	r.synced, r.linear = s, fresh.Level == statelog.ReadLinearizable
	return nil
}

// grant answers a repository the read may not be served from (step 4): one
// deleted or purging, and one the asker's grant does not let it read.
func (r *read) grant() error {
	repo := r.synced.Repo
	if repo.Status == code.RepoStatusDeleted || repo.Status == code.RepoStatusPurging {
		gone := code.RepoGone{ID: repo.ID, Name: repo.Name, Status: repo.Status, DeletedAt: repo.DeletedAt}
		return fmt.Errorf("%w: %s", code.ErrRepoNotFound, gone.Detail())
	}
	if g := code.GrantOf(r.q.Asker, repo, r.roster); !g.MayRead() {
		return fmt.Errorf("%w: %s", code.ErrNotPermitted, g.ReadRefusal())
	}
	return nil
}

// objects is steps 4 and 5 — and their ONE second look at the log's head: NO
// REFUSAL IS CONCLUDED FROM A COPY THAT MAY BE BEHIND THE LOG, as the code
// host never refuses a person's git on a stale read. An id the copy lacks may
// be a push the asker knows of and this node has not synced; a repository the
// copy calls deleted, or a grant its row refuses, may be one an activation
// restored or opened since. So a read that would refuse on a copy not read at
// the log's head syncs there once, judges the grant again on the row it
// brings, and only then answers — a refused read costs one barrier, and a
// served one none.
func (r *read) objects(ctx context.Context) (found, error) {
	for {
		err := r.grant()
		if err == nil {
			objs, absent, ferr := r.find(ctx)
			switch {
			case ferr != nil:
				return found{}, ferr
			case absent == "":
				return objs, nil
			case r.linear:
				return found{}, fmt.Errorf("%w: repository %s holds no object %s", code.ErrObjectNotFound,
					r.synced.Repo.Name, absent)
			}
		} else if r.linear {
			return found{}, err
		}
		if err := r.sync(ctx, statelog.Freshness{Level: statelog.ReadLinearizable}); err != nil {
			return found{}, err
		}
	}
}

// answer runs the kind's own git over objs (step 6).
func (r *read) answer(ctx context.Context, objs found, at cursor) (code.GitAnswer, error) {
	switch r.q.Kind {
	case code.GitTree:
		return r.treeRead(ctx, objs, at)
	case code.GitBlob:
		return r.blobRead(ctx, objs, at)
	case code.GitDiff:
		return r.diffRead(ctx, objs, at)
	}
	return r.compareRead(ctx, objs)
}

// unready is a refusal about this node, which another git node may answer.
func (r *read) unready(why code.UnreadyReason, detail string) error {
	return &code.ErrGitUnready{Reason: why, Detail: detail}
}

// bound is limit, cut to the request's deadline less the reply's margin
// (code.ReplyMargin), so a node that runs out of time says so while its asker
// still listens; a request with no time left for it is its own deadline,
// passed.
func (r *read) bound(ctx context.Context, limit time.Duration) (time.Duration, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		return limit, nil
	}
	left := time.Until(dl) - code.ReplyMargin
	if left <= 0 {
		return 0, fmt.Errorf("query: repository %s: the request's deadline leaves no time to read: %w",
			r.q.RepoID, context.DeadlineExceeded)
	}
	return min(limit, left), nil
}

// failed is what a git process's end means for the read: nil for one that
// answered; unready, syncing, for one that reported damage — failed or not,
// since git answers some questions over damage it reports and exits 0 — and
// for one the pool refused the copy's shape for (the cache repairs both:
// nothing here names a gitproc.Repairer, so the cache's observer acts on a
// report at once); unready, share, for a read lane busy past gitproc.SlotWait;
// and for a run its deadline ended, slice when the read can be narrowed —
// another node would take as long — and unready otherwise, a short process no
// healthy node takes that long over.
func (r *read) failed(ctx context.Context, what string, res gitproc.Result, err error, slice error) error {
	if res.Damage != nil {
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s met damage the node is "+
			"repairing (%v); another git node answers meanwhile", r.s.o.NodeID, what, r.q.RepoID, res.Damage))
	}
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, gitproc.ErrBusy):
		return r.unready(code.UnreadyShare, fmt.Sprintf("%s's read lane was busy past %s; another git node "+
			"answers meanwhile", r.s.o.NodeID, gitproc.SlotWait))
	case errors.Is(err, gitproc.ErrDeadline) && slice != nil:
		return slice
	case errors.Is(err, gitproc.ErrDeadline), errors.Is(err, gitproc.ErrRepoShape):
		return r.unready(code.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s did not complete: %v; "+
			"another git node answers meanwhile", r.s.o.NodeID, what, r.q.RepoID, err))
	}
	return fmt.Errorf("query: %s over repository %s: %w", what, r.q.RepoID, err)
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
	}
	// A comparison answers its first page of files and no more: a diff of
	// its merge base and head reads the rest.
	return parseCursor(q.Cursor, q.Kind, map[byte]int{})
}
