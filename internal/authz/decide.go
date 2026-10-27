package authz

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// Class is one authority rule, shared by every action that obeys it.
//
// THE RULES ARE THE CLOSED SET, NOT THE VERBS. There are dozens of verbs and
// sixteen ways to decide one, so a table keyed on the verb would state
// sixteen rules dozens of times and drift on whichever copy somebody edited.
// A verb picks its class in [rules] and the class is written once, here.
type Class string

const (
	// ClassEverybody — everybody's overview: the org chart, the agents'
	// status, spend, schedules, who to ask, the routes that admit a request
	// a tool then decides. Any principal whose stage may act, and nothing
	// else asked.
	//
	// A CLASS RATHER THAN AN ABSENT ROW, because "no rule" is refused
	// ([ReasonUnknownAction]) — a verb somebody forgot must not ship open —
	// and a verb everybody may take is a decision that has to be written
	// down where a reader can see it was made.
	ClassEverybody Class = "everybody"

	// ClassApp — using one app: every tracker read and write, every page
	// read and write. Whoever holds the row's app ([rule.app]), which an
	// administrator holds through [iam.Expand].
	ClassApp Class = "app"

	// ClassAnyApp — a gesture that draws on whichever apps the caller uses:
	// a question answered from the company's pages and its work, which
	// searches each half only for a caller who holds its app. Whoever holds
	// any one app; a caller with none is told every app would do.
	ClassAnyApp Class = "any_app"

	// ClassAppOrAsked — a work item somebody may be ASKED about. Whoever
	// holds the row's app, OR the person an open ask on the item names
	// ([Object.Asked]): a colleague who put a question to somebody needs
	// them to be able to read it and answer, and a person who answers
	// questions is not thereby somebody who uses the tracker.
	//
	// THE ASKED ARM IS NARROW BY CONSTRUCTION. It admits only a caller an
	// OPEN ask names, and only once a surface has read the item and
	// resolved its asks — an object nobody has read yet is answered
	// [ErrUnresolved] for a caller without the app, so the surface reads the
	// row and decides again — and the tracker's writer still refuses an
	// answer from anybody but the person asked. After answering, the item
	// is the tracker's again.
	ClassAppOrAsked Class = "app_or_asked"

	// ClassSelf — the acting principal's OWN working state: its diary, its
	// episodes, its skills, its onboarding marker, a tool skill it loads
	// into this session.
	//
	// NO ADMIN PATH, which is what makes it its own class rather than
	// [ClassOwnRecord]. internal/agent/builtin states the boundary these
	// verbs rest on: they take no handle at all, because "an agent
	// recalling another's episodes or writing into another's diary would
	// make the per-seat memory a shared one". Somebody else READING a seat's
	// memory is a different verb with a rule of its own
	// ([ActionSeatTrailRead]), and admitting it here too would be a second
	// answer to one question — which is the drift this whole package
	// exists to remove.
	ClassSelf Class = "self"

	// ClassOwnRecord — a person's own inbox and pins. The owner, or the
	// admin path that lets an administrator unstick a departed person's
	// queue — and the row's app beside either ([rule.app]).
	//
	// NO LEAD PATH. A lead may re-order what their report works on
	// ([ClassOwnOrLead]); marking somebody's mail read is not the same
	// gesture and nobody asked for it.
	ClassOwnRecord Class = "own_record"

	// ClassOwnOrLead — a person's priorities and their day, and a seat's
	// own controls and trail. The owner, whoever leads them, or the admin
	// path — and the row's app beside any of them, where the row names one.
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
	// It reads the container's KIND for [Chart.LeadsUnit]'s reason — a
	// project key and a unit key are two relations — and the WORKSPACE
	// container reaches no relation at all, so a company-wide tab is the
	// admin path alone.
	ClassSavedView Class = "saved_view"

	// ClassContainer — a container's own policy. For a PROJECT: its
	// fields, its default assignee, a task's routing unit, a tag rename, the
	// project's archive. Whoever leads that container, or the admin path —
	// and the object's KIND picks which of the two relations is asked.
	ClassContainer Class = "container"

	// ClassTidy — taking one's own work out of circulation and putting it
	// back: trashing, restoring and renaming a page, removing and restoring
	// a work item. Its container's lead, the admin path, OR whoever WROTE
	// it ([Object.Author]: a page's creator, an item's reporter).
	//
	// THE AUTHOR ARM IS WHAT MAKES IT ITS OWN CLASS rather than
	// [ClassDestructive]. Somebody who filed a duplicate or drafted a page in
	// the wrong place should not need a lead to tidy it up, and a container
	// lead taking a colleague's work out of their project is a different
	// gesture from that colleague withdrawing their own. What the author
	// arm does NOT reach is anybody else's work: a subtree of items under
	// theirs stays the lead's (the tool says so), and a PURGE, which
	// nothing undoes, stays admin's.
	ClassTidy Class = "tidy"

	// ClassSubtree — the org chart: the seats and units of the company
	// document. The admin path, or a LEAD over the part concerned: a unit
	// is in a principal's SUBTREE when they are the effective lead of it or
	// of any unit above it ([Chart.LeadsUnit]), and [Object.Container]
	// names the unit a change reaches — empty for the company root, which no
	// unit holds and so is nobody's subtree, refused [ReasonNotLead] like a
	// unit somebody else leads.
	//
	// ONE QUESTION PER PLACE, AND THE SURFACE ASKS IT OF EVERY PLACE A
	// WRITE REACHES, on both sides of it (internal/api/configapi): where a
	// seat or a unit was, where it is now and what its references name,
	// while a project, space, channel, address or contact id it states is
	// the company's ([ActionConfigWrite]) like a credential. That is what
	// keeps a lead inside their own team — a lead who could hand their
	// unit to somebody else, make a report the CEO's manager or declare a
	// project as their team's would gain authority outside it through the
	// very relations this package decides everything else by.
	//
	// A WRITE NOBODY HAS READ YET ([Object.Unresolved]) is admitted to
	// anybody bound to a seat, the only kind of principal a unit can name
	// as its lead, so the surface can read the body and decide on what it
	// changes; somebody bound to none leads nothing and is refused before
	// it is read. The gate asks NO CHART: "leads somebody" would refuse a
	// lead alone in the team they are about to staff.
	ClassSubtree Class = "subtree"

	// ClassDestructive — moving a task out of its project and removing a
	// file from one. The CONTAINER's lead, or the admin path: a colleague
	// may file work in a project and may not carry it away again.
	ClassDestructive Class = "destructive"

	// ClassAuthored — editing and removing a comment, on a page or on a
	// work item. Whoever wrote it, or the admin path.
	ClassAuthored Class = "authored"

	// ClassAdmin — running the company: configuration, secrets,
	// integrations, the catalogue, the tool-skill pages, the directory's
	// writes, the deployment, the company-wide trail, the purges. Admin,
	// and nobody else.
	ClassAdmin Class = "admin"

	// ClassDirectory — one person's directory row and what hangs off it:
	// reading it, their credentials, their sessions. The person the row is
	// ABOUT, or admin.
	//
	// THE OBJECT IS A PERSON ID, never a login or a handle — this is the
	// one place in the table where that is true, and it is forced: the
	// identity estate keys on an id precisely because a person changes
	// their login, and a self check against a mutable name would open
	// somebody else's row the day they swapped.
	//
	// ONE CLASS where there were two: a directory READ admitted an auditor
	// beside the people manager, and a directory GESTURE did not, and with
	// both of those folded into admin the two rules said the same thing.
	ClassDirectory Class = "directory"

	// ClassShared is one artifact, decided by the caller's ROLE on it, the
	// best of owning it, a share naming their seat, a unit they sit in or
	// the whole company, and leading the agent that owns it. The row names
	// the least role it admits ([rule.minRole]); the owner holds every role.
	// See "An artifact is decided per object, by its shares" in the package
	// doc, and [decideShared] for the order of its arms.
	//
	// A RELATION CLASS, so a row that also names the `artifacts` app asks it
	// beside the role ([withApp]): an editor without the app writes the
	// artifact's data and does not publish it.
	ClassShared Class = "shared"
)

// Classes are the sixteen, in declaration order.
var Classes = []Class{
	ClassEverybody, ClassApp, ClassAnyApp, ClassAppOrAsked, ClassSelf,
	ClassOwnRecord, ClassOwnOrLead, ClassSavedView, ClassContainer,
	ClassTidy, ClassSubtree, ClassDestructive, ClassAuthored, ClassAdmin,
	ClassDirectory, ClassShared,
}

// relationClasses are the classes that decide on the principal's RELATION to
// the object (its owner, its lead, its author, a share) and that ask the
// row's app beside the relation ([withApp]).
var relationClasses = []Class{
	ClassOwnRecord, ClassOwnOrLead, ClassSavedView, ClassContainer,
	ClassTidy, ClassDestructive, ClassAuthored, ClassShared,
}

// Decide answers whether p may do a to o.
//
// # The order of the checks is the contract
//
// STAGE FIRST, because a principal part-way through enrolment holds
// permissions it may not yet use, and every rule below would happily honour
// them.
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
// THEN THE HUMAN-ONLY AND SEAT-ONLY BARS, which are facts about the VERB and
// therefore decided before any class reads any relation — see
// [rule.humanOnly] and [rule.seatOnly] — and the PERSON-PRESENT bar where it
// covers every arm, for the same reason: a machine token is refused a verb
// that needs somebody at a keyboard before any class weighs what it carries
// ([rule.presence]). Where the bar spares the self arm it is asked once the
// class has said which arm admitted. One exception keeps a bar from naming
// what it should not: an artifact the caller holds no relation to is answered
// not found, exactly as the shared class answers them ([concealed]).
//
// THEN A SEAT GRANT, where the row names one, and for an AGENT only: its role
// carrying the grant is the whole of its answer ([rule.seatGrant]), and the
// class below never reads it — a seat holds no permission the class could
// admit it on, and an agent the grant refuses is told so rather than that it
// lacks admin. Every other principal goes on to the class.
//
// THE ADMIN PATH IS CHECKED BEFORE THE CHART, on every class that has one,
// because the chart can fail and the permission cannot: an administrator must
// not be told "I cannot tell" by a node that is behind. [ClassShared] asks an
// administrator's relations first, so an admission a share already makes is
// not recorded as an override, and still decides the admin path before any
// chart failure could reach the answer.
//
// THE ROW'S APP IS ASKED AFTER THE RELATION ([withApp]), so a refusal names
// what would actually admit: a lead short of the app is told the app, and a
// caller the relation refused is told admin. A caller short of the app is
// never answered unknown on a relation row, because no answer the chart could
// give would admit them.
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
	// AN AGENT NEVER TAKES A HUMAN-ONLY VERB, checked before the class so
	// the refusal names what it actually is: an agent holding the
	// permission is told it is a seat, not that it lacks a capability it
	// plainly has. See [rule.humanOnly].
	if r.humanOnly && p.Kind == iam.KindSeat {
		return concealed(ctx, p, r, o, chart, Decision{Reason: ReasonSeatRefused})
	}
	// AND NOTHING BUT AN AGENT TAKES A SEAT-ONLY ONE, for the mirror
	// reason: a person holding admin is told the verb is an agent's, not
	// sent to ask for a permission that would change nothing. See
	// [rule.seatOnly].
	if r.seatOnly && p.Kind != iam.KindSeat {
		return concealed(ctx, p, r, o, chart, Decision{Reason: ReasonSeatOnly})
	}
	_, throughToken := p.MachineToken()
	if throughToken && r.presence == PresenceRequired {
		return concealed(ctx, p, r, o, chart, Decision{Reason: ReasonTokenRefused})
	}
	var d Decision
	if r.seatGrant != "" && p.Kind == iam.KindSeat {
		d = decideSeatGrant(ctx, p, r.seatGrant, chart)
	} else {
		d = withApp(p, r, decideClass(ctx, p, r, o, chart))
	}
	// THE PROOF LAST, and only over an ADMISSION: a refusal is already
	// the answer, and an unknown is already "ask me again". See the
	// package doc and [rule.recency].
	if d.Unknown() || !d.Allowed {
		return d
	}
	// AN ARM ONLY A PERSON MAY TAKE, now that the class has said which arm
	// admitted: the self arm of a row that spares it is the token acting
	// as its own owner, and every other arm is somebody else's record.
	if throughToken && r.presence == PresenceRequiredForOthers &&
		d.Reason != ReasonSelf {

		return Decision{Reason: ReasonTokenRefused}
	}
	need := recencyFor(r, d)
	if p.Proved(need, now) {
		return d
	}
	return Decision{Reason: ReasonStepUp, Recency: need, Grants: d.Grants}
}

// concealed is a bar on the VERB (an agent taking a person's verb, a token
// taking one that needs somebody present) answered for the object it was
// asked of: the bar itself, except where the object is an artifact the caller
// holds NO RELATION to, which is answered exactly as [ClassShared] answers
// such a caller, not found.
//
// THE BAR WOULD OTHERWISE BE AN EXISTENCE ORACLE. A surface reads the head
// before it decides, so an id nothing holds is answered 404 there, and a bar
// rendered as a 403 for an existing artifact tells a caller with no relation
// that a private artifact exists (03 section 2.7: a caller with no relation is
// answered as an unknown id). The relations are asked only on that path, and
// the bar keeps its own name for everybody the artifact is disclosed to (its
// audience and an administrator), so a person is still told the verb is not a
// token's, and an agent that it is not a seat's.
//
// A RELATION THE CHART COULD NOT ANSWER is unknown rather than either answer:
// the bar names the artifact's existence, the not found denies it, and the
// node cannot tell which of the two is true for this caller.
func concealed(ctx context.Context, p iam.Principal, r rule, o Object, chart Chart,
	bar Decision) Decision {

	if r.class != ClassShared || o.Unresolved || (o.Owner == "" && len(o.Shares) == 0) ||
		p.Can(adminGrant) {

		return bar
	}
	rel := relate(ctx, p, o, chart)
	switch {
	case rel.role != "":
		return bar
	case rel.err != nil:
		return Decision{Reason: ReasonNotShared, Err: rel.err}
	}
	return consulting(Decision{Reason: ReasonNotShared, Standing: rel.standing(false)},
		adminGrant)
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
	case ClassEverybody:
		return Decision{Allowed: true, Reason: ReasonEverybody}

	case ClassApp:
		return granted(p, r.app)

	case ClassAnyApp:
		// ANY ONE APP, and a refusal names every one of them: holding
		// either would admit, and admin is left out because it covers
		// both ([Decision.Grants]).
		for _, app := range iam.Apps {
			if p.Can(app) {
				return consulting(Decision{Allowed: true, Reason: ReasonGrant}, iam.Apps...)
			}
		}
		return consulting(Decision{Reason: ReasonNoGrant}, iam.Apps...)

	case ClassAppOrAsked:
		if p.Can(r.app) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, r.app)
		}
		// THE PERSON ASKED, by the seat they hold or the login they act
		// under — an ask names a seat handle, and a person acting as
		// their seat is named by it.
		if o.Asked != "" && (p.Seat != "" && p.Seat == o.Asked ||
			p.Login != "" && p.Login == o.Asked) {

			return Decision{Allowed: true, Reason: ReasonAsked}
		}
		if o.Unresolved {
			// NOBODY HAS READ THE ITEM YET, so nobody knows whether an
			// open ask on it names this caller: read it, resolve its
			// asks, and decide again on the row.
			return Decision{Reason: ReasonNoGrant, Err: ErrUnresolved}
		}
		return consulting(Decision{Reason: ReasonNoGrant}, r.app)

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

	case ClassOwnRecord, ClassOwnOrLead:
		// NO PERMISSION ON THE UNNAMED REFUSAL: it is taken before the
		// admin path is consulted, so no permission would have changed it.
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
		// EVERY ANSWER BELOW IS ONE THE ADMIN PATH WOULD HAVE
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
		// which the check above is.
		return consulting(Decision{Reason: ReasonNotLead}, adminGrant)

	case ClassContainer, ClassDestructive:
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		if o.Unresolved && o.Container == "" {
			// THE ROW IS NOT READ YET, so nobody knows which project it
			// is filed in: read it and decide again. Answered as a
			// refusal naming nothing, a surface offering verbs before it
			// had read a row listed none of these to a project's own
			// lead.
			return Decision{Reason: ReasonNotLead, Err: ErrUnresolved}
		}
		if o.Container == "" {
			return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)
		}
		return consulting(leadsByKind(ctx, chart, actorOf(p), o), adminGrant)

	case ClassTidy:
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		// THE AUTHOR TIDIES THEIR OWN, by the login they act under or the
		// seat they hold — a bound person's writes are recorded under
		// their seat's handle.
		if o.Author != "" && (p.Login != "" && p.Login == o.Author ||
			p.Seat != "" && p.Seat == o.Author) {

			return consulting(Decision{Allowed: true, Reason: ReasonAuthor}, adminGrant)
		}
		if o.Unresolved {
			// NOBODY HAS READ THE ROW, so neither its author nor its
			// container is known: read it and decide again.
			return Decision{Reason: ReasonNotLead, Err: ErrUnresolved}
		}
		if o.Container == "" {
			return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)
		}
		return consulting(leadsByKind(ctx, chart, actorOf(p), o), adminGrant)

	case ClassSubtree:
		// THE ADMIN PATH, and it needs no chart.
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		if o.Unresolved {
			// THE BODY IS NOT READ YET. Anybody bound to a seat could
			// lead some unit, so the surface reads the write and
			// decides each place it reaches; nobody else leads any.
			if p.Seat == "" {
				return consulting(Decision{Reason: ReasonNotLead}, adminGrant)
			}
			return consulting(Decision{Allowed: true, Reason: ReasonLead}, adminGrant)
		}
		if o.Container == "" {
			// THE ROOT IS REFUSED AS A UNIT SOMEBODY ELSE LEADS IS, with
			// no chart asked: the remedy is the same permission, and the
			// surface decides a read of an id it cannot find at the root,
			// so a reason of its own would tell that read apart from a
			// seat in another team.
			return consulting(Decision{Reason: ReasonNotLead}, adminGrant)
		}
		return consulting(leadsUnit(ctx, chart, actorOf(p), o.Container), adminGrant)

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

	case ClassAdmin:
		return granted(p, adminGrant)

	case ClassDirectory:
		// THE PERSON THEMSELVES FIRST, compared on the ID and never on
		// a name. An object naming nobody — a listing — cannot reach
		// this arm, which is correct: a listing is not about anybody
		// in particular, so it falls through to admin.
		if o.Owner != "" && p.ID != uuid.Nil && p.ID.String() == o.Owner {
			return consulting(Decision{Allowed: true, Reason: ReasonSelf}, adminGrant)
		}
		if p.Can(adminGrant) {
			return consulting(Decision{Allowed: true, Reason: ReasonGrant}, adminGrant)
		}
		// NO GRANT rather than NOT SELF, because the permission is what
		// almost every caller here is missing and the self arm is the
		// exception — telling somebody without admin that they "are not
		// that person" sends them to the wrong fix.
		return consulting(Decision{Reason: ReasonNoGrant}, adminGrant)

	case ClassShared:
		return decideShared(ctx, p, r, o, chart)
	}
	// UNREACHABLE WHILE Classes AND THIS SWITCH AGREE, which a test in
	// this package asserts in both directions. It is a refusal rather
	// than a panic for the reason the unknown action above is: the safe
	// answer to "I have no rule" is no.
	return Decision{Reason: ReasonUnknownAction}
}

// decideSeatGrant is an agent seat on a row naming grant: admitted exactly
// where the chart says its role carries the grant ([Chart.SeatGranted]),
// refused [ReasonNotGranted] where it does not, and UNKNOWN where the chart
// cannot say — never a refusal, for [Chart]'s reason: a seat whose role does
// carry the grant, told on a lagging node that it does not, would go and ask
// for something it already holds.
//
// IT NAMES THE GRANT ([Decision.SeatGrant]) on every answer and NO PERMISSION
// on any: no permission a seat could hold would change it, and the remedy is
// the role's, in the company document.
func decideSeatGrant(ctx context.Context, p iam.Principal, grant SeatGrant, chart Chart) Decision {
	switch {
	case p.Seat == "":
		return Decision{Reason: ReasonNotGranted, SeatGrant: grant}
	case chart == nil:
		return Decision{Reason: ReasonNotGranted, SeatGrant: grant, Err: ErrNoChart}
	}
	held, err := chart.SeatGranted(ctx, p.Seat, grant)
	switch {
	case err != nil:
		return Decision{Reason: ReasonNotGranted, SeatGrant: grant, Err: err}
	case held:
		return Decision{Allowed: true, Reason: ReasonSeatGrant, SeatGrant: grant}
	}
	return Decision{Reason: ReasonNotGranted, SeatGrant: grant}
}

// withApp holds a RELATION class's decision to the row's app ([rule.app]).
//
// AFTER THE CLASS, so the refusal names exactly what would admit. A caller
// the relation admitted — the owner, a lead, an author — and who lacks the
// app is one app short, and told so. A caller the relation would only have
// been TOLD TO RESOLVE ([ErrUnresolved]) and who lacks the app is refused the
// same way, before anything is read: such a caller is never let through to a
// directory lookup or a row read the app would still refuse. A caller the
// relation refused is told admin, its admin path, which already covers every
// app.
//
// AND A CHART THAT COULD NOT ANSWER CANNOT ADMIT a caller the app refuses, so
// that caller is refused naming the app rather than answered unknown. Every
// arm of a row that names an app needs the app — the owner's, a lead's and an
// author's beside their relation, and the admin path through [iam.Expand],
// which is checked before the chart — so whatever the chart would have said,
// the answer was already no. Left unknown, it was a 503 telling a caller to
// come back to a refusal: a person holding no app who leads a unit, asking
// for a report's queue while the node applied a revision.
//
// UNCHANGED for a row naming no app, for a class that is not a relation
// (the app classes decide on the app themselves), and for a caller who holds
// the app — an administrator among them, through [iam.Expand] — whose unknown
// stays unknown, because for them the chart's answer is the decision.
func withApp(p iam.Principal, r rule, d Decision) Decision {
	if r.app == "" || !slices.Contains(relationClasses, r.class) || p.Can(r.app) {
		return d
	}
	// Admitted, told to resolve, or a chart that could not answer: none of
	// them can be an admission for a caller without the app.
	if d.Allowed || d.Err != nil {
		return consulting(Decision{Reason: ReasonNoGrant}, r.app)
	}
	return d
}

// granted is the permission check, with the two reasons it produces.
//
// An EMPTY permission names nothing in [Decision.Grants], because there is no
// permission that would have admitted anybody: it is a gate somebody forgot
// to fill in, and a refusal naming "" as the remedy would send a reader to
// ask for a permission that does not exist.
func granted(p iam.Principal, g iam.Grant) Decision {
	if g == "" {
		return Decision{Reason: ReasonNoGrant}
	}
	if p.Can(g) {
		return consulting(Decision{Allowed: true, Reason: ReasonGrant}, g)
	}
	return consulting(Decision{Reason: ReasonNoGrant}, g)
}

// consulting records which permissions the rule that decided would have
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

// leadsByKind asks the chart about the container a task, a project, a page or
// a page container is filed in, by the relation its KIND names.
//
// THE KIND PICKS THE RELATION, for [Chart.LeadsContainer]'s reason: a page
// container and a tracker project are two key classes a unit declares in two
// fields, and one relation asked with the other's key matches only a company
// that spelled them the same.
func leadsByKind(ctx context.Context, chart Chart, actor string, o Object) Decision {
	if o.Kind == KindContainer || o.Kind == KindPage {
		return leadsContainer(ctx, chart, actor, o.Container)
	}
	return leadsProject(ctx, chart, actor, o.Container)
}

// actorOf is the handle the CHART knows this principal by.
//
// THE SEAT, FALLING BACK TO THE LOGIN. A chart holds seats: a person — every
// person holds one — or a token bound to one is asked about by that seat's
// handle, and a principal bound to none (a token, or a service account) is
// asked about by their login, which the chart will not find — answering false, correctly, because
// they lead nobody in it.
func actorOf(p iam.Principal) string {
	if p.Seat != "" {
		return p.Seat
	}
	return p.Login
}
