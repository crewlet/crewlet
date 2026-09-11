package atlassian_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
)

// verbatim reads a config value without resolving it.
func verbatim(v string) string { return v }

// ONE ACCOUNT, ONE ANSWER — AND BOTH PRODUCTS HAVE TO GIVE IT.
//
// An agent gets ONE Atlassian service account; Jira and Confluence each
// authenticate as it. "Does this seat have an Atlassian credential" was
// answered in three packages by three key lists that had drifted, and the
// reported failure is the exact shape that produces:
// `mcp_env.atlassian.JIRA_API_TOKEN` — the spelling [PlanFor]'s own note tells
// operators to write — read as ready to Jira and as no credential at all to
// Confluence, on the same seat, in the same block, for ever.
func TestBothProductsReadOneSharedAtlassianCredential(t *testing.T) {
	t.Parallel()
	env := map[string]map[string]string{"atlassian": {
		"JIRA_API_TOKEN": "cloud-token", "JIRA_USERNAME": "agent@acme.example",
	}}
	for _, product := range []atlassian.Product{
		atlassian.ProductJira, atlassian.ProductConfluence, atlassian.ProductAny,
	} {
		cred, where := atlassian.CredentialAt(product, env, verbatim)
		if !cred.Held() {
			t.Errorf("%q found no credential in the shared block; the other "+
				"product reads the same account from the same key", product)
			continue
		}
		if cred.Token != "cloud-token" {
			t.Errorf("%q token = %q", product, cred.Token)
		}
		// AND THE ADDRESS, which is the account's rather than a product's:
		// Cloud authenticates base64(email:token) and refuses a bearer, so a
		// seat holding only the token authenticates as nobody.
		if cred.Email != "agent@acme.example" {
			t.Errorf("%q email = %q, want the account's address", product, cred.Email)
		}
		if where != "atlassian.JIRA_API_TOKEN" {
			t.Errorf("%q found it at %q", product, where)
		}
	}
}

// A DATA CENTER PAT DOES NOT CROSS PRODUCTS, which is why the fallback above
// is limited to the `*_API_TOKEN` spellings.
//
// On Cloud one API token belongs to the ACCOUNT and works against both
// products. A `*_PERSONAL_TOKEN` is a Data Center PAT, issued by one product
// and refused by the other — so reading a Confluence PAT as a Jira credential
// would hand the tracker something it will be 401ed for, and report the seat
// ready. The spelling already carries the distinction.
func TestAPersonalTokenIsNotBorrowedByTheOtherProduct(t *testing.T) {
	t.Parallel()
	env := map[string]map[string]string{"atlassian": {
		"CONFLUENCE_PERSONAL_TOKEN": "dc-pat",
	}}
	if cred := atlassian.CredentialFor(atlassian.ProductConfluence, env, verbatim); !cred.Held() {
		t.Fatal("Confluence cannot read its own personal token")
	}
	if cred := atlassian.CredentialFor(atlassian.ProductJira, env, verbatim); cred.Held() {
		t.Errorf("Jira borrowed a Confluence Data Center PAT (%q); it is issued "+
			"by one product and refused by the other", cred.Token)
	}
}

// A PRODUCT-SPECIFIC BLOCK BEATS THE SHARED ONE.
//
// `mcp_env.jira` and `mcp_env.confluence` exist only where an operator chose a
// product-specific server, so a specific declaration wins. Trying the shared
// block first over a merged key list would let a split Data Center pair
// authenticate Jira with its Confluence token.
func TestAProductBlockWinsOverTheSharedOne(t *testing.T) {
	t.Parallel()
	env := map[string]map[string]string{
		"atlassian": {"ATLASSIAN_API_TOKEN": "shared"},
		"jira":      {"JIRA_PERSONAL_TOKEN": "jira-dc"},
	}
	if got := atlassian.CredentialFor(atlassian.ProductJira, env, verbatim).Token; got != "jira-dc" {
		t.Errorf("Jira read %q, want its own block's token", got)
	}
	// AND CONFLUENCE, which has no block of its own, still reads the shared
	// one rather than being shadowed by a block that is not for it.
	if got := atlassian.CredentialFor(atlassian.ProductConfluence, env, verbatim).Token; got != "shared" {
		t.Errorf("Confluence read %q, want the shared block's token", got)
	}
}

// AN EMPTY OR WRONG-PRODUCT BLOCK DOES NOT SHADOW A POPULATED ONE, because a
// block is selected by what it HOLDS rather than by where it sits in the order.
func TestABlockWithNothingReadableIsSkipped(t *testing.T) {
	t.Parallel()
	env := map[string]map[string]string{
		// A Confluence-only Data Center PAT, which Jira may not borrow.
		"atlassian": {"CONFLUENCE_PERSONAL_TOKEN": "dc-pat"},
		"jira":      {},
	}
	if cred := atlassian.CredentialFor(atlassian.ProductJira, env, verbatim); cred.Held() {
		t.Errorf("Jira read %q out of a block holding nothing for it", cred.Token)
	}
}

// A SCHEME IS STRIPPED, AND BASIC IS LEFT ALONE.
//
// `Authorization: Bearer <pat>` is a whole header carrying the credential
// behind a scheme, and stripping it lets one config shape serve both an HTTP
// MCP server and this lookup. A Basic header's payload is already email:token,
// so re-encoding it would produce a credential that authenticates as nobody.
// Confluence has never read this key and does now.
func TestABearerSchemeIsStrippedAndBasicIsNot(t *testing.T) {
	t.Parallel()
	bearer := map[string]map[string]string{"atlassian": {"Authorization": "Bearer pat-123"}}
	for _, product := range []atlassian.Product{atlassian.ProductJira, atlassian.ProductConfluence} {
		if got := atlassian.CredentialFor(product, bearer, verbatim).Token; got != "pat-123" {
			t.Errorf("%q read %q, want the token without its scheme", product, got)
		}
	}
	basic := map[string]map[string]string{"atlassian": {"Authorization": "Basic YWJjOmRlZg=="}}
	if got := atlassian.CredentialFor(atlassian.ProductJira, basic, verbatim).Token; got != "Basic YWJjOmRlZg==" {
		t.Errorf("a Basic header was rewritten to %q; its payload is already "+
			"email:token and re-encoding authenticates as nobody", got)
	}
}

// THE ADDRESS COMES FROM THE BLOCK THE TOKEN DID.
//
// A seat with a Data Center PAT under `jira` and a Cloud address under
// `atlassian` holds two credentials rather than one, and pairing them makes a
// third that authenticates as nobody.
func TestTheAddressIsReadFromTheTokensOwnBlock(t *testing.T) {
	t.Parallel()
	env := map[string]map[string]string{
		"atlassian": {"ATLASSIAN_EMAIL": "cloud@acme.example"},
		"jira":      {"JIRA_PERSONAL_TOKEN": "dc-pat"},
	}
	cred := atlassian.CredentialFor(atlassian.ProductJira, env, verbatim)
	if cred.Token != "dc-pat" {
		t.Fatalf("token = %q", cred.Token)
	}
	if cred.Email != "" {
		t.Errorf("email = %q, taken from a different block than the token: an "+
			"empty address selects the bearer scheme, which is what a Data "+
			"Center PAT needs", cred.Email)
	}
}
