package config

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Redaction is what stands between the config read surface and every
// credential a company holds. Two things have to be true and only one of them
// is about masking: the mask must cover every credential FIELD, and a masked
// document must be able to come back — a config nobody can edit is a config
// nobody maintains.
//
// The vendor each block names is incidental to every assertion below; what
// each one contributes is a SHAPE — a literal credential, a ${VAR}
// reference, a per-seat credential, a credential inside a map. The fixture
// has to PARSE, so the blocks are written as the validator wants them.

const credentialDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-literal-key", "${ROTATED_KEY}"]
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: sk-embeddings-literal
  sandbox:
    fake: true
    setup:
      - name: registry
        files: {"/root/.npmrc": "//registry.example.com/:_authToken=setup-file-literal"}
        env: {REGISTRY_TOKEN: setup-env-literal}
integrations:
  mattermost:
    enabled: true
    url: https://mm.example.com
    team: acme
  confluence:
    url: https://wiki.example.com
    token: cf-literal-token
    webhook_secret: cf-literal-secret
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "${GITLAB_SIGNING}"
    token: gl-literal-pat
mcp_servers:
  - name: notion
    command: notion-mcp
    env:
      NOTION_TOKEN: literal-notion-token
  - name: tracker
    transport: http
    url: https://tracker.example.com/mcp
    headers:
      Authorization: "Bearer header-literal-token"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      notion: {NOTION_TOKEN: per-seat-literal}
    integrations:
      mattermost:
        bot_token: mm-literal-bot-token
`

func credentialCompany(t *testing.T) *Company {
	t.Helper()
	cfg, err := ParseCompany([]byte(credentialDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func TestNoLiteralCredentialSurvivesRedaction(t *testing.T) {
	t.Parallel()
	// The assertion is over the SERIALIZED form, because that is what
	// reaches a reader. Checking the fields the test happens to know about
	// would pass for a document carrying a credential in a field nobody
	// thought to look at, which is exactly the failure the tag prevents.
	redacted := credentialCompany(t).Redact()
	blob, err := json.Marshal(redacted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, literal := range []string{
		"sk-literal-key", "sk-embeddings-literal", "pl-literal-secret",
		"gl-literal-pat", "literal-notion-token", "per-seat-literal",
		"mm-literal-bot-token", "header-literal-token", "setup-file-literal",
		"setup-env-literal",
	} {
		if strings.Contains(string(blob), literal) {
			t.Errorf("the redacted config still carries %q", literal)
		}
	}
}

func TestAReferenceIsNotACredential(t *testing.T) {
	t.Parallel()
	// It NAMES a credential rather than being one, the value it points at
	// is not in this document, and it is the half an operator edits.
	// Masking it would make the read surface useless for its one purpose.
	blob, err := json.Marshal(credentialCompany(t).Redact())
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"${ROTATED_KEY}", "${GITLAB_SIGNING}"} {
		if !strings.Contains(string(blob), reference) {
			t.Errorf("the redacted config lost the reference %q", reference)
		}
	}
}

func TestRedactionLeavesEverythingElseAlone(t *testing.T) {
	t.Parallel()
	// A read surface that dropped the org chart while masking a token
	// would be worse than one that leaked: the operator cannot see what
	// they are editing.
	original := credentialCompany(t)
	redacted := original.Redact()
	if redacted.Name != "Acme" {
		t.Errorf("name = %q", redacted.Name)
	}
	if len(redacted.Roles) != 1 || redacted.Roles[0].Handle != "ceo" {
		t.Errorf("roles = %+v", redacted.Roles)
	}
	if got := redacted.Providers.LLM["zulu"].Model; got != "claude-sonnet-5" {
		t.Errorf("model = %q", got)
	}
	if !reflect.DeepEqual(redacted.Providers.LLMOrder, original.Providers.LLMOrder) {
		t.Errorf("the provider order changed: %v", redacted.Providers.LLMOrder)
	}
}

// A TOGGLE SURVIVES REDACTION, SET TO WHAT THE OPERATOR SET IT TO.
//
// A toggle keeps its state unexported, and the redacting copy once walked
// exported fields only, so every explicit toggle read as unset: a schedule
// kept in config with `enabled: false` came back from GET /config as a
// schedule that fires, and sending that document back enabled it. Each value
// here is the NON-default one, because a toggle that read as unset would
// otherwise resolve to the same answer and prove nothing.
func TestRedactionKeepsEveryToggle(t *testing.T) {
	t.Parallel()
	original, err := ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu: {type: anthropic, model: claude-sonnet-5, api_keys: ["sk-literal-key"]}
turn_engine:
  extension_enabled: false
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false}
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    learning_enabled: false
    schedules:
      - {name: standup, cron: "0 9 * * 1-5", task: "Post the standup", enabled: false, catchup: false}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	redacted := original.Redact()

	for name, toggle := range map[string]Toggle{
		"turn_engine.extension_enabled": redacted.TurnEngine.ExtensionEnabled,
		"mcp_servers[0].shared":         redacted.MCPServers[0].Shared,
		"roles[0].learning_enabled":     redacted.Roles[0].LearningEnabled,
		"roles[0].schedules[0].enabled": redacted.Roles[0].Schedules[0].Enabled,
		"roles[0].schedules[0].catchup": redacted.Roles[0].Schedules[0].Catchup,
	} {
		if !toggle.IsSet() || toggle.Or(true) {
			t.Errorf("%s read as %+v after redaction, want explicitly false", name, toggle)
		}
	}
}

// A REDACTED DOCUMENT SENT BACK IS THE DOCUMENT THAT WAS STORED.
//
// The property every GET-edit-PUT depends on, asserted over the shipped
// example companies, which set more of the schema than any fixture written
// for one test: masking and restoring must between them change nothing at
// all. A field the redacting copy drops, like the toggles above, fails here
// whichever type it lives on, including a type added after this test.
func TestARedactedExampleRoundTripsExactly(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("found %d example companies, want at least 2: %v", len(files), files)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			original, err := ParseCompanyDocument(data)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			want, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			sentBack := original.Redact()
			sentBack.RestoreRedacted(original)
			got, err := json.Marshal(sentBack)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("a redacted document restored against its own revision "+
					"differs from it:\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestRedactionDoesNotTouchTheOriginal(t *testing.T) {
	t.Parallel()
	// The caller's config is what a running engine reads. Masking in place
	// would leave the process holding a company whose every credential is
	// the literal "__redacted__" — an outage produced by looking at
	// something.
	original := credentialCompany(t)
	_ = original.Redact()
	if got := original.Providers.LLM["zulu"].APIKeys[0]; got != "sk-literal-key" {
		t.Fatalf("the original was masked in place: api key = %q", got)
	}
	if got := original.Roles[0].Integrations.Mattermost.BotToken; got != "mm-literal-bot-token" {
		t.Fatalf("the original was masked in place: bot token = %q", got)
	}
}

func TestAnEmptyCredentialStaysEmpty(t *testing.T) {
	t.Parallel()
	// "no credential" and "a credential I could not see" are opposite
	// facts. Masking an empty one would let a round trip turn the first
	// into the second.
	cfg := credentialCompany(t)
	cfg.Integrations.GitLab.Token = ""
	if got := cfg.Redact().Integrations.GitLab.Token; got != "" {
		t.Errorf("an empty credential redacted to %q", got)
	}
}

func TestAMaskedConfigCanBeSentBack(t *testing.T) {
	t.Parallel()
	// GET-edit-PUT is the whole point. Without the restore, a reader who
	// fetched the config, changed one line and sent it back would replace
	// every credential in the company with the mask — silently, and only
	// discovered when each integration started failing to authenticate.
	original := credentialCompany(t)
	edited := original.Redact()
	edited.Name = "Acme Renamed"
	edited.RestoreRedacted(original)

	if edited.Name != "Acme Renamed" {
		t.Errorf("the edit was lost: name = %q", edited.Name)
	}
	if got := edited.Providers.LLM["zulu"].APIKeys[0]; got != "sk-literal-key" {
		t.Errorf("api key = %q, want the one the prior revision held", got)
	}
	if got := edited.Providers.LLM["zulu"].APIKeys[1]; got != "${ROTATED_KEY}" {
		t.Errorf("the reference was disturbed: %q", got)
	}
	if got := edited.Roles[0].Integrations.Mattermost.BotToken; got != "mm-literal-bot-token" {
		t.Errorf("bot token = %q", got)
	}
	if got := edited.Roles[0].MCPEnv["notion"]["NOTION_TOKEN"]; got != "per-seat-literal" {
		t.Errorf("mcp_env credential = %q", got)
	}
	if got := edited.MCPServers[0].Env["NOTION_TOKEN"]; got != "literal-notion-token" {
		t.Errorf("mcp server env = %q", got)
	}
	if got := edited.MCPServers[1].Headers["Authorization"]; got != "Bearer header-literal-token" {
		t.Errorf("mcp server header = %q", got)
	}
	step := edited.Providers.Sandbox.Setup[0]
	if got := step.Env["REGISTRY_TOKEN"]; got != "setup-env-literal" {
		t.Errorf("setup step env = %q", got)
	}
	if got := step.Files["/root/.npmrc"]; !strings.Contains(got, "setup-file-literal") {
		t.Errorf("setup step file = %q", got)
	}
}

// ONLY A WHOLE REFERENCE IS SHOWN.
//
// A value that embeds a reference beside literal text is a credential with a
// pointer in it, and the literal half is what leaks. The resolver expands an
// embedded reference, so these values are legitimate configuration rather
// than typos, which is exactly why they reach a read surface.
func TestOnlyAWholeReferenceSurvivesRedaction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, value string
		shown       bool
	}{
		{"a whole reference", "${TRACKER_TOKEN}", true},
		{"a whole reference with surrounding space", "  ${TRACKER_TOKEN} ", true},
		{"a reference embedded after a literal", "sk-live-SECRET-${SUFFIX}", false},
		{"a prefixed reference", "Bearer ${TRACKER_TOKEN}", false},
		{"two references side by side", "${USER}:${PASSWORD}", false},
		{"a shell expansion the resolver ignores", "secret-${line#host=}", false},
		{"a numeric name the resolver never substitutes", "${1}", false},
		{"an unclosed brace", "sk-${UNCLOSED", false},
		{"a plain literal", "sk-literal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mask(tc.value, true)
			switch {
			case tc.shown && got != tc.value:
				t.Errorf("mask(%q) = %q, want the reference shown", tc.value, got)
			case !tc.shown && got != Redacted:
				t.Errorf("mask(%q) = %q, want the mask", tc.value, got)
			}
		})
	}
}

// AN EMBEDDED REFERENCE STILL ROUND-TRIPS.
//
// Masking a value the operator can partly read is only safe if sending the
// document back restores it: otherwise a GET-edit-PUT would replace the
// header with the marker, and validation would refuse an edit that touched
// something else entirely.
func TestAnEmbeddedReferenceIsMaskedAndRestored(t *testing.T) {
	t.Parallel()
	original := credentialCompany(t)
	original.MCPServers[1].Headers["Authorization"] = "Bearer ${TRACKER_TOKEN}"

	edited := original.Redact()
	if got := edited.MCPServers[1].Headers["Authorization"]; got != Redacted {
		t.Fatalf("header on the read = %q, want the mask", got)
	}
	edited.RestoreRedacted(original)
	if got := edited.MCPServers[1].Headers["Authorization"]; got != "Bearer ${TRACKER_TOKEN}" {
		t.Errorf("header after the restore = %q, want the stored value", got)
	}
}

func TestARealChangeToACredentialIsKept(t *testing.T) {
	t.Parallel()
	// Only the marker is substituted. A caller who actually rotated a
	// credential must not have their new value silently replaced by the
	// old one — which would make rotation through this surface impossible.
	original := credentialCompany(t)
	edited := original.Redact()
	edited.Integrations.Confluence.WebhookSecret = "a-new-secret"
	edited.RestoreRedacted(original)
	if got := edited.Integrations.Confluence.WebhookSecret; got != "a-new-secret" {
		t.Errorf("webhook secret = %q, want the caller's new value", got)
	}
}

func TestAClearedCredentialStaysCleared(t *testing.T) {
	t.Parallel()
	// Removing a credential is a real operation — disabling an
	// integration's write access without deleting the block. Restoring it
	// would make that impossible and look like the API ignored the edit.
	original := credentialCompany(t)
	edited := original.Redact()
	edited.Integrations.GitLab.Token = ""
	edited.RestoreRedacted(original)
	if got := edited.Integrations.GitLab.Token; got != "" {
		t.Errorf("token = %q, want the clearing honoured", got)
	}
}

// nested exercises the walker's own contract: a tag on a composite field
// covers everything inside it. No production field is shaped this way today,
// and the rule has to hold before one is — a tag that worked on a map and
// silently did not on a struct is worse than no tag at all.
type nested struct {
	Public string
	Inner  nestedInner `secret:"true"`
}

type nestedInner struct {
	Value  string
	Values []string
	Keyed  map[string]string
}

func TestATagCoversEverythingBeneathIt(t *testing.T) {
	t.Parallel()
	in := nested{
		Public: "visible",
		Inner: nestedInner{
			Value:  "literal",
			Values: []string{"one", "${REF}"},
			Keyed:  map[string]string{"k": "two"},
		},
	}
	out := reflect.New(reflect.TypeOf(in))
	copyMasking(reflect.ValueOf(in), out.Elem(), false)
	got, _ := out.Interface().(*nested)

	if got.Public != "visible" {
		t.Errorf("an untagged field was masked: %q", got.Public)
	}
	if got.Inner.Value != Redacted {
		t.Errorf("a string inside a tagged struct = %q, want the mask", got.Inner.Value)
	}
	if got.Inner.Values[0] != Redacted {
		t.Errorf("a slice inside a tagged struct = %q, want the mask", got.Inner.Values[0])
	}
	if got.Inner.Values[1] != "${REF}" {
		t.Errorf("a reference inside a tagged struct = %q", got.Inner.Values[1])
	}
	if got.Inner.Keyed["k"] != Redacted {
		t.Errorf("a map inside a tagged struct = %q, want the mask", got.Inner.Keyed["k"])
	}
}

func TestAShortenedKeyListRefusesToGuess(t *testing.T) {
	t.Parallel()
	// Removing a key moves every later slot. Restoring by position would
	// write one credential into another's place — and the result
	// authenticates as the wrong account rather than failing, which is the
	// worst available outcome.
	original := credentialCompany(t)
	edited := original.Redact()
	provider := edited.Providers.LLM["zulu"]
	provider.APIKeys = provider.APIKeys[:1]
	edited.Providers.LLM["zulu"] = provider
	edited.RestoreRedacted(original)

	if got := edited.Providers.LLM["zulu"].APIKeys[0]; got != Redacted {
		t.Errorf("api key = %q, want the mask left standing rather than a "+
			"credential resolved against a list that changed shape", got)
	}
}

func TestAReorderedKeyListRefusesToGuess(t *testing.T) {
	t.Parallel()
	// A list has no correspondence but position. A caller who added or
	// removed a key has changed which slot means what, so resolving masks
	// by position would write one credential into another's place — and
	// the result would authenticate as the wrong account rather than fail.
	original := credentialCompany(t)
	edited := original.Redact()
	provider := edited.Providers.LLM["zulu"]
	provider.APIKeys = append(provider.APIKeys, "${THIRD_KEY}")
	edited.Providers.LLM["zulu"] = provider
	edited.RestoreRedacted(original)

	if got := edited.Providers.LLM["zulu"].APIKeys[0]; got != Redacted {
		t.Errorf("api key = %q, want the mask left standing so validation "+
			"reports it rather than a credential landing in the wrong slot", got)
	}
	// And validation DOES report it. Refusing to guess is right; storing the
	// result silently is not — the literal would be handed to a provider as
	// an API key and fail hours later with an error naming nothing.
	err := edited.Validate()
	if err == nil {
		t.Fatal("a config still holding a redaction mask validated")
	}
	if !strings.Contains(err.Error(), "api_keys") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

// TestEveryCredentialFieldIsTagged is the guard the tag exists for.
//
// A hand-maintained list of secret PATHS is maintained by whoever remembers it
// exists, so the day somebody adds integrations.newthing.token the read
// surface starts publishing it and nothing fails. This walks the config type
// and fails on a field whose name says credential and whose tag does not.
func TestEveryCredentialFieldIsTagged(t *testing.T) {
	t.Parallel()
	// Names that mean "this holds a credential". Deliberately broad: a
	// false positive is one `secret:"true"` to add or one exemption to
	// write down, and a false negative is a published credential.
	credential := []string{"token", "secret", "apikey", "password", "credential"}

	// The exemptions, each of which is a NAME rather than a credential.
	// Listed here so adding one is a decision somebody wrote down.
	exempt := map[string]bool{
		// Numeric caps that merely count tokens (a token budget is a
		// mapping of them, one per calendar window).
		"TokenBudget": true, "BudgetTokens": true, "SummarizeMaxTokens": true,
		"CompactionBudgetTokens": true, "MaxTokens": true, "TokenExpiryDays": true,
		"ReasoningBudgetTokens": true, "MinTokensPerTask": true,
		"SandboxMinBudgetTokens": true,
		// Scope NAMES minted onto a token, not the token.
		"TokenScopes": true,
		// A path on disk, not the material at it.
		"StateDir": true,
		// Tuning for how a dead credential is retried.
		"Cooldowns": true,
		// The Forge app's public identifier: it is the JWT audience, and
		// it is in every manifest the operator installs.
		"ForgeAppID": true,
	}

	// THE TYPE RULE, beside the name rule. A map named Env, Headers or Files
	// is where a process's credentials are handed to it, and its NAME says
	// nothing credential-like: MCPServer.Headers (an authorization header)
	// and SandboxSetupStep.Env and .Files (a registry token, an auth file)
	// all passed the name rule untagged and were published verbatim. Every
	// such map is tagged unless it is exempted here, by Type.Field, with the
	// reason it holds no credential.
	credentialMaps := map[string]bool{"Env": true, "Headers": true, "Files": true}
	exemptMaps := map[string]string{
		// None today. An entry is a decision somebody wrote down, e.g.
		// "Thing.Env": "names of variables only; the values live elsewhere".
	}
	// By SHAPE, not by type identity: a named `type EnvMap map[string]string`
	// or a pointer to one carries a process's credentials exactly as the
	// bare map does, and comparing against map[string]string itself would
	// wave both through.
	stringMap := func(rt reflect.Type) bool {
		for rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		return rt.Kind() == reflect.Map && rt.Key().Kind() == reflect.String &&
			rt.Elem().Kind() == reflect.String
	}

	var walk func(t reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice ||
			rt.Kind() == reflect.Map || rt.Kind() == reflect.Array {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			field := rt.Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.ToLower(field.Name)
			tagged := field.Tag.Get(secretTag) == "true"
			if stringMap(field.Type) && credentialMaps[field.Name] && !tagged {
				if _, ok := exemptMaps[rt.Name()+"."+field.Name]; !ok {
					t.Errorf("%s.%s is a map[string]string named %s and is not "+
						"tagged secret:\"true\": a process's credentials are "+
						"handed to it through exactly this shape, so the config "+
						"read surface publishes them. Tag it, or exempt it with "+
						"the reason it holds none",
						path+rt.Name(), field.Name, field.Name)
				}
			}
			for _, needle := range credential {
				if strings.Contains(name, needle) && !tagged && !exempt[field.Name] {
					t.Errorf("%s.%s looks like a credential and is not tagged "+
						"secret:\"true\", so the config read surface publishes it",
						path+rt.Name(), field.Name)
					break
				}
			}
			walk(field.Type, path+rt.Name()+".", seen)
		}
	}
	walk(reflect.TypeOf(Company{}), "", map[reflect.Type]bool{})
}

// rosterDoc is two seats and two MCP servers, each holding a credential of
// its own — the shape that makes a mis-matched restore visible, because
// every value says which member it belongs to.
const rosterDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-literal-key"]
mcp_servers:
  - name: notion
    command: notion-mcp
    env: {TOKEN: notion-literal}
  - name: tracker
    command: tracker-mcp
    env: {TOKEN: tracker-literal}
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker: {TOKEN: ceo-literal}
  - name: CTO
    handle: cto
    llm: zulu
    mcp_env:
      tracker: {TOKEN: cto-literal}
`

func rosterCompany(t *testing.T) *Company {
	t.Helper()
	cfg, err := ParseCompany([]byte(rosterDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// A CREDENTIAL BELONGS TO THE SEAT, NOT TO THE SLOT.
//
// A caller who reordered the roster — the whole edit — used to have every
// mask resolved against whichever seat now sat in that position, so the CEO
// received the CTO's token and the CTO the CEO's. Silently: the lengths
// matched so nothing was refused, and no mask was left standing for
// [Company.UnresolvedMasks] to report. Each agent then authenticated to its
// tools AS ITS COLLEAGUE, which is the one failure a per-seat credential
// exists to make impossible.
func TestAReorderedRosterKeepsEachSeatsOwnCredential(t *testing.T) {
	t.Parallel()
	original := rosterCompany(t)
	edited := original.Redact()
	edited.Roles[0], edited.Roles[1] = edited.Roles[1], edited.Roles[0]
	edited.MCPServers[0], edited.MCPServers[1] = edited.MCPServers[1], edited.MCPServers[0]
	edited.RestoreRedacted(original)

	if err := edited.Validate(); err != nil {
		t.Fatalf("a reordered roster no longer validates: %v", err)
	}
	for _, want := range []struct{ handle, token string }{
		{"ceo", "ceo-literal"}, {"cto", "cto-literal"},
	} {
		var got string
		for i := range edited.Roles {
			if edited.Roles[i].IdentityKey() == want.handle {
				got = edited.Roles[i].MCPEnv["tracker"]["TOKEN"]
			}
		}
		if got != want.token {
			t.Errorf("%s holds %q after a reorder, want %q — a reorder handed "+
				"one seat another's credential", want.handle, got, want.token)
		}
	}
	for _, want := range []struct{ name, token string }{
		{"notion", "notion-literal"}, {"tracker", "tracker-literal"},
	} {
		var got string
		for i := range edited.MCPServers {
			if edited.MCPServers[i].Name == want.name {
				got = edited.MCPServers[i].Env["TOKEN"]
			}
		}
		if got != want.token {
			t.Errorf("mcp server %s holds %q after a reorder, want %q",
				want.name, got, want.token)
		}
	}
}

// AND ADDING A SEAT DOES NOT STRAND EVERY OTHER SEAT'S CREDENTIAL.
//
// Matching by position had to refuse the whole list when the lengths differed,
// so appending one seat to a redacted document left every EXISTING seat still
// holding the marker — and the caller had to re-supply credentials they had
// never seen to add a colleague. Identity has no such coupling: the seats that
// were there are matched, and only the new one is left to be filled in.
func TestAnAddedSeatDoesNotStrandTheOtherSeatsCredentials(t *testing.T) {
	t.Parallel()
	original := rosterCompany(t)
	edited := original.Redact()
	edited.Roles = append(edited.Roles, Role{
		Name: "CFO", Handle: "cfo",
		LLM: PhaseLLM{Default: ProviderKeys{"zulu"}},
	})
	edited.RestoreRedacted(original)

	for i := range edited.Roles {
		handle := edited.Roles[i].IdentityKey()
		if handle == "cfo" {
			continue
		}
		if got := edited.Roles[i].MCPEnv["tracker"]["TOKEN"]; got == Redacted {
			t.Errorf("%s still holds the marker after an unrelated seat was "+
				"added — adding a colleague must not require re-supplying "+
				"credentials the caller never saw", handle)
		}
	}
	if err := edited.Validate(); err != nil {
		t.Fatalf("adding a seat with no credentials of its own was refused: %v", err)
	}
}

// A DUPLICATED IDENTITY IS MATCHED TO NOTHING, NEITHER BY NAME NOR BY SLOT.
//
// Identity is only safe while it is unique, so a document carrying the same
// handle twice must not have one seat's credentials resolved against the
// other's. Falling back to POSITION, as the restore once did, is the one
// correspondence certain to be wrong after a reorder: the CEO's slot held the
// CTO's token. Both masks stay standing instead, and validation names them.
func TestADuplicatedIdentityIsMatchedToNothing(t *testing.T) {
	t.Parallel()
	original := rosterCompany(t)
	original.Roles[1].Handle = "ceo" // two seats, one identity
	edited := original.Redact()
	edited.Roles[0], edited.Roles[1] = edited.Roles[1], edited.Roles[0]
	edited.RestoreRedacted(original)

	for i := range edited.Roles {
		if got := edited.Roles[i].MCPEnv["tracker"]["TOKEN"]; got != Redacted {
			t.Errorf("roles[%d] holds %q, want the mask left standing for a "+
				"duplicated identity", i, got)
		}
	}
	assertUnresolved(t, edited, "roles[0].mcp_env.tracker.TOKEN", "roles[1].mcp_env.tracker.TOKEN")
}

// AN IDENTIFIED LIST INSIDE A MEMBER NEVER FALLS BACK TO POSITION EITHER.
//
// Sandbox setup steps are matched by name within their list. A submitted
// document is refused for two steps of one name, but a stored revision from
// before that rule can still hold them, and it is the prior a write restores
// from. A reorder must not trade their registry tokens.
func TestDuplicateStepNamesAreMatchedToNothing(t *testing.T) {
	t.Parallel()
	original := credentialCompany(t)
	original.Providers.Sandbox.Setup = []SandboxSetupStep{
		{Name: "registry", Env: map[string]string{"TOKEN": "first-registry-literal"}},
		{Name: "registry", Env: map[string]string{"TOKEN": "second-registry-literal"}},
	}
	edited := original.Redact()
	steps := edited.Providers.Sandbox.Setup
	steps[0], steps[1] = steps[1], steps[0]
	edited.RestoreRedacted(original)

	for i, step := range edited.Providers.Sandbox.Setup {
		if got := step.Env["TOKEN"]; got != Redacted {
			t.Errorf("setup[%d] holds %q, want the mask left standing", i, got)
		}
	}
}

// movingDoc is a company whose every seat and unit holds a credential of its
// own, named after its owner, so any restore that matched the wrong member is
// visible in the value it produced.
const movingDoc = `
name: Acme
providers:
  llm:
    zulu: {type: anthropic, model: claude-sonnet-5, api_keys: ["sk-literal-key"]}
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false}
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env: {tracker: {TOKEN: ceo-literal}}
units:
  - name: Engineering
    mcp_env: {tracker: {TOKEN: engineering-literal}}
    roles:
      - {name: CTO, handle: cto, llm: zulu, mcp_env: {tracker: {TOKEN: cto-literal}}}
    children:
      - name: Platform
        mcp_env: {tracker: {TOKEN: platform-literal}}
        roles:
          - {name: SRE, handle: sre, llm: zulu, mcp_env: {tracker: {TOKEN: sre-literal}}}
  - name: Product
    roles:
      - {name: PM, handle: pm, llm: zulu, mcp_env: {tracker: {TOKEN: pm-literal}}}
`

// seatTokens reads each seat's tracker token by handle, wherever it sits.
func seatTokens(c *Company) map[string]string {
	out := map[string]string{}
	for role := range c.EachRole() {
		out[role.IdentityKey()] = role.MCPEnv["tracker"]["TOKEN"]
	}
	return out
}

// unitTokens reads each unit's tracker token by name, wherever it sits.
func unitTokens(c *Company) map[string]string {
	out := map[string]string{}
	var walk func(units []Unit)
	walk = func(units []Unit) {
		for i := range units {
			if token, ok := units[i].MCPEnv["tracker"]["TOKEN"]; ok {
				out[units[i].Name] = token
			}
			walk(units[i].Children)
		}
	}
	walk(c.Units)
	return out
}

// assertUnresolved fails unless exactly these paths still hold the mask.
func assertUnresolved(t *testing.T, c *Company, want ...string) {
	t.Helper()
	var got []string
	for _, path := range c.UnresolvedMasks() {
		got = append(got, path.String())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("unresolved masks = %v, want %v", got, want)
	}
}

// A CREDENTIAL FOLLOWS ITS SEAT OR UNIT WHEREVER IT MOVES.
//
// Matching inside one list only restored a member that stayed in the list it
// had been in. Moving a seat into a unit, or a team under another department,
// left its masks standing, so the builder's most ordinary edit was refused
// with a redaction error on credentials the operator never touched.
func TestACredentialFollowsItsMemberAcrossAMove(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(c *Company)
	}{
		{"a root seat moves into a unit", func(c *Company) {
			c.Units[1].Roles = append(c.Units[1].Roles, c.Roles[0])
			c.Roles = nil
		}},
		{"a seat moves from one unit to another", func(c *Company) {
			c.Units[1].Roles = append(c.Units[1].Roles, c.Units[0].Roles[0])
			c.Units[0].Roles = nil
		}},
		{"a unit seat moves to the root", func(c *Company) {
			c.Roles = append(c.Roles, c.Units[1].Roles[0])
			c.Units[1].Roles = nil
		}},
		{"a unit moves under another unit", func(c *Company) {
			c.Units[1].Children = append(c.Units[1].Children, c.Units[0].Children[0])
			c.Units[0].Children = nil
		}},
		{"a child unit moves to the top level", func(c *Company) {
			c.Units = append(c.Units, c.Units[0].Children[0])
			c.Units[0].Children = nil
		}},
		{"units and seats are reordered", func(c *Company) {
			c.Units[0], c.Units[1] = c.Units[1], c.Units[0]
			c.Roles = append(c.Roles, c.Units[0].Roles[0])
			c.Roles[0], c.Roles[1] = c.Roles[1], c.Roles[0]
			c.Units[0].Roles = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original, err := ParseCompany([]byte(movingDoc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			edited := original.Redact()
			tc.edit(edited)
			edited.RestoreRedacted(original)

			if got, want := seatTokens(edited), seatTokens(original); !maps.Equal(got, want) {
				t.Errorf("seat credentials after the edit = %v, want %v", got, want)
			}
			if got, want := unitTokens(edited), unitTokens(original); !maps.Equal(got, want) {
				t.Errorf("unit credentials after the edit = %v, want %v", got, want)
			}
			assertUnresolved(t, edited)
			if err := edited.Validate(); err != nil {
				t.Errorf("the moved company does not validate: %v", err)
			}
		})
	}
}

// A RENAME IS A NEW IDENTITY, AND ITS MASKS ARE REFUSED RATHER THAN GUESSED.
//
// A seat's identity is its handle, so a new display name over a declared
// handle keeps its credentials, while a new handle has no prior value of its
// own. A renamed unit loses its own credentials the same way, and the seats
// inside it, whose identities did not change, keep theirs.
func TestARenameIsRefusedAndOnlyTheRenamedMemberLosesItsMask(t *testing.T) {
	t.Parallel()
	original, err := ParseCompany([]byte(movingDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	edited := original.Redact()
	edited.Units[0].Roles[0].Name = "Chief Technology Officer" // handle cto is declared, so kept
	edited.Units[1].Roles[0].Handle = "product-manager"        // a new identity
	edited.Units[0].Name = "Eng"                               // a new unit identity
	edited.RestoreRedacted(original)

	tokens := seatTokens(edited)
	if tokens["cto"] != "cto-literal" || tokens["sre"] != "sre-literal" {
		t.Errorf("seats whose identity did not change lost their credentials: %v", tokens)
	}
	assertUnresolved(t, edited,
		"units[0].mcp_env.tracker.TOKEN",
		"units[1].roles[0].mcp_env.tracker.TOKEN")
	err = edited.Validate()
	if err == nil || !strings.Contains(err.Error(), "units[1].roles[0].mcp_env.tracker.TOKEN") {
		t.Errorf("validation does not name the renamed seat's credential: %v", err)
	}
}

// AN EMPTY IDENTITY MATCHES NOTHING.
//
// A seat whose name derives no handle is refused by validation, but it can sit
// in a stored revision a write restores from. Every such seat shares the empty
// identity, so matching on it hands one seat's credentials to another.
func TestAnEmptyIdentityMatchesNothing(t *testing.T) {
	t.Parallel()
	original, err := ParseCompanyDocument([]byte(`
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false}
roles:
  - {name: "!!!", mcp_env: {tracker: {TOKEN: punctuation-literal}}}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	edited := original.Redact()
	edited.RestoreRedacted(original)
	assertUnresolved(t, edited, "roles[0].mcp_env.tracker.TOKEN")
}

// THE DUPLICATE SIBLING PROBE: TWO UNITS OF ONE NAME NEVER TRADE CREDENTIALS.
//
// A stored revision a build before the name rules admitted can hold two units
// called "Platform", each with its own team's token. Reordering them, or
// moving and renaming one, restored through position or through either of the
// two names hands one team the other's token with no mask left standing to
// refuse it. The only safe outcome is a mask error on both.
func TestTheDuplicateSiblingProbeIsAMaskErrorNeverASwap(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false}
units:
  - name: Platform
    mcp_env: {tracker: {TOKEN: secret-A}}
    roles: [{name: Dev A, handle: dev-a}]
  - name: Platform
    mcp_env: {tracker: {TOKEN: secret-B}}
    roles: [{name: Dev B, handle: dev-b}]
  - name: Payments
    roles: [{name: Dev C, handle: dev-c}]
`
	for _, tc := range []struct {
		name       string
		edit       func(c *Company)
		unresolved []string
	}{
		{
			"the two are swapped",
			func(c *Company) { c.Units[0], c.Units[1] = c.Units[1], c.Units[0] },
			[]string{"units[0].mcp_env.tracker.TOKEN", "units[1].mcp_env.tracker.TOKEN"},
		},
		{
			"one moves under another unit and is renamed",
			func(c *Company) {
				moved := c.Units[1]
				moved.Name = "Payments Platform"
				c.Units[2].Children = append(c.Units[2].Children, moved)
				c.Units = []Unit{c.Units[0], c.Units[2]}
			},
			[]string{"units[0].mcp_env.tracker.TOKEN", "units[1].children[0].mcp_env.tracker.TOKEN"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original, err := ParseCompanyDocument([]byte(doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			edited := original.Redact()
			tc.edit(edited)
			edited.RestoreRedacted(original)

			blob, err := json.Marshal(edited)
			if err != nil {
				t.Fatal(err)
			}
			for _, literal := range []string{"secret-A", "secret-B"} {
				if strings.Contains(string(blob), literal) {
					t.Errorf("a unit of a duplicated name was restored to %q: %s", literal, blob)
				}
			}
			assertUnresolved(t, edited, tc.unresolved...)
			if err := edited.Validate(); err == nil || !strings.Contains(err.Error(), Redacted) {
				t.Errorf("validation does not report the standing masks: %v", err)
			}
			// The seats inside still carry nothing to restore and keep
			// their identities; nothing about the probe touched them.
			if tokens := seatTokens(edited); len(tokens) != 3 {
				t.Errorf("seats = %v, want the three seats untouched", tokens)
			}
		})
	}
}

// EVERY COLLECTION WHOSE MEMBERS CARRY CREDENTIALS CAN NAME ITSELF.
//
// The guard the interface exists for, and the same shape as
// [TestEveryCredentialFieldIsTagged]: a slice of structs that holds a
// credential anywhere beneath it is a collection whose members must be
// matchable by identity, because position will otherwise hand one member's
// credential to another. Nothing fails when a new one forgets — it just
// silently goes back to the old, wrong correspondence.
func TestEveryCredentialCarryingCollectionCanNameItself(t *testing.T) {
	t.Parallel()
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Map:
			walk(typ.Elem(), path)
		case reflect.Slice:
			element := typ.Elem()
			if element.Kind() == reflect.Struct && holdsCredential(element, map[reflect.Type]bool{}) &&
				!element.Implements(reflect.TypeOf((*identified)(nil)).Elem()) {
				t.Errorf("%s is a slice of %s, which carries a credential and "+
					"has no IdentityKey — its masks will be restored by "+
					"POSITION, so a reorder hands one member's credential to "+
					"another", path, element.Name())
			}
			walk(element, path+"[]")
		case reflect.Struct:
			for i := range typ.NumField() {
				field := typ.Field(i)
				if field.IsExported() {
					walk(field.Type, path+"."+field.Name)
				}
			}
		}
	}
	walk(reflect.TypeOf(Company{}), "Company")
}

// holdsCredential reports whether a type carries a secret-tagged field
// anywhere beneath it.
func holdsCredential(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		return holdsCredential(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			if field.Tag.Get(secretTag) == "true" || holdsCredential(field.Type, seen) {
				return true
			}
		}
	}
	return false
}

// A STANDING MASK SAYS EVERY REASON IT COULD NOT BE MATCHED, AND WHAT TO DO.
//
// The refusal once named only a new or renamed member and a bare list that
// changed length. A stored revision with two units called "Platform" leaves
// masks standing on units nobody created or renamed, and an operator told
// only those two causes has nothing to act on.
func TestAStandingMaskNamesEveryCauseAndTheRemedy(t *testing.T) {
	t.Parallel()
	original, err := ParseCompanyDocument([]byte(`
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false}
units:
  - {name: Platform, mcp_env: {tracker: {TOKEN: secret-A}}, roles: [{name: Dev A, handle: dev-a}]}
  - {name: Platform, mcp_env: {tracker: {TOKEN: secret-B}}, roles: [{name: Dev B, handle: dev-b}]}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	edited := original.Redact()
	edited.RestoreRedacted(original)

	err = edited.ValidateRunnable()
	if err == nil {
		t.Fatal("a document holding standing masks validated")
	}
	for _, want := range []string{
		"units[0].mcp_env.tracker.TOKEN",
		"new or was renamed",
		"more than one member",
		"changed length",
		"${VAR} reference",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}
