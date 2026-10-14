package code

import (
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
}

// NewReader builds the code host's read side.
func NewReader(o ReaderOptions) (*Reader, error) {
	if o.Log == nil {
		return nil, errors.New("code: a reader needs the code log's read authority — " +
			"without it every read level is a label rather than a guarantee")
	}
	r := &Reader{log: o.Log, limits: o.Limits, pageRows: MaxSnapshotItems, pageBytes: MaxPageBytes}
	if r.limits == nil {
		r.limits = DefaultLimits
	}
	return r, nil
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
	pushes            int64
	lastPushAt        sql.NullInt64
	createdAt         int64
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
		v                       repoView
		status, managed, owners string
		deleted                 sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, repoViewSQL, id).Scan(&v.ref.ID, &v.ref.Name, &status, &managed,
		&owners, &v.ref.Owner.Ref, &v.ref.DefaultBranch, &v.ref.Description, &deleted, &v.ref.Refs,
		&v.branches, &v.tags, &v.tombstones, &v.ref.LivePacks, &v.ref.LiveBytes, &v.ref.LiveObjects,
		&v.tombstonesThrough, &v.pushes, &v.lastPushAt, &v.createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoView{}, false, nil
	case err != nil:
		return repoView{}, false, fmt.Errorf("code: read repository %s: %w", id, err)
	}
	v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
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
// pack and every live ref — or as a delta from q.Since — the packs added and
// the refs moved (tombstones included) above it.
//
// # One monotone history across pages
//
// A snapshot of more than one page is read in several transactions, possibly
// on several nodes, while the repository is pushed to. Three rules keep its
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
//   - A later page re-checks, in its own transaction, that the history did
//     not move under the snapshot: the same stream, the same generation, and
//     for a delta no tombstone above its floor swept. Any failure is
//     [ErrSnapshotMoved], and the consumer reads the snapshot again from its
//     first page, which decides full or delta afresh.
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
			full := q.Since.IsZero() || q.Since.Stream != topics.CodeLogStream ||
				q.Since.Generation != cp.At.Generation || q.Since.Packed() < v.tombstonesThrough
			cur = StateCursor{Full: full, Phase: phasePacks, From: cp.At, Seen: cp.At, Stream: cp.KeyedTo}
			if !full {
				cur.Since = q.Since
			}
		} else {
			cur = *q.Cursor
			switch {
			case !cp.KeyedTo.Equal(cur.Stream) || cp.At.Generation != cur.From.Generation:
				return fmt.Errorf("%w: the log under %s was recreated or reanchored", ErrSnapshotMoved, q.RepoID)
			case !cur.Full && cur.Since.Packed() < v.tombstonesThrough:
				return fmt.Errorf("%w: a deletion %s's delta needed was swept", ErrSnapshotMoved, q.RepoID)
			}
			cur.Seen = cp.At
		}
		out.Repo, out.Position, out.StreamCreated, out.Full = v.ref, cur.From, cur.Stream, cur.Full
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
		case cur.Phase == phaseRefs && !first:
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

// checkCursor refuses a cursor this reader did not issue.
func checkCursor(c *StateCursor) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrBadCursor, why) }
	switch {
	case c.Phase != phasePacks && c.Phase != phaseRefs && c.Phase != phaseTail:
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
	}
	if c.After == "" {
		return nil
	}
	switch {
	case c.Phase == phasePacks && c.Full:
		if _, _, ok := strings.Cut(c.After, "/"); !ok {
			return bad("after " + c.After)
		}
	case c.Phase == phasePacks, c.Phase == phaseTail:
		if _, _, _, ok := cutPackKey(c.After); !ok {
			return bad("after " + c.After)
		}
	case c.Phase == phaseRefs && !c.Full:
		at, _, ok := strings.Cut(c.After, "/")
		if _, err := strconv.ParseInt(at, 10, 64); !ok || err != nil {
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
//     (about 460 KB each: MaxCommandsPerPush commands at the longest ref and
//     MaxIntroduced ids) ends at the byte budget after its tenth.
const (
	defaultRepoLimit = 100
	maxRepoLimit     = 500
	defaultPushLimit = 50
	maxPushLimit     = 200
)

// ReadLevels are the levels [Reader.Read] serves: every level but session,
// which it refuses because a surface reading the code host holds no position
// of its own on its log to wait for.
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

// Read answers a person's or a surface's read: a listing of repositories, one
// repository in detail, or one repository's push history, at one of
// [ReadLevels] — stale unless asked.
func (r *Reader) Read(ctx context.Context, q ReadQuery) (ReadAnswer, error) {
	if err := q.validate(); err != nil {
		return ReadAnswer{}, err
	}
	fresh := q.Fresh
	switch {
	case fresh.Level == "":
		fresh.Level = statelog.ReadStale
	case !slices.Contains(ReadLevels, fresh.Level):
		return ReadAnswer{}, fmt.Errorf("%w: %q is not a level a code read serves: it serves %s", ErrBadLevel,
			fresh.Level, levelList(ReadLevels))
	}
	var (
		out   ReadAnswer
		query statelog.Query
		serve func(*sql.Tx, statelog.Position) error
	)
	switch q.Kind {
	case ReadRepos:
		query = fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.repos(ctx, tx, q, cp, &out) }
	case ReadRepo:
		query = fresh.Resolved(resolver(q.Repo), false)
		serve = func(tx *sql.Tx, cp statelog.Position) error { return r.repo(ctx, tx, q.Repo, cp, &out) }
	case ReadPushes:
		query = fresh.Resolved(resolver(q.Repo), true)
		serve = func(tx *sql.Tx, _ statelog.Position) error { return r.pushes(ctx, tx, q, &out) }
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

// validate refuses a query naming no kind this build reads, a repository that
// cannot be one, a limit past its bound — refused naming it, never clamped —
// or a status filter outside a listing.
func (q ReadQuery) validate() error {
	if !q.Kind.Valid() {
		return fmt.Errorf("%w: kind: %q is not one of %v", ErrInvalid, q.Kind, ReadKinds())
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
	bound := map[ReadKind]int{ReadRepos: maxRepoLimit, ReadPushes: maxPushLimit}[q.Kind]
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

// limitOr is a limit, or its kind's default.
func limitOr(limit, fallback int) int {
	if limit == 0 {
		return fallback
	}
	return limit
}

// repos serves a listing of repositories, by name.
func (r *Reader) repos(ctx context.Context, tx *sql.Tx, q ReadQuery, cp statelog.Position, out *ReadAnswer) error {
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
	var ids []string
	for rows.Next() {
		var (
			v                       repoView
			status, managed, owners string
			deleted                 sql.NullInt64
			head                    sql.NullString
		)
		if err := rows.Scan(&v.ref.ID, &v.ref.Name, &status, &managed, &owners, &v.ref.Owner.Ref,
			&v.ref.DefaultBranch, &v.ref.Description, &deleted, &v.ref.Refs, &v.ref.LivePacks,
			&v.ref.LiveBytes, &v.ref.LiveObjects, &v.pushes, &v.lastPushAt, &head); err != nil {
			_ = rows.Close()
			return fmt.Errorf("code: list repositories: %w", err)
		}
		v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
		if deleted.Valid {
			v.ref.DeletedAt = store.DecodeTime(deleted.Int64)
		}
		v.ref.Position = cp
		s := v.summary(head.String)
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
	// the row that fills it, and the cursor is that row's name.
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for i, s := range out.Repos {
		if pg.add(s.encodedBytes()) {
			out.Next = s.Name
			out.Repos = out.Repos[:i+1]
			break
		}
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

// repo serves one repository in detail.
func (r *Reader) repo(ctx context.Context, tx *sql.Tx, name string, cp statelog.Position, out *ReadAnswer) error {
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
	var head sql.NullString
	err = tx.QueryRowContext(ctx, headSQL, id, branchPrefix+v.ref.DefaultBranch).Scan(&head)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("code: read %s's head: %w", name, err)
	}
	// THE COUNTS ARE THE ROW'S, kept by the applier as it moves the refs:
	// counted here they were a walk of every ref the namespace holds,
	// tombstones included, on every detail read.
	out.Repo = &RepoDetail{RepoSummary: v.summary(head.String),
		Branches: v.branches, Tags: v.tags, Tombstones: v.tombstones,
		CreatedAt: store.DecodeTime(v.createdAt), Limits: r.limits()}
	return nil
}

// pushes serves one repository's push history, newest first, keyset-paged by
// (at, position, push_id) — the push id because group commit puts up to 64
// pushes in one record, sharing its position and its instant, so a cursor
// without it skips or repeats the pushes of a record a page boundary splits.
func (r *Reader) pushes(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	id, found, err := claimIn(ctx, tx, q.Repo)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
	}
	limit := limitOr(q.Limit, defaultPushLimit)
	var rows *sql.Rows
	if q.Cursor == "" {
		rows, err = tx.QueryContext(ctx, pushesFirstSQL, id, limit)
	} else {
		at, position, pushID, ok := cutPushCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, pushesAfterSQL, id, at, at, position, position, pushID, limit)
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
			out.Next = pushCursor(row)
			break
		}
	}
	return rows.Err()
}

// pushCursor is "<at micros>/<packed position>/<push_id>".
func pushCursor(row PushRow) string {
	return strconv.FormatInt(store.EncodeTime(row.At), 10) + "/" +
		strconv.FormatInt(row.Position.Packed(), 10) + "/" + row.PushID
}

// cutPushCursor reads a [pushCursor].
func cutPushCursor(s string) (int64, int64, string, bool) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 || parts[2] == "" {
		return 0, 0, "", false
	}
	at, err1 := strconv.ParseInt(parts[0], 10, 64)
	position, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, "", false
	}
	return at, position, parts[2], true
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
// is ONE statement over its own partial index (replicated 0043), so a page
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
	repoViewSQL = `SELECT id, name, status, managed_by, owner_kind, owner, default_branch,
		description, deleted_at, refs, branches, tags, tombstones, live_packs, live_bytes,
		live_objects, tombstones_through, pushes, last_push_at, created_at FROM code_repos WHERE id = ?`
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
	pushColumns = `push_id, repo_id, position, at, actor, actor_kind, actor_seat, node, atomic,
		commands, pack_id, pack_bytes, introduced, introduced_count, received_at`
	pushSQL        = `SELECT ` + pushColumns + ` FROM code_pushes WHERE push_id = ?`
	pushesFirstSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		ORDER BY at DESC, position DESC, push_id DESC LIMIT ?`
	pushesAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND at <= ? AND (at < ? OR position < ? OR (position = ? AND push_id < ?))
		ORDER BY at DESC, position DESC, push_id DESC LIMIT ?`
	reposSQL = `SELECT r.id, r.name, r.status, r.managed_by, r.owner_kind, r.owner,
		r.default_branch, r.description, r.deleted_at, r.refs, r.live_packs, r.live_bytes,
		r.live_objects, r.pushes, r.last_push_at, h.target
		FROM code_repos r LEFT JOIN code_refs h INDEXED BY ` + refsKey + `
			ON h.repo_id = r.id AND h.ref = 'refs/heads/' || r.default_branch AND h.target != ''
		WHERE r.status IN (SELECT value FROM json_each(?)) AND r.name > ?
		ORDER BY r.name LIMIT ?`
	protectionsOfSQL = `SELECT repo_id, pattern, allow_force, allow_delete, direct_push
		FROM code_protections WHERE repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
		ORDER BY repo_id, pattern`
	headSQL = `SELECT target FROM code_refs INDEXED BY ` + refsKey + `
		WHERE repo_id = ? AND ref = ? AND target != ''`
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
