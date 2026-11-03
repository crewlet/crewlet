package setupapi

import (
	"strings"
	"testing"

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

// THE CALLER'S WORDS ARE STORED WHOLE, on one line. They were cut to 120
// bytes for a row the renderer already elides, so a connect's note was the
// one summary in the history whose end nobody could read.
func TestTheAuditSummaryCarriesTheCallersWordsWhole(t *testing.T) {
	t.Parallel()
	words := strings.Repeat("é", 200) + " rotated after the incident"
	got := auditSummary(integration.KindGitHub, "  "+words+"\nsecond line ")
	if got != "connect github: "+words+" second line" {
		t.Errorf("auditSummary = %q, want the words whole on one line", got)
	}
	if got := auditSummary(integration.KindGitHub, " \n "); got != "connect github" {
		t.Errorf("an empty note = %q, want the generated sentence alone", got)
	}
}

// A FORM SAVES BEFORE A FIELD THAT WAITS ON ITS MINTED VALUE IS FILLED. A
// vendor computes such a field's Required from whether the minted value is
// held (the vendor's setup needs the key's public half before it can name
// what the field asks for), so the submission that mints it names the other
// fields alone: the mint is an ES256 key, the dependent field stays absent,
// and every guard the save path runs before the write takes the submission.
func TestASaveMintsAKeyBeforeAFieldThatWaitsOnIt(t *testing.T) {
	t.Parallel()
	reqs := []setup.Requirement{
		{Field: "account", Kind: setup.KindID, Required: true, ConfigPath: "integrations.x.account"},
		{Field: "signing_key", Kind: setup.KindSecret, Mintable: true, Shape: setup.ShapeES256Key,
			ConfigPath: "integrations.x.signing_key"},
		{Field: "trust_link", Kind: setup.KindID, ConfigPath: "integrations.x.trust_link"},
	}
	values := map[string]string{"account": "acct-1"}
	if err := mintInto(values, reqs, []string{"signing_key"}); err != nil {
		t.Fatalf("mintInto: %v", err)
	}
	if _, err := setup.ES256KeyID(values["signing_key"]); err != nil {
		t.Fatalf("the minted value is not an ES256 key: %v", err)
	}
	if _, sent := values["trust_link"]; sent {
		t.Fatal("the mint filled the field that waits on it")
	}
	if err := refuseUngated(values, reqs); err != nil {
		t.Errorf("refuseUngated: %v", err)
	}
	if err := refuseEmpty(values, reqs); err != nil {
		t.Errorf("refuseEmpty: %v", err)
	}
	for _, r := range reqs {
		if value, sent := values[r.Field]; sent {
			if refused := r.Refusal(strings.TrimSpace(value)); refused != nil {
				t.Errorf("%s refuses the submission: %s", r.Field, refused.Problem)
			}
		}
	}
}
