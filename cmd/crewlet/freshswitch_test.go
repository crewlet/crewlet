package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// EACH COMMAND HANDS ITS PASS ITS OWN FLAG, SPELLED AS IT DECLARES IT.
//
// A pass refuses its recreate switch over a secret the company document holds
// as a literal, and the refusal names the switch, because dropping it is one
// of the two ways on. A pass spells nothing itself: it could spell only one
// surface's switch, and the setup route's caller, who sends a body field and
// has no flag, would be told to re-run with one. So each command is what
// spells it, and a command handing the wrong spelling, or none, would name
// another surface's switch or a blank.
//
// Each run reaches its vendor only for the credential probe, which the fake
// below answers for every product, and is refused before anything is listed,
// minted or registered.
func TestEachRecreateFlagReachesItsPassAsDeclared(t *testing.T) {
	t.Parallel()
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a refused run wrote to its vendor: %s %s", r.Method, r.URL.Path)
			http.Error(w, "no writes", http.StatusForbidden)
			return
		}
		// EVERY PRODUCT'S IDENTITY, in one answer: Jira and Confluence
		// read an account, GitHub a login.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accountId":"acct-1","name":"bot","login":"bot",` +
			`"username":"bot","displayName":"Bot","type":"known"}`))
	}))
	t.Cleanup(vendor.Close)

	const literal = "aLiteralSecretNobodyShouldEverSeeAgain"
	for _, tc := range []struct {
		command, flag, field, doc string
	}{
		{"jira", "-recreate-webhook", "integrations.jira.webhook_secret", `
name: Nimbus
integrations:
  jira:
    url: ` + vendor.URL + `
    token: tok-org
    webhook_secret: ` + literal + `
roles:
  - name: SWE
    handle: swe
    goal: ship
`},
		{"github", "-recreate-webhooks", "integrations.github.webhook_secret", `
name: Nimbus
integrations:
  github:
    enabled: true
    url: ` + vendor.URL + `
    token: tok-org
    webhook_secret: ` + literal + `
    provisioning:
      org: acme
roles:
  - name: SWE
    handle: swe
    goal: ship
`},
		{"confluence", "-recreate-webhooks", "integrations.confluence.webhook_secret", `
name: Nimbus
integrations:
  confluence:
    url: ` + vendor.URL + `
    email: bot@example.com
    token: tok-org
    webhook_secret: ` + literal + `
roles:
  - name: SWE
    handle: swe
    goal: ship
`},
	} {
		t.Run(tc.command, func(t *testing.T) {
			t.Parallel()
			cfg := bootstrapWithKeyring(t, "k1")
			company := writeFile(t, t.TempDir(), "company.yaml", tc.doc)
			out, errs, err := cliIn(t, noEnviron, tc.command, "provision", company,
				tc.flag, "-public-url", "https://engine.example.com", "-print",
				"-config", cfg)
			if err == nil {
				t.Fatalf("%s over a literal went ahead:\n%s%s", tc.flag, out, errs)
			}
			for _, want := range []string{
				tc.field + " is a literal",
				tc.flag + " has no variable to mint a fresh one into",
				"re-run with " + tc.flag,
				"run without " + tc.flag + " to register the value it holds as it is",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if strings.Contains(err.Error(), "recreate_webhooks") {
				t.Errorf("the command's refusal names the setup route's field:\n%v", err)
			}
			if strings.Contains(err.Error()+out+errs, literal) {
				t.Errorf("the run repeats the secret:\n%v\n%s%s", err, out, errs)
			}
		})
	}
}
