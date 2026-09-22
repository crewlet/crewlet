// Package authz decides whether a principal may do a thing to an object.
//
// ONE FUNCTION AND ONE TABLE. [Decide] is the whole surface, and every
// authority rule this engine has is a row in [classOf] beside the verb it
// governs. What that buys is the thing five hand-written gates could not: a
// seat's own tool, the operator MCP and an HTTP route asking about one verb
// get one answer, because there is one answer to get.
//
// # What it replaces, and what each of those got wrong
//
// Three structs carried authority into the tracker as BOOLS the caller had
// already resolved — PersonAuthority{Lead, Person}, ProjectAuthority{Lead,
// Operator}, TagAuthority. Each was filled by a different surface from a
// different lookup, and each of those lookups answered `false` for a question
// it could not answer:
//
//   - `Person` existed only because an operator's actor was a TOKEN's label,
//     which is not a handle in the chart, so no ancestor walk could ever match
//     it and `Lead` was false for every operator by construction. A login is a
//     handle now ([iam.Principal]), so the flag is the hole rather than the
//     fix.
//   - `LeadsProjectOf` answers false for a project it cannot resolve AND for a
//     node that holds no company at all — a lagging node, one mid-apply, one
//     draining. Those are opposite facts wearing one value: the first is "you
//     do not lead this", the second is "I cannot tell", and collapsing them
//     silently demotes every lead in the company for as long as the node is
//     behind.
//
// So [Chart] is three-valued and [Decision.Err] is its own outcome. A refusal
// is a fact about the principal; an error is a fact about this node, and a
// surface that reported the second as the first would send somebody to ask for
// an authority they already hold.
//
// # It is PURE OVER VALUES, apart from the chart
//
// Everything [Decide] needs arrives as an argument: the principal, the verb,
// the object. The one thing it cannot hold is the org chart — that is a fact
// about the company, it is edited live, and a copy here would be a second
// opinion about the hierarchy. So the chart is a two-method seam the consumer
// implements, and every other input is a value a test can write down. A rule
// exercised only through a running engine is a rule nobody re-reads.
package authz

import (
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// Action is one verb this engine authorizes.
//
// THE VERB, NOT THE ROUTE. `PATCH /work/items/{key}` and `update_work_item`
// are one action asked two ways, and naming the route would make them two —
// which is how the operator MCP and a seat's own tools came to answer
// differently about the same write.
//
// Its zero is invalid, like every named type in [iam]: an action a caller
// forgot to fill in must not resolve to a rule.
type Action string

// ObjectKind is what sort of thing an action acts on.
//
// It is carried on the OBJECT rather than inferred from the action, because
// one class governs several kinds — a destructive verb reaches a task and a
// page — and the rule needs to know which container to ask about.
type ObjectKind string

const (
	// KindTask is one work item.
	KindTask ObjectKind = "task"
	// KindProject is a project and the policy that governs its items.
	KindProject ObjectKind = "project"
	// KindUnit is a team in the org chart, and the container a chart
	// content write is decided against.
	KindUnit ObjectKind = "unit"
	// KindPage is one knowledge page.
	KindPage ObjectKind = "page"
	// KindContainer is a page container.
	KindContainer ObjectKind = "container"
	// KindPerson is a person's own record: their inbox, pins, priorities.
	KindPerson ObjectKind = "person"
	// KindCompany is the company itself — its configuration, its
	// credentials, its integrations, the node it runs on. The operator
	// surfaces, which name their own grant on the action.
	KindCompany ObjectKind = "company"
)

// ObjectKinds are the six, in declaration order.
var ObjectKinds = []ObjectKind{
	KindTask, KindProject, KindUnit, KindPage, KindContainer, KindPerson,
	KindCompany,
}

// Valid reports whether a kind is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Object is the thing being acted on, and the facts the rules ask about it.
//
// EVERY FIELD IS OPTIONAL and the zero is meaningful: a class reads only the
// fields it needs, and a class whose field is empty refuses rather than
// guessing. What is NOT here is anything derivable — the rules ask the chart
// for a lead relation and never carry one, because a caller that resolved it
// would be stating what it read in another snapshot.
type Object struct {
	// Kind is what sort of thing this is.
	Kind ObjectKind

	// ID is the object's own address, for the reason a decision is
	// recorded: a refusal an operator reads names the thing.
	ID string

	// Owner is the login a PERSONAL object belongs to — whose inbox,
	// whose pins, whose priorities. Empty on everything else.
	Owner string

	// Author is the login that WROTE this, for the one class where
	// authorship is the authority: a page comment is edited and removed
	// by whoever left it.
	//
	// Separate from Owner because they are different relations and the
	// same login in both fields is a coincidence: a comment on your own
	// page has one author and no owner at all.
	Author string

	// Container is the project a task is filed under, or the container a
	// page sits in. It is what the container and destructive classes ask
	// the chart about.
	Container string
}

// Decision is what [Decide] concluded.
//
// THREE OUTCOMES IN TWO FIELDS, and the pairing is the contract: Err non-nil
// is UNKNOWN and Allowed is meaningless beside it. A caller that read Allowed
// first would turn "this node cannot tell" into "no", which is the exact
// collapse this package exists to undo.
type Decision struct {
	// Allowed is the answer, and it is only an answer when Err is nil.
	Allowed bool

	// Reason is why, for the log line and the refusal an operator reads.
	// Set on an allow as well as a refusal: "which rule let this through"
	// is the question an audit asks.
	Reason Reason

	// Err is why this node could not decide. NEVER a refusal — see the
	// package doc, and [Decision.Unknown].
	Err error
}

// Unknown reports a decision this node could not reach.
//
// A METHOD rather than a nil check at every call site, because the check is
// the thing callers get wrong: `if !d.Allowed { refuse }` reads correctly and
// is wrong exactly when the store blinked.
func (d Decision) Unknown() bool { return d.Err != nil }

// Reason names the rule that decided, in the words a refusal uses.
//
// A NAMED STRING WITH A CLOSED SET, so a surface rendering one cannot invent a
// reason and a test can assert which rule fired rather than that the answer
// happened to be right. An unknown value off a newer peer's audit row renders
// rather than failing, the same rule [iam.Grant] follows.
type Reason string

const (
	// ReasonGrant is a capability the principal carries.
	ReasonGrant Reason = "grant"
	// ReasonSelf is a personal object the principal owns.
	ReasonSelf Reason = "self"
	// ReasonAuthor is an authored object the principal wrote.
	ReasonAuthor Reason = "author"
	// ReasonLead is a lead relation the chart answered for.
	ReasonLead Reason = "lead"
	// ReasonNoGrant is a refusal: the principal carries no capability
	// that covers this verb.
	ReasonNoGrant Reason = "no_grant"
	// ReasonNotSelf is a refusal: a personal object belonging to somebody
	// else, and no admin path.
	ReasonNotSelf Reason = "not_self"
	// ReasonNotLead is a refusal: the chart answered, and the principal
	// does not lead this.
	ReasonNotLead Reason = "not_lead"
	// ReasonNotAuthor is a refusal: somebody else wrote it.
	ReasonNotAuthor Reason = "not_author"
	// ReasonSeatRefused is a refusal an agent gets and a person does not.
	// See [ClassPurge].
	ReasonSeatRefused Reason = "seat_refused"
	// ReasonStage is a refusal: this principal is not through enrolment,
	// so nothing it asks for is granted yet.
	ReasonStage Reason = "stage"
	// ReasonUnnamed is a refusal: the action named an object the rule
	// needs a field of, and the field is empty. A rule that guessed here
	// would authorize against a container nobody named.
	ReasonUnnamed Reason = "unnamed"
	// ReasonUnknownAction is a refusal: this build has no rule for the
	// verb. It is the one refusal that is a BUILD mistake rather than an
	// authority fact — see [Decide].
	ReasonUnknownAction Reason = "unknown_action"
)

// Reasons are every one, in declaration order.
var Reasons = []Reason{
	ReasonGrant, ReasonSelf, ReasonAuthor, ReasonLead,
	ReasonNoGrant, ReasonNotSelf, ReasonNotLead, ReasonNotAuthor,
	ReasonSeatRefused, ReasonStage, ReasonUnnamed, ReasonUnknownAction,
}

// Valid reports whether a reason is one this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// adminGrant is the capability that overrides every relation-based class.
//
// [iam.GrantFleetOperate] rather than a grant of its own, because that is what
// the closed ten has for "whoever runs this deployment" and a second admin
// grant beside it would be a second answer to one question. Its own name here
// so the eleven rules below say ADMIN where they mean it, and one edit moves
// every one of them the day the vocabulary grows a people-management grant.
const adminGrant = iam.GrantFleetOperate
