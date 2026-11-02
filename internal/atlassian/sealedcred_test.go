package atlassian_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/org"
)

// THE ENGINE-OWNED NAMES ANSWER FIRST (D4). A seat's token and address are
// sealed under names the engine derives from its handle; mcp_env is read only
// for a document written before per-seat access, and a sealed value wins over
// whatever such a document still names.
func TestASeatsSealedCredentialIsReadBeforeAnythingItsDocumentNames(t *testing.T) {
	t.Parallel()
	if atlassian.SinkToken("sre") != "ATLASSIAN_TOKEN_SRE" || atlassian.SinkEmail("sre") != "ATLASSIAN_EMAIL_SRE" {
		t.Fatalf("sink names = %q, %q", atlassian.SinkToken("sre"), atlassian.SinkEmail("sre"))
	}
	sealed := map[string]string{
		"${ATLASSIAN_TOKEN_SRE}": "sealed-token",
		"${ATLASSIAN_EMAIL_SRE}": "sre@serviceaccount.atlassian.com",
		"${LEGACY_TOKEN}":        "legacy-token",
	}
	value := func(v string) string { return sealed[v] }
	seat := &org.Role{Name: "SRE", MCPEnv: map[string]map[string]string{
		"atlassian": {"ATLASSIAN_API_TOKEN": "${LEGACY_TOKEN}"},
	}}
	cred := atlassian.CredentialOf(atlassian.ProductJira, seat, value)
	if cred.Token != "sealed-token" || cred.Email != "sre@serviceaccount.atlassian.com" {
		t.Fatalf("credential = %+v, want the sealed one", cred)
	}
	delete(sealed, "${ATLASSIAN_TOKEN_SRE}")
	if cred := atlassian.CredentialOf(atlassian.ProductConfluence, seat, value); cred.Token != "legacy-token" {
		t.Fatalf("with nothing sealed the credential = %+v, want the document's", cred)
	}
	// A VALUE FUNCTION THAT DOES NOT RESOLVE hands the reference back, which
	// is not a sealed credential.
	verbatimSeat := atlassian.CredentialOf(atlassian.ProductJira, seat, func(v string) string { return v })
	if verbatimSeat.Token != "${LEGACY_TOKEN}" {
		t.Fatalf("read verbatim, the credential = %+v, want the document's reference", verbatimSeat)
	}
	human := &org.Role{Name: "Ana", Kind: org.KindHuman}
	if atlassian.CredentialOf(atlassian.ProductJira, human, value).Held() {
		t.Fatal("a person was read as holding a seat credential")
	}
}
