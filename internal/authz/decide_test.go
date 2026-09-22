package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// chart is a fixture hierarchy: who leads whom, and who leads what.
type chart struct {
	leads    map[[2]string]bool
	projects map[[2]string]bool
	err      error
}

func (c chart) Leads(_ context.Context, actor, subject string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.leads[[2]string{actor, subject}], nil
}

func (c chart) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.projects[[2]string{actor, project}], nil
}

// nimbus is the fixture every case below decides against: the CTO leads the
// SRE, and leads the PLATFORM project.
func nimbus() chart {
	return chart{
		leads:    map[[2]string]bool{{"cto", "sre"}: true},
		projects: map[[2]string]bool{{"cto", "PLATFORM"}: true},
	}
}

// person is a human principal with these grants.
func person(login string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: login, Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: grants}
}

// seat is an agent principal acting as a seat.
func seat(handle string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: handle, Kind: iam.KindSeat,
		Seat: handle, Stage: iam.StageActive, Grants: grants}
}

// THE WHOLE TABLE, ONE CASE PER ROW OF IT.
//
// Written as a table because the rules ARE a table: what makes this suite
// worth anything is that a reader can see nine classes decided side by side
// and spot the row whose answer is wrong for its neighbours.
func TestTheAuthorityTableDecidesEveryClass(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		p       iam.Principal
		action  authz.Action
		object  authz.Object
		allowed bool
		reason  authz.Reason
	}{
		// --- read ---------------------------------------------------- //
		{"state:read reads the board", person("jane.doe", iam.GrantStateRead),
			authz.ActionWorkList, authz.Object{Kind: authz.KindTask},
			true, authz.ReasonGrant},
		{"a seat reads the board", seat("sre", iam.GrantStateRead),
			authz.ActionPageRead, authz.Object{Kind: authz.KindPage},
			true, authz.ReasonGrant},
		// AND A NARROW CREDENTIAL DOES NOT. The whole value of a read
		// grant is that an operator can withhold it: a service account
		// minted to file bugs reads no board, no page and no fleet.
		{"work:write alone reads nothing", person("ci.release", iam.GrantWorkWrite),
			authz.ActionWorkList, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonNoGrant},

		// --- the caller's own working state -------------------------- //
		{"a seat reads its own episodes", seat("sre"),
			authz.ActionEpisodesQuery, authz.Object{Kind: authz.KindPerson},
			true, authz.ReasonSelf},
		{"a seat writes its own diary with no grant at all", seat("sre"),
			authz.ActionMemoryPersist, authz.Object{Kind: authz.KindPerson},
			true, authz.ReasonSelf},
		{"and never somebody else's", seat("sre"),
			authz.ActionMemoryPersist,
			authz.Object{Kind: authz.KindPerson, Owner: "cto"},
			false, authz.ReasonNotSelf},
		// NOT EVEN THE ADMIN PATH, which is what makes this its own class:
		// an operator reading a seat's memory is a different surface with a
		// grant of its own, and admitting it here too would be a second
		// answer to one question.
		{"the admin grant does not reach a seat's diary",
			person("jane.doe", iam.GrantFleetOperate), authz.ActionMemoryPersist,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},

		// --- colleague write ----------------------------------------- //
		{"an ask spends a colleague's turn, so it needs the write",
			seat("sre", iam.GrantWorkWrite), authz.ActionColleagueAsk,
			authz.Object{Kind: authz.KindPerson, Owner: "cto"},
			true, authz.ReasonGrant},
		{"a read-only credential cannot make every seat think",
			person("status.board", iam.GrantStateRead), authz.ActionColleagueAsk,
			authz.Object{Kind: authz.KindPerson, Owner: "cto"},
			false, authz.ReasonNoGrant},
		{"work:write files an item", seat("sre", iam.GrantWorkWrite),
			authz.ActionWorkCreate, authz.Object{Kind: authz.KindTask},
			true, authz.ReasonGrant},
		{"work:write does not author a page", seat("sre", iam.GrantWorkWrite),
			authz.ActionPageCreate, authz.Object{Kind: authz.KindPage},
			false, authz.ReasonNoGrant},
		{"knowledge:write authors a page", seat("sre", iam.GrantKnowledgeWrite),
			authz.ActionPageCreate, authz.Object{Kind: authz.KindPage},
			true, authz.ReasonGrant},

		// --- own record ---------------------------------------------- //
		{"a seat marks its own inbox", seat("sre"),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonSelf},
		{"a lead does not mark a report's inbox", seat("cto"),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},
		{"the admin path unsticks a queue", person("jane.doe", iam.GrantFleetOperate),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonGrant},
		{"an unnamed owner is refused", seat("sre"),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson},
			false, authz.ReasonUnnamed},

		// --- own or lead --------------------------------------------- //
		{"a seat sets its own priorities", seat("sre"),
			authz.ActionPrioritiesSet, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonSelf},
		{"a lead sets a report's priorities", seat("cto"),
			authz.ActionPrioritiesSet, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonLead},
		{"a colleague does not", seat("design"),
			authz.ActionPrioritiesSet, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},

		// --- container ------------------------------------------------ //
		{"a project's lead writes its policy", seat("cto"),
			authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague does not", seat("sre"),
			authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		{"the admin path does", person("jane.doe", iam.GrantFleetOperate),
			authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonGrant},
		{"an unnamed container is refused", seat("cto"),
			authz.ActionProjectWrite, authz.Object{Kind: authz.KindProject},
			false, authz.ReasonUnnamed},

		// --- destructive ---------------------------------------------- //
		{"the container's lead removes an item", seat("cto"),
			authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague who may file may not remove",
			seat("sre", iam.GrantWorkWrite), authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"},
			false, authz.ReasonNotLead},

		// --- purge ----------------------------------------------------- //
		{"a person with the grant purges",
			person("jane.doe", iam.GrantFleetOperate), authz.ActionWorkPurge,
			authz.Object{Kind: authz.KindTask}, true, authz.ReasonGrant},
		{"a seat holding the grant does not",
			seat("sre", iam.GrantFleetOperate), authz.ActionWorkPurge,
			authz.Object{Kind: authz.KindTask}, false, authz.ReasonSeatRefused},
		{"a person without it does not", person("jane.doe"),
			authz.ActionWorkPurge, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonNoGrant},

		// --- authored --------------------------------------------------- //
		{"the author edits their comment", person("jane.doe"),
			authz.ActionPageCommentEdit,
			authz.Object{Kind: authz.KindPage, Author: "jane.doe"},
			true, authz.ReasonAuthor},
		{"somebody else does not", person("sam.rey"),
			authz.ActionPageCommentEdit,
			authz.Object{Kind: authz.KindPage, Author: "jane.doe"},
			false, authz.ReasonNotAuthor},

		// --- operator ---------------------------------------------------- //
		{"config:write patches the company",
			person("jane.doe", iam.GrantConfigWrite), authz.ActionConfigWrite,
			authz.Object{Kind: authz.KindCompany}, true, authz.ReasonGrant},
		{"config:read does not", person("jane.doe", iam.GrantConfigRead),
			authz.ActionConfigWrite, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonNoGrant},
		{"secrets:write does not reveal one",
			person("jane.doe", iam.GrantSecretWrite), authz.ActionSecretReveal,
			authz.Object{Kind: authz.KindCompany}, false, authz.ReasonNoGrant},

		// --- stage --------------------------------------------------------- //
		{"an invited principal may do nothing yet",
			iam.Principal{ID: uuid.New(), Login: "new.hire", Kind: iam.KindPerson,
				Stage:  iam.StageInvited,
				Grants: []iam.Grant{iam.GrantFleetOperate, iam.GrantStateRead}},
			authz.ActionWorkList, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonStage},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), c.p, c.action, c.object, nimbus())
			if d.Unknown() {
				t.Fatalf("decided UNKNOWN: %v", d.Err)
			}
			if d.Allowed != c.allowed {
				t.Errorf("allowed = %v, want %v (reason %q)",
					d.Allowed, c.allowed, d.Reason)
			}
			if d.Reason != c.reason {
				t.Errorf("reason = %q, want %q", d.Reason, c.reason)
			}
		})
	}
}

// AND FLIPPING THE CHART INVERTS EVERY ANSWER THAT TURNS ON IT.
//
// The control for the table above. Without it every lead case could be
// passing because the rule ignores the chart entirely and answers from the
// grant, which is exactly the collapse that made `Lead` false for every
// operator in the surface this replaces.
func TestFlippingTheChartInvertsEveryLeadAnswer(t *testing.T) {
	t.Parallel()
	inverted := chart{
		leads:    map[[2]string]bool{{"sre", "cto"}: true},
		projects: map[[2]string]bool{{"sre", "PLATFORM"}: true},
	}
	for _, c := range []struct {
		name   string
		actor  string
		action authz.Action
		object authz.Object
	}{
		{"priorities", "cto", authz.ActionPrioritiesSet,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}},
		{"project policy", "cto", authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"removal", "cto", authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			was := authz.Decide(t.Context(), seat(c.actor), c.action, c.object, nimbus())
			is := authz.Decide(t.Context(), seat(c.actor), c.action, c.object, inverted)
			if !was.Allowed {
				t.Fatalf("the fixture does not allow this to begin with: %q", was.Reason)
			}
			if is.Allowed {
				t.Errorf("still allowed with the lead relation reversed, so the "+
					"rule is not reading the chart at all (reason %q)", is.Reason)
			}
		})
	}
}

// A CHART THAT COULD NOT ANSWER IS UNKNOWN, NEVER A REFUSAL.
//
// The seam this replaces opened with `if c == nil || c.Org == nil { return
// false }`, so a node that was booting, applying a revision or simply behind
// told every lead in the company that they lead nobody. It reads as a
// permissions bug and the node reports itself healthy throughout. An error
// here has to reach the caller as its own outcome, or the surface answers 403
// and sends somebody to ask for an authority they already hold.
func TestAChartReadErrorIsUnknownNotARefusal(t *testing.T) {
	t.Parallel()
	behind := errors.New("this node is behind the chart log")
	for _, c := range []struct {
		name   string
		action authz.Action
		object authz.Object
	}{
		{"priorities", authz.ActionPrioritiesSet,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}},
		{"project policy", authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), seat("cto"), c.action, c.object,
				chart{err: behind})
			if !d.Unknown() {
				t.Fatalf("a chart that could not answer decided %v (%q), which "+
					"a surface renders as a refusal", d.Allowed, d.Reason)
			}
			if !errors.Is(d.Err, behind) {
				t.Errorf("err = %v, want the chart's own", d.Err)
			}
			if d.Allowed {
				t.Error("Allowed is true beside an error, which a caller " +
					"reading it first would honour")
			}
		})
	}
}

// AND THE ADMIN PATH DOES NOT NEED THE CHART.
//
// The control for the case above, and the reason the grant is checked first:
// an operator holding the deployment's own grant must not be told "I cannot
// tell" by a node that is merely lagging. Without this, closing the hole
// above would have opened a worse one — every operator locked out whenever
// the chart is unavailable.
func TestTheAdminPathDecidesWithNoChartAtAll(t *testing.T) {
	t.Parallel()
	admin := person("jane.doe", iam.GrantFleetOperate)
	for _, c := range []struct {
		name   string
		action authz.Action
		object authz.Object
	}{
		{"priorities", authz.ActionPrioritiesSet,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}},
		{"project policy", authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"removal", authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), admin, c.action, c.object, authz.NoChart{})
			if d.Unknown() {
				t.Fatalf("the admin path asked a chart it does not need: %v", d.Err)
			}
			if !d.Allowed || d.Reason != authz.ReasonGrant {
				t.Errorf("allowed = %v (%q), want the grant to decide",
					d.Allowed, d.Reason)
			}
		})
	}
}

// A VERB WITH NO RULE IS REFUSED, AND SAYS IT IS THIS BUILD'S MISTAKE.
//
// The alternative is a default class, which is how a new verb ships ungated —
// and ships looking correct. The completeness walk keeps it out of
// production; this is what happens if it ever gets there.
func TestAVerbWithNoRuleIsRefused(t *testing.T) {
	t.Parallel()
	d := authz.Decide(t.Context(), person("jane.doe", iam.GrantFleetOperate),
		authz.Action("delete_the_company"), authz.Object{Kind: authz.KindCompany},
		nimbus())
	if d.Allowed {
		t.Fatal("a verb this build has no rule for was allowed")
	}
	if d.Reason != authz.ReasonUnknownAction {
		t.Errorf("reason = %q, want the unknown-action refusal", d.Reason)
	}
	// AND IT IS NOT UNKNOWN. An error here would make this "ask me again",
	// so a verb with no rule would answer 503 to a request that can never
	// succeed — on every retry, for ever.
	if d.Unknown() {
		t.Errorf("a verb with no rule decided UNKNOWN (%v), which a surface "+
			"renders as a retry rather than as a refusal", d.Err)
	}
}
