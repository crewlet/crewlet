package executor

import (
	"slices"
	"strings"
)

// THE PAIRING RULE of a kept session's reading ([holdingsPairing]): where the
// request a resumed session sends pairs a call's use with its result, read off
// the order Claude Code's resume hands a thread's records ([threadChain]). That
// request (2.1.310 and 2.1.322, read as data) folds an assistant record into
// the latest earlier message of its `message.id` unless a user message with no
// tool result came between (2.1.322's Dm, 2.1.310's Yh); answers a call's first
// use only from the message right after it, else with a synthetic error,
// stripping a result anywhere else (FQs, TSn); and stamps a fresh id only on an
// id-less record with no `requestId` (e_n, TJt), so id-less replies with one
// merge across messages (C0). It treats by name only its own tools', its memory
// server's and Poll calls (IAi, dvs, KFt), none bridged. So a call pairs by
// structure alone: more would count held a call whose result was never handed.

// claudeHarnessIDPrefixes prefix the ids Claude Code's request writes for
// rounds of its own: a poll's (2.1.322's NKe, 2.1.310's iLe), a project's read
// documents' (Lvo, KZn), a voice call record's (K1e) and a delivered task's
// (ke, bEt). A results record answering one is not plain, so a call under one
// is never paired (C4).
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

// splitBase is the id a batch's split call or result id was split from: id
// without a trailing `_` and ASCII digits, as Claude Code reads one (`/_(\d+)$/`,
// 2.1.322's _2e, 2.1.310's qGe); false where it has none.
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

// pairMark is what the pairing rule files of one record ([holdingsPairing.see]),
// and what rule 2 adds of a subagent's ([holdingsPairing.flag]).
type pairMark uint16

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
	// pairAssistant is an assistant record, plain or not; pairUser a user record.
	pairAssistant
	pairUser
	// pairLegStart is a subagent's own user record starting a leg of its work,
	// and pairFinished its own reply that finished one, as rule 2 alone files
	// them ([legStart], [finishedReply]).
	pairLegStart
	pairFinished
)

// holdingsPairing marks each record of one thread by its handle as it is read,
// then decides each pair asked by rule (C), structure alone, for a call whose
// use is in record U and result in record R: C0, U's `message.id` says
// something ([threadChain.Reply]); C1, the caller's: one use and one result,
// of a bridged tool ([claudeBridgedToolPrefix]), no batch, no split
// ([holdingsWalk.idsFold]); C2, U and R handed under every variant
// ([handedYes]); then on every order ([threadChain.EachOrder]), F the first
// record of U's reply: C3, R after F and only plain records of U's reply or
// plain results records from F to the later of U and R ([holdingsPairing.see]);
// C4, R plain results; C5, no user record before F answers a call of U's
// reply; C6, no compaction boundary at or after F. It refuses, knowingly, some
// shapes the request pairs, none measured. One goroutine owns it; no lock.
type holdingsPairing struct {
	// marks is each record's, by handle - 1.
	marks []pairMark
}

// see files the marks of line, record h, whose message m the chain decoded
// once. A record past the chain's bound, or none, has none.
func (p *holdingsPairing) see(h chainHandle, line threadLine, m threadMessage) {
	if h <= 0 {
		return
	}
	for len(p.marks) < int(h) {
		p.marks = append(p.marks, 0)
	}
	var mark pairMark
	switch line.Head.Type {
	case recordAssistant:
		mark |= pairAssistant
		if plainReply(line) {
			mark |= pairPlainReply
		}
	case recordUser:
		mark |= pairUser
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

// flag adds mark to record h's marks, filed by see already; a record past the
// chain's bound, or none, has none.
func (p *holdingsPairing) flag(h chainHandle, mark pairMark) {
	if h > 0 && int(h) <= len(p.marks) {
		p.marks[h-1] |= mark
	}
}

// plainReply reports whether line, an assistant record, is a plain record of
// its reply ([pairPlainReply]). A mark its accessor cannot read counts as
// carried.
func plainReply(line threadLine) bool {
	virtual, virtualErr := line.IsVirtual()
	failed, failedErr := line.APIError()
	model, modelErr := line.Model()
	batch, batchErr := line.BatchToolUses(func(string) {})
	return virtualErr == nil && !virtual && failedErr == nil && !failed && modelErr == nil &&
		model != claudeSyntheticModel && batchErr == nil && !batch
}

// plainResults reports whether line, a user record whose message is m, is a
// plain results record ([pairPlainResults]). A mark or origin kind its accessor
// cannot read counts as carried.
func plainResults(line threadLine, m threadMessage) bool {
	if blocks := m.Blocks(); m.IsText() || blocks.Len() == 0 || !blocks.All(blockToolResult) {
		return false
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

// counts hands each, for every call id a user or assistant record's block
// carries, its key and whether the block is its use, once End decided chain.
func (p *holdingsPairing) counts(chain *threadChain, each func(key chainCallID, use bool)) {
	for h := chainHandle(1); int(h) <= len(p.marks); h++ {
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

// pairAsk is one pair asked of a thread, the records of a call's use and of
// its result, and whether [holdingsPairing.pairs] pairs them.
type pairAsk struct {
	use, result chainHandle
	paired      bool
	// reply is the use's reply, the pass's own.
	reply chainReplyID
}

// pairReply is what the pass gathers of one asked reply, in one order: its
// first record's place and that of the first user record holding a result of
// its calls.
type pairReply struct {
	first, results int
}

// pairs answers each of asks of chain, once End decided it, by rule (C) of
// [holdingsPairing], C1 aside. One pass over each order makes every test a few
// lookups: each asked reply's first place, prefix counts of the records neither
// plain kind and of changes of reply, the first user record holding a result
// of each asked reply's calls, and the last boundary. Linear, with no cap.
func (p *holdingsPairing) pairs(chain *threadChain, asks []*pairAsk) {
	replies := map[chainReplyID]*pairReply{}
	for _, a := range asks {
		a.paired = false
		reply, ok := chain.Reply(a.use)
		// C2 here, and again on every order, which holds U and R only where
		// every variant it is handed under hands them.
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
	// THE CALLS OF EACH ASKED REPLY, from every record carrying its id, handed
	// or not: C5's results.
	answering := map[chainCallID][]chainReplyID{}
	for h := chainHandle(1); int(h) <= len(p.marks); h++ {
		reply, ok := chain.Reply(h)
		if !ok || replies[reply] == nil {
			continue
		}
		for _, key := range chain.Uses(h) {
			if !slices.Contains(answering[key], reply) {
				answering[key] = append(answering[key], reply)
			}
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
		// previous is the last assistant record's reply, met whether one was.
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
			case bad[end+1]-bad[first] != 0: // C3 and C4: only plain records from F to the later
				a.paired = false
			case changes[end+1]-changes[first+1] != 0: // C3: every reply record of U's reply
				a.paired = false
			case r.results >= 0 && r.results < first: // C5: 2.1.322 regroups such a split reply's results after it
				a.paired = false
			case lastBoundary >= first: // C6: the relaunch's first request cuts the conversation there
				a.paired = false
			}
		}
		return true
	})
}
