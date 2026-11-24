package chatlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// THE WRITE PATH: one record per gesture, published on the subject it contends
// for: one snapshot, decide and form the expectation inside it, publish, let the
// broker arbitrate ([statelog.Publisher]'s rule). A node behind can only fail to
// write, never corrupt a conversation. Every write answers applied, pending or
// unknown, never a bool.
//
// A room's own state arbitrates (topic, archive, membership, prune, erase:
// decided against the room's last record). Messages are sequenced: post, edit,
// delete and reaction carry NO expectation, since a rejection would lose a typed
// message. What the decide cannot see the applier decides ([Store.decided]).
// A create arbitrates on a named room's address or a direct room's derived id.

// Recipients, mentions and the unit's lead are resolved INSIDE the decide,
// against this node's applied rows and the live roster, never at wake time: the
// winner may not have applied the post yet, and a lead is a fact about the
// EPOCH. An archived room takes no new post or reply ([ErrArchived]); edits,
// take-backs, moderation and reactions stay open, or moderating a closed room
// would be an un-archive. A room of unknown kind takes nothing ([unknownKind]).
//
// Who may make a gesture is the authority table's, decided before here; this
// store judges the DECISION it receives ([MessageAuthority], [ChannelAuthority])
// against its own snapshot, and refuses a write into an unreadable room
// ([reachable]).

// Not written: a RENAME (a channel's name is its create's address) and a FOLLOW
// gesture (BEING NAMED subscribes you; the applier derives `chat_follows` from a
// thread's mentions).

// Store is the company's chat as a write authority.
type Store struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read for the two answers no decision
	// formed: a direct conversation already open ([Store.OpenDirect]), and a
	// gesture another copy of its operation landed ([Store.answer]). Never the
	// write path's own snapshot, and nothing read through it carries an
	// expectation.
	db store.ReplicatedReader

	// roster is the company as it is RIGHT NOW; none of it is derivable from
	// the log, which is why it is a seam.
	roster Roster

	now func() time.Time

	// newOpID mints the operation id for a gesture no key derives one for
	// ([Store.operation]): an actor with no [Actor.OpKey]. A NAMED room's id is
	// its create's operation's ([roomIDOf]): a create that lost its race must
	// not have written a room under an id the winner also chose. A direct
	// conversation's id is derived ([DirectChannelID]), which decides which
	// subject each create arbitrates on.
	newOpID func() string

	// defaultPrivate resolves a create that states no visibility. See
	// [Options.DefaultPrivate].
	defaultPrivate bool
}

// Roster is what the write path asks about the company the rooms belong to.
//
// A consumer-defined seam: the org chart is a large surface, and a write path
// reaching all of it could decide things the log has no record of.
//
// IT MUST NOT BLOCK. Every method runs INSIDE the decide's read transaction,
// where the framework forbids anything that can wait (broker, coordination
// read, model). The roster is the epoch's chart, held in memory.
type Roster interface {
	// Seat reports whether the company still has this seat, and whether it is
	// a PERSON: a handle the company no longer has is a wake nothing can run,
	// and a person is ADDRESSABLE but never woken ([Route]).
	Seat(handle string) (exists, human bool)

	// Lead names the seat that answers for a unit's ROOM (the room named
	// room, recorded as unit's), or empty where nobody does: the
	// [ReasonAnswerer] of a unit's room.
	//
	// THE ROOM AS WELL AS ITS UNIT, because whose a unit room is belongs to the
	// CHART, not the row: a unit that stops declaring its room, or leaves the
	// chart, leaves a room staffed with nobody, and its lead must not go on
	// being woken from it. The implementation answers by the same rule that
	// decides who is in the room.
	Lead(unit, room string) string

	// Senior names the AGENT seat among handles that stands highest in the org
	// chart: the fewest managers above it, ties broken by the chart's order.
	// Empty where none is an agent seat. It is the [ReasonAnswerer] of every
	// named room that is not a unit's, asked over its members. It lives behind
	// this seam because a second copy of "who manages whom" would be a second
	// answer.
	Senior(handles []string) string

	// Seats is every seat handle the company has, people and agents: what an
	// auto-join room accounts for when the setting is turned on
	// ([ChannelPatch.AutoJoinSeats]).
	Seats() []string
}

// Options configure a store.
type Options struct {
	// Publisher is the domain's write authority, and Roster the company
	// a message's wakes are resolved against.
	//
	// DB is the replicated estate an answer is READ BACK from when no
	// decision formed it ([Store]'s field); a decision never reads it.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader
	Roster    Roster

	// Now is the clock the AUTHORED instants are stamped from; nil takes the
	// wall clock in UTC. An argument so a test can pin it and nothing here
	// reads a clock the applier is forbidden. Every instant that reaches a ROW
	// is the broker's; this one reaches the envelope only.
	Now func() time.Time

	// DefaultPrivate is what a create that states NO visibility resolves
	// to: true makes such a room private, false public. A plain bool so
	// this package needs no config dependency; its zero value is the
	// documented default (public).
	DefaultPrivate bool
}

// NewStore builds the company's chat over its own log.
//
// THREE SEAMS ARE REQUIRED AND EACH IS REFUSED BY NAME, since a missing one fails
// silently: no publisher writes nothing; no store answers a retried post or
// open conversation with nothing read back; no roster wakes whoever the record
// named, people and departed seats included.
//
// THE NODE GATE IS NOT BUILT HERE: an eviction wakes nobody and reads no
// roster, so its writer is [NewGateWriter].
func NewStore(opts Options) (*Store, error) {
	if opts.Publisher == nil {
		return nil, errors.New("chatlet: a write authority is required — every " +
			"write here is a record on the chat log, and a store with no " +
			"publisher could validate a message and then write it nowhere")
	}
	if opts.DB.IsZero() {
		return nil, errors.New("chatlet: a store is required: an answer no " +
			"decision formed — a direct conversation already open, a gesture " +
			"another copy of its operation landed — is read back from this " +
			"node's own applied rows")
	}
	if opts.Roster == nil {
		return nil, errors.New("chatlet: a roster is required: who a message " +
			"wakes is resolved at write time, and without one every wake " +
			"would reach handles the company no longer has and people a turn " +
			"must never answer for")
	}
	s := &Store{
		publisher: opts.Publisher, db: opts.DB, roster: opts.Roster,
		now: opts.Now, newOpID: freshOpID,
		defaultPrivate: opts.DefaultPrivate,
	}
	if s.now == nil {
		s.now = nowUTC
	}
	return s, nil
}

// nowUTC is the wall clock a store takes when a caller pins none.
func nowUTC() time.Time { return time.Now().UTC() }

// The sentinels, separate because each asks something different of the caller:
// negotiate a name, open a conversation, reopen a room, take a reaction off,
// ask for a permission, or redo a read.
var (
	// ErrNotFound reports a room or a message that is not there.
	ErrNotFound = errors.New("chatlet: no such record")

	// ErrNameTaken reports a channel name the company already holds.
	ErrNameTaken = errors.New("chatlet: another room already has that name")

	// ErrRoomExists reports a room whose own id is already claimed: a DIRECT
	// conversation's create only, since a named room's create contends for its
	// address.
	ErrRoomExists = errors.New("chatlet: that conversation is already open")

	// ErrConflict reports a write that lost its race too many times.
	ErrConflict = errors.New("chatlet: the room kept changing under this write")

	// ErrMembersChanged reports a membership stated against a read the room
	// has since moved past ([Store.SetMembers]' expected members): a STALE
	// READ, cured by reading the members again, not a lost race
	// ([ErrConflict]).
	ErrMembersChanged = errors.New("chatlet: the room's members changed after they were read")

	// ErrArchived reports a room that takes no new post or reply; edits,
	// deletes and reactions stay open (see the head of this file).
	ErrArchived = errors.New("chatlet: this room is archived")

	// ErrReactionsFull reports a reaction that would put a
	// [MaxReactionEmoji] + 1st distinct emoji on a message. Decided by the
	// applier at the record's position ([VerdictReactionsFull]), because a
	// toggle cannot see the set it joins. THE MESSAGE'S STATE, not the
	// argument: the same emoji lands once somebody takes one off.
	ErrReactionsFull = errors.New("chatlet: this message has all the emoji it can")

	// ErrForbidden reports a gesture this actor may not make.
	ErrForbidden = errors.New("chatlet: that is not this actor's to do")

	// ErrUnknownKind reports a room whose kind this build does not know —
	// a newer node in the fleet created it — so this node cannot say who
	// may reach it and serves nothing in it.
	//
	// NOT [ErrForbidden]: that is a fact about the ACTOR (remedy: a
	// permission); this is about the NODE (remedy: its upgrade, after which the
	// same call lands).
	ErrUnknownKind = errors.New("chatlet: a room of a kind this build does not know")

	// ErrNoActivation refuses a unit room's create or membership that does
	// not name the configuration activation it came from. See
	// [Store.SetUnitMembers]: there is no honest default, and a caller
	// holding no activation has no chart to apply.
	ErrNoActivation = errors.New("chatlet: a unit room's chart must name the " +
		"instant its configuration was activated")
)

// archivedRoom is the sentence a post into an archived room is refused with:
// what refuses it, and [reopenRoom].
const archivedRoom = "this room is archived, so it takes no new messages. " + reopenRoom

// archivers is who archives a room and opens it again: the archive's own
// relation (authz's chat.channel.archive — the room's creator, the lead of the
// unit whose room it is or of a unit above it, or an administrator), named
// because a room has no owner of its own a person could be sent to.
//
// ONE PHRASE FOR BOTH REFUSALS that say it (an archive refused, a post into an
// archived room), so they name the same people alike.
const archivers = "whoever created it, the lead of its team or of a team " +
	"above it, or an admin"

// reopenRoom is who changes an archived room, said at the write and at the
// verdict alike ([archivers]).
const reopenRoom = "Only " + archivers + " can reopen it"

// Actor is who is making a write.
//
// IT TRAVELS PER CALL, since one node's chat serves every seat and person
// through one write path.
type Actor struct {
	// Handle is the seat this write acts as (an agent's, or a person's human
	// seat, ADR-0032) and the name it is recorded under.
	//
	// A ROOM IS SPOKEN IN AS A SEAT only: a credential acting as itself
	// ([AuthorOperator]) writes nothing said in a room ([Actor.speaks]). The
	// destructions and retention setting are the exception ([Store.Erase],
	// [Store.Prune], [Store.SetRetention]): none is in a room's voice, so an
	// admin credential makes them under its own name.
	Handle string

	Kind       AuthorKind
	OperatorID string

	// TurnID is the RUN that made this write: provenance for audit (ADR-0017).
	// It derives nothing; a re-run turn posts the same message by the gesture's
	// [Actor.OpKey], and an id seeded from the run would be a second copy on
	// every redelivery.
	TurnID string

	Chain []string

	// WrittenFor is whom this actor's writes are made for (ADR-0109): for an
	// [AuthorAgent], whoever its turn works for, carried on every record it
	// writes that routes to somebody ([MutationRecord.WorkingFor]), never the
	// writing seat. AN AGENT THAT STATES NOTHING WRITES NOTHING ([decide]);
	// every other kind carries nobody ([types.WrittenFor]). A read ignores it.
	//
	// internal/estate refuses such a write before it is asked anywhere.
	// `omitzero` keeps an actor carrying nobody byte for byte as a build
	// without the field sends it ([Actor.UnmarshalJSONFrom]).
	WrittenFor types.WrittenFor `json:",omitzero"`

	// OpKey is the caller's operation key: the value a RETRY of the same
	// gesture reproduces, from which every gesture derives its operation id
	// ([Store.operation]). EMPTY MINTS A FRESH ID PER CALL, wrong for a caller
	// that must retry: a post answered `unknown` is safe to retry only under
	// the SAME id, since the operation ledger recognises a second arrival.
	//
	// An engine-minted operation id, held to [statelog.CheckCallerOpID] by
	// every surface and by [Actor.validate] ([statelog.KeyedOpID]).
	OpKey string
}

// UnmarshalJSONFrom decodes an actor that crossed to a data node in a chat
// write's arguments (internal/estate): whom its writes are for is what the
// building node stated ([types.WrittenFor.Received]), WHATEVER ARRIVED; an
// absent carrier is nobody.
//
// Through the caller's own decoder, so on the estate's wire an undeclared
// member refuses the request at its path; an UnmarshalJSON would re-decode
// under encoding/json's defaults, which drop it silently.
func (a *Actor) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire Actor
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(a)); err != nil {
		// Unwrapped, against the %w rule: json/v2 wraps any error but a
		// *jsonv2.SemanticError in a second one, and the estate's wire names
		// the refused member from the first.
		return err
	}
	a.WrittenFor = a.WrittenFor.Received()
	return nil
}

// recordedFor is whom a record was written for ([MutationRecord.WorkingFor]),
// or why this actor may write none: an agent's carrier on a record routing to
// somebody ([Notify.Recipients]), nobody on any other; a refusal for an agent
// stating nobody or another kind carrying somebody ([types.WrittenFor.For]).
//
// Decided by the recipients, never by silence: a silent answer to an asker
// still wakes them; a seat held back ([Notify.Held]) does not count. Asked of
// every record in [decide].
func (a Actor) recordedFor(notify *Notify) (*types.WorkingFor, error) {
	carrier, err := a.WrittenFor.For(iam.ActorKind(a.Kind))
	if err != nil {
		return nil, fmt.Errorf("chatlet: %s's write: %w", a.Name(), err)
	}
	if notify == nil || len(notify.Recipients) == 0 {
		return nil, nil
	}
	return carrier, nil
}

// Name is how this actor is recorded and rendered.
//
// THE HANDLE, ALWAYS: no "operator:<token>" spelling, or "who said this" would
// have two vocabularies.
func (a Actor) Name() string { return strings.TrimSpace(a.Handle) }

// IsHuman reports an actor the routing treats as a person: a human seat, which
// is the kind every person writes as.
func (a Actor) IsHuman() bool { return a.Kind == AuthorHuman }

// wrote reports whether this actor is m's author: its name, said as the engine
// ([AuthorSystem]) where m is the engine's line and as a seat where m is a
// seat's. A NAME ALONE IS NOT AN AUTHOR: the engine's line is stored under a
// name of its own (internal/iam's EngineNames), and a seat of that name would
// otherwise rewrite the engine's words, or the engine a seat's.
func (a Actor) wrote(m Message) bool {
	return m.Author == a.Name() && (m.AuthorKind == AuthorSystem) == (a.Kind == AuthorSystem)
}

// validate refuses an actor that cannot be recorded at all.
func (a Actor) validate() error {
	if !a.Kind.Valid() {
		return invalid("actor.kind", "%q is not one of %s", a.Kind, words.Or(AuthorKinds()))
	}
	// A KEY IS AN OPERATION ID THE ENGINE MINTED, or no key ([Actor.OpKey]).
	// A key outside the grammar carries no instant, read as minted at the
	// epoch, so once the ledger had swept anything each such gesture would
	// be answered `unknown` unpublished, for ever. Refused at the one
	// boundary every write passes.
	if key := strings.TrimSpace(a.OpKey); key != "" {
		if err := statelog.CheckCallerOpID(key); err != nil {
			return invalid("actor.op_key", "%v", err)
		}
	}
	if a.Name() == "" {
		return invalid("actor.handle", "a %s write names nobody — a seat, a "+
			"person's seat, or for a credential acting as itself its own "+
			"login — and a write nobody made is not an audit trail", a.Kind)
	}
	return nil
}

// speaks refuses an actor that would say something IN a room without being a
// seat: a credential acting as itself ([AuthorOperator]).
//
// A credential's login is a speaker nobody can reply to, mention or find in the
// membership; a pipeline that should post is given a human seat with its
// credential bound to it. Asked after [Actor.validate] by every gesture said in
// a room; the authority table refuses such a principal first, and this is the
// backstop.
func (a Actor) speaks() error {
	if a.Kind == AuthorOperator {
		return fmt.Errorf("%w: %s is a credential acting as itself, and a room "+
			"is spoken in as a SEAT — sign in as the person whose seat this is, "+
			"or bind the credential to a human seat", ErrForbidden, a.Name())
	}
	return nil
}

// seatedOrSystem is [Actor.validate] and [Actor.speaks] together, which every
// gesture said in a room passes before it reads anything.
func (a Actor) seatedOrSystem() error {
	if err := a.validate(); err != nil {
		return err
	}
	return a.speaks()
}

// Written is what a write reports back.
//
// An unvouched answer ([statelog.Result.Unvouched]: `unknown` because this
// node's ledger cannot vouch for the operation) carries the operation and
// nothing else: every field a decision fills is ZERO, as none describes a record
// that exists. [Written.ChangeID] names the operation to finish on a node whose
// ledger can answer it. [answered] is the one place every answer passes through.
type Written struct {
	// Channel is the room this write was about, as the decision read it (for
	// a create, as the record wrote it).
	Channel Channel

	// Message is what a message gesture authored; zero for a room gesture.
	// Its CreatedAt is ZERO on a message this call decided: the instant is the
	// BROKER'S (what keeps every node's copy byte-identical) and does not exist
	// yet. A caller needing it reads the message back; a retry answered as
	// another copy IS read back ([Store.answer]).
	Message Message

	// Revision is the log revision the room's row is AT after this write, in
	// the one number space every reader answers in ([ChannelRevision]): a
	// landed record's own position, which the applier stamps into the row, or
	// the row the snapshot read when nothing was published.
	//
	// NEVER A LITERAL ZERO on a write that succeeded: zero is both "this node
	// has applied nothing here" and "nobody answered", which is why an
	// unvouched answer carries exactly that.
	Revision uint64

	// ChangeID is the operation id, and for a message the message's own id
	// (see the head of ids.go).
	ChangeID string

	// Outcome is the framework's three-valued answer, unchanged, except that
	// a message gesture's `applied` becomes `pending` where this node's
	// applier consumed the record without deciding it (it holds it retained).
	Outcome statelog.Result

	// Verdict is what this node's applier decided about a message gesture's
	// record ([Verdict]), read back once applied. EMPTY on a room gesture, and
	// on a message gesture this node has not applied (`pending`, `unknown`).
	Verdict Verdict
}

// answered is a write's answer settled against its outcome: everything the
// decision filled, except where this node's ledger cannot vouch for the
// operation, then the operation id and outcome alone ([Written]). One function
// because the rule is a property of the outcome: every decision fills the
// answer before the framework says whether it may stand.
func answered(w Written) Written {
	if w.Outcome.Unvouched {
		return Written{ChangeID: w.ChangeID, Outcome: w.Outcome}
	}
	return w
}

// ChannelRevision is the SQL expression a room's own log revision is read with,
// written ONCE so no reader disagrees.
//
// MAX(version, scoped_through), per the schema: a record writing this row FROM
// ANOTHER SUBJECT stamps `scoped_through`, never `version`, so the row's broker
// expectation keeps matching its subject. Every message is such a record, so
// `version` alone would not move when somebody spoke and a caller waiting to
// see its own message would wait for ever.
const ChannelRevision = `MAX(version, scoped_through)`

// ---- the two channel-state gestures that make a room ------------------- //

// NewChannel is a room to create.
type NewChannel struct {
	// Name is the address the create arbitrates on, normalised here and
	// checked against [ValidName].
	Name string

	// Kind is one of the NAMED kinds; a direct conversation is opened with
	// [Store.OpenDirect], which arbitrates differently.
	Kind Kind

	Topic   string
	Purpose string

	// Unit is the unit whose room this is, required for [KindUnit] and
	// refused for every other kind.
	Unit string

	// Members is the founding membership beyond the author, who is always in
	// the room unless the author is the engine ([AuthorSystem]).
	Members []Member

	// THERE IS NO RETENTION HERE: a shorter horizon is a prune on a delay, so
	// it is an admin's own gesture ([Store.SetRetention]), not something any
	// room's creator could schedule.

	// ActivatedAt is when the configuration a [KindUnit] room comes from was
	// activated, stamped on the room ([Channel.ChartEpoch]): required for that
	// kind ([ErrNoActivation]), refused for every other.
	ActivatedAt time.Time
}

// CreateChannel makes a named room.
//
// ONE RECORD, ARBITRATED ON THE ADDRESS (two fresh uuids would never contend);
// its apply writes the claim, room and founding membership in one transaction,
// so no name is held by a room never written.
func (s *Store) CreateChannel(ctx context.Context, actor Actor, in NewChannel) (
	Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	name := NormalizeName(in.Name)
	if !ValidName(name) {
		return Written{}, invalid("name", "%q is not a channel name — it is the "+
			"address the create arbitrates on, so it is lower-case "+
			"alphanumerics and hyphens, starting with an alphanumeric, at most "+
			"%d bytes", in.Name, MaxChannelName)
	}
	// An unstated visibility is resolved here only, so
	// `chat.chatlet.default_channel_private` means the same on every
	// surface, and BEFORE the gate below, for which "" is no answer.
	kind := in.Kind
	if kind == "" {
		kind = KindPublic
		if s.defaultPrivate {
			kind = KindPrivate
		}
	}
	if !kind.Named() {
		return Written{}, invalid("kind", "%q is not a kind a named room can "+
			"have — a direct conversation is opened with OpenDirect, whose "+
			"create arbitrates on the participants' own derived id rather than "+
			"on an address", in.Kind)
	}
	// A UNIT'S ROOM IS MADE BY THE ORG CHART ONLY: its membership has one
	// writer ([Store.SetUnitMembers]). One anybody else made would have a
	// founding membership frozen for good, which Join, Leave and SetMembers
	// all refuse to change.
	if kind == KindUnit && actor.Kind != AuthorSystem {
		return Written{}, fmt.Errorf("%w: a %s room is the org chart's — the "+
			"engine opens one for a unit that declares a `channel` and keeps "+
			"its membership from the chart, and one made any other way is a "+
			"room whose membership nobody could ever change. Declare the "+
			"channel on the unit in the company document, or make a %s or %s "+
			"room", ErrForbidden, KindUnit, KindPublic, KindPrivate)
	}

	// ONLY THE CHART'S ROOM IS STAMPED, with its activation ([Store.SetUnitMembers]).
	var epoch int64
	switch {
	case kind == KindUnit && in.ActivatedAt.IsZero():
		return Written{}, fmt.Errorf("%w (room %s)", ErrNoActivation, name)
	case kind == KindUnit:
		epoch = configplane.ActivationStamp(in.ActivatedAt)
	case !in.ActivatedAt.IsZero():
		return Written{}, invalid("activated_at", "a %s room names the "+
			"activation of a configuration — only a %s room is the org "+
			"chart's", kind, KindUnit)
	}

	at := s.now()
	// OVER THE ADDRESS AND WHAT THE CREATE ASKS FOR (the kind as stated): the
	// same key asking another room under that address is another operation,
	// which the address then refuses as taken.
	opID, opErr := s.operation(actor, "create", name, createAsk{
		Kind: in.Kind, Topic: in.Topic, Purpose: in.Purpose,
		Unit: strings.TrimSpace(in.Unit), Members: sortedMembers(in.Members),
		ActivatedAt: in.ActivatedAt,
	})
	if opErr != nil {
		return Written{}, opErr
	}
	room := Channel{
		V: DocumentVersion, ID: roomIDOf(opID), Kind: kind, Name: name,
		Topic: in.Topic, Purpose: in.Purpose, Unit: strings.TrimSpace(in.Unit),
		CreatedAt: at, CreatedBy: actor.Name(), CreatedByKind: actor.Kind,
		ChartEpoch: epoch,
	}
	// THE AUTHOR IS A MEMBER, unless it is the ENGINE: [AuthorSystem]'s name
	// is no seat, and as a member nobody could address it.
	members := sortedMembers(slices.Clone(in.Members))
	if actor.Kind != AuthorSystem {
		members = withMember(in.Members, Member{Handle: actor.Name()})
	}
	subject := ChannelNameSubject(name)
	scope := ScopeSet{Terms: []ScopeTerm{
		{Kind: TermName, ID: ChannelToken(name)},
		{Kind: TermChannel, ID: room.ID},
	}}
	payload := ChannelCreate{
		V: DocumentVersion, ChannelID: room.ID, Kind: kind, Name: name,
		Unit: room.Unit, Topic: in.Topic, Purpose: in.Purpose,
		Members: members, CreatedBy: actor.Name(), CreatedByKind: actor.Kind,
		ChartEpoch: epoch,
	}

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternCreate,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// The room cap is a fact about the estate that no record can
			// count, so it is refused only here, inside a snapshot.
			held, err := countChannelsTx(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held >= MaxChannels {
				return statelog.Decision{}, invalid("name", "this company "+
					"holds %d live channels and the maximum is %d — past it "+
					"the sidebar is a search problem rather than a list. "+
					"Archive a room and this one can be created: an archived "+
					"room is still read and still searched, it just takes no "+
					"new messages", held, MaxChannels)
			}
			return decide(stamp, actor, subject, OpCreate, scope, opID, payload, nil, at)
		},
	})
	if errors.Is(err, ErrNameTaken) {
		// ANOTHER NAME LEADS, since it always works: names are one company-wide
		// namespace, so the holder may be a private room the creator cannot open.
		return Written{}, fmt.Errorf("%w: #%s is already another room's name, "+
			"so pick another one (or open #%s if you're in it)", ErrNameTaken,
			name, name)
	}
	if err != nil {
		return Written{}, err
	}
	// No row existed to read, so no prior revision: a create either takes the
	// address or is refused as taken.
	return s.answer(ctx, room.ID, "", Written{
		Channel: room, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	})
}

// OpenDirect opens the conversation between these participants, and reports
// whether it had to be created.
//
// The second value is whether anything was written (a seat opens the
// conversation on every send, so it is usually there already); false beside an
// unvouched answer too, which the `unknown` beside it tells apart. The refusal
// IS the answer: the id is derived from the participants ([DirectChannelID]),
// so two sides publish the same create and one wins; the loser is told it
// exists and this reads the room back. The key ([Actor.OpKey]) derives the
// operation, never the room, so a repeated call is one create, not a refusal.
func (s *Store) OpenDirect(ctx context.Context, actor Actor, participants []string) (
	Written, bool, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, false, err
	}
	handles := cleanHandles(append(slices.Clone(participants), actor.Name()))
	if len(handles) < 2 {
		return Written{}, false, invalid("participants", "a direct "+
			"conversation names %d participant(s) — its id IS the participant "+
			"set, so a set of one derives a room nobody else can reach",
			len(handles))
	}
	if len(handles) > MaxDMParticipants {
		return Written{}, false, invalid("participants", "a direct "+
			"conversation names %d participants and the maximum is %d — past "+
			"that a conversation is a room, and a room has a name somebody can "+
			"join by", len(handles), MaxDMParticipants)
	}
	kind := KindDM
	if len(handles) > 2 {
		kind = KindGroup
	}

	at := s.now()
	id := DirectChannelID(handles).String()
	// Nothing but the room: its id IS the participant set.
	opID, opErr := s.operation(actor, "open", id, nil)
	if opErr != nil {
		return Written{}, false, opErr
	}
	members := make([]Member, 0, len(handles))
	for _, h := range handles {
		members = append(members, Member{Handle: h})
	}
	room := Channel{
		V: DocumentVersion, ID: id, Kind: kind, CreatedAt: at,
		CreatedBy: actor.Name(), CreatedByKind: actor.Kind,
	}
	subject := ChannelSubject(id)
	scope := ScopeSet{Subject: true}
	payload := ChannelCreate{
		V: DocumentVersion, ChannelID: id, Kind: kind, Members: members,
		CreatedBy: actor.Name(), CreatedByKind: actor.Kind,
	}

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternCreate,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// NOTHING IS READ: the whole state is the participants the id
			// was derived from, and the room's own row is the guard the
			// framework checks in this snapshot.
			return decide(stamp, actor, subject, OpCreate, scope, opID, payload, nil, at)
		},
	})
	switch {
	case errors.Is(err, ErrRoomExists):
		held, revision, readErr := s.room(ctx, id)
		if readErr != nil {
			return Written{}, false, readErr
		}
		// THE OUTCOME IS SPELLED OUT here, the one place that does, since
		// the framework's no-op arm cannot reach this case: the guard
		// refuses a create BEFORE a decision's emptiness is consulted. It
		// is APPLIED (the node has the row in hand) with that row's
		// version, as no record landed; a zero outcome is none of the
		// three and would not tell an open conversation from an
		// unestablished write.
		outcome := result
		outcome.Outcome = statelog.OutcomeApplied
		outcome.OpID = opID
		outcome.Version = int64(revision)
		return Written{
			Channel: held, Revision: revision, ChangeID: opID, Outcome: outcome,
		}, false, nil
	case err != nil:
		return Written{}, false, err
	}
	// A COLLAPSED ANSWER STILL REPORTS THE CREATE: an earlier copy of THIS
	// operation wrote the room, which [Store.answer] reads back.
	written, err := s.answer(ctx, id, "", Written{
		Channel: room, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	})
	return written, !result.Unvouched, err
}

// ---- the room's own state ---------------------------------------------- //

// PatchChannel changes a room's settings.
//
// It takes the record's own payload so no second struct drifts from
// [ChannelPatch]; the version is set here. A patch changing nothing publishes
// nothing and is reported applied ([ChannelPatch.Validate] refuses an empty one).
//
// An archive is not any member's: the caller decides it against the room it
// read and passes [ChannelAuthority]. A patch moving the archive without
// admission is refused, and one admitted on a unit the room is no longer under
// is a conflict.
func (s *Store) PatchChannel(ctx context.Context, actor Actor, channelID string,
	patch ChannelPatch, authority ChannelAuthority) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	patch.V = DocumentVersion
	if err := patch.Validate(); err != nil {
		return Written{}, err
	}

	at := s.now()
	// The authority is who asks, not what, so it is not part of the operation.
	opID, opErr := s.operation(actor, "patch", channelID, patch)
	if opErr != nil {
		return Written{}, opErr
	}
	subject := ChannelSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var read uint64

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, revision, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := s.reachableTx(ctx, tx, room, actor); err != nil {
				return statelog.Decision{}, err
			}
			// BEFORE THE NO-OP: it answers success, which this refusal
			// withholds on grounds the room no longer stands on.
			if patch.Archived != nil {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := authority.archives(room); err != nil {
					return statelog.Decision{}, err
				}
			}
			// The company as it stands NOW, from this round's snapshot, never
			// an earlier one.
			patch.AutoJoinSeats = nil
			if patch.AutoJoin != nil {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := authority.autoJoins(room); err != nil {
					return statelog.Decision{}, err
				}
				if *patch.AutoJoin && !room.AutoJoin {
					patch.AutoJoinSeats = cleanHandles(s.roster.Seats())
					slices.Sort(patch.AutoJoinSeats)
				}
			}
			// The room is the answer either way, taken BEFORE the no-op
			// return: an idempotent patch must not report success on a zero
			// room.
			out, read = room, revision
			applyPatch(&out, patch, at)
			if sameRoom(room, out) {
				return statelog.Decision{}, nil
			}
			return decide(stamp, actor, subject, OpPatch, scope, opID, patch, nil, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.answer(ctx, channelID, "", Written{
		Channel: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	})
}

// ChannelAuthority is what a caller was admitted to do to a room's settings
// beyond what any member may.
//
// A value stating the DECISION: the caller asks the authority table's archive
// relation (`chat.channel.archive`: the room's creator, the lead of its unit or
// a unit above, or an admin) and passes the result, as this package holds no
// chart or grants.
type ChannelAuthority struct {
	// Archive reports a caller admitted to archive or reopen this room. THE
	// ZERO VALUE ADMITS NEITHER.
	Archive bool

	// ArchiveUnit is the unit the room was recorded under when its archive was
	// decided (the one the lead relation was asked about), empty for a room no
	// unit holds.
	//
	// A PRECONDITION, NOT A LABEL: the room was read outside this snapshot, and
	// the chart can re-home it between ([MemberSet.Unit]), so a lead of the unit
	// it LEFT would archive it under the unit that now holds it. Checked in the
	// decide's snapshot and refused as a conflict: the caller re-reads,
	// re-decides and re-sends.
	ArchiveUnit string

	// AutoJoin reports a caller admitted to turn the room's auto-join on or off
	// ([Channel.AutoJoin]): an administrator (`chat.channel.autojoin`). The zero
	// value admits nobody.
	AutoJoin bool
}

// autoJoins refuses an auto-join setting this authority does not admit, or
// on a room that cannot take one: only a public room does (a unit's room is the
// chart's, a direct conversation its people, a private room's membership who
// may read it).
func (a ChannelAuthority) autoJoins(room Channel) error {
	if !a.AutoJoin {
		return fmt.Errorf("%w: adding everybody who joins the company to %s "+
			"is an administrator's setting", ErrForbidden, room.called())
	}
	if room.Kind != KindPublic {
		return invalid("auto_join", "%s is a %s room, and only a public room adds "+
			"everybody who joins the company", room.called(), room.Kind)
	}
	return nil
}

// archives refuses an archive this authority does not admit on room as the
// decide's own snapshot holds it.
//
// THE ROOM'S NEW UNIT IS NOT NAMED: a unit's key is minted, never typed by the
// reader, and "another team" is all their next move needs.
func (a ChannelAuthority) archives(room Channel) error {
	if !a.Archive {
		return fmt.Errorf("%w: archiving %s, or reopening it, changes it for "+
			"everyone in it. Only %s can do that", ErrForbidden, room.called(),
			archivers)
	}
	if room.Unit != a.ArchiveUnit {
		return fmt.Errorf("%w: %s moved to another team after this was decided. "+
			"Read it again, then try again", ErrConflict, room.called())
	}
	return nil
}

// SetMembers replaces a room's membership, WHOLE.
//
// Whole: a delta cannot rebuild a row on replay and a node missing a removal
// would serve a private room to somebody taken out. A set built from a stale
// read may re-grant a member removed meanwhile, so expected is that read: when
// not nil and the room, AS THIS SNAPSHOT HOLDS IT, holds anything else, the
// write is [ErrMembersChanged] ([ChannelRevision] cannot stand in: it moves
// with every message). A set already held is the no-op. nil states no
// expectation; a pointer to the empty list expects an empty room.
func (s *Store) SetMembers(ctx context.Context, actor Actor, channelID string,
	members []Member, expected *[]Member) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	opID, opErr := s.operation(actor, "members", channelID, sortedMembers(members))
	if opErr != nil {
		return Written{}, opErr
	}
	return s.members(ctx, actor, channelID, opID, func(room Channel, held []Member) ([]Member, error) {
		// A UNIT'S ROOM IS THE CHART'S, and this gesture could overwrite it
		// wholesale. Refused, not filtered: a named set states the WHOLE
		// membership, with no half to honour. The engine's reconcile calls
		// [Store.SetUnitMembers] instead.
		if room.Kind == KindUnit {
			return nil, fmt.Errorf("%w: %s is a unit's own room, and the "+
				"org chart decides who's in it. A change here would be undone "+
				"the next time the company's configuration is applied. Change "+
				"the unit in the company document instead",
				ErrForbidden, room.called())
		}
		if expected != nil && !slices.Equal(sortedMembers(members), held) &&
			!slices.Equal(sortedMembers(*expected), held) {
			return nil, fmt.Errorf("%w: the members of %s changed after they "+
				"were read", ErrMembersChanged, room.called())
		}
		return members, nil
	})
}

// Join puts the actor in a room.
//
// A seat already in the room is told so, whatever its kind ([Store.setMembership]'s
// no-op); checked after the kinds below, a member re-joining their private
// room would be refused. Three kinds refuse everybody else: a private room's
// membership is the only way in ([reachable]); a direct conversation's
// membership IS its identity; a unit's room is the ORG CHART'S, as [Store.Leave].
func (s *Store) Join(ctx context.Context, actor Actor, channelID string) (Written, error) {
	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	opID, opErr := s.operation(actor, "join", channelID, nil)
	if opErr != nil {
		return Written{}, opErr
	}
	return s.members(ctx, actor, channelID, opID, func(room Channel, held []Member) ([]Member, error) {
		if slices.ContainsFunc(held, func(m Member) bool { return m.Handle == actor.Name() }) {
			return held, nil
		}
		if room.Kind == KindUnit {
			return nil, fmt.Errorf("%w: %s is a unit's own room, and the "+
				"org chart decides who's in it. Joining it would be undone the "+
				"next time the company's configuration is applied. Move the "+
				"seat into the unit instead", ErrForbidden, room.called())
		}
		return withMember(held, Member{Handle: actor.Name()}), nil
	})
}

// Leave takes the actor out of a room.
//
// A UNIT'S ROOM REFUSES IT: that membership is the chart's, so the next apply
// would undo a leave, and a silent revert is worse than a refusal.
func (s *Store) Leave(ctx context.Context, actor Actor, channelID string) (Written, error) {
	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	opID, opErr := s.operation(actor, "leave", channelID, nil)
	if opErr != nil {
		return Written{}, opErr
	}
	return s.members(ctx, actor, channelID, opID, func(room Channel, held []Member) ([]Member, error) {
		if room.Kind == KindUnit {
			return nil, fmt.Errorf("%w: %s is a unit's own room, and the "+
				"org chart decides who's in it. Leaving it would be undone the "+
				"next time the company's configuration is applied",
				ErrForbidden, room.called())
		}
		return withoutMember(held, actor.Name()), nil
	})
}

// SetUnitMembers staffs a unit room for unit, with members, from the org chart
// activated at activatedAt. The second value is what was written
// ([MembershipWrite]): this runs on every boot and apply, so the room usually
// holds the set already. The one writer [Store.SetMembers] and [Store.Join]
// refuse for: [AuthorSystem]'s alone, taking the WHOLE set (a diff would decide
// from rows read elsewhere), under a fresh operation per call. A room under
// another unit key is RE-HOMED by the same record ([MemberSet.Unit]); only
// [KindUnit] rooms are taken. Stamped with the activation ([Channel.ChartEpoch]),
// never walked back: an OLDER one decides nothing; the SAME one only a
// different set or unit; a LATER one is a re-stamp. A zero instant is
// [ErrNoActivation], an empty unit [ErrInvalid].
func (s *Store) SetUnitMembers(ctx context.Context, actor Actor, channelID, unit string,
	members []Member, activatedAt time.Time) (Written, MembershipWrite, error) {

	unit = strings.TrimSpace(unit)
	if unit == "" {
		return Written{}, MembershipUnchanged, invalid("unit", "the org chart "+
			"staffs room %s for no unit — a unit room is staffed for the unit "+
			"that declares it, and one staffed for nobody is ReleaseUnitRoom's",
			channelID)
	}
	return s.chartMembership(ctx, actor, channelID, activatedAt, unit, members)
}

// ReleaseUnitRoom staffs a unit room with nobody, from the org chart activated
// at activatedAt: the room no unit in that chart declares any more.
//
// The room's unit stays where it is, which is why this is not
// [Store.SetUnitMembers] with an empty set: a named unit would be READ in
// another transaction, and a room re-homed meanwhile would be written back
// under the old key. The record moves no unit ([MemberSet.Unit] empty). Every
// other rule is [Store.SetUnitMembers'].
func (s *Store) ReleaseUnitRoom(ctx context.Context, actor Actor, channelID string,
	activatedAt time.Time) (Written, MembershipWrite, error) {

	return s.chartMembership(ctx, actor, channelID, activatedAt, "", nil)
}

// chartMembership is the org chart's membership write: [Store.setMembership]
// stamped with the activation, refused to anybody but the engine and to any
// room but a unit's. unit is the key the room is re-homed to, or empty for a
// write that moves no unit ([Store.ReleaseUnitRoom]).
func (s *Store) chartMembership(ctx context.Context, actor Actor, channelID string,
	activatedAt time.Time, unit string, members []Member) (Written, MembershipWrite, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, MembershipUnchanged, err
	}
	if actor.Kind != AuthorSystem {
		return Written{}, MembershipUnchanged, fmt.Errorf("%w: a %s room's "+
			"membership and its unit are the org chart's, written by the engine "+
			"alone — change the unit in the company document instead",
			ErrForbidden, KindUnit)
	}
	if activatedAt.IsZero() {
		return Written{}, MembershipUnchanged, fmt.Errorf("%w (room %s)",
			ErrNoActivation, channelID)
	}
	chart := chartWrite{epoch: configplane.ActivationStamp(activatedAt), unit: unit}
	return s.setMembership(ctx, actor, channelID, s.newOpID(), chart,
		plainMembership(func(room Channel, _ []Member) ([]Member, error) {
			if room.Kind != KindUnit {
				return nil, fmt.Errorf("%w: %s isn't a unit's own room, and the "+
					"org chart decides who's in a unit's room only. A change "+
					"here would take over a room someone else owns", ErrForbidden,
					room.called())
			}
			return members, nil
		}))
}

// chartWrite is what the org chart's own membership write carries beside the
// set, and the zero value is every other writer's: no activation, no unit.
type chartWrite struct {
	// epoch is the activation stamp ([configplane.ActivationStamp]), zero
	// for a write that is not the chart's.
	epoch int64

	// unit is the key the room is re-homed to when it is recorded under
	// another, and empty for a write that moves no unit.
	unit string
}

// MembershipWrite is what one membership write did to a room.
//
// FOUR ANSWERS, because the chart's reconcile logs exactly one: a write that
// moved nobody must not read as a changed membership, and a room that changed
// hands is news whoever moved with it. Only the chart's writes
// ([Store.SetUnitMembers], [Store.ReleaseUnitRoom]) can re-stamp or re-home.
type MembershipWrite string

const (
	// MembershipUnchanged is a call that published nothing (the room already
	// held the set, at this activation or later) or whose answer this node
	// cannot vouch for.
	MembershipUnchanged MembershipWrite = "unchanged"

	// MembershipRestamped is a newer activation written over the set the
	// room already held: the stamp moved and nobody did.
	MembershipRestamped MembershipWrite = "restamped"

	// MembershipMoved is a write that changed who is in the room.
	MembershipMoved MembershipWrite = "moved"

	// MembershipRehomed is a write that recorded the room under another unit
	// ([MemberSet.Unit]), possibly with the same seats (a renamed unit).
	MembershipRehomed MembershipWrite = "rehomed"
)

// Valid reports whether w is one of the four answers.
func (w MembershipWrite) Valid() bool {
	switch w {
	case MembershipUnchanged, MembershipRestamped, MembershipMoved, MembershipRehomed:
		return true
	}
	return false
}

// members is a membership write that is nobody's chart: [Store.setMembership]
// with no activation, without its second value (only the reconcile reads it).
func (s *Store) members(ctx context.Context, actor Actor, channelID, opID string,
	next func(Channel, []Member) ([]Member, error)) (Written, error) {

	written, _, err := s.setMembership(ctx, actor, channelID, opID, chartWrite{},
		plainMembership(next))
	return written, err
}

// memberDecision is what a membership write decides inside the snapshot: the
// set the room becomes, and the seats an auto-join room accounts for with it.
// The tx is the snapshot's own ([Store.AutoJoin] reads its account).
type memberDecision func(tx *sql.Tx, room Channel, held []Member) (memberChange, error)

// memberChange is a [memberDecision]'s answer.
type memberChange struct {
	set        []Member
	autoJoined []string
}

// plainMembership is a decision accounting for nobody: every membership write
// but auto-join's.
func plainMembership(next func(Channel, []Member) ([]Member, error)) memberDecision {
	return func(_ *sql.Tx, room Channel, held []Member) (memberChange, error) {
		set, err := next(room, held)
		return memberChange{set: set}, err
	}
}

// setMembership is the shared shape of every membership write: read the room
// and its current set inside one snapshot, let the caller say what the set
// becomes, and publish only if it moved, under the operation derived from the
// caller's verb and key (a join and a leave under one key are two), reporting
// what it published ([MembershipWrite]). chart is a CHART write's activation
// and unit ([Store.SetUnitMembers]), zero for every other.
func (s *Store) setMembership(ctx context.Context, actor Actor, channelID, opID string,
	chart chartWrite, next memberDecision) (Written, MembershipWrite, error) {

	at := s.now()
	subject := ChannelSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var read uint64
	wrote := MembershipUnchanged

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET ON EVERY ROUND: only the last says what this call did,
			// after a lost arbitration finds the winner's set.
			wrote = MembershipUnchanged
			room, revision, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out, read = room, revision
			held, err := readMembersTx(ctx, tx, room.ID)
			if err != nil {
				return statelog.Decision{}, err
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := reachable(room, held, actor); err != nil {
				return statelog.Decision{}, err
			}
			if room.Kind.Direct() {
				// A DIRECT CONVERSATION'S IDENTITY IS ITS PARTICIPANT SET:
				// adding one names a different room ([Store.OpenDirect]).
				// AFTER [reachable]: anybody can derive an id from two
				// handles, so refused before it this would tell a stranger
				// that those two talk.
				return statelog.Decision{}, fmt.Errorf("%w: a direct "+
					"conversation is the people in it, so changing who's in it "+
					"would make it a different conversation. Open one with the "+
					"people you want instead", ErrForbidden)
			}
			change, err := next(tx, room, held)
			if err != nil {
				return statelog.Decision{}, err
			}
			set := change.set
			// THE EMPTY LIST, NEVER NULL: emptying a room is spelled by an
			// empty list ([MemberSet]), not "members": null.
			set = sortedMembers(set)
			if set == nil {
				set = []Member{}
			}
			// A RE-HOME IS DECIDED against this snapshot's unit, never the
			// caller's read; the record carries a unit only when it moves one.
			rehome := ""
			if chart.unit != "" && chart.unit != room.Unit {
				rehome = chart.unit
			}
			switch {
			case chart.epoch != 0 && room.ChartEpoch > chart.epoch:
				// A LATER CONFIGURATION ALREADY WON: during a rollout the newer
				// one must not be walked back by an older node's apply
				// arriving second.
				return statelog.Decision{}, nil
			case slices.Equal(set, held) && rehome == "" && room.ChartEpoch >= chart.epoch &&
				len(change.autoJoined) == 0:
				// AN UNCHANGED MEMBERSHIP IS A NO-OP, not a record, or the log
				// grows with retries. For the chart that is a room already at
				// this activation; an OLDER one is re-stamped below.
				return statelog.Decision{}, nil
			}
			// THE SAME SET AT A NEWER ACTIVATION IS A RE-STAMP: no member
			// touched, no change announced. A room that changed hands is a
			// re-home whoever moved with it.
			switch {
			case rehome != "":
				wrote = MembershipRehomed
			case slices.Equal(set, held):
				wrote = MembershipRestamped
			default:
				wrote = MembershipMoved
			}
			return decide(stamp, actor, subject, OpMembers, scope, opID,
				MemberSet{V: DocumentVersion, Members: set, ChartEpoch: chart.epoch,
					Unit: rehome, AutoJoined: change.autoJoined}, nil, at)
		},
	})
	if err != nil {
		return Written{}, MembershipUnchanged, err
	}
	written, err := s.answer(ctx, channelID, "", Written{
		Channel: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	})
	// Nothing is vouched written on an unvouched answer ([Written]); the
	// outcome beside it says the write may have landed.
	if result.Unvouched {
		wrote = MembershipUnchanged
	}
	return written, wrote, err
}

// Erase removes message rows permanently — an admin redacting what should
// never have been said.
//
// Arbitrated on the room, its blast radius, so an erase racing an archive has
// one order. A person's or a credential's gesture, refused HERE to a seat and
// the engine (an author could erase the evidence). It reaches past membership
// without reading, so a credential pasted into a private room can be destroyed.
// The operation is the caller's, WHOLE: an `unknown` erase may have landed, so
// the retry is the same operation, bound to the room and [EraseSet] (ids.go).
func (s *Store) Erase(ctx context.Context, actor Actor, opID, channelID string,
	messageIDs []string, reason string) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	if err := requireOpID(opID, "erase"); err != nil {
		return Written{}, err
	}
	ids := EraseSet(messageIDs)
	if len(ids) == 0 {
		return Written{}, invalid("message_ids", "an erase names no messages, "+
			"which is a gate-installing record that removes nothing")
	}
	if len(ids) > MaxEraseMessages {
		return Written{}, invalid("message_ids", "an erase names %d messages "+
			"and the maximum is %d — a record is never split across "+
			"transactions, so more than that is one apply over the row budget. "+
			"Issue more than one gesture; each is its own auditable record",
			len(ids), MaxEraseMessages)
	}

	at := s.now()
	subject := ChannelSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var read uint64

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if !erases(actor.Kind) {
				return statelog.Decision{}, fmt.Errorf("%w: an erase was asked "+
					"for by a %s and only a person (%s) or a credential (%s) "+
					"may publish one — deleting what somebody wrote is a "+
					"compliance gesture, and an author that could erase its "+
					"own messages could erase the evidence of what it did",
					ErrForbidden, actor.Kind, AuthorHuman, AuthorOperator)
			}
			room, revision, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out, read = room, revision
			// Not reachability (an erase reaches past membership) but the
			// shared unknown-kind refusal ([unknownKind]).
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			// EVERY NAMED MESSAGE MUST BE IN THIS ROOM: an id from another
			// room removes nothing yet would be `applied` and audited as a
			// redaction that did not happen. An id already erased passes,
			// so re-issuing a partly landed gesture is not refused.
			known, err := eraseableTx(ctx, tx, room.ID, ids)
			if err != nil {
				return statelog.Decision{}, err
			}
			for _, id := range ids {
				if !known[id] {
					// The message by the caller's id, the one way to say which
					// is missing; the room as a person knows it.
					return statelog.Decision{}, fmt.Errorf("%w: message %s isn't "+
						"in %s. An erase removes exactly what it names, so "+
						"nothing was removed", ErrNotFound, id, room.called())
				}
			}
			return decide(stamp, actor, subject, OpErase, scope, opID, MessageErase{
				V: DocumentVersion, MessageIDs: ids, Reason: reason,
				By: actor.Name(), ByKind: actor.Kind,
			}, nil, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.answer(ctx, channelID, "", Written{
		Channel: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	})
}

// Prune removes everything in a room stored before an instant, which is how
// retention is enforced.
//
// A record, not a local sweep: "older than a year" on each node's clock deletes
// a different set per node; a log cutoff is one number every applier compares
// against the broker's timestamps. The retention duty's, a person's or a
// credential's gesture; a seat that could prune would delete a year of
// decisions irreversibly. It reaches past membership and takes the caller's
// operation whole, for [Store.Erase]'s reason.
func (s *Store) Prune(ctx context.Context, actor Actor, opID, channelID string,
	cutoff time.Time) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	if err := requireOpID(opID, "prune"); err != nil {
		return Written{}, err
	}
	if !erases(actor.Kind) && actor.Kind != AuthorSystem {
		return Written{}, fmt.Errorf("%w: a prune was asked for by a %s — it "+
			"deletes everything a room said before an instant, on every node, "+
			"with no inverse, so it is the retention duty's gesture (%s), a "+
			"person's (%s) or a credential's (%s)", ErrForbidden, actor.Kind,
			AuthorSystem, AuthorHuman, AuthorOperator)
	}
	at := s.now()
	if cutoff.After(at) {
		return Written{}, invalid("cutoff", "a prune's cutoff is %s and the "+
			"clock says %s — everything is stored before an instant in the "+
			"future, so this would empty the room rather than enforce a "+
			"horizon measured backwards from now",
			cutoff.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
	}

	subject := ChannelSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var read uint64

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, revision, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out, read = room, revision
			// As [Store.Erase].
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			return decide(stamp, actor, subject, OpPrune, scope, opID,
				Prune{V: DocumentVersion, Cutoff: cutoff.UTC()}, nil, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.answer(ctx, channelID, "", Written{
		Channel: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	})
}

// SetRetention states how long a room keeps what is said in it: days, or nil
// for the company's default, and [RetentionForever] for a room never pruned.
//
// Its own gesture, not a patch field: a shorter horizon is a prune on a delay
// ([Prune]), which an admin alone may schedule on a surface that can ask
// step-up. A person's or a credential's, reaching past membership, like
// [Store.Erase]. A setting matching the room's publishes nothing and is
// reported applied.
func (s *Store) SetRetention(ctx context.Context, actor Actor, channelID string,
	days *int) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	if !erases(actor.Kind) {
		return Written{}, fmt.Errorf("%w: a room's retention was asked for by a "+
			"%s — a shorter one is a prune on a delay, so it is a person's (%s) or "+
			"a credential's (%s) to set", ErrForbidden, actor.Kind, AuthorHuman,
			AuthorOperator)
	}
	payload := RetentionSet{V: DocumentVersion, RetentionDays: days}
	if err := payload.Validate(); err != nil {
		return Written{}, err
	}
	at := s.now()
	opID, opErr := s.operation(actor, "retention", channelID, retentionAsk{Days: days})
	if opErr != nil {
		return Written{}, opErr
	}
	subject := ChannelSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var read uint64

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, revision, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out, read = room, revision
			// As [Store.Erase], and BEFORE the no-op, so an unclassifiable room
			// is never answered as though its horizon were set.
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			if sameRetention(room.RetentionDays, days) {
				return statelog.Decision{}, nil
			}
			out.RetentionDays = cloneDays(days)
			return decide(stamp, actor, subject, OpRetention, scope, opID, payload, nil, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.answer(ctx, channelID, "", Written{
		Channel: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	})
}

// erases reports an author kind a destruction may be made by: a person, or a
// credential acting as itself. Never a seat (a turn's side effect must not be a
// year of a room), nor the engine, whose one destruction is the retention
// duty's prune ([Store.Prune] admits it by name).
func erases(kind AuthorKind) bool {
	return kind == AuthorHuman || kind == AuthorOperator
}

// sameRetention reports two overrides that keep a room for the same horizon:
// both absent, or both present and equal. A present zero is [RetentionForever]
// and is NOT an absent one.
func sameRetention(a, b *int) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return *a == *b
}

// cloneDays is an override the caller cannot alias.
func cloneDays(days *int) *int {
	if days == nil {
		return nil
	}
	held := *days
	return &held
}

// ---- what anybody says ------------------------------------------------- //

// NewMessage is something to say.
type NewMessage struct {
	Body string

	// Links are what the message points at (there are no attachments, see the
	// package doc): absolute http, https or mailto addresses, stored as sent
	// ([LinkSchemes]).
	Links []string

	// Mentions are the handles the body named, already resolved to seats by
	// the surface. The decide drops the ones the company no longer has, so a
	// mention of somebody who left is neither a wake nor a row.
	Mentions []string

	// Collective says the message addressed the whole room (`@channel`).
	Collective bool

	// Asks says an AGENT'S message expects an answer from the seats it
	// reaches. Without it, it wakes only the seats it names, the seats whose
	// questions it answers and those still owing an answer in its thread
	// (ADR-0113). Ignored for a person, whose message asks whatever it says.
	Asks bool

	// WHAT MAKES A POST IDEMPOTENT is the actor's key ([Actor.OpKey]). It
	// derives the MESSAGE'S id, also its operation's (head of ids.go), over the
	// room, the message answered and the words, so a remark made again after a
	// different one is a second message, not the first one's retry.
}

// Validate refuses a message for what it SAYS — nothing at all, a body or a
// set of links or mentions over its cap, a link that is not an address
// ([LinkSchemes]) — which no room and no row can make acceptable.
//
// EXPORTED because a gesture that writes BEFORE the post must ask it first:
// send_dm opens the conversation and then posts, and a message refused only
// after the open landed would leave an empty room in every participant's list.
// What only the post's snapshot can decide (archived, actor not in it) stays
// the post's; [Store.Post] and [Store.Reply] run this same check.
func (m NewMessage) Validate() error {
	if m.Body == "" && len(m.Links) == 0 {
		return invalid("body", "a post carries neither text nor a link — an "+
			"empty message is a wake for nothing")
	}
	if err := checkBody(m.Body); err != nil {
		return err
	}
	if err := checkLinks(m.Links); err != nil {
		return err
	}
	return checkMentions(cleanHandles(m.Mentions))
}

// Post says something in a room.
func (s *Store) Post(ctx context.Context, actor Actor, channelID string,
	in NewMessage) (Written, error) {

	return s.post(ctx, actor, channelID, "", in)
}

// Reply answers a message, in its thread.
//
// A THREAD IS ONE LEVEL DEEP: a reply to a reply carries the same ROOT, making
// "this thread" a range scan, so the root is resolved here from the message
// answered, not taken from the caller.
func (s *Store) Reply(ctx context.Context, actor Actor, channelID, replyTo string,
	in NewMessage) (Written, error) {

	if strings.TrimSpace(replyTo) == "" {
		return Written{}, invalid("reply_to", "a reply names no message — a "+
			"message that answers nothing is a post, which is a different "+
			"gesture with a different thread")
	}
	return s.post(ctx, actor, channelID, replyTo, in)
}

// post is the shared shape of a room post and a thread reply.
func (s *Store) post(ctx context.Context, actor Actor, channelID, replyTo string,
	in NewMessage) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	// PURE-VALUE CAPS ARE REFUSED BEFORE A SNAPSHOT: nothing about a body's
	// length depends on a row. The payload's Validate runs again inside the
	// decide, which checks the collections the decision builds.
	if err := in.Validate(); err != nil {
		return Written{}, err
	}
	mentions := cleanHandles(in.Mentions)
	verb := "post"
	if replyTo != "" {
		verb = "reply"
	}

	at := s.now()
	// THE MESSAGE ID IS THE OPERATION ID (head of ids.go), derived OVER WHAT
	// THE CALLER ASKED FOR: room, the message answered as named, and the words,
	// never the resolved thread root, which a retry could find moved.
	id, err := s.messageOperation(actor, verb, channelID, askOf(actor, replyTo, in))
	if err != nil {
		return Written{}, err
	}
	opID := id
	subject := MessageSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var message Message

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		// SEQUENCED, no expectation: arbitration would serialise the hottest
		// subject behind itself, and a rejection is a message somebody typed
		// and lost. What the snapshot cannot see (an earlier record archiving
		// the room or removing the author) the applier decides, and
		// [Store.decided] reads that before this call answers.
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			d, draftErr := s.draft(ctx, tx, actor, channelID, replyTo, id, in, mentions)
			out = d.room
			if draftErr != nil {
				return statelog.Decision{}, draftErr
			}
			message = d.message
			return decide(stamp, actor, subject, OpPost, scope, opID, d.payload, d.notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.decided(ctx, OpPost, channelID, id, Written{
		Channel: out, Message: message.forReaders(), Revision: writtenRevision(result, 0),
		ChangeID: opID, Outcome: result,
	})
}

// drafted is a post as its decision forms it: the room it is said in, the
// payload and the message the record carries, and the routing snapshot a wake
// is derived from.
type drafted struct {
	room    Channel
	payload MessagePost
	message Message
	notify  *Notify
	routing Routing
}

// draft forms a post or a reply inside a snapshot, without publishing it.
//
// ONE FUNCTION FOR THE POST AND ITS DRY RUN: [Store.post] decides from it and
// [Store.Reach] answers a composer from it, so a preview reaches exactly whom
// the same words would when sent (same checks, thread, mentions and routing,
// [Store.notifyOf]); a rule of its own would drift.
//
// THE ROOM IS RETURNED EVEN ON A REFUSAL that read it, since the answer names it.
func (s *Store) draft(ctx context.Context, tx *sql.Tx, actor Actor, channelID, replyTo,
	id string, in NewMessage, mentions []string) (drafted, error) {

	// THE ROOM IS READ FIRST, and not only for routing: the applier REFUSES a
	// post into a room it has no row for (under strict replay its create is
	// below this position), so publishing one would stall every node's applier
	// on this domain.
	room, _, err := readRoomTx(ctx, tx, channelID)
	if err != nil {
		return drafted{}, err
	}
	out := drafted{room: room}
	if room.ArchivedAt != nil {
		// Still readable; takes nothing new until reopened (authz's
		// chat.channel.archive says by whom).
		return out, fmt.Errorf("%w: %s", ErrArchived, archivedRoom)
	}
	held, err := readMembersTx(ctx, tx, room.ID)
	if err != nil {
		return out, err
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := postable(room, held, actor); err != nil {
		return out, err
	}
	root, rootAuthor, err := s.threadOf(ctx, tx, room.ID, replyTo)
	if err != nil {
		return out, err
	}
	named := s.employed(mentions)
	asks := in.Asks && actor.Kind == AuthorAgent
	out.payload = MessagePost{
		V: DocumentVersion, MessageID: id, Body: in.Body,
		ThreadRoot: root, Mentions: named,
		Collective: in.Collective, Links: in.Links, Asks: asks,
		Author: actor.Name(), AuthorKind: actor.Kind,
		// The rule [postable] applied, for the applier to apply again: none
		// for the engine's narration, which is no member.
		MembersOnly: privateRoom(room.Kind) && actor.Kind != AuthorSystem,
	}
	out.message = Message{
		V: DocumentVersion, ID: id, ChannelID: room.ID,
		ThreadRoot: root, Author: out.payload.Author,
		AuthorKind: out.payload.AuthorKind, Body: in.Body,
		Links: in.Links, Mentions: named,
		Collective: in.Collective, Asks: asks,
	}
	out.routing = Routing{
		ChannelKind: room.Kind, AuthorKind: actor.Kind,
		Members: held, Mentions: named, Collective: in.Collective,
		ThreadRoot: root, RootAuthor: rootAuthor, Asks: asks,
	}
	out.notify, err = s.notifyOf(ctx, tx, actor, room, out.message, out.routing)
	if err != nil {
		return out, err
	}
	// Whom it reached, as the applier writes it on the row.
	out.message.Reach = reachOf(actor.Kind, out.notify)
	return out, nil
}

// Draft is what a post WOULD do if it were sent now: the room, the thread it
// would join, and whom it would reach ([Reach]), as [Store.Reach] answers it.
type Draft struct {
	ChannelID  string `json:"channel_id"`
	ThreadRoot string `json:"thread_root,omitempty"`

	// Reach is whom the post would wake and why, in routing order; nil for
	// an author the routing records no reach for (anyone but a person).
	Reach *Reach `json:"reach,omitempty"`

	// Mentions are the seats the words name, and Collective whether they
	// address the whole room (`@channel`): the routing's two inputs from
	// the words, said back so a composer shows what it read.
	Mentions   []string `json:"mentions,omitempty"`
	Collective bool     `json:"collective,omitempty"`

	// Quiet is why a draft that reaches no agent reaches none, and empty
	// when it reaches one: [QuietNoAgent] or [QuietAddressed].
	Quiet Quiet `json:"quiet,omitempty"`
}

// Quiet is why a person's post would reach no agent.
type Quiet string

const (
	// QuietNoAgent is a room with no agent seat to answer it: no agent
	// member (for a unit's room, no lead), and none the words name.
	QuietNoAgent Quiet = "no_agent"

	// QuietAddressed is a post that names only people: it is addressed to
	// them, so no agent is asked to answer for the room
	// ([Routing.namesASeat]).
	QuietAddressed Quiet = "addressed_to_people"
)

// Reach answers whom a post or a reply would reach if it were sent now,
// without sending it: a DRY RUN of the post's decision ([Store.draft]),
// published nowhere. A post's refusals (archived, not postable) come back here
// so a composer learns them before the send. A draft with no words is not
// refused: a composer shows whom an empty box would reach.
func (s *Store) Reach(ctx context.Context, actor Actor, channelID, replyTo string,
	in NewMessage) (Draft, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Draft{}, err
	}
	if err := checkBody(in.Body); err != nil {
		return Draft{}, err
	}
	mentions := cleanHandles(in.Mentions)
	if err := checkMentions(mentions); err != nil {
		return Draft{}, err
	}
	var out Draft
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		d, err := s.draft(ctx, tx, actor, channelID, strings.TrimSpace(replyTo), "", in, mentions)
		if err != nil {
			return err
		}
		out = Draft{
			ChannelID: d.room.ID, ThreadRoot: d.message.ThreadRoot,
			Reach: d.message.Reach, Mentions: d.message.Mentions,
			Collective: d.message.Collective,
		}
		if out.Reach != nil && len(out.Reach.Recipients) == 0 {
			out.Quiet = QuietNoAgent
			if d.routing.namesASeat(s.roster.Seat, actor.Name()) {
				out.Quiet = QuietAddressed
			}
		}
		return nil
	})
	if err != nil {
		return Draft{}, err
	}
	return out, nil
}

// answer is a gesture's answer: [answered]'s, and for a call answered as
// ANOTHER COPY of its operation ([statelog.Result.Collapsed]) the room and
// message as this node's rows have them. Every gesture returns through it.
// A collapsed answer is read from the ROWS, not the request or this call's
// decision: the ledger answered before any decision ran ([statelog.Snap.Held]),
// or the broker's duplicate window collapsed this append onto an earlier copy
// decided on rows and a roster that may have moved. What exists is what the
// first record wrote ([Store.operation] derives from what the call ASKED for,
// so the words agree). A row this node does not hold (`pending`, erased,
// pruned) answers with the identity alone. Any other read failure is returned
// beside the identity, never as an empty message; the same call again writes
// nothing.
func (s *Store) answer(ctx context.Context, channelID, messageID string,
	w Written) (Written, error) {

	w = answered(w)
	if !w.Outcome.Collapsed {
		// This call's own answer: landed, pending, or (an `unknown` with a
		// lost acknowledgement) possibly landed. Not an unvouched `unknown`,
		// which [answered] emptied: nothing of it was appended.
		return w, nil
	}
	room := Channel{ID: channelID}
	var message Message
	if messageID != "" {
		message = Message{ID: messageID, ChannelID: channelID}
	}
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		held, _, err := readRoomTx(ctx, tx, channelID)
		switch {
		case errors.Is(err, ErrNotFound):
			held = Channel{ID: channelID}
		case err != nil:
			return err
		}
		said := message
		if messageID != "" {
			got, err := readMessageTx(ctx, tx, channelID, messageID)
			switch {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				return err
			default:
				// The stored document as readers are handed it, so a repeat
				// hands over no requester either ([Message.forReaders]).
				said = got.forReaders()
			}
		}
		// ASSIGNED ONLY ONCE BOTH READS ARE IN: the store re-runs this body on a
		// conflict, and a kept room would pair two snapshots.
		room, message = held, said
		return nil
	})
	w.Channel, w.Message = room, message
	if err != nil {
		what := "room " + channelID
		if messageID != "" {
			what = "message " + messageID + " in room " + channelID
		}
		return w, fmt.Errorf("chatlet: the gesture on %s landed under an earlier "+
			"copy of its operation, and this node could not read it back — "+
			"making the same call again is answered without writing again: %w",
			what, err)
	}
	return w, nil
}

// EditMessage is a message's new content.
//
// Mentions are re-carried, since they derive from the body. Links do not: nil
// KEEPS the message's links and a non-nil list, empty included, replaces them
// (a plain slice could not tell the two apart, and a typo fix would drop every
// link). A retried edit is one record under the actor's key ([Actor.OpKey]),
// derived over the message and the new words.
type EditMessage struct {
	Body     string
	Links    *[]string
	Mentions []string
}

// Edit rewrites a message's body in place.
//
// Only the author, an admin included, checked against the decide's snapshot:
// words somebody else rewrote would be attributed to a person who did not write
// them, and are quoted in every wake. Removal is [Store.Delete] and destruction
// [Store.Erase]; both record who did it, which an edit cannot.
//
// An edit wakes only whom it newly names: re-arming the room's standing would
// page a thread on every typo fix.
func (s *Store) Edit(ctx context.Context, actor Actor, channelID, messageID string,
	in EditMessage) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	if in.Body == "" && in.Links != nil && len(*in.Links) == 0 {
		return Written{}, emptiedByEdit()
	}
	if err := checkBody(in.Body); err != nil {
		return Written{}, err
	}
	if in.Links != nil {
		if err := checkLinks(*in.Links); err != nil {
			return Written{}, err
		}
	}
	mentions := cleanHandles(in.Mentions)
	if err := checkMentions(mentions); err != nil {
		return Written{}, err
	}

	at := s.now()
	// THE OPERATION NAMES WHAT THE CALL SENT: an edit that kept the links sent
	// none (a nil list), not the links the decide will find, which this side of
	// the snapshot cannot read.
	opID, opErr := s.operation(actor, "edit", messageID, editAsk{
		Channel: channelID, Body: strings.TrimSpace(in.Body), Links: in.Links,
	})
	if opErr != nil {
		return Written{}, opErr
	}
	subject := MessageSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var message Message

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		// SEQUENCED, for [Store.post]'s reason.
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, _, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = room
			// An unclassifiable room takes nothing, an edit included
			// ([unknownKind]); authorship is no substitute.
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			held, err := readMessageTx(ctx, tx, room.ID, messageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := isEvent(held, "edit"); err != nil {
				return statelog.Decision{}, err
			}
			if !actor.wrote(held) {
				return statelog.Decision{}, fmt.Errorf("%w: this message was "+
					"written by %s, and only its author can edit it",
					ErrForbidden, held.Author)
			}
			if held.DeletedAt != nil {
				return statelog.Decision{}, fmt.Errorf("%w: this message was "+
					"taken back, so there's nothing left to edit", ErrNotFound)
			}
			// The links the message ends with, resolved against this snapshot's
			// row so the record carries them whole.
			links := held.Links
			if in.Links != nil {
				links = *in.Links
			}
			if in.Body == "" && len(links) == 0 {
				return statelog.Decision{}, emptiedByEdit()
			}
			members, err := readMembersTx(ctx, tx, room.ID)
			if err != nil {
				return statelog.Decision{}, err
			}
			named := s.employed(mentions)
			message = held
			message.Body, message.Links, message.Mentions = in.Body, links, named
			message.EditedBy, message.EditedByKind = actor.Name(), actor.Kind

			// A SYSTEM LINE'S EDIT WAKES NOBODY, as its post: narration is
			// rendered, not answered.
			var notify *Notify
			if actor.Kind != AuthorSystem {
				// Only the handles this edit adds; the rest heard at the post.
				woken, truncated := ResolveMentions(Routing{
					ChannelKind: room.Kind, AuthorKind: actor.Kind,
					Members: members, Mentions: without(named, held.Mentions),
					Asks: held.Asks,
				})
				notify = &Notify{
					MessageID: messageID, ChannelID: room.ID,
					ChannelName: room.Name, ChannelKind: room.Kind,
					ThreadRoot: held.ThreadRoot, Author: actor.Name(),
					AuthorKind: actor.Kind, Excerpt: Excerpt(in.Body),
					Mentions: named,
					Recipients: RecipientsOf(
						Route(woken, s.roster.Seat, actor.Name())),
					WakesTruncated: truncated,
				}
			}
			return decide(stamp, actor, subject, OpEdit, scope, opID, MessageEdit{
				V: DocumentVersion, MessageID: messageID, Body: in.Body,
				Mentions: named, Links: links,
				EditedBy: actor.Name(), EditedByKind: actor.Kind,
			}, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.decided(ctx, OpEdit, channelID, messageID, Written{
		Channel: out, Message: message.forReaders(), Revision: writtenRevision(result, 0),
		ChangeID: opID, Outcome: result,
	})
}

// emptiedByEdit refuses an edit leaving a message empty, found before the
// snapshot (both halves cleared) or inside it (kept links the row lacks).
func emptiedByEdit() error {
	return invalid("body", "an edit leaves the message with neither text nor "+
		"a link — emptying a message is a delete, which keeps the row so the "+
		"thread hung off it still has its root")
}

// Delete tombstones a message: the body goes, the row stays.
//
// THE ROW STAYS so a thread hung off this message keeps its root and replies.
// Removing the row is [Store.Erase], a different gesture with a gate.
//
// THE AUTHOR OR A MODERATOR: anybody may take their own words back; taking
// somebody else's down is moderation ([MessageAuthority]), decided by the
// authority table and judged here against the author this snapshot reads. Never
// decided from the actor's KIND.
func (s *Store) Delete(ctx context.Context, actor Actor, channelID,
	messageID string, authority MessageAuthority) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	at := s.now()
	// Nothing but the message and its room; authority is who asks, not what.
	opID, opErr := s.operation(actor, "delete", messageID, channelID)
	if opErr != nil {
		return Written{}, opErr
	}
	subject := MessageSubject(channelID)
	scope := ScopeSet{Subject: true}
	var out Channel
	var message Message

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		// SEQUENCED, for [Store.post]'s reason.
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, _, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = room
			// Not reachability (a delete reaches past membership, for its
			// author and a moderator) but the shared unknown-kind refusal.
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			held, err := readMessageTx(ctx, tx, room.ID, messageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := isEvent(held, "delete"); err != nil {
				return statelog.Decision{}, err
			}
			if !actor.wrote(held) && !authority.Moderate {
				return statelog.Decision{}, fmt.Errorf("%w: this message was "+
					"written by %s. Only its author can take it back, and "+
					"removing someone else's words needs an admin",
					ErrForbidden, held.Author)
			}
			message = held
			message.DeletedBy, message.DeletedByKind = actor.Name(), actor.Kind
			// A DELETION WAKES NOBODY: there is nothing to read or answer.
			return decide(stamp, actor, subject, OpDelete, scope, opID, MessageDelete{
				V: DocumentVersion, MessageID: messageID,
				DeletedBy: actor.Name(), DeletedByKind: actor.Kind,
			}, nil, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.decided(ctx, OpDelete, channelID, messageID, Written{
		Channel: out, Message: message.forReaders(), Revision: writtenRevision(result, 0),
		ChangeID: opID, Outcome: result,
	})
}

// MessageAuthority is what a caller may do to a message somebody else wrote.
//
// A value stating the DECISION: the caller asks the delete's authority row
// (an author on the stored message; a moderator on ADMIN alone, not on any
// relation to the room) and passes the result, as this package holds no grants.
type MessageAuthority struct {
	// Moderate reports a caller authorized to take down a message they did not
	// write. It reaches PAST MEMBERSHIP: a moderator takes down what was said in
	// a room they are not in, and is never shown the rest.
	//
	// THERE IS NO EDIT EQUIVALENT: a take-down leaves a visible tombstone, but
	// an EDIT would attribute words to a person who did not write them, on a row
	// quoted in every wake it produced.
	Moderate bool
}

// React puts one emoji on a message.
func (s *Store) React(ctx context.Context, actor Actor, channelID, messageID,
	emoji string) (Written, error) {

	return s.react(ctx, actor, channelID, messageID, emoji, false)
}

// Unreact takes one back.
//
// ONE RECORD SHAPE, a TOGGLE rather than a set: two people reacting to one
// message never overwrite each other.
func (s *Store) Unreact(ctx context.Context, actor Actor, channelID, messageID,
	emoji string) (Written, error) {

	return s.react(ctx, actor, channelID, messageID, emoji, true)
}

// react is both signs of the toggle.
//
// THE SIGN IS PART OF THE OPERATION (it is the verb): add and take-back are two
// records, the same gesture again under its key one. Collapsing on content alone
// would leave a re-added reaction wherever the ledger's sweep left it.
func (s *Store) react(ctx context.Context, actor Actor, channelID, messageID,
	emoji string, removed bool) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	verb := "react"
	if removed {
		verb = "unreact"
	}
	at := s.now()
	opID, opErr := s.operation(actor, verb, messageID, reactAsk{
		Channel: channelID, Emoji: strings.TrimSpace(emoji),
	})
	if opErr != nil {
		return Written{}, opErr
	}
	subject := MessageSubject(channelID)
	scope := ScopeSet{Subject: true}
	payload := Reaction{
		V: DocumentVersion, MessageID: messageID,
		Emoji: strings.TrimSpace(emoji), Removed: removed, By: actor.Name(),
	}
	if !removed {
		// This build's cap, stamped so every applier declines at it.
		payload.Cap = MaxReactionEmoji
	}
	if err := payload.Validate(); err != nil {
		return Written{}, err
	}
	var out Channel

	result, err := s.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		// SEQUENCED: a toggle cannot see the set it joins, so whether it is
		// one emoji too many is the applier's call ([MaxReactionEmoji]).
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, _, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = room
			// THE SHARED REACHABILITY RULE: edit and delete are gated on
			// authorship, but a reaction has no gate of its own, so without
			// this a handle knowing a room id and message id would attach a
			// durable row inside a private conversation it cannot open.
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := s.reachableTx(ctx, tx, room, actor); err != nil {
				return statelog.Decision{}, err
			}
			held, err := readMessageTx(ctx, tx, room.ID, messageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := isEvent(held, "reaction"); err != nil {
				return statelog.Decision{}, err
			}
			// A message this snapshot holds full is refused before the append,
			// in the verdict's words ([VerdictReactionsFull]); the cap is the
			// applier's, this only saves a certain refusal.
			if !removed {
				full, err := reactionsFull(ctx, tx, payload)
				if err != nil {
					return statelog.Decision{}, err
				}
				if full {
					return statelog.Decision{}, VerdictReactionsFull.refusal(OpReact,
						room)
				}
			}
			// A REACTION NEVER WAKES ANYBODY nor discharges a reply
			// obligation, or a seat could answer every message with a thumb.
			return decide(stamp, actor, subject, OpReact, scope, opID, payload, nil, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.decided(ctx, OpReact, channelID, "", Written{
		Channel: out, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	})
}

// decided is a message gesture's answer once the applier has decided its
// record: [Store.answer]'s where it landed, the domain's refusal where not.
// A message record is SEQUENCED ([statelog.PatternSequenced]): the applier
// decides it at its position from rows the decision could not see, so `applied`
// only says this node consumed it. The verdict is read in the snapshot holding
// those rows, only on `applied` (otherwise [Written.Verdict] is empty), and
// answered with the sentinel the decision would have refused with
// ([Verdict.refusal]). A ledger-answered retry ([statelog.Result.Collapsed])
// reads its first copy's verdict. A verdict the horizon let go is `unknown`
// ([verdictLost], [statelog.Result.Unvouched]); a gate-dropped record is this
// node's own eviction between decision and append.
func (s *Store) decided(ctx context.Context, op OpKind, channelID, messageID string,
	w Written) (Written, error) {

	at := w.Outcome.Position
	if w.Outcome.Outcome != statelog.OutcomeApplied || at.IsZero() {
		return s.answer(ctx, channelID, messageID, w)
	}
	var (
		v  Verdict
		ok bool
	)
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		v, ok, err = verdictIn(ctx, tx, w.ChangeID, at)
		return err
	})
	switch {
	case err != nil:
		return answered(w), fmt.Errorf("chatlet: the %s landed at %s and this node could "+
			"not read what its applier decided about it — making the same call again "+
			"is answered without writing again: %w", op, at, err)
	case !ok:
		// RETAINED: consumed, not applied; its effect is not known here yet.
		w.Outcome.Outcome = statelog.OutcomePending
		return s.answer(ctx, channelID, messageID, w)
	case v == verdictLost:
		// UNKNOWN, AND UNVOUCHED: decided, but the decision is no longer kept,
		// so no position and no room ([answered]).
		return answered(Written{ChangeID: w.ChangeID, Outcome: statelog.Result{
			Outcome: statelog.OutcomeUnknown, OpID: w.ChangeID,
			Waited: w.Outcome.Waited, Unvouched: true,
		}}), nil
	case v == verdictDropped:
		return Written{ChangeID: w.ChangeID, Outcome: w.Outcome}, &statelog.Unavailable{
			Reason: statelog.ReasonEvicted,
			Detail: fmt.Sprintf("the %s's record at %s was dropped by every node's "+
				"eviction gate: its writer was removed from the fleet before it "+
				"landed, and nothing it said was applied anywhere", op, at),
			Position: at, OpID: w.ChangeID,
		}
	case v.Landed():
		w.Verdict = v
		return s.answer(ctx, channelID, messageID, w)
	}
	// REFUSED: nothing of this gesture is in the room, so the answer names
	// the room and the operation and no message.
	return Written{Channel: w.Channel, ChangeID: w.ChangeID, Outcome: w.Outcome,
		Verdict: v}, v.refusal(op, w.Channel)
}

// ---- forming and publishing one record --------------------------------- //

// publish runs one request and translates the framework's refusals into this
// package's own vocabulary.
//
// Outcomes travel unchanged; refusals map to what they mean HERE. Exhausted
// compare-and-set rounds are a room somebody kept changing. A refused create
// reads the subject's KIND: an ADDRESS is a name somebody holds; a ROOM is a
// direct conversation whose derived id exists ([Store.OpenDirect]). A conflict
// carries the framework's sentinel but not its error, whose text names a broker
// path. Rooms and messages only: the node gate goes to the framework directly
// ([GateWriter]), as this would name a room after a node id.
func (s *Store) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	result, err := s.publisher.Publish(ctx, req)
	return result, publishRefusal(ObjectKind(req.Subject.Kind), err)
}

// publishRefusal is [Store.publish]'s translation of what the framework
// answered a write on a subject of kind, nil for nil.
func publishRefusal(kind ObjectKind, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, statelog.ErrExists):
		if kind == KindChannelName {
			// The name is the create's to say ([Store.CreateChannel]): this
			// subject's id is a digest no reader can place.
			return ErrNameTaken
		}
		return ErrRoomExists
	case errors.Is(err, statelog.ErrConflict):
		return fmt.Errorf("%w: %w", ErrConflict, statelog.ErrConflict)
	}
	return err
}

// decide builds one record inside the snapshot's own transaction.
//
// Every payload is validated here ([Payload]): a cap enforced at each caller is
// missing from the next. The framework's stamp (writer and generation) rides
// every record and this is the one builder; the publisher refuses a record
// without it ([statelog.Stamp]). A function, not a method: the room store and
// the node gate ([GateWriter], no roster) share it.
func decide(stamp statelog.Stamp, actor Actor, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload Payload, notify *Notify, at time.Time) (statelog.Decision, error) {

	// The op rides the subject kind the table names. The applier dispatches
	// on (kind, op), so a mismatch would fault the domain on every node;
	// refused here it is one failed call. Not [invalid]: the request is fine.
	want, ok := SubjectKind(op, createKindOf(payload))
	if !ok || subject.Kind != want {
		return statelog.Decision{}, fmt.Errorf("chatlet: a %s record was formed "+
			"on a %s subject and the op table places %s on %q — a record "+
			"published there would fault every node's applier", op,
			subject.Kind, op, want)
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if err := payload.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if err := notify.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	// A RECORD THAT WAKES NOBODY BY CONSTRUCTION MUST NOT CARRY A SNAPSHOT: the
	// wake filter asks whether a record has a [Notify], so one on a kind nothing
	// routes would be invisible and look like a feed bug.
	if notify != nil && !subject.Kind.Routable() {
		return statelog.Decision{}, fmt.Errorf("chatlet: a %s record carries a "+
			"routing snapshot and no wake is ever derived from that kind — see "+
			"ObjectKind.Routable", subject.Kind)
	}
	writtenFor, err := actor.recordedFor(notify)
	if err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("chatlet: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// No version: the encoder stamps the lowest that reads this
			// record ([RecordVersion]).
			OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Mutation:   body,
		Actor:      actor.Name(),
		ActorKind:  actor.Kind,
		OperatorID: actor.OperatorID,
		TurnID:     actor.TurnID,
		Chain:      actor.Chain,
		WorkingFor: writtenFor,
		Notify:     notify,
	}
	// THE RECORD'S SIZE IS THE FRAMEWORK'S TO REFUSE, against the stream's
	// bound ([statelog.StreamSpec.MaxRecordBytes], [MaxRecordBytes]): past it,
	// `record_too_large`. Every collection here is capped below it, which the
	// record tests hold at every cap.
	encoded, err := Encode(record)
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{Payload: encoded}, nil
}

// createKindOf is the channel kind a create's payload states, for the one op
// whose subject kind depends on it ([SubjectKind]); "" for every other
// payload, which [SubjectKind] never reads.
func createKindOf(payload Payload) Kind {
	if create, ok := payload.(ChannelCreate); ok {
		return create.Kind
	}
	return ""
}

// wire is a subject as the framework names it.
func wire(s Subject) statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// writtenRevision is the number a write reports as [Written.Revision].
//
// A record that landed answers with ITS OWN composed position, what the applier
// stamps into the row, so a caller can compare it against a later read to know
// whether its write is visible. A decision that published nothing answers with
// the revision the row was already at, read in the decision's snapshot.
func writtenRevision(result statelog.Result, read uint64) uint64 {
	if result.Position.Seq == 0 {
		return read
	}
	return uint64(result.Position.Packed())
}

// ---- the routing snapshot ---------------------------------------------- //

// notifyOf builds the routing snapshot a wake is derived from, or nil for a
// record that wakes nobody.
//
// A system line wakes nobody and is nil, not an empty snapshot: nil is what the
// wake filter asks about, and a payload flag is read after a version-gated
// decode a newer build's record does not survive. Everything else carries one
// even when it wakes nobody, since a card and a mention feed render from it.
func (s *Store) notifyOf(ctx context.Context, tx *sql.Tx, actor Actor, room Channel,
	message Message, routing Routing) (*Notify, error) {

	if actor.Kind == AuthorSystem {
		return nil, nil
	}
	if routing.ThreadRoot != "" {
		parties, err := threadPartiesTx(ctx, tx, room.ID, routing.ThreadRoot)
		if err != nil {
			return nil, err
		}
		followers, err := threadFollowersTx(ctx, tx, room.ID, routing.ThreadRoot)
		if err != nil {
			return nil, err
		}
		owing, err := owingTx(ctx, tx, room.ID, routing.ThreadRoot, actor.Name())
		if err != nil {
			return nil, err
		}
		awaiting, err := awaitingTx(ctx, tx, room.ID, routing.ThreadRoot, actor.Name())
		if err != nil {
			return nil, err
		}
		routing.ThreadParticipants, routing.Followers = parties, followers
		routing.Owing, routing.Awaiting = owing, awaiting
	}
	answers, answering, err := answersTx(ctx, tx, room, routing.ThreadRoot, actor.Name())
	if err != nil {
		return nil, err
	}
	routing.Answers = answers
	routing.Answerer = s.answerer(room, actor, routing)

	candidates, truncated := Resolve(routing)
	woken := Route(candidates, s.roster.Seat, actor.Name())
	held := Route(Held(routing, candidates), s.roster.Seat, actor.Name())
	return &Notify{
		MessageID: message.ID, ChannelID: room.ID, ChannelName: room.Name,
		ChannelKind: room.Kind, ThreadRoot: message.ThreadRoot,
		Author: message.Author, AuthorKind: message.AuthorKind,
		Excerpt: Excerpt(message.Body), Mentions: message.Mentions,
		Answerer: routing.Answerer,
		// Routed at write time so the feed never subtracts; the parser routes
		// again through this same function against its own roster (a seat may
		// have left since).
		Recipients:     RecipientsOf(woken),
		WakesTruncated: truncated,
		Asks:           routing.Asks,
		Held:           RecipientsOf(held),
		Asked:          askedOf(room, actor, routing, woken),
		Answering:      answering,
	}, nil
}

// askedOf is whom an AGENT'S asking message asked ([Notify.Asked]): the seats
// it woke for anything but their own stake or an answer, the people it named
// and, in a direct conversation, everybody else in it. Each gets a receipt, so
// the answer wakes the asker. Nil for a person's message (its receipts are its
// reach) and for an agent's that asks nothing.
func askedOf(room Channel, actor Actor, routing Routing, woken []Candidate) []string {
	if actor.Kind != AuthorAgent || !routing.Asks {
		return nil
	}
	seen := map[string]bool{actor.Name(): true}
	var out []string
	add := func(handle string) {
		if handle != "" && !seen[handle] {
			seen[handle] = true
			out = append(out, handle)
		}
	}
	for _, c := range woken {
		if !c.Reason.Stake() {
			add(c.Handle)
		}
	}
	for _, handle := range routing.mentioned() {
		add(handle)
	}
	if room.Kind.Direct() {
		for _, handle := range memberHandles(routing.Members) {
			add(handle)
		}
	}
	return out
}

// answersTx is the authors of the asks this message answers ([ReasonAnswer]):
// the author's open receipts in its thread, or anywhere in a direct
// conversation; and for a top-level post in a named room, each agent's newest
// top-level ask there, whose id it returns for the applier to mark answered.
func answersTx(ctx context.Context, tx *sql.Tx, room Channel, root, author string) (
	askers, answering []string, err error) {

	query := `
		SELECT r.message_id, m.author_handle,
		       ROW_NUMBER() OVER (PARTITION BY m.author_handle ORDER BY m.channel_seq DESC) AS newest
		  FROM chat_receipts r INDEXED BY chat_receipts_open_idx
		  JOIN chat_messages m ON m.id = r.message_id
		 WHERE r.channel_id = ? AND r.handle = ? AND r.answer_id = ''
		   AND m.author_handle <> ? AND m.deleted_at IS NULL`
	args := []any{room.ID, author, author}
	top := false
	switch {
	case room.Kind.Direct():
	case root != "":
		query += ` AND r.thread_key = ?`
		args = append(args, root)
	default:
		top = true
		query += ` AND m.thread_root = '' AND m.author_kind = ?`
		args = append(args, string(AuthorAgent))
	}
	// Each asker's newest ask, chosen before the cap so older asks never crowd
	// another's out.
	query = `SELECT message_id, author_handle FROM (` + query + `)
		 WHERE newest = 1 ORDER BY author_handle LIMIT ?`
	args = append(args, MaxThreadParticipants)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("chatlet: read the asks %s answers: %w", author, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var message, asker string
		if err := rows.Scan(&message, &asker); err != nil {
			return nil, nil, err
		}
		askers = append(askers, asker)
		if top {
			answering = append(answering, message)
		}
	}
	return askers, answering, rows.Err()
}

// awaitingTx is the agents that asked something in this thread, other than the
// author, with no answer yet ([ReasonAwaiting]), less a co-asker's: an author
// whose only stake is awaiting an addressee does not wake another seat awaiting
// the same one.
func awaitingTx(ctx context.Context, tx *sql.Tx, channelID, root, author string) (
	[]string, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT m.author_handle, m.author_kind, r.handle
		  FROM chat_receipts r INDEXED BY chat_receipts_open_idx
		  JOIN chat_messages m ON m.id = r.message_id
		 WHERE r.channel_id = ? AND r.thread_key = ? AND r.answer_id = ''
		   AND m.deleted_at IS NULL
		 ORDER BY m.author_handle, r.handle LIMIT ?`,
		channelID, root, MaxThreadParticipants*MaxThreadParticipants)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read the open asks of thread %s: %w", root, err)
	}
	defer func() { _ = rows.Close() }()
	asked := map[string][]string{}
	var askers []string
	owes := false
	for rows.Next() {
		var asker, kind, addressee string
		if err := rows.Scan(&asker, &kind, &addressee); err != nil {
			return nil, err
		}
		owes = owes || addressee == author
		// A person's own ask is a stake making a co-asker, but only an agent
		// is woken for one.
		if _, seen := asked[asker]; !seen && AuthorKind(kind) == AuthorAgent {
			askers = append(askers, asker)
		}
		asked[asker] = append(asked[asker], addressee)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return coAskersOut(askers, asked, author, owes), nil
}

// coAskersOut is the askers a reply by author wakes: every one but the author,
// less those awaiting an addressee the author awaits too when the author owes
// nothing in the thread. Capped at [MaxThreadParticipants].
func coAskersOut(askers []string, asked map[string][]string, author string, owes bool) []string {
	var out []string
	for _, asker := range askers {
		if asker == author || len(out) == MaxThreadParticipants {
			continue
		}
		if !owes && slices.ContainsFunc(asked[asker], func(a string) bool {
			return slices.Contains(asked[author], a)
		}) {
			continue
		}
		out = append(out, asker)
	}
	return out
}

// owingTx is the seats asked something in this thread, other than the author,
// that have not answered yet ([ReasonOwing]): an open receipt on a standing
// message that still asks them (an agent's asking message, or a person's that
// still names them). A receipt for a seat a person's message merely reached
// asked it nothing, nor does a message taken back or edited to drop the name.
func owingTx(ctx context.Context, tx *sql.Tx, channelID, root, author string) (
	[]string, error) {

	return handlesTx(ctx, tx, `
		SELECT DISTINCT r.handle
		  FROM chat_receipts r INDEXED BY chat_receipts_open_idx
		  JOIN chat_messages m ON m.id = r.message_id
		 WHERE r.channel_id = ? AND r.thread_key = ? AND r.answer_id = ''
		   AND r.handle <> ? AND m.deleted_at IS NULL
		   AND (m.author_kind = ? OR EXISTS (SELECT 1 FROM chat_mentions x
		        WHERE x.message_id = m.id AND x.handle = r.handle))
		 ORDER BY r.handle LIMIT ?`,
		channelID, root, author, string(AuthorAgent), MaxThreadParticipants)
}

// answerer resolves the seat a person's post in this room is answered by when
// it reaches nobody else, or empty.
//
// Asked only where it can be used ([answerable]). A message naming a seat the
// company has, a person included, is addressed and carries none
// ([Routing.namesASeat]).
//
// A unit's room is answered by the unit's lead, through the chart rather than
// the membership (a unit that stops declaring its room gives it to nobody).
// Every other named room is answered by its most senior agent member, so a
// private room's answerer can always read it; none if no agent is a member.
func (s *Store) answerer(room Channel, actor Actor, routing Routing) string {
	if !answerable(room.Kind, actor.Kind) || routing.namesASeat(s.roster.Seat, actor.Name()) {
		return ""
	}
	if room.Kind == KindUnit {
		if room.Unit == "" {
			return ""
		}
		return s.roster.Lead(room.Unit, room.Name)
	}
	return s.roster.Senior(memberHandles(routing.Members))
}

// employed drops the handles the company no longer has.
//
// AT WRITE TIME: a `chat_mentions` row for somebody who left is a feed entry
// nobody opens, replicated to every node for the message's life.
func (s *Store) employed(handles []string) []string {
	if len(handles) == 0 {
		return nil
	}
	out := make([]string, 0, len(handles))
	for _, h := range handles {
		if exists, _ := s.roster.Seat(h); exists {
			out = append(out, h)
		}
	}
	return out
}

// threadOf resolves the thread a reply belongs to, and the seat that started
// it ([Message.speaker]).
//
// A REPLY TO A REPLY IS A REPLY TO THE THREAD (one level deep), so the root is
// read off the message answered, not taken from the caller, keeping "this
// thread" a range scan.
func (s *Store) threadOf(ctx context.Context, tx *sql.Tx, channelID, replyTo string) (
	root, author string, err error) {

	if replyTo == "" {
		return "", "", nil
	}
	parent, err := readMessageTx(ctx, tx, channelID, replyTo)
	if err != nil {
		return "", "", err
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := isEvent(parent, "reply"); err != nil {
		return "", "", err
	}
	if parent.ThreadRoot == "" {
		return parent.ID, parent.speaker(), nil
	}
	held, err := readMessageTx(ctx, tx, channelID, parent.ThreadRoot)
	if err != nil {
		return "", "", err
	}
	return held.ID, held.speaker(), nil
}

// without is the handles in `handles` that `drop` does not hold, in order: what
// an edit newly names, or (reversed) stopped naming.
func without(handles, drop []string) []string {
	gone := make(map[string]bool, len(drop))
	for _, h := range drop {
		gone[h] = true
	}
	out := make([]string, 0, len(handles))
	for _, h := range handles {
		if !gone[h] {
			out = append(out, h)
		}
	}
	return out
}

// ---- reading this node's own rows, inside a decision ------------------- //

// room reads one room outside a decision, for the answer a direct
// conversation's create needs when the room was already there.
func (s *Store) room(ctx context.Context, channelID string) (Channel, uint64, error) {
	var room Channel
	var revision uint64
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		room, revision, err = readRoomTx(ctx, tx, channelID)
		return err
	})
	return room, revision, err
}

// readRoomTx reads one room from inside a decision's own transaction, and the
// log revision the row was written through.
//
// THE REVISION TRAVELS WITH THE ROOM: one fact about one row, and a revision
// read after a concurrent apply would not match the room beside it.
//
// THROUGH THE DOCUMENT, although every field is also a column: the document
// carries a NEWER BUILD'S fields through this node untouched, and a patch
// rebuilt from columns would drop them on a mid-upgrade fleet's first edit.
func readRoomTx(ctx context.Context, tx *sql.Tx, channelID string) (Channel, uint64, error) {
	if strings.TrimSpace(channelID) == "" {
		return Channel{}, 0, invalid("channel_id", "a write names no room")
	}
	var document []byte
	var revision int64
	err := tx.QueryRowContext(ctx,
		`SELECT document, `+ChannelRevision+` FROM chat_channels WHERE id = ?`,
		channelID).Scan(&document, &revision)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Channel{}, 0, notFound()
	case err != nil:
		return Channel{}, 0, fmt.Errorf("chatlet: read room %s: %w", channelID, err)
	}
	room, err := DecodeChannel(document)
	if err != nil {
		return Channel{}, 0, err
	}
	return room, uint64(revision), nil
}

// readMembersTx reads one room's membership, IN HANDLE ORDER.
//
// The applier's own order ([replaceMembers] sorts by handle), so a decision
// compares two slices in one order.
func readMembersTx(ctx context.Context, tx *sql.Tx, channelID string) ([]Member, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT handle, follow_all FROM chat_members WHERE channel_id = ?
		  ORDER BY handle`, channelID)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read room %s's members: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Member
	for rows.Next() {
		var m Member
		var followAll int
		if err := rows.Scan(&m.Handle, &followAll); err != nil {
			return nil, fmt.Errorf("chatlet: scan a member of room %s: %w",
				channelID, err)
		}
		m.FollowAll = followAll != 0
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatlet: read room %s's members: %w", channelID, err)
	}
	return out, nil
}

// readMessageTx reads one message of one room from inside a decision.
//
// THE ROOM IS PART OF THE PREDICATE: a message id names a message anywhere in
// the company, and a gesture on a message in another room would write a record
// whose subject is not the message's room, which this domain's ordering rests on.
func readMessageTx(ctx context.Context, tx *sql.Tx, channelID, messageID string) (
	Message, error) {

	if strings.TrimSpace(messageID) == "" {
		return Message{}, invalid("message_id", "a write names no message")
	}
	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM chat_messages WHERE id = ? AND channel_id = ?`,
		messageID, channelID).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Message{}, fmt.Errorf("%w: there's no such message in this room",
			ErrNotFound)
	case err != nil:
		return Message{}, fmt.Errorf("chatlet: read message %s: %w", messageID, err)
	}
	return DecodeMessage(document)
}

// threadPartiesTx is who has spoken in one thread.
//
// IT READS ONE MORE THAN THE CAP, so the routing can tell a thread that fits
// from one it cut: a read of exactly [MaxThreadParticipants] is full either way,
// and the record would report a complete wake set for a truncated thread.
func threadPartiesTx(ctx context.Context, tx *sql.Tx, channelID, root string) (
	[]string, error) {

	return handlesTx(ctx, tx, `
		SELECT handle FROM chat_thread_participants
		 WHERE channel_id = ? AND thread_root = ?
		 ORDER BY handle LIMIT ?`,
		channelID, root, MaxThreadParticipants+1)
}

// threadFollowersTx is who subscribed to one thread.
//
// BOUNDED BY [MaxRecipients] plus one, for [threadPartiesTx]'s reason: the follow
// arm has no cap of its own.
func threadFollowersTx(ctx context.Context, tx *sql.Tx, channelID, root string) (
	[]string, error) {

	return handlesTx(ctx, tx, `
		SELECT handle FROM chat_follows
		 WHERE channel_id = ? AND thread_root = ?
		 ORDER BY handle LIMIT ?`,
		channelID, root, MaxRecipients+1)
}

// handlesTx runs one handle query.
func handlesTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read the thread's handles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var handle string
		if err := rows.Scan(&handle); err != nil {
			return nil, fmt.Errorf("chatlet: scan a handle: %w", err)
		}
		out = append(out, handle)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatlet: read the thread's handles: %w", err)
	}
	return out, nil
}

// countChannelsTx is how many LIVE rooms the company holds.
//
// ARCHIVED ROOMS DO NOT COUNT: there is no channel delete, so counting them
// would leave a company at [MaxChannels] unable to create another, its refusal
// telling it to archive one to no effect. Archiving ends only new messages, the
// cost this cap bounds (a sidebar of live rooms and a routing surface). The cap
// is independent of search, which chunks its room list (`search.chatPostings`).
func countChannelsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var held int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_channels WHERE archived_at IS NULL`).
		Scan(&held); err != nil {
		return 0, fmt.Errorf("chatlet: count this company's live channels: %w", err)
	}
	return held, nil
}

// eraseableTx is which of these ids an erase may legitimately name: a message
// this room still holds, or one a previous erase already destroyed.
//
// THE SECOND HALF MAKES A RE-ISSUED GESTURE POSSIBLE: an erase that partly
// landed (acknowledgement lost, operation id swept) is re-issued by an operator
// who cannot know which half took, and refusing it for an already-gone message
// would leave the rest readable for ever.
func eraseableTx(ctx context.Context, tx *sql.Tx, channelID string, ids []string) (
	map[string]bool, error) {

	found := make(map[string]bool, len(ids))
	// Two index lookups, at most [MaxEraseMessages] bind parameters each,
	// well inside any driver's variable limit; a join would hand the
	// planner a predicate over a union.
	for _, prefix := range []string{
		`SELECT id FROM chat_messages WHERE channel_id = ? AND id IN (`,
		`SELECT message_id FROM chat_deletions WHERE channel_id = ? AND message_id IN (`,
	} {
		args := make([]any, 0, len(ids)+1)
		args = append(args, channelID)
		for _, id := range ids {
			args = append(args, id)
		}
		query := prefix + strings.Repeat("?,", len(ids)-1) + "?)"
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("chatlet: read what an erase of room %s would "+
				"remove: %w", channelID, err)
		}
		for rows.Next() {
			var id string
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("chatlet: scan an erase candidate: %w", err)
			}
			found[id] = true
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("chatlet: read what an erase of room %s would "+
				"remove: %w", channelID, err)
		}
	}
	return found, nil
}

// ---- the small shared rules -------------------------------------------- //

// reachable refuses a write into a room the actor cannot even read: a private
// room ([privateRoom]) admits only members, which are seats, so never the
// engine ([AuthorSystem]) whatever seat shares its name; a public or unit room
// admits anybody. An outsider is told the room is not there ([notFound]): its
// existence is itself information. Edit, delete, erase, prune and retention have their own
// authority; a moderator's delete and the destructions ([erases]) reach past
// membership.
//
// AN UNKNOWN KIND IS REFUSED, as [Visible] does: [privateRoom] reads a newer
// peer's kind as "not private", and defaulting an authorization to yes cannot be
// walked back (only a rolling upgrade brings such a room here).
func reachable(room Channel, members []Member, actor Actor) error {
	if !room.Kind.Valid() {
		return unknownKind(room)
	}
	if !privateRoom(room.Kind) {
		return nil
	}
	for _, m := range members {
		if m.Handle == actor.Name() && actor.Kind != AuthorSystem {
			return nil
		}
	}
	return notFound()
}

// postable is [reachable] for a post or a reply: the engine's line
// ([AuthorSystem]) is said in any room, a private one included, since it tells
// the room what befell a conversation there (internal/engine's
// conversationnotice.go) and only the engine writes as it. Its every other
// gesture is held to [reachable], as anybody's.
func postable(room Channel, members []Member, actor Actor) error {
	if actor.Kind == AuthorSystem && room.Kind.Valid() {
		return nil
	}
	return reachable(room, members, actor)
}

// reachableTx is [reachable] where the caller has not read the membership; it
// reads nothing for a room that refuses nobody.
//
// THE UNKNOWN-KIND REFUSAL IS REPEATED HERE: the membership read is skipped for
// every room [privateRoom] calls public, an unknown kind included, so
// delegating would return nil on precisely the rooms the guard exists for.
func (s *Store) reachableTx(ctx context.Context, tx *sql.Tx, room Channel, actor Actor) error {
	if !room.Kind.Valid() {
		return unknownKind(room)
	}
	if !privateRoom(room.Kind) {
		return nil
	}
	members, err := readMembersTx(ctx, tx, room.ID)
	if err != nil {
		return err
	}
	return reachable(room, members, actor)
}

// unknownKind is the one spelling of that refusal for the two guards above.
//
// IT NAMES THE UPGRADE, not the room: the gesture and actor are fine, and what
// is missing is this node's knowledge of the room's kind ([ErrUnknownKind], not
// [ErrForbidden]). It NEVER SAYS THE ROOM'S NAME: this node cannot tell who may
// read the room, so a name said here could reach somebody holding only its id.
func unknownKind(room Channel) error {
	return fmt.Errorf("%w: a newer node made this room a %q room, a kind this "+
		"node doesn't know, so it can't tell who may read it. Upgrade this node. "+
		"Until then it serves and takes nothing in that room", ErrUnknownKind,
		room.Kind)
}

// applyPatch moves a room's document by a patch, so a caller's answer is what
// the apply will produce. A NIL FIELD IS UNCHANGED: a pointer is the only shape
// telling "set to empty" from "leave alone".
func applyPatch(room *Channel, patch ChannelPatch, at time.Time) {
	if patch.Topic != nil {
		room.Topic = *patch.Topic
	}
	if patch.Purpose != nil {
		room.Purpose = *patch.Purpose
	}
	if patch.AutoJoin != nil {
		room.AutoJoin = *patch.AutoJoin
	}
	if patch.Archived != nil {
		if *patch.Archived {
			if room.ArchivedAt == nil {
				// THE WRITER'S CLOCK, IN THIS ANSWER ONLY: the row's instant
				// is the BROKER'S (byte-identical copies). This one shows the
				// caller an archived room and lets the no-op comparison see
				// something moved.
				archived := at
				room.ArchivedAt = &archived
			}
		} else {
			room.ArchivedAt = nil
		}
	}
}

// sameRoom reports a patch that moved nothing.
//
// IT COMPARES THE FIELDS A PATCH CAN MOVE, not the whole document: the rest is
// immutable (id, kind, name, creator) or another record's ([MemberSet.Unit],
// [RetentionSet]), and an unrelated field must not decide whether a topic
// change is a no-op.
func sameRoom(before, after Channel) bool {
	return before.Topic == after.Topic && before.Purpose == after.Purpose &&
		(before.ArchivedAt == nil) == (after.ArchivedAt == nil) &&
		before.AutoJoin == after.AutoJoin
}

// withMember adds one member if the set does not already name them.
func withMember(members []Member, add Member) []Member {
	for _, m := range members {
		if m.Handle == add.Handle {
			return sortedMembers(members)
		}
	}
	return sortedMembers(append(slices.Clone(members), add))
}

// withoutMember removes one handle.
func withoutMember(members []Member, handle string) []Member {
	out := make([]Member, 0, len(members))
	for _, m := range members {
		if m.Handle != handle {
			out = append(out, m)
		}
	}
	return out
}

// sortedMembers puts a membership in handle order, the applier's order. NOT
// COSMETIC: a collection in record order lands differently on every node whose
// writer built it from a map, and the replicated file's checksum diverges.
func sortedMembers(members []Member) []Member {
	out := slices.Clone(members)
	slices.SortFunc(out, func(x, y Member) int { return strings.Compare(x.Handle, y.Handle) })
	return out
}

// cleanHandles trims, drops empties and de-duplicates, keeping the caller's
// order (for mentions, the order the mention cap keeps).
func cleanHandles(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
