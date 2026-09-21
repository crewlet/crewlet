package configapi_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
)

// The diff walker, on the shapes a config document actually takes. What it has
// to get right is which side a path exists on — an operator reading "removed"
// where a field was added is being told the opposite of what happened.

func companyFrom(t *testing.T, doc string) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func pathsOf(changes []configapi.Change) map[string]configapi.Change {
	out := make(map[string]configapi.Change, len(changes))
	for _, change := range changes {
		out[change.Path] = change
	}
	return out
}

// serverDoc is companyDoc with a LIST in it: `mcp_servers`, which is what a
// settings revision still carries and what every case here about list
// positions needs.
const serverDoc = companyDoc + `
mcp_servers:
  - {name: tracker, transport: http, url: "https://mcp.example.com"}
`

func TestADiffTellsAddedFromRemoved(t *testing.T) {
	t.Parallel()
	base := companyFrom(t, serverDoc)
	grown := companyFrom(t, serverDoc+
		"  - {name: notion, command: notion-mcp}\n")

	forward, err := configapi.Changes(base, grown)
	if err != nil {
		t.Fatal(err)
	}
	if change, present := pathsOf(forward)["mcp_servers[1]"]; !present || change.Kind != configapi.KindAdded {
		t.Errorf("adding a server reads as %+v, want added", change)
	}

	backward, err := configapi.Changes(grown, base)
	if err != nil {
		t.Fatal(err)
	}
	if change, present := pathsOf(backward)["mcp_servers[1]"]; !present || change.Kind != configapi.KindRemoved {
		t.Errorf("removing a server reads as %+v, want removed", change)
	}
}

func TestADiffReachesIntoNestedValues(t *testing.T) {
	t.Parallel()
	// The path is what makes the answer useful. "providers changed" is not
	// an answer; "providers.llm.zulu.model went sonnet to haiku" is.
	base := companyFrom(t, companyDoc)
	changed := companyFrom(t, strings.Replace(companyDoc,
		"model: claude-sonnet-5", "model: claude-haiku-4-5", 1))

	changes, err := configapi.Changes(base, changed)
	if err != nil {
		t.Fatal(err)
	}
	change, present := pathsOf(changes)["providers.llm.zulu.model"]
	if !present {
		var paths []string
		for _, c := range changes {
			paths = append(paths, c.Path)
		}
		t.Fatalf("no change at the nested path; got %v", paths)
	}
	if change.From != "claude-sonnet-5" || change.To != "claude-haiku-4-5" {
		t.Errorf("change = %+v", change)
	}
}

func TestAFieldTurnedOffReadsAsRemoved(t *testing.T) {
	t.Parallel()
	// Surprising, and correct. The diff is over the document as STORED, and
	// a false boolean carrying omitempty is not in it — so turning an
	// integration off removes the key rather than changing its value. The
	// From side still carries what it was, which is the fact an operator
	// needs; pinning it here so the wording is a decision rather than an
	// accident somebody later "fixes" by diffing structs and losing the
	// property that every path is a field the operator wrote.
	base := companyFrom(t, companyDoc)
	off := companyFrom(t, strings.Replace(companyDoc, "    enabled: true", "    enabled: false", 1))

	changes, err := configapi.Changes(base, off)
	if err != nil {
		t.Fatal(err)
	}
	change, present := pathsOf(changes)["integrations.gitlab.enabled"]
	if !present {
		t.Fatal("turning the integration off produced no change at all")
	}
	if change.Kind != configapi.KindRemoved || change.From != true {
		t.Errorf("change = %+v, want it removed with its previous value", change)
	}
}

func TestAnUnchangedDocumentDiffsToNothing(t *testing.T) {
	t.Parallel()
	// The most common comparison an operator makes — "did anything change"
	// — and the one a line diff answers wrongly, because marshalling a Go
	// map reorders keys.
	base := companyFrom(t, companyDoc)
	changes, err := configapi.Changes(base, companyFrom(t, companyDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("two identical documents differ in %d places: %+v", len(changes), changes)
	}
}

func TestASubtreeIsAddedWholeRatherThanLeafByLeaf(t *testing.T) {
	t.Parallel()
	// A first import against nothing. Every top-level section is reported
	// as ONE addition carrying its whole value, not as a change per leaf —
	// which is the difference between a readable "providers: added" and
	// several hundred lines saying the same thing. It also has to work
	// rather than panic on the nil side, because it is the diff an operator
	// runs first.
	changes, err := configapi.Changes(nil, companyFrom(t, companyDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("a diff against nothing found no changes")
	}
	byPath := pathsOf(changes)
	for _, section := range []string{"name", "providers", "integrations"} {
		change, present := byPath[section]
		if !present {
			t.Errorf("no change for the %q section", section)
			continue
		}
		if change.Kind != configapi.KindAdded {
			t.Errorf("%s reads as %q, want added", section, change.Kind)
		}
	}
	// One entry per section, not one per leaf.
	if _, present := byPath["providers.llm.zulu.model"]; present {
		t.Error("the addition was expanded leaf by leaf, so a first import " +
			"is several hundred lines saying one thing")
	}
}

// wideCompanyDoc is a document whose every provider names one model, so two
// of them differ in MaxChanges+10 LEAVES — which is what a cap is for. An
// addition is reported as a whole subtree and needs none.
func wideCompanyDoc(model string) string {
	doc := strings.Builder{}
	doc.WriteString("name: Acme\nproviders:\n  llm:\n")
	for i := range configapi.MaxChanges + 10 {
		fmt.Fprintf(&doc, "    p%04d: {type: anthropic, model: %s, api_keys: [\"${K}\"]}\n", i, model)
	}
	doc.WriteString("mcp_servers:\n  - {name: tracker, command: tracker-mcp}\n")
	return doc.String()
}

// THE COMPARISON IS COMPLETE AND THE CAP IS THE CALLER'S. Where a diff has to
// be cut is a property of what is rendering it — a response body has a size
// budget and a terminal does not — so a bound taken in here is one the CLI
// could not lift, and it bought no work either: the walk builds every change
// before it can sort them.
func TestADiffReportsEveryChangeItFound(t *testing.T) {
	t.Parallel()
	changes, err := configapi.Changes(companyFrom(t, wideCompanyDoc("one")),
		companyFrom(t, wideCompanyDoc("two")))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != configapi.MaxChanges+10 {
		t.Fatalf("%d changes, want every one of the %d leaves that moved",
			len(changes), configapi.MaxChanges+10)
	}
	// NOTHING IN THE LISTING IS A MARKER. The cut used to append a pathless
	// entry whose To was a sentence, so every renderer had to know one of
	// its changes was not a change — the dashboard did not, and drew it as
	// a blank path turning undefined into prose.
	for _, c := range changes {
		if c.Path == "" {
			t.Fatalf("a change with no path is in the listing: %+v", c)
		}
	}
}

// THE ANSWER IS CUT AND SAYS SO, because this is the side with a size budget.
// Truncating is fine; truncating SILENTLY is not — a short diff reads as
// "that is all that changed".
func TestTheDiffAnswerIsCappedAndReportsTheTotal(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	first := s.seed(t, wideCompanyDoc("one"), nil)
	second := s.seed(t, wideCompanyDoc("two"), nil)

	body, err := s.service().Diff(t.Context(), second, first)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	changes, _ := body["changes"].([]configapi.Change)
	if len(changes) != configapi.MaxChanges {
		t.Fatalf("the answer carries %d changes, want the cap of %d",
			len(changes), configapi.MaxChanges)
	}
	if total, _ := body["changes_total"].(int); total != configapi.MaxChanges+10 {
		t.Errorf("changes_total is %v, want the %d that actually moved",
			body["changes_total"], configapi.MaxChanges+10)
	}
}

// AN EMPTY LISTING IS A LIST, like every other listing this surface answers.
// `null` is a shape a typed client guards for a second time to learn that
// nothing changed.
func TestADiffOfIdenticalRevisionsAnswersAnEmptyList(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	first := s.seed(t, companyDoc, nil)
	second := s.seed(t, companyDoc, nil)

	body, err := s.service().Diff(t.Context(), second, first)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	changes, ok := body["changes"].([]configapi.Change)
	if !ok || changes == nil {
		t.Fatalf("changes is %#v, want an empty list", body["changes"])
	}
	if len(changes) != 0 {
		t.Errorf("%d changes between two copies of one document", len(changes))
	}
	if total, _ := body["changes_total"].(int); total != 0 {
		t.Errorf("changes_total is %v, want 0", body["changes_total"])
	}
}

func TestAChangeInsideAListElementIsFound(t *testing.T) {
	t.Parallel()
	// Lists correspond by POSITION, and the position itself is not the
	// answer: what an operator asks is which FIELD of which seat changed.
	// Reporting the whole element as changed would make a one-character
	// edit to a handle read as a seat replaced.
	base := companyFrom(t, serverDoc)
	renamed := companyFrom(t, strings.Replace(serverDoc, "name: tracker", "name: issues", 1))

	changes, err := configapi.Changes(base, renamed)
	if err != nil {
		t.Fatal(err)
	}
	change, present := pathsOf(changes)["mcp_servers[0].name"]
	if !present {
		var paths []string
		for _, c := range changes {
			paths = append(paths, c.Path)
		}
		t.Fatalf("the change inside the list element was not found; got %v", paths)
	}
	if change.Kind != configapi.KindChanged || change.From != "tracker" || change.To != "issues" {
		t.Errorf("change = %+v", change)
	}
}

// A ROTATED CREDENTIAL IS A CHANGE, AND NEITHER VALUE IS IN IT.
//
// The diff used to compare two REDACTED documents, where every literal
// credential is the same marker on both sides, so a rotation diffed to
// nothing: the one change an operator opens a diff to confirm. Both halves are
// asserted, because reporting the change by comparing raw values and then
// printing them passes the first half and leaks.
func TestARotatedCredentialIsReportedWithoutEitherValue(t *testing.T) {
	t.Parallel()
	base := companyFrom(t, companyDoc)
	rotated := companyFrom(t, strings.Replace(companyDoc, `"sk-literal"`, `"sk-rotated-literal"`, 1))

	changes, err := configapi.Changes(base, rotated)
	if err != nil {
		t.Fatal(err)
	}
	change, present := pathsOf(changes)["providers.llm.zulu.api_keys[0]"]
	if !present {
		t.Fatalf("a rotated credential diffed to nothing: %+v", changes)
	}
	if change.Kind != configapi.KindChanged || change.From != config.Redacted || change.To != config.Redacted {
		t.Errorf("change = %+v, want a change from the mask to the mask", change)
	}
	assertNoValue(t, changes, "sk-literal", "sk-rotated-literal")
}

// AN ADDED OR REMOVED SUBTREE CARRIES ITS VALUE REDACTED.
//
// An addition is reported as the whole subtree, so a server added with its own
// credentials would otherwise print them in full. It is appended at the END of
// the list, so position alone makes it an addition or a removal.
func TestAnAddedOrRemovedSubtreeIsRedacted(t *testing.T) {
	t.Parallel()
	const literal = "per-server-literal-token"
	withServer := serverDoc + `  - name: notion
    command: notion-mcp
    env: {TOKEN: ` + literal + `}
`
	base := companyFrom(t, serverDoc)
	grown := companyFrom(t, withServer)

	for name, pair := range map[string][2]*config.Company{
		"added": {base, grown}, "removed": {grown, base},
	} {
		changes, err := configapi.Changes(pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if _, present := pathsOf(changes)["mcp_servers[1]"]; !present {
			t.Errorf("%s: the server is not reported: %+v", name, changes)
		}
		assertNoValue(t, changes, literal)
	}
}

// assertNoValue fails when any rendered change carries one of the values.
func assertNoValue(t *testing.T, changes []configapi.Change, values ...string) {
	t.Helper()
	rendered := fmt.Sprintf("%+v", changes)
	for _, value := range values {
		if strings.Contains(rendered, value) {
			t.Errorf("the diff carries the credential %q: %s", value, rendered)
		}
	}
}
