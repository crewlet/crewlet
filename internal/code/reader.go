package code

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about the code host from this node's own applied
// rows.
//
// EVERY READ GOES THROUGH [statelog.Reader], which is what makes a level a
// property of the answer: the refusal ladder (an evicted node, one below the
// trim floor, one whose applier has stopped serves nothing), the coverage
// probe over what the read is ABOUT, then the freshness target — and only then
// the rows, in ONE transaction.
//
// AND THE POSITION IS READ IN THAT TRANSACTION. The framework's answer names
// the checkpoint it waited for, read before the transaction opened; the
// applier can commit in between, so rows can be NEWER than that position. A
// snapshot's delta floor, its full-or-delta decision and its stream identity
// are claims about the rows, so each read here reads the code log's checkpoint
// row beside its rows and reports THAT ([statelog.CheckpointOf] over the read's
// own transaction) — read separately, one of them ends up describing another
// checkpoint.
type Reader struct {
	log    *statelog.Reader
	limits func() Limits
	roster func() Roster

	// pageRows and pageBytes bound one page: MaxSnapshotItems and
	// MaxPageBytes, and a test's smaller values (export_test.go).
	pageRows  int
	pageBytes int
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
	// ([GrantOf]). Nil — or a roster that answers nil, a node with no epoch
	// installed yet — refuses every agent's read [ErrNotServing], a refusal
	// about THIS reader, so the estate asks a node that holds a chart: never
	// "every seat reads", and never "this seat reads nothing" either, which
	// only a chart may answer. A person's read asks nothing of it, since no
	// chart decides a person's standing.
	Roster func() Roster
}

// NewReader builds the code host's read side.
func NewReader(o ReaderOptions) (*Reader, error) {
	if o.Log == nil {
		return nil, errors.New("code: a reader needs the code log's read authority — " +
			"without it every read level is a label rather than a guarantee")
	}
	r := &Reader{log: o.Log, limits: o.Limits, roster: o.Roster, pageRows: MaxSnapshotItems,
		pageBytes: MaxPageBytes}
	if r.limits == nil {
		r.limits = DefaultLimits
	}
	return r, nil
}

// chartFor is the roster a read by asker is judged by: the serving node's
// current chart for a seat's read and for every read that judges a change's
// readiness (readiness: [ReadQuery.judgesReadiness]), read ONCE a read so
// every row of a listing is judged by one chart, and none for anything else.
//
// A SEAT ASKING A READER THAT HOLDS NO CHART IS REFUSED ABOUT THE READER —
// [ErrNotServing], before any row is read — and never judged. A node holds no
// chart between the start of its code reader and the install of its first
// epoch: at every boot, and at a first apply whose reconcile of code.repos
// runs before the epoch is installed. Judged there, every seat stood nowhere
// ([GrantOf] with no roster): a definite "not permitted", or a listing with
// every repository left out — an ANSWER, which ends the estate's walk with
// what every peer holding a chart would contradict. Refused about itself,
// the reader is a node that ran nothing (estate's ranNothing), and the walk
// asks the next data node. A chart that DENIES the seat is still the answer.
// A READ OF CHANGES IS REFUSED SO WHOEVER ASKS, for the same reason: judged
// with no chart, no seat's approval would count and no seat would maintain
// the repository, a readiness every peer holding a chart would contradict.
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
	return q.Kind == ReadChanges || q.Kind == ReadChange && q.Cursor == "" && q.Part.detailed()
}

// inTx is one open transaction as a statelog.ReadEstate, so the checkpoint is
// read with the rows.
type inTx struct{ tx *sql.Tx }

func (t inTx) Read(_ context.Context, fn func(*sql.Tx) error) error { return fn(t.tx) }

// checkpointIn is the code log's checkpoint in tx: the position the rows read
// in tx were applied through, and the stream they are keyed to.
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

// checkFreshness is EVERY code read's one rule for the freshness it is asked
// at, a point read's and a listing's alike: any level, refusing an EMPTY one —
// a code read states its freshness, because a cache's sync and a git request
// each need a specific one, and [ReadQuery.freshness] states the REST read
// path's for a principal's read that named none — and a session read naming no
// position on the code log, which would wait for nothing. A floor on ANOTHER
// log is the request's mistake, refused first and as the framework's reader
// refuses it ([statelog.ErrForeignPosition]), so a session read naming only
// such a floor is told which floor is misplaced rather than that it named none.
func checkFreshness(f statelog.Freshness) error {
	switch {
	case f.Level == "":
		return fmt.Errorf("%w: a code read states its freshness", ErrBadLevel)
	case !f.Level.Valid():
		return fmt.Errorf("%w: %q is not a read level", ErrBadLevel, f.Level)
	}
	if err := f.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("code: this read's floor: %w", err)
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
			return statelog.ScopeSet{}, fmt.Errorf("code: resolve %q: %w", name, err)
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
		return "", false, fmt.Errorf("code: read the claim on %q: %w", name, err)
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
// ([pushRulesIn]: no gate) in tx, at the checkpoint cp — the row every git
// request's admission ([Reader.Resolve]), every page of a git node's sync
// ([Reader.Snapshot]), every resolution of a ref and of a repository's detail,
// and the lifecycle decides read. A decide that rewrites the rules reads them
// whole beside it ([protectionsIn]), as it reads the path owners.
func repoViewIn(ctx context.Context, tx *sql.Tx, id string, cp statelog.Position) (repoView, bool, error) {
	var (
		v                                                 repoView
		status, managed, kind, owners, visibility, search string
		deleted                                           sql.NullInt64
		active                                            int64
	)
	err := tx.QueryRowContext(ctx, repoViewSQL, id).Scan(&v.ref.ID, &v.ref.Name, &status, &managed, &kind,
		&owners, &v.ref.Owner.Ref, &visibility, &search, &v.ref.DefaultBranch, &v.ref.Description, &deleted,
		&active, &v.ref.Refs, &v.branches, &v.tags, &v.tombstones, &v.ref.LivePacks, &v.ref.LiveBytes,
		&v.ref.LiveObjects, &v.ref.DropEpoch, &v.tombstonesThrough, &v.retiredThrough, &v.layersPosition, &v.pushes,
		&v.lastPushAt, &v.createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoView{}, false, nil
	case err != nil:
		return repoView{}, false, fmt.Errorf("code: read repository %s: %w", id, err)
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
// included — the caller decides what such a repository answers, and answers
// it with [RepoGone]'s sentence for its state, built from the ref's Status and
// DeletedAt and the deciding epoch's code.deleted_retention_days: a deleted
// one is told when its purge MAY BEGIN, and a purging one that nothing brings
// it back. Never a "restorable until" deadline, which would promise an
// instant nothing holds the purge to, and which a purge already begun does not
// have at all.
//
// AN OLD NAME a rename left redirecting answers the repository it redirects
// to, with [RepoRef.Redirect] naming its current name — the code host
// serves a clone or a fetch of it and refuses a push naming the new name, and
// a tool given the old name says so — for as long as the old name's row
// lasts, which its release ends.
func (r *Reader) Resolve(ctx context.Context, q ResolveQuery) (RepoRef, error) {
	if err := checkFreshness(q.Fresh); err != nil {
		return RepoRef{}, err
	}
	if !ValidName(q.Name) {
		return RepoRef{}, fmt.Errorf("%w: name: %q is not a repository name", ErrInvalid, q.Name)
	}
	var out RepoRef
	_, err := r.log.Read(ctx, q.Fresh.Resolved(resolver(q.Name), false), func(tx *sql.Tx) error {
		cp, err := checkpointIn(ctx, tx)
		if err != nil {
			return err
		}
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

// page is one page's budget as it fills: at least one row, and it is full at
// its row limit or once its rows' JSON reaches its byte budget — so a page
// never carries more than its budget and one row. A row's size is its own
// computed length (rowbytes.go), never an encoding made to be measured.
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

// full reports a page that takes no more rows.
func (p *page) full() bool { return p.rows >= p.maxRows || p.bytes >= p.maxB }

// left is how many rows the page may still take.
func (p *page) left() int { return p.maxRows - p.rows }

// Snapshot answers one page of a repository's refs and packs: in full — every
// pack and every live ref — or as a delta from q.Since — the packs added, the
// refs moved (tombstones included) and the packs retired above it. Page 1
// decides which ([choose]): a delta only from a position on this generation
// of the log, at or past the repository's tombstone mark and its retired
// mark, and carrying no more rows than the full answer would — the
// retirements each would serve counted on its own side, so those both would
// serve cancel.
//
// # Retirements
//
// A pack a record retires leaves code_packs for code_packs_retired, so a cache
// holding its file learns of it only from a RETIRED phase, served after the
// refs (and a delta's pushes): every retirement in (the phase's floor, page
// 1's checkpoint], in (position, pack id, part) order. A delta's floor is its
// own Since. A full answer's is q.RetiredSince, the caller's own position —
// what the full answer stands in for a delta from — so a cache reading the
// whole of a repository, as every git node does at its first round after a
// restart, can still tell a pack file a compaction retired from one no row
// accounts for, and keep what a merge leaves whole (its commit-graph) rather
// than drop it for every repository compacted while the node was down. A
// full answer serves them only when it can serve EVERY one ([choose]), and
// RetiredFrom says whether it did.
//
// # Published layers
//
// The repository's published chain is the FIRST phase, served WHOLE on page 1
// alone, read in page 1's own transaction: on every full answer, an empty
// chain included, and on a delta exactly when a record changed the chain above
// its floor (the repository's layers_position), and LayersServed says which.
// It is never continued, so no cursor names it and a later page serves none:
// the widest chain the bounds admit is MaxPublishedLayers rows and at most
// MaxPacksPerRepo checksums across them, well under a page, and charged to
// page 1's budget, so the packs after it get what is left. Read at page 1's
// checkpoint it is consistent with the packs the snapshot serves — every pack
// a layer names is live there, because nothing retires a published pack but
// the record that replaces its layer — and a publication after it is the next
// delta's to serve, as a retirement after it is.
//
// # One monotone history across pages
//
// A snapshot of more than one page is read in several transactions, possibly
// on several nodes, while the repository is pushed to. Four rules keep its
// pages one history:
//
//   - A ref a later page serves may name a commit whose pack was added after
//     the packs phase passed its key (a push received earlier can commit
//     later), so a snapshot of more than one page ends with a TAIL: every
//     pack added after page 1's checkpoint. A pack served twice is harmless —
//     a cache's target is a set keyed by pack.
//   - Every later page reads at session, floored at the highest checkpoint
//     any earlier page was read at (the cursor's Seen), so whichever node
//     answers has applied at least what every earlier page was read at, or
//     refuses `behind` and the router asks another — and the tail's last read
//     is at or past every refs read.
//   - The retired phase has NO TAIL: it stops at page 1's checkpoint, and a
//     pack retired after it is the next delta's to serve, from a floor that
//     checkpoint names. A later page that no longer finds such a pack among
//     the packs costs no ref an object, because the record that retired it
//     added the packs holding its objects (a merge drops none), after the
//     checkpoint, which the tail serves.
//   - A later page re-checks, in its own transaction, that the history did
//     not move under the snapshot: the same stream, the same generation, for
//     a delta no tombstone above its floor swept, no retirement above the
//     retired phase's floor swept, and the same drop epoch — pages spanning
//     an expunge's begin would serve, live, the packs it rewrote beside a row
//     carrying its epoch, and a push checked against that copy would carry
//     an epoch its objects were not read at ([Proposal.DropEpoch]). Any
//     failure is [ErrSnapshotMoved], and the consumer reads the snapshot
//     again from its first page, which decides full or delta afresh.
//
// The answer's Position is ALWAYS page 1's checkpoint — the position the whole
// snapshot is complete as of, which a cache records once it applied every page.
func (r *Reader) Snapshot(ctx context.Context, q StateQuery) (RepoSnapshot, error) {
	if q.RepoID == "" {
		return RepoSnapshot{}, fmt.Errorf("%w: repo_id: a snapshot names its repository", ErrInvalid)
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
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		out = RepoSnapshot{}
		cp, err := checkpointIn(ctx, tx)
		if err != nil {
			return err
		}
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
// with the floor the answer's retired phase serves from: Since on a delta,
// and on a full answer q.RetiredSince where it can serve every retirement
// above it, else zero.
//
// A DELTA ONLY FROM A FLOOR WHOSE HISTORY IS WHOLE: a position on this
// generation of the code log — one from another number space compares as
// nothing in this one — and at or past both of the repository's marks, since
// below the one a sweep removed the tombstones that told of deletions above
// the floor, and below the other the retired rows that told of retirements.
// Short of that the full answer is the only complete one.
//
// A FULL ANSWER SERVES ITS CALLER'S RETIREMENTS ONLY WHEN IT CAN SERVE EVERY
// ONE: RetiredSince on this generation, at or past the retired mark for the
// same reason, and at most [MaxFullRetirements] of them above it — one page,
// whatever the repository's own rows, which say nothing of what serving none
// costs ([MaxFullRetirements] says why). Short of any of them it serves none
// rather than some, so a RetiredFrom it answers always names a floor its
// retirements are COMPLETE above: a cache handed none treats every file no
// row names as one no row accounts for, which is correct and only dearer.
//
// THEN THE TWO ANSWERS ARE WEIGHED LIKE WITH LIKE ([deltaOutweighs]): the
// delta's refs, packs and retirements against the full answer's refs, packs
// and the retirements it would serve. A caller asking from the position it
// holds — what a full answer stands in for a delta from, and a git node's
// cache's own — has a full answer that would serve the very retirements the
// delta carries: they sit on both sides and cancel, and the delta is answered
// full only where its refs and packs outweigh the full answer's. Counted on
// the delta's side alone, they would replace a delta by a full answer
// carrying them all over again beside every row the repository holds — more
// rows, and a round that sweeps the cache's whole directory and rewrites its
// refs where the delta was applied in place. Only those the full answer
// would not serve — past the bound, or where its caller asked for none —
// weigh against the delta alone: a cache a thousand compactions behind reads
// the fifty packs its repository holds rather than some sixteen thousand
// retirements ([MaxFullRetirements]), and gives up only telling a retired
// pack file from one no row accounts for.
//
// EACH FLOOR'S RETIREMENTS ARE COUNTED ONCE, in one bounded statement over the
// index the retired phase reads, deep enough for every question asked of
// them: from a RetiredSince that is not the delta's floor, one past the bound
// (whether the full answer serves them); from the delta's floor, one past the
// larger of the bound and the full answer's rows where the full answer's
// floor is the same (whether it serves them, and else whether they alone
// outweigh it), and otherwise one past what the full answer would carry.
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
		return 0, fmt.Errorf("code: count %s's retirements above %s: %w", repoID, floor, err)
	}
	return n, nil
}

// deltaOutweighs reports whether a delta from since — its refs moved and packs
// added, and retired retirements — would carry more rows than the full answer
// it stands in for — the repository's whole rows, refs and live packs, and
// the served retirements it would carry ([choose]): then the full answer is
// served, which is always correct and here the smaller.
//
// # Why a delta can outweigh the full answer
//
// A delta carries every ref row that moved above its floor, TOMBSTONES
// INCLUDED, and a full answer only the live refs. A repository holds up to
// MaxTombstonesPerRepo tombstones beside its live refs, so a cache synced
// before a clean-up that deleted more refs than the repository keeps — sixty
// thousand deleted, forty thousand left — would be handed a delta of sixty
// thousand rows where the full answer is forty, on every git node holding the
// repository, and a cache holds a whole snapshot in memory before it writes
// any of it. Answered here, NO DELTA IS EVER MORE ROWS THAN THE FULL ANSWER,
// so by rows the largest answer the reader serves is what it was before
// tombstones were bounded apart — a full snapshot of a full repository — and
// the retirements a full answer serves beside its rows, at most one page
// ([MaxFullRetirements]).
//
// # Every row each answer carries, packs included
//
// A delta's packs are those added above its floor and a full answer's every
// pack the repository holds, so the packs only ever favour the delta: a
// repository of many packs keeps its delta past a clean-up a repository of
// few would not, which a comparison of the refs alone would answer in full
// for a fraction of the rows. The full answer's size is the repository's own
// counters, kept as the rows move (live_packs is every pack row: a retired
// pack leaves code_packs for code_packs_retired), and the retirements it
// would serve; the delta's refs and packs are COUNTED, each table at most one
// row past the room its retirements leave beside the full answer, over the
// index the delta's pages read (code_refs_changed_idx, code_packs_added_idx)
// — so the usual delta after a push reads a handful of index entries, and the
// count never reads more of either index than the larger answer would. A tie
// is a delta: a cache applies a delta in place, where a full answer sweeps
// its directory to the snapshot's image.
//
// # Retirements, like with like
//
// A delta carries every pack retired above its floor, and a full answer the
// retirements above its caller's RetiredSince where it can serve every one —
// the very ones the delta carries when, as a cache always does, the caller
// asks from the delta's own floor. Each side is weighed with what it would
// carry, so retirements both would serve cancel, and only those the full
// answer would not serve weigh against the delta alone ([choose] counts them,
// and says why).
//
// # Rows, and what that leaves in bytes
//
// It compares ROWS because a row is what both indexes count in a bounded
// read; an answer's bytes are the JSON its names spell, which only a read of
// every name the full answer carries could sum. So in bytes the rule holds an
// answer at its widest, not each answer to its own full answer — a delta of
// long deleted names beside short live ones may be wider than the full answer
// it replaces — and at its widest, a delta passes the full answer of a
// repository of as many refs and packs only by tombstones carried in place of
// packs, at most a tombstone less a pack row a pack: nothing for names JSON
// writes as themselves, 1 108 bytes a pack for names it spells longest
// ([MaxTombstonesPerRepo] gives the arithmetic). Weighing each pack as a
// fraction of the widest ref row would close that, and answer a repository of
// many packs in full for every delta of ordinary names, whose tombstones are
// narrower than a pack row rather than wider.
func deltaOutweighs(ctx context.Context, tx *sql.Tx, repoID string, since, retired, whole, served int64) (
	bool, error) {

	room := whole + served - retired
	if room < 0 {
		return true, nil
	}
	var moved int64
	if err := tx.QueryRowContext(ctx, deltaRowsSQL, repoID, since, room+1).Scan(&moved); err != nil {
		return false, fmt.Errorf("code: count %s's delta: %w", repoID, err)
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
			// THE LAST PHASE COMPLETED — and on page 1 there is no tail,
			// because a snapshot read in one transaction is one history
			// already.
			return nil, nil
		}
	}
}

// layersPhase serves the repository's whole published chain onto page 1, its
// parts joined to their layers, and charges every row to the page — whole
// whatever the page has left, since the phase never resumes, and the widest
// chain the bounds admit is far under a page (TestTheLayersPhaseFitsPageOne).
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
			return fmt.Errorf("code: read %s's published layers' parts: %w", repoID, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				id, object, hash string
				n                int
				size             int64
			)
			if err := rows.Scan(&id, &n, &object, &hash, &size); err != nil {
				return fmt.Errorf("code: read %s's published layers' parts: %w", repoID, err)
			}
			i, ok := at[id]
			if !ok {
				// A PART OF NO LAYER OF THE CHAIN: the apply removes a
				// layer's parts with its row, so this is rows no record
				// wrote, which no snapshot may answer as a chain.
				return fmt.Errorf("code: %s holds part %d of layer %s, which its chain does not", repoID, n, id)
			}
			k, err := objstore.ParseKey(object)
			if err != nil {
				return fmt.Errorf("code: part %d of %s's layer %s names %q: %w", n, repoID, id, object, err)
			}
			if n != len(chain[i].Layer.Parts) {
				return fmt.Errorf("code: %s's layer %s holds part %d where part %d was due", repoID, id, n,
					len(chain[i].Layer.Parts))
			}
			chain[i].Layer.Parts = append(chain[i].Layer.Parts, LayerPart{Object: k, Hash: objstore.Hash(hash),
				Size: size})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("code: read %s's published layers' parts: %w", repoID, err)
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
		return false, fmt.Errorf("code: read %s's packs: %w", repoID, err)
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
		return false, fmt.Errorf("code: read %s's packs: %w", repoID, err)
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
		)
		if err := rows.Scan(&p.PackID, &p.Part, &object, &hash, &p.Size, &p.IndexBytes,
			&p.ReverseBytes, &p.GitChecksum, &p.Objects, &p.Kind, &p.State, &added, &content); err != nil {
			return false, fmt.Errorf("code: read a pack: %w", err)
		}
		k, err := objstore.ParseKey(object)
		if err != nil {
			return false, fmt.Errorf("code: pack %s names %q: %w", p.PackID, object, err)
		}
		p.Object, p.Hash, p.Added, p.Content = k, objstore.Hash(hash), unpacked(added), unpacked(content)
		out.Packs = append(out.Packs, p)
		served++
		cur.After = key(p)
		if pg.add(p.encodedBytes()) {
			return false, rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("code: read packs: %w", err)
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
		return false, fmt.Errorf("code: read %s's refs: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	asked, served := pg.left(), 0
	for rows.Next() {
		var ref RefRow
		var position int64
		if err := rows.Scan(&ref.Ref, &ref.OID, &position); err != nil {
			return false, fmt.Errorf("code: read a ref: %w", err)
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
		return false, fmt.Errorf("code: read refs: %w", err)
	}
	return served < asked, nil
}

// pushesPhase serves a delta's pushes phase: every push of the repository
// applied in (Since, From] — above the cache's synced position and at or below
// page 1's checkpoint — in log order, keyset after the last one served.
//
// BOUNDED ABOVE BY PAGE 1'S CHECKPOINT, never by the page's own: the snapshot
// is complete as of From, which the cache records as its position, so a push
// applied after it is the NEXT delta's — served here too, it would be folded
// into the cache's index twice, once by each round. And bounded BELOW
// exclusively, for the same reason: a push at Since is the round before's.
//
// EACH PUSH COSTS THE PAGE WHAT IT READS where that is more than what it
// sends ([scanIntroduced]): its row stores every command its client sent, and
// the phase sends only the refs it landed.
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
		return false, fmt.Errorf("code: read %s's pushes since %s: %w", repoID, cur.Since, err)
	}
	defer func() { _ = rows.Close() }()
	asked, served := pg.left(), 0
	for rows.Next() {
		in, cost, err := scanIntroduced(rows)
		if err != nil {
			return false, fmt.Errorf("code: read %s's pushes since %s: %w", repoID, cur.Since, err)
		}
		out.Pushes = append(out.Pushes, in)
		served++
		cur.After = logCursor(in.Position.Packed(), in.PushID)
		if pg.add(cost) {
			return false, rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("code: read %s's pushes since %s: %w", repoID, cur.Since, err)
	}
	return served < asked, nil
}

// retiredPhase serves the retired phase: every pack a record retired in
// (RetiredFrom, From] — above the phase's floor and at or below page 1's
// checkpoint — in (position, pack id, part) order, the order of
// code_packs_retired_repo_idx, keyset after the last one served.
//
// BOUNDED ABOVE BY PAGE 1'S CHECKPOINT, never by the page's own, for the
// pushes phase's reason: the snapshot is complete as of From, which the cache
// records as its position, so a pack retired after it is the NEXT delta's to
// serve, from that position — served here too, it would be served twice, and
// as of a position at which the rows still named it live. And bounded BELOW
// exclusively: a retirement at the floor is one the round that synced to it
// already took.
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
		return false, fmt.Errorf("code: read %s's retired packs above %s: %w", repoID, cur.RetiredFrom, err)
	}
	defer func() { _ = rows.Close() }()
	asked, served := pg.left(), 0
	for rows.Next() {
		var (
			row                 RetiredRow
			retiredAt, position int64
		)
		if err := rows.Scan(&row.PackID, &row.Part, &row.GitChecksum, &retiredAt, &position); err != nil {
			return false, fmt.Errorf("code: read %s's retired packs above %s: %w", repoID, cur.RetiredFrom, err)
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
		return false, fmt.Errorf("code: read %s's retired packs above %s: %w", repoID, cur.RetiredFrom, err)
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
		return PushOutcome{}, fmt.Errorf("%w: at: a push's outcome is read at its record's position", ErrInvalid)
	case q.RepoID == "" || q.PushID == "":
		return PushOutcome{}, fmt.Errorf("%w: a push's outcome names its repository and its id", ErrInvalid)
	}
	query := statelog.FloorAt(statelog.ReadSession, q.At).Query(
		statelog.ScopeSet{Paths: []string{RepoPath(q.RepoID)}}, false)
	var out PushOutcome
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		out = PushOutcome{}
		cp, err := checkpointIn(ctx, tx)
		if err != nil {
			return err
		}
		out.Position = cp.At
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
		return PushRow{}, false, fmt.Errorf("code: read push %s: %w", pushID, err)
	}
	return row, true, nil
}

// scanner is a row or a row of rows.
type scanner interface{ Scan(dest ...any) error }

// scanPush reads a push row in pushColumns' order, and answers the length of
// its stored commands — encoding/json's encoding of the list it decodes to,
// which is what the row's own encoding carries, so a page measures it without
// encoding it again. A row a NEWER build applied may store a field this build
// does not decode; its stored length is then the longer, and a page counted
// by it ends early rather than late.
//
// A ROW NAMING NO WAY THIS BUILD KNOWS IS REFUSED ([ErrInvalid], naming the
// push), never read as one: its column's default is the empty string rather
// than a way (replicated 0063), so an apply that forgot to write it is a
// failed read here, where a default naming a way would have answered every
// push the engine built as one a client's git sent.
func scanPush(s scanner) (PushRow, int, error) {
	var (
		row                       PushRow
		position, at, received    int64
		via, commands, introduced string
	)
	if err := s.Scan(&row.PushID, &row.RepoID, &position, &at, &row.Actor, &row.ActorKind,
		&row.ActorSeat, &via, &row.Run.TurnID, &row.Run.WorkBackend, &row.Run.WorkKey, &row.Run.Model,
		&row.Run.RunID, &row.Run.CodingAgent, &row.Node, &row.Atomic, &commands, &row.PackID, &row.PackBytes,
		&introduced, &row.IntroducedCount, &received); err != nil {
		return PushRow{}, 0, err
	}
	if row.Via = PushVia(via); !row.Via.Valid() {
		return PushRow{}, 0, fmt.Errorf("%w: push %s's row names its way as %q, which is not one of %v",
			ErrInvalid, row.PushID, via, PushVias())
	}
	row.Position, row.At, row.ReceivedAt = unpacked(position), store.DecodeTime(at), store.DecodeTime(received)
	if err := json.Unmarshal([]byte(commands), &row.Commands); err != nil {
		return PushRow{}, 0, fmt.Errorf("code: push %s's commands: %w", row.PushID, err)
	}
	if introduced != "" {
		row.Introduced = strings.Split(introduced, " ")
	}
	return row, len(commands), nil
}

// scanIntroduced reads a push row as the index of which push introduced each
// commit is built from it ([introducedOf]), and answers what serving it costs
// a page: the larger of the row it SENDS and the commands it READ.
//
// A PAGE OF CONDENSED PUSHES IS HELD TO WHAT IT READS, not only to what it
// sends. A push row stores every command its client sent, a refused one too
// ([RecordedRef]), and the condensed row only the refs the push landed — so
// a push of one branch beside 999 refused refs at MaxRefNameBytes, a mirror
// push refused everywhere but one branch or a crafted client's, is 656 bytes
// sent and 403 KB read and decoded. Counted by what it sends, a page took
// 6 393 of them and decoded 2.6 GB of commands to send four mebibytes: past
// any estate read's attempt, and again on every retry, so a backfill or a
// delta sync that met a run of them never got past it. Nothing else a push
// row holds is wider than what it sends — its introduced ids are sent whole,
// as are its id, its position and its principal, and the rest is a few
// fixed columns — so a page reads at most its byte budget and one row of
// commands beside the rows it sends: the history listing's bound, which
// sends the commands themselves.
func scanIntroduced(rows *sql.Rows) (IntroducedRow, int, error) {
	pushed, commands, err := scanPush(rows)
	if err != nil {
		return IntroducedRow{}, 0, err
	}
	in := introducedOf(pushed)
	return in, max(in.encodedBytes(), commands), nil
}

// The read limits, past which a query is refused naming the bound rather than
// clamped.
//
// A page's BYTES are held to MaxPageBytes whatever its count — measured with
// every row's rules and commands, and where a page sends its pushes condensed
// (the `introduced` read, a snapshot's pushes phase) with the commands it read
// to condense them ([scanIntroduced]) — so these bound what one request READS
// and are sized to what a surface shows of it:
//
//   - a listing of repositories: 100 by default, a screen of them, and 500
//     at most, which with every repository at MaxProtectionsPerRepo rules is
//     32 000 rule rows for the page's one protections statement, and ten
//     thousand repositories in twenty pages;
//   - a push history: 50 by default, a screen of it, and 200 at most, which
//     at a typical push row (1–2 KB: a few commands and the commits they
//     introduced) is under half a megabyte, while a page of maximal pushes
//     ends at the byte budget after its tenth (about 460 KB each:
//     MaxCommandsPerPush commands at the longest ref and MaxIntroduced ids)
//     — after its third where every byte of every ref is one JSON writes as
//     six (`<`, `>`, `&`, which a ref may hold: 1.7 MB a row);
//   - a refs listing: 200 by default, a screen of branches, and 1 000 at
//     most, which at the longest ref (MaxRefNameBytes) and its oid is about
//     385 KB, under a megabyte for names JSON writes as themselves — and
//     ends at the byte budget like every page where it spells them longer;
//   - a listing of changes: 50 by default, a screen of them, and 200 at
//     most, a push history's bounds for the same reader — a person's screen
//     or a seat's tool — over rows of 0.6–0.9 KB (a person's change with a
//     short title, and a seat's naming the turn, the run and the work item
//     it was opened for), so under 200 KB, while a page of the widest
//     (titles, refs and close reasons every byte of which JSON writes as six)
//     ends at the byte budget first. The bound is on the rows a page READS,
//     whatever its filters keep of them, so a page a seat reads little of
//     costs what a full one does and no more;
//   - a change's revisions: 200 by default and 1 000 at most, a refs
//     listing's bounds, for rows as narrow as a ref's — an oid, a handle, a
//     push id, a position and an instant, about 250 bytes — so a page of
//     1 000 is about 250 KB, and a change pushed two hundred times is one
//     page;
//   - a change's reviewers: a revision's bounds, for rows as narrow — two
//     handles, a reason, a state, a position, and a note most asks leave
//     empty — so a change's every ask, MaxReviewersPerChange at once and
//     the withdrawn ones beside them, is one page, and one of notes at
//     their 1 KiB ends at the byte budget first;
//   - a change's comments: a listing of changes' bounds, 50 a screen of a
//     timeline and 200 at most, over rows of a few hundred bytes to
//     MaxCommentBytes — 200 one-line comments are under 100 KB, and a page
//     of long ones ends at the byte budget first;
//   - a change's reviews and its findings: a comment's bounds, for rows of
//     the same width — a review's summary up to MaxReviewSummaryBytes, a
//     finding's narrow row of about 600 bytes beside a title, a body up to
//     MaxFindingBodyBytes and a suggestion up to MaxSuggestionBytes — so a
//     review of MaxFindingsPerReview findings is one page of them where
//     they say what a reviewer's finding usually says, and a page of
//     findings with long suggestions ends at the byte budget first;
//   - a listing of checks: a listing of changes' bounds, 50 a screen and
//     200 at most, over rows of about 700 bytes — a key, a state, a target,
//     a runner and five instants and positions — beside a summary of up to
//     CheckSummaryBytes, so 200 attempts summarised in a line are under
//     200 KB, and a page of summaries at their 4 KiB ends near 950 KB, under
//     the byte budget; the bound is on the rows a page READS, so a window a
//     seat reads little of costs what a full page does and no more.
//
// The maxima are exported for a client that pages: the most a page may ask
// for of each, which a page past them is refused naming.
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
	MaxChecksPerPage     = 200
	defaultRevisionLimit = 200
	MaxRevisionsPerPage  = 1_000
)

// BriefCommands is how many of a push's commands a row of its history read
// IN BRIEF carries ([ReadQuery.Brief]): FIVE, the lines a screen gives one
// push before its row stands taller than the one-line rows around it. A push
// of a branch or a handful of them — a person's, a seat's, an engine write's,
// which is one ref — is whole in five, with nothing left out; a mirror's or
// an import's push of up to MaxCommandsPerPush refs is five lines and the
// count of the rest ([PushRow.Omitted]) — 140 to 190 kilobytes of commands
// fewer on a row a poll sends again every minute, before its introduced ids —
// and the whole of it one read of the push away ([ReadQuery.Push]).
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
	fresh, levelErr := q.freshness()
	if levelErr != nil {
		return ReadAnswer{}, levelErr
	}
	// THE ONE CHART every row of this answer is judged by, read before the
	// transaction: a listing judged row by row against a chart an apply
	// swapped mid-page would answer a page no epoch ever held. And read
	// before any row: a seat asking a reader that holds none is refused
	// about the reader ([Reader.chartFor]), and so is every read of changes,
	// whose readiness the chart judges.
	chart, chartErr := r.chartFor(q.Asker, q.judgesReadiness())
	if chartErr != nil {
		return ReadAnswer{}, chartErr
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
		// A POINT READ OF EVERY REPOSITORY THE RUN WAS GIVEN: a record this
		// node deferred on one of them may carry a push of the run's its rows
		// do not hold, and an answer past it would say the run delivered less
		// than it did — so such a read refuses, and the engine asks another
		// node, rather than answer a run's pushes with some of them missing.
		paths := make([]string, len(q.RunPushes.Repos))
		for i, id := range q.RunPushes.Repos {
			paths[i] = RepoPath(id)
		}
		query = fresh.Query(statelog.ScopeSet{Paths: paths}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.runPushes(ctx, tx, q, &out) }
	case ReadRefs:
		// A FIRST PAGE IS READ AS THE DETAIL IT CARRIES: a point read, which
		// a record this node deferred on the repository refuses, as kind
		// repo is refused, rather than detail a repository around it; every
		// later page is the listing's, a set.
		query = fresh.Resolved(resolver(q.Repo), q.Cursor != "")
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.refsListing(ctx, tx, q, chart, cp, &out) }
	case ReadChanges:
		// A SET READ: of the repository it names, resolved by its name as
		// a push history is, or of the domain, as a listing of
		// repositories is — a change a deferred record holds back is
		// listed as its rows stand.
		if q.Repo != "" {
			query = fresh.Resolved(resolver(q.Repo), true)
		} else {
			query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		}
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.changes(ctx, tx, q, chart, &out) }
	case ReadChange:
		// A FIRST PAGE IS READ AS THE DETAIL IT CARRIES, a point read of the
		// repository whose records write the change, as a first page of
		// refs is; a later page is a page of revisions, a set, and so is
		// every page of a part of its conversation — whose records name the
		// repository's path in their scope too, so the repository's
		// deferrals order them.
		query = fresh.Resolved(resolver(q.Repo), q.Cursor != "" || !q.Part.detailed())
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.change(ctx, tx, q, chart, &out) }
	case ReadIntroduced:
		// A POINT READ OF THE REPOSITORY, as a snapshot is: an index built
		// past a record this node deferred on the repository would miss the
		// pushes it carried and say nothing, so such a read refuses and the
		// backfill asks again.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.Introduced.RepoID)}}, false)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.introduced(ctx, tx, q, cp, &out) }
	case ReadCompactionDue:
		// A SET READ OF THE DOMAIN, as a listing of repositories is: a
		// repository a deferred record holds back is listed as its rows
		// stand, and the compaction that follows is decided again from
		// the committer's own snapshot, which holds every input to them.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.compactionDue(ctx, tx, q, &out) }
	case ReadPublicationDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is, for the
		// same reason: the publication that follows is decided again from
		// the committer's own snapshot.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.publicationDue(ctx, tx, q, &out) }
	case ReadExpungeDue:
		// A SET READ OF THE DOMAIN, as the compaction's listing is, for the
		// same reason: every expunge record that follows is decided again
		// from the committer's own snapshot.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.expungeDue(ctx, tx, q, &out) }
	case ReadChangesDue:
		// A SET READ, of the domain or of the one repository
		// (changesdue.go): every record the home asks after it is decided
		// again from its writer's own snapshot.
		query = q.changesDueQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.changesDue(ctx, tx, q, &out) }
	case ReadChecks:
		// A SET READ, of the repository it names or of the domain for the
		// window (checksread.go).
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.checks(ctx, tx, q, chart, &out) }
	case ReadCheck:
		// A POINT READ of the repository's checks, as a change's detail is
		// one of the repository's rows.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.check(ctx, tx, q, chart, &out) }
	case ReadChecksDue:
		// A SET READ, of the domain or of the one repository, as
		// changes_due's: the dispatch that follows is decided again by the
		// repository's checks writer from its own snapshot.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.dueCheckWork(ctx, tx, q, &out) }
	case ReadCheckBox:
		// A POINT READ of the repository's checks, as a `check` read is: a
		// box is never admitted by an attempt read around a record this node
		// deferred on them.
		query = q.checksQuery(fresh)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.checkBox(ctx, tx, q, &out) }
	case ReadExpungeTips:
		// A POINT READ OF THE REPOSITORY, as an `introduced` read is: a
		// record this node deferred on the repository may carry a drop the
		// answer would leave out, and an expunge that read past it would
		// leave dropping an object the drop's grace keeps — so such a read
		// refuses, and the maintainer asks another node.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.ExpungeTips.RepoID)}}, false)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.expungeTips(ctx, tx, q, &out) }
	}
	answer, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		out = ReadAnswer{}
		cp, err := checkpointIn(ctx, tx)
		if err != nil {
			return err
		}
		out.Position = cp.At
		return serve(tx, cp.At)
	})
	if err != nil {
		return ReadAnswer{}, err
	}
	out.Level = answer.Level
	return out, nil
}

// validate refuses a query naming no kind this build reads, no asker or one no
// principal can be, a repository that cannot be one, a limit past its bound —
// refused naming it, never clamped — or a field outside the kind that reads
// it: a status filter outside a listing of repositories, a prefix outside a
// refs listing, a brief or one push outside a push history, a change's number
// or a part of it outside a change read, a state, an owner, a work item, a
// reviewer or a need outside a listing of changes, a range outside an
// `introduced` read, a run outside a `run_pushes` read, a floor of live packs
// outside a `compaction_due` read.
func (q ReadQuery) validate() error {
	if !q.Kind.Valid() {
		return fmt.Errorf("%w: kind: %q is not one of %v", ErrInvalid, q.Kind, ReadKinds())
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
	case ReadChangesDue:
		return q.validateChangesDue()
	case ReadChecksDue:
		return q.validateChecksDue()
	case ReadExpungeTips:
		return q.validateExpungeTips()
	case ReadCheckBox:
		return q.validateCheckBox()
	}
	// WHO ASKS IS NAMED, or nothing is read: an answer is held to the
	// asker's grant, and an absent asker would be judged as somebody nobody
	// named.
	if why := q.Asker.invalid(); why != "" {
		return fmt.Errorf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	switch {
	case q.Introduced != nil:
		return fmt.Errorf("%w: introduced: only an introduced read names a range of pushes", ErrInvalid)
	case q.RunPushes != nil:
		return fmt.Errorf("%w: run_pushes: only a run_pushes read names a coding run", ErrInvalid)
	case q.ChangesDue != nil:
		return fmt.Errorf("%w: changes_due: only a changes_due read names the repository its home owes "+
			"change work in", ErrInvalid)
	case q.ChecksDue != nil:
		return fmt.Errorf("%w: checks_due: only a checks_due read names the repository its home owes "+
			"check work in", ErrInvalid)
	case q.Checks != nil && q.Kind != ReadChecks && q.Kind != ReadCheck:
		return fmt.Errorf("%w: checks: only a checks or a check read names a change's, a commit's or an "+
			"attempt's checks", ErrInvalid)
	case q.CheckBox != nil:
		return fmt.Errorf("%w: check_box: only a check_box read names an attempt by its key", ErrInvalid)
	case q.ExpungeTips != nil:
		return fmt.Errorf("%w: expunge_tips: only an expunge_tips read names a repository's ref drops", ErrInvalid)
	case q.MinPacks != 0:
		return fmt.Errorf("%w: min_packs: only a compaction_due read names a floor of live packs", ErrInvalid)
	case q.Prefix != "" && q.Kind != ReadRefs:
		return fmt.Errorf("%w: prefix: only a refs listing names a prefix", ErrInvalid)
	case q.Prefix != "" && !validRefPrefix(q.Prefix):
		return fmt.Errorf("%w: prefix: %q is not refs/heads/ or refs/tags/ or a directory of refs under "+
			"either, ending in /", ErrInvalid, q.Prefix)
	case q.Brief && q.Kind != ReadPushes:
		return fmt.Errorf("%w: brief: only a push history is read in brief", ErrInvalid)
	case q.Push != "" && q.Kind != ReadPushes:
		return fmt.Errorf("%w: push: only a push history is narrowed to one push", ErrInvalid)
	case q.PathOwners && q.Kind != ReadRepo:
		return fmt.Errorf("%w: path_owners: only a read of one repository answers its path owners", ErrInvalid)
	case q.Gates && q.Kind != ReadRepo && (q.Kind != ReadRefs || q.Cursor != ""):
		return fmt.Errorf("%w: gates: only a read answering one repository's detail — a read of one "+
			"repository, or a refs listing's first page — answers its rules' merge gates", ErrInvalid)
	case q.Push != "" && !ValidPushID(q.Push):
		return fmt.Errorf("%w: push: %q is not a push's id, a UUIDv7 in its canonical lowercase spelling",
			ErrInvalid, q.Push)
	case q.Push != "" && (q.Cursor != "" || q.Limit != 0 || q.Brief):
		return fmt.Errorf("%w: push: one push is read whole, never paged, cut or read in brief", ErrInvalid)
	case q.Change != 0 && q.Kind != ReadChange:
		return fmt.Errorf("%w: change: only a change read names a change's number", ErrInvalid)
	case q.State != "" && q.Kind != ReadChanges:
		return fmt.Errorf("%w: state: only a listing of changes is narrowed to a change's state", ErrInvalid)
	case q.Owner != "" && q.Kind != ReadChanges:
		return fmt.Errorf("%w: owner: only a listing of changes is narrowed to a change's owner", ErrInvalid)
	case q.Work != "" && q.Kind != ReadChanges:
		return fmt.Errorf("%w: work: only a listing of changes is narrowed to a work item", ErrInvalid)
	case q.Reviewer != "" && q.Kind != ReadChanges:
		return fmt.Errorf("%w: reviewer: only a listing of changes is narrowed to a reviewer", ErrInvalid)
	case q.Needs != "" && q.Kind != ReadChanges:
		return fmt.Errorf("%w: needs: only a listing of changes is narrowed to what its changes wait on",
			ErrInvalid)
	case q.Part != "" && q.Kind != ReadChange:
		return fmt.Errorf("%w: part: only a change read names a part of the change", ErrInvalid)
	case q.Part != "" && !q.Part.Valid():
		return fmt.Errorf("%w: part: %q is not one of %v", ErrInvalid, q.Part, ChangeParts())
	}
	switch {
	case q.Kind == ReadRepos, q.Kind == ReadChanges && q.Repo == "", q.Kind == ReadChecks && q.Repo == "":
	case !ValidName(q.Repo):
		return fmt.Errorf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
	}
	if q.Kind != ReadRepos && len(q.Status) > 0 {
		return fmt.Errorf("%w: status: only a listing of repositories filters by status", ErrInvalid)
	}
	for _, s := range q.Status {
		if !s.Valid() {
			return fmt.Errorf("%w: status: %q is not one of %v", ErrInvalid, s, RepoStatuses())
		}
	}
	switch q.Kind {
	case ReadChanges:
		if err := q.validateChanges(); err != nil {
			return err
		}
	case ReadChange:
		if q.Change < 1 {
			return fmt.Errorf("%w: change: %d is no change's number: a repository numbers its changes "+
				"from 1", ErrInvalid, q.Change)
		}
	case ReadChecks, ReadCheck:
		if err := q.validateChecks(); err != nil {
			return err
		}
	}
	bound := map[ReadKind]int{ReadRepos: MaxReposPerPage, ReadPushes: MaxPushesPerPage,
		ReadRefs: MaxRefsPerPage, ReadChanges: MaxChangesPerPage, ReadChange: q.Part.maxPerPage(),
		ReadChecks: MaxChecksPerPage}[q.Kind]
	switch {
	case q.Limit < 0:
		return fmt.Errorf("%w: limit: %d", ErrInvalid, q.Limit)
	case q.Kind == ReadRepo && q.Limit != 0:
		return fmt.Errorf("%w: limit: one repository is not paged", ErrInvalid)
	case q.Limit > bound:
		return fmt.Errorf("%w: limit: %d is past the %d one page of %s carries", ErrInvalid,
			q.Limit, bound, q.Kind)
	}
	return nil
}

// validateIntroduced is [ReadQuery.validate] for an `introduced` read: the
// engine's own, so it names NO asker — one there would be judged by nothing,
// and a read that looked held to a grant while it was not is worse than one
// that says it is nobody's — and its repository by id in its range, which
// runs one way: (After, Through] ascending, or below Before descending.
func (q ReadQuery) validateIntroduced() error {
	in := q.Introduced
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
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
	case q.MinPacks != 0:
		return refused("min_packs", "only a compaction_due read names a floor of live packs")
	case q.Change != 0:
		return refused("change", "only a change read names a change's number")
	case q.State != "":
		return refused("state", "only a listing of changes is narrowed to a change's state")
	case q.Owner != "":
		return refused("owner", "only a listing of changes is narrowed to a change's owner")
	case q.Work != "":
		return refused("work", "only a listing of changes is narrowed to a work item")
	case q.RunPushes != nil:
		return refused("run_pushes", "only a run_pushes read names a coding run")
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
	case q.ExpungeTips != nil:
		return refused("expunge_tips", "only an expunge_tips read names a repository's ref drops")
	case q.Reviewer != "":
		return refused("reviewer", "only a listing of changes is narrowed to a reviewer")
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

// validateRunPushes is [ReadQuery.validate] for a `run_pushes` read: the
// engine's own — the record of what a coding run delivered, read when the run
// is collected — so it names NO asker, for an `introduced` read's reason, and
// no seat it reads for; it names the run by the launch id its pushes' rows
// carry, the repositories the run was given by id — one to MaxRunRepos, each
// once — and the position the run was launched at on the code log, never
// zero: a read from the log's start would scan its repositories' whole
// history for pushes no run made before its launch. Nothing else a listing
// names, and a limit of the pushes a page reads, at most MaxSnapshotItems as
// an `introduced` read's ([Reader.runPushes] says why).
func (q ReadQuery) validateRunPushes() error {
	in := q.RunPushes
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a run_pushes read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a run_pushes read is the engine's own, which no seat makes for another")
	case in == nil:
		return refused("run_pushes", "a run_pushes read names its coding run, the run's repositories and the "+
			"position it was launched at")
	case !canonicalUUID(in.Run):
		return refused("run_pushes.run", fmt.Sprintf("%q is not a UUID in its canonical lowercase spelling — "+
			"a coding run is read by the launch id its pushes name", in.Run))
	case len(in.Repos) == 0 || len(in.Repos) > MaxRunRepos:
		return refused("run_pushes.repos", fmt.Sprintf("%d repositories; a coding run is given one to %d "+
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
		{"min_packs", q.MinPacks != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates},
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

// validateCompactionDue is [ReadQuery.validate] for a `compaction_due` read:
// the engine's own — a git node's compactor polling for the repositories a
// compaction may be due in — so it names NO asker, as an `introduced` read
// does: a principal's read of the duty listing would be held to nothing,
// and every surface a principal reads the code host through names its asker,
// so the one place they all meet refuses it here. It names its floor of live
// packs, at least one, and nothing else: no repository, no filter, no range
// and no limit, since a page is MaxSnapshotItems repositories.
func (q ReadQuery) validateCompactionDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
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
		{"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates},
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

// validatePublicationDue is [ReadQuery.validate] for a `publication_due` read:
// the engine's own — a git node's publisher polling for the repositories a
// publication may be due in — so it names NO asker, for the `compaction_due`
// read's reason, and nothing else at all: no repository, no filter, no range,
// no floor and no limit, since its partial index holds exactly the
// repositories it lists and a page is MaxSnapshotItems of them.
func (q ReadQuery) validatePublicationDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
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
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates},
	} {
		if f.set {
			return refused(f.field, "a publication_due read lists every repository holding a pack no "+
				"published layer covers, a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateExpungeDue is [ReadQuery.validate] for an `expunge_due` read: the
// engine's own — a repository's maintainer polling for the expunges it owes —
// so it names NO asker, for the `compaction_due` read's reason, and nothing
// else at all: no repository, no filter, no range, no floor and no limit,
// since its partial index holds exactly the repositories it lists and a page
// is MaxSnapshotItems of them.
func (q ReadQuery) validateExpungeDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	if q.Asker != (Principal{}) {
		return refused("asker", "an expunge_due read is the engine's own, which no principal makes")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates},
	} {
		if f.set {
			return refused(f.field, "an expunge_due read lists every repository with an expunge open or a "+
				"purge requested, a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateExpungeTips is [ReadQuery.validate] for an `expunge_tips` read: the
// engine's own — a repository's maintainer reading what an expunge keeps
// beside the refs — so it names NO asker, for an `introduced` read's reason;
// it names its repository by id and the instant its drops are listed from,
// never zero (an expunge always keeps a grace's worth or none, counted back
// from its scan), and nothing else — no filter, no range and no limit, since
// a page is MaxSnapshotItems drops.
func (q ReadQuery) validateExpungeTips() error {
	in := q.ExpungeTips
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
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
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates},
	} {
		if f.set {
			return refused(f.field, "an expunge_tips read lists one repository's ref drops since an instant, a "+
				"page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// freshness is the level a read is served at: EVERY LEVEL, by every code
// read's one rule ([checkFreshness]) — `session` only beside a position on the
// code log, which is the position it waits for, and a floor on any other log
// refused as the request's mistake. Where none is named, a principal's read and
// a duty listing are read stale — the REST read path's level, and what a poll
// of the duty listings needs — and the engine's own `introduced` and
// `run_pushes` are refused, since whoever builds an index names the level its
// pages must be read at, and whoever reads what a run delivered the level its
// "nothing" may be said at: a run's pushes read stale would answer none for a
// push another node has already applied.
//
// # Why a principal's read serves `session`
//
// It refused the level, because a surface reading the code host held no
// position of its own on the code log to wait for. A screen does now: a write
// it makes to the code log answers with where its record landed, and the
// dashboard reads its own writes back by asking every question of that log at
// `session` with the position as `min_position` (its SESSION_QUERIES), so the
// list it just changed never comes back from a node that has not applied the
// change. Refused, every such read was a `400` the screen could only answer by
// reading `stale` — rows from before the press. A bare `session` stays
// refused: it would wait for the zero position and label this node's prefix
// `session`, the wrong label the level exists to make impossible.
func (q ReadQuery) freshness() (statelog.Freshness, error) {
	fresh := q.Fresh
	if fresh.Level == "" && q.Kind != ReadIntroduced && q.Kind != ReadRunPushes && q.Kind != ReadExpungeTips {
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

// limitOr is a limit, or its kind's default.
func limitOr(limit, fallback int) int {
	if limit == 0 {
		return fallback
	}
	return limit
}

// repos serves a listing of repositories, by name, of those the asker may
// read — and, where it reads for a seat its turn is acting for, that seat may
// read too ([JudgeRead]) — each carrying the asker's access to it.
//
// THE PAGE IS WHAT WAS SCANNED, NOT WHAT WAS KEPT: a repository the asker may
// not read, or its ceiling may not, is left out, and the cursor is still the
// last row the statement scanned — so a page can be shorter than its limit,
// or empty with a Next, and a caller pages until no Next comes back. Cut at
// the kept rows instead, a page of repositories the asker may not read would
// end the listing early, or a filter run until the page filled would scan
// without bound for a seat that reads little.
func (r *Reader) repos(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {
	statuses := q.Status
	if len(statuses) == 0 {
		statuses = []RepoStatus{RepoStatusActive, RepoStatusArchived}
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
	rows, err := tx.QueryContext(ctx, reposSQL, list, q.Cursor, limit)
	if err != nil {
		return fmt.Errorf("code: list repositories: %w", err)
	}
	var (
		ids         []string
		scanned     int
		lastScanned string
	)
	for rows.Next() {
		var (
			v                                                 repoView
			status, managed, kind, owners, visibility, search string
			deleted                                           sql.NullInt64
			active                                            int64
			head                                              sql.NullString
		)
		if err := rows.Scan(&v.ref.ID, &v.ref.Name, &status, &managed, &kind, &owners, &v.ref.Owner.Ref,
			&visibility, &search, &v.ref.DefaultBranch, &v.ref.Description, &deleted, &active, &v.ref.Refs,
			&v.ref.LivePacks, &v.ref.LiveBytes, &v.ref.LiveObjects, &v.ref.DropEpoch, &v.pushes, &v.lastPushAt,
			&head); err != nil {
			_ = rows.Close()
			return fmt.Errorf("code: list repositories: %w", err)
		}
		scanned, lastScanned = scanned+1, v.ref.Name
		v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
		v.ref.Kind, v.ref.Visibility, v.ref.Search = RepoKind(kind), Visibility(visibility), SearchMode(search)
		v.ref.LastActivityAt = activityAt(active)
		g, refused := JudgeRead(q.Asker, q.CeilingSeat, v.ref, chart)
		if refused != nil {
			continue
		}
		if deleted.Valid {
			v.ref.DeletedAt = store.DecodeTime(deleted.Int64)
		}
		v.ref.Position = cp
		s := v.summary(head.String)
		s.Access = g.Access.String()
		out.Repos = append(out.Repos, s)
		ids = append(ids, s.ID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("code: list repositories: %w", err)
	}
	_ = rows.Close()
	if err := protectionsOf(ctx, tx, ids, out.Repos); err != nil {
		return err
	}
	// MEASURED WITH THEIR RULES, which are read in one statement for the
	// whole page and are most of a row's bytes when a repository has many:
	// counted before them, a page of 500 repositories at 64 long rules each
	// was ten megabytes past a budget it believed it kept. The page is cut at
	// the row that fills it, and the cursor is that row's name; a page cut at
	// no kept row is full when the statement filled its limit, and its cursor
	// the last row scanned.
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for i, s := range out.Repos {
		if pg.add(s.encodedBytes()) {
			out.Next = s.Name
			out.Repos = out.Repos[:i+1]
			return nil
		}
	}
	if scanned == limit {
		out.Next = lastScanned
	}
	return nil
}

// protectionsOf fills a page of repositories' rules in ONE statement, each as
// a push is judged by it ([PushRule]): a listing answers no merge gate, which
// one repository's detail answers a read that asks ([ReadQuery.Gates]) — so a
// page decodes none, and its rows weigh what it sends of them.
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
		return fmt.Errorf("code: read the listing's protections: %w", err)
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
			return fmt.Errorf("code: read the listing's protections: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		i := at[id]
		repos[i].Protections = append(repos[i].Protections, p)
	}
	return rows.Err()
}

// repo serves q's repository in detail, to an asker that may read it — and,
// where it reads for the seat its turn is acting for, that seat may read it
// too ([JudgeRead]).
//
// A REPOSITORY THE ASKER MAY NOT READ IS [ErrNotPermitted], and one its
// ceiling may not [ErrAboveCeiling], each judged after its row is read, and
// one that does not exist is still [ErrRepoNotFound]: that a `visibility:
// unit` repository exists is no secret — its name is in the company's
// document — and what it holds is.
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
	head, _, _, err := liveRefIn(ctx, tx, id, branchPrefix+v.ref.DefaultBranch)
	if err != nil {
		return fmt.Errorf("code: read %s's head: %w", name, err)
	}
	// THE COUNTS ARE THE ROW'S, kept by the applier as it moves the refs:
	// counted here they were a walk of every ref the namespace holds,
	// tombstones included, on every detail read.
	summary := v.summary(head)
	summary.Access = g.Access.String()
	out.Repo = &RepoDetail{RepoSummary: summary,
		Branches: v.branches, Tags: v.tags, Tombstones: v.tombstones,
		CreatedAt: store.DecodeTime(v.createdAt), Limits: r.limits()}
	// THE PATH OWNERS ONLY TO A READ THAT ASKS ([ReadQuery.PathOwners]): a
	// refs listing's first page and every resolution of a repository read
	// this detail for its row, and at their bound the path owners are a
	// quarter of a mebibyte of JSON each of them would carry and drop.
	if q.PathOwners {
		if out.Repo.PathOwners, err = pathOwnersIn(ctx, tx, id); err != nil {
			return err
		}
	}
	// AND THE GATES ONLY TO A READ THAT ASKS ([ReadQuery.Gates]), for the
	// path owners' reason: read in the same transaction as the rules the row
	// carries, so every gate names a rule of the row.
	if q.Gates {
		if out.Repo.Gates, err = gatesIn(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// pushes serves one repository's push history, newest first IN LOG ORDER —
// the order the pushes landed in — keyset-paged by (position, push_id): the
// push id because group commit puts up to 64 pushes in one record, sharing its
// position and its instant, so a cursor without it skips or repeats the
// pushes of a record a page boundary splits. Never by the record's instant,
// which is the broker's and steps back across a stream leader's change, so it
// only approximates the order (replicated 0056). The history is the
// repository's contents, so an asker that may not read the repository is
// answered [ErrNotPermitted], and one whose ceiling may not [ErrAboveCeiling],
// as [Reader.repo] answers them.
func (r *Reader) pushes(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	id, found, err := claimIn(ctx, tx, q.Repo)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
	}
	if err = mayReadIn(ctx, tx, id, q.Asker, q.CeilingSeat, chart); err != nil {
		return err
	}
	if q.Push != "" {
		// ONE PUSH, WHOLE, by its key — and only one this repository's
		// history holds: another repository's push, or one past
		// PushHistory, is no row of this history, which is an answer with
		// none.
		row, found, err := pushIn(ctx, tx, q.Push)
		if err != nil {
			return fmt.Errorf("code: read %s's push %s: %w", q.Repo, q.Push, err)
		}
		if found && row.RepoID == id {
			out.Pushes = []PushRow{row}
		}
		return nil
	}
	limit := limitOr(q.Limit, defaultPushLimit)
	var rows *sql.Rows
	if q.Cursor == "" {
		rows, err = tx.QueryContext(ctx, pushesFirstSQL, id, limit)
	} else {
		position, pushID, ok := cutLogCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, pushesAfterSQL, id, position, position, pushID, limit)
	}
	if err != nil {
		return fmt.Errorf("code: read %s's pushes: %w", q.Repo, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		row, commands, err := scanPush(rows)
		if err != nil {
			return fmt.Errorf("code: read %s's pushes: %w", q.Repo, err)
		}
		// A ROW IN BRIEF IS CHARGED THE ROW IT READ — every command and
		// every introduced id, read and decoded whole to be cut and counted —
		// or what it sends where that is the larger: a page held to what it
		// sends alone would read a run of maximal pushes for a few hundred
		// bytes each, and so without bound, the rule a condensed page keeps
		// ([scanIntroduced]). So a brief page READS what a whole one does
		// and sends a fraction of it.
		charge := row.encodedBytes(commands)
		if q.Brief {
			row = row.brief()
			charge = max(charge, row.encodedBytes(row.commandsBytes()))
		}
		out.Pushes = append(out.Pushes, row)
		if pg.add(charge) {
			out.Next = logCursor(row.Position.Packed(), row.PushID)
			break
		}
	}
	return rows.Err()
}

// refsListing serves one page of a repository's LIVE refs under a prefix, by
// name, keyset after the last ref served, to an asker that may read the
// repository, and whose ceiling may ([JudgeRead]) — and on the first page the
// repository in detail too, read as kind repo reads it in the same
// transaction, so what a seat asks first of a repository — what it is, and
// its branches — is one read and one barrier.
//
// OVER THE LIVE REFS' INDEX (code_refs_live_idx, replicated 0054, its
// predicate repeated verbatim), never code_refs' key: a range of the key
// holds every tombstone under the prefix as well, kept RefTombstoneHorizon
// so a delta learns of a deletion — up to MaxTombstonesPerRepo of them, each
// a row the page steps over, with a lookup apiece, before it fills — where
// the partial index holds the live refs alone. The names under a directory
// are, in byte order, every name between dir + "/" and dir + "0", the range
// the push decide asks of the same index.
//
// The detail rides beside the page's rows in the half of the estate reply
// MaxPageBytes leaves free, as a snapshot page's repository row does.
func (r *Reader) refsListing(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	cp statelog.Position, out *ReadAnswer) error {

	prefix := cmp.Or(q.Prefix, branchPrefix)
	after := prefix
	var id string
	if q.Cursor == "" {
		if err := r.repo(ctx, tx, q, chart, cp, out); err != nil {
			return err
		}
		id = out.Repo.ID
	} else {
		if !strings.HasPrefix(q.Cursor, prefix) {
			return fmt.Errorf("%w: %q is not a ref under %s", ErrBadCursor, q.Cursor, prefix)
		}
		var found bool
		var err error
		if id, found, err = claimIn(ctx, tx, q.Repo); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		if err = mayReadIn(ctx, tx, id, q.Asker, q.CeilingSeat, chart); err != nil {
			return err
		}
		after = q.Cursor
	}
	limit := min(limitOr(q.Limit, defaultRefLimit), r.pageRows)
	rows, err := tx.QueryContext(ctx, refsUnderSQL, id, after,
		strings.TrimSuffix(prefix, "/")+"0", limit)
	if err != nil {
		return fmt.Errorf("code: read %s's refs under %s: %w", q.Repo, prefix, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var ref RefRow
		var position int64
		if err := rows.Scan(&ref.Ref, &ref.OID, &position); err != nil {
			return fmt.Errorf("code: read %s's refs under %s: %w", q.Repo, prefix, err)
		}
		ref.Position = unpacked(position)
		out.Refs = append(out.Refs, ref)
		if pg.add(ref.encodedBytes()) {
			out.Next = ref.Ref
			break
		}
	}
	return rows.Err()
}

// introduced serves one page of a repository's pushes in LOG ORDER for a git
// node's index of which push introduced each commit: (After, Through]
// ascending — Through zero meaning this read's own checkpoint — or below
// Before descending, newest first, down to the oldest push the rows keep;
// keyset after the last push served, `<packed position>/<push id>`, since
// group commit puts up to 64 pushes at one position. A page ends at its row
// limit or once its rows reach MaxPageBytes, each counted by what it sends or
// by the commands it read, whichever is more ([scanIntroduced]), and always
// serves one. A repository with no row — purged — is [ErrRepoNotFound], which
// drops the index that asked.
//
// ITS ROW LIMIT IS MaxSnapshotItems, a snapshot page's, and never the history
// listing's MaxPushesPerPage: that is a screen of history a person reads,
// and this is the engine's own read, whose cost its bytes bound — what a
// page sends and what it reads — whatever its count. So the row limit need
// only stop a page of the narrowest pushes about where its bytes would, and
// it does: a seat's deletion of a one-letter branch, with no commit to
// introduce, is 345 bytes sent, so ten thousand of them are 3.45 MB of a
// page's 4 MiB, while a page of pushes of three commits each ends at its
// bytes first, at about 7 500 (internal/code/cache's measured section). Two
// hundred a page would read a monorepo's 18 million pushes of 181 days back
// in 90 000 pages rather than 2 400.
func (r *Reader) introduced(ctx context.Context, tx *sql.Tx, q ReadQuery, cp statelog.Position,
	out *ReadAnswer) error {

	in := q.Introduced
	var name string
	switch err := tx.QueryRowContext(ctx, repoNameSQL, in.RepoID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, in.RepoID)
	case err != nil:
		return fmt.Errorf("code: read repository %s: %w", in.RepoID, err)
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
		return fmt.Errorf("code: read %s's pushes in log order: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		row, cost, err := scanIntroduced(rows)
		if err != nil {
			return fmt.Errorf("code: read %s's pushes in log order: %w", name, err)
		}
		out.Introduced = append(out.Introduced, row)
		if pg.add(cost) {
			out.Next = logCursor(row.Position.Packed(), row.PushID)
			break
		}
	}
	return rows.Err()
}

// runPushes serves one page of a coding run's pushes ([ReadRunPushes]): each
// repository the query names, in its order, and its pushes above the run's
// launch position in LOG ORDER — the position, then the push id, since group
// commit lands up to 64 pushes at one position — answering the rows that name
// the run; keyset after the last push the page READ, `<repository
// index>/<packed position>/<push id>`, whether or not that push was the run's.
// A repository's pushes running out moves the page on to the next
// repository's.
//
// NOT HELD TO A GRANT: the engine reads its own run's record for the seat that
// launched the run, and the run was given only repositories that seat could
// read at its launch. A repository with no rows — purged since the run pushed
// to it — answers no push of the run's, which is what the rows say of it.
//
// A PAGE IS HELD TO WHAT IT READS, NOT ONLY TO WHAT IT ANSWERS. The run's
// pushes are a few among every push its repositories took while it ran —
// other seats', people's, other runs' — and code_pushes_repo_idx carries no
// run (replicated 0070 says why), so finding them reads each of those rows to
// test the run it names: its whole record, the overflow pages of a wide one
// included. Held only to what it answered, a page read every push between
// one of the run's and the next, however many and however wide — a week of
// the large model's busiest repository in one page where the run pushed once
// in it, and 80 MB where 200 pushes of 400 KB stood between two of the run's,
// with nothing about a run bounding its length. So a page READS at most its
// row limit of its repositories' pushes — MaxSnapshotItems unless the query
// asks fewer, an engine read's, as [Reader.introduced]'s, because what bounds
// its cost is its bytes — and stops once those WEIGH MaxPageBytes, each by
// the row_bytes the index carries beside its key, so a row past the page's
// bytes is never looked up at all: always one, so a page always advances, and
// so at most one row past the bound ([pushesWithin]'s rule, the history
// horizon's and a purge part's). What it answers is held to MaxPageBytes
// apart, each push it keeps by what it sends. A page that reads its bound
// ends there whether or not it kept a push — none, and a cursor, which the
// engine reads on past — so no request costs more than a page's reads,
// whatever the run's length and whoever else pushed beside it.
//
// THE WINDOW IS WEIGHED WHERE IT LIES ([runSpanOf]): what the pushes the page
// has rows left for weigh, summed by one aggregate of the index's widths, and
// the last one's place read off the same range — both covering seeks, which
// look up no row and hand Go none — and only where they weigh more than the
// page's bytes left are their weights walked, to the one that fills it. Then
// the run's own rows in exactly that window: one ranged seek of the same
// index, each row it reaches looked up to test the run. Weighing every push
// in Go instead, each handed across the driver with its place and its weight,
// made a week's read five times as long (2.5 s).
//
// MEASURED (BenchmarkRunPushes, one run of each arm on two E-cores of a Core
// Ultra 7 265U): an hour of a run in the large model's busiest repository, 70
// pushes a minute, reads its 4 200 pushes in one page of 4.0 ms; a week of it,
// 705 600 pushes, in 71 pages of at most 9.9 ms, 0.49 s in all — where, held
// only to what it answered, one page had read the whole week wherever the run
// pushed once in it; that hour with 200 of the others' pushes at 403 KB, 80 MB
// of rows, in 19 pages of at most 4.3 ms; MaxRunRepos such repositories' hour,
// 33 600 pushes, in 4 pages of at most 7.8 ms; and a quiet repository's hour in
// 0.8 ms. Every page is under a tenth of statelog.ReadBudget and the week's
// read under twice it — the rule the benchmark decides by — so a page's row
// bound stays an engine read's MaxSnapshotItems.
func (r *Reader) runPushes(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	in := q.RunPushes
	after := in.After.Packed()
	// A PAGE READS AFTER A PLACE: its cursor's push, or — on the first page
	// of a repository — (after+1, ""), the place before every push at a
	// position past the launch, since positions are whole numbers and no
	// push's id is empty; so one statement of each kind serves both.
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
		return false, fmt.Errorf("code: weigh the pushes to %s a page of run %s's may read: %w", repo, in.Run,
			err)
	}
	if span.pushes == 0 {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, runPushesInSQL, repo, in.Run, position, pushID, span.lastPosition,
		span.lastID)
	if err != nil {
		return false, fmt.Errorf("code: read run %s's pushes to %s: %w", in.Run, repo, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, commands, err := scanPush(rows)
		if err != nil {
			return false, fmt.Errorf("code: read run %s's pushes to %s: %w", in.Run, repo, err)
		}
		out.Pushes = append(out.Pushes, row)
		if *sent += row.encodedBytes(commands); *sent >= reads.maxB {
			out.Next = runPushCursor(i, row.Position.Packed(), row.PushID)
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("code: read run %s's pushes to %s: %w", in.Run, repo, err)
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
// archived repositories holding at least q.MinPacks live packs, MOST LIVE PACKS
// FIRST and then by id, each with the version its row holds — one ranged walk
// of code_repos_packs_idx (replicated 0060) from its top, keyset on (live
// packs, id) after a cursor, the index's partial predicate repeated verbatim
// so the planner may use it at all.
//
// MOST FIRST, because a compactor takes them in this order: a repository near
// MaxPacksPerRepo, whose pushes are about to be refused compaction_behind, must
// never wait behind a thousand barely due. ACTIVE AND ARCHIVED, because an
// archived repository is still read, and every cold sync of it pays a GET for
// every pack it holds. THE VERSION, because it is what a compactor keys its
// memory of a plan found not due by, so a repository whose row has not moved
// since costs it a lookup, never a sync.
//
// A FULL LISTING, never a delta from the last poll's: a delta would need an
// index on code_repos.version, which every record about a repository rewrites,
// where this index moves only with a repository's live packs.
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
		return fmt.Errorf("code: list the repositories a compaction may be due in: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c       CompactionCandidate
			version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.LivePacks, &c.LiveObjects, &version); err != nil {
			return fmt.Errorf("code: list the repositories a compaction may be due in: %w", err)
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
// archived repositories whose live packs are past their published ones —
// every one holding a live pack no published layer covers — LEAST RECENTLY
// PUBLISHED FIRST, one never published before every other, then by id, each
// with what a publication's due rule reads and the version its row was read
// at.
//
// THE PUBLISHED COUNTS ARE THE LAST PUBLICATION'S RECOUNT, and a push moves
// only the live ones: a pack pushed since that is byte-identical to a
// published one — its git checksum one a layer names — counts in the tail
// until the next publication recounts it, so the listing can name a
// repository with nothing to publish — which only a plan over its packs can
// tell, answering it not due ([PublicationCandidate]; replicated 0062 says
// why no push recounts).
//
// ONE WALK of code_repos_unpublished_idx (replicated 0062), its partial
// predicate repeated verbatim, which holds exactly the repositories it lists:
// a repository whose every pack its last recount found covered, and none
// added since, costs the listing nothing, however many there are. LEAST
// RECENTLY PUBLISHED FIRST, so the repository that has waited longest is
// planned first, and a page cut at MaxSnapshotItems ends at the most recently
// published. A FULL LISTING, never a delta from the last poll's, for the
// compaction's listing's reason.
//
// AND NONE WITH AN EXPUNGE OPEN: rule 6g refuses every publication between an
// expunge's begin and its commit (publication.go), so a repository mid-expunge
// is left out — however many of its packs no layer covers, the kept packs its
// begin wrote among them, since a begin that drops layers recounts the
// published ones down — and listed again once its commit lands. Listed, it
// would have its publisher plan and build a layer for the committer to refuse
// stale, at every poll until the commit: where the begin dropped the chain's
// root, a whole base layer with its bitmap, the costliest build a git node
// makes. A residual predicate on the same walk, over the row the listing reads
// for its columns anyway: the expunges open are a handful in any fleet.
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
		return fmt.Errorf("code: list the repositories a publication may be due in: %w", err)
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
			return fmt.Errorf("code: list the repositories a publication may be due in: %w", err)
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

// expungeDue serves one page of an `expunge_due` read: the active and archived
// repositories with an expunge open or a purge requested and not served, BY
// ID, each with the expunge, the request and its position, when the
// repository was last expunged, its live packs, bytes and objects, and the
// version its row was read at.
//
// ONE WALK of code_repos_expunging_idx (replicated 0064), its partial
// predicate repeated verbatim, which holds exactly the repositories with an
// expunge open or a request waiting — a handful in a fleet of any size, every
// one of which its maintainer finishes or serves before it plans anything
// else — keyset on the id after a cursor. A deleted or purging repository's
// stays out: its open expunge waits for its restore, or goes with its purge.
// A FULL LISTING, never a delta from the last poll's, for the compaction's
// listing's reason.
func (r *Reader) expungeDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	// THE CURSOR IS THE LAST ID SERVED, and "" before the first: every id
	// is a UUID, past "" in byte order, so one statement serves every page.
	if q.Cursor != "" && validRepoID("cursor", q.Cursor) != nil {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, expungeDueSQL, q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("code: list the repositories an expunge is owed in: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c                            ExpungeCandidate
			requested, expunged, version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Open, &c.Request, &requested, &expunged, &c.LivePacks,
			&c.LiveBytes, &c.LiveObjects, &version); err != nil {
			return fmt.Errorf("code: list the repositories an expunge is owed in: %w", err)
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
			out.Next = c.ID
			break
		}
	}
	return rows.Err()
}

// expungeTips serves one page of an `expunge_tips` read ([ReadExpungeTips]):
// the object each of the repository's ref drops left whose record's broker
// instant is at or after the query's — ONE SEEK of code_ref_drops' key
// (replicated 0061 led it with the instant for this read), a covering one, in
// the key's order, keyset after the last drop the page read. Each object is
// answered once a page: one force-pushed away again and again is one tip
// however many drops name it, and a later page naming it again costs its
// reader a map lookup. A repository with no row — purged — is
// [ErrRepoNotFound].
//
// NEVER HELD TO A GRANT: the engine reads what its own expunge keeps, and the
// objects leave this node only to the maintainer that keeps them.
func (r *Reader) expungeTips(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	in := q.ExpungeTips
	var name string
	switch err := tx.QueryRowContext(ctx, repoNameSQL, in.RepoID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, in.RepoID)
	case err != nil:
		return fmt.Errorf("code: read repository %s: %w", in.RepoID, err)
	}
	since := store.EncodeTime(in.Since)
	var (
		rows *sql.Rows
		err  error
	)
	switch c, ok := cutTipsCursor(q.Cursor); {
	case q.Cursor != "" && (!ok || c.at < since):
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, expungeTipsSQL, in.RepoID, since, r.pageRows)
	default:
		rows, err = tx.QueryContext(ctx, expungeTipsAfterSQL, in.RepoID, c.at, c.position, c.ref, c.old,
			r.pageRows)
	}
	if err != nil {
		return fmt.Errorf("code: read %s's ref drops: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	answered := map[string]bool{}
	for rows.Next() {
		var c tipsCursor
		if err := rows.Scan(&c.at, &c.position, &c.ref, &c.old); err != nil {
			return fmt.Errorf("code: read %s's ref drops: %w", name, err)
		}
		// A ROW READ COUNTS whether or not its object was answered already:
		// the page is held to what it reads as well as to what it sends.
		size := 0
		if !answered[c.old] {
			answered[c.old] = true
			out.Tips = append(out.Tips, c.old)
			size = stringBytes(c.old) + 1
		}
		if pg.add(size) {
			out.Next = c.String()
			break
		}
	}
	return rows.Err()
}

// tipsCursor is one ref drop's place in its key's order — the instant as the
// column stores it, the position, the ref and the object — and an
// `expunge_tips` page's cursor, "<at>:<position>:<old>:<ref>": the ref last,
// since a ref may hold every character but a colon (git's ref rules), which
// the integers and the object id hold none of.
type tipsCursor struct {
	at, position int64
	ref, old     string
}

// String is c as a cursor.
func (c tipsCursor) String() string {
	return strconv.FormatInt(c.at, 10) + ":" + strconv.FormatInt(c.position, 10) + ":" + c.old + ":" + c.ref
}

// cutTipsCursor reads a [tipsCursor].
func cutTipsCursor(s string) (tipsCursor, bool) {
	parts := strings.SplitN(s, ":", 4)
	if len(parts) != 4 || parts[2] == "" || parts[3] == "" {
		return tipsCursor{}, false
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return tipsCursor{}, false
	}
	position, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || position < 0 {
		return tipsCursor{}, false
	}
	return tipsCursor{at: at, position: position, old: parts[2], ref: parts[3]}, true
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

// ResolveRef answers the refs of one repository a query names, at the level it
// names, to an asker that may read the repository — in ONE transaction: the
// name's claim; the repository, a deleted or purging one answered
// [ErrRepoNotFound] in [RepoGone]'s sentence for its state, which the code
// host's admission says too; the asker's grant ([ErrNotPermitted]) and its
// ceiling's ([ErrAboveCeiling], [JudgeRead]); then each ref — a whole ref as
// itself, or a short name as a branch and then as a tag — with the position
// of the record that last moved it ([ResolvedRef.Moved]), its row's own, read
// in the same lookup. A deleted ref is absent, and a name none of its refs
// holds is answered so ([ResolvedRef.Missing]) beside the names that
// resolved, never failing them: whether a call can go on without it is its
// caller's to say. Its scope is the name's, as [Reader.Resolve]'s is.
func (r *Reader) ResolveRef(ctx context.Context, q RefQuery) (RefAnswer, error) {
	if err := checkFreshness(q.Fresh); err != nil {
		return RefAnswer{}, err
	}
	if !ValidName(q.Repo) {
		return RefAnswer{}, fmt.Errorf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
	}
	if why := q.Asker.invalid(); why != "" {
		return RefAnswer{}, fmt.Errorf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return RefAnswer{}, fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	// EVERY NAME IS CHECKED BEFORE ANY READ: a query naming one no ref can
	// have is the asker's mistake, refused whole.
	switch n := len(q.Refs); {
	case n == 0:
		return RefAnswer{}, fmt.Errorf("%w: refs: a resolve names at least one ref", ErrInvalid)
	case n > MaxRefsPerQuery:
		return RefAnswer{}, fmt.Errorf("%w: refs: %d refs, past the %d one resolve reads", ErrInvalid, n,
			MaxRefsPerQuery)
	}
	tried := make([][]string, len(q.Refs))
	for i, ref := range q.Refs {
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
	_, err := r.log.Read(ctx, q.Fresh.Resolved(resolver(q.Repo), false), func(tx *sql.Tx) error {
		out = RefAnswer{}
		cp, err := checkpointIn(ctx, tx)
		if err != nil {
			return err
		}
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
			resolved, err := firstLiveIn(ctx, tx, id, names)
			if err != nil {
				return fmt.Errorf("code: read %s of %s: %w", q.Refs[i], q.Repo, err)
			}
			resolved.Name = q.Refs[i]
			if resolved.Ref == "" {
				resolved.Missing = fmt.Sprintf("repository %s holds no live %s", q.Repo, strings.Join(names, " or "))
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
		oid, moved, found, err := liveRefIn(ctx, tx, id, name)
		switch {
		case err != nil:
			return ResolvedRef{}, err
		case found:
			return ResolvedRef{Ref: name, OID: oid, Moved: moved}, nil
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
			return nil, fmt.Errorf("%w: ref: %q is not a ref", ErrInvalid, ref)
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
		return nil, fmt.Errorf("%w: ref: %q is no ref, branch or tag name", ErrInvalid, ref)
	}
	return names, nil
}

// liveRefIn is repository id's live ref by its whole name, read in tx: the
// object it names, and the position of the record that last moved it — its
// row's, which every move of the ref writes (apply_push.go) — or false where
// no live ref has the name.
func liveRefIn(ctx context.Context, tx *sql.Tx, id, ref string) (string, statelog.Position, bool, error) {
	var (
		oid   string
		moved int64
	)
	switch err := tx.QueryRowContext(ctx, liveRefSQL, id, ref).Scan(&oid, &moved); {
	case errors.Is(err, sql.ErrNoRows):
		return "", statelog.Position{}, false, nil
	case err != nil:
		return "", statelog.Position{}, false, err
	}
	return oid, unpacked(moved), true, nil
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
		return RepoRef{}, fmt.Errorf("code: read repository %s's grant: %w", id, err)
	}
	ref.Status, ref.Owner.Kind, ref.Visibility = RepoStatus(status), OwnerKind(owners), Visibility(visibility)
	return ref, nil
}

// logCursor is "<packed position>/<id>": a row's place in log order — a
// push's, by the position of the record that carried it and its push id, the
// keyset of the push listing and of the `introduced` read alike; and a
// change's, by the position of the record that opened it and its id, the
// keyset of a listing of changes across repositories. The id is part of the
// place because one record holds several of either: up to 64 pushes, each
// opening a change of its own.
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

// lifePage is how many repositories one page of [Reader.WalkLives] reads: a
// thousand rows of four short columns — an id, a name of at most 63 bytes,
// two words — about 150 KB, one statement seeking the name index, so the ten
// thousand repositories a document may declare are ten statements, each
// holding one connection of the estate's reader pool for a few milliseconds
// rather than the whole walk.
const lifePage = 1_000

// WalkLives hands visit every repository a reconcile of code.repos acts on —
// active, archived or deleted; a purge's is the code-retention duty's — in
// name order, a page of [lifePage] at a time, each page one STALE read of
// this node's rows. A visit's error ends the walk with it.
//
// THE RECONCILE'S OWN READ, because the listing a surface reads is the wrong
// shape for it: [Reader.Read]'s repositories carry the head (a join of
// code_refs a row), every protection rule of the page, the description and
// the counters, all of them measured against the page's byte budget — 529 ms
// and 51 MB allocated over ten thousand repositories of one short rule, and
// 1.97 s and 352 MB over two thousand at validation's maxima, where a reconcile
// reads an id, a name, a status and a manager. STALE IS ENOUGH: every
// decision the reconcile takes from it is taken again inside its own snapshot
// after the activation's fence, so a row the walk missed is caught at the
// next apply and one it showed wrongly is decided away.
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
			return fmt.Errorf("code: list the repositories a reconcile acts on: %w", err)
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

// dueRepo is one repository the code-retention pass steps: what the pass
// needs of it and nothing more — its id to step, its name to report and hand
// on — and where it sits in the listing's order.
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

// due lists one page of what the code-retention pass steps, in the order it
// steps them: every PURGING repository first, whatever the retention — a
// purge once begun is always finished, since its rows are partly gone and
// nothing restores it — and then every DELETED repository whose deletion is
// at or before the cutoff; each phase oldest deletion first, and the page
// resuming after q.after.
//
// A STALE READ of this node's rows, in one transaction, of the three columns
// the pass uses: every step decides again in its own snapshot, so a
// repository that moved on since the listing is answered there. Each phase
// is ONE statement over its own partial index (replicated 0052), so a page
// reads the rows it lists — never the repositories deleted inside the
// retention, nor a repository's settings and rules the pass has no use for:
// read whole, a listing of ten thousand due repositories was two statements
// a repository and every protection rule each holds, re-read at every pass
// however few of them the pass could step.
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
				return fmt.Errorf("code: read the repositories purging: %w", err)
			}
			if len(out) == q.limit {
				return nil
			}
			after, afterID = math.MinInt64, ""
		}
		var err error
		if out, err = dueIn(ctx, tx, out, false, dueDeletedSQL, store.EncodeTime(q.cutoff),
			after, after, afterID, q.limit-len(out)); err != nil {
			return fmt.Errorf("code: read the repositories deleted past the retention: %w", err)
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

// purgedAt reports whether this node's rows hold repository id PURGED — its
// row gone and its deletion marker written — read at the session floor at,
// the position of the purge's last record: a node that has not applied it
// waits for it, for the read's budget, and refuses `behind` past that rather
// than answering from before it. False is a definite answer: at that
// position this node's copy still holds the repository's row, so the record
// there removed nothing here.
func (r *Reader) purgedAt(ctx context.Context, id string, at statelog.Position) (bool, error) {
	if at.IsZero() {
		return false, fmt.Errorf("%w: at: a purge is confirmed at its last record's position", ErrInvalid)
	}
	var purged bool
	query := statelog.FloorAt(statelog.ReadSession, at).Query(
		statelog.ScopeSet{Paths: []string{RepoPath(id)}}, false)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		var rows, marked int
		if err := tx.QueryRowContext(ctx, purgeStateSQL, id, id).Scan(&rows, &marked); err != nil {
			return fmt.Errorf("code: read whether %s is purged: %w", id, err)
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
		default_branch, description, deleted_at, last_activity_at, refs, branches, tags, tombstones, live_packs,
		live_bytes, live_objects, drop_epoch, tombstones_through, retired_through, layers_position, pushes,
		last_push_at, created_at FROM code_repos WHERE id = ?`
	packColumns       = `pack_id, part, object, hash, size, index_bytes, reverse_bytes, git_checksum, objects, kind, state, added_position, content_position`
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
	// ?2, at most ?3 deep over the index the retired phase reads: what page
	// 1's choice knows of the retirements a full answer would serve above its
	// caller's RetiredSince and a delta carries above its floor, counted once
	// a floor (choose).
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
		work_backend, work_key, model, run_id, coding_agent, node, atomic, commands, pack_id, pack_bytes,
		introduced, introduced_count, received_at`
	pushSQL        = `SELECT ` + pushColumns + ` FROM code_pushes WHERE push_id = ?`
	pushesFirstSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		ORDER BY position DESC, push_id DESC LIMIT ?`
	pushesAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position <= ? AND (position < ? OR push_id < ?)
		ORDER BY position DESC, push_id DESC LIMIT ?`
	reposSQL = `SELECT r.id, r.name, r.status, r.managed_by, r.kind, r.owner_kind, r.owner, r.visibility,
		r.search, r.default_branch, r.description, r.deleted_at, r.last_activity_at, r.refs, r.live_packs,
		r.live_bytes, r.live_objects, r.drop_epoch, r.pushes, r.last_push_at, h.target
		FROM code_repos r LEFT JOIN code_refs h INDEXED BY ` + refsKey + `
			ON h.repo_id = r.id AND h.ref = 'refs/heads/' || r.default_branch AND h.target != ''
		WHERE r.status IN (SELECT value FROM json_each(?)) AND r.name > ?
		ORDER BY r.name LIMIT ?`
	// pushRulesOfSQL is a listing page's rules as a push is judged by them,
	// in one statement ([protectionsOf]) — pushRulesSQL for every repository
	// the page keeps, never selecting the gate.
	pushRulesOfSQL = `SELECT repo_id, pattern, allow_force, allow_delete, direct_push
		FROM code_protections WHERE repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
		ORDER BY repo_id, pattern`
	// repoGrantSQL is what a grant reads of one repository's row, and its
	// status ([grantRefIn]).
	repoGrantSQL = `SELECT name, status, owner_kind, owner, visibility FROM code_repos WHERE id = ?`
	// liveRefSQL is one live ref's target and the position it last moved at,
	// by its whole name: a repository's head, and a ref a RefQuery names
	// (liveRefIn).
	liveRefSQL = `SELECT target, position FROM code_refs INDEXED BY ` + refsKey + `
		WHERE repo_id = ? AND ref = ? AND target != ''`
	// refsUnderSQL is a page of the live refs between two names, by name —
	// over the live refs' partial index, its predicate verbatim
	// (refsListing).
	refsUnderSQL = `SELECT ref, target, position FROM code_refs INDEXED BY ` + liveRefsKey + `
		WHERE repo_id = ? AND ref > ? AND ref < ? AND target != '' ORDER BY ref LIMIT ?`
	// THE PUSHES IN LOG ORDER, over code_pushes_repo_idx as replicated 0056
	// orders it: those in (?2, ?3] oldest first — an `introduced` read
	// ascending and a delta's pushes phase — first and after a cursor
	// (?3 the upper bound, ?4 and ?5 the cursor), and those below ?2 newest
	// first, whose keyset page is the history listing's own pushesAfterSQL.
	pushesAboveSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position > ? AND position <= ? ORDER BY position, push_id LIMIT ?`
	pushesAboveAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position >= ? AND position <= ? AND (position > ? OR push_id > ?)
		ORDER BY position, push_id LIMIT ?`
	pushesBelowSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position < ? ORDER BY position DESC, push_id DESC LIMIT ?`
	// A CODING RUN'S PUSHES TO ONE REPOSITORY, over code_pushes_repo_idx in
	// log order, each after a PLACE — ?2 a position and ?3 a push id, the >=
	// beside the OR keeping the range seek (runPushes says what the place of
	// a repository's first page is). The WINDOW a page may read, weighed:
	// the next ?4 pushes' count and what their rows weigh (runWeighSQL), the
	// weights one by one where those fill the page (runWeightsSQL), and the
	// place of the window's last push, ?4 pushes past its first
	// (runLastSQL) — all of it in the index, so each is a covering seek that
	// looks up no row. Then the RUN'S OWN in exactly that window
	// (runPushesInSQL): after the place ?3, ?4 and up to its last push ?5, ?6
	// inclusive, each row the seek reaches looked up to test the run (?2),
	// which the index does not carry.
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
	// predicate repeated verbatim, and keyset on (deleted_at, id): the >=
	// beside the OR is what keeps the index's range seek, which a bare OR of
	// the two terms would give up.
	livesSQL = `SELECT id, name, status, managed_by FROM code_repos
		WHERE name > ? AND status IN ('active', 'archived', 'deleted')
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
	// THE COMPACTOR'S DUE LISTING, most live packs first and then by id, over
	// code_repos_packs_idx walked from its top — its partial predicate
	// repeated verbatim — and after a cursor with the <= beside the OR that
	// keeps the range seek (planner.go).
	compactionDueFirstSQL = `SELECT id, name, live_packs, live_objects, version FROM code_repos
		INDEXED BY code_repos_packs_idx
		WHERE status IN ('active', 'archived') AND live_packs >= ?
		ORDER BY live_packs DESC, id DESC LIMIT ?`
	compactionDueAfterSQL = `SELECT id, name, live_packs, live_objects, version FROM code_repos
		INDEXED BY code_repos_packs_idx
		WHERE status IN ('active', 'archived') AND live_packs >= ? AND live_packs <= ?
			AND (live_packs < ? OR id < ?)
		ORDER BY live_packs DESC, id DESC LIMIT ?`
	// THE PUBLISHER'S DUE LISTING, least recently published first and then by
	// id, over code_repos_unpublished_idx — its partial predicate repeated
	// verbatim — from the index's start, the column's floor (0, a repository
	// never published: no row holds less), and after a cursor with the >=
	// beside the OR that keeps the range seek (planner.go). No repository with
	// an expunge open, whose publication rule 6g refuses: a residual predicate
	// on the same walk, over the row the listing reads anyway.
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
	expungeDueSQL = `SELECT id, name, expunge_id, expunge_request, expunge_request_position, expunged_at,
		live_packs, live_bytes, live_objects, version FROM code_repos INDEXED BY code_repos_expunging_idx
		WHERE (expunge_id != '' OR expunge_request != '') AND status IN ('active', 'archived') AND id > ?
		ORDER BY id LIMIT ?`
	// AN EXPUNGE'S TIPS: a repository's ref drops from an instant, in the
	// order of code_ref_drops' key, which leads with the instant — the first
	// page from the instant, a later one after the last drop read.
	expungeTipsSQL = `SELECT at, position, ref, old FROM code_ref_drops INDEXED BY ` + dropsKey + `
		WHERE repo_id = ? AND at >= ? ORDER BY at, position, ref, old LIMIT ?`
	expungeTipsAfterSQL = `SELECT at, position, ref, old FROM code_ref_drops INDEXED BY ` + dropsKey + `
		WHERE repo_id = ?1 AND at >= ?2
		AND (at > ?2 OR position > ?3 OR (position = ?3 AND (ref > ?4 OR (ref = ?4 AND old > ?5))))
		ORDER BY at, position, ref, old LIMIT ?6`
	// A SNAPSHOT'S LAYERS PHASE: every part of a repository's chain, in its
	// key's order, joined to its layer (layersOfSQL) in Go.
	layerPartsOfSQL = `SELECT layer_id, part, object, hash, size FROM code_layer_parts
		WHERE repo_id = ? ORDER BY layer_id, part`
)
