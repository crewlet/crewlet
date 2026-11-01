package config_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// Three states, and a plain number has two. A seat that says nothing about the
// knob must inherit the provider default — reading its zero value as "never
// pause" would tear the box down the moment its run asked a question, losing
// the checkout the answer was going to resume.
func TestASeatThatSaysNothingAboutPausingInheritsTheDefault(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      env: {GITHUB_TOKEN: "${GITHUB_TOKEN}"}
`)
	if c.PauseTTLSeconds != nil {
		t.Fatalf("pause_ttl_seconds = %v, want unset so the provider default applies", *c.PauseTTLSeconds)
	}
}

// An explicit zero is a real instruction: never hold a blocked box.
func TestAnExplicitZeroPauseIsNotTheSameAsSayingNothing(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      pause_ttl_seconds: 0
`)
	if c.PauseTTLSeconds == nil {
		t.Fatal("an explicit 0 was read as unset")
	}
	if *c.PauseTTLSeconds != 0 {
		t.Fatalf("pause_ttl_seconds = %v, want 0", *c.PauseTTLSeconds)
	}
}

// A negative pause is refused, naming the field: it cannot mean a duration,
// "no expiry" is the snapshot leak the knob exists to prevent, and inherit is
// said by leaving the field out.
func TestANegativePauseIsRefused(t *testing.T) {
	_, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: SWE
    sandbox:
      pause_ttl_seconds: -1
`))
	if !errors.Is(err, config.ErrOutOfRange) {
		t.Fatalf("a negative pause = %v, want %v", err, config.ErrOutOfRange)
	}
	if !strings.Contains(err.Error(), "roles[0].sandbox.pause_ttl_seconds") {
		t.Fatalf("the error does not name the field: %v", err)
	}
}

func parseSandboxSeat(t *testing.T, roleBlock string) *config.RoleSandbox {
	t.Helper()
	doc := `
name: Nimbus
providers:
  llm:
    fast:
      model: claude-golden
  sandbox:
    local: {}
    default_run_in: direct
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
      env: {GITHUB_TOKEN: "${GITHUB_TOKEN}"}
`)
	if c.MaxTurns != nil {
		t.Fatalf("max_turns = %v, want unset so the provider default applies", *c.MaxTurns)
	}
}

// An explicit zero is how a seat opts OUT of a company-wide cap.
func TestAnExplicitZeroRoundCapIsNotTheSameAsSayingNothing(t *testing.T) {
	c := parseSandboxSeat(t, `
    sandbox:
      max_turns: 0
`)
	if c.MaxTurns == nil {
		t.Fatal("an explicit 0 was read as unset, so the seat cannot escape a company cap")
	}
	if *c.MaxTurns != 0 {
		t.Fatalf("max_turns = %v, want 0", *c.MaxTurns)
	}
}

// A negative cap is refused rather than clamped, as a negative pause is: a
// negative here is a mistake, and saying so is more use than silently reading
// it as uncapped.
func TestANegativeRoundCapIsRefused(t *testing.T) {
	_, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: SWE
    sandbox:
      max_turns: -2
`))
	if err == nil {
		t.Fatal("a negative round cap was accepted")
	}
	if !strings.Contains(err.Error(), "max_turns") {
		t.Fatalf("the error does not name the field: %v", err)
	}
}

// THE COMPANY-WIDE ROUND CAP IS ON UNLESS IT IS SAID TO BE OFF, and saying so
// survives the store.
//
// It is the only bound on a runaway executor run, so a company that says
// nothing runs under the shipped cap rather than under none, and an explicit
// 0 — uncapped — is a setting rather than an absence: a stored revision is
// decoded onto the defaults, so a zero the encoder dropped would come back as
// the cap the company turned off.
func TestTheCompanyRoundCapIsOnUnlessItIsSaidToBeOff(t *testing.T) {
	const catalogue = `
name: Nimbus
providers:
  sandbox:
    fake: true
`
	silent, err := config.ParseCompany([]byte(catalogue))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := silent.Providers.Sandbox.MaxTurns(); got != 96 {
		t.Errorf("a catalogue that says nothing caps a run at %d, want the shipped 96", got)
	}
	var none *config.SandboxProvider
	if got := none.MaxTurns(); got != 96 {
		t.Errorf("no catalogue at all reads a cap of %d, want the shipped 96", got)
	}

	off, err := config.ParseCompany([]byte(catalogue + "    default_max_turns: 0\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stored, err := json.Marshal(off)
	if err != nil {
		t.Fatal(err)
	}
	back, err := config.DecodeCompany(stored)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*config.Company{"authored": off, "stored": back} {
		if got := c.Providers.Sandbox.MaxTurns(); got != 0 {
			t.Errorf("%s: an explicit 0 caps a run at %d, want uncapped", name, got)
		}
	}
}

// A RUN'S COMMITS CREDIT CREWLET UNLESS THE COMPANY SAYS THEY DO NOT, and
// saying so survives the store.
//
// The credit is on by default, so a company that says nothing has its runs'
// commits credited, and a catalogue that is not there at all reads the same
// (it runs no box, so nothing reads it). An explicit false is a setting rather
// than an absence: a stored revision is decoded onto the defaults, so a false
// the encoder dropped would come back as the credit the company turned off.
//
// Mutation: read the field as a plain bool, or drop the pointer's false on
// encoding, and this goes red.
func TestARunsCommitsCreditCrewletUnlessTheCompanySaysNot(t *testing.T) {
	const catalogue = `
name: Nimbus
providers:
  sandbox:
    fake: true
`
	silent, err := config.ParseCompany([]byte(catalogue))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !silent.Providers.Sandbox.AttributesCommits() {
		t.Error("a catalogue that says nothing credits no commit, want the credit on")
	}
	var none *config.SandboxProvider
	if !none.AttributesCommits() {
		t.Error("no catalogue at all reads the credit off, want it on")
	}

	for value, want := range map[string]bool{"true": true, "false": false} {
		said, err := config.ParseCompany([]byte(catalogue + "    commit_attribution: " + value + "\n"))
		if err != nil {
			t.Fatalf("parse commit_attribution: %s: %v", value, err)
		}
		stored, err := json.Marshal(said)
		if err != nil {
			t.Fatal(err)
		}
		back, err := config.DecodeCompany(stored)
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]*config.Company{"authored": said, "stored": back} {
			if got := c.Providers.Sandbox.AttributesCommits(); got != want {
				t.Errorf("%s with commit_attribution: %s credits commits: %v, want %v", name, value, got, want)
			}
		}
	}
}
