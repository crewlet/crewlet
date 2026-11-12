package executor

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHICH OF A ROUND'S CALLS A CLAUDE CODE SESSION HOLDS, READ OFF ITS KEPT
// ARCHIVE ([Runner.ReadHoldings]).
//
// Claude Code files a model's call under the id it sent the bridge
// ([sandbox.BridgeCall.CallID]): the call's use in an assistant record, its
// result in a user record after it. So a kept archive says, of each call a
// relaunch asks about, which records name it ([sandbox.CallRecords]), and
// [sandbox.SessionHoldings.Holds] alone turns those facts into a verdict. The
// reading reads the session's MAIN THREAD: a subagent's thread is a member it
// does not read, so a call recorded only there is absent from the reading, and
// told in the main list, as a call recorded in a deeper thread is.
//
// THE WALK, member by member as the take's scrub wrote the archive
// ([eachMember]), the reading's context asked before each:
//
//   - every member's RAW name first: one named by an absolute path or through
//     a `..` element is no reading, whichever member it is, since a tar that
//     restores it can write over the file a resumed session reads
//     ([claudeMemberOf]);
//   - each name, cleaned, is named through the line layer and never by a rule
//     of the reading's own ([claudeMemberOf]): the session's main thread is
//     read, and every other member (another session's file, a subagent's, a
//     deeper thread, the session's saved compaction under either of its names)
//     is passed over, its bytes read through the archive's meter as tar moves
//     past it;
//   - the main thread held twice, under two spellings, is no reading, since
//     tar restores the last copy and the reading could have read the other;
//     and one that is no regular file is no reading, as the box refuses one;
//   - the main thread is read a whole line at a time ([threadReader]), the
//     context asked between lines, every head fed to the thread's restarts
//     ([threadRestarts.See]), and every counted record's message decoded
//     ([threadLine.Message]): a user or assistant record, chained and the main
//     thread's own ([threadHead.Main]). Every other line counts for nothing.
//
// A SAVED COMPACTION IS NO QUESTION OF THE READING'S ([claudePrecompact]): it
// bears on whether a continuation's first request hands its model a summary in
// place of the history the archive holds, which the launch that continues the
// session judges before it asks for a reading at all.
//
// TWO METERS, ONE BUDGET. The archive's gzip stream is metered at
// [threadPlainMax] by the scrub's own meter ([boundedPlain]), which also bounds
// what tar passes over unread; and each member the reading reads is handed
// what is left of that budget once what the members before it handed out is
// counted ([handedReader]), so the reading's bound is the reader's own,
// whatever members an archive holds.
//
// THE STREAM IS READ TO ITS END BEFORE ANYTHING IS DECIDED. What the session
// store streams is provisional until io.EOF, and tar stops at its end of
// archive, so the reading drains the stream itself. gzip may already have read
// past tar's end and met the store's fault there: the drain meets it again
// because the store's stream latches its error. The gzip trailer is not
// checked: every byte under it came from a chunk the store authenticated.
//
// LIVE BY FILE ORDER: a result is live where its line follows the main
// thread's last restart. Measured (probe T, 2.1.310 and 2.1.322): a
// continuation hands its model every whole-line result its main thread holds
// after its last restart, verbatim and with its call, the results off the
// chain the thread's last record descends from among them, which parallel
// calls leave; and a result before a restart is told, the safe direction, a
// compaction's preserved segment included.
//
// RULE 4, THE LOGGED RESULT: a call is held only through its one result, whose
// is_error is the log's failure and whose text ([resultText]) is as long as the
// log's output and opens alike ([sandbox.HoldingsOpeningBytes]); a result
// whose content is no string and no one text block has no text a log's output
// can match. Rule 3 is met by it: a result written by a process that never saw
// the call's outcome (a resume's for a call it found open, a stopped harness's
// own, the text of a call its harness gave up on) never carries the log's
// text, so no mark such a process writes is read.
//
// EVERY OUTCOME. A reading: the stream read to its end within every bound, and
// each asked call's records counted. No reading, whole, and never a refusal of
// the relaunch, where the session's id names no thread; the ask asks about no
// call, holds more than a round's log lists, or asks about a call whose id is
// none the log keeps or is carried by another call of the ask; its writer is
// older than [holdingsWriterFloor], or unknown; its caller left while it
// waited for its slots, or between members or lines; the archive did not open,
// is no gzip or no tar, or inflates past its bound; a member is named
// absolutely or through `..`; the main thread is held twice, is no regular
// file, or is not there; a line of it is past its bound or is no record, or a
// counted record's message does not decode; or the stream does not end at
// io.EOF. The error says which, in words a relaunch logs ([holdingsError]).
//
// THE BOUNDS, AND WHAT A READING HOLDS. The archive's plaintext at
// [threadPlainMax], what its read members hand out at the same budget, a line
// at [threadLineMax], the asked calls at [sandbox.MaxBridgeCalls], and the
// writer at its floor. At its worst a reading holds the line reader's buffer;
// one line and the slice that gathered it; the line's raw keys and its
// message's decode, each at most a line; what the message's calls keep (a
// line of the smallest results keeps about three times the line); one result's
// text; and a fact of a few words for each asked call: about 75 MiB, inside
// the hundred or so one of the process's transcript slots is sized for
// ([TranscriptReadsAtOnce]).

// holdingsWriterFloor is the oldest Claude Code release whose sessions a
// reading reads ([ClaudeCode.holdingsFloor]).
//
// 2.1.310, THE OLDEST RELEASE THE READING'S RULES WERE MEASURED ON: the probes
// that settled them (T among them) ran 2.1.310 and 2.1.322. The reading
// decides from the shape of what a release recorded, and a release older than
// any measured could record a restart in a shape the restart rule does not
// know, which would read as none and hold a result its continuation is not
// handed. Not the carry floor a writer is held to ([claudeCarriedWriterFloor],
// 2.1.300), which says what another box can continue: a question of the
// harness's resume, not of how its records read. A writer below it, or none
// recorded, is no reading.
const holdingsWriterFloor = "2.1.310"

// holdingsFloor is [holdingsWriterFloor].
func (ClaudeCode) holdingsFloor() string { return holdingsWriterFloor }

// checkHoldings holds ask, and the session archive names, to what a reading
// answers, before anything is opened: a session whose id names a thread
// ([claudeMainThread]), and an ask of 1 to [sandbox.MaxBridgeCalls] calls, at
// least one of them asked, each asked call's id one the log keeps
// ([sandbox.ValidCallID]) and carried by no other call of the ask, asked or
// not.
//
// AN ID ANOTHER CALL CARRIES IS NO READING, NEVER A GUESS: the session files
// both calls' records under the one id, so no record says whose it is, and an
// ask that holds two such calls is a telling that broke its own rule.
func (ClaudeCode) checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error {
	if _, ok := claudeMainThread(archive.SessionID); !ok {
		return &holdingsError{why: "the session's id names no Claude Code thread"}
	}
	if len(ask.Calls) == 0 || len(ask.Calls) > sandbox.MaxBridgeCalls {
		return &holdingsError{why: fmt.Sprintf("the ask names %d calls, and a reading answers 1 to %d: "+
			"a round's log lists at most that many", len(ask.Calls), sandbox.MaxBridgeCalls)}
	}
	carried, asked := make(map[string]int, len(ask.Calls)), 0
	for _, c := range ask.Calls {
		carried[c.ID]++
		if c.Asked {
			asked++
		}
	}
	if asked == 0 {
		return &holdingsError{why: "the ask asks about none of its calls"}
	}
	for i, c := range ask.Calls {
		switch {
		case !c.Asked:
		case !sandbox.ValidCallID(c.ID):
			return &holdingsError{why: fmt.Sprintf("call %d of the ask is asked about, and names no id "+
				"the log keeps", i+1)}
		case carried[c.ID] > 1:
			return &holdingsError{why: fmt.Sprintf("call %d of the ask is asked about, and its id %q is "+
				"carried by another call of the ask, so no record says whose it is", i+1, c.ID)}
		}
	}
	return nil
}

// readHoldings reads body, archive's stream, within bounds, for the calls ask
// marks asked. See the file comment.
func (ClaudeCode) readHoldings(ctx context.Context, body io.Reader, archive sandbox.SessionArchive,
	ask sandbox.HoldingsAsk, bounds scrubBounds) (sandbox.SessionHoldings, error) {

	w := newHoldingsWalk(archive.SessionID, ask, bounds)
	if err := eachMember(body, bounds, func(h *tar.Header, member io.Reader) error {
		return w.member(ctx, h, member)
	}); err != nil {
		return sandbox.SessionHoldings{}, w.failed(ctx, err)
	}
	// DRAINED TO io.EOF, whatever tar left of it: what the store hands out
	// is provisional until then, and its stream latches its error, so a fault
	// gzip met past tar's end is met here again (the file comment).
	if _, err := io.Copy(io.Discard, body); err != nil {
		return sandbox.SessionHoldings{}, w.failed(ctx, &holdingsError{
			why: "the session's stream did not end whole", cause: err})
	}
	if !w.read[w.main] {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session's archive holds no main thread"}
	}
	return w.holdings(), nil
}

// holdingsWalk is one reading's state ([ClaudeCode.readHoldings]).
type holdingsWalk struct {
	// session is the session read, and main its main thread's member.
	session, main string
	// bounds is what the reading is held to, and handed what the members it
	// read have handed out of bounds.plain between them.
	bounds scrubBounds
	handed int64
	// read is every member read, by its cleaned name: a member named twice is
	// one the reading may have read in place of the one a resume reads.
	read map[string]bool
	// calls are the asked calls' facts, by id.
	calls map[string]*callFacts
	// restarts follows the main thread's heads, and lastRestart is the line
	// of its last restart, 0 where it has none.
	restarts    threadRestarts
	lastRestart int
}

// callFacts is what the main thread's counted records say of one asked call.
type callFacts struct {
	uses, results int
	// line is the place of its first result's line; failed, textBytes and
	// opening that result's ([sandbox.CallRecords]).
	line      int
	failed    bool
	textBytes int
	opening   [32]byte
}

// newHoldingsWalk is a reading of session's archive, within bounds, for the
// calls ask marks asked.
func newHoldingsWalk(session string, ask sandbox.HoldingsAsk, bounds scrubBounds) *holdingsWalk {
	main, _ := claudeMainThread(session)
	w := &holdingsWalk{session: session, main: main, bounds: bounds, read: map[string]bool{},
		calls: map[string]*callFacts{}}
	for _, c := range ask.Calls {
		if c.Asked {
			w.calls[c.ID] = &callFacts{}
		}
	}
	return w
}

// member reads one member of the archive, its header h and its bytes member.
func (w *holdingsWalk) member(ctx context.Context, h *tar.Header, member io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// THE RAW NAME, BEFORE IT IS CLEANED: cleaned, `projects/crewlet/x/../y`
	// is a name with no `..` in it, which a tar that restores the raw one
	// need not write where the cleaned one says.
	if outsideArchive(h.Name) {
		return &holdingsError{why: "the session's archive names a member " + memberNamed(h.Name) +
			" by an absolute path or through `..`, which a restore can write over the session's own files"}
	}
	name := path.Clean(h.Name)
	if m, ok := claudeMemberOf(w.session, name); !ok || m.Kind != memberMainThread {
		return nil
	}
	if w.read[name] {
		return &holdingsError{why: "the session's archive holds its main thread twice, and a restore " +
			"keeps the last copy"}
	}
	w.read[name] = true
	if h.Typeflag != tar.TypeReg {
		return &holdingsError{why: "the session's main thread is no regular file, which no box restores"}
	}
	return w.mainThread(ctx, member)
}

// outsideArchive reports whether name, a member's raw name, is absolute or
// holds a `..` element.
func outsideArchive(name string) bool {
	if strings.HasPrefix(name, "/") {
		return true
	}
	for element := range strings.SplitSeq(name, "/") {
		if element == ".." {
			return true
		}
	}
	return false
}

// mainThread reads the session's main thread, whose bytes member hands out, a
// whole line at a time.
func (w *holdingsWalk) mainThread(ctx context.Context, member io.Reader) error {
	lines := newThreadReader(&handedReader{r: member, handed: &w.handed}, w.bounds.plain-w.handed)
	for {
		// BETWEEN LINES, since a line's read takes no context: a reading
		// overruns its caller's wait by one line's read and decode at most.
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := lines.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return &holdingsError{why: "the session's main thread was not read whole", cause: err}
		}
		if w.restarts.See(line.Head) {
			w.lastRestart = line.N
		}
		if head := line.Head; (head.Type != recordUser && head.Type != recordAssistant) || !head.Chained() ||
			!head.Main() {
			continue
		}
		message, err := line.Message()
		if err != nil {
			return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread is no record", line.N),
				cause: err}
		}
		for _, call := range message.Calls() {
			w.count(call, line.N)
		}
	}
}

// count files call, a block of the main thread's line n, under its asked id:
// a use, or a result, the first result's facts kept.
func (w *holdingsWalk) count(call threadCall, n int) {
	facts := w.calls[call.ID]
	if facts == nil {
		return
	}
	switch call.Kind {
	case blockToolUse:
		facts.uses++
	case blockToolResult:
		facts.results++
		if facts.results > 1 {
			return
		}
		facts.line, facts.failed, facts.textBytes = n, call.IsError != nil && *call.IsError, -1
		if text, ok := resultText(call.Content); ok {
			facts.textBytes = len(text)
			facts.opening = sha256.Sum256([]byte(text[:min(sandbox.HoldingsOpeningBytes, len(text))]))
		}
	}
}

// failed is err, which ended the reading, as the reading answers it: its
// caller's own error where its caller left, whatever the stream made of the
// reading stopping under it; the reading's own words where it has them; and
// else the reading's words around it.
func (w *holdingsWalk) failed(ctx context.Context, err error) error {
	var own *holdingsError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &own):
		return err
	}
	return &holdingsError{why: "the session's archive was not read whole", cause: err}
}

// holdings is what the reading found: every asked call some counted record
// names, and Live where its first result's line follows the last restart.
func (w *holdingsWalk) holdings() sandbox.SessionHoldings {
	calls := make(map[string]sandbox.CallRecords, len(w.calls))
	for id, f := range w.calls {
		if f.uses == 0 && f.results == 0 {
			continue
		}
		calls[id] = sandbox.CallRecords{Threads: 1, Uses: f.uses, Results: f.results,
			Live: f.results > 0 && f.line > w.lastRestart, Failed: f.failed, TextBytes: f.textBytes,
			Opening: f.opening}
	}
	return sandbox.SessionHoldings{Read: true, Kind: sandbox.HoldingsByID, Calls: calls,
		Subagents: map[string]sandbox.SubagentThread{}}
}

// handedReader is a read member's bytes, counted into handed as they are
// handed out: what the members a reading read have handed out between them,
// of the one budget they share ([threadPlainMax]).
type handedReader struct {
	r      io.Reader
	handed *int64
}

// Read reads the member, counting what it hands out.
func (h *handedReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	*h.handed += int64(n)
	return n, err
}

// errNotOneText is a result's content that is neither a string nor one text
// block ([resultText]).
var errNotOneText = errors.New("not one text block")

// resultText is the text a tool_result's content, raw, holds: a JSON string, or
// a list of exactly one element, an object whose `type` is "text" and whose
// `text` is a string, other keys passed over, each key matched exactly and a
// key written twice read by its last value ([wireDecode]); false for every
// other content, absent and null among it.
//
// THE LIST IS READ NO FURTHER THAN ITS SECOND ELEMENT ([eachElement]), so a
// list a line long costs what its first two elements do.
func resultText(content json.RawMessage) (string, bool) {
	if len(content) > 0 && content[0] == '"' {
		var text string
		if err := wireDecode(content, &text); err != nil {
			return "", false
		}
		return text, true
	}
	text, blocks := "", 0
	err := eachElement(content, func(raw json.RawMessage) error {
		blocks++
		if blocks > 1 || !wireObject(raw) {
			return errNotOneText
		}
		var block struct {
			Type json.RawMessage `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if err := wireDecode(raw, &block); err != nil {
			return err
		}
		kind, err := wireRequiredString("type", block.Type)
		if err != nil || kind != blockText {
			return errNotOneText
		}
		text, err = wireRequiredString("text", block.Text)
		return err
	})
	return text, err == nil && blocks == 1
}
