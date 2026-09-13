package atlassian_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/jira"
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

// A WHOLE `Authorization` HEADER IS READ AS THE PAIR THAT REBUILDS IT.
//
// Both product clients construct their own header from (email, token): an
// empty email selects `Bearer <token>`, a present one selects
// `Basic base64(email:token)`. So what this lookup owes them is the pair, not
// the text somebody typed.
//
// The Basic case was stored whole, in the token, with no email — the exact
// shape that selects the BEARER scheme. Every seat configured that way sent
// `Authorization: Bearer Basic <payload>`, was refused by Jira and Confluence
// alike, and reported an identity failure over a credential that was perfectly
// good. Asserted here through [jira.AuthOf], which is the header the client
// actually sends, because the old case asserted the stored token and a stored
// token is not what authenticates anything.
func TestAWholeAuthorizationHeaderRebuildsTheHeaderItCameFrom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, header, wantSent, wantEmail, wantToken string
	}{{
		name: "a bearer pat keeps its scheme and gains no address",
		// Data Center: the address is genuinely absent, and its absence is
		// what selects the bearer scheme.
		header: "Bearer pat-123", wantSent: "Bearer pat-123",
		wantEmail: "", wantToken: "pat-123",
	}, {
		name: "a basic header comes apart and goes back together",
		// base64("agent@acme.example:tok-456")
		header:    "Basic YWdlbnRAYWNtZS5leGFtcGxlOnRvay00NTY=",
		wantSent:  "Basic YWdlbnRAYWNtZS5leGFtcGxlOnRvay00NTY=",
		wantEmail: "agent@acme.example", wantToken: "tok-456",
	}, {
		name: "the scheme is matched without regard to case",
		// What curl prints, and what a person copies out of it. Matched
		// case-sensitively, the word stayed in the token and the client
		// sent `Bearer bearer pat-123`.
		header: "bearer pat-123", wantSent: "Bearer pat-123",
		wantEmail: "", wantToken: "pat-123",
	}, {
		name: "an unpadded basic payload is still a pair",
		// base64 without "=" padding, which a person writing the header by
		// hand produces and which a strict decoder calls malformed.
		header:    "Basic YWdlbnRAYWNtZS5leGFtcGxlOnRvay00NTY",
		wantSent:  "Basic YWdlbnRAYWNtZS5leGFtcGxlOnRvay00NTY=",
		wantEmail: "agent@acme.example", wantToken: "tok-456",
	}, {
		name: "a password containing a colon survives the split",
		// base64("agent@acme.example:tok:with:colons") — only the FIRST
		// colon separates, which is what RFC 7617 says.
		header:    "Basic YWdlbnRAYWNtZS5leGFtcGxlOnRvazp3aXRoOmNvbG9ucw==",
		wantSent:  "Basic YWdlbnRAYWNtZS5leGFtcGxlOnRvazp3aXRoOmNvbG9ucw==",
		wantEmail: "agent@acme.example", wantToken: "tok:with:colons",
	}, {
		name: "a payload that is not a pair is left exactly as it came",
		// Not base64 at all. Taken apart on a guess it would authenticate
		// as somebody else; left alone, the refusal it earns is reported
		// against the seat that holds it.
		header: "Basic not-base64!!", wantSent: "Bearer Basic not-base64!!",
		wantEmail: "", wantToken: "Basic not-base64!!",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]map[string]string{"atlassian": {"Authorization": tc.header}}
			for _, product := range []atlassian.Product{
				atlassian.ProductJira, atlassian.ProductConfluence,
			} {
				cred := atlassian.CredentialFor(product, env, verbatim)
				if cred.Email != tc.wantEmail || cred.Token != tc.wantToken {
					t.Errorf("%s read (%q, %q), want (%q, %q)",
						product, cred.Email, cred.Token, tc.wantEmail, tc.wantToken)
				}
			}
			// THE HEADER EACH CLIENT ACTUALLY SENDS, which is the only
			// thing that authenticates and the half no case used to
			// check. Driven through the real constructors against a real
			// listener, because "the pair is right" and "the header is
			// right" are two facts and it was the join that was broken.
			pair := atlassian.CredentialFor(atlassian.ProductJira, env, verbatim)
			if got := sentByJira(t, pair); got != tc.wantSent {
				t.Errorf("jira sends %q, want %q", got, tc.wantSent)
			}
			if got := sentByConfluence(t, pair); got != tc.wantSent {
				t.Errorf("confluence sends %q, want %q", got, tc.wantSent)
			}
		})
	}
}

// authSeen serves one request and reports the Authorization header it carried.
func authSeen(t *testing.T, call func(base string)) string {
	t.Helper()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"accountId":"a","emailAddress":"a@b.c","results":[]}`)
		}))
	defer srv.Close()
	call(srv.URL)
	return seen
}

// sentByJira is the header [jira.Client] puts on the wire for this pair.
func sentByJira(t *testing.T, cred atlassian.Credential) string {
	t.Helper()
	return authSeen(t, func(base string) {
		client, err := jira.NewClient(jira.ClientOptions{
			URL: base, Email: cred.Email, Token: cred.Token,
		})
		if err != nil {
			t.Fatalf("jira.NewClient: %v", err)
		}
		//nolint:errcheck // the answer is the header the server saw
		client.Me(t.Context())
	})
}

// sentByConfluence is the same for [confluence.Client], because one account
// authenticates at both and a fix to one of them is half a fix.
func sentByConfluence(t *testing.T, cred atlassian.Credential) string {
	t.Helper()
	return authSeen(t, func(base string) {
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: base, Email: cred.Email, Token: cred.Token,
		})
		if err != nil {
			t.Fatalf("confluence.NewClient: %v", err)
		}
		//nolint:errcheck // the answer is the header the server saw
		client.Me(t.Context())
	})
}

// A BASIC HEADER'S OWN ADDRESS WINS OVER THE BLOCK'S.
//
// The payload is a PAIR. Pairing its secret with an address written elsewhere
// in the same block makes a third credential that authenticates as nobody —
// the same rule [TestTheAddressIsReadFromTheTokensOwnBlock] states one level
// up, applied inside a single value.
func TestABasicHeaderKeepsItsOwnAddress(t *testing.T) {
	t.Parallel()
	env := map[string]map[string]string{"atlassian": {
		// base64("agent@acme.example:tok-456")
		"Authorization":   "Basic YWdlbnRAYWNtZS5leGFtcGxlOnRvay00NTY=",
		"ATLASSIAN_EMAIL": "somebody-else@acme.example",
	}}
	got := atlassian.CredentialFor(atlassian.ProductJira, env, verbatim)
	if got.Email != "agent@acme.example" {
		t.Errorf("email = %q, want the address inside the header it came with", got.Email)
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
