package inbox

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// MaxEntries bounds each of a person's three lists. Entries the seen-through
// position already answers are pruned on every write ([Apply]), which is what
// keeps a record small rather than a cap that discards.
const MaxEntries = 256

// MaxSnoozeAhead bounds how far into the future a notice may be snoozed.
//
// A YEAR. A snooze is "not now", and one past the horizon a person plans over
// is a delete that does not say so — the notice leaves the inbox and the
// person who snoozed it will not be at the company when it returns.
const MaxSnoozeAhead = 365 * 24 * time.Hour

// MaxRows is how many notices one page of an inbox carries.
//
// FIFTY, against an activity feed's two hundred, because the two answer
// different questions. A feed is scrolled and an inbox is WORKED: a person or
// an agent reads it to decide what to do next, and a page longer than one
// sitting is a page whose tail is never reached. It is also the bound on what
// one turn's prompt can carry a summary of without displacing the work.
const MaxRows = 50

// ErrFull reports a gesture that would leave one of a person's lists past
// [MaxEntries].
//
// ITS OWN SENTINEL, because the remedy is a different gesture rather than a
// smaller one: the lists hold only what the seen-through position does not
// already answer, so a full one is a person marking notices one at a time
// that `read_through` would mark in one move — and a caller told "invalid"
// would go looking for what is wrong with the notice.
var ErrFull = errors.New("inbox: this inbox list is at its ceiling")

// ErrInvalid marks a gesture these rules refuse on what it SAYS: the same
// gesture can never land, and a different one can.
//
// A DOMAIN'S WRITER CLASSES IT AS ITS OWN content refusal, with the sentence
// unchanged ([Sentence]): the gesture is a write on that domain's log, and
// every surface that classes the domain's refusals — a tool's, a route's —
// has to class this one with them, rather than learn a second vocabulary for
// one verb.
var ErrInvalid = errors.New("inbox: invalid gesture")

// Refusal is a gesture these rules refuse: the sentinel that classes it and
// the sentence a person reads, without the package prefix [Refusal.Error]
// adds.
//
// EXPORTED, FIELDS AND ALL, because a mark is decided on the data node whose
// copy holds the person's inbox and its refusal crosses the estate router's
// wire (internal/estate) to the node whose caller is told: the wire rebuilds an
// error TYPE from its exported fields — Mark as a nested wire error, so it
// still answers errors.Is — and an unexported one arrived as its message
// alone, so [Sentence] on the asking node found no refusal and handed its
// reader the whole "inbox: …". Only this package builds one.
type Refusal struct {
	// Mark is the sentinel that classes the refusal: [ErrInvalid] or
	// [ErrFull].
	Mark error
	// Why is the sentence, without the package prefix.
	Why string
}

func (e *Refusal) Error() string { return "inbox: " + e.Why }
func (e *Refusal) Unwrap() error { return e.Mark }

// invalid is a refusal marked [ErrInvalid].
func invalid(format string, args ...any) error {
	return &Refusal{Mark: ErrInvalid, Why: fmt.Sprintf(format, args...)}
}

// full is a refusal marked [ErrFull].
func full(format string, args ...any) error {
	return &Refusal{Mark: ErrFull, Why: fmt.Sprintf(format, args...)}
}

// Sentence is a refusal of these rules as a person reads it: its sentence,
// without the package prefix its Go error carries. Any other error — one these
// rules did not write, or a refusal a caller wrapped in context of its own —
// is its whole message, because context is never dropped to tidy a prefix
// away.
func Sentence(err error) string {
	var r *Refusal
	if errors.As(err, &r) && r.Error() == err.Error() {
		return r.Why
	}
	return err.Error()
}

// Position is where a person's inbox has been read to on one domain's log.
//
// THE TRIPLE, not a bare sequence: a recreated stream restarts sequences at
// one, so a stored number from before it compares as current and an inbox
// reads "nothing unread" for ever.
//
// # The generation is stored, and omitted when it is zero
//
// A domain keeps it PACKED in its column, as every durable position is kept,
// so a reanchor compares correctly: a position kept as the bare sequence would
// read back as generation zero after one, and every notice in the new
// generation would compare above it — an inbox that could never be read past
// again. A zero generation is omitted from the JSON, which keeps a position in
// a stream's first generation to its two meaningful keys.
type Position struct {
	Stream     string `json:"stream"`
	Generation uint32 `json:"generation,omitempty"`
	Seq        uint64 `json:"seq"`
}

// Packed is the position as the one integer a column stores and a comparison
// reads — [statelog.Position.Packed] over the same two numbers.
func (p Position) Packed() uint64 {
	return uint64(statelog.Position{Generation: p.Generation, Seq: p.Seq}.Packed())
}

// Entry is one notice on a person's lists, with the position it sits at.
//
// Position is PACKED, `(generation << 40) | seq`, the form every durable
// position takes — so an entry from before a reanchor compares below one
// after it instead of as a small number in the same space. A domain's writer
// finds it from the notice's own row inside its decide rather than taking it
// from the caller; see [Apply].
type Entry struct {
	RecordID string     `json:"record_id"`
	Position uint64     `json:"position"`
	Until    *time.Time `json:"until,omitempty"`
}

// Marks is a person's own state over ONE domain's notices: how far they have
// read, the notices they marked out of order or put off, and which of that
// domain's reasons are theirs to act on. R is the domain's reason.
//
// A MARK OVER THE FEED, NEVER THE FEED: an [Entry] carries a record id and a
// position and no content at all. The notices are the domain's rows, which
// its applier — which outlives every writer — wrote; these are what the
// person did about them. That split is what makes an inbox survive a person
// who never writes: somebody who has marked nothing still has an inbox.
//
// # The lists are overrides of the position, in opposite directions
//
// Everything at or below [Marks.SeenThrough] reads as read and everything
// above it as unread, so a list entry is only ever an EXCEPTION: Read holds
// notices above the position somebody marked read out of order, and Unread
// notices at or below it somebody marked unread again. An entry on the side
// the position already answers says nothing, and [Apply] drops it on every
// write.
type Marks[R ~string] struct {
	// Generation is anchored to the stream's own identity, and it is the
	// THIRD belt behind the identity refusal and the wrong-stream read
	// refusal. It stays because the failure it was written for — a
	// founder's inbox silently reading empty for ever — is one nobody
	// would report as a bug.
	Generation string `json:"generation,omitempty"`

	// SeenThrough is a POSITION TRIPLE rather than a number, because a
	// number from a dead sequence space compares as if it were current.
	SeenThrough Position `json:"seen_through,omitzero"`

	Read    []Entry `json:"read,omitempty"`
	Unread  []Entry `json:"unread,omitempty"`
	Snoozed []Entry `json:"snoozed,omitempty"`

	// PrimaryReasons is the person's own answer to "which of these are
	// mine to act on". EMPTY IS "I HAVE NOT SAID", never "nothing is
	// primary": a domain answers it with its shipped default.
	PrimaryReasons []R `json:"primary_reasons,omitempty"`
}

// Snooze is one notice put off until an instant.
type Snooze struct {
	RecordID string
	Until    time.Time
}

// Gesture is one move a person makes on their own inbox. R is the domain's
// reason.
//
// # A GESTURE, never the lists
//
// An inbox used to be written by handing over all three lists and the
// watermark, each REPLACING what was stored — so a screen marking one notice
// read had to send everything it had last read, and anything it had not read
// was cleared: one `read` from a tab opened before a snooze erased the
// snooze, and a caller that sent only the notice it meant wiped every other
// mark and the position with it. What a person can say about their inbox is
// "this one is done", "put this one off", "I have read everything up to
// here" — so that is what a gesture says, and a domain's writer resolves it
// ([Apply]) against the record its DECIDE reads, which is the one snapshot the
// broker arbitrates. Two screens marking two notices at once both land.
//
// A record id appears in at most ONE of the four lists: a notice both read
// and unread in one gesture is a contradiction nothing could order.
type Gesture[R ~string] struct {
	// Read marks notices read — the "Done" of an inbox. It also lifts a
	// snooze on the notice, because a notice somebody has dealt with is
	// not one to bring back.
	Read []string

	// Unread marks notices unread again, including one at or below the
	// seen-through position: that is the only case the mark changes
	// anything, since everything above the position is unread already.
	Unread []string

	// Snooze puts notices off until an instant, at most [MaxSnoozeAhead]
	// away. A notice already snoozed moves to the new instant.
	Snooze []Snooze

	// Unsnooze brings notices back now.
	Unsnooze []string

	// ReadThrough moves the seen-through position FORWARD: everything at
	// or below it reads as read. ADVANCE ONLY, and generation-aware — a
	// position at or behind the stored one changes nothing, so a stale
	// tab's "mark all read" cannot un-read what a newer one read. Nil
	// leaves the position alone.
	ReadThrough *statelog.Position

	// PrimaryReasons declares which reasons are the person's to act on.
	// Nil leaves the declaration alone; an EMPTY list takes the domain's
	// shipped default back.
	PrimaryReasons *[]R
}

// Empty reports a gesture that would move nothing.
func (g Gesture[R]) Empty() bool {
	return len(g.Read) == 0 && len(g.Unread) == 0 && len(g.Snooze) == 0 &&
		len(g.Unsnooze) == 0 && g.ReadThrough == nil && g.PrimaryReasons == nil
}

// Records is every notice the gesture names, in the order it names them: what
// a domain's writer finds the position of inside its decide, for [Apply].
func (g Gesture[R]) Records() []string {
	out := slices.Concat(g.Read, g.Unread, g.Unsnooze)
	for _, snooze := range g.Snooze {
		out = append(out, snooze.RecordID)
	}
	return out
}

// CheckGesture refuses a gesture no record could honour, before anything is
// read.
//
// stream is the log the domain files its notices on, so the only one an inbox
// is read through; reasons are the reasons it routes under, so the only ones
// a person may declare primary. Every refusal is of g's CONTENT: [ErrFull] for
// a gesture naming more notices than a list holds, and [ErrInvalid] for every
// other, each written as the sentence a person reads ([Sentence]).
func CheckGesture[R ~string](g Gesture[R], stream string, reasons []R,
	now time.Time) error {

	if g.Empty() {
		return invalid("this inbox gesture marks nothing — name " +
			"notices to read, unread, snooze or unsnooze, a position to read " +
			"through, or the primary reasons")
	}
	seen := map[string]string{}
	name := func(id, list string) error {
		if strings.TrimSpace(id) == "" {
			return invalid("a %s mark names no record", list)
		}
		if prior, twice := seen[id]; twice {
			return invalid("record %s is named by both %s and %s in "+
				"one gesture, and nothing could say which it meant", id, prior,
				list)
		}
		seen[id] = list
		return nil
	}
	for _, part := range []struct {
		ids  []string
		list string
	}{{g.Read, "read"}, {g.Unread, "unread"}, {g.Unsnooze, "unsnooze"}} {
		for _, id := range part.ids {
			if err := name(id, part.list); err != nil {
				return err
			}
		}
	}
	// A SNOOZE PAST THE HORIZON IS A DELETE THAT DOES NOT SAY SO: the notice
	// leaves the inbox and the person who snoozed it will not be at the
	// company when it returns. Refused naming the entry rather than
	// clamped, because a clamp would put it back on a date nobody chose.
	// And one that is already due is not a snooze at all.
	horizon := now.Add(MaxSnoozeAhead)
	for _, snooze := range g.Snooze {
		if err := name(snooze.RecordID, "snooze"); err != nil {
			return err
		}
		switch {
		case !snooze.Until.After(now):
			return invalid("record %s is snoozed until %s, which has "+
				"already come — a snooze names an instant in the future",
				snooze.RecordID, snooze.Until.Format(time.RFC3339))
		case snooze.Until.After(horizon):
			return invalid("record %s is snoozed until %s, which is "+
				"more than %s away — a snooze that far out is a delete that "+
				"does not say so", snooze.RecordID,
				snooze.Until.Format(time.RFC3339), MaxSnoozeAhead)
		}
	}
	if len(seen) > MaxEntries {
		// A GESTURE LONGER THAN A LIST CAN HOLD cannot land whatever the
		// record holds, so it is refused before the decide reads it.
		return full("this gesture names %d notices and a list holds at "+
			"most %d — mark everything you have seen with `read_through` "+
			"instead", len(seen), MaxEntries)
	}
	if at := g.ReadThrough; at != nil {
		if err := at.Valid(); err != nil {
			return invalid("%s is not a position to read through: %v", at, err)
		}
		// THE DOMAIN'S OWN LOG, which is where every notice a person reads
		// through is filed: a position on any other stream is in a number
		// space this inbox does not count in.
		if at.Stream != stream {
			return invalid("%s is a position on %s, and this inbox is read "+
				"through a position on %s — the log its notices are filed on",
				at, at.Stream, stream)
		}
	}
	if g.PrimaryReasons != nil {
		for _, reason := range *g.PrimaryReasons {
			if !slices.Contains(reasons, reason) {
				return invalid("%q is not a wake reason", reason)
			}
		}
	}
	return nil
}

// Apply moves a person's marks by one gesture.
//
// PURE OVER VALUES — the marks, the gesture, where each notice sits and the
// instant — so the rule is exercised without a database, and a domain's
// decide is its only caller. positions holds the PACKED position of every
// notice the gesture names ([Gesture.Records]), which the domain reads from
// the notice's own row in that decide: a mark has to carry the position its
// notice sits at, because the lists are pruned against the seen-through
// position, and a caller that supplied it could get it wrong in the one way
// that loses the mark silently — a read mark at position zero is pruned on
// the write that makes it.
//
// Moving the position forward clears every `unread` exception, because "I
// have read everything up to here" is the gesture that says so. The ORDER:
// the position first, then the marks, so "read through here, but keep this
// one unread" is one gesture that means what it says.
//
// A list the gesture would leave past [MaxEntries] is refused [ErrFull]
// rather than trimmed. Every list is a FRESH slice, never the one m held, so
// a caller comparing the marks before and after with reflect.DeepEqual
// compares two values rather than one.
func Apply[R ~string](m *Marks[R], g Gesture[R], positions map[string]uint64,
	at time.Time) error {

	read := slices.Clone(m.Read)
	unread := slices.Clone(m.Unread)
	snoozed := slices.Clone(m.Snoozed)

	if g.ReadThrough != nil {
		next := Position{Stream: g.ReadThrough.Stream,
			Generation: g.ReadThrough.Generation, Seq: g.ReadThrough.Seq}
		if advances(m.SeenThrough, next) {
			m.SeenThrough = next
			// THE STREAM'S OWN IDENTITY rides beside the position — the
			// third belt [Marks.Generation] describes.
			m.Generation = next.Stream
			unread = nil
		}
	}
	if g.PrimaryReasons != nil {
		m.PrimaryReasons = nilIfEmpty(slices.Clone(*g.PrimaryReasons))
	}

	entry := func(id string) Entry {
		return Entry{RecordID: id, Position: positions[id]}
	}
	for _, id := range g.Read {
		unread = withoutEntry(unread, id)
		snoozed = withoutEntry(snoozed, id)
		read = append(withoutEntry(read, id), entry(id))
	}
	for _, id := range g.Unread {
		read = withoutEntry(read, id)
		unread = append(withoutEntry(unread, id), entry(id))
	}
	for _, snooze := range g.Snooze {
		until := snooze.Until.UTC()
		mark := entry(snooze.RecordID)
		mark.Until = &until
		snoozed = append(withoutEntry(snoozed, snooze.RecordID), mark)
	}
	for _, id := range g.Unsnooze {
		snoozed = withoutEntry(snoozed, id)
	}

	seen := m.SeenThrough.Packed()
	m.Read = keptEntries(read, func(e Entry) bool { return seen == 0 || e.Position > seen })
	m.Unread = keptEntries(unread, func(e Entry) bool { return seen != 0 && e.Position <= seen })
	// AN EXPIRED SNOOZE IS NOT AN ENTRY, it is a notice that is back — and
	// nothing needs to move it, because a notice with no live snooze reads
	// by the position and the two lists like any other.
	m.Snoozed = keptEntries(snoozed, func(e Entry) bool { return live(e.Until, at) })

	for _, part := range []struct {
		entries []Entry
		what    string
	}{{m.Read, "read"}, {m.Unread, "unread"}, {m.Snoozed, "snoozed"}} {
		if len(part.entries) > MaxEntries {
			// THE LIST AND NOT THE PERSON: the caller named whose inbox
			// this is in the call.
			return full("this inbox's %s list would hold %d notices and the "+
				"maximum is %d — mark everything you have seen with "+
				"`read_through`, which moves the position the lists are kept "+
				"against", part.what, len(part.entries), MaxEntries)
		}
	}
	return nil
}

// advances reports whether next is a position past the stored one.
//
// GENERATION FIRST, for the reason [statelog.Position.Before] states: a
// sequence from before a reanchor is below every sequence after it however
// large. A position on ANOTHER STREAM than the stored one replaces it, because
// the caller's was checked against the live stream ([CheckGesture]) and the
// stored one is therefore from a stream that no longer exists — comparing
// their numbers would be comparing two unrelated spaces.
func advances(stored, next Position) bool {
	if stored.Stream != next.Stream || stored.Seq == 0 {
		return true
	}
	return next.Packed() > stored.Packed()
}

// live reports a snooze still asleep at now: no instant, or one after now.
//
// ONE TEST FOR EVERY READER OF A SNOOZE — the prune [Apply] runs, the mark a
// page carries ([Classifier.Classify]) and the set a domain's scan hides
// ([Classifier.Asleep]) — so a notice the scan hides is exactly a notice the
// page would have marked snoozed, and one whose time has come is back
// everywhere at once.
func live(until *time.Time, now time.Time) bool {
	return until == nil || until.After(now)
}

// withoutEntry is the list with every entry for one record dropped, as a fresh
// slice.
func withoutEntry(entries []Entry, id string) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.RecordID != id {
			out = append(out, e)
		}
	}
	return out
}

// keptEntries is the entries a rule keeps, nil when none are.
func keptEntries(entries []Entry, keep func(Entry) bool) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	return nilIfEmpty(out)
}

// nilIfEmpty is the stored form of an empty list: absent, so marks that hold
// nothing encode exactly like marks nobody wrote.
func nilIfEmpty[T any](in []T) []T {
	if len(in) == 0 {
		return nil
	}
	return in
}
