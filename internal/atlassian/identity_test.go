package atlassian_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/org"
)

// The seat-credential grammar. Every case here is one both former copies had
// to agree on and did not — this file is the reason there is now one.

// pass resolves a ${VAR} the way the config resolver would, from a map.
func pass(values map[string]string) func(string) string {
	return func(in string) string {
		if v, ok := values[in]; ok {
			return v
		}
		return in
	}
}

func TestAHumanSeatHoldsNoCredential(t *testing.T) {
	t.Parallel()
	seat := &org.Role{
		Name: "Jane Founder",
		Kind: org.KindHuman,
		MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_API_TOKEN": "tok", "JIRA_USERNAME": "jane@example.com",
		}},
	}
	// A human is addressable through contact.atlassian_account_id and is
	// never probed for a tool credential. Reading one would register a
	// person's own account as a seat's identity.
	if cred := atlassian.CredentialOf(seat, pass(nil)); cred.Held() {
		t.Fatalf("a human seat resolved a credential: %+v", cred)
	}
}

func TestTheBlockThatHoldsATokenWinsOverNameOrder(t *testing.T) {
	t.Parallel()
	// THE BUG THE ENGINE'S OWN COPY HAD. `atlassian` is first in SeatEnvs,
	// so a scan that picked by position would find this seat's empty block
	// and report a working seat as having no credential at all.
	seat := &org.Role{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{
			"atlassian": {"JIRA_URL": "https://acme.atlassian.net"},
			"jira": {
				"JIRA_API_TOKEN": "real-token",
				"JIRA_USERNAME":  "swe@example.com",
			},
		},
	}
	cred := atlassian.CredentialOf(seat, pass(nil))
	if cred.Token != "real-token" || cred.Email != "swe@example.com" {
		t.Fatalf("credential = %+v, want the block that holds a token", cred)
	}
}

func TestAConfluenceOnlyBlockStillHoldsTheAtlassianIdentity(t *testing.T) {
	t.Parallel()
	// One account serves both products, so a seat that wrote only the wiki
	// spelling still has a tracker identity. The tracker's former copy did
	// not know the CONFLUENCE_ keys at all.
	seat := &org.Role{
		Name: "Tech Writer",
		MCPEnv: org.MCPEnv{"confluence": {
			"CONFLUENCE_API_TOKEN": "wiki-token",
			"CONFLUENCE_USERNAME":  "writer@example.com",
		}},
	}
	cred := atlassian.CredentialOf(seat, pass(nil))
	if cred.Token != "wiki-token" || cred.Email != "writer@example.com" {
		t.Fatalf("credential = %+v", cred)
	}
}

func TestABearerSchemeIsStrippedAndABasicHeaderIsNot(t *testing.T) {
	t.Parallel()
	bearer := &org.Role{
		Name: "Agent A",
		MCPEnv: org.MCPEnv{"atlassian": {
			"Authorization": "Bearer dc-personal-token",
		}},
	}
	if got := atlassian.CredentialOf(bearer, pass(nil)).Token; got != "dc-personal-token" {
		t.Errorf("bearer token = %q, want the value without the scheme", got)
	}

	// A Basic header's payload is ALREADY base64(email:token). Stripping
	// the word would hand back a credential that authenticates as nobody,
	// and there is no ${VAR} under it to mint into either.
	basic := &org.Role{
		Name: "Agent B",
		MCPEnv: org.MCPEnv{"atlassian": {
			"Authorization": "Basic ZW1haWw6dG9rZW4=",
		}},
	}
	if got := atlassian.CredentialOf(basic, pass(nil)).Token; got != "Basic ZW1haWw6dG9rZW4=" {
		t.Errorf("basic header = %q, want it left whole", got)
	}
}

func TestATokenWithNoAddressIsTheBearerCase(t *testing.T) {
	t.Parallel()
	// Empty Email is a SETTING, not an absence: it selects bearer
	// authentication, which is what a Data Center personal access token
	// wants. Defaulting an address here would refuse every Data Center
	// seat on the auth scheme rather than on the credential.
	seat := &org.Role{
		Name:   "Agent DC",
		MCPEnv: org.MCPEnv{"jira": {"JIRA_PERSONAL_TOKEN": "pat"}},
	}
	cred := atlassian.CredentialOf(seat, pass(nil))
	if !cred.Held() || cred.Email != "" {
		t.Fatalf("credential = %+v, want a token and no address", cred)
	}
}

func TestTheCredentialIsResolvedThroughTheReferences(t *testing.T) {
	t.Parallel()
	seat := &org.Role{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_API_TOKEN": "${TOKEN_SWE}",
			"JIRA_USERNAME":  "${EMAIL_SWE}",
		}},
	}
	cred := atlassian.CredentialOf(seat, pass(map[string]string{
		"${TOKEN_SWE}": "resolved-token",
		"${EMAIL_SWE}": "swe@example.com",
	}))
	if cred.Token != "resolved-token" || cred.Email != "swe@example.com" {
		t.Fatalf("credential = %+v", cred)
	}
}

func TestASeatWithNoAtlassianBlockHoldsNothing(t *testing.T) {
	t.Parallel()
	seat := &org.Role{
		Name:   "Agent Chat",
		MCPEnv: org.MCPEnv{"slack": {"SLACK_BOT_TOKEN": "xoxb-x"}},
	}
	if cred := atlassian.CredentialOf(seat, pass(nil)); cred.Held() {
		t.Fatalf("credential = %+v, want none", cred)
	}
	if block := atlassian.SeatBlock(seat, pass(nil)); block != nil {
		t.Fatalf("block = %v, want nil", block)
	}
}

func TestACredentialIsUsableAsAMapKey(t *testing.T) {
	t.Parallel()
	// The engine caches a resolved account id BY CREDENTIAL, which is what
	// makes a config apply free. A field that made this type
	// uncomparable would not fail to compile at the definition — it would
	// panic at the first cache write, on the boot path.
	seen := map[atlassian.Credential]string{}
	seen[atlassian.Credential{Token: "t", Email: "e"}] = "account-1"
	if got := seen[atlassian.Credential{Token: "t", Email: "e"}]; got != "account-1" {
		t.Fatalf("lookup = %q", got)
	}
	if _, ok := seen[atlassian.Credential{Token: "t"}]; ok {
		t.Fatal("the same token under a different address hit the same key")
	}
}
