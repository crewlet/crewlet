package config

import (
	"strings"
	"testing"
)

// A ROUTE VARIABLE WHOSE VALUE IS NO ROUTE IS REFUSED, NAMING IT AND NEVER ITS
// VALUE. An entry's identity as a session's entry is built from the variables
// its profile declares a route, each cleaned to its route
// (harnessprofile.Profile.RouteValue); a literal value that parses as no
// address and no plain name, or whose path holds a key, would put what may be
// a credential in it. A ${VAR} is held to the same rule once resolved, when
// the entry is built.
//
// Mutation: admit the value, report it in the problem, or judge a ${VAR} before
// it resolves, and a row goes red.
func TestARouteVariableWhoseValueIsNoRouteIsRefused(t *testing.T) {
	t.Parallel()
	for value, refused := range map[string]bool{
		`"user:planted-1@gateway.example.com"`:                                     true,
		`"https://user:p#planted-2@gateway.example.com"`:                           true,
		`"https://gateway.example.com/sk-planted4plantedplantedplantedplanted/v1"`: true,
		`"gateway.example.com/sk-planted-5/v1"`:                                    true,
		`"https://user:planted-3@gateway.example.com/v1"`:                          false,
		`"https://gateway.example.com/v1"`:                                         false,
		`"${GATEWAY_URL}"`:                                                         false,
	} {
		doc := harnessEntry("harness: claude-code", "env: {ANTHROPIC_BASE_URL: "+value+"}")
		cfg, err := ParseCompanyDocument([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		var found *Problem
		for _, p := range Problems(cfg.Validate()) {
			if p.Path == "providers.llm.sub.env.ANTHROPIC_BASE_URL" {
				found = &p
			}
		}
		switch {
		case refused && (found == nil || found.Kind != "shape"):
			t.Errorf("ANTHROPIC_BASE_URL %s: problem %+v, want it refused", value, found)
		case refused && strings.Contains(found.Message, "planted"):
			t.Errorf("the refusal echoes the value: %s", found.Message)
		case !refused && found != nil:
			t.Errorf("ANTHROPIC_BASE_URL %s refused: %v", value, found)
		}
	}
}

// AN IDENTITY VARIABLE THAT IS A CREDENTIAL IS REFUSED, overrides included,
// naming the entry and the variable: an entry's identity is built from its
// profile's identity_env, so a credential there would put the key in it. A
// credential is what the profile signs in by (token_env, api_key_env, a
// credential of sign_in_env, any credential-named variable under env_sign_in),
// so an OAuth host stays an identity variable where only listed names sign in.
//
// Mutation: drop the refusal, judge identity_env by its names' words, or skip
// env_sign_in, and a row goes red.
func TestAnIdentityVariableThatIsACredentialIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		harness, name string
		refused       bool
	}{
		{"claude-code", "ANTHROPIC_AUTH_TOKEN", true},
		{"claude-code", "ANTHROPIC_API_KEY", true},
		{"claude-code", "KIMI_OAUTH_HOST", false},
		{"opencode", "OPENROUTER_API_KEY", true},
		{"opencode", "OPENROUTER_REGION", false},
	} {
		doc := harnessEntry("harness: "+tc.harness, "overrides: {identity_env: ["+tc.name+"]}")
		cfg, err := ParseCompanyDocument([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		var found *Problem
		for _, p := range Problems(cfg.Validate()) {
			if strings.HasPrefix(p.Path, "providers.llm.sub.overrides") && strings.Contains(p.Message, tc.name) {
				found = &p
			}
		}
		switch {
		case tc.refused && found == nil:
			t.Errorf("%s overrides.identity_env [%s] was admitted, want it refused naming it", tc.harness, tc.name)
		case !tc.refused && found != nil:
			t.Errorf("%s overrides.identity_env [%s] was refused: %v", tc.harness, tc.name, found)
		}
	}
}
