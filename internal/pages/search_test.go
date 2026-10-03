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
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
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
	})
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
	if !answer.Partitions.Complete() || answer.Partitions.Addressed != 0 {
		t.Errorf("one corpus searched alone states partition coverage %+v, want none",
			answer.Partitions)
	}
}

// ONE CORPUS SEARCHED ALONE IS ITS SLICE FUSED: the answer a node gives when it
// holds the whole knowledge base is exactly the fusion of its own slice — the
// path a gather takes over several — so there is one ranking rule, not two.
func TestASearchIsItsOwnSliceFused(t *testing.T) {
	t.Parallel()
	docs := map[string][2]string{}
	for i := range 8 {
		docs[fmt.Sprintf("eng-%02d", i)] = [2]string{"ENG",
			fmt.Sprintf("Deploy step %d %s", i, slices.Repeat([]string{"deploy "}, i))}
	}
	s := searchCorpus(t, docs)
	q := knowledge.Query{Text: "deploy", Limit: 5}
	slice, err := s.Slice(t.Context(), q)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	if len(slice.Candidates.Lexical) == 0 || len(slice.Hits) == 0 {
		t.Fatalf("the slice holds no candidates: %+v", slice)
	}
	merged := pages.MergeSearch([]pages.SearchSlice{slice}, q).Hits
	if got := s.Search(t.Context(), q).Hits; !reflect.DeepEqual(got, merged) || len(got) != 5 {
		t.Fatalf("searched %+v, want its own slice fused %+v", got, merged)
	}
}
