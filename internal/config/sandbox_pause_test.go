package config_test

import (
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
