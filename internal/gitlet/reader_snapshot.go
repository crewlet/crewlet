package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

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
