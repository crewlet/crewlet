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
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: sk-a-literal-key}
      env: {ROTATED_KEY: "${ROTATED_KEY}"}
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
		{Path: "providers.llm.zulu.env.ROTATED_KEY", Name: "ROTATED_KEY"},
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
		if strings.HasPrefix(ref.Path, "providers.llm.zulu.auth.api_key") {
			t.Errorf("a literal api key was reported as a reference to %q", ref.Name)
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

// A VARIABLE THE COMPANY READS WITHOUT NAMING IT IS STILL READ. An entry that
// writes no credential signs in from its CLI's own variable — the token
// `crewlet llm login -capture-token` stored, the key already exported — and
// from the bundle `crewlet llm export -secret-store` stored, and the Secrets
// screen calls a row nothing reads safe to delete. So each is reported at the
// field that falls back, by the mode that reads it, and a field the entry
// writes is reported as written and never as its fallback too.
//
// Mutation: report References alone, and every implied row below is missing;
// ignore the auth mode, and the plan entry gains the API key it never reads.
func TestAnEntrysFallbackCredentialIsReportedAsRead(t *testing.T) {
	t.Parallel()
	cfg, err := ParseCompany([]byte(`
name: Acme
providers:
  llm:
    plan:
      model: sonnet
    metered:
      model: haiku
      auth: {mode: api-key}
    named:
      model: sonnet
      auth: {token: "${MY_TOKEN}", credential_bundle: "${MY_BUNDLE}"}
    host:
      agent: codex
      model: gpt-5
      auth: {mode: inherit-env}
  embeddings:
    type: openai
    model: text-embedding-3-small
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := cfg.VariablesRead()
	want := []Reference{
		{Path: "providers.embeddings.api_key", Name: EmbeddingKeyVar},
		{Path: "providers.llm.host.auth.credential_bundle", Name: "CREWLET_LLM_CLI_HOST_CREDENTIALS"},
		{Path: "providers.llm.metered.auth.api_key", Name: "ANTHROPIC_API_KEY"},
		{Path: "providers.llm.metered.auth.credential_bundle", Name: "CREWLET_LLM_CLI_METERED_CREDENTIALS"},
		{Path: "providers.llm.named.auth.credential_bundle", Name: "MY_BUNDLE"},
		{Path: "providers.llm.named.auth.token", Name: "MY_TOKEN"},
		{Path: "providers.llm.plan.auth.credential_bundle", Name: "CREWLET_LLM_CLI_PLAN_CREDENTIALS"},
		{Path: "providers.llm.plan.auth.token", Name: "CLAUDE_CODE_OAUTH_TOKEN"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("VariablesRead mismatch\n got %v\nwant %v", got, want)
	}
	// The literal walk is unchanged: it answers what the document NAMES.
	for _, ref := range References(cfg) {
		if ref.Name == "CLAUDE_CODE_OAUTH_TOKEN" || ref.Name == EmbeddingKeyVar {
			t.Errorf("References reported %v, which the document never names", ref)
		}
	}
}
