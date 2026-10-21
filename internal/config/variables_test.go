package config

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/envref"
)

// variablesDoc reads one variable each way a company document can: a
// reference in a field, each LLM entry's conventional key where it names none,
// a cli-agent entry's conventional token and bundle where it points at
// neither, and the embedder's key where it names none.
const variablesDoc = `
name: Acme
providers:
  llm:
    conventional:
      type: anthropic
      model: claude-sonnet-5
    named:
      type: openai
      model: gpt-5
      api_keys: ["${ACME_OPENAI_KEY}"]
    sub-agent:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
    pointed:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
        auth:
          token: "${POINTED_TOKEN}"
          credential_bundle: "${POINTED_BUNDLE}"
  embeddings:
    type: openai
    model: text-embedding-3-small
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "${GITLAB_SIGNING}"
roles:
  - name: CEO
    handle: ceo
    llm: conventional
`

// A DOCUMENT READS ITS CONVENTIONS AS WELL AS ITS REFERENCES, and the listing
// says so.
//
// A reader that is handed only the values a document reads — a CLI run asking
// a running node for them — is handed exactly this list, so a variable missing
// from it resolves from the environment even where the store holds it:
// `crewlet llm export -secret-store` writes a bundle under the conventional
// name, which no document references. And a convention is listed only where
// its entry names nothing of its own — an entry pointing at its own variable
// reads that one, and its convention never.
func TestADocumentReadsItsConventionsAsWellAsItsReferences(t *testing.T) {
	t.Parallel()
	company, err := ParseCompany([]byte(variablesDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := company.VariablesRead()
	for _, want := range []string{
		"GITLAB_SIGNING", "ACME_OPENAI_KEY", "POINTED_TOKEN", "POINTED_BUNDLE",
		AnthropicKeyVar,                         // the anthropic entry names no api_keys
		"CLAUDE_CODE_OAUTH_TOKEN",               // claude-code's token_env, for sub-agent
		"CREWLET_LLM_CLI_SUB_AGENT_CREDENTIALS", // sub-agent's bundle
		OpenAIKeyVar,                            // the embedder names no api_key
	} {
		if !slices.Contains(got, want) {
			t.Errorf("VariablesRead() = %v, missing %s", got, want)
		}
	}
	if slices.Contains(got, "CREWLET_LLM_CLI_POINTED_CREDENTIALS") {
		t.Errorf("an entry pointing at its own bundle still lists the conventional "+
			"one: %v", got)
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("VariablesRead() = %v, want it sorted with each name once", got)
	}
}

// AN EMBEDDER NAMING ITS OWN KEY READS THAT ONE, so OPENAI_API_KEY is not
// listed for it — and nothing else in this document reads it.
func TestAnEntryNamingItsOwnKeyListsNoConvention(t *testing.T) {
	t.Parallel()
	company, err := ParseCompany([]byte(`
name: Acme
providers:
  llm:
    named:
      type: openai
      model: gpt-5
      api_keys: ["${ACME_OPENAI_KEY}"]
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: "${EMBED_KEY}"
roles:
  - name: CEO
    handle: ceo
    llm: named
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := company.VariablesRead()
	if !slices.Equal(got, []string{"ACME_OPENAI_KEY", "EMBED_KEY"}) {
		t.Errorf("VariablesRead() = %v, want exactly the two named keys", got)
	}
}

// EVERY NAME LISTED IS ONE A STORE CAN HOLD.
//
// A token_env an override spells outside the reference grammar is refused on
// every write, but a provisioner acts on a file held to the runnable rules
// alone, so it can still carry one — and listed, it was sent to a node's batch
// read, which refused the whole request: every run reading the company's
// values through a node failed, while the same run on a stopped node read a
// plain miss. No store keeps such a name, so it is not listed and resolves
// from the environment on every route, as the engine resolves it; the
// entry's bundle, whose name is derived in the grammar, still is.
func TestEveryVariableReadIsOneAStoreCanHold(t *testing.T) {
	t.Parallel()
	company, err := ParseCompanyDocument([]byte(cliEntry(
		"cli: {agent: claude-code, overrides: {token_env: MY-TOKEN}}")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// THE PROVISIONERS' LOAD: the runnable rules, which this file passes.
	if err := company.ValidateRunnable(); err != nil {
		t.Fatalf("a file a provisioner acts on was refused: %v", err)
	}
	got := company.VariablesRead()
	if slices.Contains(got, "MY-TOKEN") {
		t.Errorf("VariablesRead() = %v, which lists a name no store can hold", got)
	}
	if !slices.Contains(got, "CREWLET_LLM_CLI_SUB_CREDENTIALS") {
		t.Errorf("VariablesRead() = %v, missing the entry's bundle", got)
	}
	for _, name := range got {
		if !envref.ValidName(name) {
			t.Errorf("VariablesRead() lists %q, which no store can hold", name)
		}
	}
}
