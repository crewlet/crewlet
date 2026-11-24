package executor

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
	"github.com/crewlet/crewlet/internal/words"
)

// A captured session, read back as the transcript a person reads.
//
// It is read here, beside the scripts that pack it ([Runner.OpenCapture]),
// because only the runner knows its harness's layout: Claude Code's archive is
// a gzipped tar of the session's JSONL transcript, its subagents' and its
// spilled tool results ([claudeArchiveFormat]); OpenCode's is the gzipped
// `opencode session export` document ([openCodeArchiveFormat]). A format this
// build does not pack is not read ([Carries]). Its entries ([EntryKinds],
// counted by [TranscriptVersion]) run in recorded order, the run's own then
// each subagent's; bookkeeping and thinking are none, and a part the read could
// not read is [EntryUnread], so a gap never reads as continuity. Every read is
// bounded ([Reader.Bounds], [TranscriptReadsAtOnce]) and redacted.

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

	// EntryLateCall is a call of this work that finished after the run that
	// made it had stopped waiting for it, as the engine handed it to the run
	// while it worked: the engine's heading, its words, the call's outcome and
	// the call ([lateCallHead]); or the line the run's own hook printed as it
	// handed only such calls over ([hookLineKind]).
	EntryLateCall EntryKind = "late_call"

	// EntryWake is a message somebody wrote about this work while the run
	// worked, as the engine handed it to the run: the engine's heading naming
	// its writer, where it was written, and the writer's words in their fence
	// ([wakeHeadOpen]); or the line the run's own hook printed as it handed
	// such messages and no note over ([hookLineKind]).
	EntryWake EntryKind = "wake"

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
	// read, said where it was: a line that is not a record, a line longer than
	// the read reads, a record or a message of no shape this read knows, a
	// transcript that is no plain file, a text whose redaction could not be
	// read, a call's result or a text joined from parts with a part written
	// out of order past the value bound, a record past the lines and elements
	// one read reads.
	EntryUnread EntryKind = "unread"
)

// EntryKinds are every kind an entry can be.
var EntryKinds = []EntryKind{
	EntryBrief, EntryNote, EntryLateCall, EntryWake, EntryModel, EntryToolCall, EntryToolResult, EntrySummary,
	EntryUnread,
}

// Valid reports whether a kind off the wire is one this build writes.
func (k EntryKind) Valid() bool { return slices.Contains(EntryKinds, k) }

// TranscriptVersion is the version of what this build counts as a transcript's
// entries. An ordinal holds under one version, so a read surface's cursor
// carries it, and a cursor of another (a build beside this one in a rolling
// upgrade) is read again from the start.
//
// RAISED BY ANY CHANGE THAT MOVES AN ORDINAL: a kind added, a record read that
// was not, one read as more entries or fewer, one no longer read. 10 reads a
// Codex or Hermes person's words past the value bound as one entry (R1), and a
// Hermes row by its keys' exact names, as the line walk reads them; what each
// earlier one moved is its commit's.
const TranscriptVersion = 10

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

	// Bytes is the size of what the entry's whole text redacts to (the bytes
	// [EntryPlace.TextFrom] counts in), never its size as recorded, which beside
	// a text shown whole says how long each value the redaction took out was.
	// Cut says the text was cut, so a reader says "the first 16 KiB of 2.1 MiB"
	// rather than presenting an opening as the whole.
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
	// budget, or the record held more lines and elements than one read reads
	// ([TranscriptBounds.ReadLines]). Its entries are all that can be shown.
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

	// TextFrom is where the text handed out begins, as a byte offset in the
	// entry's whole redaction: 0 for an opening, else where a match's window
	// begins ([windowAround]). Never the record's bytes: the window is cut from
	// the redaction, and a credential it replaced has no length a reader is shown.
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

	// ReadLines bounds the lines and elements one read frames: every line of
	// a Claude Code transcript and every block and result part in them, every
	// message, step item and answer part of an OpenCode export, and every
	// line within the line bound of another harness's record.
	ReadLines int `json:"read_lines"`

	// ReadBytes bounds the bytes one read takes as the archive inflates —
	// and, apart, the bytes the members of a tar hand the reader.
	ReadBytes int64 `json:"read_bytes"`
}

// The bounds every read the engine makes is held to ([TranscriptReadBounds]).
const (
	// TranscriptMaxEntries is the most entries one read hands out.
	//
	// TWO HUNDRED: a run's turn makes tens of calls and results, so one page
	// holds an ordinary run whole, far inside [TranscriptMaxBytes] at the median
	// entry's few hundred bytes, and about as many as a person reads before
	// paging.
	TranscriptMaxEntries = 200

	// TranscriptMaxBytes is the most text one read's entries hold together.
	//
	// ONE MEBIBYTE: sixty-four entries at [TranscriptEntryMaxBytes], and an
	// eighth of what one reply carries between nodes (queue.MaxPayloadBytes), so
	// an answer fits every transport the engine has, a browser's socket included.
	TranscriptMaxBytes = 1 << 20

	// TranscriptEntryMaxBytes is the most of one entry's text a read hands
	// out, cut on a rune boundary and marked.
	//
	// SIXTEEN KIBIBYTES: a model's longest message, a call's input and a result
	// of three hundred lines of code are whole within it; a longer text's opening
	// says what it was, and the entry states its whole size.
	TranscriptEntryMaxBytes = 16 << 10

	// TranscriptReadBytes is the most bytes one read takes as the archive
	// inflates, and the most the members of a tar hand the reader.
	//
	// What a kept session's scrub reads ([scrubPlainMax]), so an archive kept
	// reads whole and one that inflates past it stops there. A tar's members are
	// held to it APART, on what they hand out: a sparse member hands out holes
	// the archive does not hold, and unmetered, a member of 1.5 KiB could hold the
	// read for its declared size, up to an exbibyte.
	TranscriptReadBytes = scrubPlainMax

	// TranscriptReadLines is the most lines and elements one read frames.
	//
	// 2^20, the thread layer's bound on a member's lines ([threadLinesMax]):
	// eight times the lines of a session at the plaintext bound, so a real
	// session reads whole, and a record of empty elements to the read budget
	// costs a read seconds, not minutes. Past it the read ends where it is, said.
	// A person's read and a seat's take it; the take's meter and a holdings
	// reading, which hold no entry, read on.
	TranscriptReadLines = threadLinesMax

	// TranscriptValueMaxBytes is the most of one value a read holds: a decoded
	// string or name, or a call's input compacted. A longer one is streamed
	// from its first byte through a redaction into its entry, never held
	// ([streamedEntry]): its whole redaction's opening and size, as held.
	//
	// TWO MiB, where holding costs a read no more than streaming, measured
	// (BenchmarkAPersonsReadAtItsBounds): a held value is redacted whole, 13 to
	// 14 times the value live where its redaction lengthens most (`pwd=x` lines),
	// 27 to 29 MiB just within 2 MiB, 54 to 58 within 4, 107 to 115 within 8;
	// streamed, 16 to 29 at any length. Past it the engine writes only a brief
	// of over half a million tokens, read as one entry, never split into notes.
	TranscriptValueMaxBytes = 2 << 20
)

// TranscriptReadBounds are the bounds every read the engine makes is held to.
func TranscriptReadBounds() TranscriptBounds {
	return TranscriptBounds{
		Entries: TranscriptMaxEntries, Bytes: TranscriptMaxBytes,
		EntryBytes: TranscriptEntryMaxBytes, ReadLines: TranscriptReadLines,
		ReadBytes: TranscriptReadBytes,
	}
}

// The bounds a seat's read of its own sessions is held to
// ([SeatTranscriptBounds]): a person's lines and read bounds, the rest held to
// a tool's answer of at most 64 KiB, which the seat's model pays for on every
// later request (internal/agent/builtin's ToolAnswerBytes).
const (
	// SeatTranscriptMaxEntries is the most entries a seat's read hands out.
	//
	// FIFTY, a quarter of a person's page: a stretch of a run a model reads
	// through; a seat looking for something narrows a page by kind or by words
	// rather than paging the whole.
	SeatTranscriptMaxEntries = 50

	// SeatTranscriptMaxBytes is the most text a seat's page holds together.
	//
	// THIRTY-TWO KIBIBYTES, half a tool's answer: the other half is the first
	// page's row (about 11 KiB at its bounds), each entry's fence lines (about
	// 120 bytes) and the answer's JSON. The caller that encodes a page trims it
	// by entries where JSON escaping takes it past the answer's bound.
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
// held to: a person's lines and read bounds, and a page and an entry held to
// what a tool's answer carries. A person's read is unchanged by them
// ([TranscriptReadBounds]).
func SeatTranscriptBounds() TranscriptBounds {
	return TranscriptBounds{
		Entries: SeatTranscriptMaxEntries, Bytes: SeatTranscriptMaxBytes,
		EntryBytes: SeatTranscriptEntryMaxBytes, ReadLines: TranscriptReadLines,
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

	// Terms keeps only entries whose whole text holds every one of them, as
	// internal/textindex makes and matches terms (Terms, Reading.FindIn; a
	// call's text through its JSON escapes, [readingOf]), in what the
	// redaction shows of the entry's own words ([transcriptBuilder.held]), each
	// handed out as the window around the earliest ([windowAround]). None
	// keeps every entry, each as its opening.
	Terms []string
}

// keepsKind reports whether f keeps an entry of kind: one of its kinds, or
// [EntryUnread] whatever it asks, since nothing can say what kind a gap was,
// and passed over it would read as continuity.
func (f TranscriptFilter) keepsKind(kind EntryKind) bool {
	return len(f.Kinds) == 0 || kind == EntryUnread || slices.Contains(f.Kinds, kind)
}

// TranscriptReadsAtOnce is the most transcripts this process decodes at once,
// whichever socket or REST caller asks for them ([ReadTranscript]).
//
// TWO, BY WHAT A READ COSTS AT ITS BOUNDS, measured: at most
// [TranscriptReadHeldBytes] live, so two hold 64 MiB (96 MiB with the heap's
// room under the engine's memory limit, [MemoryPools]), each one core's work for
// up to [PersonTranscriptReadWorst] (a seat's for words 58 s); one slot would
// queue every reader behind the longest archive, and more would take more of a
// node's cores than a page is worth. A seat's read takes the seats' one slot
// ([SeatTranscriptReadsAtOnce]); a reading has its own ([HoldingsReadsAtOnce]).
const TranscriptReadsAtOnce = 2

// TranscriptReadHeldBytes is the most one read holds at its bounds, measured
// (BenchmarkAPersonsReadAtItsBounds, every harness's every case, one a process
// on two cores of the build machine, four runs): 32 MiB live at GOGC=10, a
// Claude Code value of a key's armour and 200-byte tokens streamed to the read
// budget; OpenCode's at most 31, every credential shape streamed to it; every
// other harness's at most 29; its heap peaking at 58 MiB at the default GOGC,
// every credential shape streamed to the read budget.
const TranscriptReadHeldBytes = 32 << 20

// PersonTranscriptReadWorst is the longest a person's read takes at its
// bounds, of what harnesses and ordinary tools write, measured likewise: one
// value of `password:` lines streamed to the read budget, 41.2 to 46.1 s for
// 240 MiB, slowest two at once on two cores (49.2 s at the budget), and a
// tenth more for a node busier than that machine. Every credential shape
// takes 33 s. Dense tokens and a kept session's text, mostly random bytes,
// pack to 179 and 177 MiB of 240 ([keptGzipLevel]), so no archive within its
// 48 MiB bound holds the budget's worth of them. A value of `pwd=x` lines, a
// password's value every six bytes the budget long, which no harness or tool
// writes, takes 98 s (129 escaped in a JSON string): it sets no wait, and a
// read of it fails closed once it runs past [TranscriptAnsweredWithin].
const PersonTranscriptReadWorst = 55 * time.Second

// TranscriptAnsweredWithin is how long a person's read of a transcript can
// take to answer: [PersonTranscriptReadWorst] and before it one slot's wait. A
// read queued behind [TranscriptReadsAtOnce] others waits for the first of them
// to end, a person's at most, since seats hold at most
// [SeatTranscriptReadsAtOnce] of the slots (measured likewise behind two such
// reads: 83.1 to 87.8 s for 240 MiB, 93.6 s at the budget). The dashboard
// waits this long (TRANSCRIPT_QUERY_TIMEOUT_MS, held to it), and the socket
// ends the read there (the frame's `wait_ms`): a read past it fails closed,
// answered nothing.
const TranscriptAnsweredWithin = 2 * PersonTranscriptReadWorst

// transcriptReads holds a slot for each transcript this process is reading,
// at most [TranscriptReadsAtOnce]: taken before a read opens the session's
// object, and given back once the read has closed it ([readTranscript]).
var transcriptReads = make(chan struct{}, TranscriptReadsAtOnce)

// SeatTranscriptReadsAtOnce is the most of the process's
// [TranscriptReadsAtOnce] slots seats' reads hold at once: a seat's read takes
// one of these, and then one of the process's.
//
// ONE, INSIDE THE PROCESS'S TWO, never a pool beside them, which would double
// what reads hold at their worst: a seat's read holds what a person's does (up
// to [TranscriptReadHeldBytes]), for words a little longer (58 s at the budget
// beside a person's read), and one slot is always a person's while a seat
// reads.
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
// not one its format describes; Why says which way, in one of this file's own
// sentences naming the layer at fault, the compression or the layout.
//
// NEVER THE DECODER'S WORDS, which quote what they read, and a box's agent can
// write anything into what its run packs: the decoder's error is kept beneath
// Why for errors.Is and errors.As, and Error says Why alone.
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
// format, which open opens, from its entry from on, held to
// [TranscriptReadBounds]: every entry's text redacted and then cut, the
// page's cut said ([Transcript.Cut]). Each page is read from the archive's
// start, so a read costs what the archive does, not how deep a reader pages.
// It stops, with ctx's error, when ctx is done, checked a line at a time.
//
// open IS CALLED ONLY ONCE A [TranscriptReadsAtOnce] SLOT IS HELD, since an
// opened object holds its store's connection while it waits, and the slot is
// given back only once the archive is closed. A failure of open or of the
// source is returned wrapped; [ErrUnsupportedArchive] is refused before a
// slot is taken; a malformed archive is a [MalformedArchiveError].
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

	return newTranscriptBuilder(from, bounds, filter).decode(ctx, c, archive)
}

// decode is one read of archive, which c's harness packed, into b.
func (b *transcriptBuilder) decode(ctx context.Context, c carrier, archive io.Reader) (Transcript, error) {
	src := &sourceReader{r: archive}
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
	return Transcript{Entries: b.entries, Places: b.places, From: b.from, Next: b.next, Cut: b.cut}, nil
}

// transcriptBuilder is one read's page as it is built: every entry the
// archive holds is counted, and those from the page's first ordinal on are
// shaped and kept until the page holds all it hands out.
type transcriptBuilder struct {
	from   int
	bounds TranscriptBounds
	filter TranscriptFilter

	// valueMax is the most of one text an entry is shaped from held; a longer
	// one is streamed ([transcriptBuilder.add]). redactor, where a test sets
	// one, is what a streamed text is redacted through in place of a
	// [redact.Stream].
	valueMax int
	redactor func(io.Writer) entryRedaction

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

// newTranscriptBuilder is a read's builder from the entry from on, held to
// bounds, narrowed by filter, and holding a value to [TranscriptValueMaxBytes].
func newTranscriptBuilder(from int, bounds TranscriptBounds, filter TranscriptFilter) *transcriptBuilder {
	return &transcriptBuilder{from: from, next: from, bounds: bounds, filter: filter,
		valueMax: TranscriptValueMaxBytes, entries: []TranscriptEntry{}, places: []EntryPlace{},
		calls: map[string]string{}, inflated: budgetReader{left: bounds.ReadBytes},
		handed: budgetReader{left: bounds.ReadBytes}}
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

// readingOf is how an entry of kind writes its words. A tool call's text is its
// input as JSON ([compactJSON]), read through its escapes ([textindex.JSON]),
// since read as written a `\n` glues its `n` to the next word; every other text
// is read as written ([textindex.Plain]), where a Windows path's `\new` would
// otherwise lose its `n` to a line break.
func readingOf(kind EntryKind) textindex.Reading {
	if kind == EntryToolCall {
		return textindex.JSON
	}
	return textindex.Plain
}

// shape is an entry as it is handed out, its text already cut from its
// redaction: the size it states ([TranscriptEntry.Bytes]), its names redacted
// and bounded, and EACH STRING CLONED: an unchanged cut is a substring of a
// held value of up to [TranscriptValueMaxBytes], which a page would keep alive.
func shape(e TranscriptEntry, size int) TranscriptEntry {
	e.Bytes = size
	e.Thread, _, _ = entryOpening(e.Thread, transcriptNameMaxBytes)
	e.Tool, _, _ = entryOpening(e.Tool, transcriptNameMaxBytes)
	e.Text, e.Thread, e.Tool = strings.Clone(e.Text), strings.Clone(e.Thread), strings.Clone(e.Tool)
	return e
}

// entryOpening is text redacted WHOLE and then cut to at most bound bytes,
// whether it was cut, and the size of the whole redaction: an opening of what
// the whole text redacts to ([redact.Prefix]), read in bounded memory. A text
// held is at most [TranscriptValueMaxBytes]; a longer one is streamed
// ([streamedEntry]).
func entryOpening(text string, bound int) (string, bool, int) {
	opening, whole := redact.Prefix(text, bound+1)
	return textcut.Within(opening, bound), whole > bound, whole
}

// windowAround is the window of shown (an entry's whole redaction) around the
// byte offset at: at most bound bytes, a third of it before at as
// textindex.Snippet places one, marked where shown runs on; where it begins;
// and whether it is less than all of shown.
//
// Cut from the whole redaction, neither end cuts a credential, and NEVER A
// MARKER IN PART, which reads as the entry's own words ("ACTED:github-token]"):
// either end inside markers moves past them, away from at ([redact.MarkersAround]),
// so the window still holds the word it was found by.
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

// unread adds the entry saying a part of the record was not read.
func (b *transcriptBuilder) unread(what string) error {
	return b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, what)
}

// called remembers which tool the call id is of, for its result, but never an
// id or a name past [transcriptNameMaxBytes], which no harness writes and which
// would hold strings of any length until their results came. Such a call's
// result names no tool, as one past [maxPendingCalls] does.
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

// ---- notes and late calls ----------------------------------------------- //

// headingOpen opens every heading the engine hands a running job words under:
// a person's note's ([noteHeadOpen]), a late call's ([lateCallHead]) and a
// wake's ([wakeHeadOpen]).
const headingOpen = "## A "

// fenceOpen and fenceClose open the two marker lines of a fence around words
// somebody else wrote (internal/provenance's Fence), which the words inside it
// never spell, the fence escaping them. A heading between the two is the
// writer's, never the engine's ([fenceEnd]).
const (
	fenceOpen  = "[untrusted:"
	fenceClose = "[/untrusted:"
)

// headingAt is where the first heading in text at or after from opens, where
// its line ends and which kind it heads ([headingKind]), and false where there
// is none; never one inside a fence ([fenceEnd]), which is its writer's words.
// Every heading opens so wherever a harness filed it, so it is found by it,
// never by a record shape a release can move. from is a line's start, or the
// line break before one.
//
// A HEADING STARTS ITS LINE, or follows nothing on it but a harness's framing
// ([headingFraming]), so words a tool wrote into a line (an error the engine
// quotes in a late call's) never head an entry. EACH LINE IS READ ONCE, at its
// first opening: re-scanning from every opening made a line of [maxLineBytes]
// openings cost tens of core-minutes.
func headingAt(text string, from int) (at, end int, kind EntryKind, ok bool) {
	for from < len(text) {
		i := strings.Index(text[from:], headingOpen)
		if i < 0 {
			return 0, 0, "", false
		}
		at = from + i
		if past, inside := fenceEnd(text, from, at); inside {
			from = past
			continue
		}
		start := from + strings.LastIndexByte(text[from:at], '\n') + 1
		end = len(text)
		if n := strings.IndexByte(text[at:], '\n'); n >= 0 {
			end = at + n
		}
		if kind, ok = headingKind(text[at:end]); ok && headingFraming().MatchString(text[start:at]) {
			return at, end, kind, true
		}
		from = end + 1
	}
	return 0, 0, "", false
}

// headingKind is the kind of entry line heads, and false for no heading.
func headingKind(line string) (EntryKind, bool) {
	switch {
	case line == lateCallHead:
		return EntryLateCall, true
	case len(line) > len(noteHeadOpen)+len(noteHeadClose) && strings.HasPrefix(line, noteHeadOpen) &&
		strings.HasSuffix(line, noteHeadClose):
		return EntryNote, true
	case len(line) > len(wakeHeadOpen)+len(wakeHeadClose) && strings.HasPrefix(line, wakeHeadOpen) &&
		strings.HasSuffix(line, wakeHeadClose):
		return EntryWake, true
	}
	return "", false
}

// fenceTokenMax bounds the token a fence's opening names before its first
// space: a provenance boundary's sixteen characters, and room. An opening
// naming a longer one, or none, opens no fence.
const fenceTokenMax = 64

// fenceEnd reports whether at, in text past from, falls inside a fence that
// opens at or after from, and where that fence ends: past its closing line, or
// at text's end where it never closes. Each opening is read once and the scan
// only moves on, so a text of many openings costs about its length.
func fenceEnd(text string, from, at int) (past int, inside bool) {
	for from < at {
		i := strings.Index(text[from:at], fenceOpen)
		if i < 0 {
			return 0, false
		}
		open := from + i
		head := text[open+len(fenceOpen) : min(len(text), open+len(fenceOpen)+fenceTokenMax)]
		token, _, named := strings.Cut(head, " ")
		if !named || token == "" {
			from = open + len(fenceOpen)
			continue
		}
		closing := strings.Index(text[open:], fenceClose+token+"]")
		if closing < 0 {
			return len(text), true
		}
		past = open + closing + len(fenceClose) + len(token) + 1
		if past > at {
			return past, true
		}
		from = past
	}
	return 0, false
}

// headedIn is every note, wake and late call text carries, by kind, each from its
// heading to the next one's or to text's end — a hook's answer joins what one
// step is handed in one text ([ClaudeCode.hookAnswer]) — trimmed of the system
// reminder a harness closes them in, in order and one at a time, so a reader
// that stops reads no further.
func headedIn(text string) iter.Seq2[EntryKind, string] {
	return func(yield func(EntryKind, string) bool) {
		at, end, kind, ok := headingAt(text, 0)
		for ok {
			next, nextEnd, nextKind, more := headingAt(text, end)
			stop := len(text)
			if more {
				stop = next
			}
			words := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[at:stop]), "</system-reminder>"))
			if !yield(kind, words) {
				return
			}
			at, end, kind, ok = next, nextEnd, nextKind, more
		}
	}
}

// headingFraming is all a harness puts before a heading it hands its model:
// a system reminder's opening tag, Claude Code's `<event> hook additional
// context:` ([claudeHookContext]), and Qwen Code's words before a drained
// message ([qwenNoteFraming]).
var headingFraming = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\s*(?:<system-reminder>\s*)?(?:\w+` +
		regexp.QuoteMeta(claudeHookContext) + `\s*|` + regexp.QuoteMeta(qwenNoteFraming) + `)?$`)
})

// opensWithHeading reports text that is a note, a wake or a late call — its first
// heading has nothing before it but what a harness frames one with — as
// against a brief that carries a note further down, as a launch's brief does
// with the notes that arrived before it.
func opensWithHeading(text string) bool {
	at, _, _, ok := headingAt(text, 0)
	return ok && headingFraming().MatchString(text[:at])
}

// ---- what the readers share -------------------------------------------- //

// carriedInstant is at where an answer's RFC 3339 JSON can carry it (the
// years 0 to 9999), and zero otherwise, so the entry is sent with no time. A
// box's agent can write any instant, and one the encoding fails on fails its
// page (a REST 500, no socket answer) and every page reached by its cursor.
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

// eachKeyInPlace is [eachKey] handing each value as the sub-slice of object
// it is, by its offset: object is read in place, never copied, so a value
// kept outlives the walk as long as object does.
func eachKeyInPlace(object json.RawMessage, each func(name []byte, value json.RawMessage) error) error {
	return eachKey(object, func(name []byte, value json.RawMessage, at int) error {
		end := at + len(value)
		return each(name, object[at:end:end])
	})
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

// lastByte is r, remembering the last byte it handed out.
type lastByte struct {
	r    io.Reader
	last byte
	read bool
}

func (l *lastByte) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.last, l.read = p[n-1], true
	}
	return n, err
}
