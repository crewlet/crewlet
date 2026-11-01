package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WRITE AUTHORITY, and the one rule every path here obeys.
//
//	Take ONE snapshot of your own rows. Decide and form the expectation
//	inside it. Publish. Let the broker arbitrate. Never guess.
//
// [statelog.Publisher] is what enforces it; this file is the identity
// estate's decides.
//
// # What this domain adds to the rule
//
// UNIQUENESS IS DECIDED ON ONE SUBJECT. There is no unique index anywhere in
// this estate and there cannot be one, so every write that sets or frees a
// login, an address or a seat is a record on the DIRECTORY subject
// ([Writer.publishDirectory]): its decide reads the whole directory in its own
// snapshot and refuses a value somebody else holds, and the broker refuses a
// second write decided from the same snapshot. What the snapshot cannot vouch
// for is a record this node RETAINED, which still moved the subject's anchor
// without writing its rows — so a decide that takes a value first asks whether
// any such record exists ([wholeDirectory]).
//
// AND THE READ INSIDE THE SNAPSHOT IS ADVISORY WHERE IT CROSSES A LOG. A seat
// binding is checked against the ORG CHART, which is a different domain on a
// different stream: nothing orders the two, so a bind and a seat's removal can
// both be valid and both win. The residue — a person bound to a seat the chart
// no longer has — is a LEGAL NAMED STATE the session layer answers with a 403
// naming the seat, not a state this domain can prevent. The check is here to
// catch a typo and to hold every binding to a HUMAN seat
// ([Writer.humanSeat]), and it says so.
//
// # Why the actor is on the writer and never on the call
//
// An authentication trail whose author field is chosen by the caller is not a
// trail. So a surface acts as exactly one party: the identity comes from the
// surface's own IMMUTABLE context — the credential on a request, the session
// it resolved — and a surface serving many parties takes one writer per party
// through [Writer.As]. Neither a model nor a request body can reach it.

// Writer is one party's authority to change the identity estate.
type Writer struct {
	publisher *statelog.Publisher

	// db is the replicated estate. It is never the write path's own
	// snapshot — that is the framework's, taken per append — and nothing
	// decided against it is paired with an expectation. A READER, because
	// nothing here writes it: only the log's applier does.
	db store.ReplicatedReader

	// blinds derives the blind a directory decide compares an address by,
	// and sealer seals the values that belong to somebody.
	//
	// A WRITER WITH NEITHER CAN STILL DO MOST OF THIS. Ending a session,
	// bumping an epoch, suspending somebody and removing them need no key
	// at all — which is what lets access be revoked while the company's
	// secret store, which holds the blind-index key, cannot be reached:
	// the one operation an outage must never block.
	//
	// THE BLINDER IS RESOLVED PER WRITE and never held from construction:
	// see [Blinds] for what holding it cost.
	blinds Blinds
	sealer *Sealer

	// seats is the organisation this node runs, which every seat binding
	// is checked against. Nil refuses every one as unavailable: see
	// [Writer.humanSeat].
	seats SeatLookup

	// Actor and ActorKind are who this writer acts as. ActorKind is the
	// PRINCIPAL's kind — person, machine, engine — because that is what
	// this domain's own records carry; the node's own writer is
	// [NodeKind].
	Actor     string
	ActorKind iam.Kind

	// OperatorID is the credential this writer's party acts THROUGH —
	// [iam.Actor.OperatorID], a machine token's `pat:<id>` beside the owner
	// it acts as, a browser session's `session:<lineage>` — so a token's
	// gesture is told apart from its owner's in both trails: on every
	// event this writer announces, and on every record it writes
	// ([MutationRecord.OperatorID]).
	//
	// EMPTY ON THE NODE'S OWN WRITER, which acts through no credential.
	//
	// Set on a party [Writer.As] derived, and never carried by As from the
	// writer it cloned: it is the party's, like the grants. [Writer.For]
	// sets it to the credential of the person it writes for.
	OperatorID string

	// Principal is the id of the principal this writer's party IS — a
	// person's or a machine's own id in this directory when the party is
	// one of them — and empty for a party that is nobody here: the node's
	// own writer.
	//
	// ON THE PARTY AND NEVER ON A CALL, for the actor's reason. A gesture
	// decided on WHO is making it — a person's token is theirs alone to
	// mint ([Writer.mayMintFor]) — used to be decided on a field of the
	// call the caller filled in, so the domain's rule was exactly as strong
	// as the one route that filled it correctly, and any other caller could
	// state a minter of its choosing on a writer acting as anybody. It is
	// the party's, derived with the rest of it from the principal
	// ([Writer.As]), and a caller holding a writer cannot restate it.
	Principal string

	// Grants is what this writer's party is entitled to. A writer with
	// none is a REAL PARTY rather than a misconfiguration — a person
	// changing their own password holds no administrative capability —
	// so the zero value is fail-closed rather than refused.
	Grants []iam.Grant

	// Now is the writer's clock, for the AUTHORED instant only. Nothing
	// this clock produces reaches a row: every instant the applier stores
	// is the broker's, which is what makes one node's copy byte-identical
	// to another's.
	Now func() time.Time

	// events is where a landed record's decision is announced. See
	// [Events].
	events Events
}

// Events is where this writer announces what a landed record DECIDED.
//
// # Why the writer, and only for what the decide forms
//
// Two facts on the audit trail are not the caller's to state, because the
// caller does not hold them: which grants a person write ADDED and REMOVED —
// the before is read inside the snapshot the record is formed in, and a caller
// that read it separately would be describing a different transaction — and
// which session GENERATION a company-wide invalidation moved to, which is read
// and incremented in the same place. Everything else on the trail is known to
// the surface that asked, and is announced there.
//
// ONLY ON A DEFINITE OUTCOME. An applied or a pending write is durable at its
// position and every data node will apply it; an `unknown` one may or may not
// have landed, and the durable half of the trail — the `iam_history` row the
// applier writes — is what answers that. A live row claiming a change that did
// not happen would be the one false line in the feed.
//
// Consumer-defined, one method; the node's audit trail is what satisfies it.
// Nil announces nothing, which is a writer in a test or a tool.
type Events interface {
	Emit(ctx context.Context, payload events.Payload)
}

// SeatLookup is the organisation this node runs, as narrowly as a seat binding
// asks it: whether it holds a seat at a HANDLE (ADR-0013), and what kind of
// seat that is — every binding names a human one ([Writer.humanSeat]).
//
// DECLARED HERE, by the consumer, in the shape the session layer's own seam
// takes ([session.Chart]), so the engine's one view of its running org
// satisfies both. An error is never "no such seat": [session.ErrNoCompany] is
// a node running no company yet, which the writer refuses as [ErrNoCompany],
// and any other is the unknown arm.
type SeatLookup interface {
	Seat(ctx context.Context, handle string) (session.Seat, bool, error)
}

// NodeKind is the principal kind the node's own writer authors as: the sweeps,
// a re-seal, a password re-hashed at this build's cost, a company-wide
// invalidation — everything the engine decides for itself — and the kind a
// generation record that names nobody records the node under.
//
// THE ENGINE, NEVER A MACHINE. A machine is a token with nobody behind it, and
// a row authored by one is drawn as that credential's login; written as one,
// every row the engine wrote for itself read in the trail as a service account
// named after the node id, a login no machine grammar admits
// ([iam.Principal.Validate]). [iam.KindEngine] is the kind that exists so the
// engine's housekeeping is never mistaken for somebody's decision, and the
// tracker's node writer is its `system` already.
const NodeKind = iam.KindEngine

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Blinds derives address blinds, and Sealer seals the values that
	// belong to somebody. Both optional: see [Writer.blinds].
	Blinds Blinds
	Sealer *Sealer

	// Seats is the organisation this node runs — the org chart of the
	// configuration epoch it applied — which every seat binding is checked
	// against: a create, an invitation, a redemption and a bind. Optional,
	// and a writer handed none refuses every one of them as unavailable
	// rather than binding unchecked — which, since every person holds a
	// seat, is a writer that can create no person and invite nobody.
	Seats SeatLookup

	// Actor and ActorKind are who the writer acts as, until [Writer.As]
	// or [Writer.For] derives another: the node's own writer is its id and
	// [NodeKind].
	Actor     string
	ActorKind iam.Kind
	Grants    []iam.Grant
	Now       func() time.Time

	// Events announces what a landed record decided. Optional; see
	// [Events].
	Events Events
}

// NewWriter builds one party's authority.
//
// A WRITER WITH NO ACTOR IS REFUSED at construction rather than writing an
// empty author column: an unattributed change in THIS domain is one nobody can
// be asked about, and "" is indistinguishable from a surface that forgot.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("iamdomain: a writer with no publisher has no " +
			"way to append, so every call would report a change nobody made")
	case deps.DB.IsZero():
		return nil, errors.New("iamdomain: a writer with no store cannot take " +
			"the snapshot every decide is made inside")
	case deps.Actor == "":
		return nil, errors.New("iamdomain: a writer with no actor would write " +
			"an empty author column — an unattributed identity change is one " +
			"nobody can be asked about, and it is indistinguishable from a " +
			"surface that forgot to say")
	case !deps.ActorKind.Valid():
		return nil, fmt.Errorf("iamdomain: actor kind %q is not one this build "+
			"knows, and the kind is a COLUMN rather than a prefix on the name "+
			"— a row carrying an unknown one cannot be filtered or attributed",
			deps.ActorKind)
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	// SAFE FOR CONCURRENT USE: nothing a call writes outlives it, so the
	// node's own writer serves the sign-in surface, the duties and every
	// request that acts as the deployment at once.
	return &Writer{
		publisher: deps.Publisher, db: deps.DB,
		blinds: deps.Blinds, sealer: deps.Sealer, seats: deps.Seats,
		Actor: deps.Actor, ActorKind: deps.ActorKind,
		Grants: deps.Grants, Now: now, events: deps.Events,
	}, nil
}

// As is a writer for the party a principal is, sharing this one's plumbing.
//
// THE PARTY IS THE PRINCIPAL, derived whole and in one place: the name its
// records and events carry and the credential beside it ([iam.ActorFor]), its
// kind, its grants and its id. A caller used to hand those over one argument
// at a time and set the credential after, so a party was whatever combination
// somebody assembled — and the one fact a gesture decides on who is making it,
// the principal's id, was not on it at all.
//
// EVERYTHING IS REPLACED rather than carried from the writer this clones,
// which is the whole point: a surface serving many parties derives one writer
// per request, and grants, a credential or an id that carried forward would
// hand the next caller the last one's authority — or the last one's name on
// their events. Deriving one is safe from any goroutine: nothing it copies is
// ever written after construction.
func (w *Writer) As(p iam.Principal) *Writer {
	if w == nil {
		return nil
	}
	actor := iam.ActorFor(p)
	next := *w
	next.Actor = actor.Name
	next.ActorKind = p.Kind
	next.Grants = p.Grants
	next.OperatorID = actor.OperatorID
	next.Principal = ""
	if p.ID != uuid.Nil {
		next.Principal = p.ID.String()
	}
	return &next
}

// For is this writer's authority writing on somebody's behalf: the records it
// publishes, and the facts it announces, name them as the author — the name,
// the kind and the credential [iam.ActorFor] gives them — while what it may do
// is still this writer's own.
//
// FOR THE GESTURES A PERSON MAKES THROUGH THE NODE: signing in and out, a
// second factor's spend, an invitation's redemption, a reset link, enrolling
// their own authenticator. The node's own writer authors those, because the
// person holds no permission the record asks of its party — a redemption's
// enrolment needs admin, and nobody is a principal before they have
// signed in — and the trail named the node as WHO for every one of them, so
// it could not say who had signed in, only that somebody had. The node only
// wrote the record; the person made the gesture. What the engine decides for
// itself — a sweep, a re-seal, a password re-hashed at this build's cost —
// keeps the node as its author. And what a redemption CONFERS is announced as
// the decision of whoever issued the invitation ([Writer.Redeem]), never as the
// person it writes for: they redeemed the link, and chose none of its grants.
//
// Unlike [Writer.As] it carries no grants and no id from the principal: a
// party's authority is never widened or narrowed by whom it writes for.
func (w *Writer) For(p iam.Principal) *Writer {
	if w == nil {
		return nil
	}
	actor := iam.ActorFor(p)
	next := *w
	next.Actor = actor.Name
	next.ActorKind = p.Kind
	next.OperatorID = actor.OperatorID
	return &next
}

// announce publishes one decided fact once its record is known to have landed.
//
// NEVER FOR A COLLAPSED CALL ([statelog.Result.Collapsed]). A collapsed answer
// is an operation that landed as a copy this call cannot prove is its own —
// found in the ledger before this call's decide ran, acknowledged by the broker
// as a duplicate, or found in the ledger when an ambiguous append was resolved
// — so the facts an announcement carries, which a decide reads inside its own
// snapshot, may describe a decision nothing published or one never taken at
// all: a generation of zero, a grant delta from nothing. A copy that landed was
// announced by the call that made it where that call saw a definite outcome of
// its own; where none did, the `iam_history` row its apply wrote is the trail,
// as it is for every unknown.
func (w *Writer) announce(ctx context.Context, result statelog.Result, err error,
	payload events.Payload) {

	if w.events == nil || err != nil || result.Collapsed ||
		result.Outcome == statelog.OutcomeUnknown {
		return
	}
	w.events.Emit(ctx, payload)
}

// ErrCollapsed reports a gesture whose operation LANDED but whose ANSWER this
// call cannot give: the answer is computed inside the decide — the counters a
// session bearer carries, what a minted token was granted — and the framework
// answered the call with a copy of the operation it cannot prove is this
// call's own ([statelog.Result.Collapsed]), so the decide that computed the
// answer may never have run, or may not be the one published.
//
// UNAVAILABLE rather than a fault or an outcome, and on purpose: the operation
// did land, so it is not unknown, and a caller handed a value that describes
// another decision builds a credential on it — a bearer carrying an epoch the
// session was not opened at is one a revocation between the two does not end.
// What the caller does next is what every unknown's caller does for a
// credential: the landed copy is one nobody holds, it expires, and a fresh
// operation mints another.
var ErrCollapsed = fmt.Errorf("this change can't be confirmed. Send it again "+
	"with a new key: %w", statelog.ErrUnavailable)

// errNothingToPublish is what a decide returns when its snapshot shows the
// write has nothing left to do — a conditional revocation whose epoch has
// already moved past the one it was asked about. [Writer.request] turns it
// into the framework's empty decision, which answers `applied` with no record.
var errNothingToPublish = errors.New("iamdomain: nothing to publish")

// grantDelta is what one write added to a permission set and what it took
// away, each sorted, so two nodes describing one write describe it
// identically.
//
// COMPARED THROUGH [iam.Expand] ON BOTH SIDES, as every comparison of
// permissions is: an app an administrator already held through admin is not
// "added" by a save that spells it out, so saving `[admin]` as `[admin,
// worklet, doclet]` announces no change and revokes no reset link, while
// taking admin away from `[admin]` removes admin and every app it carried.
// What is reported is still the row's own names — a name this build does not
// know included — because the trail records what the rows said.
func grantDelta(before, after []iam.Grant) (added, removed []string) {
	held, holds := iam.Expand(before), iam.Expand(after)
	for _, g := range holds {
		if !slices.Contains(held, g) && !slices.Contains(added, string(g)) {
			added = append(added, string(g))
		}
	}
	for _, g := range held {
		if !slices.Contains(holds, g) && !slices.Contains(removed, string(g)) {
			removed = append(removed, string(g))
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// Can reports whether this writer's party holds a permission, counting every
// app admin includes.
//
// FAIL-CLOSED ON AN UNKNOWN NAME, through [iam.Principal.Can]'s own rule: a
// name a newer peer wrote that this build cannot read answers false, because
// a denylist would have admitted it.
func (w *Writer) Can(g iam.Grant) bool { return iam.Principal{Grants: w.Grants}.Can(g) }

// ErrRefused reports a write this writer's party may not make.
//
// DISTINCT FROM A CONFLICT AND FROM AN OUTAGE, which is the three-valued rule
// applied to authority: "you may not", "somebody else won" and "this node
// could not tell" are three answers a surface renders as 403, 409 and 503.
//
// EVERY ONE IS A [Refusal], which says whether a permission would lift it, and
// nothing in this package builds it any other way — a gate in this package's
// tests holds that. Compare it with errors.Is; read what lifts it with
// errors.As.
var ErrRefused = errors.New("not allowed")

// Refusal is a record this writer's party may not publish, and whether a
// permission would lift it.
//
// TWO KINDS OF "MAY NOT", and a surface answers them differently. One a
// permission lifts — a record only an administrator publishes, a permission
// the party cannot confer because it does not hold it — names that permission,
// so a caller knows what to ask for. One the record's own rule refuses
// whatever the party holds — a person's token minted by anybody but them, an
// owner who may not act, an invitation already spent, a name this build does
// not know — names none, because asking for a permission would change
// nothing. Both were one sentinel wrapped in a sentence, so every refusal was
// answered as the first kind, naming every permission there is.
//
// EXPORTED, FIELDS AND ALL, because a refusal crosses the estate router's wire
// (internal/estate) whenever the node that decided it is not the node whose
// caller is told, and the wire rebuilds an error type from its exported
// fields. Only this package builds one ([needs], [forbidden]).
type Refusal struct {
	// Grants are the permissions any ONE of which, held by the party, would
	// have admitted the record — internal/authz's minimal cover: an app for
	// an app, admin for admin. EMPTY where the record's own rule refused it,
	// whatever the party holds.
	Grants []iam.Grant

	// Err is the refusal's sentence and its %w chain — [ErrTokenRefused]
	// among it for a record asked through a machine token. THE SENTENCE A
	// PERSON READS ([Sentence]), so it names what it refuses as they know
	// it — "this invitation", "that session", "their account" — and never by
	// an id they did not choose.
	Err error

	// About is what the refusal was about, by the ids an operator reading
	// the log needs and the person refused never chose: an invitation's id,
	// a session's lineage, the access token a request came through. EMPTY
	// where the sentence says it all. [Refusal.Error] carries it, which is
	// what every log line prints; [Sentence] does not. It used to be written
	// into the sentence, so a person was told "invitation 0192f… has
	// already been used" and "session 4c1e… belongs to person 0192a…".
	About string
}

func (e *Refusal) Error() string {
	text := ErrRefused.Error() + ": " + e.Err.Error()
	if e.About != "" {
		text += " (" + e.About + ")"
	}
	return text
}
func (e *Refusal) Unwrap() []error { return []error{ErrRefused, e.Err} }

// about sets what e was about for the log ([Refusal.About]), worded as
// fmt.Sprintf words format and args, and returns e.
func (e *Refusal) about(format string, args ...any) *Refusal {
	e.About = fmt.Sprintf(format, args...)
	return e
}

// needs is a refusal any one of grants would have lifted, worded as
// fmt.Errorf words format and args.
func needs(grants []iam.Grant, format string, args ...any) *Refusal {
	return &Refusal{Grants: slices.Clone(grants), Err: fmt.Errorf(format, args...)}
}

// forbidden is a refusal no permission lifts, worded as fmt.Errorf words
// format and args.
func forbidden(format string, args ...any) *Refusal {
	return &Refusal{Grants: []iam.Grant{}, Err: fmt.Errorf(format, args...)}
}

// mayAdminister refuses a record only an administrator may publish, and every
// administrative record written through a machine token.
//
// # Why the domain decides this at all, when internal/authz exists
//
// It is not a second opinion about the same question. internal/authz decides
// whether a REQUEST may reach a route; this decides whether a RECORD may be
// published, and the two have different reach: a record can be published by a
// duty, by a CLI, by a migration and by a test, none of which passes through a
// route. The domain is the last frame that sees every one of them.
//
// WHAT IT GUARDS IS THE ASYMMETRY IN THIS ESTATE: changing your own password
// and suspending somebody else are both "a record on a person's subject", and
// only the arbitration tells them apart — which is to say, not at all.
//
// # The permission is admin
//
// The NODE's own writer — which authors the invite redemption and the sweeps —
// holds admin too, so a redemption, bounded by its invitation, passes.
//
// # And never through a machine token, whatever it carries
//
// A token may be minted with admin, because what a pipeline may do is its
// owner's to choose. But an administrative identity record — enrolling
// somebody, inviting them, editing their permissions, suspending or removing
// them, ending everybody's sessions — decides who may do anything at all, and
// a token proves nobody is at a keyboard. The authority table refuses such a
// request at its row (internal/authz's `iam.write` needs a person present); this refuses
// the RECORD, because a record is formed by more than the routes that table
// guards, and the guarantee it keeps — that no token ever writes the
// directory — used to rest on a permission no token could carry, which is
// gone. CHECKED FIRST, so a token holding admin is told it is a token rather
// than that it lacks a permission it plainly has. The CLASS decides, never the
// spelling of an id: an operator id naming the machine-token class is a token,
// whether or not this build can parse what follows.
//
// # Through the Dashboard Assistant, only an approved edit
//
// A request the assistant makes is its person's own, composed from their rows,
// so an administrator's assistant carries admin, and a write it makes is one
// the administrator approved from a server-rendered preview (ADR-0051): an
// edit of somebody's row, a move of their login or seat, a stage, a removal,
// an invitation withdrawn. Those land as the administrator's own, with the
// grant named as the credential.
//
// THE SIGN-IN CEREMONIES DO NOT ([assistRefusedOps]): an enrolment carries a
// first password link or spends an invitation, an invitation is a link, and
// ending every session is a session ceremony. The authority table keeps their
// verbs from the assistant (internal/authz's human-only list, ADR-0052); this
// refuses the RECORD for the token's reason, so no surface the table does not
// guard can be the way a model is handed a credential. Checked first beside
// the token, on the class as well.
//
// # Which refusal it is
//
// A party without admin is refused NAMING admin, which lifts it; a machine
// token is refused naming nothing, since no permission on a token would — a
// person signs in — and so is the Dashboard Assistant on a sign-in ceremony,
// which its person takes themselves.
func (w *Writer) mayAdminister(op OpKind) error {
	if strings.HasPrefix(w.OperatorID, iam.MachineTokenPrefix) {
		return forbidden("%w. Sign in and do it yourself", ErrTokenRefused).
			about("%s, through %s", op, w.OperatorID)
	}
	if strings.HasPrefix(w.OperatorID, iam.AssistPrefix) && slices.Contains(assistRefusedOps, op) {
		return forbidden("%w. Do it yourself from People & access (%s, %s)",
			ErrAssistRefused, op, w.OperatorID)
	}
	if w.Can(iam.GrantAdmin) {
		return nil
	}
	return needs([]iam.Grant{iam.GrantAdmin}, "only an admin can do that").
		about("%s, by a party holding %s", op, grantWords(w.Grants))
}

// assistRefusedOps are the administrative records the Dashboard Assistant
// never writes, whoever approved what: each hands somebody a way to sign in
// or ends sessions ([Writer.mayAdminister]).
var assistRefusedOps = []OpKind{OpEnrol, OpInvite, OpInvalidate}

// ErrTokenRefused reports an identity record asked through a machine token that
// needs a person present: an administrative one ([Writer.mayAdminister]), and a
// token's mint ([Writer.mayMintFor]). Every error carrying it is a [Refusal]
// naming no permission, so a surface that only knows "may not" still refuses;
// one that tells the two apart answers that a person must be present rather
// than that a permission is missing.
var ErrTokenRefused = errors.New("access tokens can't change people or access")

// ErrAssistRefused reports an administrative identity record, or a token mint,
// asked through the Dashboard Assistant ([Writer.mayAdminister],
// [Writer.mayMintFor]). Every error carrying it is a [Refusal] naming no
// permission, for [ErrTokenRefused]'s reason, and a surface that tells them
// apart answers that the person takes the gesture themselves rather than that
// a permission is missing.
var ErrAssistRefused = errors.New("only the person can issue sign-in links, invitations or tokens, or end sessions, never Ask Crewlet")

// publish runs one decide through the framework's own write authority.
//
// EVERY PATH IN THIS FILE GOES THROUGH IT, which is what makes the actor and
// the clock impossible to forget: a decide that built its own
// [statelog.Request] would be one append that did not carry them.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (
	statelog.Result, error) {

	// NO MINT INSTANT BESIDE THE ID: the id carries its own
	// ([statelog.OpMintedAt]), and one stamped here would be this CALL's —
	// later than the mint on every retry, which is the one case the ledger
	// cannot vouch for.
	result, err := w.publisher.Publish(ctx, req)
	var refused *statelog.Unavailable
	if errors.As(err, &refused) && refused.Reason == statelog.ReasonOpReused {
		// THE FRAMEWORK'S "this id already names another write", in this
		// domain's words. Every create here derives what it creates from its
		// operation KEY, so a key reused for another request is refused by
		// the ledger before any decide runs — and a caller that answers a
		// reused key with 409 and a fresh key asks for this domain's
		// sentinel, which the framework cannot know. Both stay readable: the
		// refusal is still the framework's, with its position.
		err = fmt.Errorf("%w: %w", ErrOperationReused, err)
	}
	return result, err
}

// publishDirectory publishes one record on the DIRECTORY subject.
//
// # The scope is read before the snapshot and confirmed inside it
//
// A record's scope is stated before the framework takes the snapshot its
// decide runs in — the framework needs it to probe for a deferral and to wait
// — and a directory record's scope depends on rows: the buckets of the people
// whose removal tombstones a seat bind stamps ([leaversOf]), and of the
// address a removal erases ([eraseSealed]). So scopeOf is asked once
// outside and again inside, and a round whose snapshot names a bucket the
// scope did not publishes nothing and is read again — at most
// [directoryScopeAttempts] times.
//
// decide forms the payload in the record's own snapshot, or refuses.
func (w *Writer) publishDirectory(ctx context.Context, op OpKind, person, opID,
	reason string, scopeOf func(context.Context, *sql.Tx) (ScopeSet, error),
	decide func(*sql.Tx) ([]byte, error)) (statelog.Result, error) {

	for attempt := 1; ; attempt++ {
		var scope ScopeSet
		if err := w.db.Read(ctx, func(tx *sql.Tx) (err error) {
			scope, err = scopeOf(ctx, tx)
			return err
		}); err != nil {
			return statelog.Result{}, err
		}
		rec, err := w.record(DirectorySubject(), op, person, scope, nil, reason)
		if err != nil {
			return statelog.Result{}, err
		}
		result, err := w.publish(ctx, w.request(ctx, &rec, opID,
			statelog.PatternArbitrated, func(tx *sql.Tx) (err error) {
				var now ScopeSet
				if now, err = scopeOf(ctx, tx); err != nil {
					return err
				}
				if !scope.Covers(now) {
					return errScopeMoved
				}
				rec.Mutation, err = decide(tx)
				return err
			}))
		if !errors.Is(err, errScopeMoved) || attempt == directoryScopeAttempts {
			return result, err
		}
	}
}

// errScopeMoved is a directory record whose snapshot named a bucket its scope,
// read a moment before, did not cover. [Writer.publishDirectory] reads it
// again.
var errScopeMoved = fmt.Errorf("iamdomain: the people a directory record "+
	"writes moved between reading its scope and deciding it: %w",
	statelog.ErrConflict)

// directoryScopeAttempts bounds how often a directory record re-reads a scope
// its snapshot overtook.
//
// THREE, for the reason every bounded re-read here gives: what moves the set is
// a removal of the seat's earlier holder, or an invitation redeemed at the
// person's address, between two reads a few milliseconds apart — each of which
// lands once — and a set still moving after two re-reads is being rewritten in
// a loop, worth the conflict it answers rather than a write that spins on it.
const directoryScopeAttempts = 3

// wholeDirectory refuses a decide that reads the whole directory on a node
// that RETAINS a record it could not apply.
//
// # Why a decide that takes a value asks this
//
// A directory decide calls a login, an address or a seat FREE because no row
// in its snapshot holds it. A record this node retained — a newer build's, one
// signed under a keyring key it was not restarted with — moved the directory's
// anchor without writing its rows, so it may be the very record that took the
// value, and the broker would accept a write decided from rows that do not
// hold it. The framework's own probe ([statelog.Snap]) asks only about the
// buckets the record declares, which is the people it writes, never the
// people whose values it compares against — so this asks the WHOLE deferral
// index.
//
// THE FRAMEWORK'S OWN REFUSAL, `deferred`, so every surface already maps it to
// a 503 another node can serve. It costs a node holding any retained record its
// directory writes for the length of a rolling upgrade, which is when they are
// rarest. A REMOVAL does not ask it: it frees values rather than taking one,
// and a record retained about the same person is refused by the framework's
// probe over the removal's own buckets.
func wholeDirectory(ctx context.Context, tx *sql.Tx) error {
	d, hit, err := statelog.DeferredIn(ctx, tx, Domain{},
		RootScope().Resolve(DirectorySubject()))
	if err != nil {
		return fmt.Errorf("iamdomain: ask whether this node retains a record "+
			"the directory may hold: %w", err)
	}
	if !hit {
		return nil
	}
	return &statelog.Unavailable{
		Reason:   statelog.ReasonDeferred,
		Position: d.Position,
		Detail: fmt.Sprintf("this node holds a record at version %d it cannot "+
			"decode, at %s, and a directory decision reads every person's "+
			"login, address and seat — that record may hold one of them, so "+
			"another node can decide this", d.Version, d.Position),
	}
}

// record builds one of this writer's records, with everything the writer
// carries already on it.
func (w *Writer) record(subject Subject, op OpKind, person string,
	scope ScopeSet, mutation []byte, reason string) (MutationRecord, error) {

	if err := subject.Validate(); err != nil {
		return MutationRecord{}, err
	}
	if err := scope.Validate(subject); err != nil {
		return MutationRecord{}, err
	}
	if len(reason) > MaxReason {
		return MutationRecord{}, fmt.Errorf("%w: the reason is %d bytes, and "+
			"the limit is %d. Keep it to a short label",
			ErrInvalid, len(reason), MaxReason)
	}
	rec := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// THE VERSION IS LEFT TO THE ENCODER, which stamps the lowest
			// that carries what the record holds ([Encode]). See
			// [RecordVersion] for why never the ceiling.
			Subject:   subject,
			Op:        op,
			CreatedAt: w.Now().UTC(),
			Scope:     scope,
		},
		Mutation:   mutation,
		Person:     person,
		Actor:      w.Actor,
		ActorKind:  w.ActorKind,
		OperatorID: w.OperatorID,
		Reason:     reason,
	}
	return rec, nil
}

// request wraps one record in the framework's own request shape.
//
// THE RECORD IS TAKEN BY POINTER, and that is what lets a decide FILL IT. Half
// the gestures in this domain form their payload inside the snapshot — a
// revocation reads the epoch it is bumping, an identity change reads the login
// and the seat it leaves alone, a removal reads the values it is releasing — and
// the framework may run a decide AGAIN against a fresh snapshot, so the
// mutation the encode below reads has to be the one the last run produced.
// Taken by value, every one of those wrote into a copy nothing encoded and
// published an empty payload that every data node then failed to decode.
func (w *Writer) request(ctx context.Context, rec *MutationRecord, opID string,
	pattern statelog.Pattern, decide func(*sql.Tx) error) statelog.Request {

	// THE OP ID GOES ON THE RECORD, not only on the request. The framework
	// reads the ledger row back by the id the ENVELOPE carries, so a record
	// published under one and applied under another is an operation nothing
	// can resolve an ambiguous publish against — which the framework
	// refuses by name rather than letting it look like it landed.
	rec.OpID = opID
	return statelog.Request{
		Subject: statelog.Subject{
			Kind: string(rec.Subject.Kind), ID: rec.Subject.ID,
		},
		Scope:   rec.Scope.Resolve(rec.Subject),
		OpID:    opID,
		Pattern: pattern,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if err := removedPersonIn(ctx, tx, *rec); err != nil {
				return statelog.Decision{}, err
			}
			if decide != nil {
				if err := decide(tx); errors.Is(err, errNothingToPublish) {
					// THE FRAMEWORK'S OWN "NOTHING TO WRITE": an empty
					// decision answers applied, with no position,
					// because no record exists.
					return statelog.Decision{}, nil
				} else if err != nil {
					return statelog.Decision{}, err
				}
			}
			// THE STAMP IS THE FRAMEWORK'S ([statelog.Stamp]): the node
			// that writes the record and the generation of the snapshot it
			// was decided from — which every applier's eviction gate reads,
			// and the publisher refuses a record without. Set on every
			// round, since each round is a fresh snapshot.
			rec.Writer, rec.Gen = stamp.Writer, stamp.Gen
			payload, err := Encode(*rec)
			if err != nil {
				return statelog.Decision{}, err
			}
			return statelog.Decision{Payload: payload}, nil
		},
	}
}

// removedPersonIn refuses, inside a decide's snapshot, a record about a person
// a removal has destroyed whose SUBJECT is not that person's own — a directory
// record, a session.
//
// # Why the domain asks here and the framework does not
//
// The framework's guard reads the deletion marker on the record's SUBJECT
// ([iamGuards]), and only a person's own subject carries one. Every other
// record about somebody names them in its PAYLOAD, which the applier's removal
// gate decodes — and which the publisher-side reader reads too only for the
// body it is handed ([markedPerson]): a record this node appended and a removal
// raced is named `deleted` there, but a person removed BEFORE the snapshot
// would have had the record appended only to be dropped by every data node — a
// second removal of somebody already removed among them. Refused here, it is
// refused as what it is before anything is appended, with the reason the
// resolution would have given for a person's own subject.
//
// ONE PRIMARY-KEY READ, and only on a record that names a person: an
// invitation, a sweep, an invalidation and the log's own gates name nobody.
func removedPersonIn(ctx context.Context, tx *sql.Tx, rec MutationRecord) error {
	if rec.Person == "" || rec.Subject.Kind == KindPerson {
		return nil
	}
	var removed bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM iam_removed WHERE person_id = ?)`,
		rec.Person).Scan(&removed); err != nil {
		return fmt.Errorf("iamdomain: read whether person %s was removed: %w",
			rec.Person, err)
	}
	if !removed {
		return nil
	}
	return &statelog.Unavailable{
		Reason: statelog.ReasonDeleted,
		Detail: fmt.Sprintf("person %s was removed and stays removed, so a %s "+
			"on %s about them applies on no node", rec.Person, rec.Op, rec.Subject),
		OpID: rec.OpID,
	}
}
