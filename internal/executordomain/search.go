package executordomain

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textindex"
)

// A SEAT'S SEARCH OF ITS OWN SESSIONS (package doc, "A seat searches its own
// rows by their words"); not of transcripts, since every one at once is tens
// of objects fetched, unsealed and decoded per call. A term matches only
// WHOLE, so no hit confirms a credential's prefix a letter at a time, and only
// in the row's own words as its redaction shows them ([Session.Redacted],
// redact.OwnWords), never in a redacted credential or a marker. The raw words
// are first asked only whether they MAY hold every term
// (textindex.Reading.MayHold), which no shown row fails; asked for whole terms
// instead, they would miss a word a marker now ends, `deploy` before a token
// (TestTwoRowsThatRedactAlikeAreSearchedAlike). Rows are redacted as written
// (executorkeep.Excerpt); this redaction catches rules added since. Unranked,
// newest begun first: a ranked page could carry no cursor a scan goes on from.

// SearchField is one field of a session's row a search matches, by the name a
// reader of a hit is told it by.
type SearchField string

const (
	// SearchAsked is what the turn was asked ([Session.AskExcerpt]).
	SearchAsked SearchField = "asked"

	// SearchConversation is the conversation the turn served
	// ([Session.ConversationKey]): a thread, an issue, a pull request.
	SearchConversation SearchField = "conversation"

	// SearchCompletedWork is what the turn's reviewer said landed, the first
	// of the four fields of internal/executorkeep's Ending a search matches.
	SearchCompletedWork SearchField = "completed_work"

	// SearchBlockedOn is what the turn's executor said blocked it.
	SearchBlockedOn SearchField = "blocked_on"

	// SearchStopReason is why whoever stopped the turn did.
	SearchStopReason SearchField = "stop_reason"

	// SearchFailure is the engine's account of what failed.
	SearchFailure SearchField = "failure"
)

// SearchFields are every field a search matches, in the order a hit names
// them.
var SearchFields = []SearchField{
	SearchAsked, SearchConversation, SearchCompletedWork, SearchBlockedOn, SearchStopReason,
	SearchFailure,
}

// Valid reports whether a field off the wire is one this build matches.
func (f SearchField) Valid() bool { return slices.Contains(SearchFields, f) }

// SessionSearch is one page of a search of a seat's sessions ([Reader.Search]).
type SessionSearch struct {
	// Query is the words every hit's row holds, as internal/textindex's Terms
	// makes terms of them. One that makes no term is refused [ErrInvalid].
	Query string

	// Cursor is the `Next` of the page before, "" for the first.
	Cursor string

	// Limit is the most hits a page holds: [DefaultPageSize] where it names
	// none, and never more than [MaxPageSize].
	Limit int
}

// SearchHit is one session a search found.
type SearchHit struct {
	// Session is the row as the log holds it, as every read of a row answers
	// it; a surface shows it through its one redaction ([Session.Redacted]).
	Session Session

	// Matched are the fields whose words hold a term of the query, in
	// [SearchFields]' order, as the row's redaction shows its own words.
	Matched []SearchField

	// Snippet is a redacted window of the first field holding the most terms,
	// around its earliest term of its own, never a marker's: at most
	// [SearchSnippetBytes], marked where it was cut.
	Snippet string `untrusted:"fence"`
}

// SearchPage is one page of a search of a seat's sessions.
type SearchPage struct {
	Hits []SearchHit

	// Next is the cursor the search goes on from: where the last row this
	// page examined began, so the next page examines the rows begun before
	// it. Empty where the page reached the seat's oldest row.
	Next string

	// Scanned is how many rows the page examined, at most
	// [SearchScanRows], and Exhausted whether they reached the seat's oldest
	// row, so a page that stopped short is never read as nothing older
	// matching.
	Scanned   int
	Exhausted bool

	// Position is the checkpoint the page was read at, and Complete false
	// where a record this node could not decode may hold rows missing from
	// it.
	Position statelog.Position
	Complete bool
}

// Search is one page of a seat's sessions whose words hold every term of
// ask's query, the latest begun first, after ask's cursor, at the level fresh
// asks for, in one read examining at most [SearchScanRows] rows and holding
// its hits to the bytes the read carries ([statelog.RowsOf]).
//
// It reads the seat's page statement one row past the page, so a page that
// stops knows whether an older row exists.
func (r *Reader) Search(ctx context.Context, handle string, ask SessionSearch,
	fresh statelog.Freshness) (SearchPage, error) {

	if err := checkSeat(handle, ""); err != nil {
		return SearchPage{}, err
	}
	terms := textindex.Terms(ask.Query)
	if len(terms) == 0 {
		return SearchPage{}, invalid("the query %q holds no word to search by: a word is two or "+
			"more letters or digits", ask.Query)
	}
	limit := ask.Limit
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	after, err := parseCursor(ask.Cursor)
	if err != nil {
		return SearchPage{}, err
	}
	var out SearchPage
	answer, err := r.log.Read(ctx, fresh.Query(SeatScope(handle), true), func(tx *sql.Tx) error {
		out = SearchPage{}
		rows, queryErr := tx.QueryContext(ctx, pageStatement, handle, after, SearchScanRows+1)
		if queryErr != nil {
			return queryErr
		}
		var (
			lastBegun int64
			older     bool
			found     []pendingHit
		)
		for rows.Next() {
			// A ROW PAST WHAT THE PAGE EXAMINES is only counted: it says the
			// page did not reach the seat's oldest.
			if out.Scanned == SearchScanRows || len(found) == limit {
				older = true
				break
			}
			var first int64
			s, scanErr := scanSession(trailing{rows, &first})
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			if hit, matched := searchHit(s, terms); matched {
				found = append(found, pendingHit{hit: hit, scanned: out.Scanned, before: lastBegun})
			}
			out.Scanned++
			lastBegun = first
		}
		if closeErr := errors.Join(rows.Err(), rows.Close()); closeErr != nil {
			return closeErr
		}
		sessions := make([]Session, len(found))
		for i, f := range found {
			sessions[i] = f.hit.Session
		}
		if mediaErr := withMedia(ctx, tx, sessions); mediaErr != nil {
			return mediaErr
		}
		held := statelog.RowsOf(ctx)
		for i, f := range found {
			f.hit.Session = sessions[i]
			if !held.TakeEncoded(f.hit) {
				// THE REFUSED ROW IS LEFT UNEXAMINED: the cursor stays on the
				// row before it, so the next page starts at it. A kept hit
				// precedes any refusal, so that row exists.
				older, out.Scanned, lastBegun = true, f.scanned, f.before
				break
			}
			out.Hits = append(out.Hits, f.hit)
		}
		out.Exhausted = !older
		if older {
			out.Next = strconv.FormatInt(lastBegun, 10)
		}
		return nil
	})
	if err != nil {
		return SearchPage{}, err
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// pendingHit is a hit a search found, held to the read's bytes once its
// rows are closed and its media read: how many rows it examined before it,
// and where the row before it began.
type pendingHit struct {
	hit     SearchHit
	scanned int
	before  int64
}

// searchedText is one field of a row a search matches, and its words.
type searchedText struct {
	field SearchField
	text  string
}

// searchedTexts are the words of s a search matches, field by field, in
// [SearchFields]' order: what the turn was asked, its conversation, and the
// prose of its ending, where it has ended.
func searchedTexts(s Session) []searchedText {
	out := []searchedText{{SearchAsked, s.AskExcerpt}, {SearchConversation, s.ConversationKey}}
	if e := s.Ending; e != nil {
		out = append(out, searchedText{SearchCompletedWork, e.CompletedWork},
			searchedText{SearchBlockedOn, e.BlockedOn}, searchedText{SearchStopReason, e.StopReason},
			searchedText{SearchFailure, e.Failure})
	}
	return out
}

// joined is texts as one text whose fields no term runs across: a line break
// is no letter and no digit, so the analyzer splits a term there.
func joined(texts []searchedText) string {
	words := make([]string, len(texts))
	for i, t := range texts {
		words[i] = t.text
	}
	return strings.Join(words, "\n")
}

// searchHit is s as a search for terms finds it, and false where the words a
// reader is shown of it do not hold every term of their own (see atop this
// file). MayHold may be asked of the joined raw fields because a redaction
// keeps the words around its markers and the joining line break is no letter
// or digit, so every word of the redacted fields is a piece of a raw word.
func searchHit(s Session, terms []string) (SearchHit, bool) {
	if !textindex.Plain.MayHold(joined(searchedTexts(s)), terms) {
		return SearchHit{}, false
	}
	shown := searchedTexts(s.Redacted())
	if _, all := textindex.Plain.FindIn(redact.OwnWords(joined(shown)), terms); !all {
		return SearchHit{}, false
	}
	hit := SearchHit{Session: s}
	best, most := "", 0
	for _, t := range shown {
		held := heldTerms(t.text, terms)
		if held == 0 {
			continue
		}
		hit.Matched = append(hit.Matched, t.field)
		if held > most {
			best, most = t.text, held
		}
	}
	// CENTRED ON THE FIRST TERM THE FIELD SHOWS OF ITS OWN, as the hit was
	// decided, not on the same letters in a marker before it (`token` in
	// `[REDACTED:github-token]`), which in a long field misses the found word.
	hit.Snippet = textindex.SnippetFrom(best, SearchSnippetBytes, func(folded string) int {
		at, _ := textindex.Plain.FindIn(redact.OwnWords(folded), terms)
		return at
	})
	return hit, true
}

// heldTerms is how many of terms text — a field's redacted words — holds of
// its own, each as a whole term, never in a marker the redaction wrote.
func heldTerms(text string, terms []string) int {
	held := 0
	for _, term := range terms {
		if _, all := textindex.Plain.FindIn(redact.OwnWords(text), []string{term}); all {
			held++
		}
	}
	return held
}
