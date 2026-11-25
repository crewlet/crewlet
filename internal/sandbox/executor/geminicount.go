package executor

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
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

// geminiAnswer is one answer as the count reads it: its estimate in tokens,
// its response's JSON bytes, and the records that carry it.
type geminiAnswer struct {
	tokens, bytes int64
	carriers      []string
}

// geminiCall is one call as the count reads it: its name's length, and the
// records that carry it (a reply's toolCalls, or a compression's copy of it
// as a content part).
type geminiCall struct {
	name     int
	carriers []string
}

// geminiCounting is one walk's account of a Gemini CLI chat file for its
// count ([geminiCounting.line]): each record's largest copy, in quarter
// tokens and its text's UTF-8 bytes; each answer's largest; each call; the
// records removed; the order records came in and whether each is a
// compression's summary; and what the walk could not read.
type geminiCounting struct {
	quarters, bytes map[string]int64
	answers         map[string]*geminiAnswer
	calls           map[string]*geminiCall
	removed         map[string]bool
	order           []string
	snapshot        map[string]bool
	compressions    geminiCompressionLines[string]
	marks           countMarks
}

func newGeminiCounting() *geminiCounting {
	return &geminiCounting{quarters: map[string]int64{}, bytes: map[string]int64{},
		answers: map[string]*geminiAnswer{}, calls: map[string]*geminiCall{}, removed: map[string]bool{},
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
// as copies of their records and removes what it names, and one that orders
// the records (`orderIds`, whose effect on the history was not measured) is
// unread; an entry is a copy of its record. Anything else is unread. A user
// entry is a compression's summary by its lines ([geminiCompressionLines]),
// known a line late.
func (c *geminiCounting) line(raw []byte) {
	user := c.read(raw)
	if summary, ok := c.compressions.line(raw, user, user != ""); ok {
		c.snapshot[summary] = true
	}
}

// read reads one line ([geminiCounting.line]) and is the id of the user
// entry it holds, "" for none.
func (c *geminiCounting) read(raw []byte) string {
	if lineSeparators(raw) {
		c.marks = c.marks.with(sandbox.CountUnread)
		return ""
	}
	var w geminiCountLine
	if !wireObject(raw) || wireAlias(raw, &w) != nil {
		c.marks = c.marks.with(sandbox.CountUnread)
		return ""
	}
	user := ""
	switch {
	case !absent(w.Patch):
		var patch struct {
			Updates   []json.RawMessage `json:"updates"`
			RemoveIDs []string          `json:"removeIds"`
			OrderIDs  json.RawMessage   `json:"orderIds"`
		}
		if wireDecode(w.Patch, &patch) != nil || !absent(patch.OrderIDs) {
			c.marks = c.marks.with(sandbox.CountUnread)
			return ""
		}
		for _, u := range patch.Updates {
			var update geminiCountLine
			if wireAlias(u, &update) != nil {
				c.marks = c.marks.with(sandbox.CountUnread)
				return ""
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
			return ""
		}
		if id := c.record(w); kind == "user" {
			user = id
		}
	}
	if len(c.quarters)+len(c.answers)+len(c.calls)+len(c.removed) > geminiCountIDs {
		c.marks = c.marks.with(sandbox.CountUnread)
	}
	return user
}

// record reads one copy of a record, and is its id, "" where it has none.
func (c *geminiCounting) record(w geminiCountLine) string {
	var id string
	if wireDecode(w.ID, &id) != nil || id == "" {
		c.marks = c.marks.with(sandbox.CountUnread)
		return ""
	}
	if _, seen := c.quarters[id]; !seen {
		c.order = append(c.order, id)
	}
	q, text := c.content(id, w.Content)
	thoughtQ, thoughtText := c.thoughts(w.Thoughts)
	q += thoughtQ + c.toolCalls(id, w.ToolCalls)
	c.quarters[id] = max(c.quarters[id], q)
	c.bytes[id] = max(c.bytes[id], text+thoughtText)
	return id
}

// content is the quarter tokens of a record's content, a text or its parts,
// its answers kept apart and its calls kept, and its texts' UTF-8 bytes.
func (c *geminiCounting) content(id string, raw json.RawMessage) (int64, int64) {
	if absent(raw) {
		return 0, 0
	}
	var text string
	if wireDecode(raw, &text) == nil {
		return geminiTextQuarters(text), int64(len(text))
	}
	var parts []json.RawMessage
	if wireDecode(raw, &parts) != nil {
		parts = []json.RawMessage{raw}
	}
	var q, bytes int64
	for i, part := range parts {
		var p struct {
			Text     json.RawMessage `json:"text"`
			Answer   json.RawMessage `json:"functionResponse"`
			Call     json.RawMessage `json:"functionCall"`
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
			q += geminiTextQuarters(s)
			bytes += int64(len(s))
		case !absent(p.Answer):
			c.answer(id, i, p.Answer)
		case !absent(p.Inline), !absent(p.FileData):
			c.marks = c.marks.with(sandbox.CountMedia)
		default:
			if !absent(p.Call) {
				var call struct {
					ID   json.RawMessage `json:"id"`
					Name json.RawMessage `json:"name"`
				}
				if wireAlias(p.Call, &call) != nil {
					c.marks = c.marks.with(sandbox.CountUnread)
				}
				c.call(id, i, call.ID, call.Name)
			}
			n, err := jsLength(part)
			if err != nil {
				c.marks = c.marks.with(sandbox.CountUnread)
			}
			q += n
		}
	}
	return q, bytes
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
// call is kept and each result read as an answer.
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
		c.call(id, -(i + 1), call.ID, call.Name)
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

// call keeps one call of record id under its id, or with none under at, its
// place in the record (a content part's index, a toolCalls entry's below
// zero), with its name's length.
func (c *geminiCounting) call(id string, at int, rawID, rawName json.RawMessage) {
	var key, name string
	if wireDecode(rawID, &key) != nil || key == "" {
		key = "\x00" + id + "\x00" + strconv.Itoa(at)
	}
	_ = wireDecode(rawName, &name)
	got := c.calls[key]
	if got == nil {
		got = &geminiCall{}
		c.calls[key] = got
	}
	got.name = max(got.name, len(name))
	if n := len(got.carriers); n == 0 || got.carriers[n-1] != id {
		got.carriers = append(got.carriers, id)
	}
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

// count is the count c read, under marks the read set before the walk, with
// taken bytes put back ([chatCounter]): the bound, rounded up;
// [sandbox.CountMasked] where the answers reach [geminiMaskProtect] or the
// history's bytes as 0.70.0 sizes it for its collapse (its texts' UTF-8 and
// its answers' JSON), with the context allowed, reach [geminiCollapseBytes];
// restarted at the newest summary kept. Only
// what a kept record carries is restored, so a kept call whose every answer
// sits in a removed record gets the synthetic answer a resume hands it.
func (c *geminiCounting) count(marks countMarks, taken int64) *sandbox.ResumedCount {
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
	kept := func(carriers []string) bool {
		return slices.ContainsFunc(carriers, func(id string) bool { return !c.removed[id] })
	}
	for _, a := range c.answers {
		if kept(a.carriers) {
			answers, view, responses = answers+a.tokens, view+a.bytes, responses+1
		}
	}
	for callID, call := range c.calls {
		if a := c.answers[callID]; kept(call.carriers) && (a == nil || !kept(a.carriers)) {
			answers += (int64(call.name) + geminiSyntheticAnswerBytes + 3) / 4
			view, responses = view+geminiSyntheticAnswerBytes, responses+1
		}
	}
	// WHAT THE SCRUB TOOK OUT may have been any answer's or text's: a token
	// a byte on the bound, its bytes on the history, a quarter on the answers.
	quarters += 4 * (answers + taken)
	view += taken
	// A COLLAPSE PREVIEWS ONLY ANSWERS older than the newest three tool turns,
	// so a history of three answers or fewer has none it changes.
	collapses := view+geminiContextAllowance >= geminiCollapseBytes && responses > geminiCollapseKeptTurns
	if answers+(taken+3)/4 >= geminiMaskProtect || collapses {
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
// units a quarter a unit, unless it holds [redact.Marker].
//
// A TEXT HOLDING A MARKER IS CHARGED BY THE SHORT RULE WHATEVER ITS LENGTH:
// the rule is no sum of its characters, and a marker the scrub wrote in place
// of a shorter value can carry a kept text past the line the box's own copy
// stays under, at a sixth of its charge. Every text the scrub changes holds a
// marker and every other is kept as it was, so with what the scrub took out
// ([scrubTally]) the count never falls below the copy's.
func geminiTextQuarters(s string) int64 {
	marked := strings.Contains(s, redact.Marker)
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
	if units > 100_000 && !marked {
		return units
	}
	return q
}

// meter reads a Gemini CLI archive for the take's meter ([carrier.meter]):
// the count of its one chat file, by the rules of [geminiRelease] alone, its
// bound no estimate of the next request (the launch's floor reads none).
func (GeminiCLI) meter(ctx context.Context, r io.Reader, _, release string, _ *passBudget) (meterReading, error) {
	return chatMeter(ctx, newGeminiCounting(), r, geminiChatMember, release, geminiRelease)
}

// sessionTraits is how a relaunch judges a Gemini CLI session: in the box the
// launch gets, since a box's settings can move its point ([geminiPoint]);
// calls keyed by their place in the log, since its reading reads by content
// (geminiholdings.go); a brief at a byte a token, above the 0.75 a byte
// Gemini CLI's estimate reaches at most.
func (GeminiCLI) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingByLogPlace,
		Charge: 1}
}
