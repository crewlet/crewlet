package iamapi_test

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// THE DASHBOARD OFFERS EXACTLY THE ENGINE'S GRANTS, in its order, and withholds
// from a token exactly the grants the engine never lets one carry.
//
// People & access confers grants — an invitation, an edit, a service account,
// a token — by ticking them off `GRANTS`, and greys out of a token's mint the
// ones in `TOKEN_WITHHELD_GRANTS`. A grant the engine grew that the list lacks
// is one no administrator can confer from the dashboard, and one the list kept
// that the engine dropped is a box every write refuses.
//
// Mutation: drop a grant from either list, or reorder `GRANTS`, and this fails.
func TestTheDashboardOffersExactlyTheEnginesGrants(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []iam.Grant
	}{
		{"GRANTS", iam.AllGrants},
		{"TOKEN_WITHHELD_GRANTS", iam.PersonPresentGrants},
	} {
		body, err := clientsource.Literal(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		got := clientsource.Strings(body)
		if len(got) == 0 {
			t.Fatalf("%s reads as empty, so this gate certifies nothing", tc.name)
		}
		want := make([]string, 0, len(tc.want))
		for _, g := range tc.want {
			want = append(want, string(g))
		}
		if !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v, want the engine's, in order: %v",
				tc.name, got, want)
		}
	}
}

// THE DASHBOARD CHECKS A LOGIN BY THE ENGINE'S GRAMMAR, login for login.
//
// Every form that takes a login holds it to `PERSON_LOGIN` or `MACHINE_LOGIN`,
// `MAX_LOGIN` and `CREDENTIAL_CLASSES` before it posts, so a person who types
// their own name is told the rule under the field rather than handed the
// domain's refusal. Held by BEHAVIOUR rather than by spelling: the patterns are
// compiled here and a corpus run through them and through [iam.ValidLoginFor],
// so a grammar change on either side that answers any of these differently
// fails — and a pattern that compiles to nothing fails on the corpus's controls.
//
// Mutation: drop the dot from `PERSON_LOGIN`'s separator, raise `MAX_LOGIN`, or
// drop a class from `CREDENTIAL_CLASSES`, and this fails.
func TestTheDashboardChecksALoginByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	pattern := func(name string) *regexp.Regexp {
		source, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		re, err := regexp.Compile(source)
		if err != nil {
			t.Fatalf("%s does not compile: %v", name, err)
		}
		return re
	}
	person, machine := pattern("PERSON_LOGIN"), pattern("MACHINE_LOGIN")
	raw, err := clientsource.Scalar(tree, "MAX_LOGIN")
	if err != nil {
		t.Fatal(err)
	}
	maxLogin, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("MAX_LOGIN is %q: %v", raw, err)
	}
	body, err := clientsource.Literal(tree, "CREDENTIAL_CLASSES")
	if err != nil {
		t.Fatal(err)
	}
	classes := clientsource.Strings(body)

	// THE DASHBOARD'S ANSWER, as lib/login.ts composes the four.
	dashboard := func(kind iam.Kind, login string) bool {
		re := person
		if kind == iam.KindMachine {
			re = machine
			for _, c := range classes {
				if strings.HasPrefix(login, c) {
					return false
				}
			}
		}
		return re.MatchString(login) && len(login) <= maxLogin
	}
	corpus := []string{
		"jane.doe", "erin.ng", "dana-sre.ops", "a.b", "a1.b2.c3",
		"ci:release", "token:ops", "deploy:bot-1", "a:b:c",
		"Frank", "erin", "Erin NG", "erin ng", "jane..doe", ".jane", "jane.",
		"jane.doe-", "-jane.doe", "jane_doe", "jane.Doe", "jane.doé",
		"ci::release", "ci:", ":ci", "ci:Release", "ci:erin.ng", "jane.doe:x",
		"pat:release", "session:abc", "pat:", "patrol:x", "sessions:x",
		strings.Repeat("a", 30) + "." + strings.Repeat("b", 33),
		strings.Repeat("a", 30) + "." + strings.Repeat("b", 34),
		strings.Repeat("a", 30) + ":" + strings.Repeat("b", 33),
		strings.Repeat("a", 30) + ":" + strings.Repeat("b", 34),
		"",
	}
	admitted := 0
	for _, kind := range []iam.Kind{iam.KindPerson, iam.KindMachine} {
		for _, login := range corpus {
			engine := iam.ValidLoginFor(kind, login)
			if engine {
				admitted++
			}
			if got := dashboard(kind, login); got != engine {
				t.Errorf("a %s login %q: the dashboard answers %v, the engine %v",
					kind, login, got, engine)
			}
		}
	}
	if admitted == 0 {
		t.Fatal("the engine admits nothing in the corpus, so it certifies nothing")
	}
}

// THE DASHBOARD MINTS WITHIN THE ENGINE'S TOKEN LIFETIMES.
//
// The token dialog says what an empty lifetime takes and refuses one past the
// ceiling before it posts — it posted `400` and showed the engine's sentence
// back — so its two numbers are the engine's, in days. Mutation: move either
// constant on one side and this fails.
func TestTheDashboardMintsWithinTheEnginesTokenLifetimes(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, want := range map[string]time.Duration{
		"TOKEN_DEFAULT_DAYS": credential.DefaultTokenLifetime,
		"TOKEN_MAX_DAYS":     credential.MaxTokenLifetime,
	} {
		raw, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		days, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q: %v", name, raw, err)
		}
		if got := time.Duration(days) * 24 * time.Hour; got != want {
			t.Errorf("the dashboard's %s is %d days, the engine's %v", name, days, want)
		}
	}
}
