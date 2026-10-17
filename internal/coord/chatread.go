package coord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// A person's chat read state: how far they have read in each room they have
// opened, which rooms they have muted, and whether they are in do-not-disturb.
//
// # Whose record it is: the SEAT's
//
// The record is kept under [iam.RecordOwner] of the principal flushing it —
// the one name every principal's own record is kept under, an inbox, a pin and
// a personal view among them — and for everybody chat admits that is a HUMAN
// SEAT's handle: chat speaks and reads as the seat a person, or a machine bound
// to one, acts as, and a principal acting as no human seat is refused before
// it reaches this store. So `handle` below is that seat's handle, and nothing
// here knows about logins or person ids.
//
// THE SEAT'S NEXT HOLDER INHERITS IT. A person holds one human seat from the
// day they are added to the day they are removed (ADR-0032), and freeing a
// seat leaves what accumulated under its handle for whoever holds it next —
// the person once reinstated, or a successor once they are moved or removed.
// Their cursors, their mutes and their do-not-disturb pass with the seat
// exactly as its tracker inbox, its pins and its personal views already do,
// and so do the rooms and conversations chat derives from the same handle.
// Keying by the person instead would be a second rule for "a principal's own
// record" beside [iam.RecordOwner], and keying by an agent id is not open at
// all: a human seat has none. A reset when the holder changes would be a
// write on every data node driven by the identity applier for one record of
// one seat, and is deliberately not made.
//
// # Why it is here and not on the chat log
//
// It is the one piece of chat state that is NOT a fact about the company. A
// cursor is a fact about one person's attention, nobody replays it, and no
// other member of the fleet derives anything from its history — which is the
// whole of what the state log is for.
//
// The volume decides it. At twenty people reading through a working day and a
// flush no oftener than every fifteen seconds, cursors alone are tens of
// thousands of writes a day: comparable to the entire message census, on a
// strictly ordered identity-claiming log, entering the trim's terms, every
// snapshot, every rejoining node's transfer and the identity claim itself —
// for a number nobody will ever read back. Coordination is where "everything
// else a fleet has to agree on" lives, it already has compare-and-set, and a
// cursor written here costs one key.
//
// # Why the bucket has NO age
//
// A read cursor has no expiry that means anything. A person who has not
// opened a room in four months has still read it up to where they read it,
// and an aged-out cursor would silently mark a whole channel unread — the
// exact opposite of the fact it stores. So the bucket joins the ageless slots
// beside the activation pointer, the channel records, the sandbox runs, the
// sealed secrets, the mailbox registry and the positions register, and
// removal is a DECISION rather than a clock. A removal leaves a marker, as
// every removal in an ageless family does, and [Markers.SweepMarkers] takes it
// under the maintenance duty — so the bucket does not keep a trace of
// everybody who ever left.
//
// # Why a bucket of its own, and not a class in the positions register
//
// The positions register is the other bucket with no age, and its eight
// classes share it for exactly that retention ([PositionKey]). This record
// does not join them, for what writes it and what reads it. Every tab of every
// person flushes it — tens of thousands of writes a day — and every chat
// screen reads it from the stream leader, while the register is written one
// node's heartbeat or one person's gesture at a time and is read by the trim,
// the state log's write fence and the capacity window, on decisions about what
// every other node may delete or publish. A bucket is a stream, and on a fleet
// a stream is ONE raft group with ONE leader: as a class of the register, the
// read state's flushes would be ordered through the same group, and its reads
// answered by the same leader, as the trim's inputs and the capacity window's
// exclusion. A bucket of its own keeps the busiest record the fleet writes off
// the path of its most consequential ones, and its forgetter — a reconcile
// against the company's seats, which nothing in the register has — reads a
// listing of this record alone.
//
// # Who forgets a record, and when
//
// The maintenance duty's reconcile (internal/maintenance), against the HUMAN
// SEATS of the company the fleet is pointed at — the seat is whose record it
// is, so the seat leaving is what ends it, never its holder leaving — and in
// two steps for the mailbox registry's reason: ABSENCE IS OBSERVED, NOT
// INFERRED. The first sweep that finds a reader's seat missing stamps the
// record ([ChatReads.SetChatReadAbsence]); a sweep that finds it back clears
// the stamp; and a record is forgotten only once its stamp is older than the
// reconcile's grace ([ChatReads.ForgetChatRead] takes that bound and refuses a
// record stamped later, or not at all). So a seat deleted from the chart by
// accident and restored within the grace keeps every cursor, and one that
// stays gone is forgotten a grace later rather than never.
//
// The stamp lives IN the record because nothing else outlives the duty
// moving: a reconcile that remembered absences in memory would restart every
// grace at every handoff, and on a fleet that restarts more often than the
// grace, forget nobody.
//
// # Why advancing is a merge rather than a write
//
// The caller never writes against a revision. Two tabs belonging to one
// person read two different rooms at once, and a last-writer-wins PUT would
// lose whichever flush landed first — not a conflict worth reporting to
// anybody, just a channel that quietly re-unreads itself. So the operation is
// a MERGE: cursors only ever move forward, a room a flush does not mute or
// unmute keeps the state it had, and the store resolves the race itself by
// retrying its own read-modify-write. That is safe precisely BECAUSE the
// merge is idempotent — re-applying it is a no-op, so a retry cannot lose
// anything.
//
// # Why a mute is a room rather than a list
//
// A flush MUTES or UNMUTES named rooms; it never sends the list. The list is a
// set one person edits a room at a time, and a write that replaced it would
// have to know the whole of it first — which a screen opened a moment ago does
// not, and which two tabs each holding their own copy would each write over
// the other's toggle with. Per room, the gesture needs nothing but the room,
// and two tabs muting two rooms at once both land, exactly as two cursors do.

// MaxReadCursors bounds how many channels one person's record remembers.
//
// 256, matching the bound the tracker puts on a person's own lists, and the
// number is the easy half. The hard half is what happens AT the cap, and it is
// genuinely different here: the tracker's cap is safe because entries below
// the seen-through position are PRUNED on every write, so the list bounds
// itself and the cap is a backstop nobody reaches. A read cursor is never
// prunable — every one of them is still true.
//
// So this IS the discarding cap, and the discard is stated rather than
// discovered: past 256 the LEAST RECENTLY ADVANCED cursor is dropped, and that
// channel reads as unread from its tail the next time the person opens it. The
// cost is a stale badge on the room somebody has ignored longest, which is the
// least wrong thing to lose; the alternative — refusing the write — would
// freeze every other cursor in the record to protect the one nobody looks at.
const MaxReadCursors = 256

// ErrInvalidReadState marks a flush the merge REFUSED for what it carries — a
// cursor or a mute naming no room, a room both muted and unmuted, a mute that
// would take the list past [MaxReadCursors] — as opposed to a store that could
// not take it.
//
// A SENTINEL because the two ask opposite things of the caller, and a caller
// holding only the error cannot tell them apart otherwise: a store that was
// unreachable or kept losing its compare-and-set is worth asking again, while
// a refused flush is refused the same way every time it is sent, so "try
// again" over it is an instruction nobody can carry out. Both backends merge
// through [MergeChatRead] and return its error as it is, so this survives to
// whoever made the call.
var ErrInvalidReadState = errors.New("coord: a chat read flush was refused")

// ReadStateSentence is a flush the merge refused, as the caller that sent it
// reads it: what was wrong with it and what to do — without the "coord: a chat
// read flush was refused:" a Go error opens with, which names the package it
// came from and says "refused" a second time inside a refusal. Any other
// error, or a refusal a caller wrapped in context of its own, is its whole
// message, because context is never dropped to tidy a prefix away — the rule
// internal/chatlet's Sentence keeps for the chat store's refusals.
//
// A MODEL READS IT: the read state is written through the operator
// catalogue, whose tool answers a refused flush in this sentence to the model
// that made the call, a person's assistant acting for the one who muted those
// rooms. A person flushing from the dashboard is answered the refusal's class
// in its code's own words instead, never a tool's sentence (ADR-0060). It
// lives here, beside the sentinel, because this package writes the sentences:
// a caller stripping the prefix itself would be relying on how they are
// spelled.
func ReadStateSentence(err error) string {
	if errors.Is(err, ErrInvalidReadState) {
		// THE PREFIX IS THE SENTINEL'S OWN TEXT, which is exactly what a
		// refusal built as "%w: <sentence>" opens with — and exactly what
		// it does not open with once a caller wrapped it.
		if rest, ok := strings.CutPrefix(err.Error(), ErrInvalidReadState.Error()+": "); ok {
			return rest
		}
	}
	return err.Error()
}

// ChatReadState is one person's whole chat read state.
type ChatReadState struct {
	// Cursors maps a channel id to the PACKED log position that person
	// has read through. Packed rather than a triple because it is only
	// ever compared, never decomposed: a cursor that cannot be compared
	// with a message's own version is not a cursor.
	Cursors map[string]int64 `json:"cursors,omitempty"`

	// Muted are the channels that raise no badge and no push. A mute
	// suppresses NOTICE, never delivery: the messages are still there and
	// still searchable, and a seat's wake is not affected by anybody's
	// mute at all.
	Muted []string `json:"muted,omitempty"`

	// DNDUntil is when do-not-disturb ends. The ZERO INSTANT means not in
	// do-not-disturb, and an instant in the past means it is already over
	// — which is why it is an instant rather than a bool: a bool would
	// need a second gesture to clear it and would strand a person silenced
	// by a toggle they forgot.
	DNDUntil time.Time `json:"dnd_until,omitzero"`

	// UpdatedAt is diagnostic. Nothing branches on it — the bucket has no
	// age — but a record with no stamp makes "since when" unanswerable
	// without reading the broker's own metadata.
	UpdatedAt time.Time `json:"updated_at,omitzero"`

	// Revision is the store's own revision of the record this state was
	// read at or written at; zero for a person with no record.
	//
	// IT ORDERS TWO STATES OF ONE PERSON, which nothing else here can. The
	// store serialises a person's flushes, but what each answers — and what
	// the live channel pushes from it — travels on its own, so a screen can
	// be handed an older state after a newer one and must keep the newer.
	// [ChatReadState.UpdatedAt] cannot order them: it is each flush's own
	// instant, stamped by whichever node took it, not the order the store
	// took them in. This is that order: strictly increasing across every
	// write to the record, and for the life of the store, so a record
	// forgotten and written again still moves past every state the old one
	// ever reported.
	//
	// NEVER STORED, because a record cannot know the revision it will be
	// written at: a backend stamps it on every state it answers and keeps it
	// out of what it persists.
	Revision uint64 `json:"revision,omitempty"`

	// AbsentSince is when the reconcile first found this record's seat
	// missing from the company the fleet is pointed at. ZERO while the
	// company has it.
	//
	// MACHINERY, written only by [ChatReads.SetChatReadAbsence]: a flush
	// neither sets nor clears it, because a flush is evidence about one
	// node's revision rather than the fleet's, and the reconcile reads the
	// fleet's.
	AbsentSince time.Time `json:"absent_since,omitzero"`
}

// PersonChatRead is one person's read state NAMED BY ITS SEAT — the form a
// client is sent one in: the live channel's `chat_cursor_moved` push and the
// rail's own `read`.
//
// THE NAME IS WHAT MAKES THE REVISION USABLE. [ChatReadState.Revision] orders
// two states of ONE record and nothing else — on the coordination KV it is the
// bucket's own sequence, shared by every seat's record — so a client holding
// one seat's state must be able to tell that the next is another seat's, which
// it meets whenever the seat it reads as is not the one it read as before — a
// tab handed to another reader, a socket reconnecting for a person moved to
// another seat — and REPLACE it rather than compare two records' revisions and
// drop the newcomer's as older. A state that does not say whose it is leaves
// the client nothing to tell that by.
//
// NEVER STORED, for [ChatReadState.Revision]'s reason: the record is keyed by
// the seat, so the name is a fact about the key rather than the value, and a
// surface that hands a state out stamps the seat it read it for.
type PersonChatRead struct {
	// Handle is the seat whose record this is.
	Handle string `json:"handle"`
	ChatReadState
}

// WithAbsence is the state with its absence stamp set to since, reporting
// whether that changed anything.
//
// THE ONE RULE FOR THE STAMP, shared by both backends for [MergeChatRead]'s
// reason. A non-zero since stamps a record that has none and KEEPS an earlier
// stamp, because the grace is measured from the FIRST sweep that saw the seat
// missing — a later sweep restamping would restart it every tick and forget
// nobody. The zero instant clears it: the company has the seat again.
func (s ChatReadState) WithAbsence(since time.Time) (ChatReadState, bool) {
	switch {
	case since.IsZero() && s.AbsentSince.IsZero():
		return s, false
	case since.IsZero():
		s.AbsentSince = time.Time{}
		return s, true
	case !s.AbsentSince.IsZero():
		return s, false
	}
	s.AbsentSince = since.UTC()
	return s, true
}

// AbsentBy reports whether the state has been stamped absent at or before
// cutoff: the one condition under which [ChatReads.ForgetChatRead] removes it.
func (s ChatReadState) AbsentBy(cutoff time.Time) bool {
	return !s.AbsentSince.IsZero() && !s.AbsentSince.After(cutoff)
}

// ChatReader is one seat holding read state, as the reconcile lists them: the
// handle, and when the seat was first found missing from the company.
type ChatReader struct {
	Handle string

	// AbsentSince is [ChatReadState.AbsentSince]: zero while the company
	// has the seat.
	AbsentSince time.Time
}

// ChatReadDelta is what one flush changes.
//
// EVERY FIELD IS OPTIONAL AND ABSENT MEANS UNCHANGED, which is what lets two
// tabs flush different facts about one person without either erasing the
// other's. A room a flush does not name — a cursor, a mute or an unmute — is
// left exactly as it was.
type ChatReadDelta struct {
	// Cursors are advanced, never set: a value at or below what is stored
	// is ignored. A cursor that could move backwards would make "unread"
	// flicker as two tabs disagree about which is further along.
	Cursors map[string]int64

	// Mute adds rooms to the muted set and Unmute takes them out of it;
	// every room neither names keeps the state it had. A ROOM AT A TIME,
	// never the list — see "Why a mute is a room rather than a list".
	//
	// Muting a muted room and unmuting one that is not are no-ops, so a
	// repeated toggle is safe; naming one room in both is refused
	// ([ErrInvalidReadState]), because the flush would be stating two
	// opposite facts about it and no order between them is the caller's.
	Mute   []string
	Unmute []string

	// DNDUntil replaces when non-nil, including with the zero instant,
	// which is how do-not-disturb is cleared.
	DNDUntil *time.Time

	// At stamps the record. Passed in rather than read from a clock here,
	// because every other write in this package takes its instant from the
	// caller and a package that reads the clock in one place is a package
	// whose tests are serial.
	At time.Time
}

// ChatReads is the company's per-person chat read state, one record per human
// seat — see "Whose record it is" above for why the seat.
type ChatReads interface {
	// ChatRead reports one person's read state.
	//
	// A person who has read nothing is a ZERO STATE and no error: the
	// absence of a record is the ordinary condition of somebody who has
	// not opened chat yet, and making the caller distinguish it from a
	// failure would push a three-valued answer onto a question that has
	// two honest ones.
	ChatRead(ctx context.Context, handle string) (ChatReadState, error)

	// AdvanceChatRead merges a flush into one person's record and reports
	// the state that resulted.
	//
	// FORWARD-ONLY on the cursors and PER ROOM on the mutes, so it is
	// idempotent, and two tabs racing over different rooms produce the
	// same record whichever lands first: the store's own retry cannot lose
	// a flush. Past [MaxReadCursors] the least recently advanced cursor is
	// dropped — see the constant.
	//
	// THREE KINDS OF FAILURE, told apart by sentinel: [ErrInvalidReadState]
	// is a flush the merge refused, the same way every time it is sent;
	// [ErrWriteUnknown] is a write that was sent and never answered, which
	// may have landed; and every other error — a read that failed before
	// any write, every compare-and-set lost to another writer — changed
	// nothing. Sending the same flush again is safe after either of the
	// last two, because the merge is idempotent: a flush that did land is
	// merged a second time into the same record.
	AdvanceChatRead(ctx context.Context, handle string, delta ChatReadDelta) (ChatReadState, error)

	// SetChatReadAbsence stamps when a record's seat was first found missing
	// from the company the fleet is pointed at — since, unless an earlier
	// stamp stands — or, with the zero instant, clears the stamp because the
	// company has the seat again. See [ChatReadState.WithAbsence] for the
	// rule. Reports whether the seat has a record at all: one without is left
	// without, because an absence is a fact about a record and stamping it
	// onto nothing would create one for a seat nobody ever read a room as.
	//
	// A compare-and-set like every write here, so a person's own flush
	// landing beside it loses neither the flush nor the stamp. Its errors
	// are [ChatReads.AdvanceChatRead]'s: [ErrWriteUnknown] marks the one
	// that may have landed.
	SetChatReadAbsence(ctx context.Context, handle string, since time.Time) (bool, error)

	// ForgetChatRead removes a person's record, but only one stamped absent
	// at or before absentBy ([ChatReadState.AbsentBy]), reporting whether it
	// did.
	//
	// The bucket has no age, so this is the ONLY way a record leaves it, and
	// the condition is checked against the record as the write finds it
	// rather than as the caller last listed it: a seat a peer's sweep has
	// just found back in the company — its stamp cleared — is kept, and so is
	// one whose stamp is younger than the grace the caller is applying.
	// False with no error is that answer, or no record at all. An error
	// marked [ErrWriteUnknown] is a removal sent and never answered, which
	// may have happened.
	ForgetChatRead(ctx context.Context, handle string, absentBy time.Time) (bool, error)

	// ChatReaders lists every seat holding read state, by handle, each with
	// its absence stamp.
	//
	// What makes the reconcile possible at all: without it, finding the
	// records of seats that left would mean knowing which seats used to be
	// here, which is the question the org chart no longer answers. RAISES rather
	// than answering short, on an unreachable store and on a record it
	// cannot decode: a reader left out of the list is one the reconcile
	// never judges, and so never forgets.
	ChatReaders(ctx context.Context) ([]ChatReader, error)

	// WatchChatReads streams every record that exists, then the
	// [ChatReadUpdate.Current] marker, then every change after it: a
	// flush, an absence stamp, a forget.
	//
	// WHAT THE LIVE CHANNEL IS DERIVED FROM. A person's tabs are attached
	// to whichever nodes their sockets reached, and a flush is taken by
	// whichever node served it — so every node serving sockets watches,
	// and each pushes a changed record to that seat's own sockets on that
	// node. The push comes from the durable record rather than from the
	// node that took the write telling its peers, so there is no message
	// a node carries out on another's word, a flush through any surface
	// reaches every tab, and a node that was not listening loses nothing
	// the record does not still hold.
	//
	// The channel is closed when ctx ends or the watch fails. A CLOSED
	// CHANNEL IS NOT A FORGET: the watcher has stopped hearing, and a
	// consumer watches again — the records that exist then are delivered
	// again before the next marker, so a change it missed while it was
	// not listening is in that answer, at its revision, and one it already
	// pushed is recognised as no newer than what it holds.
	WatchChatReads(ctx context.Context) (<-chan ChatReadUpdate, error)
}

// ChatReadUpdate is one change a read-state watch delivers.
type ChatReadUpdate struct {
	// Handle names the seat whose record it is. Empty on the marker.
	Handle string

	// State is the record as it now stands, stamped with its
	// [ChatReadState.Revision], and nil when the record was forgotten.
	State *ChatReadState

	// Current marks the end of the records that existed when the watch
	// started: every update before it is one of those, and every update
	// after it is a change. A consumer that re-syncs after watching again
	// does it from what precedes the marker, the only point at which the
	// watch can say a record it has not delivered does not exist.
	Current bool
}

// MergeChatRead applies a flush to a stored state and reports the result.
//
// THE ONE IMPLEMENTATION OF THE MERGE RULE, shared by both backends, because a
// merge written twice is two answers to "did this cursor move" and the two
// would differ first on exactly the case nobody tests: a delta that arrives
// out of order.
//
// It is PURE and IDEMPOTENT. Pure, so both backends and the suite exercise the
// same arithmetic without a store. Idempotent, so re-applying it is a no-op —
// which is what makes a lost compare-and-set safe to retry, and is the whole
// reason the caller never writes against a revision. The result carries none:
// the store stamps [ChatReadState.Revision] once it knows where the write
// landed.
func MergeChatRead(state ChatReadState, delta ChatReadDelta) (ChatReadState, error) {
	out := ChatReadState{
		Cursors:     make(map[string]int64, len(state.Cursors)+len(delta.Cursors)),
		DNDUntil:    state.DNDUntil,
		UpdatedAt:   state.UpdatedAt,
		AbsentSince: state.AbsentSince,
	}
	maps.Copy(out.Cursors, state.Cursors)

	for channel, at := range delta.Cursors {
		if channel == "" {
			return ChatReadState{}, fmt.Errorf("%w: a read cursor needs a "+
				"channel; the flush named one with no id, which is a client "+
				"that lost the value on the way here", ErrInvalidReadState)
		}
		// FORWARD ONLY. A cursor that could move backwards would make an
		// unread badge flicker as two tabs disagree about which is
		// further along — and the loser would be whichever flush the
		// network happened to deliver second.
		if at > out.Cursors[channel] {
			out.Cursors[channel] = at
		}
	}
	evictStaleCursors(out.Cursors)

	muted, err := mergeMutes(state.Muted, delta.Mute, delta.Unmute)
	if err != nil {
		return ChatReadState{}, err
	}
	out.Muted = muted
	if delta.DNDUntil != nil {
		out.DNDUntil = delta.DNDUntil.UTC()
	}
	if !delta.At.IsZero() {
		out.UpdatedAt = delta.At.UTC()
	}
	if len(out.Cursors) == 0 {
		// An empty map and a nil one encode differently, and the record
		// is compared by the suite: one shape, so a person who has muted
		// a room and read nothing round-trips identically on both
		// backends.
		out.Cursors = nil
	}
	return out, nil
}

// mergeMutes is the muted set after one flush: stored, plus every room mute
// names, less every room unmute names — sorted, so a set two backends hold is
// one value.
//
// THE CAP IS CHECKED AGAINST THE SET THE FLUSH LEAVES, and only when the flush
// grows it. A mute is ONE deliberate edit by one person, so a set past
// [MaxReadCursors] is REFUSED rather than cut — the opposite of what the
// cursors do, and deliberately: a silent truncation is a room they muted that
// keeps notifying them and no way to find out, where a cursor is machinery and
// losing the stalest one costs a stale badge. A flush that does not grow the
// set is never refused for its size, so an unmute, or a flush about something
// else entirely, always lands on whatever the record holds.
func mergeMutes(stored, mute, unmute []string) ([]string, error) {
	if len(mute) == 0 && len(unmute) == 0 {
		return slices.Clone(stored), nil
	}
	set := make(map[string]bool, len(stored)+len(mute))
	for _, channel := range stored {
		set[channel] = true
	}
	lifted := make(map[string]bool, len(unmute))
	for _, channel := range unmute {
		if channel == "" {
			return nil, fmt.Errorf("%w: an unmute needs a channel; the flush "+
				"named one with no id, which is a client that lost the value "+
				"on the way here", ErrInvalidReadState)
		}
		lifted[channel] = true
	}
	for _, channel := range mute {
		switch {
		case channel == "":
			return nil, fmt.Errorf("%w: a mute needs a channel; the flush "+
				"named one with no id, which is a client that lost the value "+
				"on the way here", ErrInvalidReadState)
		case lifted[channel]:
			return nil, fmt.Errorf("%w: %s is both muted and unmuted in one "+
				"flush; send the one that is meant", ErrInvalidReadState, channel)
		}
		set[channel] = true
	}
	for channel := range lifted {
		delete(set, channel)
	}
	if len(set) > MaxReadCursors && len(set) > len(stored) {
		return nil, fmt.Errorf("%w: muting that would hold %d channels and "+
			"the cap is %d; unmute something first", ErrInvalidReadState,
			len(set), MaxReadCursors)
	}
	if len(set) == 0 {
		// ONE SHAPE FOR "NOTHING MUTED", for the cursors' reason below:
		// the record is compared by the suite, and an empty list and a
		// nil one encode differently.
		return nil, nil
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// evictStaleCursors bounds the map at [MaxReadCursors].
//
// THE STALEST CURSOR GOES, and "stalest" is the LOWEST POSITION rather than a
// timestamp nobody stores: a position is a place on a totally ordered log, so
// the cursor furthest back in it is by construction the room this person has
// read least recently. That is the fact a second `updated_at` per channel
// would have carried, derived from the value already there.
func evictStaleCursors(cursors map[string]int64) {
	if len(cursors) <= MaxReadCursors {
		return
	}
	type entry struct {
		channel string
		at      int64
	}
	all := make([]entry, 0, len(cursors))
	for channel, at := range cursors {
		all = append(all, entry{channel, at})
	}
	// Ordered by position and then by channel id, so two nodes evicting
	// the same over-full record drop the same cursor: the tie-break is
	// what stops "identical inputs, identical output" from depending on
	// Go's map iteration order.
	slices.SortFunc(all, func(a, b entry) int {
		if a.at != b.at {
			return cmp.Compare(a.at, b.at)
		}
		return cmp.Compare(a.channel, b.channel)
	})
	for _, drop := range all[:len(all)-MaxReadCursors] {
		delete(cursors, drop.channel)
	}
}
