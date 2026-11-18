package gitlet_test

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// scriptedWords is an index of changes' words that answers what a case scripts and counts its asks.
type scriptedWords struct {
	mu    sync.Mutex
	held  search.ChangeHeld
	asked int
}

func (w *scriptedWords) Holding(context.Context, search.ChangeQuery) (search.ChangeHeld, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked++
	return w.held, nil
}

func (w *scriptedWords) script(held search.ChangeHeld) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held = held
}

func (w *scriptedWords) asks() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.asked
}

// wordsReader is a reader of n's rows that takes its candidates from words.
func wordsReader(t *testing.T, n *node, words gitlet.ChangeWords) *gitlet.Reader {
	t.Helper()
	r, err := gitlet.NewReader(gitlet.ReaderOptions{Log: n.authority, ChangeWords: words,
		Now: func() time.Time { return harnessAt }, Roster: func() gitlet.Roster { return n.chart }})
	if err != nil {
		t.Fatalf("build a reader with an index of changes' words: %v", err)
	}
	return r
}

// realWords is this node's own index of changes' words, swept until it has caught up.
func realWords(t *testing.T, n *node) *search.ChangeIndexer {
	t.Helper()
	x := search.NewChangeIndexer(n.nodeDB, n.db.Reader(), gitlet.Domain{})
	sweepWords(t, x)
	return x
}

func sweepWords(t *testing.T, x *search.ChangeIndexer) {
	t.Helper()
	for range 1_000 {
		worked, err := x.Sweep(t.Context(), time.Now())
		if err != nil {
			t.Fatalf("sweep the index of changes' words: %v", err)
		}
		if worked == 0 {
			return
		}
	}
	t.Fatal("the index of changes' words never caught up")
}

// estateInstance is the name n's estate gives its own copy.
func estateInstance(t *testing.T, n *node) string {
	t.Helper()
	var instance string
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) (err error) {
		instance, err = statelog.EstateInstance(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatalf("read the estate's instance: %v", err)
	}
	return instance
}

// wordsPages is every page r answers q, limit rows a page.
func wordsPages(t *testing.T, r *gitlet.Reader, q gitlet.ReadQuery, limit int) []gitlet.ReadAnswer {
	t.Helper()
	q.Kind, q.Limit, q.Fresh = gitlet.ReadChanges, limit, stale
	var out []gitlet.ReadAnswer
	for range 100 {
		page, err := r.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(changes) as %+v: %v", q, err)
		}
		out = append(out, page)
		if page.Next == "" {
			return out
		}
		q.Cursor = page.Next
	}
	t.Fatalf("the listing %+v did not end in a hundred pages", q)
	return nil
}

// wordsListed is every change the pages list, by reference, and how wholly the last one searched.
func wordsListed(pages []gitlet.ReadAnswer) ([]string, *gitlet.ChangesText) {
	var refs []string
	for _, page := range pages {
		for _, c := range page.Changes {
			refs = append(refs, c.Ref)
		}
	}
	return refs, pages[len(pages)-1].ChangesText
}

// summariesOf is every change r lists for q, as its pages served them.
func summariesOf(t *testing.T, r *gitlet.Reader, q gitlet.ReadQuery) []gitlet.ChangeSummary {
	t.Helper()
	var out []gitlet.ChangeSummary
	for _, page := range wordsPages(t, r, q, 50) {
		out = append(out, page.Changes...)
	}
	return out
}

// A LISTING NARROWED BY WORDS IS EXACT AS OF ITS SNAPSHOT: a change edited past the index's watermark
// is judged by its row, a candidate at or below it is trusted only where the read holds every record
// the index was decided from, and an index that is not this copy's, or is rebuilding, is not used.
// Mutation: trust candidates where not covered, or trust every candidate whatever its row's position.
func TestAListingNarrowedByWordsIsExactAsOfItsSnapshot(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	pushedBy(t, c, repo, ada, update(liveTopicRef, gitlet.ZeroOID, oidOf("T")))
	open := func(source, title string) gitlet.ChangeResult {
		t.Helper()
		p := opening(repo, source, mainRef, dev)
		p.Open.Title = title
		res, err := c.ProposeChange(t.Context(), p)
		requireChanged(t, res, err)
		return res
	}
	held1 := open(featureRef, "Fix the login retry")
	moved2 := open(releaseRef, "Bump the cache")
	trusted3 := open(liveTopicRef, "Clean up")
	all := summariesOf(t, n.reader, gitlet.ReadQuery{Asker: ada, Repo: "web"})
	through := all[0].Position.Packed()
	if all[0].ID != trusted3.ChangeID {
		t.Fatalf("the premise: web!3 moved last, but the listing opens at %s", all[0].Ref)
	}
	// PAST THE WATERMARK: web!2 now holds the word, web!1 no longer does.
	retitle := func(res gitlet.ChangeResult, title string) {
		t.Helper()
		edited, err := c.ProposeChange(t.Context(), retitled(repo, res.ChangeID, dev, res.Version, title))
		requireChanged(t, edited, err)
	}
	retitle(held1, "Fix the retry")
	retitle(moved2, "Fix the login cache")

	instance := estateInstance(t, n)
	// The index says web!1 and web!3 hold "login": web!3's row, at the watermark, does not.
	indexed := search.ChangeHeld{IDs: []string{trusted3.ChangeID, held1.ChangeID}, Through: through,
		BuiltFrom: instance}
	words := &scriptedWords{}
	r := wordsReader(t, n, words)
	for _, tc := range []struct {
		name     string
		held     search.ChangeHeld
		tail     int
		want     []string
		building bool
	}{
		{"covered", indexed, gitlet.MaxChangeTextTail, []string{"web!3", "web!2"}, false},
		{"not covered", func() search.ChangeHeld { h := indexed; h.Seen = 1 << 62; return h }(),
			gitlet.MaxChangeTextTail, []string{"web!2"}, true},
		{"rebuilding", func() search.ChangeHeld { h := indexed; h.Rebuilding = true; return h }(),
			gitlet.MaxChangeTextTail, []string{"web!2"}, true},
		{"built from another copy", func() search.ChangeHeld { h := indexed; h.BuiltFrom = "another"; return h }(),
			gitlet.MaxChangeTextTail, []string{"web!2"}, true},
		// One change judged past the watermark, web!2's, the newest move; web!1, a candidate moved past
		// it beyond that bound, is judged too.
		{"a tail past its bound", indexed, 1, []string{"web!3", "web!2"}, true},
	} {
		words.script(tc.held)
		gitlet.SetWordsBounds(r, gitlet.MaxChangeTextCandidates, tc.tail)
		got, text := wordsListed(wordsPages(t, r, gitlet.ReadQuery{Asker: ada, Repo: "web", Text: "login"}, 50))
		if !slices.Equal(got, tc.want) || text == nil || text.Building != tc.building {
			t.Errorf("%s: lists %v (%+v), want %v building %v", tc.name, got, text, tc.want, tc.building)
		}
	}
}

// A WORDS LISTING NEVER ANSWERS A CHANGE ITS ASKER MAY NOT READ: a seat outside a unit repository's
// unit, or reading for a colleague outside it, lists none of its changes; a person lists every one; a
// deleted repository's change is listed only where it is named.
// Mutation: keep candidates without judging their repositories.
func TestAWordsListingNeverAnswersAChangeItsAskerMayNotRead(t *testing.T) {
	t.Parallel()
	n, ids := grantedChanges(t)
	all := summariesOf(t, n.reader, gitlet.ReadQuery{Asker: ada, State: gitlet.ChangeStateOpen})
	var everyID []string
	for _, s := range all {
		everyID = append(everyID, s.ID)
	}
	words := &scriptedWords{}
	r := wordsReader(t, n, words)
	instance := estateInstance(t, n)
	for name, held := range map[string]search.ChangeHeld{
		"candidates": {IDs: everyID, Through: all[0].Position.Packed(), BuiltFrom: instance},
		"the tail":   {BuiltFrom: instance},
	} {
		words.script(held)
		for _, tc := range []struct {
			who     gitlet.Principal
			ceiling string
			want    []string
		}{
			{agent("vp-sales"), "", []string{"platform!1", "sales-private!1"}},
			{agent("dev"), "vp-sales", []string{"platform!1"}},
			{ada, "", []string{"platform!1", "plat-private!1", "sales-private!1"}},
		} {
			got, _ := wordsListed(wordsPages(t, r, gitlet.ReadQuery{Asker: tc.who, CeilingSeat: tc.ceiling,
				Text: "upload"}, 50))
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s: %+v reading for %q lists %v, want %v", name, tc.who, tc.ceiling, got, tc.want)
			}
		}
	}
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t3), ids["platform"]); err != nil {
		t.Fatalf("delete platform: %v", err)
	}
	n.drain()
	words.script(search.ChangeHeld{BuiltFrom: instance})
	if got, _ := wordsListed(wordsPages(t, r, gitlet.ReadQuery{Asker: ada, Text: "upload"}, 50)); !slices.Equal(got,
		[]string{"plat-private!1", "sales-private!1"}) {
		t.Errorf("once platform is deleted the words list %v, want the two live repositories' changes", got)
	}
	if got, _ := wordsListed(wordsPages(t, r, gitlet.ReadQuery{Asker: ada, Repo: "platform", Text: "upload"},
		50)); !slices.Equal(got, []string{"platform!1"}) {
		t.Errorf("the deleted platform's own words listing is %v, want platform!1", got)
	}
}

// A WORDS LISTING KEEPS THE LISTING'S ORDER: by number in a repository, by opening in a state and
// for words alone, by last move for a live state so sorted and a window; each page full, and a kept
// prefix's cursor resumes at the next change.
// Mutation: sort by id, or ignore the cursor.
func TestAWordsListingKeepsTheListingsOrder(t *testing.T) {
	t.Parallel()
	n := liveWorld(t)
	c, _ := committerOn(t, n, n.pub)
	// TWO CHANGES NO LONGER HOLD THE WORD: web!3 and app!1.
	unmatched := map[string]bool{"web!3": true, "app!1": true}
	for _, s := range summariesOf(t, n.reader, gitlet.ReadQuery{Asker: ada, State: gitlet.ChangeStateOpen}) {
		if unmatched[s.Ref] {
			res, err := c.ProposeChange(t.Context(), retitled(s.RepoID, s.ID, ada, s.Version, "Tidy the docs"))
			requireChanged(t, res, err)
		}
	}
	r := wordsReader(t, n, realWords(t, n))
	refsOf := func(cs []gitlet.ChangeSummary) []string {
		var out []string
		for _, s := range cs {
			if !unmatched[s.Ref] {
				out = append(out, s.Ref)
			}
		}
		return out
	}
	var everyState []gitlet.ChangeSummary
	for _, state := range gitlet.ChangeStates() {
		everyState = append(everyState, summariesOf(t, n.reader, gitlet.ReadQuery{Asker: ada, State: state})...)
	}
	slices.SortFunc(everyState, func(a, b gitlet.ChangeSummary) int {
		return cmp.Or(cmp.Compare(b.OpenedPosition.Packed(), a.OpenedPosition.Packed()), strings.Compare(b.ID, a.ID))
	})
	for name, q := range map[string]gitlet.ReadQuery{
		"a repository":        {Repo: "web"},
		"a terminal state":    {State: gitlet.ChangeStateMerged},
		"a live state":        {State: gitlet.ChangeStateOpen},
		"a live state sorted": {State: gitlet.ChangeStateOpen, ChangeSort: gitlet.ChangeSortUpdated},
		"a window":            {Window: gitlet.ChangeWindow("7d")},
		"words alone":         {},
	} {
		q.Asker = ada
		want := refsOf(everyState)
		if name != "words alone" {
			want = refsOf(summariesOf(t, n.reader, q))
		}
		if len(want) < 2 && name != "a terminal state" {
			t.Fatalf("%s: the premise lists %v, too few to hold an order", name, want)
		}
		q.Text = "upload"
		for _, limit := range []int{1, 2, 50} {
			pages := wordsPages(t, r, q, limit)
			if got, _ := wordsListed(pages); !slices.Equal(got, want) {
				t.Errorf("%s, %d a page: the words list %v, want %v", name, limit, got, want)
			}
			for i, page := range pages[:len(pages)-1] {
				if len(page.Changes) != limit {
					t.Errorf("%s, %d a page: page %d holds %d rows with more to come", name, limit, i+1,
						len(page.Changes))
				}
			}
		}
		// A KEPT PREFIX RESUMES at the change after it.
		first := wordsPages(t, r, q, 50)[0]
		if len(first.Changes) < 2 {
			continue
		}
		q.Kind, q.Limit, q.Fresh, q.Cursor = gitlet.ReadChanges, 50, stale, q.ChangesCursor(first.Changes[0])
		rest, err := r.Read(t.Context(), q)
		if err != nil || len(rest.Changes) == 0 || rest.Changes[0].Ref != first.Changes[1].Ref {
			t.Errorf("%s: resumed after %s the words list %+v (%v), want %s first", name, first.Changes[0].Ref,
				rest.Changes, err, first.Changes[1].Ref)
		}
	}
}

// A WORDS LISTING IS TRUNCATED WHERE THE INDEX SAYS SO, and only where the index is this copy's.
// Mutation: drop the index's truncation, or pass it on from an index the read does not use.
func TestAWordsListingIsTruncatedWhereTheIndexSaysSo(t *testing.T) {
	t.Parallel()
	n, _ := grantedChanges(t)
	words := &scriptedWords{}
	r := wordsReader(t, n, words)
	instance := estateInstance(t, n)
	for _, tc := range []struct {
		builtFrom string
		want      bool
	}{{instance, true}, {"another", false}} {
		words.script(search.ChangeHeld{Truncated: true, BuiltFrom: tc.builtFrom})
		_, text := wordsListed(wordsPages(t, r, gitlet.ReadQuery{Asker: ada, Text: "upload"}, 50))
		if text == nil || text.Truncated != tc.want {
			t.Errorf("an index built from %q that says it truncated: the page says %+v, want truncated %v",
				tc.builtFrom, text, tc.want)
		}
	}
}

// THE WORDS ARE JUDGED ON EACH ROW ANOTHER WALK LEADS: beside a reviewer, a need, a work item or a
// readiness bucket the listing is that walk's rows holding the words, a page the rows it read, every
// page saying it was narrowed, the index never asked and a node keeping none serving it.
// Mutation: route these through the index, or let the words lead beside a readiness bucket.
func TestTheWordsAreJudgedOnEachRowAnotherWalkLeads(t *testing.T) {
	t.Parallel()
	n, repo, ids := readinessWorld(t, gitlet.MergeByWriters, everyStanding()...)
	c, _ := committerOn(t, n, n.pub)
	item := types.WorkItem{Backend: types.WorkJira, ID: "10042", Key: "ENG-7", Project: "ENG"}
	version := func(id string) int64 {
		return int64(n.count(`SELECT version FROM code_changes WHERE id = ?`, id))
	}
	edit := func(name string, set func(e *gitlet.ChangeEdit)) {
		t.Helper()
		e := &gitlet.ChangeEdit{RepoID: repo, ChangeID: ids[name]}
		set(e)
		res, err := c.ProposeChange(t.Context(), writing(ada, version(ids[name]), func(p *gitlet.ChangeProposal) {
			p.Edit = e
		}))
		requireChanged(t, res, err)
	}
	// a2, c1 and c3 no longer hold the word; a1 and a2 are of one work item.
	tidy := "Tidy the docs"
	edit("a1", func(e *gitlet.ChangeEdit) { e.WorkItem = &item })
	edit("a2", func(e *gitlet.ChangeEdit) { e.Title, e.WorkItem = &tidy, &item })
	edit("c1", func(e *gitlet.ChangeEdit) { e.Title = &tidy })
	edit("c3", func(e *gitlet.ChangeEdit) { e.Title = &tidy })
	unmatched := map[string]bool{"web!3": true, "web!5": true, "web!9": true}

	words := &scriptedWords{}
	readers := map[string]*gitlet.Reader{"an index": wordsReader(t, n, words), "no index": n.reader}
	emptyWithCursor := false
	for _, q := range []gitlet.ReadQuery{
		{Asker: ada, Reviewer: "plat-lead"},
		{Asker: cto, Needs: gitlet.NeedFix},
		{Asker: cto, Needs: gitlet.NeedMe},
		{Asker: dev, Needs: gitlet.NeedMe},
		{Asker: ada, Needs: gitlet.NeedPerson},
		{Asker: ada, Work: item.Ref()},
		{Asker: ada, Readiness: gitlet.BucketReady},
		{Asker: ada, Readiness: gitlet.BucketApproval},
		{Asker: ada, Readiness: gitlet.BucketChecks},
	} {
		var want []string
		for _, s := range summariesOf(t, n.reader, q) {
			if !unmatched[s.Ref] {
				want = append(want, s.Ref)
			}
		}
		q.Text = "upload"
		for name, r := range readers {
			pages := wordsPages(t, r, q, 1)
			if got, _ := wordsListed(pages); !slices.Equal(got, want) {
				t.Errorf("%s, %+v: the words list %v, want %v", name, q, got, want)
			}
			for i, page := range pages {
				if page.ChangesText == nil || *page.ChangesText != (gitlet.ChangesText{}) {
					t.Errorf("%s, %+v: page %d says %+v, want an empty changes_text", name, q, i+1,
						page.ChangesText)
				}
				emptyWithCursor = emptyWithCursor || len(page.Changes) == 0 && page.Next != ""
			}
		}
	}
	if !emptyWithCursor {
		t.Error("no page was empty with a cursor, so no case shows a page is the rows it read")
	}
	if asked := words.asks(); asked != 0 {
		t.Errorf("the index was asked %d times for listings another walk leads", asked)
	}
}

// A WORDS LISTING IS REFUSED PAST ITS BOUNDS: more bytes than a search takes, no word, or more words
// than one search takes; eight words, words alone and words in opening order are read.
// Mutation: drop the words' own check.
func TestAWordsListingIsRefusedPastItsBounds(t *testing.T) {
	t.Parallel()
	n, _ := grantedChanges(t)
	words := &scriptedWords{}
	words.script(search.ChangeHeld{BuiltFrom: estateInstance(t, n)})
	r := wordsReader(t, n, words)
	read := func(q gitlet.ReadQuery) error {
		q.Kind, q.Asker, q.Fresh = gitlet.ReadChanges, ada, stale
		_, err := r.Read(t.Context(), q)
		return err
	}
	for name, tc := range map[string]struct {
		q     gitlet.ReadQuery
		field string
	}{
		"past the bytes":      {gitlet.ReadQuery{Text: strings.Repeat("a", 401)}, "text:"},
		"no word":             {gitlet.ReadQuery{Text: "a b ."}, "text:"},
		"nine words":          {gitlet.ReadQuery{Text: "one two three four five six seven eight nine"}, "text:"},
		"words by last move":  {gitlet.ReadQuery{Text: "upload", ChangeSort: gitlet.ChangeSortUpdated}, "change_sort:"},
		"another kind's text": {gitlet.ReadQuery{Kind: gitlet.ReadRepos, Text: "upload"}, "text:"},
	} {
		if name == "another kind's text" {
			q := tc.q
			q.Asker, q.Fresh = ada, stale
			if _, err := r.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) ||
				!strings.Contains(err.Error(), tc.field) {
				t.Errorf("%s: %v, want ErrInvalid naming %s", name, err, tc.field)
			}
			continue
		}
		if err := read(tc.q); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"eight words":         {Text: "one two three four five six seven upload"},
		"words alone":         {Text: "upload"},
		"words by opening":    {Text: "upload", ChangeSort: gitlet.ChangeSortOpened},
		"words at the bounds": {Text: strings.Repeat("a", 400)},
	} {
		if err := read(q); err != nil {
			t.Errorf("%s: refused %v", name, err)
		}
	}
}

// A NODE KEEPING NO WORDS INDEX REFUSES WHAT THE WORDS LEAD, about itself, so the estate asks a node
// that keeps one, and serves the words beside another walk.
// Mutation: answer the words' listing as an empty one.
func TestANodeKeepingNoWordsIndexRefusesWhatTheWordsLead(t *testing.T) {
	t.Parallel()
	n, _ := grantedChanges(t)
	_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: ada, Text: "upload",
		Fresh: stale})
	if !errors.Is(err, gitlet.ErrNotServing) {
		t.Errorf("a node keeping no index answered a listing its words lead with %v, want ErrNotServing", err)
	}
	if _, err = n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: ada, Text: "upload",
		Readiness: gitlet.BucketApproval, Fresh: stale}); err != nil {
		t.Errorf("a node keeping no index refused the words beside a readiness bucket: %v", err)
	}
}

// A CHANGE EDITED A MOMENT AGO IS FOUND BY ITS NEW WORDS, by this node's own index: read before any
// sweep it is found by its new title and not its old, and so after one.
// Mutation: trust the index's candidates past its watermark, or read no tail.
func TestAChangeEditedAMomentAgoIsFoundByItsNewWords(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	x := realWords(t, n)
	r := wordsReader(t, n, x)
	edited, err := c.ProposeChange(t.Context(), retitled(repo, opened.ChangeID, dev, opened.Version,
		"Fix the flaky login"))
	requireChanged(t, edited, err)
	check := func(when string) {
		t.Helper()
		for word, want := range map[string][]string{"flaky": {"web!1"}, "upload": nil} {
			got, text := wordsListed(wordsPages(t, r, gitlet.ReadQuery{Asker: ada, Text: word}, 50))
			if !slices.Equal(got, want) || text == nil || *text != (gitlet.ChangesText{}) {
				t.Errorf("%s: %q lists %v (%+v), want %v and a whole search", when, word, got, text, want)
			}
		}
	}
	check("before a sweep")
	sweepWords(t, x)
	check("after a sweep")
}
