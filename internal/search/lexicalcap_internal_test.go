package search

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// capDoc is one document of a corpus written straight into the index: its
// length and how often it holds each term.
type capDoc struct {
	id     string
	length int
	terms  map[string]int
}

// writeCapCorpus writes kb_docs and kb_postings rows directly, in one
// transaction.
//
// DIRECTLY rather than through a sweep, for [indexDoc]'s reason and a sharper
// one: the property is about a term with more than [maxPostingScan] postings,
// and tokenising that many pages to state a length and a frequency would be
// the slowest test in the package asserting what the rows already say.
func writeCapCorpus(t *testing.T, db *store.DB, docs []capDoc) {
	t.Helper()
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		limit := db.Caps().MaxVariables
		if _, err := store.InsertRows(t.Context(), tx, limit,
			`INSERT INTO kb_docs (id, source, source_id, search_shard, container,
			                      title, excerpt, length, source_rev, indexed_at)
			 VALUES`, `(?, 'page', ?, 0, 'ENG', ?, '', ?, 1, 0)`, "",
			len(docs), func(i int) []any {
				d := docs[i]
				return []any{"page:" + d.id, d.id, d.id, d.length}
			}); err != nil {
			return err
		}
		type row struct {
			doc, term string
			freq      int
		}
		var rows []row
		for _, d := range docs {
			for term, freq := range d.terms {
				rows = append(rows, row{"page:" + d.id, term, freq})
			}
		}
		_, err := store.InsertRows(t.Context(), tx, limit,
			`INSERT INTO kb_postings (doc_id, term, freq) VALUES`, `(?, ?, ?)`, "",
			len(rows), func(i int) []any {
				return []any{rows[i].doc, rows[i].term, rows[i].freq}
			})
		return err
	}); err != nil {
		t.Fatalf("write the corpus: %v", err)
	}
}

// A DOCUMENT PAST ONE TERM'S CAP IS RANKED ON EVERY TERM IT HOLDS.
//
// `common` is in six thousand documents, so its read stops at five thousand
// and `target`, the longest of them, is past the cut. `target` and `decoy`
// hold `rare` identically; `target` also holds `common`, so it truly outranks
// `decoy`. Read alone, the cut costs `target` its `common` contribution, the
// two tie on `rare`, and the tie goes to `decoy` by id — a document that
// matched fewer of the query's words, ranked first.
func TestADocumentPastACappedTermIsResolvedExactly(t *testing.T) {
	t.Parallel()
	db := openInternalStore(t)
	var docs []capDoc
	for i := range 6000 {
		docs = append(docs, capDoc{id: fmt.Sprintf("c%04d", i),
			length: 50 + i%100, terms: map[string]int{"common": 2}})
	}
	docs = append(docs,
		capDoc{id: "target", length: 400, terms: map[string]int{"rare": 1, "common": 1}},
		capDoc{id: "decoy", length: 400, terms: map[string]int{"rare": 1}},
	)
	writeCapCorpus(t, db, docs)
	x := NewIndexer(db)

	hits, err := x.Search(t.Context(), LexicalQuery{Text: "rare common", Limit: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 || hits[0].ID != "target" {
		t.Fatalf("ranked %v first — `target` holds both words and `decoy` one, "+
			"so a first place for anything else is the cap costing a document "+
			"a word it holds", hitIDs(hits))
	}

	// THE FIXTURE DISCRIMINATES: the cap really did leave `target` out of
	// the `common` read, or the first-place assertion proves nothing — and
	// the RESOLUTION is what put it back, not the whole read, which nothing
	// unread can send this query to: `rare` was read whole, so every
	// document no read reached lacks it, and `common`'s ceiling alone is
	// below the score of a document that holds it.
	corpus, err := x.corpus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lists := make([]termList, 0, 2)
	for _, term := range []string{"rare", "common"} {
		list, err := x.postings(t.Context(), term, LexicalQuery{}, corpus, maxPostingScan)
		if err != nil {
			t.Fatal(err)
		}
		lists = append(lists, list)
	}
	if !lists[1].capped {
		t.Fatal("the `common` read was not capped, so this case is not about the cap")
	}
	for _, p := range lists[1].postings {
		if p.DocID == "page:target" {
			t.Fatal("the capped `common` read reached `target`, so this case " +
				"is not about a document past the cut")
		}
	}
	if rankLists(lists, corpus).unreached(2) {
		t.Fatal("the first read leaves this query to the whole read, so the " +
			"first place above says nothing about the resolution")
	}
}

// A DOCUMENT NO CAPPED READ REACHED IS STILL FOUND.
//
// `alpha` and `beta` are each in six thousand documents of their own, and
// `both` holds the two words, but as the longest document either list holds,
// so both first reads stop before it. Its two contributions still sum past any
// single one's, so it is the true first place — and no first read reached it,
// so no resolution can find it. Only reading the capped lists whole does.
func TestADocumentNoCappedReadReachedIsFoundByTheWholeRead(t *testing.T) {
	t.Parallel()
	db := openInternalStore(t)
	var docs []capDoc
	for i := range 6000 {
		docs = append(docs,
			capDoc{id: fmt.Sprintf("a%04d", i), length: 50 + i%100,
				terms: map[string]int{"alpha": 1}},
			capDoc{id: fmt.Sprintf("b%04d", i), length: 50 + i%100,
				terms: map[string]int{"beta": 1}})
	}
	docs = append(docs, capDoc{id: "both", length: 149,
		terms: map[string]int{"alpha": 1, "beta": 1}})
	writeCapCorpus(t, db, docs)
	x := NewIndexer(db)

	hits, err := x.Search(t.Context(), LexicalQuery{Text: "alpha beta", Limit: 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "both" {
		t.Fatalf("ranked %v first — `both` holds both words and outscores every "+
			"document holding one, so anything else first is the cap choosing "+
			"the answer", hitIDs(hits))
	}

	// THE FIXTURE DISCRIMINATES: no first read reached `both`, so neither
	// they nor a resolution of what they reached can have put it first.
	corpus, err := x.corpus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"alpha", "beta"} {
		list, err := x.postings(t.Context(), term, LexicalQuery{}, corpus, maxPostingScan)
		if err != nil {
			t.Fatal(err)
		}
		if !list.capped {
			t.Fatalf("the %q read was not capped, so this case is not about the cap", term)
		}
		for _, p := range list.postings {
			if p.DocID == "page:both" {
				t.Fatalf("the capped %q read reached `both`, so a resolution "+
					"could find it and this case is not about the whole read", term)
			}
		}
	}
}

func hitIDs(hits []LexicalHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}
