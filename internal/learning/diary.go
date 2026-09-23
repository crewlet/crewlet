package learning

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// DiaryKind is how long an observation is meant to live.
type DiaryKind string

const (
	// DiaryLong is a durable observation with no expiry.
	DiaryLong DiaryKind = "diary_long"
	// DiaryShort is an observation with a deadline — something true for
	// this sprint, this incident, this quarter.
	DiaryShort DiaryKind = "diary_short"
)

// MaxContentBytes bounds one written diary note, for EVERY writer.
//
// A note is meant to be re-read in a later turn's prompt, so its cost is paid
// on every turn that recalls it, not once — and it is read back into the
// relevance filter's candidate pool as well, which is what makes an unbounded
// row a bound on nothing. Two thousand is a long paragraph and a short page;
// past that the model is writing a document, and a document belongs in the
// knowledge base where colleagues can read it too.
//
// TWO THOUSAND BYTES, WHICH THE NAME DOES NOT SAY. Both enforcers spend it
// through len() on a Go string — [PersistDecider.write] here and the
// `reflect_and_persist` builtin's own guard, which takes this constant rather
// than declaring a second one — so the unit is bytes and the two agree, which
// is the property this constant exists for. The number is right either way:
// prose in English is one byte a character, so nothing about the paragraph
// this is sized against moves. What moves is prose that is not ASCII, where
// the cap falls at roughly 660 characters of CJK rather than 2 000, and a seat
// writing in one of those languages is asked to be about a third as long.
//
// THE BYTE IS THE UNIT ANY OF THIS CAN HONESTLY COUNT, which is why the fix is
// not a rune count here. What the cap is really rationing is prompt budget, so
// the true unit is tokens — and a token count needs a tokenizer per provider,
// at a boundary that has no provider in it. Between the two proxies, a byte is
// the one both writers and the store's own column already agree on; a rune
// count in one writer and a byte count in the other would be precisely the
// disagreement this constant was hoisted to end. See [perTurnDetail] for the
// same choice made for the same reason one layer up.
//
// HERE rather than beside either writer. There are two — the reflect_and_persist
// tool and the post-turn PersistDecider — and they disagreed: the tool refused
// an over-long note naming the limit, while the decider wrote whatever the
// classifier produced. One store, one rule, stated where the store is. The two
// writers still differ in what they DO about it, and that difference is
// deliberate: a model holding the text can tighten it and retry, so the tool
// refuses; the decider has no one to ask, so it skips and says so.
const MaxContentBytes = 2000

// DiaryEntry is one private observation a seat made about its own work.
//
// PRIVATE is the operative word, and the reason the table is called a diary
// rather than a memory: it is the seat's own log, not knowledge other seats
// can query. Every read is scoped to one agent id, and there is no cross-agent
// surface at all.
type DiaryEntry struct {
	ID string

	// AgentID is the DERIVED uuid (org name plus handle), not the handle.
	// Renaming a handle then cleanly orphans the old rows rather than
	// mixing them into the new identity's memory.
	AgentID string

	Kind    DiaryKind
	Content string

	// TTLUntil is the deadline for a short entry, zero for a long one.
	TTLUntil time.Time

	Source   string
	TurnID   string
	Metadata map[string]any

	// RetrievalCount and LastRetrievedAt are how often the entry has been
	// recalled, and when it last was, as [Diary.MarkRetrieved] records them.
	RetrievalCount  int
	LastRetrievedAt time.Time

	// Embedding is the vector [Diary.Recall] ranks on.
	//
	// FILLED AT THE WRITE, not by the caller: [Diary.Write] embeds
	// [DiaryEntry.Content] when the diary was built with [WithEmbedding]
	// and the entry carries no vector of its own. Both writers hand the
	// store a note and nothing else — [PersistDecider.write] and the
	// `reflect_and_persist` builtin — so the store is the one place the
	// rule can be stated once, for the same reason [MaxContentBytes] is
	// stated here rather than beside either of them: a vector one writer
	// produced and the other did not would make a note's recallability
	// depend on which path wrote it.
	//
	// ONE VECTOR, NOT A WINDOW SET, which is where this differs from
	// [Episode.Embeddings]: a note is bounded at [MaxContentBytes] by every
	// writer, and that bound is inside [EpisodeWindowBytes] — the window an
	// episode summary has to be split into — so there is nothing here to
	// split and no window count to carry.
	//
	// EMPTY IS A FIRST-CLASS STATE, and there are two ways to reach it: a
	// company that configures no providers.embeddings has no embedder to
	// give the diary, and an embedder that failed on this write leaves the
	// note without one. Either way the note is written and [Diary.Recall]
	// then matches nothing for that row, so the `## Personal memory` block's
	// similarity ∪ recency pool falls back to its recency half. That is the
	// right way round: the note cannot be reconstructed later and the vector
	// can, by nothing more than a re-embed.
	Embedding []float32

	CreatedAt time.Time
}

// Expired reports whether a short entry's deadline has passed.
func (d DiaryEntry) Expired(now time.Time) bool {
	return !d.TTLUntil.IsZero() && !now.Before(d.TTLUntil)
}

// Diary is a seat's private observation log.
type Diary struct {
	db *store.DB

	// embed fills [DiaryEntry.Embedding] on the way in, or is nil where a
	// company configured no vector backend. See [WithEmbedding].
	embed Embed
}

// DiaryOption configures a diary at construction.
type DiaryOption func(*Diary)

// WithEmbedding gives a diary the vector backend its writes need.
//
// OPTIONAL RATHER THAN AN ARGUMENT because most callers open a diary to READ
// — the dashboard's query source, the turn-start prefetch, the retention
// sweep — and a read takes its query vector from its own caller, so handing
// those three a provider would be a dependency none of them uses. Inside the
// engine every diary, read and write alike, is built by one constructor that
// passes this, so "does this one embed?" is not a per-site decision there.
//
// A nil embedder is allowed and means the same thing as omitting the option:
// a company with no providers.embeddings writes notes with no vector. See
// [DiaryEntry.Embedding] for what that costs.
func WithEmbedding(embed Embed) DiaryOption {
	return func(d *Diary) { d.embed = embed }
}

// NewDiary wraps a database handle.
func NewDiary(db *store.DB, opts ...DiaryOption) *Diary {
	d := &Diary{db: db}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Write records one observation.
//
// A DURABLE NOTE IS REFUSED, NOT MADE ROOM FOR, once the seat already keeps
// [DiaryLongCap] of them: the answer is a [*DiaryFullError] naming the cap,
// and nothing the seat holds is touched. See [DiaryLongCap] for why a refusal
// rather than an eviction.
//
// Writing an id that is already stored is a no-op that reports success, at
// the cap as below it: that write is a replay of one that already landed.
func (d *Diary) Write(ctx context.Context, e DiaryEntry) error {
	switch {
	case e.ID == "" || e.AgentID == "":
		return fmt.Errorf("learning: a diary entry needs an id and an agent")
	case e.Kind != DiaryLong && e.Kind != DiaryShort:
		// The column has a CHECK constraint, so an unknown kind would fail
		// at the driver with a message naming the constraint and not the
		// caller. Refusing here says which field and which values.
		return fmt.Errorf("learning: diary kind must be %q or %q, got %q",
			DiaryLong, DiaryShort, e.Kind)
	case e.Kind == DiaryShort && e.TTLUntil.IsZero():
		// A short entry with no deadline never expires, which makes it a
		// long entry wearing the wrong label — and one the expiry sweep
		// will never look at, because that scan is indexed on a non-NULL
		// ttl.
		return fmt.Errorf("learning: a %q entry needs a deadline", DiaryShort)
	case e.Kind == DiaryLong && !e.TTLUntil.IsZero():
		return fmt.Errorf("learning: a %q entry must not carry a deadline", DiaryLong)
	}

	// THE CAP IS READ TWICE, and only the second read decides. This one is
	// here so a note that is going to be refused does not spend an
	// embeddings call first; the one inside the insert's transaction is the
	// rule, because the embedding call below sits between the two and
	// another writer for the same seat can land in that gap.
	if e.Kind == DiaryLong {
		if err := refuseWhenFull(ctx, d.db.SQL(), e); err != nil {
			return err
		}
	}

	// THE VECTOR IS MADE HERE, before anything is encoded, and only when the
	// caller brought none: an entry that already carries one was given it by
	// whoever built it, and re-embedding would spend a provider call to
	// replace a value somebody chose.
	if len(e.Embedding) == 0 {
		e.Embedding = d.vector(ctx, e)
	}

	// Same policy as an episode's embedding, and for the same reason — see
	// Episodes.encodeEmbedding. A wrong width fails the write; a non-finite
	// component costs the vector and not the observation.
	var blob any
	if len(e.Embedding) > 0 {
		packed, err := d.db.EncodeVector(e.Embedding)
		switch {
		case errors.Is(err, store.ErrVectorNotFinite):
			log.Warn("diary_embedding_discarded", "entry", e.ID, "error", err.Error())
		case err != nil:
			return fmt.Errorf("learning: encode diary embedding: %w", err)
		default:
			blob = packed
		}
	}
	// ONE TRANSACTION for the count and the insert. [store.DB.Tx] takes the
	// file's write lock at BEGIN, so no other write can land between the
	// two, and the count this refuses on is the count the insert lands
	// against.
	err := d.db.Tx(ctx, func(tx *sql.Tx) error {
		if e.Kind == DiaryLong {
			if refused := refuseWhenFull(ctx, tx, e); refused != nil {
				return refused
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO agent_diary (id, agent_id, kind, content, ttl_until, source,
				turn_id, metadata, retrieval_count, last_retrieved_at, embedding, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, ?)
			ON CONFLICT (id) DO NOTHING`,
			e.ID, e.AgentID, string(e.Kind), e.Content, store.NullTime(e.TTLUntil),
			e.Source, e.TurnID, jsonObject(e.Metadata), blob, store.EncodeTime(e.CreatedAt))
		return err
	})
	var full *DiaryFullError
	switch {
	case errors.As(err, &full):
		return err
	case err != nil:
		return fmt.Errorf("learning: write diary entry %s: %w", e.ID, err)
	}
	return nil
}

// DiaryFullError is a durable note refused because its seat already keeps
// [DiaryLongCap] of them.
//
// Its text is what a seat reads: the `reflect_and_persist` builtin puts a
// failed write's error text in front of the model, so the message says what
// happened to the note and where else a fact can go.
type DiaryFullError struct {
	// AgentID is the seat, as the derived id the diary is keyed on.
	AgentID string
	// Held is how many durable notes the seat keeps. It can exceed the cap:
	// see [DiaryLongCap] for the one way a seat gets there.
	Held int
}

func (e *DiaryFullError) Error() string {
	return fmt.Sprintf("learning: this seat already keeps %d durable notes and %d "+
		"(learning.DiaryLongCap) is the most one seat may keep, so this note was not "+
		"kept, and none of the notes already kept was dropped to make room for it. "+
		"Every durable note is refused the same way while the seat keeps %d or more. "+
		"A fact your colleagues need as well belongs in the knowledge base",
		e.Held, DiaryLongCap, DiaryLongCap)
}

// refuseWhenFull answers a [*DiaryFullError] when the seat already keeps
// [DiaryLongCap] durable notes and e is not one of them.
//
// THE ID IS ASKED FIRST, so a replay of a note that already landed is not
// refused: the insert would have done nothing with it, and a refusal would
// tell the writer that a note it holds was never kept.
func refuseWhenFull(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, e DiaryEntry,
) error {
	var stored, held int
	if err := q.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM agent_diary WHERE id = ?),
			(SELECT count(*) FROM agent_diary WHERE agent_id = ? AND kind = 'diary_long')`,
		e.ID, e.AgentID).Scan(&stored, &held); err != nil {
		return fmt.Errorf("learning: count %s's durable diary notes: %w", e.AgentID, err)
	}
	if stored == 0 && held >= DiaryLongCap {
		return &DiaryFullError{AgentID: e.AgentID, Held: held}
	}
	return nil
}

// vector embeds a note's content, or reports none.
//
// NEVER AN ERROR, which is the whole policy: the note is what cannot be
// reconstructed and the vector is what can, so a provider that is rate
// limited, slow or misconfigured costs this note's similarity hits and never
// the note. The failure is logged where it happens, because a seat whose
// diary silently stopped being recallable looks exactly like a seat that
// learned nothing.
//
// NO SECOND DEADLINE. One note is one call — a note is bounded at
// [MaxContentBytes], so there is no window set to bound the way
// [DefaultEmbedTimeout] bounds an episode's — and the embeddings provider
// already bounds the call it makes. A timeout here would be a second opinion
// about one round trip, free to disagree with the first.
func (d *Diary) vector(ctx context.Context, e DiaryEntry) []float32 {
	if d.embed == nil || strings.TrimSpace(e.Content) == "" {
		return nil
	}
	v, err := d.embed(ctx, e.Content)
	if err != nil {
		log.WarnContext(ctx, "diary_embedding_failed", "entry", e.ID,
			"agent_id", e.AgentID, "error", err.Error(),
			"detail", "the note is stored without a vector, so recall will not "+
				"surface it; the recency half of the memory pool still will")
		return nil
	}
	// NIL RATHER THAN AN EMPTY SLICE, for the reason [Episodist.vector]
	// gives: only nil is what [DiaryEntry.Embedding] documents as "no
	// vector", and an empty one would reach the encoder as a vector of
	// nothing.
	if len(v) == 0 {
		return nil
	}
	return v
}

const diaryColumns = `id, agent_id, kind, content, ttl_until, source, turn_id,
	metadata, retrieval_count, last_retrieved_at, embedding, created_at`

func scanDiary(rows interface{ Scan(...any) error }) (DiaryEntry, error) {
	var (
		e                  DiaryEntry
		kind, metadata     string
		created            int64
		ttl, lastRetrieved sql.NullInt64
		embedding          []byte
	)
	if err := rows.Scan(&e.ID, &e.AgentID, &kind, &e.Content, &ttl, &e.Source,
		&e.TurnID, &metadata, &e.RetrievalCount, &lastRetrieved, &embedding, &created,
	); err != nil {
		return DiaryEntry{}, err
	}
	e.Kind = DiaryKind(kind)
	e.TTLUntil = store.TimeAt(ttl)
	e.LastRetrievedAt = store.TimeAt(lastRetrieved)
	e.CreatedAt = store.DecodeTime(created)
	if err := json.Unmarshal([]byte(metadata), &e.Metadata); err != nil {
		// The observation itself is what the seat needs; its metadata is
		// provenance. Losing the second must not cost the first.
		log.Warn("diary_metadata_undecodable", "entry", e.ID, "error", err)
	}
	if len(embedding) > 0 {
		if vec, err := store.DecodeVector(embedding); err == nil {
			e.Embedding = vec
		} else {
			log.Warn("diary_embedding_undecodable", "entry", e.ID, "error", err)
		}
	}
	return e, nil
}

// defaultDiaryListing is how many entries [Diary.Recent] returns for a caller
// that names no limit.
//
// A FLOOR, NOT A POLICY. Every production caller passes an explicit limit —
// the turn-start prefetch its own recency budget, the dashboard its page size,
// the memory tool a clamped tool argument — so this answers only a caller that
// asked for nothing, and it answers with a screenful rather than a page: the
// value of an unbounded default here is a seat's entire diary rendered into a
// prompt, which is the failure the limit exists for.
const defaultDiaryListing = 10

// Recent returns a seat's most recent LIVE entries, newest first.
//
// Live means unexpired. An expired short entry is filtered on READ as well as
// swept in the background, because the sweep runs on a timer and a memory that
// has just passed its deadline is exactly as wrong as one that passed it a
// week ago.
func (d *Diary) Recent(ctx context.Context, agentID string, now time.Time, limit int) ([]DiaryEntry, error) {
	if limit <= 0 {
		limit = defaultDiaryListing
	}
	rows, err := d.db.SQL().QueryContext(ctx,
		`SELECT `+diaryColumns+` FROM agent_diary
		 WHERE agent_id = ? AND (ttl_until IS NULL OR ttl_until > ?)
		 ORDER BY created_at DESC, id DESC LIMIT ?`,
		agentID, store.EncodeTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("learning: recent diary for %s: %w", agentID, err)
	}
	return collectDiary(rows)
}

// Recall returns a seat's most similar live entries.
//
// Same shape as episode recall and for the same reasons — a scan the database
// ranks, a relevance floor, a total order, a Go re-score over the rows that
// survive — but scoped to one agent id, which is what makes the diary private.
// See [Episodes.Recall] for why each of those is the way it is; the only thing
// this adds is the liveness predicate.
//
// Live means unexpired, applied here as well as by the background sweep,
// because the sweep runs on a timer and a memory that has just passed its
// deadline is exactly as wrong as one that passed it a week ago.
//
// It answers empty for a row with no vector, which is a real and supported
// state rather than a failure — see [DiaryEntry.Embedding] for the two ways
// a note reaches the store without one.
func (d *Diary) Recall(ctx context.Context, agentID string, q RecallQuery, now time.Time) ([]DiaryHit, error) {
	if agentID == "" {
		return nil, fmt.Errorf("learning: diary recall needs an agent")
	}
	if len(q.Embedding) == 0 {
		return nil, ErrNoEmbedding
	}
	limit, floor := q.Limit, q.MinSimilarity
	if limit <= 0 {
		limit = defaultRecallLimit
	}
	if floor == 0 {
		floor = defaultMinSimilarity
	}
	probe, width, err := vectorProbe(d.db, q.Embedding)
	if err != nil {
		return nil, fmt.Errorf("learning: diary recall for %s: %w", agentID, err)
	}
	rows, err := d.db.SQL().QueryContext(ctx,
		`SELECT `+diaryColumns+` FROM agent_diary WHERE id IN (
		    SELECT id FROM (
		        SELECT id, created_at,
		               vector_distance_cos(embedding, ?) AS distance
		        FROM agent_diary
		        WHERE agent_id = ?
		          AND embedding IS NOT NULL
		          AND length(embedding) = ?
		          AND (ttl_until IS NULL OR ttl_until > ?)
		    )
		    WHERE distance <= ?
		    ORDER BY distance ASC, created_at DESC, id DESC
		    LIMIT ?
		 )`,
		probe, agentID, width, store.EncodeTime(now), 1-floor, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: diary recall for %s: %w", agentID, err)
	}
	entries, err := collectDiary(rows)
	if err != nil {
		return nil, err
	}
	var hits []DiaryHit
	for _, e := range entries {
		sim, ok := cosine(q.Embedding, e.Embedding)
		if !ok || sim < floor {
			continue
		}
		hits = append(hits, DiaryHit{Entry: e, Similarity: sim})
	}
	rankDiary(hits)
	return hits, nil
}

// DiaryHit is one recalled observation with its similarity.
type DiaryHit struct {
	Entry      DiaryEntry
	Similarity float64
}

// rankDiary orders hits most-similar first, then newest, then by id — the
// same total order episode recall uses, for the same reason.
func rankDiary(hits []DiaryHit) {
	slices.SortFunc(hits, func(a, b DiaryHit) int {
		return cmp.Or(
			cmp.Compare(b.Similarity, a.Similarity),
			b.Entry.CreatedAt.Compare(a.Entry.CreatedAt),
			cmp.Compare(b.Entry.ID, a.Entry.ID),
		)
	})
}

// MarkRetrieved records that entries were recalled.
//
// Best effort and deliberately not part of Recall's return path: a seat that
// recalled a memory has had the benefit whether or not the counter moved, and
// failing a recall because its bookkeeping failed would trade the useful half
// of the operation for the statistical half.
//
// ONE STATEMENT over the whole set rather than one per id. The per-id loop
// this replaces returned on its first error, so a single failure silently
// abandoned every id after it — and the ids come from one filter pass, so
// there is no reason to visit them one round trip at a time.
func (d *Diary) MarkRetrieved(ctx context.Context, ids []string, at time.Time) {
	if len(ids) == 0 {
		return
	}
	// DEDUPED, because the count is "how many times this entry was
	// selected", and one filter pass selecting an id twice is one use.
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return
	}
	args := make([]any, 0, len(unique)+1)
	args = append(args, store.EncodeTime(at))
	for _, id := range unique {
		args = append(args, id)
	}
	query := `UPDATE agent_diary SET retrieval_count = retrieval_count + 1,
		 last_retrieved_at = ? WHERE id IN (?` +
		strings.Repeat(",?", len(unique)-1) + `)`
	if _, err := d.db.SQL().ExecContext(ctx, query, args...); err != nil {
		log.WarnContext(ctx, "diary_retrieval_not_recorded",
			"entries", len(unique), "error", err)
	}
}

// DiaryLongCap is how many durable entries one seat may keep.
//
// A CAP RATHER THAN A HORIZON, because a diary_long row is a fact the agent
// deliberately marked durable — "the release train is Thursdays", "this
// reviewer wants tests first" — and those do not stop being true after a
// quarter. Expiring them by age would delete exactly what the kind exists to
// protect.
//
// But they cannot be unbounded either: recall scans every embedded row a seat
// owns, once per turn, at 7.5 µs a row (the same measurement the episode
// threshold is set from, re-measured at the pin). Five hundred is that
// threshold's budget (about 3.9 ms at the start of every turn), and a
// seat holding five hundred durable facts about its own work is already far
// past what a person would.
//
// ENFORCED AT THE WRITE, BY REFUSAL: [Diary.Write] answers the next durable
// note with a [*DiaryFullError] naming this cap. Refused rather than made room
// for, because making room means deleting a fact the seat chose to keep
// without the seat ever being told, so it goes on acting as if it still knew
// it. A refusal is said to whoever is writing, while the note is still in
// their hands.
//
// A REFUSAL DOES NOT LIFT. Nothing in this build deletes a durable note —
// [Diary.Expire] deletes only notes with a deadline, and [Diary.Write] refuses
// a durable note that carries one — so a seat that reaches the cap has every
// later durable note refused from then on.
//
// A SEAT CAN HOLD MORE, by one route: memsync's hydration writes a seat's rows
// into this table directly rather than through [Diary.Write], so rows two
// nodes each wrote before either had the other's can meet on one node. Such a
// seat stays over the cap, and its durable notes are refused as at the cap.
//
// NO SWEEP TRIMS IT BACK, for two reasons. The seat is never told: a sweep
// runs outside every turn, so a note dropped there is a fact the seat chose to
// keep and goes on acting as if it still knew — the eviction this cap refuses
// to make at the write. And it would not stay dropped: memsync carries a
// seat's rows between nodes and carries no deletes, so a node that hydrates
// the seat again inserts a dropped row again. A seat over the cap therefore
// stays over it, which costs its recall scan the rows past the cap and costs
// the seat nothing it knows.
const DiaryLongCap = 500

// Expire deletes short entries whose deadline has passed.
func (d *Diary) Expire(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.db.SQL().ExecContext(ctx,
		`DELETE FROM agent_diary WHERE ttl_until IS NOT NULL AND ttl_until <= ?`,
		store.EncodeTime(now))
	if err != nil {
		return 0, fmt.Errorf("learning: expire diary: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("learning: expire diary: %w", err)
	}
	return n, nil
}

func collectDiary(rows *sql.Rows) ([]DiaryEntry, error) {
	defer rows.Close()
	var out []DiaryEntry
	for rows.Next() {
		e, err := scanDiary(rows)
		if err != nil {
			return nil, fmt.Errorf("learning: scan diary entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read diary: %w", err)
	}
	return out, nil
}

// jsonObject renders a map as a JSON object, never as null.
//
// Same reason as jsonList: the column is NOT NULL with a '{}' default, and a
// nil map marshals to "null", which then fails every JSON query against it.
func jsonObject(v map[string]any) string {
	if len(v) == 0 {
		return "{}"
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(blob)
}
