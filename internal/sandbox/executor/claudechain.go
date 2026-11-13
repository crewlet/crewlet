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
// reading of a kept session walks them ([threadChain]): its own rule, read in
// 2.1.310's and 2.1.322's shipped code, never a rule of the reading's.
//
// A RESUME HANDS ITS MODEL A CHAIN, never the file. It walks from one record,
// the LEAF, to the record its `parentUuid` names, and on, until a parent that
// is null, the empty string or absent ends the walk, and reverses what it
// walked. Off that chain it hands over only what a reply on it recovers: the
// reply's other records, found by their `message.id`, which every record of
// one reply carries, and the results whose parent is one of them, which is how
// the results of calls made at once, each chained to its own call, reach the
// model though the thread's last record descends from one of them alone
// (probe T's trees, both releases). A record of any other reply off the chain,
// and a user record that is no call's result, reaches the model by no path: so
// an attempt the harness withdrew and failed to remove from the file, which no
// later record descends from, holds a call no continuation hands over, and the
// reading holds none of its calls.
//
// A RECORD IS HANDED ([threadChain.handed]) where it lies past its thread's
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
// off every record of the thread ([threadChain.retract]), where the resume
// reads them off the records it hands over alone, so it takes away no less
// than the resume does.
//
// A CALL IS ANSWERED ([threadChain.answers]) only where the resume hands its
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
//   - where a compaction kept a segment, the resume picks its leaf among the
//     thread's leaves, the checkpoint first where it is one: so in a main
//     thread holding a compaction's boundary, a checkpoint that is a user or
//     assistant record other than the leaf is taken for the leaf, the earlier
//     of the two the pick can land on;
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
// records retract, counted together ([threadChain.room]).
//
// 262,144, TWICE THE LINES A THREAD AT THE PLAINTEXT'S BOUND HOLDS: 256 MiB
// ([threadPlainMax]) of lines of 2,053 bytes, a real main thread's average
// (probe P6), is 130,754, and a session at the archive's bound inflates to
// about 118 MiB. One thread's records are held at a time, each a uuid's place
// in a map, a parent and its marks, the uuid of a parent that came later, and
// each reply's id as a digest: at the bound, every record a reply of its own
// id naming a parent that comes later, 35 MiB live, its heap peaking at 69 MiB
// (BenchmarkAReadingAtItsBounds, on two cores). A uuid a record retracts is
// held as its first ten bytes in a set, less than any record costs, so a
// thread at the bound that retracts holds no more than that (computed, not
// measured). Past it, a main thread is no reading and a subagent's thread not
// whole.
const holdingsMaxRecords = 1 << 18

// What a record's parent is, where it is no record of the thread
// ([threadChain.parent]).
const (
	// chainRoot is a parent that ends the walk: null, the empty string or
	// absent, each falsy to the harness's walk.
	chainRoot int32 = -1
	// chainNowhere is a parent naming no record of the thread.
	chainNowhere int32 = -2
	// chainLater is a parent not read when its child was, looked up once the
	// thread is read whole ([threadChain.resolve]).
	chainLater int32 = -3
)

// What a record is to the walk ([threadChain.marks]).
const (
	// markUserOrAssistant is a user or assistant record: the leaf of a main
	// thread is the nearest one up from its last record.
	markUserOrAssistant uint8 = 1 << iota
	// markReply is a counted assistant record whose reply's id was read
	// ([threadChain.replies]).
	markReply
	// markResults is a user record holding a call's result.
	markResults
	// markRetracted is a record whose uuid a retraction names
	// ([threadChain.retract]): handed to no model, wherever it lies.
	markRetracted
)

// errChainBound is a thread holding more records than a reading walks the
// chain of ([holdingsMaxRecords]).
var errChainBound = errors.New("its thread holds more records than a reading walks the chain of")

// threadChain is one thread's records as the walk reads them: by uuid, each
// with its parent and its marks, and each counted reply's id as a digest.
type threadChain struct {
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
	// they write ([threadChain.retract]), kept until the thread is read
	// whole and its records marked by them.
	retracted map[[10]byte]struct{}
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
	// broken is whether a record could not be filed ([threadChain.add]):
	// none is filed after it, and a subagent's thread is not whole.
	broken bool
	// onChain marks the records walked from the leaf; ordered those of them
	// below the walk's first parent written after its record, where the
	// chain's order is the file's ([threadChain.walk]); and handedReplies the
	// ids of the replies among them past the last restart.
	onChain, ordered []uint64
	handedReplies    map[[32]byte]struct{}
}

// chainLaterParent is a record whose parent was not read when it was.
type chainLaterParent struct {
	record int32
	parent [16]byte
}

// chainReply is a counted reply's record and its id's digest.
type chainReply struct {
	record int32
	id     [32]byte
}

// newThreadChain is the walk of a thread not read yet, filing at most bound
// records.
func newThreadChain(bound int) *threadChain {
	return &threadChain{bound: bound, index: map[[16]byte]int32{}, retracted: map[[10]byte]struct{}{}, lastOwn: -1}
}

// add files line, a user, assistant, attachment or system record of the
// thread, its own where own, with marks, and answers its place: an error
// where its uuid is no lowercase canonical UUID or is another record's, or
// where the thread has no room left for it ([threadChain.room]).
func (c *threadChain) add(line threadLine, own bool, marks uint8) (int32, error) {
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
// records ([threadChain.add]) and what its records retract
// ([threadChain.retract]) count toward one bound, c.bound, since each is kept
// until the thread is read whole, and a retraction costs no more of it than a
// record does.
func (c *threadChain) room() error {
	if len(c.parent)+len(c.retracted) >= c.bound {
		return fmt.Errorf("%w, %d", errChainBound, c.bound)
	}
	return nil
}

// retract files uuid, an entry of a record's `retractedMessageUuids`
// ([threadLine.Retracted]): Claude Code's resume hands its model no record
// whose uuid's first 24 characters are uuid's, so the reading files those
// 24, where a lowercase canonical uuid, the only kind a record it files
// carries, can begin with them ([retractedPrefix]); an entry no such uuid
// begins with retracts no record it files, and is passed over. An error where
// the thread has no room left for it ([threadChain.room]).
func (c *threadChain) retract(uuid string) error {
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
func (c *threadChain) mark(record int32, marks uint8) { c.marks[record] |= marks }

// reply files record, a counted reply's, under its id.
func (c *threadChain) reply(record int32, id string) {
	c.replies = append(c.replies, chainReply{record: record, id: sha256.Sum256([]byte(id))})
	c.mark(record, markReply)
}

// restart says the record filed last is at or before the thread's last
// restart.
func (c *threadChain) restart() { c.scopeFrom = int32(len(c.parent)) }

// resolve looks up the parents not read when their children were, now the
// thread is read whole: each the record it names, or chainNowhere; and marks
// each record whose uuid begins with a uuid's first 24 characters that a
// record of the thread retracts ([threadChain.retract]), wherever the record
// lies, since the resume drops it from what it walked.
func (c *threadChain) resolve() {
	for _, l := range c.later {
		parent, held := c.index[l.parent]
		if !held {
			parent = chainNowhere
		}
		c.parent[l.record] = parent
	}
	c.later = nil
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
func (c *threadChain) done() { c.index = nil }

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
	// thread's walk does not follow ([threadChain.walk]).
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
// thread's walk does not follow ([threadChain.walk]), or, where scoped, a
// record at or before the last restart. Each step goes to a record written
// before the last, so the walk ends within as many steps as the thread holds
// records.
func (c *threadChain) up(record int32, scoped bool, stop func(int32) bool) (int32, chainEnd) {
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
// call's use and its result by ([threadChain.answers]), and the walk meets no
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
func (c *threadChain) walk(leaf int32, main bool) chainEnd {
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
func (c *threadChain) on(record int32) bool {
	return c.onChain != nil && c.onChain[record/64]&(1<<(record%64)) != 0
}

// handed reports whether the resume hands its model record (the file
// comment): past the last restart, retracted by no record of the thread, and
// on the chain, or a reply's record of an id a reply on the chain carries, or
// a user record of results whose parent is one. False for -1, no record of the
// thread.
func (c *threadChain) handed(record int32) bool {
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
func (c *threadChain) replyHanded(record int32) bool {
	id, filed := c.replyOf(record)
	if !filed {
		return false
	}
	_, handed := c.handedReplies[id]
	return handed
}

// replyOf is the digest of the id of record's reply, where record is a
// counted reply's record whose id was read ([threadChain.reply]).
func (c *threadChain) replyOf(record int32) ([32]byte, bool) {
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
// result only after its call: both handed ([threadChain.handed]), and
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
func (c *threadChain) answers(use, result int32) bool {
	switch {
	case !c.handed(use) || !c.handed(result):
		return false
	case c.on(result):
		return c.on(use) && c.inOrder(use) && c.inOrder(result) && use < result
	}
	return c.sameReply(use, c.parent[result]) && (c.on(use) || use < result)
}

// inOrder reports whether record is on the chain below the walk's first step
// to a parent written after its record ([threadChain.walk]).
func (c *threadChain) inOrder(record int32) bool {
	return c.ordered != nil && c.ordered[record/64]&(1<<(record%64)) != 0
}

// sameReply reports whether records a and b are records of one reply: each a
// counted reply's record whose id was read, the two ids one.
func (c *threadChain) sameReply(a, b int32) bool {
	idA, filedA := c.replyOf(a)
	idB, filedB := c.replyOf(b)
	return filedA && filedB && idA == idB
}
