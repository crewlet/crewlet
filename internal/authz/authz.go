// Package authz decides whether a principal may do a thing to an object.
//
// ONE FUNCTION AND ONE TABLE. [Decide] is the whole surface, and every
// authority rule this engine has is a row in [rules] beside the verb it
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
// opinion about the hierarchy. So the chart is a four-method seam the consumer
// implements, and every other input is a value a test can write down. A rule
// exercised only through a running engine is a rule nobody re-reads.
//
// THE INSTANT IS AN ARGUMENT TOO, for the same reason. Some verbs ask how
// RECENTLY the principal proved who they are — see below — and a Decide that
// read the clock itself could not be asked about a proof forty minutes old
// without waiting forty minutes. The caller passes when it is deciding; an
// HTTP guard passes the request's own instant.
//
// # How recently somebody proved who they are
//
// A session lives for days and a laptop is left unlocked, so the gestures that
// change what a company IS ask for a proof taken minutes ago rather than on
// Monday: that is the STEP-UP, and every row states how recent a proof it asks
// for ([iam.Recency]) — none, `step_up` (an hour by default: the company's
// configuration, chart, integrations and credential writes, the identity
// directory's writes and the deployment's own controls) or `step_up_sensitive`
// (fifteen minutes: revealing a secret, changing what somebody already
// enrolled may do or how they prove who they are — whichever surface the
// change comes through — and ending every session in the company). Those are
// the design's windows: the sensitive one is kept for the gestures that hand
// over a value or an authority somebody holds, and asking it of every
// directory write sent an administrator back to re-prove for each invitation.
//
// IT IS DECIDED HERE, ON THE ROW, and nowhere else. It was a setting nothing
// read: the sign-in surface could record a proof and no surface outside it ever
// asked for one, so a cookie from last week reached every one of those
// gestures. On the row, a REST route and a tool asking about one verb get one
// answer, and a walk holds every row to having decided — a zero recency is
// refused, because read as "none" it is a sensitive verb that shipped open.
//
// A ROW MAY ASK ITS SELF ARM A DIFFERENT WINDOW from every other arm, read off
// the reason the class admitted on: ending every session somebody holds asks
// nothing of the person themselves — the first thing they do on finding an
// intruder — and the ordinary window of an administrator ending somebody
// else's, which stops every pipeline that colleague runs as well.
//
// THE PROOF IS ASKED AFTER THE RULE ADMITS, so a caller who could never take
// the verb is told what they lack rather than sent to confirm their identity
// first, and the refusal ([ReasonStepUp]) names the window it needs so a
// client can ask the person and replay the request. Who counts as having
// proved is the principal's business ([iam.Principal.Proved]): a session
// proved when it signed in or stepped up, while a credential with nobody at a
// keyboard — a Tier A token, a machine token, the development principal — is
// fresh by construction, because there is nothing else it could ever present.
// No TOOL asks for a proof (a walk holds that too): a seat has no keyboard, and
// the operator's MCP surface is not a step-up surface.
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
	// KindView is a saved view — a named query with a shape, which is
	// EITHER one person's strip or a container's shared tab depending on
	// whether it names an owner. Its own kind rather than [KindPerson],
	// because the personal kind is where an absent owner is read as the
	// CALLER, and a view with no owner is shared rather than mine.
	KindView ObjectKind = "view"
	// KindCompany is the company itself — its configuration, its
	// credentials, its integrations, the node it runs on. The operator
	// surfaces, which name their own grant on the action.
	KindCompany ObjectKind = "company"
)

// ObjectKinds are every kind, in declaration order.
var ObjectKinds = []ObjectKind{
	KindTask, KindProject, KindUnit, KindPage, KindContainer, KindPerson,
	KindView, KindCompany,
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

	// Unresolved says Owner is a name the caller TYPED that nobody has
	// resolved to the record it addresses yet: somebody else's LOGIN,
	// which the identity directory resolves to the record its holder acts
	// under — a bound person's seat, or the login itself.
	//
	// DECIDED BEFORE THE DIRECTORY IS ASKED, which is the whole reason it
	// exists. Asked after, the directory's answer reached callers it could
	// never admit: a login nobody holds was refused on the name as typed
	// while a held one this node could not resolve answered 503 carrying
	// the seat it was bound to, so a caller with no authority over anybody
	// learnt which logins exist and whose seat each holds from the
	// difference. Decided first, the admin grant and the caller's own login
	// admit as they would on the record, a class with no lead path refuses,
	// a caller who leads nobody is refused exactly as they would be on
	// somebody's seat they do not lead — so what the directory says is
	// never theirs to learn — and a caller who leads somebody is answered
	// [ErrUnresolved]: resolve the login, then decide again on the record.
	//
	// Read by the two personal classes and by nothing else.
	Unresolved bool

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

	// ContainerKind is what sort of thing Container names, for the one
	// class whose object is neither the container nor governed by a
	// single relation: a saved view sits on a project, a unit, a person
	// or the workspace, and those are four different questions.
	//
	// SEPARATE FROM Kind rather than overloading it, because Kind is what
	// the object IS — [ClassColleagueWrite] picks a write grant from it —
	// and a view whose Kind said "project" would be a view the colleague
	// rules read as a project. Empty on every other class, which reads
	// Kind and never this.
	ContainerKind ObjectKind
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

	// Grants are the capabilities the deciding rule would have admitted
	// THIS principal on, for THIS object — any one of them is enough.
	// The capability a grant rule asks for, the admin grant a relation
	// rule is overridden by, both directory grants a directory read
	// accepts. Empty where no capability could have changed the answer:
	// the self rule, which has no admin path, an object missing the field
	// its rule reads before it consults any grant, and the refusals that
	// are not about capability at all — an enrolment stage, a seat taking
	// a human-only verb, a verb with no rule.
	//
	// ON THE DECISION rather than looked up beside it, because a refusal
	// that names what would have admitted the caller has to name what the
	// rule that decided actually consulted: a second function answering
	// "what does this verb need" is a second copy of [Decide]'s switch,
	// and it is the copy that drifts. A walk in this package's tests holds
	// the two together over every verb, kind and grant.
	Grants []iam.Grant

	// Recency is the proof the deciding rule asked for, set on a
	// [ReasonStepUp] refusal and on nothing else: the rule would have
	// admitted this principal, and their proof of who they are is older
	// than the window this names. It is what a client needs in order to
	// ask the person to confirm who they are and replay the request, which
	// is why it rides on the decision rather than being looked up beside
	// it.
	Recency iam.Recency
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
	// See [rule.humanOnly].
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
	// ReasonStepUp is the one refusal the principal clears themselves: the
	// rule ADMITS them, and their proof of who they are is older than the
	// window the verb asks for ([Decision.Recency]). Confirming who they
	// are and asking again is the whole remedy — no grant would change it.
	ReasonStepUp Reason = "step_up"
)

// Reasons are every one, in declaration order.
var Reasons = []Reason{
	ReasonGrant, ReasonSelf, ReasonAuthor, ReasonLead,
	ReasonNoGrant, ReasonNotSelf, ReasonNotLead, ReasonNotAuthor,
	ReasonSeatRefused, ReasonStage, ReasonUnnamed, ReasonUnknownAction,
	ReasonStepUp,
}

// Valid reports whether a reason is one this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// adminGrant is the capability that overrides every relation-based class.
//
// [iam.GrantFleetOperate] rather than a grant of its own, because that is what
// the closed eleven has for "whoever runs this deployment" and a second admin
// grant beside it would be a second answer to one question. Its own name here
// so the rules that use it say ADMIN where they mean it.
//
// AND NOT [iam.GrantPeopleManage], although that grant exists and its name
// reads like the right one for a person's queue. It is authority over PERSON
// ROWS in the identity estate — who is enrolled and what they carry — and the
// directory classes are where it is asked. A person's work record, their inbox
// and their day are the TRACKER's, and the admin path over those is whoever
// runs the deployment: an administrator who onboards a team has no business
// reading everybody's inbox by virtue of it, and folding the two would make
// the grant that can grant also the grant that reads.
const adminGrant = iam.GrantFleetOperate
