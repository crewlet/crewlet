package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
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

	// blinder derives the subject a claim on an address arbitrates on, and
	// sealer seals the values that belong to one person.
	//
	// A WRITER WITH NEITHER CAN STILL DO MOST OF THIS. Ending a session,
	// bumping an epoch, suspending somebody and removing them need no key
	// at all — which is what lets a node with no keyring still revoke
	// access, the one operation an outage must never block.
	blinder *Blinder
	sealer  *Sealer

	// Actor and ActorKind are who this writer acts as.
	Actor     string
	ActorKind iam.Kind

	// Grants is what this writer's party is entitled to. A writer with
	// none is a REAL PARTY rather than a misconfiguration — a person
	// changing their own password holds no administrative capability —
	// so the zero value is fail-closed rather than refused.
	Grants []iam.Grant

	// after is this writer's own high-water mark, handed from step to step
	// by a gesture that writes more than once. An enrolment is exactly
	// that: claim the address, claim the login, write the person, each on
	// its own subject, each needing to decide from a state containing the
	// one before it.
	after statelog.Position

	// Now is the writer's clock, for the AUTHORED instant only. Nothing
	// this clock produces reaches a row: every instant the applier stores
	// is the broker's, which is what makes one node's copy byte-identical
	// to another's.
	Now func() time.Time
}

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        *store.DB

	// Blinder derives claim subjects, and Sealer seals a person's own
	// values. Both optional: see [Writer.blinder].
	Blinder *Blinder
	Sealer  *Sealer

	Actor     string
	ActorKind iam.Kind
	Grants    []iam.Grant
	Now       func() time.Time
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
	return &Writer{
		publisher: deps.Publisher, db: deps.DB,
		blinder: deps.Blinder, sealer: deps.Sealer,
		Actor: deps.Actor, ActorKind: deps.ActorKind,
		Grants: deps.Grants, Now: now,
	}, nil
}

// As is a writer for another party, sharing this one's plumbing.
//
// THE GRANTS REPLACE rather than accumulate, which is the whole point: a
// surface serving many parties derives one writer per request, and grants that
// carried forward would hand the next caller the last one's authority.
func (w *Writer) As(actor string, kind iam.Kind, grants []iam.Grant) *Writer {
	if w == nil {
		return nil
	}
	next := *w
	next.Actor = actor
	next.ActorKind = kind
	next.Grants = grants
	next.after = statelog.Position{}
	return &next
}

// After is this writer's own high-water mark, which a multi-step gesture hands
// from step to step so each decides from a state containing the one before it.
func (w *Writer) After() statelog.Position { return w.after }

// advance records a landed position as this writer's own mark.
func (w *Writer) advance(at statelog.Position) {
	if at.Packed() > w.after.Packed() {
		w.after = at
	}
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

// publish runs one decide through the framework's own write authority.
//
// EVERY PATH IN THIS FILE GOES THROUGH IT, which is what makes the actor, the
// clock and the writer's high-water mark impossible to forget: a decide that
// built its own [statelog.Request] would be one append that did not carry them.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (
	statelog.Result, error) {

	req.Session = w.after
	req.MintedAt = w.Now()
	result, err := w.publisher.Publish(ctx, req)
	if err == nil {
		w.advance(result.Position)
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
		return MutationRecord{}, fmt.Errorf("iamdomain: the reason on this %s "+
			"is %d bytes and the cap is %d — it is rendered into an "+
			"authentication trail beside the op that caused it, so it says "+
			"WHICH cause fired rather than narrating", op, len(reason), MaxReason)
	}
	version := RecordVersion
	if op == OpRemove || op == OpEviction {
		// PINNED FOR EVER. A gate-installing record a node could not
		// read would be deferred, and a deferred removal here is
		// somebody off-boarded still signing in.
		version = GateRecordVersion
	}
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V:         version,
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
				if err := decide(tx); err != nil {
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
