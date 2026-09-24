// Package learning is the agent-learning subsystem: what a seat remembers
// about its own work, who it worked with, and what it has taught itself.
//
// Everything here is BEST EFFORT by design. A seat whose memory is
// unreachable is a seat with less context, never a seat that cannot work —
// so a failed write is logged and a failed read answers empty. The one
// exception is stated at the call site that makes it.
package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
)

var log = logging.Get("learning")

// Kind tells the two row shapes in the episode table apart.
type Kind string

const (
	// KindRaw is one completed turn.
	KindRaw Kind = "raw"
	// KindCompacted is a cluster summary the lifecycle worker folds raw
	// rows into. Count is how many it replaces.
	KindCompacted Kind = "compacted"
)

// Episode is one completed agent turn, or one compacted cluster.
type Episode struct {
	ID        string
	Handle    string
	Role      string
	TaskID    string
	TurnID    string
	StartedAt time.Time
	EndedAt   time.Time

	PlanSummary   string
	TaskSummary   string
	ToolSequence  []string
	SkillsUsed    []string
	ReviewOutcome string
	Duration      time.Duration

	// Embeddings are the task summary's vectors, one per WINDOW of it, in
	// the order the windows were read — or nil when the embeddings provider
	// was unreachable at write time.
	//
	// SEVERAL, because a summary is not bounded and one vector represents
	// one subject: the episodist splits a long summary into overlapping
	// windows and embeds every one, so nothing is cut and every byte reaches
	// some vector. [Episodist.Reflect] fills it; see [EpisodeWindowBytes]
	// for the window and node migration 0030 for how the set is stored.
	//
	// A short summary — which is almost every one — is exactly one window,
	// so this holds a single vector and the row is byte-for-byte what it was
	// before windowing existed.
	//
	// Recall scores an episode as its NEAREST window (see [Episodes.Recall]),
	// never as a mean: a turn is worth recalling because one part of what it
	// did matches, not because all of it does.
	//
	// THE ORDINAL IS NOT AN ADDRESS. Nothing maps a window back to the slice
	// of text it came from, so a window the store refuses (a non-finite
	// vector) is dropped rather than held as a gap, and the rest still rank.
	//
	// Nil is a supported state, not a failure: recall skips such rows while
	// the time-window and outcome queries still surface them. A transient
	// outage must never cost an episode.
	Embeddings [][]float32

	// EmbeddingModel is the model that made Embeddings, and empty when there
	// are none — or when the row was written by a build or at a time that
	// did not record one. Recall compares a query only with the episodes of
	// its own model, so an empty one is left out of similarity ranking; see
	// [Embedder] for why a matching width is not enough.
	EmbeddingModel string

	Kind  Kind
	Count int

	// WorkKey is the identity of the work this turn did. Empty when the
	// turn had no ledgerable trigger — a scheduled fire, a sub-agent, a
	// sandbox resume.
	WorkKey string

	// ConversationKey is which conversation the turn served. Empty for
	// triggers with no derivable conversation, and always empty on a
	// compacted row, whose cluster spans conversations by construction.
	ConversationKey string

	// The compacted-row fields. All zero on a raw row.
	ExemplarTurnIDs   []string
	ConsolidatedInto  string
	CommonTaskPattern string
	CommonOutcome     string
	SuccessRate       float64
	SubjectsInvolved  []string
	NotablePatterns   string
}

// Episodes is the durable episode memory.
type Episodes struct{ db *store.DB }

// NewEpisodes wraps a database handle.
func NewEpisodes(db *store.DB) *Episodes { return &Episodes{db: db} }

const episodeInsertSQL = `
INSERT INTO episodes (
	id, agent_handle, agent_role, task_id, turn_id, started_at, ended_at,
	plan_summary, task_summary, tool_sequence, skills_used, review_outcome,
	duration_ms, embedding, embedding_windows, embedding_model, kind, count,
	exemplar_turn_ids, consolidated_into_skill_id, common_task_pattern,
	common_outcome, success_rate, subjects_involved, notable_patterns, work_key,
	conversation_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (agent_handle, work_key) DO NOTHING`

// episodeInsertArgs binds [episodeInsertSQL], once, for every writer of the
// table.
//
// ONE ARGUMENT LIST, and it is not tidiness: the statement is bound
// POSITIONALLY, so a second hand-written list is a second chance to pair a
// column with the wrong value — and the failure is silent wherever the two
// types agree, which for this table's fourteen TEXT columns is most of it. The
// lifecycle's compacted-row insert used to keep its own copy under a comment
// promising it matched; the promise is now the code.
//
// blob and windows come from [Episodes.encodeEmbedding] together, because they
// are one fact about one value: a blob of N packed vectors and a count that
// said anything but N would make recall read somebody else's bytes as a vector.
//
// THE MODEL IS WRITTEN ONLY BESIDE A BLOB. A row whose every window was
// refused lands with no vector, and a model named on it would be a claim
// about a vector that is not there.
func episodeInsertArgs(ep Episode, blob any, windows int) []any {
	model := ""
	if blob != nil {
		model = ep.EmbeddingModel
	}
	return []any{
		ep.ID, ep.Handle, ep.Role, ep.TaskID, ep.TurnID,
		store.EncodeTime(ep.StartedAt), store.EncodeTime(ep.EndedAt),
		ep.PlanSummary, ep.TaskSummary, jsonList(ep.ToolSequence), jsonList(ep.SkillsUsed),
		ep.ReviewOutcome, ep.Duration.Milliseconds(), blob, windows, store.NullText(model),
		string(ep.Kind), ep.Count, jsonList(ep.ExemplarTurnIDs),
		store.NullText(ep.ConsolidatedInto), ep.CommonTaskPattern, ep.CommonOutcome,
		ep.SuccessRate, jsonList(ep.SubjectsInvolved), ep.NotablePatterns,
		store.NullText(ep.WorkKey), store.NullText(ep.ConversationKey),
	}
}

// Append records one episode, at most once per (seat, work key).
//
// EXACTLY-ONCE ON THE WORK KEY, and it is a real unique index rather than a
// read-then-write: two writers racing inside one process cannot both see "not
// there" and both insert. A turn can legitimately be worked twice — a
// redelivery, or an honest re-run after the completion ledger fails open — and
// an episode keyed on nothing simply lands twice, then feeds every later
// recall and skill synthesis, weighting the agent's behaviour with an event
// that happened once.
//
// THE INDEX IS THIS NODE'S, like the table. That is the whole scope of the
// guarantee and it is the right one: episodes are a seat's memory, read by the
// node running that seat and never by a peer, so a duplicate written on two
// DIFFERENT nodes is two rows in two databases neither of which the other
// reads — no recall sees both. What is collapsed here is the duplicate one
// reader would otherwise be shown twice.
//
// An empty work key maps to SQL NULL, which the index treats as distinct from
// every other NULL — so an unkeyed turn is never deduped against another. That
// is the whole reason the column is nullable: the empty string would collide
// every unkeyed turn a seat ever ran onto one row.
//
// Reports whether the row was WRITTEN. False means a duplicate was collapsed,
// which is the guard working and not a failure.
func (e *Episodes) Append(ctx context.Context, ep Episode) (bool, error) {
	if ep.ID == "" || ep.Handle == "" {
		return false, fmt.Errorf("learning: an episode needs an id and a seat")
	}
	if ep.Kind == "" {
		ep.Kind = KindRaw
	}
	if ep.Count < 1 {
		ep.Count = 1
	}
	blob, windows, err := e.encodeEmbedding(ep.Embeddings)
	if err != nil {
		return false, err
	}
	res, err := e.db.SQL().ExecContext(ctx, episodeInsertSQL,
		episodeInsertArgs(ep, blob, windows)...)
	if err != nil {
		return false, fmt.Errorf("learning: append episode %s: %w", ep.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("learning: append episode %s: %w", ep.ID, err)
	}
	return n > 0, nil
}

// encodeEmbedding packs the window vectors END TO END and reports how many
// landed, refusing one of the wrong width and DEGRADING one that is not finite.
//
// The count is returned beside the bytes rather than derived from them, and
// node migration 0030 is where that argument is written down: the width is a
// runtime property, so `len(blob) / (4 * width)` can divide exactly under a
// model the row was not embedded with and hand recall a window count that is
// arithmetically clean and entirely wrong.
//
// The width is checked because the column is a plain BLOB: Turso does not
// enforce a declared vector width (measured), so a mismatched vector stores
// happily and then makes every distance query against it return nothing — a
// seat whose recall silently stops working, with no error anywhere. That is a
// configuration fault, and the write fails.
//
// A NaN or an infinity is a different thing and gets the opposite answer: that
// WINDOW is dropped and the rest are kept, exactly as if the provider had been
// unreachable for it. Failing the write instead would cost the episode, and an
// episode is the record of a turn that really happened — the one thing this
// subsystem may not lose to a bad response from an embeddings API. Storing it
// anyway is not an option either: [store.ErrVectorNotFinite] says why. Dropping
// one window rather than the whole set is what [Episode.Embeddings] means by
// the ordinal not being an address: nothing maps a vector back to its slice of
// text, so a shorter set still ranks the row on everything that did embed.
func (e *Episodes) encodeEmbedding(windows [][]float32) (any, int, error) {
	if len(windows) == 0 {
		return nil, 0, nil
	}
	var packed []byte
	kept := 0
	for at, window := range windows {
		if len(window) == 0 {
			continue
		}
		blob, err := e.db.EncodeVector(window)
		switch {
		case errors.Is(err, store.ErrVectorNotFinite):
			log.Warn("episode_embedding_window_discarded",
				"window", at, "windows", len(windows), "error", err.Error())
			continue
		case err != nil:
			// A WIDTH FAULT FAILS THE WRITE, because it is the same
			// fault for every window: the vectors came from one call
			// to one provider, so a second window would report the
			// same mismatch and the row must not land half-embedded
			// under a configuration nobody has noticed is wrong.
			return nil, 0, fmt.Errorf("learning: encode embedding window %d of %d: %w",
				at, len(windows), err)
		}
		packed = append(packed, blob...)
		kept++
	}
	if kept == 0 {
		// Every window was refused, which is the same outcome as an
		// unreachable provider: the row lands, recall skips it, and the
		// time-window queries still surface it.
		return nil, 0, nil
	}
	return packed, kept, nil
}

const episodeColumns = `id, agent_handle, agent_role, task_id, turn_id,
	started_at, ended_at, plan_summary, task_summary, tool_sequence,
	skills_used, review_outcome, duration_ms, embedding, embedding_windows,
	embedding_model, kind, count,
	exemplar_turn_ids, consolidated_into_skill_id, common_task_pattern,
	common_outcome, success_rate, subjects_involved, notable_patterns,
	work_key, conversation_key`

func scanEpisode(rows interface{ Scan(...any) error }) (Episode, error) {
	var (
		ep                                     Episode
		started, ended, durationMS             int64
		embedding                              []byte
		windows                                int
		model                                  sql.NullString
		kind                                   string
		toolSeq, skills, exemplars, subjects   string
		consolidated, workKey, conversationKey sql.NullString
	)
	if err := rows.Scan(
		&ep.ID, &ep.Handle, &ep.Role, &ep.TaskID, &ep.TurnID,
		&started, &ended, &ep.PlanSummary, &ep.TaskSummary, &toolSeq,
		&skills, &ep.ReviewOutcome, &durationMS, &embedding, &windows,
		&model, &kind, &ep.Count,
		&exemplars, &consolidated, &ep.CommonTaskPattern,
		&ep.CommonOutcome, &ep.SuccessRate, &subjects, &ep.NotablePatterns,
		&workKey, &conversationKey,
	); err != nil {
		return Episode{}, err
	}
	ep.StartedAt = store.DecodeTime(started)
	ep.EndedAt = store.DecodeTime(ended)
	ep.Duration = time.Duration(durationMS) * time.Millisecond
	ep.Kind = Kind(kind)
	ep.ToolSequence = parseList(toolSeq)
	ep.SkillsUsed = parseList(skills)
	ep.ExemplarTurnIDs = parseList(exemplars)
	ep.SubjectsInvolved = parseList(subjects)
	ep.ConsolidatedInto = store.Text(consolidated)
	ep.WorkKey = store.Text(workKey)
	ep.ConversationKey = store.Text(conversationKey)
	ep.EmbeddingModel = store.Text(model)
	if len(embedding) > 0 {
		vectors, err := splitWindows(embedding, windows)
		if err != nil {
			// A row whose vectors cannot be read is still a usable
			// episode for the time-window and outcome queries. Losing
			// the whole row over its recall vector would be a worse
			// trade than losing the recall.
			log.Warn("episode_embedding_undecodable", "episode", ep.ID,
				"windows", windows, "bytes", len(embedding), "error", err)
		} else {
			ep.Embeddings = vectors
		}
	}
	return ep, nil
}

// splitWindows unpacks the blob into the window vectors it holds.
//
// THE COUNT IS ASKED FOR, NOT INFERRED. The blob is the windows packed end to
// end with no separator, so the only thing that can say where one ends is the
// `embedding_windows` column written beside it — and node migration 0030 has
// the arithmetic showing why guessing from the length is not a weaker answer
// but a wrong one.
//
// A row whose length is not a whole multiple of its count is REFUSED rather
// than split on the nearest boundary that works: the two disagree only if the
// row was written by something that did not keep them together, and a plausible
// split of an implausible row is a vector made of two half-vectors, which
// ranks against every query and means nothing.
func splitWindows(blob []byte, windows int) ([][]float32, error) {
	if windows < 1 {
		return nil, fmt.Errorf("learning: %d bytes of embedding are labelled %d windows",
			len(blob), windows)
	}
	if len(blob)%windows != 0 {
		return nil, fmt.Errorf("learning: %d bytes of embedding do not divide into %d windows",
			len(blob), windows)
	}
	width := len(blob) / windows
	out := make([][]float32, 0, windows)
	for at := 0; at < len(blob); at += width {
		vector, err := store.DecodeVector(blob[at : at+width])
		if err != nil {
			return nil, err
		}
		out = append(out, vector)
	}
	return out, nil
}

// defaultEpisodeListing is what [Episodes.Recent] returns for a caller that
// names no limit.
//
// A FLOOR, NOT POLICY, for the same reason [defaultDiaryListing] is: every
// production caller passes an explicit limit.
const defaultEpisodeListing = 10

// Recent returns a seat's most recent episodes, newest first.
func (e *Episodes) Recent(ctx context.Context, handle string, limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = defaultEpisodeListing
	}
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE agent_handle = ? ORDER BY ended_at DESC, id DESC LIMIT ?`,
		handle, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: recent episodes for %s: %w", handle, err)
	}
	return collectEpisodes(rows)
}

// EpisodeFilter narrows which of a seat's episodes a read considers. The zero
// value narrows nothing.
//
// IT IS APPLIED IN SQL, BEFORE THE LIMIT, on every read that takes one — which
// is the whole reason it is a type rather than a loop over what came back. A
// filter applied after a LIMIT answers a different question: asked for the
// five newest failures, it returns the failures among the five newest turns,
// and a seat whose last five turns all succeeded is told it has never failed.
type EpisodeFilter struct {
	// Conversation keeps the episodes of one conversation, by the
	// `{source}:{local}` key the turn served. Empty keeps every episode,
	// including those with no conversation — it never matches the turns
	// that HAVE none, which is what a comparison against '' would do.
	Conversation string

	// Outcome keeps the episodes that ended this way, compared exactly
	// against the stored `review_outcome`. The outcomes an episode is
	// written with are [SettledOutcomes]; a caller taking the value from
	// somebody else normalises and checks it before it gets here, so that
	// a value no row can hold is refused rather than answered with nothing.
	Outcome string
}

// where renders the filter as predicates over the episodes table under alias,
// each beginning " AND ", with the values they bind in order.
//
// THE STATEMENT TEXT VARIES with which fields are set, rather than binding an
// always-true alternative for each field left empty, so each statement states
// exactly the predicate it means and nothing a planner has to see through.
//
// THE OUTCOME TERM CARRIES A UNARY `+`, which is SQLite's spelling for "this
// term may not drive an index", and the driver honours it. Without it the
// planner answers an outcome filter with a MULTI-INDEX AND over
// episodes_outcome_ended_at_idx — an index led by the outcome, not the seat —
// which reads every seat's rows with that outcome to find one seat's (seen in
// the plan; TestAListingSeeksOneSeatWhateverItFilters and
// TestRecallScansOneSeatRatherThanTheTable are the guards). Kept off it, the
// term filters the rows the seat's own index seek returns.
func (f EpisodeFilter) where(alias string) (string, []any) {
	var sql string
	var args []any
	if f.Conversation != "" {
		sql += " AND " + alias + ".conversation_key = ?"
		args = append(args, f.Conversation)
	}
	if f.Outcome != "" {
		sql += " AND +" + alias + ".review_outcome = ?"
		args = append(args, f.Outcome)
	}
	return sql, args
}

// EpisodeQuery asks for one page of a seat's episodes, newest first.
type EpisodeQuery struct {
	Handle string
	Filter EpisodeFilter

	// Offset is how many matching episodes, newest first, the page starts
	// past. Positions are counted afresh on each read, so an episode
	// written between two reads moves the next page by one.
	Offset int

	// Limit is the page size, and it must be positive: a listing whose
	// caller named no size has no honest default to take, because the
	// caller is the one that knows what the rows are for.
	Limit int
}

// EpisodePage is one page of a seat's episodes.
type EpisodePage struct {
	// Episodes is the page, newest first, at most [EpisodeQuery.Limit].
	Episodes []Episode

	// Truncated says more episodes match past this page. It is read off
	// one row past the page rather than inferred from a full page, so a
	// seat holding exactly Limit matching episodes is not told it holds
	// more. The rest is the next page: the same query with Offset moved
	// past this one.
	Truncated bool
}

// List returns one page of a seat's episodes, newest first, narrowed by the
// query's filter before its limit is applied.
//
// ONE ROW PAST THE PAGE IS READ AS EVIDENCE and never returned, which is what
// [EpisodePage.Truncated] reports.
func (e *Episodes) List(ctx context.Context, q EpisodeQuery) (EpisodePage, error) {
	switch {
	case q.Handle == "":
		return EpisodePage{}, fmt.Errorf("learning: an episode listing needs a seat")
	case q.Limit <= 0:
		return EpisodePage{}, fmt.Errorf("learning: an episode listing for %s needs "+
			"a positive limit, got %d", q.Handle, q.Limit)
	case q.Offset < 0:
		return EpisodePage{}, fmt.Errorf("learning: an episode listing for %s needs "+
			"an offset of zero or more, got %d", q.Handle, q.Offset)
	}
	statement, filterArgs := listStatement(q.Filter)
	args := append([]any{q.Handle}, filterArgs...)
	args = append(args, q.Limit+1, q.Offset)
	rows, err := e.db.SQL().QueryContext(ctx, statement, args...)
	if err != nil {
		return EpisodePage{}, fmt.Errorf("learning: list episodes for %s: %w", q.Handle, err)
	}
	found, err := collectEpisodes(rows)
	if err != nil {
		return EpisodePage{}, err
	}
	page := EpisodePage{Episodes: found}
	if len(found) > q.Limit {
		page.Episodes, page.Truncated = found[:q.Limit], true
	}
	return page, nil
}

// listStatement is [Episodes.List]'s query for one filter, binding the seat,
// the filter's values, the limit and the offset in that order.
//
// NAMED rather than written at the call site for the reason [recallStatement]
// is: its plan is asserted — the seat's newest-first walk must be a seek on
// episodes_agent_ended_at_idx whatever the filter adds — and a test that
// retyped the statement would stop describing it.
func listStatement(f EpisodeFilter) (string, []any) {
	filter, args := f.where("e")
	return `SELECT ` + episodeColumns + ` FROM episodes e
		 WHERE e.agent_handle = ?` + filter + `
		 ORDER BY e.ended_at DESC, e.id DESC LIMIT ? OFFSET ?`, args
}

// EPISODES HAVE NO Purge, deliberately, and this note is here so the next
// reader does not add one.
//
// Every other short-horizon table gets a range delete on a single horizon,
// wired into the maintenance sweep. Episodes cannot: their retention is
// [Lifecycle.Pass], which applies FOUR different horizons to four different
// row states — mid-state rows go early, rows a skill absorbed go after an
// audit grace, exemplars of a compacted cluster stay raw, and the compacted
// summaries themselves are kept for years or forever. A single
// `DELETE WHERE ended_at < cutoff` would collapse all four, and the row it
// would take first is the compacted summary — the only record of a whole era
// of a seat's work, standing in for hundreds of turns that are already gone.

func collectEpisodes(rows *sql.Rows) ([]Episode, error) {
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, fmt.Errorf("learning: scan episode: %w", err)
		}
		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read episodes: %w", err)
	}
	return out, nil
}

// jsonList renders a string slice as a JSON array, never as null.
//
// A nil slice marshals to `null`, and the columns are NOT NULL with a '[]'
// default — so a nil would be stored as the four characters "null" and read
// back as a parse failure on every subsequent read of that row.
func jsonList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(blob)
}

// parseList reads a JSON array column, tolerating anything else.
//
// A row written by a different version, or hand-edited, costs its list rather
// than the whole episode: the plan summary and the outcome are what recall is
// for, and they are still readable.
func parseList(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// ErrNoEmbedding reports a recall asked for without a query vector.
var ErrNoEmbedding = errors.New("learning: recall needs a query embedding")

// ErrNoModel reports a recall whose query vector names no model. Such a vector
// is comparable with nothing stored — see [RecallQuery.Model] — so the recall
// is refused rather than answered with a ranking of nothing.
var ErrNoModel = errors.New("learning: recall needs the model its query embedding was made by")
