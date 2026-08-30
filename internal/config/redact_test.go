package config

import (
	"encoding/json"
	"reflect"
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
		"mm-literal-bot-token",
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
		// Numeric caps that merely count tokens.
		"TokenBudget": true, "BudgetTokens": true, "SummarizeMaxTokens": true,
		"CompactionBudgetTokens": true, "MaxTokens": true, "TokenExpiryDays": true,
		"ReasoningBudgetTokens": true, "SubagentMinPerChildTokens": true,
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

// A MEMBER THAT CANNOT NAME ITSELF IS NOT MATCHED BY NAME.
//
// The fallback matters as much as the matching: identity is only safe while
// it is unique, so a document carrying the same handle twice must not have
// one seat's credentials resolved against the other's.
func TestADuplicateIdentityFallsBackRatherThanPickingOne(t *testing.T) {
	t.Parallel()
	original := rosterCompany(t)
	original.Roles[1].Handle = "ceo" // two seats, one identity
	edited := original.Redact()
	edited.Roles[0], edited.Roles[1] = edited.Roles[1], edited.Roles[0]
	edited.RestoreRedacted(original)

	// Positional matching is what a non-identity earns, so the reorder is
	// resolved against the wrong slot — but the ambiguity is the document's,
	// and [Company.Validate] is what refuses a roster with two of one handle.
	if got := edited.Roles[0].MCPEnv["tracker"]["TOKEN"]; got != "ceo-literal" {
		t.Errorf("a duplicated identity was matched by name anyway: %q", got)
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
