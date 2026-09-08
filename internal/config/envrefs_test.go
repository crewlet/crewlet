package config

import (
	"slices"
	"strings"
	"testing"
)

// The reference index is what stands between an operator and a credential
// they delete without knowing who was reading it. Two things have to be true:
// every field that names a variable is reported, at a path the operator can
// find in their own document, and the two questions this file answers never
// disagree about which fields are reachable.
//
// One credential with SEVERAL READERS is the shape that matters. The fixture
// points `mcp_servers[0].env` and one seat's `mcp_env` at the same variable
// on purpose: a name-only answer says "something references this", which is
// not something anybody can act on.

const referenceDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-a-literal-key", "${ROTATED_KEY}"]
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "${GITLAB_SIGNING}"
    token: "${GITLAB_TOKEN}"
mcp_servers:
  - name: notion
    command: notion-mcp
    env:
      NOTION_TOKEN: "${NOTION_TOKEN}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      notion: {NOTION_TOKEN: "${NOTION_TOKEN}"}
    integrations:
      slack:
        bot_token: "${SLACK_BOT_TOKEN_CEO}"
        signing_secret: "${SLACK_SIGNING_CEO}"
`

func referenceCompany(t *testing.T) *Company {
	t.Helper()
	cfg, err := ParseCompany([]byte(referenceDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// TestEveryReferenceIsReportedAtItsOwnPath is the invariant the Secrets
// screen's delete confirmation rests on: a variable read from two places
// reports two paths, and each is spelled the way the operator's document
// spells it.
func TestEveryReferenceIsReportedAtItsOwnPath(t *testing.T) {
	t.Parallel()
	got := References(referenceCompany(t))
	want := []Reference{
		{Path: "integrations.gitlab.signing_secret", Name: "GITLAB_SIGNING"},
		{Path: "integrations.gitlab.token", Name: "GITLAB_TOKEN"},
		{Path: "mcp_servers[0].env.NOTION_TOKEN", Name: "NOTION_TOKEN"},
		{Path: "providers.llm.zulu.api_keys[1]", Name: "ROTATED_KEY"},
		{Path: "roles[0].integrations.slack.bot_token", Name: "SLACK_BOT_TOKEN_CEO"},
		{Path: "roles[0].integrations.slack.signing_secret", Name: "SLACK_SIGNING_CEO"},
		{Path: "roles[0].mcp_env.notion.NOTION_TOKEN", Name: "NOTION_TOKEN"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("References mismatch\n got %v\nwant %v", got, want)
	}
}

// TestALiteralIsNotAReference guards the half that would make the delete
// confirmation cry wolf: a literal credential names no variable, so removing
// a secret row cannot possibly affect it.
func TestALiteralIsNotAReference(t *testing.T) {
	t.Parallel()
	for _, ref := range References(referenceCompany(t)) {
		if strings.HasPrefix(ref.Path, "providers.llm.zulu.api_keys[0]") {
			t.Errorf("a literal api key was reported as a reference to %q", ref.Name)
		}
	}
}

// TestTheTwoAnswersNeverDisagreeAboutWhatIsReachable is why there is one
// walk. The fingerprint half decides whether a config change is a change at
// all; a second traversal that missed a field one of them reaches would fail
// silently on exactly that.
func TestTheTwoAnswersNeverDisagreeAboutWhatIsReachable(t *testing.T) {
	t.Parallel()
	cfg := referenceCompany(t)
	seen := map[string]struct{}{}
	for _, ref := range References(cfg) {
		seen[ref.Name] = struct{}{}
	}
	names := ReferencedNames(cfg)
	if len(names) != len(seen) {
		t.Fatalf("ReferencedNames = %v, but References found %d distinct names",
			names, len(seen))
	}
	for _, name := range names {
		if _, ok := seen[name]; !ok {
			t.Errorf("ReferencedNames reported %q, which References never saw", name)
		}
	}
}

// TestAnEmptyDocumentReferencesNothing pins the answer a company with no
// credentials gets: an empty list rather than a nil the caller has to guess
// the meaning of.
func TestAnEmptyDocumentReferencesNothing(t *testing.T) {
	t.Parallel()
	if got := References(&Company{Name: "Acme"}); len(got) != 0 {
		t.Errorf("References on a bare company = %v, want none", got)
	}
	if got := References(nil); len(got) != 0 {
		t.Errorf("References(nil) = %v, want none", got)
	}
}
