package config

import (
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Parsing must leave a reference ALONE. `crewlet validate` runs on a laptop
// where no credential exists, and a stored revision that carried resolved
// secrets would leak them into every export and every dashboard view.
func TestTierBKeepsReferencesVerbatim(t *testing.T) {
	t.Setenv("ACME_KEY", "sk-real-value")
	cfg, err := ParseCompany([]byte(`
name: Acme
providers:
  llm:
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ACME_KEY}"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers.LLM["default"].APIKeys[0]; got != "${ACME_KEY}" {
		t.Fatalf("parsing resolved a Tier B reference: %q", got)
	}
}

// Tier A is the opposite: the store path, the broker URL and the API tokens
// are needed the instant the process starts, so they are resolved before
// the document is decoded.
func TestTierAResolvesAtLoad(t *testing.T) {
	t.Setenv("ACME_DB", "/srv/acme.db")
	t.Setenv("ACME_TOKEN", "tok-123")
	cfg, err := ParseBootstrap([]byte(`
store:
  path: "${ACME_DB}"
api:
  port: 8000
  auth:
    tokens:
      - id: founder
        token: "${ACME_TOKEN}"
`), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != "/srv/acme.db" {
		t.Fatalf("store path = %q", cfg.Store.Path)
	}
	if cfg.API.Auth.Tokens[0].Token != "tok-123" {
		t.Fatalf("token = %q", cfg.API.Auth.Tokens[0].Token)
	}
}

// THE STORE WINS. Env-first would let a stale .env shadow a freshly rotated
// secret, which surfaces days later as an auth error from a provider rather
// than at the point of rotation — the exact failure a secret store exists
// to remove.
func TestStoreBeatsEnvironment(t *testing.T) {
	t.Setenv("ROTATING", "stale-from-dotenv")
	r := WithStore(MapSource{"ROTATING": "fresh-from-store"})
	if got := r.Value("${ROTATING}"); got != "fresh-from-store" {
		t.Fatalf("the store must win; got %q", got)
	}
}

// A stored EMPTY value is authoritative, not a miss: an operator who
// deliberately stored an empty credential has said something, and falling
// through to a stale export would undo it.
func TestStoredEmptyValueIsAuthoritative(t *testing.T) {
	t.Setenv("MAYBE", "from-env")
	r := WithStore(MapSource{"MAYBE": ""})
	if got := r.Value("${MAYBE}"); got != "" {
		t.Fatalf("a stored empty value must stop the chain; got %q", got)
	}
}

func TestEnvironmentIsTheFallback(t *testing.T) {
	t.Setenv("ONLY_ENV", "from-env")
	r := WithStore(MapSource{"SOMETHING_ELSE": "x"})
	if got := r.Value("${ONLY_ENV}"); got != "from-env" {
		t.Fatalf("got %q", got)
	}
}

// An unresolved reference expands to the empty string — matching the shell
// — and is REPORTED, because the empty string is not detectable
// downstream: "Bearer ${TOKEN}" with TOKEN unset becomes "Bearer ", which
// is truthy-but-broken and reads as a rejected credential rather than a
// missing one.
func TestUnresolvedReferenceIsEmptyAndReported(t *testing.T) {
	t.Parallel()
	r := NewResolver(MapSource{})
	value, missing := r.Expand("Bearer ${ACME_NOT_SET}")
	if value != "Bearer " {
		t.Fatalf("value = %q", value)
	}
	if !slices.Equal(missing, []string{"ACME_NOT_SET"}) {
		t.Fatalf("missing = %v", missing)
	}
}

func TestDocumentResolutionReportsThePath(t *testing.T) {
	t.Parallel()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(`
api:
  auth:
    tokens:
      - id: founder
        token: "${ACME_ABSENT_TOKEN}"
`), &doc); err != nil {
		t.Fatal(err)
	}
	missing, err := NewResolver(MapSource{}).Document(&doc, reflect.TypeFor[Bootstrap]())
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %#v", missing)
	}
	if missing[0].Path != "api.auth.tokens[0].token" {
		t.Fatalf("path = %q", missing[0].Path)
	}
	if !slices.Equal(missing[0].Names, []string{"ACME_ABSENT_TOKEN"}) {
		t.Fatalf("names = %v", missing[0].Names)
	}
}

// A config's key space is its schema, and a schema that changed with the
// environment would not be one.
func TestDocumentResolutionNeverTouchesKeys(t *testing.T) {
	t.Setenv("KEYNAME", "port")
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("\"${KEYNAME}\": 1\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := EnvOnly().Document(&doc, reflect.TypeFor[map[string]any]()); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := doc.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["${KEYNAME}"]; !ok {
		t.Fatalf("a key was substituted: %v", out)
	}
}

// A substituted scalar must stay a string. An unquoted ${PORT} resolving to
// "8080" that was retagged as an integer would decode into a string field
// as a type error.
func TestSubstitutedScalarStaysAString(t *testing.T) {
	t.Setenv("ACME_STORE_NAME", "8080")
	cfg, err := ParseBootstrap([]byte("store:\n  path: ${ACME_STORE_NAME}\n"), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != "8080" {
		t.Fatalf("path = %q", cfg.Store.Path)
	}
}

// A shadowed name is logged by NAME. A value must never reach a log line —
// that is the whole reason the warning exists rather than a diff.
func TestShadowedNameIsLoggedOnceByNameOnly(t *testing.T) {
	t.Setenv("SHADOWED", "env-credential-aaa")
	var buf strings.Builder
	r := WithStore(MapSource{"SHADOWED": "store-credential-bbb"})
	r.log = testLogger(&buf)

	r.Value("${SHADOWED}")
	r.Value("${SHADOWED}")

	out := buf.String()
	if strings.Count(out, "secret_shadowed_env") != 1 {
		t.Fatalf("want exactly one warning, got:\n%s", out)
	}
	if !strings.Contains(out, "SHADOWED") {
		t.Fatalf("the warning must name the variable:\n%s", out)
	}
	for _, value := range []string{"env-credential-aaa", "store-credential-bbb"} {
		if strings.Contains(out, value) {
			t.Fatalf("a credential value reached the log:\n%s", out)
		}
	}
}

// Agreeing sources are not a shadow: warning on every name the store and a
// correctly-synced .env both hold would bury the one that matters.
func TestAgreeingSourcesAreNotShadowed(t *testing.T) {
	t.Setenv("AGREES", "same")
	var buf strings.Builder
	r := WithStore(MapSource{"AGREES": "same"})
	r.log = testLogger(&buf)
	r.Value("${AGREES}")
	if strings.Contains(buf.String(), "secret_shadowed_env") {
		t.Fatalf("agreeing sources warned:\n%s", buf.String())
	}
}

func TestMapResolutionReportsPerKey(t *testing.T) {
	t.Setenv("KNOWN", "yes")
	out, missing := EnvOnly().Map("role.mcp_env.atlassian", map[string]string{
		"JIRA_TOKEN": "${KNOWN}",
		"JIRA_EMAIL": "${ACME_UNSET_EMAIL}",
	})
	if out["JIRA_TOKEN"] != "yes" || out["JIRA_EMAIL"] != "" {
		t.Fatalf("out = %v", out)
	}
	if len(missing) != 1 || missing[0].Path != "role.mcp_env.atlassian.JIRA_EMAIL" {
		t.Fatalf("missing = %#v", missing)
	}
}

// A setup step's env is left VERBATIM: it is resolved exactly once, with
// the rest of the sandbox env at launch. Resolving here too would
// double-resolve a secret whose real value contains a literal ${...}.
func TestSetupStepResolvesFilesAndCommandsOnly(t *testing.T) {
	t.Setenv("ACME_STEP_TOKEN", "tok")
	step := SandboxSetupStep{
		Name:     "git-auth",
		Files:    map[string]string{"/tmp/creds": "password=${ACME_STEP_TOKEN}"},
		Commands: []string{"echo ${ACME_STEP_TOKEN}"},
		Env:      map[string]string{"GITLAB_TOKEN": "${ACME_STEP_TOKEN}"},
		Brief:    "use ${ACME_STEP_TOKEN}",
	}
	out, missing := step.Resolve("providers.sandbox.setup[0]", EnvOnly())
	if len(missing) != 0 {
		t.Fatalf("missing = %#v", missing)
	}
	if out.Files["/tmp/creds"] != "password=tok" || out.Commands[0] != "echo tok" {
		t.Fatalf("files/commands not resolved: %+v", out)
	}
	if out.Env["GITLAB_TOKEN"] != "${ACME_STEP_TOKEN}" {
		t.Fatalf("env must stay verbatim, got %q", out.Env["GITLAB_TOKEN"])
	}
	if out.Brief != "use ${ACME_STEP_TOKEN}" {
		t.Fatalf("brief must stay verbatim, got %q", out.Brief)
	}
}

// The pre-launch presence check tests the REFERENCES, not the resolved
// value: an embedded "Bearer ${TOKEN}" with TOKEN unset resolves to a
// non-empty "Bearer ", so a caller checking the result would miss exactly
// the composite shapes config allows.
func TestResolvableTestsTheReferencesNotTheValue(t *testing.T) {
	t.Parallel()
	r := NewResolver(MapSource{"SET": "v", "BLANK": ""})
	cases := []struct {
		value string
		want  bool
	}{
		{"literal", true},
		{"${SET}", true},
		{"Bearer ${SET}", true},
		{"Bearer ${MISSING}", false},
		// An empty credential is not a credential, even though a source
		// answered for it — which is where this differs from Expand.
		{"${BLANK}", false},
		{"${SET}${MISSING}", false},
	}
	for _, tc := range cases {
		if got := r.Resolvable(tc.value); got != tc.want {
			t.Fatalf("Resolvable(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// A RESOLVED VALUE IS READ AS THE FIELD IT LANDS IN READS A LITERAL.
//
// Into a number or a switch, the resolved text is read exactly as the same
// characters written in the file would be — which is what "a reference works
// anywhere in Tier A" promised and what was not true: the value was a string
// everywhere, so no number field decoded one and a switch took only the YAML
// 1.1 words. Into TEXT it is the string, character for character, even when
// it spells a number or YAML's null — the value is data, never syntax.
func TestAReferenceIsReadAsTheFieldReadsALiteral(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap([]byte(`api:
  port: ${API_PORT}
stream:
  debug: "${STREAM_DEBUG}"
node:
  max_concurrent: ${MAX}
coordination:
  lease_ttl_seconds: "${LEASE}"
store:
  path: ${STORE_NAME}
logging:
  level: ${LEVEL}
`), NewResolver(MapSource{
		"API_PORT": "9090", "STREAM_DEBUG": "true", "MAX": "0x10",
		"LEASE": "12.5", "STORE_NAME": "null", "LEVEL": "debug",
	}))
	if err != nil {
		t.Fatalf("a Tier A file taking every kind of field from the environment was refused: %v", err)
	}
	if cfg.API.Port != 9090 {
		t.Errorf("api.port = %d, want 9090", cfg.API.Port)
	}
	if !cfg.Stream.Debug {
		t.Error("stream.debug is false; ${STREAM_DEBUG}=true is `debug: true`")
	}
	if cfg.Node.MaxConcurrent != 16 {
		t.Errorf("node.max_concurrent = %d, want 16 — 0x10 is read as the literal it spells", cfg.Node.MaxConcurrent)
	}
	if cfg.Coordination.LeaseTTLSeconds != 12.5 {
		t.Errorf("coordination.lease_ttl_seconds = %v, want 12.5", cfg.Coordination.LeaseTTLSeconds)
	}
	if cfg.Store.Path != "null" {
		t.Errorf("store.path = %q, want the text \"null\": a resolved value is data, "+
			"and reading it as YAML would have made it an absence", cfg.Store.Path)
	}
}

// A NUMBER OR A SWITCH READS WHAT A FILE CARRIED THE WAY A TEXT FIELD DOES: the
// space around it is not part of the value. A variable read out of a mounted
// secret, a .env line or a captured command routinely ends in a newline, every
// Tier A text field is trimmed of it (normalize.go), and `port: 8080` written
// in the file carries none — so a port read that way booted nothing while a
// host name read the same way booted fine. Space INSIDE the value is still the
// value, and still refused.
func TestATypedReferenceIsTrimmedAsTextIs(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap([]byte("api:\n  port: ${API_PORT}\nstream:\n  debug: ${STREAM_DEBUG}\n"+
		"node:\n  max_concurrent: ${MAX}\nstore:\n  path: ${STORE}\n"),
		NewResolver(MapSource{"API_PORT": "9090\n", "STREAM_DEBUG": " true\r\n", "MAX": "\t8 ",
			"STORE": "/var/lib/crewlet/company.db\n"}))
	if err != nil {
		t.Fatalf("values carrying the newline a file ends in were refused: %v", err)
	}
	if cfg.API.Port != 9090 || !cfg.Stream.Debug || cfg.Node.MaxConcurrent != 8 {
		t.Errorf("api.port = %d, stream.debug = %v, node.max_concurrent = %d; want 9090, true, 8",
			cfg.API.Port, cfg.Stream.Debug, cfg.Node.MaxConcurrent)
	}
	if cfg.Store.Path != "/var/lib/crewlet/company.db" {
		t.Errorf("store.path = %q: the text field beside them must trim the same way", cfg.Store.Path)
	}

	_, err = ParseBootstrap([]byte("api:\n  port: ${API_PORT}\n"), NewResolver(MapSource{"API_PORT": "80 80"}))
	if !errors.Is(err, ErrShape) {
		t.Fatalf("api.port from %q: err = %v, want %v — only the space AROUND a value goes", "80 80", err, ErrShape)
	}
}

// A NUMBER OR A SWITCH TAKES A WHOLE REFERENCE WITH A VALUE, or the file is
// refused naming the field.
//
// Text around a reference would make a number's type a property of how two
// strings concatenate, so it is refused rather than spliced. A reference that
// resolves to NOTHING is refused too: empty is how a file says "unset", and a
// port that silently fell back to its default because a variable was missing
// would boot a node on a port nobody chose. And a value that is not the
// field's kind — a word, a fraction, YAML syntax — is refused as the literal
// would be, never read as the syntax it contains.
func TestATypedFieldRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, yaml string
		env        MapSource
		kind       error
		path       string
	}{
		{"text around a number's reference", "api:\n  port: \"80${TAIL}\"\n",
			MapSource{"TAIL": "80"}, ErrShape, "api.port"},
		{"text around a switch's reference", "stream:\n  debug: \"y${TAIL}\"\n",
			MapSource{"TAIL": "es"}, ErrShape, "stream.debug"},
		{"a number's reference nothing answered", "api:\n  port: ${API_PORT}\n",
			MapSource{}, ErrMissing, "api.port"},
		{"a switch's reference set to nothing", "stream:\n  debug: ${STREAM_DEBUG}\n",
			MapSource{"STREAM_DEBUG": ""}, ErrMissing, "stream.debug"},
		{"a number that is a word", "api:\n  port: ${API_PORT}\n",
			MapSource{"API_PORT": "http"}, ErrShape, "api.port"},
		{"a number that is YAML syntax", "api:\n  port: ${API_PORT}\n",
			MapSource{"API_PORT": "[8080]"}, ErrShape, "api.port"},
		{"a number with a comment in it", "api:\n  port: ${API_PORT}\n",
			MapSource{"API_PORT": "8080 # the default"}, ErrShape, "api.port"},
		{"a whole number that is a fraction", "api:\n  port: ${API_PORT}\n",
			MapSource{"API_PORT": "8080.5"}, ErrShape, "api.port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseBootstrap([]byte(tc.yaml), NewResolver(tc.env))
			if !errors.Is(err, tc.kind) {
				t.Fatalf("err = %v, want %v", err, tc.kind)
			}
			problems := Problems(err)
			if len(problems) != 1 || problems[0].Path != tc.path {
				t.Fatalf("problems = %+v, want one at %s", problems, tc.path)
			}
		})
	}

	// AND TEXT STILL SPLICES: a text field takes a reference anywhere in
	// its value, which is what `edge-${ZONE}` as a node id needs.
	cfg, err := ParseBootstrap([]byte("node:\n  id: \"edge-${ZONE}\"\n"), NewResolver(MapSource{"ZONE": "eu"}))
	if err != nil || cfg.Node.ID != "edge-eu" {
		t.Fatalf("node.id = (%q, %v), want edge-eu", cfg.Node.ID, err)
	}
}

// THE WHOLE-REFERENCE PATTERN IS THE RESOLVER'S. The schema restates the rule a
// number or a switch is held to, so the two are held to one verdict over the
// shapes around it: surrounding text, surrounding space, two references, the
// names the grammar refuses.
func TestWholeReferencePatternIsTheResolvers(t *testing.T) {
	t.Parallel()
	schemaSays := regexp.MustCompile(wholeReferencePattern)
	for _, value := range []string{
		"${A}", "${a_b1}", "${_X}", "${A}${B}", "x${A}", "${A}x", " ${A}", "${A} ",
		"${A}\n", "${1}", "${}", "$A", "${A", "", "8080",
	} {
		if got, want := schemaSays.MatchString(value), wholeReference(value); got != want {
			t.Errorf("%q: the schema's pattern says %v, the resolver %v", value, got, want)
		}
	}
}
