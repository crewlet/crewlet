package operator

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
)

// operationKey is the context key [WithOperation] stores under. An unexported
// type, so no other package can write or shadow it.
type operationKey struct{}

// WithOperation marks ctx as carrying the operation a transport named for ONE
// call — the identity a retry of that call repeats, and the one every write
// the call makes is derived from ([builtin.Actor.Operation],
// [builtin.PageDeps.Operation], [builtin.ChatDeps.Operation]).
//
// A TRANSPORT SETS IT, never a tool argument: the operation decides which
// writes are one, and a caller who could put it in the arguments of an
// ordinary call could collapse somebody else's write by guessing theirs. The
// act transport derives it from its envelope's `request_id` and the token that
// sent it ([builtin.RequestOperation]), so a request id names only that
// credential's own writes. A transport that names no request — MCP — sets
// nothing: there, a tracker write names its own operation ([builtin.OperatorTools]
// mints one and answers it as `op_id`, and the call brought back with it is
// that operation again), and a page write or a chat write is a new one.
//
// An empty operation is none: stored as given and read back as "", which
// every consumer treats exactly as a call that named none.
func WithOperation(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, operationKey{}, op)
}

// operationFrom reads the operation [WithOperation] stored, or "".
func operationFrom(ctx context.Context) string {
	op, _ := ctx.Value(operationKey{}).(string)
	return op
}

// seatKey is the context key [withSeat] stores under.
type seatKey struct{}

// seatPin is one call's pinned seat and the operator it was pinned FOR.
type seatPin struct{ operator, seat string }

// withSeat pins the seat ONE call's caller is bound to — "" for a token
// nobody binds — so every frame of that call reads the one answer.
//
// # Why the seat is resolved once per call and carried
//
// The binding lives in the company chart, which a config apply replaces
// while a call is running. Resolved at each frame that asks, one call asked
// three times — the act transport's admission, the actor the tool writes as
// ([WorkActor]) and the audit record ([Server.dispatch]) — and an apply that
// rebound or unbound the token between them admitted a call as one person,
// wrote it as another or as nobody, and audited it under a third. A call is
// made BY whoever it was admitted as, so the first answer is the call's, and
// a rebinding applies from the next call on.
//
// THE DISPATCH SETS IT, for every transport — the act transport's admission
// pins it first, and the dispatch reuses that answer rather than asking
// again. Read back by [pinnedSeat], which tells a pinned "" (an unbound
// caller) from no pin at all.
//
// # The pin names the operator it was made for
//
// A context is inherited by everything derived from it, and an operator is
// put on one with [auth.WithOperator] — so a context re-keyed to ANOTHER
// credential would still carry this caller's pin, and every resolution that
// reads the pin first would answer this caller's seat for somebody else. That
// is not hypothetical: the live channel once resolved every socket on a node
// that way, under a person's own pinned request, and closed each other
// person's as rebound. So the pin is stored with operatorID, and
// [pinnedSeat] answers it only to a context whose operator is still that one.
func withSeat(ctx context.Context, operatorID, seat string) context.Context {
	return context.WithValue(ctx, seatKey{}, seatPin{operator: operatorID, seat: seat})
}

// pinnedSeat reads the seat [withSeat] pinned, and whether one was — for the
// operator on ctx. A pin made for any other operator is no pin at all: the
// caller is resolved from the chart, as one nobody admitted.
func pinnedSeat(ctx context.Context) (string, bool) {
	pin, ok := ctx.Value(seatKey{}).(seatPin)
	if !ok {
		return "", false
	}
	if operatorID, _ := auth.OperatorFrom(ctx); operatorID != pin.operator {
		return "", false
	}
	return pin.seat, true
}

// WorkActor and PageActor read the operator off the request's context.
//
// # An unbound token identifies as itself
//
// A Tier A token has a NAME — the key in `api.auth.tokens` — and that name is
// what lands on the record: `founder`, `ci`, `ops-bot`. It is not a seat and
// must not look like one, so the actor KIND is [tracker.AuthorOperator] — the
// discriminator every renderer and every recipient rule already reads — and
// the credential is recorded again in `OperatorID` so an audit can ask what
// one token did without reasoning about kinds.
//
// The name goes in the author field rather than being left empty because the
// tracker requires one: a record carrying no author is a history row nobody
// can attribute, which is the single thing this surface exists to prevent.
//
// The alternative — asking the caller to name a seat to act as — was rejected:
// it lets anybody with the token write as anybody, and a tracker whose author
// field can be chosen by the writer is not an audit trail.
//
// # A BOUND token also says who it IS, and that is a different field
//
// `contact.crewlet_operator_id` binds a token to a human seat, and that
// binding is an ATTRIBUTION rather than an address — so it changes nothing
// above. What it answers is the question the rule above does not: whose inbox,
// whose pins, whose queue, whose day. Those are the PERSON's, and the person
// is the seat. So the seat travels in [builtin.Actor.Seat], beside an author
// that is still the token and a kind that is still `operator`.
//
// Left unresolved, the person tools wrote a second person record named after
// the credential and `get_my_work` answered for a party of one that no
// colleague had ever filed anything against.
//
// A CONSTRUCTOR because the resolution needs the chart, which is a Tier B
// value a config apply replaces — so it is read per call rather than captured.
// A nil chart, or a build with none loaded, resolves no seat, which is exactly
// an unbound token and an ordinary state. lookup resolves a `${VAR}` binding
// ([seatFor]).
//
// THE SEAT THE CALL WAS PINNED TO WINS ([withSeat]): every call through the
// dispatch carries the one answer its admission and its audit read, and the
// chart is read here only for a caller that reached the actor some other way.
func WorkActor(chart func() *org.Organization, lookup org.EnvLookup) func(
	context.Context, *turnctx.Turn) (builtin.Actor, error) {

	return func(ctx context.Context, _ *turnctx.Turn) (builtin.Actor, error) {
		id, ok := auth.OperatorFrom(ctx)
		if !ok || id == "" {
			return builtin.Actor{}, fmt.Errorf("operator: no operator on this request")
		}
		// THE OPERATOR'S OWN NAME IS THE HANDLE, and the kind says it
		// is not a seat. A tracker whose author field is chosen by the
		// writer is not an audit trail, so there is deliberately no way
		// for a caller to name a seat to act as.
		actor := builtin.Actor{
			Handle: id, Kind: tracker.AuthorOperator, OperatorID: id,
			Seat: personOf(ctx, chart, lookup, id),
		}
		// AND THE OPERATION THE TRANSPORT NAMED, where it named one: it
		// is what makes a retried request the same writes. See
		// [WithOperation].
		actor.Operation = operationFrom(ctx)
		return actor, nil
	}
}

// personOf is the seat a request's token is bound to: the one the call was
// pinned to ([withSeat]), else the chart's answer ([seatFor]). One function for
// both surfaces, so a token is never one person on a work commit and another
// on a page commit.
func personOf(ctx context.Context, chart func() *org.Organization, lookup org.EnvLookup, id string) string {
	if seat, pinned := pinnedSeat(ctx); pinned {
		return seat
	}
	return seatFor(chart, lookup, id)
}

// seatFor is the chart seat a token id is bound to, or "".
//
// lookup resolves a `crewlet_operator_id: ${FOUNDER_ID}`: the serving node's
// own chain ([Options.Env]) — the secret store, then the environment it was
// handed — which is where every other consumer of `contact` resolves one, and
// the same lookup the other direction is handed ([builtin.Parties]), which is
// what stops a company being bound for one direction and unbound for the
// other. Nil reads the process environment.
func seatFor(chart func() *org.Organization, lookup org.EnvLookup, id string) string {
	if chart == nil {
		return ""
	}
	o := chart()
	if o == nil {
		return ""
	}
	seat := o.SeatByOperatorID(id, lookup)
	if seat == nil {
		return ""
	}
	return seat.Handle()
}

// PageActor is [WorkActor] for the knowledge base, and records the SAME
// operator under the SAME name.
//
// THE HANDLE IS THE TOKEN'S OWN NAME, exactly as above. It used to be left
// empty here, and `pages.Actor.Name` falls back to `"operator:" + OperatorID`
// for an actor with no handle — so one person writing through one surface was
// recorded as `founder` on a work commit and `operator:founder` on a page
// commit. The kind is already on the row, in its own column, so the prefix was
// a second encoding of a fact the row carries; what it bought was that the
// audit feed, which is the one screen that reads both histories, showed the
// same person as two people three rows apart, and that a reader filtering on
// a name matched half of what they did.
//
// # And the person behind it, as on a work commit
//
// The seat the token is bound to travels in [pages.Actor.Seat] beside it, for
// the questions that are the person's: whose subscription a watch changes,
// whose team space a page that names none lands in, who a history row draws,
// and who is not woken by their own write. Without it a person's dashboard
// watch followed the page as their credential, which no wake can reach, and a
// page they created was watched by nobody who could be told about it.
//
// A CONSTRUCTOR for the reason [WorkActor] is one: the binding is read off the
// chart per call.
func PageActor(chart func() *org.Organization, lookup org.EnvLookup) func(
	context.Context, *turnctx.Turn) (pages.Actor, error) {

	return func(ctx context.Context, _ *turnctx.Turn) (pages.Actor, error) {
		id, ok := auth.OperatorFrom(ctx)
		if !ok || id == "" {
			return pages.Actor{}, fmt.Errorf("operator: no operator on this request")
		}
		return pages.Actor{
			Handle: id, Kind: pages.AuthorOperator, OperatorID: id,
			Seat: personOf(ctx, chart, lookup, id),
		}, nil
	}
}

// ChatActor resolves the request's credential to the SEAT it is bound to, and
// records the write as that person's, made through that credential — the
// attribution every write on this surface carries (ADR-0024), in the one shape
// a room can render.
//
// # What a chat write records, and why it differs from the tracker's
//
// THREE FIELDS: the bound seat's HANDLE as the message's author, the kind
// [chat.AuthorOperator], and the token's id as `OperatorID`. The tracker and
// the wiki put the token's own name in the author field and the seat beside it
// ([WorkActor]), because a token genuinely is not a colleague there: it files
// work, and the row says a token filed it. A room cannot: a message's author is
// whoever spoke, the room renders that name beside the words, and a room where
// `ci` or `ops-bot` can appear as a speaker is one where "who said this" has
// two vocabularies. So the author is the seat, found by the chain Tier A
// already provides — `api.auth.tokens` maps a presented credential to an
// operator id, and a seat binds one with `contact.crewlet_operator_id`.
//
// THE KIND AND THE TOKEN ARE STILL THE OPERATOR'S, exactly as on every other
// write here: a dashboard write and the same person's assistant's read
// identically in an audit, and an audit can still ask what one token did. The
// kind is also what the store's own rules key on — an operator may take back a
// message somebody else wrote, which is the moderation a credential that can
// erase a room already holds.
//
// # The seat is the one the call was ADMITTED as
//
// [withSeat] pinned it — at the act transport's admission, or at the dispatch
// for a transport that admits nobody by seat — and the audit record names that
// pin. So this reads the pin FIRST and the chart only to learn what the pinned
// seat is: resolved again from the token, an apply that rebound it while the
// call ran wrote the message as one person and audited it under another. The
// chart is asked for the binding only by a caller that reached this some other
// way, which is [WorkActor]'s rule too — and asked through lookup, the serving
// node's own chain ([Options.Env]), for [seatFor]'s reason. Nil reads the
// process environment.
//
// # An unbound token gets nothing, reads included
//
// It is refused NAMING THE FIELD, because the remedy is a line of company
// configuration rather than a different credential. Refusing the READS too is
// the part worth stating: a private room's contents are decided by its
// membership, and a caller the engine cannot resolve to a seat has no
// membership — so serving it "everything public" would be inventing a
// viewer the company never declared. The alternative the tracker rejects for
// writes is worse here in both directions: letting the caller name a seat to
// read as would hand anybody with the token every private conversation in the
// company.
//
// # And it must be a HUMAN seat
//
// A token bound to an AGENT seat is refused rather than accepted quietly. The
// binding exists so a person's own credential is recognised as theirs; aimed
// at an agent it would let whoever holds the token speak in that agent's
// voice, and every colleague in the room would read the message as the agent's
// own words. The admission pins whichever seat the token names, of either
// kind, so this holds for a pinned seat exactly as for a looked-up one.
//
// # Every refusal is a [ChatActorError], typed by why
//
// Because the three are fixed in three different places, and a surface that
// renders them has to tell them apart without reading a sentence: a token no
// seat binds is the act transport's own refusal ([CodeUnbound]) and is
// answered under that code everywhere, while an agent seat and a seat an apply
// removed are each their own.
func ChatActor(company func() *org.Organization, lookup org.EnvLookup) func(
	context.Context, *turnctx.Turn) (chat.Actor, error) {

	bound := ChatViewer(company, lookup)
	return func(ctx context.Context, _ *turnctx.Turn) (chat.Actor, error) {
		id, ok := auth.OperatorFrom(ctx)
		if !ok || id == "" {
			return chat.Actor{}, &ChatActorError{Refusal: ChatUnbound}
		}
		handle, pinned := pinnedSeat(ctx)
		if !pinned {
			return bound(id)
		}
		if handle == "" {
			return chat.Actor{}, &ChatActorError{Refusal: ChatUnbound, Operator: id}
		}
		// THE CHART IS READ FOR WHAT THE SEAT IS, never for whose it is:
		// the binding was decided when the call was admitted.
		var seat *org.Role
		if company != nil {
			if chart := company(); chart != nil {
				seat = chart.SeatByHandle(handle)
			}
		}
		if seat == nil {
			return chat.Actor{}, &ChatActorError{Refusal: ChatSeatRemoved,
				Operator: id, Seat: handle}
		}
		return chatSpeaker(seat, id)
	}
}

// ChatViewer is [ChatActor] for a credential no call admitted: the person one
// operator id speaks as in chat, read off the chart alone, or the
// [ChatActorError] that says why it is nobody.
//
// It is the live channel's resolution of a SOCKET, and it is ChatActor's own
// for every caller the dispatch did not pin — one function, so a person who
// may post in a room is exactly the person whose socket may be shown it.
//
// IT TAKES NO CONTEXT, which is the point of it being separate. A socket is
// re-resolved on every pass its hub makes, and a pass runs on behalf of
// whatever triggered it — among them a person's own request pushing their read
// state, which carries the seat the dispatch pinned for that one caller.
// Resolved through ChatActor under it, every socket on the node read that pin
// and resolved to the caller. One credential over one chart needs nothing a
// context could carry, so this is handed nothing a context could carry.
//
// THROUGH lookup, the serving node's own chain, as [seatFor] resolves every
// other operator binding on this surface: read from the process environment
// instead, a person bound by a sealed or a handed `${VAR}` was admitted by the
// act transport and refused here as bound to no seat. Nil reads the process
// environment.
func ChatViewer(company func() *org.Organization, lookup org.EnvLookup) func(
	operatorID string) (chat.Actor, error) {

	return func(operatorID string) (chat.Actor, error) {
		if operatorID == "" {
			return chat.Actor{}, &ChatActorError{Refusal: ChatUnbound}
		}
		var seat *org.Role
		if company != nil {
			if chart := company(); chart != nil {
				seat = chart.SeatByOperatorID(operatorID, lookup)
			}
		}
		if seat == nil {
			return chat.Actor{}, &ChatActorError{Refusal: ChatUnbound,
				Operator: operatorID}
		}
		return chatSpeaker(seat, operatorID)
	}
}

// chatSpeaker is the actor a credential speaks as through seat, or the refusal
// of an agent seat — the one rule both the pinned and the looked-up seat pass
// through, because the admission pins whichever seat a token names, of either
// kind.
func chatSpeaker(seat *org.Role, operatorID string) (chat.Actor, error) {
	if !seat.IsHuman() {
		return chat.Actor{}, &ChatActorError{Refusal: ChatAgentSeat,
			Operator: operatorID, Seat: seat.Handle()}
	}
	return chat.Actor{
		Handle: seat.Handle(), Kind: chat.AuthorOperator, OperatorID: operatorID,
	}, nil
}

// ChatRefusal is why [ChatActor] resolved a credential to nobody.
type ChatRefusal string

const (
	// ChatUnbound is a caller no seat binds: a token no `kind: human`
	// seat names in `contact.crewlet_operator_id`, a disabled guard's
	// anonymous caller, or a request carrying no operator at all. Fixed in
	// the company chart — or, for the anonymous caller, in Tier A.
	//
	// SPELLED AS [CodeUnbound], by construction rather than by agreement:
	// it is the act transport's own refusal of the same caller, and a client
	// that recognises the code there — the CLI's dedicated sentence, the
	// dashboard's write block — must recognise it on every chat route too.
	ChatUnbound = ChatRefusal(CodeUnbound)

	// ChatAgentSeat is a token bound to an AGENT seat, which would speak in
	// that agent's voice. Fixed by moving the binding onto the person's own
	// seat.
	ChatAgentSeat ChatRefusal = "agent_seat"

	// ChatSeatRemoved is a call admitted as a seat that a configuration
	// apply removed while it ran. Fixed by nothing but asking again, under
	// the binding the chart holds now.
	ChatSeatRemoved ChatRefusal = "seat_removed"
)

// Valid reports whether r is one of the refusals above.
func (r ChatRefusal) Valid() bool {
	switch r {
	case ChatUnbound, ChatAgentSeat, ChatSeatRemoved:
		return true
	}
	return false
}

// Code is the refusal's code on a route that answers it: its own word, which
// for an unbound caller is the act transport's [CodeUnbound] (see
// [ChatUnbound]) and for the other two is theirs alone, since neither is that
// rule.
func (r ChatRefusal) Code() httpjson.Code { return httpjson.Code(r) }

// ChatActorError is [ChatActor]'s refusal: why, under which credential, and
// the seat it named or was admitted as. Its sentence names what to change.
type ChatActorError struct {
	Refusal ChatRefusal

	// Operator is the credential's id, empty for a request carrying none.
	Operator string

	// Seat is the seat the credential named or was admitted as — empty for
	// an unbound caller, which named none.
	Seat string
}

func (e *ChatActorError) Error() string {
	switch e.Refusal {
	case ChatSeatRemoved:
		return fmt.Sprintf("the token %q was admitted as the seat %q, which the "+
			"company's chart no longer has — a configuration apply removed it "+
			"while this call ran, so nothing was said or read as it", e.Operator, e.Seat)
	case ChatAgentSeat:
		return fmt.Sprintf("the token %q is bound to %q, which is an agent seat — "+
			"move `contact.crewlet_operator_id: %s` onto the `kind: human` seat "+
			"this person is, or this credential would speak in that agent's own "+
			"voice", e.Operator, e.Seat, e.Operator)
	}
	switch e.Operator {
	case "":
		return "this request carries no operator, and a message's author is the " +
			"`kind: human` seat a token is bound to"
	case auth.AnonymousOperator:
		return "api.auth.disabled is set, so this caller was never checked and is " +
			"nobody, and a message's author is a person — enable api.auth with a " +
			"token for each person, and bind it with contact.crewlet_operator_id " +
			"on their `kind: human` seat"
	}
	return fmt.Sprintf("the token %q is bound to no seat, and a message's author "+
		"is a seat — set `contact.crewlet_operator_id: %s` on the `kind: human` "+
		"seat this person is, because a caller may never name one", e.Operator,
		e.Operator)
}
