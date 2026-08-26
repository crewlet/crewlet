package confluence_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

const (
	acctLead   = "712020:aaaa-lead"
	acctWriter = "712020:bbbb-writer"
	acctSWE    = "712020:cccc-swe"
	acctOther  = "712020:dddd-outsider"
)

// THE REST BASE IS THE SINGLE MOST LIKELY WAY TO HAVE A CORRECT CREDENTIAL
// FAIL EVERYTHING.
//
// A Cloud site serves Confluence under /wiki and a gateway does not; getting
// it wrong lands every call on the Jira side of the same host, or 404s.
func TestTheRESTBaseIsDerivedPerDeployment(t *testing.T) {
	t.Parallel()
	for address, want := range map[string]string{
		"https://acme.atlassian.net":                  "https://acme.atlassian.net/wiki",
		"https://acme.atlassian.net/":                 "https://acme.atlassian.net/wiki",
		"https://acme.atlassian.net/wiki":             "https://acme.atlassian.net/wiki",
		"https://api.atlassian.com/ex/confluence/abc": "https://api.atlassian.com/ex/confluence/abc",
		"https://wiki.example.com":                    "https://wiki.example.com",
		"https://confluence.internal:8443/confluence": "https://confluence.internal:8443/confluence",
	} {
		if got := confluence.RESTBase(address); got != want {
			t.Errorf("RESTBase(%q) = %q, want %q", address, got, want)
		}
	}
}

// instance is a Confluence that records what it was asked.
type instance struct {
	*httptest.Server

	mu      sync.Mutex
	paths   []string
	queries []string
	auth    []string
	bodies  []map[string]any
	reply   func(path string) (int, string)
}

func newInstance(t *testing.T, reply func(path string) (int, string)) *instance {
	t.Helper()
	inst := &instance{reply: reply}
	inst.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		inst.mu.Lock()
		inst.paths = append(inst.paths, req.URL.Path)
		inst.queries = append(inst.queries, req.URL.RawQuery)
		inst.auth = append(inst.auth, req.Header.Get("Authorization"))
		inst.bodies = append(inst.bodies, body)
		inst.mu.Unlock()

		status, payload := inst.reply(req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(inst.Close)
	return inst
}

func (i *instance) lastQuery() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.queries) == 0 {
		return ""
	}
	return i.queries[len(i.queries)-1]
}

func client(t *testing.T, inst *instance) *confluence.Client {
	t.Helper()
	c, err := confluence.NewClient(confluence.ClientOptions{URL: inst.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A SEARCH SCOPED TO NOTHING, ON THE ORG CREDENTIAL, IS REFUSED.
//
// An empty scope plus a seat riding the shared org account means searching
// the whole instance — which is how one seat reads a page its own account
// never could.
func TestAnUnscopedSearchOnTheOrgCredentialIsRefused(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) {
		t.Error("a request was made for a search that should have been refused")
		return 200, `{"results":[]}`
	})
	searcher := confluence.NewSearcher(confluence.SearcherOptions{Org: client(t, inst)})

	o := &org.Organization{Name: "nimbus"}
	o.Normalize()
	if searcher.CanSearch(&org.Role{Name: "SWE"}, o) {
		t.Fatal("the pre-gate allowed a search that cannot be permitted")
	}
	if hits := searcher.Search(context.Background(), knowledge.Query{
		Text: "how do we deploy", Org: o, Seat: &org.Role{Name: "SWE"},
	}); len(hits) != 0 {
		t.Fatalf("hits = %+v", hits)
	}
}

// A SEAT WITH ITS OWN CREDENTIAL SEARCHES UNSCOPED, because Confluence's own
// ACLs bound what comes back.
func TestASeatWithItsOwnCredentialSearchesUnscoped(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) {
		return 200, `{"results":[{"id":"1","title":"Deploy runbook",
			"space":{"key":"ENG"},"body":{"storage":{"value":"<p>Run the pipeline.</p>"}}}]}`
	})
	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe"}
	searcher := confluence.NewSearcher(confluence.SearcherOptions{
		Org: client(t, inst),
		ForSeat: func(*org.Role) (*confluence.Client, bool) {
			return client(t, inst), true
		},
	})
	o := &org.Organization{Name: "nimbus"}
	o.Normalize()

	if !searcher.CanSearch(seat, o) {
		t.Fatal("a seat with its own credential was refused the pre-gate")
	}
	hits := searcher.Search(context.Background(), knowledge.Query{
		Text: "how do we deploy", Org: o, Seat: seat,
	})
	if len(hits) != 1 || hits[0].Title != "Deploy runbook" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Snippet != "Run the pipeline." {
		t.Errorf("snippet = %q", hits[0].Snippet)
	}
	if q := inst.lastQuery(); !strings.Contains(q, "type+%3D+page") {
		t.Errorf("query = %q", q)
	}
	if strings.Contains(inst.lastQuery(), "space+IN") {
		t.Error("an unscoped search sent a space filter")
	}
}

// A CONFIGURED SCOPE NARROWS THE QUERY, and it is the ORG-WIDE scope: a
// unit's own space is its identity, and letting an identity double as a read
// scope is how an agent ends up unable to read the page it was told to
// follow.
func TestAConfiguredScopeNarrowsTheQuery(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) { return 200, `{"results":[]}` })
	searcher := confluence.NewSearcher(confluence.SearcherOptions{Org: client(t, inst)})

	o := &org.Organization{Name: "nimbus"}
	o.ConfluenceSpaces = []string{"handbook", "ENG"}
	o.Units = []*org.Unit{{Name: "Ops", ConfluenceSpace: "OPS"}}
	o.Normalize()

	searcher.Search(context.Background(), knowledge.Query{
		Text: "deploy", Org: o, Seat: &org.Role{Name: "SWE"},
	})
	q := inst.lastQuery()
	for _, want := range []string{"HANDBOOK", "ENG"} {
		if !strings.Contains(q, want) {
			t.Errorf("the scope is missing %s: %s", want, q)
		}
	}
	if strings.Contains(q, "OPS") {
		t.Error("a unit's own space narrowed the org-wide read scope")
	}
}

// AN AUTO-DRAFTED SKILL MUST NOT REACH A PLANNER during its review window,
// and the exclusion has TWO tests because the first can silently stop
// matching.
func TestAutoDraftsAreHiddenByAncestorAndByTitle(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) {
		return 200, `{"results":[
			{"id":"1","title":"Draft by ancestor","space":{"key":"ENG"},
			 "ancestors":[{"title":"Auto-Drafted Skills"}],
			 "body":{"storage":{"value":"<p>unreviewed</p>"}}},
			{"id":"2","title":"[Auto-draft] Draft by title","space":{"key":"ENG"},
			 "body":{"storage":{"value":"<p>unreviewed</p>"}}},
			{"id":"3","title":"Real page","space":{"key":"ENG"},
			 "body":{"storage":{"value":"<p>reviewed</p>"}}}]}`
	})
	searcher := confluence.NewSearcher(confluence.SearcherOptions{
		Org: client(t, inst),
		ForSeat: func(*org.Role) (*confluence.Client, bool) {
			return client(t, inst), true
		},
	})
	o := &org.Organization{Name: "nimbus"}
	o.Normalize()

	hits := searcher.Search(context.Background(), knowledge.Query{
		Text: "deploy", Org: o, Seat: &org.Role{Name: "SWE"},
	})
	if len(hits) != 1 || hits[0].Title != "Real page" {
		t.Fatalf("an unreviewed draft reached the planner: %+v", hits)
	}
}

// THE SKILLS SPACE IS MACHINERY, not knowledge: a planner told to read a
// tool skill would follow an instruction written for a different phase.
func TestTheSkillsSpaceIsNotKnowledge(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) {
		return 200, `{"results":[
			{"id":"1","title":"A skill","space":{"key":"TS"},
			 "body":{"storage":{"value":"<p>x</p>"}}},
			{"id":"2","title":"A page","space":{"key":"ENG"},
			 "body":{"storage":{"value":"<p>y</p>"}}}]}`
	})
	searcher := confluence.NewSearcher(confluence.SearcherOptions{
		Org: client(t, inst), SkillsSpace: "ts",
		ForSeat: func(*org.Role) (*confluence.Client, bool) {
			return client(t, inst), true
		},
	})
	o := &org.Organization{Name: "nimbus"}
	o.Normalize()

	hits := searcher.Search(context.Background(), knowledge.Query{
		Text: "x", Org: o, Seat: &org.Role{Name: "SWE"},
	})
	if len(hits) != 1 || hits[0].Container != "ENG" {
		t.Fatalf("a tool-skill page was returned as knowledge: %+v", hits)
	}
}

// A SEARCH NEVER FAILS THE CALLER: a turn must not die because a wiki was
// slow, so every failure path is an empty result.
func TestASearchFailureIsAnEmptyBlock(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) {
		return 500, `{"message":"the instance is unwell"}`
	})
	searcher := confluence.NewSearcher(confluence.SearcherOptions{
		Org: client(t, inst),
		ForSeat: func(*org.Role) (*confluence.Client, bool) {
			return client(t, inst), true
		},
	})
	o := &org.Organization{Name: "nimbus"}
	o.Normalize()
	if hits := searcher.Search(context.Background(), knowledge.Query{
		Text: "deploy", Org: o, Seat: &org.Role{Name: "SWE"},
	}); hits != nil {
		t.Fatalf("hits = %+v", hits)
	}
}

// THE CQL IS ESCAPED, or a page title with a quote in it would end the
// literal early and the query would mean something nobody wrote.
func TestTheQueryIsEscapedAndCapped(t *testing.T) {
	t.Parallel()
	got := confluence.BuildCQL(`say "hello" \ now`, []string{`EN"G`}, false)
	if !strings.Contains(got, `\"hello\"`) || !strings.Contains(got, `\\`) {
		t.Fatalf("cql = %s", got)
	}
	if !strings.Contains(got, `"EN\"G"`) {
		t.Errorf("the space key was not escaped: %s", got)
	}
	// The terms only — "text ~" itself contains an x, which is exactly the
	// kind of thing an assertion over the whole string gets wrong.
	long := confluence.BuildCQL(strings.Repeat("x", confluence.MaxQueryChars+50), nil, true)
	_, terms, _ := strings.Cut(long, `text ~ "`)
	terms = strings.TrimSuffix(terms, `"`)
	if len(terms) != confluence.MaxQueryChars {
		t.Errorf("a long query was capped to %d characters", len(terms))
	}
	// AN EMPTY RESULT IS A REFUSAL, and the caller skips the request.
	if confluence.BuildCQL("deploy", nil, false) != "" {
		t.Error("an unscoped search on the org credential built a query")
	}
	if confluence.BuildCQL("   ", []string{"ENG"}, true) != "" {
		t.Error("an empty query built a search")
	}
}

// STORAGE FORMAT BECOMES PROSE, and the block boundaries survive — a page
// whose bulleted list collapses into one sentence reads to a model as a
// different document.
func TestFlatteningKeepsBlockBoundaries(t *testing.T) {
	t.Parallel()
	got := confluence.Flatten(
		`<p>Steps:</p><ul><li>open the app</li><li>log in</li></ul>` +
			`<p>Then &amp; only then<br/>call support.</p>`)
	for _, want := range []string{"Steps:", "open the app", "log in", "Then & only then"} {
		if !strings.Contains(got, want) {
			t.Errorf("flattened text is missing %q:\n%s", want, got)
		}
	}
	// THE NEWLINES ARE THE POINT, not merely that the words are apart: a
	// page whose bulleted list collapses onto one line reads to a model as
	// a paragraph rather than as a list of steps.
	lines := strings.Split(got, "\n")
	var onItsOwnLine int
	for _, line := range lines {
		if strings.TrimSpace(line) == "open the app" || strings.TrimSpace(line) == "log in" {
			onItsOwnLine++
		}
	}
	if onItsOwnLine != 2 {
		t.Errorf("the list items did not survive as lines:\n%q", got)
	}
	if strings.Contains(got, "<") {
		t.Errorf("markup survived:\n%s", got)
	}
}

// A SKILL PAGE DECODES FROM BOTH CODE-BLOCK SHAPES, because which one a page
// carries depends on how it was authored — and an undecodable skill page is
// indistinguishable from an ordinary page.
func TestASkillPageDecodesFromEitherShape(t *testing.T) {
	t.Parallel()
	frontmatter := "key: deploy\ntrigger:\n  tools: [run_pipeline]"
	macro := `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">yaml` +
		`</ac:parameter><ac:plain-text-body><![CDATA[` + frontmatter +
		`]]></ac:plain-text-body></ac:structured-macro><p>Always tag the release.</p>`
	plain := `<pre><code>` + frontmatter + `</code></pre><p>Always tag the release.</p>`

	for name, storage := range map[string]string{"macro": macro, "pre/code": plain} {
		got := confluence.DecodeSkillPage(storage)
		if !strings.Contains(got, "key: deploy") || !strings.Contains(got, "Always tag the release") {
			t.Errorf("%s decoded to:\n%s", name, got)
		}
	}
	// AN ORDINARY PAGE IS NOT A BROKEN SKILL. Reporting one as such would
	// fill the log with findings nobody can act on.
	if got := confluence.DecodeSkillPage(`<p>Just some notes.</p>`); got != "" {
		t.Errorf("an ordinary page decoded as a skill: %q", got)
	}
}

// THE ENCODE IS THE INVERSE OF THE DECODE. A skill promoted from a draft is
// re-written, and a round trip that changed what the page means would change
// what every seat is told.
func TestASkillPageRoundTrips(t *testing.T) {
	t.Parallel()
	frontmatter := "key: deploy\ntrigger:\n  tools: [run_pipeline]"
	storage := confluence.EncodeSkillPage(frontmatter, "Always tag the release.")
	got := confluence.DecodeSkillPage(storage)
	if !strings.Contains(got, "key: deploy") {
		t.Fatalf("the frontmatter did not survive:\n%s", got)
	}
	if !strings.Contains(got, "Always tag the release.") {
		t.Fatalf("the body did not survive:\n%s", got)
	}
}

// --- routing ---------------------------------------------------------- //

func registry(t *testing.T) *notify.Registry {
	t.Helper()
	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Eng Lead", DeclaredHandle: "lead"},
		{Name: "SWE", DeclaredHandle: "swe"},
		{Name: "Writer", DeclaredHandle: "writer"},
	}}
	o.Normalize()
	reg := notify.NewRegistry(o)
	for id, handle := range map[string]string{
		acctLead: "lead", acctSWE: "swe", acctWriter: "writer",
	} {
		if err := reg.Register(confluence.Backend, id, handle); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func parser(t *testing.T, mutate func(*confluence.ParserOptions)) *confluence.Parser {
	t.Helper()
	opts := confluence.ParserOptions{
		SiteURL:     "https://wiki.example.com",
		Leads:       map[string]string{"ENG": "lead"},
		SkillsSpace: "TS",
	}
	if mutate != nil {
		mutate(&opts)
	}
	return confluence.NewParser(opts)
}

func pageEvent(event, space, storage, actor string) types.RawWebhook {
	return types.RawWebhook{Body: map[string]any{
		"event": event,
		"user":  map[string]any{"accountId": actor, "displayName": "Ana"},
		"page": map[string]any{
			"id": "1001", "title": "Deploy runbook",
			"space": map[string]any{"key": space},
			"body":  map[string]any{"storage": map[string]any{"value": storage}},
		},
	}}
}

func route(t *testing.T, p *confluence.Parser, w types.RawWebhook) []notify.Routed {
	t.Helper()
	out, err := p.Parse(context.Background(), w, registry(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return out
}

// BEING MENTIONED IS THE ONE DIRECTED ASK a wiki produces, and it outranks
// the space fallback.
func TestAMentionReachesTheColleagueNamed(t *testing.T) {
	t.Parallel()
	storage := `<p>Can <ri:user ri:account-id="` + acctSWE + `"/> confirm this?</p>`
	got := route(t, parser(t, nil), pageEvent("page_updated", "ENG", storage, acctWriter))
	if len(got) != 1 {
		t.Fatalf("want one copy, got %d", len(got))
	}
	if got[0].To.ExternalIDs[0] != acctSWE {
		t.Errorf("addressed to %+v", got[0].To)
	}
	if got[0].Metadata[confluence.RoutedViaField] != confluence.ViaMention {
		t.Errorf("routed via %q", got[0].Metadata[confluence.RoutedViaField])
	}
}

// A DATA CENTER MENTION IS SPELLED DIFFERENTLY, and a parser that knew one
// spelling would route nothing on the other — silently, because "nobody was
// mentioned" is an ordinary answer.
func TestADataCentreMentionAlsoRoutes(t *testing.T) {
	t.Parallel()
	storage := `<p>Ping <ri:user ri:userkey="` + acctSWE + `"/></p>`
	got := route(t, parser(t, nil), pageEvent("page_updated", "ENG", storage, acctWriter))
	if len(got) != 1 || got[0].To.ExternalIDs[0] != acctSWE {
		t.Fatalf("copies = %+v", got)
	}
}

// A PAGE NOBODY WAS NAMED IN REACHES THE SPACE'S LEAD — not as a request,
// which the prompt is careful about, but because a space's documentation
// changing under a team is something its lead is the only one positioned to
// notice.
func TestAnUnattributedPageChangeReachesTheSpaceLead(t *testing.T) {
	t.Parallel()
	got := route(t, parser(t, nil), pageEvent("page_updated", "ENG", "<p>edited</p>", acctOther))
	if len(got) != 1 || got[0].To.Handle != "lead" {
		t.Fatalf("copies = %+v", got)
	}
	if got[0].Metadata[confluence.RoutedViaField] != confluence.ViaSpaceLead {
		t.Errorf("routed via %q", got[0].Metadata[confluence.RoutedViaField])
	}
}

// THE LEAD'S OWN EDIT MUST NOT RE-TRIGGER THE LEAD, or a lead writing in
// their own team's space wakes themselves for as long as nobody looks.
func TestTheLeadIsNotWokenByTheirOwnEdit(t *testing.T) {
	t.Parallel()
	if got := route(t, parser(t, nil),
		pageEvent("page_updated", "ENG", "<p>edited</p>", acctLead)); len(got) != 0 {
		t.Fatalf("the lead woke themselves: %+v", got)
	}
}

// THE SKILLS SPACE IS INDEXED AND NEVER ROUTED: a seat woken because its own
// company's prompt fragment changed has nothing to do about it — but the
// registry has to hear about every change, including that one.
func TestTheSkillsSpaceIsIndexedAndNotRouted(t *testing.T) {
	t.Parallel()
	var indexed []string
	p := parser(t, func(o *confluence.ParserOptions) {
		// THE SKILLS SPACE HAS A LEAD in this fixture, deliberately:
		// without one the space fallback would find nobody and the test
		// would pass with the exclusion removed.
		o.Leads = map[string]string{"ENG": "lead", "TS": "lead"}
		o.OnPage = func(_ context.Context, _, pageID string) error {
			indexed = append(indexed, pageID)
			return nil
		}
	})
	if got := route(t, p, pageEvent("page_updated", "TS", "<p>a skill</p>", acctWriter)); len(got) != 0 {
		t.Fatalf("a skills-space page woke somebody: %+v", got)
	}
	if len(indexed) != 1 || indexed[0] != "1001" {
		t.Fatalf("the indexer did not see the change: %v", indexed)
	}
}

// SPACE BOOKKEEPING NAMES NOBODY. Routing a label or a permission change
// spends a turn on "somebody added a label".
func TestEventsThatConcernNobodyRouteNothing(t *testing.T) {
	t.Parallel()
	p := parser(t, nil)
	for _, event := range []string{
		"label_added", "space_permissions_updated", "attachment_created", "",
	} {
		if got := route(t, p, pageEvent(event, "ENG", "<p>x</p>", acctWriter)); len(got) != 0 {
			t.Errorf("%q produced %d notification(s)", event, len(got))
		}
	}
}

// A SPACE NOBODY LEADS ROUTES TO NOBODY rather than to a guess.
func TestAnUnmappedSpaceRoutesToNobody(t *testing.T) {
	t.Parallel()
	if got := route(t, parser(t, nil),
		pageEvent("page_updated", "OPS", "<p>x</p>", acctWriter)); len(got) != 0 {
		t.Fatalf("copies = %+v", got)
	}
}

// THE TRIGGER CARRIES AN EXTRACT AND A POINTER, never the whole page: a wiki
// page can be tens of kilobytes, and most recipients read the trigger and
// drop it.
func TestTheTriggerIsAnExtractAndAPointer(t *testing.T) {
	t.Parallel()
	long := "<p>" + strings.Repeat("word ", 500) + "</p>"
	got := route(t, parser(t, nil), pageEvent("page_updated", "ENG", long, acctWriter))
	if len(got) != 1 {
		t.Fatalf("copies = %+v", got)
	}
	if len(got[0].Body) > 700 {
		t.Errorf("the whole page travelled: %d bytes", len(got[0].Body))
	}
	if got[0].Metadata["url"] != "https://wiki.example.com/wiki/spaces/ENG/pages/1001" {
		t.Errorf("url = %q", got[0].Metadata["url"])
	}
	if !(confluence.Prompt{}).RequiresRecon(got[0].Inbound) {
		t.Error("an extract was not marked as a pointer")
	}
	rendered := (confluence.Prompt{}).Build(got[0].Inbound, nil)
	if !strings.Contains(rendered, "## Get full context") {
		t.Errorf("the prompt does not send the seat to the page:\n%s", rendered)
	}
}

// A WIKI CHANGE IS NOT A REQUEST, and the prompt has to say so — a wiki
// whose comments are acknowledgements is a wiki whose comments nobody reads.
func TestTheSpacePromptTellsTheLeadSilenceIsOrdinary(t *testing.T) {
	t.Parallel()
	got := route(t, parser(t, nil), pageEvent("page_updated", "ENG", "<p>x</p>", acctWriter))
	rendered := (confluence.Prompt{}).Build(got[0].Inbound, nil)
	if !strings.Contains(rendered, "Staying silent is the ordinary outcome") {
		t.Fatalf("the prompt frames a page edit as a request:\n%s", rendered)
	}
	// A MENTION IS the exception, and gets the evaluation framing.
	mention := route(t, parser(t, nil), pageEvent("page_updated", "ENG",
		`<p><ri:user ri:account-id="`+acctSWE+`"/> thoughts?</p>`, acctWriter))
	if got := (confluence.Prompt{}).Build(mention[0].Inbound, nil); !strings.Contains(got, "mentioned") {
		t.Errorf("a mention was framed as space activity:\n%s", got)
	}
}

// THE PAGE IS THE CONVERSATION, so a burst of edits coalesces into one
// trigger rather than one turn each.
func TestThePageIsTheConversation(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"page_id": "1001", "space": "ENG"}
	if got := (confluence.Prompt{}).ConversationKey(meta, ""); got != "1001" {
		t.Fatalf("conversation key = %q", got)
	}
}

// A PAGE SNAPSHOT COLLAPSES IN A DIGEST and a comment does not: five saves
// is the same page five times, where five comments are five things somebody
// said.
func TestADigestCollapsesPageSnapshots(t *testing.T) {
	t.Parallel()
	if got := (confluence.Prompt{}).DigestBody("page_updated", "the whole page"); got != "" {
		t.Errorf("a page snapshot survived a digest: %q", got)
	}
	if got := (confluence.Prompt{}).DigestBody("comment_created", "what somebody said"); got == "" {
		t.Error("a comment was collapsed")
	}
}
