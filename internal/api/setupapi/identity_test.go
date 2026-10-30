package setupapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/secrets"
)

// A company whose one agent is provisioned into every app that grades it by
// an account rather than by an app of its own.
const identityDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {
    "atlassian": {"org_id": "${ORG_ID}", "api_key": "${ORG_KEY}"},
    "jira": {"cloud_id": "c1", "site_url": "https://acme.atlassian.net",
             "email": "${OPS_EMAIL}", "token": "${OPS_TOKEN}"},
    "gitlab": {"enabled": true, "url": "https://gitlab.com", "signing_secret": "${GL_SIGN}",
               "provisioning": {"group": "acme", "admin_token": "${GL_ADMIN}"}},
    "mattermost": {"enabled": true, "url": "https://chat.example.com", "team": "acme",
                   "provisioning": {"admin_token": "${MM_ADMIN}", "username_prefix": "agent-"}},
    "datadog": {"enabled": true, "route_to": "sre-lead", "webhook_token": "${DD_HOOK}",
                "provisioning": {"site": "datadoghq.com", "api_key": "${DD_API}", "app_key": "${DD_APP}"}}
  },
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu",
     "integrations": {"mattermost": {"enabled": false, "bot_token": "${MATTERMOST_BOT_TOKEN_SRE_LEAD}"}},
     "mcp_env": {
       "atlassian": {"JIRA_API_TOKEN": "${ATLASSIAN_TOKEN_SRE_LEAD}", "JIRA_USERNAME": "${ATLASSIAN_EMAIL_SRE_LEAD}"},
       "gitlab": {"GITLAB_TOKEN": "${GITLAB_TOKEN_SRE_LEAD}"},
       "datadog": {"DD_APP_KEY": "${DATADOG_APP_KEY_SRE_LEAD}"}
     }}
  ]
}`

// detailOf reads one agent's roster line off a tool's setup answer.
func detailOf(t *testing.T, s *surface, kind string) string {
	t.Helper()
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/"+kind, "", nil))
	rows, _ := state["seats"].([]any)
	for _, row := range rows {
		seat, _ := row.(map[string]any)
		if handle, _ := seat["handle"].(string); handle == "sre-lead" {
			detail, _ := seat["detail"].(string)
			return detail
		}
	}
	t.Fatalf("%s lists no sre-lead: %v", kind, rows)
	return ""
}

// A WORKING SEAT SAYS WHO IT IS, in the app's own words.
//
// The row said where the credential was kept, `mcp_env.datadog.DD_APP_KEY`,
// which is a fact about this company's YAML: true, the same for every agent
// bar the block name, and no help to somebody reading the app's own user list
// trying to work out which account is which colleague.
func TestAWorkingSeatNamesTheAccountItIsAtEachApp(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, identityDoc)
	// Every seat credential sealed, because the identity is shown only over
	// a seat that works: a derived name is what the engine WOULD create
	// rather than proof it did.
	for name, value := range map[string]string{
		"ATLASSIAN_TOKEN_SRE_LEAD": "atl-token",
		"ATLASSIAN_EMAIL_SRE_LEAD":     "crewlet-sre-lead@acme.invalid",
		"GITLAB_TOKEN_SRE_LEAD":    "gl-token",
		"DATADOG_APP_KEY_SRE_LEAD":   "dd-key",
		"MATTERMOST_BOT_TOKEN_SRE_LEAD":        "mm-token",
	} {
		if err := s.vault.Set(t.Context(), name, value, testAuthor, secrets.SourceAPI, pinned); err != nil {
			t.Fatal(err)
		}
	}

	for kind, want := range map[string]string{
		// RECORDED, not derived: Atlassian assigns the address when it
		// creates the account, so the pass writes it onto the seat.
		"jira": "crewlet-sre-lead@acme.invalid",
		// DERIVED BY THE SAME FUNCTION THE PASS USES, so the roster and
		// the account are one answer rather than two rules that can drift.
		"gitlab":     "crewlet-sre-lead",
		"datadog":    "crewlet-sre-lead@",
		"mattermost": "@agent-sre-lead",
	} {
		if got := detailOf(t, s, kind); !strings.HasPrefix(got, want) {
			t.Errorf("%s detail = %q, want it to name the account, starting %q", kind, got, want)
		}
	}
}

// AND A SEAT THAT IS NOT SET UP STILL SAYS WHAT TO DO.
//
// A derived name is what the engine WOULD create. Printed against a seat with
// nothing sealed it would name an account that does not exist, over a badge
// reading "not set up".
func TestASeatWithNothingSealedNamesNoAccount(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, identityDoc)

	for _, kind := range []string{"gitlab", "datadog", "mattermost", "jira"} {
		got := detailOf(t, s, kind)
		// THE SENTENCE SAYING WHAT IS WRONG, rather than an account name.
		// A derived name printed here would name an account nothing has
		// created, over a badge reading "not set up".
		if !strings.Contains(got, "yet") && !strings.Contains(got, "did not resolve") {
			t.Errorf("%s detail = %q, which reads as an account rather than a next step", kind, got)
		}
		if got == "" {
			t.Errorf("%s detail is empty, so the row says nothing at all", kind)
		}
	}
}
