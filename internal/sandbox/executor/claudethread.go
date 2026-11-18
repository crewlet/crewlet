package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/steer"
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
// a hook's note reached the model, the one that tells a relaunch which of its
// calls a kept session holds, and the one that bounds what a continued
// session's harness counts before its first request, at a take's context
// meter and in the read that collects a job, [resumedReading]) reads a Claude
// Code thread through this one layer, so the rules it is read by are held
// once, each in one walk of a thread's whole lines that every reading of it
// shares ([walkThread]). The launch floor's figure at the meter
// ([contextReading]), which decides nothing of what the session recorded,
// reads a thread's replies through this layer too, so every reading of a
// thread reads a reply's usage by one rule ([threadLine.Usage]). The rules:
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
//     box wrote costs its reader one element at a time; and AN OBJECT WHOSE
//     KEYS ARE OPEN, as a map's are, A KEY AT A TIME ([eachKey]), never
//     decoded into a map of every key first.
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
//     but a call's, through [threadMessage.Calls], and an attachment's
//     `toolUseID` leaves it only as a key ([threadLine.AttachmentToolUseID]),
//     since a PostToolUse hook's firing carries the id of the call it followed.
//     An id written inside a record's text never leaves it as bytes: of a
//     call's input, a result's text, a record's own toolUseResult, a message's
//     text and a hook's words the layer hands out only the facts it reads of
//     them ([threadCall], [threadLine.HandsBack], [threadLine.AgentAnswer],
//     [threadMessage.NamesHandback], and the asked take tokens a hook's words
//     name, [threadLine.HookTakes]). A task notification's `<tool-use-id>`
//     leaves it as the leg its text claims ([threadMessage.Notification]),
//     which its one reader matches only against a call it read.
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
// time, each line decoded into its head and its facts ([threadReader.Next]).
// One goroutine reads it. A THREAD'S MEMBER: a subagent's metadata is one JSON
// document with no line break ([claudeSubagentMeta]), which it reads as nothing
// but a torn piece. ONE READER READS A SESSION'S MEMBERS, one after another
// ([threadReader.Reset]): a reading of a session's threads keeps one reader's
// buffers whatever members it reads, never one a member.
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
	// asked is the take tokens each line names of a hook's words
	// ([threadReader.Asking]).
	asked []string
}

// newThreadReader reads r, one member, a whole line at a time: at most maxBytes
// of it, the caller's figure ([threadPlainMax] for a whole thread, what is left
// of an archive's budget for one member of it); at or below zero, no byte. The
// member is a thread, never a subagent's metadata ([claudeSubagentMeta]).
func newThreadReader(r io.Reader, maxBytes int64) *threadReader {
	maxBytes = max(maxBytes, 0)
	return &threadReader{r: bufio.NewReaderSize(threadSource{r}, lineChunkBytes), maxBytes: maxBytes, left: maxBytes}
}

// Reset makes t read r, one member, within maxBytes, as [newThreadReader]
// does: whatever t read before, how it ended among it (a torn piece, a bound,
// an error, a context that ended), is gone, and t reads r exactly as a fresh
// reader would, keeping only its buffer.
func (t *threadReader) Reset(r io.Reader, maxBytes int64) {
	maxBytes = max(maxBytes, 0)
	t.r.Reset(threadSource{r})
	t.maxBytes, t.left = maxBytes, maxBytes
	t.n, t.torn, t.end, t.asked = 0, false, nil, nil
}

// maxAskedTakes is the most take tokens one reading asks
// ([threadReader.Asking]): a hook record's masks' width, 64 bits.
const maxAskedTakes = 64

// The most takes a job's row asks ([sandbox.MainThreadAsk.Takes]): every
// serve of each note and item it holds. A bound raised past the masks fails
// this build, never a job's read.
var _ [maxAskedTakes - (steer.MaxTurnNotes+sandbox.MaxJobItems)*sandbox.MaxNoteServes]struct{}

// errAskedPastMasks is an ask of more take tokens than a hook record's masks
// hold ([threadReader.Asking]).
var errAskedPastMasks = errors.New("executor: more take tokens are asked than a hook record's masks hold")

// Asking makes every line t reads from here on name which of tokens, take
// tokens ([claudeTakeToken]), a hook's words hold ([threadLine.HookTakes]):
// bit i for tokens[i]. Called before the first [threadReader.Next];
// [threadReader.Reset] clears it. Past [maxAskedTakes] tokens it asks nothing
// and answers [errAskedPastMasks]: the ask comes off a row, which a peer with
// wider bounds may have written, so its caller decides nothing of them.
func (t *threadReader) Asking(tokens []string) (err error) {
	t.asked, err = askedTokens(tokens)
	return err
}

// askedTokens is tokens as a reader keeps them asked, a copy, or none and
// [errAskedPastMasks] past [maxAskedTakes].
func askedTokens(tokens []string) ([]string, error) {
	if len(tokens) > maxAskedTakes {
		return nil, fmt.Errorf("%w: %d asked, %d held", errAskedPastMasks, len(tokens), maxAskedTakes)
	}
	return slices.Clone(tokens), nil
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
// dropped and the line decoded into its head and its facts
// ([decodeThreadLine]). It answers:
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
//   - ctx's error once ctx has ended, asked before each line is read: the
//     reading is over, and stays over, whatever context a later Next is
//     handed. A line's read itself asks no context, so a reading overruns its
//     caller's wait by one line's read and decode at most. ctx is the one
//     place a reading's caller is heard: its callers ask none of their own
//     between lines.
func (t *threadReader) Next(ctx context.Context) (threadLine, error) {
	if t.end != nil {
		return threadLine{}, t.end
	}
	if err := ctx.Err(); err != nil {
		t.end = err
		return threadLine{}, err
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
	return decodeThreadLine(t.n, raw[:len(raw)-1], t.asked)
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
	// Fault is how Claude Code's load reads the line ([lineFault]).
	Fault lineFault
	// Type is the kind of record the line holds where it is one object whose
	// head the decode refused (faultHead) and its `type` is a kind Claude
	// Code's load reads ([recordKind]); its zero value otherwise, so no
	// string the box wrote is held.
	Type recordKind
	// Size is how many bytes of the member the line takes, its break
	// included, so a reader that sums them with its records' sums the bytes
	// the load reads.
	Size int
	// Bytes is what a reader of its bytes reads of the line, its break dropped
	// ([threadBytes]), as a record's line carries it ([threadLine.Bytes]): what
	// a reader judges a line by where it reads lines by their bytes as Claude
	// Code's large-file readers do, whether the load reads a record off the
	// line or not ([threadChain.Unread]). Error does not say it.
	Bytes threadBytes
}

// lineFault is how Claude Code's load reads a whole line this layer reads as
// no record ([threadLineError]). Its load parses a thread a chunk at a time
// (2.1.322's $1t through Bun.JSONL.parseChunk, 2.1.310's $Pt the same):
// where a value fails, it keeps every value it read before the failure and
// resumes after the first line break at or after it. So a line whose JSON
// fails before any whole value yields the load nothing, as this layer reads
// it, and every other kind of line the layer refuses can yield the load more
// than nothing. Its zero value is no fault: a reader takes a line whose
// fault is not [lineFault.Valid] for one whose load no reader can say.
type lineFault string

const (
	// faultSyntax is a line that yields the load no value: its JSON fails at
	// a byte inside it before any whole value, or it holds white space alone.
	faultSyntax lineFault = "syntax"
	// faultNotObject is one whole value that is no object: the load reads it
	// and passes it over at its test of a record (2.1.322's iVe, 2.1.310's
	// oMt).
	faultNotObject lineFault = "not_object"
	// faultHead is one object whose head key is of the wrong type
	// ([headOf]): the load reads it, and keeps it under its own
	// reading of that key where it is a kind the load reads
	// ([threadLineError.Type]).
	faultHead lineFault = "head"
	// faultPrefix is a line that holds the beginning of a value its break
	// cuts: the load reads on past the break into the next line, as JSON
	// allows a break inside a value.
	faultPrefix lineFault = "prefix"
	// faultTrailing is one whole value followed by more than white space: the
	// load keeps the value, and fails at what follows it.
	faultTrailing lineFault = "trailing"
	// faultLead is a line that begins with a NUL or a UTF-8 byte order mark,
	// which the load's large-file reader takes off a line where its small one
	// does not.
	faultLead lineFault = "lead"
	// faultDepth is a value nested deeper than the layer's decode reads
	// (10,000 levels, encoding/json/jsontext's own bound): the load may read
	// it whole, with no such bound of the layer's.
	faultDepth lineFault = "depth"
)

// Valid reports whether f is one of the faults a refused line is read as.
func (f lineFault) Valid() bool {
	switch f {
	case faultSyntax, faultNotObject, faultHead, faultPrefix, faultTrailing, faultLead, faultDepth:
		return true
	}
	return false
}

// recordKind is the type of a record Claude Code's load reads, one of eight:
// the four its resume keeps in the map it walks (user, assistant, attachment
// and system), the checkpoint of the leaf it starts from (last-prompt), the
// progress record it bridges past (progress), a subagent's reference to the
// thread it forked from (fork-context-ref), and the record of the tool
// results it hands replaced (content-replacement), whose list 2.1.310's load
// spreads into the session's, failing the resume where that is no list
// ([threadLine.ReplacementCount]). Its zero value is none of them: a line of
// another type, or one whose type is no string, which the load passes over or
// files where no reader of a thread looks.
type recordKind string

// recordKinds is the eight kinds, as the layer's own strings ([kindOf]).
var recordKinds = [...]recordKind{recordUser, recordAssistant, recordAttachment, recordSystem, recordLastPrompt,
	recordProgress, recordForkContextRef, recordContentReplacement}

// Valid reports whether k is one of the eight kinds.
func (k recordKind) Valid() bool { return slices.Contains(recordKinds[:], k) }

// Error says which line is no record, and why.
func (e *threadLineError) Error() string {
	return fmt.Sprintf("executor: line %d of a Claude Code thread is no record: %v", e.N, e.Err)
}

// Unwrap is what the line's decode met.
func (e *threadLineError) Unwrap() error { return e.Err }

// threadLine is one whole line of a thread: its place, its size, what a
// reader of its bytes reads of them ([threadBytes]) and its head, beside the
// facts the accessors below read ([lineFacts]), all from the line's one
// decode ([factsOf]).
//
// IT NEVER HANDS OUT THE LINE'S BYTES: every reader reads a line by its head,
// its facts and its byte facts, so none holds what the reader's buffer held.
// Its head and its facts are its own, shared with no other line
// ([lineFacts]), and its byte facts are a value of a fixed size, as a refused
// line's are ([threadLineError]), so a reader keeps a line as long as it
// likes, and keeping one keeps no line's bytes.
//
// A VALUE AS LONG AS A LINE IT READS FOR ITS READERS, never hands out: what a
// call's input, a result's text, a record's own toolUseResult and a message's
// text are read for (the subagent a SendMessage sends to, a result's length
// and digests, a handback and what an answer says of a subagent's leg, the
// subagent and the leg a task notification names) the layer reads itself, by
// its readers' own rules, a shape a rule does not read answered by that rule
// rather than with an error, so a release that reshapes one costs that reader
// alone, as a key one reader reads does, and what it hands out is facts of a
// bounded size ([threadCall], [threadLine.HandsBack],
// [threadLine.AgentAnswer], [threadMessage.Notification]).
//
// A KEY ONLY ONE READER READS IS HELD TO ITS TYPE IN THAT READER'S ACCESSOR,
// never in the head: a release that reshapes it is that accessor's error and
// costs the reader that asks, so a reshaped attachment never blinds a reader
// of calls, a reshaped origin never blinds a reader of attachments, and a
// reshaped uuid blinds neither. A key a reader comes to need is added beside
// these, a fact of the same decode, with its own accessor and its own error.
type threadLine struct {
	// N is the line's place among the member's whole lines, from 1.
	N int
	// Size is how many bytes of the member the line takes, its break
	// included, as a refused line's Size is ([threadLineError]), so a reader
	// that sums them sums the bytes the load reads.
	Size int
	// Bytes is what a reader of its bytes reads of the line, its break
	// dropped ([threadBytes]): what Claude Code's large-file readers read a
	// line by ([threadChain], THE LARGE FILE), and the marks the resumed count
	// reads ([resumedReading]).
	Bytes threadBytes
	// Head is what every reader needs of the line.
	Head threadHead

	// f is what the accessors read ([lineFacts]); nil reads as a line of no
	// keys ([threadLine.facts]).
	f *lineFacts
}

// headOf holds the head's keys, k, to their types: `type`, `subtype` and
// `agentId` strings, `parentUuid` null or a string, `isSidechain` a boolean,
// each where present, and a user record's `replacesSpan` a boolean where
// present. THE ONE RULE OF A HEAD, whichever reading of the line hands it the
// keys.
// ANOTHER TYPE IS NO RECORD: a newer Claude Code that changes one fails toward
// no reading, which every reader takes for "not confirmed", never for a fact.
//
// A KEY'S KIND ALONE DECIDES EVERY ERROR IT GIVES ([headKey]), never its raw
// value: a reading that keeps a key's kind but not its value, one too long to
// keep, still refuses the line's head exactly as a reading that kept it does.
// A boolean's value is its kind too.
//
// `replacesSpan` IS HELD ON A USER RECORD ALONE, the one record the harness
// reads it on and the one the restart rule reads it on ([threadRestarts.See]).
// On any other record it says nothing to any reader, so whatever it holds
// there, null or a string among it, never costs a reader the line.
func headOf(k headKeys) (threadHead, error) {
	var (
		h   threadHead
		err error
	)
	if h.Type, err = k.typ.text("type"); err != nil {
		return threadHead{}, err
	}
	if h.Subtype, err = k.subtype.text("subtype"); err != nil {
		return threadHead{}, err
	}
	if h.AgentID, err = k.agentID.text("agentId"); err != nil {
		return threadHead{}, err
	}
	switch k.parent.kind {
	case 0, '"':
		h.Parent = k.parent.raw
	case 'n':
		h.Parent = jsonNull
	default:
		return threadHead{}, fmt.Errorf("parentUuid is %s, neither null nor a string", k.parent.kind)
	}
	if k.sidechain.kind != 0 {
		var side bool
		if side, err = k.sidechain.boolean("isSidechain"); err != nil {
			return threadHead{}, err
		}
		h.Sidechain = &side
	}
	if k.replacesSpan.kind != 0 && h.Type == recordUser {
		if h.ReplacesSpan, err = k.replacesSpan.boolean("replacesSpan"); err != nil {
			return threadHead{}, err
		}
	}
	return h, nil
}

// headKeys is the head's keys as a line writes them ([headOf]).
type headKeys struct {
	typ, subtype, agentID, parent, sidechain, replacesSpan headKey
}

// headKey is one key of a line's head as a reading of the line hands it to
// [headOf]: kind, its value's kind, 0 where the key is absent; and raw, its
// value as written, where the reading kept it, nil where it kept the kind
// alone. The head's rule refuses a key by its kind, and reads its raw only
// for the value it hands out, so a head built of a kind alone hands out no
// value for that key: the reading that kept no value hands that head to no
// reader.
type headKey struct {
	kind valueKind
	raw  json.RawMessage
}

// headKeyOf is raw, a head key's value as written, nil where the key is
// absent, as [headOf] reads it: its kind, and itself.
func headKeyOf(raw json.RawMessage) headKey { return headKey{kind: kindOfValue(raw), raw: raw} }

// text is k, a key named key, held to a string: "" where it is absent, and
// where its value was not kept.
func (k headKey) text(key string) (string, error) {
	switch {
	case k.kind == 0:
		return "", nil
	case k.kind != '"':
		return "", fmt.Errorf("%s is %s, not a string", key, k.kind)
	case k.raw == nil:
		return "", nil
	}
	return wireRequiredString(key, k.raw)
}

// boolean is k, a key named key that is there, held to a boolean: its value
// is its kind, 't' or 'f'.
func (k headKey) boolean(key string) (bool, error) {
	if k.kind != 't' && k.kind != 'f' {
		return false, fmt.Errorf("%s is %s, not a boolean", key, k.kind)
	}
	return k.kind == 't', nil
}

// threadHead is what EVERY reader needs of a whole line, held to its type by
// the line's decode: what kind of record it is and where it sits, never what
// it says, and never an id of a model's call. The record's own uuid is no part
// of it: one reader reads it, through [threadLine.UUID].
type threadHead struct {
	// Type and Subtype are the record's `type` and `subtype`; "" where absent.
	Type, Subtype string
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
	// alone ([headOf]).
	ReplacesSpan bool
}

// Chained reports whether the record is in a chain AS CLAUDE CODE'S RESUME
// READS ONE: a user, assistant, attachment or system record, whatever
// `parentUuid` it carries or lacks, or a record of any other type that
// carries one, null or not. Its resume loads every record of those four types
// into the one map it walks, whatever its `parentUuid` (2.1.310's and
// 2.1.322's test of a record is its type alone), and a parentless one of them
// roots a chain ([threadRestarts.See]); a record of another type is the
// harness's bookkeeping, which carries no `parentUuid` (every kind measured,
// at [threadHead.Parent]), and one that carries one is read as written.
//
// THE ONE PREDICATE every reader asks, so none of them parts from another:
// the restart rule ([threadRestarts.See]), the writer floor a reading holds
// each record to ([writerBelowFloor]) and the resumed count's charge
// ([resumedReading.line]). Read by `parentUuid` alone, a parentless record of
// those four types is read and walked by the resume, and by the reading, and
// no floor held it or count charged it. No release measured writes one (none
// of the 11,727 such records of the 521 thread files the probes kept lacks a
// `parentUuid`), so nothing measured moves.
func (h threadHead) Chained() bool {
	switch h.Type {
	case recordUser, recordAssistant, recordAttachment, recordSystem:
		return true
	}
	return h.Parent != nil
}

// Parentless reports whether the record names no parent AS CLAUDE CODE'S
// RESUME READS ONE: its `parentUuid` absent, null or the empty string, each
// falsy to JavaScript, where every walk the resume makes up a thread's chain
// stops, as at a root (2.1.310's w$t and 2.1.322's A7t, `if(!H)break`, and the
// walk to a turn record behind 2.1.322's leaf set, Cjt, alike). Any other
// `parentUuid` is a string that names the record's parent, whichever record
// that is.
//
// IT SAYS NOTHING OF WHETHER THE RECORD IS IN A CHAIN: the harness's
// bookkeeping carries no `parentUuid` and is parentless, and is in no chain
// ([threadHead.Chained], [threadRestarts.See]). Its readers: the restart rule
// ([threadRestarts.See]), and the model of which records Claude Code's resume
// hands its model, which reads a parent by this one rule, so the two never
// part.
func (h threadHead) Parentless() bool {
	return h.Parent == nil || bytes.Equal(h.Parent, jsonNull) || string(h.Parent) == `""`
}

// Main reports whether the record is the main thread's own: `isSidechain`
// present and false, and no `agentId`. A record that says neither is no main
// thread's, whatever file holds it.
func (h threadHead) Main() bool { return h.Sidechain != nil && !*h.Sidechain && h.AgentID == "" }

// threadDecoding is the ONE set of options every decode this layer makes goes
// through ([wireDecode], [eachElement], [eachKey]), so a value reads as Claude
// Code's own JSON.parse reads it, whichever decode reads it. Each option is
// set for its own reason:
//
//   - EXACT KEY NAMES (encoding/json/v2's MatchCaseInsensitiveNames, off): a
//     key fills the field it names exactly, as JSON.parse holds `toolUseID`
//     and `toolUseId` apart. encoding/json's own decode takes a key that
//     differs from a field's name only in case for that field, the last of the
//     two written winning: decoded so, a line carrying `parentUuid` and then
//     `ParentUuid` would read as the second, and an attachment carrying a
//     hook's firing as its `toolUseID` and then a `toolUseId` would key the
//     second as the call it followed ([threadLine.AttachmentToolUseID]), a
//     spelling 2.1.322 already writes, where a subagent's metadata names the
//     call that started it.
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
// person's transcript, [claudeRecord] and [openCodeMessageWire]; what an
// OpenCode session's steps measured, [openCodeMeasuredWire]). An object's
// key fills the field it names exactly, a key written twice holds the last
// value written, and a string's invalid UTF-8 or lone surrogate reads as
// U+FFFD.
func wireDecode(raw []byte, v any) error {
	return jsonv2.Unmarshal(raw, v, threadDecoding)
}

// eachElement hands each element of list, raw JSON holding one list, to each,
// in order, as it reads it: the element's raw bytes, as written but for the
// whitespace around them, valid only while each runs, so each copies what it
// keeps, and where in list they sit, the offset of their first byte. It NEVER
// COLLECTS THEM. It stops at the first error each returns, reads no element
// after it, and returns that error unchanged; a list that is no list, or that
// does not decode, is an error of its own.
//
// THE ONE WALK of a list a box wrote, under [threadDecoding], so what a list
// costs its reader is what each makes of one element at a time, never the
// whole list first. Decoded whole, a line at [threadLineMax] holding a list of
// four million `1`s, which a box can write into its own thread, would cost the
// engine host a header and a copy for every element, hundreds of MiB, before
// anything refused the first. Its readers: a message's content
// ([threadLine.Message]), a reply's applied context edits
// ([threadLine.ToolResultsCleared]) and its usage's iterations ([readUsage], a
// thread's reply's and a run's event stream's), the results a
// content-replacement record replaces ([threadLine.ReplacedResults]) and how
// many entries it lists ([threadLine.ReplacementCount]), the calls a batch's
// reply was split into ([threadLine.BatchToolUses]), the records a
// compaction's boundary says it kept ([threadLine.Preserved]) and those a
// refused or malformed reply's retry takes back ([threadLine.Retracted]), the
// lists an attachment's payload holds ([threadLine.AttachmentWellFormed]), the
// lists a run's event stream and a person's transcript of a session decode (a
// message's blocks, [eachClaudeBlock]; a tool's answer, [toolAnswer]; an
// OpenCode step's items, [eachOpenCodeItem]; a run's result's errors,
// [claudeResult.errorsSaid]; a page of an OpenCode session's message list,
// [openCodeEvents.page]), a call's result content, which the reader that
// tells a relaunch which of its calls a kept session holds (exact telling)
// reads no further than its second element, and a loaded-tools record's list,
// which the scrub cuts a definition out of where it sits in the line
// ([withoutDefinitions]).
func eachElement(list json.RawMessage, each func(element json.RawMessage, at int) error) error {
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
		if err = each(element, int(dec.InputOffset())-len(element)); err != nil {
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

// eachKey hands each key of object, raw JSON holding one object, to each, in
// order, as it reads it, beside that key's value: the key's name unquoted as
// [threadDecoding] reads a name (an escape resolved, and invalid UTF-8 or a
// lone surrogate as U+FFFD), the value's raw bytes, as written but for the
// whitespace around them, and where in object the value sits, the offset of
// its first byte. Each is valid only while each runs, so each copies what it
// keeps. It NEVER COLLECTS THEM, and never folds two keys of
// one name into one: a key written twice is handed both times, in its place,
// and which of the two counts is each's to say (JSON.parse reads the last).
// It stops at the first error each returns, reads no key after it, and
// returns that error unchanged; an object that is no object, or that does not
// decode, is an error of its own.
//
// THE ONE WALK of an object whose keys a box chose, as [eachElement] is of a
// list, under the same options. An object whose keys a reader names is
// decoded into a struct that holds those keys, every other key passed over
// and never kept; one whose keys are open, as a map's are, is walked, so what
// it costs its reader is what each makes of one key at a time, never a map of
// every key first. Decoded whole into a map, a result line of one mebibyte
// naming a model in each of its 95,315 keys cost the run's event stream
// 286,787 allocations and 45 MB, 42.6 times the line, and a line of four
// mebibytes four times that (measured): about 1.4 GB at the 32 MiB a line of
// the stream may hold. Its readers: a run's result's spend model by model
// ([claudeResult.sinceLastResult]), and the scrub's reading of a loaded-tools
// record, which finds where the record's attachment and the attachment's list
// sit in the line ([lastValue]) and the name of each definition it lists
// ([brokenDefinition]).
//
// THE NAME IS HANDED AS BYTES, in a buffer the walk reuses, never as a
// string: a string would be an allocation for every key, which a reader that
// keeps none would pay for each key of an object of millions. A reader
// compares it as string(name), which costs nothing, and makes a string of a
// name it keeps.
func eachKey(object json.RawMessage, each func(name []byte, value json.RawMessage, at int) error) error {
	if trimmed := bytes.TrimLeft(object, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("is %s, not an object", wireKind(trimmed))
	}
	dec := jsontext.NewDecoder(bytes.NewBuffer(object), threadDecoding)
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	var name []byte
	for dec.PeekKind() != '}' {
		quoted, err := dec.ReadValue()
		if err != nil {
			return err
		}
		// UNQUOTED BEFORE ITS VALUE IS READ, into the walk's own buffer: what
		// the decoder hands out is valid only until its next read. The name
		// was read whole, so the only errors unquoting it meets are a lone
		// surrogate's and invalid UTF-8's, each of which it writes as U+FFFD
		// in its place, as the options' own decode of a name does and drops
		// the error.
		name, _ = jsontext.AppendUnquote(name[:0], quoted)
		value, err := dec.ReadValue()
		if err != nil {
			return err
		}
		if err = each(name, value, int(dec.InputOffset())-len(value)); err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return errors.New("an object is followed by more than whitespace")
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

// wireCountMax is the largest count [wireCount] reads: 2^53 - 1
// (Number.MAX_SAFE_INTEGER), up to which Claude Code's own JSON.parse reads
// every whole number exactly as written. Past it, JSON.parse reads a count as
// the nearest double, which can be another number than the one written
// (9,007,199,254,740,993 reads as 9,007,199,254,740,992), so a count past it
// is refused, never read otherwise than the harness reads it. And four counts
// at it sum below 2^55, so no int64 sum of a reply's counts wraps, nor one of
// them and a whole thread's bytes ([threadPlainMax]), where four counts that
// each fit an int64 can sum past one, to a negative, which the harness's own
// sum of doubles never does.
const wireCountMax = 1<<53 - 1

// wireCount is raw, a key named key, held to a count: a whole number as JSON
// writes one, its digits alone with no sign, fraction or exponent, at most
// [wireCountMax]; 0 where the key is absent. Checked by its bytes before it is
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
	if n > wireCountMax {
		return 0, fmt.Errorf("%s is %d, past %d, the most JSON.parse reads exactly", key, n, int64(wireCountMax))
	}
	return n, nil
}

// wireKind says what raw is: "absent" where its key is not there, else the
// kind of JSON value it holds.
func wireKind(raw json.RawMessage) string { return kindOfValue(raw).String() }

// valueKind is the kind of a JSON value as its first byte tells it, as
// written: '"' a string, '{' an object, '[' a list, 't' and 'f' a boolean,
// 'n' null, any other a number; 0 where the value is absent. The facts of a
// line hold a value's kind where a reader asks only that of it
// ([lineFacts]), and an error words it as [wireKind] does.
type valueKind byte

// kindOfValue is raw's kind ([valueKind]).
func kindOfValue(raw []byte) valueKind {
	if len(raw) == 0 {
		return 0
	}
	return valueKind(raw[0])
}

// String says what k is, as an error words it: "absent", "a string", "an
// object", "a list", "a boolean", "null" or "a number".
func (k valueKind) String() string {
	switch k {
	case 0:
		return "absent"
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
	o := l.facts().origin
	if o == nil {
		return "", nil
	}
	if !wireObject(o) {
		return "", fmt.Errorf("origin is %s, not an object", wireKind(o))
	}
	var origin struct {
		Kind json.RawMessage `json:"kind"`
	}
	if err := wireDecode(o, &origin); err != nil {
		return "", fmt.Errorf("origin: %w", err)
	}
	return wireString("origin.kind", origin.Kind)
}

// UUID is the record's own `uuid`, the id a chained record after it names as
// its parent: "" where the key is absent, as it is on every record outside
// the chain (the harness's bookkeeping, at [threadHead.Parent]), and where it
// is the empty string, which UUID does not tell apart. HANDED OVER AS
// WRITTEN, with no rule of its length or shape: every chained record measured
// carries a 36-byte uuid (9,806 of 9,806, and none of the 6,003 records
// outside the chain carries the key), and its one reader holds it to a rule
// of its own.
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as a key one reader reads is: a
// string where present. Any other type is this accessor's error, never the
// line's, so a reshaped uuid costs only the reader that asks for it. Every
// other reader reads no uuid, and held in the head, a release that wrote one
// of another type would cost each of them every line that carries it, which
// is every chained line of every thread.
//
// ITS READER is the resumed count ([resumedReading]), which bounds what a
// continued session's harness counts before its first request. It reads the
// uuid of every line the restart rule flags ([threadRestarts.See]), and keeps
// the newest's, which a later relaunch compares with the one the job's launch
// handed it, to know whether the job restarted the session after its brief:
// never by counting restarts, since the harness's trim after a compaction drops
// every record before the last one ([claudeLocalGCFloor]), so a thread that
// restarted again can hold no more of them than it did. A uuid that is absent,
// empty or longer than 36 bytes, and this accessor's error, are no reading to
// it.
//
// Read off the line's one decode, its raw `uuid`, never a second read of the
// line.
func (l threadLine) UUID() (string, error) { return wireString("uuid", l.facts().uuid) }

// Version is the record's `version`, the release of Claude Code that wrote
// it: "" where the key is absent, and an error where it holds no string. It
// is the box's word, as long as a line, handed out as written.
//
// EVERY CHAINED RECORD NAMES ITS OWN WRITER: each of the 11,727 chained
// records of the 432 main and 89 subagent threads the probes kept of 2.1.310
// and 2.1.322 carries a string `version`, its writer's release, whatever its
// kind (user, assistant, attachment and system alike). A session continued
// across releases holds the records of each, so the release a session's row
// records, its last writer's, says nothing of the records an older one wrote
// before it. The harness's bookkeeping carries one on some kinds and not on
// others, which no reader reads.
//
// IN AN ACCESSOR, NEVER THE HEAD: one reader reads it, the reading of which
// calls a kept session holds ([writerBelowFloor]), so a release that reshapes
// it costs that reading alone, never a reader of attachments or of a
// transcript's words.
func (l threadLine) Version() (string, error) { return wireString("version", l.facts().version) }

// HandsBack reports whether a user record's own `toolUseResult` carries a
// handback ([carriesHandback]); false on every other record, which carries
// none a reader reads.
//
// IT IS THE RECORD'S, NEVER A CALL'S: a reader takes it for every result the
// record holds, a handback of any one of them being one the record carries.
func (l threadLine) HandsBack() bool {
	return l.Head.Type == recordUser && l.facts().result.handsBack
}

// AgentAnswer is what a user record's own `toolUseResult` says of a
// subagent's leg ([agentAnswer]): whether an Agent or Task call's subagent
// completed, the subagent it names, and whether a SendMessage was refused;
// none of them on every other record.
//
// IT IS THE RECORD'S, NEVER A CALL'S: a reader takes it for a call's only
// where the record's message holds that call's one tool_result and nothing
// else, as every record measured that carries one does.
func (l threadLine) AgentAnswer() (completed bool, agent string, refused bool) {
	if l.Head.Type != recordUser {
		return false, "", false
	}
	r := &l.facts().result
	return r.completed, r.agent, r.refused
}

// answerOf is what a leg's result record's own toolUseResult says of the leg
// ([agentAnswer]), its `status`, `agentId` and `success` as written: whether
// an Agent or Task call's subagent completed (`status` "completed"), the
// subagent it names (`agentId`, "" where it names none a reading reads), and
// whether a SendMessage was refused (`success`, the literal false).
func answerOf(status, agentID, success json.RawMessage) (completed bool, agent string, refused bool) {
	s, err := wireRequiredString("status", status)
	completed = err == nil && s == claudeAgentCompleted
	if agent, err = wireRequiredString("agentId", agentID); err != nil || !validClaudeAgent(agent) {
		agent = ""
	}
	return completed, agent, string(success) == "false"
}

// The record types and subtypes this layer reads by name.
const (
	recordUser       = "user"
	recordAssistant  = "assistant"
	recordAttachment = "attachment"
	recordSystem     = "system"

	// recordProgress is a record of a tool's progress, a legacy shape that
	// Claude Code's load keeps out of the map it walks, re-parenting every
	// record after it that names its uuid as a parent to the progress
	// record's own parent instead (2.1.322's L_e and 2.1.310's YBe, the
	// load's legacy progress bridge). No release the probes ran writes one:
	// none of the 521 thread files they kept holds one.
	recordProgress = "progress"
	// recordContentReplacement is the record of the results a reply's calls
	// returned that the harness hands its model replaced
	// ([threadLine.ReplacedResults]), and replacementToolResult the kind of
	// its entry that replaces a tool's result.
	recordContentReplacement = "content-replacement"
	replacementToolResult    = "tool-result"
	// recordLastPrompt is the record of the leaf Claude Code's resume starts
	// from ([threadLine.Checkpoint]).
	recordLastPrompt = "last-prompt"
	// recordForkContextRef is the record a forked subagent's thread carries
	// to refer its load to the thread it forked from, whose walk 2.1.322's
	// subagent load (Rye) sets before its own.
	recordForkContextRef = "fork-context-ref"
	// attachmentForkBriefing is the attachment a fork's copy of a session
	// carries ([threadLine.AttachmentType]), and subtypeFileSnapshot the
	// system record of a file's snapshot. Claude Code's resume takes neither
	// for the last record a session wrote where 2.1.322 chooses the leaf it
	// starts from; 2.1.310 takes a file's snapshot for one. No thread the
	// probes kept holds either.
	attachmentForkBriefing = "fork_briefing"
	subtypeFileSnapshot    = "file_snapshot"

	// subtypeRefusalFallback is the system record Claude Code writes where a
	// model refused a reply and it moved the request to another model,
	// naming the records it took back out of the conversation
	// ([threadLine.Retracted]).
	subtypeRefusalFallback = "model_refusal_fallback"

	// subtypeCompactBoundary is a compaction's boundary ([threadRestarts]).
	subtypeCompactBoundary = "compact_boundary"
	// subtypeMicrocompactBoundary is a microcompaction's boundary, which no
	// release measured writes ([threadRestarts.See]).
	subtypeMicrocompactBoundary = "microcompact_boundary"

	// editClearToolUses is the server's context edit that clears the content
	// of old tool results ([threadLine.ToolResultsCleared]).
	editClearToolUses = "clear_tool_uses_20250919"
)

// hookSystemMessage is the record of a hook's firing that holds its answer's
// system message, the take's token among it: the reading that confirms a
// hook's note reached its session reads it ([threadLine.HookTakes]).
const hookSystemMessage = "hook_system_message"

// hookRecordPrefix begins the type of every attachment a hook's firing
// writes: only such a record's words are read for takes
// ([threadLine.HookTakes]), so an asking read decodes no skill listing.
const hookRecordPrefix = "hook_"

// The attachments whose payloads Claude Code's resume holds to a shape
// ([threadLine.AttachmentWellFormed]): two of the records of a hook's firing,
// its success and its additional context, and the others.
const (
	hookSuccess              = "hook_success"
	hookAdditionalContext    = "hook_additional_context"
	attachmentInvokedSkills  = "invoked_skills"
	attachmentSkillListing   = "skill_listing"
	attachmentTaskReminder   = "task_reminder"
	attachmentTodoReminder   = "todo_reminder"
	attachmentFile           = "file"
	attachmentAlreadyRead    = "already_read_file"
	attachmentWithheldMemory = "withheld_memory"
)

// skillListingMaxNames and skillListingMaxNameUnits are the most names a skill
// listing's resume keeps it with, and the most UTF-16 code units each may
// take (2.1.322's bCr and TCr, 2.1.310's fJo and pJo): Claude Code's own
// bounds, which [threadLine.AttachmentWellFormed] reads them by.
const (
	skillListingMaxNames     = 4096
	skillListingMaxNameUnits = 512
)

// threadMessage is a user or assistant record's message as its one decode
// left it: what its readers ask of its content where that is one string (that
// it is one, the subagent and the leg a task notification names, whether it
// names the handback tool), or its blocks. IT HOLDS NO TEXT: a prompt or a
// notification is as long as whatever wrote it made it, and each fact its
// readers ask of one is read as the text streams ([textNotice],
// [textHandback], [textSays]). AND NOTHING OF A BLOCK BUT A CALL: a list holds
// as many blocks as whatever wrote it made it, and what its readers ask of the
// blocks that are no call is how many there are and which kinds among them
// ([threadBlocks]), a count and a set.
type threadMessage struct {
	isText bool
	// notification and namesHandback are [threadMessage.Notification]'s and
	// [threadMessage.NamesHandback]'s.
	notification  taskNotice
	namesHandback bool
	blocks        threadBlocks
	calls         []threadCall
	// saysText and saysAny are [threadMessage.Says]'s.
	saysText, saysAny bool
}

// threadCall is one call block of a message: a model's call, or the result it
// was answered with.
//
// IT HOLDS FACTS, NEVER A CALL'S BYTES: a call's input and a result's content
// are each as long as whatever wrote them made them, and what a reader asks of
// them is the subagent a SendMessage sends to and the text a result holds as a
// log's output is compared with, each read here, by value, so a call costs the
// same whatever its input or its result held.
type threadCall struct {
	// Kind is "tool_use" or "tool_result".
	Kind string
	// ID is the call's id: a tool_use's `id`, a tool_result's `tool_use_id`.
	ID string
	// Name is the tool a tool_use called.
	Name string
	// To is the subagent a tool_use's `input` sends to ([sendTarget]): its
	// `to`, where that is a string naming an agent; "" else, and on a
	// tool_result.
	To string
	// Failed is whether a tool_result's `is_error` is true; false where it is
	// absent, and on a tool_use.
	Failed bool
	// Text is a tool_result's text ([resultText]), as the reading compares it
	// with a log's output ([threadResultText]); no text, its zero value, where
	// its content holds none, and on a tool_use.
	Text threadResultText
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

// blockKeys is what a content block's rule reads of it ([blockCallOf]), each
// key's raw as written, nil where absent: its `type`, `id`, `name`,
// `tool_use_id` and `is_error`; to, the subagent its `input` sends to
// ([sendTargetOf]); and text, its `content`'s text ([threadResultText]).
type blockKeys struct {
	typ, id, name, toolUseID, isError json.RawMessage
	to                                string
	text                              threadResultText
}

// blockCallOf is a content block of kind, b its keys, as [threadLine.Message]
// holds one to its types: an object whose `type` is a string; a tool_use's
// `id` a string that names a call ([callID]) and its `name` a string; a
// tool_result's `tool_use_id` the same and its `is_error` a boolean where
// present. Its call carries its kind, the layer's own string where the layer
// names it ([knownKind]), and of a tool_use its target, of a tool_result its
// failure and its text; the other keys of a block that is no call are read by
// no rule. THE ONE RULE OF A BLOCK, whichever reading of the line hands it
// the keys.
func blockCallOf(kind valueKind, b blockKeys) (threadCall, error) {
	if kind != '{' {
		return threadCall{}, fmt.Errorf("is %s, not an object", kind)
	}
	k, err := wireRequiredString("type", b.typ)
	if err != nil {
		return threadCall{}, err
	}
	c := threadCall{Kind: knownKind(k)}
	switch k {
	case blockToolUse:
		if c.ID, err = callID("id", b.id); err != nil {
			return threadCall{}, err
		}
		if c.Name, err = wireRequiredString("name", b.name); err != nil {
			return threadCall{}, err
		}
		c.To = b.to
	case blockToolResult:
		if c.ID, err = callID("tool_use_id", b.toolUseID); err != nil {
			return threadCall{}, err
		}
		if b.isError != nil {
			if c.Failed, err = wireBool("is_error", b.isError); err != nil {
				return threadCall{}, err
			}
		}
		c.Text = b.text
	}
	return c, nil
}

// sendTargetOf is the subagent a SendMessage call's input sends to, input its
// kind and to its `to` as written: `to` where the input is an object and that
// is a string naming an agent ([validClaudeAgent]); "" else, a send to no
// subagent a reading reads.
func sendTargetOf(input valueKind, to json.RawMessage) string {
	if input != '{' {
		return ""
	}
	target, err := wireRequiredString("to", to)
	if err != nil || !validClaudeAgent(target) {
		return ""
	}
	return target
}

// resultElementIsText reports whether a tool_result's content list's one
// element is its text ([resultText]): an object, kind, whose `type`, typ, is
// the string "text", and whose `text` is of the kind text, a string.
func resultElementIsText(kind valueKind, typ json.RawMessage, text valueKind) bool {
	if kind != '{' || text != '"' {
		return false
	}
	k, err := wireRequiredString("type", typ)
	return err == nil && k == blockText
}

// knownKind is kind, a block's type, held as this layer's own constant where
// it is one of the blocks the layer names, so a call a message holds keeps no
// string of its own for its kind.
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

// threadBlocks is what a message's content list says of its blocks
// ([threadMessage.Blocks]): how many it holds, and which kinds among them.
//
// A COUNT AND A SET, NEVER A KIND A BLOCK: every reader asks how many blocks a
// list holds or whether they are all of some kinds (a leg's start, a finished
// reply, a plain results record, a result alone on its record), and none asks
// a block's place in the list. So a list of a million blocks holds two words
// here, not a million kinds.
type threadBlocks struct {
	n    int
	seen blockSet
}

// blockSet is a set of the kinds of block, one member for each kind the layer
// names and one, blockOther, for every kind it does not.
type blockSet uint8

const (
	blockSetToolUse blockSet = 1 << iota
	blockSetToolResult
	blockSetText
	blockSetThinking
	blockSetRedactedThinking
	// blockOther is every kind the layer does not name: one member for all of
	// them, since no reader tells one unnamed kind from another, and each
	// reads one as a block of no kind it names.
	blockOther
)

// blockOf is kind's member of a [blockSet]: its own where the layer names it,
// blockOther else.
func blockOf(kind string) blockSet {
	switch kind {
	case blockToolUse:
		return blockSetToolUse
	case blockToolResult:
		return blockSetToolResult
	case blockText:
		return blockSetText
	case blockThinking:
		return blockSetThinking
	case blockRedactedThinking:
		return blockSetRedactedThinking
	}
	return blockOther
}

// add counts a block of kind.
func (b *threadBlocks) add(kind string) {
	b.n++
	b.seen |= blockOf(kind)
}

// Len is how many blocks the list holds: 0 for a list of none, and for every
// message that holds no list, a content that is one string
// ([threadMessage.IsText] tells it) and the zero message among them.
func (b threadBlocks) Len() int { return b.n }

// All reports whether every block is of one of kinds, each one of the layer's
// own names of a block (blockText, blockToolUse...): true of a list of none,
// which a reader tells by [threadBlocks.Len]. A kind the layer does not name
// matches no block, so a block of a kind the layer does not name is all of no
// kinds a reader can ask for: never a sign that a reply finished, nor a block
// that holds no call.
func (b threadBlocks) All(kinds ...string) bool {
	var named blockSet
	for _, kind := range kinds {
		if s := blockOf(kind); s != blockOther {
			named |= s
		}
	}
	return b.seen&^named == 0
}

// Message is a user or assistant record's `message`, read once, as its line
// is decoded ([factsOf]): a reader that asks twice is handed what that one
// reading left, never a second decode. It is the zero message for every other
// record, whatever `message` it carries: an api-request-blob repeats what a
// request sent, and a call in it is no call the thread made.
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
// however many follow, and what a list read to its end leaves held is a call
// for each call block, each kept as it is accepted, and of every block its
// count and its kind's member of a set ([threadBlocks]), nothing a block:
// while it reads, a message holds, beyond those, one copy of its content and
// one block's decode. A list of four million `1`s at [threadLineMax] is
// refused at its first block for the allocations a list of one costs and, in
// bytes, the copies of the message and of its content its line's decode
// makes; a line at that bound of the smallest blocks of a kind no reader
// names leaves nothing of them held; and a line at that bound of the smallest
// results a list can hold leaves their calls held, about three and a half
// times the line (measured): a call's fields for each, at most a quarter over
// for append's growth.
func (l threadLine) Message() (threadMessage, error) {
	// record is what an error of the message calls the record.
	var record string
	switch l.Head.Type {
	case recordAssistant:
		record = "an assistant record"
	case recordUser:
		record = "a user record"
	default:
		return threadMessage{}, nil
	}
	m := &l.facts().message
	if err := m.object(record); err != nil {
		return threadMessage{}, err
	}
	if m.contentErr != nil {
		return threadMessage{}, fmt.Errorf("%s's message: %w", record, m.contentErr)
	}
	return m.content, nil
}

// callID is raw, a call block's key named key, held to a string that names a
// call. AN ID THAT NAMES NONE IS REFUSED: the API names every call a model
// makes, no block measured carries an empty id (811 calls and 744 results, every
// thread the probes kept), and handed out, a call whose id names none is one
// no reader could match against a call it knows.
func callID(key string, raw json.RawMessage) (string, error) {
	id, err := wireRequiredString(key, raw)
	if err == nil && id == "" {
		err = fmt.Errorf("%s names no call", key)
	}
	return id, err
}

// taskNotice is the subagent and the leg a task notification's text names
// ([textNotice]), each a string of its own of at most 64 bytes, never a piece
// of the text; ok where it names them.
type taskNotice struct {
	agent, leg string
	ok         bool
}

// IsText reports whether the message's content is one string: a prompt, a
// task notification or a message sent to a subagent, rather than blocks.
func (m threadMessage) IsText() bool { return m.isText }

// Notification is the subagent and the leg the message's text names where it
// is a TASK NOTIFICATION's ([textNotice]): its lines `<task-notification>`,
// `<task-id>A</task-id>`, `<tool-use-id>L</tool-use-id>`, any output files'
// and `<status>completed</status>`, A an agent ([validClaudeAgent]) and L an
// id the call-id rule keeps ([sandbox.ValidCallID]); ok false for any other
// text, and for a message of blocks. It is the text's own claim, whatever
// record holds it: its reader takes it for a notification only where the
// record's origin says so, and matches the leg only against a call it read.
func (m threadMessage) Notification() (agent, leg string, ok bool) {
	return m.notification.agent, m.notification.leg, m.notification.ok
}

// NamesHandback reports whether the message's text names the handback tool
// ([claudeToolHandback]) anywhere ([textHandback]): what marks a task
// notification's leg as answered by a handback. False for a message of blocks.
func (m threadMessage) NamesHandback() bool { return m.namesHandback }

// Blocks is how many blocks the message's content list holds, and which
// kinds among them ([threadBlocks]): none where the message holds no list, a
// content that is one string ([threadMessage.IsText] tells it) and the zero
// message every record but a user's or an assistant's reads as among them,
// and none for a list of none. A reader tells a turn's own message from the
// results of its calls by it: a user record whose blocks are tool_result
// alone, or not. A kind the layer does not name is a kind of its own in the
// set, for its reader to treat as unknown: never as a sign that a reply
// finished, nor as a block that holds no call.
func (m threadMessage) Blocks() threadBlocks { return m.blocks }

// Calls is the message's call blocks: an assistant's tool_use blocks, a user's
// tool_result blocks. THE ONE WAY AN ID LEAVES THIS LAYER AS A CALL'S: no
// accessor hands out "a line's ids", which would hand a reader of calls a
// hook's firing; an attachment's `toolUseID` leaves only as a key
// ([threadLine.AttachmentToolUseID]); and a task notification's
// `<tool-use-id>` leaves as the leg its text claims
// ([threadMessage.Notification]), which its one reader matches only against a
// call it read here.
func (m threadMessage) Calls() []threadCall { return m.calls }

// Says reports whether the message SAYS ANYTHING, as Claude Code reads a
// message's text: text, whether a block of the type "text" holds a string
// `text` that JavaScript's trim() leaves something of (2.1.322's t5t); any,
// whether the content is one such string, or any block, of whatever type,
// holds a string `text` trim() leaves something of (2.1.322's SUo). Both false
// on the zero message.
//
// WHAT IT SAYS. A subagent answers the call that started it with the text of
// its last reply that says something (2.1.322's pkn, through t5t), so a leg
// whose last reply says nothing answers with an earlier reply's text; and the
// resume classes a reply that makes no call by whether it says anything
// (2.1.322's LE, through SUo: a reply of text, or a thinking or a blank one),
// where that class decides the leaf an unseen flag may move a resume to.
//
// JAVASCRIPT'S trim(), NOT strings.TrimSpace: trim() takes U+0009, U+000B,
// U+000C, U+0020, U+00A0, U+FEFF, the category Zs, U+000A, U+000D, U+2028 and
// U+2029 off a string's ends ([jsTrimKeeps]), where strings.TrimSpace takes
// U+0085 too and leaves U+FEFF, so a text of U+FEFF alone says nothing to the
// harness and a text of U+0085 alone says something.
//
// ITS READERS: the reading that tells a relaunch which of its calls a kept
// session holds (exact telling), which takes a subagent's leg for finished
// only on a reply whose text says something, and the model of which records
// Claude Code's resume hands its model, which classes a reply as its resume
// does.
//
// Read as [threadLine.Message] walks the message's blocks, off the one decode
// of each block it makes, never a second read of the content.
func (m threadMessage) Says() (text, any bool) { return m.saysText, m.saysAny }

// jsTrimKeeps reports whether JavaScript's String.prototype.trim keeps r on a
// string's end: whether r is neither its white space (U+0009, U+000B, U+000C,
// U+0020, U+00A0, U+FEFF and the category Zs) nor its line terminator
// (U+000A, U+000D, U+2028, U+2029). Not unicode.IsSpace, which
// strings.TrimSpace reads, and which holds U+0085 for space and U+FEFF for
// none. So a text says something to the harness exactly where it holds a rune
// this keeps ([threadMessage.Says]).
func jsTrimKeeps(r rune) bool {
	switch r {
	case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029':
		return false
	}
	return !unicode.Is(unicode.Zs, r)
}

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
// "end_turn" or "stop_sequence" on every other record of text alone. Its
// reader takes a leg for finished on "end_turn" alone, with no API error and
// a model other than "<synthetic>" ([threadLine.APIError],
// [threadLine.Model]), as Claude Code itself tests a reply with which its
// model ended its turn: every reply the harness writes itself in place of a
// model's, as 2.1.310 and 2.1.322 build one, is of the model "<synthetic>" and
// stops at "stop_sequence" (a resume's "No response requested.", a failed
// request's error) or, where it calls a tool, at "tool_use".
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
	// HELD TO AN OBJECT BEFORE IT IS READ: a null holds no key at all, and
	// would read as a reply that says nothing of its stop.
	m := &l.facts().message
	if err := m.object("an assistant record"); err != nil {
		return "", err
	}
	return wireString("stop_reason", m.stopReason)
}

// APIError reports whether an assistant record is the reply Claude Code wrote
// itself where its request failed for good, in place of a model's: its
// `isApiErrorMessage`, true where it says so; false where it is absent or
// false, and on every record that is no assistant's, whatever it carries.
//
// WHAT THE MARK SAYS, as 2.1.310 and 2.1.322 write and read it. A request that
// fails for good (an overload past its retries, a prompt past its window, a
// credential refused) leaves a reply of the harness's own in its thread, its
// text the error's ("API Error: ..."), its model "<synthetic>"
// ([threadLine.Model]), stopped at "stop_sequence", and marked true; the reply
// a resume writes for a turn it found unanswered is marked false. The
// harness's request skips a reply marked true and of that model (2.1.322's A8,
// 2.1.310's mee: `isApiErrorMessage===!0`), and its resume classes a reply
// marked anything JavaScript reads as true, or of that model, as one its
// harness wrote (2.1.322's LE). A subagent answers the call that started it
// with its last reply that says something (2.1.322's pkn), and a failed
// request's reply says its error, so a subagent whose last request failed so
// answers with that text, though no model read what the request carried.
// Every reply the probes kept that carries the mark is of the model
// "<synthetic>": 40 a resume wrote, false, and 2 a failed request left, true.
//
// ITS READERS: the model of which records Claude Code's resume hands its
// model, which reads a reply its harness wrote as the resume classes one,
// where that class decides the leaf an unseen flag of its may move to; and
// the reading that tells a relaunch which of its calls a kept session holds
// (exact telling), which pairs a call only within its reply's own records
// none of which its harness wrote, and takes a subagent's leg for finished
// only on a reply its model wrote ([threadLine.StopReason]).
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as a key one reader reads is: a
// boolean where present. Any other type, null among it, is this accessor's
// error, never the line's, so a reshaped mark costs only the readers that ask
// for it, and never false, which its readers would take for a model's reply:
// the harness reads the one mark as JavaScript's truth where its resume
// classes a reply and as the JSON true alone where its request skips one, and
// only a boolean reads alike to both.
//
// Read off the line's one decode, its raw `isApiErrorMessage`, never a second
// read of the line.
func (l threadLine) APIError() (bool, error) {
	f := l.facts()
	if l.Head.Type != recordAssistant || f.apiError == nil {
		return false, nil
	}
	return wireBool("isApiErrorMessage", f.apiError)
}

// Model is the model that wrote the reply an assistant record holds, its
// message's `model` as written: "<synthetic>" where Claude Code wrote the
// reply itself in place of a model's (a resume's "No response requested.", a
// failed request's error); "" where the key is absent, and on every record
// that is no assistant's, whatever its `message` carries.
//
// ITS READERS are [threadLine.APIError]'s, each of which asks both, as the
// harness does: its resume classes a reply of the model "<synthetic>" as its
// harness's own whatever its mark says (2.1.322's LE: `message.model===$i`),
// and its request skips a reply only where both say so (2.1.322's A8,
// 2.1.310's mee).
//
// HELD TO ITS TYPE HERE, for these readers, as [threadLine.Usage] holds the
// same key for its own: a string where present. Any other type, null among
// it, is this accessor's error, never "", which its readers would take for a
// model's reply, and never Message's, so a reshaped model costs only the
// readers that ask for it.
//
// Read off the message's own key as written, which the line's one decode
// keeps ([factsOf]), as [threadLine.StopReason] reads its key.
func (l threadLine) Model() (string, error) {
	if l.Head.Type != recordAssistant {
		return "", nil
	}
	// HELD TO AN OBJECT BEFORE IT IS READ: a null holds no key at all, and
	// would read as a reply that names no model.
	m := &l.facts().message
	if err := m.object("an assistant record"); err != nil {
		return "", err
	}
	return wireString("model", m.model)
}

// MessageID is the id of the reply an assistant record holds, its message's
// `id`, which every record of one reply carries, since the harness writes a
// reply a record a block ([threadLine.StopReason]): "" where the key is
// absent, and on every record that is no assistant's, whatever its `message`
// carries.
//
// WHAT THE ID SAYS, as 2.1.310 and 2.1.322 read it. Once Claude Code's resume
// has walked a thread's chain from its leaf, it hands its model, beside each
// reply on that chain, the reply's other records, found by this id wherever
// they lie, and the results whose parent is one of them (2.1.322's fWo,
// 2.1.310's Rpo, each grouping every assistant record whose `message.id` is
// truthy); and its request folds the records of one id into one message.
//
// ITS READERS: the model of which records Claude Code's resume hands its
// model, which groups a reply's records by it as the resume does, and through
// that model the reading that tells a relaunch which of its calls a kept
// session holds (exact telling), which pairs a call by the reply its use
// belongs to, and the one that confirms a hook's note reached the model.
//
// HELD TO ITS TYPE HERE, as [threadLine.Usage] holds the same key for its
// own reader: a string where present. Any other type, null among it, is this
// accessor's error, never "", and never Message's, so a reshaped id costs
// only the readers that ask for it.
//
// AND TO A TEXT ITS READERS COMPARE AS THE HARNESS DOES: an id holding the
// replacement character, U+FFFD, is this accessor's error too. A lone
// surrogate reads alike to both: the layer's decode reads it as U+FFFD
// ([threadDecoding]), and the resume's load makes every string of each record
// it keeps well-formed the same way before it compares one (2.1.322's tIe,
// 2.1.310's hYe, JavaScript's toWellFormed), so `msg_\ud800` and `msg_\ud801`
// are one id to it too. A byte that is no UTF-8 need not read alike: the
// layer's decode reads each such byte as one U+FFFD, where the harness's read
// of its file can read a run of them as one, so two ids it holds apart (one
// ending in the two bytes of a sequence cut short, E2 82, which it reads as
// one U+FFFD, and one ending in FF FF, two) would decode as one and recover
// each other's records, which no resume does. A U+FFFD read off an id cannot
// say which it came from, so every id holding one is refused. Refused, such an
// id costs its readers their reading of the thread, the direction that tells,
// never a record recovered under another reply's id. Every one of the 2,239
// assistant records of the 521 thread files the probes kept carries an ASCII
// id of at most 36 bytes; only a box rewriting its own file writes another.
//
// Read off the message's own key as written, which the line's one decode
// keeps ([factsOf]), as [threadLine.StopReason] reads its key.
func (l threadLine) MessageID() (string, error) {
	if l.Head.Type != recordAssistant {
		return "", nil
	}
	// HELD TO AN OBJECT BEFORE IT IS READ: a null holds no key at all, and
	// would read as a reply that names no id.
	m := &l.facts().message
	if err := m.object("an assistant record"); err != nil {
		return "", err
	}
	id, err := wireString("id", m.id)
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(id, utf8.RuneError) {
		return "", errors.New("id holds the replacement character, which a lone surrogate or a byte that is " +
			"no UTF-8 decodes as, so two ids the harness holds apart can read as one")
	}
	return id, nil
}

// threadTime is the form Claude Code writes a record's time in, JavaScript's
// toISOString (YYYY-MM-DDTHH:MM:SS.sssZ), as a layout of package time
// ([threadLine.Timestamp]).
const threadTime = "2006-01-02T15:04:05.000Z"

// Timestamp is when a record says it was written, its `timestamp`: ms, the
// milliseconds since the Unix epoch, where it is the form Claude Code writes
// (`YYYY-MM-DDTHH:MM:SS.sssZ`, JavaScript's toISOString) of a real time, and
// present true; present false where the key is absent.
//
// WHAT THE TIME SAYS, as 2.1.310 and 2.1.322 read it. Claude Code's resume
// compares two records' times both ways: as strings, where it takes a
// session's newest record (its load's `timestamp>co`, 2.1.322's Szt and
// 2.1.310's GMt) and the newest record under the last (2.1.322's Gr); and as
// Date.parse's milliseconds, where it takes the latest of a set of leaves
// (2.1.322's cj, 2.1.310's p0) and walks on past a parent it cannot find to
// the record of the nearest earlier time (2.1.322's vjt, 2.1.310's Epo, at
// most 5,000 ms before). On that one form the two orders are one, and
// Date.parse is exact to the millisecond. Every one of the 11,727 user,
// assistant, attachment and system records of the 521 thread files the
// probes kept carries one in that form.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model, which orders records by these milliseconds wherever the resume
// orders them by either reading.
//
// MILLISECONDS, NOT A time.Time, as CLAUDE.md would have a time held: on the
// one form this admits, the resume's two orders are the milliseconds' order,
// exact, so a time.Time would add nothing its reader compares, and its reader
// keeps one a record at its bound, in 8 bytes rather than a time.Time's 24.
//
// HELD TO THAT ONE FORM HERE, AND HERE ALONE, as a key one reader reads is:
// anything else is this accessor's error, never the line's. That is a value
// that is no string, and a string of another form or of no real time, such
// as an offset in place of the Z, no milliseconds, 2026-02-29, an hour of 24
// or a second of 60: on another form the string order and the time order can
// part, and Date.parse reads it by rules no reader can be sure of, so no
// reader can say which record the resume takes for the newest.
//
// Read off the line's one decode, its raw `timestamp`, never a second read of
// the line.
func (l threadLine) Timestamp() (ms int64, present bool, err error) {
	raw := l.facts().timestamp
	if raw == nil {
		return 0, false, nil
	}
	text, err := wireRequiredString("timestamp", raw)
	if err != nil {
		return 0, false, err
	}
	// Parsed by the layout, then held to it: time.Parse takes some text the
	// layout would not write, which formatting it again tells apart.
	t, err := time.Parse(threadTime, text)
	if err != nil || t.Format(threadTime) != text {
		return 0, false, errors.New("timestamp is not a real time in the form YYYY-MM-DDTHH:MM:SS.sssZ")
	}
	return t.UnixMilli(), true, nil
}

// IsMeta reports whether a user record is META, written by its harness rather
// than said by a person or returned by a call: its `isMeta` the JSON true;
// false where it is absent or false, and on every record that is no user's,
// whatever it carries.
//
// WHAT THE MARK SAYS, as 2.1.310 and 2.1.322 read it. Claude Code's resume reads
// a user record marked so that holds no tool_result as one hanging off the
// reply before it, which it hands on with that reply (2.1.322's UF,
// `isMeta===!0`), and walks on through it where it gathers a reply's records
// (2.1.322's X_e, 2.1.310's Yce, the same test); its request reads the mark as
// JavaScript's truth where it classes a turn (2.1.322's LE). The harness
// writes it on a skill's text, a coordinator's message, a malformed reply's
// retry and the like: 38 user records of the 521 thread files the probes kept
// carry it, each true.
//
// ITS READERS read its error each its own way: the model of which records
// Claude Code's resume hands its model reads only the JSON true as the mark,
// as the resume does, and so its error as no mark; the reading that tells a
// relaunch which of its calls a kept session holds (exact telling) takes a
// record for plain only where the mark is false or absent, and so its error
// as the mark.
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as a key one reader reads is: a
// boolean where present. Any other type, null among it, is this accessor's
// error, never the line's.
//
// Read off the line's one decode, its raw `isMeta`, never a second read of the
// line.
func (l threadLine) IsMeta() (bool, error) {
	raw := l.facts().isMeta
	if l.Head.Type != recordUser || raw == nil {
		return false, nil
	}
	return wireBool("isMeta", raw)
}

// SourceToolAssistantUUID is the record a user record says its results answer,
// its `sourceToolAssistantUUID`, as the 16 bytes of the lowercase canonical
// uuid it holds ([uuidBytes]), and true; false where the key is absent, holds
// no such uuid, or the record is no user's.
//
// WHAT IT SAYS, as 2.1.310 and 2.1.322 read it. Claude Code writes it on a
// call's result, naming the reply that made the call. Its resume reads it in
// one place: where a result's ids are not all of calls its parent made, a
// resume with the flag tengu_foamy_spring on hangs the result off the record
// this names, where that is another record than its parent and one of the
// thread, and of its side (2.1.322's fWo, 2.1.310's Rpo). Each of the 1,068
// results of the 521 thread files the probes kept names its parent.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model, which reads a result a resume could hang elsewhere as one it cannot
// place.
//
// A UUID OF ANOTHER FORM NAMES NO RECORD, and reads as none: the resume finds a
// record by its uuid as written, and the records of a thread its reader
// models each carry a lowercase canonical one, the one form a uuid's 16 bytes
// are written in one to one.
//
// Read off the line's one decode, its raw `sourceToolAssistantUUID`, never a
// second read of the line.
func (l threadLine) SourceToolAssistantUUID() ([16]byte, bool) {
	raw := l.facts().sourceToolAssistantUUID
	if l.Head.Type != recordUser || raw == nil {
		return [16]byte{}, false
	}
	source, err := wireRequiredString("sourceToolAssistantUUID", raw)
	if err != nil {
		return [16]byte{}, false
	}
	return uuidBytes(source)
}

// uuidBytes is s, a string or the bytes a streaming reader holds (a string
// made of them would cost an allocation an entry), as the 16 bytes of a
// LOWERCASE CANONICAL UUID, and true: 36 characters, a hyphen at 8, 13, 18 and
// 23 and a lowercase hex digit elsewhere, whatever its version and variant.
// False for every other string, one of an uppercase digit among them: Claude
// Code keys a thread's records by their uuids as written, so only this form
// maps 16 bytes to one string and back.
//
// BYTES, NOT CLAUDE.md'S uuid.UUID, though as wide: uuid.Parse also reads
// uppercase digits, no hyphens, braces or a urn:uuid: prefix, each a string
// Claude Code's load holds apart from the lowercase one, so a type whose parse
// its readers would reach for would merge records the load keeps apart.
func uuidBytes[T ~string | ~[]byte](s T) ([16]byte, bool) {
	var u [16]byte
	if len(s) != 36 {
		return [16]byte{}, false
	}
	n := 0
	for i := 0; i < len(s); {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return [16]byte{}, false
			}
			i++
			continue
		}
		hi, lo := lowercaseHexDigit(s[i]), lowercaseHexDigit(s[i+1])
		if hi < 0 || lo < 0 {
			return [16]byte{}, false
		}
		u[n] = byte(hi<<4 | lo)
		n, i = n+1, i+2
	}
	return u, true
}

// lowercaseHexDigit is the value of c as a lowercase hex digit, and -1 where
// it is none ([uuidBytes]).
func lowercaseHexDigit(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

// AttachmentToolUseID is the call an attachment record says it followed, as a
// key: the SHA-256 of its attachment's `toolUseID` as the layer's decode reads
// that string, and true; false where the attachment is no object (a list
// among them) or holds no `toolUseID`, or one that is no string, and on every
// record that is no attachment.
//
// WHAT IT SAYS, as 2.1.322 reads it. Where the last record a session wrote
// descends from the record its checkpoint names through a batch's hook
// records, the resume asks which call that batch followed by the `toolUseID`
// of the last of them, where that is an attachment of an object holding a
// string there (2.1.322's Y1t: `D(attachment)&&"toolUseID"in attachment`),
// and keeps the last record for the leaf it starts from where that call is
// one the reply above it made. Every one of the 825 attachments of the 521
// thread files the probes kept that carries the key carries a string.
//
// A HOOK'S FIRING IS NEVER A CALL'S ID: this hands no id out at all, only a key
// its reader compares with keys it makes of a reply's calls' ids the same
// way, so no hook's firing reaches a reader of calls through it. A PostToolUse
// hook's firing is the id of the call it followed, and a batch's an id of its
// own (`hook-<uuid>`), which no call carries.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model.
//
// Read off the attachment's own keys as written, which the line's one decode
// keeps ([factsOf]), never a second read of the line.
func (l threadLine) AttachmentToolUseID() (key [32]byte, isString bool) {
	a := &l.facts().attachment
	if l.Head.Type != recordAttachment || a.kind != '{' || a.err != nil {
		return [32]byte{}, false
	}
	id, err := wireRequiredString("toolUseID", a.toolUseID)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(id)), true
}

// IsVirtual reports whether a user or assistant record is VIRTUAL: its
// `isVirtual` the JSON true; false where it is absent or false, and on every
// record of another type, whatever it carries.
//
// WHAT THE MARK SAYS, as 2.1.310 and 2.1.322 read it. A virtual record is kept
// in its thread and handed to no model: the request's fold passes over every
// user or assistant record marked so (2.1.322's nS, 2.1.310's Vw:
// `isVirtual===!0`), as it passes over a failed request's reply
// ([threadLine.APIError]). No thread the probes kept holds one.
//
// ITS READER is the reading that tells a relaunch which of its calls a kept
// session holds (exact telling), which pairs a call only across a stretch of
// PLAIN records, its reply's own and its results', so that the request the
// continued session makes holds the call's use and its result in one
// exchange: a virtual record is no plain one, and nor is one whose mark this
// accessor cannot read. [threadLine.InterruptedByShutdown],
// [threadLine.IsCompactSummary], [threadLine.TaskDelivery] and
// [threadLine.BatchToolUses] each mark a record no plain one for it too.
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as a key one reader reads is: a
// boolean where present. Any other type, null among it, is this accessor's
// error, never false: the harness reads the mark as the JSON true where its
// request passes a record over and as JavaScript's truth in other passes, and
// only a boolean reads alike to both.
//
// Read off the line's one decode, its raw `isVirtual`, never a second read of
// the line.
func (l threadLine) IsVirtual() (bool, error) {
	raw := l.facts().isVirtual
	if (l.Head.Type != recordUser && l.Head.Type != recordAssistant) || raw == nil {
		return false, nil
	}
	return wireBool("isVirtual", raw)
}

// InterruptedByShutdown reports whether a user record says a SHUTDOWN
// INTERRUPTED the calls its results answer: its `interruptedByShutdown` the
// JSON true; false where it is absent or false, and on every record that is
// no user's, whatever it carries.
//
// WHAT THE MARK SAYS, as 2.1.310 and 2.1.322 write and read it. Where the
// harness is stopped while a call runs, it writes that call's result marked
// so, and a resume's request reads a results record marked so apart from the
// rest (2.1.322's H3, 2.1.310's Xue: `interruptedByShutdown===!0` on a user
// record holding a tool_result): it drops one whose results other records
// carry (2.1.322's Iot), rewrites one whose calls nothing else answers to the
// interrupted mark (2.1.322's Dyn), and merges none into the user message
// beside it (2.1.322's pCo, which reads the mark as JavaScript's truth). No
// thread the probes kept holds one.
//
// ITS READER is [threadLine.IsVirtual]'s: a results record marked so is no
// plain one, and nor is one whose mark this accessor cannot read.
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as [threadLine.IsVirtual]'s mark is,
// for its reason: a boolean where present, and any other type, null among it,
// this accessor's error, never false.
//
// Read off the line's one decode, its raw `interruptedByShutdown`, never a
// second read of the line.
func (l threadLine) InterruptedByShutdown() (bool, error) {
	raw := l.facts().interruptedByShutdown
	if l.Head.Type != recordUser || raw == nil {
		return false, nil
	}
	return wireBool("interruptedByShutdown", raw)
}

// IsCompactSummary reports whether a user record is a COMPACTION'S SUMMARY:
// its `isCompactSummary` the JSON true; false where it is absent or false,
// and on every record that is no user's, whatever it carries.
//
// WHAT THE MARK SAYS, as 2.1.310 and 2.1.322 write and read it. A compaction
// hands its model the conversation so far as one user record marked so,
// chained to its boundary, and the harness reads such a record apart from the
// prompts and results around it wherever it weighs a turn (2.1.322's LE
// classes one that holds no result `user_summary`, before any meta mark),
// reading the mark as the JSON true in some passes and as JavaScript's truth
// in others. Each of the 22 compactions' summaries the probes kept carries
// it, true.
//
// ITS READER is [threadLine.IsVirtual]'s: a results record marked so is no
// plain one, and nor is one whose mark this accessor cannot read.
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as [threadLine.IsVirtual]'s mark is,
// for its reason: a boolean where present, and any other type, null among it,
// this accessor's error, never false.
//
// Read off the line's one decode, its raw `isCompactSummary`, never a second
// read of the line.
func (l threadLine) IsCompactSummary() (bool, error) {
	raw := l.facts().isCompactSummary
	if l.Head.Type != recordUser || raw == nil {
		return false, nil
	}
	return wireBool("isCompactSummary", raw)
}

// TaskDelivery reports whether a user record carries `taskDelivery`, a
// background task's result the harness delivers in it, whatever the key
// holds, null among it. False where it is absent, and on every record that is
// no user's.
//
// WHAT THE KEY SAYS, as 2.1.310 and 2.1.322 read it. The request's fold
// renders a user record that carries one, by its truth, as a delivered task's
// round (2.1.322's Fu, 2.1.310's _i): a call and its result of the harness's
// own, spliced after the last assistant message or pushed at the end, which
// can part a reply's message from the results that answer it; and the pass
// that orders attachments before the fold reads the key by its presence
// (`taskDelivery!==void 0`). No thread the probes kept holds one.
//
// ITS READER is [threadLine.IsVirtual]'s: a results record that carries one is
// no plain one. ITS PRESENCE IS THE WHOLE READING, the widest the harness
// makes of the key, so whatever it holds this accessor has no error.
//
// Read off the line's one decode, its raw `taskDelivery`, never a second read
// of the line.
func (l threadLine) TaskDelivery() bool {
	return l.Head.Type == recordUser && l.facts().taskDelivery
}

// BatchToolUses reports whether an assistant record carries `batchToolUses`,
// the calls of a batch its harness split one call of the model's into, and
// hands the `id` of each entry to each, in order. False, and nothing handed,
// where the key is absent, and on every record that is no assistant's.
//
// WHAT THE KEY SAYS, as 2.1.310 and 2.1.322 write and read it. Where a model's
// call to a tool that takes a batch of entries is split into a call an entry,
// the harness writes the reply with the split calls, each id the batch's own
// and a `_<n>`, and lists the batch's own calls here, `{id, name}` each
// (2.1.322's Q0n); its request folds the split calls back into one by this
// list (2.1.322's GWi, 2.1.310's x2s, in the fold of an assistant record),
// and their results the same way by the digits their ids end in (2.1.322's
// zWi). No thread the probes kept holds one.
//
// ITS READER is [threadLine.IsVirtual]'s: a reply that carries one is no plain
// one, and a call an entry names is one the request may fold into another,
// which that reader reads as told.
//
// HELD TO ITS SHAPE HERE, AND HERE ALONE, as a key one reader reads is: a list
// where present, walked an element at a time ([eachElement]), each entry an
// object whose `id` is a string. Anything else, null among it, is this
// accessor's error: the harness's fold matches an entry of no string id to
// calls no batch split, and a list of no shape throws in its request. An
// error ends the walk, and the entries before it have been handed.
//
// Read once, as the line is decoded ([factsOf]), off its raw `batchToolUses`
// there: never a second read of the line, and a reader that asks again is
// handed what that reading held.
func (l threadLine) BatchToolUses(each func(id string)) (bool, error) {
	b := &l.facts().batch
	if l.Head.Type != recordAssistant || !b.present {
		return false, nil
	}
	// REPLAYED, THEN ANSWERED: every entry before one its rule refuses, in
	// order, then that entry's error, as a walk of the list hands them.
	for _, id := range b.ids {
		each(id)
	}
	if b.err != nil {
		return false, fmt.Errorf("batchToolUses: %w", b.err)
	}
	return true, nil
}

// Checkpoint is what a last-prompt record says of the leaf Claude Code's
// resume starts from: leaf, its `leafUuid` where that is a string, "" where
// it is absent, null or the empty string, each of which names no leaf to the
// harness; and marked, whether its `explicit` or its `rewound` is the JSON
// true. Nothing, and no error, on every record of another type.
//
// WHAT THE RECORD SAYS, as 2.1.310 and 2.1.322 write and read it. The harness
// writes one as a session's metadata, naming the record it last wrote, and a
// resume takes the newest one that names a leaf (`if(leafUuid)`), since the
// last compaction, for the leaf it starts from wherever the session's last
// record does not descend from it (2.1.322's Szt and 2.1.310's GMt, the
// last-prompt branch of the load). A MARKED one is written by a rewind
// (`explicit` and `rewound`, naming where the conversation was cut), a
// withdrawal (`explicit`, naming none) and a fork's copy: each makes the
// resume start from it whatever came after, or from nothing at all. All 837
// the probes kept name a leaf, and none is marked.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model, which takes the leaf a record names as the resume does, and reads no
// thread a marked record may have moved; through it, the reading that tells
// a relaunch which of its calls a kept session holds (exact telling) and the
// one that confirms a hook's note reached the model.
//
// HELD TO ITS TYPE HERE, AND HERE ALONE, as a key one reader reads is:
// `leafUuid` a string or null where present. Any other type is this
// accessor's error, never the line's: the harness itself takes any value its
// JavaScript reads as true for a leaf, so a reader that passed a number or an
// object by would keep a leaf the harness had replaced. A mark is the JSON
// true alone, as the harness tests it (`===!0`), whatever else it holds: a
// mark of another value marks nothing.
//
// Read off the line's one decode, its raw `leafUuid`, `explicit` and
// `rewound`, never a second read of the line. The raw value the line's
// decode hands a key is the value as written, but for the whitespace around
// it, so a mark is the bytes `true` exactly.
func (l threadLine) Checkpoint() (leaf string, marked bool, err error) {
	if l.Head.Type != recordLastPrompt {
		return "", false, nil
	}
	f := l.facts()
	marked = bytes.Equal(f.explicit, jsonTrue) || bytes.Equal(f.rewound, jsonTrue)
	if f.leafUUID == nil || bytes.Equal(f.leafUUID, jsonNull) {
		return "", marked, nil
	}
	leaf, err = wireRequiredString("leafUuid", f.leafUUID)
	return leaf, marked, err
}

// jsonTrue is JSON's true, the one value a checkpoint's mark is read as set by
// ([threadLine.Checkpoint]).
var jsonTrue = []byte("true")

// LeafKeyed reports whether a last-prompt record carries `leafUuid` at all,
// whatever it holds, null among it. False where the key is absent, and on
// every record of another type.
//
// WHAT IT SAYS, as 2.1.310 and 2.1.322 read it. Where a session's newest record
// is not its last one, a resume starts from the newest that descends from the
// last, but only where no last-prompt record carries the key with any value
// (2.1.322's Szt, `leafUuid!==void 0`; 2.1.310's GMt the same), so a record
// whose `leafUuid` is null, which names no leaf ([threadLine.Checkpoint]),
// still turns that rule off.
//
// ITS READER is [threadLine.Checkpoint]'s, the model of which records Claude
// Code's resume hands its model, which applies that rule only where no
// record carries the key.
//
// HELD TO [threadLine.Checkpoint]'s TYPE: `leafUuid` a string or null where
// present, and anything else this accessor's error, as it is Checkpoint's, so
// the one key reads alike through both.
func (l threadLine) LeafKeyed() (bool, error) {
	leaf := l.facts().leafUUID
	if l.Head.Type != recordLastPrompt || leaf == nil {
		return false, nil
	}
	if !bytes.Equal(leaf, jsonNull) && leaf[0] != '"' {
		return false, fmt.Errorf("leafUuid is %s, neither a string nor null", wireKind(leaf))
	}
	return true, nil
}

// AttachmentType is an attachment record's `attachment.type`: "" where the
// attachment or its type is absent, and on every record that is no
// attachment.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model, which reads no main thread holding a fork's briefing
// ([attachmentForkBriefing]): the resume takes no such record for the last a
// session wrote, as 2.1.310 and 2.1.322 choose the leaf it starts from
// (2.1.322's jWo, 2.1.310's Ypo).
//
// HELD TO ITS TYPE HERE: `attachment` an object and its `type` a string,
// where present. Any other shape is this accessor's error, never the line's.
// The harness's own test of a fork's briefing (`attachment?.type ===
// "fork_briefing"`) reads every such attachment as none, and so may its
// reader.
//
// Read off the attachment's own keys as written, which the line's one decode
// keeps ([factsOf]), never a second read of the line.
func (l threadLine) AttachmentType() (string, error) {
	a := &l.facts().attachment
	if l.Head.Type != recordAttachment || a.kind == 0 {
		return "", nil
	}
	if a.kind != '{' {
		return "", fmt.Errorf("an attachment record's attachment is %s, not an object", a.kind)
	}
	if a.err != nil {
		return "", fmt.Errorf("an attachment: %w", a.err)
	}
	return wireString("attachment.type", a.typ)
}

// HookEvent is a hook record's event, its attachment's `hookEvent`: "" on
// every other record and where the key is absent; an error where it is no
// string, this accessor's alone, never the attachment's
// ([threadLine.AttachmentWellFormed] reads no event). Read off the
// attachment's own keys as written, which the line's one decode keeps.
func (l threadLine) HookEvent() (string, error) {
	a := &l.facts().attachment
	if l.Head.Type != recordAttachment || a.kind == 0 {
		return "", nil
	}
	if a.kind != '{' {
		return "", fmt.Errorf("an attachment record's attachment is %s, not an object", a.kind)
	}
	if a.err != nil {
		return "", fmt.Errorf("an attachment: %w", a.err)
	}
	return wireString("attachment.hookEvent", a.event)
}

// HookTakes is which of the take tokens its reader asked
// ([threadReader.Asking]) a hook record's words name: content of its
// attachment's `content`, stdout of a hook_success's `stdout`, each where it
// is a string; bit i for token i, each whole and exactly ([textTakes]). Zero
// on every other record, an attachment no hook wrote ([hookRecordPrefix])
// among them. A hook's words leave the layer only so, never as bytes.
func (l threadLine) HookTakes() (content, stdout uint64) {
	a := &l.facts().attachment
	return a.contentTokens, a.stdoutTokens
}

// ReplacedResults hands each, in order, the id of every tool result a
// content-replacement record says its harness hands its model replaced: each
// entry of the record's `replacements` whose `kind` is "tool-result", by its
// `toolUseId`. Nothing, and no error, on every record of another type.
//
// WHAT THE RECORD SAYS. Behind a flag Anthropic's servers set
// (tengu_hawthorn_steeple, off where no server says), Claude Code holds the
// tool results a reply's calls returned to a budget of 200,000 characters a
// message, and past it saves the largest to a file and hands its model a
// preview in each one's place. The result's own record keeps the whole
// output; the replacement is recorded beside it as this record, in the thread
// whose result it replaces (no `agentId` in the main thread's file, the
// subagent's in a subagent's), as 2.1.310 and 2.1.322 write it, and a resume
// hands its model the recorded preview again wherever the flag is on
// (2.1.322's b1i and aho, by each tool-result entry's toolUseId). So a result
// such a record names is one its session's model was not handed whole,
// whatever its own record holds.
//
// ITS READER is the reading that tells a relaunch which of its calls a kept
// session holds (exact telling), which reads every result such a record names
// as not handed whole to the continued session's model, whether or not the
// flag is on there.
//
// HELD TO ITS SHAPES HERE, AND HERE ALONE, as a key one reader reads is:
// `replacements` a list that is there, walked an element at a time
// ([eachElement]); each element an object whose `kind` is a string and,
// where that is "tool-result", whose `toolUseId` is a string. Anything else
// is this accessor's error, never the line's. An entry of another kind is
// passed over, as the harness's resume passes it over, and the preview itself
// is never read.
//
// Read once, as the line is decoded ([factsOf]), off its raw `replacements`
// there: never a second read of the line, and a reader that asks again is
// handed what that reading held.
func (l threadLine) ReplacedResults(each func(id string)) error {
	if l.Head.Type != recordContentReplacement {
		return nil
	}
	// REPLAYED, THEN ANSWERED: every result replaced before an entry its rule
	// refuses, in order, then that entry's error, as a walk of the list hands
	// them.
	r := &l.facts().replacements
	if err := r.list(); err != nil {
		return err
	}
	for _, id := range r.ids {
		each(id)
	}
	return r.err
}

// ReplacementCount is how many entries a content-replacement record's
// `replacements` lists, of every kind and shape. 0, and no error, on every
// record of another type.
//
// WHAT THE LOAD DOES WITH THEM, as 2.1.310 and 2.1.322 read the record (the
// content-replacement branch of 2.1.310's load, GMt, and of 2.1.322's, Szt).
// Each passes the list into one call's arguments whole, to add it to the
// session's (2.1.310's `push(...replacements)`; 2.1.322's the same of the
// entries that are objects, once it has found a list), and a call takes no
// more arguments than its engine allows: past that, the spread throws, and
// with it the load, and the resume fails. And 2.1.310 spreads whatever the
// key holds, which throws where it is no list (absent, null, a number, a
// boolean, an object), where 2.1.322 passes such a record over. No thread the
// probes kept holds such a record.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model, which reads no thread whose load may fail at one.
//
// HELD TO ITS SHAPE HERE, AND HERE ALONE, as a key one reader reads is: a
// list that is there, walked an element at a time ([eachElement]), each
// element counted whatever it is. Anything else, a string among it, is this
// accessor's error, never the line's.
//
// Read once, as the line is decoded ([factsOf]), off its raw `replacements`
// there: never a second read of the line, and a reader that asks again is
// handed what that reading held.
func (l threadLine) ReplacementCount() (int, error) {
	if l.Head.Type != recordContentReplacement {
		return 0, nil
	}
	r := &l.facts().replacements
	if err := r.list(); err != nil {
		return 0, fmt.Errorf("replacements: %w", err)
	}
	return r.count, nil
}

// Retracted hands each, in order and as written, every entry of a record's
// `retractedMessageUuids`, the uuids it says its harness took back out of the
// conversation: on a user or assistant record and on a system record of
// subtype model_refusal_fallback, the records 2.1.322 reads the key on,
// 2.1.310 the last alone. Nothing, and no error, where the key is absent,
// and on every record of another type or subtype, whatever it carries.
//
// WHAT THE RECORDS SAY, as 2.1.310 and 2.1.322 write and read them. Where a
// model's reply is refused as it streams and the harness moves the request to
// another model, or it retries a reply whose call it could not parse, it
// takes the records that attempt wrote back out of the conversation, and
// names them: a refusal's fallback on a system record of its own, a retry on
// the meta user record it sends in their place, a reply it gives up with on
// itself, naming none. THE SESSION'S OWN RESUME, once it has walked the
// chain, drops every record but a system one whose uuid's first 24
// characters are those of an entry any such record names, wherever on the
// chain or beside it the record lies (2.1.322's eGe and 2.1.310's qGn, each
// cutting an entry at 24 characters, cE and Gb): the first 24, since the
// harness derives the uuids of the records it splits one message into from
// that message's own, alike in those (2.1.322's cMn). So an entry of 24
// characters or more drops by its first 24 alone, and one of fewer is no
// uuid's first 24 and drops nothing; this accessor hands each as written,
// whatever its length, for its reader to cut.
//
// NO SUBAGENT'S LOAD OR CONTINUATION RUNS THAT FILTER: a subagent's own load
// hands its conversation as it walks it (2.1.322's Rye, 2.1.310's FBe), and a
// SendMessage continuation runs its request's passes on that with no
// retraction filter among them, so what a subagent's own records name is
// handed on there. No thread the probes kept holds such a list.
//
// ITS READERS: the model of which records Claude Code's resume hands its
// model, which drops what a main thread's records name as each release does,
// and the reading that tells a relaunch which of its calls a kept session
// holds (exact telling), which reads a subagent's thread holding such a list
// as not whole, since what the subagent's model was handed there is not what
// its file holds.
//
// WHAT IT HANDS ITS READERS: how many entries the list holds, and the first 24
// characters of each entry that begins with a canonical uuid's ([chainPrefix]),
// in order: what each reader reads of it, the resume's model the prefixes and
// the reading of a subagent's thread whether it lists any entry. An entry of
// fewer characters, or that begins otherwise, is counted and names no prefix,
// since every record the model files carries a lowercase canonical uuid. AT
// MOST [threadChainMaxRecords] PREFIXES ARE HELD: past them the list says so
// ([threadRetracted.Prefixes]) and holds no more, still counting and checking
// every entry after them. No reader reads past them, since the model refuses a
// list past the room its records leave, which is below that bound.
//
// HELD TO ITS SHAPE HERE, AND HERE ALONE, as a key one reader reads is: a
// list of strings where present, walked an element at a time ([eachElement]).
// Anything else, null and an entry that is no string among it, is this
// accessor's error, never the line's, handed with what the list held before
// it: the entries before an entry that is no string, counted and their
// prefixes held, as a reader that walks the list would have read them first.
// No release's resume throws on such a list: each one's load repairs a
// refusal's fallback before its filter reads it, keeping a list's strings and
// dropping a value that is no list (2.1.322's UWt @293630140 through M2
// @293633304, 2.1.310's cPt @247026592 through m1 @247029756), and 2.1.322's
// filter reads a user's or an assistant's list for its strings alone, and a
// value that is no list as none (eGe @295286851).
// The error is conservative: its readers refuse a list the harness wrote in
// no shape it writes rather than follow it through those repairs, and no
// thread the probes kept holds one.
//
// Read once, as the line is decoded ([factsOf]), off its raw
// `retractedMessageUuids` there: never a second read of the line, and a
// reader that asks again is handed what that reading held.
func (l threadLine) Retracted() (threadRetracted, error) {
	switch {
	case l.Head.Type == recordUser, l.Head.Type == recordAssistant:
	case l.Head.Type == recordSystem && l.Head.Subtype == subtypeRefusalFallback:
	default:
		return threadRetracted{}, nil
	}
	f := l.facts()
	return f.retracted, f.retractedErr
}

// threadRetracted is what a record's retraction list names
// ([threadLine.Retracted]).
type threadRetracted struct {
	// Entries is how many entries the list holds, each string counted
	// whatever it names, up to an entry that is no string.
	Entries int
	// prefixes is the canonical prefix of each entry that begins with one
	// ([chainPrefix]), in order, at most [threadChainMaxRecords]; more
	// whether an entry past those began with one too.
	prefixes [][chainPrefixBytes]byte
	more     bool
}

// Prefixes is the first 24 characters of each entry that begins with a
// canonical uuid's, as [chainPrefix] reads them, in order; all false where the
// list names more than [threadChainMaxRecords] such, of which held is the
// first [threadChainMaxRecords].
func (r threadRetracted) Prefixes() (held [][chainPrefixBytes]byte, all bool) {
	return r.prefixes, !r.more
}

// entry reads the list's next entry, a string, by what [chainPrefix] reads of
// it, prefix and canonical: counted, and its prefix held where it is
// canonical, while fewer than [threadChainMaxRecords] are held
// ([threadLine.Retracted]).
func (r *threadRetracted) entry(p [chainPrefixBytes]byte, canonical bool) {
	r.Entries++
	if canonical {
		if len(r.prefixes) == threadChainMaxRecords {
			r.more = true
		} else {
			r.prefixes = append(r.prefixes, p)
		}
	}
}

// replacedID is the rule of an entry of a content-replacement record's list
// ([threadLine.ReplacedResults]), kind the entry's kind and entryKind and
// toolUseID its `kind` and `toolUseId` as written: an object whose `kind` is
// a string and, where that is "tool-result", whose `toolUseId` is a string,
// the id of the result it replaces; replaced false for an entry of another
// kind, which no reader reads.
func replacedID(kind valueKind, entryKind, toolUseID json.RawMessage) (id string, replaced bool, err error) {
	if kind != '{' {
		return "", false, fmt.Errorf("a replacement is %s, not an object", kind)
	}
	k, err := wireRequiredString("replacements.kind", entryKind)
	if err != nil || k != replacementToolResult {
		return "", false, err
	}
	if id, err = wireRequiredString("replacements.toolUseId", toolUseID); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// batchEntryID is the rule of an entry of an assistant record's
// `batchToolUses` ([threadLine.BatchToolUses]), kind the entry's kind and id
// its `id` as written: an object whose `id` is a string.
func batchEntryID(kind valueKind, id json.RawMessage) (string, error) {
	if kind != '{' {
		return "", fmt.Errorf("an entry is %s, not an object", kind)
	}
	return wireRequiredString("id", id)
}

// AttachmentWellFormed reports whether Claude Code's resume keeps an
// attachment record past its drop of the attachments whose payload is not of
// their type's shape (2.1.322's Fce through SCr, 2.1.310's dne through cJo,
// the same rules and bounds), which both releases apply to a resumed main
// thread's records right after their retraction filter. True on every record
// that is no attachment, as the drop passes it.
//
// THE RULES, the payload being the record's `attachment`, and an object
// meaning a JSON object, never null or a list (2.1.322's D, 2.1.310's H):
// the payload an object whose `type` is a string, or the drop takes it; then
// by that type:
//
//   - invoked_skills: `skills` a list whose every element is an object or a
//     list;
//   - hook_success: `content` a string;
//   - skill_listing: `names` absent, or a list of at most 4,096 strings, each
//     at most 512 UTF-16 code units long, as JavaScript counts a string;
//   - hook_additional_context: `content` a list of strings;
//   - task_reminder and todo_reminder: `content` a list of objects;
//   - file and already_read_file: `content` an object;
//   - withheld_memory: `by` a string that is not empty, and `entries` a list
//     of objects, each with a string `path`, a string `displayPath` and a
//     `why` that is one of denied, outside, unjudged and unsettled, the own
//     keys of the reasons the harness names (2.1.322's vY, 2.1.310's kq);
//   - every other type: kept.
//
// Every one of the 7,519 attachments of the 521 thread files the probes kept
// is kept by them.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model, which hands no attachment the drop takes; through it, the one that
// confirms a hook's note reached the model reads a hook's success or
// additional context the drop took as handed to no model, by no rule of its
// own.
//
// AN ERROR WHERE THE LAYER CANNOT COUNT AS THE HARNESS DOES: a skill's name
// past 512 units as the layer's decode reads it, holding a byte that is no
// UTF-8. The decode reads each such byte as one U+FFFD, where the harness's
// read of its file can read several as one, so the name may be under the
// bound to the harness, and no reader can say whether the record is kept.
// Read the same way, a name the layer counts at or under the bound is under
// it to the harness too.
//
// Read once, as the line is decoded ([factsOf]), each list walked an element
// at a time ([eachElement]), and every rule's answer kept, so the accessor
// picks by the attachment's type: never a second read of the line.
func (l threadLine) AttachmentWellFormed() (bool, error) {
	if l.Head.Type != recordAttachment {
		return true, nil
	}
	a := &l.facts().attachment
	if a.kind != '{' {
		return false, nil
	}
	if a.err != nil {
		return false, fmt.Errorf("an attachment: %w", a.err)
	}
	// A payload of no type, or of one that is no string, is one the drop
	// takes: an answer, never an error. A string the decode cannot read is the
	// layer's own failure, and its error.
	if kindOfValue(a.typ) != '"' {
		return false, nil
	}
	kind, err := wireRequiredString("type", a.typ)
	if err != nil {
		return false, fmt.Errorf("an attachment: %w", err)
	}
	switch kind {
	case attachmentInvokedSkills:
		return a.skillsShaped, nil
	case hookSuccess:
		return a.content == '"', nil
	case attachmentSkillListing:
		return a.namesKept, a.namesErr
	case hookAdditionalContext:
		return a.contentStrings, nil
	case attachmentTaskReminder, attachmentTodoReminder:
		return a.contentObjects, nil
	case attachmentFile, attachmentAlreadyRead:
		return a.content == '{', nil
	case attachmentWithheldMemory:
		by, byErr := wireRequiredString("by", a.by)
		return byErr == nil && by != "" && a.entriesKept, nil
	}
	return true, nil
}

// withheldKept reports whether an entry of a withheld_memory attachment is of
// the shape Claude Code's resume keeps the attachment with
// ([threadLine.AttachmentWellFormed]), kind the entry's kind, path and
// displayPath the kinds of its `path` and `displayPath`, why its `why` as
// written: an object whose `path` and `displayPath` are strings and whose
// `why` is one of "denied", "outside", "unjudged" and "unsettled".
func withheldKept(kind, path valueKind, why json.RawMessage, displayPath valueKind) bool {
	if kind != '{' || path != '"' || displayPath != '"' {
		return false
	}
	w, err := wireRequiredString("why", why)
	if err != nil {
		return false
	}
	switch w {
	case "denied", "outside", "unjudged", "unsettled":
		return true
	}
	return false
}

// skillNameKept is the rule of one of a skill listing's names
// ([skillNamesKept]), units its length in UTF-16 code units as the layer's
// decode reads it and valid whether its bytes as written are UTF-8: kept
// where it is at most [skillListingMaxNameUnits] units long; and kept but
// unreadable where it is longer and holds a byte that is no UTF-8, which the
// decode reads as one U+FFFD where the harness's read of its file can read
// several as one, so no reader can say whether it is under the bound.
func skillNameKept(units int, valid bool) (kept, unreadable bool) {
	switch {
	case units <= skillListingMaxNameUnits:
		return true, false
	case !valid:
		return true, true
	}
	return false, false
}

// errSkillNameUnmeasured is a skill listing whose names are kept but for one
// past [skillListingMaxNameUnits] that holds a byte that is no UTF-8
// ([skillNameKept]): an answer the layer cannot give
// ([threadLine.AttachmentWellFormed]).
var errSkillNameUnmeasured = errors.New("a skill's name holds a byte that is no UTF-8, so its length in UTF-16 " +
	"code units is not one the layer can count as Claude Code does")

// threadPreserved is what a compaction's boundary says it kept of the thread
// before it ([threadLine.Preserved]).
type threadPreserved struct {
	// Named is whether the boundary names what it kept at all: a list, even
	// one of none, or a segment. A resume acts on every boundary that does,
	// the newest of them: it relinks the thread by the records one names, and
	// where it names none, or is not the last boundary, it drops every record
	// written before the last boundary all the same (2.1.322's iWo, 2.1.310's
	// ypo, each taking the boundary whose `preservedMessages` or
	// `preservedSegment` JavaScript reads as true). So a list of none, whose
	// anchor may be empty too, is never read as a boundary that names
	// nothing.
	Named bool
	// Kept is whether it says it kept records, and Anchor names the record
	// they hang from. Segment is whether it names them as a segment, from
	// Tail up to Head by parent; else it lists them.
	Kept       bool
	Anchor     string
	Segment    bool
	Head, Tail string
	// Listed is how many entries its list holds, each counted whether it is
	// a canonical uuid or not; none for a segment. UUIDs is each entry that
	// is a canonical uuid ([uuidBytes]), in order, at most
	// [threadChainMaxRecords] of them, More whether an entry past those was
	// one too; and Nowhere whether an entry is none, a record no chain the
	// model files carries.
	Listed  int
	UUIDs   [][16]byte
	More    bool
	Nowhere bool
}

// list reads the list's next entry, a string, by what [uuidBytes] reads of
// it, u and canonical: counted; held where it is a canonical uuid, while
// fewer than [threadChainMaxRecords] are held, and More set where one is past
// them; Nowhere set where it is none ([threadLine.Preserved]).
func (p *threadPreserved) list(u [16]byte, canonical bool) {
	p.Listed++
	switch {
	case !canonical:
		p.Nowhere = true
	case len(p.UUIDs) == threadChainMaxRecords:
		p.More = true
	default:
		p.UUIDs = append(p.UUIDs, u)
	}
}

// Preserved is what a compact_boundary record says its compaction kept of the
// thread before it, each record it lists by uuid, in order. Nothing, and no
// error, where the record carries no `compactMetadata`, or null, or keeps
// nothing, and on every record of another type or subtype.
//
// WHAT THE RECORD SAYS, as 2.1.310 and 2.1.322 write and read it. A
// compaction that kept the last stretch of the conversation before it names
// that stretch on its boundary: `preservedMessages`, the kept records' uuids
// in order and the record they hang from (`anchorUuid`, the summary after the
// boundary), and `preservedSegment`, the stretch's two ends and the same
// anchor. A resume reads the newest boundary that names either, the list
// where there is one and else the segment, walked from its tail up by parent
// to its head (2.1.322's iWo and aWo, 2.1.310's ypo and _po); and where that
// is the last boundary and every record it names is in the thread, it
// RELINKS the thread: it chains the kept records after the anchor, hangs the
// anchor's other children from the last of them, and drops every other
// record before that boundary. A boundary that names either acts though it
// keeps nothing: where it lists none, or is not the last boundary, the resume
// relinks nothing and drops every record before the last boundary all the
// same ([threadPreserved.Named]). Each of the 22 boundaries the probes kept
// names both, alike.
//
// ITS READER is the model of which records Claude Code's resume hands its
// model, which reads what a relink moves under the records it walks, and
// where a relinked resume may start; through it, the reading that tells a
// relaunch which of its calls a kept session holds (exact telling) and the
// one that confirms a hook's note reached the model.
//
// HELD TO ITS SHAPES HERE, AND HERE ALONE, as a key one reader reads is:
// `compactMetadata` an object; its `preservedMessages`, where present and not
// null, an object of a string `anchorUuid` and a list `uuids` of strings,
// walked an element at a time ([eachElement]); else its `preservedSegment`,
// where present and not null, an object of the strings `headUuid`,
// `anchorUuid` and `tailUuid`. Anything else is this accessor's error, never
// the line's. No such shape makes a resume throw: each release's load
// repairs a boundary's metadata before its relink reads it, putting an object
// in place of a value that is none and dropping a `preservedMessages` that is
// no object holding a list of `uuids` (2.1.322's pjo @293631807 with mjo
// @293632499, 2.1.310's xfo @247028259 with Afo @247028951), and the relink
// reads a segment of any shape without throwing (2.1.322's aWo @293744710,
// 2.1.310's _po @247129357). The error is conservative: its readers refuse a
// boundary the harness wrote in no shape it writes rather than follow it
// through those repairs, and each boundary the probes kept is of its shape.
// AN ERROR IS HANDED WITH WHAT THE LIST HELD BEFORE IT, in the order the
// shapes are checked (the metadata's kind, the list's, its anchor, its
// `uuids`' kind, then each entry): the entries before the first that is no
// string, counted and their uuids held, as a reader that walks the list reads
// them first, and none where the error comes before the list. AT MOST
// [threadChainMaxRecords] UUIDS ARE HELD: past them More says so and no more
// is held, every entry after still counted and checked. No reader reads past
// them, since the model refuses a list past the room its records leave, which
// is below that bound.
//
// Read once, as the line is decoded ([factsOf]), off its raw `compactMetadata`
// there: never a second read of the line, and a reader that asks again is
// handed what that reading held.
func (l threadLine) Preserved() (threadPreserved, error) {
	if l.Head.Type != recordSystem || l.Head.Subtype != subtypeCompactBoundary {
		return threadPreserved{}, nil
	}
	c := &l.facts().compacted
	switch {
	case c.kind == 0 || c.kind == 'n':
		return threadPreserved{}, nil
	case c.kind != '{':
		return threadPreserved{}, fmt.Errorf("compactMetadata is %s, not an object", c.kind)
	case c.err != nil:
		return threadPreserved{}, fmt.Errorf("compactMetadata: %w", c.err)
	case c.messages != 0 && c.messages != 'n':
		return c.list, c.listErr
	case c.segment != nil && !bytes.Equal(c.segment, jsonNull):
		return preservedSegment(c.segment)
	}
	return threadPreserved{}, nil
}

// preservedSegment is what a compaction's `preservedSegment`, raw, says it
// kept ([threadLine.Preserved]): the segment from its tail up by parent to
// its head, which a resume relinks wherever that walk reaches its head.
func preservedSegment(raw json.RawMessage) (threadPreserved, error) {
	if !wireObject(raw) {
		return threadPreserved{}, fmt.Errorf("compactMetadata.preservedSegment is %s, not an object", wireKind(raw))
	}
	var segment struct {
		Head   json.RawMessage `json:"headUuid"`
		Anchor json.RawMessage `json:"anchorUuid"`
		Tail   json.RawMessage `json:"tailUuid"`
	}
	if err := wireDecode(raw, &segment); err != nil {
		return threadPreserved{}, fmt.Errorf("compactMetadata.preservedSegment: %w", err)
	}
	p := threadPreserved{Named: true, Kept: true, Segment: true}
	var err error
	if p.Head, err = wireRequiredString("preservedSegment.headUuid", segment.Head); err != nil {
		return threadPreserved{}, err
	}
	if p.Anchor, err = wireRequiredString("preservedSegment.anchorUuid", segment.Anchor); err != nil {
		return threadPreserved{}, err
	}
	if p.Tail, err = wireRequiredString("preservedSegment.tailUuid", segment.Tail); err != nil {
		return threadPreserved{}, err
	}
	return p, nil
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
// alone with no sign, fraction or exponent, at most 2^53 - 1, the most
// JSON.parse reads exactly ([wireCount], [wireCountMax]). So the four counts
// sum below 2^55, and no int64 sum of them wraps, nor one of them and a whole
// thread's bytes. Anything else is this accessor's error and never Message's,
// so a reshaped usage costs only the reader that asks. A null usage, and a
// null count, are errors too, never no usage or 0, though Claude Code itself
// reads them so: no release measured writes either (every one of 1,891
// replies measured carries a string id and model and a usage of four whole
// counts), and a figure refused costs its reader a reading, the direction in
// which it tells, where one read as 0 would anchor it on a count nobody
// measured.
//
// THE COUNTS CLAUDE CODE COUNTS THE REPLY BY, OR AN ERROR. Usage hands the
// usage's own four counts, and Claude Code counts a reply by them but where
// the usage lists `iterations`: there it counts the list's last entry that is
// no advisor's or compaction's in their place, where that entry is whole
// ([usageCounted] has the rule). So Usage reads the list, an entry at a time,
// and is an error where the entry Claude Code counts holds other counts than
// the usage's own: a reader that counts what Usage hands counts what the
// harness counts, or has no reading. A FORWARD GUARD, in the direction it
// tells: every reply measured that lists iterations lists none (1,865 of
// 1,865, and 26 more carry null), so it refuses nothing measured, but 2.1.322
// ships a server-side advisor, turned on by a setting, whose replies list an
// advisor_message entry beside the reply's own, and read by its usage alone,
// a reply whose counted entry holds more would read short, and a session its
// harness compacts at its first request would read as one that holds its
// history.
//
// ITS READERS are the resumed count, which reads it on every main-thread
// reply, to bound what a continued session's harness counts before its first
// request, as Claude Code counts it to judge whether to compact there: from
// the newest reply whose usage counts, its four counts' sum, OUTPUT among
// them, placed at the first record of that reply's ID, which Claude Code
// takes off a record that carries no usage too, so ID and Model come with
// every reply, usage or not; and the launch floor's figure at the take's
// meter ([contextReading]), the input side of the last main-thread reply
// whose usage it reads with no error. Which reply counts, and what
// "<synthetic>" or an empty ID means, are each reader's to judge, never this
// accessor's.
//
// Read off the message's own keys as written, which the line's one decode
// keeps ([factsOf]), never a second read of the line.
func (l threadLine) Usage() (threadUsage, bool, error) {
	if l.Head.Type != recordAssistant {
		return threadUsage{}, false, nil
	}
	// HELD TO AN OBJECT BEFORE IT IS READ: a null holds no key at all, and
	// would read as a reply that names no usage.
	m := &l.facts().message
	if err := m.object("an assistant record"); err != nil {
		return threadUsage{}, false, err
	}
	var (
		u   threadUsage
		err error
	)
	if u.ID, err = wireString("id", m.id); err != nil {
		return threadUsage{}, false, err
	}
	if u.Model, err = wireString("model", m.model); err != nil {
		return threadUsage{}, false, err
	}
	if m.usage == nil {
		return u, false, nil
	}
	if err = readUsage(m.usage, &u); err != nil {
		return threadUsage{}, false, err
	}
	return u, true, nil
}

// readUsage reads usage, a message's `usage` that is there, into u's four
// counts, by the rule [threadLine.Usage] holds a reply's usage to: an object,
// each count a whole number in digits alone at most 2^53 - 1 where present and
// 0 where absent ([wireCount]), and its `iterations` no entry Claude Code
// counts in the usage's place with other counts ([usageCounted]). Anything else
// is an error, null among it, and u's counts are then not to be read.
//
// ONE RULE FOR A REPLY'S USAGE, IN A THREAD AND IN A RUN'S STREAM: a thread's
// reply ([threadLine.Usage]), which the resumed count and the launch floor's
// figure ([contextReading]) read alike, and a run's event stream, whose
// assistant lines carry the same message the thread records
// ([claudeEvents.measure]), so the figure a collection reads off the stream
// for the copy its box holds is read by the rule the thread's readers read it
// by, never by a rule of its own.
//
// ONE OTHER READER OF A USAGE KEEPS ITS OWN READING, for what its figure is.
// The run's spend ([looseUsage]) reads a count of another kind as none and the
// rest as written: it is what the run is charged, the share of the seat's
// windows its requests took, and a count it cannot read costs the charge that
// count, never the line's whole spend.
func readUsage(usage json.RawMessage, u *threadUsage) error {
	if !wireObject(usage) {
		return fmt.Errorf("usage is %s, not an object", wireKind(usage))
	}
	var counts struct {
		Input      json.RawMessage `json:"input_tokens"`
		CacheWrite json.RawMessage `json:"cache_creation_input_tokens"`
		CacheRead  json.RawMessage `json:"cache_read_input_tokens"`
		Output     json.RawMessage `json:"output_tokens"`
		Iterations json.RawMessage `json:"iterations"`
	}
	if err := wireDecode(usage, &counts); err != nil {
		return fmt.Errorf("usage: %w", err)
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
		var err error
		if *c.to, err = wireCount(c.key, c.raw); err != nil {
			return err
		}
	}
	return usageCounted(*u, counts.Iterations)
}

// The kinds of entry a usage's `iterations` lists that [usageCounted] reads:
// those Claude Code passes over to the entry it counts, and those it counts.
const (
	iterationAdvisor    = "advisor_message"
	iterationCompaction = "compaction"
	iterationMessage    = "message"
	iterationFallback   = "fallback_message"
)

// usageCounted is nil where Claude Code counts a reply by u, its usage's own
// four counts, and an error where it counts an entry of the usage's
// `iterations`, raw, whose counts are other than u's ([threadLine.Usage]).
//
// CLAUDE CODE'S RULE, read in the shipped code of 2.1.310 and 2.1.322, the
// one each anchors its count on: where the usage's input side is above zero
// and its iterations a list, the list's LAST entry that is no object of type
// advisor_message or compaction is counted in the usage's place, where it is
// a WHOLE entry (an object of type message or fallback_message, each of its
// four counts a finite number at or above zero, its input side above zero).
// Anywhere else the usage's own counts are: an input side of zero, iterations
// absent, null or no list, an empty list, one with no such entry, and a last
// such entry that is not whole.
//
// AN ENTRY AT A TIME ([eachElement]), never the whole list first and never
// collected: what it keeps of the list is a copy of one entry, the last it
// may count, made into the room the one before it left, and an object's type
// is read and let go of as the walk meets it. So a list costs what its
// entries' types cost one at a time, never a slice of them, and a list of
// entries that are no objects costs no allocation more than a list of one.
func usageCounted(u threadUsage, iterations json.RawMessage) error {
	if u.Input+u.CacheWrite+u.CacheRead == 0 || len(iterations) == 0 || iterations[0] != '[' {
		return nil
	}
	var last []byte
	if err := eachElement(iterations, func(entry json.RawMessage, _ int) error {
		passed, err := iterationPassedOver(entry)
		if err == nil && !passed {
			last = append(last[:0], entry...)
		}
		return err
	}); err != nil {
		return fmt.Errorf("usage.iterations: %w", err)
	}
	counted, whole := wholeIteration(last)
	if !whole {
		return nil
	}
	if own := [...]float64{float64(u.Input), float64(u.CacheWrite), float64(u.CacheRead),
		float64(u.Output)}; counted != own {
		return fmt.Errorf("usage.iterations: Claude Code counts an entry of %g input, %g cache write, %g cache read "+
			"and %g output tokens in place of the usage's own %d, %d, %d and %d", counted[0], counted[1], counted[2],
			counted[3], u.Input, u.CacheWrite, u.CacheRead, u.Output)
	}
	return nil
}

// iterationPassedOver reports whether entry, one of a usage's iterations, is
// one Claude Code passes over to the entry it counts: an object whose type is
// advisor_message or compaction ([usageCounted]).
func iterationPassedOver(entry json.RawMessage) (bool, error) {
	if !wireObject(entry) {
		return false, nil
	}
	var it struct {
		Type json.RawMessage `json:"type"`
	}
	if err := wireDecode(entry, &it); err != nil {
		return false, err
	}
	if len(it.Type) == 0 || it.Type[0] != '"' {
		return false, nil
	}
	var kind string
	if err := wireDecode(it.Type, &kind); err != nil {
		return false, err
	}
	return kind == iterationAdvisor || kind == iterationCompaction, nil
}

// wholeIteration is the four counts of entry, one of a usage's iterations, in
// the order [threadUsage] holds them, and whether it is whole: an object of
// type message or fallback_message, each of its four counts a finite number
// at or above zero, and its input side above zero, as Claude Code requires of
// the entry it counts ([usageCounted]). A number past a double's range, which
// JSON.parse reads as Infinity, is no finite one.
func wholeIteration(entry []byte) ([4]float64, bool) {
	if !wireObject(entry) {
		return [4]float64{}, false
	}
	var it struct {
		Type       json.RawMessage `json:"type"`
		Input      json.RawMessage `json:"input_tokens"`
		CacheWrite json.RawMessage `json:"cache_creation_input_tokens"`
		CacheRead  json.RawMessage `json:"cache_read_input_tokens"`
		Output     json.RawMessage `json:"output_tokens"`
	}
	if wireDecode(entry, &it) != nil {
		return [4]float64{}, false
	}
	if kind, err := wireRequiredString("type", it.Type); err != nil ||
		(kind != iterationMessage && kind != iterationFallback) {
		return [4]float64{}, false
	}
	var counts [4]float64
	for i, raw := range [...]json.RawMessage{it.Input, it.CacheWrite, it.CacheRead, it.Output} {
		n, err := wireNumber("", raw)
		if err != nil || n < 0 || math.IsInf(n, 0) {
			return [4]float64{}, false
		}
		counts[i] = n
	}
	return counts, counts[0]+counts[1]+counts[2] > 0
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
// user turn of its own, which the edit never clears. The resumed count, which
// bounds what a continued session's harness counts before its first request,
// reads it on every main-thread reply, and takes a reply that says so, or
// that this accessor cannot read, for a session that may compact there: in a
// cleared state Claude Code counts the cleared tokens back into the count it
// compacts by, which no line of the thread states.
//
// Read off the message's own keys as written, which the line's one decode
// keeps ([factsOf]), never a second read of the line.
func (l threadLine) ToolResultsCleared() (bool, error) {
	if l.Head.Type != recordAssistant {
		return false, nil
	}
	// HELD TO AN OBJECT BEFORE IT IS READ: a null holds no key at all, and
	// would read as a message that reports no edit.
	m := &l.facts().message
	if err := m.object("an assistant record"); err != nil {
		return false, err
	}
	report := m.contextManagement
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
	if err := eachElement(applied.Edits, func(raw json.RawMessage, _ int) error {
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
// rooting a chain (below) that is not the thread's first chained record; or
// the thread's first chained record where it roots no chain.
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
// A ROOT IS WHAT CLAUDE CODE'S WALK READS AS ONE. Its resume loads every user,
// assistant, attachment and system record of a thread into the one map it
// walks, whatever `parentUuid` the record carries (2.1.310's and 2.1.322's
// test of a record is its type alone), and walks a chain back from a record,
// parent by parent, to the first that is parentless ([threadHead.Parentless]):
// its `parentUuid` null, the empty string or absent, each falsy to it alike.
// So such a record is in a chain ([threadHead.Chained]), and roots one,
// whichever of the three it
// carries, and what lies before it is in no chain a resume walks through it. A
// record of another type is the harness's bookkeeping, in no chain, where it
// carries no `parentUuid` (every kind measured, at [threadHead.Parent]); one
// that carries one is read by it as written, null and the empty string a
// root. No release measured writes a user, assistant, attachment or system
// record with no `parentUuid` or an empty one (none of the 11,727 such records
// of the 521 thread files the probes kept), so the rule moves nothing
// measured; a later release, or a box that rewrote its file, that writes one
// restarts the thread there, where read by the literal null alone, what lies
// before it would be counted as handed on.
//
// ITS READERS: the resumed count, which bounds what a continued session's
// harness counts before its first request and reads a restart as where the
// job may have started its context over; the reading that tells a relaunch
// which of its calls a kept session holds (exact telling), which counts a
// result, a subagent's answer and its notice only past the main thread's last
// restart, and reads a subagent's thread that restarts anywhere as not whole;
// and the one that confirms a hook's note reached the model, which holds a
// note only where no restart follows it. The model of which records Claude
// Code's resume hands its model reads a root by the same rule, through
// [threadHead.Parentless].
//
// A thread with no chained record restarts nowhere: it holds no chain to
// start over, and what that means is its reader's to say.
func (r *threadRestarts) See(h threadHead) bool {
	chained := h.Chained()
	root := chained && h.Parentless()
	first := chained && !r.chained
	r.chained = r.chained || chained
	switch {
	case h.Subtype == subtypeCompactBoundary:
		return true
	case h.Type == recordSystem && h.Subtype == subtypeMicrocompactBoundary:
		return true
	case h.ReplacesSpan:
		return true
	case first:
		return !root
	}
	return root
}
