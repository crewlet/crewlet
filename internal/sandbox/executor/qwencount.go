package executor

import (
	"cmp"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT QWEN CODE COUNTS A CONTINUED SESSION AT, at its first request (0.28.4,
// measured): a RECORDED figure, never an estimate of the history, while one
// is recorded: the newest reply's prompt (its total where it names none) and
// output, or the newest compression's new size, beside a quarter of the
// brief's UTF-16 units and a dated reminder; it compresses where that is at
// least its point ([qwenPoint]). The count is an upper bound of it read whole
// off the kept chat file ([qwenCounting]); twenty tool-answer images after
// the restart compress whatever the figure ([sandbox.CountMedia]), and tool
// answers past [qwenClearsAt] are cleared ([sandbox.CountMasked]).

// qwenSyntheticFailureBytes is the failure 0.28.4 hands a call with no answer
// at its first request (151 bytes, measured), counted a token a byte.
const qwenSyntheticFailureBytes = 151

// qwenImageCompaction is how many images in tool answers 0.28.4 compacts at
// whatever the figure (19 did not, 20 did).
const qwenImageCompaction = 20

// qwenClearsAt is the characters of built-in tool answers past which 0.28.4's
// first request replaces the oldest with `[Old tool result content cleared]`
// (over 500,000 in all; 25 of 24,508 had 15 cleared, MEASURE-P2 Q6.2). Every
// tool answer is counted toward it, an MCP tool's too: whether those are
// cleared, or count toward the 500,000, is not measured (one MCP answer past
// it was the newest, which is always kept), so an over-count only tells more.
const qwenClearsAt = 500_000

// qwenCountIDs bounds the calls one count keeps: those one message carries,
// and those left open at once. Past it the session is unread rather than held
// in memory: Qwen Code runs a reply's calls before its next request, so a
// session leaves open no more than one reply's, and no reply recorded carries
// more than 10; both sets at it hold 2.6 MiB at the word bound ([qwenCounting.pastBase]).
const qwenCountIDs = 1 << 12

// qwenCounting is one walk's account of a Qwen Code chat file for its count
// ([qwenCounting.line]): the anchor's figure, the bytes of every message
// after it, the calls no answer followed, the images and the bytes of tool
// answers since the restart, whether the restart's own history did not read,
// and what the walk could not read; and the line it reads.
type qwenCounting struct {
	anchored, compacted, unsure, historyUnread bool
	base, tail, answers                        int64
	restart                                    string
	// restartLong says the restart's uuid runs past the walk's word bound.
	restartLong bool
	open        map[string]bool
	images      int
	marks       countMarks
	l           qwenCountLine
}

// qwenCountLine is what the count reads of one line on the walk, each member
// as the whole-line read decoded it: its type and subtype, read as words; its
// uuid; and its usage, its message and its compression's payload, each read
// as it streams until its type picks which counts. typed says its type took
// its value, twice that a member the count reads was written twice ([twice]).
type qwenCountLine struct {
	kind, subtype string
	typed, twice  bool
	uuid          recordWord
	usage         qwenCountUsage
	message       qwenCountContent
	payload       qwenCountPayload
}

// qwenCountUsage is a reply's usage as the count reads it: present where the
// member is there and not null, bad where its decode refused it, and each
// figure with whether it was written ([qwenFigure]).
type qwenCountUsage struct {
	present, bad                          bool
	prompt, candidates, thoughts, total   int64
	hasPrompt, hasCandidates, hasThoughts bool
	hasTotal                              bool
}

// qwenCountContent is a message, or an entry of a compression's history, as the
// count reads it: its size as written; whether it is an object whose parts
// are a list of objects, as every decode of it asked (parts), its answers'
// own parts as the images' decode asked (images), and its calls' and
// answers' ids as the calls' decode asked (calls); its images, its answers'
// bytes, and its calls and answers in order, while within [qwenCountIDs].
type qwenCountContent struct {
	present, object             bool
	partsBad, imagesBad, idsBad bool
	size, answers               int64
	images                      int
	ops                         []qwenCountCall
	opsPast                     bool
}

// qwenCountCall is a part's call (opened) or answer (closed), by its id's key
// ([recordWord.key]).
type qwenCountCall struct {
	opened bool
	id     string
}

// imageCount is how many inline or file data parts the content holds,
// directly or in a tool answer's own parts, none where those do not read.
func (m *qwenCountContent) imageCount() int {
	if !m.object || m.partsBad || m.imagesBad {
		return 0
	}
	return m.images
}

// answerBytes is at least the characters of the tool answers the content
// holds, toward [qwenClearsAt]: each answer's bytes as the file holds them,
// which JSON.stringify wrote, never fewer than its UTF-16 units; the whole
// content where its parts do not read.
func (m *qwenCountContent) answerBytes() int64 {
	switch {
	case !m.present:
		return 0
	case !m.object || m.partsBad:
		return m.size
	}
	return m.answers
}

// qwenCountPayload is a compression's payload as the count reads it: whether it
// decoded (an object, its info one of a whole number or none), its new size,
// whether its history is a list, and that history's images and answers.
type qwenCountPayload struct {
	decoded, history bool
	newCount         int64
	hasNew           bool
	images           int
	answers          int64
}

// pastBase is never: the most a count keeps, [qwenCountIDs] calls open and as
// many of one message's, holds 2.6 MiB live at the word bound
// (BenchmarkAChatCountAtItsBounds' ids_long), within the meter's base.
func (*qwenCounting) pastBase() bool { return false }

// tee is the trim alone: no Qwen Code rule reads a line's bytes.
func (*qwenCounting) tee(trim *lineTrim) io.Writer { return trim }

// start starts the next line.
func (c *qwenCounting) start() {
	ops := c.l.message.ops[:0]
	c.l = qwenCountLine{}
	c.l.message.ops = ops
}

// The members a line's reader reads, a bit each for [twice].
const (
	qwenCountUUID uint16 = 1 << iota
	qwenCountType
	qwenCountSubtype
	qwenCountMessage
	qwenCountUsageMember
	qwenCountPayloadMember
)

// record reads the line's members.
func (c *qwenCounting) record(w *lineWalk) error {
	l := &c.l
	var seen uint16
	member := func(bit uint16) { l.twice = twice(&seen, bit) || l.twice }
	return w.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "uuid":
			member(qwenCountUUID)
			l.uuid, _, err = w.idField(k)
		case "type":
			member(qwenCountType)
			l.kind, l.typed, err = w.wordField(k)
		case "subtype":
			member(qwenCountSubtype)
			l.subtype, _, err = w.wordField(k)
		case "message":
			member(qwenCountMessage)
			err = w.qwenContent(k, &l.message, true, &l.twice)
		case "usageMetadata":
			member(qwenCountUsageMember)
			err = w.qwenUsage(k, &l.usage, &l.twice)
		case "systemPayload":
			member(qwenCountPayloadMember)
			err = w.qwenPayload(k, &l.payload, &l.twice)
		default:
			err = w.skip(k)
		}
		return err
	})
}

// line takes one line: a compression restarts the count at its new size; a
// reply with a figure anchors it again; every other message adds its bytes.
func (c *qwenCounting) line(object bool) {
	l := &c.l
	if !object || l.twice || !l.typed {
		c.marks = c.marks.with(sandbox.CountUnread)
		return
	}
	switch {
	case l.kind == "system" && l.subtype == "chat_compression":
		c.compression(l)
	case l.kind == "assistant":
		if figure, ok, read := qwenFigure(l.usage); !read {
			c.unsure = true
		} else if ok {
			c.anchored, c.unsure, c.base, c.tail = true, false, figure, 0
		}
		c.message(&l.message)
	case l.kind == "user" || l.kind == "tool_result":
		c.message(&l.message)
	}
}

// compression restarts the count at a chat_compression: its uuid the restart,
// its history's images and answers counted, its new size the figure where it
// reads. A history that does not read is marked for good: no later reply
// counts what it restores ([qwenCounting.count]).
func (c *qwenCounting) compression(l *qwenCountLine) {
	c.restart, c.restartLong = l.uuid.text, l.uuid.long
	c.open, c.images, c.answers, c.tail = map[string]bool{}, 0, 0, 0
	c.anchored, c.compacted, c.unsure = false, true, false
	p := &l.payload
	c.historyUnread = !p.decoded || !p.history
	if c.historyUnread {
		return
	}
	c.images, c.answers = p.images, p.answers
	if p.hasNew && p.newCount > 0 {
		c.anchored, c.compacted, c.base = true, false, p.newCount
	}
}

// qwenFigure is what 0.28.4 reads off a reply's usage: its prompt beside its
// output where the prompt is above zero (the larger of the total and the
// three parts), its total where it names no prompt; ok false where it reads
// none (the reply is passed over), read false where the usage does not decode.
func qwenFigure(u qwenCountUsage) (figure int64, ok, read bool) {
	switch {
	case !u.present:
		return 0, false, true
	case u.bad:
		return 0, false, false
	}
	of := func(set bool, v int64) int64 {
		if !set {
			return 0
		}
		return max(v, 0)
	}
	switch {
	case u.hasPrompt && u.prompt > 0:
		return max(of(u.hasTotal, u.total), u.prompt+of(u.hasCandidates, u.candidates)+
			of(u.hasThoughts, u.thoughts)), true, true
	case !u.hasPrompt && of(u.hasTotal, u.total) > 0:
		return u.total, true, true
	}
	return 0, false, true
}

// message adds a record's message to the tail, a token a byte, and reads its
// calls and answers and images.
func (c *qwenCounting) message(m *qwenCountContent) {
	if !m.present {
		return
	}
	c.tail += m.size
	c.images += m.imageCount()
	c.answers += m.answerBytes()
	if !m.object || m.partsBad || m.idsBad {
		return
	}
	if c.open == nil {
		c.open = map[string]bool{}
	}
	for _, op := range m.ops {
		if op.opened {
			c.open[op.id] = true
		} else {
			delete(c.open, op.id)
		}
	}
	if m.opsPast || len(c.open) > qwenCountIDs {
		c.marks = c.marks.with(sandbox.CountUnread)
	}
}

// qwenUsage reads a reply's usage, the value k begins, into u.
func (w *lineWalk) qwenUsage(k jsonscan.Kind, u *qwenCountUsage, again *bool) error {
	*u = qwenCountUsage{}
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ObjectStart:
	default:
		u.present, u.bad = true, true
		return w.skip(k)
	}
	u.present = true
	var seen uint16
	return w.members(func(name string, k jsonscan.Kind) error {
		var v *int64
		var set *bool
		var bit uint16
		switch name {
		case "promptTokenCount":
			v, set, bit = &u.prompt, &u.hasPrompt, 1
		case "candidatesTokenCount":
			v, set, bit = &u.candidates, &u.hasCandidates, 2
		case "thoughtsTokenCount":
			v, set, bit = &u.thoughts, &u.hasThoughts, 4
		case "totalTokenCount":
			v, set, bit = &u.total, &u.hasTotal, 8
		default:
			return w.skip(k)
		}
		*again = twice(&seen, bit) || *again
		*v, *set = 0, k != jsonscan.Null
		if k == jsonscan.Null {
			return nil
		}
		n, ok, err := w.intField(k)
		*v, u.bad = n, u.bad || !ok
		return err
	})
}

// qwenPayload reads a compression's payload, the value k begins, into p:
// its info and its history, each entry counted as a content ([qwenCountContent]).
func (w *lineWalk) qwenPayload(k jsonscan.Kind, p *qwenCountPayload, again *bool) error {
	*p = qwenCountPayload{}
	switch k {
	case jsonscan.Null:
		p.decoded = true
		return nil
	case jsonscan.ObjectStart:
	default:
		return w.skip(k)
	}
	p.decoded = true
	var seen uint16
	var entry qwenCountContent
	return w.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "info":
			*again = twice(&seen, 1) || *again
			p.newCount, p.hasNew = 0, false
			ok, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "newTokenCount" {
					return w.skip(k)
				}
				*again = twice(&seen, 4) || *again
				n, ok, err := w.intField(k)
				p.newCount, p.hasNew, p.decoded = n, k != jsonscan.Null, p.decoded && ok
				return err
			})
			p.decoded = p.decoded && ok
			return err
		case "compressedHistory":
			*again = twice(&seen, 2) || *again
			p.history, p.images, p.answers = k == jsonscan.ArrayStart, 0, 0
			if !p.history {
				return w.skip(k)
			}
			return w.elements(func(k jsonscan.Kind) (bool, error) {
				err := w.qwenContent(k, &entry, false, again)
				p.images += entry.imageCount()
				p.answers += entry.answerBytes()
				return true, err
			})
		}
		return w.skip(k)
	})
}

// qwenContent reads the content k begins into m, its calls and answers
// among it where calls.
func (w *lineWalk) qwenContent(k jsonscan.Kind, m *qwenCountContent, calls bool, again *bool) error {
	*m = qwenCountContent{ops: m.ops[:0]}
	if k == jsonscan.Null {
		return nil
	}
	m.present, m.object = true, k == jsonscan.ObjectStart
	var seen uint16
	size, err := w.sizedValue(k, func() error {
		if !m.object {
			return w.skip(k)
		}
		return w.members(func(name string, k jsonscan.Kind) error {
			if name != "parts" {
				return w.skip(k)
			}
			*again = twice(&seen, 1) || *again
			m.partsBad, m.imagesBad, m.idsBad, m.images, m.answers = false, false, false, 0, 0
			m.ops, m.opsPast = m.ops[:0], false
			ok, err := w.listField(k, func(k jsonscan.Kind) error { return w.qwenPart(k, m, calls, again) })
			m.partsBad = m.partsBad || !ok
			return err
		})
	})
	m.size = size
	return err
}

// The members of a part [lineWalk.qwenPart] reads, a bit each for [twice].
const (
	qwenCountPartInline uint16 = 1 << iota
	qwenCountPartFile
	qwenCountPartAnswer
	qwenCountPartCall
)

// qwenPart reads one part of a content, the value k begins, into m: its
// images, its answer's bytes, and its call or answer where calls.
func (w *lineWalk) qwenPart(k jsonscan.Kind, m *qwenCountContent, calls bool, again *bool) error {
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ObjectStart:
	default:
		m.partsBad = true
		return w.skip(k)
	}
	var seen uint16
	var opened, closed, inline, file bool
	var call, answer recordWord
	images := 0
	err := w.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "inlineData", "fileData":
			bit := qwenCountPartInline
			if name == "fileData" {
				bit = qwenCountPartFile
			}
			*again = twice(&seen, bit) || *again
			present := k != jsonscan.Null
			if name == "inlineData" {
				inline = present
				return w.skipInline(k)
			}
			file = present
			return w.skip(k)
		case "functionResponse":
			*again = twice(&seen, qwenCountPartAnswer) || *again
			closed, answer, images = k == jsonscan.ObjectStart, recordWord{}, 0
			if k == jsonscan.Null {
				return nil
			}
			if k != jsonscan.ObjectStart {
				m.imagesBad, m.idsBad = true, true
			}
			n, err := w.sizedValue(k, func() error {
				if k != jsonscan.ObjectStart {
					return w.skip(k)
				}
				var inner uint16
				return w.members(func(name string, k jsonscan.Kind) error {
					switch name {
					case "id":
						*again = twice(&inner, 1) || *again
						var ok bool
						var err error
						answer, ok, err = w.idField(k)
						m.idsBad = m.idsBad || !ok
						return err
					case "parts":
						*again = twice(&inner, 2) || *again
						images = 0
						ok, err := w.listField(k, func(k jsonscan.Kind) error {
							n, ok, err := w.qwenData(k, again)
							images += n
							m.imagesBad = m.imagesBad || !ok
							return err
						})
						m.imagesBad = m.imagesBad || !ok
						return err
					}
					return w.skip(k)
				})
			})
			m.answers += n
			return err
		case "functionCall":
			*again = twice(&seen, qwenCountPartCall) || *again
			opened, call = k == jsonscan.ObjectStart, recordWord{}
			var inner uint16
			ok, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "id" {
					return w.skip(k)
				}
				*again = twice(&inner, 1) || *again
				var ok bool
				var err error
				call, ok, err = w.idField(k)
				m.idsBad = m.idsBad || !ok
				return err
			})
			m.idsBad = m.idsBad || !ok
			return err
		}
		return w.skip(k)
	})
	if inline || file {
		images++
	}
	m.images += images
	switch {
	case !calls:
	case len(m.ops) >= qwenCountIDs && (opened || closed):
		m.opsPast = true
	case opened:
		m.ops = append(m.ops, qwenCountCall{opened: true, id: call.key()})
	case closed:
		m.ops = append(m.ops, qwenCountCall{id: answer.key()})
	}
	return err
}

// qwenData reads one of an answer's own parts, the value k begins, and is
// whether it is an image (inline or file data), and whether its decode took it.
func (w *lineWalk) qwenData(k jsonscan.Kind, again *bool) (int, bool, error) {
	switch k {
	case jsonscan.Null:
		return 0, true, nil
	case jsonscan.ObjectStart:
	default:
		return 0, false, w.skip(k)
	}
	var seen uint16
	var inline, file bool
	err := w.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "inlineData":
			*again = twice(&seen, 1) || *again
			inline = k != jsonscan.Null
		case "fileData":
			*again = twice(&seen, 2) || *again
			file = k != jsonscan.Null
		}
		if name == "inlineData" {
			return w.skipInline(k)
		}
		return w.skip(k)
	})
	if inline || file {
		return 1, true, err
	}
	return 0, true, err
}

// count is the count c read, under marks the read set before the walk, with
// taken bytes put back ([chatCounter]): the anchor's figure, every message
// after it and a failure for each call left unanswered;
// [sandbox.CountCompacted] for a compression whose size or
// history does not read, [sandbox.CountUsage] where nothing recorded a figure
// or the newest usage does not read, [sandbox.CountMasked] where the answers
// since the restart, a failure for each unanswered call among them, may reach
// [qwenClearsAt].
func (c *qwenCounting) count(marks countMarks, taken int64) *sandbox.ResumedCount {
	marks |= c.marks
	// A RESTART NO COUNT CARRIES (past [sandbox.RestartIDMaxBytes]) leaves the
	// count unread as it is read, as a record would hold it
	// ([sandbox.ResumedCount]), never a bound beside a restart cut short.
	if c.restartLong || len(c.restart) > sandbox.RestartIDMaxBytes {
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

// meter reads a Qwen Code archive for the take's meter ([carrier.meter]): the
// count of its one chat file, by the rules of [qwenRelease] alone, its bound
// no estimate of the next request (the launch's floor reads none).
func (QwenCode) meter(ctx context.Context, r io.Reader, _, release string, large *passBudget) (meterReading,
	error) {
	return chatMeter(ctx, new(qwenCounting), r, qwenChatMember, release, qwenRelease, large)
}

var _ mainThreadHarness = QwenCode{}

// threadTraits is what a Qwen Code run's main-thread read settles: its copy's
// count alone, a copy its harness was cut off in uncounted; its notes are
// confirmed off its record ([recordConfirmer]).
func (QwenCode) threadTraits() sandbox.ThreadTraits {
	return sandbox.ThreadTraits{Reads: true, Summaries: true}
}

// readMainThread counts the copy of ask's session a Qwen Code job's box holds,
// where ask asks ([chatCopyCount]): the one chat file naming it
// ([qwenChatFile]), its newest summary read in the same walk
// ([qwenSummaryReading]), or with no count off the notes' walk.
func (QwenCode) readMainThread(ctx context.Context, box sandbox.Sandbox, _ Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	var reading sandbox.MainThreadReading
	var summary *summaryRead
	if ask.Summary {
		if why := summaryReleaseWhy(ask.Release, qwenRelease); why != "" {
			reading.Summary = summaryUnread(why)
		} else {
			summary = &summaryRead{}
		}
	}
	if !ask.Count {
		if summary != nil {
			_, _ = QwenCode{}.notesHeld(ctx, box, Session{ID: ask.Session}, nil, summary)
			reading.Summary = summary.reading()
		}
		return reading
	}
	var file string
	if sandbox.ValidSessionID(ask.Session) {
		file, _ = qwenChatFile(ctx, box, ask.Session)
	}
	var counter chatCounter
	if ask.Count {
		counter = new(qwenCounting)
	}
	var lines *chatSummaryLines
	if summary != nil {
		lines = &newQwenSummaryReading(summary).chatSummaryLines
	}
	count, err := chatCopyCount(ctx, box, file, counter, ask.Release, qwenRelease, lines)
	reading.Count = count
	if summary != nil {
		if err = cmp.Or(err, lines.err); err != nil {
			summary.fail("the session's chat file was not read to its end: " + err.Error())
		}
		reading.Summary = summary.reading()
	}
	return reading
}

// sessionTraits is how a relaunch judges a Qwen Code session: in the box the
// launch gets, since a box's settings can move its point or its window
// ([qwenPoint]); its calls keyed by their place in the round's log, which its
// reading reads by content, as Qwen Code hands the bridge no call id
// ([QwenCode.readHoldings]); a brief at a byte a token, above 0.28.4's quarter
// of a UTF-16 unit.
func (QwenCode) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingByLogPlace,
		Charge: 1}
}

// qwenDefaultWindow is 0.28.4's DEFAULT_TOKEN_LIMIT, the window of a model no
// pattern names.
const qwenDefaultWindow = 200_000

// qwenLeastWindow is the least window [qwenWindow] names, for a run that
// pins no model (0.28.4 then runs its own default, whatever a box sets).
const qwenLeastWindow = 131_072

// qwenWindowPattern is one row of [qwenWindows]: a nil re is [qwenKimiK3].
type qwenWindowPattern struct {
	re     *regexp.Regexp
	window int64
}

// qwenWindows is 0.28.4's PATTERNS, the first match of a normalized model
// its window ([qwenWindow]), the catalog off as the runner pins it
// (QWEN_CODE_MODELS_DEV=off). Compiled on first use, never at init: every
// push's hook links this package (TestTheHookPaysLittleForTheEnginesInit).
var qwenWindows = sync.OnceValue(func() []qwenWindowPattern {
	return []qwenWindowPattern{
		{regexp.MustCompile(`^gemini-3`), 1_000_000}, {regexp.MustCompile(`^gemini-`), 1_000_000},
		{regexp.MustCompile(`^gpt-5`), 272_000}, {regexp.MustCompile(`^gpt-`), 131_072},
		{regexp.MustCompile(`^o\d`), 200_000}, {regexp.MustCompile(`^claude-opus-(?:4-(?:6|7|8)|5)`), 1_000_000},
		{regexp.MustCompile(`^claude-`), 200_000}, {regexp.MustCompile(`^qwen3-coder-plus`), 1_000_000},
		{regexp.MustCompile(`^qwen3-coder-flash`), 1_000_000}, {regexp.MustCompile(`^qwen3\.\d`), 1_000_000},
		{regexp.MustCompile(`^qwen4`), 1_000_000}, {regexp.MustCompile(`^qwen-plus-latest$`), 1_000_000},
		{regexp.MustCompile(`^qwen-flash-latest$`), 1_000_000}, {regexp.MustCompile(`^qwen-latest-series-`), 1_000_000},
		{regexp.MustCompile(`^coder-model$`), 1_000_000}, {regexp.MustCompile(`^qwen3-max`), 262_144},
		{regexp.MustCompile(`^qwen3-coder-`), 262_144}, {regexp.MustCompile(`^qwen`), 262_144},
		{regexp.MustCompile(`^deepseek-(?:v4|flash)`), 1_000_000}, {regexp.MustCompile(`^deepseek`), 131_072},
		{regexp.MustCompile(`^glm-5(\.[01])?(-|$)`), 202_752}, {regexp.MustCompile(`^glm-(?:[5-9]|\d{2,})`), 1_000_000},
		{regexp.MustCompile(`^glm-`), 202_752}, {regexp.MustCompile(`^minimax-m3`), 1_000_000},
		{regexp.MustCompile(`^minimax-m2\.5`), 196_608}, {regexp.MustCompile(`^minimax-`), 200_000},
		{regexp.MustCompile(`^(?:kimi-)?k3-256k$`), 262_144},
		// KIMI_K3_GENERATION, /^(?:kimi-)?k3(?!-256k)/, is [qwenKimiK3]: Go has no
		// lookahead.
		{nil, 1_000_000},
		{regexp.MustCompile(`^kimi-`), 262_144}, {regexp.MustCompile(`^seed-oss`), 524_288},
	}
})

// qwenKimiK3 is 0.28.4's KIMI_K3_GENERATION: `k3`, after `kimi-` or at the
// start, not followed by `-256k`.
func qwenKimiK3(s string) bool {
	k3 := func(rest string) bool {
		return strings.HasPrefix(rest, "k3") && !strings.HasPrefix(rest[2:], "-256k")
	}
	return k3(s) || strings.HasPrefix(s, "kimi-") && k3(s[len("kimi-"):])
}

// qwenWindow is the window 0.28.4 holds model to: its PATTERNS' first match on
// the model normalized ([qwenNormalized]), else its default; the least of the
// table for a run that names none. A box setting a window is marked, not read
// ([sandbox.PointSettings]).
func qwenWindow(model string) int64 {
	if strings.TrimSpace(model) == "" {
		return qwenLeastWindow
	}
	norm := qwenNormalized(model)
	for _, w := range qwenWindows() {
		if w.re == nil && qwenKimiK3(norm) || w.re != nil && w.re.MatchString(norm) {
			return w.window
		}
	}
	return qwenDefaultWindow
}

// qwenNormalizing is [qwenNormalized]'s expressions, compiled on first use
// as [qwenWindows] is.
var qwenNormalizing = sync.OnceValue(func() qwenNormalizers {
	return qwenNormalizers{
		routeSuffix:   regexp.MustCompile(`:(?:free|beta|extended|thinking|online|nitro|floor|exacto|latest|\d+(?:\.\d+)?(?:x\d+)?b(?:-[\w.]+)*)$`),
		spaces:        regexp.MustCompile(`\s+`),
		claudeDot:     regexp.MustCompile(`^(claude-[a-z]+-\d+(?:-\d+)?)\.(\d+)(?:\.\d+)*`),
		keepsVersion:  regexp.MustCompile(`^qwen-(?:plus|flash|vl-max)-latest$|^kimi-k2-\d{4}$|^deepseek-v4`),
		versionSuffix: regexp.MustCompile(`^(?:\d{4,}|\d+x\d+b|v\d+(?:\.\d+)*|latest|exp)$`),
		dotted:        regexp.MustCompile(`^\d+(?:\.\d+)+$`),
		quantSuffix:   regexp.MustCompile(`-(?:\d?bit|int[48]|bf16|fp16|q[45]|quantized)$`),
	}
})

// qwenNormalizers holds [qwenNormalizing]'s expressions.
type qwenNormalizers struct {
	routeSuffix, spaces, claudeDot, keepsVersion, versionSuffix, dotted, quantSuffix *regexp.Regexp
}

// qwenNormalized is 0.28.4's normalize(model), step for step; its one
// lookbehind (a dotted version stripped only after two segments) is spelled out.
func qwenNormalized(model string) string {
	n := qwenNormalizing()
	s := strings.TrimSpace(strings.ToLower(model))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "|"); i >= 0 {
		s = s[i+1:]
	}
	s = n.routeSuffix.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	s = n.spaces.ReplaceAllString(s, "-")
	s = n.claudeDot.ReplaceAllString(s, "$1-$2")
	s = strings.ReplaceAll(s, "-preview", "")
	if !n.keepsVersion.MatchString(s) {
		if i := strings.LastIndex(s, "-"); i >= 0 {
			last, before := s[i+1:], s[:i]
			j := strings.LastIndex(before, "-")
			if n.versionSuffix.MatchString(last) || (n.dotted.MatchString(last) && j >= 0 && j < len(before)-1) {
				s = before
			}
		}
	}
	return n.quantSuffix.ReplaceAllString(s, "")
}
