package executor

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT QWEN CODE COUNTS A CONTINUED SESSION AT, at its first request (0.28.4,
// measured): a RECORDED figure, never an estimate of the history, while one
// is recorded: the newest reply's prompt (its total where it names none) and
// output, or the newest compression's new size, beside a quarter of the
// brief's UTF-16 units and a dated reminder; it compresses where that is at
// least its point ([qwenPoint]). The count is an upper bound of it read whole
// off the kept chat file ([qwenCounting]); twenty tool-answer images after
// the restart compress whatever the figure ([sandbox.CountMedia]).

// qwenSyntheticFailureBytes is the failure 0.28.4 hands a call with no answer
// at its first request (151 bytes, measured), counted a token a byte.
const qwenSyntheticFailureBytes = 151

// qwenImageCompaction is how many images in tool answers 0.28.4 compacts at
// whatever the figure (19 did not, 20 did).
const qwenImageCompaction = 20

// qwenCounting is one walk's account of a Qwen Code chat file for its count
// ([qwenCounting.line]): the anchor's figure, the bytes of every message
// after it, the calls no answer followed, the images since the restart, and
// what the walk could not read.
type qwenCounting struct {
	anchored, compacted, unsure bool
	base, tail                  int64
	restart                     string
	open                        map[string]bool
	images                      int
	marks                       countMarks
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
// its new size the figure where it reads, and its history's images counted.
func (c *qwenCounting) compression(w qwenCountLine) {
	c.restart = ""
	_ = wireDecode(w.UUID, &c.restart)
	c.open, c.images = map[string]bool{}, 0
	var payload struct {
		Info struct {
			New *int64 `json:"newTokenCount"`
		} `json:"info"`
		History []json.RawMessage `json:"compressedHistory"`
	}
	if wireDecode(w.Payload, &payload) != nil || payload.Info.New == nil || *payload.Info.New <= 0 {
		c.anchored, c.compacted, c.unsure, c.tail = false, true, false, 0
		return
	}
	for _, content := range payload.History {
		c.images += qwenImages(content)
	}
	c.anchored, c.compacted, c.unsure, c.base, c.tail = true, false, false, *payload.Info.New, 0
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

// count is the count c read, under marks the read set before the walk: the
// anchor's figure, every message after it and a failure for each call left
// unanswered; [sandbox.CountCompacted] for a compression whose size does not
// read, [sandbox.CountUsage] where nothing recorded a figure or the newest
// usage does not read.
func (c *qwenCounting) count(marks countMarks) *sandbox.ResumedCount {
	marks |= c.marks
	switch {
	case c.compacted && !c.anchored:
		marks = marks.with(sandbox.CountCompacted)
	case !c.anchored || c.unsure:
		marks = marks.with(sandbox.CountUsage)
	}
	if c.images >= qwenImageCompaction {
		marks = marks.with(sandbox.CountMedia)
	}
	restart := c.restart
	out := &sandbox.ResumedCount{Restart: &restart}
	switch mark := marks.first(); mark {
	case "":
		out.Tokens = c.base + c.tail + int64(len(c.open))*qwenSyntheticFailureBytes
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
	var marks countMarks
	if release != qwenRelease {
		marks = marks.with(sandbox.CountRelease)
	}
	var c qwenCounting
	err := chatMemberLines(r, qwenChatMember, func(line []byte) error {
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

// sessionTraits is how a relaunch judges a Qwen Code session: in the box the
// launch gets, since a box's settings can move its point or its window
// ([qwenPoint]); no call keying, as no reading of its session exists; a brief
// at a byte a token, above 0.28.4's quarter of a UTF-16 unit.
func (QwenCode) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone, Charge: 1}
}

// qwenDefaultWindow is 0.28.4's DEFAULT_TOKEN_LIMIT, the window of a model no
// pattern names.
const qwenDefaultWindow = 200_000

// qwenLeastWindow is the least window [qwenWindow] names, for a run that
// pins no model (0.28.4 then runs its own default, whatever a box sets).
const qwenLeastWindow = 131_072

// qwenWindows is 0.28.4's PATTERNS, the first match of a normalized model
// its window ([qwenWindow]), the catalog off as the runner pins it
// (QWEN_CODE_MODELS_DEV=off).
var qwenWindows = []struct {
	re     *regexp.Regexp
	window int64
}{
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
	for _, w := range qwenWindows {
		if w.re == nil && qwenKimiK3(norm) || w.re != nil && w.re.MatchString(norm) {
			return w.window
		}
	}
	return qwenDefaultWindow
}

var (
	qwenRouteSuffix   = regexp.MustCompile(`:(?:free|beta|extended|thinking|online|nitro|floor|exacto|latest|\d+(?:\.\d+)?(?:x\d+)?b(?:-[\w.]+)*)$`)
	qwenSpaces        = regexp.MustCompile(`\s+`)
	qwenClaudeDot     = regexp.MustCompile(`^(claude-[a-z]+-\d+(?:-\d+)?)\.(\d+)(?:\.\d+)*`)
	qwenKeepsVersion  = regexp.MustCompile(`^qwen-(?:plus|flash|vl-max)-latest$|^kimi-k2-\d{4}$|^deepseek-v4`)
	qwenVersionSuffix = regexp.MustCompile(`^(?:\d{4,}|\d+x\d+b|v\d+(?:\.\d+)*|latest|exp)$`)
	qwenDotted        = regexp.MustCompile(`^\d+(?:\.\d+)+$`)
	qwenQuantSuffix   = regexp.MustCompile(`-(?:\d?bit|int[48]|bf16|fp16|q[45]|quantized)$`)
)

// qwenNormalized is 0.28.4's normalize(model), step for step; its one
// lookbehind (a dotted version stripped only after two segments) is spelled out.
func qwenNormalized(model string) string {
	s := strings.TrimSpace(strings.ToLower(model))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "|"); i >= 0 {
		s = s[i+1:]
	}
	s = qwenRouteSuffix.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	s = qwenSpaces.ReplaceAllString(s, "-")
	s = qwenClaudeDot.ReplaceAllString(s, "$1-$2")
	s = strings.ReplaceAll(s, "-preview", "")
	if !qwenKeepsVersion.MatchString(s) {
		if i := strings.LastIndex(s, "-"); i >= 0 {
			last, before := s[i+1:], s[:i]
			j := strings.LastIndex(before, "-")
			if qwenVersionSuffix.MatchString(last) || (qwenDotted.MatchString(last) && j >= 0 && j < len(before)-1) {
				s = before
			}
		}
	}
	return qwenQuantSuffix.ReplaceAllString(s, "")
}
