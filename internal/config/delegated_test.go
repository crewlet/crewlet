package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/delegated"
)

// delegatedDoc is an SSH company with an agent at operate (sre), a person
// (ops-lead), two agents that hold no SSH access on two models (security-a and
// security-b) and one more on the first one's model (security-c), under rules.
func delegatedDoc(t *testing.T, rules string) string {
	t.Helper()
	return "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n" +
		"  llm:\n    fast:\n      model: sonnet\n    other:\n      model: opus\n" +
		"integrations:\n  ssh:\n" + sshBlock(t, "") + "\nroles:\n" +
		"  - {name: SRE, handle: sre, integrations: {ssh: {enabled: true, level: operate, hosts: [api-1]}}}\n" +
		"  - {name: Ops Lead, handle: ops-lead, kind: human}\n" +
		"  - {name: Security A, handle: security-a, llm: fast}\n" +
		"  - {name: Security B, handle: security-b, llm: other}\n" +
		"  - {name: Security C, handle: security-c, llm: fast}\n" +
		"delegated_decisions:\n  enabled: true\n  rules:\n" + rules
}

// A delegate is an agent seat that cannot request what it decides: a person
// is refused, and so is an agent holding SSH access for ssh_operate, and so is
// a kind that is never delegated or one two rules claim.
//
// Mutation: drop the requester check, and the seat that makes operate calls
// approves its own.
func TestADelegateIsAnAgentThatCannotRequestWhatItDecides(t *testing.T) {
	t.Parallel()
	good := "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead]}\n"
	if _, err := config.ParseCompany([]byte(delegatedDoc(t, good))); err != nil {
		t.Fatalf("a rule naming an agent with no SSH access was refused: %v", err)
	}
	for _, c := range []struct{ name, rules, path, kind string }{
		{"a person delegate", "    - {kinds: [ssh_operate], delegates: [ops-lead], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].delegates[0]", "unknown_value"},
		{"a delegate that operates", "    - {kinds: [ssh_operate], delegates: [sre], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].delegates[0]", "conflict"},
		{"an unknown seat", "    - {kinds: [ssh_operate], delegates: [nobody], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].delegates[0]", "unknown_value"},
		{"other is never delegated", "    - {kinds: [other], delegates: [security-a], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].kinds[0]", "unknown_value"},
		{"a kind in two rules", good + "    - {kinds: [ssh_operate], delegates: [security-b], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[1].kinds[0]", "conflict"},
		{"three delegates", "    - {kinds: [ssh_operate], delegates: [security-a, security-b, security-c], " +
			"escalate_to: [ops-lead]}\n", "delegated_decisions.rules[0].delegates", "out_of_range"},
		{"a sample rate past its bound", "    - {kinds: [ssh_operate], delegates: [security-a], " +
			"escalate_to: [ops-lead], sample_one_in: 5000}\n", "delegated_decisions.rules[0].sample_one_in", "out_of_range"},
	} {
		_, err := config.ParseCompany([]byte(delegatedDoc(t, c.rules)))
		if p, ok := problemAt(err, c.path); !ok || p.Kind != c.kind {
			t.Errorf("%s: %v, want %s at %s", c.name, err, c.kind, c.path)
		}
	}
}

// A panel's two delegates run on two different first llm entries, so one
// model's blind spot is not both: the same model, or a seat naming none, is
// refused.
//
// Mutation: drop the model comparison, and a panel of one model twice reads
// as two opinions.
func TestAPanelRunsOnTwoModels(t *testing.T) {
	t.Parallel()
	two := "    - {kinds: [ssh_operate], delegates: [security-a, security-b], escalate_to: [ops-lead]}\n"
	if _, err := config.ParseCompany([]byte(delegatedDoc(t, two))); err != nil {
		t.Fatalf("a panel on two models was refused: %v", err)
	}
	same := "    - {kinds: [ssh_operate], delegates: [security-a, security-c], escalate_to: [ops-lead]}\n"
	_, err := config.ParseCompany([]byte(delegatedDoc(t, same)))
	if p, ok := problemAt(err, "delegated_decisions.rules[0].delegates"); !ok || !strings.Contains(p.Message, "different") {
		t.Errorf("a panel of one model twice: %v, want a conflict saying the models must differ", err)
	}
}

// A rule names at least one person to escalate to, and only people.
//
// Mutation: accept an empty escalate_to, and a company whose administrators
// leave has requests nobody is shown.
func TestAnEscalationNamesAPerson(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, rules, path, kind string }{
		{"no one", "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: []}\n",
			"delegated_decisions.rules[0].escalate_to", "missing"},
		{"an agent", "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [security-b]}\n",
			"delegated_decisions.rules[0].escalate_to[0]", "unknown_value"},
	} {
		_, err := config.ParseCompany([]byte(delegatedDoc(t, c.rules)))
		if p, ok := problemAt(err, c.path); !ok || p.Kind != c.kind {
			t.Errorf("%s: %v, want %s at %s", c.name, err, c.kind, c.path)
		}
	}
	// AND WHO THEY ARE MAY APPROVE: the rule's escalate_to joins the
	// approvers for its kind.
	cfg, err := config.ParseCompany([]byte(delegatedDoc(t,
		"    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead]}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.SSHMayApprove(); len(got) != 1 || got[0] != "ops-lead" {
		t.Errorf("the people who may approve an operate command are %v, want [ops-lead]", got)
	}
	if delegates, n, ok := cfg.DelegatedDecisions.RuleFor(delegated.KindSSHOperate); !ok || n != 1 ||
		len(delegates.Delegates) != 1 {
		t.Errorf("the rule for ssh_operate is %+v, %d, %v", delegates, n, ok)
	}
	cfg.DelegatedDecisions.Enabled = false
	if _, _, ok := cfg.DelegatedDecisions.RuleFor(delegated.KindSSHOperate); ok {
		t.Errorf("a disabled block still delegates ssh_operate")
	}
}
