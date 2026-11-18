package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// Reader answers questions about Gitlet from this node's own applied rows.
//
// EVERY READ GOES THROUGH [statelog.Reader], so a level is a property of the
// answer: the refusal ladder, the coverage probe over what the read is ABOUT,
// the freshness target, then the rows in ONE transaction.
//
// AND THE POSITION IS THE ROWS' OWN: every read is stamped with the checkpoint
// [statelog.Reader.ReadThrough] hands its transaction — the checkpoint row
// carried over the barriers reached at release. The release position alone can
// be OLDER than the rows; the row alone LAGS decoded, uncommitted barriers, so a
// linearizable read could stamp below another node's earlier answer, which a
// cache takes for its log gone back.
type Reader struct {
	log    *statelog.Reader
	limits func() Limits
	roster func() Roster
	now    func() time.Time

	// pageRows and pageBytes bound one page: MaxSnapshotItems and
	// MaxPageBytes, and a test's smaller values (export_test.go).
	pageRows  int
	pageBytes int
	// tipWindow bounds the landings one target's tip work reads above its
	// green: MaxTipLandings, and a test's smaller value (export_test.go).
	tipWindow int
	// reposSized and lastPushWalk bound a listing's total and its cards: MaxReposSized and
	// MaxLastPushWalk, or a test's.
	reposSized, lastPushWalk int
	// refsScanned and activeCounted bound a refs listing's scan and its active
	// count: MaxRefsScanned and MaxActiveBranchesCounted, or a test's.
	refsScanned, activeCounted int
	// pushesScanned bounds a narrowed push history's page: MaxPushesScanned, or a test's.
	pushesScanned int
	// spokenScanned bounds a live state's conversation side: MaxSpokenScanned, or a test's.
	spokenScanned int
	// standingCounted bounds a standing count's places a state: MaxStandingCounted, or a test's.
	standingCounted int
	// queueCounted bounds a queue read's and a counts read's walks: MaxQueueCounted, or a test's.
	queueCounted int
	// words is this node's index of changes' words, nil where it keeps none.
	words ChangeWords
	// textCandidates and textTail bound a words listing: MaxChangeTextCandidates and MaxChangeTextTail,
	// or a test's.
	textCandidates, textTail int
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	// Log is the code log's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader

	// Limits are the epoch's code.limits, for RepoDetail alone; nil reads as
	// [DefaultLimits].
	Limits func() Limits

	// Roster is the SERVING node's current epoch's chart (the engine's
	// codeRoster), read once a read: an agent asker is judged by it
	// ([GrantOf]). Nil, or a roster answering nil, refuses every agent's read
	// [ErrNotServing] so the estate asks a node holding a chart
	// ([Reader.chartFor]). A person's read asks nothing of it.
	Roster func() Roster

	// Now is the serving node's clock, which a change's routing is dated by
	// ([RoutedPage.RetainedFrom]): its notices are kept by THIS node's sweeps.
	// Nil reads the wall clock.
	Now func() time.Time

	// ChangeWords is this node's index of changes' words; nil where it keeps none, and a listing the
	// words lead is then refused [ErrNotServing], so the estate asks a node that keeps one.
	ChangeWords ChangeWords
}

// NewReader builds Gitlet's read side.
func NewReader(o ReaderOptions) (*Reader, error) {
	if o.Log == nil {
		return nil, errors.New("gitlet: a reader needs the code log's read authority — " +
			"without it every read level is a label rather than a guarantee")
	}
	r := &Reader{log: o.Log, limits: o.Limits, roster: o.Roster, now: o.Now, pageRows: MaxSnapshotItems,
		pageBytes: MaxPageBytes, tipWindow: MaxTipLandings, refsScanned: MaxRefsScanned,
		activeCounted: MaxActiveBranchesCounted, spokenScanned: MaxSpokenScanned}
	r.pushesScanned = MaxPushesScanned
	if r.limits == nil {
		r.limits = DefaultLimits
	}
	r.standingCounted = MaxStandingCounted
	if r.now == nil {
		r.now = time.Now
	}
	r.reposSized, r.lastPushWalk = MaxReposSized, MaxLastPushWalk
	r.queueCounted = MaxQueueCounted
	r.words, r.textCandidates, r.textTail = o.ChangeWords, MaxChangeTextCandidates, MaxChangeTextTail
	return r, nil
}

// chartFor is the roster a read by asker is judged by: the serving node's
// current chart for a seat's read and for every read judging a change's
// readiness ([ReadQuery.judgesReadiness]), read ONCE a read so a listing is
// judged by one chart, and none for anything else.
//
// A READER HOLDING NO CHART (from boot until its first epoch installs) REFUSES
// such a read [ErrNotServing] before any row, never judges it: judged with no
// roster every seat stands nowhere and no approval counts, an ANSWER that ends
// the estate's walk with what every peer holding a chart would contradict.
// Refused, the node ran nothing (estate's ranNothing) and the walk asks the
// next data node. A chart that DENIES the seat is still the answer.
func (r *Reader) chartFor(asker Principal, readiness bool) (Roster, error) {
	if asker.Kind != PrincipalAgent && !readiness {
		return nil, nil
	}
	var chart Roster
	if r.roster != nil {
		chart = r.roster()
	}
	switch {
	case chart != nil:
		return chart, nil
	case asker.Kind == PrincipalAgent:
		return nil, fmt.Errorf("%w: this node holds no chart to judge a seat's read by", ErrNotServing)
	}
	return nil, fmt.Errorf("%w: this node holds no chart to judge a change's readiness by", ErrNotServing)
}

// judgesReadiness reports a read whose answer carries a change's readiness,
// which the chart judges: a listing of changes, and a change's first page of
// its revisions, which carries its detail.
func (q ReadQuery) judgesReadiness() bool {
	return q.Kind == ReadChanges || q.Kind == ReadChange && q.Cursor == "" && q.pagesDetail() && !q.Brief
}

// resolves reports a read that may resolve the repository it names
// ([ReadQuery.Resolve]): one that names it by name and judges the asker's
// grant on it — a `change` read, and a `changes` or a `checks` listing naming
// one repository.
func (q ReadQuery) resolves() bool {
	return q.Kind == ReadChange || (q.Kind == ReadChanges || q.Kind == ReadChecks) && q.Repo != ""
}

// inTx is one open transaction as a statelog.ReadEstate, so the checkpoint is
// read with the rows.
type inTx struct{ tx *sql.Tx }

func (t inTx) Read(_ context.Context, fn func(*sql.Tx) error) error { return fn(t.tx) }

// checkpointIn is the code log's checkpoint row in tx, for a WRITE's decision
// and a node's read-back of its own rows, which the read authority does not
// serve. A read through the authority uses its handed stamp instead: this row
// lags the barriers the read was released past ([Reader]).
func checkpointIn(ctx context.Context, tx *sql.Tx) (statelog.Checkpoint, error) {
	cp, _, err := statelog.CheckpointOf(ctx, inTx{tx}, topics.CodeLogStream)
	return cp, err
}

// unpacked is a packed position on the code log.
func unpacked(packed int64) statelog.Position {
	return statelog.Position{
		Stream:     topics.CodeLogStream,
		Generation: uint32(packed / statelog.GenerationStride),
		Seq:        uint64(packed % statelog.GenerationStride),
	}
}

// checkFreshness is EVERY code read's one rule for its freshness: any level
// but an EMPTY one (a cache's sync and a git request each need a specific one;
// [ReadQuery.freshness] fills the REST path's), and no session read naming no
// code-log position, which would wait for nothing. A floor on ANOTHER log is
// refused first ([statelog.ErrForeignPosition]), so a session read naming only
// such a floor is told which floor is misplaced.
func checkFreshness(f statelog.Freshness) error {
	switch {
	case f.Level == "":
		return fmt.Errorf("%w: a code read states its freshness", ErrBadLevel)
	case !f.Level.Valid():
		return fmt.Errorf("%w: %q is not a read level", ErrBadLevel, f.Level)
	}
	if err := f.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("gitlet: this read's floor: %w", err)
	}
	if f.Level == statelog.ReadSession && f.Floor(topics.CodeLogStream).IsZero() {
		return fmt.Errorf("%w: a session read names the position it must include", ErrBadLevel)
	}
	return nil
}

// resolver is the scope a read naming a repository by NAME is about, read in
// the read's own transaction: its name claim, and the repository the claim
// names when there is one.
func resolver(name string) statelog.Resolver {
	token := NameToken(name)
	return func(ctx context.Context, tx *sql.Tx) (statelog.ScopeSet, error) {
		var id string
		err := tx.QueryRowContext(ctx, claimSQL, token).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return statelog.ScopeSet{Paths: []string{NamePath(token)}}, nil
		case err != nil:
			return statelog.ScopeSet{}, fmt.Errorf("gitlet: resolve %q: %w", name, err)
		}
		return statelog.ScopeSet{Paths: []string{NamePath(token), RepoPath(id)}}, nil
	}
}

// claimIn is the repository a name names in tx.
func claimIn(ctx context.Context, tx *sql.Tx, name string) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, claimSQL, NameToken(name)).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("gitlet: read the claim on %q: %w", name, err)
	}
	return id, true, nil
}

// repoView is a repository's row as the reads serve it.
type repoView struct {
	ref               RepoRef
	branches, tags    int64
	tombstones        int64
	tombstonesThrough int64
	// retiredThrough is the highest position a retire_sweep removed the
	// repository's retired rows at or below: below it, the rows that told of
	// a retirement may be gone.
	retiredThrough int64
	// layersPosition is the position of the last record that changed the
	// repository's published chain: a delta from below it serves the chain.
	layersPosition int64
	pushes         int64
	lastPushAt     sql.NullInt64
	createdAt      int64
}

// summary is the view as a listing renders it.
func (v repoView) summary(head string) RepoSummary {
	s := RepoSummary{RepoRef: v.ref, Head: head, Pushes: v.pushes}
	if v.lastPushAt.Valid {
		s.LastPushAt = store.DecodeTime(v.lastPushAt.Int64)
	}
	return s
}

// activityAt is a repository's idle clock as a read serves it: the instant its
// row holds, or none where it holds none.
func activityAt(encoded int64) time.Time {
	if encoded == 0 {
		return time.Time{}
	}
	return store.DecodeTime(encoded)
}

// namedIn reads the repository a name names in tx, at the checkpoint cp — and
// where the name is an OLD one still redirecting to it after a rename, says so
// ([RepoRef.Redirect]): the repository's current name and when the redirect
// ends. False where nothing holds the name, or its row is gone.
func namedIn(ctx context.Context, tx *sql.Tx, name string, cp statelog.Position) (repoView, bool, error) {
	id, redirect, found, err := claimRowIn(ctx, tx, NameToken(name))
	if err != nil || !found {
		return repoView{}, false, err
	}
	v, found, err := repoViewIn(ctx, tx, id, cp)
	if err != nil || !found {
		return repoView{}, false, err
	}
	if redirect.Valid && v.ref.Name != name {
		v.ref.Redirect, v.ref.RedirectUntil = v.ref.Name, store.DecodeTime(redirect.Int64)
	}
	return v, true, nil
}

// repoViewIn reads one repository and its rules as a push is judged by them
// ([pushRulesIn]: no gate) in tx, at the checkpoint cp. A decide that rewrites
// the rules reads them whole beside it ([protectionsIn]).
func repoViewIn(ctx context.Context, tx *sql.Tx, id string, cp statelog.Position) (repoView, bool, error) {
	var (
		v                                                 repoView
		status, managed, kind, owners, visibility, search string
		deleted                                           sql.NullInt64
		active                                            int64
	)
	err := tx.QueryRowContext(ctx, repoViewSQL, id).Scan(&v.ref.ID, &v.ref.Name, &status, &managed, &kind,
		&owners, &v.ref.Owner.Ref, &visibility, &search, &v.ref.Mirrored, &v.ref.DefaultBranch,
		&v.ref.Description, &deleted, &active, &v.ref.Refs, &v.branches, &v.tags, &v.tombstones,
		&v.ref.LivePacks, &v.ref.LiveBytes,
		&v.ref.LiveObjects, &v.ref.DropEpoch, &v.tombstonesThrough, &v.retiredThrough, &v.layersPosition, &v.pushes,
		&v.lastPushAt, &v.createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoView{}, false, nil
	case err != nil:
		return repoView{}, false, fmt.Errorf("gitlet: read repository %s: %w", id, err)
	}
	v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
	v.ref.Kind, v.ref.Visibility, v.ref.Search = RepoKind(kind), Visibility(visibility), SearchMode(search)
	v.ref.LastActivityAt = activityAt(active)
	if deleted.Valid {
		v.ref.DeletedAt = store.DecodeTime(deleted.Int64)
	}
	if v.ref.Protections, err = pushRulesIn(ctx, tx, id); err != nil {
		return repoView{}, false, err
	}
	v.ref.Position = cp
	return v, true, nil
}

// Resolve answers the repository a name names, deleted and purging ones
// included: the caller answers such a one with [RepoGone]'s sentence (when its
// purge MAY BEGIN, or that nothing brings it back), never a "restorable until"
// deadline nothing holds the purge to.
//
// AN OLD NAME a rename left redirecting answers the repository it redirects
// to, with [RepoRef.Redirect] naming its current name (a clone or fetch is
// served, a push refused naming the new name), until the old name's row is
// released.
func (r *Reader) Resolve(ctx context.Context, q ResolveQuery) (RepoRef, error) {
	if err := checkFreshness(q.Fresh); err != nil {
		return RepoRef{}, err
	}
	if !ValidName(q.Name) {
		return RepoRef{}, refusedf("%w: name: %q is not a repository name", ErrInvalid, q.Name)
	}
	var out RepoRef
	query := q.Fresh.Resolved(resolver(q.Name), false)
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		v, found, err := namedIn(ctx, tx, q.Name, cp.At)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Name)
		}
		out = v.ref
		return nil
	})
	if err != nil {
		return RepoRef{}, err
	}
	return out, nil
}

// page is one page's budget as it fills: at least one row, full at its row
// limit or once its rows' JSON reaches its byte budget, so it never carries
// more than its budget and one row. A row's size is its computed length
// (rowbytes.go).
type page struct {
	rows, bytes   int
	maxRows, maxB int
}

// add counts in one row of the given encoded size and reports whether the
// page is now full.
func (p *page) add(bytes int) bool {
	p.rows++
	p.bytes += bytes
	return p.full()
}

// charge counts in bytes a page read beside what it sends, and no row.
func (p *page) charge(bytes int) { p.bytes += bytes }

// full reports a page that takes no more rows.
func (p *page) full() bool { return p.rows >= p.maxRows || p.bytes >= p.maxB }

// left is how many rows the page may still take.
func (p *page) left() int { return p.maxRows - p.rows }

// Snapshot answers one page of a repository's refs and packs, in full or as a
// delta from q.Since; page 1 decides which ([choose]). Phases: the published
// chain FIRST, whole on page 1 alone (a full answer, or a delta whose floor is
// below layers_position), its packs live as only the record replacing a layer
// retires them; packs; refs; a delta's pushes; RETIRED packs in (floor, page
// 1's checkpoint], from Since or, on a full answer, q.RetiredSince so a
// restarted cache can tell a retired file from an unaccounted one; then a
// multi-page TAIL of packs added after page 1 (a pack served twice is
// harmless), since a later ref may name one. Later pages read at session
// floored at the cursor's Seen and re-check stream, generation, sweeps and drop
// epoch ([Proposal.DropEpoch]), else [ErrSnapshotMoved]. Retirements have no
// tail: the retiring record added its objects' packs. Position is page 1's.
func (r *Reader) Snapshot(ctx context.Context, q StateQuery) (RepoSnapshot, error) {
	if q.RepoID == "" {
		return RepoSnapshot{}, refusedf("%w: repo_id: a snapshot names its repository", ErrInvalid)
	}
	scope := statelog.ScopeSet{Paths: []string{RepoPath(q.RepoID)}}
	first := q.Cursor == nil
	var query statelog.Query
	if first {
		if err := checkFreshness(q.Fresh); err != nil {
			return RepoSnapshot{}, err
		}
		query = q.Fresh.Query(scope, false)
	} else {
		if err := checkCursor(q.Cursor); err != nil {
			return RepoSnapshot{}, err
		}
		query = statelog.FloorAt(statelog.ReadSession, q.Cursor.Seen).Query(scope, false)
	}
	var out RepoSnapshot
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = RepoSnapshot{}
		v, found, err := repoViewIn(ctx, tx, q.RepoID, cp.At)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.RepoID)
		}
		var cur StateCursor
		if first {
			cur = StateCursor{Phase: phasePacks, From: cp.At, Seen: cp.At, Stream: cp.KeyedTo,
				DropEpoch: v.ref.DropEpoch}
			if cur.Full, cur.RetiredFrom, err = choose(ctx, tx, q, cp.At, v); err != nil {
				return err
			}
			if !cur.Full {
				cur.Since, cur.Pushes = q.Since, q.Pushes
			}
		} else {
			cur = *q.Cursor
			switch {
			case !cp.KeyedTo.Equal(cur.Stream) || cp.At.Generation != cur.From.Generation:
				return fmt.Errorf("%w: the log under %s was recreated or reanchored", ErrSnapshotMoved, q.RepoID)
			case !cur.Full && cur.Since.Packed() < v.tombstonesThrough:
				return fmt.Errorf("%w: a deletion %s's delta needed was swept", ErrSnapshotMoved, q.RepoID)
			case !cur.RetiredFrom.IsZero() && cur.RetiredFrom.Packed() < v.retiredThrough:
				return fmt.Errorf("%w: a retirement %s's snapshot serves was swept", ErrSnapshotMoved, q.RepoID)
			case v.ref.DropEpoch != cur.DropEpoch:
				return fmt.Errorf("%w: an expunge began on %s under the snapshot (drop epoch %d, now %d)",
					ErrSnapshotMoved, q.RepoID, cur.DropEpoch, v.ref.DropEpoch)
			}
			cur.Seen = cp.At
		}
		out.Repo, out.Position, out.StreamCreated, out.Full = v.ref, cur.From, cur.Stream, cur.Full
		out.RetiredFrom = cur.RetiredFrom
		layers := first && (cur.Full || v.layersPosition > cur.Since.Packed())
		next, err := r.fill(ctx, tx, q.RepoID, &out, cur, first, layers)
		if err != nil {
			return err
		}
		out.Next = next
		return nil
	})
	if err != nil {
		return RepoSnapshot{}, err
	}
	return out, nil
}

// choose is page 1's choice between a delta from q.Since and the full answer,
// with the floor the retired phase serves from: Since on a delta; on a full
// answer q.RetiredSince where it can serve EVERY retirement above it, else zero.
//
// A delta needs a floor whose history is whole: on this generation of the log
// and at or past both the tombstone and the retired marks, below which a sweep
// removed rows telling of deletions or retirements. A full answer serves its
// caller's retirements only all or none (this generation, past the retired
// mark, at most [MaxFullRetirements]), so a RetiredFrom always names a COMPLETE
// floor. The answers are weighed like with like ([deltaOutweighs]): retirements
// both would serve cancel, so a cache asking from its own position is not sent
// a full answer for retirements it would get anyway. Each floor is counted once.
func choose(ctx context.Context, tx *sql.Tx, q StateQuery, at statelog.Position, v repoView) (
	bool, statelog.Position, error) {

	whole := v.ref.Refs + v.ref.LivePacks
	delta := onLog(q.Since, at) && q.Since.Packed() >= v.tombstonesThrough && q.Since.Packed() >= v.retiredThrough
	same := delta && q.RetiredSince == q.Since
	var (
		served int64
		from   statelog.Position
	)
	if !same && onLog(q.RetiredSince, at) && q.RetiredSince.Packed() >= v.retiredThrough {
		above, err := retiredAbove(ctx, tx, q.RepoID, q.RetiredSince, MaxFullRetirements+1)
		if err != nil {
			return false, statelog.Position{}, err
		}
		if above <= MaxFullRetirements {
			served, from = above, q.RetiredSince
		}
	}
	if !delta {
		return true, from, nil
	}
	depth := whole + served + 1
	if same {
		depth = max(MaxFullRetirements, whole) + 1
	}
	retired, err := retiredAbove(ctx, tx, q.RepoID, q.Since, depth)
	if err != nil {
		return false, statelog.Position{}, err
	}
	if same && retired <= MaxFullRetirements {
		served, from = retired, q.Since
	}
	full, err := deltaOutweighs(ctx, tx, q.RepoID, q.Since.Packed(), retired, whole, served)
	switch {
	case err != nil:
		return false, statelog.Position{}, err
	case full:
		return true, from, nil
	}
	return false, q.Since, nil
}

// onLog reports whether p is a position on the generation of the code log that
// at, page 1's checkpoint, is on: the only positions a row's position orders
// against.
func onLog(p, at statelog.Position) bool {
	return !p.IsZero() && p.Stream == topics.CodeLogStream && p.Generation == at.Generation
}

// retiredAbove counts repoID's retirements above floor, at most depth of them.
func retiredAbove(ctx context.Context, tx *sql.Tx, repoID string, floor statelog.Position, depth int64) (
	int64, error) {

	var n int64
	if err := tx.QueryRowContext(ctx, retiredAboveSQL, repoID, floor.Packed(), depth).Scan(&n); err != nil {
		return 0, fmt.Errorf("gitlet: count %s's retirements above %s: %w", repoID, floor, err)
	}
	return n, nil
}

// deltaOutweighs reports whether a delta from since (its refs moved, packs
// added and retired retirements) would carry more ROWS than the full answer
// (whole: live refs and packs, plus the served retirements, [choose]); then
// the full answer is served. A delta carries TOMBSTONES and a full answer only
// live refs, so without this a cache behind a mass ref deletion would be handed
// more rows than the repository holds, and a cache holds a snapshot in memory.
// Packs only favour the delta. The delta's rows are counted at most one past
// the room left, over code_refs_changed_idx and code_packs_added_idx. A tie is
// a delta: applied in place rather than sweeping the cache's directory. Rows,
// not bytes: only reading every name could sum bytes, so a delta of long
// deleted names may be wider than its full answer, by at most what
// [MaxTombstonesPerRepo] computes.
func deltaOutweighs(ctx context.Context, tx *sql.Tx, repoID string, since, retired, whole, served int64) (
	bool, error) {

	room := whole + served - retired
	if room < 0 {
		return true, nil
	}
	var moved int64
	if err := tx.QueryRowContext(ctx, deltaRowsSQL, repoID, since, room+1).Scan(&moved); err != nil {
		return false, fmt.Errorf("gitlet: count %s's delta: %w", repoID, err)
	}
	return moved > room, nil
}

// fill serves the snapshot's phases from cur, in order, until the page is
// full or the last phase completes, and answers where the next page resumes —
// nil when the snapshot is complete. layers serves the published chain first,
// whole, which only page 1 is asked to.
func (r *Reader) fill(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	cur StateCursor, first, layers bool) (*StateCursor, error) {

	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	if layers {
		if err := r.layersPhase(ctx, tx, repoID, out, &pg); err != nil {
			return nil, err
		}
		if pg.full() {
			// ONLY UNDER A TEST'S PAGE: the widest chain fits a page with
			// room, and a page the chain filled resumes at its packs.
			next := cur
			return &next, nil
		}
	}
	for {
		var done bool
		var err error
		switch cur.Phase {
		case phasePacks:
			floor := cur.Since.Packed()
			if cur.Full {
				done, err = r.packsFull(ctx, tx, repoID, out, &cur, &pg)
			} else {
				done, err = r.packsAfter(ctx, tx, repoID, floor, out, &cur, &pg)
			}
		case phaseRefs:
			done, err = r.refs(ctx, tx, repoID, out, &cur, &pg)
		case phasePushes:
			done, err = r.pushesPhase(ctx, tx, repoID, out, &cur, &pg)
		case phaseRetired:
			done, err = r.retiredPhase(ctx, tx, repoID, out, &cur, &pg)
		case phaseTail:
			done, err = r.packsAfter(ctx, tx, repoID, cur.From.Packed(), out, &cur, &pg)
		default:
			return nil, fmt.Errorf("%w: phase %q", ErrBadCursor, cur.Phase)
		}
		if err != nil {
			return nil, err
		}
		if !done {
			next := cur
			return &next, nil
		}
		// A PHASE COMPLETES only on a statement that answered fewer rows
		// than the page had room for, so the page has room for the next.
		switch {
		case cur.Phase == phasePacks:
			cur.Phase, cur.After = phaseRefs, ""
		case cur.Phase == phaseRefs && cur.Pushes:
			cur.Phase, cur.After = phasePushes, ""
		case (cur.Phase == phaseRefs || cur.Phase == phasePushes) && !cur.RetiredFrom.IsZero():
			cur.Phase, cur.After = phaseRetired, ""
		case (cur.Phase == phaseRefs || cur.Phase == phasePushes || cur.Phase == phaseRetired) && !first:
			cur.Phase, cur.After = phaseTail, ""
		default:
			// THE LAST PHASE COMPLETED; page 1 has no tail, being one
			// transaction's history already.
			return nil, nil
		}
	}
}

// layersPhase serves the repository's whole published chain onto page 1, its
// parts joined to their layers, charging every row to the page: whole, since
// the phase never resumes and the widest chain is far under a page
// (TestTheLayersPhaseFitsPageOne).
func (r *Reader) layersPhase(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	pg *page) error {

	chain, err := layersOf(ctx, tx, repoID)
	if err != nil {
		return err
	}
	if len(chain) > 0 {
		at := make(map[string]int, len(chain))
		for i, row := range chain {
			at[row.Layer.LayerID] = i
		}
		rows, err := tx.QueryContext(ctx, layerPartsOfSQL, repoID)
		if err != nil {
			return fmt.Errorf("gitlet: read %s's published layers' parts: %w", repoID, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				id, object, hash string
				n                int
				size             int64
			)
			if err := rows.Scan(&id, &n, &object, &hash, &size); err != nil {
				return fmt.Errorf("gitlet: read %s's published layers' parts: %w", repoID, err)
			}
			i, ok := at[id]
			if !ok {
				// A PART OF NO LAYER OF THE CHAIN: the apply removes a
				// layer's parts with its row, so this is rows no record
				// wrote, which no snapshot may answer as a chain.
				return fmt.Errorf("gitlet: %s holds part %d of layer %s, which its chain does not", repoID, n, id)
			}
			k, err := objstore.ParseKey(object)
			if err != nil {
				return fmt.Errorf("gitlet: part %d of %s's layer %s names %q: %w", n, repoID, id, object, err)
			}
			if n != len(chain[i].Layer.Parts) {
				return fmt.Errorf("gitlet: %s's layer %s holds part %d where part %d was due", repoID, id, n,
					len(chain[i].Layer.Parts))
			}
			chain[i].Layer.Parts = append(chain[i].Layer.Parts, LayerPart{Object: k, Hash: objstore.Hash(hash),
				Size: size})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("gitlet: read %s's published layers' parts: %w", repoID, err)
		}
	}
	for _, row := range chain {
		pg.add(row.encodedBytes())
	}
	out.Layers, out.LayersServed = chain, true
	return nil
}

// packsFull serves a full snapshot's packs in primary-key order.
func (r *Reader) packsFull(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	cur *StateCursor, pg *page) (bool, error) {

	var rows *sql.Rows
	var err error
	if cur.After == "" {
		rows, err = tx.QueryContext(ctx, packsFullFirstSQL, repoID, pg.left())
	} else {
		id, part, ok := strings.Cut(cur.After, "/")
		if !ok {
			return false, fmt.Errorf("%w: after %q", ErrBadCursor, cur.After)
		}
		rows, err = tx.QueryContext(ctx, packsFullAfterSQL, repoID, id, id, part, pg.left())
	}
	if err != nil {
		return false, fmt.Errorf("gitlet: read %s's packs: %w", repoID, err)
	}
	return r.scanPacks(rows, out, cur, pg, func(p PackRow) string { return p.PackID + "/" + p.Part })
}

// packsAfter serves the packs added above a floor, in (added_position,
// pack_id, part) order: a delta's packs from its Since, and the tail's from
// page 1's checkpoint.
func (r *Reader) packsAfter(ctx context.Context, tx *sql.Tx, repoID string, floor int64,
	out *RepoSnapshot, cur *StateCursor, pg *page) (bool, error) {

	var rows *sql.Rows
	var err error
	if cur.After == "" {
		rows, err = tx.QueryContext(ctx, packsAddedFirstSQL, repoID, floor, pg.left())
	} else {
		at, id, part, ok := cutPackKey(cur.After)
		if !ok {
			return false, fmt.Errorf("%w: after %q", ErrBadCursor, cur.After)
		}
		rows, err = tx.QueryContext(ctx, packsAddedAfterSQL, repoID, at, at, id, id, part, pg.left())
	}
	if err != nil {
		return false, fmt.Errorf("gitlet: read %s's packs: %w", repoID, err)
	}
	return r.scanPacks(rows, out, cur, pg, func(p PackRow) string {
		return strconv.FormatInt(p.Added.Packed(), 10) + "/" + p.PackID + "/" + p.Part
	})
}

// cutPackKey reads "<packed>/<pack_id>/<part>".
func cutPackKey(s string) (int64, string, string, bool) {
	at, rest, ok := strings.Cut(s, "/")
	if !ok {
		return 0, "", "", false
	}
	id, part, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, "", "", false
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", "", false
	}
	return n, id, part, true
}

// scanPacks serves the rows a packs statement answered, counting each into
// the page, and reports whether the phase is complete.
func (r *Reader) scanPacks(rows *sql.Rows, out *RepoSnapshot, cur *StateCursor, pg *page,
	key func(PackRow) string) (bool, error) {

	defer func() { _ = rows.Close() }()
	asked, served := pg.left(), 0
	for rows.Next() {
		var (
			p              PackRow
			object, hash   string
			added, content int64
			arrived        int64
		)
		if err := rows.Scan(&p.PackID, &p.Part, &object, &hash, &p.Size, &p.IndexBytes,
			&p.ReverseBytes, &p.GitChecksum, &p.Objects, &p.Kind, &p.State, &added, &content,
			&arrived); err != nil {
			return false, fmt.Errorf("gitlet: read a pack: %w", err)
		}
		k, err := objstore.ParseKey(object)
		if err != nil {
			return false, fmt.Errorf("gitlet: pack %s names %q: %w", p.PackID, object, err)
		}
		p.Object, p.Hash, p.Added, p.Content = k, objstore.Hash(hash), unpacked(added), unpacked(content)
		// ZERO IS NEVER: the column's default, and no record's broker
		// instant.
		if arrived != 0 {
			p.Arrived = store.DecodeTime(arrived)
		}
		out.Packs = append(out.Packs, p)
		served++
		cur.After = key(p)
		if pg.add(p.encodedBytes()) {
			return false, rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("gitlet: read packs: %w", err)
	}
	return served < asked, nil
}

// refs serves the refs phase: a full snapshot's live refs by name, or a
// delta's refs moved above its floor in (position, ref) order, tombstones
// included.
func (r *Reader) refs(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	cur *StateCursor, pg *page) (bool, error) {

	var rows *sql.Rows
	var err error
	switch {
	case cur.Full && cur.After == "":
		rows, err = tx.QueryContext(ctx, refsFullFirstSQL, repoID, pg.left())
	case cur.Full:
		rows, err = tx.QueryContext(ctx, refsFullAfterSQL, repoID, cur.After, pg.left())
	case cur.After == "":
		rows, err = tx.QueryContext(ctx, refsDeltaFirstSQL, repoID, cur.Since.Packed(), pg.left())
	default:
		at, ref, ok := strings.Cut(cur.After, "/")
		n, perr := strconv.ParseInt(at, 10, 64)
		if !ok || perr != nil {
			return false, fmt.Errorf("%w: after %q", ErrBadCursor, cur.After)
		}
		rows, err = tx.QueryContext(ctx, refsDeltaAfterSQL, repoID, n, n, ref, pg.left())
	}
	if err != nil {
		return false, fmt.Errorf("gitlet: read %s's refs: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	asked, served := pg.left(), 0
	for rows.Next() {
		var ref RefRow
		var position int64
		if err := rows.Scan(&ref.Ref, &ref.OID, &position); err != nil {
			return false, fmt.Errorf("gitlet: read a ref: %w", err)
		}
		ref.Position = unpacked(position)
		out.Refs = append(out.Refs, ref)
		served++
		if cur.Full {
			cur.After = ref.Ref
		} else {
			cur.After = strconv.FormatInt(position, 10) + "/" + ref.Ref
		}
		if pg.add(ref.encodedBytes()) {
			return false, rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("gitlet: read refs: %w", err)
	}
	return served < asked, nil
}

// pushesPhase serves every push applied in (Since, From], in log order,
// keyset after the last one served. ABOVE BY PAGE 1'S CHECKPOINT, never the
// page's own: the cache records From as its position, so a later push is the
// next delta's and would otherwise be folded in twice; below exclusively, as a
// push at Since is the round before's. Each push costs the page what it READS
// ([scanIntroduced]).
func (r *Reader) pushesPhase(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	cur *StateCursor, pg *page) (bool, error) {

	var rows *sql.Rows
	var err error
	if cur.After == "" {
		rows, err = tx.QueryContext(ctx, pushesAboveSQL, repoID, cur.Since.Packed(), cur.From.Packed(),
			pg.left())
	} else {
		position, pushID, ok := cutLogCursor(cur.After)
		if !ok {
			return false, fmt.Errorf("%w: after %q", ErrBadCursor, cur.After)
		}
		rows, err = tx.QueryContext(ctx, pushesAboveAfterSQL, repoID, position, cur.From.Packed(), position,
			pushID, pg.left())
	}
	if err != nil {
		return false, fmt.Errorf("gitlet: read %s's pushes since %s: %w", repoID, cur.Since, err)
	}
	defer func() { _ = rows.Close() }()
	asked, served := pg.left(), 0
	for rows.Next() {
		in, cost, err := scanIntroduced(rows)
		if err != nil {
			return false, fmt.Errorf("gitlet: read %s's pushes since %s: %w", repoID, cur.Since, err)
		}
		out.Pushes = append(out.Pushes, in)
		served++
		cur.After = logCursor(in.Position.Packed(), in.PushID)
		if pg.add(cost) {
			return false, rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("gitlet: read %s's pushes since %s: %w", repoID, cur.Since, err)
	}
	return served < asked, nil
}

// retiredPhase serves every pack a record retired in (RetiredFrom, From], in
// code_packs_retired_repo_idx's (position, pack id, part) order, keyset after
// the last one served. Bounded as [Reader.pushesPhase] is: a retirement after
// From is the next delta's, and one at the floor the last round's.
func (r *Reader) retiredPhase(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	cur *StateCursor, pg *page) (bool, error) {

	var rows *sql.Rows
	var err error
	if cur.After == "" {
		rows, err = tx.QueryContext(ctx, retiredFirstSQL, repoID, cur.RetiredFrom.Packed(), cur.From.Packed(),
			pg.left())
	} else {
		at, id, part, ok := cutPackKey(cur.After)
		if !ok {
			return false, fmt.Errorf("%w: after %q", ErrBadCursor, cur.After)
		}
		rows, err = tx.QueryContext(ctx, retiredAfterSQL, repoID, at, cur.From.Packed(), at, id, id, part,
			pg.left())
	}
	if err != nil {
		return false, fmt.Errorf("gitlet: read %s's retired packs above %s: %w", repoID, cur.RetiredFrom, err)
	}
	defer func() { _ = rows.Close() }()
	asked, served := pg.left(), 0
	for rows.Next() {
		var (
			row                 RetiredRow
			retiredAt, position int64
		)
		if err := rows.Scan(&row.PackID, &row.Part, &row.GitChecksum, &retiredAt, &position); err != nil {
			return false, fmt.Errorf("gitlet: read %s's retired packs above %s: %w", repoID, cur.RetiredFrom, err)
		}
		row.RetiredAt, row.Position = store.DecodeTime(retiredAt), unpacked(position)
		out.Retired = append(out.Retired, row)
		served++
		cur.After = strconv.FormatInt(position, 10) + "/" + row.PackID + "/" + row.Part
		if pg.add(row.encodedBytes()) {
			return false, rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("gitlet: read %s's retired packs above %s: %w", repoID, cur.RetiredFrom, err)
	}
	return served < asked, nil
}

// checkCursor refuses a cursor this reader did not issue.
func checkCursor(c *StateCursor) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrBadCursor, why) }
	switch {
	case c.Phase == phaseLayers:
		return bad("the layers phase is served whole on a snapshot's first page, and no page resumes it")
	case c.Phase != phasePacks && c.Phase != phaseRefs && c.Phase != phasePushes && c.Phase != phaseRetired &&
		c.Phase != phaseTail:
		return bad(fmt.Sprintf("phase %q", c.Phase))
	case c.From.Stream != topics.CodeLogStream || c.From.IsZero():
		return bad("it names no position on the code log")
	case c.Seen.Stream != c.From.Stream || c.Seen.Packed() < c.From.Packed():
		return bad("it was read below where it began")
	case c.Stream.IsZero():
		return bad("it names no stream")
	case c.DropEpoch < 0:
		return bad("it names a drop epoch below 0")
	case c.Full != c.Since.IsZero():
		return bad("a full snapshot has no delta floor, and a delta has one")
	case !c.Full && c.Since.Stream != topics.CodeLogStream:
		return bad("its delta floor is on another log")
	case c.Full && c.Pushes:
		return bad("a full snapshot serves no pushes phase")
	case c.Phase == phasePushes && !c.Pushes:
		return bad("its first page asked for no pushes phase")
	case !c.Full && c.RetiredFrom != c.Since:
		return bad("a delta serves the retirements above its own floor")
	case !c.RetiredFrom.IsZero() &&
		(c.RetiredFrom.Stream != topics.CodeLogStream || c.RetiredFrom.Generation != c.From.Generation):
		return bad("its retired floor is not a position on the generation it was read on")
	case c.Phase == phaseRetired && c.RetiredFrom.IsZero():
		return bad("its first page served no retirements")
	}
	if c.After == "" {
		return nil
	}
	switch {
	case c.Phase == phasePacks && c.Full:
		if _, _, ok := strings.Cut(c.After, "/"); !ok {
			return bad("after " + c.After)
		}
	case c.Phase == phasePacks, c.Phase == phaseRetired, c.Phase == phaseTail:
		if _, _, _, ok := cutPackKey(c.After); !ok {
			return bad("after " + c.After)
		}
	case c.Phase == phaseRefs && !c.Full:
		at, _, ok := strings.Cut(c.After, "/")
		if _, err := strconv.ParseInt(at, 10, 64); !ok || err != nil {
			return bad("after " + c.After)
		}
	case c.Phase == phasePushes:
		if _, _, ok := cutLogCursor(c.After); !ok {
			return bad("after " + c.After)
		}
	}
	return nil
}

// PushOutcome answers what became of one push, at session floored at the
// record's own position: "not found" is final only at or past it, so a node
// that cannot reach it refuses `behind` rather than answering early. Asked
// ([OutcomeQuery.Change]), it names the change the push opened from its
// options as well, by the id the push derives, in the same snapshot.
func (r *Reader) PushOutcome(ctx context.Context, q OutcomeQuery) (PushOutcome, error) {
	switch {
	case q.At.IsZero():
		return PushOutcome{}, refusedf("%w: at: a push's outcome is read at its record's position", ErrInvalid)
	case q.RepoID == "" || q.PushID == "":
		return PushOutcome{}, fmt.Errorf("%w: a push's outcome names its repository and its id", ErrInvalid)
	}
	query := statelog.FloorAt(statelog.ReadSession, q.At).Query(
		statelog.ScopeSet{Paths: []string{RepoPath(q.RepoID)}}, false)
	var out PushOutcome
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = PushOutcome{Position: cp.At}
		row, found, err := pushIn(ctx, tx, q.PushID)
		if err != nil || !found {
			return err
		}
		if row.RepoID != q.RepoID {
			return fmt.Errorf("%w: push %s is %s's, not %s's", ErrInvalid, q.PushID, row.RepoID, q.RepoID)
		}
		out.Found, out.Push = true, row
		if !q.Change {
			return nil
		}
		// THE CHANGE IT OPENED, by the id its push derives, in the
		// snapshot its row was read in.
		change, opened, err := changeRowIn(ctx, tx, PushChangeID(q.PushID))
		if err == nil && opened && change.repoID == q.RepoID {
			out.Opened = change.number
		}
		return err
	})
	if err != nil {
		return PushOutcome{}, err
	}
	return out, nil
}

// pushIn reads one push row.
func pushIn(ctx context.Context, tx *sql.Tx, pushID string) (PushRow, bool, error) {
	row, _, err := scanPush(tx.QueryRowContext(ctx, pushSQL, pushID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PushRow{}, false, nil
	case err != nil:
		return PushRow{}, false, fmt.Errorf("gitlet: read push %s: %w", pushID, err)
	}
	return row, true, nil
}

// scanner is a row or a row of rows.
type scanner interface{ Scan(dest ...any) error }

// scanPush reads a push row in pushColumns' order, and answers the length of
// its stored commands, so a page measures it without encoding it again. A row
// a NEWER build applied may store a field this build does not decode; its
// length is then the longer, and a page ends early rather than late.
//
// A ROW NAMING NO WAY THIS BUILD KNOWS IS REFUSED ([ErrInvalid]): the column's
// default is the empty string (replicated 0091), so an apply that forgot it
// fails here rather than reading as a client's push.
func scanPush(s scanner) (PushRow, int, error) {
	var (
		row                       PushRow
		position, at, received    int64
		via, commands, introduced string
	)
	if err := s.Scan(&row.PushID, &row.RepoID, &position, &at, &row.Actor, &row.ActorKind,
		&row.ActorSeat, &via, &row.Run.TurnID, &row.Run.WorkBackend, &row.Run.WorkKey, &row.Run.Model,
		&row.Run.RunID, &row.Run.Harness, &row.Node, &row.Atomic, &commands, &row.PackID, &row.PackBytes,
		&introduced, &row.IntroducedCount, &row.Offered, &received); err != nil {
		return PushRow{}, 0, err
	}
	if row.Via = PushVia(via); !row.Via.Valid() {
		return PushRow{}, 0, fmt.Errorf("%w: push %s's row names its way as %q, which is not one of %s",
			ErrInvalid, row.PushID, via, words.Or(PushVias()))
	}
	row.Position, row.At, row.ReceivedAt = unpacked(position), store.DecodeTime(at), store.DecodeTime(received)
	if err := json.Unmarshal([]byte(commands), &row.Commands); err != nil {
		return PushRow{}, 0, fmt.Errorf("gitlet: push %s's commands: %w", row.PushID, err)
	}
	if introduced != "" {
		row.Introduced = strings.Split(introduced, " ")
	}
	return row, len(commands), nil
}

// scanIntroduced reads a push row as the index of which push introduced each
// commit ([introducedOf]), and answers what serving it costs a page: the
// larger of the row it SENDS and the commands it READ.
//
// A page of condensed pushes is held to what it reads: a row stores every
// command its client sent, refused ones too ([RecordedRef]), while the
// condensed row sends only the landed refs, so a push of one branch beside 999
// refused long refs sends 656 bytes but reads 403 KB. Charged by what it sent,
// a page decoded gigabytes, past any estate read's attempt, on every retry.
// Nothing else in the row is wider than what it sends.
func scanIntroduced(rows *sql.Rows) (IntroducedRow, int, error) {
	pushed, commands, err := scanPush(rows)
	if err != nil {
		return IntroducedRow{}, 0, err
	}
	in := introducedOf(pushed)
	return in, max(in.encodedBytes(), commands), nil
}

// The read limits, past which a query is refused naming the bound rather than
// clamped. A page's BYTES are held to MaxPageBytes whatever its count,
// measured with the commands it read to condense pushes ([scanIntroduced]),
// so these bound what one request READS and are sized to a screen: a default
// that fills one, and a maximum whose typical rows stay well under the byte
// budget (500 repositories at MaxProtectionsPerRepo is 32 000 rule rows for
// one statement; 200 typical pushes under 500 KB; 1 000 longest refs about
// 385 KB; revisions and reviewers as narrow as a ref; changes, comments,
// reviews, findings and checks a push history's) while a page of the widest
// rows ends at the byte budget first. latestCheckLimit is MaxChecksPerFile,
// so a branch's latest checks are one page. The maxima are exported for a
// client that pages, and [ReadQuery.PageMax] says which one a query is held to.
const (
	defaultRepoLimit     = 100
	MaxReposPerPage      = 500
	defaultPushLimit     = 50
	MaxPushesPerPage     = 200
	defaultRefLimit      = 200
	MaxRefsPerPage       = 1_000
	defaultChangeLimit   = 50
	MaxChangesPerPage    = 200
	defaultCheckLimit    = 50
	latestCheckLimit     = MaxChecksPerFile
	MaxChecksPerPage     = 200
	defaultRevisionLimit = 200
	MaxRevisionsPerPage  = 1_000
)

// PageMax is the most rows one page of q carries, the bound its Limit is
// refused past: the one a caller that reads a kind to its end pages at, never
// [MaxSnapshotItems], which caps every page the reader serves rather than
// naming any kind's. 0 is a read no limit pages (one row, one answer, or the
// rows it names). TestEveryKindsPageMaxIsTheBoundItsReadRefusesPast holds it
// to every kind's validator, and TestEveryEngineReadAsksAPageItsKindCarries
// every limit the module's own reads ask to it.
func (q ReadQuery) PageMax() int {
	switch q.Kind {
	case ReadRepos:
		if q.DayBounds != nil {
			return MaxSparkReposPerPage
		}
		return MaxReposPerPage
	case ReadPushes:
		if q.Push != "" {
			return 0
		}
		return MaxPushesPerPage
	case ReadRefs:
		return MaxRefsPerPage
	case ReadChanges:
		return MaxChangesPerPage
	case ReadChange:
		switch {
		case q.DetailOnly, len(q.FindingIDs) > 0:
			return 0
		case q.FindingPaths:
			return MaxFindingPathsPerPage
		case q.Viewed:
			return MaxViewedPerPage
		}
		return q.Part.maxPerPage()
	case ReadChangeRouting:
		return MaxRoutedRecordsPerPage
	case ReadReviews:
		return MaxReviewsPerPage
	case ReadChecks:
		return MaxChecksPerPage
	case ReadQueue:
		return MaxQueueRowsPerPage
	case ReadLandings:
		return MaxLandingsPerPage
	case ReadInbox:
		return inbox.MaxRows
	case ReadIntroduced, ReadRunPushes, ReadChecksQueued:
		return MaxSnapshotItems
	}
	return 0
}

// BriefCommands is how many of a push's commands a row of its history read
// IN BRIEF carries ([ReadQuery.Brief]): the lines a screen gives one push.
// A person's, seat's or engine write's push is whole in five; a mirror's of
// up to MaxCommandsPerPush refs is five and a count ([PushRow.Omitted]),
// sparing 140-190 KB on a row a poll resends every minute, the rest one read
// away ([ReadQuery.Push]).
const BriefCommands = 5

// Read answers a person's, a seat's or a surface's read — a listing of
// repositories, one repository in detail, one repository's push history or
// its refs — and the engine's own reads, at the level it is asked for
// ([ReadQuery.freshness]): `session` only beside the position on the code log
// it must include, and stale where a principal's read or a duty listing names
// none.
func (r *Reader) Read(ctx context.Context, q ReadQuery) (ReadAnswer, error) {
	if err := q.validate(); err != nil {
		return ReadAnswer{}, err
	}
	if err := q.daysKept(r.now()); err != nil {
		return ReadAnswer{}, err
	}
	fresh, levelErr := q.freshness()
	if levelErr != nil {
		return ReadAnswer{}, levelErr
	}
	// THE ONE CHART every row is judged by, read before the transaction (a
	// listing judged against a chart swapped mid-page answers a page no epoch
	// held) and before any row ([Reader.chartFor]).
	chart, chartErr := r.chartFor(q.Asker, q.judgesReadiness())
	if chartErr != nil {
		return ReadAnswer{}, chartErr
	}
	if q.Kind == ReadChanges && q.textLeads() && r.words == nil {
		return ReadAnswer{}, fmt.Errorf("%w: this node keeps no index of changes' words", ErrNotServing)
	}
	var (
		out   ReadAnswer
		query statelog.Query
		serve func(*sql.Tx, statelog.Position) error
	)
	switch q.Kind {
	case ReadRepos:
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.repos(ctx, tx, q, chart, cp, &out) }
	case ReadRepo:
		query = fresh.Resolved(resolver(q.Repo), false)
		serve = func(tx *sql.Tx, cp statelog.Position) error {
			return r.repo(ctx, tx, q, chart, cp, &out)
		}
	case ReadPushes:
		query = fresh.Resolved(resolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.pushes(ctx, tx, q, chart, &out) }
	case ReadRunPushes:
		// A POINT READ OF EVERY REPOSITORY THE RUN WAS GIVEN: past a deferred
		// record it would say the run delivered less than it did, so it
		// refuses and the engine asks another node.
		paths := make([]string, len(q.RunPushes.Repos))
		for i, id := range q.RunPushes.Repos {
			paths[i] = RepoPath(id)
		}
		query = fresh.Query(statelog.ScopeSet{Paths: paths}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.runPushes(ctx, tx, q, &out) }
	case ReadRefs:
		// A FIRST PAGE IS A POINT READ, as it carries the repository's detail;
		// a later page is the listing's, a set.
		query = fresh.Resolved(resolver(q.Repo), q.Cursor != "")
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.refsListing(ctx, tx, q, chart, cp, &out) }
	case ReadChanges:
		// A SET READ of the named repository or of the domain; one that
		// resolves its repository is a point read of it ([ReadQuery.Resolve]).
		if q.Repo != "" {
			query = fresh.Resolved(resolver(q.Repo), !q.Resolve)
		} else {
			query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		}
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.changes(ctx, tx, q, chart, cp, &out) }
	case ReadChange:
		// A FIRST PAGE IS A POINT READ of the repository, carrying the detail;
		// a later page or a conversation part is a set (its records scope the
		// repository's path too), unless it resolves the repository
		// ([ReadQuery.Resolve]).
		query = fresh.Resolved(resolver(q.Repo), !q.Resolve && (q.Cursor != "" || !q.pagesDetail()))
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.change(ctx, tx, q, chart, cp, &out) }
	case ReadChangeRouting:
		// A SET READ of the repository: every record about a change scopes its
		// path, so the repository's deferrals order its notices too.
		query = fresh.Resolved(resolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.changeRouting(ctx, tx, q, chart, &out) }
	case ReadReviews:
		// A SET READ OF THE DOMAIN, as a listing of changes across
		// repositories is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.reviewsGiven(ctx, tx, q, chart, &out) }
	case ReadCommit:
		// A POINT READ: past a deferred record a commit's page would name less
		// than it belongs to. Its gates' checks and approvals, on other paths,
		// are read as their rows stand.
		query = fresh.Resolved(resolver(q.Repo), false)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.commit(ctx, tx, q, chart, cp, &out) }
	case ReadIntroduced:
		// A POINT READ OF THE REPOSITORY: an index built past a deferred record
		// would silently miss its pushes, so it refuses and the backfill asks
		// again.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.Introduced.RepoID)}}, false)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.introduced(ctx, tx, q, cp, &out) }
	case ReadCompactionDue:
		// A SET READ OF THE DOMAIN: the compaction that follows is decided
		// again from the committer's own snapshot. So are the duty listings
		// below.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.compactionDue(ctx, tx, q, &out) }
	case ReadPublicationDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.publicationDue(ctx, tx, q, &out) }
	case ReadExpungeDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.expungeDue(ctx, tx, q, &out) }
	case ReadImportsDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.importsDue(ctx, tx, q, &out) }
	case ReadMirrorsDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.mirrorsDue(ctx, tx, q, &out) }
	case ReadChangesDue:
		// A SET READ, of the domain or one repository (changesdue.go).
		query = q.changesDueQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.changesDue(ctx, tx, q, &out) }
	case ReadIndexed:
		// A SET READ OF THE DOMAIN: the indexer reads each repository from its
		// own copy, and a deferred mode change is read at the next poll.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.indexed(ctx, tx, q, &out) }
	case ReadDefaultTips:
		// A SET READ OF THE DOMAIN: the scan rereads each tip from the
		// repository's own rows.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.defaultTips(ctx, tx, q, &out) }
	case ReadChecks:
		// A SET READ, of the repository it names or of the domain for the
		// window (checksread.go).
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.checks(ctx, tx, q, chart, cp, &out) }
	case ReadCheck:
		// A POINT READ of the repository's checks.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.check(ctx, tx, q, chart, &out) }
	case ReadChecksDue:
		// A SET READ, as changes_due's: the checks writer decides the dispatch
		// again.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.dueCheckWork(ctx, tx, q, &out) }
	case ReadCheckBox:
		// A POINT READ: a box is never admitted by an attempt read around a
		// deferred record.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.checkBox(ctx, tx, q, &out) }
	case ReadChecksQueued:
		// A SET READ OF THE DOMAIN: the checks writer decides each claim again,
		// refusing one another runner holds.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.queuedChecks(ctx, tx, q, &out) }
	case ReadChecksRunning:
		// A SET READ: the checks writer decides each ending again.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.runningChecks(ctx, tx, q, &out) }
	case ReadQueue, ReadLandings, ReadGreen:
		// A SET READ, of the repository it names or of the domain
		// (queueread.go).
		query = q.queueQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error {
			switch q.Kind {
			case ReadLandings:
				return r.landings(ctx, tx, q, chart, &out)
			case ReadGreen:
				return r.greens(ctx, tx, q, chart, &out)
			}
			return r.queueListing(ctx, tx, q, chart, &out)
		}
	case ReadInbox:
		// A SET READ OF THE DOMAIN: notices come from every change's subject
		// and marks from the person's own (inboxread.go).
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.inbox(ctx, tx, q, chart, &out) }
	case ReadNoticeCards:
		// A SET READ OF THE DOMAIN, as an `inbox` read is: the changes it
		// names lie on any repository's subjects (inboxread.go).
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.noticeCards(ctx, tx, q, chart, &out) }
	case ReadCommitMarks:
		// A SET READ, as a listing of its checks is: a deferred attempt or change is
		// read as its row stands, so a history page polled each minute is not refused.
		query = fresh.Resolved(checksResolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.commitMarks(ctx, tx, q, chart, &out) }
	case ReadCounts:
		// A SET READ, of the repository it names or of the domain, as a listing of changes is.
		if q.Repo != "" {
			query = fresh.Resolved(resolver(q.Repo), true)
		} else {
			query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		}
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.counts(ctx, tx, q, chart, &out) }
	case ReadExpungeTips:
		// A POINT READ OF THE REPOSITORY: past a deferred drop an expunge
		// would drop an object the drop's grace keeps, so it refuses.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.ExpungeTips.RepoID)}}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.expungeTips(ctx, tx, q, &out) }
	case ReadMirrorPush:
		// A POINT READ OF THE REPOSITORY: past a deferred record it could
		// publish a commit its checks never passed, or to a target no longer
		// named, so it refuses.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.MirrorPush.RepoID)}}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.mirrorPush(ctx, tx, q, &out) }
	case ReadBranchChanges:
		// A POINT READ OF EVERY REPOSITORY IT NAMES: past a deferred record it
		// could name a delivered ref by a change it no longer heads, so it
		// refuses.
		paths := make([]string, len(q.BranchChanges.Repos))
		for i, repo := range q.BranchChanges.Repos {
			paths[i] = RepoPath(repo.RepoID)
		}
		query = fresh.Query(statelog.ScopeSet{Paths: paths}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.branchChanges(ctx, tx, q, &out) }
	}
	answer, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = ReadAnswer{Position: cp.At}
		return serve(tx, cp.At)
	})
	if err != nil {
		return ReadAnswer{}, err
	}
	out.Level = answer.Level
	return out, nil
}

// validate refuses a query naming no kind this build reads, no valid asker, a
// repository that cannot be one, a limit past its bound (never clamped), or a
// field outside the kind that reads it, so no field is answered ignored.
func (q ReadQuery) validate() error {
	if !q.Kind.Valid() {
		return refusedf("%w: kind: %q is not one of %s", ErrInvalid, q.Kind, words.Or(ReadKinds()))
	}
	// A RESOLUTION ONLY WHERE A NAME IS JUDGED, before any kind's own
	// rules, so no kind's list of what it refuses has to spell it.
	if q.Resolve && !q.resolves() {
		return refusedf("%w: resolve: only a change read, or a listing of one repository's changes or "+
			"checks, resolves the repository it names", ErrInvalid)
	}
	// A FIELD ONE PRINCIPAL'S KIND READS ALONE is refused on every other kind
	// here, before any kind's own lists, which a field added later is missing
	// from.
	if err := q.kindOwnedField(); err != nil {
		return err
	}
	switch q.Kind {
	case ReadIntroduced:
		return q.validateIntroduced()
	case ReadRunPushes:
		return q.validateRunPushes()
	case ReadCompactionDue:
		return q.validateCompactionDue()
	case ReadPublicationDue:
		return q.validatePublicationDue()
	case ReadExpungeDue:
		return q.validateExpungeDue()
	case ReadImportsDue:
		return q.validateBareListing("an imports_due read lists every repository being imported")
	case ReadMirrorsDue:
		return q.validateBareListing("a mirrors_due read lists every repository with a mirror")
	case ReadChangesDue:
		return q.validateChangesDue()
	case ReadIndexed:
		return q.validateIndexed()
	case ReadDefaultTips:
		return q.validateDefaultTips()
	case ReadChecksDue:
		return q.validateChecksDue()
	case ReadExpungeTips:
		return q.validateExpungeTips()
	case ReadMirrorPush:
		return q.validateMirrorPush()
	case ReadBranchChanges:
		return q.validateBranchChanges()
	case ReadCheckBox:
		return q.validateCheckBox()
	case ReadChecksQueued:
		return q.validateChecksQueued()
	case ReadChecksRunning:
		return q.validateChecksRunning()
	}
	// WHO ASKS IS NAMED, or nothing is read: an answer is held to the asker's
	// grant.
	if why := q.Asker.invalid(); why != "" {
		return refusedf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	switch {
	case q.Introduced != nil:
		return refusedf("%w: introduced: only an introduced read names a range of pushes", ErrInvalid)
	case q.RunPushes != nil:
		return refusedf("%w: run_pushes: only a run_pushes read names an executor", ErrInvalid)
	case q.ChangesDue != nil:
		return refusedf("%w: changes_due: only a changes_due read names the repository its home owes "+
			"change work in", ErrInvalid)
	case q.ChecksDue != nil:
		return refusedf("%w: checks_due: only a checks_due read names the repository its home owes "+
			"check work in", ErrInvalid)
	case q.Checks != nil && q.Kind != ReadChecks && q.Kind != ReadCheck:
		return refusedf("%w: checks: only a checks or a check read names a change's, a commit's or an "+
			"attempt's checks", ErrInvalid)
	case q.CheckBox != nil:
		return refusedf("%w: check_box: only a check_box read names an attempt by its key", ErrInvalid)
	case q.ChecksRunning != nil:
		return refusedf("%w: checks_running: only a checks_running read names the repository whose running "+
			"checks it reads", ErrInvalid)
	case q.MirrorPush != nil:
		return refusedf("%w: mirror_push: only a mirror_push read names the repository whose push mirror "+
			"it reads", ErrInvalid)
	case q.ChecksQueued != nil:
		return refusedf("%w: checks_queued: only a checks_queued read names the cell whose queued checks it "+
			"reads", ErrInvalid)
	case q.DefaultTips != nil:
		return refusedf("%w: default_tips: only a default_tips read names the repository whose tip it reads",
			ErrInvalid)
	case q.Target != "" && q.Kind != ReadQueue && q.Kind != ReadLandings && q.Kind != ReadGreen:
		return refusedf("%w: target: only a queue, a landings or a green read names a branch's queue",
			ErrInvalid)
	case q.ExpungeTips != nil:
		return refusedf("%w: expunge_tips: only an expunge_tips read names a repository's ref drops", ErrInvalid)
	case !q.ScannedBefore.IsZero():
		return refusedf("%w: scanned_before: only an expunge_due read lists the repositories last judged "+
			"before an instant", ErrInvalid)
	case q.BranchChanges != nil:
		return refusedf("%w: branch_changes: only a branch_changes read names the branches whose live "+
			"changes it answers", ErrInvalid)
	case q.MinPacks != 0:
		return refusedf("%w: min_packs: only a compaction_due read names a floor of live packs", ErrInvalid)
	case q.Prefix != "" && q.Kind != ReadRefs && q.Kind != ReadRepos:
		return refusedf("%w: prefix: only a refs listing and a repositories listing name a prefix", ErrInvalid)
	case q.Prefix != "" && q.Kind == ReadRefs && !validRefPrefix(q.Prefix):
		return refusedf("%w: prefix: %q is not refs/heads/ or refs/tags/ or a directory of refs under "+
			"either, ending in /", ErrInvalid, q.Prefix)
	case q.Prefix != "" && q.Kind == ReadRepos && !validRepoPrefix(q.Prefix):
		return refusedf("%w: prefix: no repository name begins %q: a name is lowercase letters, digits and "+
			"hyphens, beginning with a letter or a digit, and a seat's own holds one dot", ErrInvalid, q.Prefix)
	case q.Brief && q.Kind != ReadPushes && q.Kind != ReadChange:
		return refusedf("%w: brief: only a push history and a change's first page are read in brief",
			ErrInvalid)
	case q.Brief && q.Kind == ReadChange && (q.Part != "" || q.Cursor != ""):
		return refusedf("%w: brief: only a change's first page of its revisions, or its row alone with "+
			"detail_only, is read in brief — named by no part or cursor", ErrInvalid)
	case q.Push != "" && q.Kind != ReadPushes:
		return refusedf("%w: push: only a push history is narrowed to one push", ErrInvalid)
	case q.PathOwners && q.Kind != ReadRepo:
		return refusedf("%w: path_owners: only a read of one repository answers its path owners", ErrInvalid)
	case q.Gates && q.Kind != ReadRepo && (q.Kind != ReadRefs || q.Cursor != ""):
		return refusedf("%w: gates: only a read answering one repository's detail — a read of one "+
			"repository, or a refs listing's first page — answers its rules' merge gates", ErrInvalid)
	case q.Push != "" && !ValidPushID(q.Push):
		return refusedf("%w: push: %q is not a push's id, a UUIDv7 in its canonical lowercase spelling",
			ErrInvalid, q.Push)
	case q.Push != "" && (q.Cursor != "" || q.Limit != 0 || q.Brief):
		return refusedf("%w: push: one push is read whole, never paged, cut or read in brief", ErrInvalid)
	case q.Change != 0 && q.Kind != ReadChange && q.Kind != ReadChangeRouting:
		return refusedf("%w: change: only a change read and a change's routing name a change's number",
			ErrInvalid)
	case q.State != "" && q.Kind != ReadChanges:
		return refusedf("%w: state: only a listing of changes is narrowed to a change's state", ErrInvalid)
	case q.Owner != "" && q.Kind != ReadChanges:
		return refusedf("%w: owner: only a listing of changes is narrowed to a change's owner", ErrInvalid)
	case q.Work != "" && q.Kind != ReadChanges:
		return refusedf("%w: work: only a listing of changes is narrowed to a work item", ErrInvalid)
	case q.Reviewer != "" && q.Kind != ReadChanges && q.Kind != ReadReviews:
		return refusedf("%w: reviewer: only a listing of changes or of a reviewer's reviews names a reviewer",
			ErrInvalid)
	case q.Needs != "" && q.Kind != ReadChanges:
		return refusedf("%w: needs: only a listing of changes is narrowed to what its changes wait on",
			ErrInvalid)
	case q.Window != "" && q.Kind != ReadChanges:
		return refusedf("%w: window: only a listing of changes is narrowed to the changes that moved "+
			"within a window", ErrInvalid)
	case q.Part != "" && q.Kind != ReadChange:
		return refusedf("%w: part: only a change read names a part of the change", ErrInvalid)
	case q.Part != "" && !q.Part.Valid():
		return refusedf("%w: part: %q is not one of %s", ErrInvalid, q.Part, words.Or(ChangeParts()))
	case q.DetailOnly && q.Kind != ReadChange:
		return refusedf("%w: detail_only: only a change read answers a change's detail", ErrInvalid)
	case q.DetailOnly && (q.Part != "" || q.Cursor != "" || q.Limit != 0):
		return refusedf("%w: detail_only: a change's detail alone pages no part — named by no part, cursor "+
			"or limit", ErrInvalid)
	case len(q.FindingIDs) > 0 && (q.Kind != ReadChange || q.Part != PartFindings):
		return refusedf("%w: finding_ids: only a change read's findings part is narrowed to the findings it "+
			"names", ErrInvalid)
	case len(q.FindingIDs) > 0 && q.Limit != 0:
		return refusedf("%w: finding_ids: the findings named are the read's rows, which no limit cuts",
			ErrInvalid)
	case q.FindingPaths && (q.Part != "" || q.DetailOnly || q.Brief || len(q.FindingIDs) > 0):
		return refusedf("%w: finding_paths: a change's findings by path are a page of their own, "+
			"named by no part, detail_only, brief or finding_ids", ErrInvalid)
	case q.Viewed && (q.Part != "" || q.DetailOnly || q.Brief || len(q.FindingIDs) > 0 || q.FindingPaths):
		return refusedf("%w: viewed: a change's viewed files are a page of their own, named by no part, "+
			"detail_only, brief, finding_ids or finding_paths", ErrInvalid)
	}
	switch {
	case q.Kind == ReadInbox && q.Repo != "":
		return refusedf("%w: repo: an inbox is read across every repository the asker may read", ErrInvalid)
	case q.Kind == ReadNoticeCards && q.Repo != "":
		return refusedf("%w: repo: an inbox page's cards are read across every repository its changes are on",
			ErrInvalid)
	case q.Kind == ReadReviews && q.Repo != "":
		return refusedf("%w: repo: a reviewer's reviews are listed across every repository the asker may "+
			"read, and a change's own reviews are a change read's part", ErrInvalid)
	case q.Kind == ReadRepos, q.Kind == ReadChanges && q.Repo == "", q.Kind == ReadChecks && q.Repo == "",
		q.Kind == ReadQueue && q.Repo == "", q.Kind == ReadReviews, q.Kind == ReadInbox,
		q.Kind == ReadNoticeCards, q.Kind == ReadCounts && q.Repo == "":
	case !ValidName(q.Repo):
		return refusedf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
	}
	if q.Kind != ReadRepos && len(q.Status) > 0 {
		return refusedf("%w: status: only a listing of repositories filters by status", ErrInvalid)
	}
	for _, s := range q.Status {
		if !s.Valid() {
			return refusedf("%w: status: %q is not one of %s", ErrInvalid, s, words.Or(RepoStatuses()))
		}
	}
	switch q.Kind {
	case ReadRepos:
		if err := q.validateRepos(); err != nil {
			return err
		}
	case ReadRefs:
		if q.RefListing != nil {
			if err := q.validateRefListing(); err != nil {
				return err
			}
		}
	case ReadChanges:
		if err := q.validateChanges(); err != nil {
			return err
		}
	case ReadChange, ReadChangeRouting:
		if q.Change < 1 {
			return refusedf("%w: change: %d is no change's number: a repository numbers its changes "+
				"from 1", ErrInvalid, q.Change)
		}
		if err := validateFindingIDs(q.FindingIDs); err != nil {
			return err
		}
	case ReadReviews:
		if err := q.validateReviews(); err != nil {
			return err
		}
	case ReadCommit:
		if err := q.validateCommit(); err != nil {
			return err
		}
	case ReadInbox:
		if err := q.validateInbox(); err != nil {
			return err
		}
	case ReadNoticeCards:
		if err := q.validateNoticeCards(); err != nil {
			return err
		}
	case ReadCommitMarks:
		if err := q.validateCommitMarks(); err != nil {
			return err
		}
	case ReadCounts:
		if err := q.validateCounts(); err != nil {
			return err
		}
	case ReadChecks, ReadCheck:
		if err := q.validateChecks(); err != nil {
			return err
		}
	case ReadQueue, ReadLandings, ReadGreen:
		if err := q.validateQueueRead(); err != nil {
			return err
		}
	}
	if q.DayBounds != nil {
		if err := q.validateDayBounds(); err != nil {
			return err
		}
	}
	if q.PushFilter != nil {
		if err := q.validatePushFilter(); err != nil {
			return err
		}
	}
	switch bound := q.PageMax(); {
	case q.Limit < 0:
		return refusedf("%w: limit: %d", ErrInvalid, q.Limit)
	case q.Kind == ReadRepo && q.Limit != 0:
		return refusedf("%w: limit: one repository is not paged", ErrInvalid)
	case q.Limit > bound:
		return refusedf("%w: limit: %d is past the %d one page of %s carries", ErrInvalid,
			q.Limit, bound, q.Kind)
	}
	return nil
}

// kindOwnedField refuses a field only one principal's kind reads on a read of
// any other kind, the engine's own reads included ([ReadQuery.validate]).
func (q ReadQuery) kindOwnedField() error {
	switch {
	case q.Protected && q.Kind != ReadRepos:
		return refusedf("%w: protected: only a listing of repositories is narrowed to the ones a rule "+
			"protects", ErrInvalid)
	case q.Commit != "" && q.Kind != ReadCommit:
		return refusedf("%w: commit: only a commit read names a commit", ErrInvalid)
	case q.Cards && q.Kind != ReadChanges:
		return refusedf("%w: cards: only a listing of changes carries each change's card", ErrInvalid)
	case q.FindingPaths && q.Kind != ReadChange:
		return refusedf("%w: finding_paths: only a change read counts its findings by path", ErrInvalid)
	case q.Viewed && q.Kind != ReadChange:
		return refusedf("%w: viewed: only a change read answers the files its asker marked viewed", ErrInvalid)
	case q.Inbox != nil && q.Kind != ReadInbox:
		return refusedf("%w: inbox: only an inbox read names whose inbox it reads", ErrInvalid)
	case len(q.NoticeCards) > 0 && q.Kind != ReadNoticeCards:
		return refusedf("%w: notice_cards: only a notice_cards read names the changes whose cards it reads",
			ErrInvalid)
	case q.ChangeSort != "" && q.Kind != ReadChanges:
		return refusedf("%w: change_sort: only a listing of changes is ordered by when its changes were "+
			"opened or last moved", ErrInvalid)
	case q.Readiness != "" && q.Kind != ReadChanges:
		return refusedf("%w: readiness: only a listing of changes is narrowed to where their merge stands",
			ErrInvalid)
	case q.Text != "" && q.Kind != ReadChanges:
		return refusedf("%w: text: only a listing of changes is narrowed by its changes' words", ErrInvalid)
	case q.RefListing != nil && q.Kind != ReadRefs:
		return refusedf("%w: ref_listing: only a refs listing is ordered, narrowed or read in detail",
			ErrInvalid)
	case len(q.CommitMarks) > 0 && q.Kind != ReadCommitMarks:
		return refusedf("%w: commit_marks: only a commit_marks read names the commits whose marks it reads",
			ErrInvalid)
	case q.Counts != nil && q.Kind != ReadCounts:
		return refusedf("%w: counts: only a counts read names the parts it counts", ErrInvalid)
	case q.PushFilter != nil && q.Kind != ReadPushes:
		return refusedf("%w: push_filter: only a push history is narrowed to one pusher or a window", ErrInvalid)
	case len(q.DayBounds) > 0 && q.Kind != ReadRepos && q.Kind != ReadRepo && q.Kind != ReadPushes:
		return refusedf("%w: day_bounds: only a listing of repositories, a read of one and a push history "+
			"count pushes by the asker's days", ErrInvalid)
	case q.RepoFacts && q.Kind != ReadRepos && q.Kind != ReadRepo:
		return refusedf("%w: repo_facts: only a listing of repositories and a read of one answer a "+
			"repository's facts", ErrInvalid)
	case q.RepoKind != "" && q.Kind != ReadRepos:
		return refusedf("%w: repo_kind: only a listing of repositories is narrowed to one kind", ErrInvalid)
	case q.RepoSort != "" && q.Kind != ReadRepos:
		return refusedf("%w: repo_sort: only a listing of repositories is ordered by name or last push",
			ErrInvalid)
	case q.Trains && q.Kind != ReadQueue:
		return refusedf("%w: trains: only a queue read answers its trains and ejections", ErrInvalid)
	}
	return nil
}

// validateIntroduced is [ReadQuery.validate] for an `introduced` read: the
// engine's own, so it names NO asker (one would look held to a grant that
// judges nothing), and its repository by id in a range running one way:
// (After, Through] ascending, or below Before descending.
func (q ReadQuery) validateIntroduced() error {
	in := q.Introduced
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	onLog := func(p statelog.Position) bool { return p.IsZero() || p.Stream == topics.CodeLogStream }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "an introduced read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "an introduced read is the engine's own, which no seat makes for another")
	case in == nil:
		return refused("introduced", "an introduced read names its repository and its range")
	case in.RepoID == "":
		return refused("introduced.repo_id", "an introduced read names its repository by id")
	case q.Repo != "" || len(q.Status) > 0 || q.Prefix != "":
		return refused("repo", "an introduced read names its repository in its range and filters nothing")
	case q.Brief:
		return refused("brief", "an introduced read condenses every push in its range its own way")
	case q.Push != "":
		return refused("push", "an introduced read names its pushes by its range alone")
	case q.PathOwners:
		return refused("path_owners", "only a read of one repository answers its path owners")
	case q.Gates:
		return refused("gates", "only a read answering one repository's detail answers its rules' merge gates")
	case q.DetailOnly:
		return refused("detail_only", "only a change read answers a change's detail")
	case len(q.FindingIDs) > 0:
		return refused("finding_ids", "only a change read's findings part is narrowed to the findings it names")
	case q.MinPacks != 0:
		return refused("min_packs", "only a compaction_due read names a floor of live packs")
	case q.Change != 0:
		return refused("change", "only a change read and a change's routing name a change's number")
	case q.State != "":
		return refused("state", "only a listing of changes is narrowed to a change's state")
	case q.Owner != "":
		return refused("owner", "only a listing of changes is narrowed to a change's owner")
	case q.Work != "":
		return refused("work", "only a listing of changes is narrowed to a work item")
	case q.RunPushes != nil:
		return refused("run_pushes", "only a run_pushes read names an executor")
	case q.ChangesDue != nil:
		return refused("changes_due", "only a changes_due read names the repository its home owes change work "+
			"in")
	case q.ChecksDue != nil:
		return refused("checks_due", "only a checks_due read names the repository its home owes check work in")
	case q.Checks != nil:
		return refused("checks", "only a checks or a check read names a change's, a commit's or an attempt's "+
			"checks")
	case q.CheckBox != nil:
		return refused("check_box", "only a check_box read names an attempt by its key")
	case q.ChecksRunning != nil:
		return refused("checks_running", "only a checks_running read names the repository whose running "+
			"checks it reads")
	case q.MirrorPush != nil:
		return refused("mirror_push", "only a mirror_push read names the repository whose push mirror it reads")
	case q.ChecksQueued != nil:
		return refused("checks_queued", "only a checks_queued read names the cell whose queued checks it reads")
	case q.DefaultTips != nil:
		return refused("default_tips", "only a default_tips read names the repository whose tip it reads")
	case q.ExpungeTips != nil:
		return refused("expunge_tips", "only an expunge_tips read names a repository's ref drops")
	case !q.ScannedBefore.IsZero():
		return refused("scanned_before", "only an expunge_due read lists the repositories last judged "+
			"before an instant")
	case q.Target != "":
		return refused("target", "only a queue, a landings or a green read names a branch's queue")
	case q.Reviewer != "":
		return refused("reviewer", "only a listing of changes or of a reviewer's reviews names a reviewer")
	case q.Needs != "":
		return refused("needs", "only a listing of changes is narrowed to what its changes wait on")
	case q.Part != "":
		return refused("part", "only a change read names a part of the change")
	case !onLog(in.After) || !onLog(in.Through) || !onLog(in.Before):
		return refused("introduced", "every position it names is on the code log")
	case in.Descending && in.Before.IsZero():
		return refused("introduced.before", "a descending read names the position it reads below")
	case in.Descending && (!in.After.IsZero() || !in.Through.IsZero()):
		return refused("introduced", "a descending read names Before alone")
	case !in.Descending && !in.Before.IsZero():
		return refused("introduced.before", "an ascending read names After and Through")
	case !in.Through.IsZero() && in.Through.Packed() < in.After.Packed():
		return refused("introduced.through", "the range ends below where it begins")
	case q.Limit < 0:
		return refused("limit", strconv.Itoa(q.Limit))
	case q.Limit > MaxSnapshotItems:
		return refused("limit", fmt.Sprintf("%d is past the %d one page of %s carries", q.Limit,
			MaxSnapshotItems, q.Kind))
	}
	return nil
}

// validateRunPushes is [ReadQuery.validate] for a `run_pushes` read, the
// engine's own when a run is collected: NO asker or ceiling seat (an
// `introduced` read's reason); the run's launch id; its repositories by id,
// one to MaxRunRepos, each once; and its launch position on the code log,
// never zero, which would scan whole histories for pushes no run made. A
// page's limit is at most MaxSnapshotItems ([Reader.runPushes]).
func (q ReadQuery) validateRunPushes() error {
	in := q.RunPushes
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a run_pushes read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a run_pushes read is the engine's own, which no seat makes for another")
	case in == nil:
		return refused("run_pushes", "a run_pushes read names its executor, the run's repositories and the "+
			"position it was launched at")
	case !canonicalUUID(in.Run):
		return refused("run_pushes.run", fmt.Sprintf("%q is not a UUID in its canonical lowercase spelling — "+
			"an executor is read by the launch id its pushes name", in.Run))
	case len(in.Repos) == 0 || len(in.Repos) > MaxRunRepos:
		return refused("run_pushes.repos", fmt.Sprintf("%d repositories; an executor is given one to %d "+
			"(MaxRunRepos)", len(in.Repos), MaxRunRepos))
	case in.After.IsZero():
		return refused("run_pushes.after", "a run_pushes read names the position its run was launched at: "+
			"nothing the run pushed applied below it")
	case in.After.Stream != topics.CodeLogStream:
		return refused("run_pushes.after", fmt.Sprintf("%s is a position on another log than the code log (%s)",
			in.After, topics.CodeLogStream))
	case in.After.Valid() != nil:
		return refused("run_pushes.after", in.After.Valid().Error())
	case q.Limit < 0:
		return refused("limit", strconv.Itoa(q.Limit))
	case q.Limit > MaxSnapshotItems:
		return refused("limit", fmt.Sprintf("%d is past the %d pushes one page of %s reads", q.Limit,
			MaxSnapshotItems, q.Kind))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"min_packs", q.MinPacks != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"checks_queued", q.ChecksQueued != nil}, {"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a run_pushes read names its repositories in its run and filters nothing")
		}
	}
	seen := make(map[string]bool, len(in.Repos))
	for i, id := range in.Repos {
		switch {
		case !canonicalUUID(id):
			return refused(fmt.Sprintf("run_pushes.repos[%d]", i), fmt.Sprintf("%q is not a repository's id "+
				"in its canonical lowercase spelling", id))
		case seen[id]:
			return refused(fmt.Sprintf("run_pushes.repos[%d]", i), fmt.Sprintf("%s is named twice — a run's "+
				"repositories are read once each", id))
		}
		seen[id] = true
	}
	return nil
}

// validateCompactionDue is [ReadQuery.validate] for a `compaction_due` read,
// the compactor's own poll: NO asker (a principal's read of a duty listing
// would be held to nothing), its floor of live packs, at least one, and
// nothing else; a page is MaxSnapshotItems repositories.
func (q ReadQuery) validateCompactionDue() error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	if q.Asker != (Principal{}) {
		return refused("asker", "a compaction_due read is the engine's own, which no principal makes")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"checks_queued", q.ChecksQueued != nil}, {"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a compaction_due read lists every repository at its floor of live "+
				"packs, a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	if q.MinPacks < 1 {
		return refused("min_packs", fmt.Sprintf("%d; a compaction_due read names the fewest live packs a "+
			"repository it lists holds, at least 1", q.MinPacks))
	}
	return nil
}

// validatePublicationDue is [ReadQuery.validate] for a `publication_due` read,
// the publisher's own poll: NO asker (`compaction_due`'s reason) and nothing
// else, since its partial index holds exactly what it lists.
func (q ReadQuery) validatePublicationDue() error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	if q.Asker != (Principal{}) {
		return refused("asker", "a publication_due read is the engine's own, which no principal makes")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"checks_queued", q.ChecksQueued != nil}, {"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a publication_due read lists every repository holding a pack no "+
				"published layer covers, a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateExpungeDue is [ReadQuery.validate] for an `expunge_due` read, the
// maintainer's own poll: a bare listing ([ReadQuery.validateBareListing]) but
// for the instant its weekly arm lists from ([ReadQuery.ScannedBefore]).
func (q ReadQuery) validateExpungeDue() error {
	bare := q
	bare.ScannedBefore = time.Time{}
	return bare.validateBareListing("an expunge_due read lists every repository with an expunge open or a " +
		"purge requested, then every one last judged before its scanned_before")
}

// validateBareListing is [ReadQuery.validate] for a duty listing naming
// nothing but its cursor: the engine's own, so NO asker, and no limit, since
// its partial index holds exactly what it lists and a page is MaxSnapshotItems.
// lists is the sentence a refusal says what it lists in.
func (q ReadQuery) validateBareListing(lists string) error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	if q.Asker != (Principal{}) {
		return refused("asker", fmt.Sprintf("a %s read is the engine's own, which no principal makes", q.Kind))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"checks_queued", q.ChecksQueued != nil}, {"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, lists+", a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateDefaultTips is [ReadQuery.validate] for a `default_tips` read: a
// bare listing, or ONE repository by its id ([DefaultTipsQuery]).
func (q ReadQuery) validateDefaultTips() error {
	one := q.DefaultTips
	q.DefaultTips = nil
	if err := q.validateBareListing("a default_tips read lists every active project repository"); err != nil {
		return err
	}
	if one == nil {
		return nil
	}
	if q.Cursor != "" {
		return refusedf("%w: cursor: a default_tips read of one repository answers one row", ErrInvalid)
	}
	if parsed, err := uuid.Parse(one.RepoID); err != nil || parsed.String() != one.RepoID {
		return refusedf("%w: default_tips.repo_id: not a repository's id", ErrInvalid)
	}
	return nil
}

// validateIndexed is [ReadQuery.validate] for an `indexed` read, the
// indexer's own listing: held to an `expunge_due` read's rule.
func (q ReadQuery) validateIndexed() error {
	if err := q.validateExpungeDue(); err != nil {
		return fmt.Errorf("%w (an indexed read is held to the same: it lists the repositories searched "+
			"by index, a page of MaxSnapshotItems at a time, after its cursor)", err)
	}
	return nil
}

// validateExpungeTips is [ReadQuery.validate] for an `expunge_tips` read, the
// maintainer's own: NO asker, its repository by id and the instant its drops
// are listed from, never zero (the scan's less the grace kept), and nothing
// else; a page is MaxSnapshotItems drops.
func (q ReadQuery) validateExpungeTips() error {
	in := q.ExpungeTips
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "an expunge_tips read is the engine's own, which no principal makes")
	case in == nil:
		return refused("expunge_tips", "an expunge_tips read names its repository and the instant its drops are "+
			"listed from")
	case !canonicalUUID(in.RepoID):
		return refused("expunge_tips.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", in.RepoID))
	case in.Since.IsZero():
		return refused("expunge_tips.since", "an expunge_tips read names the instant its drops are listed from: "+
			"the scan's, less the grace the expunge keeps")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"checks_queued", q.ChecksQueued != nil}, {"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "an expunge_tips read lists one repository's ref drops since an instant, a "+
				"page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateMirrorPush is [ReadQuery.validate] for a `mirror_push` read, the
// home's own: NO asker, its repository by id and nothing else, no cursor or
// limit, since a repository holds one push mirror and one green row a
// protected branch.
func (q ReadQuery) validateMirrorPush() error {
	in := q.MirrorPush
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a mirror_push read is the engine's own, which no principal makes")
	case in == nil:
		return refused("mirror_push", "a mirror_push read names the repository whose push mirror it reads")
	case !canonicalUUID(in.RepoID):
		return refused("mirror_push.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", in.RepoID))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil}, {"cursor", q.Cursor != ""},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"checks_queued", q.ChecksQueued != nil}, {"default_tips", q.DefaultTips != nil},
		{"branch_changes", q.BranchChanges != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
	} {
		if f.set {
			return refused(f.field, "a mirror_push read answers one repository's push mirror and green rows, "+
				"and names nothing else")
		}
	}
	return nil
}

// mirrorPush serves a `mirror_push` read ([ReadMirrorPush]): the repository's
// push mirror (none where it has none or is gone) and every green row, from
// one snapshot, each a primary-key seek: a home asks it once an attempt.
func (r *Reader) mirrorPush(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	id := q.MirrorPush.RepoID
	remotes, err := remotesIn(ctx, tx, id)
	if err != nil || remotes.to == nil {
		return err
	}
	c := MirrorCandidate{ID: id, To: remotes.mirrorTo()}
	if err = tx.QueryRowContext(ctx, repoNameSQL, id).Scan(&c.Name); err != nil {
		return fmt.Errorf("gitlet: read repository %s's name: %w", id, err)
	}
	green, err := greenRowsIn(ctx, tx, id)
	if err != nil {
		return err
	}
	out.Mirrors, out.Green = []MirrorCandidate{c}, green
	return nil
}

// validateBranchChanges is [ReadQuery.validate] for a `branch_changes` read,
// the engine's own for a collected run's delivered refs: NO asker; one to
// [MaxRunRepos] repositories by id, once each, each with branches under
// refs/heads/ once each, [MaxRefsPerQuery] in all; and nothing else, since one
// page answers every branch, each one change at most.
func (q ReadQuery) validateBranchChanges() error {
	in := q.BranchChanges
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a branch_changes read is the engine's own, which no principal makes")
	case in == nil:
		return refused("branch_changes", "a branch_changes read names its repositories and their branches")
	case len(in.Repos) == 0 || len(in.Repos) > MaxRunRepos:
		return refused("branch_changes.repos", fmt.Sprintf("%d repositories; a branch_changes read names one "+
			"to %d (MaxRunRepos)", len(in.Repos), MaxRunRepos))
	}
	repos := make(map[string]bool, len(in.Repos))
	branches := 0
	for i, repo := range in.Repos {
		field := fmt.Sprintf("branch_changes.repos[%d]", i)
		switch {
		case !canonicalUUID(repo.RepoID):
			return refused(field+".repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
				"lowercase spelling", repo.RepoID))
		case repos[repo.RepoID]:
			return refused(field+".repo_id", fmt.Sprintf("%s is named twice — a repository's branches are "+
				"named together", repo.RepoID))
		case len(repo.Refs) == 0:
			return refused(field+".refs", "a repository is named with the branches whose changes the read answers")
		}
		repos[repo.RepoID] = true
		if branches += len(repo.Refs); branches > MaxRefsPerQuery {
			return refused(field+".refs", fmt.Sprintf("past %d branches; a branch_changes read names at most %d "+
				"in all (MaxRefsPerQuery)", MaxRefsPerQuery, MaxRefsPerQuery))
		}
		named := make(map[string]bool, len(repo.Refs))
		for j, ref := range repo.Refs {
			if why := branchProblem(ref); why != "" {
				return refused(fmt.Sprintf("%s.refs[%d]", field, j), why)
			}
			if named[ref] {
				return refused(fmt.Sprintf("%s.refs[%d]", field, j), fmt.Sprintf("%s is named twice", ref))
			}
			named[ref] = true
		}
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil}, {"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"cursor", q.Cursor != ""}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil}, {"checks_queued", q.ChecksQueued != nil}, {"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a branch_changes read answers its repositories' branches' live changes "+
				"in one page, and names nothing else")
		}
	}
	return nil
}

// branchChanges serves a `branch_changes` read ([ReadBranchChanges]): the live
// change each named branch heads, in the query's order, one seek of
// code_changes_open_idx a branch (liveBySourceSQL), in one transaction. NOT
// HELD TO A GRANT: the engine names its own run's refs.
func (r *Reader) branchChanges(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	for _, repo := range q.BranchChanges.Repos {
		var heads []BranchChange
		if err := inChunks(repo.Refs, func(part []string) error {
			encoded, err := json.Marshal(part)
			if err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, liveBySourceSQL, repo.RepoID, string(encoded))
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				row, err := scanChange(rows.Scan)
				if err != nil {
					return err
				}
				heads = append(heads, BranchChange{RepoID: repo.RepoID, Ref: row.source, ChangeID: row.id,
					Number: row.number})
			}
			return rows.Err()
		}); err != nil {
			return fmt.Errorf("gitlet: read the live changes of repository %s's branches: %w", repo.RepoID, err)
		}
		order := make(map[string]int, len(repo.Refs))
		for i, ref := range repo.Refs {
			order[ref] = i
		}
		slices.SortFunc(heads, func(a, b BranchChange) int { return order[a.Ref] - order[b.Ref] })
		out.BranchChanges = append(out.BranchChanges, heads...)
	}
	return nil
}

// freshness is the level a read is served at: any level [checkFreshness]
// admits, so a screen reads its own writes back at `session` from the
// position its write answered (the dashboard's SESSION_QUERIES). Where none is
// named, a principal's read and a duty listing are read stale, the REST path's
// level; the engine's own `introduced`, `run_pushes`, `expunge_tips` and
// `branch_changes` are refused, since their caller must name the level its
// "nothing" may be said at: read stale, a run's pushes would answer none for a
// push another node already applied.
func (q ReadQuery) freshness() (statelog.Freshness, error) {
	fresh := q.Fresh
	if fresh.Level == "" && q.Kind != ReadIntroduced && q.Kind != ReadRunPushes && q.Kind != ReadExpungeTips &&
		q.Kind != ReadBranchChanges {
		fresh.Level = statelog.ReadStale
	}
	return fresh, checkFreshness(fresh)
}

// validRefPrefix reports a prefix a refs listing may name: refs/heads/ or
// refs/tags/, or a directory of refs under either — a seat's own namespace —
// ending in "/". A prefix that no ref could begin with is refused rather than
// answered empty.
func validRefPrefix(prefix string) bool {
	return (strings.HasPrefix(prefix, branchPrefix) || strings.HasPrefix(prefix, tagPrefix)) &&
		strings.HasSuffix(prefix, "/") && ValidRef(prefix+"x")
}

// validRepoPrefix reports a prefix a repositories listing may name: a name
// whole, or the opening of one, which a "0" completes since a digit may end a
// label and follow a hyphen or dot. Any other is refused, not answered empty.
func validRepoPrefix(prefix string) bool {
	return ValidName(prefix) || ValidName(prefix+"0")
}

// prefixEnd is the first string past every one prefix begins: prefix with its
// last byte one higher, so [prefix, prefixEnd) is one range of an index by
// name. A refs listing's prefix ends in "/" and a repository name is ASCII
// throughout, so the last byte is never 0xff and the step never carries.
func prefixEnd(prefix string) string {
	return prefix[:len(prefix)-1] + string([]byte{prefix[len(prefix)-1] + 1})
}

// limitOr is a limit, or its kind's default.
func limitOr(limit, fallback int) int {
	if limit == 0 {
		return fallback
	}
	return limit
}

// repos serves a listing of repositories, by name or by last push
// ([reposListed]), of those the asker and its ceiling may read ([JudgeRead]),
// each carrying the asker's access.
//
// THE PAGE IS WHAT WAS SCANNED, NOT WHAT WAS KEPT: the cursor is the last row
// scanned, so a page may be short or empty with a Next and a caller pages
// until no Next. Cut at kept rows, a page of unreadable repositories would
// end the listing early, or filling a page would scan without bound.
func (r *Reader) repos(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {
	statuses := q.Status
	if len(statuses) == 0 {
		// THE REPOSITORIES IN USE, importing ones included (fetched, filling as
		// the import lands); deleted and purging ones are asked for.
		statuses = []RepoStatus{RepoStatusImporting, RepoStatusActive, RepoStatusArchived}
	}
	names := make([]string, len(statuses))
	for i, s := range statuses {
		names[i] = string(s)
	}
	list, err := jsonList(names)
	if err != nil {
		return err
	}
	limit := min(limitOr(q.Limit, defaultRepoLimit), r.pageRows)
	if len(q.DayBounds) > 0 {
		// The sparks' read is measured at this page at most, whatever the default.
		limit = min(limit, MaxSparkReposPerPage)
	}
	listed, err := reposListed(ctx, tx, q, list, limit)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(listed))
	cursors := make([]string, 0, len(listed))
	for _, l := range listed {
		g, refused := JudgeRead(q.Asker, q.CeilingSeat, l.v.ref, chart)
		if refused != nil {
			continue
		}
		l.v.ref.Position = cp
		s := l.v.summary(l.head)
		s.Access = g.Access.String()
		out.Repos = append(out.Repos, s)
		ids = append(ids, s.ID)
		cursors = append(cursors, l.cursor)
	}
	if err = protectionsOf(ctx, tx, ids, out.Repos); err != nil {
		return err
	}
	// ITS FACTS WHERE ASKED, read before the page is cut so it is measured with them, and
	// on a first page of a listing narrowed by its kind and statuses alone, its whole.
	if q.RepoFacts {
		facts := make([]*RepoSummary, len(out.Repos))
		for i := range out.Repos {
			facts[i] = &out.Repos[i]
		}
		if err = withFacts(ctx, tx, facts); err != nil {
			return err
		}
		if q.Cursor == "" && q.Prefix == "" && !q.Protected {
			if out.ReposTotal, err = r.reposTotal(ctx, tx, q, chart, statuses, list); err != nil {
				return err
			}
		}
	}
	// ITS PUSHES ON THE ASKER'S DAYS where DayBounds asks, before the cut so the page is measured with them.
	if len(q.DayBounds) > 0 {
		days, sparkErr := pushesOnDays(ctx, tx, ids, q.DayBounds)
		if sparkErr != nil {
			return sparkErr
		}
		for i := range out.Repos {
			out.Repos[i].Spark = days[out.Repos[i].ID]
		}
	}
	// MEASURED WITH THEIR RULES, most of a row's bytes when a repository has
	// many. The page is cut at the row that fills it, its cursor that row's;
	// otherwise full when the statement filled its limit, its cursor the last
	// row scanned. EACH ROW'S CARD is read as the page fills, its push rows
	// charged to the page, so a row past the cut reads none.
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for i := range out.Repos {
		if q.RepoFacts {
			if err = r.carded(ctx, tx, &out.Repos[i], &pg); err != nil {
				return err
			}
		}
		if pg.add(out.Repos[i].encodedBytes()) {
			out.Next = cursors[i]
			out.Repos = out.Repos[:i+1]
			return nil
		}
	}
	if len(listed) == limit {
		out.Next = listed[len(listed)-1].cursor
	}
	return nil
}

// protectionsOf fills a page of repositories' rules in ONE statement, each as
// a push is judged by it ([PushRule]): a listing answers no merge gate
// ([ReadQuery.Gates]), so its rows weigh what it sends.
func protectionsOf(ctx context.Context, tx *sql.Tx, ids []string, repos []RepoSummary) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, pushRulesOfSQL, list)
	if err != nil {
		return fmt.Errorf("gitlet: read the listing's protections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	at := make(map[string]int, len(repos))
	for i, s := range repos {
		at[s.ID] = i
	}
	for rows.Next() {
		var id, direct string
		var p PushRule
		if err := rows.Scan(&id, &p.Pattern, &p.AllowForce, &p.AllowDelete, &direct); err != nil {
			return fmt.Errorf("gitlet: read the listing's protections: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		i := at[id]
		repos[i].Protections = append(repos[i].Protections, p)
	}
	return rows.Err()
}

// repo serves q's repository in detail, and the record that last moved it, to
// an asker and ceiling that may read it ([JudgeRead]): else [ErrNotPermitted]
// or [ErrAboveCeiling], judged after the row is read. A missing one is
// [ErrRepoNotFound]: a `visibility: unit` repository's existence is no secret
// (the company's document names it), only its contents are.
func (r *Reader) repo(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	cp statelog.Position, out *ReadAnswer) error {
	name := q.Repo
	v, found, err := namedIn(ctx, tx, name, cp)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, name)
	}
	id := v.ref.ID
	g, err := JudgeRead(q.Asker, q.CeilingSeat, v.ref, chart)
	if err != nil {
		return err
	}
	head, _, err := liveRefIn(ctx, tx, id, branchPrefix+v.ref.DefaultBranch)
	if err != nil {
		return fmt.Errorf("gitlet: read %s's head: %w", name, err)
	}
	// THE COUNTS ARE THE ROW'S, kept by the applier, never a walk of every ref
	// on every detail read.
	summary := v.summary(head.OID)
	summary.Access = g.Access.String()
	out.Repo = &RepoDetail{RepoSummary: summary,
		Branches: v.branches, Tags: v.tags, Tombstones: v.tombstones,
		CreatedAt: store.DecodeTime(v.createdAt), Limits: r.limits()}
	// THE PATH OWNERS ONLY TO A READ THAT ASKS ([ReadQuery.PathOwners]): at
	// their bound a quarter MiB that every resolution would carry and drop.
	if q.PathOwners {
		if out.Repo.PathOwners, err = pathOwnersIn(ctx, tx, id); err != nil {
			return err
		}
	}
	// AND THE GATES ONLY TO A READ THAT ASKS ([ReadQuery.Gates]), with who
	// set each rule and when, in one seek of the rules ([ruleDetailsIn]) in
	// the transaction the row's rules came from, so every gate names one.
	if q.Gates {
		if out.Repo.Gates, out.Repo.RuleSources, err = ruleDetailsIn(ctx, tx, id); err != nil {
			return err
		}
	}
	// ITS PUSHES ON THE ASKER'S DAYS AND WHO PUSHED, where DayBounds asks beside RepoFacts.
	if len(q.DayBounds) > 0 {
		var days map[string][]int64
		if days, err = pushesOnDays(ctx, tx, []string{id}, q.DayBounds); err != nil {
			return err
		}
		out.Repo.Spark = days[id]
		if out.Repo.Pushers, err = pushersIn(ctx, tx, id, q.DayBounds, MaxPushersAnswered); err != nil {
			return err
		}
	}
	// AND ITS FACTS ONLY TO A READ THAT ASKS ([ReadQuery.RepoFacts]).
	if q.RepoFacts {
		if err = withFacts(ctx, tx, []*RepoSummary{&out.Repo.RepoSummary}); err != nil {
			return err
		}
		if err = r.carded(ctx, tx, &out.Repo.RepoSummary, nil); err != nil {
			return err
		}
	}
	// AND ITS EXPUNGES' AND OTHER HOSTS' PROGRESS (a purge, an import, a
	// mirror's last sync), in one seek of its row ([detailRowIn]).
	out.Repo.Expunge, out.Repo.Remotes, out.Repo.Version, err = detailRowIn(ctx, tx, id)
	return err
}

// detailRowIn reads what a repository's detail serves of its row beyond its
// summary (its expunges [expungeState.view], its other hosts [RemoteState]
// and the record that last moved it) in ONE primary-key statement, since
// every resolution of a repository reads this detail. The applier's and the
// decide's narrower reads ([expungeStateSQL], [remotesSQL]) stay their own.
func detailRowIn(ctx context.Context, tx *sql.Tx, id string) (ExpungeState, RemoteState, statelog.Position,
	error) {
	var (
		state                             expungeState
		remotes                           RemoteState
		from, to, source, failed          string
		chunks, doneAt, syncedAt, version int64
	)
	dests := append(state.dests(), &from, &to, &source, &chunks, &doneAt, &failed, &syncedAt, &version)
	if err := tx.QueryRowContext(ctx, detailRowSQL, id).Scan(dests...); err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's detail row: %w", id, err)
	}
	var err error
	if remotes.MirrorFrom, err = remoteOf[MirrorSource](from); err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's mirror_from: %w", id, err)
	}
	if remotes.MirrorTo, err = remoteOf[MirrorTarget](to); err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's mirror_to: %w", id, err)
	}
	imported, err := remoteOf[ImportSource](source)
	if err != nil {
		return ExpungeState{}, RemoteState{}, statelog.Position{},
			fmt.Errorf("gitlet: read repository %s's import_source: %w", id, err)
	}
	if imported != nil {
		remotes.Import = &ImportState{Source: *imported, Chunks: chunks, Error: failed}
		if doneAt != 0 {
			remotes.Import.DoneAt = store.DecodeTime(doneAt)
		}
	}
	if syncedAt != 0 {
		remotes.MirrorSyncedAt = store.DecodeTime(syncedAt)
	}
	return state.view(), remotes, unpacked(version), nil
}

// detailRowSQL reads what [detailRowIn] serves, by the primary key: the
// expunge columns first, in [expungeStateSQL]'s order ([expungeState.dests]),
// then the other hosts', then the row's version — registered in plan_test.go.
const detailRowSQL = `SELECT drop_epoch, expunge_id, expunge_request, expunge_request_position,
	expunged_at, expunge_dropped_objects, expunge_dropped_bytes, mirror_from, mirror_to, import_source,
	import_seq, imported_at, import_error, mirror_synced_at, version FROM code_repos WHERE id = ?`

// pushes serves one repository's push history, newest first IN LOG ORDER,
// keyset-paged by (position, push_id): group commit puts up to 64 pushes in
// one record sharing its position, and the broker's instant steps back across
// a leader change (replicated 0084). The history is the repository's contents,
// refused as [Reader.repo] refuses. Each row names the change its record
// merged ([PushRow.Change]); a narrowed history is [Reader.pushesNarrowed]'s.
func (r *Reader) pushes(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	id := repo.ref.ID
	if q.Push != "" {
		// ONE PUSH, WHOLE, by its key, and only one this history holds;
		// another repository's is an answer with none.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		row, found, err := pushIn(ctx, tx, q.Push)
		if err != nil {
			return fmt.Errorf("gitlet: read %s's push %s: %w", q.Repo, q.Push, err)
		}
		if found && row.RepoID == id {
			rows := []PushRow{row}
			var named mergedNames
			named.add(0, row)
			if _, err = named.name(ctx, tx, repo.ref, rows); err != nil {
				return err
			}
			out.Pushes = rows
		}
		return nil
	}
	// THE REPOSITORY'S PUSHES ON THE ASKER'S DAYS where DayBounds asks, whatever the page holds.
	if len(q.DayBounds) > 0 {
		var days map[string][]int64
		if days, err = pushesOnDays(ctx, tx, []string{id}, q.DayBounds); err != nil {
			return err
		}
		out.PushDays = days[id]
	}
	if q.PushFilter != nil {
		return r.pushesNarrowed(ctx, tx, q, repo.ref, out)
	}
	limit := limitOr(q.Limit, defaultPushLimit)
	var rows *sql.Rows
	if q.Cursor == "" {
		rows, err = tx.QueryContext(ctx, pushesFirstSQL, id, limit)
	} else {
		position, pushID, ok := cutHistoryCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, pushesAfterSQL, id, position, position, pushID, limit)
	}
	if err != nil {
		return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	var (
		named   mergedNames
		charges []int
	)
	for rows.Next() {
		var (
			row      PushRow
			commands int
		)
		if row, commands, err = scanPush(rows); err != nil {
			return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
		}
		named.add(len(out.Pushes), row)
		// A ROW IN BRIEF IS CHARGED THE ROW IT READ, or what it sends if
		// larger: charged by what it sends alone, a page of maximal pushes
		// would read without bound ([scanIntroduced]).
		charge := row.encodedBytes(commands)
		if q.Brief {
			row = row.brief()
			charge = max(charge, row.encodedBytes(row.commandsBytes()))
		}
		out.Pushes = append(out.Pushes, row)
		charges = append(charges, charge)
		if pg.add(charge) {
			out.Next = logCursor(row.Position.Packed(), row.PushID)
			break
		}
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read %s's pushes: %w", q.Repo, err)
	}
	grew, err := named.name(ctx, tx, repo.ref, out.Pushes)
	if err != nil {
		return err
	}
	if kept := recut(charges, grew, limit, r.pageBytes); kept < len(out.Pushes) {
		out.Pushes = out.Pushes[:kept]
		last := out.Pushes[kept-1]
		out.Next = logCursor(last.Position.Packed(), last.PushID)
	}
	return nil
}

// refsListing serves one page of a repository's LIVE refs under a prefix, by
// name or as a [RefListingQuery] orders them, keyset after the last ref
// served, to an asker and ceiling that may read it ([JudgeRead]); the first
// page carries the repository's detail too, in the same transaction, so a
// seat's first question is one read. The detail rides in the half of the
// estate reply MaxPageBytes leaves free.
//
// OVER code_refs_live_idx (replicated 0082, its predicate repeated verbatim),
// never code_refs' key, whose range also holds up to MaxTombstonesPerRepo
// tombstones to step over. A directory's names lie between dir + "/" and
// dir + "0", the push decide's range.
func (r *Reader) refsListing(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	cp statelog.Position, out *ReadAnswer) error {

	prefix := cmp.Or(q.Prefix, branchPrefix)
	after, err := q.refsAfter(prefix)
	if err != nil {
		return err
	}
	var id string
	if q.Cursor == "" {
		if err = r.repo(ctx, tx, q, chart, cp, out); err != nil {
			return err
		}
		id = out.Repo.ID
	} else {
		var found bool
		if id, found, err = claimIn(ctx, tx, q.Repo); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		if err = mayReadIn(ctx, tx, id, q.Asker, q.CeilingSeat, chart); err != nil {
			return err
		}
	}
	// THE PAGE IS refslisting.go's, by name where nothing orders it otherwise.
	if q.RefListing == nil {
		q.RefListing = &RefListingQuery{}
	}
	return r.refsListed(ctx, tx, q, chart, cp, id, after, out)
}

// introduced serves one page of a repository's pushes in LOG ORDER for a Gitlet
// node's index of which push introduced each commit: (After, Through]
// ascending (Through zero: this read's checkpoint) or below Before descending;
// keyset `<packed position>/<push id>`, since group commit puts up to 64 pushes
// at one position. A page serves at least one push and ends at its row limit
// or MaxPageBytes, charged as [scanIntroduced] says. A purged repository is
// [ErrRepoNotFound], which drops the index that asked.
//
// ITS ROW LIMIT IS MaxSnapshotItems, never MaxPushesPerPage (a person's
// screen): its bytes bound its cost, and the narrowest pushes (345 bytes) fill
// about the same 4 MiB (internal/gitlet/cache's measured section), where 200 a
// page would read a monorepo's 181 days in 90 000 pages rather than 2 400.
func (r *Reader) introduced(ctx context.Context, tx *sql.Tx, q ReadQuery, cp statelog.Position,
	out *ReadAnswer) error {

	in := q.Introduced
	var name string
	switch err := tx.QueryRowContext(ctx, repoNameSQL, in.RepoID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, in.RepoID)
	case err != nil:
		return fmt.Errorf("gitlet: read repository %s: %w", in.RepoID, err)
	}
	through := in.Through.Packed()
	if in.Through.IsZero() {
		through = cp.Packed()
	}
	limit := min(limitOr(q.Limit, r.pageRows), r.pageRows)
	var rows *sql.Rows
	var err error
	switch position, pushID, keyed := cutLogCursor(q.Cursor); {
	case q.Cursor != "" && (!keyed || (in.Descending && position >= in.Before.Packed()) ||
		(!in.Descending && position <= in.After.Packed())):
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	case in.Descending && q.Cursor == "":
		rows, err = tx.QueryContext(ctx, pushesBelowSQL, in.RepoID, in.Before.Packed(), limit)
	case in.Descending:
		rows, err = tx.QueryContext(ctx, pushesAfterSQL, in.RepoID, position, position, pushID, limit)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, pushesAboveSQL, in.RepoID, in.After.Packed(), through, limit)
	default:
		rows, err = tx.QueryContext(ctx, pushesAboveAfterSQL, in.RepoID, position, through, position, pushID,
			limit)
	}
	if err != nil {
		return fmt.Errorf("gitlet: read %s's pushes in log order: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		row, cost, err := scanIntroduced(rows)
		if err != nil {
			return fmt.Errorf("gitlet: read %s's pushes in log order: %w", name, err)
		}
		out.Introduced = append(out.Introduced, row)
		if pg.add(cost) {
			out.Next = logCursor(row.Position.Packed(), row.PushID)
			break
		}
	}
	return rows.Err()
}

// runPushes serves one page of an executor's pushes ([ReadRunPushes]): each
// named repository in order, its pushes above the launch position in LOG
// ORDER, answering those naming the run; keyset after the last push READ,
// `<repository index>/<packed position>/<push id>`, the run's or not. NOT HELD
// TO A GRANT: the run was given only repositories its seat could read.
//
// A PAGE IS HELD TO WHAT IT READS: code_pushes_repo_idx carries no run
// (replicated 0098), so every push beside the run's is read to test it. A
// page reads at most MaxSnapshotItems pushes, stopping once their row_bytes
// WEIGH MaxPageBytes ([pushesWithin]'s rule), weighed by covering seeks
// ([runSpanOf]), never in Go; it may end with none kept and a cursor.
// BenchmarkRunPushes holds every page under a tenth of statelog.ReadBudget.
func (r *Reader) runPushes(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	in := q.RunPushes
	after := in.After.Packed()
	// A PAGE READS AFTER A PLACE: its cursor's push, or on a repository's
	// first page (after+1, ""), before every push past the launch, since no
	// push's id is empty.
	start, position, pushID := 0, after+1, ""
	if q.Cursor != "" {
		var keyed bool
		if start, position, pushID, keyed = cutRunPushCursor(q.Cursor); !keyed || start >= len(in.Repos) ||
			position <= after {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
	}
	reads := page{maxRows: min(limitOr(q.Limit, r.pageRows), r.pageRows), maxB: r.pageBytes}
	sent := 0
	for i := start; i < len(in.Repos); i++ {
		full, err := r.runPushesOf(ctx, tx, i, in, position, pushID, &reads, &sent, out)
		if err != nil || full {
			return err
		}
		position, pushID = after+1, ""
	}
	return nil
}

// runPushesOf reads the pushes of the run's i-th repository after the place
// (position, pushID), as many as reads still takes, adds those naming the run
// to out — what they send counted in sent — and reports whether the page is
// full, setting out.Next to the last push it READ: the one that filled its
// reads, or the run's push that filled what it sends.
func (r *Reader) runPushesOf(ctx context.Context, tx *sql.Tx, i int, in *RunPushesQuery, position int64,
	pushID string, reads *page, sent *int, out *ReadAnswer) (bool, error) {

	repo := in.Repos[i]
	span, err := runSpanOf(ctx, tx, repo, position, pushID, *reads)
	if err != nil {
		return false, fmt.Errorf("gitlet: weigh the pushes to %s a page of run %s's may read: %w", repo, in.Run,
			err)
	}
	if span.pushes == 0 {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, runPushesInSQL, repo, in.Run, position, pushID, span.lastPosition,
		span.lastID)
	if err != nil {
		return false, fmt.Errorf("gitlet: read run %s's pushes to %s: %w", in.Run, repo, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, commands, err := scanPush(rows)
		if err != nil {
			return false, fmt.Errorf("gitlet: read run %s's pushes to %s: %w", in.Run, repo, err)
		}
		out.Pushes = append(out.Pushes, row)
		if *sent += row.encodedBytes(commands); *sent >= reads.maxB {
			out.Next = runPushCursor(i, row.Position.Packed(), row.PushID)
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("gitlet: read run %s's pushes to %s: %w", in.Run, repo, err)
	}
	reads.rows += span.pushes
	reads.bytes += span.weight
	if reads.full() {
		out.Next = runPushCursor(i, span.lastPosition, span.lastID)
		return true, nil
	}
	return false, nil
}

// runSpan is how far a page reads one repository's pushes: how many, what
// their rows weigh, and the last one's place in log order.
type runSpan struct {
	pushes, weight int
	lastPosition   int64
	lastID         string
}

// runSpanOf is the span of a repository's pushes after the place (position,
// pushID) that a page may still read, what it has read so far counted in
// reads, from code_pushes_repo_idx alone: as many as it has rows left, and
// none past the first whose weight fills its bytes (reads is the page's copy,
// so weighing moves nothing the page counts).
func runSpanOf(ctx context.Context, tx *sql.Tx, repo string, position int64, pushID string,
	reads page) (runSpan, error) {

	var span runSpan
	if err := tx.QueryRowContext(ctx, runWeighSQL, repo, position, pushID, reads.left()).Scan(&span.pushes,
		&span.weight); err != nil {
		return runSpan{}, err
	}
	if span.pushes == 0 {
		return span, nil
	}
	if reads.bytes+span.weight >= reads.maxB {
		// THEY WEIGH MORE THAN THE PAGE'S BYTES LEFT: their weights, in log
		// order, to the one that fills it.
		var err error
		if span.pushes, span.weight, err = runWeights(ctx, tx, repo, position, pushID, reads); err != nil {
			return runSpan{}, err
		}
	}
	err := tx.QueryRowContext(ctx, runLastSQL, repo, position, pushID, span.pushes-1).Scan(&span.lastPosition,
		&span.lastID)
	return span, err
}

// runWeights walks the weights of a repository's pushes after the place
// (position, pushID) in log order, counting each into reads — the page's copy
// — and answers how many it walked and what they weigh: up to the push that
// fills the page, or as many as it has rows left.
func runWeights(ctx context.Context, tx *sql.Tx, repo string, position int64, pushID string,
	reads page) (pushes, weight int, err error) {

	rows, err := tx.QueryContext(ctx, runWeightsSQL, repo, position, pushID, reads.left())
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var w int
		if err := rows.Scan(&w); err != nil {
			return 0, 0, err
		}
		pushes, weight = pushes+1, weight+w
		if reads.add(w) {
			break
		}
	}
	return pushes, weight, rows.Err()
}

// runPushCursor is "<repository index>/<packed position>/<push id>": a push's
// place among the pushes a run's read pages through — the repository of the
// run's query it was made to, then its place in that repository's log order,
// its [logCursor].
func runPushCursor(repo int, position int64, pushID string) string {
	return strconv.Itoa(repo) + "/" + logCursor(position, pushID)
}

// cutRunPushCursor reads a [runPushCursor].
func cutRunPushCursor(s string) (repo int, position int64, pushID string, ok bool) {
	index, rest, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, "", false
	}
	repo, err := strconv.Atoi(index)
	if err != nil || repo < 0 || strconv.Itoa(repo) != index {
		return 0, 0, "", false
	}
	position, pushID, ok = cutLogCursor(rest)
	return repo, position, pushID, ok
}

// compactionDue serves one page of a `compaction_due` read: the active and
// archived repositories holding at least q.MinPacks live packs, with their
// row's version, one ranged walk of code_repos_packs_idx (replicated 0088,
// its predicate repeated verbatim), keyset on (live packs, id).
//
// MOST LIVE PACKS FIRST, so one near MaxPacksPerRepo (about to refuse pushes
// compaction_behind) never waits. ARCHIVED too: it is still read, each cold
// sync a GET a pack. THE VERSION keys the compactor's memory of a plan found
// not due. A FULL LISTING, never a delta, which would need an index on
// code_repos.version, rewritten by every record.
func (r *Reader) compactionDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	var (
		rows *sql.Rows
		err  error
	)
	switch packs, id, keyed := cutDueCursor(q.Cursor); {
	case q.Cursor != "" && !keyed:
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, compactionDueFirstSQL, q.MinPacks, r.pageRows)
	default:
		rows, err = tx.QueryContext(ctx, compactionDueAfterSQL, q.MinPacks, packs, packs, id, r.pageRows)
	}
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories a compaction may be due in: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c       CompactionCandidate
			version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.LivePacks, &c.LiveObjects, &version); err != nil {
			return fmt.Errorf("gitlet: list the repositories a compaction may be due in: %w", err)
		}
		c.Version = unpacked(version)
		out.Due = append(out.Due, c)
		if pg.add(c.encodedBytes()) {
			out.Next = dueCursor(c)
			break
		}
	}
	return rows.Err()
}

// dueCursor is "<live packs>/<id>": a repository's place in a `compaction_due`
// listing's order.
func dueCursor(c CompactionCandidate) string {
	return strconv.FormatInt(c.LivePacks, 10) + "/" + c.ID
}

// cutDueCursor reads a [dueCursor].
func cutDueCursor(s string) (int64, string, bool) {
	at, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return 0, "", false
	}
	packs, err := strconv.ParseInt(at, 10, 64)
	if err != nil || packs < 0 {
		return 0, "", false
	}
	return packs, id, true
}

// publicationDue serves one page of a `publication_due` read: the active and
// archived repositories holding a live pack no published layer covers, LEAST
// RECENTLY PUBLISHED FIRST (never published first), then by id, with what the
// due rule reads and the row's version: ONE WALK of code_repos_unpublished_idx
// (replicated 0090, its predicate repeated verbatim), a full listing. The
// published counts are the last publication's recount, so a listed repository
// may have nothing to publish, which only a plan can tell
// ([PublicationCandidate]).
//
// NONE WITH AN EXPUNGE OPEN: rule 6g refuses those publications
// (publication.go), and listed, its publisher would build a layer (perhaps a
// whole base) for the committer to refuse at every poll.
func (r *Reader) publicationDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	var (
		rows *sql.Rows
		err  error
	)
	switch at, id, keyed := cutPublishCursor(q.Cursor); {
	case q.Cursor != "" && !keyed:
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, publicationDueFirstSQL, r.pageRows)
	default:
		rows, err = tx.QueryContext(ctx, publicationDueAfterSQL, at, at, id, r.pageRows)
	}
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories a publication may be due in: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c                    PublicationCandidate
			lastPush             sql.NullInt64
			published, tookMs, v int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.LivePacks, &c.PublishedPacks, &c.LiveObjects,
			&c.PublishedObjects, &lastPush, &published, &tookMs, &v); err != nil {
			return fmt.Errorf("gitlet: list the repositories a publication may be due in: %w", err)
		}
		if lastPush.Valid {
			c.LastPushAt = store.DecodeTime(lastPush.Int64)
		}
		// ZERO IS NEVER: the column's default, and no publication's broker
		// instant.
		if published != 0 {
			c.PublishedAt = store.DecodeTime(published)
		}
		c.PublishTook, c.Version = time.Duration(tookMs)*time.Millisecond, unpacked(v)
		out.Publish = append(out.Publish, c)
		if pg.add(c.encodedBytes()) {
			out.Next = publishCursor(published, c.ID)
			break
		}
	}
	return rows.Err()
}

// expungeDue serves one page of an `expunge_due` read in two ARMS, the first
// whole before the second, each a full listing with its partial predicate
// repeated verbatim:
//
//  1. OPEN: active and archived repositories with an expunge open or a purge
//     requested, BY ID, one walk of code_repos_expunging_idx (replicated 0092);
//  2. WEEKLY ([ReadQuery.ScannedBefore]): every OTHER active or archived
//     repository of any kind (repo_size counts every live byte) last JUDGED
//     ([ExpungeScan]) at or before the instant or never, LEAST RECENTLY FIRST,
//     one seek of code_repos_expunge_due_idx (replicated 0139).
//
// Deleted or purging: in neither (its expunge awaits a restore, or the purge).
func (r *Reader) expungeDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	weekly, after, ok := cutExpungeCursor(q.Cursor)
	if !ok || (weekly && q.ScannedBefore.IsZero()) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	if !weekly {
		rows, err := tx.QueryContext(ctx, expungeDueSQL, q.Cursor, r.pageRows)
		if err != nil {
			return fmt.Errorf("gitlet: list the repositories an expunge is owed in: %w", err)
		}
		full, err := scanExpungeDue(rows, &pg, out, func(c ExpungeCandidate, _ int64) string { return c.ID })
		if err != nil || full || q.ScannedBefore.IsZero() {
			return err
		}
	}
	before := store.EncodeTime(q.ScannedBefore)
	var (
		rows *sql.Rows
		err  error
	)
	if after == nil {
		rows, err = tx.QueryContext(ctx, expungeWeeklySQL, before, r.pageRows-pg.rows)
	} else {
		rows, err = tx.QueryContext(ctx, expungeWeeklyAfterSQL, before, after.at, after.at, after.id,
			r.pageRows-pg.rows)
	}
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories an expunge may be due in: %w", err)
	}
	_, err = scanExpungeDue(rows, &pg, out, func(c ExpungeCandidate, scanned int64) string {
		return weeklyCursor(scanned, c.ID)
	})
	return err
}

// scanExpungeDue reads one arm's rows of an `expunge_due` page into out,
// counting each against pg; full reports a page that took its last row, its
// cursor named by next from the row and its expunge_scanned_at.
func scanExpungeDue(rows *sql.Rows, pg *page, out *ReadAnswer,
	next func(c ExpungeCandidate, scanned int64) string) (full bool, err error) {

	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c                                     ExpungeCandidate
			requested, expunged, scanned, version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Open, &c.Request, &requested, &expunged, &scanned, &c.LivePacks,
			&c.LiveBytes, &c.LiveObjects, &version); err != nil {
			return false, fmt.Errorf("gitlet: list the repositories an expunge is owed in: %w", err)
		}
		if c.Request != "" {
			c.Requested = unpacked(requested)
		}
		// ZERO IS NEVER: the column's default, and no commit's broker
		// instant.
		if expunged != 0 {
			c.ExpungedAt = store.DecodeTime(expunged)
		}
		c.Version = unpacked(version)
		out.Expunge = append(out.Expunge, c)
		if pg.add(c.encodedBytes()) {
			out.Next = next(c, scanned)
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// weeklyPrefix opens an `expunge_due` cursor in its WEEKLY arm: the last
// row's expunge_scanned_at, encoded, and its id — "weekly:<at>:<id>". A cursor in
// the open arm is the last id served, a UUID, which never opens so.
const weeklyPrefix = "weekly:"

// weeklyCursor is the cursor after a weekly-arm row last judged at scanned
// (encoded) of id.
func weeklyCursor(scanned int64, id string) string {
	return weeklyPrefix + strconv.FormatInt(scanned, 10) + ":" + id
}

// expungeAfter is a weekly-arm cursor read back: the row it names.
type expungeAfter struct {
	at int64
	id string
}

// cutExpungeCursor reads an `expunge_due` cursor: "" or a repository id in the
// open arm (weekly false), or a weekly-arm cursor and the row it names; ok is
// false for a cursor no page answered.
func cutExpungeCursor(cursor string) (weekly bool, after *expungeAfter, ok bool) {
	rest, isWeekly := strings.CutPrefix(cursor, weeklyPrefix)
	if !isWeekly {
		return false, nil, cursor == "" || validRepoID("cursor", cursor) == nil
	}
	at, id, found := strings.Cut(rest, ":")
	n, err := strconv.ParseInt(at, 10, 64)
	if !found || err != nil || n < 0 || validRepoID("cursor", id) != nil {
		return true, nil, false
	}
	return true, &expungeAfter{at: n, id: id}, true
}

// indexed serves one page of an `indexed` read ([ReadIndexed]): the active
// and archived repositories searched by index, by name after the cursor, one
// seek of code_repos_search_idx (replicated 0107, its predicate repeated
// verbatim). A FULL LISTING, as [Reader.compactionDue]'s.
func (r *Reader) indexed(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	// THE CURSOR IS THE LAST NAME SERVED, and "" before the first: a name
	// is never empty, so one statement serves every page.
	if q.Cursor != "" && !ValidName(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, indexedSQL, string(SearchIndexed), q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories searched by index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var c IndexedRepo
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return fmt.Errorf("gitlet: list the repositories searched by index: %w", err)
		}
		out.Indexed = append(out.Indexed, c)
		if pg.add(2 + field("id", stringBytes(c.ID), true) + field("name", stringBytes(c.Name), false)) {
			out.Next = c.Name
			break
		}
	}
	return rows.Err()
}

// defaultTips serves one page of a `default_tips` read ([ReadDefaultTips]):
// the active project repositories by name after the cursor, each with its
// default branch's tip, one walk of code_repos_name_idx; a full listing.
func (r *Reader) defaultTips(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.Cursor != "" && !ValidName(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	query, args := defaultTipsSQL, []any{q.Cursor, string(RepoProject), r.pageRows}
	if q.DefaultTips != nil {
		query, args = defaultTipSQL, []any{q.DefaultTips.RepoID, string(RepoProject)}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories' default tips: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c    DefaultTip
			head sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Visibility, &head); err != nil {
			return fmt.Errorf("gitlet: list the repositories' default tips: %w", err)
		}
		c.Head = head.String
		out.DefaultTips = append(out.DefaultTips, c)
		if pg.add(2 + field("id", stringBytes(c.ID), true) + field("name", stringBytes(c.Name), false) +
			field("visibility", stringBytes(string(c.Visibility)), false) +
			field("head", stringBytes(c.Head), false)) {
			out.Next = c.Name
			break
		}
	}
	return rows.Err()
}

// importsDue serves one page of an `imports_due` read: the repositories being
// imported, BY ID, with their source, chunks landed and row version, one walk
// of code_repos_importing_idx (replicated 0123, its predicate repeated
// verbatim); a full listing.
func (r *Reader) importsDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.Cursor != "" && validRepoID("cursor", q.Cursor) != nil {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, importsDueSQL, q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories being imported: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c       ImportCandidate
			source  string
			version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &source, &c.Seq, &version); err != nil {
			return fmt.Errorf("gitlet: list the repositories being imported: %w", err)
		}
		// AN IMPORTING REPOSITORY NAMES ITS SOURCE, fixed at the create; one
		// naming none is listed with none, and its home ends it.
		if src, err := remoteOf[ImportSource](source); err != nil {
			return fmt.Errorf("gitlet: list the repositories being imported: %s: %w", c.ID, err)
		} else if src != nil {
			c.Source = *src
		}
		c.Version = unpacked(version)
		out.Imports = append(out.Imports, c)
		if pg.add(c.encodedBytes()) {
			out.Next = c.ID
			break
		}
	}
	return rows.Err()
}

// mirrorsDue serves one page of a `mirrors_due` read: every live push mirror
// BY ID (code_repos_mirror_to_idx), then every active pull mirror LEAST
// RECENTLY SYNCED FIRST (code_repos_mirrored_idx, replicated 0123), each
// predicate repeated verbatim; the cursor names its arm ([cutMirrorsCursor]),
// and a page ending the push arm goes on into the pull arm.
func (r *Reader) mirrorsDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	arm, after, at, ok := cutMirrorsCursor(q.Cursor)
	if !ok {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	if arm == "t" {
		full, err := r.pushMirrors(ctx, tx, after, &pg, out)
		if err != nil || full {
			return err
		}
		after, at = "", -1
	}
	rows, err := tx.QueryContext(ctx, pullMirrorsSQL, at, at, after, r.pageRows)
	if err != nil {
		return fmt.Errorf("gitlet: list the pull mirrors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c       MirrorCandidate
			from    string
			synced  int64
			version int64
		)
		if err = rows.Scan(&c.ID, &c.Name, &from, &synced, &version); err != nil {
			return fmt.Errorf("gitlet: list the pull mirrors: %w", err)
		}
		if c.From, err = remoteOf[MirrorSource](from); err != nil {
			return fmt.Errorf("gitlet: list the pull mirrors: %s: %w", c.ID, err)
		}
		// ZERO IS NEVER: the column's default, and no record's instant.
		if synced != 0 {
			c.SyncedAt = store.DecodeTime(synced)
		}
		c.Version = unpacked(version)
		out.Mirrors = append(out.Mirrors, c)
		if pg.add(c.encodedBytes()) {
			out.Next = fmt.Sprintf("f:%d:%s", synced, c.ID)
			break
		}
	}
	return rows.Err()
}

// pushMirrors serves the push mirrors' arm of a `mirrors_due` page after the
// id after, and reports a page it filled — whose cursor it set.
func (r *Reader) pushMirrors(ctx context.Context, tx *sql.Tx, after string, pg *page, out *ReadAnswer) (
	bool, error) {

	rows, err := tx.QueryContext(ctx, pushMirrorsSQL, after, r.pageRows)
	if err != nil {
		return false, fmt.Errorf("gitlet: list the push mirrors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c  MirrorCandidate
			to string
		)
		if err = rows.Scan(&c.ID, &c.Name, &to); err != nil {
			return false, fmt.Errorf("gitlet: list the push mirrors: %w", err)
		}
		if c.To, err = remoteOf[MirrorTarget](to); err != nil {
			return false, fmt.Errorf("gitlet: list the push mirrors: %s: %w", c.ID, err)
		}
		out.Mirrors = append(out.Mirrors, c)
		if pg.add(c.encodedBytes()) {
			out.Next = "t:" + c.ID
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// cutMirrorsCursor reads a `mirrors_due` cursor: "" is the push arm's first
// row; `t:<id>` the push arm after id; `f:<instant>:<id>` the pull arm after
// the mirror synced at instant (store.EncodeTime, 0 for never) whose id is
// id. ok is false for anything else.
func cutMirrorsCursor(cursor string) (arm, after string, at int64, ok bool) {
	switch {
	case cursor == "":
		return "t", "", 0, true
	case strings.HasPrefix(cursor, "t:"):
		id := strings.TrimPrefix(cursor, "t:")
		return "t", id, 0, validRepoID("cursor", id) == nil
	case strings.HasPrefix(cursor, "f:"):
		instant, id, cut := strings.Cut(strings.TrimPrefix(cursor, "f:"), ":")
		n, err := strconv.ParseInt(instant, 10, 64)
		if !cut || err != nil || n < 0 || validRepoID("cursor", id) != nil {
			return "", "", 0, false
		}
		return "f", id, n, true
	}
	return "", "", 0, false
}

// expungeTips serves one page of an `expunge_tips` read ([ReadExpungeTips]):
// the objects an expunge keeps beside the refs (M6-D2), in the phases
// [tipsPhase] lists, each ONE SEEK of its index, keyset after the last row
// read. Drops are those at or after the query's instant (code_ref_drops' key,
// led by the instant, replicated 0089). The rest are IN FLIGHT and kept even
// by a purge: revision heads of draft or open changes (an interdiff reads
// force-pushed heads), live queue entries, candidates (rows and a pack, never
// a ref, M5-D12) and green rows; closed history keeps nothing. Each object is
// answered once a page. A purged repository is [ErrRepoNotFound]. NEVER HELD
// TO A GRANT: the objects go only to the maintainer.
func (r *Reader) expungeTips(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	in := q.ExpungeTips
	var name string
	switch err := tx.QueryRowContext(ctx, repoNameSQL, in.RepoID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, in.RepoID)
	case err != nil:
		return fmt.Errorf("gitlet: read repository %s: %w", in.RepoID, err)
	}
	since := store.EncodeTime(in.Since)
	from := tipsCursor{phase: tipsDrops}
	if q.Cursor != "" {
		c, ok := cutTipsCursor(q.Cursor)
		if !ok || (c.phase == tipsDrops && c.at < since) {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		from = c
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	answered := map[string]bool{}
	phases := tipsPhases()
	for i := slices.Index(phases, from.phase); i < len(phases); i++ {
		phase := phases[i]
		var after *tipsCursor
		if phase == from.phase && q.Cursor != "" {
			after = &from
		}
		full, err := r.tipsOf(ctx, tx, phase, in.RepoID, since, after, &pg, answered, out)
		if err != nil {
			return fmt.Errorf("gitlet: read the objects %s's expunge keeps (%s): %w", name, phase, err)
		}
		if full {
			return nil
		}
	}
	return nil
}

// tipsOf reads one phase of an `expunge_tips` page into out, from its start or
// after the row after names, counting every row read against pg
// ([tipAnswered]). full reports a page that took its last row, its cursor
// that row's place.
func (r *Reader) tipsOf(ctx context.Context, tx *sql.Tx, phase tipsPhase, repoID string, since int64,
	after *tipsCursor, pg *page, answered map[string]bool, out *ReadAnswer) (full bool, err error) {

	if phase == tipsRevisions {
		return r.revisionTips(ctx, tx, repoID, after, pg, answered, out)
	}
	var rows *sql.Rows
	switch phase {
	case tipsDrops:
		if after == nil {
			rows, err = tx.QueryContext(ctx, expungeTipsSQL, repoID, since, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, expungeTipsAfterSQL, repoID, after.at, after.position, after.ref,
				after.old, r.pageRows)
		}
	case tipsQueue:
		if after == nil {
			rows, err = tx.QueryContext(ctx, queueTipsSQL, repoID, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, queueTipsAfterSQL, repoID, after.ref, after.position, r.pageRows)
		}
	case tipsCandidates:
		if after == nil {
			rows, err = tx.QueryContext(ctx, candidateTipsSQL, repoID, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, candidateTipsAfterSQL, repoID, after.ref, after.id, after.position,
				r.pageRows)
		}
	case tipsGreen:
		if after == nil {
			rows, err = tx.QueryContext(ctx, greenTipsSQL, repoID, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, greenTipsAfterSQL, repoID, after.ref, r.pageRows)
		}
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		c := tipsCursor{phase: phase}
		var oid string
		switch phase {
		case tipsDrops:
			err = rows.Scan(&c.at, &c.position, &c.ref, &c.old)
			oid = c.old
		case tipsQueue:
			err = rows.Scan(&c.ref, &c.position, &oid)
		case tipsCandidates:
			err = rows.Scan(&c.ref, &c.id, &c.position, &oid)
		case tipsGreen:
			err = rows.Scan(&c.ref, &oid)
		}
		if err != nil {
			return false, err
		}
		if tipAnswered(c, oid, pg, answered, out) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// revisionTips is the revisions phase of an `expunge_tips` page
// ([Reader.tipsOf]): live changes by source branch over code_changes_open_idx,
// then each one's revisions by key. TWO SEEKS, NOT A JOIN: the planner would
// sort every live revision by an inner column. A branch heads one live change
// (decideOpen's change_exists), but the cursor names the change too, since a
// new change may replace it between pages: only the cursor's own change
// resumes after its revision; any other starts from its first.
func (r *Reader) revisionTips(ctx context.Context, tx *sql.Tx, repoID string, after *tipsCursor, pg *page,
	answered map[string]bool, out *ReadAnswer) (full bool, err error) {

	q, args := liveChangeTipsSQL, []any{repoID, r.pageRows}
	if after != nil {
		q, args = liveChangeTipsFromSQL, []any{repoID, after.ref, r.pageRows}
	}
	for {
		batch, err := liveChangesOf(ctx, tx, q, args...)
		if err != nil {
			return false, err
		}
		for _, ch := range batch {
			from := int64(0)
			if after != nil && ch.id == after.id {
				from = after.position
			}
			if full, err := r.changeRevisionTips(ctx, tx, ch, from, pg, answered, out); full || err != nil {
				return full, err
			}
		}
		if len(batch) < r.pageRows {
			return false, nil
		}
		q, args = liveChangeTipsAfterSQL, []any{repoID, batch[len(batch)-1].ref, r.pageRows}
	}
}

// liveChangeTip is one live change a revisions phase reads: its id and its
// source branch.
type liveChangeTip struct{ id, ref string }

// liveChangesOf is the live changes q names, read whole and closed before
// their revisions are read, so no two statements are open at once on tx.
func liveChangesOf(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]liveChangeTip, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []liveChangeTip
	for rows.Next() {
		var c liveChangeTip
		if err := rows.Scan(&c.id, &c.ref); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// changeRevisionTips answers the heads of ch's revisions after revision from
// into out, in their number's order, as [Reader.tipsOf] answers a row.
func (r *Reader) changeRevisionTips(ctx context.Context, tx *sql.Tx, ch liveChangeTip, from int64, pg *page,
	answered map[string]bool, out *ReadAnswer) (full bool, err error) {

	rows, err := tx.QueryContext(ctx, changeRevisionTipsSQL, ch.id, from, r.pageRows)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		c := tipsCursor{phase: tipsRevisions, ref: ch.ref, id: ch.id}
		var oid string
		if err := rows.Scan(&c.position, &oid); err != nil {
			return false, err
		}
		if tipAnswered(c, oid, pg, answered, out) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// tipAnswered answers oid into out unless the page answered it already, and
// counts its row against pg either way (a page is held to what it reads),
// reporting a page that took its last row, its cursor c.
func tipAnswered(c tipsCursor, oid string, pg *page, answered map[string]bool, out *ReadAnswer) bool {
	size := 0
	if !answered[oid] {
		answered[oid] = true
		out.Tips = append(out.Tips, oid)
		size = stringBytes(oid) + 1
	}
	if pg.add(size) {
		out.Next = c.String()
		return true
	}
	return false
}

// tipsPhase is one phase of an `expunge_tips` read ([Reader.expungeTips]): the
// table whose rows name what an expunge keeps.
type tipsPhase string

const (
	// tipsDrops is the ref drops since the read's instant (code_ref_drops).
	tipsDrops tipsPhase = "drops"
	// tipsRevisions is every revision head of a live change (code_revisions).
	tipsRevisions tipsPhase = "revisions"
	// tipsQueue is every live queue entry's head (code_queue).
	tipsQueue tipsPhase = "queue"
	// tipsCandidates is every candidate's tip (code_candidates).
	tipsCandidates tipsPhase = "candidates"
	// tipsGreen is every target's green row (code_green).
	tipsGreen tipsPhase = "green"
)

// tipsPhases is the phases in the order a read serves them.
func tipsPhases() []tipsPhase {
	return []tipsPhase{tipsDrops, tipsRevisions, tipsQueue, tipsCandidates, tipsGreen}
}

// tipsCursor is one row's place in its phase's index order, and an
// `expunge_tips` page's cursor, "<phase>:" then: drops
// "<at>:<position>:<old>:<ref>", revisions "<n>:<change>:<source_ref>", queue
// "<seq>:<target_ref>", candidates "<ordinal>:<train>:<target_ref>", green
// "<target_ref>". The ref is LAST, since it may hold every character but a
// colon (git's ref rules). position is a drop's record position, a revision's
// number, an entry's seq or a candidate's ordinal; id a revision's change or
// a candidate's train.
type tipsCursor struct {
	phase        tipsPhase
	at, position int64
	ref, old     string
	id           string
}

// String is c as a cursor.
func (c tipsCursor) String() string {
	p := string(c.phase) + ":"
	switch c.phase {
	case tipsDrops:
		return p + strconv.FormatInt(c.at, 10) + ":" + strconv.FormatInt(c.position, 10) + ":" + c.old + ":" + c.ref
	case tipsRevisions, tipsCandidates:
		return p + strconv.FormatInt(c.position, 10) + ":" + c.id + ":" + c.ref
	case tipsGreen:
		return p + c.ref
	}
	return p + strconv.FormatInt(c.position, 10) + ":" + c.ref
}

// cutTipsCursor reads a [tipsCursor].
func cutTipsCursor(s string) (tipsCursor, bool) {
	phase, rest, ok := strings.Cut(s, ":")
	c := tipsCursor{phase: tipsPhase(phase)}
	if !ok || !slices.Contains(tipsPhases(), c.phase) {
		return tipsCursor{}, false
	}
	fields := map[tipsPhase]int{tipsDrops: 4, tipsRevisions: 3, tipsQueue: 2, tipsCandidates: 3, tipsGreen: 1}
	parts := strings.SplitN(rest, ":", fields[c.phase])
	if len(parts) != fields[c.phase] || slices.Contains(parts, "") {
		return tipsCursor{}, false
	}
	c.ref = parts[len(parts)-1]
	if c.phase == tipsGreen {
		return c, true
	}
	n := 0
	if c.phase == tipsDrops {
		at, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return tipsCursor{}, false
		}
		c.at, n = at, 1
		c.old = parts[2]
	}
	position, err := strconv.ParseInt(parts[n], 10, 64)
	if err != nil || position < 0 {
		return tipsCursor{}, false
	}
	c.position = position
	if c.phase == tipsRevisions || c.phase == tipsCandidates {
		c.id = parts[1]
	}
	return c, true
}

// publishCursor is "<published_at>/<id>", the instant as the column stores it:
// a repository's place in a `publication_due` listing's order.
func publishCursor(publishedAt int64, id string) string {
	return strconv.FormatInt(publishedAt, 10) + "/" + id
}

// cutPublishCursor reads a [publishCursor].
func cutPublishCursor(s string) (int64, string, bool) {
	at, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return 0, "", false
	}
	published, err := strconv.ParseInt(at, 10, 64)
	if err != nil || published < 0 {
		return 0, "", false
	}
	return published, id, true
}

// ResolveRef answers the refs of one repository a query names, at its level,
// to an asker and ceiling that may read it ([JudgeRead]), in ONE transaction:
// a deleted or purging repository is [ErrRepoNotFound] in [RepoGone]'s
// sentence. Each ref resolves whole, a short name as a branch then a tag, and
// [HeadRef] as the default branch this transaction's row names, with the
// position that last moved it ([ResolvedRef.Moved]). A name that resolves to
// nothing is answered [ResolvedRef.Missing] beside the others, never failing
// them: whether a call goes on is its caller's to say.
func (r *Reader) ResolveRef(ctx context.Context, q RefQuery) (RefAnswer, error) {
	if err := checkFreshness(q.Fresh); err != nil {
		return RefAnswer{}, err
	}
	if !ValidName(q.Repo) {
		return RefAnswer{}, refusedf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
	}
	if why := q.Asker.invalid(); why != "" {
		return RefAnswer{}, refusedf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return RefAnswer{}, fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	// EVERY NAME IS CHECKED BEFORE ANY READ: a query naming one no ref can
	// have is the asker's mistake, refused whole.
	switch n := len(q.Refs); {
	case n == 0:
		return RefAnswer{}, refusedf("%w: refs: a resolve names at least one ref", ErrInvalid)
	case n > MaxRefsPerQuery:
		return RefAnswer{}, refusedf("%w: refs: %d refs, past the %d one resolve reads", ErrInvalid, n,
			MaxRefsPerQuery)
	}
	tried := make([][]string, len(q.Refs))
	for i, ref := range q.Refs {
		if ref == HeadRef {
			continue // the default branch, which the row read below names
		}
		names, nameErr := refNames(ref)
		if nameErr != nil {
			return RefAnswer{}, nameErr
		}
		tried[i] = names
	}
	chart, chartErr := r.chartFor(q.Asker, false)
	if chartErr != nil {
		return RefAnswer{}, chartErr
	}
	var out RefAnswer
	query := q.Fresh.Resolved(resolver(q.Repo), false)
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		out = RefAnswer{}
		id, found, err := claimIn(ctx, tx, q.Repo)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		v, found, err := repoViewIn(ctx, tx, id, cp.At)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		if v.ref.Status == RepoStatusDeleted || v.ref.Status == RepoStatusPurging {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, RepoGone{ID: id, Name: v.ref.Name,
				Status: v.ref.Status, DeletedAt: v.ref.DeletedAt, ManagedBy: v.ref.ManagedBy}.Detail())
		}
		if _, err := JudgeRead(q.Asker, q.CeilingSeat, v.ref, chart); err != nil {
			return err
		}
		out = RefAnswer{Repo: v.ref, Refs: make([]ResolvedRef, len(q.Refs)), Position: cp.At}
		for i, names := range tried {
			said := ""
			if q.Refs[i] == HeadRef {
				names, said = []string{branchPrefix + v.ref.DefaultBranch}, ", its default branch"
			}
			resolved, err := firstLiveIn(ctx, tx, id, names)
			if err != nil {
				return fmt.Errorf("gitlet: read %s of %s: %w", q.Refs[i], q.Repo, err)
			}
			resolved.Name = q.Refs[i]
			if resolved.Ref == "" {
				resolved.Missing = fmt.Sprintf("repository %s holds no live %s%s", q.Repo, words.Or(names), said)
			}
			out.Refs[i] = resolved
		}
		return nil
	})
	if err != nil {
		return RefAnswer{}, err
	}
	return out, nil
}

// firstLiveIn is the first of names repository id holds as a live ref, read in
// tx — its whole name, the object it names and where it last moved — or a
// zero ResolvedRef where it holds none of them.
func firstLiveIn(ctx context.Context, tx *sql.Tx, id string, names []string) (ResolvedRef, error) {
	for _, name := range names {
		resolved, found, err := liveRefIn(ctx, tx, id, name)
		switch {
		case err != nil:
			return ResolvedRef{}, err
		case found:
			return resolved, nil
		}
	}
	return ResolvedRef{}, nil
}

// refNames is every ref one name of a [RefQuery] may be, in the order they
// are tried: a whole ref as itself; a short name as a branch, then as a tag —
// each only where git could hold it. A name that can be none is
// [ErrInvalid], refused before any read.
func refNames(ref string) ([]string, error) {
	if strings.HasPrefix(ref, "refs/") {
		if !ValidRef(ref) {
			return nil, refusedf("%w: ref: %q is not a ref", ErrInvalid, ref)
		}
		return []string{ref}, nil
	}
	var names []string
	if gitref.ValidBranch(ref) {
		names = append(names, branchPrefix+ref)
	}
	if ValidRef(tagPrefix + ref) {
		names = append(names, tagPrefix+ref)
	}
	if len(names) == 0 {
		return nil, refusedf("%w: ref: %q is no ref, branch or tag name", ErrInvalid, ref)
	}
	return names, nil
}

// liveRefIn is repository id's live ref by its whole name, read in tx: the
// object, the position and broker instant of the record that last moved it
// (apply_push.go writes them), and the seat that set it in its own namespace
// ([seatOf]), in one key lookup; or false where no live ref has the name.
func liveRefIn(ctx context.Context, tx *sql.Tx, id, ref string) (ResolvedRef, bool, error) {
	var (
		out       = ResolvedRef{Ref: ref}
		moved, at int64
	)
	switch err := tx.QueryRowContext(ctx, liveRefSQL, id, ref).Scan(&out.OID, &moved, &at, &out.Seat); {
	case errors.Is(err, sql.ErrNoRows):
		return ResolvedRef{}, false, nil
	case err != nil:
		return ResolvedRef{}, false, err
	}
	out.Moved, out.At = unpacked(moved), store.DecodeTime(at)
	return out, true, nil
}

// mayReadIn refuses an asker that may not read repository id — or the seat it
// reads for, ceilingSeat, that may not ([JudgeRead]) — judged from what a
// grant reads of its row in tx ([grantRefIn]).
func mayReadIn(ctx context.Context, tx *sql.Tx, id string, asker Principal, ceilingSeat string,
	chart Roster) error {
	ref, err := grantRefIn(ctx, tx, id)
	if err != nil {
		return err
	}
	_, err = JudgeRead(asker, ceilingSeat, ref, chart)
	return err
}

// grantRefIn is what a grant reads of repository id's row in tx — its name, its
// owner and its visibility — and its status beside them, by one primary-key
// read: [ErrRepoNotFound] where no row is there.
func grantRefIn(ctx context.Context, tx *sql.Tx, id string) (RepoRef, error) {
	var (
		ref                        = RepoRef{ID: id}
		status, owners, visibility string
	)
	err := tx.QueryRowContext(ctx, repoGrantSQL, id).Scan(&ref.Name, &status, &owners, &ref.Owner.Ref,
		&visibility)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RepoRef{}, fmt.Errorf("%w: %s", ErrRepoNotFound, id)
	case err != nil:
		return RepoRef{}, fmt.Errorf("gitlet: read repository %s's grant: %w", id, err)
	}
	ref.Status, ref.Owner.Kind, ref.Visibility = RepoStatus(status), OwnerKind(owners), Visibility(visibility)
	return ref, nil
}

// logCursor is "<packed position>/<id>": a push's or a change's place in log
// order, by its record's position and its id, since one record holds up to 64
// pushes, each opening a change of its own.
func logCursor(packed int64, id string) string {
	return strconv.FormatInt(packed, 10) + "/" + id
}

// cutLogCursor reads a [logCursor].
func cutLogCursor(s string) (int64, string, bool) {
	at, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return 0, "", false
	}
	position, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return position, id, true
}

// RepoLife is one repository as a reconcile of code.repos reads it: its id and
// its name, whether it is live, deleted or purging, and who manages it — and
// nothing else.
type RepoLife struct {
	ID        string
	Name      string
	Status    RepoStatus
	ManagedBy ManagedBy
}

// lifePage is how many repositories one page of [Reader.WalkLives] reads:
// a thousand rows of four short columns, about 150 KB, so the ten thousand a
// document may declare are ten statements, each holding a reader-pool
// connection for milliseconds rather than the whole walk.
const lifePage = 1_000

// WalkLives hands visit every repository a reconcile of code.repos acts on
// (importing, active, archived or deleted; a purge is the code-retention
// duty's) in name order, a page of [lifePage] at a time, each one STALE read.
// A visit's error ends the walk.
//
// ITS OWN READ: [Reader.Read]'s listing joins the head, the rules and the
// counters a reconcile never uses, at hundreds of ms and MB over a full
// document. STALE IS ENOUGH: every decision is taken again in the reconcile's
// own snapshot after the activation's fence.
func (r *Reader) WalkLives(ctx context.Context, visit func(RepoLife) error) error {
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	page := make([]RepoLife, 0, lifePage)
	after := ""
	for {
		_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
			page = page[:0]
			rows, err := tx.QueryContext(ctx, livesSQL, after, lifePage)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var (
					l               RepoLife
					status, managed string
				)
				if err := rows.Scan(&l.ID, &l.Name, &status, &managed); err != nil {
					return err
				}
				l.Status, l.ManagedBy = RepoStatus(status), ManagedBy(managed)
				page = append(page, l)
			}
			return rows.Err()
		})
		if err != nil {
			return fmt.Errorf("gitlet: list the repositories a reconcile acts on: %w", err)
		}
		for _, l := range page {
			if err := visit(l); err != nil {
				return err
			}
		}
		if len(page) < lifePage {
			return nil
		}
		after = page[len(page)-1].Name
	}
}

// dueRepo is one repository the code-retention pass steps: its id, its name
// to report, and its place in the listing's order.
type dueRepo struct {
	id, name string

	// purging says its purge has begun, so it is listed whatever the
	// retention.
	purging bool

	// deletedAt is its deletion's instant as stored: the listing's order,
	// with the id.
	deletedAt int64
}

// dueQuery asks for one page of what the code-retention pass steps.
type dueQuery struct {
	// cutoff is now less the retention: a DELETED repository is listed only
	// once its deletion is at or before it.
	cutoff time.Time

	// after is the last repository of the page before; nil for the first.
	after *dueRepo

	// limit is the most repositories the page lists.
	limit int
}

// due lists one page of what the code-retention pass steps, in its order:
// every PURGING repository first, whatever the retention (a begun purge is
// always finished: nothing restores partly gone rows), then every DELETED one
// at or before the cutoff; each phase oldest deletion first, resuming after
// q.after.
//
// A STALE READ of the three columns the pass uses, since every step decides
// again in its own snapshot; each phase ONE statement over its own partial
// index (replicated 0080), so a page reads only the rows it lists.
func (r *Reader) due(ctx context.Context, q dueQuery) ([]dueRepo, error) {
	if q.limit <= 0 {
		return nil, fmt.Errorf("%w: a page of the repositories due lists at least one", ErrInvalid)
	}
	var out []dueRepo
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		out = out[:0]
		var (
			after   = int64(math.MinInt64)
			afterID string
			purging = true
		)
		if q.after != nil {
			after, afterID, purging = q.after.deletedAt, q.after.id, q.after.purging
		}
		if purging {
			var err error
			if out, err = dueIn(ctx, tx, out, true, duePurgingSQL, after, after, afterID,
				q.limit); err != nil {
				return fmt.Errorf("gitlet: read the repositories purging: %w", err)
			}
			if len(out) == q.limit {
				return nil
			}
			after, afterID = math.MinInt64, ""
		}
		var err error
		if out, err = dueIn(ctx, tx, out, false, dueDeletedSQL, store.EncodeTime(q.cutoff),
			after, after, afterID, q.limit-len(out)); err != nil {
			return fmt.Errorf("gitlet: read the repositories deleted past the retention: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// dueIn appends to out every repository query lists, each marked purging or
// not.
func dueIn(ctx context.Context, tx *sql.Tx, out []dueRepo, purging bool, query string,
	args ...any) ([]dueRepo, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		d := dueRepo{purging: purging}
		if err := rows.Scan(&d.id, &d.name, &d.deletedAt); err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// purgedAt reports whether this node's rows hold repository id PURGED (row
// gone, deletion marker written), read at session floored at the purge's last
// record, so a node behind it refuses `behind` rather than answering early.
// False is definite: the record there removed nothing here.
func (r *Reader) purgedAt(ctx context.Context, id string, at statelog.Position) (bool, error) {
	if at.IsZero() {
		return false, refusedf("%w: at: a purge is confirmed at its last record's position", ErrInvalid)
	}
	var purged bool
	query := statelog.FloorAt(statelog.ReadSession, at).Query(
		statelog.ScopeSet{Paths: []string{RepoPath(id)}}, false)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		var rows, marked int
		if err := tx.QueryRowContext(ctx, purgeStateSQL, id, id).Scan(&rows, &marked); err != nil {
			return fmt.Errorf("gitlet: read whether %s is purged: %w", id, err)
		}
		purged = rows == 0 && marked > 0
		return nil
	})
	return purged, err
}

// The reader's statements, each registered in plan_test.go so the pinned
// driver's planner is held to the index each is written for.
const (
	repoViewSQL = `SELECT id, name, status, managed_by, kind, owner_kind, owner, visibility, search,
		mirror_from != '', default_branch, description, deleted_at, last_activity_at, refs, branches, tags,
		tombstones, live_packs,
		live_bytes, live_objects, drop_epoch, tombstones_through, retired_through, layers_position, pushes,
		last_push_at, created_at FROM code_repos WHERE id = ?`
	packColumns       = `pack_id, part, object, hash, size, index_bytes, reverse_bytes, git_checksum, objects, kind, state, added_position, content_position, arrived_at`
	packsFullFirstSQL = `SELECT ` + packColumns + ` FROM code_packs WHERE repo_id = ?
		ORDER BY pack_id, part LIMIT ?`
	packsFullAfterSQL = `SELECT ` + packColumns + ` FROM code_packs INDEXED BY ` + packsKey + `
		WHERE repo_id = ? AND pack_id >= ? AND (pack_id > ? OR part > ?)
		ORDER BY pack_id, part LIMIT ?`
	packsAddedFirstSQL = `SELECT ` + packColumns + ` FROM code_packs WHERE repo_id = ?
		AND added_position > ? ORDER BY added_position, pack_id, part LIMIT ?`
	packsAddedAfterSQL = `SELECT ` + packColumns + ` FROM code_packs WHERE repo_id = ?
		AND added_position >= ? AND (added_position > ? OR pack_id > ? OR (pack_id = ? AND part > ?))
		ORDER BY added_position, pack_id, part LIMIT ?`
	refsFullFirstSQL = `SELECT ref, target, position FROM code_refs WHERE repo_id = ? AND target != ''
		ORDER BY ref LIMIT ?`
	refsFullAfterSQL = `SELECT ref, target, position FROM code_refs INDEXED BY ` + refsKey + `
		WHERE repo_id = ? AND target != '' AND ref > ? ORDER BY ref LIMIT ?`
	refsDeltaFirstSQL = `SELECT ref, target, position FROM code_refs WHERE repo_id = ? AND position > ?
		ORDER BY position, ref LIMIT ?`
	refsDeltaAfterSQL = `SELECT ref, target, position FROM code_refs WHERE repo_id = ?
		AND position >= ? AND (position > ? OR ref > ?) ORDER BY position, ref LIMIT ?`
	// retiredAboveSQL is a BOUNDED COUNT of a repository's retirements above
	// ?2, at most ?3 deep over the retired phase's index ([choose]).
	retiredAboveSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ?1 AND retired_position > ?2 LIMIT ?3)`
	// deltaRowsSQL is a BOUNDED COUNT of the refs a delta from ?2 moves and
	// the packs it adds, each table counted at most ?3 deep over the index
	// the delta's pages read (deltaOutweighs).
	deltaRowsSQL = `SELECT
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_refs WHERE repo_id = ?1 AND position > ?2 LIMIT ?3))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_packs WHERE repo_id = ?1 AND added_position > ?2 LIMIT ?3))`
	// THE RETIRED PHASE, over code_packs_retired_repo_idx in its order —
	// those in (?2, ?3] first, and after a cursor (?3 the upper bound, ?4 to
	// ?7 the cursor) with the >= beside the OR that keeps the range seek.
	retiredColumns  = `pack_id, part, git_checksum, retired_at, retired_position`
	retiredFirstSQL = `SELECT ` + retiredColumns + ` FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ? AND retired_position > ?
		AND retired_position <= ? ORDER BY retired_position, pack_id, part LIMIT ?`
	retiredAfterSQL = `SELECT ` + retiredColumns + ` FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ? AND retired_position >= ?
		AND retired_position <= ? AND (retired_position > ? OR pack_id > ? OR (pack_id = ? AND part > ?))
		ORDER BY retired_position, pack_id, part LIMIT ?`
	pushColumns = `push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
		work_backend, work_key, model, run_id, harness, node, atomic, commands, pack_id, pack_bytes,
		introduced, introduced_count, offered, received_at`
	pushSQL        = `SELECT ` + pushColumns + ` FROM code_pushes WHERE push_id = ?`
	pushesFirstSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		ORDER BY position DESC, push_id DESC LIMIT ?`
	pushesAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position <= ? AND (position < ? OR push_id < ?)
		ORDER BY position DESC, push_id DESC LIMIT ?`
	reposColumns = `SELECT r.id, r.name, r.status, r.managed_by, r.kind, r.owner_kind, r.owner, r.visibility,
		r.search, r.mirror_from != '', r.default_branch, r.description, r.deleted_at, r.last_activity_at, r.refs,
		r.live_packs,
		r.live_bytes, r.live_objects, r.drop_epoch, r.pushes, r.last_push_at, h.target`
	reposHead = ` LEFT JOIN code_refs h INDEXED BY ` + refsKey + `
			ON h.repo_id = r.id AND h.ref = 'refs/heads/' || r.default_branch AND h.target != ''`
	// reposFrom is a listing's rows, by name, and protectedReposFrom those a
	// rule protects ([ReadQuery.Protected]), walked by [protectedReposKey],
	// NAMED since code_repos_name_idx serves the same order, its predicate
	// repeated verbatim. Each ends at the name range the statements below spell.
	reposFrom = reposColumns + `
		FROM code_repos r` + reposHead + `
		WHERE r.status IN (SELECT value FROM json_each(?)) AND `
	protectedReposFrom = reposColumns + `
		FROM code_repos r INDEXED BY ` + protectedReposKey + reposHead + `
		WHERE r.` + protectedRepos + ` AND r.status IN (SELECT value FROM json_each(?)) AND `
	// reposSQL is a page of the whole listing, past its cursor, and
	// protectedReposSQL a page of the protected ones.
	reposSQL          = reposFrom + `r.name > ? ORDER BY r.name LIMIT ?`
	protectedReposSQL = protectedReposFrom + `r.name > ? ORDER BY r.name LIMIT ?`
	// THE NAMES A PREFIX BEGINS ([ReadQuery.Prefix]), below [prefixEnd]: FROM
	// the prefix on a first page (a name may be it whole), PAST the cursor
	// after. Two statements, since with both lower bounds the planner may seek
	// by the empty cursor and walk every name below the prefix.
	reposUnderFirstSQL          = reposFrom + `r.name >= ? AND r.name < ? ORDER BY r.name LIMIT ?`
	reposUnderAfterSQL          = reposFrom + `r.name > ? AND r.name < ? ORDER BY r.name LIMIT ?`
	protectedReposUnderFirstSQL = protectedReposFrom + `r.name >= ? AND r.name < ? ORDER BY r.name LIMIT ?`
	protectedReposUnderAfterSQL = protectedReposFrom + `r.name > ? AND r.name < ? ORDER BY r.name LIMIT ?`
	// pushRulesOfSQL is a listing page's rules as a push is judged by them,
	// in one statement ([protectionsOf]) — pushRulesSQL for every repository
	// the page keeps, never selecting the gate.
	pushRulesOfSQL = `SELECT repo_id, pattern, allow_force, allow_delete, direct_push
		FROM code_protections WHERE repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
		ORDER BY repo_id, pattern`
	// repoGrantSQL is what a grant reads of one repository's row, and its
	// status ([grantRefIn]).
	repoGrantSQL = `SELECT name, status, owner_kind, owner, visibility FROM code_repos WHERE id = ?`
	// liveRefSQL is one live ref's target, the position it last moved at, that
	// move's instant and the seat that set it, by its whole name: a
	// repository's head, and a ref a RefQuery names (liveRefIn).
	liveRefSQL = `SELECT target, position, at, seat FROM code_refs INDEXED BY ` + refsKey + `
		WHERE repo_id = ? AND ref = ? AND target != ''`
	// refsUnderSQL is a page of the live refs between two names, by name —
	// over the live refs' partial index, its predicate verbatim
	// (refsListing).
	refsUnderSQL = `SELECT ref, target, position, at FROM code_refs INDEXED BY ` + liveRefsKey + `
		WHERE repo_id = ? AND ref > ? AND ref < ? AND target != '' ORDER BY ref LIMIT ?`
	// THE PUSHES IN LOG ORDER over code_pushes_repo_idx (replicated 0084):
	// (?2, ?3] oldest first, first and after a cursor (?4, ?5), and below ?2
	// newest first, paged by the history's own pushesAfterSQL.
	pushesAboveSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position > ? AND position <= ? ORDER BY position, push_id LIMIT ?`
	pushesAboveAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position >= ? AND position <= ? AND (position > ? OR push_id > ?)
		ORDER BY position, push_id LIMIT ?`
	pushesBelowSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position < ? ORDER BY position DESC, push_id DESC LIMIT ?`
	// AN EXECUTOR'S PUSHES TO ONE REPOSITORY over code_pushes_repo_idx, after
	// a PLACE (?2 position, ?3 push id; the >= beside the OR keeps the range
	// seek). The window is weighed by covering seeks: the next ?4 pushes'
	// count and weight (runWeighSQL), each weight (runWeightsSQL), the last
	// one's place (runLastSQL). Then the RUN'S OWN in that window
	// (runPushesInSQL, ?5/?6 its last push inclusive), each row looked up to
	// test the run, which the index does not carry.
	runWeighSQL = `SELECT COUNT(*), COALESCE(SUM(row_bytes), 0) FROM (SELECT row_bytes FROM code_pushes
		WHERE repo_id = ?1 AND position >= ?2 AND (position > ?2 OR push_id > ?3)
		ORDER BY position, push_id LIMIT ?4)`
	runWeightsSQL = `SELECT row_bytes FROM code_pushes WHERE repo_id = ?1 AND position >= ?2
		AND (position > ?2 OR push_id > ?3) ORDER BY position, push_id LIMIT ?4`
	runLastSQL = `SELECT position, push_id FROM code_pushes WHERE repo_id = ?1 AND position >= ?2
		AND (position > ?2 OR push_id > ?3) ORDER BY position, push_id LIMIT 1 OFFSET ?4`
	runPushesInSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?1 AND run_id = ?2
		AND position >= ?3 AND (position > ?3 OR push_id > ?4) AND position <= ?5
		AND (position < ?5 OR push_id <= ?6) ORDER BY position, push_id`
	// THE RETENTION PASS'S TWO PHASES, each on its own partial index, the
	// predicate repeated verbatim, keyset on (deleted_at, id): the >= beside
	// the OR keeps the range seek a bare OR gives up.
	livesSQL = `SELECT id, name, status, managed_by FROM code_repos
		WHERE name > ? AND status IN ('importing', 'active', 'archived', 'deleted')
		ORDER BY name LIMIT ?`
	duePurgingSQL = `SELECT id, name, deleted_at FROM code_repos
		WHERE status = 'purging' AND deleted_at >= ? AND (deleted_at > ? OR id > ?)
		ORDER BY deleted_at, id LIMIT ?`
	dueDeletedSQL = `SELECT id, name, deleted_at FROM code_repos
		WHERE status = 'deleted' AND deleted_at <= ? AND deleted_at >= ? AND (deleted_at > ? OR id > ?)
		ORDER BY deleted_at, id LIMIT ?`
	purgeStateSQL = `SELECT
		(SELECT COUNT(*) FROM code_repos WHERE id = ?),
		(SELECT COUNT(*) FROM code_deletions WHERE repo_id = ?)`
	// THE COMPACTOR'S DUE LISTING over code_repos_packs_idx from its top, its
	// predicate repeated verbatim, the <= beside the OR keeping the range seek
	// (planner.go).
	compactionDueFirstSQL = `SELECT id, name, live_packs, live_objects, version FROM code_repos
		INDEXED BY code_repos_packs_idx
		WHERE status IN ('active', 'archived') AND live_packs >= ?
		ORDER BY live_packs DESC, id DESC LIMIT ?`
	compactionDueAfterSQL = `SELECT id, name, live_packs, live_objects, version FROM code_repos
		INDEXED BY code_repos_packs_idx
		WHERE status IN ('active', 'archived') AND live_packs >= ? AND live_packs <= ?
			AND (live_packs < ? OR id < ?)
		ORDER BY live_packs DESC, id DESC LIMIT ?`
	// THE PUBLISHER'S DUE LISTING over code_repos_unpublished_idx, its
	// predicate repeated verbatim, from the column's floor 0 (never published)
	// and after a cursor (planner.go). expunge_id = '' is a residual predicate
	// ([Reader.publicationDue]).
	publicationDueColumns = `id, name, live_packs, published_packs, live_objects, published_objects,
		last_push_at, published_at, publish_took_ms, version`
	publicationDueFirstSQL = `SELECT ` + publicationDueColumns + ` FROM code_repos
		INDEXED BY code_repos_unpublished_idx
		WHERE live_packs > published_packs AND status IN ('active', 'archived') AND expunge_id = ''
			AND published_at >= 0
		ORDER BY published_at, id LIMIT ?`
	publicationDueAfterSQL = `SELECT ` + publicationDueColumns + ` FROM code_repos
		INDEXED BY code_repos_unpublished_idx
		WHERE live_packs > published_packs AND status IN ('active', 'archived') AND expunge_id = ''
			AND published_at >= ? AND (published_at > ? OR id > ?)
		ORDER BY published_at, id LIMIT ?`
	// THE EXPUNGES OWED, by id over code_repos_expunging_idx — its partial
	// predicate repeated verbatim — after the cursor, "" from its start.
	expungeDueSQL = `SELECT ` + expungeDueColumns + ` FROM code_repos INDEXED BY code_repos_expunging_idx
		WHERE (expunge_id != '' OR expunge_request != '') AND status IN ('active', 'archived') AND id > ?
		ORDER BY id LIMIT ?`
	// THE EXPUNGES THE WEEKLY ARM ASKS ABOUT over code_repos_expunge_due_idx,
	// its predicate repeated verbatim, up to the instant; the open arm's are
	// left out.
	expungeWeeklySQL = `SELECT ` + expungeDueColumns + ` FROM code_repos INDEXED BY code_repos_expunge_due_idx
		WHERE status IN ('active', 'archived') AND expunge_scanned_at <= ? AND expunge_id = ''
			AND expunge_request = ''
		ORDER BY expunge_scanned_at, id LIMIT ?`
	expungeWeeklyAfterSQL = `SELECT ` + expungeDueColumns + ` FROM code_repos
		INDEXED BY code_repos_expunge_due_idx
		WHERE status IN ('active', 'archived') AND expunge_scanned_at <= ? AND expunge_scanned_at >= ?
			AND (expunge_scanned_at > ? OR id > ?) AND expunge_id = '' AND expunge_request = ''
		ORDER BY expunge_scanned_at, id LIMIT ?`
	expungeDueColumns = `id, name, expunge_id, expunge_request, expunge_request_position, expunged_at,
		expunge_scanned_at, live_packs, live_bytes, live_objects, version`
	// THE REPOSITORIES SEARCHED BY INDEX, by name over code_repos_search_idx
	// — its partial predicate repeated verbatim — after the cursor, "" from
	// its start.
	indexedSQL = `SELECT id, name FROM code_repos INDEXED BY code_repos_search_idx
		WHERE search = ? AND status IN ('active', 'archived') AND name > ? ORDER BY name LIMIT ?`
	// THE ACTIVE PROJECT REPOSITORIES' DEFAULT TIPS, by name over
	// code_repos_name_idx, the tip joined as a listing's is (reposHead).
	defaultTipsSQL = `SELECT r.id, r.name, r.visibility, h.target FROM code_repos r INDEXED BY code_repos_name_idx` +
		reposHead + ` WHERE r.name > ? AND r.status = 'active' AND r.kind = ? ORDER BY r.name LIMIT ?`
	// AND ONE OF THEM, by its id: no row for a repository that is not one.
	defaultTipSQL = `SELECT r.id, r.name, r.visibility, h.target FROM code_repos r` + reposHead + `
		WHERE r.id = ? AND r.status = 'active' AND r.kind = ?`
	// THE IMPORTS OWED, by id over code_repos_importing_idx — its partial
	// predicate repeated verbatim — after the cursor, "" from its start.
	importsDueSQL = `SELECT id, name, import_source, import_seq, version FROM code_repos
		INDEXED BY code_repos_importing_idx WHERE status = 'importing' AND id > ? ORDER BY id LIMIT ?`
	// THE PUSH MIRRORS, by id over code_repos_mirror_to_idx, live ones only:
	// an importing one has nothing to push, a deleted one is going.
	pushMirrorsSQL = `SELECT id, name, mirror_to FROM code_repos INDEXED BY code_repos_mirror_to_idx
		WHERE mirror_to != '' AND status IN ('active', 'archived') AND id > ? ORDER BY id LIMIT ?`
	// THE PULL MIRRORS, least recently synced first over
	// code_repos_mirrored_idx, keyset after (instant, id): the instant seeks
	// the index, and the id breaks a tie inside one instant.
	pullMirrorsSQL = `SELECT id, name, mirror_from, mirror_synced_at, version FROM code_repos
		INDEXED BY code_repos_mirrored_idx WHERE mirror_from != '' AND status = 'active'
			AND mirror_synced_at >= ? AND (mirror_synced_at > ? OR id > ?)
		ORDER BY mirror_synced_at, id LIMIT ?`
	// AN EXPUNGE'S TIPS: a repository's ref drops from an instant, in the
	// order of code_ref_drops' key, which leads with the instant — the first
	// page from the instant, a later one after the last drop read.
	expungeTipsSQL = `SELECT at, position, ref, old FROM code_ref_drops INDEXED BY ` + dropsKey + `
		WHERE repo_id = ? AND at >= ? ORDER BY at, position, ref, old LIMIT ?`
	expungeTipsAfterSQL = `SELECT at, position, ref, old FROM code_ref_drops INDEXED BY ` + dropsKey + `
		WHERE repo_id = ?1 AND at >= ?2
		AND (at > ?2 OR position > ?3 OR (position = ?3 AND (ref > ?4 OR (ref = ?4 AND old > ?5))))
		ORDER BY at, position, ref, old LIMIT ?6`
	// AN EXPUNGE'S IN-FLIGHT TIPS (M6-D2), each phase from its start and after
	// a place, in its index's order ([Reader.expungeTips]); a live change's
	// revisions are TWO seeks, never a join ([Reader.revisionTips]).
	liveChangeTipsSQL = `SELECT id, source_ref FROM code_changes INDEXED BY code_changes_open_idx
		WHERE repo_id = ?1 AND ` + liveChanges + ` ORDER BY source_ref LIMIT ?2`
	liveChangeTipsFromSQL = `SELECT id, source_ref FROM code_changes INDEXED BY code_changes_open_idx
		WHERE repo_id = ?1 AND source_ref >= ?2 AND ` + liveChanges + ` ORDER BY source_ref LIMIT ?3`
	liveChangeTipsAfterSQL = `SELECT id, source_ref FROM code_changes INDEXED BY code_changes_open_idx
		WHERE repo_id = ?1 AND source_ref > ?2 AND ` + liveChanges + ` ORDER BY source_ref LIMIT ?3`
	changeRevisionTipsSQL = `SELECT n, head_oid FROM code_revisions WHERE change_id = ?1 AND n > ?2
		ORDER BY n LIMIT ?3`
	queueTipsSQL = `SELECT target_ref, seq, head_oid FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ?1 AND state IN ('waiting', 'testing') ORDER BY target_ref, seq LIMIT ?2`
	queueTipsAfterSQL = `SELECT target_ref, seq, head_oid FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ?1 AND state IN ('waiting', 'testing') AND target_ref >= ?2
			AND (target_ref > ?2 OR seq > ?3)
		ORDER BY target_ref, seq LIMIT ?4`
	candidateTipsSQL = `SELECT target_ref, train, ordinal, tip_oid
		FROM code_candidates INDEXED BY code_candidates_train_idx
		WHERE repo_id = ?1 ORDER BY target_ref, train, ordinal LIMIT ?2`
	candidateTipsAfterSQL = `SELECT target_ref, train, ordinal, tip_oid
		FROM code_candidates INDEXED BY code_candidates_train_idx
		WHERE repo_id = ?1 AND target_ref >= ?2
			AND (target_ref > ?2 OR train > ?3 OR (train = ?3 AND ordinal > ?4))
		ORDER BY target_ref, train, ordinal LIMIT ?5`
	greenTipsSQL = `SELECT target_ref, commit_oid FROM code_green WHERE repo_id = ?1
		ORDER BY target_ref LIMIT ?2`
	greenTipsAfterSQL = `SELECT target_ref, commit_oid FROM code_green WHERE repo_id = ?1 AND target_ref > ?2
		ORDER BY target_ref LIMIT ?3`
	// A SNAPSHOT'S LAYERS PHASE: every part of a repository's chain, in its
	// key's order, joined to its layer (layersOfSQL) in Go.
	layerPartsOfSQL = `SELECT layer_id, part, object, hash, size FROM code_layer_parts
		WHERE repo_id = ? ORDER BY layer_id, part`
)
