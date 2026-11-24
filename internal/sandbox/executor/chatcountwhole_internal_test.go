package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE WHOLE-LINE COUNTS OF A CHAT FILE (Qwen Code's and Gemini CLI's) are the
// oracles the counts on the walk are held to (chatcountscan_internal_test.go):
// the reads this build made before it, each line held to its bound and decoded
// whole, with what the walk decides applied where it decides it: a line
// trimmed of white space JSON takes no blank for is unread; a restart past
// [sandbox.RestartIDMaxBytes] leaves its count unread; a Qwen Code message of
// more calls and answers than [qwenCountIDs], or more calls open, and a Gemini
// CLI line holding more than [geminiCountBaseIDs] of a record's answers and
// calls or of a patch's parts, are unread. A
// key written twice in a member a count reads leaves the walk's count unread,
// which no oracle applies: a test of its own holds it.

// countWhole is a count's whole-line read: it takes a line, and counts.
type countWhole interface {
	line(raw []byte)
	count(marks countMarks, taken int64) *sandbox.ResumedCount
	unread()
}

func (c *qwenCountingWhole) unread()   { c.marks = c.marks.with(sandbox.CountUnread) }
func (c *geminiCountingWhole) unread() { c.marks = c.marks.with(sandbox.CountUnread) }

// chatFileLinesWhole hands c each line of r, one chat file, held whole to
// [codexRolloutLineMaxBytes] and trimmed ([codexLines]), a blank one passed
// over, a torn last one never read, one past the bound failing the read; a
// line with white space about it that JSON takes no blank for is unread, as
// the walk's scanner refuses it.
func chatFileLinesWhole(ctx context.Context, r io.Reader, c countWhole) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for n := 1; ; n++ {
		line, past, err := codexRolloutLine(br)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("executor: reading chat file line %d: %w", n, err)
		case past:
			return fmt.Errorf("%w: line %d", errChatLine, n)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		body := bytes.TrimSpace(line)
		switch {
		case len(body) == 0:
		case !bytes.Equal(body, bytes.Trim(line, " \t\r\n")):
			c.unread()
		default:
			c.line(body)
		}
	}
}

// countWholeOf reads chat into c under release's rules alone, and is the
// marks of its count and the read's error.
func countWholeOf(ctx context.Context, c countWhole, chat []byte, release, measured string) (countMarks, error) {

	var marks countMarks
	if release != measured {
		marks = marks.with(sandbox.CountRelease)
	}
	err := chatFileLinesWhole(ctx, bytes.NewReader(chat), c)
	if err != nil {
		marks = marks.with(sandbox.CountUnread)
	}
	return marks, err
}

// qwenCountingWhole is one walk's account of a Qwen Code chat file for its count
// ([qwenCountingWhole.line]): the anchor's figure, the bytes of every message
// after it, the calls no answer followed, the images and the bytes of tool
// answers since the restart, whether the restart's own history did not read,
// and what the walk could not read.
type qwenCountingWhole struct {
	anchored, compacted, unsure, historyUnread bool
	base, tail, answers                        int64
	restart                                    string
	open                                       map[string]bool
	images                                     int
	marks                                      countMarks
}

// qwenCountLineWhole is one record as the count reads it.
type qwenCountLineWhole struct {
	UUID    json.RawMessage `json:"uuid"`
	Type    json.RawMessage `json:"type"`
	Subtype json.RawMessage `json:"subtype"`
	Message json.RawMessage `json:"message"`
	Usage   json.RawMessage `json:"usageMetadata"`
	Payload json.RawMessage `json:"systemPayload"`
}

// qwenCountUsageWhole is a reply's usage as the count reads it, nil where absent.
type qwenCountUsageWhole struct {
	Prompt     *int64 `json:"promptTokenCount"`
	Candidates *int64 `json:"candidatesTokenCount"`
	Thoughts   *int64 `json:"thoughtsTokenCount"`
	Total      *int64 `json:"totalTokenCount"`
}

// line reads one record: a compression restarts the count at its new size; a
// reply with a figure anchors it again; every other message adds its bytes.
func (c *qwenCountingWhole) line(raw []byte) {
	var w qwenCountLineWhole
	var kind, subtype string
	if !wireObject(raw) || wireAlias(raw, &w) != nil || wireDecode(w.Type, &kind) != nil {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	_ = wireDecode(w.Subtype, &subtype)
	switch {
	case kind == "system" && subtype == "chat_compression":
		c.compression(w)
	case kind == "assistant":
		if figure, ok, read := qwenFigureWhole(w.Usage); !read {
			c.unsure = true
		} else if ok {
			c.anchored, c.unsure, c.base, c.tail = true, false, figure, 0
		}
		c.message(w.Message)
	case kind == "user" || kind == "tool_result":
		c.message(w.Message)
	}
}

// compression restarts the count at a chat_compression: its uuid the restart,
// its history's images and answers counted, its new size the figure where it
// reads. A history that does not read is marked for good: no later reply
// counts what it restores ([qwenCountingWhole.count]).
func (c *qwenCountingWhole) compression(w qwenCountLineWhole) {
	c.restart = ""
	_ = wireDecode(w.UUID, &c.restart)
	c.open, c.images, c.answers, c.tail = map[string]bool{}, 0, 0, 0
	c.anchored, c.compacted, c.unsure = false, true, false
	var payload struct {
		Info struct {
			New *int64 `json:"newTokenCount"`
		} `json:"info"`
		History json.RawMessage `json:"compressedHistory"`
	}
	var history []json.RawMessage
	c.historyUnread = wireDecode(w.Payload, &payload) != nil || absent(payload.History) ||
		wireDecode(payload.History, &history) != nil
	for _, content := range history {
		c.images += qwenImagesWhole(content)
		c.answers += qwenAnswerBytesWhole(content)
	}
	if !c.historyUnread && payload.Info.New != nil && *payload.Info.New > 0 {
		c.anchored, c.compacted, c.base = true, false, *payload.Info.New
	}
}

// qwenFigureWhole is what 0.28.4 reads off a reply's usage: its prompt beside its
// output where the prompt is above zero (the larger of the total and the
// three parts), its total where it names no prompt; ok false where it reads
// none (the reply is passed over), read false where the usage does not decode.
func qwenFigureWhole(raw json.RawMessage) (figure int64, ok, read bool) {
	if absent(raw) {
		return 0, false, true
	}
	var u qwenCountUsageWhole
	if wireDecode(raw, &u) != nil {
		return 0, false, false
	}
	of := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return max(*p, 0)
	}
	switch {
	case u.Prompt != nil && *u.Prompt > 0:
		return max(of(u.Total), *u.Prompt+of(u.Candidates)+of(u.Thoughts)), true, true
	case u.Prompt == nil && of(u.Total) > 0:
		return *u.Total, true, true
	}
	return 0, false, true
}

// message adds a record's message to the tail, a token a byte, and reads its
// calls and answers and images.
func (c *qwenCountingWhole) message(raw json.RawMessage) {
	if absent(raw) {
		return
	}
	c.tail += int64(len(raw))
	c.images += qwenImagesWhole(raw)
	c.answers += qwenAnswerBytesWhole(raw)
	var m struct {
		Parts []struct {
			Call *struct {
				ID string `json:"id"`
			} `json:"functionCall"`
			Answer *struct {
				ID string `json:"id"`
			} `json:"functionResponse"`
		} `json:"parts"`
	}
	if wireDecode(raw, &m) != nil {
		return
	}
	if c.open == nil {
		c.open = map[string]bool{}
	}
	calls := 0
	for _, p := range m.Parts {
		switch {
		case p.Call != nil:
			c.open[p.Call.ID], calls = true, calls+1
		case p.Answer != nil:
			delete(c.open, p.Answer.ID)
			calls++
		}
	}
	// THE WALK'S BOUND ON A MESSAGE'S CALLS AND ON THOSE OPEN ([qwenCountIDs]).
	if calls > qwenCountIDs || len(c.open) > qwenCountIDs {
		c.marks = c.marks.with(sandbox.CountUnread)
	}
}

// qwenAnswerBytesWhole is at least the characters of the tool answers a message's
// parts hold, toward [qwenClearsAt]: each answer's bytes as the file holds
// them, which JSON.stringify wrote, never fewer than its UTF-16 units; the
// whole message where its parts do not read.
func qwenAnswerBytesWhole(raw json.RawMessage) int64 {
	var m struct {
		Parts []struct {
			Answer json.RawMessage `json:"functionResponse"`
		} `json:"parts"`
	}
	if wireDecode(raw, &m) != nil {
		return int64(len(raw))
	}
	var n int64
	for _, p := range m.Parts {
		if !absent(p.Answer) {
			n += int64(len(p.Answer))
		}
	}
	return n
}

// qwenImagesWhole is how many inline or file data parts a message's parts hold,
// directly or in a tool answer's own parts.
func qwenImagesWhole(raw json.RawMessage) int {
	type data struct {
		Inline json.RawMessage `json:"inlineData"`
		File   json.RawMessage `json:"fileData"`
	}
	var m struct {
		Parts []struct {
			data
			Answer *struct {
				Parts []data `json:"parts"`
			} `json:"functionResponse"`
		} `json:"parts"`
	}
	if wireDecode(raw, &m) != nil {
		return 0
	}
	n := 0
	count := func(d data) {
		if !absent(d.Inline) || !absent(d.File) {
			n++
		}
	}
	for _, p := range m.Parts {
		count(p.data)
		if p.Answer != nil {
			for _, d := range p.Answer.Parts {
				count(d)
			}
		}
	}
	return n
}

// count is the count c read, under marks the read set before the walk, with
// taken bytes put back ([chatCounter]): the anchor's figure, every message
// after it and a failure for each call left unanswered;
// [sandbox.CountCompacted] for a compression whose size or
// history does not read, [sandbox.CountUsage] where nothing recorded a figure
// or the newest usage does not read, [sandbox.CountMasked] where the answers
// since the restart, a failure for each unanswered call among them, may reach
// [qwenClearsAt].
func (c *qwenCountingWhole) count(marks countMarks, taken int64) *sandbox.ResumedCount {
	marks |= c.marks
	// THE WALK'S RESTART PAST WHAT A COUNT CARRIES, unread as it is read.
	if len(c.restart) > sandbox.RestartIDMaxBytes {
		marks = marks.with(sandbox.CountUnread)
	}
	switch {
	case c.historyUnread || c.compacted && !c.anchored:
		marks = marks.with(sandbox.CountCompacted)
	case !c.anchored || c.unsure:
		marks = marks.with(sandbox.CountUsage)
	}
	// WHAT THE SCRUB TOOK OUT may have been any answer's: on the answers'
	// bytes, and a token a byte on the figure.
	if c.answers+taken+int64(len(c.open))*qwenSyntheticFailureBytes >= qwenClearsAt {
		marks = marks.with(sandbox.CountMasked)
	}
	if c.images >= qwenImageCompaction {
		marks = marks.with(sandbox.CountMedia)
	}
	restart := c.restart
	out := &sandbox.ResumedCount{Restart: &restart}
	switch mark := marks.first(); mark {
	case "":
		out.Tokens = c.base + c.tail + taken + int64(len(c.open))*qwenSyntheticFailureBytes
	case sandbox.CountUnread, sandbox.CountRelease:
		out.Mark, out.Restart = mark, nil
	default:
		out.Mark = mark
	}
	return out
}

// geminiCountingWhole is one walk's account of a Gemini CLI chat file for its
// count ([geminiCountingWhole.line]): each record's largest copy, in quarter
// tokens and its text's UTF-8 bytes; each answer's largest; each call; the
// records removed; the order records came in and whether each is a
// compression's summary; and what the walk could not read.
type geminiCountingWhole struct {
	// entries is the answers and calls the record read holds, which past
	// [geminiCountBaseIDs] leave the walk's count unread.
	entries         int
	quarters, bytes map[string]int64
	answers         map[string]*geminiAnswer
	calls           map[string]*geminiCall
	removed         map[string]bool
	order           []string
	snapshot        map[string]bool
	marks           countMarks
}

func newGeminiCountingWhole() *geminiCountingWhole {
	return &geminiCountingWhole{quarters: map[string]int64{}, bytes: map[string]int64{},
		answers: map[string]*geminiAnswer{}, calls: map[string]*geminiCall{}, removed: map[string]bool{},
		snapshot: map[string]bool{}}
}

// geminiCountLineWhole is one line of the chat file as the count reads it.
type geminiCountLineWhole struct {
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
// unread; an entry is a copy of its record. Anything else is unread.
func (c *geminiCountingWhole) line(raw []byte) {
	if lineSeparatorsWhole(raw) {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	var w geminiCountLineWhole
	if !wireObject(raw) || wireAlias(raw, &w) != nil {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	switch {
	case !absent(w.Patch):
		var patch struct {
			Updates   []json.RawMessage `json:"updates"`
			RemoveIDs []string          `json:"removeIds"`
			OrderIDs  json.RawMessage   `json:"orderIds"`
		}
		if wireDecode(w.Patch, &patch) != nil || !absent(patch.OrderIDs) {
			c.marks = c.marks.with(sandbox.CountUnread)
			return
		}
		// THE WALK HOLDS A PATCH'S PARTS, each update with its answers and
		// calls and each removal, to [geminiCountBaseIDs] until its line ends.
		held := len(patch.RemoveIDs)
		defer func() {
			if held > geminiCountBaseIDs {
				c.marks = c.marks.with(sandbox.CountUnread)
			}
		}()
		for _, u := range patch.Updates {
			var update geminiCountLineWhole
			if wireAlias(u, &update) != nil {
				c.marks = c.marks.with(sandbox.CountUnread)
				return
			}
			c.entries = 0
			c.record(update)
			held += 1 + c.entries
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
func (c *geminiCountingWhole) record(w geminiCountLineWhole) {
	var id string
	if wireDecode(w.ID, &id) != nil || id == "" {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	if _, seen := c.quarters[id]; !seen {
		c.order = append(c.order, id)
	}
	c.entries = 0
	defer func() {
		if c.entries > geminiCountBaseIDs {
			c.marks = c.marks.with(sandbox.CountUnread)
		}
	}()
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
// its answers kept apart and its calls kept, its texts' UTF-8 bytes, and
// whether it opens with a compression's summary.
func (c *geminiCountingWhole) content(id string, raw json.RawMessage) (int64, int64, bool) {
	if absent(raw) {
		return 0, 0, false
	}
	var text string
	if wireDecode(raw, &text) == nil {
		return geminiTextQuartersWhole(text), int64(len(text)), false
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
			if i == 0 && strings.HasPrefix(strings.TrimSpace(s), geminiStateSnapshot) {
				snapshot = true
			}
			q += geminiTextQuartersWhole(s)
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
			n, err := jsLengthWhole(part)
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
func (c *geminiCountingWhole) thoughts(raw json.RawMessage) (int64, int64) {
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
		q += geminiTextQuartersWhole(text)
		bytes += int64(len(text))
	}
	return q, bytes
}

// toolCalls is the quarter tokens of a reply's calls, each as the JSON of
// `{functionCall:{id,name,args}}` at a token a quarter, its id counted; each
// call is kept and each result read as an answer.
func (c *geminiCountingWhole) toolCalls(id string, raw json.RawMessage) int64 {
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
			l, err := jsLengthWhole(v)
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
func (c *geminiCountingWhole) call(id string, at int, rawID, rawName json.RawMessage) {
	var key, name string
	if wireDecode(rawID, &key) != nil || key == "" {
		key = "\x00" + id + "\x00" + strconv.Itoa(at)
	}
	_ = wireDecode(rawName, &name)
	c.entries++
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
func (c *geminiCountingWhole) answer(id string, at int, raw json.RawMessage) {
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
	c.entries++
	response := int64(4)
	if !absent(a.Response) {
		l, err := jsLengthWhole(a.Response)
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
func (c *geminiCountingWhole) count(marks countMarks, taken int64) *sandbox.ResumedCount {
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
	// THE WALK'S RESTART PAST WHAT A COUNT CARRIES, unread as it is read.
	if len(restart) > sandbox.RestartIDMaxBytes {
		marks = marks.with(sandbox.CountUnread)
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

// geminiTextQuartersWhole is Gemini CLI 0.70.0's estimate of a text, in quarter
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
func geminiTextQuartersWhole(s string) int64 {
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

// lineSeparatorsWhole reports whether raw, a chat file line, holds U+2028 or
// U+2029, raw or escaped: a JavaScript reader may split the line there, so
// the history it restores is not the one the line reads as. The take's meter
// reads the scrub's output, which writes both escaped (scrubjson.go), so the
// escaped form is the one a take meets; a text holding the six characters
// themselves reads as one too, which only tells more.
func lineSeparatorsWhole(raw []byte) bool {
	if bytes.Contains(raw, []byte(" ")) || bytes.Contains(raw, []byte(" ")) {
		return true
	}
	lower := bytes.ToLower(raw)
	return bytes.Contains(lower, []byte(`\u2028`)) || bytes.Contains(lower, []byte(`\u2029`))
}

// jsLengthWhole is the length of JSON.stringify of the JSON value raw, as UTF-8
// bytes, AT LEAST what JavaScript writes: a string's characters escaped as it
// escapes them (a quote, a backslash or one of five controls in two, another
// control in six, and an unreadable byte as a lone surrogate's six), a number
// at its own length where it has no exponent and at 25 where it has one (no
// double's shortest form is longer), and a name given twice counted twice,
// though JavaScript keeps one.
func jsLengthWhole(raw []byte) (int64, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), threadDecoding)
	type open struct {
		object bool
		n      int
	}
	var (
		stack []open
		n     int64
	)
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		kind := tok.Kind()
		if kind == '}' || kind == ']' {
			stack = stack[:len(stack)-1]
			n++
			continue
		}
		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			switch {
			case top.object && top.n%2 == 1:
				n++ // the name's colon
			case top.n > 0:
				n++ // the comma before
			}
			top.n++
		}
		switch kind {
		case '{', '[':
			stack = append(stack, open{object: kind == '{'})
			n++
		case 'n', 't':
			n += 4
		case 'f':
			n += 5
		case '0':
			if s := tok.String(); strings.ContainsAny(s, "eE") {
				n += 25
			} else {
				n += int64(len(s))
			}
		case '"':
			n += jsStringLengthWhole(tok.String())
		}
	}
}

// jsStringLengthWhole is the UTF-8 length of s as JSON.stringify writes it,
// quotes included ([jsLengthWhole]).
func jsStringLengthWhole(s string) int64 {
	n := int64(2)
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
			n += 2
		case r < 0x20 || r == utf8.RuneError:
			n += 6
		default:
			n += int64(utf8.RuneLen(r))
		}
	}
	return n
}
