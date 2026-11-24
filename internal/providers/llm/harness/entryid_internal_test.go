package harness

import (
	"bytes"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/secrets"
)

// AN ENTRY'S ID MOVES WITH WHERE ITS CALLS GO, AND WITH NOTHING ELSE OF ITS
// ENV. A carried session continues only under the entry it ran under, so the
// id tells apart two entries of one harness, and one entry whose auth mode,
// route (a gateway's path included), cloud switch or model's provider moved.
// A credential under any name (a header, a URL's userinfo or query, an oddly
// named key), a tuning variable and one address spelled two ways leave it as
// it was. Keyed by the fleet's sign-in key, so it is no plain digest.
//
// Mutation: drop the key, the auth mode, the route variables or OpenCode's model
// provider, keep a URL's userinfo, query, default port or trailing slash, drop
// its path or port, digest any other variable, or digest unkeyed: a row goes red.
func TestAnEntrysIDMovesWithWhereItsCallsGoAndWithNothingElseOfItsEnv(t *testing.T) {
	t.Parallel()
	claude, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	opencode, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	fleet := testFleet("a")
	gateway := map[string]string{"ANTHROPIC_BASE_URL": "https://gateway-a.example.com/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-ant-placeholder-a"}
	under := func(plans *Plans, key string, mode AuthMode, edit func(map[string]string)) *Provider {
		env := maps.Clone(gateway)
		if edit != nil {
			edit(env)
		}
		routes, err := readRoutes(key, claude, env)
		if err != nil {
			t.Fatalf("readRoutes: %v", err)
		}
		return &Provider{key: key, harness: "claude-code", model: "claude-sonnet-5", profile: claude,
			auth: Auth{Mode: mode, APIKey: "sk-ant-placeholder-key"}, env: env, routes: routes, plans: plans}
	}
	entry := func(key string, mode AuthMode, edit func(map[string]string)) *Provider {
		return under(fleet, key, mode, edit)
	}
	const cloudflare = "https://gateway.ai.cloudflare.com/v1/"
	id := entry("primary", AuthEnv, nil).EntryID()
	if len(id) != entryIDChars {
		t.Fatalf("EntryID = %q, want %d hex characters", id, entryIDChars)
	}
	set := func(name, value string) func(map[string]string) {
		return func(env map[string]string) { env[name] = value }
	}
	for name, tc := range map[string]struct {
		p    *Provider
		same bool
	}{
		"the same entry built again":        {entry("primary", AuthEnv, nil), true},
		"its gateway token replaced":        {entry("primary", AuthEnv, set("ANTHROPIC_AUTH_TOKEN", "sk-ant-b")), true},
		"a bearer in its custom headers":    {entry("primary", AuthEnv, set("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer planted-1")), true},
		"a bearer in its telemetry headers": {entry("primary", AuthEnv, set("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer planted-2")), true},
		"its gateway's address with userinfo": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://user:planted-3@gateway-a.example.com/v1")), true},
		"its gateway's address with a key in its query": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-a.example.com/v1?key=planted-4")), true},
		"its gateway's address with a trailing slash": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-a.example.com/v1/")), true},
		"its gateway's address with its default port": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-a.example.com:443/v1")), true},
		"an oddly named gateway credential": {entry("primary", AuthEnv, set("GW_SESAME", "planted-5")), true},
		"a tuning variable":                 {entry("primary", AuthEnv, set("MAX_THINKING_TOKENS", "8000")), true},
		"another entry on the same route":   {entry("secondary", AuthEnv, nil), false},
		"its gateway moved": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-b.example.com/v1")), false},
		"its gateway's port moved": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-a.example.com:8443/v1")), false},
		"its gateway's path moved": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-a.example.com/v2")), false},
		"the same entry under another fleet's key": {under(testFleet("b"),
			"primary", AuthEnv, nil), false},
		"its region moved":        {entry("primary", AuthEnv, set("AWS_REGION", "eu-west-1")), false},
		"its cloud switch thrown": {entry("primary", AuthEnv, set("CLAUDE_CODE_USE_BEDROCK", "1")), false},
		"its auth mode moved":     {entry("primary", AuthAPIKey, nil), false},
	} {
		if got := tc.p.EntryID(); (got == id) != tc.same {
			t.Errorf("%s: EntryID = %q beside %q, want the same: %v", name, got, id, tc.same)
		}
	}
	// AN AWS PROFILE IS NAMED CASE-SENSITIVELY: Prod and prod are two.
	if entry("primary", AuthEnv, set("AWS_PROFILE", "Prod")).EntryID() ==
		entry("primary", AuthEnv, set("AWS_PROFILE", "prod")).EntryID() {
		t.Error("two AWS profiles differing in case share an id")
	}
	// A GATEWAY ROUTED BY ITS PATH: one account's gateway is not another's.
	acct1 := entry("primary", AuthEnv, set("ANTHROPIC_BASE_URL", cloudflare+"0123456789abcdef0123456789abcdef/gw/anthropic"))
	acct2 := entry("primary", AuthEnv, set("ANTHROPIC_BASE_URL", cloudflare+"fedcba9876543210fedcba9876543210/gw/anthropic"))
	if acct1.EntryID() == acct2.EntryID() {
		t.Error("two accounts' gateways on one host share an id")
	}
	// THE PROVIDER OPENCODE'S MODEL NAMES is its route; the model past it is not.
	oc := func(model string) *Provider {
		return &Provider{key: "oc", harness: "opencode", model: model, profile: opencode, plans: fleet}
	}
	if oc("anthropic/claude-sonnet-5").EntryID() == oc("openrouter/anthropic/claude-sonnet-5").EntryID() {
		t.Error("an OpenCode entry whose model names another provider keeps its id")
	}
	if oc("openrouter/openai/gpt-6").EntryID() != oc("openrouter/anthropic/claude-sonnet-5").EntryID() {
		t.Error("an OpenCode entry whose model moved within its provider took a new id")
	}
}

// AN ENTRY WHOSE ROUTE IS NO ROUTE IS REFUSED, NAMING THE VARIABLE AND NEVER
// ITS VALUE: built from a resolved env, a route variable's value that parses
// as no address and no plain name, or whose path holds a key, would put what
// may be a credential in the entry's id.
//
// Mutation: build the entry anyway, or put the value in the error, and a row
// goes red.
func TestAnEntryWhoseRouteIsNoRouteIsRefusedWithoutItsValue(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"user:planted-1@gateway.example.com",
		"https://user:p#planted-2@gateway.example.com", "https://gateway.example.com/sk-planted3plantedplantedplantedplanted/v1"} {
		_, err := New(Config{Key: "gw", Harness: "claude-code", Model: "claude-sonnet-5", StateDir: t.TempDir(),
			Timeout: time.Minute, Env: map[string]string{"ANTHROPIC_BASE_URL": value}})
		if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") || strings.Contains(err.Error(), "planted") {
			t.Errorf("an entry whose gateway is %q built with %v, want refused naming the variable alone", value, err)
		}
	}
	if _, err := New(Config{Key: "gw", Harness: "claude-code", Model: "claude-sonnet-5", StateDir: t.TempDir(),
		Timeout: time.Minute, Env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example.com/v1"}}); err != nil {
		t.Errorf("an entry whose gateway is an address was refused: %v", err)
	}
}

// AN ENTRY'S ID IS WHAT ITS ENV WAS WHEN IT WAS BUILT: [New] reads its routes
// once, so the id never reads a value again, nor one the caller's map was
// given after.
//
// Mutation: read the env in EntryID, and the id moves.
func TestAnEntrysIDIsReadOnceWhenItIsBuilt(t *testing.T) {
	t.Parallel()
	env := map[string]string{"ANTHROPIC_BASE_URL": "https://gateway-a.example.com/v1"}
	p, err := New(Config{Key: "gw", Harness: "claude-code", Model: "claude-sonnet-5", StateDir: t.TempDir(),
		Timeout: time.Minute, Env: env, Plans: testFleet("a")})
	if err != nil {
		t.Fatal(err)
	}
	id := p.EntryID()
	if id == "" {
		t.Fatal("an entry under the fleet's key has no id")
	}
	env["ANTHROPIC_BASE_URL"] = "https://gateway-b.example.com/v1"
	if got := p.EntryID(); got != id {
		t.Errorf("an entry's id moved with its env after it was built: %q, then %q", id, got)
	}
}

// AN ENTRY UNDER A KEY DRAWN FOR ITS PROCESS HAS NO ID, so nothing it records
// is matched: such a key (nil Plans, the doctor's) is no other node's or
// restart's, and an id under it would refuse every session another one ran.
//
// Mutation: compute the id under a drawn key, and it is no longer empty.
func TestAnEntryUnderADrawnKeyHasNoID(t *testing.T) {
	t.Parallel()
	p, err := New(Config{Key: "gw", Harness: "claude-code", Model: "claude-sonnet-5", StateDir: t.TempDir(),
		Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if id := p.EntryID(); id != "" {
		t.Errorf("an entry under a drawn key has the id %q, want none", id)
	}
}

// testFleet is a fleet's registry whose keys stand derived from seed.
func testFleet(seed string) *Plans {
	return FleetPlans(func(info string) []byte { return []byte(seed + "/" + info) })
}

// THE ENTRY KEY IS NOT THE SIGN-IN KEY: each is derived from the keyring under
// an info of its own, so neither id is computed under the other's key; and a
// registry made for sign-ins alone names no entry.
//
// Mutation: derive the entry key under SignInKeyInfo, give NewPlans an entry
// key, or compute the id under the sign-in key, and a row goes red.
func TestTheEntryKeyIsNotTheSignInKey(t *testing.T) {
	t.Parallel()
	ring := secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}}
	plans := FleetPlans(ring.Derive)
	if len(plans.entryKey) == 0 || len(plans.key) == 0 || bytes.Equal(plans.entryKey, plans.key) {
		t.Errorf("the fleet's entry key and sign-in key are not two keys: %x, %x", plans.entryKey, plans.key)
	}
	if NewPlans([]byte("a fleet key")).entryKey != nil {
		t.Error("a registry made for sign-ins alone has an entry key")
	}
	// THE ID IS THE ENTRY KEY'S: the sign-in key moves nothing, the entry key
	// moves it.
	under := func(signIn, entry string) string {
		plans := &Plans{key: []byte(signIn), entryKey: []byte(entry), plans: map[SignInID]*Plan{}}
		return (&Provider{key: "primary", harness: "claude-code", plans: plans}).EntryID()
	}
	if under("sign-in a", "entry a") != under("sign-in b", "entry a") {
		t.Error("two registries sharing an entry key give one entry two ids")
	}
	if under("sign-in a", "entry a") == under("sign-in a", "entry b") {
		t.Error("two registries differing in their entry key give one entry one id")
	}
}
