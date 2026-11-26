package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A ROUND CAP IS AT MOST ITS CEILING, a seat's and the company's default
// alike: the ceiling itself and 0 (uncapped) load, one past it is refused
// naming the field, the ceiling and the two ways out, splitting the work or
// lifting the cap. A capped run's record of its requests stays in its box
// (RoundCapCeiling says why), so a cap past it would bound that record by
// nothing an operator chose.
//
// Mutation: drop either refusal, or compare with >= instead of >, and a row
// goes red.
func TestARoundCapIsAtMostItsCeiling(t *testing.T) {
	t.Parallel()
	company := func(n int) string {
		return fmt.Sprintf("name: Acme\nproviders:\n  sandbox:\n    fake: true\n    default_max_turns: %d\n", n)
	}
	seat := func(n int) string {
		return fmt.Sprintf("name: Acme\nproviders:\n  sandbox:\n    fake: true\nroles:\n  - name: SWE\n"+
			"    sandbox:\n      max_turns: %d\n", n)
	}
	for _, c := range []struct {
		name string
		doc  func(int) string
		path string
	}{{"the company's default", company, "providers.sandbox.default_max_turns"},
		{"a seat's cap", seat, "roles[0].sandbox.max_turns"}} {
		for _, n := range []int{0, RoundCapCeiling} {
			if _, err := ParseCompany([]byte(c.doc(n))); err != nil {
				t.Errorf("%s of %d is refused: %v", c.name, n, err)
			}
		}
		err := rejects(t, c.doc(RoundCapCeiling+1), c.path)
		if !errors.Is(err, ErrOutOfRange) {
			t.Errorf("%s past the ceiling is refused as %v, want out of range", c.name, err)
		}
		for _, want := range []string{fmt.Sprintf("at most %d", RoundCapCeiling), "split the work",
			"set 0 for uncapped"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s past the ceiling is refused without %q: %v", c.name, want, err)
			}
		}
	}
}

// THE ROUND CAPS' SCHEMA STATES THEIR CEILING. The js tags are literals
// beside RoundCapCeiling that nothing else compares, so a ceiling moved
// without its tags leaves an editor accepting a cap the engine refuses at
// load.
//
// Mutation: leave either tag's max at another value, or drop it, and its arm
// goes red.
func TestTheRoundCapsSchemaStatesTheirCeiling(t *testing.T) {
	t.Parallel()
	defs, _ := schemaDoc(t, TierCompany)["$defs"].(map[string]any)
	for def, field := range map[string]string{"SandboxProvider": "default_max_turns", "RoleSandbox": "max_turns"} {
		d, _ := defs[def].(map[string]any)
		props, _ := d["properties"].(map[string]any)
		prop, _ := props[field].(map[string]any)
		if got, ok := prop["maximum"].(float64); !ok || int(got) != RoundCapCeiling {
			t.Errorf("%s.%s's schema maximum is %v, the validator's ceiling %d", def, field, prop["maximum"],
				RoundCapCeiling)
		}
	}
}
