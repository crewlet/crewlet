package delegated_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/delegated"
)

// A panel allows only when every delegate allowed, and anything else it ends
// on goes to people: one delegate's allow waits on the other, and two that
// differ are a disagreement.
//
// Mutation: decide a panel on its first answer, or read allow beside deny as
// allow, and a request one model refused is carried out.
func TestAPanelAllowsOnlyWhenEveryDelegateAllows(t *testing.T) {
	t.Parallel()
	panel := []string{"security-a", "security-b"}
	allow := func(by string) delegated.Given {
		return delegated.Given{By: by, Verdict: delegated.Allow, Confidence: delegated.High}
	}
	deny := func(by string) delegated.Given {
		return delegated.Given{By: by, Verdict: delegated.Deny, Confidence: delegated.High}
	}
	for _, c := range []struct {
		name  string
		given []delegated.Given
		want  delegated.Outcome
	}{
		{"one allow waits", []delegated.Given{allow("security-a")}, delegated.Outcome{}},
		{"both allow", []delegated.Given{allow("security-a"), allow("security-b")},
			delegated.Outcome{Decided: true, Verdict: delegated.Allow}},
		{"both deny", []delegated.Given{deny("security-a"), deny("security-b")},
			delegated.Outcome{Decided: true, Verdict: delegated.Deny}},
		{"allow beside deny", []delegated.Given{allow("security-a"), deny("security-b")},
			delegated.Outcome{Decided: true, Verdict: delegated.Escalate,
				Overruled: []string{delegated.OverruledDisagreement}}},
		{"one escalation decides at once", []delegated.Given{{By: "security-b", Verdict: delegated.Escalate,
			Confidence: delegated.High}}, delegated.Outcome{Decided: true, Verdict: delegated.Escalate}},
		{"an answer from outside the panel is not its other half",
			[]delegated.Given{allow("security-a"), allow("intruder")}, delegated.Outcome{}},
	} {
		got := delegated.Combine(panel, c.given)
		if got.Decided != c.want.Decided || got.Verdict != c.want.Verdict ||
			!slices.Equal(got.Overruled, c.want.Overruled) {
			t.Errorf("%s: a panel came to %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := delegated.Combine([]string{"security-a"}, []delegated.Given{allow("security-a")}); !got.Decided ||
		got.Verdict != delegated.Allow {
		t.Errorf("a single delegate's allow came to %+v, want an allow", got)
	}
}

// Low confidence goes to people whatever the verdict beside it, a deny
// included, and an unknown confidence is low.
//
// Mutation: honour a low-confidence allow, or read an empty confidence as
// high, and an unsure delegate decides for a person.
func TestALowConfidenceAlwaysEscalates(t *testing.T) {
	t.Parallel()
	for _, v := range delegated.Verdicts() {
		for _, conf := range []delegated.Confidence{delegated.Low, ""} {
			got := delegated.Combine([]string{"sec"}, []delegated.Given{{By: "sec", Verdict: v, Confidence: conf}})
			if !got.Decided || got.Verdict != delegated.Escalate ||
				!slices.Contains(got.Overruled, delegated.OverruledLowConfidence) {
				t.Errorf("a %s at confidence %q came to %+v, want an escalation for low confidence", v, conf, got)
			}
		}
	}
}

// One decision in N is spot-checked, the same request always alike: about
// one in ten of a thousand at the default, every one at one in one.
//
// Mutation: sample on a counter or never, and the share drifts or is none.
func TestOneDecisionInNIsSpotChecked(t *testing.T) {
	t.Parallel()
	picked := 0
	for i := range 1000 {
		request := fmt.Sprintf("0190f3a8-0000-7000-8000-%012d", i)
		first := delegated.Sampled(request, delegated.DefaultSampleOneIn)
		if first {
			picked++
		}
		if again := delegated.Sampled(request, delegated.DefaultSampleOneIn); again != first {
			t.Fatalf("request %s is sampled one way and then the other", request)
		}
		if !delegated.Sampled(request, 1) {
			t.Fatalf("one in one did not sample %s", request)
		}
	}
	if picked < 60 || picked > 140 {
		t.Errorf("one in %d sampled %d of 1000 decisions", delegated.DefaultSampleOneIn, picked)
	}
	if _, ok := delegated.SensitiveLabel([]string{"infra", "Security"}); !ok {
		t.Errorf("a Security label reads as not sensitive")
	}
	if !delegated.KindSSHOperate.Delegable() || delegated.KindOther.Delegable() {
		t.Errorf("ssh_operate is delegable %v and other %v", delegated.KindSSHOperate.Delegable(),
			delegated.KindOther.Delegable())
	}
}

// Every built-in path class catches its conventional places, at the root and
// below it, and leaves an ordinary source file alone; a rule's own glob adds
// to them, and a glob nobody can read matches every path.
//
// Mutation: drop a class's glob, read `**` as one component or more, or let a
// glob that does not parse match nothing, and a change touching that place is
// merged by a delegate alone.
func TestEveryPathClassCatchesItsConventionalPlaces(t *testing.T) {
	t.Parallel()
	for class, paths := range map[string][]string{
		delegated.PathCIPipelines:    {".github/workflows/ci.yml", ".gitlab-ci.yml", "build/Jenkinsfile", "Jenkinsfile"},
		delegated.PathCodeOwners:     {"CODEOWNERS", ".github/CODEOWNERS"},
		delegated.PathSecretFiles:    {".env", "services/api/.env.production", "deploy/tls.key", "config/secrets/db.yaml"},
		delegated.PathAccessPolicies: {"platform/iam/roles.yaml", "policy/allow.rego"},
		delegated.PathInfrastructure: {"main.tf", "infra/terraform/vpc/main.tf", "stacks/Pulumi.prod.yaml"},
		delegated.PathMigrations:     {"db/migrations/0042_drop_users.sql", "schema.sql"},
		delegated.PathCompany:        {"company.yaml", "ops/crewlet.yaml"},
	} {
		for _, p := range paths {
			matches := delegated.EscalatingPaths([]string{"README.md", p}, nil)
			if !slices.ContainsFunc(matches, func(m delegated.PathMatch) bool { return m.Class == class && m.Path == p }) {
				t.Errorf("%s does not escalate as %s: %+v", p, class, matches)
			}
		}
	}
	if got := delegated.EscalatingPaths([]string{"web/src/app.ts", "docs/guide.md"}, nil); len(got) != 0 {
		t.Errorf("ordinary source escalates: %+v", got)
	}
	rule := delegated.EscalatingPaths([]string{"web/src/app.ts", "billing/rates.go"}, []string{"billing/**"})
	if len(rule) != 1 || rule[0].Class != delegated.PathRule || rule[0].Path != "billing/rates.go" {
		t.Errorf("a rule's own glob matched %+v, want billing/rates.go", rule)
	}
	if bad := delegated.EscalatingPaths([]string{"web/src/app.ts"}, []string{"/bad//glob"}); len(bad) != 1 {
		t.Errorf("a glob that does not parse matched %+v, want every path", bad)
	}
	for _, k := range []delegated.Kind{delegated.KindPRMerge, delegated.KindDeploy} {
		if !k.Delegable() {
			t.Errorf("%s is not delegable", k)
		}
	}
}

// A subject is what a request decides: the same fields are the same subject
// whoever words the ask, and a different kind, field or split of the same
// text between fields is another.
//
// Mutation: join the fields with a separator instead of prefixing each with
// its length, and a command that holds the separator passes for another
// server's.
func TestASubjectIsWhatARequestDecidesNeverItsWording(t *testing.T) {
	t.Parallel()
	base := delegated.Subject(delegated.KindPRMerge, "github", "acme/web#42", "a1", "squash")
	if base != delegated.Subject(delegated.KindPRMerge, "github", "acme/web#42", "a1", "squash") || len(base) != 64 {
		t.Fatalf("the same fields gave %q, want one SHA-256 in hex each time", base)
	}
	for name, other := range map[string]string{
		"another kind":         delegated.Subject(delegated.KindDeploy, "github", "acme/web#42", "a1", "squash"),
		"another head":         delegated.Subject(delegated.KindPRMerge, "github", "acme/web#42", "b2", "squash"),
		"another method":       delegated.Subject(delegated.KindPRMerge, "github", "acme/web#42", "a1", "merge"),
		"a field moved across": delegated.Subject(delegated.KindPRMerge, "github", "acme/web#42a1", "", "squash"),
		"a field fewer":        delegated.Subject(delegated.KindPRMerge, "github", "acme/web#42", "a1squash"),
	} {
		if other == base {
			t.Errorf("%s is the same subject as the request it differs from", name)
		}
	}
}
