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

// The write authority ([statelog.Publisher]): take ONE snapshot of your own
// rows, decide and form the expectation inside it, publish, let the broker
// arbitrate, never guess. This file is the identity estate's decides.
//
// Uniqueness is decided on the directory subject ([Writer.publishDirectory];
// package doc, "ONE SUBJECT FOR THE DIRECTORY"), and a decide that takes a
// value first asks [wholeDirectory] about records this node retained. A seat
// check reads the org chart's log, which nothing orders against this one, so
// it is advisory: a binding to a since-removed seat is a legal state the
// session layer answers with a 403 ([Writer.humanSeat]). The actor is on the
// writer, never the call: an author the caller chooses is no trail, so a
// surface acts as one party from its immutable context ([Writer.As]).

// Writer is one party's authority to change the identity estate.
type Writer struct {
	publisher *statelog.Publisher

	// db is the replicated estate, a READER because only the log's applier
	// writes it. It is never a decide's snapshot (the framework takes that
	// per append), so nothing decided against it carries an expectation.
	db store.ReplicatedReader

	// blinds derives the blind a directory decide compares an address by,
	// and sealer seals the values that belong to somebody. Both optional:
	// ending a session, bumping an epoch, suspending and removing need no
	// key, so access can be revoked while the secret store holding the
	// blind-index key is unreachable. The blinder is resolved per write,
	// never held ([Blinds]).
	blinds Blinds
	sealer *Sealer

	// seats is the organisation every seat binding is checked against; nil
	// refuses every one as unavailable ([Writer.humanSeat]).
	seats SeatLookup

	// Actor and ActorKind are who this writer acts as. ActorKind is the
	// PRINCIPAL's kind (person, machine, engine), which this domain's records
	// carry; the node's own writer is [NodeKind].
	Actor     string
	ActorKind iam.Kind

	// OperatorID is the credential this writer's party acts THROUGH
	// ([iam.Actor.OperatorID]: a machine token's `pat:<id>`, a browser
	// session's `session:<lineage>`), so a token's gesture is told apart from
	// its owner's on every event and record ([MutationRecord.OperatorID]).
	// Empty on the node's own writer. It is the party's: [Writer.As] and
	// [Writer.For] set it from the principal, never carry it from the clone.
	OperatorID string

	// Principal is the id of the principal this writer's party IS, empty for
	// the node's own writer. On the party, never on a call, for the actor's
	// reason: a gesture decided on who makes it ([Writer.mayMintFor]) must not
	// rest on a field the caller fills. [Writer.As] derives it; a caller
	// holding a writer cannot restate it.
	Principal string

	// Grants is what this writer's party is entitled to. None is a real party
	// (a person changing their own password), so the zero value is
	// fail-closed rather than refused.
	Grants []iam.Grant

	// Now is the writer's clock, for the AUTHORED instant only: every instant
	// the applier stores is the broker's, which keeps every node's copy
	// byte-identical.
	Now func() time.Time

	// events announces what a landed record decided ([Events]).
	events Events
}

// Events is where this writer announces what a landed record DECIDED: the
// grants a person write added and removed, and the session generation a
// company-wide invalidation moved to. The caller cannot state those, since they
// are read inside the record's own snapshot; the asking surface announces
// everything else.
//
// Only on a definite outcome ([Writer.announce]): an `unknown` write may not
// have landed, so a live row could claim a change that never happened; its
// `iam_history` row is the trail. Consumer-defined; the
// node's audit trail satisfies it, and nil announces nothing.
type Events interface {
	Emit(ctx context.Context, payload events.Payload)
}

// SeatLookup is the organisation this node runs, as narrowly as a seat binding
// asks: whether it holds a seat at a HANDLE (ADR-0013), and its kind
// ([Writer.humanSeat]). Shaped like [session.Chart], so the engine's one view
// of its org satisfies both. An error is never "no such seat":
// [session.ErrNoCompany] is a node running no company yet, refused as
// [ErrNoCompany], and any other is the unknown arm.
type SeatLookup interface {
	Seat(ctx context.Context, handle string) (session.Seat, bool, error)
}

// NodeKind is the principal kind the node's own writer authors as: the sweeps,
// a re-seal, a re-hash, a company-wide invalidation, and a generation record
// that names nobody. The engine, never a machine: a row authored by a machine
// reads in the trail as that credential's login, here a service account named
// after the node id, which no machine grammar admits
// ([iam.Principal.Validate]).
const NodeKind = iam.KindEngine

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Blinds derives address blinds, and Sealer seals the values that
	// belong to somebody. Both optional: see [Writer.blinds].
	Blinds Blinds
	Sealer *Sealer

	// Seats is the org chart of the configuration epoch this node applied,
	// which every seat binding (create, invitation, redemption, bind) is
	// checked against. Optional, but a writer without it refuses every
	// binding as unavailable, so it creates and invites nobody.
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

// NewWriter builds one party's authority. A writer with no actor is refused:
// an empty author column is a change nobody can be asked about, and
// indistinguishable from a surface that forgot.
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
	// Safe for concurrent use: nothing a call writes outlives it, so the
	// node's own writer serves every surface at once.
	return &Writer{
		publisher: deps.Publisher, db: deps.DB,
		blinds: deps.Blinds, sealer: deps.Sealer, seats: deps.Seats,
		Actor: deps.Actor, ActorKind: deps.ActorKind,
		Grants: deps.Grants, Now: now, events: deps.Events,
	}, nil
}

// As is a writer for the party a principal is, sharing this one's plumbing.
//
// The party is derived whole from the principal: name and credential
// ([iam.ActorFor]), kind, grants and id. Everything is replaced, never carried
// from the writer this clones: a surface derives one writer per request, and
// anything carried would hand the next caller the last one's authority or name.
// Safe from any goroutine: nothing it copies is written after construction.
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

// For is this writer's authority writing on somebody's behalf: its records and
// announcements name them as author ([iam.ActorFor]'s name, kind and
// credential), while what it may do stays this writer's own.
//
// For the gestures a person makes through the node (signing in and out, a
// second factor, a redemption, a reset link, enrolling their own
// authenticator): the node writes the record, since the person holds no
// permission it asks, but the person made the gesture. What the engine decides
// for itself keeps the node as author, and what a redemption CONFERS is
// announced as the inviter's decision ([Writer.Redeem]). Unlike [Writer.As] it
// carries no grants and no id: whom it writes for never changes its authority.
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
// Never for a collapsed call ([statelog.Result.Collapsed]): the operation
// landed as a copy this call cannot prove its own, so the facts its decide read
// may describe a decision never published (a generation of zero, a grant delta
// from nothing). The call that made the copy announced it if it saw a definite
// outcome; otherwise its `iam_history` row is the trail.
func (w *Writer) announce(ctx context.Context, result statelog.Result, err error,
	payload events.Payload) {

	if w.events == nil || err != nil || result.Collapsed ||
		result.Outcome == statelog.OutcomeUnknown {
		return
	}
	w.events.Emit(ctx, payload)
}

// ErrCollapsed reports a gesture whose operation LANDED but whose answer this
// call cannot give: the answer (a bearer's counters, a token's grants) is
// computed inside a decide, and the call was answered with a copy it cannot
// prove its own ([statelog.Result.Collapsed]).
//
// Unavailable, not unknown, since it did land: a caller handed another
// decision's value would build a credential on it, and a bearer at an epoch its
// session was not opened at survives a revocation between the two. The landed
// copy is held by nobody and expires; a fresh operation mints another.
var ErrCollapsed = fmt.Errorf("this change can't be confirmed. Send it again "+
	"with a new key: %w", statelog.ErrUnavailable)

// errNothingToPublish is what a decide returns when its snapshot shows the
// write has nothing left to do, such as a conditional revocation whose epoch
// already moved. [Writer.request] turns it into the framework's empty
// decision, which answers `applied` with no record.
var errNothingToPublish = errors.New("iamdomain: nothing to publish")

// grantDelta is what one write added to a permission set and took away, each
// sorted so every node describes it identically.
//
// Compared through [iam.Expand] on both sides, as every permission comparison
// is: saving `[admin]` as `[admin, worklet, doclet]` announces no change and
// revokes no reset link, and taking admin away removes every app it carried.
// It reports the rows' own names, unknown ones included: the trail records
// what the rows said.
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
// app admin includes. Fail-closed on a name this build cannot read
// ([iam.Principal.Can]).
func (w *Writer) Can(g iam.Grant) bool { return iam.Principal{Grants: w.Grants}.Can(g) }

// ErrRefused reports a write this writer's party may not make, distinct from a
// conflict and an outage so a surface answers 403, 409 and 503 apart. Every
// one is a [Refusal] (TestErrRefusedIsOnlyEverBuiltAsARefusal): compare with
// errors.Is, and read what lifts it with errors.As.
var ErrRefused = errors.New("not allowed")

// Refusal is a record this writer's party may not publish, and whether a
// permission would lift it.
//
// One a permission lifts (an admin-only record, a permission the party cannot
// confer because it lacks it) names that permission, so the caller knows what
// to ask for. One the record's own rule refuses whatever the party holds (a
// person's token minted by anybody but them, a spent invitation, an unknown
// name) names none.
//
// Exported, fields and all, because the estate router's wire (internal/estate)
// rebuilds an error type from its exported fields when the deciding node is
// not the caller's. Only this package builds one ([needs], [forbidden]).
type Refusal struct {
	// Grants are the permissions any ONE of which, held by the party, would
	// have admitted the record (internal/authz's minimal cover). EMPTY where
	// the record's own rule refused it.
	Grants []iam.Grant

	// Err is the refusal's sentence and %w chain, [ErrTokenRefused] among it
	// through a machine token. The sentence a person reads ([Sentence]), so it
	// names what it refuses as they know it, never by an id they did not
	// choose.
	Err error

	// About names what the refusal was about by the ids an operator needs (an
	// invitation's id, a session's lineage, the access token), empty where
	// the sentence says it all. [Refusal.Error] carries it into every log
	// line; [Sentence] keeps it from the person.
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
// administrative record asked through a machine token, or through the
// Dashboard Assistant for [assistRefusedOps].
//
// internal/authz gates a REQUEST's route; this gates the RECORD, which a duty,
// the CLI or a test publishes with no route. A token is refused even holding
// admin: these records decide who may do anything, and a token proves nobody
// is at a keyboard. The assistant's approved edits pass (ADR-0051), its sign-in
// ceremonies do not (ADR-0052). Both are checked first, on the operator id's
// class, never its spelling, and refused naming no permission; a party without
// admin is refused naming admin. The node's own writer holds admin.
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
// needs a person present: an administrative one ([Writer.mayAdminister]) or a
// token's mint ([Writer.mayMintFor]). It is always a [Refusal] naming no
// permission, so a surface that tells the two apart answers that a person must
// be present rather than that a permission is missing.
var ErrTokenRefused = errors.New("access tokens can't change people or access")

// ErrAssistRefused reports an administrative identity record, or a token mint,
// asked through the Dashboard Assistant ([Writer.mayAdminister],
// [Writer.mayMintFor]): a [Refusal] naming no permission, as [ErrTokenRefused]
// is, answered as a gesture the person takes themselves.
var ErrAssistRefused = errors.New("only the person can issue sign-in links, invitations or tokens, or end sessions, never Ask Crewlet")

// publish runs one decide through the framework's write authority. Every path
// here goes through it, so no append can forget the actor and the clock.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (
	statelog.Result, error) {

	// No mint instant beside the id: the id carries its own
	// ([statelog.OpMintedAt]), and one stamped here would be this call's,
	// later than the mint on every retry.
	result, err := w.publisher.Publish(ctx, req)
	var refused *statelog.Unavailable
	if errors.As(err, &refused) && refused.Reason == statelog.ReasonOpReused {
		// The framework's "this id already names another write", in this
		// domain's words: every create derives what it creates from its
		// operation key, and a caller answering a reused key with 409 and a
		// fresh key asks for [ErrOperationReused]. The framework's refusal
		// stays in the chain, with its position.
		err = fmt.Errorf("%w: %w", ErrOperationReused, err)
	}
	return result, err
}

// publishDirectory publishes one record on the DIRECTORY subject; decide forms
// the payload in the record's own snapshot, or refuses.
//
// The framework needs the scope before it takes the snapshot (to probe for a
// deferral and to wait), but a directory record's scope depends on rows
// ([leaversOf], [eraseSealed]). So scopeOf is read outside and again inside,
// and a round whose snapshot names a bucket the scope missed publishes nothing
// and reads again, at most [directoryScopeAttempts] times.
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
// its snapshot overtook. Three: what moves the set between reads milliseconds
// apart (a removal of the seat's earlier holder, an invitation redeemed at the
// address) lands once, so a set still moving after two re-reads is in a loop,
// worth the conflict it answers rather than a spin.
const directoryScopeAttempts = 3

// wholeDirectory refuses a decide that reads the whole directory on a node
// that RETAINS a record it could not apply.
//
// A directory decide calls a value FREE because no row in its snapshot holds
// it, but a retained record (a newer build's, or one signed under a keyring key
// this node lacks) moved the anchor without writing rows, so the broker would
// accept a decide that missed it. [statelog.Snap] probes only the record's own
// buckets, so this asks the whole deferral index and answers `deferred`, a 503
// another node serves; the cost is a retaining node's directory writes during a
// rolling upgrade. A removal skips it: it frees values rather than taking one,
// and the framework's probe covers its own buckets.
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
			// The version is left to the encoder, which stamps the lowest
			// that carries what the record holds ([Encode]), never the
			// ceiling ([RecordVersion]).
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

// request wraps one record in the framework's request shape.
//
// The record is taken by POINTER so a decide can fill it: half the gestures
// here form their payload inside the snapshot, and the framework may rerun a
// decide on a fresh one, so the encode must read the last run's mutation. A
// copy would publish an empty payload that every data node fails to decode.
func (w *Writer) request(ctx context.Context, rec *MutationRecord, opID string,
	pattern statelog.Pattern, decide func(*sql.Tx) error) statelog.Request {

	// The op id goes on the record too: the framework reads the ledger row
	// back by the envelope's id, and refuses a record whose id differs, since
	// no ambiguous publish could be resolved against it.
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
					// The framework's "nothing to write": applied, with
					// no position.
					return statelog.Decision{}, nil
				} else if err != nil {
					return statelog.Decision{}, err
				}
			}
			// The stamp is the framework's ([statelog.Stamp]): the writing
			// node and its snapshot's generation, which every applier's
			// eviction gate reads and the publisher requires. Set every
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

// removedPersonIn refuses, inside a decide's snapshot, a record about a removed
// person whose SUBJECT is not their own: a directory record, a session.
//
// The framework's guard reads the deletion marker on the record's subject
// ([iamGuards]), and only a person's own subject carries one; other records
// name the person in a payload, which [markedPerson] reads only for the body
// it is handed. Without this, a record about somebody removed before the
// snapshot would be appended only for every data node to drop it. One
// primary-key read, only for a record naming a person.
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
