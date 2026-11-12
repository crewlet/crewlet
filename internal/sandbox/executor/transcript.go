package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
	"github.com/crewlet/crewlet/internal/words"
)

// A captured session, read back as the transcript a person reads.
//
// A collection keeps the archive a run packed when its harness exited
// ([Runner.OpenCapture]), and that archive is the whole record of what the run
// read, called and wrote, so a seat's trail reads it. It is read HERE, beside
// the scripts that pack it, because what it holds is the harness's own record
// and only the runner knows its layout: Claude Code's archive is a gzipped tar
// of the session's transcript (`projects/crewlet/<session>.jsonl`, one JSON
// record a line), its sidecar directory — each subagent's transcript under
// `subagents/`, and the tool results spilled to files — and its task list
// ([claudeArchiveFormat]); OpenCode's is the document
// `opencode session export` prints, gzipped: the session and its settled
// messages, each one of the kinds OpenCode 2 writes, told apart by its `type`
// ([openCodeArchiveFormat], [openCodeMessage]). A format this build does not
// pack is not read ([Carries]).
//
// # Entries, in the order the harness recorded them
//
// A transcript is an ordered list of entries, each one of [EntryKinds]: what
// the run was briefed, the notes a person sent it while it worked, what its
// model wrote, every tool it called and what the tool answered, and the
// compaction summaries the harness wrote in place of the conversation before
// them. The run's own conversation comes first and then each subagent's, in
// the order the archive holds them, every entry of a subagent naming it. What
// is NOT an entry is the harness's bookkeeping — a session's title, a snapshot
// of the files it edited, its system notices and the notices it writes into a
// person's turn, the model's thinking (its working, not what the run did, as
// the live transcript treats it), and a tool result Claude Code spilled to a
// file, which the result's own entry names. A part of the record this read
// could not read is an entry of its own ([EntryUnread]), said where it was — a
// line that is no record, a record or a message of no shape this read knows, a
// transcript that is no plain file — so a gap is never read as continuity.
// Which entries there are, and how many each record makes, is
// [TranscriptVersion]'s.
//
// A NOTE IS KNOWN BY ITS HEADING, wherever the harness filed it. Every note
// opens with the heading the engine writes ([noteHeadOpen]); OpenCode admits
// one as a person's message, and Claude Code hands one to its model as a
// hook's additional context, kept in a record of the harness's own whose shape
// is the harness's and moves between releases — so every record Claude Code
// writes for itself is searched for that heading, rather than read by a shape
// this build would have to pin. A heading is a line's — the engine writes it
// on a line of its own, after the framing a harness puts before it — so a line
// holds one at most, and the search reads each line once, whatever the record
// holds ([noteAt]). The line the run's own hook prints as it hands
// notes over, which the live transcript shows as `[steer]`, is a note entry
// too: a release that keeps the line and not the context leaves it the notes'
// one trace.
//
// # Bounded, and redacted at the read
//
// Every read is held to [TranscriptReadBounds]: the entries one read hands out
// and their text together, each entry's text, one line of the record, and the
// bytes the archive inflates to — and, apart, the bytes the members of a tar
// hand the reader, since a sparse member hands out bytes the archive does not
// hold. A read hands out a page — the entries from an ordinal on — and says why
// it ends where it does ([TranscriptCut]); the next page is read again from the
// archive's start, so the work one read costs is bounded by the archive rather
// than by how deep a reader pages. Every entry's text is passed through
// [redact.Secrets] BEFORE it is cut, whatever the capture did, and what an
// entry hands out is an OPENING OF WHAT ITS WHOLE TEXT REDACTS TO
// ([redactedWithin]): a credential that begins inside the bound is read whole,
// since a key cut in half by a bound is a key the redaction no longer
// recognises, nothing the read could not settle is handed out, and none of the
// rest of a long text is scanned, which would be scanned only to be dropped —
// but by a seat's read for words, which reads every text it searches whole
// (below). And the size an entry states is its redaction's, never the size as
// recorded to the byte, which beside a text shown whole said how long each
// value the redaction took out was ([TranscriptEntry.Bytes]).
// And a read stops when its caller's context is done, checked a line or a
// message at a time, and between the notes and the values one line holds.
//
// HOW MANY READS RUN AT ONCE is bounded too, for the whole process: at most
// [TranscriptReadsAtOnce], whichever socket or REST caller asks. A read takes
// one of those slots before it opens the session's object — so it is handed
// what opens the archive rather than an archive already open — and gives it
// back once it has closed the object again; a read past them waits, nothing
// opened, for as long as its caller does.
//
// # A seat's own page: narrower, by kind or by words, and one slot
//
// A seat reads its own sessions too ([ReadBySeat]), and every page it reads is
// input its model pays for on every request after, inside a tool's answer of
// at most 64 KiB (internal/agent/builtin's ToolAnswerBytes). So its page is
// held to [SeatTranscriptBounds] — a quarter of a person's entries, a
// thirty-second of the text and half an entry — and narrowed by a
// [TranscriptFilter]: to some kinds of entry, or to the entries whose whole
// text holds every term of a query, each handed out as the window of its text
// around the earliest ([windowAround]). Every entry counts toward the ordinals
// whether a filter keeps it or not, so one cursor grammar serves a filtered
// page and a whole one ([Transcript.Places], [Transcript.Next]); and a part of
// the record the read could not read ([EntryUnread]) is handed out whatever
// the filter asks, since nothing can say what kind it was or which words it
// held — passed over, it read as continuity, and a search past it said it had
// searched what it never read. And seats' reads take at most
// [SeatTranscriptReadsAtOnce] of the process's slots between them, so a node
// whose seats page transcripts still reads a person's.
//
// A MATCH IS DECIDED ON WHAT ITS READER IS SHOWN, and on nothing else of the
// text as the harness recorded it. An entry is redacted WHOLE, and is a hit
// only where what that redaction shows of the entry's own words — never a
// marker's ([redact.OwnWords]) — holds every term; its window is cut from that
// redaction, around the earliest term it shows. Decided on the recorded text,
// a term found only inside a credential counted toward a hit whenever another
// term was shown, so whether an entry was a hit confirmed a guess at a value
// the redaction took out, one call at a time; and a window placed on the
// earliest term as recorded, where that one was inside a credential, passed
// the entry over though it showed the word further on. The recorded text is
// asked one thing first, which costs no redaction: whether its words could hold
// every term at all ([textindex.Reading.MayHold]), which no entry whose
// redaction shows them fails, so it decides nothing — only an entry it lets
// through is redacted. Asked whether the recorded text held every term WHOLE
// instead, it decided: a word the redaction's marker ends, `deploy` before a
// token it takes out, is a piece of a longer word as recorded, so the entry
// was passed over unless a credential the redaction took out held the word
// whole, and a hit then said what that credential held. And A FULL PAGE STOPS
// AT A HIT, decided before the page's bound is asked: stopped at an entry the
// recorded text let through, where the next read starts — and so what a
// reader is told the page stopped at — said whether a word the redaction took
// out was there. The recorded text is read whole anyway, so redacting an entry
// it lets through costs one more pass over it, and the read's budget bounds
// both.
//
// A CALL'S WORDS ARE ITS INPUT'S STRINGS', read through their JSON escapes
// ([readingOf]): its text is that input as JSON, where a line break or a tab
// in a string is written `\n` or `\t` glued to the word after it, so read as
// written, the first word of every line after a command's first, or of a file
// a call wrote, was in no word of the entry, and a read for it passed the call
// over — or said no entry held it.
//
// # What is the archive's fault, and what is its source's
//
// A failure of the source the archive came through — an object the store no
// longer holds, bytes that do not open, as the object is opened or as it is
// read — is returned as it came, wrapped, for the caller to tell apart
// ([sourceReader]). An archive that read whole and is not one its format
// describes is a [MalformedArchiveError], which says which way in this
// package's words alone: a box's own agent can rewrite what its run packs
// before a collection takes it, and a decoder's own message quotes what it
// read.

// EntryKind is who one entry of a session's transcript is from: the words a
// read surface groups and labels a transcript by.
type EntryKind string

const (
	// EntryBrief is what the run was handed to do: the engine's brief, a
	// later launch's on the same session, or — in a subagent's thread — the
	// brief the run's own model wrote it.
	EntryBrief EntryKind = "brief"

	// EntryNote is a note a person sent the run while it worked, as its
	// harness handed it to the model — the engine's heading, naming who sent
	// it, and their words — or the line the run's own hook printed as it
	// handed notes over, saying whose reached the run.
	EntryNote EntryKind = "note"

	// EntryModel is what the model wrote.
	EntryModel EntryKind = "model"

	// EntryToolCall is a tool the model called, its input as JSON.
	EntryToolCall EntryKind = "tool_call"

	// EntryToolResult is what a tool answered.
	EntryToolResult EntryKind = "tool_result"

	// EntrySummary is a compaction summary: the harness's own account of the
	// conversation before it, which the model read in its place.
	EntrySummary EntryKind = "summary"

	// EntryUnread is a part of the harness's record this read could not
	// read — a line past the bound one may hold, one that is not a record, a
	// record or a message of no shape this read knows, a transcript that is
	// no plain file, a message of an export past its bound — said where it
	// was.
	EntryUnread EntryKind = "unread"
)

// EntryKinds are every kind an entry can be.
var EntryKinds = []EntryKind{
	EntryBrief, EntryNote, EntryModel, EntryToolCall, EntryToolResult, EntrySummary, EntryUnread,
}

// Valid reports whether a kind off the wire is one this build writes.
func (k EntryKind) Valid() bool { return slices.Contains(EntryKinds, k) }

// TranscriptVersion is the version of what this build counts as a
// transcript's entries: which records and parts of a harness's record are
// entries, and how many entries each makes. An ordinal into a transcript is an
// ordinal under one version, so a read surface's cursor carries it, and a
// cursor of another — a page read on a build that counted entries otherwise,
// which a rolling upgrade serves beside this one — is read again from the start
// rather than continued at an ordinal that names another entry here.
//
// RAISED BY ANY CHANGE THAT MOVES AN ORDINAL: a kind added, a record read that
// was not, one read as more entries or as fewer, one no longer read. At 2, an
// OpenCode export's messages are read as the kinds OpenCode 2 writes them in,
// where a build at 1 read every one of them as no entry. At 3, every key a
// read takes is read exactly as written, as JSON.parse, which both harnesses
// read their records with, reads it: a Claude Code record's, its message's
// and its blocks', and an OpenCode message's, its step's items' and a failed
// call's failure's. A build at 2 matched a key by case and, of a key written
// twice, kept the last value of its field's kind, or the two objects merged:
// no record a release writes reads otherwise (no record the probes kept holds
// either), and one written so reads as more entries or as fewer.
const TranscriptVersion = 3

// TranscriptEntry is one entry of a session's transcript, as a read surface
// sends it.
type TranscriptEntry struct {
	Kind EntryKind `json:"kind"`

	// At is when the harness recorded the entry, zero where its record
	// carries no time — or one no answer can carry ([carriedInstant]).
	At time.Time `json:"at,omitzero"`

	// Thread is the subagent whose transcript holds the entry — its sidecar
	// file's own name — and empty for the run's own conversation. The box's
	// agent can write any name there, so it is somebody else's words.
	Thread string `json:"thread,omitempty" untrusted:"name"`

	// Tool is the tool a call or a result is of: the harness's own, or one
	// an MCP server named.
	Tool string `json:"tool,omitempty" untrusted:"name"`

	// Failed marks a tool result the harness recorded as an error.
	Failed bool `json:"failed,omitempty"`

	// Text is what the entry says, a call's input as JSON: redacted, and at
	// most [TranscriptBounds.EntryBytes] of it, marked where it was cut.
	Text string `json:"text" untrusted:"fence"`

	// Bytes is the size of the entry's whole text as its redaction shows
	// it — what a reader would be shown of it whole, in whose bytes
	// [EntryPlace.TextFrom] counts — wherever the read redacted all of it:
	// a match's hit, and every text short enough that the read took it
	// whole ([redactedWithin]). Of a longer text, whose redaction past its
	// opening the read never made, it is the size the harness recorded,
	// rounded up to a whole KiB ([statedSize]). Cut says the text was cut: a
	// reader says "the first 16 KiB of 2.1 MiB" rather than presenting an
	// opening as the whole.
	//
	// NEVER THE SIZE AS RECORDED TO THE BYTE, which beside a text shown
	// whole said how long each value its redaction took out was — that size
	// less the text's, the marker's added back — so two records that redact
	// alike, a password of seven letters and one of nine, read apart, one
	// read an entry.
	Bytes int  `json:"bytes"`
	Cut   bool `json:"cut,omitempty"`
}

// TranscriptCut is why one read's entries end before the transcript does.
type TranscriptCut string

const (
	// CutEntries is a read that holds the most entries one read hands out,
	// with more after them.
	CutEntries TranscriptCut = "entries"

	// CutBytes is a read whose entries' text reached the most one read hands
	// out, with more entries after them.
	CutBytes TranscriptCut = "bytes"

	// CutRead is a read that stopped before the transcript's end where no
	// later read can go further: the archive's bytes ran past the read
	// budget, or a message of an export past the bound one may hold left
	// what follows it unreadable. Its entries are all that can be shown.
	CutRead TranscriptCut = "read"
)

// TranscriptCuts are every cut a read can name.
var TranscriptCuts = []TranscriptCut{CutEntries, CutBytes, CutRead}

// Valid reports whether a cut off the wire is one this build names.
func (c TranscriptCut) Valid() bool { return slices.Contains(TranscriptCuts, c) }

// Transcript is one read of a session's transcript: its entries from From on.
type Transcript struct {
	Entries []TranscriptEntry

	// Places says where each of Entries is, in their order: its ordinal among
	// every entry the transcript holds, whatever a filter kept, and where in
	// what its whole text redacts to the text handed out begins.
	Places []EntryPlace

	// From is the ordinal the read started at, and Next the ordinal of the
	// first entry it neither handed out nor passed over: the one a following
	// read starts from — after the last of Entries where the read kept every
	// entry it examined, past the entries a filter passed over where it did
	// not, and at an entry the filter keeps where the page had no room for it.
	From, Next int

	// Cut is why Entries ends before the transcript does, and empty where it
	// does not.
	Cut TranscriptCut
}

// EntryPlace is where one entry a read handed out is.
type EntryPlace struct {
	// Ordinal is the entry's place among every entry of the transcript,
	// counted whether or not a filter keeps it, so it names the same entry
	// in a filtered read and a whole one: what a cursor continues from.
	Ordinal int

	// TextFrom is where the text handed out begins, as a byte offset in what
	// the entry's whole text redacts to — the text a reader would be shown of
	// it whole: 0 for an opening, and where a match's window begins
	// ([windowAround]). In the redaction's bytes, never the record's, because
	// the window is cut from the redaction, and a credential it replaced has
	// no length a reader is shown.
	TextFrom int
}

// More reports entries after Next that a read from Next hands out.
func (t Transcript) More() bool { return t.Cut == CutEntries || t.Cut == CutBytes }

// TranscriptBounds are what one read of a transcript is held to, as a read
// surface states them beside what it hands out.
type TranscriptBounds struct {
	// Entries and Bytes bound one read's entries and their text together.
	Entries int `json:"entries"`
	Bytes   int `json:"bytes"`

	// EntryBytes bounds one entry's text.
	EntryBytes int `json:"entry_bytes"`

	// LineBytes bounds one line of a harness's record, or about one message
	// of an export, read whole.
	LineBytes int `json:"line_bytes"`

	// ReadBytes bounds the bytes one read takes as the archive inflates —
	// and, apart, the bytes the members of a tar hand the reader.
	ReadBytes int64 `json:"read_bytes"`
}

// The bounds every read the engine makes is held to ([TranscriptReadBounds]).
const (
	// TranscriptMaxEntries is the most entries one read hands out.
	//
	// TWO HUNDRED: a run's turn makes tens of tool calls, each a call and a
	// result, so one page holds an ordinary run whole, and at the median
	// entry's few hundred bytes it is far inside [TranscriptMaxBytes]. It is
	// also about as many entries as a person reads down before they would
	// rather page than scroll.
	TranscriptMaxEntries = 200

	// TranscriptMaxBytes is the most text one read's entries hold together.
	//
	// ONE MEBIBYTE: sixty-four entries at [TranscriptEntryMaxBytes], about
	// fifteen thousand lines of text, and an eighth of the eight mebibytes
	// the engine lets one reply carry between nodes (queue.MaxPayloadBytes),
	// so an answer built of it fits every transport the engine has, a
	// browser's socket included.
	TranscriptMaxBytes = 1 << 20

	// TranscriptEntryMaxBytes is the most of one entry's text a read hands
	// out, cut on a rune boundary and marked.
	//
	// SIXTEEN KIBIBYTES: a model's longest message, a call's input and a
	// tool result of three hundred lines of code are whole within it. A
	// whole file read or a build's log is longer, and its opening is what
	// says which it was; the entry states its whole size beside it.
	TranscriptEntryMaxBytes = 16 << 10

	// TranscriptReadBytes is the most bytes one read takes as the archive
	// inflates, and the most the members of a tar hand the reader.
	//
	// TWO HUNDRED AND FIFTY-SIX MEBIBYTES: eight times the most an archive is
	// stored as (internal/executorkeep's SessionMaxBytes, 32 MiB), which is
	// gzip's ratio over JSON text, so an ordinary session's archive reads
	// whole within it — and one that inflates past it, as anything a box
	// packs can, stops there rather than holding the engine for as long as
	// it inflates. A tar's members are held to it APART, on what they hand
	// out: a member's bytes come out of what the archive inflates to, so on
	// a packed archive they are never more, but a sparse member hands out its
	// holes without the archive holding them, and unmetered, a member a
	// kibibyte and a half long holds the read for as long as its declared
	// size — an exbibyte, as the format allows.
	TranscriptReadBytes = 256 << 20
)

// TranscriptReadBounds are the bounds every read the engine makes is held to.
func TranscriptReadBounds() TranscriptBounds {
	return TranscriptBounds{
		Entries: TranscriptMaxEntries, Bytes: TranscriptMaxBytes,
		EntryBytes: TranscriptEntryMaxBytes, LineBytes: maxLineBytes,
		ReadBytes: TranscriptReadBytes,
	}
}

// The bounds a seat's read of its own sessions is held to
// ([SeatTranscriptBounds]): its line and read bounds are a person's, and the
// rest is held to what a tool's answer carries — at most 64 KiB, the whole
// answer, which the seat's model pays for on every request of its session
// after it (internal/agent/builtin's ToolAnswerBytes).
const (
	// SeatTranscriptMaxEntries is the most entries a seat's read hands out.
	//
	// FIFTY, a quarter of a person's page: an ordinary run's turn is tens of
	// calls and their results, so a page is a stretch of what a run did that
	// a model reads through, and a seat looking for something narrows a page
	// to some kinds of entry or to its words rather than paging the whole.
	SeatTranscriptMaxEntries = 50

	// SeatTranscriptMaxBytes is the most text a seat's page holds together.
	//
	// THIRTY-TWO KIBIBYTES, half a tool's answer: the other half is the
	// first page's row — its whole ending, about 11 KiB at its bounds — the
	// fence lines around each entry's text, about 120 bytes an entry, and the
	// answer's own JSON. A page whose text JSON escapes past the answer's
	// bound is trimmed by entries by the caller that encodes it.
	SeatTranscriptMaxBytes = 32 << 10

	// SeatTranscriptEntryMaxBytes is the most of one entry's text a seat's
	// read hands out.
	//
	// EIGHT KIBIBYTES, half a person's: a model's message and a call's input
	// are whole within it, and a result's opening says which it was; at a
	// person's sixteen, one long tool result would be a page of its own.
	SeatTranscriptEntryMaxBytes = 8 << 10
)

// SeatTranscriptBounds are the bounds a seat's read of its own sessions is
// held to: a person's line and read bounds, and a page and an entry held to
// what a tool's answer carries. A person's read is unchanged by them
// ([TranscriptReadBounds]).
func SeatTranscriptBounds() TranscriptBounds {
	return TranscriptBounds{
		Entries: SeatTranscriptMaxEntries, Bytes: SeatTranscriptMaxBytes,
		EntryBytes: SeatTranscriptEntryMaxBytes, LineBytes: maxLineBytes,
		ReadBytes: TranscriptReadBytes,
	}
}

// Reader is whose read of a transcript one is, which decides the bounds its
// page is held to ([Reader.Bounds]) and the slots it waits for: a person's,
// through a read surface, or a seat's, of its own sessions.
type Reader string

const (
	// ReadByPerson is a person's read, through a read surface: held to
	// [TranscriptReadBounds], in one of the process's slots.
	ReadByPerson Reader = "person"

	// ReadBySeat is a seat's read of its own sessions: held to
	// [SeatTranscriptBounds], in the seats' one slot and then one of the
	// process's ([SeatTranscriptReadsAtOnce]).
	ReadBySeat Reader = "seat"
)

// Readers are every reader a transcript is read for.
var Readers = []Reader{ReadByPerson, ReadBySeat}

// Valid reports whether r is a reader this build reads for.
func (r Reader) Valid() bool { return slices.Contains(Readers, r) }

// Bounds are the bounds r's read is held to.
func (r Reader) Bounds() TranscriptBounds {
	if r == ReadBySeat {
		return SeatTranscriptBounds()
	}
	return TranscriptReadBounds()
}

// TranscriptFilter narrows a read to the entries a reader asks for. The zero
// filter keeps every entry.
type TranscriptFilter struct {
	// Kinds keeps only entries of these kinds; none keeps every kind. The
	// caller refuses a kind that is not one of [EntryKinds].
	Kinds []EntryKind

	// Terms keeps only entries whose whole text holds every one of them —
	// a query's terms as internal/textindex's Terms makes them, matched as it
	// matches them (textindex.Reading.FindIn), a call's text through its
	// JSON escapes ([readingOf]) — in what the redaction shows of the entry's
	// own words, each handed out as the window of that redaction around the
	// earliest ([windowAround]); none keeps every entry, each as its
	// opening.
	Terms []string
}

// keepsKind reports whether f keeps an entry of kind: one of its kinds, or a
// part of the record the read could not read ([EntryUnread]), which f keeps
// whatever it asks.
func (f TranscriptFilter) keepsKind(kind EntryKind) bool {
	return len(f.Kinds) == 0 || kind == EntryUnread || slices.Contains(f.Kinds, kind)
}

// TranscriptReadsAtOnce is the most transcripts this process decodes at once,
// whichever socket or REST caller asks for them ([ReadTranscript]).
//
// TWO, BY WHAT ONE READ CAN HOLD: a line of the harness's record — or about
// one message of an export — read whole at the 32 MiB a line may run to
// ([TranscriptBounds.LineBytes]), the growth of the slice that gathers it, and
// the copy decoding it makes, on the order of 100 MiB; so the worst case for
// two reads is about 200 MiB. Nothing else bounds how many run at once: a
// socket has at most four queries in flight (stream.MaxInFlightQueries), but
// a node serves every tab open on it, and REST bounds none. One slot would
// queue every reader behind the longest archive; two let a second reader
// through while one is read.
//
// A SEAT'S READ FOR WORDS holds one more text at its worst: the whole
// redaction of an entry whose words could hold every term, which is at most a
// line's, so about a third more, in the one slot seats take between them
// ([SeatTranscriptReadsAtOnce]).
//
// A READ PAST THEM WAITS for a slot for as long as its caller waits, holding
// no decoder while it does — and no opened object, whose store connection it
// would hold — and a slot frees within one bounded read: every read is held
// to [TranscriptReadBounds], and stops when its caller leaves.
const TranscriptReadsAtOnce = 2

// transcriptReads holds a slot for each transcript this process is reading,
// at most [TranscriptReadsAtOnce]: taken before a read opens the session's
// object, and given back once the read has closed it ([readTranscript]).
var transcriptReads = make(chan struct{}, TranscriptReadsAtOnce)

// SeatTranscriptReadsAtOnce is the most of the process's
// [TranscriptReadsAtOnce] slots seats' reads hold at once: a seat's read takes
// one of these, and then one of the process's.
//
// ONE, INSIDE THE PROCESS'S TWO, never a pool beside them: a pool of its own
// would double what reads can hold at their worst, about 100 MiB each. A node
// runs every role in one process by default, so without it a node's seats
// paging transcripts could fill both slots and queue a person's read behind
// every one of them; with it, one slot is always a person's to take while a
// seat reads. A node that runs seats alone serves no person a transcript, and
// its seats' reads take turns in one slot: each is bounded, and a seat's page
// is a fraction of a person's.
const SeatTranscriptReadsAtOnce = 1

// seatTranscriptReads holds a slot for each transcript a seat is reading, at
// most [SeatTranscriptReadsAtOnce].
var seatTranscriptReads = make(chan struct{}, SeatTranscriptReadsAtOnce)

// takeSlots takes the slots by's read waits for — a seat's own, and then one
// of the process's — waiting for each for as long as ctx lasts, and returns
// what gives them back, the process's first. A read whose ctx ends while it
// waits gives back what it took, and opens nothing.
func takeSlots(ctx context.Context, by Reader) (func(), error) {
	if by != ReadBySeat {
		return takeSlot(ctx, transcriptReads)
	}
	seat, err := takeSlot(ctx, seatTranscriptReads)
	if err != nil {
		return nil, err
	}
	shared, err := takeSlot(ctx, transcriptReads)
	if err != nil {
		seat()
		return nil, err
	}
	return func() {
		shared()
		seat()
	}, nil
}

// takeSlot takes a slot of slots for one read, waiting for one for as long as
// ctx lasts, and returns what gives it back. A read whose ctx is already done
// takes none, a slot free or not: its caller has left, and an object opened
// now would be read for nobody.
func takeSlot(ctx context.Context, slots chan struct{}) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// transcriptNameMaxBytes bounds a thread's or a tool's name. A tool's name is
// at most sixty-four characters to every vendor's model API and a sidecar's a
// harness's own short id, so a longer one is not a name, and is cut.
const transcriptNameMaxBytes = 256

// maxPendingCalls bounds the calls a read remembers the tools of while their
// results are still to come: a harness runs a few at once, and a record that
// names thousands without answering them is not one a harness wrote, so the
// results past the bound name no tool rather than the read holding them all.
const maxPendingCalls = 1024

// The ways a read fails.
var (
	// ErrUnsupportedArchive reports an archive of a harness or a format this
	// build does not read; [Carries] says why.
	ErrUnsupportedArchive = errors.New("executor: this build does not read the session's archive")

	// ErrMalformedArchive reports an archive that read whole from its source
	// and is not one its format describes: every [MalformedArchiveError] is
	// one to errors.Is.
	ErrMalformedArchive = errors.New("executor: the session's archive is not one its format describes")
)

// MalformedArchiveError is an archive that read whole from its source and is
// not one its format describes, and Why says which way, in this package's own
// words: one of a few sentences this file writes, each naming the layer that
// found the fault — the compression, or the layout inside it.
//
// NEVER THE DECODER'S WORDS, which quote what they read — a JSON decoder names
// the token it did not expect, and a token can be a whole string of the
// archive's — while a box's own agent can write anything into what its run
// packs. So what the decoder said is kept beneath Why, for errors.Is and
// errors.As, and Error says Why alone: a failure logged or handed on carries
// nothing of the archive's.
type MalformedArchiveError struct {
	Why string
	err error
}

func (e *MalformedArchiveError) Error() string { return ErrMalformedArchive.Error() + ": " + e.Why }

// Is makes a malformed archive [ErrMalformedArchive] to errors.Is.
func (e *MalformedArchiveError) Is(target error) bool { return target == ErrMalformedArchive }

// Unwrap is the decoder's own failure.
func (e *MalformedArchiveError) Unwrap() error { return e.err }

// The ways an archive is not one its format describes, as
// [MalformedArchiveError.Why] says them: every sentence it says.
const (
	whyNotGzip   = "it is not gzip-compressed"
	whyCorrupt   = "its compressed bytes are corrupt"
	whyCutShort  = "it ends part way through"
	whyNotTar    = "it holds no tar of its harness's files"
	whyNotExport = "it holds no export document of its harness's"
	whyMalformed = "its bytes are not laid out as its format describes"
)

// The sentinels a layer marks a failure with, which [malformed] says as one of
// the sentences above.
var (
	// errNotGzip is an archive whose first bytes are no gzip header.
	errNotGzip = errors.New("executor: the archive is not gzip-compressed")

	// errNotTar is a Claude Code archive whose inflated bytes are no tar.
	errNotTar = errors.New("executor: the archive holds no tar")

	// errNotExport is an OpenCode archive whose inflated bytes are no export
	// document.
	errNotExport = errors.New("executor: the archive holds no export document")
)

// malformed is err — the archive's own failure, its source's ruled out — as a
// [MalformedArchiveError]: the compression's fault before the layout's, since a
// layout read through a stream that broke reports the break as its own.
func malformed(err error) *MalformedArchiveError {
	var corrupt flate.CorruptInputError
	why := whyMalformed
	switch {
	case errors.Is(err, errNotGzip):
		why = whyNotGzip
	case errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum), errors.As(err, &corrupt):
		why = whyCorrupt
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		why = whyCutShort
	case errors.Is(err, errNotTar):
		why = whyNotTar
	case errors.Is(err, errNotExport):
		why = whyNotExport
	}
	return &MalformedArchiveError{Why: why, err: err}
}

// The sentinels a read stops on, which never leave this file.
var (
	// errTranscriptPage stops a read that holds all it hands out, its cut
	// already said.
	errTranscriptPage = errors.New("executor: the read holds all it hands out")

	// errReadBudget is the archive's bytes past the read budget.
	errReadBudget = errors.New("executor: the transcript's read budget is spent")

	// errValuePastBound is one value of an export past the bound one may
	// hold.
	errValuePastBound = errors.New("executor: a value of the export is past the bound one may hold")
)

// Carries reports whether this build reads a session archive harness wrote in
// format — the one format each harness's runner packs ([carrier]) — and, where
// it does not, why in a person's words.
func Carries(harness string, format int) (bool, string) {
	c, why := carried(harness, format)
	return c != nil, why
}

// carried is the carrier that reads harness's archives of format, or nil and
// why there is none.
func carried(harness string, format int) (carrier, string) {
	c, ok := carrierOf(harness)
	switch {
	case !ok:
		return nil, fmt.Sprintf("this build carries no session of the harness %q", harness)
	case format != c.ArchiveFormat():
		return nil, fmt.Sprintf("the session's archive is format %d, and this build reads "+
			"%s's format %d alone", format, harness, c.ArchiveFormat())
	}
	return c, ""
}

// ReadTranscript reads the transcript of one session archive harness wrote in
// format — the bytes a run packed when its harness exited, as a collection
// keeps them, which open opens — from its entry from on, held to
// [TranscriptReadBounds]: every entry's text redacted and then cut, the read's
// page and its cut said ([Transcript.Cut]). It stops where ctx is done, with
// ctx's error.
//
// THE READ OPENS THE ARCHIVE AND CLOSES IT, inside one of the process's
// [TranscriptReadsAtOnce] slots: open is called only once a slot is held — an
// object opened before then would hold its store's connection while the read
// waited — and the slot is given back only once the archive is closed, so no
// read leaves an archive open past its slot. A read whose ctx ends while it
// waits for one returns ctx's error, having opened nothing.
//
// A failure of open, or of the archive as it is read, is returned wrapped, as
// it came; an archive this build does not read is [ErrUnsupportedArchive],
// refused before a slot is taken; one that is not what its format describes is
// a [MalformedArchiveError].
func ReadTranscript(ctx context.Context, harness string, format int,
	open func(context.Context) (io.ReadCloser, error), from int) (Transcript, error) {
	return readTranscript(ctx, harness, format, open, from, ReadByPerson, TranscriptReadBounds(),
		TranscriptFilter{})
}

// ReadTranscriptAs is [ReadTranscript] for by: held to by's bounds
// ([Reader.Bounds]), through by's slots, and narrowed to the entries filter
// keeps — every entry counted toward the ordinals all the same, and Next the
// first the read neither handed out nor passed over.
func ReadTranscriptAs(ctx context.Context, by Reader, filter TranscriptFilter, harness string, format int,
	open func(context.Context) (io.ReadCloser, error), from int) (Transcript, error) {
	if !by.Valid() {
		return Transcript{}, fmt.Errorf("executor: a transcript is read for %s, not %q", words.Or(Readers), by)
	}
	return readTranscript(ctx, harness, format, open, from, by, by.Bounds(), filter)
}

// readTranscript is [ReadTranscript] for by, held to bounds and narrowed by
// filter.
func readTranscript(ctx context.Context, harness string, format int,
	open func(context.Context) (io.ReadCloser, error), from int, by Reader,
	bounds TranscriptBounds, filter TranscriptFilter) (Transcript, error) {

	c, why := carried(harness, format)
	if c == nil {
		return Transcript{}, fmt.Errorf("%w: %s", ErrUnsupportedArchive, why)
	}
	if from < 0 {
		return Transcript{}, fmt.Errorf("executor: a transcript is read from its entry 0 on, "+
			"or a later one, not from %d", from)
	}
	release, err := takeSlots(ctx, by)
	if err != nil {
		return Transcript{}, err
	}
	// DEFERRED BEFORE THE ARCHIVE IS OPENED, SO IT RUNS AFTER IT IS CLOSED:
	// deferred calls run last first, and a slot given back while its archive
	// is still open is one more archive open than the slots allow.
	defer release()
	archive, err := open(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		// A READ ITS CALLER ABANDONED stops as abandoned, whatever the store
		// made of the open stopping under it.
		return Transcript{}, ctx.Err()
	case err != nil:
		return Transcript{}, fmt.Errorf("executor: opening the session's archive: %w", err)
	}
	defer func() { _ = archive.Close() }()
	return decodeTranscript(ctx, c, archive, from, bounds, filter)
}

// decodeTranscript is one read of archive, which c's harness packed: its
// transcript from the entry from on, held to bounds and narrowed by filter.
func decodeTranscript(ctx context.Context, c carrier, archive io.Reader, from int,
	bounds TranscriptBounds, filter TranscriptFilter) (Transcript, error) {

	src := &sourceReader{r: archive}
	b := &transcriptBuilder{from: from, next: from, bounds: bounds, filter: filter,
		entries: []TranscriptEntry{}, places: []EntryPlace{},
		calls: map[string]string{}, inflated: budgetReader{left: bounds.ReadBytes},
		handed: budgetReader{left: bounds.ReadBytes}}
	err := b.read(ctx, c, src)
	switch {
	case err != nil && !errors.Is(err, errTranscriptPage) && ctx.Err() != nil:
		// A READ ITS CALLER ABANDONED stops as abandoned, whatever the source
		// or a decoder made of the stream stopping under it.
		return Transcript{}, ctx.Err()
	case src.err != nil:
		// THE SOURCE'S OWN FAILURE FIRST, whatever the decoding made of it:
		// gzip, tar and JSON each report a stream that stopped in their own
		// words, and an object that did not open is not a malformed archive.
		return Transcript{}, fmt.Errorf("executor: reading the session's archive: %w", src.err)
	case err == nil, errors.Is(err, errTranscriptPage):
	case b.inflated.spent, b.handed.spent:
		b.cut = CutRead
	default:
		return Transcript{}, malformed(err)
	}
	return Transcript{Entries: b.entries, Places: b.places, From: from, Next: b.next, Cut: b.cut}, nil
}

// transcriptBuilder is one read's page as it is built: every entry the
// archive holds is counted, and those from the page's first ordinal on are
// shaped and kept until the page holds all it hands out.
type transcriptBuilder struct {
	from   int
	bounds TranscriptBounds
	filter TranscriptFilter

	// inflated is the archive's bytes as they inflate, and handed the bytes
	// the members of a tar hand the reader: each held to the read budget.
	inflated, handed budgetReader

	// seen is how many entries the read has passed, the page's and those
	// before it; next the ordinal after the last it examined from the
	// page's first on, whether the page kept it or a filter passed it over;
	// entries, their places and bytes are the page's own.
	seen    int
	next    int
	entries []TranscriptEntry
	places  []EntryPlace
	bytes   int
	cut     TranscriptCut

	// thread is the subagent whose transcript the lines being read are,
	// empty for the run's own.
	thread string

	// calls names a call still waiting on its result by its id, so the
	// result says which tool answered — at most [maxPendingCalls] of them,
	// and only within the one transcript a call and its result share.
	calls map[string]string
}

// read inflates the archive and hands it to its harness's reader, the bytes it
// inflates to held to the read budget.
func (b *transcriptBuilder) read(ctx context.Context, c carrier, src io.Reader) error {
	zr, err := gzip.NewReader(src)
	switch {
	case errors.Is(err, gzip.ErrHeader), errors.Is(err, io.EOF):
		return fmt.Errorf("%w: %w", errNotGzip, err)
	case err != nil:
		return err
	}
	b.inflated.r = zr
	return c.transcript(ctx, b, &b.inflated)
}

// add counts one entry of the transcript and keeps it where it is on the page,
// raw its whole text as the harness recorded it — where the filter keeps it.
// It answers errTranscriptPage once the page holds all it hands out, the cut
// said, so the read stops there.
//
// AN ENTRY BEFORE THE PAGE IS COUNTED AND NOTHING MORE: its text is not
// redacted, which is the expensive part, because nobody is shown it. And one
// the filter passes over is counted and examined: its kind read, and for a
// match its whole text searched, which costs no redaction either — only an
// entry whose words as recorded could hold every term is redacted, whole, to
// decide whether it is a hit on what its reader is shown (see the file
// comment), and only an entry the page hands out otherwise.
//
// A HIT IS DECIDED BEFORE THE PAGE'S BOUND IS ASKED, so a full page stops at
// an entry the filter keeps whatever the recorded text holds: one more entry
// redacted a page, against a stop that said what the redaction took out.
//
// A PART OF THE RECORD THE READ COULD NOT READ is kept whatever the filter
// asks ([TranscriptFilter.keepsKind]) and handed out as its opening, never
// searched: its text is this package's own account of the gap, which holds no
// word of what the gap held.
func (b *transcriptBuilder) add(e TranscriptEntry, raw string) error {
	ordinal := b.seen
	b.seen++
	if ordinal < b.from {
		return nil
	}
	if !b.filter.keepsKind(e.Kind) {
		b.next = ordinal + 1
		return nil
	}
	matching := len(b.filter.Terms) > 0 && e.Kind != EntryUnread
	var shown string
	at := 0
	if matching {
		reading := readingOf(e.Kind)
		if !reading.MayHold(raw, b.filter.Terms) {
			b.next = ordinal + 1
			return nil
		}
		shown = redact.Secrets(raw)
		first, all := reading.FindIn(redact.OwnWords(shown), b.filter.Terms)
		if !all {
			b.next = ordinal + 1
			return nil
		}
		at = first
	}
	if len(b.entries) == b.bounds.Entries {
		b.cut = CutEntries
		return errTranscriptPage
	}
	textFrom := 0
	var whole int
	if matching {
		e.Text, textFrom, e.Cut = windowAround(shown, at, b.bounds.EntryBytes)
		whole = len(shown)
	} else {
		e.Text, e.Cut, whole = redactedWithin(raw, b.bounds.EntryBytes)
	}
	e = shape(e, statedSize(len(raw), whole))
	// THE FIRST ENTRY OF A PAGE IS ALWAYS KEPT: one entry is bounded on its
	// own, and a page that could not hold it would be a page no read gets
	// past.
	if len(b.entries) > 0 && b.bytes+len(e.Text) > b.bounds.Bytes {
		b.cut = CutBytes
		return errTranscriptPage
	}
	b.entries = append(b.entries, e)
	b.places = append(b.places, EntryPlace{Ordinal: ordinal, TextFrom: textFrom})
	b.bytes += len(e.Text)
	b.next = ordinal + 1
	return nil
}

// readingOf is how an entry of kind writes its words, as a read for words reads
// them: a tool call's text is its input as JSON, which this read writes it as
// ([compactJSON]), so its words are its strings' — read through their escapes
// ([textindex.JSON]), where a string's line break written `\n` glued its `n`
// to the word after it. Every other entry's text is words as the harness, the
// model or a tool wrote them, read as written ([textindex.Plain]): read as JSON,
// a Windows path's `\new` would lose its `n` to a line break.
func readingOf(kind EntryKind) textindex.Reading {
	if kind == EntryToolCall {
		return textindex.JSON
	}
	return textindex.Plain
}

// shape is an entry as it is handed out, its text already cut from its
// redaction: the size it states ([TranscriptEntry.Bytes]), and its names
// redacted and bounded.
//
// EACH OF ITS OWN BYTES, copied: what the redaction and the cut hand back is,
// wherever they changed nothing, the very string the entry was read from, and
// that is a piece of a decoded record of up to a line's 32 MiB — a short note
// is a substring of the value that carried it. Kept on the page, a few bytes of
// it held all of it until the answer was sent, so a page of a few KiB could
// hold the whole read budget.
func shape(e TranscriptEntry, size int) TranscriptEntry {
	e.Bytes = size
	e.Thread, _, _ = redactedWithin(e.Thread, transcriptNameMaxBytes)
	e.Tool, _, _ = redactedWithin(e.Tool, transcriptNameMaxBytes)
	e.Text, e.Thread, e.Tool = strings.Clone(e.Text), strings.Clone(e.Thread), strings.Clone(e.Tool)
	return e
}

// statedSize is the size an entry states ([TranscriptEntry.Bytes]) of a text
// its harness recorded in recorded bytes: whole, the size of what the text
// redacts to, where the read made all of that redaction, and recorded rounded
// up to a whole [statedSizeGrain] where it did not, whole being negative.
func statedSize(recorded, whole int) int {
	if whole >= 0 {
		return whole
	}
	return (recorded + statedSizeGrain - 1) / statedSizeGrain * statedSizeGrain
}

// statedSizeGrain is what the size of a text a read redacted only the opening
// of is rounded up to ([statedSize]).
//
// A KIBIBYTE. Such a text runs past a read's window of it — the entry's bound
// and twice the longest credential the redaction knows, a private key's block
// of up to 64 KiB — so past a hundred and thirty KiB, and a reader is told its
// size as "the first 8 KiB of 2.1 MiB", which a kibibyte moves by under one
// percent. A value the redaction takes out of its opening — a password, a
// token, tens of bytes — then moves the size a reader is told only where it
// carries the text across a kibibyte, never by its own length.
const statedSizeGrain = 1 << 10

// redactedWithin is text redacted and then cut to at most bound bytes, marked
// where it was cut, whether it was, and the size of what the whole text redacts
// to where the read made that redaction — a text the window below takes whole
// — or -1 where it redacted only an opening: an OPENING OF WHAT THE WHOLE TEXT
// REDACTS TO, the bytes it hands out ones the whole text's redaction would.
//
// WHAT IS REDACTED IS WHAT CAN REACH THE CUT, and [redactContext] past it —
// the head: a credential that begins inside the bound ends within that much
// of it, a private key's block being the longest shape the redaction knows,
// so it is read whole and redacted as one, where a key cut in half by the
// bound would be one the redaction no longer recognises. And the head is
// redacted INSIDE A WINDOW [redactContext] LONGER STILL, which settles it: a
// credential that begins in the head ends in the window, so the window's
// redaction of the head is the whole text's. The rest of a long text is never
// scanned: a line of a record may run to [maxLineBytes], and redacting all of
// it to hand out its first [TranscriptEntryMaxBytes] was the whole cost of a
// read many times over.
//
// ONLY WHAT THE HEAD REDACTS TO IS HANDED OUT, never the window's own end,
// which the window cut where it ended — a credential begun there is one the
// redaction did not see whole. Cut by the output's bytes alone, the end came
// through whenever the redaction shrank what was before it: a head of private
// keys, each block a marker of twenty-two bytes, left a window of eighty KiB
// redacted to less than the bound, and the GitHub token the window had cut
// was handed out as its first twenty characters. So what is handed out is as
// much of the window's redaction as the head's own redaction agrees with: all
// of it up to the head's end where nothing runs across that end, and up to
// where a credential does — which the head alone cuts, and the window reads
// whole — where something does. Such a credential is not handed out at all;
// the entry is cut where it begins.
func redactedWithin(text string, bound int) (string, bool, int) {
	head := textcut.Bytes(text, bound+redactContext)
	// AT LEAST redactContext PAST THE HEAD: a cut walks back to a rune's
	// start, at most utf8.UTFMax-1 bytes, and the window is never shorter
	// than the head and the longest credential after it.
	window := textcut.Bytes(text, len(head)+redactContext+utf8.UTFMax-1)
	out := redact.Secrets(window)
	if len(window) == len(text) {
		// THE WHOLE TEXT WAS READ, and its redaction is the whole text's.
		if len(out) <= bound {
			return out, false, len(out)
		}
		return textcut.Within(out, bound), true, len(out)
	}
	settled := textcut.Bytes(out, sharedPrefix(out, redact.Secrets(head)))
	return textcut.Opening(settled, bound), true, -1
}

// sharedPrefix is how many bytes a and b open with alike.
func sharedPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// windowAround is the window of shown — an entry's whole text as the
// redaction leaves it — around the byte offset at: at most bound bytes, a third
// of it before at as textindex.Snippet places a snippet's, marked at its end
// where shown runs on past it; where in shown it begins; and whether it is less
// than all of shown.
//
// CUT FROM THE WHOLE TEXT'S REDACTION, so neither end can cut a credential in
// half: every credential the redaction knows is a marker in shown, wherever
// the window falls. And NEVER A MARKER IN PART, since a marker cut to
// "ACTED:github-token]" reads as words of the entry's own: a start that would
// fall inside the markers the redaction wrote moves on past them, and an end
// back to where they begin ([redact.MarkersAround]) — each away from at, so the
// window still holds the word it was found by, however long a run of markers
// it met.
func windowAround(shown string, at, bound int) (string, int, bool) {
	if len(shown) <= bound {
		return shown, 0, false
	}
	// THE ROOM a window's text takes, a cut's marker beside it inside bound.
	room := max(bound-len(textcut.Opening("", bound)), 1)
	start := max(at-room/3, 0)
	for start > 0 && !utf8.RuneStart(shown[start]) {
		start--
	}
	if _, past, inside := redact.MarkersAround(shown, start); inside {
		start = past
	}
	end := min(start+room, len(shown))
	for end < len(shown) && !utf8.RuneStart(shown[end]) {
		end--
	}
	if begin, _, inside := redact.MarkersAround(shown, end); inside {
		end = begin
	}
	if end == len(shown) {
		return shown[start:], start, start > 0
	}
	return textcut.Opening(shown[start:end], bound), start, true
}

// prose adds one entry of words — a brief, the model's, a summary — unless it
// holds none. A brief that is a note — its words open with a note's heading,
// inside nothing but what a harness wraps one in — is the note, or the notes,
// it carries.
func (b *transcriptBuilder) prose(ctx context.Context, kind EntryKind, at time.Time, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if kind == EntryBrief && opensWithNote(text) {
		return b.notes(ctx, at, text)
	}
	return b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, text)
}

// notes adds an entry for every note text carries ([notesIn]), as it finds
// each: a page that holds all it hands out stops the search there, and so
// does a caller that left — one text can carry a note on every line of the
// mebibytes a line may run to.
func (b *transcriptBuilder) notes(ctx context.Context, at time.Time, text string) error {
	for note := range notesIn(text) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.add(TranscriptEntry{Kind: EntryNote, At: at, Thread: b.thread}, note); err != nil {
			return err
		}
	}
	return nil
}

// unread adds the entry saying a part of the record was not read.
func (b *transcriptBuilder) unread(what string) error {
	return b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, what)
}

// called remembers which tool the call id is of, for its result — but never
// an id or a tool's name past [transcriptNameMaxBytes], which no harness
// writes: remembered whole, each held a string of any length a record names,
// at a thousand calls the read keeps, for as long as their results were to
// come. Such a call's result names no tool, as one past [maxPendingCalls]
// does.
func (b *transcriptBuilder) called(id, tool string) {
	if id == "" || len(id) > transcriptNameMaxBytes || len(tool) > transcriptNameMaxBytes ||
		len(b.calls) >= maxPendingCalls {
		return
	}
	b.calls[id] = tool
}

// answered is the tool the call id was of, which the record no longer needs
// once its result is read.
func (b *transcriptBuilder) answered(id string) string {
	tool := b.calls[id]
	delete(b.calls, id)
	return tool
}

// ---- notes --------------------------------------------------------------- //

// noteAt is where the first note heading in text at or after from opens, and
// where the line it is on ends, and false where there is none: the heading's
// opening, a sender's name, and the rest of the heading closing its line
// ([noteHeadOpen], [noteHeadClose]).
//
// EACH LINE IS READ ONCE, whatever it holds. Whether an opening is a heading
// is a matter of how its line ends, which every opening on the line shares: so
// where the first opening on a line is no heading, none after it on that line
// is either, and the search goes on from the next line. It used to look for
// the line's end again from every opening, and a line of a record — which may
// run to [maxLineBytes] — written as nothing but openings cost the openings
// times the line, tens of minutes of a core for one line. A heading found, the
// next is looked for from the end of its line, for the same reason: a line
// holds one heading at most.
func noteAt(text string, from int) (at, end int, ok bool) {
	for from < len(text) {
		i := strings.Index(text[from:], noteHeadOpen)
		if i < 0 {
			return 0, 0, false
		}
		at = from + i
		end = len(text)
		if n := strings.IndexByte(text[at:], '\n'); n >= 0 {
			end = at + n
		}
		if line := text[at:end]; len(line) > len(noteHeadOpen)+len(noteHeadClose) &&
			strings.HasSuffix(line, noteHeadClose) {
			return at, end, true
		}
		from = end + 1
	}
	return 0, 0, false
}

// notesIn is every note text carries, each from its heading to the next one's
// or to text's end — a hook's answer joins the notes of one step in one text
// (ClaudeHookAnswer) — trimmed of the system reminder a harness closes them
// in, in order and one at a time, so a reader that stops reads no further.
func notesIn(text string) iter.Seq[string] {
	return func(yield func(string) bool) {
		at, end, ok := noteAt(text, 0)
		for ok {
			next, nextEnd, more := noteAt(text, end)
			stop := len(text)
			if more {
				stop = next
			}
			note := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[at:stop]), "</system-reminder>"))
			if !yield(note) {
				return
			}
			at, end, ok = next, nextEnd, more
		}
	}
}

// noteFraming is all a harness puts before a note it hands its model: a
// system reminder's opening tag, and Claude Code's `<event> hook additional
// context:` ([claudeHookContext]).
var noteFraming = regexp.MustCompile(`^\s*(?:<system-reminder>\s*)?(?:\w+` +
	regexp.QuoteMeta(claudeHookContext) + `\s*)?$`)

// opensWithNote reports text that is a note — its first note heading has
// nothing before it but what a harness frames one with — as against a brief
// that carries a note further down, as a launch's brief does with the notes
// that arrived before it.
func opensWithNote(text string) bool {
	at, _, ok := noteAt(text, 0)
	return ok && noteFraming.MatchString(text[:at])
}

// ---- Claude Code ------------------------------------------------------- //

// transcript reads a Claude Code archive ([claudeArchiveFormat]).
func (ClaudeCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.claudeArchive(ctx, archive)
}

// claudeRecord is the part of one line of Claude Code's transcript a read
// takes, as its one decode leaves it ([wireDecode]), each key by its exact
// name and raw, and read loosely by type ([transcriptBuilder.claudeLine]):
// what kind of record it is, when it was written, whether it is one the
// harness wrote for itself (`isMeta`) or a compaction summary, and its message
// — an object whose content is a string or a list of blocks. A system
// record's subtype and content are its own, read for the one line the run's
// own hook prints as it hands the model a note ([claudeSteerSaid]).
type claudeRecord struct {
	Type             json.RawMessage `json:"type"`
	Subtype          json.RawMessage `json:"subtype"`
	Timestamp        json.RawMessage `json:"timestamp"`
	IsMeta           json.RawMessage `json:"isMeta"`
	IsCompactSummary json.RawMessage `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
	Content          json.RawMessage `json:"content"`
}

// claudeArchive reads the session's transcript and each subagent's, a member
// at a time, and passes every other member — the tool results spilled to
// files, the task list — unread.
//
// WHAT A MEMBER HANDS OUT IS METERED, apart from what the archive inflates to
// ([TranscriptReadBytes]); and every member is read a line at a time through
// ONE buffer, reset for each, since an archive of empty members is allowed
// hundreds of thousands of them.
func (b *transcriptBuilder) claudeArchive(ctx context.Context, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	lines := bufio.NewReaderSize(&b.handed, lineChunkBytes)
	line := func(raw []byte) error { return b.claudeLine(ctx, raw) }
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		thread, ok := claudeThread(h.Name)
		if !ok {
			continue
		}
		b.thread = thread
		clear(b.calls)
		if h.Typeflag != tar.TypeReg {
			// A TRANSCRIPT THAT IS NO PLAIN FILE — a link, a directory — is
			// not read through: a link names another member or a path the
			// archive does not hold, and neither is this record. Said, where
			// the transcript was.
			if unreadErr := b.unread("a transcript of the harness's record is not a plain " +
				"file in the archive, and was not read"); unreadErr != nil {
				return unreadErr
			}
			continue
		}
		lines.Reset(&b.handed)
		if readErr := readLines(lines, b.bounds.LineBytes, line, b.claudeLinePastBound); readErr != nil {
			return readErr
		}
	}
}

// claudeThread is whose transcript a member of the archive named name is: the
// session's own, `projects/crewlet/<session>.jsonl` (the empty thread), or a
// subagent's, `projects/crewlet/<session>/subagents/<name>.jsonl` (the thread
// named after its file) — and false for every other member.
func claudeThread(name string) (string, bool) {
	rest, ok := strings.CutPrefix(path.Clean(name), "projects/"+claudeProjectDir+"/")
	if !ok {
		return "", false
	}
	stem, ok := strings.CutSuffix(rest, ".jsonl")
	if !ok {
		return "", false
	}
	switch parts := strings.Split(stem, "/"); {
	case len(parts) == 1 && parts[0] != "":
		return "", true
	case len(parts) == 3 && parts[1] == "subagents" && parts[2] != "":
		return parts[2], true
	}
	return "", false
}

// claudeLinePastBound says where a line past the bound was.
func (b *transcriptBuilder) claudeLinePastBound(n int64) error {
	return b.unread(fmt.Sprintf("a line of %s of the harness's record was not read: it is "+
		"past the %s one line may hold", humanSize(n), humanSize(int64(b.bounds.LineBytes))))
}

// claudeLine reads one record: a person's turn of the conversation — the
// brief, a note, a compaction summary, or the results of the model's tool
// calls — or the model's own, its text and its calls. Every other record is
// the harness's own, and no entry but for the notes it carries
// ([transcriptBuilder.claudeNotes]).
//
// A LINE THAT IS NO OBJECT IS NO RECORD, whatever JSON it is: an array or a
// number decodes into a record as nothing at all, and the line would vanish.
// Within an object the record is DECODED LOOSELY, as the stream is
// ([decodeOpenCodeEvent]'s reason): a field whose type a release changed is
// skipped rather than losing the record. And BY ITS KEYS EXACTLY AS WRITTEN,
// as Claude Code's own JSON.parse reads them ([threadDecoding]), its message
// and its blocks as much as the record, so one record is read by one rule,
// and as the run's event stream reads the same record ([claudeEventWire]).
func (b *transcriptBuilder) claudeLine(ctx context.Context, line []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	if !jsonObject(line) {
		return b.notARecord(len(line))
	}
	var rec claudeRecord
	if wireDecode(line, &rec) != nil {
		return b.notARecord(len(line))
	}
	at := claudeTime(looseString(rec.Timestamp))
	switch record := looseString(rec.Type); {
	case record == "assistant":
		return b.claudeMessage(ctx, rec.Message, EntryModel, at)
	case record == "user" && !looseTrue(rec.IsMeta):
		kind := EntryBrief
		if looseTrue(rec.IsCompactSummary) {
			kind = EntrySummary
		}
		return b.claudeMessage(ctx, rec.Message, kind, at)
	case record == "system":
		// THE LINE THE RUN'S OWN HOOK PRINTS as it hands the model a note,
		// which the live transcript shows as `[steer]`: a note in its own
		// right here, since a release that keeps no hook context leaves it
		// the note's one trace. ITS OWN LINE, as the harness recorded it, for
		// the entry to redact, bound and state the size of as every entry's
		// text is — never the live view's rendering of it, which is cut to a
		// phase event's line and counts the lines after it in words the
		// harness never wrote, and was redacted whole for every record of
		// the kind, those before the page included.
		if said, ok := claudeSteerSaid(looseString(rec.Subtype), rec.Content); ok {
			hookLine, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
			if hookLine = strings.TrimSpace(hookLine); hookLine != "" {
				if err := b.add(TranscriptEntry{Kind: EntryNote, At: at, Thread: b.thread}, hookLine); err != nil {
					return err
				}
			}
		}
	}
	return b.claudeNotes(ctx, line, at)
}

// notARecord says a line of the record, n bytes long, that is no record.
func (b *transcriptBuilder) notARecord(n int) error {
	return b.unread(fmt.Sprintf("a line of %s of the harness's record is not a record, "+
		"and was not read", humanSize(int64(n))))
}

// unshaped says a message of the harness's record of no shape this read knows.
func (b *transcriptBuilder) unshaped() error {
	return b.unread("a message of the harness's record has no shape this read knows, " +
		"and was not read")
}

// claudeNotes adds an entry for every note the engine sent the run that a
// record the harness wrote for itself carries, wherever in the record: the
// harness hands a note to its model as a hook's additional context, and which
// record keeps that, under which field, is the harness's own and moves between
// releases, where the heading every note opens with is the engine's. Read in
// the record's own order, so one read counts a record's notes as the next one
// does — and a value at a time, its caller asked at each whether it is still
// there: a record of a line's mebibytes can hold millions of values.
func (b *transcriptBuilder) claudeNotes(ctx context.Context, line []byte, at time.Time) error {
	if !bytes.Contains(line, []byte(noteHeadOpen)) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	// TO THE RECORD'S END: it read whole as a record already, so its end is
	// the only place this stops.
	for tok, err := dec.Token(); err == nil; tok, err = dec.Token() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		text, ok := tok.(string)
		if !ok {
			continue
		}
		if notesErr := b.notes(ctx, at, text); notesErr != nil {
			return notesErr
		}
	}
	return nil
}

// claudeMessage reads a record's message — an object, its content a string or
// a list of blocks — as kind, and says one of no shape this read knows rather
// than reading it as a message that said nothing.
func (b *transcriptBuilder) claudeMessage(ctx context.Context, raw json.RawMessage, kind EntryKind,
	at time.Time) error {

	if absent(raw) {
		return nil
	}
	if !jsonObject(raw) {
		return b.unshaped()
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if err := wireDecode(raw, &message); err != nil {
		return b.unshaped()
	}
	return b.claudeContent(ctx, message.Content, kind, at)
}

// claudeContent reads a message's content: a string is one entry of kind, and
// a list of blocks, read a block at a time ([eachClaudeBlock]), is an entry a
// block: its text as kind, a tool call, a tool's result. A thinking block is
// the model's working and no entry, and so is every block of a type this
// build does not know; content that is neither a string nor a list is said.
func (b *transcriptBuilder) claudeContent(ctx context.Context, raw json.RawMessage, kind EntryKind,
	at time.Time) error {

	raw = bytes.TrimSpace(raw)
	switch {
	case absent(raw):
		return nil
	case raw[0] == '"':
		var text string
		if err := wireDecode(raw, &text); err != nil {
			return b.unshaped()
		}
		return b.prose(ctx, kind, at, text)
	case raw[0] != '[':
		return b.unshaped()
	}
	return eachClaudeBlock(raw, func(block claudeBlock) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch block.Type {
		case "text":
			return b.prose(ctx, kind, at, block.Text)
		case "tool_use":
			b.called(block.ID, block.Name)
			return b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread,
				Tool: block.Name}, compactJSON(block.Input))
		case "tool_result":
			return b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread,
				Tool: b.answered(block.ToolUseID), Failed: block.IsError},
				claudeResultText(block.Content))
		}
		return nil
	})
}

// claudeTime is a record's `timestamp`, an RFC 3339 instant, or zero where it
// carries none this read can parse, or none an answer can carry
// ([carriedInstant]): the parser takes the year 0000 at any offset, which is a
// year before it in UTC.
func claudeTime(raw string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return carriedInstant(at.UTC())
}

// carriedInstant is at where an answer can carry it — in the years 0 to 9999,
// all RFC 3339 spells, which is how an answer's JSON writes an instant — and
// zero otherwise, so the entry is sent with no time.
//
// AN INSTANT AN ANSWER CANNOT CARRY FAILS THE PAGE THAT HOLDS IT: encoding the
// answer fails on it, which REST answers with a 500 and the socket with no
// answer at all, and every page after it is reached only through that one's
// cursor. And a box's own agent can write any instant into what its run packs.
func carriedInstant(at time.Time) time.Time {
	if year := at.Year(); year < 0 || year > 9999 {
		return time.Time{}
	}
	return at
}

// compactJSON is a call's input as one line of JSON, or as it came where it is
// not JSON.
func compactJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return string(raw)
	}
	return out.String()
}

// jsonNull is JSON's null, which a field may hold in place of a value.
var jsonNull = []byte("null")

// absent reports a value a record leaves out or holds null in.
func absent(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, jsonNull)
}

// jsonObject reports whether raw is a JSON object — a record or a message, as
// against a value of another kind, which would decode into one as nothing.
func jsonObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// ---- OpenCode ------------------------------------------------------------ //

// transcript reads an OpenCode archive ([openCodeArchiveFormat]).
func (OpenCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.openCodeExport(ctx, archive)
}

// openCodeKind is which kind of message one message of an OpenCode export is:
// its `type`, the tag of OpenCode 2's `SessionMessage.Info`
// (packages/schema/src/session-message.ts), whose eleven kinds are the same at
// 2.0.32 and 2.2.5. Who writes each, and what a transcript makes of it, is
// [transcriptBuilder.openCodeMessage]'s.
type openCodeKind string

// The kinds of message an OpenCode export holds.
const (
	openCodeUser             openCodeKind = "user"
	openCodeAssistant        openCodeKind = "assistant"
	openCodeCompaction       openCodeKind = "compaction"
	openCodeSynthetic        openCodeKind = "synthetic"
	openCodeSystem           openCodeKind = "system"
	openCodeSkill            openCodeKind = "skill"
	openCodeShell            openCodeKind = "shell"
	openCodeIdle             openCodeKind = "idle"
	openCodeAgentSwitched    openCodeKind = "agent-switched"
	openCodeModelSwitched    openCodeKind = "model-switched"
	openCodeLocationSwitched openCodeKind = "location-switched"
)

// The statuses a tool call settles in (`SessionMessage.ToolState`): with its
// answer, or on a failure. A compaction that settled with its summary is
// `completed` too.
const (
	openCodeCompleted  = "completed"
	openCodeCallFailed = "error"
)

// openCodeMessage is one message of an OpenCode export, as far as this
// package reads one: OpenCode 2's `SessionMessage.Info`, a flat object whose
// `type` says which kind it is ([openCodeKind]) and whose other fields are that
// kind's own, each absent from a kind that does not carry it. Decoded loosely
// ([transcriptBuilder.openCodeNext]), so a field a release retyped reads as
// its zero and the rest of the message as written.
//
// ONE TYPE FOR EVERY READER OF AN EXPORT: a session's transcript reads who
// wrote a message, when and what it says ([transcriptBuilder.openCodeMessage]),
// and the context a session carries what its steps' requests measured and
// whether each failed ([exportMessageInput]), both off this one, so no two
// readers of the one document disagree about what a message is, its kind or
// its failure. OpenCode 1.x's `{"info": {"role": …}, "parts": […]}` is no
// message of this document: every message of a 2.x export read in that shape
// had no role and no parts, and a transcript showed nothing.
type openCodeMessage struct {
	// Type is the message's kind.
	Type openCodeKind `json:"type"`

	// Time is when OpenCode recorded the message, in milliseconds since the
	// epoch: when a person's message arrived, a step began, a compaction
	// started.
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`

	// Text is a person's words on a user message — and OpenCode's own on its
	// synthetic, system and skill messages, which no entry reads.
	Text string `json:"text"`

	// Content is an assistant step's, raw: the model's text, its reasoning and
	// the tools it called, in order ([openCodeContent]). Raw, so content that
	// is no list is said rather than read as a step that wrote nothing.
	Content json.RawMessage `json:"content"`

	// Status is a compaction's — running, completed or failed — and Summary
	// what a completed one hands the model in place of the conversation
	// before it.
	Status  string `json:"status"`
	Summary string `json:"summary"`

	// Tokens is what an assistant step's request measured, and Error, raw,
	// the failure the step ended on: absent, or null, where it ended without
	// one. The context a session carries is read off both
	// ([exportMessageInput]).
	Tokens *openCodeTokens `json:"tokens"`
	Error  json.RawMessage `json:"error"`
}

// openCodeTokens is what one step's request measured (`TokenUsage.Info`):
// its prompt not served from a cache, and the cache's reads and writes beside
// it, which together are the whole prompt the request carried.
type openCodeTokens struct {
	Input int64 `json:"input"`
	Cache struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// openCodeContent is one item of an assistant step's content
// (`SessionMessage.AssistantContent`), told apart by `type`: `text` the model
// wrote, its `reasoning`, or a `tool` it called — the tool's Name, the call
// as it stands, and when OpenCode recorded it, in milliseconds since the
// epoch: when the model began it, when it ran, when it settled. Read an item
// at a time ([eachOpenCodeItem]).
type openCodeContent struct {
	Type  string        `json:"type"`
	Text  string        `json:"text"`
	Name  string        `json:"name"`
	State openCodeState `json:"state"`
	Time  struct {
		Created   int64 `json:"created"`
		Ran       int64 `json:"ran"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// openCodeState is a tool call as it stands (`SessionMessage.ToolState`), told
// apart by `status`: `streaming` while the model writes its input, which is
// then the text written so far; `running` once it is called; `completed`, its
// answer the list in `content`; or `error`, its failure in `error` and what
// it answered before it in `content`. Each raw, and read by what it holds.
type openCodeState struct {
	Status  string          `json:"status"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	Error   json.RawMessage `json:"error"`
}

// openCodeItemWire is one item of a step's content as its decode leaves it
// ([wireDecode]): each key an [openCodeContent] holds, by its exact name and
// raw, read into the item by [openCodeItemWire.item].
type openCodeItemWire struct {
	Type  json.RawMessage `json:"type"`
	Text  json.RawMessage `json:"text"`
	Name  json.RawMessage `json:"name"`
	State json.RawMessage `json:"state"`
	Time  json.RawMessage `json:"time"`
}

// item is the item w holds, DECODED LOOSELY, as the export's whole decode read
// one: a key of another kind than its field's reads as that field's zero
// value, a call's state or its times that are no object as none, and the rest
// of the item as written.
func (w openCodeItemWire) item() openCodeContent {
	item := openCodeContent{Type: looseString(w.Type), Text: looseString(w.Text), Name: looseString(w.Name)}
	if wireObject(w.State) {
		var state struct {
			Status  json.RawMessage `json:"status"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
			Error   json.RawMessage `json:"error"`
		}
		if wireDecode(w.State, &state) == nil {
			item.State = openCodeState{Status: looseString(state.Status), Input: state.Input,
				Content: state.Content, Error: state.Error}
		}
	}
	if wireObject(w.Time) {
		var times struct {
			Created   json.RawMessage `json:"created"`
			Ran       json.RawMessage `json:"ran"`
			Completed json.RawMessage `json:"completed"`
		}
		if wireDecode(w.Time, &times) == nil {
			item.Time.Created = looseInt(times.Created)
			item.Time.Ran = looseInt(times.Ran)
			item.Time.Completed = looseInt(times.Completed)
		}
	}
	return item
}

// eachOpenCodeItem hands each item of a step's content to each, in order, as
// it reads it, and returns each's first error, as it returned it, having read
// no item after it. A content that is no list has no items, and one that
// stops decoding part way has none after the fault, which no content of a
// message decoded whole can have ([transcriptBuilder.openCodeNext]).
//
// AN ITEM AT A TIME ([eachElement]), each decoded by its keys exactly as
// written ([threadDecoding]) and loosely by type ([openCodeItemWire.item]),
// and never the whole list first, which held an item's 160 bytes for every
// element the list held, however small the element, before any was looked
// at. An element that is no object is the zero item, as the whole decode left
// it, so each is handed every element.
func eachOpenCodeItem(content json.RawMessage, each func(openCodeContent) error) error {
	var (
		wire    openCodeItemWire
		stopped error
	)
	// The walk's own error is no reader's: the items before it were read.
	_ = eachElement(content, func(element json.RawMessage) error {
		wire = openCodeItemWire{}
		if wireObject(element) && wireDecode(element, &wire) != nil {
			wire = openCodeItemWire{}
		}
		stopped = each(wire.item())
		return stopped
	})
	return stopped
}

// openCodeExport reads the export document a message at a time, its other
// fields (the session's own) passed: `{"info": …, "messages": [{"type":
// "user", …}, {"type": "assistant", …}, …]}`.
//
// ONE MESSAGE IS HELD AT A TIME, to about [TranscriptBounds.LineBytes] — the
// bound a line of a record is read whole to — counted afresh before every
// step of the walk, so a message past it is said, and the read stops there: a
// JSON decoder cannot find where the next value begins without reading the
// one it is in.
//
// A DOCUMENT THAT ENDS BEFORE IT CLOSES ENDS PART WAY THROUGH, whatever the
// decoder calls it. Asked whether a list goes on at the input's end, the
// decoder this module builds with answers that it does and keeps the end as a
// syntax error — "unexpected end of JSON input", not the io.ErrUnexpectedEOF a
// value cut short reports — which the next read returns; read as any other
// syntax error, an export cut between two of its messages was said to hold no
// export document at all. So a syntax error met once the archive's bytes have
// all been read, with nothing the decoder has not parsed but separators, is the
// document's end coming too soon.
func (b *transcriptBuilder) openCodeExport(ctx context.Context, archive io.Reader) error {
	held := &heldValue{r: archive, max: b.bounds.LineBytes}
	dec := json.NewDecoder(held)
	err := b.pastHeld(held, b.openCodeDocument(ctx, dec, held))
	var syntax *json.SyntaxError
	switch {
	case !errors.As(err, &syntax):
		return err
	case held.ended && onlySeparators(dec.Buffered()):
		return fmt.Errorf("%w: %w", io.ErrUnexpectedEOF, err)
	}
	return fmt.Errorf("%w: %w", errNotExport, err)
}

// onlySeparators reports whether r holds nothing but what JSON puts between
// two tokens — whitespace, and the comma or colon a value awaits its next
// after.
func onlySeparators(r io.Reader) bool {
	var chunk [512]byte
	for {
		n, err := r.Read(chunk[:])
		if len(bytes.Trim(chunk[:n], " \t\r\n,:")) > 0 {
			return false
		}
		if err != nil {
			return true
		}
	}
}

// openCodeDocument walks the document, its messages handed to
// [transcriptBuilder.openCodeNext] one at a time.
func (b *transcriptBuilder) openCodeDocument(ctx context.Context, dec *json.Decoder, held *heldValue) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for held.reset(); dec.More(); held.reset() {
		key, keyErr := dec.Token()
		if keyErr != nil {
			return keyErr
		}
		held.reset()
		if key != "messages" {
			var passed json.RawMessage
			if passErr := dec.Decode(&passed); passErr != nil {
				return passErr
			}
			continue
		}
		if openErr := expectDelim(dec, '['); openErr != nil {
			return openErr
		}
		for held.reset(); dec.More(); held.reset() {
			if messageErr := b.openCodeNext(ctx, dec); messageErr != nil {
				return messageErr
			}
		}
		if closeErr := expectDelim(dec, ']'); closeErr != nil {
			return closeErr
		}
	}
	return expectDelim(dec, '}')
}

// openCodeNext decodes the next message of the list and reads it. A message
// that is no object is said; within one, a field whose type a release changed
// is skipped rather than losing the message, as the stream's events are
// ([decodeOpenCodeEvent]), and every key is read exactly as written
// ([openCodeMessageWire]), as its step's items are ([eachOpenCodeItem]).
func (b *transcriptBuilder) openCodeNext(ctx context.Context, dec *json.Decoder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if !jsonObject(raw) {
		return b.unread("a message of the export is not a message, and was not read")
	}
	var w openCodeMessageWire
	if err := wireDecode(raw, &w); err != nil {
		return err
	}
	return b.openCodeMessage(ctx, w.message())
}

// openCodeMessageWire is one message of an export as its decode leaves it
// ([wireDecode]): each key a person's transcript reads of an [openCodeMessage],
// by its exact name and raw, read into the message by
// [openCodeMessageWire.message].
type openCodeMessageWire struct {
	Type    json.RawMessage `json:"type"`
	Time    json.RawMessage `json:"time"`
	Text    json.RawMessage `json:"text"`
	Content json.RawMessage `json:"content"`
	Status  json.RawMessage `json:"status"`
	Summary json.RawMessage `json:"summary"`
}

// message is the message w holds, DECODED LOOSELY, as the export's whole
// decode read one: a key of another kind than its field's reads as that
// field's zero value, a time that is no object as none, and the rest of the
// message as written. Its tokens and its error are left as none: a person's
// transcript reads neither, and the context a session carries reads both off
// the export itself ([exportMessageInput]).
func (w openCodeMessageWire) message() openCodeMessage {
	m := openCodeMessage{Type: openCodeKind(looseString(w.Type)), Text: looseString(w.Text), Content: w.Content,
		Status: looseString(w.Status), Summary: looseString(w.Summary)}
	var at struct {
		Created json.RawMessage `json:"created"`
	}
	if wireObject(w.Time) && wireDecode(w.Time, &at) == nil {
		m.Time.Created = looseInt(at.Created)
	}
	return m
}

// pastHeld is how a walk of an export ended: a part of it past the bound one
// may hold is said, and ends the read where it was; every other end is
// returned as it came.
func (b *transcriptBuilder) pastHeld(held *heldValue, err error) error {
	if err == nil || !held.over {
		return err
	}
	if unreadErr := b.unread(fmt.Sprintf("a part of the export past %s was not read, and "+
		"nothing after it could be", humanSize(int64(b.bounds.LineBytes)))); unreadErr != nil {
		return unreadErr
	}
	b.cut = CutRead
	return errTranscriptPage
}

// openCodeMessage reads one message by its kind. A person's message is the
// brief — or a note, where it is one: the run's plugin admits a note as a
// person's message — read by its text alone, as a Claude Code message's text
// is read and an image beside it is not; an assistant's step is what its model
// wrote and every tool it called ([transcriptBuilder.openCodeStep]); and a
// completed compaction is its summary, which the model reads in place of the
// conversation before it.
//
// EVERY OTHER KIND IS OPENCODE'S OWN, and no entry, as the records Claude Code
// writes for itself are none ([transcriptBuilder.claudeLine]):
//
//   - synthetic: a notice OpenCode writes into the person's turn — a
//     background job's ending, a mode's reminder, an instruction file it
//     loaded — the counterpart of a record Claude Code marks as its own
//     (`isMeta`). Never searched for a note: OpenCode files a note as a
//     person's message, and a notice can carry what a tool printed, which is
//     never a note;
//   - system: the run's instructions again, handed to the model as an update
//     where they changed since the session last read them;
//   - skill: a skill a person activated, its body handed to the model in a
//     message of OpenCode's own, as Claude Code hands one in a record of its
//     own;
//   - shell: a command a person ran in the session, outside the model, whose
//     output reaches the model only as a synthetic notice;
//   - idle, agent-switched, model-switched and location-switched: an
//     execution's end, and the session's agent, model or directory changed.
//
// A compaction still running, or one that failed, is none either: it handed
// the model no summary.
//
// A MESSAGE OF NO KIND THIS READ KNOWS IS SAID, as a message of no shape it
// knows is. The kinds are the whole of what the releases this build drives
// write, so one outside them comes from a release this read was not written
// for, and its words may be what the model read: a gap never read as
// continuity.
func (b *transcriptBuilder) openCodeMessage(ctx context.Context, m openCodeMessage) error {
	at := unixMillis(m.Time.Created)
	switch m.Type {
	case openCodeUser:
		return b.prose(ctx, EntryBrief, at, m.Text)
	case openCodeAssistant:
		return b.openCodeStep(ctx, m.Content, at)
	case openCodeCompaction:
		if m.Status != openCodeCompleted {
			return nil
		}
		return b.prose(ctx, EntrySummary, at, m.Summary)
	case openCodeSynthetic, openCodeSystem, openCodeSkill, openCodeShell, openCodeIdle,
		openCodeAgentSwitched, openCodeModelSwitched, openCodeLocationSwitched:
		return nil
	}
	return b.unread("a message of the export of no kind this read knows was not read")
}

// openCodeStep reads an assistant step's content, an item at a time
// ([eachOpenCodeItem]): its text as the model's words, and each tool it called
// ([transcriptBuilder.openCodeTool]). Reasoning is the model's working and no
// entry, as Claude Code's thinking is none, and so is an item of a type this
// build does not know; content that is no list is said.
func (b *transcriptBuilder) openCodeStep(ctx context.Context, content json.RawMessage, at time.Time) error {
	content = bytes.TrimSpace(content)
	switch {
	case absent(content):
		return nil
	case content[0] != '[':
		return b.unread("a message of the export whose content is not a list was not read")
	}
	return eachOpenCodeItem(content, func(item openCodeContent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch item.Type {
		case "text":
			return b.prose(ctx, EntryModel, at, item.Text)
		case "tool":
			return b.openCodeTool(item, at)
		}
		return nil
	})
}

// openCodeTool reads one tool the model called: the call, its input as JSON,
// dated when it ran — or, one that never ran, when the model began it — and,
// once it settled, what answered it, dated when it did: a completed call's
// answer, or a failed one's failure, marked. A call OpenCode stopped while the
// model was still writing it, or while it ran, has no result.
func (b *transcriptBuilder) openCodeTool(item openCodeContent, at time.Time) error {
	called := firstInstant(unixMillis(item.Time.Ran), firstInstant(unixMillis(item.Time.Created), at))
	if err := b.add(TranscriptEntry{Kind: EntryToolCall, Tool: item.Name, At: called},
		openCodeCallInput(item.State.Input)); err != nil {
		return err
	}
	settled := firstInstant(unixMillis(item.Time.Completed), at)
	switch item.State.Status {
	case openCodeCompleted:
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: item.Name, At: settled},
			openCodeAnswer(item.State.Content))
	case openCodeCallFailed:
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: item.Name, At: settled, Failed: true},
			openCodeFailure(item.State))
	}
	return nil
}

// openCodeCallInput is a call's input as one line of JSON: the object it was
// called with, or, for a call whose input the model was still writing, the
// text it had written — as JSON where it parses so far, and as written where
// it does not.
func openCodeCallInput(raw json.RawMessage) string {
	var written string
	if wireDecode(raw, &written) == nil {
		return compactJSON(json.RawMessage(written))
	}
	return compactJSON(raw)
}

// openCodeAnswer is what a tool answered, the list `Tool.Content` is, read an
// item at a time ([toolAnswer]): each text item's text, a line apart, as a
// Claude Code tool's result reads ([claudeResultText]), and each file it
// answered with as the words a file reads in ([openCodeFileText]). "" for an
// answer that is no list.
func openCodeAnswer(raw json.RawMessage) string {
	return toolAnswer(raw, func(kind, text, mime string) (string, bool) {
		switch kind {
		case "text":
			return text, strings.TrimSpace(text) != ""
		case "file":
			return openCodeFileText(mime), true
		}
		return "", false
	})
}

// openCodeFileText is what a file a tool answered with reads as in text: an
// image as a Claude Code tool's image reads ([claudeImagePlaceholder]), and
// any other file as a file. Its bytes, inline or behind a URI, are no text.
func openCodeFileText(mime string) string {
	if strings.HasPrefix(mime, "image/") {
		return claudeImagePlaceholder
	}
	return "[a file]"
}

// openCodeFailure is what a failed call answered, as OpenCode hands it to the
// model: the failure's message — the object `SessionError.Error`, its class
// where it says nothing more — and then whatever the call answered before it
// failed. The failure is decoded loosely, as the message around it is, and by
// its keys exactly as written.
func openCodeFailure(state openCodeState) string {
	var failure struct {
		Type    json.RawMessage `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if wireObject(state.Error) && wireDecode(state.Error, &failure) != nil {
		failure.Type, failure.Message = nil, nil
	}
	said := cmp.Or(looseString(failure.Message), looseString(failure.Type))
	answered := openCodeAnswer(state.Content)
	switch {
	case answered == "":
		return said
	case said == "":
		return answered
	}
	return said + "\n" + answered
}

// unixMillis is an export's instant, or zero where it carries none, or none an
// answer can carry ([carriedInstant]).
func unixMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return carriedInstant(time.UnixMilli(ms).UTC())
}

// firstInstant is at, or otherwise where it is zero.
func firstInstant(at, otherwise time.Time) time.Time {
	if at.IsZero() {
		return otherwise
	}
	return at
}

// expectDelim reads the next token of dec, which must be want; where it is
// not, the failure names what KIND of token was there, never the token — a
// string of the export is the box's words, and its whole value.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if got, ok := tok.(json.Delim); !ok || got != want {
		return fmt.Errorf("%w: it holds %s where %q belongs", errNotExport, tokenKind(tok), string(want))
	}
	return nil
}

// tokenKind is what kind of JSON token tok is, in a few words: a delimiter by
// itself, which is one of four characters, and any other token by its kind.
func tokenKind(tok json.Token) string {
	switch t := tok.(type) {
	case json.Delim:
		return fmt.Sprintf("%q", string(t))
	case string:
		return "a string"
	case float64, json.Number:
		return "a number"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	}
	return "a value"
}

// ---- the readers a read goes through ------------------------------------ //

// sourceReader is the archive as its source hands it, keeping the source's own
// first failure: what the decoding made of a stream that stopped is never
// taken for the archive's fault.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}

// budgetReader is what it reads, held to the read budget: past it, every read
// answers errReadBudget and says the budget is spent.
type budgetReader struct {
	r     io.Reader
	left  int64
	spent bool
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// AT THE BUDGET EXACTLY, a reader that ends there was read whole:
		// only a byte past it spends the budget.
		var probe [1]byte
		if n, err := b.r.Read(probe[:]); n == 0 && errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		b.spent = true
		return 0, errReadBudget
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// heldValue bounds what a JSON decoder pulls into its buffer between two
// resets — one value of an export, about — so a message too large to hold is
// refused as it is read rather than once the decoder has held all of it; and
// says whether what it reads came to its end, which tells a document cut short
// from one that holds something else ([transcriptBuilder.openCodeExport]).
type heldValue struct {
	r     io.Reader
	max   int
	n     int
	over  bool
	ended bool
}

// reset starts the count over, for the next value.
func (h *heldValue) reset() { h.n = 0 }

func (h *heldValue) Read(p []byte) (int, error) {
	if h.n >= h.max {
		h.over = true
		return 0, errValuePastBound
	}
	if len(p) > h.max-h.n {
		p = p[:h.max-h.n]
	}
	n, err := h.r.Read(p)
	h.n += n
	if errors.Is(err, io.EOF) {
		h.ended = true
	}
	return n, err
}
