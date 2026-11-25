package config_test

import (
	"strings"
	"testing"

	_ "github.com/crewlet/crewlet/internal/admob"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
)

// signInDoc is a company whose google_cloud block names the one sign-in
// client, and whose AdMob block is written as admob.
func signInDoc(shared, admob string) string {
	return `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    fast:
      model: claude-golden
integrations:
  google_cloud:
` + shared + `
  admob:
` + admob + `
roles:
  - name: Finance
    handle: finance
    llm: fast
`
}

const sharedClient = `    sign_in_client:
      client_id: "123456789012-shared.apps.googleusercontent.com"
      client_secret: ${GOOGLE_SIGN_IN_CLIENT_SECRET}`

// ONE SIGN-IN CLIENT SERVES EVERY CONSENT PRODUCT: AdMob with no oauth of its
// own validates where google_cloud names sign_in_client and signs in as it,
// and its own oauth, where written, overrides it.
//
// Mutation: require AdMob's own oauth again, or read the shared client before
// the product's own, and a case goes red.
func TestAConsentProductSignsInAsTheSharedClientUnlessItNamesItsOwn(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(signInDoc(sharedClient, "    enabled: true")))
	if err != nil {
		t.Fatalf("AdMob on the shared client was refused: %v", err)
	}
	if got := googleauth.SignInClient(c, c.Integrations.AdMob.OAuth).ID; got !=
		"123456789012-shared.apps.googleusercontent.com" {
		t.Errorf("AdMob signs in as %q, want the shared client", got)
	}
	own, err := config.ParseCompany([]byte(signInDoc(sharedClient, "    enabled: true\n"+adMobClient)))
	if err != nil {
		t.Fatalf("AdMob with its own client was refused: %v", err)
	}
	if got := googleauth.SignInClient(own, own.Integrations.AdMob.OAuth).ID; got !=
		"123456789012-abc123def456.apps.googleusercontent.com" {
		t.Errorf("AdMob with its own client signs in as %q, want its own", got)
	}
}

// THE SHARED CLIENT IS HELD TO GOOGLE'S SHAPES whether or not Google Cloud is
// on, and AdMob with neither client is refused naming its own field.
func TestTheSharedSignInClientIsHeldToItsShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, shared, admob, path string }{
		{"a client ID of another shape", "    sign_in_client:\n      client_id: \"abc\"\n      client_secret: ${S}",
			"    enabled: false", "integrations.google_cloud.sign_in_client.client_id"},
		{"no client secret", "    sign_in_client:\n      client_id: \"1-a.apps.googleusercontent.com\"",
			"    enabled: false", "integrations.google_cloud.sign_in_client.client_secret"},
		{"AdMob with no client at all", "    enabled: false", "    enabled: true", "integrations.admob.oauth"},
	} {
		_, err := config.ParseCompany([]byte(signInDoc(tc.shared, tc.admob)))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: refused with %v, want the field %s named", tc.name, err, tc.path)
		}
	}
}
