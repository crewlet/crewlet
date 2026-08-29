package plane_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plane"
)

// workspace is a Plane stand-in that records the searches it was asked for.
type workspace struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
	scopes  []string
	sizes   []string
	// pages answers a query; a query with no entry returns nothing, which
	// is what drives the relaxation ladder.
	pages map[string][]map[string]any
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	w := &workspace{pages: map[string][]map[string]any{}}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pages/search/"):
			q := r.URL.Query().Get("query")
			w.mu.Lock()
			w.queries = append(w.queries, q)
			w.scopes = append(w.scopes, r.URL.Query().Get("projects"))
			w.sizes = append(w.sizes, r.URL.Query().Get("per_page"))
			hits := w.pages[q]
			w.mu.Unlock()
			json.NewEncoder(rw).Encode(map[string]any{"results": hits})
		case strings.HasSuffix(r.URL.Path, "/projects/"):
			json.NewEncoder(rw).Encode(map[string]any{"results": []map[string]any{
				{"id": "proj-1", "identifier": "ENG", "name": "Engineering"},
				{"id": "proj-2", "identifier": "OPS", "name": "Operations"},
				{"id": "proj-skills", "identifier": "SKILLS", "name": "Tool Skills"},
			}})
		default:
			rw.Write([]byte(`{"results":[]}`))
		}
	}))
	t.Cleanup(w.Close)
	return w
}

func (w *workspace) asked() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.queries...)
}

func (w *workspace) scoped() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.scopes...)
}

func (w *workspace) pageSizes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.sizes...)
}

func page(id, name, project, body string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "project": project,
		"description_stripped": body, "updated_at": "2026-08-23T12:00:00Z",
	}
}

func searcher(t *testing.T, w *workspace, seatKey bool) *plane.Searcher {
	t.Helper()
	engine, err := plane.NewClient(plane.ClientOptions{
		URL: w.URL, Workspace: "nimbus", APIKey: "engine-key",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	opts := plane.SearcherOptions{
		Engine:        engine,
		Cache:         plane.NewProjectCache(engine, nil),
		SkillsProject: "SKILLS",
	}
	if seatKey {
		opts.ForSeat = func(*org.Role) (*plane.Client, bool) { return engine, true }
	}
	return plane.NewSearcher(opts)
}

func company(projects ...string) *org.Organization {
	o := &org.Organization{Name: "nimbus", PlaneProjects: projects,
		Roles: []*org.Role{{Name: "SWE", DeclaredHandle: "swe"}}}
	o.Normalize()
	return o
}

func TestASearchReturnsWhatTheWorkspaceKnows(t *testing.T) {
	w := newWorkspace(t)
	w.pages["deploy runbook staging"] = []map[string]any{
		page("page-1", "Deploy runbook", "proj-1",
			"Deploys go through the staging gate first. Then production."),
	}
	s := searcher(t, w, true)

	got := s.Search(t.Context(), knowledge.Query{
		Text: "deploy runbook staging", Org: company("ENG"),
	})
	if len(got) != 1 {
		t.Fatalf("got %d hits", len(got))
	}
	hit := got[0]
	if hit.Title != "Deploy runbook" || hit.PageID != "page-1" {
		t.Fatalf("hit = %+v", hit)
	}
	if hit.Container != "ENG" {
		t.Fatalf("container = %q, want the project identifier", hit.Container)
	}
	if !strings.HasSuffix(hit.URL, "/nimbus/projects/proj-1/pages/page-1") {
		t.Fatalf("url = %q", hit.URL)
	}
	if !strings.HasPrefix(hit.Snippet, "Deploys go through") {
		t.Fatalf("snippet = %q", hit.Snippet)
	}
	// The scope reached the server as project UUIDs: identifiers are a
	// 400 there, so a scope that was passed through verbatim would fail
	// the whole search rather than narrowing it.
	if got := w.scoped(); len(got) == 0 || got[0] != "proj-1" {
		t.Fatalf("the scope was sent as %q", w.scoped())
	}
}

// The server ANDs every token, so a six-term query from an auxiliary model
// matches nothing in a knowledge base of forty pages — and the honest
// outcome of a naive search is a planner concluding the company knows
// nothing about deploys.
func TestTheConjunctionRelaxesOnZeroHits(t *testing.T) {
	w := newWorkspace(t)
	// Only the FOUR-token prefix matches anything.
	w.pages["the deploy keeps failing"] = []map[string]any{
		page("page-1", "Deploy runbook", "proj-1", "Staging first."),
	}
	s := searcher(t, w, true)

	got := s.Search(t.Context(), knowledge.Query{
		Text: "the deploy keeps failing on staging again", Org: company("ENG"),
	})
	if len(got) != 1 {
		t.Fatalf("got %d hits after relaxing", len(got))
	}
	asked := w.asked()
	if len(asked) != 2 {
		t.Fatalf("asked %v, want the full query then the 4-token prefix", asked)
	}
	if asked[0] != "the deploy keeps failing on staging again" {
		t.Fatalf("the first attempt was %q", asked[0])
	}
	if asked[1] != "the deploy keeps failing" {
		t.Fatalf("the second attempt was %q", asked[1])
	}
}

// At most three searches in total, and never a single token: one term
// matches half the corpus and the endpoint ranks by RECENCY rather than
// relevance, so the result reads as noise wearing the shape of knowledge.
func TestTheLadderIsBoundedAndNeverReachesOneToken(t *testing.T) {
	w := newWorkspace(t)
	s := searcher(t, w, true)

	got := s.Search(t.Context(), knowledge.Query{
		Text: "one two three four five six", Org: company("ENG"),
	})
	if len(got) != 0 {
		t.Fatalf("a dry ladder returned %d hits", len(got))
	}
	asked := w.asked()
	if len(asked) != 3 {
		t.Fatalf("asked %v, want at most three attempts", asked)
	}
	for _, q := range asked {
		if len(strings.Fields(q)) < 2 {
			t.Fatalf("the ladder reached a single token: %q", q)
		}
	}
	// A query already short enough is asked ONCE: re-running the same
	// conjunction spends a round trip to discover the same nothing.
	w2 := newWorkspace(t)
	searcher(t, w2, true).Search(t.Context(), knowledge.Query{
		Text: "deploy", Org: company("ENG"),
	})
	if got := w2.asked(); len(got) != 1 {
		t.Fatalf("a one-token query was asked %d times: %v", len(got), got)
	}

	// And NO ATTEMPT REPEATS ANOTHER. A query exactly as long as the
	// first relax prefix would otherwise be sent twice, identically —
	// one round trip spent discovering the same nothing.
	w3 := newWorkspace(t)
	searcher(t, w3, true).Search(t.Context(), knowledge.Query{
		Text: "one two three four", Org: company("ENG"),
	})
	seen := map[string]bool{}
	for _, q := range w3.asked() {
		if seen[q] {
			t.Fatalf("the same conjunction was asked twice: %v", w3.asked())
		}
		seen[q] = true
	}
}

// Going over the server's distinct-token ceiling is a REJECTED request,
// which turns a wordy query into no knowledge at all rather than a looser
// match.
func TestAWordyQueryIsTrimmedRatherThanRefused(t *testing.T) {
	w := newWorkspace(t)
	s := searcher(t, w, true)

	long := make([]string, 0, 30)
	for i := range 30 {
		long = append(long, "term"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	s.Search(t.Context(), knowledge.Query{Text: strings.Join(long, " "), Org: company("ENG")})

	asked := w.asked()
	if len(asked) == 0 {
		t.Fatal("nothing was asked")
	}
	if n := len(strings.Fields(asked[0])); n > plane.MaxQueryTokens {
		t.Fatalf("the first attempt carried %d tokens, over the %d ceiling",
			n, plane.MaxQueryTokens)
	}
}

// UNSCOPED IS NOT UNBOUNDED. A seat on the shared engine credential searching
// unscoped would read pages its own account never could.
func TestASeatWithNoAccountOfItsOwnCannotSearchUnscoped(t *testing.T) {
	w := newWorkspace(t)
	w.pages["anything"] = []map[string]any{page("page-1", "Secret", "proj-1", "x")}

	shared := searcher(t, w, false)
	if shared.CanSearch(nil, company()) {
		t.Fatal("the gate admitted an unscoped search on a shared credential")
	}
	if got := shared.Search(t.Context(), knowledge.Query{Text: "anything", Org: company()}); len(got) != 0 {
		t.Fatalf("an unscoped shared-credential search returned %d hits", len(got))
	}
	if len(w.asked()) != 0 {
		t.Fatalf("it reached the network anyway: %v", w.asked())
	}

	// WITH a scope the operator named, the same seat searches it: the
	// narrowing is what makes it safe.
	if !shared.CanSearch(nil, company("ENG")) {
		t.Fatal("the gate refused a scoped search on a shared credential")
	}
	if got := shared.Search(t.Context(), knowledge.Query{Text: "anything", Org: company("ENG")}); len(got) != 1 {
		t.Fatalf("a scoped shared-credential search returned %d hits", len(got))
	}

	// And a seat with its OWN account searches unscoped, bounded by what
	// that account can read.
	own := searcher(t, w, true)
	if !own.CanSearch(nil, company()) {
		t.Fatal("the gate refused an unscoped search on a seat's own account")
	}
	if got := own.Search(t.Context(), knowledge.Query{Text: "anything", Org: company()}); len(got) != 1 {
		t.Fatalf("an unscoped own-account search returned %d hits", len(got))
	}
}

// An operator named a scope and none of it resolved. Searching unscoped
// would quietly ignore the narrowing they asked for.
func TestAnUnresolvableScopeSearchesNothing(t *testing.T) {
	w := newWorkspace(t)
	w.pages["anything"] = []map[string]any{page("page-1", "Page", "proj-1", "x")}
	s := searcher(t, w, true)

	got := s.Search(t.Context(), knowledge.Query{Text: "anything", Org: company("NOSUCH")})
	if len(got) != 0 {
		t.Fatalf("an unresolvable scope returned %d hits", len(got))
	}
	if len(w.asked()) != 0 {
		t.Fatalf("it searched anyway: %v", w.asked())
	}
}

// The skills project is machinery: a planner told to read a tool-skill page
// would follow an instruction meant for a different phase of a different
// turn. And the auto-draft prefix hides unreviewed drafts — which is the
// only mechanism there is here, because Plane pages carry no parent chain.
func TestMachineryAndDraftsAreNotKnowledge(t *testing.T) {
	w := newWorkspace(t)
	w.pages["deploy"] = []map[string]any{
		page("p-skill", "How to use the tracker", "proj-skills", "x"),
		page("p-draft", knowledge.AutoDraftTitlePrefix+"Deploy runbook", "proj-1", "x"),
		page("p-real", "Deploy runbook", "proj-1", "The real one."),
	}
	s := searcher(t, w, true)

	got := s.Search(t.Context(), knowledge.Query{Text: "deploy", Org: company()})
	if len(got) != 1 || got[0].PageID != "p-real" {
		t.Fatalf("admitted %+v, want only the published page", got)
	}

	// A caller who deliberately asks for drafts gets them — an EMPTY
	// non-nil exclusion is a different request from none at all.
	got = s.Search(t.Context(), knowledge.Query{
		Text: "deploy", Org: company(), ExcludeAncestors: []string{},
	})
	if len(got) != 2 {
		t.Fatalf("an explicit empty exclusion admitted %d, want the draft too", len(got))
	}
}

// A turn must not die because a wiki was slow.
func TestASearchNeverFailsItsCaller(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pages/search/") {
			mu.Lock()
			attempts++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()
	engine, err := plane.NewClient(plane.ClientOptions{
		URL: down.URL, Workspace: "nimbus", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	s := plane.NewSearcher(plane.SearcherOptions{
		Engine: engine, Cache: plane.NewProjectCache(engine, nil),
		ForSeat: func(*org.Role) (*plane.Client, bool) { return engine, true },
	})

	if got := s.Search(t.Context(), knowledge.Query{
		Text: "one two three four five six", Org: company(),
	}); got != nil {
		t.Fatalf("a failing workspace returned %+v", got)
	}
	// A FAILURE ends the ladder. Relaxing after a 500 spends two more
	// requests to be told the same thing by the same broken server.
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("a failing search was attempted %d times, want once", got)
	}
	// An empty query is not a search at all.
	if got := s.Search(t.Context(), knowledge.Query{Text: "   ", Org: company()}); got != nil {
		t.Fatalf("an empty query returned %+v", got)
	}
	var nilSearcher *plane.Searcher
	if nilSearcher.CanSearch(nil, company()) {
		t.Fatal("a nil searcher claimed it could search")
	}
	if got := nilSearcher.Search(t.Context(), knowledge.Query{Text: "x"}); got != nil {
		t.Fatalf("a nil searcher returned %+v", got)
	}
}

// The overfetch window exists so the post-filters still leave enough hits,
// which means more rows come back than the caller asked for — and the
// caller's limit is what bounds the block that is re-sent on every round of
// the Plan phase.
func TestTheCallersLimitBoundsWhatComesBack(t *testing.T) {
	w := newWorkspace(t)
	var many []map[string]any
	for i := range 20 {
		many = append(many, page("p"+string(rune('a'+i)), "Page", "proj-1", "body"))
	}
	w.pages["deploy"] = many
	s := searcher(t, w, true)

	got := s.Search(t.Context(), knowledge.Query{Text: "deploy", Org: company(), Limit: 3})
	if len(got) != 3 {
		t.Fatalf("a limit of 3 returned %d hits", len(got))
	}
	// The DEFAULT limit bounds it too, since that is what the prefetch
	// asks with.
	got = s.Search(t.Context(), knowledge.Query{Text: "deploy", Org: company()})
	if len(got) != knowledge.DefaultLimit {
		t.Fatalf("the default limit returned %d hits, want %d", len(got), knowledge.DefaultLimit)
	}
}

// Asking for more than the endpoint accepts is a 400, which turns an
// over-eager caller into NO knowledge rather than slightly less of it.
func TestAnOverLargeLimitIsClampedRatherThanRefused(t *testing.T) {
	// THROUGH THE ORDINARY STAND-IN, not a handler swapped onto it:
	// http.Server documents its fields as unwritable once Serve has begun,
	// and net/http reads Handler from every connection's own goroutine —
	// so replacing it under a live server is a data race with whatever is
	// still in flight, and the `seen` slice it closed over was a second
	// one.
	w := newWorkspace(t)
	s := searcher(t, w, true)

	s.Search(t.Context(), knowledge.Query{Text: "deploy", Org: company(), Limit: 5000})
	sizes := w.pageSizes()
	if len(sizes) == 0 {
		t.Fatal("nothing was asked")
	}
	if sizes[0] != "100" {
		t.Fatalf("per_page was sent as %q, want the endpoint's ceiling", sizes[0])
	}
}

func TestTheSearcherSatisfiesTheSeam(t *testing.T) {
	w := newWorkspace(t)
	var _ knowledge.Searcher = searcher(t, w, true)
	if got := searcher(t, w, true).Backend(); got != plane.Backend {
		t.Fatalf("Backend = %q", got)
	}
}
