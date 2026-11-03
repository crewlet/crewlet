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
  sandbox: {fake: true}
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
        enabled: false
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
// writes no credential signs in from its harness's own variable — the token
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
  sandbox: {fake: true}
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
      harness: codex
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

// AN EMBEDDER THAT NAMES ITS KEY READS NO FALLBACK. providers.embeddings
// falls back to OPENAI_API_KEY only while api_key is empty, so one that writes
// a reference is reported at that field under the name it wrote and never as
// reading the conventional key too: the Secrets screen would otherwise show
// an OPENAI_API_KEY row as read by a company whose embedder never touches it.
// The LLM entries above hold the same rule per credential; the embedder
// decides it in a branch of its own, so it is pinned on its own.
//
// Mutation: drop the empty-key guard in VariablesRead, and EmbeddingKeyVar is
// reported beside MY_KEY.
func TestAnEmbedderNamingItsKeyReadsNoFallback(t *testing.T) {
	t.Parallel()
	cfg, err := ParseCompany([]byte(`
name: Acme
providers:
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: "${MY_KEY}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Reference{{Path: "providers.embeddings.api_key", Name: "MY_KEY"}}
	if got := cfg.VariablesRead(); !slices.Equal(got, want) {
		t.Errorf("VariablesRead = %v, want %v alone", got, want)
	}
}

// ONE ENTRY'S READS ARE ITS OWN, by the rule VariablesRead uses: what the
// entry names and what it falls back to, and nothing of an entry whose key
// starts with the same letters and a dot — `gpt-5` beside `gpt-5.1`, which
// a path prefix cannot tell apart. Every entry's reads together are every
// read VariablesRead reports under providers.llm, so the two cannot drift.
func TestAnEntrysReadsAreItsOwn(t *testing.T) {
	t.Parallel()
	cfg, err := ParseCompanyDocument([]byte(`
name: Acme
roles:
  - name: CEO
    handle: ceo
    goal: Run the company
providers:
  sandbox: {fake: true}
  llm:
    gpt-5:
      harness: codex
      model: gpt-5
      env: {HTTPS_PROXY: "${PROXY_URL}"}
    gpt-5.1:
      harness: codex
      model: "${NEWER_MODEL}"
      auth: {mode: api-key, api_key: "${NEWER_KEY}"}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for key, want := range map[string][]string{
		"gpt-5":   {"CREWLET_LLM_CLI_GPT_5_CREDENTIALS", "PROXY_URL"},
		"gpt-5.1": {"CREWLET_LLM_CLI_GPT_5_1_CREDENTIALS", "NEWER_KEY", "NEWER_MODEL"},
		"absent":  nil,
	} {
		if got := cfg.LLMEntryReads(key); !slices.Equal(got, want) {
			t.Errorf("LLMEntryReads(%q) = %v, want %v", key, got, want)
		}
	}

	var every, indexed []string
	for key := range cfg.Providers.LLM {
		every = append(every, cfg.LLMEntryReads(key)...)
	}
	for _, ref := range cfg.VariablesRead() {
		if strings.HasPrefix(ref.Path, "providers.llm.") {
			indexed = append(indexed, ref.Name)
		}
	}
	slices.Sort(every)
	slices.Sort(indexed)
	if !slices.Equal(slices.Compact(every), slices.Compact(indexed)) {
		t.Errorf("the entries' reads %v are not the reads VariablesRead reports under "+
			"providers.llm, %v", every, indexed)
	}
}
