// Package authz decides whether a principal may do a thing to an object.
//
// ONE FUNCTION AND ONE TABLE. [Decide] is the whole surface, and every
// authority rule this engine has is a row in [rules] beside the verb it
// governs. What that buys is the thing five hand-written gates could not: a
// seat's own tool, the operator MCP and an HTTP route asking about one verb
// get one answer, because there is one answer to get.
//
// THE VOCABULARY IS ADMIN AND ONE PERMISSION PER APP — ADR-0033, whose
// authority this package is: see "Admin, and one permission per app" below.
//
// AN ARTIFACT IS SHARED, AND A SHARE IS A RELATION: ADR-0034, whose authority
// this package also is. The artifacts app gates authoring only; every artifact
// row is decided by one class over a five-role ladder, the lead arm opens only
// an agent owner's artifacts, and a caller with no relation is answered as if
// the artifact did not exist.
//
// THE DASHBOARD ASSISTANT IS DECIDED HERE TOO: ADR-0052, whose authority this
// package is. A request the assistant makes is its person's own, so the person
// is decided exactly as their own click is, and every row says what an
// assistant grant may do with its verb (read it, carry the one approved
// request, or never), which can only take away from what the person holds.
// The SPLIT with the request guard is deliberate: the guard holds a read
// grant to reads and an exec grant to the one request it binds, and
// [Decide] holds every grant to the row's class wherever the verb is asked,
// a verb asked from inside a tool included, which no route sees.
//
// A CHECK IS THE WRITE AT THE READ'S PROOF: ADR-0055, whose authority this
// package is. A route declaring [Policy.Checked] answers `dry_run=true` as its
// verb decided without the step-up, and that check is the one request with a
// write's method a read grant may send, so the assistant's preview asks the
// route before the person is shown a change.
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
// opinion about the hierarchy. So the chart is a five-method seam the consumer
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
// Monday: that is the STEP-UP, and every row states whether it asks for one
// ([iam.Recency]) — none, or `step_up` (an hour by default): the company's
// configuration, integrations and credential store, the identity
// directory's writes, how somebody proves who they are — whichever surface the
// change comes through — and the deployment's own controls, ending every
// session in the company among them. ONE WINDOW, for internal/config's reason:
// a second, shorter one for some of those gestures sent people to re-prove
// inside the hour they had already proved in, and what keeps a stolen machine
// credential from them is the row's own refusal of any request a token
// presented and internal/iamdomain's refusal of an administrative record
// written through one (see below) — never the age of a proof.
//
// IT IS DECIDED HERE, ON THE ROW, and nowhere else. It was a setting nothing
// read: the sign-in surface could record a proof and no surface outside it ever
// asked for one, so a cookie from last week reached every one of those
// gestures. On the row, a REST route and a tool asking about one verb get one
// answer, and a walk holds every row to having decided — a zero recency is
// refused, because read as "none" it is a step-up verb that shipped open.
//
// A ROW MAY ASK ITS SELF ARM A DIFFERENT WINDOW from every other arm, read off
// the reason the class admitted on: ending every session somebody holds asks
// nothing of the person themselves — the first thing they do on finding an
// intruder — and `step_up` of an administrator ending somebody else's, which
// stops every pipeline that colleague runs as well.
//
// THE PROOF IS ASKED AFTER THE RULE ADMITS, so a caller who could never take
// the verb is told what they lack rather than sent to confirm their identity
// first, and the refusal ([ReasonStepUp]) names the window it needs so a
// client can ask the person and replay the request. Who counts as having
// proved is the principal's business ([iam.Principal.Proved]): a session
// proved when it signed in or stepped up, while a credential with nobody at a
// keyboard is fresh by construction, because there is nothing else it could
// ever present — a Tier A token, the development principal and a machine
// token alike. So the CLOCK keeps no token off the gestures that need a person
// present, and the next section's two locks do.
//
// # Who has to be at the keyboard
//
// Some gestures need a PERSON present, whoever the credential acts as:
// revealing a secret's value, changing who may do anything or how somebody
// proves who they are, and ending sessions that are not the caller's own. A
// MACHINE TOKEN acts as its owner and is stepped up by construction, so it
// passes every relation and every window its owner would — admin included,
// which a token may be minted with — and a leaked one that could change its
// owner's second factor is the first half of taking the account. Two locks
// keep it off them:
//
//   - THE ROW SAYS SO ([Presence], [rule.presence]), and [Decide] refuses a
//     request that presented a machine token on it ([ReasonTokenRefused],
//     read off [iam.Principal.MachineToken]) — on every arm, or on every arm
//     but the self arm, which is how a token may still end its own owner's
//     sessions (itself among them) and revoke its owner's tokens, and never
//     anybody else's. It was a refusal each route remembered to make, and the
//     table admitted the token on the same verb; a route added on that verb
//     that forgot was a token changing its owner's password.
//   - THE RECORD SAYS SO TOO: internal/iamdomain refuses an administrative
//     identity record — an enrolment, an invitation, a grant edit, a
//     removal, ending everybody's sessions — written through a machine
//     token, whatever the token carries. That lock sits where a record is
//     formed rather than where a route is, because a record is published by
//     a CLI, a duty and a migration as well as by the routes this table
//     guards.
//
// A walk holds the first: every row that states a presence refuses a token on
// exactly the arms it names, and a token holding every permission is refused
// every such gesture about anybody but its owner.
//
// No TOOL asks for a proof (a walk holds that too): a seat has no keyboard, and
// the operator's MCP surface is not a step-up surface.
//
// # Admin, and one permission per app (ADR-0033)
//
// The vocabulary is admin and the apps ([iam.Grant]): `admin`, which runs the
// company and the engine, and one permission per APP (`worklet`, `doclet`
// and `artifacts`), which uses it. Every row names at most one app
// ([rule.app]) — the one gesture drawing on several names its set instead
// ([rule.anyOf]) — and that decides three things at once: a [ClassApp] row admits whoever holds the app;
// a RELATION row — a person's own record, a container's lead, a comment's
// author — admits its relation only beside the app, so a lead who does not use
// the tracker does not reorder a report's queue in it; and an administrator
// passes every app check through [iam.Expand], so admin is never told it
// lacks an app.
//
// EVERYTHING ELSE IS EVERYBODY'S ([ClassEverybody]): the org chart, the
// agents' status, spend, schedules, who to ask. A company is a shared place,
// and a person handed no app still sees what the company is doing — they just
// cannot use its apps. What runs the company is [ClassAdmin], and nothing in
// between: a reader of the trail, a manager of people and an operator of the
// deployment were three grants nobody could tell apart, and they are one.
//
// # Seat-only, human-only and seated
//
// Three facts about a VERB rather than about anybody's relation to an object,
// each a field on the row and each checked before the class. [rule.humanOnly]
// keeps an agent off a person's gesture — purging, archiving a project,
// writing the chart. [rule.seatOnly] is its mirror: only an agent seat whose
// role offers the tool takes it — asking a colleague, starting a coding run —
// with no permission at all, because which seats are offered it is the role's
// registry's decision and a person has no turn for it to spend.
// [rule.seated] is the third: a verb made AS A SEAT, which a principal acting
// as itself — a token, a service account bound to none — cannot take whatever
// it holds, because what the verb writes or reads is keyed on a seat handle
// it does not have. A person always has one (ADR-0032); a machine has one
// only once the identity directory binds it.
//
// # An artifact is decided per object, by its shares
//
// Every other kind is decided per APP: a tracker holder reads every task. An
// artifact is not, because it is somebody's own work handed to the people it
// names, so [KindArtifact] belongs to no app ([AppFor]) and one class decides
// it: [ClassShared]. A caller's ROLE on an artifact (viewer, commenter,
// contributor, editor, owner; internal/artifacts declares the ladder) is the
// best of their relations to it: owning it, a share naming their seat, a unit
// they sit in or the whole company, and leading the AGENT that owns it. A row
// names the least role it admits ([rule.minRole]).
//
// A SHARE IS A RELATION, NEVER A PERMISSION. The `artifacts` app gates
// AUTHORING only: creating one, publishing a version, renaming it, adding an
// asset, changing its data's shape, and taking one's own out of circulation
// and back. A row that reads or participates (opening, filling in data) asks
// the relation alone, so somebody invited with no app still answers the review
// sheet a colleague shared with them; an authoring row asks the app beside the
// relation ([withApp]), so editor without the app writes data and does not
// publish.
//
// NO RELATION IS NOT FOUND, NEVER FORBIDDEN ([Decision.NotFound]): an
// artifact's existence, id and title are its owner's and its audience's to
// disclose. A relation below the row's role is refused naming the role it
// needs ([ReasonRole], [Decision.Role]).
//
// THE ADMIN PATH ADMITS EVERY ROW, because admin is host access (below) and a
// refusal would promise a privacy the engine cannot keep. What the engine can
// do is make the override visible, so an admission only the admin path made is
// marked ([Decision.Elevated]) and the surfaces record it. It is consulted
// AFTER the caller's relations and before any chart failure can decide, so an
// administrator who also holds a share is admitted as a sharer (nothing to
// record), and one on a node behind its chart is still never told "I cannot
// tell".
//
// AN AGENT is held two ways further. It never manages somebody else's work
// unless the company says so ([Object.AgentsManageOthers]); and inside a turn
// somebody woke, every artifact verb it takes is decided again for the person
// it is acting for, except the edits it makes to its own work
// ([DecideWithin]).
//
// # Admin is host access, and the table does not pretend otherwise
//
// `admin` is ONE permission over the whole company configuration, and that
// configuration runs code: an `mcp_servers` entry is a command every engine
// host executes, a seat's `mcp_env` and its per-phase model keys (`llm_*`, a
// `cli-agent` provider among them) choose what a seat's children run and which
// credentials they are handed, a `sandbox` cell of `run_in: self` runs a coding
// agent on the engine host itself, and a seat's worker grants decide which of
// its tools a worker runs with. So holding it IS running code on every engine
// host, and whatever a process there can read — the keyring included — is
// reachable through it. It is conferred like host access.
//
// THE ORG CHART IS ALSO ITS LEADS' ([ClassSubtree]): a person who leads a unit
// may change the seats and units inside it without admin, every field but a
// credential's — and no field to a `${VAR}`, which the engine resolves from
// its own environment wherever one stands — and every place a change reaches
// held to their own subtree on both sides of the write
// (internal/api/configapi). What that reaches is stated rather than narrowed:
// a lead may point a seat in their team at any model chain and any sandbox
// cell the company's SETTINGS offer — which remain admin's, so whether a
// `cli-agent` provider or a host-side sandbox cell exists for a lead to choose
// is the administrator's decision — and may not name a credential or a
// variable at all.
package authz

import (
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// Action is one verb this engine authorizes.
//
// THE VERB, NOT THE ROUTE. `PATCH /worklet/items/{key}` and `update_work_item`
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
	// KindUnit is a team in the org chart, and the subtree an org chart
	// write is decided against ([ClassSubtree]).
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
	// credentials, its integrations, the node it runs on: what the admin
	// verbs and everybody's overview are about.
	KindCompany ObjectKind = "company"
	// KindArtifact is one artifact, and everything decided as it: its
	// versions, files, assets, datastore collections and rows, presence and
	// view tickets. The one kind decided PER OBJECT rather than per app: who
	// may touch an artifact is its owner and whoever it is shared with
	// ([ClassShared]), so no app admits a reader to it ([AppFor], [Visible]).
	KindArtifact ObjectKind = "artifact"
	// KindArtifactFolder is one folder of the company's artifact tree,
	// decided PER OBJECT exactly as an artifact is: its owner and its
	// top-level folder's shares ([ClassShared]). A folder grants nothing on
	// the artifacts filed in it, so no decision about an artifact ever
	// reads one (ADR-0047).
	KindArtifactFolder ObjectKind = "artifact_folder"
)

// ObjectKinds are every kind, in declaration order.
var ObjectKinds = []ObjectKind{
	KindTask, KindProject, KindUnit, KindPage, KindContainer, KindPerson,
	KindView, KindCompany, KindArtifact, KindArtifactFolder,
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
	// Read by the two personal classes, and by [ClassSubtree], where it is
	// the same question asked about a WRITE nobody has read yet: what the
	// body changes decides it, so the route admits whoever could lead any
	// part of the chart and the surface decides again on each change.
	//
	// AND BY THE CLASSES WHOSE OBJECT IS A ROW nobody has read yet —
	// [ClassAppOrAsked] (which open ask names the caller is on the item),
	// [ClassTidy], [ClassContainer] and [ClassDestructive] (which project or
	// space the row is filed in, and who wrote it): an object marked
	// unresolved there, naming no container, is answered [ErrUnresolved] for
	// a caller the row could still admit — read the row, then decide again on
	// it — so a surface offering a verb before it has read anything offers it
	// to whoever the row might let through.
	Unresolved bool

	// Author is the login or seat that WROTE this, for the classes where
	// authorship is an authority: a comment is edited and removed by
	// whoever left it ([ClassAuthored]), and a page's creator or a work
	// item's reporter tidies it — trashes, restores, renames, removes —
	// beside its container's lead ([ClassTidy]).
	//
	// EMPTY MEANS "NO AUTHOR ARM", never "anybody wrote it": a tidy of a row
	// whose creator nobody recorded is its container lead's or an
	// administrator's alone.
	//
	// Separate from Owner because they are different relations and the
	// same login in both fields is a coincidence: a comment on your own
	// page has one author and no owner at all.
	Author string

	// Asked is the seat handle an OPEN ask on this work item names — the
	// person the item's thread is waiting on an answer from — for the one
	// class where being asked is an authority ([ClassAppOrAsked]): somebody
	// a colleague put a question to reads the item and answers it without
	// the tracker app.
	//
	// SET ONLY BY A CALLER THAT HAS READ THE ITEM and resolved its asks
	// (internal/worklet's AskedOn), never from an argument: an asked arm a
	// caller could fill in for themselves would be the tracker app handed to
	// anybody who typed their own name. Empty means nobody is asked, or
	// nobody has looked.
	Asked string

	// Container is the project a task is filed under, or the container a
	// page sits in. It is what the container and destructive classes ask
	// the chart about — and, for [ClassSubtree], the key of the unit an org
	// chart change reaches, empty for the company root.
	Container string

	// ContainerKind is what sort of thing Container names, for the one
	// class whose object is neither the container nor governed by a
	// single relation: a saved view sits on a project, a unit, a person
	// or the workspace, and those are four different questions.
	//
	// SEPARATE FROM Kind rather than overloading it, because Kind is what
	// the object IS — the container classes pick a relation from it, and
	// [AppFor] an app — and a view whose Kind said "project" would be a
	// view those rules read as a project. Empty on every other class,
	// which reads Kind and never this.
	ContainerKind ObjectKind

	// Shares are an artifact's audience list, as its stored row holds it:
	// each one a seat, a unit or the whole company, with the role it grants
	// ([ClassShared]). Read beside [Object.Owner], which names the artifact's
	// owner: [iam.RecordOwner] of whoever created it or was last handed it, a
	// seat handle, or a machine login for a service account bound to none.
	//
	// SET ONLY BY A CALLER THAT HAS READ THE ARTIFACT, never from an
	// argument, exactly as [Object.Asked] is: a share a caller could state
	// for themselves would be every artifact handed to whoever typed their
	// own seat. Empty means the artifact is shared with nobody, its owner's
	// alone.
	Shares []artifacts.Share

	// OwnerIsAgent says the artifact's owner is an AGENT seat (the stored
	// owner's kind is [iam.ActorAgent]), which is what opens the LEAD ARM: a
	// seat's lead holds owner-level on the artifacts that seat owns, as they
	// already steer, pause and read its trail, and an agent that is paused or
	// removed must not orphan its work. False for a person's artifact, whose
	// lead holds nothing on it: a person's draft is theirs, and the lead
	// relation was built for a team's work queue, not for reading a report's
	// drafts.
	//
	// Read off the stored row, never inferred from the owner's name, for
	// [Object.Shares]' reason.
	OwnerIsAgent bool

	// AgentsManageOthers is the company's `artifacts.agents_manage_others`
	// setting, read by the surface from the company document it runs. False,
	// the default, keeps an agent seat to `contributor` on every artifact it
	// does not own, however it is shared and whoever it leads: it reads the
	// artifact and writes its data, and never publishes over, renames,
	// reshapes or reshares somebody else's work ([ReasonSeatRefused]). True
	// lifts the cap, and the agent holds whatever its relations grant.
	//
	// A FACT ABOUT THE COMPANY rather than the object, carried here because
	// [Decide] is pure over values: the alternative was a second argument
	// every caller of every verb passes, read by one class. The zero is the
	// closed end, so a surface that never read the setting keeps agents off
	// other people's work rather than letting them in.
	AgentsManageOthers bool
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

	// Grants are the MINIMAL permissions that would have admitted THIS
	// principal, for THIS object: any ONE of them, added to what the
	// principal holds, admits — and a permission already covered through
	// [iam.Expand] by a named one is left out, so a refusal names the
	// smallest thing to ask for. An app row names its app and never "the
	// app or admin"; a relation refused names admin, its admin path; a lead
	// who lacks the row's app is told the app; [ClassAnyApp] names every
	// app of its row's set ([rule.anyOf]). Empty where no permission could
	// have changed the answer: the self rule, which has no admin path, an
	// object missing the field its rule reads before it consults any
	// permission, and the refusals that are not about permission at all —
	// an enrolment stage, a seat taking a human-only verb, a person taking
	// a seat-only one, a principal acting as no seat taking a verb made as
	// one, a machine token taking a verb that needs a person present, a
	// verb with no rule, an agent kept off somebody else's artifact by the
	// company's setting ([Object.AgentsManageOthers]) and a turn's
	// requester who could not take the verb ([DecideWithin]).
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

	// Role is the role a [ReasonRole] refusal needed, set on that refusal
	// and on nothing else: the caller holds a relation to the artifact, and
	// it is below what the verb asks ([ClassShared]). It is what lets a
	// surface say "ask the owner for editor" rather than a bare no, and it
	// rides on the decision for [Decision.Recency]'s reason.
	Role artifacts.Role

	// Standing is what the caller holds on the artifact, set on every
	// [ClassShared] decision that reached the caller's relations, admitted
	// or refused, and zero on every other class. It is what a surface stamps
	// into the record a write forms (the effective role) and what it shows
	// a reader as their own role; see [Standing] for when it is exact.
	Standing Standing

	// Elevated reports an admission ONLY THE ADMIN PATH made: an
	// administrator whose relations to the artifact, if any, do not reach
	// the role the verb asks ([ReasonGrant] on a [ClassShared] row). The
	// override of somebody's private work is recorded where it is used, so
	// a read admitted this way is the one a surface writes an elevated-read
	// record for, and a write admitted this way is stamped elevated.
	Elevated bool
}

// Unknown reports a decision this node could not reach.
//
// A METHOD rather than a nil check at every call site, because the check is
// the thing callers get wrong: `if !d.Allowed { refuse }` reads correctly and
// is wrong exactly when the store blinked.
func (d Decision) Unknown() bool { return d.Err != nil }

// NotFound reports a refusal a surface must answer EXACTLY as it answers an
// object nobody holds: a caller with no relation at all to an artifact
// ([ReasonNotShared]).
//
// A METHOD, for [Decision.Unknown]'s reason: the rule is that existence is
// the relation's to disclose (an artifact's id, title and very existence are
// its owner's and its audience's), and a surface that compared reasons itself
// would be one more place to forget it. [EnvelopeRefusal] answers it `404
// not_found` with no reason and no grants, which is byte for byte what an
// artifact surface answers for an id nothing holds, so the two cannot be told
// apart; a tool reports it as the object not existing.
func (d Decision) NotFound() bool {
	return d.Err == nil && !d.Allowed && d.Reason == ReasonNotShared
}

// Reason names the rule that decided, in the words a refusal uses.
//
// A NAMED STRING WITH A CLOSED SET, so a surface rendering one cannot invent a
// reason and a test can assert which rule fired rather than that the answer
// happened to be right. An unknown value off a newer peer's audit row renders
// rather than failing, the same rule [iam.Grant] follows.
type Reason string

const (
	// ReasonGrant is a permission the principal carries.
	ReasonGrant Reason = "grant"
	// ReasonSelf is a personal object the principal owns.
	ReasonSelf Reason = "self"
	// ReasonAuthor is an authored object the principal wrote.
	ReasonAuthor Reason = "author"
	// ReasonLead is a lead relation the chart answered for.
	ReasonLead Reason = "lead"
	// ReasonNoGrant is a refusal: the principal carries no permission
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
	// ReasonSeatRefused is a refusal an agent gets and a person does not:
	// a human-only verb ([rule.humanOnly]), or managing an artifact the
	// agent does not own while the company keeps agents to contributor
	// there ([Object.AgentsManageOthers]).
	ReasonSeatRefused Reason = "seat_refused"
	// ReasonTokenRefused is a refusal a request that presented a MACHINE
	// TOKEN gets and the person it acts as would not: the verb needs a
	// person present, and a token proves nobody is. See [Presence].
	ReasonTokenRefused Reason = "token_refused"
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
	// ReasonEverybody is an admission no permission was asked for: the
	// verb is part of everybody's overview ([ClassEverybody]).
	ReasonEverybody Reason = "everybody"
	// ReasonAsked is an admission on being ASKED: an open ask on the work
	// item names the caller ([ClassAppOrAsked], [Object.Asked]).
	ReasonAsked Reason = "asked"
	// ReasonSeatOnly is a refusal a person or a credential gets and an
	// agent seat does not: the verb is an agent's own ([rule.seatOnly]).
	// It names no permission, because none would change it.
	ReasonSeatOnly Reason = "seat_only"
	// ReasonUnseated is a refusal a principal acting as no seat gets: the
	// verb is made AS a seat ([rule.seated]) — a Tier A token, a service
	// account bound to none, the development principal. It names no
	// permission, because none would change it: what does is a binding in
	// the identity directory.
	ReasonUnseated Reason = "unseated"
	// ReasonShared is an admission on a SHARE: the artifact is shared with
	// the caller's seat, a unit they sit in or the whole company, at a role
	// that includes what the verb asks ([ClassShared]).
	ReasonShared Reason = "shared"
	// ReasonNotShared is a refusal: the caller holds no relation at all to
	// the artifact. It is answered as an object nobody holds
	// ([Decision.NotFound]), so it says nothing about whether one exists.
	ReasonNotShared Reason = "not_shared"
	// ReasonRole is a refusal: the caller holds a relation to the artifact,
	// below the role the verb asks, which [Decision.Role] names.
	ReasonRole Reason = "role"
	// ReasonCeiling is a refusal an agent gets inside a turn somebody woke:
	// the agent may take the verb, and the person whose request it is
	// acting on could not ([DecideWithin]). It names no permission, because
	// none the agent could hold would change it.
	ReasonCeiling Reason = "ceiling"
	// ReasonAssistRefused is a refusal a request made through the
	// Dashboard Assistant gets and the person's own click would not: the
	// verb is never the assistant's ([AssistNever], the human-only list).
	// It names no permission, because none the person could hold would
	// change it; they take the verb themselves.
	ReasonAssistRefused Reason = "assist_refused"
	// ReasonAssistScope is a refusal an assistant grant gets when its
	// scope does not reach the verb: a read grant asking a verb the
	// assistant carries only as an approved request ([AssistApprove]), or
	// one that needs a person at the keyboard ([rule.presence]), which only
	// the person's approval of the request supplies.
	// The remedy is a proposal the person approves, never a permission.
	ReasonAssistScope Reason = "assist_scope"
)

// Reasons are every one, in declaration order.
var Reasons = []Reason{
	ReasonGrant, ReasonSelf, ReasonAuthor, ReasonLead,
	ReasonNoGrant, ReasonNotSelf, ReasonNotLead, ReasonNotAuthor,
	ReasonSeatRefused, ReasonTokenRefused, ReasonStage, ReasonUnnamed,
	ReasonUnknownAction, ReasonStepUp, ReasonEverybody, ReasonAsked,
	ReasonSeatOnly, ReasonUnseated, ReasonShared, ReasonNotShared, ReasonRole,
	ReasonCeiling,
	ReasonAssistRefused, ReasonAssistScope,
}

// Valid reports whether a reason is one this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// adminGrant is the permission that overrides every relation-based class: a
// person's record, a saved view, a container, a tidy, a comment, the org
// chart's subtree, a directory row.
//
// [iam.GrantAdmin], which runs the company and the engine, and which holds
// every app through [iam.Expand] — so a relation row that also asks for its
// app is never refused to an administrator for want of the app. Its own name
// here so the rules that use it say ADMIN where they mean the admin path.
const adminGrant = iam.GrantAdmin
