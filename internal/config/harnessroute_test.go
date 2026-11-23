package config

import (
	"strings"
	"testing"
)

// A ROUTE VARIABLE WHOSE VALUE IS NO ROUTE IS REFUSED, NAMING IT AND NEVER ITS
// VALUE. An entry's identity as a session's entry is built from the variables
// its profile declares a route, each reduced to its host
// (harnessprofile.RouteValue); a literal value that parses as no address and
// no plain name would be taken whole, and may hold a credential. A ${VAR} is
// held to the same rule once resolved, when the entry is built.
//
// Mutation: admit the value, report it in the problem, or judge a ${VAR} before
// it resolves, and a row goes red.
func TestARouteVariableWhoseValueIsNoRouteIsRefused(t *testing.T) {
	t.Parallel()
	for value, refused := range map[string]bool{
		`"user:planted-1@gateway.example.com"`:            true,
		`"https://user:p#planted-2@gateway.example.com"`:  true,
		`"https://user:planted-3@gateway.example.com/v1"`: false,
		`"https://gateway.example.com/v1"`:                false,
		`"${GATEWAY_URL}"`:                                false,
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
		case refused && (found == nil || found.Kind != "shape" || !strings.Contains(found.Message, "no route")):
			t.Errorf("ANTHROPIC_BASE_URL %s: problem %+v, want it refused as no route", value, found)
		case refused && strings.Contains(found.Message, "planted"):
			t.Errorf("the refusal echoes the value: %s", found.Message)
		case !refused && found != nil:
			t.Errorf("ANTHROPIC_BASE_URL %s refused: %v", value, found)
		}
	}
}
