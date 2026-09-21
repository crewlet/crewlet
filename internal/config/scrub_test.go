package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/redact"
)

// ERASING PERSONAL DATA FROM A STORED REVISION.
//
// The failure this guards is asymmetric, which is why it has several cases.
// Scrubbing too little leaves somebody's address in an append-only table on
// every node and in every backup — the defect the command exists for, and one
// an operator would believe was fixed. Scrubbing too much destroys
// configuration irreversibly. Both are permanent.

// EVERY SEAT IS REACHED, AT EVERY DEPTH.
//
// Most of a company's people are not in the top-level `roles:` list, so a
// walk that stopped there would report success over an archive it had barely
// touched.
func TestTheScrubReachesASeatAtEveryDepth(t *testing.T) {
	t.Parallel()

	document := []byte(`{
	  "name": "Acme",
	  "roles": [{"name": "CEO", "email": "ada@example.com"}],
	  "units": [{
	    "name": "Eng",
	    "roles": [{"name": "Dev", "email": "dev@example.com",
	               "contact": {"slack_user_id": "U0FOUNDER",
	                           "github_login": "devlogin"}}],
	    "children": [{
	      "name": "Core",
	      "roles": [{"name": "SRE", "email": "sre@example.com"}]
	    }]
	  }]
	}`)

	out, n, err := config.ScrubPersonalData(document)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	// THREE EMAILS AND TWO ACCOUNT IDS.
	if n != 5 {
		t.Errorf("scrubbed %d fields, want 5", n)
	}
	for _, gone := range []string{
		"ada@example.com", "dev@example.com", "sre@example.com",
		"U0FOUNDER", "devlogin",
	} {
		if strings.Contains(string(out), gone) {
			t.Errorf("%q survived the scrub, so it is still in this node's "+
				"append-only revision table and in every backup of it", gone)
		}
	}
	if !strings.Contains(string(out), redact.ScrubMask) {
		t.Errorf("nothing was tombstoned: %s", out)
	}
}

// THE SETTINGS SURVIVE, AND SO DOES EVERY FIELD THIS BUILD DOES NOT KNOW.
//
// # Why the unknown field is the case that matters
//
// A revision may have been written by a LATER build. Decoding into
// [config.Company] and re-encoding would silently drop everything this build
// has no field for, turning a privacy operation into data loss — which is the
// one outcome worse than the problem it was run to fix, and the reason the
// walk is over the generic document.
func TestTheScrubKeepsEverythingThatIsNotPersonal(t *testing.T) {
	t.Parallel()

	document := []byte(`{
	  "name": "Acme",
	  "mission": "ship it",
	  "token_budget": 1000,
	  "a_field_from_a_later_build": {"nested": ["values", 7]},
	  "roles": [{"name": "CEO", "handle": "ceo", "email": "ada@example.com",
	             "llm": "zulu", "goal": "lead",
	             "a_seat_field_from_a_later_build": "kept"}]
	}`)

	out, n, err := config.ScrubPersonalData(document)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if n != 1 {
		t.Fatalf("scrubbed %d fields, want the one email", n)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode the scrubbed document: %v", err)
	}
	if got["mission"] != "ship it" || got["token_budget"] != float64(1000) {
		t.Errorf("a setting was lost: %+v", got)
	}
	if _, kept := got["a_field_from_a_later_build"]; !kept {
		t.Error("a top-level field this build does not know was dropped — a " +
			"privacy operation that loses a later build's configuration is " +
			"worse than the archive it was run to clear")
	}
	seat := got["roles"].([]any)[0].(map[string]any)
	if seat["handle"] != "ceo" || seat["llm"] != "zulu" || seat["goal"] != "lead" {
		t.Errorf("a seat setting was lost: %+v", seat)
	}
	if seat["a_seat_field_from_a_later_build"] != "kept" {
		t.Error("a seat field this build does not know was dropped")
	}
	if seat["email"] != redact.ScrubMask {
		t.Errorf("email = %v, want the tombstone", seat["email"])
	}
}

// THE WHOLE CONTACT BLOCK GOES, KEY BY KEY.
//
// Every value in it is an account id identifying a person on some external
// surface — that is what the block IS — so the walk takes whatever keys the
// document carries rather than the six this build declares. A later build's
// seventh surface is reached by a walk and would not be by a list.
func TestTheScrubTakesTheWholeContactBlock(t *testing.T) {
	t.Parallel()

	out, n, err := config.ScrubPersonalData([]byte(`{
	  "roles": [{"name": "Sarah", "contact": {
	      "slack_user_id": "U0FOUNDER",
	      "a_surface_from_a_later_build": "sarah@later"}}]
	}`))
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if n != 2 {
		t.Errorf("scrubbed %d contact fields, want both", n)
	}
	if strings.Contains(string(out), "sarah@later") {
		t.Error("an account id under a key this build does not declare " +
			"survived, which is exactly the field a hand-written list of " +
			"the six surfaces would have missed")
	}
}

// RUNNING IT TWICE CHANGES NOTHING AND REPORTS NOTHING.
//
// An operator works through a list of revisions and re-runs. A second pass
// that counted the tombstones it wrote would report an erasure that did not
// happen, and — worse — would rewrite a row that needed no write, stamping a
// scrub time over the one that recorded the real erasure.
func TestTheScrubIsIdempotent(t *testing.T) {
	t.Parallel()

	first, n, err := config.ScrubPersonalData(
		[]byte(`{"roles": [{"name": "CEO", "email": "ada@example.com"}]}`))
	if err != nil || n != 1 {
		t.Fatalf("first pass: %d fields, %v", n, err)
	}
	second, n, err := config.ScrubPersonalData(first)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 0 {
		t.Errorf("the second pass reported %d fields, want none", n)
	}
	if string(second) != string(first) {
		t.Errorf("the second pass rewrote the document:\n%s\n%s", first, second)
	}
}

// A DOCUMENT WITH NOTHING TO SCRUB IS RETURNED BYTE FOR BYTE.
//
// Re-encoding it would reorder every object's keys, so `crewlet config diff`
// across the run would show the whole file changing to say that nothing did —
// which is precisely the reading the scrub's own stamp exists to prevent.
func TestACleanDocumentIsNotRewritten(t *testing.T) {
	t.Parallel()

	in := []byte(`{"zeta":1,"alpha":2,"roles":[{"name":"CEO","handle":"ceo"}]}`)
	out, n, err := config.ScrubPersonalData(in)
	if err != nil || n != 0 {
		t.Fatalf("scrub: %d fields, %v", n, err)
	}
	if string(out) != string(in) {
		t.Errorf("a clean document was rewritten:\nin:  %s\nout: %s", in, out)
	}
}

// AN EMPTY VALUE IS NOT A SCRUBBED ONE.
//
// A seat that declares no email has nothing to erase, and counting it would
// report an erasure over a company that never named anybody — which is the
// number an operator uses to decide whether the problem existed at all.
func TestAnAbsentFieldIsNotCounted(t *testing.T) {
	t.Parallel()

	_, n, err := config.ScrubPersonalData([]byte(
		`{"roles": [{"name": "CEO", "email": ""}, {"name": "CTO"}]}`))
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if n != 0 {
		t.Errorf("scrubbed %d fields of a company that names nobody", n)
	}
}

// AND A DOCUMENT THIS CANNOT READ IS REFUSED RATHER THAN EMPTIED.
func TestAnUnreadableDocumentIsRefused(t *testing.T) {
	t.Parallel()

	if _, _, err := config.ScrubPersonalData([]byte("not json")); err == nil {
		t.Fatal("a document that is not a company decoded cleanly, so a " +
			"payload this build cannot read would be stored back as one it can")
	}
}
