package executor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CLAUDE CODE THREAD, READ FOR WHAT IT RECORDS.
//
// A person's transcript (transcript.go) shows a session as well as it can,
// and a line it cannot read is an entry saying so. A reader that DECIDES
// something from what a session recorded (that a hook's note reached the
// model, that a call's result is held) cannot: one record it misreads is a
// wrong answer, not a gap on a page. Every such reader (the one that confirms
// a hook's note reached the model, and the one that tells a relaunch which of
// its calls a kept session holds) reads a Claude Code thread through this one
// layer, so the rules it is read by are held once. The take's context meter
// (measure.go) is no such reader and reads the main thread outside the layer:
// it decides nothing of what the session recorded, only reads one figure, the
// last request's input, for the launch floor, a best-effort figure that counts
// nothing from a line it cannot read. The rules:
//
//   - A MEMBER IS NAMED EXACTLY ([claudeMainThread], [claudeSubagentThread],
//     [claudeSubagentMeta], [claudePrecompact], and [claudeMemberOf] the other
//     way), never by [claudeThread], which takes any session's file for the
//     main thread: a reader that decides something of one session must never
//     read another session's file as it.
//   - A WHOLE LINE AT A TIME, through the scrub's own [readLine] at the scrub's
//     own bounds ([threadLineMax], [threadPlainMax]): one copy of the line
//     primitive, so a reader never holds a line the take's scrub would have
//     refused, and a last piece with no break is never a line
//     ([threadReader.Next]).
//   - PAST A BOUND, THE READING IS OVER: a line or a member past its bound
//     ends it, with nothing skipped, since a reader that skipped a line would
//     decide from a thread with a hole in it.
//   - EVERY WHOLE LINE IS DECODED into its head before anything reads it, with
//     no keyword prefilter ([threadHead]).
//   - A LIST IS READ AN ELEMENT AT A TIME ([eachElement]), each element held
//     to its type as it is read and never the whole list first, so a list a
//     box wrote costs its reader one element at a time.
//   - EVERY VALUE IS DECODED AS CLAUDE CODE'S OWN JSON.parse READS IT, under
//     one set of options ([threadDecoding]): a key exactly as written, never
//     matched by case as encoding/json matches a struct's fields; a key
//     written twice by its last value; and a string's invalid UTF-8 or lone
//     surrogate as the replacement character, never as no record.
//   - ABSENT IS NOT NULL: `parentUuid` is handed over as written
//     ([threadHead.Parent]).
//   - A KEY ONLY ONE READER READS IS HELD IN THAT READER'S ACCESSOR, so a
//     release that reshapes it costs that reader alone ([threadLine]).
//   - A MESSAGE THAT DOES NOT DECODE IS ITS LINE'S: a reader that decides from
//     a thread's messages asks [threadLine.Message] of every user and
//     assistant record of that thread, and takes its error as that line's, no
//     record, as it takes a *threadLineError, never as a line with no calls.
//   - A HOOK'S FIRING IS NEVER A CALL'S ID: no id leaves this layer AS an id
//     but a firing through [threadLine.Hook] and a call's through
//     [threadMessage.Calls] ([threadHook.Firing]). An id written inside a
//     record's text (a task notification's `<tool-use-id>`, a tool's output,
//     a call's input) is bytes the layer hands out raw, in a line's Raw, a
//     message's Text, a call's Input and Content and a record's
//     ToolUseResult: its reader's to parse, and to hold to a rule of its own.
//
// What these rules were measured on is Claude Code 2.1.310 and 2.1.322, driven
// as the engine drives a box's executor, and 2.1.118 beside 2.1.310 for a hook
// that follows one call (a skill's).

// threadLineMax is the longest whole line a Claude Code thread is read with,
// its break included: the scrub's own piece ([scrubPieceMaxBytes]). The take's
// scrub refused every session holding a longer line, so a reader of a kept
// archive meets one only where a redaction grew a line, and a reader of a
// box's live thread stops where the take would refuse the session.
const threadLineMax = scrubPieceMaxBytes

// threadPlainMax is the most of one member a reader reads: the plaintext one
// reading of an archive yields to the take's scrub at the bound a kept
// session's archive is held to ([scrubExpansion] times
// executorkeep.SessionMaxBytes, a quarter of a gibibyte, the figure
// [heldPlainMax] packs to), so a thread past it is one no kept session holds.
// The figure a reader of one whole thread passes as its bound; a reader of an
// archive's members meters the archive at it and hands each member what is
// left.
const threadPlainMax = int64(scrubExpansion) * executorkeep.SessionMaxBytes

// errThreadBound is a thread past a bound it is read within: a line longer
// than [threadLineMax], or a member longer than its reader's bound. The
// reading is over ([threadReader.Next]).
var errThreadBound = errors.New("executor: a Claude Code thread runs past a bound it is read within")

// errNotOneRecord is a whole line that does not open a JSON object: null, a
// list, a string, a number, or no JSON at all.
var errNotOneRecord = errors.New("not one JSON object")

// claudeProjectPrefix is the directory every member of a session lies under,
// relative to a Claude Code config directory and in its session's archive
// alike ([claudeProjectDir]).
const claudeProjectPrefix = "projects/" + claudeProjectDir + "/"

// claudeMainThread is the member holding a session's main thread, under a
// Claude Code config directory and in its archive alike:
// `projects/crewlet/<session>.jsonl`. False for a session id outside
// [sandbox.ValidSessionID]'s shape, which names no file.
//
// BY EXACT NAME, never [claudeThread]'s reading of a member: a person's
// transcript reads whatever session an archive holds, and takes ANY
// `projects/crewlet/<x>.jsonl` for the main thread, where a reader deciding
// something of one session would read another session's file as its own.
func claudeMainThread(session string) (string, bool) {
	if !sandbox.ValidSessionID(session) {
		return "", false
	}
	return claudeProjectPrefix + session + ".jsonl", true
}

// claudeSubagentThread is the member holding the thread of a session's
// subagent agent: `projects/crewlet/<session>/subagents/agent-<agent>.jsonl`.
// False for a session id outside [sandbox.ValidSessionID]'s shape or an agent
// id outside [validClaudeAgent]'s.
func claudeSubagentThread(session, agent string) (string, bool) {
	if !sandbox.ValidSessionID(session) || !validClaudeAgent(agent) {
		return "", false
	}
	return claudeSubagentPrefix(session) + agent + ".jsonl", true
}

// claudeSubagentMeta is the member holding subagent agent's metadata, beside
// its thread: `projects/crewlet/<session>/subagents/agent-<agent>.meta.json`.
// False where [claudeSubagentThread] is.
//
// ONE JSON DOCUMENT WITH NO LINE BREAK, NEVER A THREAD: 2.1.310 writes it so
// (every metadata measured ends in its closing brace with no break), and the
// take's scrub keeps it so. Read through [threadReader] it is no line at all,
// only a torn piece and io.EOF with no error, so its reader decodes it whole,
// within a bound of its own, under [threadDecoding].
func claudeSubagentMeta(session, agent string) (string, bool) {
	if !sandbox.ValidSessionID(session) || !validClaudeAgent(agent) {
		return "", false
	}
	return claudeSubagentPrefix(session) + agent + ".meta.json", true
}

// claudeSubagentPrefix is what every member of a session's subagent begins
// with, up to its agent id.
func claudeSubagentPrefix(session string) string {
	return claudeProjectPrefix + session + "/subagents/agent-"
}

// claudePrecompactFile is the name Claude Code saves a session's precomputed
// compaction under ([claudePrecompact]).
const claudePrecompactFile = "precompact.json"

// claudePrecompact is the two members a session's SAVED COMPACTION is read
// from, in the order Claude Code reads them:
// `projects/crewlet/<session>/precompact.json`, the one it writes, and the
// sibling `projects/crewlet/<session>.precompact.json`, which it reads only
// where the first is absent. False for a session id outside
// [sandbox.ValidSessionID]'s shape, which names no file.
//
// WHAT IT IS. Claude Code computes a compaction of a session ahead of its
// threshold, where flags its servers serve and two settings,
// precomputeCompactionEnabled and idleCompaction, allow it, and saves it
// there. A resume reads it back, up to seven days old, and swaps it in for a
// fresh summary where the threshold fires (read in 2.1.310's and 2.1.322's
// shipped code; no run measured wrote one). So a session that holds one can
// be handed a summary at its first request in place of the history it kept.
// A box's pack carries the first, in the session's directory it takes whole,
// and a restore puts it back in place.
//
// ONE KIND FOR BOTH ([memberPrecompact]): Claude Code reads either as the same
// saved compaction, the second only in the first's place, and every reader
// asks one question of either, whether the session holds one, so no reader
// can answer it for one name and miss the other, which would read a session
// that may be handed a summary as one that holds its history.
//
// ITS READER is the resumed count, which bounds what a continued session's
// harness counts before its first request: it reads a session holding either
// as one that may compact there, in a kept archive by its members' names
// ([claudeMemberOf]) and in a box by a look at each name. Its presence is the
// fact: nothing reads what it says.
func claudePrecompact(session string) ([]string, bool) {
	if !sandbox.ValidSessionID(session) {
		return nil, false
	}
	dir := claudeProjectPrefix + session
	return []string{dir + "/" + claudePrecompactFile, dir + "." + claudePrecompactFile}, true
}

// threadMemberKind is which of a session's members a name is.
type threadMemberKind int

const (
	// memberMainThread is the session's main thread ([claudeMainThread]).
	memberMainThread threadMemberKind = iota + 1
	// memberSubagentThread is a subagent's thread ([claudeSubagentThread]).
	memberSubagentThread
	// memberSubagentMeta is a subagent's metadata ([claudeSubagentMeta]).
	memberSubagentMeta
	// memberPrecompact is the session's saved compaction, under either name
	// [claudePrecompact] gives.
	memberPrecompact
)

// threadMember is a member of a session, as [claudeMemberOf] reads its name.
type threadMember struct {
	Kind threadMemberKind
	// Agent is the subagent's id; "" for the main thread and a saved
	// compaction.
	Agent string
}

// claudeMemberOf is which member of session the archive member named name is:
// the inverse of [claudeMainThread], [claudeSubagentThread],
// [claudeSubagentMeta] and [claudePrecompact], true exactly where one of them
// names it. Every other name is false: another session's file, a deeper path
// under the session's directory (a nested subagent's, a tool's spilled
// result, a saved compaction's name under the subagents' directory), a name
// beside one of them (`precompact.json.tmp`), `agent-.jsonl`, an agent id
// outside [validClaudeAgent]'s rule.
//
// THE NAME AS THE CALLER CLEANED IT (path.Clean), compared whole and never
// cleaned here: the caller owns the check for a member an archive holds twice
// under two spellings, and only it sees both. And the caller gives NO READING
// AT ALL of an archive any of whose members is named absolutely or holds a
// `..` element: path.Clean keeps `/projects/crewlet/<S>.jsonl` absolute, which
// every tar restores over the main thread by stripping its `/`, and turns
// `projects/crewlet/x/../<S>.jsonl` into the main thread's own name, which
// busybox's tar restores there too, so the member this answers for, or the
// one it refuses, need not be the file a resumed session holds.
func claudeMemberOf(session, name string) (threadMember, bool) {
	if !sandbox.ValidSessionID(session) {
		return threadMember{}, false
	}
	switch name {
	case claudeProjectPrefix + session + ".jsonl":
		return threadMember{Kind: memberMainThread}, true
	case claudeProjectPrefix + session + "/" + claudePrecompactFile,
		claudeProjectPrefix + session + "." + claudePrecompactFile:
		return threadMember{Kind: memberPrecompact}, true
	}
	rest, under := strings.CutPrefix(name, claudeSubagentPrefix(session))
	if !under {
		return threadMember{}, false
	}
	if agent, meta := strings.CutSuffix(rest, ".meta.json"); meta {
		if !validClaudeAgent(agent) {
			return threadMember{}, false
		}
		return threadMember{Kind: memberSubagentMeta, Agent: agent}, true
	}
	if agent, thread := strings.CutSuffix(rest, ".jsonl"); thread && validClaudeAgent(agent) {
		return threadMember{Kind: memberSubagentThread, Agent: agent}, true
	}
	return threadMember{}, false
}

// claudeAgentMaxBytes is the longest subagent id a member is named by.
//
// SIXTY-FOUR BYTES. 2.1.310 names a subagent `a` and sixteen hex digits (every
// subagent it ran when measured), and the bound leaves a later release room for
// a longer id.
const claudeAgentMaxBytes = 64

// validClaudeAgent reports whether agent is a subagent id a member is named
// by: 1 to [claudeAgentMaxBytes] bytes of letters, digits, `-` and `_`. So a
// slash, a dot or a NUL in an id never names a path, nor reaches what a reader
// tells anybody of it.
//
// ONE RULE FOR BOTH DIRECTIONS, from an id to its member and from a member's
// name back to its id ([claudeMemberOf]), so a name the layer reads back is
// one it would have written; and its own, never a call id's rule, since an
// agent's id is no call's and the two may move apart.
func validClaudeAgent(agent string) bool {
	return agent != "" && len(agent) <= claudeAgentMaxBytes &&
		!strings.ContainsFunc(agent, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
		})
}

// threadReader reads one member of a Claude Code session a whole line at a
// time, each line's head decoded ([threadReader.Next]). One goroutine reads
// it. A THREAD'S MEMBER: a subagent's metadata is one JSON document with no
// line break ([claudeSubagentMeta]), which it reads as nothing but a torn
// piece.
type threadReader struct {
	r *bufio.Reader
	// maxBytes is the member's bound, and left what of it the lines read so
	// far have not taken.
	maxBytes, left int64
	// n is how many whole lines have been read.
	n int
	// torn is whether the member ended in a piece with no break.
	torn bool
	// end is what ended the reading: io.EOF, errThreadBound, or an error of r.
	end error
}

// newThreadReader reads r, one member, a whole line at a time: at most maxBytes
// of it, the caller's figure ([threadPlainMax] for a whole thread, what is left
// of an archive's budget for one member of it); at or below zero, no byte. The
// member is a thread, never a subagent's metadata ([claudeSubagentMeta]).
func newThreadReader(r io.Reader, maxBytes int64) *threadReader {
	maxBytes = max(maxBytes, 0)
	return &threadReader{r: bufio.NewReaderSize(threadSource{r}, lineChunkBytes), maxBytes: maxBytes, left: maxBytes}
}

// threadSource is a member's reader whose every error but its end reaches
// [threadReader.Next] tagged as the reader's own ([threadSourceError]).
//
// TAGGED, because the line primitive's own bound is told by the error it
// wraps, errScrubBound, and a reader may wrap it too: a reader that meters a
// kept archive's plaintext with the take's scrub's own meter fails with the
// scrub's bound once the archive inflates past a reading. Untagged, that error
// would be read as a line or a member past this reader's bound, its own words
// and its errors.Is lost.
type threadSource struct{ r io.Reader }

// Read reads the member's reader, tagging every error but io.EOF.
func (s threadSource) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &threadSourceError{err: err}
	}
	return n, err
}

// threadSourceError is an error of a member's reader on its way through the
// line primitive ([threadSource]).
type threadSourceError struct{ err error }

// Error is the reader's error's own.
func (e *threadSourceError) Error() string { return e.err.Error() }

// Unwrap is the reader's error.
func (e *threadSourceError) Unwrap() error { return e.err }

// Next is the member's next WHOLE line, one that ends in "\n", its break
// dropped and its head decoded. It answers:
//
//   - io.EOF at the member's end. A last piece with no break is never a line:
//     it is a write its harness never finished, or the NULs GNU tar fills a
//     member out with when it shrank as it was packed, and a reader that
//     decoded it would take half a record for one. The box cuts exactly that
//     piece away before a harness resumes the session ([claudeSessionGate]'s
//     torn), but for two files it leaves as they are: one that holds no whole
//     line, and every subagent's thread in a box that has no find. There this
//     reader reads less than the harness may, and what it leaves is never a
//     whole record, so it holds nothing a reader counts.
//     [threadReader.Torn] says it was there.
//   - errThreadBound once a line runs past [threadLineMax] or the member past
//     the reader's bound, EVEN WHERE THE BOUND FALLS RIGHT AFTER A LINE BREAK:
//     metered with io.LimitReader, a member cut on a break reads to a clean
//     end, and a member past its bound reads as whole, the lines past the cut
//     neither read nor missed. The reading is over, and stays over: nothing
//     past the bound is read, and nothing is skipped to reach it.
//   - a *threadLineError for a whole line that is not one record (not one JSON
//     object, or a head key of the wrong type): the reader stays usable, and
//     what the line costs is the caller's to decide.
//   - any error of the member's reader, AS IT IS, which ends the reading: one
//     that wraps the take's scrub's bound, as a reader metering a kept
//     archive with the scrub's own meter returns, is that reader's, never
//     errThreadBound ([threadSource]).
func (t *threadReader) Next() (threadLine, error) {
	if t.end != nil {
		return threadLine{}, t.end
	}
	bound, member := threadLineMax, t.left < threadLineMax
	if member {
		bound = int(t.left)
	}
	raw, err := readLine(t.r, bound)
	var source *threadSourceError
	switch {
	case errors.As(err, &source):
		t.end = source.err
		return threadLine{}, t.end
	case errors.Is(err, errScrubBound) && member:
		t.end = fmt.Errorf("%w: the member runs past the %d bytes it is read within", errThreadBound, t.maxBytes)
		return threadLine{}, t.end
	case errors.Is(err, errScrubBound):
		t.end = fmt.Errorf("%w: a line runs past %d bytes", errThreadBound, threadLineMax)
		return threadLine{}, t.end
	case errors.Is(err, io.EOF):
		// readLine answers io.EOF only with what follows the last break.
		t.torn = len(raw) > 0
		t.end = io.EOF
		return threadLine{}, io.EOF
	case err != nil:
		t.end = err
		return threadLine{}, err
	}
	t.left -= int64(len(raw))
	t.n++
	return decodeThreadLine(t.n, raw[:len(raw)-1])
}

// Torn reports whether the member ended in a piece with no break, which
// [threadReader.Next] read as no line: known once Next has answered io.EOF.
func (t *threadReader) Torn() bool { return t.torn }

// threadLineError is a whole line that is not one record: not one JSON
// object, or a head key of the wrong type ([threadHead]).
type threadLineError struct {
	// N is the line's place among the member's whole lines, from 1.
	N int
	// Err is what the line's decode met.
	Err error
}

// Error says which line is no record, and why.
func (e *threadLineError) Error() string {
	return fmt.Sprintf("executor: line %d of a Claude Code thread is no record: %v", e.N, e.Err)
}

// Unwrap is what the line's decode met.
func (e *threadLineError) Unwrap() error { return e.Err }

// threadLine is one whole line of a thread: its place, its bytes and its head,
// beside the raw values of the keys the accessors below read, all from the
// line's one decode.
//
// WHAT IT HANDS OUT RAW IS VALID UNTIL THE READER'S NEXT [threadReader.Next]:
// the line's Raw, a hook's Content and Stdout, a call's Input and Content, and
// ToolUseResult. A reader that keeps any of them past that copies it, since
// the layer may hand out a line's own bytes rather than a copy of them.
//
// AND IT HANDS THEM OUT RAW ON PURPOSE: each has one reader, which holds it to
// the shape that reader reads (a SendMessage's `input.to`, a result's text, a
// hook's answer) under the layer's own options ([threadDecoding]) and walk
// ([eachElement]), so a release that reshapes one costs that reader alone, as
// a key one reader reads does.
//
// A KEY ONLY ONE READER READS IS HELD TO ITS TYPE IN THAT READER'S ACCESSOR,
// never in the head: a release that reshapes it is that accessor's error and
// costs the reader that asks, so a reshaped attachment never blinds a reader
// of calls, and a reshaped origin never blinds a reader of hooks. A key a
// reader comes to need is added beside these, read off the same decode, with
// its own accessor and its own error.
type threadLine struct {
	// N is the line's place among the member's whole lines, from 1.
	N int
	// Raw is the line, its break dropped, valid until the reader's next Next
	// (above). A carriage return before the break is the line's own, which
	// JSON reads as whitespace.
	Raw []byte
	// Head is what every reader needs of the line.
	Head threadHead

	message, attachment, origin, toolUseID, toolUseResult json.RawMessage
}

// threadWire is a whole line as its one decode ([wireDecode]) leaves it: every
// key this layer reads, by its exact name and raw, so a key that is absent is
// nil and one that is null holds the bytes `null`. Each is held to its type
// where it is read: the head's by [threadWire.head], every other by its
// accessor.
type threadWire struct {
	Type          json.RawMessage `json:"type"`
	Subtype       json.RawMessage `json:"subtype"`
	UUID          json.RawMessage `json:"uuid"`
	ParentUUID    json.RawMessage `json:"parentUuid"`
	IsSidechain   json.RawMessage `json:"isSidechain"`
	AgentID       json.RawMessage `json:"agentId"`
	ReplacesSpan  json.RawMessage `json:"replacesSpan"`
	Message       json.RawMessage `json:"message"`
	Attachment    json.RawMessage `json:"attachment"`
	Origin        json.RawMessage `json:"origin"`
	ToolUseID     json.RawMessage `json:"toolUseID"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// decodeThreadLine decodes whole line n, raw (its break dropped), into its
// head: ONE decode of the whole line ([wireDecode]), which holds every byte
// of it to JSON, then the head's keys held to their types. Nothing reads a
// line before this has: no keyword decides which lines are decoded, since a
// line no keyword marks can still be no record (a write that tore into
// another), and a reader that passed it unread would decide from the thread
// around it as though it were whole.
func decodeThreadLine(n int, raw []byte) (threadLine, error) {
	// HELD TO AN OBJECT BEFORE IT IS DECODED: a JSON null decodes into a struct
	// as nothing at all, with no error, and would pass for a record of no keys.
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return threadLine{}, &threadLineError{N: n, Err: errNotOneRecord}
	}
	var w threadWire
	if err := wireDecode(raw, &w); err != nil {
		return threadLine{}, &threadLineError{N: n, Err: err}
	}
	head, err := w.head()
	if err != nil {
		return threadLine{}, &threadLineError{N: n, Err: err}
	}
	return threadLine{
		N: n, Raw: raw, Head: head,
		message: w.Message, attachment: w.Attachment, origin: w.Origin,
		toolUseID: w.ToolUseID, toolUseResult: w.ToolUseResult,
	}, nil
}

// head holds the head's keys to their types: `type`, `subtype`, `uuid` and
// `agentId` strings, `parentUuid` null or a string, `isSidechain` a boolean,
// each where present, and a user record's `replacesSpan` a boolean where
// present.
// ANOTHER TYPE IS NO RECORD: a newer Claude Code that changes one fails toward
// no reading, which every reader takes for "not confirmed", never for a fact.
//
// `replacesSpan` IS HELD ON A USER RECORD ALONE, the one record the harness
// reads it on and the one the restart rule reads it on ([threadRestarts.See]).
// On any other record it says nothing to any reader, so whatever it holds
// there, null or a string among it, never costs a reader the line.
func (w threadWire) head() (threadHead, error) {
	var (
		h   threadHead
		err error
	)
	if h.Type, err = wireString("type", w.Type); err != nil {
		return threadHead{}, err
	}
	if h.Subtype, err = wireString("subtype", w.Subtype); err != nil {
		return threadHead{}, err
	}
	if h.AgentID, err = wireString("agentId", w.AgentID); err != nil {
		return threadHead{}, err
	}
	if h.UUID, err = wireString("uuid", w.UUID); err != nil {
		return threadHead{}, err
	}
	if w.ParentUUID != nil && !bytes.Equal(w.ParentUUID, jsonNull) && w.ParentUUID[0] != '"' {
		return threadHead{}, fmt.Errorf("parentUuid is %s, neither null nor a string", wireKind(w.ParentUUID))
	}
	h.Parent = w.ParentUUID
	if w.IsSidechain != nil {
		var side bool
		if side, err = wireBool("isSidechain", w.IsSidechain); err != nil {
			return threadHead{}, err
		}
		h.Sidechain = &side
	}
	if w.ReplacesSpan != nil && h.Type == recordUser {
		if h.ReplacesSpan, err = wireBool("replacesSpan", w.ReplacesSpan); err != nil {
			return threadHead{}, err
		}
	}
	return h, nil
}

// threadHead is what EVERY reader needs of a whole line, held to its type by
// the line's decode: what kind of record it is and where it sits, never what
// it says, and never an id of a model's call. Beside them sits the record's
// own uuid, which one reader reads: it says where the record sits too, as the
// other end of the parentUuid a record after it names it by, so a uuid that is
// no string is the line's error, as a parentUuid neither null nor a string is.
type threadHead struct {
	// Type and Subtype are the record's `type` and `subtype`; "" where absent.
	Type, Subtype string
	// UUID is the record's own `uuid`, the id a chained record after it names
	// as its parent: "" where the key is absent, as it is on every record
	// outside the chain (the harness's bookkeeping, at Parent), and where it
	// is the empty string, which the layer does not tell apart. HANDED OVER AS
	// WRITTEN, with no rule of its length or shape: every chained record
	// measured carries a 36-byte uuid (9,806 of 9,806, and none of the 6,003
	// records outside the chain carries the key), and its one reader holds it
	// to a rule of its own.
	//
	// ITS READER is the resumed count, which bounds what a continued
	// session's harness counts before its first request, to judge whether it
	// may compact there. It reads the uuid on a main compact_boundary record
	// alone, and compares the newest such boundary's with the one the
	// launch's basis recorded, to know whether the job appended a compaction
	// of its own: never by counting boundaries, since the harness's trim
	// after a compaction drops every boundary before the last
	// ([claudeLocalGCFloor]), so a thread that compacted again can hold no
	// more of them than it did. A uuid that is absent, empty or longer than
	// 36 bytes is no reading to it.
	UUID string
	// Parent is `parentUuid` AS WRITTEN: nil where the key is absent, the
	// literal null where the record roots a chain, else the quoted uuid.
	//
	// ABSENT IS NOT NULL. Every record outside the chain, the harness's own
	// bookkeeping, carries no `parentUuid` at all (queue-operation,
	// atis-latch, api-request-shape, api-request-blob, api-request,
	// last-prompt, cost-state, and tools-used, which 2.1.322 alone writes,
	// naming the tools the session has used whenever that set grows: 874
	// records of 61 main threads measured; and mode beside them), and only a
	// record that roots a chain carries null. Read as one, a thread's
	// bookkeeping before its brief would root chains of its own, the brief
	// would be no thread's first chained record, and every thread would read
	// as restarted ([threadRestarts]).
	Parent json.RawMessage
	// Sidechain is `isSidechain`; nil where absent.
	Sidechain *bool
	// AgentID is `agentId`, the subagent whose thread holds the record; ""
	// where absent and where it is the empty string, which the layer does not
	// tell apart: [threadHead.Main] reads both as naming no subagent, and a
	// reader that matches a subagent compares it with that subagent's id,
	// which [validClaudeAgent] never lets be empty.
	AgentID string
	// ReplacesSpan is a user record's `replacesSpan`: true where it says so
	// with a JSON true, false where it is absent or false. A user record that
	// says so is a restart ([threadRestarts.See]). False on every other
	// record, whatever the key holds there: it is read on a user record
	// alone ([threadWire.head]).
	ReplacesSpan bool
}

// Chained reports whether the record is in a chain: `parentUuid` present,
// null or not.
func (h threadHead) Chained() bool { return h.Parent != nil }

// Root reports whether the record roots a chain: `parentUuid` present and
// literally null.
func (h threadHead) Root() bool { return bytes.Equal(h.Parent, jsonNull) }

// Main reports whether the record is the main thread's own: `isSidechain`
// present and false, and no `agentId`. A record that says neither is no main
// thread's, whatever file holds it.
func (h threadHead) Main() bool { return h.Sidechain != nil && !*h.Sidechain && h.AgentID == "" }

// threadDecoding is the ONE set of options every decode this layer makes goes
// through ([wireDecode], [eachElement]), so a value reads as Claude Code's own
// JSON.parse reads it, whichever decode reads it. Each option is set for its
// own reason:
//
//   - EXACT KEY NAMES (encoding/json/v2's MatchCaseInsensitiveNames, off): a
//     key fills the field it names exactly, as JSON.parse holds `toolUseID`
//     and `toolUseId` apart. encoding/json's own decode takes a key that
//     differs from a field's name only in case for that field, the last of the
//     two written winning: decoded so, a line carrying `parentUuid` and then
//     `ParentUuid` would read as the second, and a hook's record carrying its
//     firing's `toolUseID` and then a `toolUseId` would hand the second out as
//     its firing, a spelling 2.1.322 already writes, where a subagent's
//     metadata names the call that started it.
//   - A KEY WRITTEN TWICE ALLOWED, AND READ BY ITS LAST VALUE (jsontext's
//     AllowDuplicateNames, on), as JSON.parse allows and reads it. Refusing
//     it, a decoder keeps every name of every object it reads, a cost a key,
//     and refuses a line the harness reads.
//   - INVALID UTF-8 AND A LONE SURROGATE READ AS THE REPLACEMENT CHARACTER,
//     U+FFFD (jsontext's AllowInvalidUTF8, on), as encoding/json's own decode
//     reads them, never refused. Claude Code writes a line with
//     JSON.stringify, which writes a surrogate that has no pair as a `\udXXX`
//     escape, and a text it cut inside a pair (an emoji at a truncation
//     point) holds one, which its JSON.parse reads back. Refused, one such
//     line would be no record, and would cost a reader its whole reading of a
//     thread the harness reads whole.
var threadDecoding = jsonv2.JoinOptions(
	jsonv2.MatchCaseInsensitiveNames(false),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// wireDecode decodes raw, one JSON value, into v under [threadDecoding]: the
// one decode every value this layer reads goes through, encoding/json/v2's,
// and every value the package's other readers of a harness's record read (a
// run's event stream, [claudeEventWire]; its result, [claudeResultWire]; a
// person's transcript, [claudeRecord] and [openCodeMessageWire]). An object's
// key fills the field it names exactly, a key written twice holds the last
// value written, and a string's invalid UTF-8 or lone surrogate reads as
// U+FFFD.
func wireDecode(raw []byte, v any) error {
	return jsonv2.Unmarshal(raw, v, threadDecoding)
}

// eachElement hands each element of list, raw JSON holding one list, to each,
// in order, as it reads it: the element's raw bytes, as written but for the
// whitespace around them, valid only while each runs, so each copies what it
// keeps. It NEVER COLLECTS THEM. It stops at the first error each returns,
// reads no element after it, and returns that error unchanged; a list that is
// no list, or that does not decode, is an error of its own.
//
// THE ONE WALK of a list a box wrote, under [threadDecoding], so what a list
// costs its reader is what each makes of one element at a time, never the
// whole list first. Decoded whole, a line at [threadLineMax] holding a list of
// four million `1`s, which a box can write into its own thread, would cost the
// engine host a header and a copy for every element, hundreds of MiB, before
// anything refused the first. Its readers: a message's content
// ([threadLine.Message]), a reply's applied context edits
// ([threadLine.ToolResultsCleared]), the lists a run's event stream and a
// person's transcript of a session decode (a message's blocks,
// [eachClaudeBlock]; a tool's answer, [toolAnswer]; an OpenCode step's items,
// [eachOpenCodeItem]; a run's result's errors, [claudeResult.errorsSaid]),
// and a call's result content, which the reader that tells a relaunch which
// of its calls a kept session holds (exact telling) reads no further than its
// second element.
func eachElement(list json.RawMessage, each func(element json.RawMessage) error) error {
	if trimmed := bytes.TrimLeft(list, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '[' {
		return fmt.Errorf("is %s, not a list", wireKind(trimmed))
	}
	dec := jsontext.NewDecoder(bytes.NewBuffer(list), threadDecoding)
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	for dec.PeekKind() != ']' {
		element, err := dec.ReadValue()
		if err != nil {
			return err
		}
		if err = each(element); err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return errors.New("a list is followed by more than whitespace")
	}
	return nil
}

// wireString is raw, a key named key, held to a string: "" where the key is
// absent.
func wireString(key string, raw json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	return wireRequiredString(key, raw)
}

// wireRequiredString is raw, a key named key, held to a string that is there.
func wireRequiredString(key string, raw json.RawMessage) (string, error) {
	// Checked by its first byte, never left to the decode: a null decodes into
	// a string as nothing at all, with no error.
	if len(raw) == 0 || raw[0] != '"' {
		return "", fmt.Errorf("%s is %s, not a string", key, wireKind(raw))
	}
	var s string
	if err := wireDecode(raw, &s); err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return s, nil
}

// wireBool is raw, a key named key, held to a boolean that is there: checked
// by its first byte, for [wireRequiredString]'s reason.
func wireBool(key string, raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || (raw[0] != 't' && raw[0] != 'f') {
		return false, fmt.Errorf("%s is %s, not a boolean", key, wireKind(raw))
	}
	var b bool
	if err := wireDecode(raw, &b); err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

// wireNumber is raw, a key named key, held to a number that is there: checked
// by its first byte, for [wireRequiredString]'s reason.
func wireNumber(key string, raw json.RawMessage) (float64, error) {
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, fmt.Errorf("%s is %s, not a number", key, wireKind(raw))
	}
	var n float64
	if err := wireDecode(raw, &n); err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// wireCount is raw, a key named key, held to a count: a whole number as JSON
// writes one, its digits alone with no sign, fraction or exponent, that fits
// an int64; 0 where the key is absent. Checked by its bytes before it is
// decoded, for [wireRequiredString]'s reason: a null decodes into an int64 as
// 0 with no error, and a sign as a negative count.
func wireCount(key string, raw json.RawMessage) (int64, error) {
	if raw == nil {
		return 0, nil
	}
	if len(raw) == 0 || bytes.ContainsFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, fmt.Errorf("%s is %s, not a count in digits alone", key, wireKind(raw))
	}
	var n int64
	if err := wireDecode(raw, &n); err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// wireKind says what raw is: "absent" where its key is not there, else the
// kind of JSON value it holds.
func wireKind(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "absent"
	}
	switch raw[0] {
	case '"':
		return "a string"
	case '{':
		return "an object"
	case '[':
		return "a list"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a number"
}

// wireObject reports whether raw is a JSON object that is there.
func wireObject(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] == '{' }

// OriginKind is the record's `origin.kind`: "" where either is absent, an error
// where `origin` is no object or its kind no string.
func (l threadLine) OriginKind() (string, error) {
	if l.origin == nil {
		return "", nil
	}
	if !wireObject(l.origin) {
		return "", fmt.Errorf("origin is %s, not an object", wireKind(l.origin))
	}
	var origin struct {
		Kind json.RawMessage `json:"kind"`
	}
	if err := wireDecode(l.origin, &origin); err != nil {
		return "", fmt.Errorf("origin: %w", err)
	}
	return wireString("origin.kind", origin.Kind)
}

// ToolUseResult is a user record's own `toolUseResult`, raw (an object, a list
// or a string, as the tool's result had it); nil where it is absent and on
// every other record, valid until the reader's next [threadReader.Next]
// ([threadLine]). Never an id: a call's id leaves this layer only through
// [threadMessage.Calls].
//
// IT IS THE RECORD'S, NEVER A CALL'S, handed out raw on purpose for its one
// reader to hold: a reader takes it for a call's only where the record's
// message holds that call's one tool_result and nothing else, as every record
// measured that carries one does.
func (l threadLine) ToolUseResult() json.RawMessage {
	if l.Head.Type != recordUser {
		return nil
	}
	return l.toolUseResult
}

// The record types and subtypes this layer reads by name.
const (
	recordUser       = "user"
	recordAssistant  = "assistant"
	recordAttachment = "attachment"
	recordSystem     = "system"

	// subtypeCompactBoundary is a compaction's boundary ([threadRestarts]).
	subtypeCompactBoundary = "compact_boundary"
	// subtypeMicrocompactBoundary is a microcompaction's boundary, which no
	// release measured writes ([threadRestarts.See]).
	subtypeMicrocompactBoundary = "microcompact_boundary"
	// hookStopSummary is the summary a Stop firing ends with: a system record
	// of this subtype, and the [threadHook.Record] it is read as.
	hookStopSummary = "stop_hook_summary"

	// editClearToolUses is the server's context edit that clears the content
	// of old tool results ([threadLine.ToolResultsCleared]).
	editClearToolUses = "clear_tool_uses_20250919"
)

// The hook attachments [threadLine.Hook] reads: what a hook's firing records.
const (
	hookSuccess           = "hook_success"
	hookSystemMessage     = "hook_system_message"
	hookAdditionalContext = "hook_additional_context"
	hookCancelled         = "hook_cancelled"
)

// threadHook is what a hook's record says of the firing it records.
type threadHook struct {
	// Record is the attachment's type, or [hookStopSummary].
	Record string
	// Event is the attachment's `hookEvent`. "" on a summary, which carries
	// none (34 of 34 measured): a summary's event is its firing's, which that
	// firing's attachments name under the same Firing.
	Event string
	// Firing is Claude Code's own id of ONE FIRING of a hook, which every
	// record of that firing shares: the attachment's `toolUseID`, or a
	// summary's top-level one. `hook-<uuid>` after a batch, a bare uuid at
	// Stop (328 of 328 attachments and 34 of 34 summaries measured).
	//
	// NEVER A CALL'S ID, although the key says tool use, and although a
	// PostToolUse hook's firing carries the id of the model's call it
	// followed (`toolu_…`, measured on a skill's hook): so a firing leaves
	// this layer here alone, and a call's id only through
	// [threadMessage.Calls], with no accessor of "a line's ids" that would
	// hand a reader of calls a hook's firing, or a reader of firings a call.
	Firing string
	// Command is the attachment's `command`, where it has one.
	Command string
	// Content is the attachment's `content`, raw: a string on a success and a
	// system message, a list on an additional context. Valid until the
	// reader's next Next ([threadLine]).
	Content json.RawMessage
	// Stdout is a success's `stdout`, raw: the hook's whole answer, as text.
	// Valid until the reader's next Next ([threadLine]).
	Stdout json.RawMessage
}

// Hook is what a HOOK's record says of the firing it records: the
// hook_success, hook_system_message, hook_additional_context and
// hook_cancelled attachments, and a system stop_hook_summary. False for every
// other record, whatever `toolUseID` it carries.
//
// An error for an attachment record whose `attachment` is no object or whose
// type is no string (it may be a hook's, and cannot be read as one), and for a
// hook's record whose fields are not of their type: an attachment's event and
// firing, and a summary's firing, strings that name something, an empty one
// refused (a summary names no event, and none is asked of it); an
// attachment's command a string where present; a success's stdout a string,
// and its content a string where present; a system message's content a
// string, and an additional context's a list. A firing whose record was
// reshaped is never read as one that recorded nothing.
//
// A TOP-LEVEL `toolUseID` IS READ ON A SUMMARY ALONE, the one record measured
// to carry its firing there: on any other, a later release could carry a
// call's id at the top, and a reader would be handed it under a hook's name.
func (l threadLine) Hook() (threadHook, bool, error) {
	switch {
	case l.Head.Type == recordAttachment:
		return attachmentHook(l.attachment)
	case l.Head.Type == recordSystem && l.Head.Subtype == hookStopSummary:
		firing, err := wireRequiredString("toolUseID", l.toolUseID)
		if err == nil && firing == "" {
			err = errors.New("toolUseID names no firing")
		}
		if err != nil {
			return threadHook{}, false, fmt.Errorf("a %s: %w", hookStopSummary, err)
		}
		return threadHook{Record: hookStopSummary, Firing: firing}, true, nil
	}
	return threadHook{}, false, nil
}

// attachmentHook is the hook an attachment records, raw the record's
// `attachment` ([threadLine.Hook]).
func attachmentHook(raw json.RawMessage) (threadHook, bool, error) {
	if !wireObject(raw) {
		return threadHook{}, false, fmt.Errorf("an attachment record's attachment is %s, not an object", wireKind(raw))
	}
	var a struct {
		Type      json.RawMessage `json:"type"`
		HookEvent json.RawMessage `json:"hookEvent"`
		ToolUseID json.RawMessage `json:"toolUseID"`
		Command   json.RawMessage `json:"command"`
		Content   json.RawMessage `json:"content"`
		Stdout    json.RawMessage `json:"stdout"`
	}
	if err := wireDecode(raw, &a); err != nil {
		return threadHook{}, false, fmt.Errorf("an attachment: %w", err)
	}
	record, err := wireRequiredString("attachment.type", a.Type)
	if err != nil {
		return threadHook{}, false, err
	}
	switch record {
	case hookSuccess, hookSystemMessage, hookAdditionalContext, hookCancelled:
	default:
		return threadHook{}, false, nil
	}
	h, err := hookFields(record, a.HookEvent, a.ToolUseID, a.Command)
	if err != nil {
		return threadHook{}, false, fmt.Errorf("a %s: %w", record, err)
	}
	h.Content, h.Stdout = a.Content, a.Stdout
	if err = hookShape(record, a.Content, a.Stdout); err != nil {
		return threadHook{}, false, fmt.Errorf("a %s: %w", record, err)
	}
	return h, true, nil
}

// hookFields is a hook attachment's firing: its event and its firing,
// strings that are there and say something, and its command, a string where
// present.
func hookFields(record string, event, firing, command json.RawMessage) (threadHook, error) {
	h := threadHook{Record: record}
	var err error
	if h.Event, err = wireRequiredString("hookEvent", event); err != nil {
		return threadHook{}, err
	}
	if h.Firing, err = wireRequiredString("toolUseID", firing); err != nil {
		return threadHook{}, err
	}
	switch {
	case h.Event == "":
		return threadHook{}, errors.New("hookEvent names no event")
	case h.Firing == "":
		return threadHook{}, errors.New("toolUseID names no firing")
	}
	if h.Command, err = wireString("command", command); err != nil {
		return threadHook{}, err
	}
	return h, nil
}

// hookShape holds a hook attachment's content and stdout to the types its
// record gives them ([threadLine.Hook]).
func hookShape(record string, content, stdout json.RawMessage) error {
	switch record {
	case hookSuccess:
		if _, err := wireRequiredString("stdout", stdout); err != nil {
			return err
		}
		_, err := wireString("content", content)
		return err
	case hookSystemMessage:
		_, err := wireRequiredString("content", content)
		return err
	case hookAdditionalContext:
		if len(content) == 0 || content[0] != '[' {
			return fmt.Errorf("content is %s, not a list", wireKind(content))
		}
	}
	return nil
}

// threadMessage is a user or assistant record's message as its one decode
// left it: its content as one string, or its blocks.
type threadMessage struct {
	text   string
	isText bool
	kinds  []string
	calls  []threadCall
}

// threadCall is one call block of a message: a model's call, or the result it
// was answered with.
type threadCall struct {
	// Kind is "tool_use" or "tool_result".
	Kind string
	// ID is the call's id: a tool_use's `id`, a tool_result's `tool_use_id`.
	ID string
	// Name is the tool a tool_use called.
	Name string
	// Input is a tool_use's `input`, raw, valid until the reader's next Next
	// ([threadLine]).
	Input json.RawMessage
	// IsError is a tool_result's `is_error`; nil where absent.
	IsError *bool
	// Content is a tool_result's `content`, raw: a string or a list of blocks.
	// Valid until the reader's next Next ([threadLine]).
	Content json.RawMessage
}

// The content blocks this layer names: the blocks that are calls, and those
// that carry a model's text and its thinking.
const (
	blockToolUse          = "tool_use"
	blockToolResult       = "tool_result"
	blockText             = "text"
	blockThinking         = "thinking"
	blockRedactedThinking = "redacted_thinking"
)

// knownKind is kind, a block's type, held as this layer's own constant where
// it is one of the blocks the layer names, so a list of such blocks holds no
// string of its own for a block's kind.
func knownKind(kind string) string {
	switch kind {
	case blockToolUse:
		return blockToolUse
	case blockToolResult:
		return blockToolResult
	case blockText:
		return blockText
	case blockThinking:
		return blockThinking
	case blockRedactedThinking:
		return blockRedactedThinking
	}
	return kind
}

// Message is a user or assistant record's `message`, decoded from the line's
// raw `message` on each call: nothing keeps it, so a reader that asks twice
// pays for two decodes. It is the zero message for every other record,
// whatever `message` it carries: an api-request-blob repeats what a request
// sent, and a call in it is no call the thread made.
//
// AN ERROR WHERE IT DOES NOT DECODE, and never a message with half a call in
// it: `message` an object; its `content` a string or a list of objects, each
// with a string `type`; a tool_use's `id` a string that names a call and its
// `name` a string; a tool_result's `tool_use_id` a string that names a call
// and its `is_error` a boolean where present. An absent or null content is no
// message either: every user and assistant record measured holds one.
//
// ITS ERROR IS ITS LINE'S. A reader that decides from a thread's messages asks
// Message of every user and assistant record of that thread, and takes an
// error for that line's, no record, as it takes a *threadLineError: never for
// a message with no calls, which would read a reshaped record that held a call,
// or a leg's start, as one that held none.
//
// A CONTENT LIST IS READ A BLOCK AT A TIME ([eachElement]), each block held to
// its type as it is read, and never decoded whole before its first block is
// checked. So a list refused at a block costs what the blocks up to it cost,
// however many follow, and what a list read to its end leaves held is a kind
// for each block and a call for each call block, each kept as it is accepted:
// while it reads, a message holds, beyond those, one copy of its content and
// one block's decode. A list of four million `1`s at [threadLineMax] is
// refused at its first block for the allocations a list of one costs and, in
// bytes, the one copy of its content, and a line at that bound of the
// smallest results a list can hold leaves their calls held, about three times
// the line (measured): a kind's header and a call's fields for each, at most
// a quarter over for append's growth.
func (l threadLine) Message() (threadMessage, error) {
	// record is what an error of the message calls the record.
	var call, record string
	switch l.Head.Type {
	case recordAssistant:
		call, record = blockToolUse, "an assistant record"
	case recordUser:
		call, record = blockToolResult, "a user record"
	default:
		return threadMessage{}, nil
	}
	if !wireObject(l.message) {
		return threadMessage{}, fmt.Errorf("%s's message is %s, not an object", record, wireKind(l.message))
	}
	var wire struct {
		Content json.RawMessage `json:"content"`
	}
	if err := wireDecode(l.message, &wire); err != nil {
		return threadMessage{}, fmt.Errorf("%s's message: %w", record, err)
	}
	m, err := messageContent(wire.Content, call)
	if err != nil {
		return threadMessage{}, fmt.Errorf("%s's message: %w", record, err)
	}
	return m, nil
}

// messageContent is a message whose `content` is content, its blocks of kind
// call its calls.
func messageContent(content json.RawMessage, call string) (threadMessage, error) {
	switch {
	case len(content) == 0 || (content[0] != '"' && content[0] != '['):
		return threadMessage{}, fmt.Errorf("content is %s, neither a string nor a list", wireKind(content))
	case content[0] == '"':
		var text string
		if err := wireDecode(content, &text); err != nil {
			return threadMessage{}, fmt.Errorf("content: %w", err)
		}
		return threadMessage{text: text, isText: true}, nil
	}
	// A BLOCK AT A TIME ([eachElement]), and the kinds and the calls grown
	// from what was accepted, never sized from the list's length, which only
	// a whole decode knows: a list refused at its first block costs that
	// block. An empty list's kinds are empty, never nil, which only a content
	// that is no list reads as.
	m := threadMessage{kinds: []string{}}
	if err := eachElement(content, func(raw json.RawMessage) error {
		b, err := contentBlock(raw)
		if err != nil {
			return fmt.Errorf("content block %d: %w", len(m.kinds)+1, err)
		}
		m.kinds = append(m.kinds, b.Kind)
		if b.Kind == call {
			m.calls = append(m.calls, b)
		}
		return nil
	}); err != nil {
		return threadMessage{}, err
	}
	return m, nil
}

// contentBlock is one block of a message's content, its kind and, for a call
// block, its call: held to the types [threadLine.Message] names.
func contentBlock(raw json.RawMessage) (threadCall, error) {
	if !wireObject(raw) {
		return threadCall{}, fmt.Errorf("is %s, not an object", wireKind(raw))
	}
	var b struct {
		Type      json.RawMessage `json:"type"`
		ID        json.RawMessage `json:"id"`
		Name      json.RawMessage `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID json.RawMessage `json:"tool_use_id"`
		IsError   json.RawMessage `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if err := wireDecode(raw, &b); err != nil {
		return threadCall{}, err
	}
	kind, err := wireRequiredString("type", b.Type)
	if err != nil {
		return threadCall{}, err
	}
	c := threadCall{Kind: knownKind(kind)}
	switch kind {
	case blockToolUse:
		if c.ID, err = callID("id", b.ID); err != nil {
			return threadCall{}, err
		}
		if c.Name, err = wireRequiredString("name", b.Name); err != nil {
			return threadCall{}, err
		}
		c.Input = b.Input
	case blockToolResult:
		if c.ID, err = callID("tool_use_id", b.ToolUseID); err != nil {
			return threadCall{}, err
		}
		if b.IsError != nil {
			var failed bool
			if failed, err = wireBool("is_error", b.IsError); err != nil {
				return threadCall{}, err
			}
			c.IsError = &failed
		}
		c.Content = b.Content
	}
	return c, nil
}

// callID is raw, a call block's key named key, held to a string that names a
// call. AN ID THAT NAMES NONE IS REFUSED, as a hook's record whose firing
// names none is ([threadLine.Hook]): the API names every call a model makes,
// no block measured carries an empty id (811 calls and 744 results, every
// thread the probes kept), and handed out, a call whose id names none is one
// no reader could match against a call it knows.
func callID(key string, raw json.RawMessage) (string, error) {
	id, err := wireRequiredString(key, raw)
	if err == nil && id == "" {
		err = fmt.Errorf("%s names no call", key)
	}
	return id, err
}

// Text is the message's content where it is one string.
func (m threadMessage) Text() (string, bool) { return m.text, m.isText }

// Kinds is each content block's type, in order. It is nil exactly where the
// message holds no list: a content that is one string ([threadMessage.Text]
// says so), and the zero message every record but a user's or an assistant's
// reads as; an empty list is an empty Kinds, never nil. A reader tells a
// turn's own message from the results of its calls by it: a user record whose
// blocks are tool_result alone, or not. A kind the layer does not name is
// handed out as written, for its reader to treat as unknown: never as a sign
// that a reply finished, nor as a block that holds no call.
func (m threadMessage) Kinds() []string { return m.kinds }

// Calls is the message's call blocks: an assistant's tool_use blocks, a user's
// tool_result blocks. THE ONLY WAY A CALL'S ID LEAVES THIS LAYER
// ([threadHook.Firing]).
func (m threadMessage) Calls() []threadCall { return m.calls }

// StopReason is why the model's reply an assistant record holds stopped, its
// message's `stop_reason`: "end_turn" where the model ended its turn,
// "tool_use" where it stopped for the calls it made, "stop_sequence", or
// whatever else the API names. "" where the key is absent, and for every
// record that is no assistant's.
//
// ITS READER is the one that tells a relaunch which of its calls a kept
// session holds (exact telling), to know whether a subagent's leg finished.
// 2.1.310 writes a model's reply as a record a block, so a reply that wrote
// text and then called a tool is two assistant records of one message id,
// the text's first: a thread cut between them ends on a record of text alone,
// which reads as a finished leg. Only the stop_reason tells the two apart:
// "tool_use" on every such text record measured, as on its call's, and
// "end_turn" or "stop_sequence" on every other record of text alone.
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as a key one reader reads is: a
// string where present. Any other type is this accessor's error, never "",
// which its reader would take for a reply that did not stop for its calls,
// and never Message's, so a reshaped stop_reason costs only the reader that
// asks for it.
func (l threadLine) StopReason() (string, error) {
	if l.Head.Type != recordAssistant {
		return "", nil
	}
	// HELD TO AN OBJECT BEFORE IT IS DECODED: a null decodes into a struct as
	// nothing at all, and would read as a reply that says nothing of its stop.
	if !wireObject(l.message) {
		return "", fmt.Errorf("an assistant record's message is %s, not an object", wireKind(l.message))
	}
	var message struct {
		StopReason json.RawMessage `json:"stop_reason"`
	}
	if err := wireDecode(l.message, &message); err != nil {
		return "", fmt.Errorf("an assistant record's message: %w", err)
	}
	return wireString("stop_reason", message.StopReason)
}

// threadUsage is what a model's reply says of the request it answered
// ([threadLine.Usage]): which reply it is, the model that wrote it, and what
// the request took, in tokens.
type threadUsage struct {
	// ID is the message's `id`, which every record of one reply carries, since
	// the harness writes a reply a record a block ([threadLine.StopReason]):
	// "" where the key is absent and where it is the empty string, which Usage
	// does not tell apart.
	ID string
	// Model is the message's `model`: the model that answered, or
	// "<synthetic>" on a reply the harness wrote itself, as a resume writes one
	// for a turn it found unanswered; "" where absent or empty.
	Model string
	// Input, CacheWrite, CacheRead and Output are the usage's `input_tokens`,
	// `cache_creation_input_tokens`, `cache_read_input_tokens` and
	// `output_tokens`, each as written: 0 where the usage does not name it, as
	// the harness reads a count it does not find, and all four 0 where the
	// record carries no usage.
	Input, CacheWrite, CacheRead, Output int64
}

// Usage is what a model's reply says of the request it answered: an assistant
// record's message's `id` and `model`, and its `usage`'s four counts. ok says
// whether the message carries a `usage`: where it carries none, ID and Model
// are handed over still, and every count is 0. Nothing, false and no error for
// every record that is no assistant's, whatever its `message` carries: no user
// record measured carries a usage, and an api-request-blob repeats what a
// request sent.
//
// HELD TO ITS TYPES HERE, AND HERE ALONE, as a key one reader reads is: `id`
// and `model` strings where present, `usage` an object where present, and
// each count, where present, a whole number as JSON writes one, its digits
// alone with no sign, fraction or exponent, that fits an int64 ([wireCount]).
// Anything else is this accessor's error and never Message's, so a reshaped
// usage costs only the reader that asks. A null usage, and a null count, are
// errors too, never no usage or 0, though Claude Code itself reads them so:
// no release measured writes either (every one of 1,891 replies measured
// carries a string id and model and a usage of four whole counts), and a
// figure refused costs its reader a reading, the direction in which it tells,
// where one read as 0 would anchor it on a count nobody measured.
//
// THE USAGE'S OWN FOUR COUNTS: its `iterations`, whose last entry Claude Code
// counts in their place where that entry is whole, are not read here, and
// which of the two a reader counts is its own to judge.
//
// ITS READERS. The resumed count reads it on every main-thread reply, to
// bound what a continued session's harness counts before its first request,
// as Claude Code counts it to judge whether to compact there: from the newest
// reply whose usage counts, its four counts' sum, OUTPUT among them, placed
// at the first record of that reply's ID, which Claude Code takes off a
// record that carries no usage too, so ID and Model come with every reply,
// usage or not. The one that tells a relaunch which of its calls a kept
// session holds (exact telling) reads ID alone, where it needs to know a
// reply's records as one. Which reply counts, and what "<synthetic>" or an
// empty ID means, are each reader's to judge, never this accessor's.
//
// Read off the line's one decode, its raw `message`, never a second read of
// the line.
func (l threadLine) Usage() (threadUsage, bool, error) {
	if l.Head.Type != recordAssistant {
		return threadUsage{}, false, nil
	}
	// HELD TO AN OBJECT BEFORE IT IS DECODED: a null decodes into a struct as
	// nothing at all, and would read as a reply that names no usage.
	if !wireObject(l.message) {
		return threadUsage{}, false, fmt.Errorf("an assistant record's message is %s, not an object", wireKind(l.message))
	}
	var message struct {
		ID    json.RawMessage `json:"id"`
		Model json.RawMessage `json:"model"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := wireDecode(l.message, &message); err != nil {
		return threadUsage{}, false, fmt.Errorf("an assistant record's message: %w", err)
	}
	var (
		u   threadUsage
		err error
	)
	if u.ID, err = wireString("id", message.ID); err != nil {
		return threadUsage{}, false, err
	}
	if u.Model, err = wireString("model", message.Model); err != nil {
		return threadUsage{}, false, err
	}
	if message.Usage == nil {
		return u, false, nil
	}
	if !wireObject(message.Usage) {
		return threadUsage{}, false, fmt.Errorf("usage is %s, not an object", wireKind(message.Usage))
	}
	var counts struct {
		Input      json.RawMessage `json:"input_tokens"`
		CacheWrite json.RawMessage `json:"cache_creation_input_tokens"`
		CacheRead  json.RawMessage `json:"cache_read_input_tokens"`
		Output     json.RawMessage `json:"output_tokens"`
	}
	if err = wireDecode(message.Usage, &counts); err != nil {
		return threadUsage{}, false, fmt.Errorf("usage: %w", err)
	}
	for _, c := range [...]struct {
		key string
		raw json.RawMessage
		to  *int64
	}{
		{"usage.input_tokens", counts.Input, &u.Input},
		{"usage.cache_creation_input_tokens", counts.CacheWrite, &u.CacheWrite},
		{"usage.cache_read_input_tokens", counts.CacheRead, &u.CacheRead},
		{"usage.output_tokens", counts.Output, &u.Output},
	} {
		if *c.to, err = wireCount(c.key, c.raw); err != nil {
			return threadUsage{}, false, err
		}
	}
	return u, true, nil
}

// ToolResultsCleared reports whether the record says the server CLEARED THE
// CONTENT OF OLD TOOL RESULTS from what the model was shown: an assistant
// record whose `message.context_management.applied_edits` holds an edit of
// type clear_tool_uses_20250919 whose `cleared_tool_uses` is above zero, the
// test Claude Code itself makes of its records as it plans its next request.
// False for every other record, whatever its `message` carries, and for an
// assistant record that reports no such edit: one with no
// `context_management` (every assistant record measured), one whose
// `context_management` is null (the reply a resume writes for a turn it found
// unanswered), one with no `applied_edits`, and one whose edits are others.
// Which thread's record counts is its reader's to judge, by the head.
//
// AN ERROR, never a report read as none, where the record's `message` is no
// object, its `context_management` neither an object nor null, its
// `applied_edits` no list, an edit no object or its type no string, or that
// edit's count no number: every edit is held so, wherever it falls in the
// list.
//
// THE EDIT. Claude Code asks the server for it on a request that follows an
// idle return (a prompt more than 3,900 s after the conversation's last
// message) in a session holding at least 30 clearable tool uses, once a flag
// Anthropic's servers serve turns it on, as they may for an engine's box on a
// first-party login; and a resume asks for it again. The server then clears
// the content of the results of the clearable tools (Read, the shell tools,
// Grep, Glob, WebSearch, WebFetch, Edit and Write, and those names served by
// an MCP server named `container`) older than the last ones it is asked to
// keep, twelve or more, and of no other tool. The thread keeps every result
// whole: this report alone says the model was shown less.
//
// COARSE: it counts what was cleared and names none of it, and the server
// keeps the last results and those of every tool it excludes, so a result
// before such a record may still have been shown. Read as cleared, it errs
// toward telling a note again.
//
// ITS READERS. The one that confirms a hook's note reached the model (the
// receipt fix's M1) reads it for a BATCH's note alone: on claude-sonnet-4-5 a
// batch's note rides its batch's last tool result into the request
// (measured), so it is cleared with that result, where a Stop's note is a
// user turn of its own, which the edit never clears. The one that tells a
// relaunch which of its calls a kept session holds (exact telling) reads it
// only to decline a reading, never for a call: in a cleared state Claude Code
// counts the cleared tokens back into the count it compacts by, which no line
// of the thread states.
//
// Read off the line's one decode, its raw `message`, never a second read of
// the line.
func (l threadLine) ToolResultsCleared() (bool, error) {
	if l.Head.Type != recordAssistant {
		return false, nil
	}
	// HELD TO AN OBJECT BEFORE IT IS DECODED: a null decodes into a struct as
	// nothing at all, and would read as a message that reports no edit.
	if !wireObject(l.message) {
		return false, fmt.Errorf("an assistant record's message is %s, not an object", wireKind(l.message))
	}
	var message struct {
		ContextManagement json.RawMessage `json:"context_management"`
	}
	if err := wireDecode(l.message, &message); err != nil {
		return false, fmt.Errorf("an assistant record's message: %w", err)
	}
	report := message.ContextManagement
	switch {
	case report == nil || bytes.Equal(report, jsonNull):
		return false, nil
	case !wireObject(report):
		return false, fmt.Errorf("context_management is %s, neither an object nor null", wireKind(report))
	}
	var applied struct {
		Edits json.RawMessage `json:"applied_edits"`
	}
	if err := wireDecode(report, &applied); err != nil {
		return false, fmt.Errorf("context_management: %w", err)
	}
	switch {
	case applied.Edits == nil:
		return false, nil
	case len(applied.Edits) == 0 || applied.Edits[0] != '[':
		return false, fmt.Errorf("context_management.applied_edits is %s, not a list", wireKind(applied.Edits))
	}
	// AN EDIT AT A TIME ([eachElement]), as a message's content is read: a list
	// of edits refused at its first costs that edit, however many follow.
	cleared, edits := false, 0
	if err := eachElement(applied.Edits, func(raw json.RawMessage) error {
		edits++
		uses, err := clearedToolUses(raw)
		if err != nil {
			return fmt.Errorf("context_management.applied_edits entry %d: %w", edits, err)
		}
		cleared = cleared || uses > 0
		return nil
	}); err != nil {
		return false, err
	}
	return cleared, nil
}

// clearedToolUses is how many tool uses one applied edit, raw, says the server
// cleared: the clear_tool_uses edit's count, and none for every other edit,
// whose count is never read.
func clearedToolUses(raw json.RawMessage) (float64, error) {
	if !wireObject(raw) {
		return 0, fmt.Errorf("is %s, not an object", wireKind(raw))
	}
	var edit struct {
		Type            json.RawMessage `json:"type"`
		ClearedToolUses json.RawMessage `json:"cleared_tool_uses"`
	}
	if err := wireDecode(raw, &edit); err != nil {
		return 0, err
	}
	kind, err := wireRequiredString("type", edit.Type)
	if err != nil || kind != editClearToolUses {
		return 0, err
	}
	return wireNumber("cleared_tool_uses", edit.ClearedToolUses)
}

// threadRestarts follows one thread's heads in order and says where its chain
// RESTARTS: where what a resumed session is handed starts over, so what lies
// before may be gone from it (a compaction summarised it, a trim dropped it,
// the file lost it).
type threadRestarts struct {
	// chained is whether the thread's first chained record has been seen.
	chained bool
}

// See reports whether the record with head h, the thread's next, is a
// RESTART: a record of subtype compact_boundary; a system record of subtype
// microcompact_boundary; a user record whose `replacesSpan` is true; one
// rooting a chain (`parentUuid` literally null) that is not the thread's
// first chained record; or the thread's first chained record where it roots
// no chain.
//
// THE FIRST CHAINED RECORD, never the first record or the first root: a
// thread's bookkeeping before its brief is no chain's, and its brief roots the
// chain the thread began with, which is no restart. A first chained record
// that roots no chain is the start of a thread whose head is gone: a trim
// after a compaction keeps a reply chained to a record it dropped, and a file
// can lose its start. What a resume holds of such a thread begins there, and
// whatever lay before it, a pair or a call among it, is in no file left to
// read, so it is a restart, and so is every root after it. Read as whole, a
// thread that lost its start and never rooted again would restart nowhere,
// and its reader would take what the lost head held for what never happened.
//
// A MICROCOMPACTION'S BOUNDARY AND A SPAN REPLACED ARE RESTARTS THOUGH NO
// RELEASE WRITES ONE. Neither 2.1.310 nor 2.1.322 has code that writes
// either record, yet each release's own readers stop at both as at a
// compaction: at a system record of either boundary's subtype, and at a user
// record whose replacesSpan is true as at a compaction's summary. A later
// release that writes one hands a resumed session less than lies before it,
// where a reader that read on would count what lies before as held; counted
// a restart, what lies before is told again, the safe direction for every
// reader. Each is read on its own record's type alone, as the harness reads
// it: the boundary's subtype here, and replacesSpan by the head, which holds
// it on a user record alone ([threadHead.ReplacesSpan]).
//
// The keep-recent clearing, which puts "[Old tool result content cleared]"
// in place of an old tool result, needs no rule: it never runs under
// `claude -p`, it writes nothing to the thread, and it never clears a batch's
// note, which is an attachment record of its own. Nor is the server's
// clearing of old tool results a restart: it clears the results of some tools
// alone, never a Stop's note, so each reader it concerns asks
// [threadLine.ToolResultsCleared] of the records it reads.
//
// A thread with no chained record restarts nowhere: it holds no chain to
// start over, and what that means is its reader's to say.
func (r *threadRestarts) See(h threadHead) bool {
	first := h.Chained() && !r.chained
	r.chained = r.chained || h.Chained()
	switch {
	case h.Subtype == subtypeCompactBoundary:
		return true
	case h.Type == recordSystem && h.Subtype == subtypeMicrocompactBoundary:
		return true
	case h.ReplacesSpan:
		return true
	case first:
		return !h.Root()
	}
	return h.Root()
}
