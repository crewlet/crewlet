package executordomain

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// asked is a turn's end in conversation, asked ask and ending as e says.
func asked(seat, conversation, turn, ask string, e executorkeep.Ending) End {
	end := ending(seat, conversation, turn, "", e)
	end.AskExcerpt = ask
	return end
}

// hitTurns is the turn ids of hits, in order.
func hitTurns(hits []SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Session.Turn
	}
	return out
}

// search is one page of seat's search for query after cursor, limit hits to a
// page, read at `stale` from e's rows.
func (e *estate) search(seat, query, cursor string, limit int) SearchPage {
	e.t.Helper()
	page, err := e.reader().Search(e.t.Context(), seat, SessionSearch{Query: query, Cursor: cursor,
		Limit: limit}, stale)
	if err != nil {
		e.t.Fatalf("%s's search for %q after %q: %v", seat, query, cursor, err)
	}
	return page
}

// A SEAT'S SEARCH FINDS EVERY SESSION WHOSE WORDS HOLD EVERY TERM, newest begun
// first, and never another seat's: the terms may sit in different fields of a
// row — what it was asked, its conversation, how it ended — and each is found
// only as a whole term, so a row holding one term, or holding a term only
// inside a longer word, is no hit. Rows are ordered by where they began,
// whatever order their turns ended in, and a page whose hits are full says
// where the next goes on from.
//
// Mutation: match any term rather than every one, order the page by the turns'
// ends, or drop the seat from the page statement, and a check goes red.
func TestASeatsSearchFindsEverySessionHoldingEveryWord(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	inAsk, split, one, inKey, part := newTurn(), newTurn(), newTurn(), newTurn(), newTurn()
	// BEGUN IN ONE ORDER, ended in the other.
	for _, run := range []struct{ turn, key string }{
		{inAsk, "chat:room"}, {split, "chat:room"}, {one, "chat:room"},
		{inKey, "github:acme/zebra#7"}, {part, "chat:room"},
	} {
		e.capture(capturing("ana", run.key, run.turn, 1, 1, 100, budget))
	}
	completed := executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	e.end(asked("ana", "chat:room", part, "redeploy the zebras", completed))
	e.end(asked("ana", "github:acme/zebra#7", inKey, "look at the flaky check",
		executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, Failure: "the deploy guard tripped"}))
	e.end(asked("ana", "chat:room", one, "a zebra and nothing else", completed))
	e.end(asked("ana", "chat:room", split, "write the zebra docs",
		executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, CompletedWork: "deploy rolled out"}))
	e.end(asked("ana", "chat:room", inAsk, "fix the Zebra deploy-pipeline", completed))
	// ANOTHER SEAT'S ROW holding both words.
	e.end(asked("ben", "chat:room", newTurn(), "zebra deploy", completed))

	page := e.search("ana", "zebra DEPLOY", "", 0)
	if want := []string{inKey, split, inAsk}; !slices.Equal(hitTurns(page.Hits), want) {
		t.Fatalf("the search found %v, want %v: every row holding both words, the latest begun "+
			"first, none holding one of them or a word that merely contains one, and none of "+
			"another seat's", hitTurns(page.Hits), want)
	}
	if !page.Exhausted || page.Next != "" || page.Scanned != 5 {
		t.Errorf("the search examined %d rows (exhausted %v, next %q), want all five of the seat's "+
			"and no cursor", page.Scanned, page.Exhausted, page.Next)
	}
	if page.Position != e.at() || !page.Complete {
		t.Errorf("the search was read at %s (complete %v), want %s, whole", page.Position,
			page.Complete, e.at())
	}
	for i, want := range [][]SearchField{
		{SearchConversation, SearchFailure}, {SearchAsked, SearchCompletedWork}, {SearchAsked},
	} {
		if got := page.Hits[i].Matched; !slices.Equal(got, want) {
			t.Errorf("hit %d matched %v, want %v", i, got, want)
		}
	}
	if snippet := page.Hits[2].Snippet; snippet != "fix the Zebra deploy-pipeline" {
		t.Errorf("the ask's hit shows %q, want the field holding both words", snippet)
	}

	// A PAGE WHOSE HITS ARE FULL goes on after the last row it examined.
	first := e.search("ana", "zebra deploy", "", 2)
	if !slices.Equal(hitTurns(first.Hits), []string{inKey, split}) || first.Exhausted || first.Next == "" {
		t.Fatalf("a page of two is %v (exhausted %v, next %q), want the two latest hits and a "+
			"cursor", hitTurns(first.Hits), first.Exhausted, first.Next)
	}
	second := e.search("ana", "zebra deploy", first.Next, 2)
	if !slices.Equal(hitTurns(second.Hits), []string{inAsk}) || !second.Exhausted || second.Next != "" {
		t.Errorf("the page after it is %v (exhausted %v, next %q), want the last hit and the "+
			"seat's oldest reached", hitTurns(second.Hits), second.Exhausted, second.Next)
	}

	for name, query := range map[string]string{"no word at all": "!! ?", "a word of one letter": "a"} {
		if _, err := e.reader().Search(t.Context(), "ana", SessionSearch{Query: query}, stale); !errors.Is(err,
			ErrInvalid) {
			t.Errorf("a query of %s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.reader().Search(t.Context(), "ana", SessionSearch{Query: "zebra", Cursor: "x"},
		stale); !errors.Is(err, ErrInvalid) {
		t.Errorf("a cursor no page handed out: %v, want ErrInvalid", err)
	}
}

// githubToken is a credential's shape the redaction knows, split by the
// analyzer at its underscore: its second term is a word a search could be
// asked for.
var githubToken = "ghp_" + strings.Repeat("Qx7Rb2", 6)

// A SEARCH MATCHES WHAT A READER WOULD BE SHOWN: a term found only inside a
// credential the row's redaction takes out finds nothing, though the row's raw
// words hold it — asked alone, or beside a word the row shows — and so does a
// term only the marker the redaction wrote in its place spells, a password
// that was the very word its marker names; a hit's snippet is cut from the
// redacted words.
//
// Mutation: match the row's raw words, read the marker's words as the row's,
// or cut the snippet from the raw words, and a check goes red.
func TestASearchMatchesWhatAReadWouldShow(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run, marked := newTurn(), newTurn()
	e.end(asked("ana", "chat:room", run, "deploy it with "+githubToken+" before noon",
		executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	e.end(asked("ana", "chat:room", marked, "log in with passwd: password",
		executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))

	inside := strings.ToLower(strings.TrimPrefix(githubToken, "ghp_"))
	for _, query := range []string{inside, "deploy " + inside, "password"} {
		if page := e.search("ana", query, "", 0); len(page.Hits) != 0 {
			t.Errorf("a search for %q, a term only a redacted credential or its marker held, found %v: "+
				"a reader of the row is never shown it", query, hitTurns(page.Hits))
		}
	}
	if page := e.search("ana", "passwd", "", 0); !slices.Equal(hitTurns(page.Hits), []string{marked}) {
		t.Errorf("a search for a word the row shows found %v, want it", hitTurns(page.Hits))
	}
	page := e.search("ana", "deploy noon", "", 0)
	if !slices.Equal(hitTurns(page.Hits), []string{run}) {
		t.Fatalf("the search found %v, want the row", hitTurns(page.Hits))
	}
	hit := page.Hits[0]
	if strings.Contains(hit.Snippet, githubToken) || !strings.Contains(hit.Snippet, "[REDACTED:") {
		t.Errorf("the hit's snippet is %q: it is cut from the words a reader is shown", hit.Snippet)
	}
	if hit.Session.AskExcerpt != "deploy it with "+githubToken+" before noon" {
		t.Errorf("the hit's row holds %q, want the row as the log holds it, which its surface "+
			"shows through its one redaction", hit.Session.AskExcerpt)
	}
}

// TWO ROWS THAT REDACT ALIKE ARE SEARCHED ALIKE. Two seats' rows whose words
// differ only in what the redaction takes out — a password a guess names and
// one it does not, as long, beside a token the rules of the build that wrote
// them did not know — are shown alike, so a search for any words finds both or
// neither, each matching the same fields with the same snippet. A word a
// redaction's marker ends, `deploy` before the token, is a word the reader is
// shown, and a hit in both, though as the rows hold it it is a piece of a
// longer word and only one of the passwords held it whole.
//
// Mutation: ask the rows' words whether they hold every term whole before
// redacting them, and a check goes red.
func TestTwoRowsThatRedactAlikeAreSearchedAlike(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	completed := executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	turns := map[string]string{}
	for seat, password := range map[string]string{"ana": "deploy", "ben": "launch"} {
		turns[seat] = newTurn()
		e.end(asked(seat, "chat:room", turns[seat], "deploy"+githubToken+" failed, password: "+password,
			completed))
	}
	for query, found := range map[string]bool{"deploy": true, "failed deploy": true, "launch": false} {
		for _, seat := range []string{"ana", "ben"} {
			page := e.search(seat, query, "", 0)
			want := []string{}
			if found {
				want = []string{turns[seat]}
			}
			if got := hitTurns(page.Hits); !slices.Equal(got, want) {
				t.Errorf("%s's search for %q found %v, want %v: the two rows are shown alike", seat, query,
					got, want)
				continue
			}
			if found && (!slices.Equal(page.Hits[0].Matched, []SearchField{SearchAsked}) ||
				page.Hits[0].Snippet != "deploy[REDACTED:github-token] failed, password: [REDACTED:password]") {
				t.Errorf("%s's hit for %q matched %v, showing %q; want the ask, as it is shown", seat, query,
					page.Hits[0].Matched, page.Hits[0].Snippet)
			}
		}
	}
}

// A HIT'S SNIPPET SHOWS THE WORD IT WAS FOUND BY. A search decides a hit on
// the words a row shows of its own, so a hit's snippet is centred on the first
// of those, never on the same letters in a marker the redaction wrote before
// it: in a field longer than a snippet, centred on `token` in the marker of a
// GitHub token, the snippet left out the sentence the row was found by.
//
// Mutation: centre the snippet on the first term anywhere in the field, a
// marker's words among them, and a check goes red.
func TestAHitsSnippetShowsTheWordItWasFoundBy(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	turn := newTurn()
	e.end(asked("ana", "chat:room", turn, githubToken+" was pushed by mistake; "+strings.Repeat("then more ", 50)+
		"and the token was rotated", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	page := e.search("ana", "token", "", 0)
	if !slices.Equal(hitTurns(page.Hits), []string{turn}) {
		t.Fatalf("the search found %v, want the row", hitTurns(page.Hits))
	}
	if snippet := page.Hits[0].Snippet; !strings.Contains(snippet, "the token was rotated") ||
		len(snippet) > SearchSnippetBytes+len("……") {
		t.Errorf("the hit's snippet is %q, want at most %d bytes around the word the row shows: the token "+
			"was rotated", snippet, SearchSnippetBytes)
	}
}

// A SEARCH THAT STOPS AT ITS BOUND SAYS WHERE AND HOW FAR: a seat whose rows
// past the newest SearchScanRows hold the only match is answered no hit, the
// rows it examined counted, not exhausted, and a cursor at the last row it
// examined, from which the next page finds the older match and reaches the
// seat's oldest row.
//
// Mutation: drop the cursor of a page stopped at its bound, or say it reached
// the seat's oldest, and the older match is never searched.
func TestASearchThatStopsAtItsBoundSaysWhereAndHowFar(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	completed := executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	older := newTurn()
	e.end(asked("ana", "chat:room", older, "the zebra migration", completed))
	for range SearchScanRows {
		e.end(asked("ana", "chat:room", newTurn(), "a plain ask", completed))
	}

	page := e.search("ana", "zebra", "", 0)
	if len(page.Hits) != 0 || page.Scanned != SearchScanRows || page.Exhausted || page.Next == "" {
		t.Fatalf("the first page found %v, examined %d rows (exhausted %v, next %q), want no hit, "+
			"%d rows examined and a cursor past them", hitTurns(page.Hits), page.Scanned,
			page.Exhausted, page.Next, SearchScanRows)
	}
	next := e.search("ana", "zebra", page.Next, 0)
	if !slices.Equal(hitTurns(next.Hits), []string{older}) || !next.Exhausted || next.Next != "" ||
		next.Scanned != 1 {
		t.Errorf("the page after it found %v, examined %d (exhausted %v, next %q), want the older "+
			"match, one row and the seat's oldest reached", hitTurns(next.Hits), next.Scanned,
			next.Exhausted, next.Next)
	}
}
