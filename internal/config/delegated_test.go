package config_test

import (
	"strings"
	"testing"
	"time"

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
		_, parseErr := config.ParseCompany([]byte(delegatedDoc(t, c.rules)))
		if p, ok := problemAt(parseErr, c.path); !ok || p.Kind != c.kind {
			t.Errorf("%s: %v, want %s at %s", c.name, parseErr, c.kind, c.path)
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

// A merge and a deploy are delegated like any kind, to any agent seat (any
// agent may ask for one, so a delegate's own request is the engine's to send
// to people), and a rule's own always-escalate paths are path globs held to a
// rule that delegates one of them.
//
// Mutation: accept paths on an ssh_operate rule, or a glob that does not
// parse, and a rule says it escalates what it never reads, or what matches
// nothing.
func TestAMergeRuleAddsOnlyPathGlobsItCanMatch(t *testing.T) {
	t.Parallel()
	good := "    - {kinds: [pr_merge, deploy], delegates: [sre], escalate_to: [ops-lead], " +
		"always_escalate_paths: [billing/**, 'services/*/payments/**']}\n"
	doc, err := config.ParseCompany([]byte(delegatedDoc(t, good)))
	if err != nil {
		t.Fatalf("a merge and deploy rule with path globs was refused: %v", err)
	}
	if rule, n, ok := doc.DelegatedDecisions.RuleFor(delegated.KindDeploy); !ok || n != 1 ||
		len(rule.AlwaysEscalatePaths) != 2 {
		t.Fatalf("deploy's rule reads %+v (%d, %v)", rule, n, ok)
	}
	if got := doc.DelegatedDecisions.EscalateFor(delegated.KindPRMerge); len(got) != 1 || got[0] != "ops-lead" {
		t.Fatalf("a merge escalates to %v, want ops-lead", got)
	}
	for _, c := range []struct{ name, rules, path, kind string }{
		{"paths on a server command rule", "    - {kinds: [ssh_operate], delegates: [security-a], " +
			"escalate_to: [ops-lead], always_escalate_paths: [billing/**]}\n",
			"delegated_decisions.rules[0].always_escalate_paths", "conflict"},
		{"a glob that does not parse", "    - {kinds: [pr_merge], delegates: [security-a], " +
			"escalate_to: [ops-lead], always_escalate_paths: ['/billing//x']}\n",
			"delegated_decisions.rules[0].always_escalate_paths[0]", "unknown_value"},
	} {
		_, parseErr := config.ParseCompany([]byte(delegatedDoc(t, c.rules)))
		if p, ok := problemAt(parseErr, c.path); !ok || p.Kind != c.kind {
			t.Errorf("%s: %v, want %s at %s", c.name, parseErr, c.kind, c.path)
		}
	}
}

// A rule's required checks are held to a merge or deploy rule, one GitHub
// repository's branch per entry, and names a check run can carry, each once;
// the rule hands them to the engine as written.
//
// Mutation: accept required checks on an ssh_operate rule, a repository that
// is not owner/name, a check twice or an entry for one branch twice, and the
// rule says it holds a merge to checks it can never read.
func TestARuleRequiresChecksOnlyOfAGitHubBranchItReleases(t *testing.T) {
	t.Parallel()
	good := "    - {kinds: [pr_merge, deploy], delegates: [sre], escalate_to: [ops-lead], required_checks: [" +
		"{repo: acme/api, branch: main, checks: [test, 'lint (ubuntu-latest)']}, " +
		"{repo: acme/web, branch: release/v2, checks: [build]}]}\n"
	doc, err := config.ParseCompany([]byte(delegatedDoc(t, good)))
	if err != nil {
		t.Fatalf("a merge rule with required checks was refused: %v", err)
	}
	rule, _, _ := doc.DelegatedDecisions.RuleFor(delegated.KindPRMerge)
	got := rule.Release().RequiredChecks
	if len(got) != 2 || got[0].Repo != "acme/api" || got[0].Branch != "main" ||
		strings.Join(got[0].Checks, ",") != "test,lint (ubuntu-latest)" || got[1].Branch != "release/v2" {
		t.Fatalf("the rule's required checks read %+v", got)
	}
	entry := func(e string) string {
		return "    - {kinds: [pr_merge], delegates: [security-a], escalate_to: [ops-lead], " +
			"required_checks: [" + e + "]}\n"
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = "{repo: acme/api, branch: b" + strings.Repeat("x", i) + ", checks: [test]}"
	}
	for _, c := range []struct{ name, rules, path, kind string }{
		{"checks on a server command rule", "    - {kinds: [ssh_operate], delegates: [security-a], " +
			"escalate_to: [ops-lead], required_checks: [{repo: acme/api, branch: main, checks: [test]}]}\n",
			"delegated_decisions.rules[0].required_checks", "conflict"},
		{"a repository with no owner", entry("{repo: api, branch: main, checks: [test]}"),
			"delegated_decisions.rules[0].required_checks[0].repo", "shape"},
		{"a repository path too deep", entry("{repo: acme/api/x, branch: main, checks: [test]}"),
			"delegated_decisions.rules[0].required_checks[0].repo", "shape"},
		{"an empty branch", entry("{repo: acme/api, branch: '', checks: [test]}"),
			"delegated_decisions.rules[0].required_checks[0].branch", "shape"},
		{"no checks", entry("{repo: acme/api, branch: main, checks: []}"),
			"delegated_decisions.rules[0].required_checks[0].checks", "missing"},
		{"an empty check", entry("{repo: acme/api, branch: main, checks: ['']}"),
			"delegated_decisions.rules[0].required_checks[0].checks[0]", "missing"},
		{"a check with a trailing space", entry("{repo: acme/api, branch: main, checks: ['test ']}"),
			"delegated_decisions.rules[0].required_checks[0].checks[0]", "shape"},
		{"a check twice", entry("{repo: acme/api, branch: main, checks: [test, test]}"),
			"delegated_decisions.rules[0].required_checks[0].checks[1]", "conflict"},
		{"one branch twice", entry("{repo: acme/api, branch: main, checks: [test]}, " +
			"{repo: Acme/API, branch: main, checks: [lint]}"),
			"delegated_decisions.rules[0].required_checks[1]", "conflict"},
		{"past 64 entries", entry(strings.Join(many, ", ")),
			"delegated_decisions.rules[0].required_checks", "out_of_range"},
	} {
		_, parseErr := config.ParseCompany([]byte(delegatedDoc(t, c.rules)))
		if p, ok := problemAt(parseErr, c.path); !ok || p.Kind != c.kind {
			t.Errorf("%s: %v, want %s at %s", c.name, parseErr, c.kind, c.path)
		}
	}
}

// A required check of a repository the company's GitHub does not cover is
// advised, never refused: it is never read, since no merge or deploy is asked
// there. An owner given by reference is known only at run time, so nothing is
// advised against it.
func TestARequiredCheckOutsideTheCompanysGitHubIsAdvised(t *testing.T) {
	t.Parallel()
	rules := "    - {kinds: [pr_merge], delegates: [security-a], escalate_to: [ops-lead], required_checks: [" +
		"{repo: acme/api, branch: main, checks: [test]}, {repo: other/api, branch: main, checks: [test]}]}\n"
	advised := func(github string) []string {
		t.Helper()
		doc := delegatedDoc(t, rules)
		if github != "" {
			doc = strings.Replace(doc, "integrations:\n", "integrations:\n  github: "+github+"\n", 1)
		}
		cfg, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		var out []string
		for _, w := range cfg.AdvisoryWarnings() {
			if strings.Contains(w.Path, "required_checks") {
				out = append(out, w.Path)
			}
		}
		return out
	}
	first := "delegated_decisions.rules[0].required_checks[0].repo"
	second := "delegated_decisions.rules[0].required_checks[1].repo"
	for _, c := range []struct {
		name, github string
		want         []string
	}{
		{"no GitHub", "", []string{first, second}},
		{"an organization", "{enabled: true, webhook_secret: s, provisioning: {org: Acme}}", []string{second}},
		{"an organization by reference", "{enabled: true, webhook_secret: s, provisioning: {org: '${GH_ORG}'}}",
			nil},
	} {
		if got := advised(c.github); strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s: advised %v, want %v", c.name, got, c.want)
		}
	}
}

// The limits every delegate is held to are the document's where it writes
// them and the engine's defaults where it does not, and a written value is
// held to its range: a deadline or a cap that would switch a limit off, or
// stretch it past what it bounds, is refused naming the key.
//
// Mutation: read a written limit as the default, or skip the range, and a
// company's caps either never apply or switch off.
func TestADelegatedLimitIsTheDocumentsWithinItsRange(t *testing.T) {
	t.Parallel()
	rule := "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead]}\n"
	cfg, err := config.ParseCompany([]byte(delegatedDoc(t, rule)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DelegatedDecisions.Limits(); got != delegated.DefaultLimits() {
		t.Errorf("a document that writes no limits holds %+v, want the defaults %+v", got,
			delegated.DefaultLimits())
	}
	written := delegatedDoc(t, rule) + "  deadline_seconds: 1500\n  allows_per_hour: 5\n  server_allows_per_hour: 3\n"
	if cfg, err = config.ParseCompany([]byte(written)); err != nil {
		t.Fatal(err)
	}
	want := delegated.Limits{Deadline: 25 * time.Minute, AllowsPerHour: 5, ServerAllowsPerHour: 3}
	if got := cfg.DelegatedDecisions.Limits(); got != want {
		t.Errorf("the document's limits read %+v, want %+v", got, want)
	}
	for _, c := range []struct{ line, path string }{
		{"  deadline_seconds: 120\n", "delegated_decisions.deadline_seconds"},
		{"  deadline_seconds: 7200\n", "delegated_decisions.deadline_seconds"},
		{"  allows_per_hour: -1\n", "delegated_decisions.allows_per_hour"},
		{"  allows_per_hour: 0\n", "delegated_decisions.allows_per_hour"},
		{"  deadline_seconds: 0\n", "delegated_decisions.deadline_seconds"},
		{"  allows_per_hour: 61\n", "delegated_decisions.allows_per_hour"},
		{"  server_allows_per_hour: 500\n", "delegated_decisions.server_allows_per_hour"},
	} {
		_, err := config.ParseCompany([]byte(delegatedDoc(t, rule) + c.line))
		if p, ok := problemAt(err, c.path); !ok || p.Kind != "out_of_range" || p.Range == nil {
			t.Errorf("%q: %v, want out_of_range at %s carrying its range", strings.TrimSpace(c.line), err, c.path)
		}
	}
}

// Every refusal of a rule carries a person's words, with no key or path, for
// the form to show under its field.
func TestARuleRefusalIsSaidInAPersonsWords(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, rules, path, said string }{
		{"no kind", "    - {kinds: [], delegates: [security-a], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].kinds", "Pick at least one kind of request."},
		{"a kind in two rules", "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead]}\n" +
			"    - {kinds: [ssh_operate], delegates: [security-b], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[1].kinds[0]",
			"Rule 1 already decides server commands. A kind of request is in one rule only."},
		{"no delegate", "    - {kinds: [ssh_operate], delegates: [], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].delegates", "Pick the agent that decides."},
		{"a person delegate", "    - {kinds: [ssh_operate], delegates: [ops-lead], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].delegates[0]",
			"Ops Lead is a person. An agent decides, and people get what it escalates."},
		{"a delegate that operates", "    - {kinds: [ssh_operate], delegates: [sre], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].delegates[0]", "SRE can ask for server commands itself, so it can't decide them."},
		{"one model twice", "    - {kinds: [ssh_operate], delegates: [security-a, security-c], escalate_to: [ops-lead]}\n",
			"delegated_decisions.rules[0].delegates",
			"Security A and Security C run on the same model. A panel needs two different models."},
		{"no one to escalate to", "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: []}\n",
			"delegated_decisions.rules[0].escalate_to", "Pick at least one person."},
		{"an agent escalated to", "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [security-b]}\n",
			"delegated_decisions.rules[0].escalate_to[0]", "Security B is an agent. Only people answer an escalation."},
		{"a rate past its bound", "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead], " +
			"sample_one_in: 5000}\n", "delegated_decisions.rules[0].sample_one_in", "From 1 to 1000."},
		{"paths on a command rule", "    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead], " +
			"always_escalate_paths: [billing/**]}\n", "delegated_decisions.rules[0].always_escalate_paths",
			"Paths apply only to merges and deploys."},
		{"a repository that is not owner/name", "    - {kinds: [pr_merge], delegates: [security-a], " +
			"escalate_to: [ops-lead], required_checks: [{repo: api, branch: main, checks: [test]}]}\n",
			"delegated_decisions.rules[0].required_checks[0].repo",
			`"api" is not a GitHub repository. Write it as owner/name, like acme/api.`},
		{"an entry with no checks", "    - {kinds: [pr_merge], delegates: [security-a], " +
			"escalate_to: [ops-lead], required_checks: [{repo: acme/api, branch: main, checks: []}]}\n",
			"delegated_decisions.rules[0].required_checks[0].checks", "Add at least one check."},
	} {
		_, err := config.ParseCompany([]byte(delegatedDoc(t, c.rules)))
		p, ok := problemAt(err, c.path)
		if !ok {
			t.Errorf("%s: %v, want a refusal at %s", c.name, err, c.path)
			continue
		}
		if p.Said != c.said {
			t.Errorf("%s: said %q, want %q", c.name, p.Said, c.said)
		}
		if strings.Contains(p.Said, "_") || strings.Contains(p.Said, "delegated_decisions") {
			t.Errorf("%s: said %q names a key", c.name, p.Said)
		}
	}
}
