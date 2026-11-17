package search

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
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

// ChangeIndexBatchRows is the most rows the walk's reach read takes past its watermark. The batch is
// widened to every row at its last position, since one push record moves up to some 1 300 changes there.
const ChangeIndexBatchRows = 512

// ChangeIndexBatchBytes is the description bytes a walk batch takes before it is cut at that row's
// position: some 170 changes of 150 words, about 1 s of writing at 10 000 indexed and 2.7 s by 30 000.
const ChangeIndexBatchBytes = 256 << 10

// ChangeDropBatch is how many documents one drop step removes, a rebuild's or a purged repository's.
const ChangeDropBatch = 64

// ChangeDocTermSlots is how many terms one document's postings have room for: a posting's slot is its
// document's seq shifted 16 bits plus the term's place, so a document's postings are one key range.
// More than the 16 512 runs Gitlet's largest title and description hold, so every term is kept.
const ChangeDocTermSlots = 1 << changeSlotBits

// changeSlotBits is the shift a slot spells; the statements spell it as the literal 16.
const changeSlotBits = 16

// ChangePurgeFollowRepos is how many repositories one sweep's purge follower visits: one seek each of
// this node's index and one estate statement for all, so a thousand come round in sixteen idle sweeps.
const ChangePurgeFollowRepos = 64

// MaxChangeTextScan is how many postings of a word, or documents of a repository, an owner or a state,
// one search reads of what it drives on; past it the answer says Truncated (maxPostingScan's reason).
const MaxChangeTextScan = 5_000

// ChangeIndexer keeps this node's index of Gitlet changes' words in step with the replicated rows: a
// forward walk over code_changes.position up to the code log's applied prefix, a purge follower, a
// rebuild on a new copy.
type ChangeIndexer struct {
	db         *store.DB              // this node's own file, where the index is kept
	estate     store.ReplicatedReader // the replicated rows it walks
	code       statelog.Domain        // the code domain, whose applied prefix bounds the walk
	repoCursor string                 // the purge follower's rotation, in memory as chat's pruneCursor
	rebuild    atomic.Bool            // a rebuild asked for, which the loop honours
	scan       int                    // MaxChangeTextScan, or a test's
}

// NewChangeIndexer builds one over this node's own store and the replicated estate beside it. code is
// the domain whose rows it walks (gitlet.Domain), handed in since this package cannot import it.
func NewChangeIndexer(db *store.DB, estate store.ReplicatedReader, code statelog.Domain) *ChangeIndexer {
	return &ChangeIndexer{db: db, estate: estate, code: code, scan: MaxChangeTextScan}
}

// RequestRebuild asks the index to drop what it holds and walk the changes again, for an estate
// replaced under the node; [ChangeIndexer.Holding] answers Rebuilding from this call on.
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

	changeStateStatement = `SELECT built_from, through_pos, seen_pos, last_seq, rebuilding
		FROM change_index_state WHERE id = 1`
	// changeAdvanceStatement moves the watermark, records the settled position the batch read and the
	// highest number it gave a document.
	changeAdvanceStatement = `UPDATE change_index_state
		SET through_pos = MAX(through_pos, ?), seen_pos = MAX(seen_pos, ?), last_seq = ?, updated_at = ?
		WHERE id = 1`
	// changeSeenStatement is a purge drop's record of the settled position it was decided from.
	changeSeenStatement = `UPDATE change_index_state SET seen_pos = MAX(seen_pos, ?), updated_at = ?
		WHERE id = 1`
	// changeBeginRebuildStatement is a rebuild's durable mark, in one statement as chat's is.
	changeBeginRebuildStatement = `UPDATE change_index_state
		SET rebuilding = 1, through_pos = 0, seen_pos = 0, last_seq = 0, updated_at = ? WHERE id = 1`
	// changeRecordInstanceStatement names the copy an index holding nothing is about to be built from.
	changeRecordInstanceStatement = `UPDATE change_index_state SET built_from = ?, updated_at = ?
		WHERE id = 1 AND through_pos = 0 AND rebuilding = 0`
	changeRebuiltStatement = `UPDATE change_index_state SET rebuilding = 0, updated_at = ? WHERE id = 1`

	changeHeldDocStatement = `SELECT seq, state, text_hash FROM change_docs INDEXED BY change_docs_id_idx
		WHERE id = ?`
	changeDocInsertStatement = `INSERT INTO change_docs (seq, id, repo_id, owner, state, text_hash)
		VALUES (?, ?, ?, ?, ?, ?)`
	changeDocRewriteStatement = `UPDATE change_docs SET state = ?, text_hash = ? WHERE seq = ?`
	changeDocStateStatement   = `UPDATE change_docs SET state = ? WHERE seq = ?`
	changeRemoveDocStatement  = `DELETE FROM change_docs WHERE seq = ?`
	// changeClearPostingsStatement deletes one document's postings, its slot range, by the table's own
	// key: no removal needs the words the document held.
	changeClearPostingsStatement = `DELETE FROM change_postings WHERE slot >= ? AND slot < ?`
	changePostingsInsert         = `INSERT INTO change_postings (slot, term) VALUES`

	changeNextRepoStatement = `SELECT repo_id FROM change_docs INDEXED BY change_docs_repo_idx
		WHERE repo_id > ? ORDER BY repo_id LIMIT 1`
	changeRepoDocsStatement = `SELECT seq FROM change_docs INDEXED BY change_docs_repo_idx
		WHERE repo_id = ? LIMIT ?`
	// changeDropBatchStatement is a rebuild's next batch: a WALK by design, as chat's is, since every
	// document it names is gone by the next one.
	changeDropBatchStatement = `SELECT seq FROM change_docs LIMIT ?`
	// changeRebuiltDropStatement discards, at a rebuild's end, any posting no document's drop took: a
	// WALK of a table that should then be empty.
	changeRebuiltDropStatement = `DELETE FROM change_postings`
)

// changeDoc is one change on its way into the index.
type changeDoc struct {
	id                   string
	repoID, owner, state string
	title, body          string
}

// Sweep advances the index by one step and reports how much it did: while a rebuild is dropping the
// index, one batch of that drop; else one step of the purge follower and one batch of the walk. Zero
// with no error is an idle index, which [ChangeIndexer.Run] waits on.
func (x *ChangeIndexer) Sweep(ctx context.Context, now time.Time) (int, error) {
	instance, err := x.estateInstance(ctx)
	if err != nil {
		return 0, err
	}
	if x.rebuild.Load() {
		if beginErr := x.beginRebuild(ctx, now, "requested"); beginErr != nil {
			return 0, beginErr
		}
		// Taken once its mark commits, so no instant between the two reads as built.
		x.rebuild.Store(false)
	}
	state, err := x.indexState(ctx)
	if err != nil {
		return 0, err
	}
	if !state.rebuilding && state.builtFrom != instance {
		if state.rebuilding, err = x.followInstance(ctx, now, state, instance); err != nil {
			return 0, err
		}
	}
	if state.rebuilding {
		return x.dropStep(ctx, now)
	}
	dropped, err := x.followPurges(ctx, now)
	if err != nil {
		return 0, err
	}
	walked, err := x.advance(ctx, now, state.through)
	return dropped + walked, err
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
		if reach, err = x.reach(ctx, tx, through); err != nil {
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
	maxVariables := x.db.Caps().MaxVariables
	err = x.db.Tx(ctx, func(tx *sql.Tx) error {
		// THE NUMBERS CONTINUE from what the last committed batch left, read in this transaction.
		state, stateErr := readChangeIndexState(ctx, tx)
		if stateErr != nil {
			return stateErr
		}
		lastSeq := state.lastSeq
		var postings []changePosting
		for _, doc := range docs {
			written, writeErr := writeChangeDoc(ctx, tx, doc, &lastSeq)
			if writeErr != nil {
				return writeErr
			}
			postings = append(postings, written...)
		}
		if writeErr := writeChangePostings(ctx, tx, postings, maxVariables); writeErr != nil {
			return writeErr
		}
		_, execErr := tx.ExecContext(ctx, changeAdvanceStatement, reach, settled, lastSeq,
			now.UTC().UnixMilli())
		return execErr
	})
	if err != nil {
		return 0, fmt.Errorf("search: index a batch of changes: %w", err)
	}
	return len(docs), nil
}

// reach is the position the next batch past through ends on: the row at which the descriptions read
// pass [ChangeIndexBatchBytes], or the last of [ChangeIndexBatchRows]; through where none moved past it.
func (x *ChangeIndexer) reach(ctx context.Context, tx *sql.Tx, through int64) (int64, error) {
	query, args := ChangeReachRead(through, ChangeIndexBatchRows)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	reach, weighed := through, int64(0)
	for rows.Next() {
		var position, bytes int64
		if scanErr := rows.Scan(&position, &bytes); scanErr != nil {
			return 0, scanErr
		}
		reach, weighed = position, weighed+bytes
		if weighed >= ChangeIndexBatchBytes {
			break
		}
	}
	return reach, rows.Err()
}

// batch reads every change past through up to reach, so a batch ends on a whole position.
func (x *ChangeIndexer) batch(ctx context.Context, tx *sql.Tx, through, reach int64) ([]changeDoc, error) {
	query, args := ChangeBatchRead(through, reach)
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

// heldChangeDoc is what the index holds for one change.
type heldChangeDoc struct {
	seq   int64
	state string
	hash  []byte
}

// changePosting is one term of one document, at its slot.
type changePosting struct {
	slot int64
	term string
}

// writeChangeDoc brings one change's document in step with its row, answering the postings it is
// owed: a new one numbered after *lastSeq and written whole, a changed text's slot range cleared and
// written again, a moved state rewritten alone, and an unchanged one, a head move's, left as it is.
func writeChangeDoc(ctx context.Context, tx *sql.Tx, doc changeDoc, lastSeq *int64) ([]changePosting, error) {
	key, err := changeKey(doc.id)
	if err != nil {
		return nil, err
	}
	hash := changeTextHash(doc.title, doc.body)
	var held heldChangeDoc
	switch scanErr := tx.QueryRowContext(ctx, changeHeldDocStatement, key).
		Scan(&held.seq, &held.state, &held.hash); {
	case errors.Is(scanErr, sql.ErrNoRows):
		terms, termsErr := changeTerms(doc)
		if termsErr != nil {
			return nil, termsErr
		}
		*lastSeq++
		if _, execErr := tx.ExecContext(ctx, changeDocInsertStatement, *lastSeq, key, doc.repoID, doc.owner,
			doc.state, hash); execErr != nil {
			return nil, fmt.Errorf("search: index change %s: %w", doc.id, execErr)
		}
		return postingsOf(*lastSeq, terms), nil
	case scanErr != nil:
		return nil, fmt.Errorf("search: read the indexed change %s: %w", doc.id, scanErr)
	}
	if string(held.hash) == string(hash) {
		if held.state == doc.state {
			return nil, nil
		}
		if _, err = tx.ExecContext(ctx, changeDocStateStatement, doc.state, held.seq); err != nil {
			return nil, fmt.Errorf("search: move the state of indexed change %s: %w", doc.id, err)
		}
		return nil, nil
	}
	terms, err := changeTerms(doc)
	if err != nil {
		return nil, err
	}
	if err = clearChangePostings(ctx, tx, held.seq); err != nil {
		return nil, fmt.Errorf("search: clear the postings of change %s: %w", doc.id, err)
	}
	if _, err = tx.ExecContext(ctx, changeDocRewriteStatement, doc.state, hash, held.seq); err != nil {
		return nil, fmt.Errorf("search: re-index change %s: %w", doc.id, err)
	}
	return postingsOf(held.seq, terms), nil
}

// postingsOf is document seq's postings, each term at its place in the document's slot range.
func postingsOf(seq int64, terms []string) []changePosting {
	out := make([]changePosting, len(terms))
	for i, term := range terms {
		out[i] = changePosting{slot: seq<<changeSlotBits | int64(i), term: term}
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

// clearChangePostings deletes document seq's postings, its whole slot range.
func clearChangePostings(ctx context.Context, tx *sql.Tx, seq int64) error {
	_, err := tx.ExecContext(ctx, changeClearPostingsStatement, seq<<changeSlotBits, (seq+1)<<changeSlotBits)
	return err
}

// removeChangeDoc drops one document and its postings, by its slot range.
func removeChangeDoc(ctx context.Context, tx *sql.Tx, seq int64) error {
	if err := clearChangePostings(ctx, tx, seq); err != nil {
		return fmt.Errorf("search: clear the postings of an indexed change: %w", err)
	}
	if _, err := tx.ExecContext(ctx, changeRemoveDocStatement, seq); err != nil {
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
// for, up to [ChangeDropBatch] documents of the first whose code_repos row is gone, and reports them.
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

// dropRepo drops up to [ChangeDropBatch] of a purged repository's documents, recording the settled
// position the purge was seen at.
func (x *ChangeIndexer) dropRepo(ctx context.Context, now time.Time, repo string, settled int64) (int, error) {
	dropped := 0
	err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		docs, err := heldDocs(ctx, tx, changeRepoDocsStatement, repo, ChangeDropBatch)
		if err != nil {
			return err
		}
		for _, seq := range docs {
			if err = removeChangeDoc(ctx, tx, seq); err != nil {
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
	return dropped, nil
}

// heldDocs reads the numbers of the documents statement names, bound by args.
func heldDocs(ctx context.Context, tx *sql.Tx, statement string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search: read the indexed changes to drop: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var seq int64
		if err = rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("search: read an indexed change to drop: %w", err)
		}
		out = append(out, seq)
	}
	return out, rows.Err()
}

// changeIndexState is the index's state row.
type changeIndexState struct {
	builtFrom              string
	through, seen, lastSeq int64
	rebuilding             bool
}

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
	var rebuilding int64
	err := tx.QueryRowContext(ctx, changeStateStatement).
		Scan(&state.builtFrom, &state.through, &state.seen, &state.lastSeq, &rebuilding)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return changeIndexState{}, errors.New("search: the change index has no state row; the node " +
			"estate is behind its migrations")
	case err != nil:
		return changeIndexState{}, fmt.Errorf("search: read the change index's state: %w", err)
	}
	state.rebuilding = rebuilding != 0
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

// beginRebuild writes a rebuild's durable mark.
func (x *ChangeIndexer) beginRebuild(ctx context.Context, now time.Time, reason string) error {
	if err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, changeBeginRebuildStatement, now.UTC().UnixMilli())
		return err
	}); err != nil {
		return fmt.Errorf("search: mark the change index for a rebuild: %w", err)
	}
	x.repoCursor = ""
	log.InfoContext(ctx, "change_index_rebuilding", "reason", reason,
		"detail", "the index of changes' words is being dropped and walked again; a listing narrowed "+
			"by words says building until the walk catches up")
	return nil
}

// followInstance records the copy a pristine index is built from, or begins the rebuild an index
// built from another copy owes, and reports whether one began.
func (x *ChangeIndexer) followInstance(ctx context.Context, now time.Time, state changeIndexState,
	instance string) (bool, error) {

	if state.through == 0 && !state.rebuilding {
		recorded := false
		if err := x.db.Tx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, changeRecordInstanceStatement, instance, now.UTC().UnixMilli())
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			recorded = n == 1
			return err
		}); err != nil {
			return false, fmt.Errorf("search: record the copy of the estate the change index walks: %w", err)
		}
		if recorded {
			return false, nil
		}
	}
	log.InfoContext(ctx, "change_index_estate_replaced",
		"built_from", state.builtFrom, "estate_instance", instance,
		"detail", "the index of changes' words was built from another copy of the replicated estate "+
			"than the one under it now, so it is rebuilt from this one")
	if err := x.beginRebuild(ctx, now, "estate_instance_moved"); err != nil {
		return false, err
	}
	return true, nil
}

// dropStep drops one batch of a rebuilding index, or ends the rebuild where none is left.
func (x *ChangeIndexer) dropStep(ctx context.Context, now time.Time) (int, error) {
	dropped, done := 0, false
	err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		dropped, done = 0, false
		docs, err := heldDocs(ctx, tx, changeDropBatchStatement, ChangeDropBatch)
		if err != nil {
			return err
		}
		if len(docs) == 0 {
			if _, err = tx.ExecContext(ctx, changeRebuiltDropStatement); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, changeRebuiltStatement, now.UTC().UnixMilli()); err != nil {
				return err
			}
			done = true
			return nil
		}
		for _, seq := range docs {
			if err = removeChangeDoc(ctx, tx, seq); err != nil {
				return err
			}
		}
		dropped = len(docs)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("search: drop a batch of the change index to rebuild it: %w", err)
	}
	if done {
		log.InfoContext(ctx, "change_index_dropped",
			"detail", "the index of changes' words is empty and its walk starts over")
		return 1, nil
	}
	return dropped, nil
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
