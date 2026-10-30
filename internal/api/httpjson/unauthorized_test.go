package httpjson_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// unauthorizedSays is the message a refusal on authority is answered with,
// built the way every surface builds one — through authz.RefusalFields, so the
// keys this package reads are the ones authz writes.
func unauthorizedSays(reason authz.Reason, grants ...iam.Grant) string {
	body := httpjson.Envelope(httpjson.CodeUnauthorized, fieldsOf(reason, grants...))
	message, _ := body["message"].(string)
	return message
}

// fieldsOf is the detail authz.RefusalFields answers a refusal for reason
// with, naming grants.
func fieldsOf(reason authz.Reason, grants ...iam.Grant) httpjson.Detail {
	_, _, detail := authz.RefusalFields(authz.Decision{Reason: reason, Grants: grants})
	return detail
}

// A REFUSAL SAYS THE RULE THAT REFUSED, AND NAMES A PERMISSION ONLY WHERE ONE
// IS A WAY PAST IT.
//
// `unauthorized` is every refusal the authority table makes, and its one
// sentence named the four permissions for all of them: a credential acting as
// no seat, an agent taking a person's verb, an account that is not active and
// an admin whose proof had gone stale were each told to ask an admin for
// access — which, holding admin, some of them had — while the `reason` beside
// the sentence said something else. Each reason is asked here twice where the
// grants matter: with a permission named beside it, which a relation's
// sentence offers and every other reason's ignores, and with none.
//
// Mutation: answer every reason the code's own sentence (drop the situated row)
// and every case but the bare no-grant one fails; drop a reason's row and its
// case fails beside the completeness check; offer the grants for a reason that
// does not cure (set grantCures on unseated) and that row fails; key the
// sentence on any word but authz's `reason` and every row fails.
func TestUnauthorizedSaysTheRuleThatRefused(t *testing.T) {
	t.Parallel()
	generic := httpjson.CodeUnauthorized.Message()
	for _, c := range []struct {
		reason authz.Reason
		grants []iam.Grant
		says   []string
		// cures: the permission named is offered as the remedy.
		cures bool
	}{
		{authz.ReasonNoGrant, []iam.Grant{iam.GrantWorklet},
			[]string{"(Worklet)", "Ask an admin"}, true},
		{authz.ReasonNoGrant, []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
			[]string{"(Worklet or Doclet)"}, true},
		{authz.ReasonNotSelf, nil, []string{"someone else", "No access would"}, false},
		{authz.ReasonNotSelf, []iam.Grant{iam.GrantAdmin},
			[]string{"someone else", "With Admin access", "Ask an admin"}, true},
		{authz.ReasonNotLead, nil, []string{"its lead in the org chart", "No access would"}, false},
		{authz.ReasonNotLead, []iam.Grant{iam.GrantAdmin}, []string{"its lead"}, true},
		{authz.ReasonNotAuthor, []iam.Grant{iam.GrantAdmin}, []string{"wrote this"}, true},
		{authz.ReasonUnnamed, []iam.Grant{iam.GrantAdmin}, []string{"doesn't say which record"}, true},
		{authz.ReasonSeatRefused, []iam.Grant{iam.GrantAdmin},
			[]string{"Only a person can do this", "not an agent"}, false},
		{authz.ReasonSeatOnly, []iam.Grant{iam.GrantAdmin},
			[]string{"Only an agent can do this"}, false},
		{authz.ReasonUnseated, []iam.Grant{iam.GrantAdmin},
			[]string{"this account has none", "crewlet iam bind"}, false},
		{authz.ReasonStage, []iam.Grant{iam.GrantAdmin},
			[]string{"isn't active", "crewlet iam activate"}, false},
		{authz.ReasonUnknownAction, []iam.Grant{iam.GrantAdmin},
			[]string{"no rule for that request"}, false},
		// An artifact's share is cleared by its owner. The admin path beside
		// it admits all the same, and named as the remedy it sent every
		// reader of a colleague's page to ask for the permission that runs
		// the company.
		{authz.ReasonRole, []iam.Grant{iam.GrantAdmin},
			[]string{"Your role on this", "share it with you"}, false},
		// No permission an agent holds lifts what its requester could not do.
		{authz.ReasonCeiling, nil, []string{"the agent can't either"}, false},
		// Nor does any the person holds lift what their assistant may not
		// do: a verb it never takes stays theirs, and a change nobody
		// approved is one it proposes.
		{authz.ReasonAssistRefused, []iam.Grant{iam.GrantAdmin},
			[]string{"The assistant never does this", "stays with you"}, false},
		{authz.ReasonAssistScope, []iam.Grant{iam.GrantAdmin},
			[]string{"only as a change you approve"}, false},
	} {
		message := unauthorizedSays(c.reason, c.grants...)
		for _, want := range c.says {
			if !strings.Contains(message, want) {
				t.Errorf("%s with grants %v says %q, want it to say %q",
					c.reason, c.grants, message, want)
			}
		}
		if message == generic {
			t.Errorf("%s is told the code's own sentence, which names every "+
				"permission whatever the rule asked for", c.reason)
		}
		offered := slices.ContainsFunc(c.grants, func(g iam.Grant) bool {
			return strings.Contains(message, strings.ToUpper(string(g)[:1])+string(g)[1:])
		})
		switch {
		case c.cures && !offered:
			t.Errorf("%s with grants %v says %q, which never names the "+
				"permission that would admit the caller", c.reason, c.grants, message)
		case !c.cures && offered:
			t.Errorf("%s with grants %v says %q, offering a permission no "+
				"grant of which would change the answer", c.reason, c.grants, message)
		}
		if !c.cures && len(c.grants) > 0 && message != unauthorizedSays(c.reason) {
			t.Errorf("%s reads differently with grants beside it (%q) than "+
				"without (%q), though none would change it", c.reason, message,
				unauthorizedSays(c.reason))
		}
		// COPY REVIEW, as every code's sentence gets: a person reads it.
		if message == "" || message[0] < 'A' || message[0] > 'Z' ||
			!strings.HasSuffix(message, ".") || strings.Contains(message, "_") {
			t.Errorf("%s says %q, which is not a sentence a person reads", c.reason, message)
		}
	}

	// THE REFUSAL THAT SAYS NOTHING MORE keeps the code's own sentence: no
	// reason at all, a no-grant refusal naming no permission, and a reason
	// no rule refuses by.
	for name, detail := range map[string]httpjson.Detail{
		"no reason":                 {},
		"no grant named":            fieldsOf(authz.ReasonNoGrant),
		"an admission":              fieldsOf(authz.ReasonGrant),
		"a reason off a newer peer": {authz.DetailReason: "invented"},
	} {
		if got := httpjson.Envelope(httpjson.CodeUnauthorized, detail)["message"]; got != generic {
			t.Errorf("%s is told %q, want the code's own sentence", name, got)
		}
	}
}

// EVERY REASON THE AUTHORITY TABLE ANSWERS `unauthorized` HAS A SENTENCE, AND
// NO OTHER.
//
// The two lists are two packages' — the reasons are authz's, and the words are
// this package's, which imports no engine machinery — so nothing but this
// holds them together: a reason authz grows would be told the code's own
// sentence (every permission, for a rule that may ask none), and a sentence
// for a reason nothing reports is what a rename leaves behind. Which reasons
// are answered `unauthorized` at all is the mapping's own answer
// ([authz.RefusalFields]): a stale proof, a token's refusal and no relation
// are answered under codes of their own, so a sentence kept for one is words
// no refusal is told. Mutation: delete a row from the table and the first
// loop fails; add one for `step_up` and the never-unauthorized arm does; add
// one keyed on a word no rule reports and the last does.
func TestEveryRefusalReasonHasASentence(t *testing.T) {
	t.Parallel()
	said := map[string]bool{}
	for _, reason := range httpjson.UnauthorizedReasons() {
		said[reason] = true
	}
	for _, reason := range authz.Reasons {
		// AN ADMISSION is carried by no refusal, and a refusal the mapping
		// answers under a code of its own (a stale proof, a token's, no
		// relation) is never `unauthorized`, so `unauthorized` has no
		// sentence for either.
		_, code, _ := authz.RefusalFields(authz.Decision{Reason: reason,
			Grants: []iam.Grant{iam.GrantAdmin}, Recency: iam.RecencyStepUp})
		if reason.Admits() || code != httpjson.CodeUnauthorized {
			if said[string(reason)] {
				t.Errorf("%q is never answered unauthorized (admits %t, answered %s), "+
					"and unauthorized keeps a sentence for it", reason, reason.Admits(), code)
			}
			delete(said, string(reason))
			continue
		}
		if !said[string(reason)] {
			t.Errorf("the authority table refuses by %q and unauthorized has no "+
				"sentence for it, so it is told every permission there is", reason)
		}
		delete(said, string(reason))
	}
	for leftover := range said {
		if !authz.Reason(leftover).Valid() {
			t.Errorf("unauthorized keeps a sentence for %q and no rule reports it", leftover)
		}
	}
}

// A PERMISSION CURES EXACTLY THE REFUSALS AUTHZ SAYS ONE LIFTS.
//
// The sentence table decides for each reason whether the grants beside it are
// a way past the rule, and internal/authz decides the same thing for itself —
// [authz.Reason.Authority], which its fold of a gesture's several decisions
// reads to tell a part a permission lifts from a bar on the caller. Two
// answers to one question drift unless something holds them together: a
// relation the fold reads as a bar would be told its remedy here, and one this
// table thinks a permission lifts would be refused by the fold as a bar.
// Mutation: flip grantCures on not_author and its row fails.
func TestGrantCuresIsAuthzsAuthorityPartition(t *testing.T) {
	t.Parallel()
	reasons := httpjson.UnauthorizedReasons()
	if len(reasons) < 10 {
		t.Fatalf("read %d reasons; a walk that stopped reading the table "+
			"certifies nothing", len(reasons))
	}
	for _, reason := range reasons {
		if got, want := httpjson.GrantCures(reason), authz.Reason(reason).Authority(); got != want {
			t.Errorf("unauthorized says a permission cures %q: %v; authz says "+
				"it is a refusal on authority: %v", reason, got, want)
		}
	}
}
