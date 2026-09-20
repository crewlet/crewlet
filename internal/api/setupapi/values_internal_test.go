package setupapi

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// AN EMPTY CREDENTIAL IS NEVER A REQUEST, whether or not the field is
// required.
//
// An optional secret that is already set is a WORKING secret. An empty
// submission seals "" over it, and every route that resolves it afterwards
// reads a value that is present and useless — which is the one state the
// three-valued resolution cannot report, because something IS there. There is
// no way to clear a credential on this route in any case: its own answer to
// "leave it alone" is to omit the field.
func TestAnEmptyOptionalCredentialIsRefused(t *testing.T) {
	t.Parallel()
	reqs := []setup.Requirement{
		{Field: "webhook_secret", Kind: setup.KindSecret, Required: false},
	}
	err := refuseEmpty(map[string]string{"webhook_secret": "  "}, reqs)
	if err == nil {
		t.Fatal("an empty optional credential was accepted, and would seal \"\" over a working one")
	}
	if !strings.Contains(err.Error(), "webhook_secret") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}

// AND AN EMPTY NON-CREDENTIAL IS STILL FINE, or the guard would refuse every
// form that legitimately leaves an optional plain field blank.
func TestAnEmptyOptionalPlainFieldIsAccepted(t *testing.T) {
	t.Parallel()
	reqs := []setup.Requirement{{Field: "team", Kind: setup.KindText, Required: false}}
	if err := refuseEmpty(map[string]string{"team": ""}, reqs); err != nil {
		t.Errorf("an optional plain field was refused for being blank: %v", err)
	}
}

// A FIELD NAMED TWICE IN generate IS TOLD SO, rather than told it was also
// supplied. Minting writes into the map the collision check reads, so the
// second mention met the first mention's own output.
func TestAFieldNamedTwiceForMintingSaysSo(t *testing.T) {
	t.Parallel()
	reqs := []setup.Requirement{
		{Field: "webhook_token", Kind: setup.KindSecret, Mintable: true},
	}
	err := mintInto(map[string]string{}, reqs, []string{"webhook_token", "webhook_token"})
	if err == nil {
		t.Fatal("a field named twice was accepted")
	}
	if strings.Contains(err.Error(), "supplied") {
		t.Errorf("the refusal claims the caller supplied a value it never sent: %v", err)
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("the refusal does not say what is actually wrong: %v", err)
	}
}

// AND A GENUINE COLLISION STILL SAYS SO, so the fix above did not simply
// delete the check it was confusing.
func TestAFieldBothSuppliedAndGeneratedSaysSo(t *testing.T) {
	t.Parallel()
	reqs := []setup.Requirement{
		{Field: "webhook_token", Kind: setup.KindSecret, Mintable: true},
	}
	err := mintInto(
		map[string]string{"webhook_token": "mine"}, reqs, []string{"webhook_token"})
	if err == nil || !strings.Contains(err.Error(), "supplied") {
		t.Errorf("err = %v, want it to name the supplied-and-generated collision", err)
	}
}

// THE CALLER'S OWN WORDS REACH THE HISTORY WHOLE.
//
// This was cut at 120 bytes, and it was the only cut on this column: `PUT
// /config` writes the same field through configapi and bounds it nowhere, so
// the same sentence survived whole from one writer and was severed from the
// other. The revision history is what an operator reads at 3am to find the
// change that broke something, and the clause that says WHY is at the end.
func TestTheAuditSummaryCarriesTheCallersWordsWhole(t *testing.T) {
	t.Parallel()
	// Multi-byte throughout, because the cut this replaced went through
	// textcut precisely so it would not leave invalid UTF-8 — so a
	// replacement that quietly reinstated a byte slice would still have to
	// fail this.
	words := strings.Repeat("é", 200) +
		" because the workspace was migrated and the old app was deleted"
	got := auditSummary(integration.KindGitHub, words)
	if !utf8.ValidString(got) {
		t.Fatalf("the summary is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, words) {
		t.Errorf("the summary does not end in what the caller wrote — it is "+
			"%d bytes against the %d supplied", len(got), len(words))
	}

	// NEWLINES STILL GO. Folding whitespace earns its place where the
	// length cut did not: a smuggled newline breaks a line-structured
	// render, which is a fact about the shape and not about the size.
	folded := auditSummary(integration.KindGitHub, "rotated\nthe\ttoken")
	if strings.ContainsAny(folded, "\n\t") {
		t.Errorf("the summary carries raw whitespace: %q", folded)
	}
}
