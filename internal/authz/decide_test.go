package authz_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// chart is a fixture hierarchy: who leads whom, and who leads what.
type chart struct {
	leads      map[[2]string]bool
	projects   map[[2]string]bool
	units      map[[2]string]bool
	containers map[[2]string]bool
	err        error
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

// LeadsUnit answers the unit relation, which is DELIBERATELY A THIRD MAP
// rather than the project one read with a different key: the fixture is what
// catches a rule asking the wrong relation, and a fake that folded the two
// would agree with any rule that asked either.
func (c chart) LeadsUnit(_ context.Context, actor, unit string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.units[[2]string{actor, unit}], nil
}

// LeadsContainer answers the page-container relation, a FOURTH map for
// [chart.LeadsUnit]'s reason.
func (c chart) LeadsContainer(_ context.Context, actor, container string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.containers[[2]string{actor, container}], nil
}

// nimbus is the fixture every case below decides against: the CTO leads the
// SRE, leads the PLATFORM project, and leads the `sre` unit.
func nimbus() chart {
	return chart{
		leads:      map[[2]string]bool{{"cto", "sre"}: true},
		projects:   map[[2]string]bool{{"cto", "PLATFORM"}: true},
		units:      map[[2]string]bool{{"cto", "sre"}: true},
		containers: map[[2]string]bool{{"cto", "RUNBOOKS"}: true},
	}
}

// decidedAt is the instant every case here is decided at.
//
// THE WALL CLOCK AT START-UP rather than a fixed date, because the cases that
// go through [authz.ContextGuard] are decided at the request's own instant and
// a proof fixed in 2026 would be stale against it.
var decidedAt = time.Now()

// proved gives a principal a proof of identity a minute old: inside both
// windows, so every case that is not about the step-up decides on its rule
// alone — the step-up has cases of its own below.
func proved(p iam.Principal) iam.Principal {
	at := decidedAt.Add(-time.Minute)
	p.ReauthAt = at.Add(time.Hour)
	p.SensitiveReauthAt = at.Add(15 * time.Minute)
	return p
}

// person is a human principal with these grants, who proved who they are a
// minute ago.
func person(login string, grants ...iam.Grant) iam.Principal {
	return proved(iam.Principal{ID: uuid.New(), Login: login, Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: grants})
}

// seat is an agent principal acting as a seat.
// personLeading is a HUMAN the chart knows by a seat handle, which is what
// somebody signed in and bound to a seat is: [authz.actorOf] asks the chart by
// the seat, and the human-only bar reads the KIND.
func personLeading(handle string, grants ...iam.Grant) iam.Principal {
	p := seat(handle, grants...)
	p.Kind = iam.KindPerson
	return proved(p)
}

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
			authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague does not", seat("sre"),
			authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		{"the admin path does", person("jane.doe", iam.GrantFleetOperate),
			authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonGrant},
		{"an unnamed container is refused", seat("cto"),
			authz.ActionProjectPolicy, authz.Object{Kind: authz.KindProject},
			false, authz.ReasonUnnamed},
		// BUT THE TOOL ITSELF IS A COLLEAGUE WRITE: adding a label is
		// open to every seat that may write work at all, and gating the
		// whole verb as the container's refused a colleague the one
		// facet internal/tracker grants them.
		{"a colleague declares a label", seat("sre", iam.GrantWorkWrite),
			authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonGrant},
		// A PAGE CONTAINER IS A THIRD KEY CLASS, and the fixture holds
		// it apart from the other two for the same reason: the CTO
		// leads the RUNBOOKS container and neither the project nor the
		// unit map answers for it. A rule that asked the project
		// relation with a container key refused the lead of that very
		// container on every company that does not spell its `space:`
		// and its `project:` the same — which is what
		// [authz.ActionContainerWrite] did before it read the kind.
		{"a container's lead renames a page in it", seat("cto"),
			authz.ActionPageRename,
			authz.Object{Kind: authz.KindContainer, Container: "RUNBOOKS"},
			true, authz.ReasonLead},
		{"a colleague does not", seat("sre"),
			authz.ActionPageRename,
			authz.Object{Kind: authz.KindContainer, Container: "RUNBOOKS"},
			false, authz.ReasonNotLead},
		{"and the project relation is not asked for it", seat("cto"),
			authz.ActionPageRename,
			authz.Object{Kind: authz.KindContainer, Container: "PLATFORM"},
			false, authz.ReasonNotLead},

		// --- saved views ---------------------------------------------- //
		//
		// ONE VERB, TWO AUTHORITIES, picked by what the view IS. The
		// payload decides and the caller cannot choose which question
		// is asked — see [authz.ClassSavedView].
		{"a seat saves its own personal view", seat("sre"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, Owner: "sre"},
			true, authz.ReasonSelf},
		{"a lead does not rearrange a report's own strip", seat("cto"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, Owner: "sre"},
			false, authz.ReasonNotSelf},
		{"a project's lead saves a shared view on it", seat("cto"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindProject,
				Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague does not share one there", seat("sre"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindProject,
				Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		{"a unit's lead shares one on the unit", seat("cto"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindUnit,
				Container: "sre"},
			true, authz.ReasonLead},
		{"a seat shares one on its own page", seat("sre"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindPerson,
				Container: "sre"},
			true, authz.ReasonSelf},
		{"and their lead does too", seat("cto"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindPerson,
				Container: "sre"},
			true, authz.ReasonLead},
		// THE WORKSPACE TAB IS THE ADMIN PATH ALONE: it lands in front
		// of every person in the company and nobody leads it.
		{"nobody leads the workspace", seat("cto"),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindCompany,
				Container: "workspace"},
			false, authz.ReasonNotLead},
		{"so the admin path saves a workspace tab",
			person("jane.doe", iam.GrantFleetOperate),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindCompany,
				Container: "workspace"},
			true, authz.ReasonGrant},
		{"a view naming neither is refused", seat("cto"),
			authz.ActionViewSave, authz.Object{Kind: authz.KindView},
			false, authz.ReasonUnnamed},
		// A UNIT IS THE OTHER CONTAINER RELATION, and the fixture holds
		// the two apart: the CTO leads the `sre` UNIT and the PLATFORM
		// PROJECT, and neither key answers the other's map. A rule that
		// asked the project relation with a unit key would refuse the
		// lead of that very unit on every company whose unit does not
		// file under a project of the same name.
		//
		// A PERSON BOUND TO THE SEAT, because that is who writes the chart:
		// it is an HTTP surface a seat's tools never reach, and every write
		// on it asks for a recent proof of identity no seat can give.
		{"a unit's lead edits its content", personLeading("cto"),
			authz.ActionChartContent,
			authz.Object{Kind: authz.KindUnit, Container: "sre"},
			true, authz.ReasonLead},
		{"a colleague in it does not", personLeading("sre"),
			authz.ActionChartContent,
			authz.Object{Kind: authz.KindUnit, Container: "sre"},
			false, authz.ReasonNotLead},
		{"a unit key is not a project key", personLeading("cto"),
			authz.ActionChartContent,
			authz.Object{Kind: authz.KindUnit, Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		// THE RUNTIME HALF IS THE COMPANY'S, whoever leads the team: a
		// seat's model chain, its credentials and its mcp_env are
		// exec.Command on every engine host.
		{"a unit's lead does not write the runtime half", personLeading("cto"),
			authz.ActionChartRuntime, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonNoGrant},
		{"the company's own grant does",
			person("jane.doe", iam.GrantConfigWrite),
			authz.ActionChartRuntime, authz.Object{Kind: authz.KindCompany},
			true, authz.ReasonGrant},
		{"structure is the company's too", personLeading("cto"),
			authz.ActionChartStructure, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonNoGrant},

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
		// AND THE SAME BAR COMPOSES WITH A RELATION rather than only
		// with the grant, which is why it is a field on the row and not
		// a class: a project's own lead archives it, and an agent that
		// leads one does not — a project nobody can file into again is
		// a company decision.
		{"a project's lead archives it", seat("cto"),
			authz.ActionProjectArchive,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			false, authz.ReasonSeatRefused},
		{"and a person who leads it does",
			personLeading("cto"), authz.ActionProjectArchive,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonLead},

		// --- authored --------------------------------------------------- //
		{"the author edits their comment", person("jane.doe"),
			authz.ActionPageCommentEdit,
			authz.Object{Kind: authz.KindPage, Author: "jane.doe"},
			true, authz.ReasonAuthor},
		{"somebody else does not", person("sam.rey"),
			authz.ActionPageCommentEdit,
			authz.Object{Kind: authz.KindPage, Author: "jane.doe"},
			false, authz.ReasonNotAuthor},
		// A WORK ITEM'S REMARK IS DECIDED BY THE SAME CLASS, and the seat
		// a person is bound to is their authorship there: a comment a
		// bound person wrote is recorded under their SEAT handle.
		{"a bound person edits the remark their seat wrote",
			personLeading("sre"), authz.ActionWorkCommentEdit,
			authz.Object{Kind: authz.KindTask, Author: "sre"},
			true, authz.ReasonAuthor},
		{"and a colleague does not", personLeading("cto"),
			authz.ActionWorkCommentEdit,
			authz.Object{Kind: authz.KindTask, Author: "sre"},
			false, authz.ReasonNotAuthor},

		// --- a rank move ------------------------------------------------- //
		// THE CAPABILITY THAT FILES WORK ARRANGES IT, and nothing less: a
		// board's order is nobody's object, so the relation is none.
		{"work:write moves a card", person("jane.doe", iam.GrantWorkWrite),
			authz.ActionWorkRank, authz.Object{Kind: authz.KindTask},
			true, authz.ReasonGrant},
		{"state:read alone does not", person("jane.doe", iam.GrantStateRead),
			authz.ActionWorkRank, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonNoGrant},

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
			d := authz.Decide(t.Context(), c.p, c.action, c.object, nimbus(), decidedAt)
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
		{"project policy", "cto", authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"removal", "cto", authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			was := authz.Decide(t.Context(), seat(c.actor), c.action, c.object, nimbus(), decidedAt)
			is := authz.Decide(t.Context(), seat(c.actor), c.action, c.object, inverted, decidedAt)
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
		{"project policy", authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), seat("cto"), c.action, c.object,
				chart{err: behind}, decidedAt)
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
		{"project policy", authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"removal", authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), admin, c.action, c.object, authz.NoChart{}, decidedAt)
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
		nimbus(), decidedAt)
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

// A REFUSAL NAMES EXACTLY THE GRANTS THAT WOULD HAVE ADMITTED THE CALLER.
//
// [authz.Decision.Grants] is what a surface puts in front of a person refused
// — "this needs fleet:operate" — so it has to be the decision's own account
// of what it consulted, never a guess made beside it. Held here against the
// decision itself, over every verb, every kind of object and every grant: for
// each refusal of a principal carrying nothing, the grants it names are
// precisely those that, carried ALONE, turn the same question into an admit
// on that grant. Both directions, because each fails differently: naming a
// grant that would not have helped sends somebody to ask for authority that
// changes nothing, and leaving one out tells them there is no way in when
// there is.
//
// TWO OBJECTS PER KIND, because several rules refuse an object missing its
// field BEFORE they consult any grant — no capability would change that
// answer, so it names none — and an object with every field filled is the
// one that reaches the relation and the admin path behind it. The chart
// answers every relation with a firm no, so a lead relation never admits
// anybody and never hides a grant.
func TestARefusalNamesExactlyTheGrantsThatWouldHaveAdmittedIt(t *testing.T) {
	t.Parallel()
	nobody := func() iam.Principal { return person("jane.doe") }
	for _, a := range authz.Actions() {
		for _, kind := range authz.ObjectKinds {
			for _, object := range []authz.Object{
				{Kind: kind},
				{Kind: kind, ID: "x", Owner: "somebody.else", Author: "somebody.else",
					Container: "ELSEWHERE", ContainerKind: authz.KindProject},
			} {
				d := authz.Decide(t.Context(), nobody(), a, object, chart{}, decidedAt)
				if d.Unknown() {
					t.Fatalf("%s on %+v was undecidable against a chart that "+
						"answers: %v", a, object, d.Err)
				}
				if d.Allowed {
					continue
				}
				var admits []iam.Grant
				for _, g := range iam.AllGrants {
					alone := person("jane.doe", g)
					got := authz.Decide(t.Context(), alone, a, object, chart{}, decidedAt)
					if got.Allowed && got.Reason == authz.ReasonGrant {
						admits = append(admits, g)
					}
				}
				named := append([]iam.Grant(nil), d.Grants...)
				slices.Sort(named)
				slices.Sort(admits)
				if !slices.Equal(named, admits) {
					t.Errorf("%s on %+v refused (%s) naming %v; the grants "+
						"that would have admitted it alone are %v",
						a, object, d.Reason, named, admits)
				}
			}
		}
	}
}
