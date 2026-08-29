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

func TestADiffTellsAddedFromRemoved(t *testing.T) {
	t.Parallel()
	base := companyFrom(t, companyDoc)
	grown := companyFrom(t, strings.Replace(companyDoc,
		"  - name: CTO\n    handle: cto\n    llm: zulu\n",
		"  - name: CTO\n    handle: cto\n    llm: zulu\n  - name: Designer\n    handle: designer\n    llm: zulu\n", 1))

	forward, err := configapi.Changes(base, grown)
	if err != nil {
		t.Fatal(err)
	}
	if change, present := pathsOf(forward)["roles[2]"]; !present || change.Kind != configapi.KindAdded {
		t.Errorf("adding a seat reads as %+v, want added", change)
	}

	backward, err := configapi.Changes(grown, base)
	if err != nil {
		t.Fatal(err)
	}
	if change, present := pathsOf(backward)["roles[2]"]; !present || change.Kind != configapi.KindRemoved {
		t.Errorf("removing a seat reads as %+v, want removed", change)
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
	for _, section := range []string{"name", "providers", "roles"} {
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

func TestADiffIsCappedAndSaysSo(t *testing.T) {
	t.Parallel()
	// A wholesale rewrite produces one change per leaf — thousands, none of
	// which a person reads. Truncating is fine; truncating SILENTLY is not,
	// because a short diff reads as "that is all that changed".
	// Two documents that differ in every one of many LEAVES, which is what
	// the cap is for — an addition is reported as a whole subtree and needs
	// no cap.
	build := func(model string) *config.Company {
		doc := strings.Builder{}
		doc.WriteString("name: Acme\nproviders:\n  llm:\n")
		for i := range configapi.MaxChanges + 10 {
			fmt.Fprintf(&doc, "    p%04d: {type: anthropic, model: %s, api_keys: [\"${K}\"]}\n", i, model)
		}
		doc.WriteString("roles:\n  - {name: CEO, handle: ceo}\n")
		return companyFrom(t, doc.String())
	}

	changes, err := configapi.Changes(build("one"), build("two"))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != configapi.MaxChanges+1 {
		t.Fatalf("%d changes, want the cap plus the notice", len(changes))
	}
	last := changes[len(changes)-1]
	note, _ := last.To.(string)
	if !strings.Contains(note, "further changes not listed") {
		t.Errorf("the truncation is silent: %+v", last)
	}
}

func TestAChangeInsideAListElementIsFound(t *testing.T) {
	t.Parallel()
	// Lists correspond by POSITION, and the position itself is not the
	// answer: what an operator asks is which FIELD of which seat changed.
	// Reporting the whole element as changed would make a one-character
	// edit to a handle read as a seat replaced.
	base := companyFrom(t, companyDoc)
	renamed := companyFrom(t, strings.Replace(companyDoc, "handle: ceo", "handle: chief", 1))

	changes, err := configapi.Changes(base, renamed)
	if err != nil {
		t.Fatal(err)
	}
	change, present := pathsOf(changes)["roles[0].handle"]
	if !present {
		var paths []string
		for _, c := range changes {
			paths = append(paths, c.Path)
		}
		t.Fatalf("the change inside the list element was not found; got %v", paths)
	}
	if change.Kind != configapi.KindChanged || change.From != "ceo" || change.To != "chief" {
		t.Errorf("change = %+v", change)
	}
}
