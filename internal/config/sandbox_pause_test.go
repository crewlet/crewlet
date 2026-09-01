package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// Three states, and a plain number has two. A seat that says nothing about the
// knob must inherit the provider default — reading its zero value as "never
// pause" would tear the box down the moment a coding agent asked a question,
// losing the checkout the answer was going to resume.
func TestASeatThatSaysNothingAboutPausingInheritsTheDefault(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      enabled: true
`)
	if c.PauseTTLSeconds != nil {
		t.Fatalf("pause_ttl_seconds = %v, want unset so the provider default applies", *c.PauseTTLSeconds)
	}
}

// An explicit zero is a real instruction: never hold a blocked box.
func TestAnExplicitZeroPauseIsNotTheSameAsSayingNothing(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      enabled: true
      pause_ttl_seconds: 0
`)
	if c.PauseTTLSeconds == nil {
		t.Fatal("an explicit 0 was read as unset")
	}
	if *c.PauseTTLSeconds != 0 {
		t.Fatalf("pause_ttl_seconds = %v, want 0", *c.PauseTTLSeconds)
	}
}

// -1 is how the field's earlier form spelled "inherit". A document that says so
// is asking for what leaving it out asks for, and failing a config over a
// spelling helps nobody.
func TestTheEarlierSpellingOfInheritPauseIsStillAccepted(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      enabled: true
      pause_ttl_seconds: -1
`)
	if c.PauseTTLSeconds == nil || *c.PauseTTLSeconds != -1 {
		t.Fatalf("pause_ttl_seconds = %v, want it accepted verbatim", c.PauseTTLSeconds)
	}
}

func parseSandboxSeat(t *testing.T, roleBlock string) *config.RoleSandbox {
	t.Helper()
	doc := `
name: Nimbus
providers:
  llm:
    fast:
      type: anthropic
      model: claude-golden
  sandbox:
    type: local
    local:
      containment: direct
roles:
  - name: SWE
    llm: fast` + roleBlock
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Roles[0].Sandbox == nil {
		t.Fatal("the seat has no sandbox block")
	}
	return cfg.Roles[0].Sandbox
}

// The round cap has the same three states as the pause TTL above, and needs a
// pointer for the same reason: without one, the seat whose work legitimately
// runs long could not escape a cap set for everybody else, because its "no
// cap" and its "say nothing" would both be zero.
func TestASeatThatSaysNothingAboutRoundsInheritsTheDefault(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      enabled: true
`)
	if c.MaxTurns != nil {
		t.Fatalf("max_turns = %v, want unset so the provider default applies", *c.MaxTurns)
	}
}

// An explicit zero is how a seat opts OUT of a company-wide cap.
func TestAnExplicitZeroRoundCapIsNotTheSameAsSayingNothing(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      enabled: true
      max_turns: 0
`)
	if c.MaxTurns == nil {
		t.Fatal("an explicit 0 was read as unset, so the seat cannot escape a company cap")
	}
	if *c.MaxTurns != 0 {
		t.Fatalf("max_turns = %v, want 0", *c.MaxTurns)
	}
}

// A negative cap is refused rather than clamped: unlike pause_ttl_seconds it
// has no earlier spelling to be compatible with, so a negative here is a
// mistake and saying so is more use than silently reading it as uncapped.
func TestANegativeRoundCapIsRefused(t *testing.T) {
	_, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: SWE
    sandbox:
      enabled: true
      max_turns: -2
`))
	if err == nil {
		t.Fatal("a negative round cap was accepted")
	}
	if !strings.Contains(err.Error(), "max_turns") {
		t.Fatalf("the error does not name the field: %v", err)
	}
}
