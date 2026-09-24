package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
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
// EVERY CLAIM IS A CREATE AT ZERO. There is no unique index anywhere in this
// estate and there cannot be one, so an address, a login and a seat binding are
// kept single-holder by the broker refusing the second publish on their
// subject — which means the create pattern is not an occasional path here, it
// is the hot path of every enrolment, every address change and every seat
// assignment. [Fence.ClearForZero] is what stops a node below the trim floor
// guessing that an absent anchor means an unclaimed address.
//
// AND THE READ INSIDE THE SNAPSHOT IS ADVISORY WHERE IT CROSSES A LOG. A seat
// binding is checked against the ORG CHART, which is a different domain on a
// different stream: nothing orders the two, so a bind and a seat's removal can
// both be valid and both win. The residue — a person bound to a seat the chart
// no longer has — is a LEGAL NAMED STATE the session layer answers with a 403
// naming the seat, not a state this domain can prevent. The check is here to
// catch a typo, and it says so.
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
	// decided against it is paired with an expectation.
	db *store.DB

	// blinds derives the subject a claim on an address arbitrates on, and
	// sealer seals the values that belong to one person.
	//
	// A WRITER WITH NEITHER CAN STILL DO MOST OF THIS. Ending a session,
	// bumping an epoch, suspending somebody and removing them need no key
	// at all — which is what lets a node with no keyring still revoke
	// access, the one operation an outage must never block.
	//
	// THE BLINDER IS RESOLVED PER WRITE and never held from construction:
	// see [Blinds] for what holding it cost.
	blinds Blinds
	sealer *Sealer

	// Actor and ActorKind are who this writer acts as. ActorKind is the
	// PRINCIPAL's kind — person, machine, engine — because that is what
	// this domain's own records carry.
	Actor     string
	ActorKind iam.Kind

	// authorKind is the same party in the vocabulary every OTHER trail
	// records an author in — iam.ActorFor's agent, human, operator or
	// system — for the one write this domain makes outside its own log: a
	// key in the company's secret store ([Writer.secretAuthor]).
	authorKind iam.ActorKind

	// OperatorID is the credential this writer's party acts THROUGH —
	// [iam.Actor.OperatorID], a machine token's `pat:<id>` beside the owner
	// it acts as — announced beside Actor on every event this writer
	// publishes, so a token's gesture is told apart from its owner's on the
	// audit feed.
	//
	// ON THE EVENTS AND NOT ON THE RECORD. The record carries its actor
	// alone, because `iam_history` is re-encoded from the record and is in
	// this domain's identity claim — a field an older build carries rather
	// than knows re-encodes to different bytes on the two builds mid-upgrade
	// — and the three gate records are pinned at [GateRecordVersion] for
	// ever. Carrying it on the log is a record version of its own, with the
	// deferral on older nodes that buys.
	//
	// Set on a party [Writer.As] derived, and never carried by As from the
	// writer it cloned: it is the party's, like the grants.
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

	// seq is this writer's SEQUENCE — the high-water mark a party's
	// successive writes hand forward, so each decides from a state
	// containing the one before it — or nil on a SHARED writer.
	//
	// # Two kinds of writer, and why the shared one carries no mark
	//
	// [NewWriter] builds the SHARED writer: the node's own, handed to the
	// identity duties, the sign-in surface and every request that acts as
	// the deployment, concurrently. It used to carry the mark itself, and
	// that was a data race — the sweep, the probe and every sign-in
	// advanced one unguarded field from their own goroutines — and a wrong
	// answer even where it did not race: one gesture's position became the
	// session wait of an unrelated one, so a sign-in waited for the sweep's
	// records to apply. So a shared writer holds nothing mutable, and each
	// of its calls is a gesture of its own: a multi-step call (an
	// enrolment is claim, claim, person) sequences its own steps and hands
	// nothing to the next call.
	//
	// [Writer.As] builds a SEQUENCE: one party's writer, for one request,
	// whose calls are ordered — the directory surface's enrol-then-bind is
	// two calls, and the bind's decide reads the enrolment's row. A
	// sequence is one goroutine's and is not safe for concurrent use; the
	// surface derives one per request precisely so that it never is.
	seq *sequence

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
// position and every node will apply it; an `unknown` one may or may not have
// landed, and the durable half of the trail — the `iam_history` row the
// applier writes — is what answers that. A live row claiming a change that did
// not happen would be the one false line in the feed.
//
// Consumer-defined, one method; the node's audit trail is what satisfies it.
// Nil announces nothing, which is a writer in a test or a tool.
type Events interface {
	Emit(ctx context.Context, payload events.Payload)
}

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        *store.DB

	// Blinds derives claim subjects, and Sealer seals a person's own
	// values. Both optional: see [Writer.blinds].
	Blinds Blinds
	Sealer *Sealer

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
	case deps.DB == nil:
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
	// SHARED: no sequence, so nothing about one call survives into the
	// next and the writer is safe for concurrent use. See [Writer.seq].
	return &Writer{
		publisher: deps.Publisher, db: deps.DB,
		blinds: deps.Blinds, sealer: deps.Sealer,
		Actor: deps.Actor, ActorKind: deps.ActorKind,
		authorKind: iam.ActorFor(iam.Principal{
			Kind: deps.ActorKind, Login: deps.Actor}).Kind,
		Grants: deps.Grants, Now: now, events: deps.Events,
	}, nil
}

// As is a writer for the party a principal is, sharing this one's plumbing,
// whose calls form ONE SEQUENCE.
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
// their events.
//
// THE SEQUENCE IS FRESH, and deriving one is safe from any goroutine: nothing
// it copies is ever written after construction, so a sign-in publishing
// through the shared writer and a request deriving its own from it at the same
// instant touch no common state.
func (w *Writer) As(p iam.Principal) *Writer {
	if w == nil {
		return nil
	}
	actor := iam.ActorFor(p)
	next := *w
	next.Actor = actor.Name
	next.ActorKind = p.Kind
	next.authorKind = actor.Kind
	next.Grants = p.Grants
	next.OperatorID = actor.OperatorID
	next.Principal = ""
	if p.ID != uuid.Nil {
		next.Principal = p.ID.String()
	}
	next.seq = &sequence{}
	return &next
}

// secretAuthor is this writer's party as the company's secret store records
// an author: the name, its kind in the actor vocabulary, and the credential it
// acted through — empty for the node's own writer, which acted through none.
func (w *Writer) secretAuthor() secrets.Author {
	return secrets.Author{Name: w.Actor, Kind: string(w.authorKind),
		OperatorID: w.OperatorID}
}

// sequence is one party's high-water mark across the calls of a request.
type sequence struct{ after statelog.Position }

// gesture is the mark one call's steps hand forward: the writer's own sequence
// when it has one, and a fresh mark of the call's own when it is shared.
func (w *Writer) gesture() *statelog.Position {
	if w.seq != nil {
		return &w.seq.after
	}
	return new(statelog.Position)
}

// announce publishes one decided fact once its record is known to have landed.
func (w *Writer) announce(ctx context.Context, result statelog.Result, err error,
	payload events.Payload) {

	if w.events == nil || err != nil || result.Outcome == statelog.OutcomeUnknown {
		return
	}
	w.events.Emit(ctx, payload)
}

// errNothingToPublish is what a decide returns when its snapshot shows the
// write has nothing left to do — a conditional revocation whose epoch has
// already moved past the one it was asked about. [Writer.request] turns it
// into the framework's empty decision, which answers `applied` with no record.
var errNothingToPublish = errors.New("iamdomain: nothing to publish")

// unresolved is a SEQUENCE's answer when one of its steps could not be
// resolved: unknown, under the GESTURE's operation id.
//
// # Three outcomes stay three, a step's included
//
// An enrolment claims an address, then a login, then writes the person; a
// rename claims the new login and then releases the old. A step whose outcome
// nothing can establish may or may not be on the log, and the step after it
// would be built on a guess — so the gesture stops there and says unknown,
// exactly as a single write would. It is answered under the gesture's own id
// because that is what a caller retries under: every step's id is derived
// from it, so the retry lands exactly the steps that did not.
func unresolved(opID string) statelog.Result {
	return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: opID}
}

// grantDelta is what one write added to a grant set and what it took away,
// each sorted, so two nodes describing one write describe it identically.
func grantDelta(before, after []iam.Grant) (added, removed []string) {
	for _, g := range after {
		if !slices.Contains(before, g) && !slices.Contains(added, string(g)) {
			added = append(added, string(g))
		}
	}
	for _, g := range before {
		if !slices.Contains(after, g) && !slices.Contains(removed, string(g)) {
			removed = append(removed, string(g))
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// Can reports whether this writer's party holds a grant.
//
// FAIL-CLOSED ON AN UNKNOWN GRANT, through [iam.Principal.Can]'s own rule: a
// grant a newer peer wrote that this build cannot name answers false, because
// a denylist would have admitted it.
func (w *Writer) Can(g iam.Grant) bool {
	for _, held := range w.Grants {
		if held == g {
			return true
		}
	}
	return false
}

// ErrRefused reports a write this writer's party may not make.
//
// DISTINCT FROM A CONFLICT AND FROM AN OUTAGE, which is the three-valued rule
// applied to authority: "you may not", "somebody else won" and "this node
// could not tell" are three answers a surface renders as 403, 409 and 503.
var ErrRefused = errors.New("iamdomain: this party may not author that record")

// mayAdminister refuses a record only an administrator may publish.
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
// # The grant is people:manage, and it used to be config:write
//
// That was wrong in both directions and wrong loudly in one. An automation
// holding the company's own grant could enrol itself a colleague, which is
// the escalation this estate exists to close; and the NODE's own writer —
// which authors the bootstrap enrolment, the invite redemption, the sweeps
// and the deactivation probe — holds [iam.GrantFleetOperate] and never
// config:write, so every one of those paths was refused. Nothing noticed,
// because the surface that exercises them builds a stub writer.
func (w *Writer) mayAdminister(op OpKind) error {
	if w.Can(AdminGrant) {
		return nil
	}
	return fmt.Errorf("%w: %s requires %s, and this party holds %v", ErrRefused,
		op, AdminGrant, w.Grants)
}

// AdminGrant is the capability every administrative record in this domain
// requires.
//
// EXPORTED so the parties that must hold it can be CHECKED rather than
// remembered. The node's own writer is one — it authors the bootstrap
// enrolment and the invite redemption on behalf of people who have no
// principal yet — and a grant list that drifts away from this one refuses
// every enrolment on a fresh deployment, silently, on a path whose tests use
// a stub writer.
const AdminGrant = iam.GrantPeopleManage

// publish runs one decide through the framework's own write authority, as a
// gesture of its own on a shared writer or as the next step of a sequence.
//
// EVERY PATH IN THIS FILE GOES THROUGH IT or through [Writer.publishAt], which
// is what makes the actor, the clock and the high-water mark impossible to
// forget: a decide that built its own [statelog.Request] would be one append
// that did not carry them.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (
	statelog.Result, error) {

	return w.publishAt(ctx, w.gesture(), req)
}

// publishAt runs one STEP of a gesture that writes more than once, waiting for
// the mark the steps before it left and advancing it.
func (w *Writer) publishAt(ctx context.Context, at *statelog.Position,
	req statelog.Request) (statelog.Result, error) {

	req.Session = *at
	req.MintedAt = w.Now()
	result, err := w.publisher.Publish(ctx, req)
	if err == nil && result.Position.Packed() > at.Packed() {
		*at = result.Position
	}
	return result, err
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
		return MutationRecord{}, fmt.Errorf("%w: the reason on this %s is %d "+
			"bytes and the cap is %d — it is rendered into an authentication "+
			"trail beside the op that caused it, so it says WHICH cause fired "+
			"rather than narrating", ErrInvalid, op, len(reason), MaxReason)
	}
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// THE LOWEST VERSION THAT CARRIES THE OP'S MEANING — a
			// gate pinned for ever, a sweep at the version whose
			// predicate it states, everything else at the base. See
			// [RecordVersion] for why never the ceiling.
			V:         writeVersion(op),
			Subject:   subject,
			Op:        op,
			CreatedAt: w.Now().UTC(),
			Scope:     scope,
		},
		Mutation:  mutation,
		Person:    person,
		Actor:     w.Actor,
		ActorKind: w.ActorKind,
		Reason:    reason,
	}, nil
}

// request wraps one record in the framework's own request shape.
//
// THE RECORD IS TAKEN BY POINTER, and that is what lets a decide FILL IT. Half
// the gestures in this domain form their payload inside the snapshot — a
// revocation reads the epoch it is bumping, a seat claim reads the chart
// position it was decided at, a removal reads the claims it is releasing — and
// the framework may run a decide AGAIN against a fresh snapshot, so the
// mutation the encode below reads has to be the one the last run produced.
// Taken by value, every one of those wrote into a copy nothing encoded and
// published an empty payload that every node then failed to decode.
func (w *Writer) request(rec *MutationRecord, opID string,
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
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
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
			payload, err := Encode(*rec)
			if err != nil {
				return statelog.Decision{}, err
			}
			env, err := Domain{}.Envelope(payload)
			if err != nil {
				return statelog.Decision{}, err
			}
			return statelog.Decision{Payload: payload, Envelope: env}, nil
		},
	}
}
