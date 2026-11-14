package executor

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
)

// WHICH RECORDS OF A THREAD CLAUDE CODE'S RESUME HANDS ITS MODEL, as the
// reading of a kept session walks them ([holdingsChain]): its own rule, read in
// 2.1.310's and 2.1.322's shipped code, never a rule of the reading's.
//
// A RESUME HANDS ITS MODEL A CHAIN, never the file. It walks from one record,
// the LEAF, to the record its `parentUuid` names, and on, until a parent that
// is null, the empty string or absent ends the walk, and reverses what it
// walked. Off that chain it hands over only what a reply on it recovers. The
// reading counts two of those recoveries: the reply's other records, found by
// their `message.id`, which every record of one reply carries, and the results
// whose parent is one of them, which is how the results of calls made at once,
// each chained to its own call, reach the model though the thread's last
// record descends from one of them alone (probe T's trees, both releases). The
// resume recovers more, and every record it recovers so the reading leaves
// told, the safe direction: a reply's tails (the attachments, system records
// and meta user records hanging off its records), and, behind a flag on by
// default, a result through the call its id names where that call's record is
// not its parent. None of them reaches a record of another reply: so an
// attempt the harness withdrew and failed to remove from the file, which no
// later record descends from, holds a call no continuation hands over, and the
// reading holds none of its calls.
//
// A RECORD IS HANDED ([holdingsChain.handed]) where it lies past its thread's
// last restart and is on the chain from the leaf, or is a reply's record whose
// id is that of a reply on the chain past the last restart, or is a user
// record of results whose parent is such a record. Past the last restart,
// since a compaction that kept a segment deletes every record before its
// boundary from what the resume walks, so nothing reached through one is sure
// to be handed; and the walk stops at the last restart for that reason.
//
// AND NO RECORD A RETRACTION NAMES IS HANDED ([threadLine.Retracted]): once it
// has walked the chain and recovered what it recovers, the resume drops every
// record but a system one whose uuid's first 24 characters are those of a
// uuid a record names as taken back (a refused reply's fallback, a malformed
// reply's retry), on the chain or beside it. The reading reads those names
// off every record of the thread ([holdingsChain.retract]), where the resume
// reads them off the records it hands over alone, so it takes away no less
// than the resume does.
//
// A CALL IS ANSWERED ([holdingsChain.answers]) only where the resume hands its
// model the call's use and then its result, since the API takes a result only
// after its call, and a result the harness's own repair finds before its call
// it drops: both on the chain, in the chain's order, which is the file's
// below the walk's first step to a parent written after its record; or the
// result recovered beside the use's own reply, which the resume places after
// every record of that reply on the chain. It places a recovered record after
// its reply's records and after the results and the bookkeeping that follow
// them there, so a use it recovers can come after its own result on the
// chain, and the reading holds no call of that shape (no harness writes one:
// a result's parent is its call's own record).
//
// THE LEAF of a main thread is the nearest user or assistant record up from
// its last record, the leaf Claude Code's resume starts from wherever nothing
// else moves it, which the reading holds to be so ([holdingsWalk.mainEnd]):
//
//   - a checkpoint (a last-prompt record) the session's last record does not
//     descend from moves it there, and one a rewind, a withdrawal or a fork
//     marked moves it wherever it names, or to nothing: each is no reading;
//   - where a compaction kept a stretch of the conversation, a resume RELINKS
//     the thread by its last boundary ([threadLine.Preserved],
//     [holdingsChain.relinks]): it chains the kept records after the summary
//     they hang from (the anchor), hangs the anchor's other children from the
//     last of them, and drops every other record before that boundary; and
//     then picks its leaf among the user and assistant records no other such
//     record names as its parent, the checkpoint first where it is one. So a
//     checkpoint that is a user or assistant record other than the leaf is
//     taken for the leaf, the earlier of the two the pick can land on,
//     wherever it can be one of those ([holdingsChain.mayLead]): where the
//     thread relinks and the checkpoint lies before the boundary, is the
//     anchor, or is named as its parent by no user or assistant record after
//     the boundary, which keeps naming it through the relink. Elsewhere the
//     resume starts from the leaf, which descends from the checkpoint through
//     records no compaction drops. Every checkpoint of a session cut off
//     mid-turn lags its last record, naming a record its next turn descends
//     from: taken for the leaf wherever the thread holds a boundary, it told
//     every call of that turn (the chain critique's C2). A relink that keeps a
//     record at or after its boundary is no reading;
//   - a record the resume keeps out of a session's last record's place (a
//     sidechain's, a fork's briefing, a file's snapshot), an assistant record
//     whose content is a string, which its load drops where it is blank and
//     rewrites where it is not, and a user or assistant record whose message
//     does not decode, which its load drops, are each no reading. No main
//     thread the probes kept holds any of them.
//
// A SUBAGENT'S THREAD is walked from its last record of its own in the file
// ([holdingsWalk.subagentEnd]): rule 2 asks what its model had seen when it
// wrote the reply its answer carries, which is the conversation its last
// record continues. Its walk must reach the thread's root; one that breaks
// leaves the thread not whole.
//
// WHERE THE WALK CANNOT BE READ, THE READING TELLS. A parent naming no record
// ends a main thread's walk, and what lies past it is told: the harness takes
// a parent by timestamp there, which only lengthens its chain, so the records
// walked to the break are handed either way. A parent written after its
// record, and a record naming itself, on a main thread's walk are no reading:
// past the first, the chain's order would no longer be the file's, which the
// reading compares a call's use and its result by; and a resume that relinks
// a compacted thread picks its leaf among the user and assistant records no
// such record names as its parent, which a record naming itself is not, so it
// may start from none at all. In a subagent's thread a parent naming no
// record, or a record the walk already took, leaves the thread not whole. A
// record of
// no lowercase canonical uuid, a uuid held twice, or more records than
// [holdingsMaxRecords] is no reading of a main thread and leaves a subagent's
// not whole; and so is a progress record, a legacy shape, which the
// harness's load drops, handing each record after it that names its uuid to
// the progress record's own parent ([recordProgress]): followed as written, a
// record naming one could read as the child of a record carrying the same
// uuid, which that load skips. And recovery is read only through a result's
// parent: the harness also recovers a result through the call its id names,
// behind a flag, which the reading leaves told.
//
// A subagent's thread can name a parent that comes later in the file (the
// attachments of its start: 74 of the 89 subagent threads the probes kept),
// so a parent is looked up once the thread is read whole, in a main thread
// too, where such a parent off its walk costs nothing.

// holdingsMaxRecords is the most user, assistant, attachment and system
// records of one thread a reading walks the chain of, and of the uuids those
// records retract or its last compaction keeps that no record before that
// compaction's boundary carries, counted together ([holdingsChain.room]).
//
// 262,144, TWICE THE LINES A THREAD AT THE PLAINTEXT'S BOUND HOLDS: 256 MiB
// ([threadPlainMax]) of lines of 2,053 bytes, a real main thread's average
// (probe P6), is 130,754, and a session at the archive's bound inflates to
// about 118 MiB. One thread's records are held at a time, each a uuid's place
// in a map, a parent and its marks, the uuid of a parent that came later, and
// each reply's id as a digest: at the bound, every record a reply of its own
// id naming a parent that comes later, 35 MiB live, its heap peaking at 69 MiB
// (BenchmarkAReadingAtItsBounds, on two cores). A uuid a record retracts is
// held as its first ten bytes in a set, and one a compaction keeps as its 16
// in another, each less than any record costs, so a thread at the bound that
// holds either holds no more than that (computed, not measured). Past it, a
// main thread is no reading and a subagent's thread not whole.
const holdingsMaxRecords = 1 << 18

// What a record's parent is, where it is no record of the thread
// ([holdingsChain.parent]).
const (
	// chainRoot is a parent that ends the walk: null, the empty string or
	// absent, each falsy to the harness's walk.
	chainRoot int32 = -1
	// chainNowhere is a parent naming no record of the thread.
	chainNowhere int32 = -2
	// chainLater is a parent not read when its child was, looked up once the
	// thread is read whole ([holdingsChain.resolve]).
	chainLater int32 = -3
)

// What a record is to the walk ([holdingsChain.marks]).
const (
	// markUserOrAssistant is a user or assistant record: the leaf of a main
	// thread is the nearest one up from its last record.
	markUserOrAssistant uint8 = 1 << iota
	// markReply is a counted assistant record whose reply's id was read
	// ([holdingsChain.replies]).
	markReply
	// markResults is a user record holding a call's result.
	markResults
	// markRetracted is a record whose uuid a retraction names
	// ([holdingsChain.retract]): handed to no model, wherever it lies.
	markRetracted
	// markTurnParent is a record a user or assistant record written after
	// the thread's last compaction's boundary names as its parent: no leaf a
	// resume that relinks the thread can pick ([holdingsChain.mayLead]).
	markTurnParent
)

// errRelinkLater is a compaction's boundary whose relink keeps a record
// written at or after it ([holdingsChain.relinks]).
var errRelinkLater = errors.New("its last compaction's boundary keeps a record written at or after it, which a " +
	"resume's relink would chain under the records the reading walks")

// errChainBound is a thread holding more records than a reading walks the
// chain of ([holdingsMaxRecords]).
var errChainBound = errors.New("its thread holds more records than a reading walks the chain of")

// holdingsChain is one thread's records as the walk reads them: by uuid, each
// with its parent and its marks, and each counted reply's id as a digest.
type holdingsChain struct {
	// bound is the most records it files ([holdingsMaxRecords]).
	bound int
	// index is each record by its uuid, kept until the thread is read whole.
	index map[[16]byte]int32
	// parent is each record's parent: a record, or chainRoot, chainNowhere or
	// chainLater; marks each record's marks.
	parent []int32
	marks  []uint8
	// later are the parents not read when their child was.
	later []chainLaterParent
	// retracted are the first 24 characters of each uuid a record of the
	// thread retracts that a record's uuid can begin with, as the ten bytes
	// they write ([holdingsChain.retract]), kept until the thread is read
	// whole and its records marked by them.
	retracted map[[10]byte]struct{}
	// relink is what the thread's last compaction's boundary says a resume
	// relinks it by ([threadLine.Preserved]), filed as its line is read and
	// decided once the thread is read whole ([holdingsChain.relinks]).
	relink chainRelink
	// replies are the counted replies' records and their ids' SHA-256, in
	// record order; a reply's id is the box's word, as long as a line, and two
	// ids are the same reply only where the harness compares them equal, which
	// holds of every id filed: [threadLine.MessageID] hands out none holding
	// the replacement character the decode makes of a lone surrogate.
	replies []chainReply
	// scopeFrom is the first record past the thread's last restart.
	scopeFrom int32
	// lastOwn is the thread's last record of its own, -1 where none.
	lastOwn int32
	// broken is whether a record could not be filed ([holdingsChain.add]):
	// none is filed after it, and a subagent's thread is not whole.
	broken bool
	// onChain marks the records walked from the leaf; ordered those of them
	// below the walk's first parent written after its record, where the
	// chain's order is the file's ([holdingsChain.walk]); and handedReplies the
	// ids of the replies among them past the last restart.
	onChain, ordered []uint64
	handedReplies    map[[32]byte]struct{}
}

// chainLaterParent is a record whose parent was not read when it was.
type chainLaterParent struct {
	record int32
	parent [16]byte
}

// chainRelink is what a thread's last compaction's boundary says a resume
// relinks the thread by ([threadLine.Preserved]).
type chainRelink struct {
	// boundary is the boundary's record, -1 where the thread holds none.
	boundary int32
	// kept is whether it keeps records; anchor the uuid of the record they
	// hang from, where named it is a lowercase canonical one.
	kept   bool
	anchor [16]byte
	named  bool
	// segment is whether it names them as a segment, from tail up to head by
	// parent, both lowercase canonical uuids where ends; else it lists them,
	// absent where one it lists is no uuid a record of the thread carries,
	// and later holds each it lists that no record before the boundary
	// carries ([holdingsChain.keep]).
	segment    bool
	head, tail [16]byte
	ends       bool
	absent     bool
	later      map[[16]byte]struct{}
}

// chainReply is a counted reply's record and its id's digest.
type chainReply struct {
	record int32
	id     [32]byte
}

// newHoldingsChain is the walk of a thread not read yet, filing at most bound
// records.
func newHoldingsChain(bound int) *holdingsChain {
	return &holdingsChain{bound: bound, index: map[[16]byte]int32{}, retracted: map[[10]byte]struct{}{},
		relink: chainRelink{boundary: -1}, lastOwn: -1}
}

// add files line, a user, assistant, attachment or system record of the
// thread, its own where own, with marks, and answers its place: an error
// where its uuid is no lowercase canonical UUID or is another record's, or
// where the thread has no room left for it ([holdingsChain.room]).
func (c *holdingsChain) add(line threadLine, own bool, marks uint8) (int32, error) {
	uuid, err := line.UUID()
	if err != nil {
		return -1, err
	}
	id, ok := canonicalUUID(uuid)
	if !ok {
		return -1, errors.New("its uuid is no lowercase canonical UUID, so no parent can name it as the harness " +
			"does")
	}
	if err = c.room(); err != nil {
		return -1, err
	}
	if _, held := c.index[id]; held {
		return -1, errors.New("its uuid is another record's, and the harness walks the last of the two")
	}
	record := int32(len(c.parent))
	parent, named := chainParent(line.Head)
	if parent == chainLater {
		if p, held := c.index[named]; held {
			parent = p
		} else {
			c.later = append(c.later, chainLaterParent{record: record, parent: named})
		}
	}
	c.index[id] = record
	c.parent = append(c.parent, parent)
	c.marks = append(c.marks, marks)
	if own {
		c.lastOwn = record
	}
	return record, nil
}

// chainParent is what head's `parentUuid` makes its record's parent: chainRoot
// where it is absent, null or the empty string; chainNowhere where it is no
// lowercase canonical UUID, which no record of a thread the reading walks
// carries; and else chainLater, the record whose uuid is named.
func chainParent(head threadHead) (int32, [16]byte) {
	if head.Parent == nil || bytes.Equal(head.Parent, jsonNull) {
		return chainRoot, [16]byte{}
	}
	named, err := wireRequiredString("parentUuid", head.Parent)
	switch {
	case err != nil:
		return chainNowhere, [16]byte{}
	case named == "":
		return chainRoot, [16]byte{}
	}
	id, ok := canonicalUUID(named)
	if !ok {
		return chainNowhere, [16]byte{}
	}
	return chainLater, id
}

// room says whether the thread has room for one more thing it files: its
// records ([holdingsChain.add]), what its records retract
// ([holdingsChain.retract]) and the records its last compaction keeps that no
// record before it carries ([holdingsChain.keep]) count toward one bound,
// c.bound, since each is kept until the thread is read whole, and each of the
// last two costs no more of it than a record does.
func (c *holdingsChain) room() error {
	if len(c.parent)+len(c.retracted)+len(c.relink.later) >= c.bound {
		return fmt.Errorf("%w, %d", errChainBound, c.bound)
	}
	return nil
}

// boundaryAt starts what a compaction's boundary, record, the thread's latest,
// says a resume relinks the thread by: nothing, until [holdingsChain.keep]
// files each record it lists and [holdingsChain.preserved] the rest.
func (c *holdingsChain) boundaryAt(record int32) {
	c.relink = chainRelink{boundary: record}
}

// keep files uuid, a record the boundary lists as kept
// ([threadLine.Preserved]): nothing where a record before the boundary
// carries it, as each record the boundaries the probes kept list does; the
// list absent where uuid is no lowercase canonical one, since no record the
// reading files carries it, so no resume relinks by the list; and else uuid
// itself, counted toward the thread's bound ([holdingsChain.room]), for
// [holdingsChain.relinks] to ask once the thread is read whole whether a
// record after the boundary carries it.
func (c *holdingsChain) keep(uuid string) error {
	id, ok := canonicalUUID(uuid)
	if !ok {
		c.relink.absent = true
		return nil
	}
	if record, held := c.index[id]; held && record < c.relink.boundary {
		return nil
	}
	if _, held := c.relink.later[id]; held {
		return nil
	}
	if err := c.room(); err != nil {
		return err
	}
	if c.relink.later == nil {
		c.relink.later = map[[16]byte]struct{}{}
	}
	c.relink.later[id] = struct{}{}
	return nil
}

// preserved files what the boundary [holdingsChain.boundaryAt] started says it
// keeps, but for the records it lists: whether it keeps any, its anchor, and
// its segment's ends where it names one.
func (c *holdingsChain) preserved(p threadPreserved) {
	r := &c.relink
	r.kept, r.segment = p.Kept, p.Segment
	r.anchor, r.named = canonicalUUID(p.Anchor)
	if p.Segment {
		head, headOK := canonicalUUID(p.Head)
		tail, tailOK := canonicalUUID(p.Tail)
		r.head, r.tail, r.ends = head, tail, headOK && tailOK
	}
}

// relinks reports whether a resume relinks the thread by its last
// compaction's boundary, as 2.1.310 and 2.1.322 do ([threadLine.Preserved]):
// where the boundary keeps records, and every record it lists, or every one
// its segment's walk from its tail up by parent to its head meets, is a
// record of the thread. It is the thread read whole: the records the walk
// filed, and their parents resolved.
//
// A RELINK THAT KEEPS A RECORD AT OR AFTER THE BOUNDARY IS errRelinkLater,
// which its reader takes for no reading: the relink chains each kept record
// after the one before it, the first after the anchor, so a kept record past
// the boundary moves the chain under the records the reading walks, which it
// does not follow. Every record a boundary the probes kept keeps lies before
// it.
func (c *holdingsChain) relinks() (bool, error) {
	r := c.relink
	if r.boundary < 0 || !r.kept {
		return false, nil
	}
	if !r.segment {
		if r.absent {
			return false, nil
		}
		for id := range r.later {
			if _, held := c.index[id]; !held {
				return false, nil
			}
		}
		if len(r.later) > 0 {
			return false, errRelinkLater
		}
		return true, nil
	}
	if !r.ends {
		return false, nil
	}
	record, tailHeld := c.index[r.tail]
	head, headHeld := c.index[r.head]
	if !tailHeld || !headHeld {
		return false, nil
	}
	// BOUNDED BY THE THREAD'S RECORDS, as the harness's own walk stops at a
	// record it already met: a segment's walk that meets one fails, and no
	// resume relinks by it.
	after := false
	for range len(c.parent) {
		after = after || record >= r.boundary
		if record == head {
			if after {
				return false, errRelinkLater
			}
			return true, nil
		}
		if record = c.parent[record]; record < 0 {
			return false, nil
		}
	}
	return false, nil
}

// mayLead reports whether a resume of the thread may start from checkpoint,
// a user or assistant record its last record descends from, rather than
// from the nearest user or assistant record up from that last record (the
// file comment's THE LEAF), where the thread holds a compaction's boundary
// and relinks says whether a resume relinks it ([holdingsChain.relinks]).
//
// NEVER WHERE NO RESUME RELINKS IT: the resume then starts from the
// checkpoint only where the last record does not descend from it, which the
// reading's walk would have found, or where what it drops of a compacted
// thread breaks that descent, which drops the checkpoint too.
//
// WHERE ONE DOES, it picks among the user and assistant records no other such
// record names as its parent, the checkpoint first where it is one of them;
// so the resume may start from the checkpoint where it lies before the
// boundary, whatever the relink makes of it; where it is the anchor, whose
// other children the relink hangs elsewhere; and where no user or assistant
// record after the boundary names it as its parent (markTurnParent), which
// keeps naming it through a relink that keeps no record after the boundary.
func (c *holdingsChain) mayLead(checkpoint int32, relinks bool) bool {
	switch {
	case !relinks:
		return false
	case checkpoint < c.relink.boundary:
		return true
	}
	if anchor, held := c.index[c.relink.anchor]; c.relink.named && held && anchor == checkpoint {
		return true
	}
	return c.marks[checkpoint]&markTurnParent == 0
}

// retract files uuid, an entry of a record's `retractedMessageUuids`
// ([threadLine.Retracted]): Claude Code's resume hands its model no record
// whose uuid's first 24 characters are uuid's, so the reading files those
// 24, where a lowercase canonical uuid, the only kind a record it files
// carries, can begin with them ([retractedPrefix]); an entry no such uuid
// begins with retracts no record it files, and is passed over. An error where
// the thread has no room left for it ([holdingsChain.room]).
func (c *holdingsChain) retract(uuid string) error {
	prefix, ok := retractedPrefix(uuid)
	if !ok {
		return nil
	}
	if _, held := c.retracted[prefix]; held {
		return nil
	}
	if err := c.room(); err != nil {
		return err
	}
	c.retracted[prefix] = struct{}{}
	return nil
}

// canonicalUUID is s as the 16 bytes it writes, where s is a UUID in its
// canonical form in lowercase hex: 8, 4, 4, 4 and 12 digits between hyphens.
//
// LOWERCASE ALONE, as every uuid Claude Code writes is: the harness names a
// parent by comparing strings, so an uppercase spelling names no record its
// lowercase twin is, and read as the same 16 bytes it would link two records
// the harness never links.
func canonicalUUID(s string) (id [16]byte, ok bool) {
	if len(s) != 36 || !canonicalHex(s, id[:]) {
		return [16]byte{}, false
	}
	return id, true
}

// retractedPrefix is the first 24 characters of s, where they are those a
// lowercase canonical uuid begins with ([canonicalUUID]): 8, 4, 4 and 4
// digits, each run followed by its hyphen, as the ten bytes they write.
//
// AS CLAUDE CODE COMPARES THEM: its resume takes an entry's first 24 UTF-16
// code units and a record's uuid's first 24, and holds them one where they
// are equal. A record the reading files carries a uuid of ASCII alone, so an
// entry matches one only where its first 24 code units are the same ASCII,
// which are then its first 24 bytes as decoded, whatever the decode makes of
// what follows them.
func retractedPrefix(s string) (prefix [10]byte, ok bool) {
	if len(s) < 24 || !canonicalHex(s[:24], prefix[:]) {
		return [10]byte{}, false
	}
	return prefix, true
}

// canonicalHex writes into id the bytes s spells, where s is the first
// len(s) characters of a lowercase canonical uuid, len(s) 24 or 36: runs of
// lowercase hex digits two to a byte, a hyphen after the 8th, 12th, 16th and
// 20th of them.
func canonicalHex(s string, id []byte) bool {
	for i, n := 0, 0; i < len(s); {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			i++
			continue
		}
		hi, okHi := lowerHex(s[i])
		lo, okLo := lowerHex(s[i+1])
		if !okHi || !okLo {
			return false
		}
		id[n] = hi<<4 | lo
		n++
		i += 2
	}
	return true
}

// lowerHex is the value of b, a lowercase hex digit.
func lowerHex(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	}
	return 0, false
}

// mark adds marks to record's.
func (c *holdingsChain) mark(record int32, marks uint8) { c.marks[record] |= marks }

// reply files record, a counted reply's, under its id.
func (c *holdingsChain) reply(record int32, id string) {
	c.replies = append(c.replies, chainReply{record: record, id: sha256.Sum256([]byte(id))})
	c.mark(record, markReply)
}

// restart says the record filed last is at or before the thread's last
// restart.
func (c *holdingsChain) restart() { c.scopeFrom = int32(len(c.parent)) }

// resolve looks up the parents not read when their children were, now the
// thread is read whole: each the record it names, or chainNowhere; marks
// each record a user or assistant record after the thread's last
// compaction's boundary names as its parent (markTurnParent); and marks each
// record whose uuid begins with a uuid's first 24 characters that a record of
// the thread retracts ([holdingsChain.retract]), wherever the record lies,
// since the resume drops it from what it walked.
func (c *holdingsChain) resolve() {
	for _, l := range c.later {
		parent, held := c.index[l.parent]
		if !held {
			parent = chainNowhere
		}
		c.parent[l.record] = parent
	}
	c.later = nil
	if c.relink.boundary >= 0 {
		for record := c.relink.boundary + 1; int(record) < len(c.parent); record++ {
			if parent := c.parent[record]; parent >= 0 && c.marks[record]&markUserOrAssistant != 0 {
				c.mark(parent, markTurnParent)
			}
		}
	}
	if len(c.retracted) > 0 {
		for id, record := range c.index {
			if _, gone := c.retracted[[10]byte(id[:10])]; gone {
				c.mark(record, markRetracted)
			}
		}
	}
	c.retracted = nil
}

// done releases what only reading the thread needed: the records by uuid.
func (c *holdingsChain) done() { c.index = nil }

// chainEnd is what ended a walk.
type chainEnd int

const (
	// endRoot is a record whose parent ends the walk.
	endRoot chainEnd = iota
	// endNowhere is a parent naming no record.
	endNowhere
	// endTaken is a record the walk had already taken.
	endTaken
	// endRestart is a record at or before the thread's last restart.
	endRestart
	// endForward is a parent written after its record, which a main
	// thread's walk does not follow ([holdingsChain.walk]).
	endForward
)

// String is what ended a walk, in words for an error.
func (e chainEnd) String() string {
	switch e {
	case endNowhere:
		return "a parent naming no record"
	case endTaken:
		return "a record it had already taken"
	case endRestart:
		return "its last restart"
	case endForward:
		return "a parent written after its record"
	}
	return "a root"
}

// up walks a main thread's records from record to their parents until stop
// holds of one, answering that one, or until the walk ends, answering -1 and
// what ended it: a root, a parent naming no record, a record naming itself (a
// record already taken), a parent written after its record, which a main
// thread's walk does not follow ([holdingsChain.walk]), or, where scoped, a
// record at or before the last restart. Each step goes to a record written
// before the last, so the walk ends within as many steps as the thread holds
// records.
func (c *holdingsChain) up(record int32, scoped bool, stop func(int32) bool) (int32, chainEnd) {
	for {
		if scoped && record < c.scopeFrom {
			return -1, endRestart
		}
		if stop(record) {
			return record, endRoot
		}
		switch parent := c.parent[record]; {
		case parent == chainRoot:
			return -1, endRoot
		case parent < 0:
			return -1, endNowhere
		case parent == record:
			return -1, endTaken
		case parent > record:
			return -1, endForward
		default:
			record = parent
		}
	}
}

// walk marks the chain from leaf, as far as it can be read, and the ids of
// the replies on it past the last restart, and answers what ended it.
//
// A MAIN THREAD'S (main) stops at its last restart and at a parent written
// after its record (endForward), which its reader takes for no reading
// ([holdingsWalk.mainEnd]): every step then goes to a record written before
// the last, so the chain's order is the file's, which the reading compares a
// call's use and its result by ([holdingsChain.answers]), and the walk meets no
// record twice. No main thread the probes kept names a parent written after
// it.
//
// A SUBAGENT'S follows such a parent, as its harness does: the attachments a
// subagent's start writes name parents written after them (74 of the 89
// subagent threads the probes kept), and so does a reply that names the
// environment its harness wrote after it (3 of them). So the walk marks
// ordered only the records below its first step to a parent written after
// its record, among which the chain's order is the file's; every record the
// probes kept that holds a call or its result lies among them.
func (c *holdingsChain) walk(leaf int32, main bool) chainEnd {
	c.onChain = make([]uint64, (len(c.parent)+63)/64)
	c.ordered = make([]uint64, len(c.onChain))
	end, ordered := endTaken, true
	for record := leaf; ; {
		if main && record < c.scopeFrom {
			end = endRestart
			break
		}
		if c.on(record) {
			break
		}
		c.onChain[record/64] |= 1 << (record % 64)
		if ordered {
			c.ordered[record/64] |= 1 << (record % 64)
		}
		parent := c.parent[record]
		if parent == chainRoot {
			end = endRoot
			break
		}
		if parent < 0 {
			end = endNowhere
			break
		}
		if parent > record {
			if main {
				end = endForward
				break
			}
			ordered = false
		}
		record = parent
	}
	c.handedReplies = map[[32]byte]struct{}{}
	for _, r := range c.replies {
		if r.record >= c.scopeFrom && c.on(r.record) {
			c.handedReplies[r.id] = struct{}{}
		}
	}
	return end
}

// on reports whether record is on the chain walked.
func (c *holdingsChain) on(record int32) bool {
	return c.onChain != nil && c.onChain[record/64]&(1<<(record%64)) != 0
}

// handed reports whether the resume hands its model record (the file
// comment): past the last restart, retracted by no record of the thread, and
// on the chain, or a reply's record of an id a reply on the chain carries, or
// a user record of results whose parent is one. False for -1, no record of the
// thread.
func (c *holdingsChain) handed(record int32) bool {
	if record < c.scopeFrom || int(record) >= len(c.parent) || c.marks[record]&markRetracted != 0 {
		return false
	}
	if c.on(record) {
		return true
	}
	switch marks := c.marks[record]; {
	case marks&markReply != 0:
		return c.replyHanded(record)
	case marks&markResults != 0:
		parent := c.parent[record]
		return parent >= c.scopeFrom && c.marks[parent]&markReply != 0 && c.replyHanded(parent)
	}
	return false
}

// replyHanded reports whether a reply on the chain past the last restart
// carries the id of record's reply.
func (c *holdingsChain) replyHanded(record int32) bool {
	id, filed := c.replyOf(record)
	if !filed {
		return false
	}
	_, handed := c.handedReplies[id]
	return handed
}

// replyOf is the digest of the id of record's reply, where record is a
// counted reply's record whose id was read ([holdingsChain.reply]).
func (c *holdingsChain) replyOf(record int32) ([32]byte, bool) {
	i, found := slices.BinarySearchFunc(c.replies, record, func(r chainReply, record int32) int {
		return cmp.Compare(r.record, record)
	})
	if !found {
		return [32]byte{}, false
	}
	return c.replies[i].id, true
}

// answers reports whether the resume hands its model record use, a call's
// use, and record result, its result, the use first, as the API takes a
// result only after its call: both handed ([holdingsChain.handed]), and
//
//   - both on the chain, below the walk's first step to a parent written
//     after its record, where the chain's order is the file's, the use
//     written first; or
//   - the result recovered beside a reply whose record the use is: the resume
//     places what it recovers beside a reply after every record of that reply
//     on the chain, so after the use where the use is on it, and in the
//     file's order among what it recovers where the use is recovered too.
//
// NO OTHER PAIR IS IN AN ORDER THE READING CAN READ: the resume places a
// recovered record after its reply's records on the chain and after the
// results, attachments and system records that follow them there, so a use
// it recovers can come after its own result on the chain, and a result it
// recovers beside another reply than the use's can come before the use.
func (c *holdingsChain) answers(use, result int32) bool {
	switch {
	case !c.handed(use) || !c.handed(result):
		return false
	case c.on(result):
		return c.on(use) && c.inOrder(use) && c.inOrder(result) && use < result
	}
	return c.sameReply(use, c.parent[result]) && (c.on(use) || use < result)
}

// inOrder reports whether record is on the chain below the walk's first step
// to a parent written after its record ([holdingsChain.walk]).
func (c *holdingsChain) inOrder(record int32) bool {
	return c.ordered != nil && c.ordered[record/64]&(1<<(record%64)) != 0
}

// sameReply reports whether records a and b are records of one reply: each a
// counted reply's record whose id was read, the two ids one.
func (c *holdingsChain) sameReply(a, b int32) bool {
	idA, filedA := c.replyOf(a)
	idB, filedB := c.replyOf(b)
	return filedA && filedB && idA == idB
}
