package executor

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"

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

// qwenCounting is one walk's account of a Qwen Code chat file for its count
// ([qwenCounting.line]): the anchor's figure, the bytes of every message
// after it, the calls no answer followed, the images and the bytes of tool
// answers since the restart, whether the restart's own history did not read,
// and what the walk could not read.
type qwenCounting struct {
	anchored, compacted, unsure, historyUnread bool
	base, tail, answers                        int64
	restart                                    string
	open                                       map[string]bool
	images                                     int
	marks                                      countMarks
}

// qwenCountLine is one record as the count reads it.
type qwenCountLine struct {
	UUID    json.RawMessage `json:"uuid"`
	Type    json.RawMessage `json:"type"`
	Subtype json.RawMessage `json:"subtype"`
	Message json.RawMessage `json:"message"`
	Usage   json.RawMessage `json:"usageMetadata"`
	Payload json.RawMessage `json:"systemPayload"`
}

// qwenCountUsage is a reply's usage as the count reads it, nil where absent.
type qwenCountUsage struct {
	Prompt     *int64 `json:"promptTokenCount"`
	Candidates *int64 `json:"candidatesTokenCount"`
	Thoughts   *int64 `json:"thoughtsTokenCount"`
	Total      *int64 `json:"totalTokenCount"`
}

// line reads one record: a compression restarts the count at its new size; a
// reply with a figure anchors it again; every other message adds its bytes.
func (c *qwenCounting) line(raw []byte) {
	var w qwenCountLine
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
		if figure, ok, read := qwenFigure(w.Usage); !read {
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
// counts what it restores ([qwenCounting.count]).
func (c *qwenCounting) compression(w qwenCountLine) {
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
		c.images += qwenImages(content)
		c.answers += qwenAnswerBytes(content)
	}
	if !c.historyUnread && payload.Info.New != nil && *payload.Info.New > 0 {
		c.anchored, c.compacted, c.base = true, false, *payload.Info.New
	}
}

// qwenFigure is what 0.28.4 reads off a reply's usage: its prompt beside its
// output where the prompt is above zero (the larger of the total and the
// three parts), its total where it names no prompt; ok false where it reads
// none (the reply is passed over), read false where the usage does not decode.
func qwenFigure(raw json.RawMessage) (figure int64, ok, read bool) {
	if absent(raw) {
		return 0, false, true
	}
	var u qwenCountUsage
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
func (c *qwenCounting) message(raw json.RawMessage) {
	if absent(raw) {
		return
	}
	c.tail += int64(len(raw))
	c.images += qwenImages(raw)
	c.answers += qwenAnswerBytes(raw)
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
	for _, p := range m.Parts {
		switch {
		case p.Call != nil:
			c.open[p.Call.ID] = true
		case p.Answer != nil:
			delete(c.open, p.Answer.ID)
		}
	}
}

// qwenAnswerBytes is at least the characters of the tool answers a message's
// parts hold, toward [qwenClearsAt]: each answer's bytes as the file holds
// them, which JSON.stringify wrote, never fewer than its UTF-16 units; the
// whole message where its parts do not read.
func qwenAnswerBytes(raw json.RawMessage) int64 {
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

// qwenImages is how many inline or file data parts a message's parts hold,
// directly or in a tool answer's own parts.
func qwenImages(raw json.RawMessage) int {
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
func (c *qwenCounting) count(marks countMarks, taken int64) *sandbox.ResumedCount {
	marks |= c.marks
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
func (QwenCode) meter(ctx context.Context, r io.Reader, _, release string, _ *passBudget) (meterReading, error) {
	return chatMeter(ctx, new(qwenCounting), r, qwenChatMember, release, qwenRelease)
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
	var also func([]byte)
	var lines *qwenSummaryReading
	if summary != nil {
		lines = newQwenSummaryReading(summary)
		also = func(raw []byte) { lines.take(ctx, raw) }
	}
	count, err := chatCopyCount(ctx, box, file, counter, ask.Release, qwenRelease, also)
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
