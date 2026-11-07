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

// A SEAT'S SEARCH OF ITS OWN SESSIONS, by the words of their rows.
//
// A seat's run finds an earlier session of its own by what its turn was asked,
// the conversation it served, and how it ended — the reviewer's account of
// what landed, what blocked a round, why it was stopped, what failed. Those are
// the row's words, and the row is in the replicated estate, so the search is
// one read on the node that holds the rows: no session's object is opened, and
// no row is shipped to be filtered somewhere else. A transcript is searched one
// session at a time, by a read of that session (internal/executorread); every
// transcript at once would be tens of objects fetched, unsealed and decoded per
// call.
//
// MATCHED AS EVERY OTHER LEXICAL SEARCH OF THE ENGINE MATCHES: the query's terms
// as internal/textindex's analyzer makes them — case folded, split on anything
// that is not a letter or a digit, terms of two characters or more, no stemming
// — and a row matches when its words hold every term, each as a whole term. So
// a word typed is a word searched, and a term found only as part of a longer
// one finds nothing, which is also what keeps a hit from confirming a
// credential's prefix a letter at a time.
//
// MATCHED ON WHAT A READER IS SHOWN: the row's words as its one redaction
// leaves them ([Session.Redacted]), and of those only the row's own — never a
// word of a marker the redaction wrote (redact.OwnWords) — so a term found
// only inside a credential the redaction takes out is no hit, and no hit says
// a row holds what its reader is never shown: read with a marker's words, a
// password that was the very word its marker spells was found. The row's
// words as the log holds them are asked one thing first, which costs no
// redaction: whether they could hold every term at all, each inside one of
// them (textindex.Reading.MayHold), which no row whose redaction shows every
// term fails, so it decides nothing — only a row it lets through is redacted
// and matched. Asked whether they held every term WHOLE instead, it decided:
// a word a redaction's marker ends, `deploy` before a token the redaction now
// takes out, is a piece of a longer word as the row holds it, so the row was
// passed over unless a credential the redaction took out held the word whole,
// and a hit then said what that credential held. A row this build wrote holds its
// words redacted already (executorkeep.Excerpt); one a rule added since
// reaches is what the redaction here is for.
//
// NEWEST BEGUN FIRST, UNRANKED, and paged as a seat's sessions are paged, by
// where each row began: a seat's own history is read by recency, and a ranked
// page could carry no cursor a scan goes on from. A page examines at most
// [SearchScanRows] rows and says how many it did and whether they reached the
// seat's oldest, so a search that stopped short is never read as one that
// found nothing.

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
	// of the four accounts of how the turn ended (internal/executorkeep's
	// Ending) a search matches.
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

	// Snippet is the window of the matched field that holds the most of the
	// query's terms — the first of them, where several hold as many — around
	// the earliest term it shows of its own, never a marker's, redacted: at
	// most [SearchSnippetBytes], marked where it was cut.
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
	// row: a page that is not exhausted searched those rows and no older
	// one, which its reader is told rather than that nothing older matches.
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
// asks for — in one read of the seat's rows, examining at most
// [SearchScanRows] of them.
//
// THE SEAT'S OWN PAGE STATEMENT, read past the page by one row, so a page
// that stops — its hits full, or its rows examined to the bound — knows
// whether a row older than the last it examined exists, and says it reached
// the seat's oldest only where none does.
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
		)
		for rows.Next() {
			// A ROW PAST WHAT THE PAGE EXAMINES is only counted: it says the
			// page did not reach the seat's oldest.
			if out.Scanned == SearchScanRows || len(out.Hits) == limit {
				older = true
				break
			}
			var first int64
			s, scanErr := scanSession(trailing{rows, &first})
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Scanned++
			lastBegun = first
			if hit, found := searchHit(s, terms); found {
				out.Hits = append(out.Hits, hit)
			}
		}
		if closeErr := errors.Join(rows.Err(), rows.Close()); closeErr != nil {
			return closeErr
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
// reader is shown of it do not hold every one of them: its words as the log
// holds them are asked first whether they could hold every term at all, and
// only a row they could is redacted and matched, on its own words alone — a
// term only a credential the redaction takes out held, and one only a marker
// it wrote spells, is no hit.
//
// THE FIELDS JOINED, as the log holds them and as their redaction shows them:
// a field's redaction copies the field's words as they stand around the
// markers it writes, and the line break joining two fields is no letter and no
// digit, so every word of a stretch of the redacted fields is a piece of a word
// of the fields themselves — what textindex.Reading.MayHold asks of them.
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
	// decided: on the first anywhere, the snippet showed the same letters in a
	// marker before it — `token` in `[REDACTED:github-token]` — and in a field
	// longer than the snippet, never the word the row was found by.
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
