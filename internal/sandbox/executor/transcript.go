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
	EntryBrief, EntryNote, EntryLateCall, EntryModel, EntryToolCall, EntryToolResult, EntrySummary,
	EntryUnread,
}

// Valid reports whether a kind off the wire is one this build writes.
func (k EntryKind) Valid() bool { return slices.Contains(EntryKinds, k) }

// TranscriptVersion is the version of what this build counts as a
// transcript's entries. An ordinal is an ordinal under one version, so a read
// surface's cursor carries it, and a cursor of another (a build beside this one
// in a rolling upgrade) is read again from the start rather than continued at
// an ordinal that names another entry here.
//
// RAISED BY ANY CHANGE THAT MOVES AN ORDINAL: a kind added, a record read that
// was not, one read as more entries or fewer, one no longer read. At 2 an
// OpenCode export's messages are read as OpenCode 2's kinds; at 3 every key is
// read exactly as written, as the harnesses' own JSON.parse reads it; at 4 a
// late call the engine hands a running job is an entry of its own.
const TranscriptVersion = 4

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

	// Bytes is the size of the entry's whole text as its redaction shows it (the
	// bytes [EntryPlace.TextFrom] counts in) wherever the read redacted all of it:
	// a match's hit, and any text short enough to take whole ([redactedWithin]).
	// Of a longer text it is the recorded size rounded up to a KiB
	// ([statedSize]). Cut says the text was cut, so a reader says "the first 16
	// KiB of 2.1 MiB" rather than presenting an opening as the whole. NEVER THE
	// RECORDED SIZE TO THE BYTE: beside a text shown whole, that says how long
	// each value the redaction took out was.
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
	// 256 MiB: eight times the most an archive is stored as (internal/executorkeep's
	// SessionMaxBytes, 32 MiB), gzip's ratio over JSON, so an ordinary archive
	// reads whole and one that inflates past it stops there. A tar's members are
	// held to it APART, on what they hand out: a sparse member hands out holes
	// the archive does not hold, and unmetered, a member of 1.5 KiB could hold the
	// read for its declared size, up to an exbibyte.
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
// ([SeatTranscriptBounds]): a person's line and read bounds, the rest held to
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

	// Terms keeps only entries whose whole text holds every one of them, as
	// internal/textindex makes and matches terms (Terms, Reading.FindIn; a
	// call's text through its JSON escapes, [readingOf]), in what the
	// redaction shows of the entry's own words ([transcriptBuilder.add]), each
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
// TWO, BY WHAT ONE READ CAN HOLD: a line read whole at up to 32 MiB
// ([TranscriptBounds.LineBytes]), its gathering slice's growth and its decoded
// copy, about 100 MiB, so about 200 MiB for two. Nothing else bounds this: a
// node serves every tab open on it, and REST bounds none. One slot would queue
// every reader behind the longest archive. A seat's read for words holds a
// third more, a relaunch's [Runner.ReadHoldings] far less (43 MiB measured at
// most), both in the seats' one slot ([SeatTranscriptReadsAtOnce]); a
// collection's pass in neither ([CollectionLargeReadsAtOnce]). A read past
// them waits holding no decoder or opened object; a slot frees within one read.
const TranscriptReadsAtOnce = 2

// transcriptReads holds a slot for each transcript this process is reading,
// at most [TranscriptReadsAtOnce]: taken before a read opens the session's
// object, and given back once the read has closed it ([readTranscript]).
var transcriptReads = make(chan struct{}, TranscriptReadsAtOnce)

// SeatTranscriptReadsAtOnce is the most of the process's
// [TranscriptReadsAtOnce] slots seats' reads hold at once: a seat's read takes
// one of these, and then one of the process's.
//
// ONE, INSIDE THE PROCESS'S TWO, never a pool beside them, which would double
// what reads hold at their worst: one slot is always a person's while a seat
// reads (a collection's pool is apart, [CollectionLargeReadsAtOnce]). A
// relaunch's [Runner.ReadHoldings] takes it too, inside its own wait; one that
// waits past it tells its session every call after its save point, never
// wrong. On two dedicated cores (BenchmarkAReadingAtItsBounds) an ordinary 1.6
// MiB archive reads in 72 to 88 ms, so 32 relaunches in 2.3 to 2.8 s, 255 MiB
// of ordinary lines in 3.1 to 3.2 s, the slowest at its bounds 21.4 to 22.6 s.
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

// add counts one entry and keeps it on the page (raw is its recorded text)
// where the filter keeps it, answering errTranscriptPage, the cut said, once
// the page is full. An entry before the page is only counted: redaction, the
// expensive part, is for what a reader is shown.
//
// A MATCH IS DECIDED ON WHAT ITS READER IS SHOWN: the whole redaction's own
// words, never a marker's ([redact.OwnWords]), since on the recorded text a
// term inside a credential confirmed a guess at it. MayHold on the recorded
// text, costing no redaction and failing no hit, picks what is redacted. A hit
// is decided before the page's bound, lest a stop say what the redaction took
// out. [EntryUnread] is kept, never searched: its text is this package's
// account of the gap.
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
// decoded record of up to a line's 32 MiB, which a page would keep alive.
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
// A KIBIBYTE: such a text runs past the entry's bound plus twice the longest
// credential (a private key's block, up to 64 KiB), so a reader's "first 8 KiB
// of 2.1 MiB" moves under one percent, and a redacted value moves the size
// only where it carries the text across a kibibyte, never by its own length.
const statedSizeGrain = 1 << 10

// redactedWithin is text redacted and then cut to at most bound bytes, whether
// it was cut, and the size of the whole text's redaction where the read made
// it, else -1: always an OPENING OF WHAT THE WHOLE TEXT REDACTS TO.
//
// A credential beginning inside the bound ends within [redactContext] past it
// (a private key's block is the longest), so only that head is redacted, inside
// a window [redactContext] longer still, where any credential begun in the head
// ends: the window's redaction of the head is the whole text's. The rest, up to
// [maxLineBytes], is never scanned. ONLY WHAT THE HEAD REDACTS TO IS HANDED
// OUT, never the window's end, where a credential may be cut and so missed: as
// much of the window's redaction as the head's own agrees with, so a credential
// running across the head's end is not handed out at all.
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

// prose adds one entry of words — a brief, the model's, a summary — unless it
// holds none. A brief that is a note or a late call — its words open with
// either's heading, inside nothing but what a harness wraps one in — is what
// it carries, each by its kind ([opensWithHeading]).
func (b *transcriptBuilder) prose(ctx context.Context, kind EntryKind, at time.Time, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if kind == EntryBrief && opensWithHeading(text) {
		return b.headed(ctx, at, text)
	}
	return b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, text)
}

// headed adds an entry for every note and late call text carries
// ([headedIn]), as it finds each: a page that holds all it hands out stops the
// search there, and so does a caller that left — one text can carry a heading
// on every line of the mebibytes a line may run to.
func (b *transcriptBuilder) headed(ctx context.Context, at time.Time, text string) error {
	for kind, words := range headedIn(text) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	return nil
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

// headingOpen opens both headings the engine hands a running job words under:
// a person's note's ([noteHeadOpen]) and a late call's ([lateCallHead]).
const headingOpen = "## A "

// headingAt is where the first heading in text at or after from opens, where
// its line ends and which kind it heads, and false where there is none: a
// note's (its opening, a sender's name, [noteHeadClose] closing the line) or a
// late call's ([lateCallHead], the whole line). Every one opens so wherever a
// harness filed it, so it is found by its heading, never by a record shape a
// release can move. from is a line's start, or the line break before one.
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
		start := from + strings.LastIndexByte(text[from:at], '\n') + 1
		end = len(text)
		if n := strings.IndexByte(text[at:], '\n'); n >= 0 {
			end = at + n
		}
		if kind, ok = headingKind(text[at:end]); ok && headingFraming.MatchString(text[start:at]) {
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
	}
	return "", false
}

// headedIn is every note and late call text carries, by kind, each from its
// heading to the next one's or to text's end — a hook's answer joins what one
// step is handed in one text (ClaudeHookAnswer) — trimmed of the system
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
// a system reminder's opening tag, and Claude Code's `<event> hook additional
// context:` ([claudeHookContext]).
var headingFraming = regexp.MustCompile(`^\s*(?:<system-reminder>\s*)?(?:\w+` +
	regexp.QuoteMeta(claudeHookContext) + `\s*)?$`)

// opensWithHeading reports text that is a note or a late call — its first
// heading has nothing before it but what a harness frames one with — as
// against a brief that carries a note further down, as a launch's brief does
// with the notes that arrived before it.
func opensWithHeading(text string) bool {
	at, _, _, ok := headingAt(text, 0)
	return ok && headingFraming.MatchString(text[:at])
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
// own hook prints as it hands the model notes or late calls ([claudeSteerSaid]).
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
// at a time, passing every other member unread. What a member hands out is
// metered apart from what the archive inflates to ([TranscriptReadBytes]), and
// every member is read through ONE buffer, reset for each: an archive may hold
// hundreds of thousands of empty members.
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
			// A TRANSCRIPT THAT IS NO PLAIN FILE (a link, a directory) is not read
			// through: it names another member or a path outside the archive. Said where
			// it was.
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

// claudeLine reads one record: a person's turn (the brief, a note, a
// compaction summary, or the results of the model's calls) or the model's
// own. Every other record is the harness's own, an entry only for the notes
// and late calls it carries ([transcriptBuilder.claudeNotes]).
//
// A LINE THAT IS NO OBJECT IS NO RECORD, rather than decoding to nothing and
// vanishing. A record is decoded loosely, as the stream is
// ([decodeOpenCodeEvent]): a retyped field is skipped, not the record. It is
// read by its keys exactly as written ([threadDecoding]), its message and
// blocks alike, as the run's event stream reads the same record
// ([claudeEventWire]).
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
		// THE LINE THE RUN'S OWN HOOK PRINTS as it hands the model a note or a
		// late call (the live `[steer]`) is an entry of its kind: a release that
		// keeps no hook context leaves it their one trace. Its own line as
		// recorded, never the live view's rendering, which is cut and counts
		// lines in words the harness never wrote.
		if said, ok := claudeSteerSaid(looseString(rec.Subtype), rec.Content); ok {
			hookLine, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
			if hookLine = strings.TrimSpace(hookLine); hookLine != "" {
				entry := TranscriptEntry{Kind: hookLineKind(hookLine), At: at, Thread: b.thread}
				if err := b.add(entry, hookLine); err != nil {
					return err
				}
			}
		}
	}
	return b.claudeNotes(ctx, line, at)
}

// hookLineKind is the kind of entry the line the run's own hook printed is:
// a late call where it names such calls alone ([hookCallOne], a count and
// [hookCallsMany]), and a note otherwise, as every line naming a note opens
// with it.
func hookLineKind(line string) EntryKind {
	count, rest, _ := strings.Cut(line, " ")
	if strings.HasPrefix(line, hookCallOne) ||
		(count != "" && strings.Trim(count, "0123456789") == "" && strings.HasPrefix(rest, hookCallsMany)) {
		return EntryLateCall
	}
	return EntryNote
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

// claudeNotes adds an entry for every note and late call a record the harness
// wrote for itself carries, wherever in it: which record and field keep a
// hook's additional context moves between releases, while the headings are
// the engine's. Read in the record's order, so every read counts them alike,
// and a value at a time, checking ctx: a line can hold millions of values.
func (b *transcriptBuilder) claudeNotes(ctx context.Context, line []byte, at time.Time) error {
	if !bytes.Contains(line, []byte(noteHeadOpen)) && !bytes.Contains(line, []byte(lateCallHead)) {
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
		if notesErr := b.headed(ctx, at, text); notesErr != nil {
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

// openCodeMessage is one message of an OpenCode export: OpenCode 2's
// `SessionMessage.Info`, a flat object whose `type` is its kind
// ([openCodeKind]) and whose other fields are that kind's own. Decoded
// loosely ([transcriptBuilder.openCodeNext]): a retyped field reads as zero.
//
// ONE TYPE FOR EVERY READER OF AN EXPORT, a session's transcript
// ([transcriptBuilder.openCodeMessage]) and the context a session carries
// ([measuredPrompt]) alike, off an export or a page of its message list, so
// no two readers disagree about a message's kind or failure. OpenCode 1.x's
// `{"info": …, "parts": …}` is not this document.
type openCodeMessage struct {
	// Type is the message's kind.
	Type openCodeKind `json:"type"`

	// Time is when OpenCode recorded the message, in milliseconds since the
	// epoch: when a person's message arrived, a step began, a compaction
	// started; and, on a step, when it settled, zero while it is in flight,
	// which only the measure of its request reads ([measuredPrompt]).
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
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
	// one. The context a session carries is read off both ([measuredPrompt]).
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

// eachOpenCodeItem hands each item of a step's content to each, in order, and
// returns each's first error, reading no item after it. Content that is no
// list has no items; one that stops decoding part way has none past the fault
// ([transcriptBuilder.openCodeNext] decoded the message whole).
//
// AN ITEM AT A TIME ([eachElement]), by keys exactly as written
// ([threadDecoding]) and loosely by type ([openCodeItemWire.item]), never the
// whole list first, which held 160 bytes per element before any was read. An
// element that is no object is the zero item, so each is handed every one.
func eachOpenCodeItem(content json.RawMessage, each func(openCodeContent) error) error {
	var (
		wire    openCodeItemWire
		stopped error
	)
	// The walk's own error is no reader's: the items before it were read.
	_ = eachElement(content, func(element json.RawMessage, _ int) error {
		wire = openCodeItemWire{}
		if wireObject(element) && wireDecode(element, &wire) != nil {
			wire = openCodeItemWire{}
		}
		stopped = each(wire.item())
		return stopped
	})
	return stopped
}

// openCodeExport reads the export document a message at a time, passing its
// other fields: `{"info": …, "messages": [{"type": "user", …}, …]}`.
//
// ONE MESSAGE IS HELD AT A TIME, to about [TranscriptBounds.LineBytes],
// counted afresh before every step of the walk; a message past it is said and
// ends the read, since a JSON decoder cannot find the next value without
// reading the one it is in.
//
// A DOCUMENT THAT ENDS BEFORE IT CLOSES ENDS PART WAY THROUGH: at the input's
// end the decoder reports a syntax error ("unexpected end of JSON input"), not
// io.ErrUnexpectedEOF, so a syntax error once the archive is all read, with
// only separators unparsed, is the document cut short, not "no export".
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
// message as written. Its tokens, its error and when a step settled are left
// as none: a person's transcript reads none of them, and the context a session
// carries reads them off the export itself ([exportMessageAt]).
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

// openCodeMessage reads one message by its kind: a person's is the brief, a
// note or a late call (the run's plugin admits both as one), by its text; an
// assistant's step is the model's words and calls
// ([transcriptBuilder.openCodeStep]); a completed compaction is its summary.
//
// EVERY OTHER KIND IS OPENCODE'S OWN and no entry, as Claude Code's own records
// are none: synthetic notices (never searched for a note: one can carry what a
// tool printed), system instruction updates, skill bodies, a person's shell
// commands, idle, and switches of agent, model or location. A compaction
// running or failed handed the model no summary. A KIND THIS READ DOES NOT KNOW
// IS SAID ([EntryUnread]): it is from a release this read was not written for,
// and its words may be what the model read.
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
