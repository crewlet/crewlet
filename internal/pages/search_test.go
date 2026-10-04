package pages_test

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// searchCorpus is a store holding the pages given, indexed, and a searcher over
// it that hides the tool-skills container.
func searchCorpus(t *testing.T, docs map[string][2]string) *pages.Searcher {
	t.Helper()
	s, db := searchStore(t, docs)
	t.Cleanup(func() { _ = db.Close() })
	return s
}

// searchStore is [searchCorpus] with the store handed back, for a case that
// takes it away from under the searcher.
func searchStore(t *testing.T, docs map[string][2]string) (*pages.Searcher, *store.DB) {
	t.Helper()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	estate := storetest.Partition(t, storetest.EstateOf(db))
	for id, doc := range docs {
		if _, err := estate.SQL().ExecContext(t.Context(), `
			INSERT INTO pages_heads (id, container, parent_id, title, title_norm, body,
			                         status, author, edit_version, created_at,
			                         updated_at, version, scoped_through, document)
			VALUES (?, ?, '', ?, lower(?), ?, 'published', '', 1, 0, 0, 1, 0, '{}')`,
			id, doc[0], doc[1], doc[1], "how we deploy: "+doc[1]); err != nil {
			t.Fatalf("insert page %s: %v", id, err)
		}
	}
	x := search.NewIndexer(db, storetest.EstateOf(db).Reader())
	for quiet := 0; quiet < 2; {
		worked, err := x.Sweep(t.Context())
		if err != nil {
			t.Fatalf("index sweep: %v", err)
		}
		if worked {
			quiet = 0
		} else {
			quiet++
		}
	}
	return pages.NewSearcher(pages.SearcherOptions{
		Index: x, SkillsContainer: func() string { return "SKILLS" },
	}), db
}

// A TOOL-SKILL PAGE COSTS THE ANSWER THAT PAGE AND NEVER A PLACE: a search walks
// the fused order past the pages it never returns, so a company whose skills
// lead every ranking still gets a full answer — where cutting first and
// filtering after handed it the short list, or none.
func TestAToolSkillPageCostsTheAnswerNoPlace(t *testing.T) {
	t.Parallel()
	docs := map[string][2]string{}
	for i := range 12 {
		// THE SKILLS LEAD: their titles are the query itself.
		docs[fmt.Sprintf("skill-%02d", i)] = [2]string{"SKILLS", "deploy deploy deploy"}
	}
	for i := range 3 {
		docs[fmt.Sprintf("eng-%02d", i)] = [2]string{"ENG", fmt.Sprintf("Release notes %d", i)}
	}
	s := searchCorpus(t, docs)
	answer := s.Search(t.Context(), knowledge.Query{Text: "deploy", Limit: 3})
	if len(answer.Hits) != 3 {
		t.Fatalf("answered %d hits, want the three ENG pages past twelve hidden skills: %+v",
			len(answer.Hits), answer.Hits)
	}
	for _, hit := range answer.Hits {
		if hit.Container != "ENG" {
			t.Errorf("a %s page was returned: %+v", hit.Container, hit)
		}
	}
}

// A SEARCH THAT COULD NOT RUN IS AN ERROR TO WHOEVER ANSWERS FOR IT, and an
// empty answer that says so to a turn.
//
// [pages.Searcher.Answer] is what the estate router serves a search with, and
// its failure is what lets the asking node tell "the knowledge base could not
// be searched" from "nothing matched": degraded to an empty answer here, the
// router would carry an empty list back as if it were the company's. Search,
// which a turn reads, stays best effort — no hits, nothing served, and this
// node named as the participant that covered none of its range.
func TestASearchThatCouldNotRunIsAnErrorToItsRouter(t *testing.T) {
	t.Parallel()
	s, db := searchStore(t, map[string][2]string{"eng-00": {"ENG", "Deploy runbook"}})
	q := knowledge.Query{Text: "deploy", Limit: 5}
	if got := s.Search(t.Context(), q); len(got.Hits) != 1 {
		t.Fatalf("the open store answered %+v, want its one page", got)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if answer, err := s.Answer(t.Context(), q); err == nil {
		t.Fatalf("a search over a closed store answered %+v, want its error", answer)
	}
	got := s.Search(t.Context(), q)
	if len(got.Hits) != 0 || got.ServedMode != "" || got.Coverage.Complete ||
		got.Coverage.BucketsMissing != search.SearchShards {
		t.Fatalf("a search that could not run answered %+v, want no hits, nothing served "+
			"and every bucket missing", got.Outcome)
	}
}

// ANSWER AND SEARCH ARE ONE RANKING: a search that runs answers the same hits
// whether the router asked for its error or a turn asked best effort.
func TestASearchThatRanAnswersTheSameEitherWay(t *testing.T) {
	t.Parallel()
	docs := map[string][2]string{}
	for i := range 8 {
		docs[fmt.Sprintf("eng-%02d", i)] = [2]string{"ENG",
			fmt.Sprintf("Deploy step %d %s", i, slices.Repeat([]string{"deploy "}, i))}
	}
	s := searchCorpus(t, docs)
	q := knowledge.Query{Text: "deploy", Limit: 5}
	answered, err := s.Answer(t.Context(), q)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if got := s.Search(t.Context(), q); !reflect.DeepEqual(got, answered) || len(got.Hits) != 5 {
		t.Fatalf("searched %+v, want what Answer answered %+v", got, answered)
	}
}
