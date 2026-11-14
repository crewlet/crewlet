package executor

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
)

// WHICH RECORDS OF A SUBAGENT'S THREAD ITS RESUME HANDED ITS MODEL, as the
// reading of a kept session walks them ([holdingsChain]): the walk rule 2
// reads a subagent's thread by, its own, read in 2.1.310's and 2.1.322's
// shipped code. The session's main thread is read by the model of Claude
// Code's resume itself ([threadChain]) and paired by the request's pairing
// rule ([holdingsPairing]).
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
// told, the safe direction. None of them reaches a record of another reply: so
// an attempt the harness withdrew and failed to remove from the file, which no
// later record descends from, holds a call no continuation hands over, and
// the reading holds none of its calls.
//
// A RECORD IS HANDED ([holdingsChain.handed]) where it is on the chain from
// the leaf, or is a reply's record whose id is that of a reply on the chain,
// or is a user record of results whose parent is such a record; AND NO RECORD
// A RETRACTION NAMES ([threadLine.Retracted]), whose uuid's first 24
// characters are those of a uuid a record names as taken back, wherever it
// lies: the reading reads those names off every record of the thread
// ([holdingsChain.retract]), so it takes away no less than the resume does.
//
// A CALL IS ANSWERED ([holdingsChain.answers]) only where the resume hands its
// model the call's use and then its result: both on the chain, in the chain's
// order, or the result recovered beside the use's own reply.
//
// THE LEAF is the thread's last record of its own in the file
// ([holdingsWalk.subagentEnd]): rule 2 asks what its model had seen when it
// wrote the reply its answer carries, which is the conversation its last
// record continues. Its walk must reach the thread's root; one that breaks, at
// a parent naming no record or a record the walk already took, leaves the
// thread not whole. So does a record of no lowercase canonical uuid, a uuid
// held twice, more records than [holdingsMaxRecords], and a progress record, a
// legacy shape, which the harness's load drops, handing each record after it
// that names its uuid to the progress record's own parent ([recordProgress]).
//
// A subagent's thread can name a parent that comes later in the file (the
// attachments of its start: 74 of the 89 subagent threads the probes kept),
// so a parent is looked up once the thread is read whole.

// holdingsMaxRecords is the most user, assistant, attachment and system
// records of one subagent's thread a reading walks the chain of, and of the
// uuids those records retract, counted together ([holdingsChain.room]).
//
// 262,144, TWICE THE LINES A THREAD AT THE PLAINTEXT'S BOUND HOLDS: 256 MiB
// ([threadPlainMax]) of lines of 2,053 bytes, a real main thread's average
// (probe P6), is 130,754, and a session at the archive's bound inflates to
// about 118 MiB. One thread's records are held at a time, each a uuid's place
// in a map, a parent and its marks, the uuid of a parent that came later, and
// each reply's id as a digest: at the bound, every record a reply of its own
// id naming a parent that comes later, 35 MiB live, its heap peaking at 69 MiB
// (BenchmarkAReadingAtItsBounds, on two cores, measured of a main thread when
// this walk read one). A uuid a record retracts is held as its first ten
// bytes in a set, less than any record costs, so a thread at the bound that
// holds one holds no more than that (computed, not measured). Past it, a
// subagent's thread is not whole.
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
	// markReply is a counted assistant record whose reply's id was read
	// ([holdingsChain.replies]).
	markReply uint8 = 1 << iota
	// markResults is a user record holding a call's result.
	markResults
	// markRetracted is a record whose uuid a retraction names
	// ([holdingsChain.retract]): handed to no model, wherever it lies.
	markRetracted
)

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
	// replies are the counted replies' records and their ids' SHA-256, in
	// record order; a reply's id is the box's word, as long as a line, and two
	// ids are the same reply only where the harness compares them equal, which
	// holds of every id filed: [threadLine.MessageID] hands out none holding
	// the replacement character the decode makes of a lone surrogate.
	replies []chainReply
	// lastOwn is the thread's last record of its own, -1 where none.
	lastOwn int32
	// broken is whether a record could not be filed ([holdingsChain.add]):
	// none is filed after it, and a subagent's thread is not whole.
	broken bool
	// onChain marks the records walked from the leaf; ordered those of them
	// below the walk's first parent written after its record, where the
	// chain's order is the file's ([holdingsChain.walk]); and handedReplies the
	// ids of the replies among them.
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

// newHoldingsChain is the walk of a thread not read yet, filing at most bound
// records.
func newHoldingsChain(bound int) *holdingsChain {
	return &holdingsChain{bound: bound, index: map[[16]byte]int32{}, retracted: map[[10]byte]struct{}{},
		lastOwn: -1}
}

// add files line, a user, assistant, attachment or system record of the
// thread, its own where own, and answers its place: an error
// where its uuid is no lowercase canonical UUID or is another record's, or
// where the thread has no room left for it ([holdingsChain.room]).
func (c *holdingsChain) add(line threadLine, own bool) (int32, error) {
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
	c.marks = append(c.marks, 0)
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
// records ([holdingsChain.add]) and what its records retract
// ([holdingsChain.retract]) count toward one bound, c.bound, since each is
// kept until the thread is read whole, and the second costs no more of it
// than a record does.
func (c *holdingsChain) room() error {
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

// resolve looks up the parents not read when their children were, now the
// thread is read whole: each the record it names, or chainNowhere; and marks
// each record whose uuid begins with a uuid's first 24 characters that a
// record of the thread retracts ([holdingsChain.retract]), wherever the
// record lies, since the resume drops it from what it walked.
func (c *holdingsChain) resolve() {
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
)

// String is what ended a walk, in words for an error.
func (e chainEnd) String() string {
	switch e {
	case endNowhere:
		return "a parent naming no record"
	case endTaken:
		return "a record it had already taken"
	}
	return "a root"
}

// walk marks the chain from leaf, a subagent's thread's last record of its
// own, as far as it can be read, and the ids of the replies on it, and
// answers what ended it.
//
// IT FOLLOWS A PARENT WRITTEN AFTER ITS RECORD, as the harness does: the
// attachments a subagent's start writes name parents written after them (74
// of the 89 subagent threads the probes kept), and so does a reply that names
// the environment its harness wrote after it (3 of them). So the walk marks
// ordered only the records below its first step to a parent written after
// its record, among which the chain's order is the file's; every record the
// probes kept that holds a call or its result lies among them.
func (c *holdingsChain) walk(leaf int32) chainEnd {
	c.onChain = make([]uint64, (len(c.parent)+63)/64)
	c.ordered = make([]uint64, len(c.onChain))
	end, ordered := endTaken, true
	for record := leaf; ; {
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
			ordered = false
		}
		record = parent
	}
	c.handedReplies = map[[32]byte]struct{}{}
	for _, r := range c.replies {
		if c.on(r.record) {
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
// comment): retracted by no record of the thread, and on the chain, or a
// reply's record of an id a reply on the chain carries, or a user record of
// results whose parent is one. False for -1, no record of the thread.
func (c *holdingsChain) handed(record int32) bool {
	if record < 0 || int(record) >= len(c.parent) || c.marks[record]&markRetracted != 0 {
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
		return parent >= 0 && c.marks[parent]&markReply != 0 && c.replyHanded(parent)
	}
	return false
}

// replyHanded reports whether a reply on the chain carries the id of record's
// reply.
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
