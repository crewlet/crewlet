package authz

import (
	"context"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// Class is one authority rule, shared by every action that obeys it.
//
// THE RULES ARE THE CLOSED SET, NOT THE VERBS. There are dozens of verbs and
// nine ways to decide one, so a table keyed on the verb would state nine rules
// dozens of times and drift on whichever copy somebody edited. A verb picks
// its class in [classOf] and the class is written once, here.
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

	// ClassContainer — a project's own policy: its fields, its default
	// assignee, its routing unit, a tag rename. Whoever leads the project,
	// or the admin path.
	ClassContainer Class = "container"

	// ClassChartObject — one object in the org chart's own public half: a
	// unit's name and purpose, a seat's goal and responsibilities.
	// Whoever leads that object, or the admin path.
	//
	// ITS OWN CLASS RATHER THAN [ClassContainer], because the chart holds
	// THREE lead relations and they are three questions: who leads a seat,
	// who leads a unit, and who leads the unit that owns a PROJECT. A
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

	// ClassPurge — a task or page purged beyond recovery. The admin
	// grant AND a principal that is not an agent.
	//
	// THE ONE CLASS THAT REFUSES A CAPABILITY SOMEBODY HOLDS. A seat
	// carrying fleet:operate is an agent that was granted the deployment's
	// controls, which is a legitimate configuration — and an irreversible
	// delete decided inside a turn is not something a model should be able
	// to reach for at all. A person or a machine credential does it.
	ClassPurge Class = "purge"

	// ClassAuthored — editing and removing a page comment. Whoever wrote
	// it, or the admin path.
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
	ClassContainer, ClassChartObject, ClassDestructive, ClassPurge,
	ClassAuthored, ClassOperator, ClassDirectoryRead, ClassDirectorySelf,
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
// THE ADMIN PATH IS CHECKED BEFORE THE CHART, on every class that has one,
// because the chart can fail and the grant cannot: an operator holding
// fleet:operate must not be told "I cannot tell" by a node that is behind.
func Decide(ctx context.Context, p iam.Principal, a Action, o Object, chart Chart) Decision {
	if !p.Stage.MayAct() {
		return Decision{Reason: ReasonStage}
	}
	r, known := rules[a]
	if !known {
		return Decision{Reason: ReasonUnknownAction}
	}
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
		if o.Owner == "" {
			return Decision{Reason: ReasonUnnamed}
		}
		if p.Login != "" && p.Login == o.Owner {
			return Decision{Allowed: true, Reason: ReasonSelf}
		}
		// THE SEAT COUNTS AS THE LOGIN for a principal acting as one.
		// A seat writes its own record as itself, and its login is the
		// machine or person behind it — comparing only the login would
		// refuse every seat its own inbox.
		if p.Seat != "" && p.Seat == o.Owner {
			return Decision{Allowed: true, Reason: ReasonSelf}
		}
		if p.Can(adminGrant) {
			return Decision{Allowed: true, Reason: ReasonGrant}
		}
		if r.class == ClassOwnRecord {
			return Decision{Reason: ReasonNotSelf}
		}
		return leads(ctx, chart, actorOf(p), o.Owner, ReasonNotSelf)

	case ClassContainer, ClassDestructive:
		if p.Can(adminGrant) {
			return Decision{Allowed: true, Reason: ReasonGrant}
		}
		if o.Container == "" {
			return Decision{Reason: ReasonUnnamed}
		}
		return leadsProject(ctx, chart, actorOf(p), o.Container)

	case ClassChartObject:
		if p.Can(adminGrant) {
			return Decision{Allowed: true, Reason: ReasonGrant}
		}
		// THE KIND PICKS THE RELATION. See the class's own doc for why
		// one relation cannot serve for all three, and what asking the
		// wrong one costs.
		switch o.Kind {
		case KindUnit:
			if o.Container == "" {
				return Decision{Reason: ReasonUnnamed}
			}
			return leadsUnit(ctx, chart, actorOf(p), o.Container)
		case KindPerson:
			if o.Owner == "" {
				return Decision{Reason: ReasonUnnamed}
			}
			// NO SELF PATH, which is what makes this different from
			// [ClassOwnOrLead]: a seat rewriting its own goal, its
			// backstory and its responsibilities is a model editing
			// the prompt it is about to run under, and nobody asked
			// for that. Its LEAD edits it.
			return leads(ctx, chart, actorOf(p), o.Owner, ReasonNotLead)
		}
		return Decision{Reason: ReasonUnnamed}

	case ClassPurge:
		// THE CAPABILITY IS NOT ENOUGH ON ITS OWN, and the order says
		// which refusal a reader gets: an agent holding the grant is
		// told it is a seat, not that it lacks a capability it plainly
		// has.
		if p.Kind == iam.KindSeat {
			return Decision{Reason: ReasonSeatRefused}
		}
		return granted(p, adminGrant)

	case ClassAuthored:
		if o.Author == "" {
			return Decision{Reason: ReasonUnnamed}
		}
		if p.Login != "" && p.Login == o.Author {
			return Decision{Allowed: true, Reason: ReasonAuthor}
		}
		if p.Seat != "" && p.Seat == o.Author {
			return Decision{Allowed: true, Reason: ReasonAuthor}
		}
		if p.Can(adminGrant) {
			return Decision{Allowed: true, Reason: ReasonGrant}
		}
		return Decision{Reason: ReasonNotAuthor}

	case ClassOperator:
		return granted(p, r.grant)

	case ClassDirectoryRead, ClassDirectorySelf:
		// THE PERSON THEMSELVES FIRST, compared on the ID and never on
		// a name. An object naming nobody — a listing — cannot reach
		// this arm, which is correct: a listing is not about anybody
		// in particular, so it falls through to the capabilities.
		if o.Owner != "" && p.ID != uuid.Nil && p.ID.String() == o.Owner {
			return Decision{Allowed: true, Reason: ReasonSelf}
		}
		if p.Can(iam.GrantPeopleManage) {
			return Decision{Allowed: true, Reason: ReasonGrant}
		}
		if r.class == ClassDirectoryRead && p.Can(iam.GrantAuditRead) {
			return Decision{Allowed: true, Reason: ReasonGrant}
		}
		// NO GRANT rather than NOT SELF, because the capability is what
		// almost every caller here is missing and the self arm is the
		// exception — telling an administrator without people:manage
		// that they "are not that person" sends them to the wrong fix.
		return Decision{Reason: ReasonNoGrant}
	}
	// UNREACHABLE WHILE Classes AND THIS SWITCH AGREE, which a test in
	// this package asserts in both directions. It is a refusal rather
	// than a panic for the reason the unknown action above is: the safe
	// answer to "I have no rule" is no.
	return Decision{Reason: ReasonUnknownAction}
}

// granted is the capability check, with the two reasons it produces.
func granted(p iam.Principal, g iam.Grant) Decision {
	if g != "" && p.Can(g) {
		return Decision{Allowed: true, Reason: ReasonGrant}
	}
	return Decision{Reason: ReasonNoGrant}
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
// THE OBJECT DECIDES, not the verb, because the split the closed ten makes is
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
		// ClassContainer and ClassOperator instead.
		return iam.GrantConfigWrite
	}
	return ""
}
