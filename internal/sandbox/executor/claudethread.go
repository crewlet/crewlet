package executor

import (
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
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CLAUDE CODE THREAD, READ FOR WHAT IT RECORDS: every reader that decides
// something from what a session recorded reads its threads through this one
// layer, by rules held once. The rules, and why each: the package doc, "A
// Claude Code thread is read a line at a time, holding what its readers
// read", and "A thread line is read in one scanner pass, never held".

// threadLineMax is the most the layer allocates for one line's facts, in sum
// ([lineMeter]): raw values kept for accessors, strings, and each block's and
// list entry's facts, charged as allocated. PAST IT THE LINE FAILS CLOSED
// ([errThreadBound]). What no reader reads (base64 of an image or PDF, a
// tool's input but its target, a prompt, a bookkeeping payload) is passed over
// unheld, and a value read whole is also held to [threadValueMax]. Eight MiB, a
// constant of its own since the scrub bounds no line: the package doc, "THE
// FACTS ARE HELD TO [threadLineMax] IN SUM", says why.
const threadLineMax = 8 << 20

// threadPlainMax is the most of one member a reader reads: what a kept
// session's scrub reads ([scrubPlainMax], the figure [heldPlainMax] packs to),
// so a thread past it is one no kept session holds. A reader of an archive meters
// the archive at it and hands each member what is left.
const threadPlainMax = scrubPlainMax

// threadLinesMax is the most whole lines a reading reads, of one thread or of
// every thread member of an archive between them. PAST IT THE READING FAILS
// CLOSED ([errThreadBound]): a line costs microseconds whatever its length, so
// [threadPlainMax] alone lets tiny lines read for minutes. 2^20 is 126 times
// the most lines any measured thread holds (8,308); its slowest shape reads in
// at most 22.6 s on two cores, inside the sandbox's 30 s holdings wait. The
// package doc, "THE FACTS ARE HELD TO [threadLineMax] IN SUM", has the rest.
const threadLinesMax = 1 << 20

// errThreadBound is a thread past a bound it is read within: a line's facts
// past [threadLineMax], a value read whole past [threadValueMax], a type that
// turns after a frame the layer passed over, or a member past its reader's
// bound. The reading is over ([threadReader.Next]).
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
// BY EXACT NAME, never [claudeThread]'s reading: a transcript takes ANY
// `projects/crewlet/<x>.jsonl` for the main thread, which would make a reader
// of one session read another's file as its own.
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
// ONE JSON DOCUMENT WITH NO LINE BREAK, NEVER A THREAD: through [threadReader]
// it is no line at all, only a torn piece and io.EOF, so its reader decodes it
// whole, within a bound of its own, under [threadDecoding].
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
// from, in Claude Code's order: `projects/crewlet/<session>/precompact.json`
// (the one it writes) and `projects/crewlet/<session>.precompact.json` (read
// only where the first is absent). False for an invalid session id. Claude Code
// may compute a compaction ahead of its threshold; a resume swaps it in (up to
// seven days old) for a fresh summary, so a session holding one may be handed a
// summary at its first request in place of its history (no run measured wrote
// one). A box's pack carries the first. ONE KIND FOR BOTH ([memberPrecompact]):
// readers ask only whether the session holds one, so none may miss a name. Its
// reader is the resumed count; presence is the fact, never what it says.
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
	// Agent is the subagent's id; "" otherwise.
	Agent string
}

// claudeMemberOf is which member of session the archive member named name is:
// the inverse of [claudeMainThread], [claudeSubagentThread],
// [claudeSubagentMeta] and [claudePrecompact], true exactly where one names it;
// every other name (another session's file, a deeper path, a neighbour such as
// `precompact.json.tmp`, `agent-.jsonl`) is false. THE NAME AS THE CALLER
// CLEANED IT (path.Clean), compared whole: only the caller sees a member held
// twice under two spellings. The caller gives NO READING AT ALL of an archive
// with an absolute or `..` member name: tar and busybox restore those over the
// main thread, so the member answered for need not be the file a resume holds.
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
// Sixty-four: 2.1.310 names a subagent `a` plus sixteen hex digits; the rest is
// room for a longer id.
const claudeAgentMaxBytes = 64

// validClaudeAgent reports whether agent is a subagent id a member is named
// by: 1 to [claudeAgentMaxBytes] bytes of letters, digits, `-` and `_`, so a
// slash, a dot or a NUL never names a path.
//
// ONE RULE FOR BOTH DIRECTIONS ([claudeMemberOf] too), and never a call id's
// rule: the two ids may move apart.
func validClaudeAgent(agent string) bool {
	return agent != "" && len(agent) <= claudeAgentMaxBytes &&
		!strings.ContainsFunc(agent, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
		})
}

// maxAskedTakes is the most take tokens one reading asks
// ([threadReader.Asking]): a hook record's masks' width, 64 bits.
const maxAskedTakes = 64

// The most takes a job's row asks ([sandbox.MainThreadAsk.Takes]): every
// serve of each note, late call's item and wake item it holds. A bound raised
// past the masks fails this build, never a job's read.
var _ [maxAskedTakes - (steer.MaxTurnNotes+sandbox.MaxJobItems)*sandbox.MaxNoteServes -
	sandbox.MaxJobWakes*sandbox.MaxWakeServes]struct{}

// errAskedPastMasks is an ask of more take tokens than a hook record's masks
// hold ([threadReader.Asking]).
var errAskedPastMasks = errors.New("executor: more take tokens are asked than a hook record's masks hold")

// threadLineError is a whole line that is not one record: not one JSON
// object, or a head key of the wrong type ([threadHead]).
type threadLineError struct {
	// N is the line's place among the member's whole lines, from 1.
	N int
	// Err is what the decode met.
	Err error
	// Fault is how Claude Code's load reads the line ([lineFault]).
	Fault lineFault
	// Type is the record kind where the line is one object whose head the
	// decode refused (faultHead) and its `type` is a kind the load reads
	// ([recordKind]); zero otherwise, so no box-written string is held.
	Type recordKind
	// Size is the bytes the line takes, its break included, so a reader that
	// sums them with its records' sums the bytes the load reads.
	Size int
	// Bytes is what a reader of its bytes reads of the line, break dropped
	// ([threadBytes]), as [threadLine.Bytes] carries it: what a reader judges a
	// line by where it reads by bytes as Claude Code's large-file readers do,
	// whether or not the load reads a record off it ([threadChain.Unread]).
	Bytes threadBytes
}

// lineFault is how Claude Code's load reads a whole line this layer reads as
// no record ([threadLineError]). Its load parses a chunk at a time
// (Bun.JSONL.parseChunk): where a value fails it keeps every value read before
// the failure and resumes after the next line break. So a line failing before
// any whole value yields the load nothing, and every other kind the layer
// refuses can yield it more. The zero value is no fault: a line whose fault is
// not [lineFault.Valid] has a load no reader can say.
type lineFault string

const (
	// faultSyntax is a line that yields the load no value: its JSON fails at
	// a byte inside it before any whole value, or it holds white space alone.
	faultSyntax lineFault = "syntax"
	// faultNotObject is one whole value that is no object: the load passes it
	// over at its test of a record.
	faultNotObject lineFault = "not_object"
	// faultHead is one object whose head key is of the wrong type
	// ([headOf]): the load keeps it under its own reading of that key where
	// it is a kind the load reads ([threadLineError.Type]).
	faultHead lineFault = "head"
	// faultPrefix is the beginning of a value its break cuts: the load reads
	// on into the next line.
	faultPrefix lineFault = "prefix"
	// faultTrailing is one whole value followed by more than white space: the
	// load keeps the value and fails at what follows.
	faultTrailing lineFault = "trailing"
	// faultLead is a line beginning with a NUL or a UTF-8 byte order mark,
	// which the load's large-file reader strips and its small one does not.
	faultLead lineFault = "lead"
	// faultDepth is a value nested deeper than the layer's decode reads
	// (10,000 levels, jsontext's bound), which the load may read whole.
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
// the four its resume walks (user, assistant, attachment, system), the leaf
// checkpoint (last-prompt), progress, a subagent's fork reference
// (fork-context-ref), and the replaced tool results (content-replacement),
// whose list the load spreads into the session's, failing the resume where
// that is no list ([threadLine.ReplacementCount]). The zero value is none of
// them: another type, or a non-string one, which no reader looks at.
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

// Unwrap is what the line's reading met.
func (e *threadLineError) Unwrap() error { return e.Err }

// threadLine is one whole line of a thread: place, size, byte facts
// ([threadBytes]), head and the facts the accessors read ([lineFacts]), all
// from the line's one pass ([threadReader.Next]). IT NEVER HANDS OUT THE LINE'S
// BYTES: head and facts are its own and the byte facts fixed-size, so keeping a
// line keeps no line's bytes. A VALUE AS LONG AS A LINE (a call's input, a
// result's text, a toolUseResult, a message's text) is read by its readers' own
// rules, a shape no rule reads answered rather than an error, so a reshaping
// release costs that reader alone; only bounded facts are handed out
// ([threadCall], [threadLine.HandsBack], [threadLine.AgentAnswer],
// [threadMessage.Notification]). A KEY ONLY ONE READER READS IS HELD TO ITS TYPE
// IN THAT READER'S ACCESSOR, never in the head.
type threadLine struct {
	// N is the line's place among the member's whole lines, from 1.
	N int
	// Size is the bytes the line takes, break included, as a refused line's is
	// ([threadLineError]), so a reader summing them sums the bytes the load
	// reads.
	Size int
	// Bytes is what a reader of its bytes reads of the line, break dropped
	// ([threadBytes]): what large-file readers read a line by ([threadChain],
	// THE LARGE FILE) and the marks the resumed count reads ([resumedReading]).
	Bytes threadBytes
	// Head is what every reader needs of the line.
	Head threadHead

	// f is what the accessors read ([lineFacts]); nil reads as a line of no
	// keys ([threadLine.facts]).
	f *lineFacts
}

// headOf holds the head's keys to their types: `type`, `subtype`, `agentId`
// strings, `parentUuid` null or a string, `isSidechain` a boolean, and a user
// record's `replacesSpan` a boolean, each where present. THE ONE RULE OF A
// HEAD, whichever reading hands it the keys. ANOTHER TYPE IS NO RECORD: a newer
// Claude Code that changes one fails toward "not confirmed", never a fact. A
// key's kind alone decides its errors ([headKey]). `replacesSpan` IS HELD ON A
// USER RECORD ALONE, where the harness and [threadRestarts.See] read it; on
// any other record it never costs a reader the line.
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

// headKey is one head key as a reading hands it to [headOf]: kind, its value's
// kind, 0 where absent; and raw, its value as written, nil where only the kind
// was kept. The rule refuses by kind and reads raw only for the value it hands
// out, so a kind-only head hands out no value for that key.
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
// [headOf]: what kind of record it is and where it sits, never what it says
// nor a call id. The uuid is no part of it: [threadLine.UUID] reads it.
type threadHead struct {
	// Type and Subtype are the record's `type` and `subtype`; "" where absent.
	Type, Subtype string
	// Parent is `parentUuid` AS WRITTEN: nil where the key is absent, the
	// literal null where the record roots a chain, else the quoted uuid.
	//
	// ABSENT IS NOT NULL. The harness's bookkeeping records (queue-operation,
	// api-request, last-prompt, cost-state, tools-used, mode, ...) carry no
	// `parentUuid`; only a record that roots a chain carries null. Read as
	// null, bookkeeping before a brief would root its own chains and every
	// thread would read as restarted ([threadRestarts]).
	Parent json.RawMessage
	// Sidechain is `isSidechain`; nil where absent.
	Sidechain *bool
	// AgentID is `agentId`, the subagent whose thread holds the record; ""
	// where absent or empty, which the layer does not tell apart
	// ([threadHead.Main]); a subagent's id is never empty
	// ([validClaudeAgent]).
	AgentID string
	// ReplacesSpan is a user record's `replacesSpan` JSON true, which makes
	// it a restart ([threadRestarts.See]); false on every other record
	// ([headOf]).
	ReplacesSpan bool
}

// Chained reports whether the record is in a chain AS CLAUDE CODE'S RESUME
// READS ONE: a user, assistant, attachment or system record, whatever
// `parentUuid` it carries or lacks, or a record of another type that carries
// one. The resume loads every record of those four types into the map it walks
// (its test is the type alone), and a parentless one roots a chain
// ([threadRestarts.See]); another type is bookkeeping ([threadHead.Parent]).
//
// THE ONE PREDICATE every reader asks, so none parts from another: the restart
// rule ([threadRestarts.See]), the writer floor ([writerBelowFloor]) and the
// resumed count's charge ([resumedReading.line]). Read by `parentUuid` alone,
// a parentless record of those types would be walked by the resume but held to
// no floor and charged no count. No measured release writes one.
func (h threadHead) Chained() bool {
	switch h.Type {
	case recordUser, recordAssistant, recordAttachment, recordSystem:
		return true
	}
	return h.Parent != nil
}

// Parentless reports whether the record names no parent AS CLAUDE CODE'S
// RESUME READS ONE: its `parentUuid` absent, null or the empty string, each
// falsy to JavaScript, where every walk the resume makes up a chain stops.
//
// IT SAYS NOTHING OF WHETHER THE RECORD IS IN A CHAIN: bookkeeping is
// parentless and in no chain ([threadHead.Chained]). Its readers, the restart
// rule ([threadRestarts.See]) and the model of which records a resume hands its
// model, read a parent by this one rule so the two never part.
func (h threadHead) Parentless() bool {
	return h.Parent == nil || bytes.Equal(h.Parent, jsonNull) || string(h.Parent) == `""`
}

// Main reports whether the record is the main thread's own: `isSidechain`
// present and false, and no `agentId`. A record that says neither is no main
// thread's, whatever file holds it.
func (h threadHead) Main() bool { return h.Sidechain != nil && !*h.Sidechain && h.AgentID == "" }

// threadDecoding is the options every reader of a harness's record decodes by,
// so a value reads as Claude Code's JSON.parse reads it ([jsonscan] follows the
// same grammar): EXACT KEY NAMES (JSON.parse holds `toolUseID` and `toolUseId`
// apart; folding them would key an attachment wrongly,
// [threadLine.AttachmentToolUseID]); A KEY WRITTEN TWICE ALLOWED, READ BY ITS
// LAST VALUE (refusing costs every name and refuses lines the harness reads);
// INVALID UTF-8 AND A LONE SURROGATE READ AS U+FFFD (JSON.stringify writes an
// unpaired surrogate as `\udXXX`, and a text cut inside a pair holds one;
// refused, it would cost a reader a whole thread).
var threadDecoding = jsonv2.JoinOptions(
	jsonv2.MatchCaseInsensitiveNames(false),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// wireDecode decodes raw, one JSON value, into v under [threadDecoding]: every
// small value the layer keeps as written and every value the package's other
// readers of a harness's record read ([openCodeMeasuredWire]); a struct of raw
// values is [wireAlias]'s.
func wireDecode(raw []byte, v any) error {
	return jsonv2.Unmarshal(raw, v, threadDecoding)
}

// wireAlias decodes raw, one JSON object, into v, a pointer to a struct whose
// every field is a json.RawMessage named by its json tag (an embedded struct's
// fields its own, under the outer struct's): each the value of the last key of
// its name ([eachKey]), RAW'S OWN BYTES, NEVER A COPY: decoded by [wireDecode],
// each value kept is copied, so a 32 MiB line read for its result or its
// message's text is held again at each level ([claudeResultWire],
// [claudeEventWire], [claudeMessageWire], [claudeBlockWire]). v is valid only
// while raw is: a reader copies what it keeps.
func wireAlias(raw []byte, v any) error {
	target := reflect.ValueOf(v).Elem()
	fields := wireAliasFields(target.Type())
	return eachKey(raw, func(name []byte, value json.RawMessage, at int) error {
		if index, ok := fields[string(name)]; ok {
			end := at + len(value)
			target.FieldByIndex(index).SetBytes(raw[at:end:end])
		}
		return nil
	})
}

// wireAliasIndex is each struct type [wireAlias] decodes, mapped to its fields'
// indexes by name, built once a type.
var wireAliasIndex sync.Map

// wireAliasFields is t's fields' indexes by name ([wireAlias]); a field that is
// no json.RawMessage is a programming error, refused where the type is first read.
func wireAliasFields(t reflect.Type) map[string][]int {
	if known, ok := wireAliasIndex.Load(t); ok {
		return known.(map[string][]int)
	}
	fields := map[string][]int{}
	var walk func(t reflect.Type, at []int)
	walk = func(t reflect.Type, at []int) {
		var embedded []reflect.StructField
		for i := range t.NumField() {
			f := t.Field(i)
			f.Index = append(slices.Clone(at), i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			switch {
			case f.Anonymous:
				embedded = append(embedded, f)
			case f.Type != reflect.TypeFor[json.RawMessage]() || name == "":
				panic(fmt.Sprintf("executor: %s.%s is no named json.RawMessage, which wireAlias reads", t, f.Name))
			case fields[name] == nil:
				fields[name] = f.Index
			}
		}
		for _, f := range embedded {
			walk(f.Type, f.Index)
		}
	}
	walk(t, nil)
	wireAliasIndex.Store(t, fields)
	return fields
}

// eachElement hands each element of list, raw JSON holding one list, to each,
// in order: its raw bytes (valid only while each runs, so each copies what it
// keeps) and its first byte's offset in list. It NEVER COLLECTS THEM. It stops
// at the first error each returns and returns it unchanged; a list that is no
// list, or does not decode, is an error of its own.
//
// THE ONE WALK of a list a box wrote, under [threadDecoding], so a list costs
// what one element costs at a time. Decoded whole, a 32 MiB event line of
// sixteen million `1`s would cost hundreds of MiB before anything refused the
// first. Readers: [threadLine.ToolResultsCleared], [readUsage],
// [eachClaudeBlock], [toolAnswer], [claudeResult.errorsSaid],
// [openCodeEvents.page].
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

// eachKey hands each key of object, raw JSON holding one object, to each in
// order with its unquoted name ([threadDecoding]), raw value and the value's
// offset, valid only while each runs. It NEVER COLLECTS THEM and never folds two
// keys of one name (JSON.parse reads the last; which counts is each's to say).
// It stops at the first error each returns, unchanged. THE ONE WALK of an
// object whose keys a box chose, as [eachElement] is of a list: open keys are
// walked, never a map first (1 MiB naming a model in 95,315 keys cost 45 MB, 42.6
// times the line, as a map). Reader: [claudeResult.sinceLastResult]. THE NAME IS
// BYTES in a reused buffer, never a string, to spare an allocation per key; a
// reader compares string(name) and copies one it keeps.
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
		// UNQUOTED BEFORE ITS VALUE IS READ: the decoder's bytes are valid only
		// until its next read. The only errors are a lone surrogate and invalid
		// UTF-8, written as U+FFFD as the options' own decode does.
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
// (Number.MAX_SAFE_INTEGER), up to which JSON.parse reads whole numbers exactly.
// Past it a count reads as the nearest double (9,007,199,254,740,993 as
// ...992), so it is refused. Four counts at it sum below 2^55, so no int64 sum
// of a reply's counts, nor one with a thread's bytes ([threadPlainMax]), wraps
// negative as the harness's sum of doubles never does.
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

// UUID is the record's own `uuid`, the id a chained record names as its parent:
// "" where absent (every record outside the chain, [threadHead.Parent]) or
// empty. HANDED OVER AS WRITTEN, its one reader holding it to a rule of its own.
// HELD TO ITS TYPE HERE ALONE: a string; else this accessor's error, never the
// line's, which in the head would cost every reader every chained line. Reader:
// the resumed count ([resumedReading]) keeps the newest restart line's uuid
// ([threadRestarts.See]), which a relaunch compares with the launch's to know
// the job restarted the session after its brief, never by counting restarts
// (the trim after a compaction drops every record before the last,
// [claudeLocalGCFloor]). A uuid absent, empty, over 36 bytes or in error is no reading.
func (l threadLine) UUID() (string, error) { return wireString("uuid", l.facts().uuid) }

// Version is the record's `version`, the release of Claude Code that wrote
// it: "" where absent, an error where it holds no string. The box's word,
// handed out as written.
//
// EVERY CHAINED RECORD NAMES ITS OWN WRITER: a session continued across
// releases holds records of each, so the release a session's row records, its
// last writer's, says nothing of older records.
//
// IN AN ACCESSOR, NEVER THE HEAD: its one reader is [writerBelowFloor], so a
// reshaping release costs that reading alone.
func (l threadLine) Version() (string, error) { return wireString("version", l.facts().version) }

// HandsBack reports whether a user record's own `toolUseResult` carries a
// handback ([lineDecoder.toolResult]); false on every other record.
//
// IT IS THE RECORD'S, NEVER A CALL'S: a handback of any result is one the
// record carries.
func (l threadLine) HandsBack() bool {
	return l.Head.Type == recordUser && l.facts().result.handsBack
}

// AgentAnswer is what a user record's own `toolUseResult` says of a
// subagent's leg ([answerOf]): whether an Agent or Task call's subagent
// completed, the subagent it names, and whether a SendMessage was refused;
// none on every other record.
//
// IT IS THE RECORD'S, NEVER A CALL'S: a reader takes it for a call's only where
// the message holds that call's one tool_result and nothing else.
func (l threadLine) AgentAnswer() (completed bool, agent string, refused bool) {
	if l.Head.Type != recordUser {
		return false, "", false
	}
	r := &l.facts().result
	return r.completed, r.agent, r.refused
}

// answerOf is what a result record's toolUseResult says of the leg
// ([threadLine.AgentAnswer]): `status` "completed", `agentId` ("" where it
// names none), and `success` the literal false for a refused SendMessage.
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

	// recordProgress is a legacy tool-progress record the load keeps out of
	// its map, re-parenting records that name it to its own parent. No probed
	// release writes one.
	recordProgress = "progress"
	// recordContentReplacement is the record of results the harness hands its
	// model replaced ([threadLine.ReplacedResults]), and replacementToolResult
	// the kind of its entry replacing a tool's result.
	recordContentReplacement = "content-replacement"
	replacementToolResult    = "tool-result"
	// recordLastPrompt is the leaf a resume starts from
	// ([threadLine.Checkpoint]).
	recordLastPrompt = "last-prompt"
	// recordForkContextRef refers a forked subagent's load to the thread it
	// forked from.
	recordForkContextRef = "fork-context-ref"
	// attachmentForkBriefing is a fork's attachment
	// ([threadLine.AttachmentType]), subtypeFileSnapshot a file snapshot's
	// system record. A resume takes neither for the last record where 2.1.322
	// chooses its leaf; 2.1.310 takes a snapshot for one.
	attachmentForkBriefing = "fork_briefing"
	subtypeFileSnapshot    = "file_snapshot"

	// subtypeRefusalFallback is the system record for a refused reply moved to
	// another model, naming the records taken back out
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

// hookSystemMessage is the hook record holding its answer's system message,
// the take's token among it ([threadLine.HookTakes]).
const hookSystemMessage = "hook_system_message"

// hookRecordPrefix begins the type of every hook attachment: only such a
// record's words name takes ([threadLine.HookTakes]), judged by its last type
// once the attachment ends ([lineDecoder.attachment]).
const hookRecordPrefix = "hook_"

// The attachments whose payloads a resume holds to a shape
// ([threadLine.AttachmentWellFormed]).
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

// skillListingMaxNames and skillListingMaxNameUnits are Claude Code's own
// bounds on a skill listing: names kept, and UTF-16 code units per name
// ([threadLine.AttachmentWellFormed]).
const (
	skillListingMaxNames     = 4096
	skillListingMaxNameUnits = 512
)

// threadMessage is a user or assistant record's message as its line's one pass
// left it: what readers ask of a string content (that it is one, the subagent
// and leg a task notification names, whether it names the handback tool), or
// its blocks. IT HOLDS NO TEXT: each fact is read as the text streams
// ([textNotice], [textHandback], [textSays]). AND NOTHING OF A BLOCK BUT A
// CALL: of the others, only a count and a set of kinds ([threadBlocks]).
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
// IT HOLDS FACTS, NEVER A CALL'S BYTES: readers ask only the subagent a
// SendMessage sends to and a result's text for comparison with a log's output,
// read here by value, so a call costs the same whatever its input held.
type threadCall struct {
	// Kind is "tool_use" or "tool_result".
	Kind string
	// ID is the call's id: a tool_use's `id`, a tool_result's `tool_use_id`.
	ID string
	// Name is the tool a tool_use called.
	Name string
	// To is the subagent a tool_use's `input` sends to ([sendTargetOf]); ""
	// else.
	To string
	// Failed is whether a tool_result's `is_error` is true.
	Failed bool
	// Text is a tool_result's text ([lineDecoder.resultContent]) as compared
	// with a log's output ([threadResultText]); zero where none.
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

// blockKeys is what a content block's rule reads of it ([blockCallOf]), raw
// keys nil where absent; to is the subagent its `input` sends to
// ([sendTargetOf]), text its `content`'s text ([threadResultText]).
type blockKeys struct {
	typ, id, name, toolUseID, isError json.RawMessage
	to                                string
	text                              threadResultText
}

// blockCallOf is a content block of kind, b its keys, held to its types by
// [threadLine.Message]: an object with a string `type`; a tool_use's `id`
// naming a call ([callID]) and string `name`; a tool_result's `tool_use_id`
// likewise and boolean `is_error` where present. Its kind is the layer's own
// string ([knownKind]). THE ONE RULE OF A BLOCK, whichever reading hands it the
// keys.
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

// sendTargetOf is the subagent a SendMessage input sends to: `to` where the
// input is an object and it is a string naming an agent ([validClaudeAgent]);
// "" else.
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

// resultElementIsText reports whether a tool_result's one content element is
// its text ([lineDecoder.resultContent]): an object of `type` "text" with a
// string `text`.
func resultElementIsText(kind valueKind, typ json.RawMessage, text valueKind) bool {
	if kind != '{' || text != '"' {
		return false
	}
	k, err := wireRequiredString("type", typ)
	return err == nil && k == blockText
}

// knownKind is kind held as this layer's own constant where it names one, so a
// call keeps no string of its own.
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
// A COUNT AND A SET, NEVER A KIND A BLOCK: readers ask how many blocks or
// whether all are of some kinds, never a block's place, so a million blocks
// hold two words.
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
	// blockOther is every kind the layer does not name; no reader tells them
	// apart.
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

// Len is how many blocks the list holds: 0 for none, and for a message holding
// no list ([threadMessage.IsText]).
func (b threadBlocks) Len() int { return b.n }

// All reports whether every block is of one of kinds, the layer's own names
// (blockText, blockToolUse...): true of a list of none (tell by
// [threadBlocks.Len]). A kind the layer does not name matches no kind a reader
// can ask for, so it is never a sign a reply finished nor a block with no call.
func (b threadBlocks) All(kinds ...string) bool {
	var named blockSet
	for _, kind := range kinds {
		if s := blockOf(kind); s != blockOther {
			named |= s
		}
	}
	return b.seen&^named == 0
}

// Message is a user or assistant record's `message`, read once in its line's
// one pass; the zero message for every other record (an api-request-blob
// repeats a request). AN ERROR WHERE IT DOES NOT DECODE, never a message with
// half a call: `message` an object, `content` a string or a list of objects
// with a string `type` ([blockCallOf]'s rule); absent or null content is no
// message either. ITS ERROR IS ITS LINE'S: a reader takes it as a
// *threadLineError, never as a message with no calls, which would read a
// reshaped record holding a call as one holding none. The content list is read
// a block at a time ([lineDecoder.blocks]); what stays held is a call per call
// block (charged to [threadLineMax]) and a count and set per [threadBlocks].
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
// call. AN EMPTY ID IS REFUSED: the API names every call, and one naming none
// could match no call a reader knows.
func callID(key string, raw json.RawMessage) (string, error) {
	id, err := wireRequiredString(key, raw)
	if err == nil && id == "" {
		err = fmt.Errorf("%s names no call", key)
	}
	return id, err
}

// taskNotice is the subagent and leg a task notification's text names
// ([textNotice]), each a string of its own of at most 64 bytes; ok where named.
type taskNotice struct {
	agent, leg string
	ok         bool
}

// IsText reports whether the message's content is one string: a prompt, a
// task notification or a message sent to a subagent, rather than blocks.
func (m threadMessage) IsText() bool { return m.isText }

// Notification is the subagent and leg the message's text names where it is a
// TASK NOTIFICATION's ([textNotice]): lines `<task-notification>`,
// `<task-id>A</task-id>`, `<tool-use-id>L</tool-use-id>`, any output files' and
// `<status>completed</status>`, A an agent ([validClaudeAgent]), L a call id
// ([sandbox.ValidCallID]); ok false for other text and for blocks. It is the
// text's own claim: its reader takes it for a notification only where the
// record's origin says so, and matches the leg only against a call it read.
func (m threadMessage) Notification() (agent, leg string, ok bool) {
	return m.notification.agent, m.notification.leg, m.notification.ok
}

// NamesHandback reports whether the message's text names the handback tool
// ([claudeToolHandback]) anywhere ([textHandback]): what marks a task
// notification's leg as answered by a handback. False for a message of blocks.
func (m threadMessage) NamesHandback() bool { return m.namesHandback }

// Blocks is how many blocks the content list holds and which kinds
// ([threadBlocks]); none for a string content ([threadMessage.IsText]), the
// zero message and an empty list. A reader tells a turn's own message from its
// results by it: a user record whose blocks are tool_result alone. An unnamed
// kind is a kind of its own in the set, never a sign a reply finished nor a
// block with no call.
func (m threadMessage) Blocks() threadBlocks { return m.blocks }

// Calls is the message's call blocks: an assistant's tool_use, a user's
// tool_result. THE ONE WAY AN ID LEAVES THIS LAYER AS A CALL'S: no accessor
// hands out "a line's ids", which would hand a reader of calls a hook's
// firing; an attachment's `toolUseID` leaves only as a key
// ([threadLine.AttachmentToolUseID]); a notification's `<tool-use-id>` leaves as
// the leg its text claims ([threadMessage.Notification]), matched only against
// a call read here.
func (m threadMessage) Calls() []threadCall { return m.calls }

// Says reports whether the message SAYS ANYTHING, as Claude Code reads text:
// text, a "text" block holds a string `text` JavaScript's trim() leaves
// something of; any, the content is such a string or any block holds one. Both
// false on the zero message. A subagent answers with its last reply that says
// something, and the resume classes a call-free reply by whether it says
// anything (leaf choice). JAVASCRIPT'S trim(), NOT strings.TrimSpace
// ([jsTrimKeeps]): TrimSpace takes U+0085 and leaves U+FEFF. Readers: exact
// telling (a leg finishes only on a reply that says something) and the model of
// which records a resume hands its model.
func (m threadMessage) Says() (text, any bool) { return m.saysText, m.saysAny }

// jsTrimKeeps reports whether JavaScript's String.prototype.trim keeps r on a
// string's end: r is neither white space (U+0009, U+000B, U+000C, U+0020,
// U+00A0, U+FEFF, category Zs) nor a line terminator (U+000A, U+000D, U+2028,
// U+2029). Not unicode.IsSpace, which holds U+0085 for space and U+FEFF for
// none ([threadMessage.Says]).
func jsTrimKeeps(r rune) bool {
	switch r {
	case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029':
		return false
	}
	return !unicode.Is(unicode.Zs, r)
}

// StopReason is why the reply an assistant record holds stopped, its message's
// `stop_reason`; "" where absent and on other records. Reader: exact telling of
// which calls a kept session holds, to know whether a subagent's leg finished.
// 2.1.310 writes a reply a record a block, so a thread cut between text and its
// tool call ends on text alone, which only the stop_reason ("tool_use") tells
// from a finished leg. A leg is finished on "end_turn" alone, with no API error
// and a model other than "<synthetic>" ([threadLine.APIError],
// [threadLine.Model]): harness-written replies are "<synthetic>". HELD TO ITS
// TYPE HERE ALONE: a string; else an error, never "" and never Message's.
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
// itself where its request failed for good: `isApiErrorMessage` true; false
// otherwise. Such a reply has the error's text, model "<synthetic>"
// ([threadLine.Model]) and "stop_sequence"; a resume's reply for an unanswered
// turn is marked false. The request skips a reply marked JSON true and of that
// model; the resume classes one JavaScript-truthy or of that model as the
// harness's own. Readers: the model of which records a resume hands its model
// (leaf choice) and exact telling (calls pair only within a reply none of whose
// records the harness wrote; a leg finishes only on a model's reply). HELD TO
// ITS TYPE HERE ALONE: a boolean; else an error, never false, since the two
// readings differ and only a boolean reads alike to both.
func (l threadLine) APIError() (bool, error) {
	f := l.facts()
	if l.Head.Type != recordAssistant || f.apiError == nil {
		return false, nil
	}
	return wireBool("isApiErrorMessage", f.apiError)
}

// Model is the model that wrote the reply an assistant record holds, its
// message's `model` as written: "<synthetic>" where Claude Code wrote the
// reply itself; "" where absent and on every other record.
//
// ITS READERS are [threadLine.APIError]'s, each asking both, as the harness
// does: its resume classes a "<synthetic>" reply as its own whatever its mark
// says, and its request skips a reply only where both say so.
//
// HELD TO ITS TYPE HERE, as [threadLine.Usage] holds the same key: a string
// where present. Another type, null included, is this accessor's error, never
// "" (which readers would take for a model's reply) and never Message's.
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
// `id`, shared by every record of one reply ([threadLine.StopReason]); "" where
// absent and on other records. A resume hands its model, beside each reply on
// the walked chain, the reply's other records found by this id wherever they
// lie. Readers: the model of which records a resume hands its model, exact
// telling, the hook-note confirmation. HELD TO ITS TYPE HERE ([threadLine.Usage]
// holds the same key): a string; else an error. AND TO A TEXT READERS COMPARE
// AS THE HARNESS DOES: an id holding U+FFFD is refused, since the decode reads
// each non-UTF-8 byte as one where the harness can read a run as one, so two ids
// it holds apart (E2 82; FF FF) would decode as one and recover each other's
// records. A lone surrogate reads alike to both. Probed ids are ASCII.
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

// Timestamp is when a record says it was written: ms since the Unix epoch and
// present true where `timestamp` has Claude Code's form
// (`YYYY-MM-DDTHH:MM:SS.sssZ`, toISOString) of a real time; present false where
// absent. The resume compares times as strings (the newest record) and as
// Date.parse milliseconds (the latest leaf; the nearest earlier record past a
// missing parent, at most 5,000 ms before); on this form the two orders agree.
// MILLISECONDS, NOT A time.Time: 8 bytes per record rather than 24 and nothing
// more to compare. HELD TO THAT ONE FORM HERE ALONE: anything else (a
// non-string, an offset, no milliseconds, 2026-02-29, hour 24, second 60) is an
// error, since another form's orders can part and no reader could say which
// record is newest. Reader: the model of which records a resume hands its model.
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

// IsMeta reports whether a user record is META, written by the harness rather
// than said by a person or returned by a call: `isMeta` JSON true; false
// otherwise. The resume reads a marked record holding no tool_result as hanging
// off the reply before it; its request reads the mark as JavaScript truth when
// it classes a turn. The harness marks a skill's text, a coordinator's message,
// a malformed reply's retry. Readers differ on its error: the model of the
// resume reads only JSON true as the mark, so an error is no mark; exact
// telling takes a record for plain only where the mark is false or absent, so
// an error is the mark. HELD TO ITS TYPE HERE ALONE: a boolean, else an error.
func (l threadLine) IsMeta() (bool, error) {
	raw := l.facts().isMeta
	if l.Head.Type != recordUser || raw == nil {
		return false, nil
	}
	return wireBool("isMeta", raw)
}

// SourceToolAssistantUUID is the reply a user record says its results answer,
// its `sourceToolAssistantUUID`, as the 16 bytes of a lowercase canonical uuid
// ([uuidBytes]), and true; false where absent, of another form, or on other
// records. With the flag tengu_foamy_spring on, a resume hangs a result whose
// ids are not all of its parent's calls off this record, if it is another
// record of the thread and of its side. Reader: the model of which records a
// resume hands its model. A UUID OF ANOTHER FORM NAMES NO RECORD: the resume
// finds records by their uuid as written, and the modelled ones are lowercase
// canonical.
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

// uuidBytes is s, a string or streamed bytes (converting would allocate per
// entry), as the 16 bytes of a LOWERCASE CANONICAL UUID, and true: 36
// characters, a hyphen at 8, 13, 18 and 23, lowercase hex elsewhere, any
// version. False for every other string: Claude Code keys records by uuids as
// written, so only this form maps to 16 bytes and back.
//
// BYTES, NOT uuid.UUID: uuid.Parse also reads uppercase, no hyphens, braces and
// a urn:uuid: prefix, strings the load holds apart, so it would merge records
// the load keeps apart.
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
// key: the SHA-256 of its attachment's string `toolUseID`, and true; false
// otherwise. The resume asks which call a batch of hook records followed by the
// last one's `toolUseID`, and keeps the last record for its leaf where that
// call is one the reply above made. A HOOK'S FIRING IS NEVER A CALL'S ID: only
// a key is handed out, compared with keys made the same way of a reply's call
// ids, so no firing reaches a reader of calls (a batch carries `hook-<uuid>`,
// which no call does). Reader: the model of which records a resume hands its model.
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
// `isVirtual` the JSON true; false where absent or false and on other types.
// A virtual record stays in its thread but is handed to no model: the request's
// fold passes it over, as it does a failed request's reply
// ([threadLine.APIError]). Reader: the exact telling of which calls a kept
// session holds, which pairs a call only across PLAIN records, so the continued
// request holds its use and result in one exchange; a virtual record, one with
// an unreadable mark, and those marked by [threadLine.InterruptedByShutdown],
// [threadLine.IsCompactSummary], [threadLine.TaskDelivery] and
// [threadLine.BatchToolUses] are no plain one. HELD TO ITS TYPE HERE ALONE: a
// boolean where present; else an error, never false (the harness reads the mark
// as JSON true in the request and JavaScript truth elsewhere).
func (l threadLine) IsVirtual() (bool, error) {
	raw := l.facts().isVirtual
	if (l.Head.Type != recordUser && l.Head.Type != recordAssistant) || raw == nil {
		return false, nil
	}
	return wireBool("isVirtual", raw)
}

// InterruptedByShutdown reports whether a user record says a SHUTDOWN
// INTERRUPTED the calls its results answer: `interruptedByShutdown` JSON true;
// false where absent or false and on other types. A stopped harness writes the
// result so marked, and a resume's request reads such a record apart (drops it
// where other records carry its results, rewrites it where nothing else answers
// its calls, never merges it into the user message beside it). No probed thread
// holds one. Reader: [threadLine.IsVirtual]'s (no plain record, nor an
// unreadable mark). HELD TO ITS TYPE HERE ALONE, for its reason.
func (l threadLine) InterruptedByShutdown() (bool, error) {
	raw := l.facts().interruptedByShutdown
	if l.Head.Type != recordUser || raw == nil {
		return false, nil
	}
	return wireBool("interruptedByShutdown", raw)
}

// IsCompactSummary reports whether a user record is a COMPACTION'S SUMMARY:
// `isCompactSummary` JSON true; false where absent or false and on other types.
// A compaction hands its model the conversation so far as one such record
// chained to its boundary, which the harness reads apart from the prompts and
// results around it wherever it weighs a turn (JSON true in some passes,
// JavaScript truth in others). Reader: [threadLine.IsVirtual]'s (no plain
// record, nor an unreadable mark). HELD TO ITS TYPE HERE ALONE, for its reason.
func (l threadLine) IsCompactSummary() (bool, error) {
	raw := l.facts().isCompactSummary
	if l.Head.Type != recordUser || raw == nil {
		return false, nil
	}
	return wireBool("isCompactSummary", raw)
}

// TaskDelivery reports whether a user record carries `taskDelivery`, a
// background task's result, whatever the key holds, null included; false where
// absent and on other types. The request's fold renders such a record, by its
// truth, as a delivered task's round (a harness call and result spliced after
// the last assistant message or at the end, which can part a reply from its
// results); the attachment-ordering pass reads the key by presence. No probed
// thread holds one. Reader: [threadLine.IsVirtual]'s. ITS PRESENCE IS THE WHOLE
// READING, the widest the harness makes, so this accessor has no error.
func (l threadLine) TaskDelivery() bool {
	return l.Head.Type == recordUser && l.facts().taskDelivery
}

// BatchToolUses reports whether an assistant record carries `batchToolUses`,
// the calls of a batch the harness split one model call into, handing each
// entry's `id` to each in order; false and nothing handed where absent and on
// other types. Split calls carry the batch's id plus `_<n>`; the request folds
// them back by this list, and their results by the digits their ids end in. No
// probed thread holds one. Reader: [threadLine.IsVirtual]'s (a reply carrying
// one is no plain one). HELD TO ITS SHAPE HERE ALONE: a list of objects with a
// string `id`; else an error, since the fold would match nothing or throw. An
// error ends the walk with earlier entries handed. Read once.
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

// Checkpoint is what a last-prompt record says of the leaf a resume starts
// from: leaf, its string `leafUuid` ("" where absent, null or empty, each naming
// no leaf), and marked, whether `explicit` or `rewound` is JSON true. Nothing
// on other types. A resume takes the newest record naming a leaf, since the last
// compaction, where the session's last record does not descend from it. A
// MARKED one (a rewind, a withdrawal, a fork's copy) makes the resume start
// from it or from nothing. Readers: the model of which records a resume hands
// its model, exact telling, the hook-note confirmation. HELD TO ITS TYPE HERE
// ALONE: `leafUuid` a string or null, else an error (the harness takes any
// truthy value for a leaf); a mark is the bytes `true` exactly (`===!0`).
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
// null included; false where absent and on other types. Where a session's
// newest record is not its last, a resume starts from the newest descending
// from the last only if no last-prompt record carries the key with any value,
// so a null `leafUuid` still turns that rule off ([threadLine.Checkpoint]).
// Reader: [threadLine.Checkpoint]'s, the model of which records a resume hands
// its model. HELD TO ITS TYPE (a string or null), so one key reads alike.
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
// ITS READER is the model of which records a resume hands its model, which
// reads no main thread holding a fork's briefing ([attachmentForkBriefing]):
// the resume takes no such record for the last a session wrote.
//
// HELD TO ITS TYPE HERE: `attachment` an object and its `type` a string, where
// present. Another shape is this accessor's error, never the line's. The
// harness's own test (`attachment?.type === "fork_briefing"`) reads every such
// attachment as none, and so may its reader.
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
// every other record and where absent; an error where no string, this
// accessor's alone ([threadLine.AttachmentWellFormed] reads no event).
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

// HookTakes is which take tokens its reader asked ([threadReader.Asking]) a
// hook record's words name: content of its attachment's `content`, stdout of a
// hook_success's `stdout`, each a string; bit i for token i, whole and exactly
// ([textTakes]). Zero on every other record, an attachment no hook wrote
// ([hookRecordPrefix]) included. A hook's words leave the layer only so.
func (l threadLine) HookTakes() (content, stdout uint64) {
	a := &l.facts().attachment
	return a.contentTokens, a.stdoutTokens
}

// ReplacedResults hands each, in order, the `toolUseId` of every "tool-result"
// entry of a content-replacement record's `replacements`: a result the harness
// handed its model as a preview (behind a server flag, past 200,000 characters
// a message) though its own record keeps the whole output, so the continued
// session's model was not handed it whole, flag on there or not. Nothing, no
// error, on another type. Reader: the exact telling of which calls a kept
// session holds. HELD TO ITS SHAPES HERE ALONE: a list of objects with a string
// `kind` and, for "tool-result", a string `toolUseId`; else this accessor's
// error. Other kinds are passed over; the preview is never read. Read once.
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
// `replacements` lists, of every kind; 0, no error, on another type. The load
// spreads the list into one call's arguments, which throws past the engine's
// limit and fails the resume (2.1.310 also where the key is no list; 2.1.322
// passes that over). Reader: the model of which records a resume hands its
// model, which reads no thread whose load may fail at one. HELD TO ITS SHAPE
// HERE ALONE: a list, each element counted; anything else is an error.
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

// Retracted hands each, in order and as written, every entry of
// `retractedMessageUuids`, the uuids the harness took back out of the
// conversation (user, assistant and model_refusal_fallback system records;
// 2.1.310 the last alone); nothing, no error, elsewhere. The resume drops every
// record but a system one whose uuid's first 24 characters match an entry's
// (split messages share them), so entries are handed whole for the reader to
// cut. A subagent's load and continuation run NO such filter. Readers: the
// model of which records a resume hands its model, and exact telling, which
// reads a subagent thread holding one as not whole. It hands the entry count
// and each canonical entry's [chainPrefix], at most [threadChainMaxRecords]
// ([threadRetracted.Prefixes] says more existed). HELD TO ITS SHAPE HERE ALONE:
// a list of strings; else an error carrying what came before it. Read once.
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

// replacedID is the rule of an entry of a content-replacement list
// ([threadLine.ReplacedResults]): an object with a string `kind` and, for
// "tool-result", a string `toolUseId`, the id of the result replaced; replaced
// false for another kind.
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

// batchEntryID is the rule of an entry of `batchToolUses`
// ([threadLine.BatchToolUses]): an object with a string `id`.
func batchEntryID(kind valueKind, id json.RawMessage) (string, error) {
	if kind != '{' {
		return "", fmt.Errorf("an entry is %s, not an object", kind)
	}
	return wireRequiredString("id", id)
}

// AttachmentWellFormed reports whether the resume keeps an attachment past its
// drop of payloads not of their type's shape; true on every non-attachment. The
// payload is an object with a string `type`, then by type: invoked_skills `skills` a list of
// objects or lists; hook_success `content` a string; skill_listing `names`
// absent or at most 4,096 strings of at most 512 UTF-16 units;
// hook_additional_context `content` a list of strings; task_reminder and
// todo_reminder `content` a list of objects; file and already_read_file
// `content` an object; withheld_memory a non-empty `by` and `entries` of objects
// with string `path`, `displayPath` and `why` of denied, outside, unjudged or
// unsettled; others kept. AN ERROR where the layer cannot count as the harness
// does: a name past 512 units with a non-UTF-8 byte.
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

// withheldKept reports whether an entry of a withheld_memory attachment has the
// shape the resume keeps it with ([threadLine.AttachmentWellFormed]): an object
// with string `path` and `displayPath` and a `why` of "denied", "outside",
// "unjudged" or "unsettled".
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

// skillNameKept is the rule of one skill-listing name ([lineDecoder.skillNames]),
// units its UTF-16 length as decoded and valid whether its bytes are UTF-8: kept
// at most [skillListingMaxNameUnits] units long; kept but unreadable where
// longer with a non-UTF-8 byte, which the harness may count differently, so no
// reader can say whether it is under the bound.
func skillNameKept(units int, valid bool) (kept, unreadable bool) {
	switch {
	case units <= skillListingMaxNameUnits:
		return true, false
	case !valid:
		return true, true
	}
	return false, false
}

// errSkillNameUnmeasured is a skill listing kept but for a name possibly past
// [skillListingMaxNameUnits] with a non-UTF-8 byte ([skillNameKept]): an answer
// the layer cannot give ([threadLine.AttachmentWellFormed]).
var errSkillNameUnmeasured = errors.New("a skill's name holds a byte that is no UTF-8, so its length in UTF-16 " +
	"code units is not one the layer can count as Claude Code does")

// threadPreserved is what a compaction's boundary says it kept of the thread
// before it ([threadLine.Preserved]).
type threadPreserved struct {
	// Named is whether the boundary names what it kept at all: a list, even
	// one of none, or a segment. A resume acts on the newest boundary that
	// does: it relinks by the records named, and where it names none, or is not
	// the last boundary, it still drops every record before the last boundary.
	// So a list of none is never read as a boundary naming nothing.
	Named bool
	// Kept is whether it says it kept records, Anchor the record they hang
	// from. Segment is whether it names them as a segment, from Tail up to Head
	// by parent; else it lists them.
	Kept       bool
	Anchor     string
	Segment    bool
	Head, Tail string
	// Listed is how many entries its list holds, canonical uuid or not; none
	// for a segment. UUIDs is each canonical entry ([uuidBytes]), in order, at
	// most [threadChainMaxRecords], More whether more followed; Nowhere whether
	// an entry is no canonical uuid, a record no modelled chain carries.
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
// thread before it, by uuid in order: `preservedMessages` (uuids plus
// `anchorUuid`, the summary after the boundary) else `preservedSegment` (head,
// anchor, tail, walked tail to head by parent). Nothing, no error, where it
// keeps nothing or on another type. The resume RELINKS by the newest boundary
// naming either, and drops older records even where it lists none
// ([threadPreserved.Named]). HELD TO ITS SHAPES HERE ALONE: anything else is
// this accessor's error, conservative since the load repairs shapes before
// relinking, and handed with the entries read before it (counted, uuids held),
// at most [threadChainMaxRecords] held (More says so; the model refuses lists
// past its room). Readers: the model of which records a resume hands its
// model, exact telling, the hook-note confirmation. Read once, an entry at a time.
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
	// ID is the message's `id`, which every record of one reply carries
	// ([threadLine.StopReason]); "" where absent or empty, which Usage does not
	// tell apart.
	ID string
	// Model is the message's `model`, or "<synthetic>" on a reply the harness
	// wrote itself; "" where absent or empty.
	Model string
	// Input, CacheWrite, CacheRead and Output are the usage's `input_tokens`,
	// `cache_creation_input_tokens`, `cache_read_input_tokens` and
	// `output_tokens` as written: 0 where not named, and all four 0 where the
	// record carries no usage.
	Input, CacheWrite, CacheRead, Output int64
}

// Usage is what a reply says of the request it answered: an assistant record's
// message `id` and `model` and its `usage`'s four counts; ok says the message
// carries a `usage` (else ID and Model still come, counts 0). HELD TO ITS
// TYPES HERE ALONE: strings, an object, whole counts at most 2^53 - 1
// ([wireCount]). A null usage or count is an error, never 0, though Claude Code
// reads it so: a 0 would anchor a reader on a count nobody measured. It errors
// where `iterations` makes Claude Code count other numbers ([usageCounted]), a
// FORWARD GUARD for 2.1.322's optional advisor. Readers: the resumed count and
// the launch floor ([contextReading]).
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

// readUsage reads a message's `usage` into u's four counts by the rule
// [threadLine.Usage] holds it to: an object, each count a whole number in digits
// alone at most 2^53 - 1 (0 where absent, [wireCount]), and `iterations` no entry
// Claude Code counts in its place with other counts ([usageCounted]); else an
// error, null included. ONE RULE FOR A REPLY'S USAGE, IN A THREAD AND IN A
// RUN'S STREAM ([claudeEvents.measure] carries the same message), so a
// collection's figure never has a rule of its own. The run's spend
// ([looseUsage]) keeps its own reading: a count it cannot read costs that
// count's charge, never the line's whole spend.
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

// usageCounted is nil where Claude Code counts a reply by u's own counts, and an
// error where it counts an `iterations` entry with other counts
// ([threadLine.Usage]). CLAUDE CODE'S RULE (2.1.310, 2.1.322): with an input
// side above zero and iterations a list, the LAST entry that is no
// advisor_message or compaction is counted instead, if WHOLE (see
// [wholeIteration]); otherwise u's own counts. AN ENTRY AT A TIME
// ([eachElement]), never collected: the only copy is the last candidate.
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

// iterationPassedOver reports whether entry is one Claude Code passes over: an
// object of type advisor_message or compaction ([usageCounted]).
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

// wholeIteration is the four counts of entry in [threadUsage]'s order, and
// whether it is whole as Claude Code requires of the entry it counts
// ([usageCounted]): type message or fallback_message, each count finite and at
// or above zero, input side above zero. A number past a double's range reads as
// Infinity in JSON.parse, so is not finite.
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

// ToolResultsCleared reports whether an assistant record's
// `message.context_management.applied_edits` holds a clear_tool_uses_20250919
// edit with `cleared_tool_uses` above zero: the server CLEARED THE CONTENT OF
// OLD TOOL RESULTS from what the model was shown, as Claude Code tests. False
// otherwise; which thread's record counts is its reader's to judge. AN ERROR,
// never a report read as none, where any shape on the path is wrong, wherever
// the edit falls in the list. Only this report says the model saw less. COARSE:
// it names nothing cleared; read as cleared it errs toward telling a note
// again. Readers: the hook-note confirmation reads it for a BATCH's note alone
// (it rides the last tool result, measured on claude-sonnet-4-5; a Stop's note
// is a user turn, never cleared); the resumed count takes it, or an unreadable
// one, as a session that may compact.
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

// See reports whether the thread's next record, head h, is a RESTART: a
// compact_boundary or microcompact_boundary system record, a user record with
// `replacesSpan` true, a root that is not the first chained record, or the first
// chained record where it roots no chain. The first chained record, not the
// first record: bookkeeping before a brief is no chain's. One rooting no chain
// starts a thread whose head is gone, so what lay before is unreadable.
// Microcompaction and span replacement count though no release writes them: the
// harness's readers stop at both, the safe direction. The server's clearing of
// old tool results is no restart ([threadLine.ToolResultsCleared]); the
// keep-recent clearing never runs under `claude -p`. A ROOT IS WHAT CLAUDE
// CODE'S WALK READS AS ONE ([threadHead.Parentless]). Readers: the resumed
// count, exact telling, the hook-note confirmation.
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

var _ mainThreadHarness = ClaudeCode{}

// readMainThread is Claude Code's read of the main thread of ask's session in
// box, in the order mainthread.go's file comment states. The release and the saved
// compaction mark the count alone: takes asked are read in the walk all the
// same, the chain first on each line, unless no walk could decide one
// ([unreadTakes]), and so is the newest summary ([claudeSummaryReading]). A
// count marked before the walk leaves no estimate of the next request, so the
// floor reads the copy's last request's input.
func (ClaudeCode) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	var reading sandbox.MainThreadReading
	takes := ask.Takes
	if why := unreadTakes(ask); why != "" {
		reading.Receipts, takes = sandbox.TakeReceipts{Why: why}, nil
	}
	var summary *claudeSummaryReading
	if ask.Summary {
		if why := summaryReleaseWhy(ask.Release, claudeSummaryReleases...); why != "" {
			reading.Summary = summaryUnread(why)
		} else {
			summary = &claudeSummaryReading{}
		}
	}
	if !ask.Count && !ask.Next && len(takes) == 0 && summary == nil {
		return reading
	}
	early := false
	marked := func(mark sandbox.CountMark) {
		early = true
		if ask.Count && reading.Count == nil {
			reading.Count = &sandbox.ResumedCount{Mark: mark}
		}
	}
	unread := func(why string) sandbox.MainThreadReading {
		marked(sandbox.CountUnread)
		if len(takes) > 0 {
			reading.Receipts = sandbox.TakeReceipts{Why: why}
		}
		if summary != nil {
			reading.Summary = summaryUnread(why)
		}
		return reading
	}
	thread, ok := claudeMainThread(ask.Session)
	if !ok {
		return unread("no session names the job's main thread")
	}
	if ask.Release == "" || !releaseAtLeast(ask.Release, claudeCountFloor) {
		marked(sandbox.CountRelease)
	}
	saved, _ := claudePrecompact(ask.Session)
	for _, name := range saved {
		if early {
			break
		}
		// Its size, never its body: an empty file is one the harness cannot
		// read, so it holds no saved compaction.
		tail, err := box.ReadTail(ctx, paths.ClaudeConfigDir()+"/"+name, 0)
		switch {
		case err != nil:
			marked(sandbox.CountUnread)
		case tail.Size > 0:
			marked(sandbox.CountPrecompact)
		}
	}
	counts, nexts := ask.Count && !early, ask.Next && !early
	if !counts && !nexts && len(takes) == 0 && summary == nil {
		return reading
	}
	file := paths.ClaudeConfigDir() + "/" + thread
	body, err := box.OpenFile(ctx, file)
	if err != nil {
		return unread("the thread did not open")
	}
	defer func() { _ = body.Close() }()
	var large passBudget
	defer large.release()
	var (
		readings []threadReading
		count    resumedReading
		next     nextReading
		hooks    *hookReading
	)
	if len(takes) > 0 {
		chain := &chainReading{c: newThreadChain("")}
		hooks = newHookReading(chain, takes)
		readings = append(readings, chain)
	}
	if counts {
		readings = append(readings, &count)
	}
	if nexts {
		readings = append(readings, &next)
	}
	if hooks != nil {
		readings = append(readings, hooks)
	}
	if summary != nil {
		readings = append(readings, summary)
	}
	walkThread(ctx, body, threadPlainMax, &large, readings...)
	if counts {
		reading.Count = count.count(0)
	}
	if nexts {
		reading.Next = next.next()
	}
	if hooks != nil {
		reading.Receipts = hooks.receipts(ctx, ask)
	}
	if summary != nil {
		reading.Summary = summary.reading(ctx, box, file)
	}
	return reading
}
