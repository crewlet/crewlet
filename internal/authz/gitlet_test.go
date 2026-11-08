package authz_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// NO CODE ROW ADMITS EVERYBODY, AND GITLET OPENS NOTHING BUT CODE (ADR-0110).
//
// Reaching a repository takes the gitlet app through every door, so every verb
// about the company's code — the dotted route verbs, every code tool, the
// inbox's code marks — names the app, refused at the table before any
// repository is read, or is admin's (who read a repository, the Gitlet nodes'
// caches), and admin includes the app. A code verb added later as
// [authz.ClassEverybody], as all of them were before, would be the one door
// the grant does not close; this walk names it. The verbs are found by their
// names, `code.` and `code_`, which every one of them carries, and the walk
// fails if it finds too few to mean anything. The other half holds the app to
// code alone: a row outside it naming gitlet would open something else to
// whoever is given code. Mutation: put code.git back on ClassEverybody and the
// first half goes red; name gitlet on a Worklet row and the second does.
func TestNoCodeRowAdmitsEverybody(t *testing.T) {
	t.Parallel()
	walked := 0
	for _, a := range authz.Actions() {
		name := string(a)
		code := strings.HasPrefix(name, "code.") || strings.HasPrefix(name, "code_")
		class, _ := authz.ClassOf(a)
		app, _ := authz.AppOf(a)
		if !code {
			if app == iam.GrantGitlet {
				t.Errorf("%s names gitlet and is no code verb, so the app opens it to "+
					"whoever is given the company's code", a)
			}
			continue
		}
		walked++
		switch {
		case class == authz.ClassEverybody:
			t.Errorf("%s admits everybody, so a principal without gitlet reaches the "+
				"company's code through it", a)
		case class == authz.ClassAdmin:
			// ADMIN'S, which includes every app through iam.Expand.
		case app != iam.GrantGitlet:
			t.Errorf("%s is decided by %s naming %q, want the gitlet app on every code "+
				"verb that is not admin's", a, class, app)
		}
	}
	// code.read, code.git, the read tools, the write tools, the change verbs,
	// the checks, the purge, the import, the readers, the marks and the status.
	if walked < 40 {
		t.Fatalf("walked %d code verbs, want every one of them (at least 40): the "+
			"names this walk finds them by have changed", walked)
	}
}

// A HOLDER OF GITLET IS NOT REFUSED AT THE TABLE, AND NOBODY ELSE REACHES CODE.
//
// The two route verbs every door goes through, decided for each kind of
// principal that can present one: a person and a credential bound to no seat
// are admitted holding gitlet and refused without it, with the app named; an
// administrator holds it through iam.Expand; a seat holds it as it holds every
// app (ADR-0033), and what it may do on a repository stays its grant. The
// CONTROL is the same principal holding every other app, which opens nothing
// here. Mutation: decide code.git on any app and the control goes red.
func TestOnlyAGitletHolderReachesTheCodeRoutes(t *testing.T) {
	t.Parallel()
	var others []iam.Grant
	for _, app := range iam.Apps {
		if app != iam.GrantGitlet {
			others = append(others, app)
		}
	}
	for _, a := range []authz.Action{authz.ActionCodeRead, authz.ActionCodeGit} {
		for _, c := range []struct {
			name    string
			p       iam.Principal
			allowed bool
		}{
			{"a person holding gitlet", person("jane.doe", iam.GrantGitlet), true},
			{"a token bound to no seat holding gitlet", machine("ci:runner", iam.GrantGitlet), true},
			{"an administrator", person("jane.doe", iam.GrantAdmin), true},
			{"a seat holding every app", seat("sre", iam.Apps...), true},
			{"a person holding every other app", person("jane.doe", others...), false},
			{"a token holding every other app", machine("ci:runner", others...), false},
		} {
			d := authz.Decide(t.Context(), c.p, a, authz.Object{}, authz.NoChart{}, decidedAt)
			if d.Unknown() || d.Allowed != c.allowed {
				t.Errorf("%s, %s: decided %+v, want allowed %v", a, c.name, d, c.allowed)
				continue
			}
			if !c.allowed && (d.Reason != authz.ReasonNoGrant || len(d.Grants) != 1 ||
				d.Grants[0] != iam.GrantGitlet) {

				t.Errorf("%s, %s: refused %+v, want no_grant naming exactly gitlet", a, c.name, d)
			}
		}
	}
}
