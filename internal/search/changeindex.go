package search

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textindex"
)

// ChangeIndexBatchRows is the most rows a walk's reach read takes past its watermark or below its
// floor. A forward batch is widened to every row at its last position, since one push record moves up
// to some 1 300 changes there.
const ChangeIndexBatchRows = 512

// ChangeIndexBatchBytes is the most description bytes a walk batch takes before it is cut at that row
// (a whole position on the forward walk): some 170 changes of 150 words.
const ChangeIndexBatchBytes = 256 << 10

// ChangeIndexBatchMinBytes is the least a halved batch budget takes: some ten such changes.
const ChangeIndexBatchMinBytes = 16 << 10

// ChangeIndexBatchWrite is how long a write may take before its kind's next budget halves; one under a
// quarter of it doubles that budget back. The node file's busy wait is 5 s, past which its event log
// drops an append.
const ChangeIndexBatchWrite = 2 * time.Second

// ChangeDropBatch is the most documents of a purged repository one drop step removes.
const ChangeDropBatch = 64

// ChangeDropBatchMin is the least a halved drop step takes.
const ChangeDropBatchMin = 8

// ChangeDocTermSlots is how many terms one document's postings have room for: a posting's slot is its
// document's key shifted 16 bits plus the term's place, so a document's postings are one key range.
// More than the 16 512 runs Gitlet's largest title and description hold, so every term is kept.
const ChangeDocTermSlots = 1 << changeSlotBits

// changeSlotBits is the shift a slot spells; the statements spell it as the literal 16.
const changeSlotBits = 16

// ChangeDocProbe is how many keys past a document's natural key a walk reads for a free one before it
// takes the arena's next: eight milliseconds of keys.
const ChangeDocProbe = 256

// A document's natural key is its change id's millisecond shifted changeDocTieBits plus that many more
// bits of the id, for a millisecond in [changeDocMinMs, changeDocEndMs) (2004 to 2109), so its slot
// range never reaches 2^63; the arena is the keys below changeDocArenaTop.
const (
	changeDocTieBits  = 5
	changeDocMinMs    = 1 << 40
	changeDocEndMs    = 1<<42 - 1
	changeDocArenaTop = changeDocMinMs << changeDocTieBits
)

// changeIDsAbove is a text above every canonical change id: the first backward batch's floor.
const changeIDsAbove = "~"

// ChangePurgeFollowRepos is how many repositories one sweep's purge follower visits: one seek each of
// this node's index and one estate statement for all, so a thousand come round in sixteen idle sweeps.
const ChangePurgeFollowRepos = 64

// MaxChangeTextScan is how many postings of a word, or documents of a repository, an owner or a state,
// one search reads of what it drives on; past it the answer says Truncated (maxPostingScan's reason).
const MaxChangeTextScan = 5_000

// ChangeIndexer keeps this node's index of Gitlet changes' words in step with the replicated rows
// (ADR-0156): a first build newest first by id, a forward walk over code_changes.position up to the
// code log's applied prefix, a purge follower, a rebuild on a new copy.
type ChangeIndexer struct {
	db         *store.DB              // this node's own file, where the index is kept
	estate     store.ReplicatedReader // the replicated rows it walks
	code       statelog.Domain        // the code domain, whose applied prefix bounds the walks
	repoCursor string                 // the purge follower's rotation, in memory as chat's pruneCursor
	rebuild    atomic.Bool            // a rebuild asked for, which the loop honours
	scan       int                    // MaxChangeTextScan, or a test's
	// Each kind of write's batch, moved by its own writes' times alone, so neither walk undoes the other's.
	forward, back, drop changeBudget
	since               func(time.Time) time.Duration // how long a write took: time.Since, or a test's
}

// changeBudget is one kind of write's batch: description bytes for a walk, documents for a drop.
type changeBudget struct{ n, least, most int }

// after moves b by how long its last write took ([nextChangeBatch]).
func (b *changeBudget) after(took time.Duration) { b.n = nextChangeBatch(b.n, took, b.least, b.most) }

// NewChangeIndexer builds one over this node's own store and the replicated estate beside it. code is
// the domain whose rows it walks (gitlet.Domain), handed in since this package cannot import it.
func NewChangeIndexer(db *store.DB, estate store.ReplicatedReader, code statelog.Domain) *ChangeIndexer {
	walk := changeBudget{n: ChangeIndexBatchBytes, least: ChangeIndexBatchMinBytes, most: ChangeIndexBatchBytes}
	return &ChangeIndexer{db: db, estate: estate, code: code, scan: MaxChangeTextScan, forward: walk,
		back: walk, drop: changeBudget{n: ChangeDropBatch, least: ChangeDropBatchMin, most: ChangeDropBatch},
		since: time.Since}
}

// RequestRebuild asks the index to begin its first build again, for an estate replaced under the
// node; [ChangeIndexer.Holding] answers Rebuilding from this call on until that build begins.
func (x *ChangeIndexer) RequestRebuild() { x.rebuild.Store(true) }

// ChangeReachRead is the positions and description sizes of the next rows past a watermark, on
// code_changes_position_idx: how far the walk's next batch reaches.
func ChangeReachRead(through int64, rows int) (string, []any) {
	return changeReachStatement, []any{through, rows}
}

// ChangeBatchRead is every change moved past a watermark up to a reach, with its description.
func ChangeBatchRead(through, reach int64) (string, []any) {
	return changeBatchStatement, []any{through, reach}
}

// ChangeBackReachRead is the ids, positions and description sizes of the next changes below a floor,
// newest minted first on code_changes' primary key: how far the first build's next batch reaches.
func ChangeBackReachRead(floor string, rows int) (string, []any) {
	return changeBackReachStatement, []any{floor, rows}
}

// ChangeBackBatchRead is every change below a floor down to a reach, at or below where the first build
// began, with its description, newest minted first.
func ChangeBackBatchRead(floor, reach string, back int64) (string, []any) {
	return changeBackBatchStatement, []any{floor, reach, back}
}

// ChangeReposRead is which of a list of repositories still has its row: a purged one has none.
func ChangeReposRead(ids []string) (string, []any) {
	list, err := json.Marshal(ids)
	if err != nil {
		// A []string always marshals; a failure here is the runtime's.
		panic(err)
	}
	return changeReposStatement, []any{string(list)}
}

// The statements the index issues, named so its plan gate explains these rather than copies of them
// (TestEveryChangeIndexServesARegisteredQuery).
const (
	changeReachStatement = `SELECT position, body_bytes FROM code_changes INDEXED BY code_changes_position_idx
		WHERE position > ? ORDER BY position LIMIT ?`
	changeBatchStatement = `SELECT c.id, c.repo_id, c.owner, c.state, c.title, COALESCE(b.body, '')
		FROM code_changes AS c INDEXED BY code_changes_position_idx
		LEFT JOIN code_change_bodies AS b ON b.change_id = c.id
		WHERE c.position > ? AND c.position <= ? ORDER BY c.position`
	changeReposStatement = `SELECT r.id FROM json_each(?) AS j CROSS JOIN code_repos AS r
		WHERE r.id = CAST(j.value AS TEXT)`
	// The first build's reads, by the primary key backwards: c.position <= ? tests each row it reads.
	changeBackReachStatement = `SELECT id, position, body_bytes FROM code_changes
		INDEXED BY sqlite_autoindex_code_changes_1 WHERE id < ? ORDER BY id DESC LIMIT ?`
	changeBackBatchStatement = `SELECT c.id, c.repo_id, c.owner, c.state, c.title, COALESCE(b.body, '')
		FROM code_changes AS c INDEXED BY sqlite_autoindex_code_changes_1
		LEFT JOIN code_change_bodies AS b ON b.change_id = c.id
		WHERE c.id < ? AND c.id >= ? AND c.position <= ? ORDER BY c.id DESC`

	changeStateStatement = `SELECT built_from, through_pos, seen_pos, back_pos, back_floor
		FROM change_index_state WHERE id = 1`
	// changeAdvanceStatement moves the watermark and records the settled position the batch read.
	changeAdvanceStatement = `UPDATE change_index_state
		SET through_pos = MAX(through_pos, ?), seen_pos = MAX(seen_pos, ?), updated_at = ? WHERE id = 1`
	// changeBackStatement records a backward batch: where the build began, the floor it reached, the
	// watermark at least there, and the settled position it read.
	changeBackStatement = `UPDATE change_index_state SET back_pos = ?, back_floor = ?,
		through_pos = MAX(through_pos, ?), seen_pos = MAX(seen_pos, ?), updated_at = ? WHERE id = 1`
	// changeSeenStatement is a purge drop's record of the settled position it was decided from.
	changeSeenStatement = `UPDATE change_index_state SET seen_pos = MAX(seen_pos, ?), updated_at = ?
		WHERE id = 1`
	// changeRestartStatement begins the first build again over the copy it names, in one statement.
	changeRestartStatement = `UPDATE change_index_state SET built_from = ?, through_pos = 0, seen_pos = 0,
		back_pos = -1, back_floor = '', updated_at = ? WHERE id = 1`

	changeHeldDocStatement = `SELECT doc, repo_id, owner, state, text_hash FROM change_docs
		INDEXED BY change_docs_id_idx WHERE id = ?`
	// changeHeldInRangeStatement is the documents of the ids a backward batch read, to remove those
	// the estate no longer holds.
	changeHeldInRangeStatement = `SELECT doc, id FROM change_docs INDEXED BY change_docs_id_idx
		WHERE id >= ? AND id < ?`
	// changeDocKeysStatement is the keys taken in a natural key's window; changeDocArenaTopStatement the
	// arena's highest. Each is one range of the table's key: Turso plans MAX(doc) as a scan.
	changeDocKeysStatement     = `SELECT doc FROM change_docs WHERE doc >= ? AND doc < ? ORDER BY doc`
	changeDocArenaTopStatement = `SELECT doc FROM change_docs WHERE doc < ? ORDER BY doc DESC LIMIT 1`
	changeDocInsertStatement   = `INSERT INTO change_docs (doc, id, repo_id, owner, state, text_hash)
		VALUES (?, ?, ?, ?, ?, ?)`
	changeDocRewriteStatement = `UPDATE change_docs SET repo_id = ?, owner = ?, state = ?, text_hash = ?
		WHERE doc = ?`
	changeDocScopeStatement  = `UPDATE change_docs SET repo_id = ?, owner = ?, state = ? WHERE doc = ?`
	changeRemoveDocStatement = `DELETE FROM change_docs WHERE doc = ?`
	// changeClearPostingsStatement deletes one document's postings, its slot range, by the table's own
	// key: no removal needs the words the document held.
	changeClearPostingsStatement = `DELETE FROM change_postings WHERE slot >= ? AND slot < ?`
	changePostingsInsert         = `INSERT INTO change_postings (slot, term) VALUES`

	changeNextRepoStatement = `SELECT repo_id FROM change_docs INDEXED BY change_docs_repo_idx
		WHERE repo_id > ? ORDER BY repo_id LIMIT 1`
	changeRepoDocsStatement = `SELECT doc FROM change_docs INDEXED BY change_docs_repo_idx
		WHERE repo_id = ? LIMIT ?`
)

// changeDoc is one change on its way into the index.
type changeDoc struct {
	id                   string
	repoID, owner, state string
	title, body          string
}

// Sweep advances the index by one step and reports how much it did: before the first build begins, its
// first backward batch alone; else one step of the purge follower, one forward batch, and one backward
// batch while the first build walks back. Zero with no error is idle, which [ChangeIndexer.Run] waits on.
func (x *ChangeIndexer) Sweep(ctx context.Context, now time.Time) (int, error) {
	instance, err := x.estateInstance(ctx)
	if err != nil {
		return 0, err
	}
	state, err := x.indexState(ctx)
	if err != nil {
		return 0, err
	}
	if asked := x.rebuild.Load(); asked || state.builtFrom != instance {
		if err = x.restart(ctx, now, state, instance, asked); err != nil {
			return 0, err
		}
		// Taken once its mark commits, so no instant between the two reads as built.
		if asked {
			x.rebuild.Store(false)
		}
		// AN INDEX NOT BEGUN only names its copy; a begun one answers nothing until its next sweep.
		if asked || state.began() {
			return 1, nil
		}
		state = changeIndexState{builtFrom: instance, backPos: -1}
	}
	// NOT BEGUN: from a watermark of 0 the forward walk would take the whole history oldest first.
	if !state.began() {
		return x.reachBack(ctx, now, state)
	}
	dropped, err := x.followPurges(ctx, now)
	if err != nil {
		return 0, err
	}
	walked, err := x.advance(ctx, now, state.through)
	if err != nil || state.backFloor == "" {
		return dropped + walked, err
	}
	back, err := x.reachBack(ctx, now, state)
	return dropped + walked + back, err
}

// advance indexes the next batch of changes past the watermark, never past the code log's applied
// prefix in the snapshot it reads, and reports the rows it walked.
func (x *ChangeIndexer) advance(ctx context.Context, now time.Time, through int64) (int, error) {
	var (
		docs    []changeDoc
		reach   int64
		settled int64
	)
	err := x.estate.Read(ctx, func(tx *sql.Tx) (err error) {
		docs, reach, settled = nil, 0, 0
		prefix, err := statelog.PrefixIn(ctx, tx, x.code)
		if err != nil {
			return err
		}
		settled = prefix.Settled.Packed()
		if reach, err = x.reach(ctx, tx, through, x.forward.n); err != nil {
			return err
		}
		// A RECORD THIS NODE RETAINS is applied later at its own position, so the walk stops below it.
		reach = min(reach, prefix.Applied().Packed())
		if reach <= through {
			return nil
		}
		docs, err = x.batch(ctx, tx, through, reach)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("search: read the changes past %d: %w", through, err)
	}
	// NOTHING APPLIED PAST THE WATERMARK, or only rows a retained record holds back: nothing is written.
	if len(docs) == 0 {
		return 0, nil
	}
	if err = x.writeBatch(ctx, &x.forward, nil, docs, changeAdvanceStatement, reach, settled,
		now.UTC().UnixMilli()); err != nil {
		return 0, fmt.Errorf("search: index a batch of changes: %w", err)
	}
	return len(docs), nil
}

// writeBatch runs first, where given, then writes docs, in the order given, and their postings by slot
// in one node transaction that ends with mark bound to args, and moves budget by how long it took.
func (x *ChangeIndexer) writeBatch(ctx context.Context, budget *changeBudget, first func(*sql.Tx) error,
	docs []changeDoc, mark string, args ...any) error {

	maxVariables := x.db.Caps().MaxVariables
	began := time.Now()
	err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		if first != nil {
			if firstErr := first(tx); firstErr != nil {
				return firstErr
			}
		}
		var postings []changePosting
		for _, doc := range docs {
			written, writeErr := writeChangeDoc(ctx, tx, doc)
			if writeErr != nil {
				return writeErr
			}
			postings = append(postings, written...)
		}
		if writeErr := writeChangePostings(ctx, tx, postings, maxVariables); writeErr != nil {
			return writeErr
		}
		_, execErr := tx.ExecContext(ctx, mark, args...)
		return execErr
	})
	if err == nil {
		budget.after(x.since(began))
	}
	return err
}

// nextChangeBatch is a batch after one that took took to write: halved past [ChangeIndexBatchWrite],
// not below least; doubled under a quarter of it; never past most.
func nextChangeBatch(n int, took time.Duration, least, most int) int {
	switch {
	case took > ChangeIndexBatchWrite:
		return min(max(n/2, least), most)
	case took < ChangeIndexBatchWrite/4:
		return min(n*2, most)
	}
	return n
}

// reach is the position the next batch past through ends on: the row at which the descriptions read
// pass budget, or the last of [ChangeIndexBatchRows]; through where none moved past it.
func (x *ChangeIndexer) reach(ctx context.Context, tx *sql.Tx, through int64, budget int) (int64, error) {
	query, args := ChangeReachRead(through, ChangeIndexBatchRows)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	reach, weighed := through, int64(0)
	for rows.Next() {
		var position, size int64
		if scanErr := rows.Scan(&position, &size); scanErr != nil {
			return 0, scanErr
		}
		reach, weighed = position, weighed+size
		if weighed >= int64(budget) {
			break
		}
	}
	return reach, rows.Err()
}

// batch reads every change past through up to reach, so a batch ends on a whole position.
func (x *ChangeIndexer) batch(ctx context.Context, tx *sql.Tx, through, reach int64) ([]changeDoc, error) {
	query, args := ChangeBatchRead(through, reach)
	return x.readDocs(ctx, tx, query, args)
}

// readDocs reads the changes a walk's batch statement answers, in its order.
func (x *ChangeIndexer) readDocs(ctx context.Context, tx *sql.Tx, query string, args []any) ([]changeDoc, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []changeDoc
	for rows.Next() {
		var doc changeDoc
		if scanErr := rows.Scan(&doc.id, &doc.repoID, &doc.owner, &doc.state, &doc.title,
			&doc.body); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// reachBack indexes the first build's next batch, newest minted first below its floor and at or below
// where it began, and drops documents of ids in that stretch the snapshot no longer holds. It reports
// the rows read, or 1 where it only began the build or marked it whole.
func (x *ChangeIndexer) reachBack(ctx context.Context, now time.Time, state changeIndexState) (int, error) {
	var (
		docs          []changeDoc
		back, settled int64
		reached       string
		read          map[string]bool
	)
	from := state.backFloor
	if !state.began() {
		from = changeIDsAbove
	}
	err := x.estate.Read(ctx, func(tx *sql.Tx) (err error) {
		docs, read = nil, nil
		prefix, err := statelog.PrefixIn(ctx, tx, x.code)
		if err != nil {
			return err
		}
		settled, back = prefix.Settled.Packed(), state.backPos
		if !state.began() {
			back = prefix.Applied().Packed()
		}
		var held int
		if reached, read, held, err = x.reachBelow(ctx, tx, from, back); err != nil || held == 0 {
			return err
		}
		docs, err = x.backBatch(ctx, tx, from, reached, back)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("search: read the changes below %q: %w", from, err)
	}
	slices.Reverse(docs)
	prune := func(tx *sql.Tx) error { return pruneChangeDocs(ctx, tx, reached, from, read) }
	if err = x.writeBatch(ctx, &x.back, prune, docs, changeBackStatement, back, reached, back, settled,
		now.UTC().UnixMilli()); err != nil {
		return 0, fmt.Errorf("search: index a batch of the first build: %w", err)
	}
	if !state.began() {
		log.InfoContext(ctx, "change_index_build_began", "back_pos", back,
			"detail", "the index of changes' words is first built newest first; a listing narrowed by "+
				"words says how far back it reaches until it is whole")
	}
	if reached == "" {
		log.InfoContext(ctx, "change_index_build_whole",
			"detail", "the first build of the index of changes' words has reached the oldest change")
	}
	return max(len(read), 1), nil
}

// reachBelow walks ids below from, newest first, summing description bytes of rows at or below back:
// reached is where they pass the budget (or a full read ends) and the next floor, "" when nothing
// older is left; read is every id in that stretch.
func (x *ChangeIndexer) reachBelow(ctx context.Context, tx *sql.Tx, from string, back int64) (reached string,
	read map[string]bool, held int, err error) {

	query, args := ChangeBackReachRead(from, ChangeIndexBatchRows)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return "", nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	read = map[string]bool{}
	var last string
	weighed := int64(0)
	for rows.Next() {
		var id string
		var position, size int64
		if err = rows.Scan(&id, &position, &size); err != nil {
			return "", nil, 0, err
		}
		read[id], last = true, id
		// A ROW ABOVE WHERE THE BUILD BEGAN is the forward walk's.
		if position > back {
			continue
		}
		held, weighed = held+1, weighed+size
		if weighed >= int64(x.back.n) {
			return id, read, held, rows.Err()
		}
	}
	if err = rows.Err(); err != nil || len(read) < ChangeIndexBatchRows {
		return "", read, held, err
	}
	return last, read, held, nil
}

// pruneChangeDocs removes the documents of ids in [reached, from) that read, every id the estate
// holds there, does not name: a copy replaced under an index keeps the documents of its own ids.
func pruneChangeDocs(ctx context.Context, tx *sql.Tx, reached, from string, read map[string]bool) error {
	low, high := make([]byte, 16), bytes.Repeat([]byte{0xff}, 17)
	var err error
	if reached != "" {
		if low, err = changeKey(reached); err != nil {
			return err
		}
	}
	if from != changeIDsAbove {
		if high, err = changeKey(from); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, changeHeldInRangeStatement, low, high)
	if err != nil {
		return fmt.Errorf("search: read the documents below %q: %w", from, err)
	}
	var gone []int64
	for rows.Next() {
		var doc int64
		var key []byte
		if err = rows.Scan(&doc, &key); err != nil {
			_ = rows.Close()
			return fmt.Errorf("search: read a document below %q: %w", from, err)
		}
		id, idErr := changeID(key)
		if idErr != nil {
			_ = rows.Close()
			return idErr
		}
		if !read[id] {
			gone = append(gone, doc)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("search: read the documents below %q: %w", from, err)
	}
	for _, doc := range gone {
		if err = removeChangeDoc(ctx, tx, doc); err != nil {
			return err
		}
	}
	return nil
}

// backBatch reads every change below from down to reached, at or below back, newest minted first.
func (x *ChangeIndexer) backBatch(ctx context.Context, tx *sql.Tx, from, reached string,
	back int64) ([]changeDoc, error) {

	query, args := ChangeBackBatchRead(from, reached, back)
	return x.readDocs(ctx, tx, query, args)
}

// heldChangeDoc is what the index holds for one change.
type heldChangeDoc struct {
	doc                  int64
	repoID, owner, state string
	hash                 []byte
}

// changePosting is one term of one document, at its slot.
type changePosting struct {
	slot int64
	term string
}

// writeChangeDoc brings one change's document in step with its row, answering the postings it is
// owed: a new one keyed by when its change was minted and written whole, a changed text's slot range
// cleared and written again, a moved scope rewritten alone, and an unchanged one left as it is.
func writeChangeDoc(ctx context.Context, tx *sql.Tx, doc changeDoc) ([]changePosting, error) {
	key, err := changeKey(doc.id)
	if err != nil {
		return nil, err
	}
	hash := changeTextHash(doc.title, doc.body)
	var held heldChangeDoc
	switch scanErr := tx.QueryRowContext(ctx, changeHeldDocStatement, key).
		Scan(&held.doc, &held.repoID, &held.owner, &held.state, &held.hash); {
	case errors.Is(scanErr, sql.ErrNoRows):
		terms, termsErr := changeTerms(doc)
		if termsErr != nil {
			return nil, termsErr
		}
		at, keyErr := docKeyFor(ctx, tx, key)
		if keyErr != nil {
			return nil, fmt.Errorf("search: key change %s: %w", doc.id, keyErr)
		}
		if _, execErr := tx.ExecContext(ctx, changeDocInsertStatement, at, key, doc.repoID, doc.owner,
			doc.state, hash); execErr != nil {
			return nil, fmt.Errorf("search: index change %s: %w", doc.id, execErr)
		}
		return postingsOf(at, terms), nil
	case scanErr != nil:
		return nil, fmt.Errorf("search: read the indexed change %s: %w", doc.id, scanErr)
	}
	if string(held.hash) == string(hash) {
		if held.repoID == doc.repoID && held.owner == doc.owner && held.state == doc.state {
			return nil, nil
		}
		if _, err = tx.ExecContext(ctx, changeDocScopeStatement, doc.repoID, doc.owner, doc.state,
			held.doc); err != nil {
			return nil, fmt.Errorf("search: move the scope of indexed change %s: %w", doc.id, err)
		}
		return nil, nil
	}
	terms, err := changeTerms(doc)
	if err != nil {
		return nil, err
	}
	if err = clearChangePostings(ctx, tx, held.doc); err != nil {
		return nil, fmt.Errorf("search: clear the postings of change %s: %w", doc.id, err)
	}
	if _, err = tx.ExecContext(ctx, changeDocRewriteStatement, doc.repoID, doc.owner, doc.state, hash,
		held.doc); err != nil {
		return nil, fmt.Errorf("search: re-index change %s: %w", doc.id, err)
	}
	return postingsOf(held.doc, terms), nil
}

// naturalChangeDocKey is the key a change's id names: its millisecond shifted five bits plus the five
// bits after the version nibble, false for a millisecond outside the natural range.
func naturalChangeDocKey(key []byte) (int64, bool) {
	var stamp [8]byte
	copy(stamp[2:], key[:6])
	ms := int64(binary.BigEndian.Uint64(stamp[:]))
	if ms < changeDocMinMs || ms >= changeDocEndMs {
		return 0, false
	}
	return ms<<changeDocTieBits | int64(key[6]&0x0f)<<1 | int64(key[7]>>7), true
}

// docKeyFor is the key a new document of the change key takes: its natural key, else the first free
// one in the [ChangeDocProbe] keys after it, else the arena's next. A batch writes its documents one
// after another in one transaction, so a key one of them took is seen by the next.
func docKeyFor(ctx context.Context, tx *sql.Tx, key []byte) (int64, error) {
	if natural, ok := naturalChangeDocKey(key); ok {
		free, found, err := firstFreeDocKey(ctx, tx, natural,
			min(natural+ChangeDocProbe, changeDocEndMs<<changeDocTieBits))
		if found || err != nil {
			return free, err
		}
	}
	var top int64
	switch err := tx.QueryRowContext(ctx, changeDocArenaTopStatement, changeDocArenaTop).Scan(&top); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read the arena's highest key: %w", err)
	case top+1 >= changeDocArenaTop:
		return 0, errors.New("the arena of keys is full")
	}
	return top + 1, nil
}

// firstFreeDocKey is the least key in [from, end) no document holds, false where every one is taken.
func firstFreeDocKey(ctx context.Context, tx *sql.Tx, from, end int64) (int64, bool, error) {
	rows, err := tx.QueryContext(ctx, changeDocKeysStatement, from, end)
	if err != nil {
		return 0, false, fmt.Errorf("read the keys after %d: %w", from, err)
	}
	defer func() { _ = rows.Close() }()
	next := from
	for rows.Next() {
		var taken int64
		if err = rows.Scan(&taken); err != nil {
			return 0, false, fmt.Errorf("read the keys after %d: %w", from, err)
		}
		if taken > next {
			break
		}
		next = taken + 1
	}
	if err = rows.Err(); err != nil {
		return 0, false, fmt.Errorf("read the keys after %d: %w", from, err)
	}
	return next, next < end, nil
}

// postingsOf is document doc's postings, each term at its place in the document's slot range.
func postingsOf(doc int64, terms []string) []changePosting {
	out := make([]changePosting, len(terms))
	for i, term := range terms {
		out[i] = changePosting{slot: doc<<changeSlotBits | int64(i), term: term}
	}
	return out
}

// writeChangePostings writes a batch's postings in the order of their slot, one statement per chunk
// ([store.InsertRows]): a new document's postings land at the end of the table's key.
func writeChangePostings(ctx context.Context, tx *sql.Tx, postings []changePosting, maxVariables int) error {
	slices.SortFunc(postings, func(a, b changePosting) int { return cmp.Compare(a.slot, b.slot) })
	if _, err := store.InsertRows(ctx, tx, maxVariables, changePostingsInsert, `(?, ?)`, "", len(postings),
		func(i int) []any { return []any{postings[i].slot, postings[i].term} }); err != nil {
		return fmt.Errorf("search: write %d postings: %w", len(postings), err)
	}
	return nil
}

// clearChangePostings deletes document doc's postings, its whole slot range.
func clearChangePostings(ctx context.Context, tx *sql.Tx, doc int64) error {
	_, err := tx.ExecContext(ctx, changeClearPostingsStatement, doc<<changeSlotBits, (doc+1)<<changeSlotBits)
	return err
}

// removeChangeDoc drops one document and its postings, by its slot range.
func removeChangeDoc(ctx context.Context, tx *sql.Tx, doc int64) error {
	if err := clearChangePostings(ctx, tx, doc); err != nil {
		return fmt.Errorf("search: clear the postings of an indexed change: %w", err)
	}
	if _, err := tx.ExecContext(ctx, changeRemoveDocStatement, doc); err != nil {
		return fmt.Errorf("search: remove an indexed change: %w", err)
	}
	return nil
}

// changeKey is a change's id as the index keys it: its UUID's 16 bytes, which sort as its canonical
// text does. Every change id is minted a UUID, so one that is none fails the batch, naming it.
func changeKey(id string) ([]byte, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("search: change id %q is not a UUID: %w", id, err)
	}
	return parsed[:], nil
}

// changeID is [changeKey]'s inverse.
func changeID(key []byte) (string, error) {
	parsed, err := uuid.FromBytes(key)
	if err != nil {
		return "", fmt.Errorf("search: an indexed change's key is not a UUID's 16 bytes: %w", err)
	}
	return parsed.String(), nil
}

// changeTextHash is a SHA-256 of exactly what decides a change's terms: NUL separates no term.
func changeTextHash(title, body string) []byte {
	sum := sha256.Sum256([]byte(title + "\x00" + body))
	return sum[:]
}

// changeTerms is a change's terms, the union of its title's and its description's, sorted. More than
// [ChangeDocTermSlots] fails the batch, naming the change, rather than lose a word.
func changeTerms(doc changeDoc) ([]string, error) {
	set := textindex.Analyze(doc.title)
	if set == nil {
		set = map[string]int{}
	}
	maps.Copy(set, textindex.Analyze(doc.body))
	if len(set) > ChangeDocTermSlots {
		return nil, fmt.Errorf("search: change %s holds %d terms, past the %d one document has room for",
			doc.id, len(set), ChangeDocTermSlots)
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// followPurges drops, in one step of a rotation over the repositories this index holds documents
// for, one drop step of documents of the first whose code_repos row is gone, and reports them.
func (x *ChangeIndexer) followPurges(ctx context.Context, now time.Time) (int, error) {
	repos, err := x.nextRepos(ctx)
	if err != nil {
		return 0, err
	}
	if len(repos) == 0 {
		x.repoCursor = ""
		return 0, nil
	}
	var (
		kept    map[string]bool
		settled int64
	)
	if err = x.estate.Read(ctx, func(tx *sql.Tx) (err error) {
		kept, settled = map[string]bool{}, 0
		prefix, err := statelog.PrefixIn(ctx, tx, x.code)
		if err != nil {
			return err
		}
		settled = prefix.Settled.Packed()
		query, args := ChangeReposRead(repos)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return err
			}
			kept[id] = true
		}
		return rows.Err()
	}); err != nil {
		return 0, fmt.Errorf("search: read which repositories the change index holds are still kept: %w", err)
	}
	for i, repo := range repos {
		if kept[repo] {
			continue
		}
		dropped, dropErr := x.dropRepo(ctx, now, repo, settled)
		if dropErr != nil {
			return 0, dropErr
		}
		// THE CURSOR STAYS BEFORE IT, so the next sweep goes on dropping the same repository.
		if i > 0 {
			x.repoCursor = repos[i-1]
		}
		return dropped, nil
	}
	x.repoCursor = repos[len(repos)-1]
	if len(repos) < ChangePurgeFollowRepos {
		x.repoCursor = ""
	}
	return 0, nil
}

// nextRepos is the next step of the purge follower's rotation: the distinct repositories after the
// cursor, one seek each of change_docs_repo_idx.
func (x *ChangeIndexer) nextRepos(ctx context.Context) ([]string, error) {
	var repos []string
	err := x.db.Read(ctx, func(tx *sql.Tx) error {
		repos = nil
		after := x.repoCursor
		for range ChangePurgeFollowRepos {
			var repo string
			switch err := tx.QueryRowContext(ctx, changeNextRepoStatement, after).Scan(&repo); {
			case errors.Is(err, sql.ErrNoRows):
				return nil
			case err != nil:
				return err
			}
			repos = append(repos, repo)
			after = repo
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search: read the next repositories the change index holds: %w", err)
	}
	return repos, nil
}

// dropRepo drops one drop step of a purged repository's documents, recording the settled position the
// purge was seen at.
func (x *ChangeIndexer) dropRepo(ctx context.Context, now time.Time, repo string, settled int64) (int, error) {
	dropped := 0
	began := time.Now()
	err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		docs, err := heldDocs(ctx, tx, changeRepoDocsStatement, repo, x.drop.n)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if err = removeChangeDoc(ctx, tx, doc); err != nil {
				return err
			}
		}
		dropped = len(docs)
		_, err = tx.ExecContext(ctx, changeSeenStatement, settled, now.UTC().UnixMilli())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("search: drop the indexed changes of purged repository %s: %w", repo, err)
	}
	x.drop.after(x.since(began))
	return dropped, nil
}

// heldDocs reads the keys of the documents statement names, bound by args.
func heldDocs(ctx context.Context, tx *sql.Tx, statement string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search: read the indexed changes to drop: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var doc int64
		if err = rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("search: read an indexed change to drop: %w", err)
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// changeIndexState is the index's state row.
type changeIndexState struct {
	builtFrom              string
	through, seen, backPos int64
	backFloor              string
}

// began reports an index whose first build has begun: its backward walk has a place to go on from.
func (s changeIndexState) began() bool { return s.backPos >= 0 }

// indexState reads the state row; its absence is a node estate behind its migrations.
func (x *ChangeIndexer) indexState(ctx context.Context) (changeIndexState, error) {
	var state changeIndexState
	err := x.db.Read(ctx, func(tx *sql.Tx) error {
		var readErr error
		state, readErr = readChangeIndexState(ctx, tx)
		return readErr
	})
	return state, err
}

// readChangeIndexState reads the state row inside tx.
func readChangeIndexState(ctx context.Context, tx *sql.Tx) (changeIndexState, error) {
	var state changeIndexState
	err := tx.QueryRowContext(ctx, changeStateStatement).
		Scan(&state.builtFrom, &state.through, &state.seen, &state.backPos, &state.backFloor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return changeIndexState{}, errors.New("search: the change index has no state row; the node " +
			"estate is behind its migrations")
	case err != nil:
		return changeIndexState{}, fmt.Errorf("search: read the change index's state: %w", err)
	}
	return state, nil
}

// estateInstance is the replicated estate's name for the copy under this node.
func (x *ChangeIndexer) estateInstance(ctx context.Context) (string, error) {
	var instance string
	err := x.estate.Read(ctx, func(tx *sql.Tx) (err error) {
		instance, err = statelog.EstateInstance(ctx, tx)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("search: read which copy of the replicated estate the change index "+
			"walks: %w", err)
	}
	return instance, nil
}

// restart begins the first build again over the copy instance, keeping every document: each walk
// rewrites what it reaches by id, and [pruneChangeDocs] removes what the copy does not hold.
func (x *ChangeIndexer) restart(ctx context.Context, now time.Time, state changeIndexState, instance string,
	asked bool) error {

	if err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, changeRestartStatement, instance, now.UTC().UnixMilli())
		return err
	}); err != nil {
		return fmt.Errorf("search: begin the change index's first build again: %w", err)
	}
	x.repoCursor = ""
	if asked || state.builtFrom != "" {
		log.InfoContext(ctx, "change_index_rebuilding", "asked", asked, "built_from", state.builtFrom,
			"estate_instance", instance,
			"detail", "the index of changes' words begins its first build again over the copy of the estate "+
				"under it, newest first; a listing narrowed by words says building until it begins")
	}
	return nil
}

// Run sweeps until ctx ends, straight back while a sweep works and an idle tick apart otherwise.
func (x *ChangeIndexer) Run(ctx context.Context) {
	for {
		worked, err := x.Sweep(ctx, time.Now())
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.WarnContext(ctx, "change_index_step_failed", "error", err.Error(),
				"detail", "the index of changes' words is behind this node's rows; a listing narrowed by "+
					"words judges what moved since and says building, and the index catches up on the "+
					"next sweep")
		case worked > 0:
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(indexIdle):
		}
	}
}
