package code

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

// chartFor is the roster an asker is judged by: the serving node's current
// chart for a seat, read ONCE a read so every row of a listing is judged by
// one chart, and none for anybody else.
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
func (r *Reader) chartFor(asker Principal) (Roster, error) {
	if asker.Kind != PrincipalAgent {
		return nil, nil
	}
	var chart Roster
	if r.roster != nil {
		chart = r.roster()
	}
	if chart == nil {
		return nil, fmt.Errorf("%w: this node holds no chart to judge a seat's read by", ErrNotServing)
	}
	return chart, nil
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

// pointLevel refuses a freshness a point read cannot serve: an EMPTY level —
// a code read states its freshness, because a cache's sync and a git
// request each need a specific one — and a session read naming no position,
// which would wait for nothing.
func pointLevel(f statelog.Freshness) error {
	switch {
	case f.Level == "":
		return fmt.Errorf("%w: a code read states its freshness", ErrBadLevel)
	case !f.Level.Valid():
		return fmt.Errorf("%w: %q is not a read level", ErrBadLevel, f.Level)
	case f.Level == statelog.ReadSession && f.MinPosition.IsZero():
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

// repoViewIn reads one repository and its rules in tx, at the checkpoint cp.
func repoViewIn(ctx context.Context, tx *sql.Tx, id string, cp statelog.Position) (repoView, bool, error) {
	var (
		v                                   repoView
		status, managed, owners, visibility string
		deleted                             sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, repoViewSQL, id).Scan(&v.ref.ID, &v.ref.Name, &status, &managed,
		&owners, &v.ref.Owner.Ref, &visibility, &v.ref.DefaultBranch, &v.ref.Description, &deleted, &v.ref.Refs,
		&v.branches, &v.tags, &v.tombstones, &v.ref.LivePacks, &v.ref.LiveBytes, &v.ref.LiveObjects,
		&v.tombstonesThrough, &v.retiredThrough, &v.pushes, &v.lastPushAt, &v.createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoView{}, false, nil
	case err != nil:
		return repoView{}, false, fmt.Errorf("code: read repository %s: %w", id, err)
	}
	v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
	v.ref.Visibility = Visibility(visibility)
	if deleted.Valid {
		v.ref.DeletedAt = store.DecodeTime(deleted.Int64)
	}
	if v.ref.Protections, err = protectionsIn(ctx, tx, id); err != nil {
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
func (r *Reader) Resolve(ctx context.Context, q ResolveQuery) (RepoRef, error) {
	if err := pointLevel(q.Fresh); err != nil {
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
		id, found, err := claimIn(ctx, tx, q.Name)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Name)
		}
		v, found, err := repoViewIn(ctx, tx, id, cp.At)
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
// decides which: a delta only from a position on this generation of the log,
// at or past the repository's tombstone mark and its retired mark, and
// carrying no more rows than the full answer ([deltaOutweighs]).
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
// full answer serves them only when it can serve EVERY one ([retiredFloor]),
// and RetiredFrom says whether it did.
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
//     a delta no tombstone above its floor swept, and no retirement above
//     the retired phase's floor swept. Any failure is [ErrSnapshotMoved], and
//     the consumer reads the snapshot again from its first page, which
//     decides full or delta afresh.
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
		if err := pointLevel(q.Fresh); err != nil {
			return RepoSnapshot{}, err
		}
		query = q.Fresh.Query(scope, false)
	} else {
		if err := checkCursor(q.Cursor); err != nil {
			return RepoSnapshot{}, err
		}
		query = statelog.Freshness{
			Level: statelog.ReadSession, MinPosition: q.Cursor.Seen,
		}.Query(scope, false)
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
			// BELOW EITHER MARK A DELTA IS INCOMPLETE: a sweep removed the
			// tombstones that told of the deletions at or below the one, and
			// the retired rows that told of the retirements at or below the
			// other.
			full := q.Since.IsZero() || q.Since.Stream != topics.CodeLogStream ||
				q.Since.Generation != cp.At.Generation || q.Since.Packed() < v.tombstonesThrough ||
				q.Since.Packed() < v.retiredThrough
			if !full {
				if full, err = deltaOutweighs(ctx, tx, q.RepoID, q.Since.Packed(), v); err != nil {
					return err
				}
			}
			cur = StateCursor{Full: full, Phase: phasePacks, From: cp.At, Seen: cp.At, Stream: cp.KeyedTo}
			if !full {
				cur.Since, cur.Pushes, cur.RetiredFrom = q.Since, q.Pushes, q.Since
			} else if cur.RetiredFrom, err = retiredFloor(ctx, tx, q.RepoID, q.RetiredSince, cp.At, v); err != nil {
				return err
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
			}
			cur.Seen = cp.At
		}
		out.Repo, out.Position, out.StreamCreated, out.Full = v.ref, cur.From, cur.Stream, cur.Full
		out.RetiredFrom = cur.RetiredFrom
		next, err := r.fill(ctx, tx, q.RepoID, &out, cur, first)
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

// deltaOutweighs reports whether a delta from since would carry more rows than
// the full answer it stands in for: then the full answer is served, which is
// always correct and here the smaller.
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
// tombstones were bounded apart: a full snapshot of a full repository.
//
// # Every row each answer carries, packs included
//
// A delta's packs are those added above its floor and a full answer's every
// pack the repository holds, so the packs only ever favour the delta: a
// repository of many packs keeps its delta past a clean-up a repository of
// few would not, which a comparison of the refs alone would answer in full
// for a fraction of the rows. The full answer's size is the repository's own
// counters, kept as the rows move (live_packs is every pack row: a retired
// pack leaves code_packs for code_packs_retired); the delta's is COUNTED, each
// table at most one row past the full answer deep, over the index the delta's
// pages read (code_refs_changed_idx, code_packs_added_idx,
// code_packs_retired_repo_idx) — so the usual delta after a push reads a
// handful of index entries, and the count never reads more of any index than
// the larger answer would. A tie is a delta: a cache applies a delta in
// place, where a full answer sweeps its directory to the snapshot's image.
//
// # Retirements count on the delta's side alone
//
// A delta carries every pack retired above its floor; the full answer it
// stands in for carries none of its own — what it serves of them is its
// caller's RetiredSince's, and only while they fit beside its rows
// ([retiredFloor]). So they are counted with the delta's other rows and
// never with the full answer's: a cache two thousand compactions behind reads
// the fifty packs its repository holds rather than twenty thousand
// retirements, and gives up only telling a retired pack file from one no row
// accounts for.
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
// writes as themselves, 1 171 bytes a pack for names it spells longest
// ([MaxTombstonesPerRepo] gives the arithmetic). Weighing each pack as a
// fraction of the widest ref row would close that, and answer a repository of
// many packs in full for every delta of ordinary names, whose tombstones are
// narrower than a pack row rather than wider.
func deltaOutweighs(ctx context.Context, tx *sql.Tx, repoID string, since int64, v repoView) (bool, error) {
	whole := v.ref.Refs + v.ref.LivePacks
	var delta int64
	if err := tx.QueryRowContext(ctx, deltaRowsSQL, repoID, since, whole+1).Scan(&delta); err != nil {
		return false, fmt.Errorf("code: count %s's delta: %w", repoID, err)
	}
	return delta > whole, nil
}

// retiredFloor is the floor a FULL answer serves retirements above: since —
// the caller's StateQuery.RetiredSince — where the answer can serve every
// retirement above it, else zero, which serves none. All three must hold:
//
//   - since is a position on this generation of the code log, the test a
//     delta's Since passes, because a position from another number space
//     compares as nothing in this one;
//   - it is at or past the repository's retired mark, because below the mark
//     a sweep removed rows that told of retirements above since;
//   - the retirements above it are no more than the full answer's own rows,
//     counted by the statement that counts them for a delta, one row past
//     that deep — the bound [deltaOutweighs] holds a delta's to, so serving
//     them at most doubles the answer.
//
// Short of any of them the full answer serves no retirement rather than some,
// so a RetiredFrom it answers always names a floor its retirements are
// COMPLETE above: a cache handed none treats every file no row names as one
// no row accounts for, which is correct and only dearer.
func retiredFloor(ctx context.Context, tx *sql.Tx, repoID string, since, at statelog.Position,
	v repoView) (statelog.Position, error) {

	if since.IsZero() || since.Stream != topics.CodeLogStream || since.Generation != at.Generation ||
		since.Packed() < v.retiredThrough {
		return statelog.Position{}, nil
	}
	whole := v.ref.Refs + v.ref.LivePacks
	var retired int64
	if err := tx.QueryRowContext(ctx, retiredAboveSQL, repoID, since.Packed(), whole+1).Scan(&retired); err != nil {
		return statelog.Position{}, fmt.Errorf("code: count %s's retirements above %s: %w", repoID, since, err)
	}
	if retired > whole {
		return statelog.Position{}, nil
	}
	return since, nil
}

// fill serves the snapshot's phases from cur, in order, until the page is
// full or the last phase completes, and answers where the next page resumes —
// nil when the snapshot is complete.
func (r *Reader) fill(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	cur StateCursor, first bool) (*StateCursor, error) {

	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
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
			p            PackRow
			object, hash string
			added        int64
		)
		if err := rows.Scan(&p.PackID, &p.Part, &object, &hash, &p.Size, &p.IndexBytes,
			&p.ReverseBytes, &p.GitChecksum, &p.Objects, &p.Kind, &p.State, &added); err != nil {
			return false, fmt.Errorf("code: read a pack: %w", err)
		}
		k, err := objstore.ParseKey(object)
		if err != nil {
			return false, fmt.Errorf("code: pack %s names %q: %w", p.PackID, object, err)
		}
		p.Object, p.Hash, p.Added = k, objstore.Hash(hash), unpacked(added)
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
func (r *Reader) pushesPhase(ctx context.Context, tx *sql.Tx, repoID string, out *RepoSnapshot,
	cur *StateCursor, pg *page) (bool, error) {

	var rows *sql.Rows
	var err error
	if cur.After == "" {
		rows, err = tx.QueryContext(ctx, pushesAboveSQL, repoID, cur.Since.Packed(), cur.From.Packed(),
			pg.left())
	} else {
		position, pushID, ok := cutPushCursor(cur.After)
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
		row, _, err := scanPush(rows)
		if err != nil {
			return false, fmt.Errorf("code: read %s's pushes since %s: %w", repoID, cur.Since, err)
		}
		in := introducedOf(row)
		out.Pushes = append(out.Pushes, in)
		served++
		cur.After = pushCursor(row.Position, row.PushID)
		if pg.add(in.encodedBytes()) {
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
	case c.Phase != phasePacks && c.Phase != phaseRefs && c.Phase != phasePushes && c.Phase != phaseRetired &&
		c.Phase != phaseTail:
		return bad(fmt.Sprintf("phase %q", c.Phase))
	case c.From.Stream != topics.CodeLogStream || c.From.IsZero():
		return bad("it names no position on the code log")
	case c.Seen.Stream != c.From.Stream || c.Seen.Packed() < c.From.Packed():
		return bad("it was read below where it began")
	case c.Stream.IsZero():
		return bad("it names no stream")
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
		if _, _, ok := cutPushCursor(c.After); !ok {
			return bad("after " + c.After)
		}
	}
	return nil
}

// PushOutcome answers what became of one push, at session floored at the
// record's own position: "not found" is final only at or past it, so a node
// that cannot reach it refuses `behind` rather than answering early.
func (r *Reader) PushOutcome(ctx context.Context, q OutcomeQuery) (PushOutcome, error) {
	switch {
	case q.At.IsZero():
		return PushOutcome{}, fmt.Errorf("%w: at: a push's outcome is read at its record's position", ErrInvalid)
	case q.RepoID == "" || q.PushID == "":
		return PushOutcome{}, fmt.Errorf("%w: a push's outcome names its repository and its id", ErrInvalid)
	}
	query := statelog.Freshness{Level: statelog.ReadSession, MinPosition: q.At}.Query(
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
		return nil
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
func scanPush(s scanner) (PushRow, int, error) {
	var (
		row                    PushRow
		position, at, received int64
		commands, introduced   string
	)
	if err := s.Scan(&row.PushID, &row.RepoID, &position, &at, &row.Actor, &row.ActorKind,
		&row.ActorSeat, &row.Node, &row.Atomic, &commands, &row.PackID, &row.PackBytes,
		&introduced, &row.IntroducedCount, &received); err != nil {
		return PushRow{}, 0, err
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

// The read limits, past which a query is refused naming the bound rather than
// clamped.
//
// A page's BYTES are held to MaxPageBytes whatever its count — measured with
// every row's rules and commands — so these bound what one request READS and
// are sized to what a surface shows of it:
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
//     ends at the byte budget like every page where it spells them longer.
//
// The maxima are exported for a client that pages: the most a page may ask
// for of each, which a page past them is refused naming.
const (
	defaultRepoLimit = 100
	MaxReposPerPage  = 500
	defaultPushLimit = 50
	MaxPushesPerPage = 200
	defaultRefLimit  = 200
	MaxRefsPerPage   = 1_000
)

// ReadLevels are the levels [Reader.Read] serves a principal's read at: every
// level but session, which it refuses because a surface reading the code host
// holds no position of its own on its log to wait for. The engine's own
// `introduced` read is the one kind served at session too, with the position
// it names: a git node's index reads the pushes of a round it synced, whose
// position it holds.
//
// ONE LIST, read by the rule and spelled by its refusal, because the refusal
// is a public answer — a REST read's 400 carries it to a person — and a
// sentence written beside the rule rather than from it told that person two
// levels existed while three were served.
var ReadLevels = []statelog.ReadLevel{
	statelog.ReadLinearizable, statelog.ReadStale, statelog.ReadConsistentPrefix,
}

// levelList spells levels as a sentence names them: "a, b and c".
func levelList(levels []statelog.ReadLevel) string {
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = string(l)
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Read answers a person's, a seat's or a surface's read — a listing of
// repositories, one repository in detail, one repository's push history or
// its refs — at one of [ReadLevels], stale unless asked; and the engine's own
// `introduced` read at the level it names.
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
	// about the reader ([Reader.chartFor]).
	chart, chartErr := r.chartFor(q.Asker)
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
			return r.repo(ctx, tx, q.Repo, q.Asker, chart, cp, &out)
		}
	case ReadPushes:
		query = fresh.Resolved(resolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.pushes(ctx, tx, q, chart, &out) }
	case ReadRefs:
		// A FIRST PAGE IS READ AS THE DETAIL IT CARRIES: a point read, which
		// a record this node deferred on the repository refuses, as kind
		// repo is refused, rather than detail a repository around it; every
		// later page is the listing's, a set.
		query = fresh.Resolved(resolver(q.Repo), q.Cursor != "")
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.refsListing(ctx, tx, q, chart, cp, &out) }
	case ReadIntroduced:
		// A POINT READ OF THE REPOSITORY, as a snapshot is: an index built
		// past a record this node deferred on the repository would miss the
		// pushes it carried and say nothing, so such a read refuses and the
		// backfill asks again.
		query = fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.Introduced.RepoID)}}, false)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.introduced(ctx, tx, q, cp, &out) }
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
// refs listing, a range outside an `introduced` read.
func (q ReadQuery) validate() error {
	if !q.Kind.Valid() {
		return fmt.Errorf("%w: kind: %q is not one of %v", ErrInvalid, q.Kind, ReadKinds())
	}
	if q.Kind == ReadIntroduced {
		return q.validateIntroduced()
	}
	// WHO ASKS IS NAMED, or nothing is read: an answer is held to the
	// asker's grant, and an absent asker would be judged as somebody nobody
	// named.
	if why := q.Asker.invalid(); why != "" {
		return fmt.Errorf("%w: asker: %s", ErrInvalid, why)
	}
	switch {
	case q.Introduced != nil:
		return fmt.Errorf("%w: introduced: only an introduced read names a range of pushes", ErrInvalid)
	case q.Prefix != "" && q.Kind != ReadRefs:
		return fmt.Errorf("%w: prefix: only a refs listing names a prefix", ErrInvalid)
	case q.Prefix != "" && !validRefPrefix(q.Prefix):
		return fmt.Errorf("%w: prefix: %q is not refs/heads/ or refs/tags/ or a directory of refs under "+
			"either, ending in /", ErrInvalid, q.Prefix)
	}
	if q.Kind != ReadRepos {
		if !ValidName(q.Repo) {
			return fmt.Errorf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
		}
		if len(q.Status) > 0 {
			return fmt.Errorf("%w: status: only a listing of repositories filters by status", ErrInvalid)
		}
	}
	for _, s := range q.Status {
		if !s.Valid() {
			return fmt.Errorf("%w: status: %q is not one of %v", ErrInvalid, s, RepoStatuses())
		}
	}
	bound := map[ReadKind]int{ReadRepos: MaxReposPerPage, ReadPushes: MaxPushesPerPage,
		ReadRefs: MaxRefsPerPage}[q.Kind]
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
	case in == nil:
		return refused("introduced", "an introduced read names its repository and its range")
	case in.RepoID == "":
		return refused("introduced.repo_id", "an introduced read names its repository by id")
	case q.Repo != "" || len(q.Status) > 0 || q.Prefix != "":
		return refused("repo", "an introduced read names its repository in its range and filters nothing")
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

// freshness is the level a read is served at: for every kind a principal
// reads, one of [ReadLevels], stale unless asked; for the engine's own
// `introduced`, any level a point read serves ([pointLevel]) — named, since
// whoever builds an index names the level its pages must be read at, and a
// session read with the position it must include.
func (q ReadQuery) freshness() (statelog.Freshness, error) {
	fresh := q.Fresh
	switch {
	case q.Kind == ReadIntroduced:
		return fresh, pointLevel(fresh)
	case fresh.Level == "":
		fresh.Level = statelog.ReadStale
	case !slices.Contains(ReadLevels, fresh.Level):
		return statelog.Freshness{}, fmt.Errorf("%w: %q is not a level a code read serves: it serves %s",
			ErrBadLevel, fresh.Level, levelList(ReadLevels))
	}
	return fresh, nil
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
// read, each carrying the asker's access to it.
//
// THE PAGE IS WHAT WAS SCANNED, NOT WHAT WAS KEPT: a repository the asker may
// not read is left out, and the cursor is still the last row the statement
// scanned — so a page can be shorter than its limit, or empty with a Next,
// and a caller pages until no Next comes back. Cut at the kept rows instead,
// a page of repositories the asker may not read would end the listing early,
// or a filter run until the page filled would scan without bound for a seat
// that reads little.
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
			v                                   repoView
			status, managed, owners, visibility string
			deleted                             sql.NullInt64
			head                                sql.NullString
		)
		if err := rows.Scan(&v.ref.ID, &v.ref.Name, &status, &managed, &owners, &v.ref.Owner.Ref,
			&visibility, &v.ref.DefaultBranch, &v.ref.Description, &deleted, &v.ref.Refs, &v.ref.LivePacks,
			&v.ref.LiveBytes, &v.ref.LiveObjects, &v.pushes, &v.lastPushAt, &head); err != nil {
			_ = rows.Close()
			return fmt.Errorf("code: list repositories: %w", err)
		}
		scanned, lastScanned = scanned+1, v.ref.Name
		v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
		v.ref.Visibility = Visibility(visibility)
		g := GrantOf(q.Asker, v.ref, chart)
		if !g.MayRead() {
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

// protectionsOf fills a page of repositories' rules in ONE statement.
func protectionsOf(ctx context.Context, tx *sql.Tx, ids []string, repos []RepoSummary) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, protectionsOfSQL, list)
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
		var p Protection
		if err := rows.Scan(&id, &p.Pattern, &p.AllowForce, &p.AllowDelete, &direct); err != nil {
			return fmt.Errorf("code: read the listing's protections: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		i := at[id]
		repos[i].Protections = append(repos[i].Protections, p)
	}
	return rows.Err()
}

// repo serves one repository in detail, to an asker that may read it.
//
// A REPOSITORY THE ASKER MAY NOT READ IS [ErrNotPermitted], judged after its
// row is read, and one that does not exist is still [ErrRepoNotFound]: that a
// `visibility: unit` repository exists is no secret — its name is in the
// company's document — and what it holds is.
func (r *Reader) repo(ctx context.Context, tx *sql.Tx, name string, asker Principal, chart Roster,
	cp statelog.Position, out *ReadAnswer) error {
	id, found, err := claimIn(ctx, tx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, name)
	}
	v, found, err := repoViewIn(ctx, tx, id, cp)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, name)
	}
	g := GrantOf(asker, v.ref, chart)
	if !g.MayRead() {
		return fmt.Errorf("%w: %s", ErrNotPermitted, g.ReadRefusal())
	}
	var head sql.NullString
	err = tx.QueryRowContext(ctx, liveRefSQL, id, branchPrefix+v.ref.DefaultBranch).Scan(&head)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("code: read %s's head: %w", name, err)
	}
	// THE COUNTS ARE THE ROW'S, kept by the applier as it moves the refs:
	// counted here they were a walk of every ref the namespace holds,
	// tombstones included, on every detail read.
	summary := v.summary(head.String)
	summary.Access = g.Access.String()
	out.Repo = &RepoDetail{RepoSummary: summary,
		Branches: v.branches, Tags: v.tags, Tombstones: v.tombstones,
		CreatedAt: store.DecodeTime(v.createdAt), Limits: r.limits()}
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
// answered [ErrNotPermitted], as [Reader.repo] answers it.
func (r *Reader) pushes(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	id, found, err := claimIn(ctx, tx, q.Repo)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
	}
	if err = mayReadIn(ctx, tx, id, q.Asker, chart); err != nil {
		return err
	}
	limit := limitOr(q.Limit, defaultPushLimit)
	var rows *sql.Rows
	if q.Cursor == "" {
		rows, err = tx.QueryContext(ctx, pushesFirstSQL, id, limit)
	} else {
		position, pushID, ok := cutPushCursor(q.Cursor)
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
		out.Pushes = append(out.Pushes, row)
		if pg.add(row.encodedBytes(commands)) {
			out.Next = pushCursor(row.Position, row.PushID)
			break
		}
	}
	return rows.Err()
}

// refsListing serves one page of a repository's LIVE refs under a prefix, by
// name, keyset after the last ref served, to an asker that may read the
// repository — and on the first page the repository in detail too, read as
// kind repo reads it in the same transaction, so what a seat asks first of a
// repository — what it is, and its branches — is one read and one barrier.
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
		if err := r.repo(ctx, tx, q.Repo, q.Asker, chart, cp, out); err != nil {
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
		if err = mayReadIn(ctx, tx, id, q.Asker, chart); err != nil {
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
// limit or at MaxPageBytes of rows, whichever first, and always serves one.
// A repository with no row — purged — is [ErrRepoNotFound], which drops the
// index that asked.
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
	switch position, pushID, keyed := cutPushCursor(q.Cursor); {
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
		pushed, _, err := scanPush(rows)
		if err != nil {
			return fmt.Errorf("code: read %s's pushes in log order: %w", name, err)
		}
		row := introducedOf(pushed)
		out.Introduced = append(out.Introduced, row)
		if pg.add(row.encodedBytes()) {
			out.Next = pushCursor(row.Position, row.PushID)
			break
		}
	}
	return rows.Err()
}

// ResolveRef answers one ref of one repository at the level the query names,
// to an asker that may read the repository — in ONE transaction: the name's
// claim; the repository, a deleted or purging one answered [ErrRepoNotFound]
// in [RepoGone]'s sentence for its state, which the code host's admission
// says too; the asker's grant ([ErrNotPermitted]); then the ref — the whole
// ref the query names, or a short name as a branch and then as a tag. A
// deleted ref is absent, and none found is [ErrRefNotFound] naming every
// name tried. Its scope is the name's, as [Reader.Resolve]'s is.
func (r *Reader) ResolveRef(ctx context.Context, q RefQuery) (RefAnswer, error) {
	if err := pointLevel(q.Fresh); err != nil {
		return RefAnswer{}, err
	}
	if !ValidName(q.Repo) {
		return RefAnswer{}, fmt.Errorf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
	}
	if why := q.Asker.invalid(); why != "" {
		return RefAnswer{}, fmt.Errorf("%w: asker: %s", ErrInvalid, why)
	}
	names, nameErr := refNames(q.Ref)
	if nameErr != nil {
		return RefAnswer{}, nameErr
	}
	chart, chartErr := r.chartFor(q.Asker)
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
				Status: v.ref.Status, DeletedAt: v.ref.DeletedAt}.Detail())
		}
		if g := GrantOf(q.Asker, v.ref, chart); !g.MayRead() {
			return fmt.Errorf("%w: %s", ErrNotPermitted, g.ReadRefusal())
		}
		for _, name := range names {
			var oid string
			switch err := tx.QueryRowContext(ctx, liveRefSQL, id, name).Scan(&oid); {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return fmt.Errorf("code: read %s of %s: %w", name, q.Repo, err)
			}
			out = RefAnswer{Repo: v.ref, Ref: name, OID: oid, Position: cp.At}
			return nil
		}
		return fmt.Errorf("%w: repository %s holds no live %s", ErrRefNotFound, q.Repo,
			strings.Join(names, " or "))
	})
	if err != nil {
		return RefAnswer{}, err
	}
	return out, nil
}

// refNames is every ref a [RefQuery]'s Ref may name, in the order they are
// tried: a whole ref as itself; a short name as a branch, then as a tag —
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

// mayReadIn refuses an asker that may not read repository id, judged from
// what a grant reads of its row in tx — its name, its owner and its
// visibility — by one primary-key read.
func mayReadIn(ctx context.Context, tx *sql.Tx, id string, asker Principal, chart Roster) error {
	var (
		ref                = RepoRef{ID: id}
		owners, visibility string
	)
	err := tx.QueryRowContext(ctx, repoGrantSQL, id).Scan(&ref.Name, &owners, &ref.Owner.Ref, &visibility)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, id)
	case err != nil:
		return fmt.Errorf("code: read repository %s's grant: %w", id, err)
	}
	ref.Owner.Kind, ref.Visibility = OwnerKind(owners), Visibility(visibility)
	if g := GrantOf(asker, ref, chart); !g.MayRead() {
		return fmt.Errorf("%w: %s", ErrNotPermitted, g.ReadRefusal())
	}
	return nil
}

// pushCursor is "<packed position>/<push_id>": a push's place in log order,
// the keyset of the push listing and of the `introduced` read alike.
func pushCursor(position statelog.Position, pushID string) string {
	return strconv.FormatInt(position.Packed(), 10) + "/" + pushID
}

// cutPushCursor reads a [pushCursor].
func cutPushCursor(s string) (int64, string, bool) {
	at, pushID, ok := strings.Cut(s, "/")
	if !ok || pushID == "" {
		return 0, "", false
	}
	position, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return position, pushID, true
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
	query := statelog.Freshness{Level: statelog.ReadSession, MinPosition: at}.Query(
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
	repoViewSQL = `SELECT id, name, status, managed_by, owner_kind, owner, visibility, default_branch,
		description, deleted_at, refs, branches, tags, tombstones, live_packs, live_bytes,
		live_objects, tombstones_through, retired_through, pushes, last_push_at, created_at
		FROM code_repos WHERE id = ?`
	packColumns       = `pack_id, part, object, hash, size, index_bytes, reverse_bytes, git_checksum, objects, kind, state, added_position`
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
	// ?2, at most ?3 deep over the index the retired phase reads: a full
	// answer's test of what it can serve above its caller's RetiredSince
	// (retiredFloor), and a delta's third term.
	retiredAboveSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ?1 AND retired_position > ?2 LIMIT ?3)`
	// deltaRowsSQL is a BOUNDED COUNT of the rows a delta from ?2 carries,
	// each table counted at most ?3 deep over the index the delta's pages
	// read (deltaOutweighs).
	deltaRowsSQL = `SELECT
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_refs WHERE repo_id = ?1 AND position > ?2 LIMIT ?3))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_packs WHERE repo_id = ?1 AND added_position > ?2 LIMIT ?3))
		+ (` + retiredAboveSQL + `)`
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
	pushColumns = `push_id, repo_id, position, at, actor, actor_kind, actor_seat, node, atomic,
		commands, pack_id, pack_bytes, introduced, introduced_count, received_at`
	pushSQL        = `SELECT ` + pushColumns + ` FROM code_pushes WHERE push_id = ?`
	pushesFirstSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		ORDER BY position DESC, push_id DESC LIMIT ?`
	pushesAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position <= ? AND (position < ? OR push_id < ?)
		ORDER BY position DESC, push_id DESC LIMIT ?`
	reposSQL = `SELECT r.id, r.name, r.status, r.managed_by, r.owner_kind, r.owner, r.visibility,
		r.default_branch, r.description, r.deleted_at, r.refs, r.live_packs, r.live_bytes,
		r.live_objects, r.pushes, r.last_push_at, h.target
		FROM code_repos r LEFT JOIN code_refs h INDEXED BY ` + refsKey + `
			ON h.repo_id = r.id AND h.ref = 'refs/heads/' || r.default_branch AND h.target != ''
		WHERE r.status IN (SELECT value FROM json_each(?)) AND r.name > ?
		ORDER BY r.name LIMIT ?`
	protectionsOfSQL = `SELECT repo_id, pattern, allow_force, allow_delete, direct_push
		FROM code_protections WHERE repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
		ORDER BY repo_id, pattern`
	// repoGrantSQL is what a grant reads of one repository's row.
	repoGrantSQL = `SELECT name, owner_kind, owner, visibility FROM code_repos WHERE id = ?`
	// liveRefSQL is one live ref's target by its whole name: a repository's
	// head, and a ref a RefQuery names.
	liveRefSQL = `SELECT target FROM code_refs INDEXED BY ` + refsKey + `
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
)
