package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textindex"
)

// Searching the company's chat, and the one thing this ranking has that the
// knowledge base's does not: a VIEWER.
//
// A private channel's contents are not a fact about the company the way a page
// is — the room's own membership decides who may read it — so every query here
// carries the set of rooms its asker may see, computed once by the caller that
// already knows the viewer. That set is REQUIRED and an empty one is REFUSED
// rather than read as "everything": the empty slice is what a caller that lost
// its viewer produces, and the two readings differ by the whole transcript.

// ChatQuery is one search over a company's chat.
type ChatQuery struct {
	// Text is what somebody typed.
	Text string

	// Channels is the set of rooms searched: the viewer's visible set, or
	// the one room of it a caller narrowed to. REQUIRED — see the file doc.
	// It is also the corpus the search is RANKED against
	// ([ChatIndexer.SearchMessages]), so it must never name a room the
	// viewer may not read.
	Channels []string

	// Author narrows to one speaker's handle, exactly as it is stored;
	// empty is anybody.
	Author string

	// Limit bounds the answer; zero takes the default.
	Limit int
}

// ChatHit is one message a search matched.
type ChatHit struct {
	MessageID  string
	ChannelID  string
	ThreadRoot string
	Author     string

	// Snippet is what the message says, cut from the transcript row when
	// the hit was read, its whitespace collapsed: the whole message when it
	// fits [ChatSnippetBytes], otherwise the window around the first query
	// term it carries, marked where it was cut.
	Snippet string

	// CreatedAt is when the message was posted — the broker's stored
	// instant, in UTC, decoded here at the edge where the row is read.
	CreatedAt time.Time
	Score     float64
}

// ErrNoViewer is a chat search that named no visible channels.
//
// ITS OWN ERROR rather than an empty result, because the two are opposite
// facts and only one of them is safe to render: a caller that genuinely has no
// visible channels is answered with no hits by the ordinary path, and a caller
// that FORGOT to resolve its viewer must not be handed one.
var ErrNoViewer = errors.New("search: a chat search needs the viewer's visible channels")

// The bounds on how many hits one chat search answers.
//
// ChatSearchLimit is what a query that names no limit is answered with:
// twenty, against the knowledge search's ten, because a chat hit is one
// sentence where a knowledge hit is a document, so the same screen holds twice
// as many of them before a person has to page.
//
// MaxChatSearchLimit is the most any query is answered with, whatever it
// asked for — and a caller's number reaches here as it was typed, a seat's
// `limit` argument included. FIFTY, because one answer is ONE TOOL RESULT for
// a seat and its ceiling is 64 KiB (the builtin's ToolAnswerBytes): a hit is
// at most [ChatSnippetBytes] of message plus three ids, a handle, an instant
// and a score, about a kilobyte rendered as prose, so fifty still renders as
// one answer rather than a refusal to answer at all — 48 KiB of prose, 58 KiB
// with a quoted character in every snippet, and sixty would not — and fifty is
// a screen, the chat reader's own default page. Without it a query asked for a
// million hits was ranked and returned whole, and the surface that asked
// refused to hand it over.
//
// AS PROSE, and the qualifier is the ceiling's to keep: a tool answer writes
// markup as it was written, but a snippet dense with what its JSON still
// writes as six bytes — a control character, a terminal's escape pasted with
// its colours — is four times that, and a page of them is refused by that
// ceiling naming `limit` — its job, since a smaller page is the remedy —
// rather than handed over cut.
const (
	ChatSearchLimit    = 20
	MaxChatSearchLimit = 50
)

// ChatSnippetBytes bounds a hit's snippet — beside the marks a cut leaves at
// either end of it.
//
// EXPORTED because a surface sizes its answers by it: a page of hits at
// [MaxChatSearchLimit] is one tool answer, and both bounds together are what
// keep that answer under its ceiling.
//
// SIX HUNDRED, which holds the whole of most messages — a chat hit IS the
// message, where a knowledge hit points at a page to open — and cuts only a
// pasted block, around the words that made it a hit. It bounds what a search
// answer carries whatever a room holds: a message may be tens of kilobytes,
// and twenty of them whole would be most of a megabyte handed to a seat as a
// tool result.
const ChatSnippetBytes = 600

// chatScanBinds is how many parameters the posting scan binds besides its
// rooms: the term, the speaker and the scan budget.
const chatScanBinds = 3

// chatRoomChunk is how many rooms one statement names, from the bound
// parameters this store's driver accepts.
//
// MEASURED, never a constant: a fixed IN list of one variable per room made the
// size of an org chart decide whether chat search worked at all — the driver
// REFUSES a statement past its limit, which a caller reads as a search that
// found nothing. Read off the store's own probe ([store.Capabilities]), a
// visible set fits one statement on every driver this build ships, and the
// chunks exist for the one that would not.
func chatRoomChunk(maxVariables int) int { return max(1, maxVariables-chatScanBinds) }

// SearchMessages ranks a company's chat against a query.
//
// IT RAISES rather than answering empty, for the reason the knowledge search
// gives: this is the storage layer, where "the store would not answer" and
// "nothing matched" are different facts, and the seam above it is what turns a
// failure into the empty block a turn tolerates.
//
// A QUERY PAST [knowledge.MaxQueryBytes] IS REFUSED before anything runs, as
// [FanOut.Search] refuses one for the other two ranked searches. Chat is a
// ranked search on the same terms — a pasted passage ranks on every incidental
// word in it and answers a question nobody asked — and here each term is also
// a postings read of its own, so an unbounded query is an unbounded number of
// them. Every surface refuses first in its own words; this is the one path a
// chat search takes on the node that ranks it, whichever node or surface
// asked.
//
// # Ranked by the rooms it searches
//
// The document count, the average length and every term's document frequency
// are SUMMED OVER [ChatQuery.Channels], never taken from the whole company.
// Taken from the company, a score is a function of how often a word is said in
// rooms the asker cannot read: a private room or a direct conversation saying
// a word more often lowers its weight in every hit the asker CAN see, and the
// scores and the order of their own search move with the contents of rooms
// they are not in. Over the searched rooms, the ranking is BM25 over exactly
// the corpus the asker reads — which is also the honest one, since that is the
// corpus they are searching. A [ChatQuery.Author] narrows the postings and not
// the statistics, as a scope narrows the knowledge search: the same word is not
// made rare by being looked for in one person's messages.
//
// The whole ranking is read in ONE SNAPSHOT of this node's index, so the
// statistics and the postings they weight agree while the walk writes beside
// it.
func (x *ChatIndexer) SearchMessages(ctx context.Context, q ChatQuery) ([]ChatHit, error) {
	if err := knowledge.CheckQuery(q.Text); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(q.Channels) == 0 {
		return nil, ErrNoViewer
	}
	terms := textindex.Terms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = ChatSearchLimit
	}
	limit = min(limit, MaxChatSearchLimit)
	// DEDUPLICATED, so a room named twice is not summed twice into the
	// statistics, and SORTED, so one set binds identically whatever order
	// it arrived in.
	rooms := slices.Clone(q.Channels)
	slices.Sort(rooms)
	rooms = slices.Compact(rooms)
	chunk := chatRoomChunk(x.db.Caps().MaxVariables)

	scores := map[string]float64{}
	err := x.db.Read(ctx, func(tx *sql.Tx) error {
		// FRESH PER ATTEMPT, for the reason a write's delta is: the store
		// retries a read that lost a snapshot race by running this again.
		clear(scores)
		corpus, err := x.roomsCorpus(ctx, tx, rooms, chunk)
		if err != nil || corpus.Docs == 0 {
			return err
		}
		for _, term := range terms {
			docs, err := x.termDocs(ctx, tx, term, rooms, chunk)
			if err != nil {
				return err
			}
			if docs == 0 {
				continue
			}
			postings, err := x.chatPostings(ctx, tx, term, rooms, q.Author, chunk)
			if err != nil {
				return err
			}
			idf := textindex.IDF(corpus.Docs, docs)
			for _, p := range postings {
				// THE CHAT PROFILE, not the prose one. A chat corpus
				// is bimodal — one-line acknowledgements beside
				// pasted blocks — and the prose tuning's length
				// penalty hands every query to whoever typed the
				// shortest reply.
				scores[p.DocID] += textindex.ChatProfile.Score(idf, p, corpus)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(scores) == 0 {
		return nil, nil
	}
	return x.hydrateChat(ctx, scores, terms, limit)
}

// roomsCorpus sums the searched rooms' statistics.
func (x *ChatIndexer) roomsCorpus(ctx context.Context, tx *sql.Tx, rooms []string,
	chunk int) (textindex.Corpus, error) {

	var docs, tokens int64
	for part := range slices.Chunk(rooms, chunk) {
		var d, t int64
		if err := tx.QueryRowContext(ctx, chatRoomsCorpusStatement(len(part)),
			anys(part)...).Scan(&d, &t); err != nil {
			return textindex.Corpus{}, fmt.Errorf("search: read the statistics of "+
				"the chat rooms searched: %w", err)
		}
		docs, tokens = docs+d, tokens+t
	}
	return corpusOf(docs, tokens), nil
}

// termDocs sums how many of the searched rooms' documents carry a term.
func (x *ChatIndexer) termDocs(ctx context.Context, tx *sql.Tx, term string,
	rooms []string, chunk int) (int, error) {

	total := 0
	for part := range slices.Chunk(rooms, chunk) {
		var n int
		if err := tx.QueryRowContext(ctx, chatTermDocsStatement(len(part)),
			append([]any{term}, anys(part)...)...).Scan(&n); err != nil {
			return 0, fmt.Errorf("search: count the chat documents carrying %q: %w", term, err)
		}
		total += n
	}
	return total, nil
}

// chatPostings reads one term's postings in the searched rooms, narrowed to one
// speaker when the query names one.
//
// AT MOST [maxPostingScan] OF THEM, and WHICH ones is the order: the scan walks
// the term's list backwards by message id, which is a UUIDv7 carrying the
// instant its post was asked, so a term in more messages than the budget
// contributes its NEWEST ones. The knowledge index keeps its most frequent
// instead, at the price of sorting the whole list; in a corpus of sentences
// the frequency is one almost everywhere, so that sort would buy an arbitrary
// subset, where the newest is the one a person searching their chat wants.
//
// ONE BUDGET ACROSS EVERY CHUNK, not one each: a limit per statement would make
// the scan this bounds grow with the room count. The chunks are disjoint
// because a document lives in exactly one room, so concatenating them is
// exact for a term under the budget; one past it spends the budget in chunk
// order, so its postings are the newest of the first chunks rather than of
// the whole list. That needs a viewer who may read more rooms than one
// statement binds — the store's measured parameter limit, tens of thousands
// ([chatRoomChunk]), against a company of at most a thousand live rooms.
func (x *ChatIndexer) chatPostings(ctx context.Context, tx *sql.Tx, term string,
	rooms []string, author string, chunk int) ([]textindex.Posting, error) {

	var postings []textindex.Posting
	budget := maxPostingScan
	for part := range slices.Chunk(rooms, chunk) {
		if budget <= 0 {
			break
		}
		args := make([]any, 0, len(part)+chatScanBinds)
		args = append(args, term)
		args = append(args, anys(part)...)
		if author != "" {
			args = append(args, author)
		}
		args = append(args, budget)
		rows, err := tx.QueryContext(ctx,
			chatPostingScanStatement(len(part), author != ""), args...)
		if err != nil {
			return nil, fmt.Errorf("search: read the chat postings for %q: %w", term, err)
		}
		for rows.Next() {
			var p textindex.Posting
			if err := rows.Scan(&p.DocID, &p.Freq, &p.Length); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("search: scan a chat posting for %q: %w", term, err)
			}
			postings = append(postings, p)
			budget--
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("search: read the chat postings for %q: %w", term, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("search: read the chat postings for %q: %w", term, err)
		}
	}
	return postings, nil
}

// chatRoomsCorpusStatement sums n rooms' statistics.
func chatRoomsCorpusStatement(n int) string {
	return `SELECT COALESCE(SUM(docs), 0), COALESCE(SUM(tokens), 0)
		  FROM chat_index_rooms WHERE channel_id IN (` + binds(n) + `)`
}

// chatTermDocsStatement sums one term's document count over n rooms.
func chatTermDocsStatement(n int) string {
	return `SELECT COALESCE(SUM(docs), 0) FROM chat_index_terms
		 WHERE term = ? AND channel_id IN (` + binds(n) + `)`
}

// chatPostingScanStatement is one term's postings in n rooms, narrowed to one
// speaker when narrowed is set.
func chatPostingScanStatement(n int, narrowed bool) string {
	speaker := ""
	if narrowed {
		speaker = ` AND d.author = ?`
	}
	return `SELECT p.doc_id, p.freq, d.length
		  FROM chat_postings p
		  JOIN chat_docs d ON d.id = p.doc_id
		 WHERE p.term = ? AND d.channel_id IN (` + binds(n) + `)` + speaker + `
		 ORDER BY p.doc_id DESC
		 LIMIT ?`
}

// chatHydrateStatement reads n messages' rows from the transcript.
func chatHydrateStatement(n int) string {
	return `SELECT id, channel_id, thread_root, author_handle, body, created_at,
		       deleted_at IS NOT NULL
		  FROM chat_messages WHERE id IN (` + binds(n) + `)`
}

// hydrateChat turns the best-scored ids into hits, reading the message rows
// themselves.
//
// FROM THE TRANSCRIPT rather than from the index, because the index keeps no
// copy of what a message says: a second copy of every message in the node
// estate would double the largest thing on this disk to save one read of rows
// this node already holds, and the transcript is current where a copy is only
// as current as the walk.
//
// A MESSAGE THE INDEX HAS AND THE TRANSCRIPT NO LONGER DOES IS SKIPPED, not
// raised. That gap is the ordinary window between a prune, an erase or a
// tombstone landing and the index following it, and a search that failed
// during it would turn a few seconds of catch-up into an outage.
func (x *ChatIndexer) hydrateChat(ctx context.Context, scores map[string]float64,
	terms []string, limit int) ([]ChatHit, error) {

	top := topN(scores, limit)
	chunk := max(1, x.estate.Caps().MaxVariables)
	found := make(map[string]ChatHit, len(top))
	err := x.estate.Read(ctx, func(tx *sql.Tx) error {
		clear(found)
		for part := range slices.Chunk(top, chunk) {
			if err := hydrateChunk(ctx, tx, part, terms, found); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search: hydrate the chat hits: %w", err)
	}
	hits := make([]ChatHit, 0, len(top))
	for _, id := range top {
		if hit, ok := found[id]; ok {
			hit.Score = scores[id]
			hits = append(hits, hit)
		}
	}
	return hits, nil
}

// hydrateChunk reads one chunk of hits into found, leaving out every message
// tombstoned since it was indexed.
func hydrateChunk(ctx context.Context, tx *sql.Tx, ids, terms []string,
	found map[string]ChatHit) error {

	rows, err := tx.QueryContext(ctx, chatHydrateStatement(len(ids)), anys(ids)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hit ChatHit
		var body string
		var createdAt int64
		var deleted bool
		if err := rows.Scan(&hit.MessageID, &hit.ChannelID, &hit.ThreadRoot,
			&hit.Author, &body, &createdAt, &deleted); err != nil {
			return err
		}
		if deleted {
			continue
		}
		hit.CreatedAt = store.DecodeTime(createdAt)
		hit.Snippet = textindex.Snippet(body, terms, ChatSnippetBytes)
		found[hit.MessageID] = hit
	}
	return rows.Err()
}
