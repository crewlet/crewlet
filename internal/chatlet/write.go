package chatlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WRITE PATH: one record per caller gesture, published on the subject of
// the thing that gesture contends for.
//
// # The authority, and every clause of it
//
//	Take ONE snapshot of this node's own rows. Decide and form the
//	expectation INSIDE it. Publish. Let the broker arbitrate. Never guess.
//
// That is [statelog.Publisher]'s rule rather than this package's, and what it
// buys here is that a node which is behind cannot corrupt a conversation — it
// can only fail to write. Every write below answers with the framework's three
// outcomes unchanged: applied, pending, unknown. There is no bool anywhere on
// this path, because "the record is durable and this node has not applied it"
// is a third fact with a third thing to do about it.
//
// # Which subject, and why this domain has two answers
//
// A ROOM'S OWN STATE ARBITRATES. A topic, a purpose, an archive, a membership
// set, a retention prune and a compliance erase are all decided against the
// room's last record on its own subject, so two writers changing one room
// contend at the broker and exactly one wins.
//
// MESSAGES ARE SEQUENCED. A post, an edit, a deletion and a reaction carry NO
// expectation at all: they ride the room's message subject, which arbitrates
// nothing, because two people talking in one room are not racing for anything,
// and a rejection there would be a message somebody typed and lost — the one
// failure a chat system may not have. What the decide below cannot see — a
// record ahead of this one that archived the room, took the author out, took
// the message back or filled its reactions — the applier decides at the
// record's position, identically on every data node, and [Store.decided] reads
// what it decided before the gesture answers; what orders two edits of one
// message is the log, which every data node applies in one order.
//
// A CREATE TAKES EITHER, decided by what it is making: a NAMED room arbitrates
// on its address, create-only, so two people typing `#launch` fight and one is
// told the name is taken; a DIRECT conversation has no name to fight over, so
// it arbitrates on its own DERIVED id and the loser is told the room is
// already there — which is the right answer, because it is.
//
// # What is resolved inside the decide, and why none of it is looked up later
//
// The recipients and the mentions a record carries are resolved HERE, inside
// the snapshot's own transaction, against this node's applied rows and the
// live roster — never at wake time. The node that wins a feed message may be
// one whose applier has not reached the post, so a parser that read the room's
// membership instead would route from a room it has not seen, or stall the
// feed until it had. The same argument settles the unit's lead: who leads a
// unit is a fact about the EPOCH, and a lead resolved later would route a
// message posted under one org chart to whoever leads under the next.
//
// # What an archive closes, and what it leaves open
//
// AN ARCHIVED ROOM TAKES NO NEW MESSAGES — a post and a reply are refused
// ([ErrArchived]) — and that is the whole of what it closes. What was already
// said there stays its authors' and stays moderated: an author may still edit
// or take back their own words, a moderator may still take a message down,
// and a reaction is still taken. Each of those is attributed and shows as what
// it is — an edit says so, a delete leaves a tombstone naming who — so the
// archive keeps an honest record rather than a frozen one; and freezing them
// would make moderating a closed room an un-archive, which reopens it to every
// post the archive was there to stop. Its settings take a patch for the same
// reason ([Store.PatchChannel]): a topic is for whoever reads the room after.
//
// A ROOM THIS BUILD CANNOT CLASSIFY TAKES NOTHING, an edit and a delete
// included ([unknownKind]). Authorship does not admit one and neither does a
// moderator's, because both are answers about a room whose readership this
// build cannot state.
//
// # Who decides what, and what this store decides itself
//
// WHO MAY MAKE A GESTURE AT ALL is the authority table's, decided on the node
// the caller asked before anything reaches here: whether this principal holds
// chat, whether a room's archive is theirs to make, whether a delete of
// somebody else's words is moderation they are admitted to, whether a
// retention, an erase or a prune is an admin's. This store holds no grant and
// no chart, so what crosses to it is the DECISION, as a value — the
// moderation of a delete ([MessageAuthority]), the grounds an archive was
// admitted on ([ChannelAuthority]) — and it is judged here against the rows in
// the decide's own snapshot, which is the one place a stored author or a
// room's current unit can be read in the transaction the record is decided in.
//
// WHICH ROOM is the domain's own, and stays here: a room the actor cannot read
// refuses a write into it ([reachable]), in that same snapshot, because
// membership is a replicated row and the write authority's one rule is that a
// decision and its expectation come from one read of them.
//
// # What this build does NOT write, stated so nobody looks for it
//
// A RENAME. A channel's name is the address its create arbitrated on, and
// moving it is a second claim plus a release of the first on a record whose
// subject is the NEW name — which needs an op, a payload and an applier arm
// this build's vocabulary does not have ([ChannelPatch] says the same). The
// schema's `scoped_through` column is where such a record would stamp the
// room, and it stays zero until one exists.
//
// A FOLLOW GESTURE. There is no op that subscribes somebody to a thread or
// unsubscribes them, and `chat_follows` is not empty for it: BEING NAMED is
// what subscribes you, and the applier derives the row from the thread's own
// mentions — written when a post or an edit names you there, taken back when
// the last message in the thread naming you stops (an edit that drops the
// name, a delete, an erase). The routing below reads those rows and never
// writes one, which is the same division every derived table in this domain
// has — participation and subscription are facts about the messages, not
// claims a record makes.

// Store is the company's chat as a write authority.
type Store struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read for the two answers no decision of
	// this call's formed: a direct conversation that was already open
	// ([Store.OpenDirect]), and a gesture another copy of its operation
	// landed ([Store.answer]). It is never the write path's own snapshot —
	// that is the framework's, taken per append — and nothing read through
	// it is paired with an expectation.
	db store.ReplicatedReader

	// roster is the company as it is RIGHT NOW: who it still employs, who
	// is a person, and who leads a unit. None of it is derivable from the
	// log, which is exactly why it is a seam.
	roster Roster

	now func() time.Time

	// newOpID mints the operation id for a gesture no key derives one for
	// ([Store.operation]) — an actor carrying no [Actor.OpKey], whose
	// gesture nothing will repeat. A NAMED room's id is its create's
	// operation's ([roomIDOf]) either way, carried on the record and the
	// creator's own: a create that lost its race must not have written a
	// room under an id the winner also chose. A direct conversation's id is
	// the opposite ([DirectChannelID]), and that difference is exactly what
	// decides which subject each create arbitrates on.
	newOpID func() string

	// defaultPrivate resolves a create that states no visibility. See
	// [Options.DefaultPrivate].
	defaultPrivate bool
}

// Roster is what the write path asks about the company the rooms belong to.
//
// A CONSUMER-DEFINED SEAM of two methods, because two of them is all this
// package needs: the org chart is a large surface and a write path that could
// reach all of it would be one that could decide things the log has no record
// of.
//
// IT MUST NOT BLOCK. Both methods are called from INSIDE the decide's own read
// transaction, where the framework forbids anything that can wait — no broker
// call, no coordination read, no model. The roster is the epoch's own chart,
// held in memory; an implementation that went to the network here would hold
// this store's read transaction open across it.
type Roster interface {
	// Seat reports whether the company still has this seat, and whether
	// that seat is a PERSON.
	//
	// TWO ANSWERS IN ONE CALL because the routing needs both and they come
	// from one lookup: a handle the company no longer has is a wake
	// nothing can run, and a person is ADDRESSABLE but never woken — see
	// [Route].
	Seat(handle string) (exists, human bool)

	// Lead names the seat that answers for a unit's ROOM — the room named
	// room, recorded as unit's — or empty where nobody does. It is read for
	// one room kind and one reason: a person who posts in a unit's room and
	// names nobody is addressing whoever leads it.
	//
	// THE ROOM AS WELL AS ITS UNIT, because whose a unit room is belongs to
	// the CHART rather than to the row: a unit that stops declaring its
	// room, or leaves the chart, leaves a room the chart staffs with nobody,
	// and its lead must not go on being woken — and obliged to answer —
	// from a room no prompt names as theirs. The unit alone cannot say
	// that; the implementation answers it by the same rule that decides
	// who is in the room.
	Lead(unit, room string) string
}

// Options configure a store.
type Options struct {
	// Publisher is the domain's write authority, and Roster the company
	// a message's wakes are resolved against.
	//
	// DB is the replicated estate an answer is READ BACK from when no
	// decision of the call formed it — see [Store]'s own field. A decision
	// never reads it: every decision reads the snapshot the framework
	// takes per append, in the transaction its expectation is formed in.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader
	Roster    Roster

	// Now is the clock the AUTHORED instants are stamped from. Nil takes
	// the wall clock in UTC. An argument rather than a package call, so a
	// test can pin it — and so nothing on this path reads a clock the
	// applier is forbidden. Every instant that reaches a ROW is the
	// broker's; this one reaches the record's envelope and nothing else.
	Now func() time.Time

	// DefaultPrivate is what a create that states NO visibility resolves
	// to: true makes such a room private, false public.
	//
	// A PLAIN BOOL converted at the engine's edge, so this package's
	// vocabulary needs no dependency on config. Its zero value is the documented default rather than
	// "unset", which is what the config field's own doc says: channels are
	// public unless somebody says otherwise.
	DefaultPrivate bool
}

// NewStore builds the company's chat over its own log.
//
// THREE SEAMS ARE REQUIRED AND EACH IS REFUSED BY NAME, because the failure of
// a missing one is silent in a different way: with no publisher a write
// validates a message and then writes it nowhere; with no store a retried post
// or an already-open conversation is answered with nothing read back; and with
// no roster every wake goes to whoever the record named, people and departed
// seats included, which is a company answering on its own people's behalf.
//
// THE NODE GATE IS NOT BUILT HERE. An eviction wakes nobody and reads no
// roster, so its writer is [NewGateWriter], built from the publisher alone.
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

// The sentinels. Each is a different thing for the caller to do, which is why
// they are separate: a name somebody else holds is a name to negotiate, a
// conversation that already exists is one to open, an archived room is a room
// to reopen, a message carrying all the emoji it may is one to take a reaction
// off first, a refusal is a permission to ask for, and a conflict is a read to
// redo.
var (
	// ErrNotFound reports a room or a message that is not there.
	ErrNotFound = errors.New("chatlet: no such record")

	// ErrNameTaken reports a channel name the company already holds.
	ErrNameTaken = errors.New("chatlet: another room already has that name")

	// ErrRoomExists reports a room whose own id is already claimed, which
	// is a DIRECT conversation's create and nothing else: a named room's
	// create contends for its address, and answering "that room exists"
	// for one would name an object nobody asked about.
	ErrRoomExists = errors.New("chatlet: that conversation is already open")

	// ErrConflict reports a write that lost its race too many times.
	ErrConflict = errors.New("chatlet: the room kept changing under this write")

	// ErrArchived reports a room that takes no new messages: a post or a
	// reply. An edit, a delete and a reaction are still taken there — see
	// the head of this file for why an archive closes nothing else.
	ErrArchived = errors.New("chatlet: this room is archived")

	// ErrReactionsFull reports a reaction that would put a
	// [MaxReactionEmoji] + 1st distinct emoji on a message. Decided by the
	// applier at the record's position ([VerdictReactionsFull]), because a
	// toggle cannot see the set it joins.
	//
	// THE MESSAGE'S STATE, not the argument, as [ErrArchived] is the room's:
	// the same emoji lands the moment somebody takes one off, and only then.
	// It was a field refusal of `emoji`, which told a caller to send another
	// emoji — the one call that is refused the same way.
	ErrReactionsFull = errors.New("chatlet: this message has all the emoji it can")

	// ErrForbidden reports a gesture this actor may not make.
	ErrForbidden = errors.New("chatlet: that is not this actor's to do")

	// ErrUnknownKind reports a room whose kind this build does not know —
	// a newer node in the fleet created it — so this node cannot say who
	// may reach it and serves nothing in it.
	//
	// NOT [ErrForbidden], and the difference is what each tells the
	// caller. Forbidden is a fact about this ACTOR: asking again is
	// refused the same way, and the remedy is a permission. This is a
	// fact about this NODE: the gesture is well formed, the actor may well
	// be entitled to it, and the remedy is this node's upgrade — after
	// which the same call lands. Classed as a permission, it told a caller
	// to stop asking for something that was about to work.
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
// ONE PHRASE FOR BOTH REFUSALS that say it, an archive refused and a post into
// an archived room, because they were two and each left somebody out: neither
// named the lead of a team above, and they called the same person an admin in
// one and an administrator in the other.
const archivers = "whoever created it, the lead of its team or of a team " +
	"above it, or an admin"

// reopenRoom is who changes an archived room, said at the write and at the
// verdict alike ([archivers]).
const reopenRoom = "Only " + archivers + " can reopen it"

// Actor is who is making a write.
//
// IT TRAVELS PER CALL rather than on the store, because one node's chat serves
// every seat and every person through one write path.
type Actor struct {
	// Handle is the seat this write acts as — an agent's, or the human seat
	// a person holds (ADR-0032) — and the name it is recorded under.
	//
	// A ROOM IS SPOKEN IN AS A SEAT, and only as one: a credential acting as
	// itself has no seat to speak as, so [AuthorOperator] writes nothing
	// said in a room ([Actor.speaks]). The destructions and the retention
	// setting are the exception ([Store.Erase], [Store.Prune],
	// [Store.SetRetention]): none is said in a room's voice, so none needs a
	// speaker, and an admin credential acting as itself makes them under
	// its own name — as the tracker's purge is made.
	Handle string

	Kind       AuthorKind
	OperatorID string

	// TurnID is the RUN that made this write — provenance, so an audit can
	// walk from a message back to the execution that said it (ADR-0017).
	// It derives nothing: what makes a re-run turn post the same message
	// rather than a second one is the gesture's [Actor.OpKey], derived from
	// the turn's unit of work, and an id seeded from the run would be a
	// second copy on every redelivery.
	TurnID string

	Chain []string

	// OpKey is the caller's own operation key: the value a RETRY of this
	// same gesture reproduces, which every gesture here derives its
	// operation id from when it is set ([Store.operation]).
	//
	// ON THE ACTOR because it travels with the call and not with the store,
	// for the same reason the actor does — and because it is how the wiki
	// takes one (`doclet.Actor.OpKey`), so the two domains a person and a
	// seat write through take a retry key the same way. EMPTY MINTS A FRESH
	// ID PER CALL, which is right for every caller that will never retry
	// the same gesture — and wrong for the one that must: a post whose
	// answer was `unknown` can only be retried safely under the SAME id,
	// because the operation ledger is what recognises a second arrival, and
	// a fresh one would say it twice.
	//
	// AN OPERATION ID THE ENGINE MINTED, held to [statelog.CheckCallerOpID]
	// by every surface that takes one and by [Actor.validate], which refuses
	// an actor carrying any other: the ids derived from it carry its instant
	// — see [statelog.KeyedOpID].
	OpKey string
}

// Name is how this actor is recorded and rendered.
//
// THE HANDLE, ALWAYS. There is deliberately no "operator:<token>" spelling
// here: a message's author is a seat, a person writes as the seat they hold,
// and a room where a token could appear as a speaker is one where "who said
// this" has two vocabularies.
func (a Actor) Name() string { return strings.TrimSpace(a.Handle) }

// IsHuman reports an actor the routing treats as a person: a human seat, which
// is the kind every person writes as.
func (a Actor) IsHuman() bool { return a.Kind == AuthorHuman }

// validate refuses an actor that cannot be recorded at all.
func (a Actor) validate() error {
	if !a.Kind.Valid() {
		return invalid("actor.kind", "%q is not one of %v", a.Kind, AuthorKinds())
	}
	// A KEY IS AN OPERATION ID THE ENGINE MINTED, or no key — the wiki's
	// rule ([Actor.OpKey]). Every id a gesture derives from it carries the
	// key's own instant, and a key outside the grammar carries none — read
	// as minted at the epoch, which is before every loss the ledger will
	// ever have, so once it had swept anything each such gesture would be
	// answered `unknown` without being published, on every attempt, for
	// ever. Refused here, at the one boundary every write passes, with the
	// sentence a caller can act on.
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
// A ROOM RENDERS A SPEAKER BY SEAT. A person is a member of a room, is
// mentioned, is woken and reads it as the seat they hold, so a line authored
// by a credential's login is a speaker nobody can reply to, mention or find in
// the room's membership — and a token that could join a private room as
// itself would read it as nobody the room's people know is there. A pipeline
// that should post is given a human seat and its credential bound to it, and
// then posts as that seat.
//
// EVERY GESTURE SAID IN A ROOM ASKS THIS, after [Actor.validate]: a post, a
// reply, an edit, a delete, a reaction, a room's create and settings, a
// membership. The authority table refuses such a principal first, naming why
// (an unseated principal holds no chat seat); this is the backstop for a
// caller that reached the store some other way.
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
// # An unvouched answer carries the operation and nothing else
//
// Where the framework answered `unknown` because this node's operation ledger
// cannot vouch for the operation ([statelog.Result.Unvouched]), every field a
// decision fills — the room, the message, the revision — is ZERO, whichever arm
// produced the answer. There are two, and neither describes a record that
// exists: a refusal taken on rows that may already hold the operation's own
// first application, and a decision the framework then declined to publish,
// which formed a complete message out of the request and appended nothing. A
// caller cannot tell the two apart and must not have to, because the true
// answer to both is the same — this node cannot say which remark, room or
// membership the operation left, if any. Reporting the decision's would name a
// message echoed off the call, for a record that was never appended. What is
// left is enough to act on: [Written.ChangeID] names the operation to finish on
// a node whose ledger can answer it, and the outcome says why that is the
// remedy. [answered] is the one place every gesture's answer passes through.
type Written struct {
	// Channel is the room this write was about, as the decision read it —
	// and, for a create, as the record wrote it. Zero on an unvouched
	// answer, like every field a decision fills.
	Channel Channel

	// Message is what a message gesture authored, and the zero value for a
	// room gesture.
	//
	// ITS CreatedAt IS ZERO on a message this call decided, deliberately. A
	// message's instant is the BROKER'S, which is what makes one node's copy
	// of a conversation byte-identical to another's — and it does not exist
	// yet when this answer is formed. A caller that needs it reads the
	// message back rather than being handed this node's own clock dressed
	// as it. A retry answered as another copy of its operation IS read back
	// ([Store.answer]), and carries the broker's instant like any read.
	//
	// ZERO ON ANY UNVOUCHED ANSWER ([statelog.Result.Unvouched]), whether
	// the decision refused or ran to completion and was never published:
	// this node cannot say which remark, if any, exists, and the message a
	// decision formed is the request's own words rather than a record's.
	// Under an `unknown` this node COULD vouch for — an acknowledgement
	// that was lost — it is this call's own decision, the one that may
	// have landed.
	Message Message

	// Revision is the log revision the room's row is AT after this write,
	// in the one number space every reader here answers in — see
	// [ChannelRevision]. A record that landed produces it from its own
	// position, which is exactly what the applier stamps into the row; a
	// decision that published nothing produces it from the row its own
	// snapshot read.
	//
	// NEVER A LITERAL ZERO on a write that succeeded: zero is the one
	// value a caller cannot act on, being both "this node has applied
	// nothing here" and "nobody answered" — which is why an unvouched
	// answer carries exactly that, rather than the revision of rows that
	// may already hold the operation's first application.
	Revision uint64

	// ChangeID is the operation id, and for a message it is the message's
	// own id — see the head of ids.go for why the two are one value.
	ChangeID string

	// Outcome is the framework's three-valued answer, carried unchanged —
	// except that a message gesture's `applied` becomes `pending` where this
	// node's applier consumed the record without deciding it (it holds it
	// retained), because what the record did is not known here yet.
	Outcome statelog.Result

	// Verdict is what this node's applier decided about a message gesture's
	// record ([Verdict]), read back once it applied it: applied, or the
	// refusal the error beside it states. EMPTY on a room gesture, and on a
	// message gesture this node has not applied — `pending` or `unknown`,
	// where the record may yet be refused.
	Verdict Verdict
}

// answered is a write's answer, settled against its outcome: everything the
// decision filled, except where this node's ledger cannot vouch for the
// operation — then the operation id and the outcome alone. See [Written].
//
// ONE FUNCTION RATHER THAN A CHECK PER GESTURE, because the rule is a property
// of the outcome and not of the gesture: every gesture's decision fills the
// answer before the framework says whether it may stand, and a gesture that
// forgot the check would report a room or a message on exactly the answer
// that says nobody here can name one.
func answered(w Written) Written {
	if w.Outcome.Unvouched {
		return Written{ChangeID: w.ChangeID, Outcome: w.Outcome}
	}
	return w
}

// ChannelRevision is the SQL expression a room's own log revision is read
// with, written ONCE because every reader compares against it and none may
// disagree.
//
// MAX(version, scoped_through) RATHER THAN version, on the rule the schema
// states: a record that writes this row FROM ANOTHER SUBJECT stamps
// `scoped_through` and never `version`, so the row's broker expectation still
// matches its own subject's last message and cannot be poisoned into permanent
// unwritability.
//
// EVERY MESSAGE IS SUCH A RECORD HERE, which is what makes this expression
// load-bearing on the hottest path rather than a provision for some future
// rename: a post arbitrates on the room's MESSAGE subject and moves the room's
// own high-water mark, so it stamps `scoped_through`. A number taken from
// `version` alone would not move when somebody spoke, and a caller waiting to
// see its own message in the room would wait for ever.
const ChannelRevision = `MAX(version, scoped_through)`

// ---- the two channel-state gestures that make a room ------------------- //

// NewChannel is a room to create.
type NewChannel struct {
	// Name is the address, and it is what the create arbitrates on. It is
	// normalised here and checked against [ValidName].
	Name string

	// Kind is one of the NAMED kinds. A direct conversation is opened with
	// [Store.OpenDirect] instead, because its create takes the other
	// arbitration discipline entirely.
	Kind Kind

	Topic   string
	Purpose string

	// Unit is the unit whose room this is, required for [KindUnit] and
	// refused for every other kind.
	Unit string

	// Members is the founding membership beyond the author, who is always
	// in the room they made — unless the author is the engine
	// ([AuthorSystem]), which narrates a room and is never in one.
	Members []Member

	// THERE IS NO RETENTION HERE. A room is made keeping what the company
	// keeps, and a shorter or longer horizon is its own gesture
	// ([Store.SetRetention]) — an admin's, because a shorter one is a prune
	// on a delay, and one a create could carry is a destruction anybody
	// making a room could schedule.

	// ActivatedAt is the instant the configuration a [KindUnit] room comes
	// from was activated, which the room is stamped with
	// ([Channel.ChartEpoch]) — required for that kind ([ErrNoActivation])
	// and refused for every other, whose membership is nobody's chart.
	ActivatedAt time.Time
}

// CreateChannel makes a named room.
//
// ONE RECORD, ARBITRATED ON THE ADDRESS. The name is what two writers contend
// for — two fresh uuids never would — and its apply writes the claim, the
// room and its founding membership in one transaction, so there is no window
// in which a name is held by a room that was never written.
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
	// AN UNSTATED VISIBILITY IS THE COMPANY'S TO DECIDE, and this is the
	// only place that decision is made. Every caller that omits the kind —
	// the operator catalogue's `create_channel`, from the dashboard or a
	// person's own assistant — arrives here with the empty value, so
	// resolving it at this one point is what makes
	// `chat.chatlet.default_channel_private` mean the same thing on every
	// surface that offers a create.
	//
	// RESOLVED BEFORE THE GATE BELOW, not inside it: the gate's subject is
	// "is this a kind a NAMED room can have", and an unresolved empty
	// string is not an answer to that question. It used to reach the gate
	// and be refused, which is why the config field had no reader at all —
	// there was no way to ask for the default.
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
	// A UNIT'S ROOM IS MADE BY THE ORG CHART AND BY NOTHING ELSE, because
	// its membership has exactly one writer ([Store.SetUnitMembers]) and
	// that writer is the engine's reconcile of the units that DECLARE a
	// channel. A unit room anybody else made is one Join, Leave and
	// SetMembers all refuse and no reconcile maintains — its founding
	// membership frozen for the life of the deployment, under a kind that
	// promises the chart keeps it.
	if kind == KindUnit && actor.Kind != AuthorSystem {
		return Written{}, fmt.Errorf("%w: a %s room is the org chart's — the "+
			"engine opens one for a unit that declares a `channel` and keeps "+
			"its membership from the chart, and one made any other way is a "+
			"room whose membership nobody could ever change. Declare the "+
			"channel on the unit in the company document, or make a %s or %s "+
			"room", ErrForbidden, KindUnit, KindPublic, KindPrivate)
	}

	// THE CHART'S ROOM IS STAMPED WITH THE CHART'S ACTIVATION, and no other
	// room is: see [Store.SetUnitMembers] for what the stamp guards.
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
	// OVER THE ADDRESS AND WHAT THE CREATE ASKS FOR, the kind as the caller
	// stated it: two creates of one address under one key asking the same
	// room are a repetition, and the same key asking another room under that
	// address is another operation — which the address then refuses as taken.
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
	// THE AUTHOR IS A MEMBER, unless it is the ENGINE: a unit's room is
	// made by the org chart, under [AuthorSystem]'s name, and that name is
	// no seat. Added, it sat in every unit room as a member nobody could
	// address, until the next apply's membership write — which states the
	// chart's set — took it back out, one record per room.
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
			// THE COMPANY'S ROOM COUNT IS CHECKED HERE AND NOWHERE
			// ELSE, because nothing in a record can count the
			// company's rooms: the cap is a fact about the estate,
			// so the only place it can be refused is inside a
			// snapshot of it.
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
		// THE ROOM THAT HAS IT is often the one meant, and it is named
		// by the name the creator typed: the subject the create lost on
		// is a digest of it. ANOTHER NAME LEADS, because it always
		// works: names are one namespace for the whole company, so the
		// room holding this one may be a private room the creator is
		// not in and cannot open.
		return Written{}, fmt.Errorf("%w: #%s is already another room's name, "+
			"so pick another one (or open #%s if you're in it)", ErrNameTaken,
			name, name)
	}
	if err != nil {
		return Written{}, err
	}
	// NO ROW EXISTED TO READ, so there is no prior revision to fall back
	// on — and a create that changed nothing is not a shape this path has:
	// it either takes the address or is refused as taken.
	return s.answer(ctx, room.ID, "", Written{
		Channel: room, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	})
}

// OpenDirect opens the conversation between these participants, and reports
// whether it had to be created.
//
// THE SECOND VALUE IS WHETHER ANYTHING WAS WRITTEN, on
// `doclet.Store.EnsureContainer`'s terms: a seat that sends a direct message
// opens the conversation on every send, so the ordinary outcome is that the
// room is already there — and a caller that could not tell that from a create
// would log a new conversation every time somebody replied.
//
// FALSE BESIDE AN UNVOUCHED ANSWER, like every other field of one ([Written]):
// the framework publishes no decision its ledger cannot vouch for, so nothing
// this node can claim was written — and the `unknown` beside it, not this
// value, is what tells that apart from a conversation already open.
//
// # Why the refusal IS the answer
//
// The id is DERIVED from the participants ([DirectChannelID]), so two sides
// opening one conversation from two nodes publish the same create on the same
// subject and exactly one wins. The loser is told the object already exists,
// which is not a failure: it is the room, and this reads it back rather than
// handing a caller a refusal it would have to interpret. One path serves both
// cases, which is why there is no pre-read here — a pre-read would be a second
// path that the race takes anyway.
//
// THE KEY DERIVES THE OPERATION, never the room: the room is its participants'
// whatever the call, and the key ([Actor.OpKey]) is what makes the same call
// made again one create rather than a second one refused as already there.
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
	// NOTHING BUT THE ROOM: its id IS the participant set, so it is the
	// whole of what an open asks for.
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
			// NOTHING IS READ HERE. A direct conversation's whole
			// state is its participants, which the caller named and
			// the id was derived from, so there is no row this
			// decision could disagree with — and the one row that
			// matters, the room's own, is the guard the framework
			// checks in this same snapshot.
			return decide(stamp, actor, subject, OpCreate, scope, opID, payload, nil, at)
		},
	})
	switch {
	case errors.Is(err, ErrRoomExists):
		held, revision, readErr := s.room(ctx, id)
		if readErr != nil {
			return Written{}, false, readErr
		}
		// THE OUTCOME IS SPELLED OUT HERE, which is the one place in
		// this package that does it, because the framework's own no-op
		// arm cannot reach this case: the guard refuses a create BEFORE
		// a decision's emptiness is consulted, so "the room is already
		// there" arrives as a refusal rather than as nothing to
		// publish. It is APPLIED — the conversation exists and this
		// node has the row in hand, which is exactly what applied means
		// — and the version is that row's own, because no record landed
		// to produce a position. Leaving the zero value instead would
		// answer with an outcome that is none of the three, and a
		// caller reading it could not tell a conversation that is
		// already open from a write nothing could be established about.
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
	// operation is what wrote the conversation, so this call made it — and
	// [Store.answer] reads the room back, since this call's own was never
	// stored.
	written, err := s.answer(ctx, id, "", Written{
		Channel: room, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	})
	return written, !result.Unvouched, err
}

// ---- the room's own state ---------------------------------------------- //

// PatchChannel changes a room's settings.
//
// IT TAKES THE RECORD'S OWN PAYLOAD rather than a second struct that maps onto
// it one for one: every field a caller may change is a field of
// [ChannelPatch], and two shapes for one set of settings is a place for them
// to drift. The version is this build's and is set here, never by the caller.
//
// A PATCH THAT CHANGES NOTHING PUBLISHES NOTHING and is reported as applied.
// The alternative is a record on the log and a live frame for a change nobody
// made — which is what [ChannelPatch.Validate] refuses an empty patch for, one
// step earlier.
//
// AN ARCHIVE IS NOT ANY MEMBER'S. A topic and a purpose are for whoever reads
// the room after; closing the room to everybody in it, or opening it again,
// is its creator's, its unit's lead's or an admin's — decided by the caller
// against the room it read, and carried here as [ChannelAuthority]. A patch
// that moves the archive with no admission for it is refused, and one admitted
// on a unit the room is no longer recorded under is refused as a conflict.
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
	// THE PATCH IS WHAT IT ASKS; the authority it is made under is who is
	// asking rather than what, so it is not part of the operation.
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
			// BEFORE THE NO-OP, never after it: the no-op answers
			// success, and a success decided on grounds the room no
			// longer stands on is the answer this refusal withholds.
			if patch.Archived != nil {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := authority.archives(room); err != nil {
					return statelog.Decision{}, err
				}
			}
			// THE ROOM IS THE ANSWER EITHER WAY, and it is taken
			// BEFORE the no-op return: a caller told its write
			// landed reads the room out of this, and an idempotent
			// patch that answered with a zero value would report
			// success on a room with no id and no kind.
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
// A VALUE RATHER THAN A BOOL for `doclet.CommentAuthority`'s reason, and it
// states the DECISION: the caller asks the room's archive relation of the
// authority table (`chat.channel.archive`: the room's creator, the lead of
// the unit a unit's room is recorded under or of any unit above it, or an
// admin) and passes what came back, because this package holds no chart and
// no grants.
type ChannelAuthority struct {
	// Archive reports a caller admitted to archive this room or reopen it.
	// THE ZERO VALUE ADMITS NEITHER, so a caller that never asked cannot
	// move a room's archive by forgetting to say so.
	Archive bool

	// ArchiveUnit is the unit the room was recorded under when its archive
	// was decided — the one the lead relation was asked about — and empty
	// for a room no unit holds.
	//
	// A PRECONDITION, NOT A LABEL, on the tracker's re-route terms: the
	// room the caller decided on was read outside this write's snapshot, and
	// the org chart can re-home a unit's room to another unit between that
	// read and this one ([MemberSet.Unit]), so a lead of the unit it LEFT
	// would land an archive on the unit that now holds it. Checked in the
	// decide's own snapshot and refused as a conflict, because nothing is
	// wrong with the request: the caller reads the room again, decides
	// again on the unit it is in now, and re-sends.
	ArchiveUnit string
}

// archives refuses an archive this authority does not admit on room as the
// decide's own snapshot holds it.
//
// THE ROOM'S NEW UNIT IS NOT NAMED: a unit's key is minted by a write, never
// typed by the person reading this, and "another team" is the whole of what
// changes their next move.
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
// A DELTA CANNOT REBUILD A ROW ON A REPLAY FROM ZERO, and membership is where
// that matters most: it is what a private room's readability IS, so a node
// that replayed a room's adds and missed one of its removals would serve a
// conversation to somebody who was taken out of it.
func (s *Store) SetMembers(ctx context.Context, actor Actor, channelID string,
	members []Member) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	opID, opErr := s.operation(actor, "members", channelID, sortedMembers(members))
	if opErr != nil {
		return Written{}, opErr
	}
	return s.members(ctx, actor, channelID, opID, func(room Channel, _ []Member) ([]Member, error) {
		// A UNIT'S ROOM IS THE CHART'S, and this is the gesture that
		// could overwrite it wholesale. It is refused here rather than
		// filtered, because a caller who names a set is stating the
		// WHOLE membership — there is no half of it this store could
		// honour without inventing which half the caller meant.
		//
		// The engine's own reconcile does not come through here: it
		// holds the store directly and calls [Store.SetUnitMembers],
		// which is the one writer this refusal makes room for.
		if room.Kind == KindUnit {
			return nil, fmt.Errorf("%w: %s is a unit's own room, and the "+
				"org chart decides who's in it. A change here would be undone "+
				"the next time the company's configuration is applied. Change "+
				"the unit in the company document instead",
				ErrForbidden, room.called())
		}
		return members, nil
	})
}

// Join puts the actor in a room.
//
// A SEAT ALREADY IN THE ROOM IS TOLD IT IS IN THE ROOM, whatever its kind: the
// set it would publish is the set the room holds, which is the no-op every
// membership write answers ([Store.setMembership]). Asked after the kinds
// below, a member re-joining their own private room was told only someone
// already in it could add them, and a seat the chart put in its unit's room
// that the chart decides who is in it.
//
// THREE KINDS REFUSE EVERYBODY ELSE, each for its own reason: a private room's
// membership is the only way in, and somebody outside it is answered as
// though it were not there, by the rule every write into it takes
// ([reachable]); a direct conversation's membership IS its identity, so adding
// somebody does not widen the room, it names a different one; and a unit's
// room is the ORG CHART'S, which is the rule [Store.Leave] already stated from
// the other side.
//
// THE UNIT REFUSAL IS THE SAME SENTENCE AS LEAVE'S, and it has to be. Leave
// refuses because "leaving it would be undone by the next apply, with nothing
// to say so" — and a join is undone by exactly the same apply, for exactly the
// same reason. Refusing one and admitting the other left a gesture that
// appears to work and silently reverts, which is the failure Leave's refusal
// exists to prevent.
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
// A UNIT'S ROOM REFUSES IT, which looks like a restriction and is the opposite:
// that membership is the org chart's own, maintained by the epoch reconcile, so
// a leave there is undone by the next apply — and a gesture that silently
// reverts is worse than one that is refused, because nothing reports it.
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
// activated at activatedAt.
//
// THE SECOND VALUE IS WHAT WAS WRITTEN ([MembershipWrite]), on
// `doclet.Store.EnsureContainer`'s terms and one step past them: this runs on
// every node at every boot and every apply, so the ordinary outcome is that
// the room already holds what the chart says — and a caller that could not
// tell that from a write would log a changed membership on every restart of a
// company nobody edited. A bool could not carry the third answer, a RE-STAMP:
// every activation moves every unit room's stamp on the node that wins it,
// re-activating an unchanged revision (the credential-rotation gesture)
// included, and a caller told only "written" logged a membership change per
// room for a set nobody moved.
//
// THE ONE WRITER [Store.SetMembers] AND [Store.Join] REFUSE FOR. Those two
// refuse a unit room because anything they wrote would be undone by the next
// apply; this is the thing that would undo it, so it is the only caller whose
// write survives. Keeping it a separate method rather than a flag on
// SetMembers is what makes that readable at the call site: the engine's
// reconcile names this, and nothing else can reach it by passing an argument.
// And it is [AuthorSystem]'s alone, the engine narrating the room — as the
// create of a unit room is ([Store.CreateChannel]) — because a seat or a
// token that could call it would be choosing a membership the chart decides.
//
// IT TAKES THE WHOLE SET, like SetMembers, because the chart states the whole
// membership: a reconcile that added without removing would leave a seat in
// its old team's room for ever after it moved, and one that diffed here would
// be deciding from rows it read in another transaction.
//
// # And the unit, which the chart may MOVE
//
// unit is the key of the unit the chart staffs this room for — the one that
// declares its channel at this activation — and a room recorded under another
// key is RE-HOMED to it by the same record ([MemberSet.Unit]): the channel
// moved to the unit that replaced its team in a reorg, or the unit was renamed
// without an `id`, or it gained an id (the room is then recorded under the
// spelling that survives a later rename). Without it a declared channel could
// never change hands: its new declarer was refused the room for good, and the
// room — no longer declared by the unit it was recorded under — was emptied.
// Nothing about who may READ the room moves with it: a unit room is readable
// by every seat of the company, so a re-home discloses nothing, and a room a
// PERSON opened is never one this takes ([KindUnit] alone).
//
// # Stamped with the activation, and never walked back
//
// The membership is the CONFIGURATION's, and every node applies one
// configuration separately — at its reconcile tick and again at every boot.
// So the room carries the activation its membership came from
// ([Channel.ChartEpoch]), and a write whose activation is OLDER than the
// room's decides nothing — its unit included: a node that boots on a revision
// the fleet has since replaced leaves the newer membership and the newer
// owner alone, where it used to write its own chart's back over them. A write
// at the SAME activation with the same set and unit decides nothing either,
// which is what makes the call free after the first node; one at the same
// activation that DIFFERS is written, because that is a room an equal-epoch
// race left wrong. A LATER activation over an unchanged set and unit is
// written too — a re-stamp, which the applier records without touching the
// members or announcing a change — because a room left at the older stamp is
// open to any activation between the two. A zero instant is refused
// ([ErrNoActivation]), and so is an empty unit ([ErrInvalid]): a room the
// chart staffs is staffed for some unit, and a room it staffs with nobody is
// [Store.ReleaseUnitRoom]'s. These are [doclet.Store.EnsureContainer]'s rules,
// and [worklet.Writer.ApplyChart]'s, for the same projection into a third
// domain.
//
// A FRESH OPERATION PER CALL, on the reasoning those two give: this is a
// reconcile decided from the row, so a second call — on another node, at the
// next boot, after a lost acknowledgement — finds the chart's set there, and N
// nodes racing one activation are settled by the broker's arbitration on the
// room. Minted as a fresh one rather than derived from the set, so nothing
// reading the call could take the id for a function of the membership.
//
// NO SPECIAL PATH THROUGH THE GUARDS. It goes through [Store.setMembership]
// exactly as every other membership write does, and needs no bypass: a unit
// room is not private ([privateRoom]), so the reachability gate admits an
// actor that is not in it, and the refusals above live in their own callers'
// closures rather than in the shared path.
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
// THE ROOM'S UNIT STAYS WHERE IT IS, and that is why this is not
// [Store.SetUnitMembers] with an empty set: a caller naming the unit would be
// naming the one it READ, in another transaction, and a room another node had
// re-homed in between would be written back under the old key. Here the
// record moves no unit at all ([MemberSet.Unit] empty), so whoever the room is
// recorded under when it is decided is who it stays recorded under — and
// declaring the channel again, on that unit or another, staffs it again.
//
// Every other rule is [Store.SetUnitMembers']: the chart's alone, stamped with
// the activation, an older one deciding nothing, the same one over an empty
// room deciding nothing.
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
		func(room Channel, _ []Member) ([]Member, error) {
			if room.Kind != KindUnit {
				return nil, fmt.Errorf("%w: %s isn't a unit's own room, and the "+
					"org chart decides who's in a unit's room only. A change "+
					"here would take over a room someone else owns", ErrForbidden,
					room.called())
			}
			return members, nil
		})
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
// FOUR ANSWERS, because the chart's reconcile logs exactly one of them: a
// write that moved nobody must not read as a changed membership, a re-stamp
// moves nobody, and a room that changed hands is news whoever moved with it.
// Only the chart's writes ([Store.SetUnitMembers], [Store.ReleaseUnitRoom])
// can produce a re-stamp or a re-home — they are the writers that carry an
// activation and a unit.
type MembershipWrite string

const (
	// MembershipUnchanged is a call that published nothing — the room
	// already held the set (at this activation or a later one, for the
	// chart) — or one whose answer this node cannot vouch for.
	MembershipUnchanged MembershipWrite = "unchanged"

	// MembershipRestamped is a newer activation written over the set the
	// room already held: the stamp moved and nobody did.
	MembershipRestamped MembershipWrite = "restamped"

	// MembershipMoved is a write that changed who is in the room.
	MembershipMoved MembershipWrite = "moved"

	// MembershipRehomed is a write that recorded the room under another
	// unit ([MemberSet.Unit]), with whatever membership that unit brings —
	// which may be the same seats, for a unit that was only renamed.
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
// with no activation, answered without its second value, which only the
// chart's reconcile reads.
func (s *Store) members(ctx context.Context, actor Actor, channelID, opID string,
	next func(Channel, []Member) ([]Member, error)) (Written, error) {

	written, _, err := s.setMembership(ctx, actor, channelID, opID, chartWrite{}, next)
	return written, err
}

// setMembership is the shared shape of every membership write: read the room
// and its current set inside one snapshot, let the caller say what the set
// becomes, and publish only if it moved — under the operation the caller
// derived from its own verb and key, since a join and a leave under one key
// are two — and report what it published ([MembershipWrite]).
//
// chart is what a CHART write carries — its activation and the unit it
// staffs the room for — and the zero value for every other one; see
// [Store.SetUnitMembers] for what each decides.
func (s *Store) setMembership(ctx context.Context, actor Actor, channelID, opID string,
	chart chartWrite, next func(Channel, []Member) ([]Member, error)) (Written, MembershipWrite, error) {

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
			// RESET ON EVERY ROUND. A round that decided to write and
			// lost the broker's arbitration is followed by one that
			// finds the winner's set already there, and only the last
			// round says what this call did.
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
				// A DIRECT CONVERSATION'S IDENTITY IS ITS
				// PARTICIPANT SET. Its id was derived from exactly
				// these handles, so adding one does not widen this
				// room — it names a different room, which
				// [Store.OpenDirect] is how you reach.
				//
				// AFTER [reachable], and only for somebody in it:
				// anybody can derive a conversation's id from two
				// handles, so refused before it, this sentence
				// told whoever asked that those two talk to each
				// other, where the reader answers that it is not
				// there.
				return statelog.Decision{}, fmt.Errorf("%w: a direct "+
					"conversation is the people in it, so changing who's in it "+
					"would make it a different conversation. Open one with the "+
					"people you want instead", ErrForbidden)
			}
			set, err := next(room, held)
			if err != nil {
				return statelog.Decision{}, err
			}
			// THE EMPTY LIST, NEVER NULL: emptying a room is a real
			// gesture and [MemberSet] says it is spelled by a record
			// that carries the empty list — a caller handing over
			// nobody as a nil slice must not publish "members": null.
			set = sortedMembers(set)
			if set == nil {
				set = []Member{}
			}
			// A RE-HOME IS DECIDED HERE, against the unit this
			// snapshot holds, and never against the one the caller
			// read: the record carries a unit only when it moves one.
			rehome := ""
			if chart.unit != "" && chart.unit != room.Unit {
				rehome = chart.unit
			}
			switch {
			case chart.epoch != 0 && room.ChartEpoch > chart.epoch:
				// A LATER CONFIGURATION ALREADY WON. Two nodes
				// applying two revisions is ordinary during a
				// rollout, and the newer one must not be walked
				// back by the older node's own apply arriving
				// second — its owner no more than its members.
				return statelog.Decision{}, nil
			case slices.Equal(set, held) && rehome == "" && room.ChartEpoch >= chart.epoch:
				// AN UNCHANGED MEMBERSHIP IS A NO-OP, not a
				// record: a seat that joins a room it is already
				// in should be told it is in the room, and a
				// record per such call is a log that grows with
				// retries rather than with decisions. For the
				// chart that is a room already at this activation;
				// one at an OLDER one is re-stamped below.
				return statelog.Decision{}, nil
			}
			// THE SAME SET AT A NEWER ACTIVATION IS A RE-STAMP, which
			// the applier records without touching a member or
			// announcing a change — and which the caller must not report
			// as a membership somebody changed. A room that changed
			// hands is a re-home whoever moved with it.
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
					Unit: rehome}, nil, at)
		},
	})
	if err != nil {
		return Written{}, MembershipUnchanged, err
	}
	written, err := s.answer(ctx, channelID, "", Written{
		Channel: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	})
	// NOTHING THIS NODE CAN VOUCH FOR WAS WRITTEN on an unvouched answer, like
	// every other field of one ([Written]): the outcome beside it is what
	// says the write may have landed.
	if result.Unvouched {
		wrote = MembershipUnchanged
	}
	return written, wrote, err
}

// Erase removes message rows permanently — an admin redacting what should
// never have been said.
//
// ARBITRATED ON THE ROOM, because the room is its blast radius: an erase
// racing an archive has a defined order rather than two. And it is a PERSON'S
// or a CREDENTIAL'S gesture — who among them may make one is the authority
// table's, decided before this is reached — refused HERE, in the decide, to a
// seat and to the engine, as well as by the payload: an author that could
// erase its own messages could erase the evidence of what it did, and a rule
// enforced only at whichever route happened to be the way in is a rule the
// next route does not have.
//
// IT REACHES PAST MEMBERSHIP without reading: a credential pasted into a
// private room is destroyed by somebody who is not in it and is never shown
// what else the room said.
//
// THE OPERATION IS THE CALLER'S, handed over WHOLE rather than derived from
// an [Actor.OpKey], exactly as a purge's is: an erase that answers `unknown`
// may or may not have destroyed what it named, so the only correct next step
// is the same operation again — and the caller that holds it is the one that
// brings it back, bound to the room and to [EraseSet] of what it names. See
// the head of ids.go for whose rule holds it.
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
			// NOT REACHABILITY — an erase reaches past membership — but the
			// unknown-kind refusal every gesture in a room shares
			// ([unknownKind]): this build serves nothing in such a room,
			// so it takes nothing in it either.
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			// EVERY NAMED MESSAGE MUST BE IN THIS ROOM, checked in
			// the same snapshot. An id from another room removes
			// nothing while the gesture is answered `applied` and
			// audited as an erase of it — a redaction the audit
			// trail records that did not happen. An id already
			// erased passes: the marker is what says so, and
			// re-issuing a gesture that partly landed must not be
			// refused.
			known, err := eraseableTx(ctx, tx, room.ID, ids)
			if err != nil {
				return statelog.Decision{}, err
			}
			for _, id := range ids {
				if !known[id] {
					// THE MESSAGE BY THE ID THE CALLER NAMED, which is the
					// one way to say which of its list is missing; the room
					// as a person knows it.
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
// A RECORD RATHER THAN A LOCAL SWEEP, and that is the whole reason the gesture
// exists: "older than a year" evaluated against each node's own clock deletes a
// different set on every node, for ever, and nothing would ever report it. A
// cutoff on the log is one number every applier compares identically against
// the broker's own stored timestamps.
//
// IT IS THE RETENTION DUTY'S GESTURE, A PERSON'S OR A CREDENTIAL'S — who among
// the last two may is the authority table's. A seat that could prune a room
// could delete a year of the company's decisions as a turn's side effect, and
// no wake and no inverse would bring it back. It reaches past membership for
// [Store.Erase]'s reason.
//
// THE OPERATION IS THE CALLER'S, handed over whole, for [Store.Erase]'s
// reason: the duty mints one per record it publishes and the human surface's
// route takes the one a retry brings back, bound to the room and the cutoff.
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
			// The unknown-kind refusal, for [Store.Erase]'s reason.
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
// ITS OWN GESTURE, NOT A SETTING ON A PATCH. A shorter horizon is a prune on a
// delay — the retention duty publishes it ([Prune]) — so a room's retention is
// a destruction somebody schedules, and who may schedule one is an admin's to
// decide, on a surface that can ask a fresh proof of who they are. As a field
// of a room's settings it was any member's: anybody who could reach a room
// could have the duty destroy a year of it, with no admin, no confirmation and
// no step-up, beside an erase that asked all three.
//
// A PERSON'S OR A CREDENTIAL'S, like [Store.Erase] — and reaching past
// membership for its reason: a room's horizon is set by somebody who is never
// shown what the room says.
//
// KEYED LIKE EVERY OTHER GESTURE ([Actor.OpKey]), and a setting that matches
// the room's publishes nothing and is reported as applied.
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
			// The unknown-kind refusal, for [Store.Erase]'s reason — and
			// BEFORE the no-op, so a room this build cannot classify is
			// never answered as though its horizon were already set.
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
// credential acting as itself. Never a seat — a turn's side effect must not be
// a year of a room — and never the engine, whose one destruction is the
// retention duty's prune, which [Store.Prune] admits by name.
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

	// Links are what the message points at. There are no attachments — see
	// the package doc — so this is the whole of what a message carries
	// besides its own prose. Each is an absolute http, https or mailto
	// address, stored as sent ([LinkSchemes] says why).
	Links []string

	// Mentions are the handles the body named, already resolved from prose
	// to seats by whichever surface read the body. The decide drops the
	// ones the company no longer has, so a mention of somebody who left is
	// not a wake and not a row.
	Mentions []string

	// Collective says the message addressed the whole room (`@channel`).
	Collective bool

	// WHAT MAKES A POST IDEMPOTENT is the actor's key ([Actor.OpKey]): a
	// re-run turn posts once, and so does a person's retried request. It
	// derives the MESSAGE'S id, which is also its operation's — see the head
	// of ids.go — over the room, the message answered and the words; a turn
	// that says two things derives two keys, so a remark made again after a
	// different one is a second message rather than the first one's retry.
}

// Validate refuses a message for what it SAYS — nothing at all, a body or a
// set of links or mentions over its cap, a link that is not an address
// ([LinkSchemes]) — which no room and no row can make acceptable.
//
// EXPORTED because a gesture that writes something BEFORE the post has to ask
// it first. send_dm opens the conversation and then says the message in it,
// and a message refused only once the open had landed left a room in every
// participant's list holding nothing, opened by a refusal. What only the
// post's own snapshot can decide — the room archived, the actor not in it —
// is still the post's; this is the half that needs no snapshot, and [Store.Post]
// and [Store.Reply] run the same check, so the two can never disagree.
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
// A THREAD IS ONE LEVEL DEEP. A reply to a reply carries the same ROOT, which
// is what makes "this thread" a range scan rather than a recursive walk — so
// the root is resolved here, from the message being answered, rather than
// taken from a caller who may have the reply's id in hand.
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
	// THE PURE-VALUE CAPS ARE REFUSED BEFORE A SNAPSHOT IS TAKEN. Nothing
	// about a body's length depends on a row, and a refusal that cost a
	// read transaction would make the cheapest mistake the most expensive
	// one. The payload's own Validate runs again inside the decide, which
	// is where the collections the decision itself builds are checked.
	if err := in.Validate(); err != nil {
		return Written{}, err
	}
	mentions := cleanHandles(in.Mentions)
	verb := "post"
	if replyTo != "" {
		verb = "reply"
	}

	at := s.now()
	// THE MESSAGE ID IS THE OPERATION ID. One message is one operation,
	// and two identifiers for one thing are two places for a retry to
	// disagree with itself — see the head of ids.go. OVER WHAT THE CALLER
	// ASKED FOR: the room, the message it answers as the caller named it,
	// and the words — never the thread root this decision resolves, which
	// a retry could find moved.
	id, err := s.messageOperation(actor, verb, channelID, messageAsk{
		ReplyTo: strings.TrimSpace(replyTo), Body: strings.TrimSpace(in.Body),
		Links: in.Links,
	})
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
		// SEQUENCED: no expectation at all. Two people talking in one
		// room are not racing for anything, and paying for arbitration
		// would serialise the hottest subject in the company behind
		// itself — a rejection there is a message somebody typed and
		// lost. What the snapshot below cannot see — a record ahead of
		// this one that archives the room or takes the author out — the
		// applier decides at its position, and [Store.decided] reads
		// what it decided before this call answers.
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE ROOM IS READ BEFORE ANYTHING ELSE, and not only
			// for the routing: the applier REFUSES a post into a
			// room it has no row for, because under a strict replay
			// the room's create is below this position on the same
			// log — so publishing one would not fail this caller, it
			// would stall every node's applier on this domain.
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			room, _, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = room
			if room.ArchivedAt != nil {
				// EVERY WORD OF IT IS STILL READABLE; it takes nothing
				// new until somebody reopens it, and who may is the
				// archive's own relation (authz's chat.channel.archive).
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrArchived,
					archivedRoom)
			}
			held, err := readMembersTx(ctx, tx, room.ID)
			if err != nil {
				return statelog.Decision{}, err
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := reachable(room, held, actor); err != nil {
				return statelog.Decision{}, err
			}
			root, rootAuthor, err := s.threadOf(ctx, tx, room.ID, replyTo)
			if err != nil {
				return statelog.Decision{}, err
			}
			named := s.employed(mentions)
			payload := MessagePost{
				V: DocumentVersion, MessageID: id, Body: in.Body,
				ThreadRoot: root, Mentions: named,
				Collective: in.Collective, Links: in.Links,
				Author: actor.Name(), AuthorKind: actor.Kind,
				// THE RULE [reachable] JUST APPLIED, carried for the
				// applier to apply again at the record's position.
				MembersOnly: privateRoom(room.Kind),
			}
			message = Message{
				V: DocumentVersion, ID: id, ChannelID: room.ID,
				ThreadRoot: root, Author: payload.Author,
				AuthorKind: payload.AuthorKind, Body: in.Body,
				Links: in.Links, Mentions: named,
				Collective: in.Collective,
			}
			notify, err := s.notifyOf(ctx, tx, actor, room, message, Routing{
				ChannelKind: room.Kind, AuthorKind: actor.Kind,
				Members: held, Mentions: named, Collective: in.Collective,
				ThreadRoot: root, RootAuthor: rootAuthor,
			})
			if err != nil {
				return statelog.Decision{}, err
			}
			return decide(stamp, actor, subject, OpPost, scope, opID, payload, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return s.decided(ctx, OpPost, channelID, id, Written{
		Channel: out, Message: message, Revision: writtenRevision(result, 0),
		ChangeID: opID, Outcome: result,
	})
}

// answer is a gesture's answer: [answered]'s, and — where the framework
// answered the call as ANOTHER COPY of its operation
// ([statelog.Result.Collapsed]) — the room and, for a message gesture, the
// message as this node's own rows have them. Every gesture returns through it,
// so a gesture written next cannot forget either half.
//
// # Why a collapsed answer is read back
//
// THE ROWS, NOT THE REQUEST, AND NOT THIS CALL'S DECISION EITHER. A collapsed
// answer is one of two things. The ledger answered it before any decision ran
// ([statelog.Snap.Held]), so this call filled in nothing at all — no room, no
// message — and an answer built from it would name a room with no id; or the
// broker's duplicate window collapsed this append onto an earlier copy, so this
// call DID decide, from rows and a roster that may have moved since the copy
// that landed: a mention the roster has dropped, a thread root, a room's
// settings, and an instant that was never stored. In both, what exists is what
// the first record wrote. A key derives the operation from what the call ASKED
// for ([Store.operation]), so the words agree — everything resolved inside the
// decision may not.
//
// A ROW THIS NODE DOES NOT HOLD answers with the identity the caller already
// holds and nothing else, and that is true wherever it arises: the copy landed
// and this node has not applied it yet — the outcome is `pending`, and a read
// once it applies answers the rest — or an erase or a prune removed the
// message since. Neither is a failure; the identity is the whole of what this
// node can truthfully say about a row it does not hold.
//
// ANY OTHER READ FAILURE IS RETURNED, wrapped and beside the identity, rather
// than swallowed into a room or a message with nothing in it — which a caller
// would render as a remark nobody made. The gesture is durable; it is the read
// back that failed, and the same call made again is answered from the ledger
// without writing anything.
func (s *Store) answer(ctx context.Context, channelID, messageID string,
	w Written) (Written, error) {

	w = answered(w)
	if !w.Outcome.Collapsed {
		// THIS CALL'S OWN ANSWER: the decision that landed, the one
		// still pending, or — under an `unknown` whose acknowledgement
		// was lost — the one that may have. NOT under an unvouched
		// `unknown` ([statelog.Result.Unvouched]), which [answered] has
		// already emptied: whether the decision refused or ran, nothing
		// of it was appended.
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
				said = got
			}
		}
		// ASSIGNED ONLY ONCE BOTH READS ARE IN: the store re-runs this
		// body on a conflicted transaction, and a room kept from an
		// attempt whose message read failed would pair two snapshots.
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
// THE MENTIONS ARE RE-CARRIED, because the body is what they were derived
// from: an edit that added a mention and did not carry the new set would leave
// the row naming whoever the first draft named.
//
// THE LINKS ARE NOT DERIVED FROM THE BODY, so they are carried only when the
// edit changes them. Nil KEEPS the message's links; a non-nil list — empty
// included — replaces them. A plain slice could not tell the two apart, and
// every edit that fixed a typo without re-sending the links silently removed
// all of them: the gesture a person makes most often destroyed the part of
// the message they did not touch. The pointer is the shape [ChannelPatch]
// gives a field whose empty value is a setting.
//
// A RETRIED EDIT IS ONE RECORD under the actor's key ([Actor.OpKey]): the
// operation is derived over the message and the new words, so the same edit
// sent again is the same operation, and an edit back to an earlier draft,
// made under another key after a different one, is another.
type EditMessage struct {
	Body     string
	Links    *[]string
	Mentions []string
}

// Edit rewrites a message's body in place.
//
// ONLY THE AUTHOR, an admin included, and it is refused against the row read
// in the decide's own snapshot. A remark somebody else can rewrite is a remark
// attributed to a person who did not make it — on a row that outlives the
// thread and is quoted in every wake it produced. Removing somebody else's
// words is [Store.Delete] and destroying them is [Store.Erase]; both record who
// did it, which is exactly what an edit cannot.
//
// AN EDIT WAKES ONLY WHOM IT NEWLY NAMES. It is not a second message: raising
// the room's standing arms again would page a thread every time somebody fixed
// a typo, while a colleague the edit now names has been addressed by it and has
// heard nothing else about it.
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
	// THE OPERATION NAMES WHAT THE CALL SENT, and an edit that kept the links
	// sent none: it is derived over that fact (a nil list) rather than over
	// the links the decide will find, which this side of the snapshot cannot
	// read. A retry sends the same thing and is the same operation; an edit
	// that names its links is a different call and a different one.
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
		// SEQUENCED, for [Store.post]'s reason: a delete ahead of this
		// record that the snapshot below could not see is decided by
		// the applier, and read back before this call answers.
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, _, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = room
			// A ROOM THIS BUILD CANNOT CLASSIFY TAKES NOTHING, an edit
			// included — the one guard every gesture in a room shares
			// ([unknownKind]). Authorship is no substitute: the author of
			// a message in such a room is exactly somebody the room's
			// unknown rule admitted.
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			held, err := readMessageTx(ctx, tx, room.ID, messageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held.Author != actor.Name() {
				return statelog.Decision{}, fmt.Errorf("%w: this message was "+
					"written by %s, and only its author can edit it",
					ErrForbidden, held.Author)
			}
			if held.DeletedAt != nil {
				return statelog.Decision{}, fmt.Errorf("%w: this message was "+
					"taken back, so there's nothing left to edit", ErrNotFound)
			}
			// THE LINKS THE MESSAGE ENDS WITH, resolved against the row
			// in this snapshot, so the record carries them whole and
			// every node writes the same row.
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

			// A SYSTEM LINE'S EDIT WAKES NOBODY, for the reason its
			// post does: the engine narrating its own room is a
			// line to render rather than a question to answer.
			var notify *Notify
			if actor.Kind != AuthorSystem {
				// ONLY THE HANDLES THIS EDIT ADDS. Everyone
				// already named heard about the message when it
				// was posted.
				woken, truncated := ResolveMentions(Routing{
					ChannelKind: room.Kind, AuthorKind: actor.Kind,
					Members: members, Mentions: without(named, held.Mentions),
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
		Channel: out, Message: message, Revision: writtenRevision(result, 0),
		ChangeID: opID, Outcome: result,
	})
}

// emptiedByEdit is the refusal of an edit that would leave a message with
// nothing in it, wherever the edit is found to: before the snapshot when it
// clears both halves itself, inside it when it keeps links the row has none of.
func emptiedByEdit() error {
	return invalid("body", "an edit leaves the message with neither text nor "+
		"a link — emptying a message is a delete, which keeps the row so the "+
		"thread hung off it still has its root")
}

// Delete tombstones a message: the body goes, the row stays.
//
// THE ROW STAYS so a thread hung off this message still has its root and its
// replies stay reachable. Removing the row is [Store.Erase], which is a
// different gesture with a different author and a gate.
//
// THE AUTHOR OR A MODERATOR. Taking your own words back is a thing anybody may
// do, and taking somebody else's down is moderation — which the caller was
// admitted to or was not ([MessageAuthority]), decided by the authority table
// before this is reached, and judged here against the author the decide's own
// snapshot reads. It is never decided from the actor's KIND: who the caller is
// says nothing about whether they may take a colleague's words down.
func (s *Store) Delete(ctx context.Context, actor Actor, channelID,
	messageID string, authority MessageAuthority) (Written, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, err
	}
	at := s.now()
	// NOTHING BUT THE MESSAGE AND ITS ROOM: a deletion says nothing else, and
	// the authority it is made under is who is asking rather than what.
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
		// SEQUENCED, for [Store.post]'s reason: an erase or a prune
		// ahead of this record is decided by the applier.
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, _, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = room
			// NOT REACHABILITY — a delete reaches past membership, for
			// its author and for a moderator — but the unknown-kind
			// refusal every gesture shares: this build serves nothing in
			// such a room, so it takes nothing in it either.
			if !room.Kind.Valid() {
				return statelog.Decision{}, unknownKind(room)
			}
			held, err := readMessageTx(ctx, tx, room.ID, messageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held.Author != actor.Name() && !authority.Moderate {
				return statelog.Decision{}, fmt.Errorf("%w: this message was "+
					"written by %s. Only its author can take it back, and "+
					"removing someone else's words needs an admin",
					ErrForbidden, held.Author)
			}
			message = held
			message.DeletedBy, message.DeletedByKind = actor.Name(), actor.Kind
			// A DELETION WAKES NOBODY. There is nothing to read and
			// nothing to answer, and a wake here would tell a thread
			// to come and look at an absence.
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
		Channel: out, Message: message, Revision: writtenRevision(result, 0),
		ChangeID: opID, Outcome: result,
	})
}

// MessageAuthority is what a caller may do to a message somebody else wrote.
//
// A VALUE RATHER THAN A BOOL for `doclet.CommentAuthority`'s reason, and it
// states the DECISION: the caller asks the delete's authority row, which
// admits an author on the stored message and a moderator on ADMIN alone — not
// on any relation to the room — and passes what came back, because this
// package holds no grants.
type MessageAuthority struct {
	// Moderate reports a caller authorized to take down a message they did
	// not write. It reaches PAST MEMBERSHIP: a moderator takes down what was
	// said in a private room or a direct conversation they are not in, and
	// is never shown the rest of it.
	//
	// THERE IS NO EDIT EQUIVALENT, deliberately. Taking a message down
	// leaves a tombstone everyone can see; an EDIT rewrites it, so a message
	// somebody else can change is a remark attributed to a person who did
	// not make it — on a row that outlives the thread and is quoted in every
	// wake it produced.
	Moderate bool
}

// React puts one emoji on a message.
func (s *Store) React(ctx context.Context, actor Actor, channelID, messageID,
	emoji string) (Written, error) {

	return s.react(ctx, actor, channelID, messageID, emoji, false)
}

// Unreact takes one back.
//
// ITS OWN CALL OVER ONE RECORD SHAPE, because the record is a TOGGLE rather
// than a set: two people reacting to one message never overwrite each other,
// which a whole-set record on an additive subject could not promise.
func (s *Store) Unreact(ctx context.Context, actor Actor, channelID, messageID,
	emoji string) (Written, error) {

	return s.react(ctx, actor, channelID, messageID, emoji, true)
}

// react is both signs of the toggle.
//
// THE SIGN IS PART OF THE OPERATION — it is the verb — so a reaction added and
// taken back are two records, while the same gesture made again under its key
// is one. A reaction added, taken back and added again is three only because
// its caller makes three calls under three keys; collapsing on the content
// alone would leave such a reaction wherever the ledger's sweep left it.
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
		// THIS BUILD'S CAP, STAMPED, so every applier declines at it.
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
		// SEQUENCED, and the reaction is why the pattern exists here: a
		// toggle cannot see the set it joins, so whether it is the
		// message's thirty-third emoji is decided by the applier
		// ([MaxReactionEmoji]) and read back before this call answers.
		Pattern: statelog.PatternSequenced,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			room, _, err := readRoomTx(ctx, tx, channelID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = room
			// THE SHARED REACHABILITY RULE, which this gesture is
			// the only one that would otherwise skip. An edit and a
			// delete are gated on AUTHORSHIP, which is strictly
			// narrower for an outsider — nobody is the author of a
			// message in a room they were never in — but a reaction
			// has no such gate of its own, so without this one a
			// handle that knows a room id and a message id attaches
			// an attributable, durable row inside a private
			// conversation it cannot open.
			if err := s.reachableTx(ctx, tx, room, actor); err != nil {
				return statelog.Decision{}, err
			}
			if _, err := readMessageTx(ctx, tx, room.ID, messageID); err != nil {
				return statelog.Decision{}, err
			}
			// A MESSAGE THIS SNAPSHOT ALREADY HOLDS FULL is refused
			// before anything is appended, in the words the applier's
			// verdict is answered with ([VerdictReactionsFull]): the
			// cap is the applier's, and this only saves the append
			// whose refusal is already certain.
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
			// A REACTION NEVER WAKES ANYBODY and never discharges a
			// reply obligation: it carries no text a triage prompt
			// could classify, and if it counted as a delivery a seat
			// could answer every addressed message with a thumb.
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

// decided is a message gesture's answer once this node's applier has decided
// its record: [Store.answer]'s where the record landed, and the domain's own
// refusal where it did not.
//
// # Why a message gesture reads its verdict back
//
// A message record is SEQUENCED ([statelog.PatternSequenced]): the applier
// decides it at its position, from rows the decision above could not see — a
// record ahead of it that archived the room, took the author out, took the
// message back or filled its reactions. So `applied` from the framework says
// only that this node consumed the record, and the answer a caller acts on is
// the verdict: read here, inside the snapshot that holds the applier's rows,
// and answered with the sentinel the decision would have refused with had the
// record ahead been in its snapshot ([Verdict.refusal]) — so a surface answers
// both arms with one class.
//
// ONLY ON `applied`. Under `pending` this node has not applied the record and
// under `unknown` it may not exist, so neither has a verdict here yet, and both
// are answered as they always were: the record may yet be refused, which
// [Written.Verdict] being empty says. A retry the ledger answers
// ([statelog.Result.Collapsed]) reads its first copy's verdict the same way,
// by the operation, so a refused post retried is refused again rather than
// answered as said.
//
// FOUR ANSWERS THE VERDICT READ CAN GIVE besides a decision: a record this
// node holds retained — consumed and not applied, so the outcome is `pending`;
// one a gate dropped everywhere, which is this node's own eviction landing
// between its decision and its append; one whose verdict the horizon may have
// let go ([verdictLost]), which is `unknown` and [statelog.Result.Unvouched],
// the framework's own answer for an operation its ledger may have lost —
// whether the record landed or was refused, nothing here can say any more;
// and a read that failed, which is returned beside the identity like
// [Store.answer]'s, since the gesture is durable and the same call made again
// is answered without writing again.
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
		// RETAINED: consumed and not applied, so what it did is not
		// known on this node yet.
		w.Outcome.Outcome = statelog.OutcomePending
		return s.answer(ctx, channelID, messageID, w)
	case v == verdictLost:
		// UNKNOWN, AND UNVOUCHED: the record is on the log and was
		// decided, and the decision is no longer kept — so the answer
		// carries no position and no room, as an unvouched `unknown`
		// from the framework does ([answered]).
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
// THE THREE OUTCOMES TRAVEL UNCHANGED. What this adds is the one thing the
// framework cannot know: which of its refusals a caller can act on, and what
// each one means HERE. A write that ran out of compare-and-set rounds is A
// ROOM SOMEBODY KEPT CHANGING, and a refused create is one of two different
// facts — which is why the translation reads the subject's KIND rather than
// answering both with one sentinel. On an ADDRESS it is a name somebody else
// holds, and there is a conversation to have about what to call the room. On a
// ROOM it can only be a direct conversation, whose id is derived from its
// participants, and "that already exists" is not a problem at all: it is the
// conversation, which [Store.OpenDirect] reads back and returns.
//
// The conflict carries the framework's SENTINEL along with this package's, so
// a caller reasoning in either vocabulary matches — and not the framework's
// error, whose text names the subject the race was lost on, a broker path
// that is the room's id or a digest of its name: carried, every caller that
// printed the error showed it to whoever read the answer. The two create
// refusals carry neither: each is the whole of what happened, while the
// framework's wording for them names an object nobody asked about.
//
// ROOMS AND MESSAGES ONLY. Every subject this store publishes on is an address,
// a room or a room's messages, so a lost race here is always a room somebody
// kept changing; the node gate publishes on a NODE's subject and goes to the
// framework directly ([GateWriter]), because the same translation there would
// name a room after a node id.
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
			// THE NAME IS THE CREATE'S TO SAY ([Store.CreateChannel]):
			// this subject's id is a digest of it, which no reader can
			// place.
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
// EVERY PAYLOAD IS VALIDATED HERE, through the one method the [Payload]
// contract exists for: a cap enforced at each caller is a cap missing from
// whichever caller is written next.
//
// THE FRAMEWORK'S STAMP RIDES EVERY RECORD, both halves, and this is the one
// builder every gesture's decision goes through — so it is the one place the
// stamp can be kept or lost. The writer is what the eviction gate compares on
// every applier in the fleet, and the generation is what makes a record from
// before a reanchor comparable and safely stale; the publisher refuses a record
// whose envelope carries neither ([statelog.Stamp]).
//
// A FUNCTION RATHER THAN A METHOD, because two writers share it and it reads
// nothing either holds: the room store's gestures and the node gate's
// ([GateWriter]), which is built without the roster a room's routing needs.
func decide(stamp statelog.Stamp, actor Actor, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload Payload, notify *Notify, at time.Time) (statelog.Decision, error) {

	// THE OP RIDES THE SUBJECT KIND THE TABLE NAMES, and this is where the
	// table is consulted rather than written down beside the writers.
	// Every gesture picks its subject constructor itself, and the applier
	// dispatches on (kind, op) on its own — so a writer that put an op on
	// the wrong kind would publish without complaint and then fault the
	// domain on every node, at apply, over a pair nobody meant. Refused
	// here, before anything is encoded, it is one failed call on the node
	// that formed it.
	//
	// NOT [invalid]: nothing about the request could be changed to fix
	// it. It is this build's own writer disagreeing with this build's own
	// table, which is a bug to report rather than a refusal to show a
	// caller.
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
	// A RECORD THAT WAKES NOBODY BY CONSTRUCTION MUST NOT CARRY A
	// SNAPSHOT. The wake filter asks whether a record has a [Notify], so a
	// notification on a kind nothing routes would be a wake derived from a
	// record no feed reads — invisible, and impossible to tell from a bug
	// in the feed.
	if notify != nil && !subject.Kind.Routable() {
		return statelog.Decision{}, fmt.Errorf("chatlet: a %s record carries a "+
			"routing snapshot and no wake is ever derived from that kind — see "+
			"ObjectKind.Routable", subject.Kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("chatlet: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// NO VERSION: the encoder stamps the lowest one that reads
			// what this record carries — see [RecordVersion].
			OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Mutation:   body,
		Actor:      actor.Name(),
		ActorKind:  actor.Kind,
		OperatorID: actor.OperatorID,
		TurnID:     actor.TurnID,
		Chain:      actor.Chain,
		Notify:     notify,
	}
	// THE WHOLE RECORD'S SIZE IS THE FRAMEWORK'S TO REFUSE, against the
	// bound this domain declares on its stream
	// ([statelog.StreamSpec.MaxRecordBytes], [MaxRecordBytes]): a record past
	// it is answered `record_too_large` before anything is sent, the one
	// refusal every domain's oversized record meets. Every collection here is
	// capped so that nothing this build forms reaches it, which the record
	// tests hold at every cap.
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
// ONE RULE IN ONE PLACE, because every path here has both arms: a record that
// landed answers with ITS OWN composed position, which is exactly what the
// applier stamps into the row, so a caller can compare it against a later read
// and know whether its write is visible. A decision that published nothing
// answers with the revision the row was already at, read inside the same
// snapshot the decision was made in.
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
// A SYSTEM LINE WAKES NOBODY BY CONSTRUCTION, and it is nil rather than a
// snapshot with an empty recipient set — because nil is the shape the wake
// filter asks about, and a flag inside the payload is read after a
// version-gated decode that a newer build's record does not survive. The
// engine narrating its own room — somebody joined, the room was archived — is
// a line to render, not a question to answer.
//
// EVERYTHING ELSE CARRIES ONE EVEN WHEN IT WAKES NOBODY, because the snapshot
// is also what a card and a mention feed render: a message that named a
// colleague who happens to be a person still named them, and a record that
// carried no snapshot would render as though it had not.
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
		routing.ThreadParticipants, routing.Followers = parties, followers
	}
	if room.Kind == KindUnit && room.Unit != "" {
		routing.Lead = s.roster.Lead(room.Unit, room.Name)
	}

	candidates, truncated := Resolve(routing)
	return &Notify{
		MessageID: message.ID, ChannelID: room.ID, ChannelName: room.Name,
		ChannelKind: room.Kind, ThreadRoot: message.ThreadRoot,
		Author: message.Author, AuthorKind: message.AuthorKind,
		Excerpt: Excerpt(message.Body), Mentions: message.Mentions,
		Lead: routing.Lead,
		// ROUTED AT WRITE TIME, so the feed never has to subtract and
		// can never forget to. The parser routes again against the
		// roster IT has — a seat may have left between the commit and
		// the wake — through this same function, which is why the two
		// can disagree about nothing.
		Recipients:     RecipientsOf(Route(candidates, s.roster.Seat, actor.Name())),
		WakesTruncated: truncated,
	}, nil
}

// employed drops the handles the company no longer has.
//
// AT WRITE TIME, so the record's mention list is what a mention feed can
// actually render: a row in `chat_mentions` for somebody who left is a feed
// entry nobody will ever open, replicated to every node for the life of the
// message.
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

// threadOf resolves the thread a reply belongs to, and who started it.
//
// A REPLY TO A REPLY IS A REPLY TO THE THREAD. A thread here is one level deep
// by construction, so the root is read off the message being answered rather
// than taken from the caller — which is what keeps "this thread" a range scan
// and keeps the routing a property of the thread rather than of a walk.
func (s *Store) threadOf(ctx context.Context, tx *sql.Tx, channelID, replyTo string) (
	root, author string, err error) {

	if replyTo == "" {
		return "", "", nil
	}
	parent, err := readMessageTx(ctx, tx, channelID, replyTo)
	if err != nil {
		return "", "", err
	}
	if parent.ThreadRoot == "" {
		return parent.ID, parent.Author, nil
	}
	held, err := readMessageTx(ctx, tx, channelID, parent.ThreadRoot)
	if err != nil {
		return "", "", err
	}
	return held.ID, held.Author, nil
}

// without is the handles in `handles` that `drop` does not hold, in their
// order: what an edit names that the words before it did not, and — the other
// way round — what it stopped naming.
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
// THE REVISION TRAVELS WITH THE ROOM rather than being read again afterwards,
// because the two are one fact about one row: read separately they would come
// from two statements, and a decision that paired a room with a revision taken
// after a concurrent apply would report a number its own answer is not at.
//
// THROUGH THE DOCUMENT, although every field it holds is also a column: the
// document is what carries a NEWER BUILD'S fields through this node untouched,
// and a patch rebuilt from columns would drop them on the first edit a
// mid-upgrade fleet applied.
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
// The order is the applier's own ([replaceMembers] sorts by handle), so a set
// this decision compares against the one it forms is comparing two slices in
// one order rather than two sets through a map.
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
// THE ROOM IS PART OF THE PREDICATE rather than something checked afterwards:
// a message id names a message anywhere in the company, and a gesture that
// edited, deleted or reacted to a message in another room would write a record
// whose subject is not the room the message is in — which is the one thing
// this domain's ordering rests on.
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
// IT READS ONE MORE THAN THE CAP, which is what lets the routing tell a thread
// that fits from one it had to cut: a read limited to exactly
// [MaxThreadParticipants] comes back full in both cases, and the record would
// then report a complete wake set for a thread it had truncated.
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
// BOUNDED BY [MaxRecipients] plus one, for [threadPartiesTx]'s reason: the
// follow arm carries no cap of its own, so the total is what bounds it and the
// extra row is how the routing knows it bit.
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
// ARCHIVED ROOMS DO NOT COUNT, and that is what makes the refusal's own
// advice true. It counted every row, and there is no channel delete in this
// vocabulary at all — so a company that had ever created [MaxChannels] rooms
// could never create another, and the refusal told it to archive one, which
// changed nothing. A cap somebody cannot get under is not a cap; it is a
// permanent stop with a remedy that reads like an oversight.
//
// AN ARCHIVED ROOM IS STILL READ, still searched and still counted by
// retention. What archiving ends is new messages, which is exactly the cost
// this cap is about: a sidebar of live rooms, and a routing surface somebody
// has to keep in their head.
//
// THE OTHER HALF OF THE OLD COUPLING IS GONE. This cap used to be load-bearing
// for chat SEARCH, because a query named one bound variable per visible room
// and the statement compiler refused past its own limit — so raising this cap
// would have broken search rather than the sidebar. `search.chatPostings`
// chunks that list now, and this constant answers only the question it is
// named for.
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
// THE SECOND HALF IS WHAT MAKES A RE-ISSUED GESTURE POSSIBLE. An erase that
// partly landed — its acknowledgement lost, its operation id swept — is
// re-issued by an operator who has no way to know which half took, and refusing
// it because a message is already gone would leave the rest readable for ever.
func eraseableTx(ctx context.Context, tx *sql.Tx, channelID string, ids []string) (
	map[string]bool, error) {

	found := make(map[string]bool, len(ids))
	// TWO STATEMENTS, at most [MaxEraseMessages] bind parameters each,
	// which is two orders of magnitude inside any driver's variable limit.
	// A join would read the same two tables and hand the planner a
	// predicate over a union; these are two index lookups.
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

// reachable refuses a write into a room the actor cannot even read.
//
// ONE RULE FOR EVERY GESTURE WITH NO NARROWER GATE OF ITS OWN, and it is the
// schema's own: a room is private when its transcript is reachable only
// through its membership ([privateRoom]), so a non-member writing there would
// be posting into a conversation they cannot open. A public or a unit room
// refuses nobody, which is what makes a unit's room the place work addressed
// to nobody in particular lands.
//
// SOMEBODY OUTSIDE A PRIVATE ROOM IS TOLD IT IS NOT THERE ([notFound]), the
// answer the reader gives them, and never that they may not: a private room's
// existence is itself information, and a direct conversation's id is derived
// from the two people in it, so a refusal that said "members only" — the room's
// name included — told anybody holding an id, or two handles, who talks to whom
// and what the room is called.
//
// FOUR GESTURES DO NOT TAKE IT, because each has an authority of its own for
// which membership is the wrong question — and they are not all narrower.
//
// An EDIT refuses anybody but the message's own AUTHOR, and a DELETE by its
// author is the same rule; both are strictly narrower, since nobody is the
// author of a message in a room they were never in. What they deliberately
// still allow is somebody taking their own words back out of a room they have
// since left.
//
// A DELETE BY A MODERATOR reaches PAST membership, deliberately. Taking
// somebody else's words down is moderation ([Store.Delete]), which the
// authority table admits an admin to ([MessageAuthority]), so a moderator may
// tombstone a message in a private room or a direct conversation they are not
// in and cannot read. An ERASE, a PRUNE and a room's RETENTION are destructions
// behind a gate of their own: they refuse a seat ([erases], and a prune also
// admits the retention duty), and they reach past membership for the same
// reason — each destroys or schedules the destruction of what a room said
// without showing it to whoever made it.
//
// AN UNKNOWN KIND IS REFUSED, which is the SAME answer [Visible] gives a
// reader and has to be: [privateRoom] is a two-valued question asked of an
// open enum, so a kind a newer peer wrote falls out of it as "not private" —
// and defaulting an AUTHORIZATION to yes because the rule is unknown is the
// one direction of this that cannot be walked back. The write path can never
// mint one ([Store.CreateChannel] refuses a kind that is not [Kind.Named] and
// [Store.OpenDirect] one that is not [Kind.Direct]), so the only way a room
// gets here is a rolling upgrade: a newer node created it and this node
// applied the record rather than dropping it. Refusing costs a gesture that
// waits for this node's upgrade; allowing writes into a room whose readership
// this build cannot state.
func reachable(room Channel, members []Member, actor Actor) error {
	if !room.Kind.Valid() {
		return unknownKind(room)
	}
	if !privateRoom(room.Kind) {
		return nil
	}
	for _, m := range members {
		if m.Handle == actor.Name() {
			return nil
		}
	}
	return notFound()
}

// reachableTx is [reachable] where the caller has not already read the
// membership. It reads nothing for a room that refuses nobody, which is the
// common case and the reason this is a function rather than an unconditional
// read at the top of every decide.
//
// THE UNKNOWN-KIND REFUSAL IS REPEATED HERE rather than left to [reachable],
// because the membership read below it is skipped for every room [privateRoom]
// calls public — and an unknown kind is one of those, so delegating would
// return nil on precisely the rooms the guard exists for.
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

// unknownKind is the one spelling of that refusal, so the two guards above
// cannot come to say it differently.
//
// IT NAMES THE UPGRADE rather than the room, because nothing the caller holds
// is wrong: the gesture is well formed and the actor is entitled to it, and
// what is missing is this node's knowledge of what the room IS — which is why
// it is [ErrUnknownKind] and not [ErrForbidden].
//
// AND IT NEVER SAYS THE ROOM'S NAME: this node cannot tell who may read the
// room, so it cannot tell whether the caller may know what it is called either,
// and a name said here would reach somebody holding only the room's id.
func unknownKind(room Channel) error {
	return fmt.Errorf("%w: a newer node made this room a %q room, a kind this "+
		"node doesn't know, so it can't tell who may read it. Upgrade this node. "+
		"Until then it serves and takes nothing in that room", ErrUnknownKind,
		room.Kind)
}

// applyPatch moves a room's document by a patch, so a caller's answer is what
// the apply will produce.
//
// A NIL FIELD IS UNCHANGED, which is what the pointer is for: it is the only
// shape that tells "set this to empty" from "leave it alone".
func applyPatch(room *Channel, patch ChannelPatch, at time.Time) {
	if patch.Topic != nil {
		room.Topic = *patch.Topic
	}
	if patch.Purpose != nil {
		room.Purpose = *patch.Purpose
	}
	if patch.Archived != nil {
		if *patch.Archived {
			if room.ArchivedAt == nil {
				// THE WRITER'S OWN CLOCK, AND ONLY IN THIS
				// ANSWER. The instant that reaches the row is
				// the BROKER'S, which is what makes one node's
				// copy of a room byte-identical to another's —
				// this one exists so the caller is handed a room
				// that is archived rather than one that still
				// reads as open, and so the no-op comparison
				// below sees that something moved.
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
// IT COMPARES THE FIELDS A PATCH CAN MOVE, not the whole document: everything
// else on a room is either immutable — its id, its kind, its name and who made
// it — or another record's to move (a unit room's unit, [MemberSet.Unit]; its
// retention, [RetentionSet]), and comparing an encoded document instead would
// make an unrelated field's presence decide whether a topic change is a no-op.
func sameRoom(before, after Channel) bool {
	return before.Topic == after.Topic && before.Purpose == after.Purpose &&
		(before.ArchivedAt == nil) == (after.ArchivedAt == nil)
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

// sortedMembers puts a membership in handle order, which is the order the
// applier writes it in.
//
// THE ORDER IS NOT COSMETIC: a collection written in the order a record
// happened to list it lands in a different order on every node whose writer
// built it from a map, and the replicated file's checksum diverges — which is
// the one claim this domain makes that nothing else would catch.
func sortedMembers(members []Member) []Member {
	out := slices.Clone(members)
	slices.SortFunc(out, func(x, y Member) int { return strings.Compare(x.Handle, y.Handle) })
	return out
}

// cleanHandles trims, drops empties and de-duplicates, keeping the caller's
// order — which for mentions is the order the body named them in, and which is
// therefore the order the mention cap keeps.
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
