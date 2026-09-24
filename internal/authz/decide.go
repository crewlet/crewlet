package authz

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// Class is one authority rule, shared by every action that obeys it.
//
// THE RULES ARE THE CLOSED SET, NOT THE VERBS. There are dozens of verbs and
// thirteen ways to decide one, so a table keyed on the verb would state
// thirteen rules dozens of times and drift on whichever copy somebody edited.
// A verb picks its class in [rules] and the class is written once, here.
type Class string

const (
	// ClassRead — the ordinary read: the board, the pages, the org chart,
	// the roster, the fleet, spend.
	//
	// NO RELATION, WHICH IS WHAT MAKES IT THE OPEN ONE — but the state
	// grant all the same. The design this implements called this class
	// "any authenticated principal", and that was written against the
	// removal of anonymous reads rather than against a vocabulary that
	// has a read grant in it. Taken literally it makes
	// [iam.GrantStateRead] open nothing at all: a service account minted
	// to file bugs would read the whole board, every prompt-adjacent
	// surface and the fleet, and an operator narrowing it to work:write
	// would find the narrowing did nothing. This package's own walk over
	// [iam.AllGrants] is what caught that.
	ClassRead Class = "read"

	// ClassSelf — the acting principal's OWN working state: its diary, its
	// episodes, its skills, its onboarding marker, a tool skill it loads
	// into this session.
	//
	// NO ADMIN PATH, which is what makes it its own class rather than
	// [ClassOwnRecord]. internal/agent/builtin states the boundary these
	// verbs rest on: they take no handle at all, because "an agent
	// recalling another's episodes or writing into another's diary would
	// make the per-seat memory a shared one". An operator READING a seat's
	// memory is a different surface with a grant of its own
	// ([iam.GrantAuditRead] over /agents/{id}/memory), and admitting
	// it here too would be a second answer to one question — which is the
	// drift this whole package exists to remove.
	ClassSelf Class = "self"

	// ClassColleagueWrite — the ordinary work of a colleague: filing,
	// commenting, updating, ranking, relating; authoring a page. Governed
	// by the capability rather than by any relation, because a company
	// that hands somebody work:write is saying exactly this.
	ClassColleagueWrite Class = "colleague_write"

	// ClassOwnRecord — a person's own inbox and pins. The owner, or the
	// admin path that lets an operator unstick a departed person's queue.
	//
	// NO LEAD PATH. A lead may re-order what their report works on
	// ([ClassOwnOrLead]); marking somebody's mail read is not the same
	// gesture and nobody asked for it.
	ClassOwnRecord Class = "own_record"

	// ClassOwnOrLead — a person's priorities and their day. The owner,
	// whoever leads them, or the admin path.
	ClassOwnOrLead Class = "own_or_lead"

	// ClassSavedView — a saved view, whose authority follows what the view
	// IS rather than who is asking. A view naming an OWNER is that
	// person's own strip and appears in nobody else's, so it is their
	// record; a view naming none is SHARED on its container — everybody
	// sees it as a tab, and `default` takes the container's landing tab
	// from whichever view held it — so it is that container's lead.
	//
	// ONE CLASS AND NOT TWO VERBS, which is the whole reason it exists:
	// the table is keyed on the ACTION, so two verbs would let the CALLER
	// choose which question is asked, and a caller writing a shared view
	// would ask the personal one. The payload decides, and there is
	// nothing to pick.
	//
	// It reads the container's KIND for [ClassChartObject]'s reason — a
	// project key and a unit key are two relations — and the WORKSPACE
	// container reaches no relation at all, so a company-wide tab is the
	// admin path alone.
	ClassSavedView Class = "saved_view"

	// ClassContainer — a container's own policy. For a PROJECT: its
	// fields, its default assignee, a task's routing unit, a tag rename, the
	// project's archive. For a PAGE CONTAINER: its own settings and a page's
	// rename. Whoever leads that container, or the admin path — and the
	// object's KIND picks which of the two relations is asked.
	ClassContainer Class = "container"

	// ClassChartObject — one object in the org chart's own public half: a
	// unit's name and purpose, a seat's goal and responsibilities.
	// Whoever leads that object, or the admin path.
	//
	// ITS OWN CLASS RATHER THAN [ClassContainer], because the chart holds
	// FOUR lead relations and they are four questions: who leads a seat,
	// who leads a unit, who leads the unit that owns a PROJECT, and who
	// leads the unit that owns a PAGE CONTAINER. A
	// project is a tracker key a unit may declare, so asking the project
	// relation with a unit key matches only a company whose unit files its
	// work under a project of the same name — and answers false everywhere
	// else, refusing the lead of that very unit with no error to notice.
	//
	// It reads the object's KIND to pick the relation, which is why a
	// chart route states one: a unit names itself in [Object.Container]
	// and a seat in [Object.Owner].
	ClassChartObject Class = "chart_object"

	// ClassDestructive — removing and restoring a task, trashing and
	// restoring a page. The CONTAINER's lead, or the admin path: a
	// colleague may file work in a project and may not take it out again.
	ClassDestructive Class = "destructive"

	// ClassAuthored — editing and removing a comment, on a page or on a
	// work item. Whoever wrote it, or the admin path.
	ClassAuthored Class = "authored"

	// ClassOperator — the company's own controls: configuration, secrets,
	// integrations, the node. The action names the grant it needs, because
	// these are the surfaces where the grants differ from each other by
	// design — reading a credential and rotating one are opposite risks.
	ClassOperator Class = "operator"

	// ClassDirectoryRead — who can reach this company, and how: the
	// directory listing, one person's row, the credentials that exist, the
	// sessions that are open, the estate's own report.
	//
	// THREE WAYS IN, and each is a different party asking the same
	// question. The person the row is ABOUT, because somebody must be able
	// to see what they themselves carry; whoever holds
	// [iam.GrantPeopleManage], because they are the party that writes it;
	// and whoever holds [iam.GrantAuditRead], because "who can reach this
	// company" is the audit question and an auditor who could not ask it
	// could not audit anything.
	//
	// THE OBJECT IS A PERSON ID, never a login or a handle — this is the
	// one place in the table where that is true, and it is forced: the
	// identity estate keys on an id precisely because a person changes
	// their login, and a self check against a mutable name would open
	// somebody else's row the day they swapped.
	ClassDirectoryRead Class = "directory_read"

	// ClassDirectorySelf — a gesture on one person's own credentials or
	// sessions: minting a token, revoking one, ending every session.
	// The person themselves, or whoever manages people.
	//
	// NO AUDIT PATH, which is what makes it its own class rather than
	// [ClassDirectoryRead]: an auditor establishes what a company's access
	// looks like and never changes it, and a grant that could end a
	// session is a grant that can lock a company out of its own engine.
	ClassDirectorySelf Class = "directory_self"
)

// Classes are the thirteen, in declaration order.
var Classes = []Class{
	ClassRead, ClassSelf, ClassColleagueWrite, ClassOwnRecord, ClassOwnOrLead,
	ClassSavedView, ClassContainer, ClassChartObject, ClassDestructive,
	ClassAuthored, ClassOperator, ClassDirectoryRead,
	ClassDirectorySelf,
}

// Decide answers whether p may do a to o.
//
// # The order of the checks is the contract
//
// STAGE FIRST, because a principal part-way through enrolment holds grants it
// may not yet use, and every rule below would happily honour them.
//
// THEN THE CLASS, and an action with no class is REFUSED rather than allowed
// or passed through. It is the one refusal that is a build mistake rather than
// an authority fact: a verb reached this function without a row, and the
// alternative — a default — is how a new verb ships ungated. The completeness
// walk in this package's tests is what makes that refusal a build failure
// instead of a production one, and [Router.Handle] refuses such an action at
// MOUNT time so no HTTP route can reach this arm at all.
//
// IT CARRIES NO ERROR, which is the opposite of what it first looked like it
// wanted. An error would make [Decision.Unknown] true, and unknown means "ask
// me again" — so a verb with no rule would answer 503 to a request that can
// never succeed, on every retry, for ever. The reason is a closed value a
// caller can branch on; that is what it is for.
//
// THEN THE HUMAN-ONLY BAR, which is a fact about the VERB and therefore
// decided before any class reads any relation — see [rule.humanOnly].
//
// THE ADMIN PATH IS CHECKED BEFORE THE CHART, on every class that has one,
// because the chart can fail and the grant cannot: an operator holding
// fleet:operate must not be told "I cannot tell" by a node that is behind.
//
// AND THE PROOF LAST, at now: a verb whose row asks for a recent proof of
// identity refuses an ADMITTED principal whose proof is older than that
// window ([ReasonStepUp], naming the window in [Decision.Recency]). Last,
// because every earlier answer is one a fresher proof would not change.
func Decide(ctx context.Context, p iam.Principal, a Action, o Object, chart Chart,
	now time.Time) Decision {

	if !p.Stage.MayAct() {
		return Decision{Reason: ReasonStage}
	}
	r, known := rules[a]
	if !known {
		return Decision{Reason: ReasonUnknownAction}
	}
	// AND AN AGENT NEVER TAKES A HUMAN-ONLY VERB, checked before the
	// class so the refusal names what it actually is: an agent holding
	// the grant is told it is a seat, not that it lacks a capability it
	// plainly has. See [rule.humanOnly].
	if r.humanOnly && p.Kind == iam.KindSeat {
		return Decision{Reason: ReasonSeatRefused}
	}
	d := decideClass(ctx, p, r, o, chart)
	// THE PROOF LAST, and only over an ADMISSION: a refusal is already
	// the answer, and an unknown is already "ask me again". See the
	// package doc and [rule.recency].
	if d.Unknown() || !d.Allowed {
		return d
	}
	need := recencyFor(r, d)
	if p.Proved(need, now) {
		return d
	}
	return Decision{Reason: ReasonStepUp, Recency: need, Grants: d.Grants}
}

// recencyFor is the proof a row asks of the arm that admitted d: the row's own
// window, or its self arm's where the row states one ([rule.selfRecency]).
//
// THE ARM IS READ OFF THE DECISION'S REASON, which is what the class
// concluded — never off the object, which says what was named and not which
// rule let the caller through: an administrator naming themselves is admitted
// as themselves, and asked what anybody is asked about their own record.
func recencyFor(r rule, d Decision) iam.Recency {
	if d.Reason == ReasonSelf && r.selfRecency != "" {
		return r.selfRecency
	}
	return r.recency
}

// decideClass is the class half of [Decide]: the one rule a row names, asked
// of this principal and this object, with every precondition [Decide] owns
// already settled.
func decideClass(ctx context.Context, p iam.Principal, r rule, o Object,
	chart Chart) Decision {

	switch r.class {
	case ClassRead:
		return granted(p, iam.GrantStateRead)

	case ClassSelf:
		// THE OBJECT IS THE CALLER, OR IT IS NOBODY. These verbs take no
		// handle, so an absent owner is not a caller who forgot to name
		// one — it is the shape of the verb, and reading it as
		// [ReasonUnnamed] would refuse every one of them. An owner that
		// names somebody else can only come from a caller inventing one.
		switch {
		case o.Owner == "":
			return Decision{Allowed: true, Reason: ReasonSelf}
		case p.Login != "" && p.Login == o.Owner,
			p.Seat != "" && p.Seat == o.Owner:
			return Decision{Allowed: true, Reason: ReasonSelf}
		}
		return Decision{Reason: ReasonNotSelf}

	case ClassColleagueWrite:
		return granted(p, writeGrantFor(o.Kind))

	case ClassOwnRecord, ClassOwnOrLead:
		// NO GRANT ON THE UNNAMED REFUSAL: it is taken before the admin
		// path is consulted, so no capability would have changed it.
		if o.Owner == "" {
			return Decision{Reason: ReasonUnnamed}
		}
		if p.Login != "" && p.Login == o.Owner {
			return consulting(Decision{Allowed: true, Reason: ReasonSelf}, adminGrant)
		}
		// THE SEAT COUNTS AS THE LOGIN for a principal acting as one.
		// A seat writes its own record as itself, and its login is the
		// machine or person behind it — comparing only the login would
		// refuse every seat its own inbox.
		if p.Seat != "" && p.Seat == o.Owner {
			return consulting(Decision{Allowed: true, Reason: ReasonSelf}, adminGrant)
		}
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		if r.class == ClassOwnRecord {
			return consulting(Decision{Reason: ReasonNotSelf}, adminGrant)
		}
		if o.Unresolved {
			// NOBODY KNOWS WHOSE RECORD THIS IS YET — see
			// [Object.Unresolved] — and the only admission left is
			// leading whoever turns out to hold it.
			return consulting(leadsSomebody(ctx, chart, actorOf(p)), adminGrant)
		}
		return consulting(leads(ctx, chart, actorOf(p), o.Owner, ReasonNotSelf),
			adminGrant)

	case ClassSavedView:
		// EVERY ANSWER BELOW IS ONE THE ADMIN GRANT WOULD HAVE
		// OVERRIDDEN, because it is consulted first.
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		// A PERSONAL VIEW IS A RECORD AND NOT A TAB, so it takes
		// [ClassOwnRecord]'s answer and not [ClassOwnOrLead]'s: a lead
		// re-orders what their report works on, and rearranging the
		// tabs above it is a gesture nobody asked a lead to make.
		if o.Owner != "" {
			if p.Login != "" && p.Login == o.Owner ||
				p.Seat != "" && p.Seat == o.Owner {

				return consulting(Decision{Allowed: true, Reason: ReasonSelf}, adminGrant)
			}
			return consulting(Decision{Reason: ReasonNotSelf}, adminGrant)
		}
		if o.Container == "" {
			return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)
		}
		switch o.ContainerKind {
		case KindProject:
			return consulting(leadsProject(ctx, chart, actorOf(p), o.Container), adminGrant)
		case KindUnit:
			return consulting(leadsUnit(ctx, chart, actorOf(p), o.Container), adminGrant)
		case KindPerson:
			// A SHARED VIEW ON SOMEBODY'S PAGE, which is theirs and
			// their lead's — the opposite of the personal arm above,
			// because this one is a tab on a page other people read
			// rather than a strip only its owner sees.
			if p.Login != "" && p.Login == o.Container ||
				p.Seat != "" && p.Seat == o.Container {

				return consulting(Decision{Allowed: true, Reason: ReasonSelf}, adminGrant)
			}
			return consulting(leads(ctx, chart, actorOf(p), o.Container, ReasonNotSelf), adminGrant)
		}
		// THE WORKSPACE, and anything this build does not know. A tab
		// every person in the company lands on is the admin path,
		// which the grant check above is.
		return consulting(Decision{Reason: ReasonNotLead}, adminGrant)

	case ClassContainer, ClassDestructive:
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		if o.Container == "" {
			return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)
		}
		// THE KIND PICKS THE RELATION, for [ClassChartObject]'s reason
		// and [Chart.LeadsContainer]'s: a page container and a tracker
		// project are two key classes a unit declares in two fields,
		// and one relation asked with the other's key matches only a
		// company that spelled them the same.
		if o.Kind == KindContainer || o.Kind == KindPage {
			return consulting(leadsContainer(ctx, chart, actorOf(p), o.Container), adminGrant)
		}
		return consulting(leadsProject(ctx, chart, actorOf(p), o.Container), adminGrant)

	case ClassChartObject:
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		// THE KIND PICKS THE RELATION. See the class's own doc for why
		// one relation cannot serve for all three, and what asking the
		// wrong one costs.
		switch o.Kind {
		case KindUnit:
			if o.Container == "" {
				return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)
			}
			return consulting(leadsUnit(ctx, chart, actorOf(p), o.Container), adminGrant)
		case KindPerson:
			if o.Owner == "" {
				return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)
			}
			// NO SELF PATH, which is what makes this different from
			// [ClassOwnOrLead]: a seat rewriting its own goal, its
			// backstory and its responsibilities is a model editing
			// the prompt it is about to run under, and nobody asked
			// for that. Its LEAD edits it.
			return consulting(leads(ctx, chart, actorOf(p), o.Owner, ReasonNotLead), adminGrant)
		}
		return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)

	case ClassAuthored:
		if o.Author == "" {
			return Decision{Reason: ReasonUnnamed}
		}
		if p.Login != "" && p.Login == o.Author {
			return consulting(Decision{Allowed: true, Reason: ReasonAuthor}, adminGrant)
		}
		if p.Seat != "" && p.Seat == o.Author {
			return consulting(Decision{Allowed: true, Reason: ReasonAuthor}, adminGrant)
		}
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		return consulting(Decision{Reason: ReasonNotAuthor}, adminGrant)

	case ClassOperator:
		return granted(p, r.grant)

	case ClassDirectoryRead, ClassDirectorySelf:
		// THE TWO WAYS IN BY CAPABILITY, and the read has one more than
		// the gesture: an auditor establishes who can reach a company
		// and never changes it.
		ways := []iam.Grant{iam.GrantPeopleManage}
		if r.class == ClassDirectoryRead {
			ways = append(ways, iam.GrantAuditRead)
		}
		// THE PERSON THEMSELVES FIRST, compared on the ID and never on
		// a name. An object naming nobody — a listing — cannot reach
		// this arm, which is correct: a listing is not about anybody
		// in particular, so it falls through to the capabilities.
		if o.Owner != "" && p.ID != uuid.Nil && p.ID.String() == o.Owner {
			return consulting(Decision{Allowed: true, Reason: ReasonSelf}, ways...)
		}
		for _, g := range ways {
			if p.Can(g) {
				return consulting(Decision{Allowed: true, Reason: ReasonGrant}, ways...)
			}
		}
		// NO GRANT rather than NOT SELF, because the capability is what
		// almost every caller here is missing and the self arm is the
		// exception — telling an administrator without people:manage
		// that they "are not that person" sends them to the wrong fix.
		return consulting(Decision{Reason: ReasonNoGrant}, ways...)
	}
	// UNREACHABLE WHILE Classes AND THIS SWITCH AGREE, which a test in
	// this package asserts in both directions. It is a refusal rather
	// than a panic for the reason the unknown action above is: the safe
	// answer to "I have no rule" is no.
	return Decision{Reason: ReasonUnknownAction}
}

// granted is the capability check, with the two reasons it produces.
//
// An EMPTY grant names nothing in [Decision.Grants], because there is no
// capability that would have admitted anybody: it is a gate somebody forgot
// to fill in, and a refusal naming "" as the remedy would send a reader to
// ask for a grant that does not exist.
func granted(p iam.Principal, g iam.Grant) Decision {
	if g == "" {
		return Decision{Reason: ReasonNoGrant}
	}
	if p.Can(g) {
		return consulting(Decision{Allowed: true, Reason: ReasonGrant}, g)
	}
	return consulting(Decision{Reason: ReasonNoGrant}, g)
}

// consulting records which capabilities the rule that decided would have
// admitted the principal on — see [Decision.Grants].
//
// ONE HELPER RATHER THAN A FIELD SET AT EACH RETURN, so an arm reads as the
// rule it is and the grant list is stated once per arm rather than once per
// outcome. The slice is always fresh: a caller appending to one decision's
// grants must not be editing another's.
func consulting(d Decision, grants ...iam.Grant) Decision {
	d.Grants = append([]iam.Grant(nil), grants...)
	return d
}

// leadsUnit asks the chart whether the actor leads one unit.
//
// THE SAME THREE-VALUED SHAPE [leadsProject] has, and written beside it
// rather than folded into it with a flag: the two ask different questions of
// the chart, and a parameter choosing between them is a call site that can
// pass the wrong one.
func leadsUnit(ctx context.Context, chart Chart, actor, unit string) Decision {
	if actor == "" {
		return Decision{Reason: ReasonNotLead}
	}
	if chart == nil {
		return Decision{Reason: ReasonNotLead, Err: ErrNoChart}
	}
	leads, err := chart.LeadsUnit(ctx, actor, unit)
	switch {
	case err != nil:
		return Decision{Reason: ReasonNotLead, Err: err}
	case leads:
		return Decision{Allowed: true, Reason: ReasonLead}
	}
	return Decision{Reason: ReasonNotLead}
}

// leads asks the chart, and turns a chart that could not answer into UNKNOWN.
//
// refusal is the reason a `false` carries, which differs by class: not leading
// somebody's priorities is "not yours and you do not lead them", where the
// container classes have only the one relation to report.
func leads(ctx context.Context, chart Chart, actor, subject string, refusal Reason) Decision {
	if actor == "" {
		return Decision{Reason: refusal}
	}
	if chart == nil {
		return Decision{Reason: ReasonNotLead, Err: ErrNoChart}
	}
	ok, err := chart.Leads(ctx, actor, subject)
	switch {
	case err != nil:
		return Decision{Reason: ReasonNotLead, Err: err}
	case ok:
		return Decision{Allowed: true, Reason: ReasonLead}
	}
	return Decision{Reason: refusal}
}

// leadsSomebody is [leads] for an owner nobody has resolved yet: a caller who
// leads nobody is refused EXACTLY as [leads] refuses them on a seat they do
// not lead — the same reason, and the same grants once the class adds them —
// so the refusal cannot say whether the name was ever looked up; a caller who
// leads somebody is told to resolve the name and ask again ([ErrUnresolved]);
// and a chart that cannot say is unknown, as it is everywhere.
func leadsSomebody(ctx context.Context, chart Chart, actor string) Decision {
	if actor == "" {
		return Decision{Reason: ReasonNotSelf}
	}
	if chart == nil {
		return Decision{Reason: ReasonNotLead, Err: ErrNoChart}
	}
	ok, err := chart.LeadsAnyone(ctx, actor)
	switch {
	case err != nil:
		return Decision{Reason: ReasonNotLead, Err: err}
	case ok:
		return Decision{Reason: ReasonNotLead, Err: ErrUnresolved}
	}
	return Decision{Reason: ReasonNotSelf}
}

// leadsContainer is [leads] over a page container key.
func leadsContainer(ctx context.Context, chart Chart, actor, container string) Decision {
	if actor == "" {
		return Decision{Reason: ReasonNotLead}
	}
	if chart == nil {
		return Decision{Reason: ReasonNotLead, Err: ErrNoChart}
	}
	ok, err := chart.LeadsContainer(ctx, actor, container)
	switch {
	case err != nil:
		return Decision{Reason: ReasonNotLead, Err: err}
	case ok:
		return Decision{Allowed: true, Reason: ReasonLead}
	}
	return Decision{Reason: ReasonNotLead}
}

// leadsProject is [leads] over a project key.
func leadsProject(ctx context.Context, chart Chart, actor, project string) Decision {
	if actor == "" {
		return Decision{Reason: ReasonNotLead}
	}
	if chart == nil {
		return Decision{Reason: ReasonNotLead, Err: ErrNoChart}
	}
	ok, err := chart.LeadsProject(ctx, actor, project)
	switch {
	case err != nil:
		return Decision{Reason: ReasonNotLead, Err: err}
	case ok:
		return Decision{Allowed: true, Reason: ReasonLead}
	}
	return Decision{Reason: ReasonNotLead}
}

// actorOf is the handle the CHART knows this principal by.
//
// THE SEAT, FALLING BACK TO THE LOGIN. A chart holds seats: a person bound to
// one is asked about by that seat's handle, and an unbound person is asked
// about by their login, which the chart will not find — answering false,
// correctly, because they lead nobody in it.
func actorOf(p iam.Principal) string {
	if p.Seat != "" {
		return p.Seat
	}
	return p.Login
}

// writeGrantFor is which capability covers writing this kind of thing.
//
// THE OBJECT DECIDES, not the verb, because the split the closed eleven makes is
// by SURFACE: a company routinely wants an automation that files bugs and may
// not edit the handbook. A kind with no write grant answers the zero, which
// [granted] refuses — the same shape as a gate somebody forgot to fill in.
func writeGrantFor(k ObjectKind) iam.Grant {
	switch k {
	case KindTask, KindProject, KindPerson:
		return iam.GrantWorkWrite
	case KindPage, KindContainer:
		return iam.GrantKnowledgeWrite
	case KindUnit:
		// A UNIT IS THE COMPANY'S OWN SHAPE, not a colleague's work, so
		// no colleague-write verb takes one — and stating the grant
		// anyway is the difference between a kind that is unreachable
		// here and one that falls through to the empty gate below by
		// accident. Chart writes reach their authority through
		// ClassChartObject and ClassOperator instead.
		return iam.GrantConfigWrite
	}
	return ""
}
