package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A BATCH BUDGET BELOW ONE CHILD'S IS REFUSED.
//
// batchCtx is the PARENT of every childCtx, and at most subagent_max_parallel
// run at once — so with the batch cap under one child's, every child past the
// first wave returns neverStarted before it calls a model. The field's own
// doc ("stop twenty of them each finishing just inside their own cap") is
// only meaningful if the batch budget exceeds one child's.
func TestABatchTimeoutBelowOneChildsIsRefused(t *testing.T) {
	t.Parallel()
	err := validateTurnEngine(t, "    subagent_timeout_seconds: 120\n    subagent_batch_timeout_seconds: 60")
	if err == nil {
		t.Fatal("a batch cap below one child's cap was accepted")
	}
	if !strings.Contains(err.Error(), "subagent_batch_timeout_seconds") {
		t.Errorf("error %q does not name the field", err)
	}
}

// EQUAL IS LEGAL and means "one wave only", the same escape hatch equal
// ceilings give — so the rule is a contradiction check rather than a
// requirement that the batch always be larger.
func TestAnEqualBatchTimeoutIsAllowed(t *testing.T) {
	t.Parallel()
	if err := validateTurnEngine(t,
		"    subagent_timeout_seconds: 120\n    subagent_batch_timeout_seconds: 120"); err != nil {
		t.Errorf("equal caps were refused: %v", err)
	}
}

// And the shipped defaults satisfy their own rule, so nobody discovers it
// from a validation failure on an untouched config.
func TestTheShippedTurnEngineDefaultsSatisfyTheBatchRule(t *testing.T) {
	t.Parallel()
	if err := validateTurnEngine(t, ""); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
}

func validateTurnEngine(t *testing.T, body string) error {
	t.Helper()
	doc := `
name: Acme
providers:
  llm:
    fast:
      type: anthropic
      model: claude-golden
turn_engine:
` + body + `
roles:
  - name: SWE
    llm: fast
`
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		return err
	}
	return c.Validate()
}
