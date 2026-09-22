package authz

import (
	"context"

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
)

// Classes are the nine, in declaration order.
var Classes = []Class{
	ClassRead, ClassColleagueWrite, ClassOwnRecord, ClassOwnOrLead,
	ClassContainer, ClassDestructive, ClassPurge, ClassAuthored, ClassOperator,
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
	}
	return ""
}
