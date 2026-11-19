package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
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
	// no plain file, a text whose redaction could not be read, a call's result
	// written out of order past the value bound, an export past the records
	// one read reads — said where it was.
	EntryUnread EntryKind = "unread"
)

// EntryKinds are every kind an entry can be.
var EntryKinds = []EntryKind{
	EntryBrief, EntryNote, EntryLateCall, EntryModel, EntryToolCall, EntryToolResult, EntrySummary,
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
// was not, one read as more entries or fewer, one no longer read. At 2 an
// OpenCode export's messages are read as OpenCode 2's kinds; at 3 every key is
// read exactly as written, as the harnesses' own JSON.parse reads it; at 4 a
// late call the engine hands a running job is an entry of its own; at 5 an
// OpenCode message of any length is read, a value past the bound as one entry,
// and a read ends at its lines bound.
const TranscriptVersion = 5

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
	// budget, or an export held more records than one read reads. Its
	// entries are all that can be shown.
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

	// LineBytes bounds one line of a harness's record read whole, and one
	// value of an export held: a longer value is streamed.
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
// TWO, BY WHAT ONE READ HOLDS at the 32 MiB line bound, measured: a Claude
// Code line's decode 134 MiB live (an OpenCode message's 97) and its whole
// redaction ([entryOpening]) up to 93 MiB more, so 227 MiB, 454 MiB for two;
// at the default GOGC the engine runs at (it sets no GOMEMLIMIT) a dense read's
// heap peaks near 400 MiB, two near 800. That is whole-entry redaction's cost,
// which a streamed redaction of each entry replaces. One slot would queue every
// reader behind the longest archive. A seat's read takes the seats' one slot
// ([SeatTranscriptReadsAtOnce]); a reading has its own ([HoldingsReadsAtOnce]).
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
// bounds, narrowed by filter, and holding a text to the line's bound.
func newTranscriptBuilder(from int, bounds TranscriptBounds, filter TranscriptFilter) *transcriptBuilder {
	return &transcriptBuilder{from: from, next: from, bounds: bounds, valueMax: bounds.LineBytes, filter: filter,
		entries: []TranscriptEntry{}, places: []EntryPlace{},
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
// decoded record of up to a line's 32 MiB, which a page would keep alive.
func shape(e TranscriptEntry, size int) TranscriptEntry {
	e.Bytes = size
	e.Thread, _, _ = entryOpening(e.Thread, transcriptNameMaxBytes)
	e.Tool, _, _ = entryOpening(e.Tool, transcriptNameMaxBytes)
	e.Text, e.Thread, e.Tool = strings.Clone(e.Text), strings.Clone(e.Thread), strings.Clone(e.Tool)
	return e
}

// entryOpening is text redacted WHOLE and then cut to at most bound bytes,
// whether it was cut, and the size of the whole redaction: an opening of what
// the whole text redacts to ([redact.Prefix]), read in bounded memory. An
// entry's text is at most [maxLineBytes], and one read is bounded by
// [TranscriptReadBytes] and [TranscriptReadsAtOnce].
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
// loosely: a retyped field reads as zero.
//
// THE CONTEXT A SESSION CARRIES reads it ([measuredPrompt]), off an export or
// a page of its message list; a person's transcript reads the same kinds off
// the one export walk ([openCodeReading]), so no two readers disagree about a
// message's kind. OpenCode 1.x's `{"info": …, "parts": …}` is not this
// document.
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

// openCodeKeys is what one decode of an object of an export saw of its keys
// ([eachKeyInPlace]): which names of the decode's own set it met (seen, a bit
// a name), whether it met a name outside that set (other), and whether it met
// one of them twice (twice). The person reader reads none of it; the one
// export walk keeps the same facts of each frame ([scanExport]), by which a
// holdings reading holds a message and a part to their sets.
type openCodeKeys struct {
	seen         uint32
	other, twice bool
}

// note notes a name by its index in the decode's set, -1 where it is none of
// them: the export walk's facts ([scanExport]) and the decode's alike.
func (k *openCodeKeys) note(i int) {
	switch {
	case i < 0:
		k.other = true
		return
	case k.seen&(1<<i) != 0:
		k.twice = true
	}
	k.seen |= 1 << i
}

// has reports whether the decode met the name of index i.
func (k openCodeKeys) has(i int) bool { return k.seen&(1<<i) != 0 }

// within reports whether every name the decode met is one of mask's, and none
// twice.
func (k openCodeKeys) within(mask uint32) bool { return !k.other && !k.twice && k.seen&^mask == 0 }

// openCodeNames is names, each by its index: a decode's set ([openCodeKeys]).
func openCodeNames(names ...string) map[string]int {
	set := make(map[string]int, len(names))
	for i, name := range names {
		set[name] = i
	}
	return set
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

// The names an item of a step's content is decoded by ([openCodeItemWire]),
// each its index in [openCodeItemNames].
const (
	itemType = iota
	itemText
	itemName
	itemState
	itemTime
	itemID
	itemExecuted
	itemNative
	itemProviderState
	itemProviderResultState
)

var openCodeItemNames = openCodeNames("type", "text", "name", "state", "time", "id", "executed", "native",
	"providerState", "providerResultState")

// The names a message of an export is read by, each its index in
// [openCodeMessageNames]: those a person's transcript reads, the message's id,
// and every other name of an assistant step's, which a holdings reading holds a
// step to on the one export walk, whose message frame is this set
// ([exportFrameNames]).
const (
	msgID = iota
	msgType
	msgTime
	msgText
	msgContent
	msgStatus
	msgSummary
	msgAgent
	msgModel
	msgSnapshot
	msgFinish
	msgRawFinish
	msgProviderState
	msgNative
	msgCost
	msgTokens
	msgError
	msgRetry
	msgMetadata
)

var openCodeMessageNames = openCodeNames("id", "type", "time", "text", "content", "status", "summary",
	"agent", "model", "snapshot", "finish", "rawFinish", "providerState", "native", "cost", "tokens", "error",
	"retry", "metadata")

// openCodeFileText is what a file a tool answered with reads as in text: an
// image as a Claude Code tool's image reads ([claudeImagePlaceholder]), and
// any other file as a file. Its bytes, inline or behind a URI, are no text.
func openCodeFileText(mime string) string {
	if strings.HasPrefix(mime, "image/") {
		return claudeImagePlaceholder
	}
	return "[a file]"
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
