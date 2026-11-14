package executor

import (
	"slices"
	"strings"
)

// WHERE THE REQUEST A RESUMED SESSION SENDS PAIRS A CALL'S USE WITH ITS RESULT,
// as the reading of a kept session holds it ([holdingsPairing]): a rule of the
// reading's own, read off the order in which Claude Code's resume hands its
// model a thread's records ([threadChain]), which is the resume's truth and
// never this file's.
//
// WHAT THE REQUEST DOES WITH THE HANDED RECORDS, in both releases, 2.1.310 and
// 2.1.322 (read as data, never run). Its fold merges an assistant record into
// the latest earlier message of its `message.id` AS WRITTEN, an absent id
// among them, unless a user message holding no tool result came between
// (2.1.322's scan `Lr.set(Wu.message.id,Mr)`, `Lr.get(yl.message.id)`
// @298577650, Dm @298586531, and the `projectread_` clause Aje @298545849;
// 2.1.310's `Oo.set(Es.message.id,kr)` @251760100, Yh). Its pairing repair
// then keeps a call's FIRST use and answers it only from the message RIGHT
// AFTER the one holding that use, the first result block of its id there,
// answering it with a synthetic error otherwise and stripping a result found
// anywhere else (2.1.322's FQs @298693792, `Ee=e[y+1]`; 2.1.310's TSn
// @251869313, line for line the same). And the deserializer stamps a fresh id
// on an assistant record whose `message.id` is falsy only where it carries no
// `requestId` (2.1.322's e_n, `!fs.message.id&&fs.requestId===void 0`
// @295266158; 2.1.310's TJt @248494944), so an id-less reply with one merges
// back into another id-less one across other messages.
//
// SO A CALL IS PAIRED ONLY BY STRUCTURE, the conservative rule (C), never by
// the request's whole model, every place of which more lenient than a release
// would be a call counted held whose result its model was never handed: for a
// call x whose use is block of record U and whose result is block of record R,
//
//   - C0: U's `message.id` is a string that says something
//     ([threadChain.Reply]): an id-less use merges across other messages where
//     it carries a requestId and stands alone where it carries none, neither a
//     shape the rule can place;
//   - C1 is its caller's: exactly one use of x and one result in the thread,
//     no batch naming x, and x neither split from nor split into another id
//     ([holdingsWalk.idsFold]);
//   - C2: the resume hands U and R under every variant ([handedYes]);
//
// and on EVERY order the resume may hand the thread in ([threadChain.EachOrder]):
//
//   - C3: where F is the first record of U's reply in the order, R comes after
//     F (which C5 holds too, R being a result of a call U's reply made), and
//     every record from F to the later of U and R is a PLAIN record of
//     U's reply or a PLAIN results record ([holdingsPairing.see]): no other
//     reply's record, no prompt, no meta, virtual or text user record, no
//     attachment and no system record lies there, so the fold puts U's block
//     in F's message and R's in the one right after it;
//   - C4: R is a plain results record, which C3 holds already, the stretch
//     running to R wherever it lies;
//   - C5: no user record before F holds a result of any call U's reply made,
//     which 2.1.322's regroup of a split reply would move after it;
//   - C6: no compaction's boundary lies at or after F, where the relaunch's
//     first request would cut the conversation;
//   - C7: where x calls the Poll tool, its reply holds no other Poll call,
//     which 2.1.322's code mode folds (KFt @293413848, `H.name===wi`, wi
//     "Poll" @288129013).
//
// MEASURED: of the calls the probes kept with one use and one result, both
// handed, (C) holds all 951 of the main threads' and all 109 of the
// subagents'. It TELLS, knowingly,
// some shapes the request does pair, none measured: another reply between F
// and U whose use merges back, a prompt merged into a results message before
// the reply goes on, a results record in the stretch answering another reply,
// a record the fold skips (a microcompaction's boundary among them), and an
// id-less reply the request does pair. Every relaxation that would hold one is
// a piece of the request's whole model, with its own false holds.
//
// ONE PASS OVER EACH ORDER makes every asked pair's test a few lookups: the
// first place of each asked reply, prefix counts of the records that are
// neither plain kind and of the changes of reply between one assistant record
// and the next, the first user record holding a result of each asked reply's
// calls, and the last boundary's place. Linear in the thread, with no cap of
// its own.

// The tool whose calls 2.1.322's code mode folds where a reply holds more
// than one ([holdingsPairing], C7).
const claudeToolPoll = "Poll"

// claudeHarnessIDPrefixes are the prefixes of the ids of the calls and results
// Claude Code's request writes of its own, which it places as it builds
// rounds of its own: a poll's (2.1.322's NKe @288139238, 2.1.310's iLe
// @243029149), a project's read documents' (Lvo @298554259, KZn @251746803),
// a voice call's record (2.1.322's K1e @293381964) and a delivered task's
// (ke @291414785, bEt @246850119). A call under such an id, or a results
// record answering one, is no plain record of a pair.
var claudeHarnessIDPrefixes = [...]string{"poll_", "projectread_", "voicecallrecord_", "gettask_"}

// harnessID reports whether id is one Claude Code's request writes of its own
// ([claudeHarnessIDPrefixes]).
func harnessID(id string) bool {
	for _, prefix := range claudeHarnessIDPrefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

// splitBase is the id a batch's split call or result of id was split from:
// id without its last `_` and the digits after it, where id ends so, as
// Claude Code reads one (`/_(\d+)$/`, 2.1.322's k @291387109 in _2e, 2.1.310's
// in qGe @244964051, the digits ASCII alone); false where it does not.
func splitBase(id string) (string, bool) {
	at := strings.LastIndexByte(id, '_')
	if at < 0 || at == len(id)-1 {
		return "", false
	}
	for i := at + 1; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return "", false
		}
	}
	return id[:at], true
}

// pairMark is what the pairing rule files of one record ([holdingsPairing.see]).
type pairMark uint8

const (
	// pairPlainReply is an assistant record no mark keeps from being a plain
	// record of its reply: not virtual, no API error, of a model other than
	// "<synthetic>", carrying no batch.
	pairPlainReply pairMark = 1 << iota
	// pairPlainResults is a user record whose content is a list of results
	// alone, at least one, none answering an id Claude Code writes of its own,
	// carrying no meta, virtual, shutdown or summary mark, no delivered task
	// and no origin.
	pairPlainResults
	// pairBoundary is a compaction's boundary.
	pairBoundary
	// pairPoll is a record holding one Poll call, and pairPolls one holding
	// more than one.
	pairPoll
	pairPolls
	// pairAssistant is an assistant record, plain or not.
	pairAssistant
	// pairCounted is a record whose calls the reading counts: a user or
	// assistant record of the main thread, chained and its own
	// ([threadHead.Main]).
	pairCounted
)

// holdingsPairing is what the pairing rule files of one thread as it is read,
// a mark a record by its handle, and then decides of each pair asked of it.
// One goroutine reads one thread; it holds no lock and starts none.
type holdingsPairing struct {
	// marks is each record's, by handle - 1.
	marks []pairMark
}

// see files the marks of line, record h of the thread, whose message is m (a
// user or assistant record's, decoded once by the chain), and whether the
// reading counts its calls. A record filed past the chain's bound, or none at
// all, has none.
func (p *holdingsPairing) see(h chainHandle, line threadLine, m threadMessage, counted bool) {
	if h <= 0 {
		return
	}
	for len(p.marks) < int(h) {
		p.marks = append(p.marks, 0)
	}
	var mark pairMark
	if counted {
		mark |= pairCounted
	}
	switch line.Head.Type {
	case recordAssistant:
		mark |= pairAssistant
		if plainReply(line) {
			mark |= pairPlainReply
		}
		polls := 0
		for _, call := range m.Calls() {
			if call.Kind == blockToolUse && call.Name == claudeToolPoll {
				polls++
			}
		}
		switch {
		case polls == 1:
			mark |= pairPoll
		case polls > 1:
			mark |= pairPolls
		}
	case recordUser:
		if plainResults(line, m) {
			mark |= pairPlainResults
		}
	case recordSystem:
		if line.Head.Subtype == subtypeCompactBoundary {
			mark |= pairBoundary
		}
	}
	p.marks[h-1] = mark
}

// plainReply reports whether line, an assistant record, is a plain record of
// its reply ([pairPlainReply]): each mark it carries read as no plain one
// where its accessor cannot read it.
func plainReply(line threadLine) bool {
	virtual, virtualErr := line.IsVirtual()
	failed, failedErr := line.APIError()
	model, modelErr := line.Model()
	batch, batchErr := line.BatchToolUses(func(string) {})
	return virtualErr == nil && !virtual && failedErr == nil && !failed && modelErr == nil &&
		model != claudeSyntheticModel && batchErr == nil && !batch
}

// plainResults reports whether line, a user record whose message is m, is a
// plain results record ([pairPlainResults]): each mark it carries read as one
// where its accessor cannot read it, and an origin of any kind, or one whose
// kind does not decode, as one.
func plainResults(line threadLine, m threadMessage) bool {
	if _, text := m.Text(); text || len(m.Kinds()) == 0 {
		return false
	}
	for _, kind := range m.Kinds() {
		if kind != blockToolResult {
			return false
		}
	}
	for _, call := range m.Calls() {
		if harnessID(call.ID) {
			return false
		}
	}
	meta, metaErr := line.IsMeta()
	virtual, virtualErr := line.IsVirtual()
	shutdown, shutdownErr := line.InterruptedByShutdown()
	summary, summaryErr := line.IsCompactSummary()
	origin, originErr := line.OriginKind()
	return metaErr == nil && !meta && virtualErr == nil && !virtual && shutdownErr == nil && !shutdown &&
		summaryErr == nil && !summary && !line.TaskDelivery() && originErr == nil && origin == ""
}

// counts hands each, for every call id a block of a counted record carries,
// its key and whether the block is its use, once End decided chain.
func (p *holdingsPairing) counts(chain *threadChain, each func(key chainCallID, use bool)) {
	for h := chainHandle(1); int(h) <= len(p.marks); h++ {
		if p.mark(h)&pairCounted == 0 {
			continue
		}
		for _, key := range chain.Uses(h) {
			each(key, true)
		}
		for _, key := range chain.Results(h) {
			each(key, false)
		}
	}
}

// mark is record h's marks.
func (p *holdingsPairing) mark(h chainHandle) pairMark {
	if h <= 0 || int(h) > len(p.marks) {
		return 0
	}
	return p.marks[h-1]
}

// pairAsk is one pair asked of a thread: the record of a call's use and the
// record of its result, whether the call is a Poll, and, once
// [holdingsPairing.pairs] answered, whether the request pairs them.
type pairAsk struct {
	use, result chainHandle
	poll        bool
	paired      bool
	// reply is the use's reply, the pass's own.
	reply chainReplyID
}

// pairReply is what the pass gathers of one asked reply: how many Poll calls
// its records make, and, in one order, its first record's place and the
// first place of a user record holding a result of a call it made.
type pairReply struct {
	polls          int
	first, results int
}

// pairs answers each of asks of chain, once End decided it: paired where (C)
// holds of the pair (the file comment), C1 aside, which is its caller's.
func (p *holdingsPairing) pairs(chain *threadChain, asks []*pairAsk) {
	replies := map[chainReplyID]*pairReply{}
	for _, a := range asks {
		a.paired = false
		reply, ok := chain.Reply(a.use)
		// C2 is held twice: here, and on every order, which holds U and R
		// only where every variant the order is handed under hands them.
		switch {
		case !ok: // C0
		case chain.Handed(a.use) != handedYes || chain.Handed(a.result) != handedYes: // C2
		default:
			a.reply, a.paired = reply, true
			if replies[reply] == nil {
				replies[reply] = &pairReply{}
			}
		}
	}
	if len(replies) == 0 {
		return
	}
	// WHAT EACH ASKED REPLY'S RECORDS CALL, over every record of the thread
	// that carries its id, handed or not: C5's results and C7's Poll calls.
	answering := map[chainCallID][]chainReplyID{}
	for h := chainHandle(1); int(h) <= len(p.marks); h++ {
		reply, ok := chain.Reply(h)
		if !ok || replies[reply] == nil {
			continue
		}
		switch mark := p.mark(h); {
		case mark&pairPolls != 0:
			replies[reply].polls += 2
		case mark&pairPoll != 0:
			replies[reply].polls++
		}
		for _, key := range chain.Uses(h) {
			if !slices.Contains(answering[key], reply) {
				answering[key] = append(answering[key], reply)
			}
		}
	}
	for _, a := range asks {
		if a.paired && a.poll && replies[a.reply].polls != 1 { // C7
			a.paired = false
		}
	}
	var bad, changes []int32
	chain.EachOrder(func(o chainOrder) bool {
		n := o.Len()
		bad, changes = append(bad[:0], 0), append(changes[:0], 0)
		lastBoundary := -1
		for _, r := range replies {
			r.first, r.results = -1, -1
		}
		// previous is the reply of the last assistant record met, and met
		// whether one was.
		var (
			previous chainReplyID
			met      bool
		)
		for i := range n {
			h := o.At(i)
			mark := p.mark(h)
			b, c := bad[i], changes[i]
			if mark&(pairPlainReply|pairPlainResults) == 0 {
				b++
			}
			if mark&pairAssistant != 0 {
				reply, _ := chain.Reply(h)
				if !met || reply != previous {
					c++
				}
				previous, met = reply, true
				if r := replies[reply]; r != nil && r.first < 0 {
					r.first = i
				}
			}
			for _, key := range chain.Results(h) {
				for _, reply := range answering[key] {
					if r := replies[reply]; r.results < 0 {
						r.results = i
					}
				}
			}
			if mark&pairBoundary != 0 {
				lastBoundary = i
			}
			bad, changes = append(bad, b), append(changes, c)
		}
		for _, a := range asks {
			if !a.paired {
				continue
			}
			r := replies[a.reply]
			use, useOK := o.Pos(a.use)
			result, resultOK := o.Pos(a.result)
			first := r.first
			end := max(use, result)
			switch {
			case !useOK || !resultOK || first < 0:
				a.paired = false
			case result <= first: // C3: R after F
				a.paired = false
			case bad[end+1]-bad[first] != 0: // C3 and C4: every record from F to the later plain
				a.paired = false
			case changes[end+1]-changes[first+1] != 0: // C3: every reply record of U's reply
				a.paired = false
			case r.results >= 0 && r.results < first: // C5
				a.paired = false
			case lastBoundary >= first: // C6
				a.paired = false
			}
		}
		return true
	})
}
