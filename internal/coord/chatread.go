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

// A person's chat read state: how far they have read in each room, which
// rooms they muted, and their do-not-disturb.
//
// The record is keyed by [iam.RecordOwner] of the principal flushing it, for
// everybody chat admits a HUMAN SEAT's handle, and it passes with the seat
// (package doc, "A person's own record here is their SEAT's"). Keying by the
// person would be a second rule for "a principal's own record", a human seat
// has no agent id, and a reset when the holder changes would be an
// identity-applier write on every data node, so none is made.

// # Why a bucket of its own
//
// The state is no fact about the company: nobody replays a cursor and no
// member derives anything from its history, and at tens of thousands of
// flushes a day (twenty people, a flush every fifteen seconds at most) the
// state log would carry them into its trim, every snapshot, every
// rejoin and the identity claim. Nor is it a class of the ageless positions
// register ([PositionKey]): a bucket is ONE raft group with ONE leader, and
// the busiest record the fleet writes would share them with the trim's inputs,
// the write fence and the capacity window.

// # Why the bucket has no age, and who forgets a record
//
// An aged-out cursor would mark a room unread, the opposite of the fact it
// stores, so removal is a decision and its marker is swept by
// [Markers.SweepMarkers]. The maintenance duty's reconcile against the
// company's HUMAN SEATS decides it in two steps, because absence is observed,
// not inferred: a sweep missing the seat stamps the record
// ([ChatReads.SetChatReadAbsence]), one finding it back clears the stamp, and
// [ChatReads.ForgetChatRead] removes only a record stamped longer than the
// grace ago. The stamp lives IN the record, so a duty handoff does not restart
// every grace.

// # Why advancing is a merge, and a mute is a room
//
// The caller never writes against a revision: a last-writer-wins PUT would
// lose one of two tabs' flushes. Cursors only move forward, a flush mutes or
// unmutes NAMED rooms (never the list, which a screen opened a moment ago does
// not know), and everything unnamed keeps its state, so the merge is idempotent
// and the store's own compare-and-set retry cannot lose a flush.

// MaxReadCursors bounds how many channels one person's record remembers.
//
// 256, the bound the tracker puts on a person's own lists. The tracker prunes
// its lists, so its cap is a backstop; a read cursor is never prunable, so
// this cap discards: past it the LEAST RECENTLY ADVANCED cursor is dropped
// ([evictStaleCursors]) and that room reads unread from its tail. A stale
// badge on the room ignored longest is the least wrong loss; refusing the
// write would freeze every other cursor to protect it.
const MaxReadCursors = 256

// ErrInvalidReadState marks a flush the merge REFUSED for what it carries (a
// cursor or a mute naming no room, a room both muted and unmuted, a mute past
// [MaxReadCursors]), as opposed to a store that could not take it.
//
// A sentinel because the two ask opposite things: an unreachable or contended
// store is worth asking again, a refused flush is refused the same way every
// time. Both backends return [MergeChatRead]'s error as it is.
var ErrInvalidReadState = errors.New("coord: a chat read flush was refused")

// ReadStateSentence is a flush the merge refused, as its sender reads it: what
// was wrong and what to do, without the sentinel's "coord: a chat read flush
// was refused:" prefix. Any other error, or a refusal a caller wrapped, is its
// whole message: context is never dropped to tidy a prefix away (the rule
// internal/chatlet's Sentence keeps).
//
// A model reads it: the operator catalogue's tool answers a refused flush in
// this sentence, while the dashboard answers the refusal's class (ADR-0060).
// It lives beside the sentinel because this package spells the sentences.
func ReadStateSentence(err error) string {
	if errors.Is(err, ErrInvalidReadState) {
		// The sentinel's own text: what a "%w: <sentence>" refusal opens
		// with, and a wrapped one does not.
		if rest, ok := strings.CutPrefix(err.Error(), ErrInvalidReadState.Error()+": "); ok {
			return rest
		}
	}
	return err.Error()
}

// ChatReadState is one person's whole chat read state.
type ChatReadState struct {
	// Cursors maps a channel id to the PACKED log position read through:
	// packed, not a triple, because it is only compared with a message's
	// own version, never decomposed.
	Cursors map[string]int64 `json:"cursors,omitempty"`

	// ThreadCursors maps a thread's root message id to the PACKED position
	// read through IN THAT THREAD, which the thread pane flushes. A reply
	// is new when above both this and the room's cursor: reading the room
	// does not open a thread. The forward rule and discarding cap of
	// [ChatReadState.Cursors].
	ThreadCursors map[string]int64 `json:"thread_cursors,omitempty"`

	// Muted are the channels that raise no badge and no push: a mute
	// suppresses notice, never delivery, and no seat's wake reads it.
	Muted []string `json:"muted,omitempty"`

	// Every are the channels whose every message reaches this reader
	// ([NotifyAll]), sorted. A channel is in at most one of this and
	// [ChatReadState.Muted]; in neither, only what names them does
	// ([NotifyMentions]).
	Every []string `json:"every,omitempty"`

	// Starred are the channels this reader starred, sorted: where the room
	// sits on their own screens, and nothing for anybody else.
	Starred []string `json:"starred,omitempty"`

	// Saved are the messages this reader saved for later, the newest save
	// first. See chatmarks.go.
	Saved []SavedMessage `json:"saved,omitempty"`

	// Threads are the threads this reader followed or unfollowed by hand,
	// the newest mark first ([ThreadMark]).
	Threads []ThreadMark `json:"threads,omitempty"`

	// DNDUntil is when do-not-disturb ends; the zero instant means off. An
	// instant, not a bool, so it ends by itself rather than stranding a
	// person silenced by a toggle they forgot.
	DNDUntil time.Time `json:"dnd_until,omitzero"`

	// UpdatedAt is diagnostic: nothing branches on it, but it answers
	// "since when" without the broker's metadata.
	UpdatedAt time.Time `json:"updated_at,omitzero"`

	// Revision is the store's revision of the record this state was read or
	// written at; zero for a person with no record.
	//
	// It orders two states of one person: answers and live pushes travel
	// separately, so a screen can get an older state after a newer one and
	// must keep the newer, and UpdatedAt is each node's own instant, not the
	// store's order. Strictly increasing for the life of the store, so a
	// record forgotten and written again moves past every state the old one
	// reported. Never stored: a record cannot know the revision it will be
	// written at, so a backend stamps it on every state it answers.
	Revision uint64 `json:"revision,omitempty"`

	// AbsentSince is when the reconcile first found this record's seat
	// missing from the company; zero while the company has it. Written only
	// by [ChatReads.SetChatReadAbsence]: a flush is evidence about one
	// node's revision, the reconcile reads the fleet's.
	AbsentSince time.Time `json:"absent_since,omitzero"`
}

// PersonChatRead is one person's read state NAMED BY ITS SEAT, as a client is
// sent it: the live channel's `chat_cursor_moved` push and the rail's `read`.
//
// The name makes the revision usable: [ChatReadState.Revision] orders states
// of ONE record (on the coordination KV it is the bucket's sequence, shared by
// every seat), so a client whose seat changed (a tab handed to another reader,
// a reconnect after a seat move) must REPLACE the state rather than drop the
// newcomer's as older. Never stored: the seat is the record's key, so a
// surface handing a state out stamps the seat it read it for.
type PersonChatRead struct {
	// Handle is the seat whose record this is.
	Handle string `json:"handle"`
	ChatReadState
}

// WithAbsence is the state with its absence stamp set to since, reporting
// whether that changed anything: the one rule for the stamp, shared by both
// backends. A non-zero since KEEPS an earlier stamp, because the grace runs
// from the FIRST sweep that saw the seat missing (restamping would restart it
// every tick and forget nobody); the zero instant clears it.
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

// ChatReadDelta is what one flush changes. Every field is optional and absent
// means unchanged, so two tabs flushing different facts erase neither's.
type ChatReadDelta struct {
	// Cursors are advanced, never set: a value at or below what is stored
	// is ignored ([advanceCursors]).
	Cursors map[string]int64

	// Mute adds rooms to the muted set and Unmute takes them out, a room at
	// a time (see "Why advancing is a merge, and a mute is a room"). A
	// repeat is a no-op; one room in both is refused ([ErrInvalidReadState]):
	// two opposite facts with no order between them.
	Mute   []string
	Unmute []string

	// ThreadCursors are advanced, never set, as [ChatReadDelta.Cursors] are,
	// keyed by a thread's root message id.
	ThreadCursors map[string]int64

	// Rewind SETS a room's cursor to exactly the position named: "mark
	// unread", the one way a cursor moves back. Zero forgets it, so the
	// whole room reads unread. A room also in Cursors is refused.
	Rewind map[string]int64

	// Notify sets how each named room reaches the reader ([NotifyLevel]),
	// moving it between [ChatReadState.Every] and [ChatReadState.Muted].
	// A room named here and in Mute or Unmute is refused.
	Notify map[string]NotifyLevel

	// Star and Unstar mark rooms starred or not, a room at a time.
	Star   []string
	Unstar []string

	// Save and Unsave keep messages for later or let them go, a message at
	// a time. A save of a message already saved keeps its first instant.
	Save   []MessageRef
	Unsave []MessageRef

	// Follow and Unfollow mark threads by hand ([ThreadMark]).
	Follow   []ThreadRef
	Unfollow []ThreadRef

	// DNDUntil replaces when non-nil, including with the zero instant,
	// which is how do-not-disturb is cleared.
	DNDUntil *time.Time

	// At stamps the record, passed in because every write in this package
	// takes its instant from the caller: one clock read here would make its
	// tests serial.
	At time.Time
}

// ChatReads is the company's per-person chat read state, one record per human
// seat (the comments atop chatread.go). Its bucket has no age: an aged-out
// cursor would mark a room unread.
type ChatReads interface {
	// ChatRead reports one person's read state. No record is the ZERO
	// STATE and no error: never having opened chat is the ordinary case,
	// not a failure.
	ChatRead(ctx context.Context, handle string) (ChatReadState, error)

	// AdvanceChatRead merges a flush into one person's record and reports
	// the resulting state. The merge is idempotent ([MergeChatRead]), so two
	// tabs racing produce one record and the store's retry loses no flush.
	// Past [MaxReadCursors] the stalest cursor is dropped.
	//
	// Three failures, by sentinel: [ErrInvalidReadState], refused the same
	// way every time; [ErrWriteUnknown], sent and never answered, which may
	// have landed; any other error (a failed read, every compare-and-set
	// lost) changed nothing. Resending is safe after the last two.
	AdvanceChatRead(ctx context.Context, handle string, delta ChatReadDelta) (ChatReadState, error)

	// SetChatReadAbsence stamps when a record's seat was first found missing
	// from the company or, with the zero instant, clears the stamp
	// ([ChatReadState.WithAbsence]). Reports whether the seat has a record:
	// one without is left without, never created for a seat nobody read as.
	// A compare-and-set, so a flush beside it loses neither; its errors are
	// [ChatReads.AdvanceChatRead]'s.
	SetChatReadAbsence(ctx context.Context, handle string, since time.Time) (bool, error)

	// ForgetChatRead removes a person's record only if stamped absent at or
	// before absentBy ([ChatReadState.AbsentBy]), reporting whether it did:
	// the ONLY way a record leaves the ageless bucket. The condition is read
	// from the record the write finds, not the caller's listing, so a stamp
	// a peer just cleared keeps it. False with no error is that answer or no
	// record; [ErrWriteUnknown] marks a removal that may have happened.
	ForgetChatRead(ctx context.Context, handle string, absentBy time.Time) (bool, error)

	// ChatReaders lists every seat holding read state, each with its absence
	// stamp: how the reconcile finds records of seats the chart no longer
	// names. It RAISES rather than answer short, on an unreachable store or
	// an undecodable record: a reader left out is never forgotten.
	ChatReaders(ctx context.Context) ([]ChatReader, error)

	// WatchChatReads streams every record that exists, then the
	// [ChatReadUpdate.Current] marker, then every change: a flush, an
	// absence stamp, a forget.
	//
	// The live channel derives from it: every node serving sockets watches
	// and pushes a changed record to that seat's sockets on it, so a push
	// comes from the durable record, never on a peer's word. The channel
	// closes when ctx ends or the watch fails, and a CLOSED CHANNEL IS NOT A
	// FORGET: watch again, and every record is redelivered at its revision
	// before the next marker.
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
	// started; every update after it is a change. A consumer re-syncs from
	// what precedes it, the only point at which the watch can say a record
	// it has not delivered does not exist.
	Current bool
}

// MergeChatRead applies a flush to a stored state and reports the result.
//
// The one implementation, shared by both backends: written twice, the two
// would first differ on a delta arriving out of order. Pure, so the suite
// exercises it without a store; idempotent, so a lost compare-and-set is safe
// to retry. The result carries no [ChatReadState.Revision]: the store stamps
// it once the write lands.
func MergeChatRead(state ChatReadState, delta ChatReadDelta) (ChatReadState, error) {
	out := ChatReadState{
		DNDUntil:    state.DNDUntil,
		UpdatedAt:   state.UpdatedAt,
		AbsentSince: state.AbsentSince,
	}
	cursors, err := advanceCursors(state.Cursors, delta.Cursors,
		"a read cursor needs a channel")
	if err != nil {
		return ChatReadState{}, err
	}
	// A rewind SETS, after every advance; an advance of the same room in
	// the flush is a second fact about one place, refused rather than ordered.
	for _, channel := range slices.Sorted(maps.Keys(delta.Rewind)) {
		at := delta.Rewind[channel]
		_, advanced := delta.Cursors[channel]
		switch {
		case channel == "":
			return ChatReadState{}, fmt.Errorf("%w: a rewind needs a channel; "+
				"the flush named one with no id, which is a client that lost "+
				"the value on the way here", ErrInvalidReadState)
		case at < 0:
			return ChatReadState{}, fmt.Errorf("%w: %s is rewound to %d, and a "+
				"position is never below zero", ErrInvalidReadState, channel, at)
		case advanced:
			return ChatReadState{}, fmt.Errorf("%w: %s is both advanced and "+
				"rewound in one flush; send the one that is meant",
				ErrInvalidReadState, channel)
		case at == 0:
			delete(cursors, channel)
		default:
			cursors[channel] = at
		}
	}
	evictStaleCursors(cursors)
	out.Cursors = nonEmptyCursors(cursors)

	threadCursors, err := advanceCursors(state.ThreadCursors, delta.ThreadCursors,
		"a thread cursor needs a thread")
	if err != nil {
		return ChatReadState{}, err
	}
	evictStaleCursors(threadCursors)
	out.ThreadCursors = nonEmptyCursors(threadCursors)

	if out.Muted, out.Every, err = mergeLevels(state.Muted, state.Every,
		delta.Mute, delta.Unmute, delta.Notify); err != nil {
		return ChatReadState{}, err
	}
	if out.Starred, err = mergeRoomSet(state.Starred, delta.Star, delta.Unstar,
		MaxReaderMarks, starWords); err != nil {
		return ChatReadState{}, err
	}
	if out.Saved, err = mergeSaved(state.Saved, delta.Save, delta.Unsave,
		delta.At); err != nil {
		return ChatReadState{}, err
	}
	if out.Threads, err = mergeThreadMarks(state.Threads, delta.Follow,
		delta.Unfollow, delta.At); err != nil {
		return ChatReadState{}, err
	}
	if delta.DNDUntil != nil {
		out.DNDUntil = delta.DNDUntil.UTC()
	}
	if !delta.At.IsZero() {
		out.UpdatedAt = delta.At.UTC()
	}
	return out, nil
}

// nonEmptyCursors is a cursor map in its one shape for "none": the suite
// compares records across both backends, and an empty map and a nil one
// encode differently.
func nonEmptyCursors(cursors map[string]int64) map[string]int64 {
	if len(cursors) == 0 {
		return nil
	}
	return cursors
}

// evictStaleCursors bounds the map at [MaxReadCursors] by dropping the LOWEST
// positions: the log is totally ordered, so the cursor furthest back is the
// room read least recently, with no per-channel timestamp to store.
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
	// Position, then channel id: two nodes evicting the same record drop
	// the same cursor, whatever Go's map iteration order.
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
