package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A Grok Build take's RECEIPT (ADR-0177's hook receipts, measured on 1.0.92):
// the session's record (chat_history.jsonl) keeps each hook answer Grok handed
// its model whole, as a line of its own: a PostToolUse or PostToolUseFailure
// answer as a system reminder naming the run's hook, a Stop block as the stop
// hook's feedback. A take is held where its token ends such a line and a
// model's answer follows it: the request that carried it completed. A hook
// Grok killed or cut short hands nothing over and leaves no token. The record
// is walked for notes ([lineWalk.noteLine]), holding no line.

var (
	_ mainThreadHarness = Grok{}
	_ threadHarness     = Grok{}
)

// threadTraits is what a Grok Build run's main-thread read settles: its hooks'
// receipts and its newest summary, read off the session's record. It counts
// nothing.
func (Grok) threadTraits() sandbox.ThreadTraits {
	return sandbox.ThreadTraits{Reads: true, Receipts: true, Summaries: true}
}

// readMainThread answers the receipts of the takes ask asks: held where the
// record shows each delivered and sent; absent where the job is over and it
// does not; undecided otherwise. The same walk reads its newest summary.
func (Grok) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	var reading sandbox.MainThreadReading
	var summary *summaryRead
	if ask.Summary {
		if why := summaryReleaseWhy(ask.Release, grokSupported()); why != "" {
			reading.Summary = summaryUnread(why)
		} else {
			summary = &summaryRead{}
		}
	}
	if len(ask.Takes) == 0 && summary == nil {
		return reading
	}
	undecided := func(why string) sandbox.MainThreadReading {
		if len(ask.Takes) > 0 {
			reading.Receipts = sandbox.TakeReceipts{Why: why}
		}
		if summary != nil {
			reading.Summary = summaryUnread(why)
		}
		return reading
	}
	if !sandbox.ValidSessionID(ask.Session) {
		return undecided(fmt.Sprintf("the run's Grok Build session %q is no session id its record is named by",
			ask.Session))
	}
	record, err := box.OpenFile(ctx, paths.GrokSessionsKey()+"/"+ask.Session+"/chat_history.jsonl")
	if err != nil {
		return undecided(fmt.Sprintf("the run's Grok Build session %s has no record in the box: %v", ask.Session, err))
	}
	held, err := grokHeld(ctx, record, summary)
	_ = record.Close()
	if err != nil {
		return undecided("the run's session record could not be read whole: " + err.Error())
	}
	if summary != nil {
		reading.Summary = summary.reading()
	}
	if len(ask.Takes) == 0 {
		return reading
	}
	var receipts sandbox.TakeReceipts
	for _, asked := range ask.Takes {
		switch {
		case held[claudeTakeToken(asked.Take)]:
			receipts.Held = append(receipts.Held, asked.Take)
		case ask.Ended:
			receipts.Absent = append(receipts.Absent, asked.Take)
		}
	}
	if len(receipts.Held) == 0 && len(receipts.Absent) == 0 {
		receipts.Why = "the run's session record does not show the takes yet, and its job is not over"
	}
	reading.Receipts = receipts
	return reading
}

// grokPlace stands in a head of [grokHookAnswers] for a hook's place in its
// event, the digits Grok writes there: the run's hooks come after the box's
// own ([grokMergeConfig]), so their place is the box's and changes with it,
// between the runs a session lives through. The take token the words end with
// tells the run's answer from another hook's.
const grokPlace = "%d"

// grokPlaceDigits is the most digits a place is read with: ten thousand hooks
// for one event in a home's settings is no box's.
const grokPlaceDigits = 4

// grokReminderTail closes a system reminder.
const grokReminderTail = "\n</system-reminder>"

// grokStopHead opens a Stop hook's feedback as the record keeps it, `Stop hook
// feedback:\n- <reason>\n` (measured on 1.0.92).
const grokStopHead = "Stop hook feedback:\n- "

// grokHookAnswers are the hooks' answers a user line hands the model: the
// reason the line gives, the head that opens its words, a hook of the home's
// settings (`user`) named by its event and place ([grokPlace]; measured on
// 1.0.92), and the tail that closes them. No head opens another; a
// repository's hook would be named under another scope.
var grokHookAnswers = [...]grokHookAnswer{
	{"system_reminder", "<system-reminder>\nContext from PostToolUse hook 'user:post_tool_use[" + grokPlace +
		"].hooks[0]':\n", grokReminderTail},
	{"system_reminder", "<system-reminder>\nContext from PostToolUseFailure hook 'user:post_tool_use_failure[" +
		grokPlace + "].hooks[0]':\n", grokReminderTail},
	{"stop_hook_feedback", grokStopHead, "\n"},
}

// grokHookAnswer is one of [grokHookAnswers].
type grokHookAnswer struct{ reason, head, tail string }

// opens is how many bytes of b its head opens, the place read as digits; 0
// where b does not open with it.
func (g grokHookAnswer) opens(b []byte) int {
	pre, post, placed := strings.Cut(g.head, grokPlace)
	if !bytes.HasPrefix(b, []byte(pre)) {
		return 0
	}
	if !placed {
		return len(pre)
	}
	n := len(pre)
	for n < len(b) && n-len(pre) < grokPlaceDigits && b[n] >= '0' && b[n] <= '9' {
		n++
	}
	if n == len(pre) || !bytes.HasPrefix(b[n:], []byte(post)) {
		return 0
	}
	return n + len(post)
}

// grokHeadMax is the longest head of [grokHookAnswers], its place at its most
// digits.
var grokHeadMax = func() int {
	n := 0
	for _, h := range grokHookAnswers {
		n = max(n, len(h.head)-len(grokPlace)+grokPlaceDigits)
	}
	return n
}()

// grokLine is a line of a session's record as the run's stream reads it
// ([grokEvents.record]).
type grokLine struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
	Reason  string          `json:"synthetic_reason"`
	Prompt  *int            `json:"prompt_index"`
	Calls   []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
	CallID string `json:"tool_call_id"`
}

// decodeGrokLine is line as the run's stream reads it, false where it is no
// JSON object.
func decodeGrokLine(line []byte) (grokLine, bool) {
	var l grokLine
	ok := json.Unmarshal(line, &l) == nil
	return l, ok
}

// text is a line's words: a string content, or its parts' text joined.
func (l grokLine) text() string {
	var s string
	if json.Unmarshal(l.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(l.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// grokHookText is what a hook of the run's settings handed the model in text,
// the words of a user line of reason, without its framing, and true; false
// where they are no such hook's.
func grokHookText(reason, text string) (string, bool) {
	for _, h := range grokHookAnswers {
		if n := h.opens([]byte(text)); n > 0 && h.reason == reason {
			return strings.TrimSuffix(text[n:], h.tail), true
		}
	}
	return "", false
}

// grokTokenOf is the take token words end with, "" for none.
func grokTokenOf(words string) string {
	words = strings.TrimRight(words, " \n")
	if len(words) < claudeTakeTokenLen {
		return ""
	}
	if token := words[len(words)-claudeTakeTokenLen:]; museIsToken(token) {
		return token
	}
	return ""
}

// grokHeld is the take tokens r, a session's record, shows held, as the receipt
// above says, reading its newest summary into summary where it is not nil
// ([grokNoteLine.summarised]). A line it cannot read with a later line after
// it makes the reading fail, since it may be the one that held or dropped a
// take, or the newest summary.
func grokHeld(ctx context.Context, r io.Reader, summary *summaryRead) (map[string]bool, error) {
	held := map[string]bool{}
	pending := map[string]bool{}
	var unread *museUnreadLine
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes)
	var l grokNoteLine
	if summary != nil {
		l.summary = &summaryText{}
	}
	for n := 1; ; n++ {
		line, err := w.noteLine(ctx, func() error { return l.read(&w) })
		switch {
		case err != nil:
			return nil, err
		case line.none:
			return held, nil
		case unread != nil:
			return nil, unread
		}
		switch {
		case line.past:
			unread = &museUnreadLine{at: n, why: fmt.Sprintf("is %d bytes, past the %d read of a line", line.size,
				maxLineBytes)}
		case line.null:
		case !line.object || !l.whole:
			unread = &museUnreadLine{at: n, why: "is not a record Grok Build writes"}
		case l.typ == "user":
			if token := l.token(); token != "" {
				pending[token] = true
			}
			if summary != nil {
				l.summarised(summary)
			}
		case l.typ == "assistant":
			for token := range pending {
				held[token] = true
			}
			clear(pending)
		}
		if line.ended {
			return held, nil
		}
	}
}

// grokNoteLine is what [grokHeld] reads of one line of a session's record:
// its type, its reason and what its words hand the model; whole where each
// member its whole-line decode reads ([grokLine]) is of its field's kind
// ([fieldKind]), and words where its content decodes as words ([grokLine.text]).
type grokNoteLine struct {
	typ, reason  string
	hook         grokHookRead
	whole, words bool
	// summary, where a read asks one, is the words as written.
	summary *summaryText
}

// grokCallFields are the members of a call [grokLine] decodes, by their
// fields' kinds.
var grokCallFields = map[string]fieldKind{"id": fieldString, "name": fieldString, "arguments": fieldString}

// read reads one line's members.
func (l *grokNoteLine) read(w *lineWalk) error {
	l.typ, l.reason, l.whole, l.words = "", "", true, false
	l.hook.reset()
	if l.summary != nil {
		l.summary.reset()
	}
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.typ, ok, err = w.wordField(k)
		case "synthetic_reason":
			l.reason, ok, err = w.wordField(k)
		case "content":
			l.words, err = l.content(w, k)
			ok = true
		case "prompt_index":
			_, ok, err = w.intField(k)
		case "tool_call_id":
			ok, err = w.typedField(k, fieldString)
		case "tool_calls":
			ok, err = w.listField(k, func(k jsonscan.Kind) error {
				took, callErr := w.typedObject(k, grokCallFields)
				l.whole = l.whole && took
				return callErr
			})
		default:
			return w.scan.Skip()
		}
		l.whole = l.whole && ok
		return err
	})
}

// content reads a line's content into its hook's reading, a string or each
// part's `text`, and reports whether it decodes as words: null, a value of
// another kind or a part of no such shape decodes as none.
func (l *grokNoteLine) content(w *lineWalk, k jsonscan.Kind) (bool, error) {
	l.hook.reset()
	to := l.summary.tee(&l.hook, k == jsonscan.String || k == jsonscan.ArrayStart)
	switch k {
	case jsonscan.String:
		return true, w.read(to)
	case jsonscan.ArrayStart:
	default:
		return false, w.scan.Skip()
	}
	words := true
	err := w.elements(func(k jsonscan.Kind) (bool, error) {
		took, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
			if name != "text" {
				return w.scan.Skip()
			}
			switch k {
			case jsonscan.String:
				return w.read(to)
			case jsonscan.Null:
				return nil
			}
			words = false
			return w.scan.Skip()
		})
		words = words && took
		return true, err
	})
	return words, err
}

// summarised makes the line's words, those of a user line, the newest
// summary where they are a compaction's, as the transcript reads one
// ([grokReading.summary]): of [grokCompactionReason], not opening with
// [grokUserInfoOpen], cut of a prompt's tags. Past the bound they cannot be
// told, and are the newest unsaid.
func (l *grokNoteLine) summarised(s *summaryRead) {
	t := l.summary
	switch {
	case l.reason != grokCompactionReason || !l.words || !t.set:
	case t.past:
		s.unshaped(errSummaryPast.Error())
	case !bytes.HasPrefix(t.buf, []byte(grokUserInfoOpen)):
		s.take(strings.TrimSuffix(strings.TrimPrefix(string(t.buf), grokQueryOpen), grokQueryClose))
	}
}

// token is the take token the line's words end with where they are a hook's
// answer of its reason, "" for none.
func (l *grokNoteLine) token() string {
	if !l.words {
		return ""
	}
	return l.hook.token(l.reason)
}

// grokHookRead reads a line's words as they are written for the hook's answer
// they hand the model ([grokHookText]): the head of [grokHookAnswers] that
// opens them, holding their opening until it is told, and the take token the
// rest ends with ([tokenEnd]).
type grokHookRead struct {
	opening []byte
	told    bool
	hook    int
	end     tokenEnd
}

// reset starts the words of a line.
func (h *grokHookRead) reset() {
	h.opening, h.told, h.hook = h.opening[:0], false, -1
}

func (h *grokHookRead) Write(p []byte) (int, error) {
	n := len(p)
	if !h.told {
		k := min(grokHeadMax-len(h.opening), len(p))
		h.opening, p = append(h.opening, p[:k]...), p[k:]
		if len(h.opening) < grokHeadMax {
			return n, nil
		}
		h.tell()
	}
	if h.hook >= 0 {
		_, _ = h.end.Write(p)
	}
	return n, nil
}

// tell tells the head the words open with, and reads what follows it.
func (h *grokHookRead) tell() {
	h.told = true
	for i, g := range grokHookAnswers {
		if n := g.opens(h.opening); n > 0 {
			h.hook = i
			h.end.reset(g.tail)
			_, _ = h.end.Write(h.opening[n:])
			return
		}
	}
}

// token is the take token the words end with where they are the answer of a
// hook a line of reason hands, "" for none.
func (h *grokHookRead) token(reason string) string {
	if !h.told {
		h.tell()
	}
	if h.hook < 0 || grokHookAnswers[h.hook].reason != reason {
		return ""
	}
	return h.end.token()
}

// tokenEnd reads words as they are written for the take token they end with
// ([grokTokenOf]) once tail, where it ends them, is cut off: it holds tail's
// length back, and of the words before it the token's length that ends where
// strings.TrimRight(words, " \n") would.
type tokenEnd struct {
	tail       string
	delay      []byte
	last, tok  [claudeTakeTokenLen]byte
	lastN      int
	n, trimmed int64
}

// reset starts words whose tail is tail.
func (e *tokenEnd) reset(tail string) {
	*e = tokenEnd{tail: tail, delay: e.delay[:0]}
}

func (e *tokenEnd) Write(p []byte) (int, error) {
	n, k := len(p), len(e.tail)
	if len(p) >= k {
		e.pass(e.delay)
		e.pass(p[:len(p)-k])
		e.delay = append(e.delay[:0], p[len(p)-k:]...)
		return n, nil
	}
	e.delay = append(e.delay, p...)
	if over := len(e.delay) - k; over > 0 {
		e.pass(e.delay[:over])
		e.delay = e.delay[:copy(e.delay, e.delay[over:])]
	}
	return n, nil
}

// pass reads p, words before the tail held back.
func (e *tokenEnd) pass(p []byte) {
	i := len(p) - 1
	for i >= 0 && (p[i] == ' ' || p[i] == '\n') {
		i--
	}
	if i >= 0 {
		keepLast(e.tok[:], e.last[:e.lastN], p[:i+1])
		e.trimmed = e.n + int64(i) + 1
	}
	e.lastN = keepLast(e.last[:], e.last[:e.lastN], p)
	e.n += int64(len(p))
}

// token ends the words, and is the take token they end with, "" for none.
func (e *tokenEnd) token() string {
	if string(e.delay) != e.tail {
		e.pass(e.delay)
	}
	e.delay = e.delay[:0]
	if e.trimmed < int64(claudeTakeTokenLen) {
		return ""
	}
	if token := string(e.tok[:]); museIsToken(token) {
		return token
	}
	return ""
}

// keepLast writes the last len(dst) bytes of a and then b to dst's start, a
// held in dst's own array or not, and is how many there are.
func keepLast(dst, a, b []byte) int {
	if len(b) >= len(dst) {
		return copy(dst, b[len(b)-len(dst):])
	}
	from := max(0, len(a)-(len(dst)-len(b)))
	n := copy(dst, a[from:])
	return n + copy(dst[n:], b)
}
