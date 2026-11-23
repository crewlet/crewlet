package executor

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT GEMINI CLI COUNTS A CONTINUED SESSION AT, at its first request (0.70.0,
// measured): it compresses where the floor of its estimate of the history it
// restores (the records, a fresh session context, the synthetic answers a
// resume adds) is at least half the model's window, the brief not counted;
// and it masks tool output past the newest 50,000 estimated tokens, and
// collapses a history of 256 KiB, before it sends. The count is an upper bound
// of that estimate, read whole off the kept chat file ([geminiCounting]); the
// session context is the box's ([GeminiCLI.readBoxPoint]); a masking or a
// collapse the history may meet is [sandbox.CountMasked].

// geminiMaskProtect is the most tool output, in Gemini CLI's estimate, that no
// first request masks: 0.70.0 masks only where the newest outputs pass 50,000
// estimated tokens (strictly) and what lies past that point is at least
// 30,000, so the least output that masks is 50,001 (one part over the line).
// A count whose outputs reach it is marked, never bounded.
const geminiMaskProtect = 50_000

// geminiCollapseBytes is the size of history at which Gemini CLI 0.70.0
// collapses older tool output to previews at a resumed first request: the
// UTF-8 bytes of its text parts and answers, its session context included.
const geminiCollapseBytes = 256 << 10

// geminiCollapseKeptTurns is how many of the newest tool turns a collapse
// leaves whole (0.70.0, read and measured: six of nine answers collapsed).
const geminiCollapseKeptTurns = 3

// geminiContextAllowance is the most session context a box may hand a
// continuation, its tree and GEMINI.md files with it, that the count allows
// for beside the history's own bytes against [geminiCollapseBytes]; a box
// whose context is larger is no point ([sandbox.PointBoxSources]). 64 KiB is
// six times the 10,143 bytes measured for a workspace of 300 files.
const geminiContextAllowance = 64 << 10

// geminiCountIDs bounds the ids one count keeps: past 1<<16 records, answers
// and calls, the session is unread rather than held in memory.
const geminiCountIDs = 1 << 16

// geminiSyntheticAnswerBytes is the JSON of the answer a resume hands a call
// with none (213 bytes, 0.70.0). The turn it closes a history ending on a
// person's record with is the request's alone, never the estimate's (measured).
const geminiSyntheticAnswerBytes = 213

// geminiSnapshot opens a compression's summary record.
const geminiStateSnapshot = "<state_snapshot>"

// geminiAnswer is one answer as the count reads it: its estimate in tokens,
// its response's JSON bytes, and the records that carry it.
type geminiAnswer struct {
	tokens, bytes int64
	carriers      []string
}

// geminiCounting is one walk's account of a Gemini CLI chat file for its
// count ([geminiCounting.line]): each record's largest copy, in quarter
// tokens and its text's UTF-8 bytes; each answer's largest; each call's name;
// the records removed; the order records came in and whether each is a
// compression's summary; and what the walk could not read.
type geminiCounting struct {
	quarters, bytes map[string]int64
	answers         map[string]*geminiAnswer
	calls           map[string]int
	removed         map[string]bool
	order           []string
	snapshot        map[string]bool
	marks           countMarks
}

func newGeminiCounting() *geminiCounting {
	return &geminiCounting{quarters: map[string]int64{}, bytes: map[string]int64{},
		answers: map[string]*geminiAnswer{}, calls: map[string]int{}, removed: map[string]bool{},
		snapshot: map[string]bool{}}
}

// geminiCountLine is one line of the chat file as the count reads it.
type geminiCountLine struct {
	ID        json.RawMessage `json:"id"`
	Type      json.RawMessage `json:"type"`
	Content   json.RawMessage `json:"content"`
	Thoughts  json.RawMessage `json:"thoughts"`
	ToolCalls json.RawMessage `json:"toolCalls"`
	Patch     json.RawMessage `json:"$patch"`
	Set       json.RawMessage `json:"$set"`
	Rewind    json.RawMessage `json:"$rewindTo"`
	SessionID json.RawMessage `json:"sessionId"`
}

// line reads one line: the header, a `$set` or a `$rewindTo` (which only
// over-counts what it rewinds) are passed over; a `$patch` adds its updates
// as copies of their records and removes what it names; an entry is a copy
// of its record. Anything else is unread.
func (c *geminiCounting) line(raw []byte) {
	if lineSeparators(raw) {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	var w geminiCountLine
	if !wireObject(raw) || wireAlias(raw, &w) != nil {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	switch {
	case !absent(w.Patch):
		var patch struct {
			Updates   []json.RawMessage `json:"updates"`
			RemoveIDs []string          `json:"removeIds"`
		}
		if wireDecode(w.Patch, &patch) != nil {
			c.marks = c.marks.with(sandbox.CountUnread)
			return
		}
		for _, u := range patch.Updates {
			var update geminiCountLine
			if wireAlias(u, &update) != nil {
				c.marks = c.marks.with(sandbox.CountUnread)
				return
			}
			c.record(update)
		}
		for _, id := range patch.RemoveIDs {
			c.removed[id] = true
		}
	case !absent(w.Set), !absent(w.Rewind), !absent(w.SessionID) && absent(w.Type):
	default:
		var kind string
		if wireDecode(w.Type, &kind) != nil || (kind != "user" && kind != "gemini") {
			c.marks = c.marks.with(sandbox.CountUnread)
			return
		}
		c.record(w)
	}
	if len(c.quarters)+len(c.answers)+len(c.calls)+len(c.removed) > geminiCountIDs {
		c.marks = c.marks.with(sandbox.CountUnread)
	}
}

// record reads one copy of a record.
func (c *geminiCounting) record(w geminiCountLine) {
	var id string
	if wireDecode(w.ID, &id) != nil || id == "" {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	if _, seen := c.quarters[id]; !seen {
		c.order = append(c.order, id)
	}
	q, text, snapshot := c.content(id, w.Content)
	thoughtQ, thoughtText := c.thoughts(w.Thoughts)
	q += thoughtQ + c.toolCalls(id, w.ToolCalls)
	if snapshot {
		c.snapshot[id] = true
	}
	c.quarters[id] = max(c.quarters[id], q)
	c.bytes[id] = max(c.bytes[id], text+thoughtText)
}

// content is the quarter tokens of a record's content, a text or its parts,
// its answers kept apart, its texts' UTF-8 bytes, and whether it opens with a
// compression's summary.
func (c *geminiCounting) content(id string, raw json.RawMessage) (int64, int64, bool) {
	if absent(raw) {
		return 0, 0, false
	}
	var text string
	if wireDecode(raw, &text) == nil {
		return geminiTextQuarters(text), int64(len(text)), false
	}
	var parts []json.RawMessage
	if wireDecode(raw, &parts) != nil {
		parts = []json.RawMessage{raw}
	}
	var q, bytes int64
	snapshot := false
	for i, part := range parts {
		var p struct {
			Text     json.RawMessage `json:"text"`
			Answer   json.RawMessage `json:"functionResponse"`
			Inline   json.RawMessage `json:"inlineData"`
			FileData json.RawMessage `json:"fileData"`
		}
		if wireAlias(part, &p) != nil {
			c.marks = c.marks.with(sandbox.CountUnread)
			continue
		}
		switch {
		case !absent(p.Text):
			var s string
			if wireDecode(p.Text, &s) != nil {
				c.marks = c.marks.with(sandbox.CountUnread)
				continue
			}
			if i == 0 && strings.HasPrefix(strings.TrimSpace(s), geminiStateSnapshot) {
				snapshot = true
			}
			q += geminiTextQuarters(s)
			bytes += int64(len(s))
		case !absent(p.Answer):
			c.answer(id, i, p.Answer)
		case !absent(p.Inline), !absent(p.FileData):
			c.marks = c.marks.with(sandbox.CountMedia)
		default:
			n, err := jsLength(part)
			if err != nil {
				c.marks = c.marks.with(sandbox.CountUnread)
			}
			q += n
		}
	}
	return q, bytes, snapshot
}

// thoughts is the quarter tokens of a reply's thoughts, each counted as the
// text `**subject** description` the history holds, though never sent, and
// their UTF-8 bytes.
func (c *geminiCounting) thoughts(raw json.RawMessage) (int64, int64) {
	if absent(raw) {
		return 0, 0
	}
	var thoughts []struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	}
	if wireDecode(raw, &thoughts) != nil {
		c.marks = c.marks.with(sandbox.CountUnread)
		return 0, 0
	}
	var q, bytes int64
	for _, t := range thoughts {
		text := t.Description
		if t.Subject != "" {
			text = "**" + t.Subject + "** " + t.Description
		}
		q += geminiTextQuarters(text)
		bytes += int64(len(text))
	}
	return q, bytes
}

// toolCalls is the quarter tokens of a reply's calls, each as the JSON of
// `{functionCall:{id,name,args}}` at a token a quarter, its id counted; each
// call's name is kept and each result read as an answer.
func (c *geminiCounting) toolCalls(id string, raw json.RawMessage) int64 {
	if absent(raw) {
		return 0
	}
	var calls []struct {
		ID     json.RawMessage   `json:"id"`
		Name   json.RawMessage   `json:"name"`
		Args   json.RawMessage   `json:"args"`
		Result []json.RawMessage `json:"result"`
	}
	if wireDecode(raw, &calls) != nil {
		c.marks = c.marks.with(sandbox.CountUnread)
		return 0
	}
	var q int64
	for i, call := range calls {
		for j, part := range call.Result {
			var p struct {
				Answer json.RawMessage `json:"functionResponse"`
			}
			if wireAlias(part, &p) == nil && !absent(p.Answer) {
				c.answer(id, 1000*(i+1)+j, p.Answer)
			}
		}
		if absent(call.Name) {
			continue
		}
		var callID, name string
		_ = wireDecode(call.ID, &callID)
		_ = wireDecode(call.Name, &name)
		c.calls[callID] = max(c.calls[callID], len(name))
		n := int64(len(`{"functionCall":{"id":,"name":,"args":}}`))
		for _, v := range []json.RawMessage{call.ID, call.Name, call.Args} {
			if absent(v) {
				n += 4
				continue
			}
			l, err := jsLength(v)
			if err != nil {
				c.marks = c.marks.with(sandbox.CountUnread)
			}
			n += l
		}
		q += n
	}
	return q
}

// answer reads one answer of record id, the at'th it carries: its estimate,
// `(name + JSON(response)) / 4` rounded up, the largest of its copies, under
// its call's id (or, with none, its place in its record).
func (c *geminiCounting) answer(id string, at int, raw json.RawMessage) {
	var a struct {
		ID       json.RawMessage `json:"id"`
		Name     json.RawMessage `json:"name"`
		Response json.RawMessage `json:"response"`
		Parts    json.RawMessage `json:"parts"`
	}
	if wireAlias(raw, &a) != nil {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	if !absent(a.Parts) {
		// A TOOL'S MEDIA rides beside its response, estimated at no length.
		c.marks = c.marks.with(sandbox.CountMedia)
	}
	var key, name string
	if wireDecode(a.ID, &key) != nil || key == "" {
		key = "\x00" + id + "\x00" + strconv.Itoa(at)
	}
	_ = wireDecode(a.Name, &name)
	response := int64(4)
	if !absent(a.Response) {
		l, err := jsLength(a.Response)
		if err != nil {
			c.marks = c.marks.with(sandbox.CountUnread)
		}
		response = l
	}
	got := c.answers[key]
	if got == nil {
		got = &geminiAnswer{}
		c.answers[key] = got
	}
	got.tokens = max(got.tokens, (int64(len(name))+response+3)/4)
	got.bytes = max(got.bytes, response)
	got.carriers = append(got.carriers, id)
}

// count is the count c read, under marks the read set before the walk: the
// bound, rounded up; [sandbox.CountMasked] where the answers reach
// [geminiMaskProtect] or the history's bytes as 0.70.0 sizes it for its
// collapse (its texts' UTF-8 and its answers' JSON), with the context allowed,
// reach [geminiCollapseBytes]; restarted at the newest summary kept.
func (c *geminiCounting) count(marks countMarks) *sandbox.ResumedCount {
	marks |= c.marks
	var quarters, answers, view, responses int64
	restart := ""
	for _, id := range c.order {
		if c.removed[id] {
			continue
		}
		quarters += c.quarters[id]
		view += c.bytes[id]
		if c.snapshot[id] {
			restart = id
		}
	}
	for _, a := range c.answers {
		for _, carrier := range a.carriers {
			if !c.removed[carrier] {
				answers, view, responses = answers+a.tokens, view+a.bytes, responses+1
				break
			}
		}
	}
	for callID, name := range c.calls {
		if _, answered := c.answers[callID]; !answered {
			answers += (int64(name) + geminiSyntheticAnswerBytes + 3) / 4
			view, responses = view+geminiSyntheticAnswerBytes, responses+1
		}
	}
	quarters += 4 * answers
	// A COLLAPSE PREVIEWS ONLY ANSWERS older than the newest three tool turns,
	// so a history of three answers or fewer has none it changes.
	collapses := view+geminiContextAllowance >= geminiCollapseBytes && responses > geminiCollapseKeptTurns
	if answers >= geminiMaskProtect || collapses {
		marks = marks.with(sandbox.CountMasked)
	}
	out := &sandbox.ResumedCount{Restart: &restart}
	switch mark := marks.first(); mark {
	case "":
		out.Tokens = (quarters + 3) / 4
	case sandbox.CountUnread, sandbox.CountRelease:
		out.Mark, out.Restart = mark, nil
	default:
		out.Mark = mark
	}
	return out
}

// geminiTextQuarters is Gemini CLI 0.70.0's estimate of a text, in quarter
// tokens: a quarter for each UTF-16 unit of an ASCII character, a token and a
// half for each other unit (an unreadable byte one unit); a text past 100,000
// units a quarter a unit.
func geminiTextQuarters(s string) int64 {
	var units, q int64
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r <= 0x7f:
			units, q = units+1, q+1
		case r >= 0x10000:
			units, q = units+2, q+12
		default:
			units, q = units+1, q+6
		}
	}
	if units > 100_000 {
		return units
	}
	return q
}

// meter reads a Gemini CLI archive for the take's meter ([carrier.meter]):
// the count of its one chat file, by the rules of [geminiRelease] alone, its
// bound no estimate of the next request (the launch's floor reads none).
func (GeminiCLI) meter(ctx context.Context, r io.Reader, _, release string, _ *passBudget) (meterReading, error) {
	var marks countMarks
	if release != geminiRelease {
		marks = marks.with(sandbox.CountRelease)
	}
	c := newGeminiCounting()
	err := chatMemberLines(r, geminiChatMember, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.line(line)
		return nil
	})
	if err != nil {
		return meterReading{count: c.count(marks.with(sandbox.CountUnread))}, err
	}
	return meterReading{count: c.count(marks)}, nil
}

// sessionTraits is how a relaunch judges a Gemini CLI session: in the box the
// launch gets, since a box's settings can move its point ([geminiPoint]); no
// call keying, as no reading of its session exists; a brief at a byte a token,
// above the 0.75 a byte Gemini CLI's estimate reaches at most.
func (GeminiCLI) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone, Charge: 1}
}
