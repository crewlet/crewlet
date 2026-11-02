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
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A captured session, read back as the transcript a person reads.
//
// A collection keeps the archive a run packed when its harness exited
// ([Runner.OpenCapture]) so a later turn can continue the session; the same
// archive is the only whole record of what the run read and wrote, so a seat's
// trail reads it too. It is read HERE, beside the scripts that pack it, because
// what it holds is the harness's own record and only the runner knows its
// layout: Claude Code's archive is a gzipped tar of the session's transcript
// (`projects/crewlet/<session>.jsonl`, one JSON record a line), its sidecar
// directory — each subagent's transcript under `subagents/`, and the tool
// results spilled to files — and its task list ([claudeArchiveFormat]);
// OpenCode's is the document `opencode session export` prints, gzipped: the
// session and its settled messages, each a list of parts
// ([openCodeArchiveFormat]). A format this build does not pack is not read
// ([Carries]).
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
// of the files it edited, its system notices, the model's thinking (its
// working, not what the run did, as the live transcript treats it), and a tool
// result Claude Code spilled to a file, which the result's own entry names. A
// part of the record this read could not read is an entry of its own
// ([EntryUnread]), said where it was — a line that is no record, a record or a
// message of no shape this read knows, a transcript that is no plain file — so
// a gap is never read as continuity. Which entries there are, and how many each
// record makes, is [TranscriptVersion]'s.
//
// A NOTE IS KNOWN BY ITS HEADING, wherever the harness filed it. Every note
// opens with the heading the engine writes ([noteHeadOpen]); OpenCode admits
// one as a person's message, and Claude Code hands one to its model as a
// hook's additional context, kept in a record of the harness's own whose shape
// is the harness's and moves between releases — so every record Claude Code
// writes for itself is searched for that heading, rather than read by a shape
// this build would have to pin. The line the run's own hook prints as it hands
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
// [redact.Secrets] BEFORE it is cut, whatever the capture did: all of it that
// can reach the entry's bound, and [redactContext] past it — a credential that
// begins inside the bound is read whole, since a key cut in half by a bound is
// a key the redaction no longer recognises — and none of the rest of a long
// text, which would be scanned only to be dropped. And a read stops when its
// caller's context is done, checked a line or a message at a time.
//
// # What is the archive's fault, and what is its source's
//
// A failure of the reader the archive came through — an object the store no
// longer holds, bytes that do not open — is returned as it came, wrapped, for
// the caller to tell apart ([sourceReader]). An archive that read whole and is
// not one its format describes is a [MalformedArchiveError], which says which
// way in this package's words alone: a box's own agent can rewrite what its run
// packs before a collection takes it, and a decoder's own message quotes what
// it read.

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
// was not, one read as more entries or as fewer, one no longer read.
const TranscriptVersion = 1

// TranscriptEntry is one entry of a session's transcript, as a read surface
// sends it.
type TranscriptEntry struct {
	Kind EntryKind `json:"kind"`

	// At is when the harness recorded the entry, zero where its record
	// carries no time.
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

	// Bytes is the whole text's size as the harness recorded it, before the
	// redaction and the cut, and Cut says the text was cut: a reader says
	// "the first 16 KiB of 2.1 MiB" rather than presenting an opening as the
	// whole.
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

	// From is the ordinal of the first of Entries among all of the
	// transcript's entries, and Next the ordinal after the last of them: the
	// one a following read starts from.
	From, Next int

	// Cut is why Entries ends before the transcript does, and empty where it
	// does not.
	Cut TranscriptCut
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
	// stored as (internal/executordomain's SessionMaxBytes, 32 MiB), which is
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

// Carries reports whether this build reads, and restores, a session archive
// harness wrote in format — the one format each harness's runner packs
// ([carrier]) — and, where it does not, why in a person's words.
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
// keeps them — from its entry from on, held to [TranscriptReadBounds]: every
// entry's text redacted and then cut, the read's page and its cut said
// ([Transcript.Cut]). It stops where ctx is done, with ctx's error.
//
// A failure of archive itself is returned wrapped, as it came; an archive this
// build does not read is [ErrUnsupportedArchive]; one that is not what its
// format describes is a [MalformedArchiveError].
func ReadTranscript(ctx context.Context, harness string, format int, archive io.Reader,
	from int) (Transcript, error) {
	return readTranscript(ctx, harness, format, archive, from, TranscriptReadBounds())
}

// readTranscript is [ReadTranscript] held to bounds.
func readTranscript(ctx context.Context, harness string, format int, archive io.Reader, from int,
	bounds TranscriptBounds) (Transcript, error) {

	c, why := carried(harness, format)
	if c == nil {
		return Transcript{}, fmt.Errorf("%w: %s", ErrUnsupportedArchive, why)
	}
	if from < 0 {
		return Transcript{}, fmt.Errorf("executor: a transcript is read from its entry 0 on, "+
			"or a later one, not from %d", from)
	}
	src := &sourceReader{r: archive}
	b := &transcriptBuilder{from: from, bounds: bounds, entries: []TranscriptEntry{},
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
	return Transcript{Entries: b.entries, From: from, Next: from + len(b.entries), Cut: b.cut}, nil
}

// transcriptBuilder is one read's page as it is built: every entry the
// archive holds is counted, and those from the page's first ordinal on are
// shaped and kept until the page holds all it hands out.
type transcriptBuilder struct {
	from   int
	bounds TranscriptBounds

	// inflated is the archive's bytes as they inflate, and handed the bytes
	// the members of a tar hand the reader: each held to the read budget.
	inflated, handed budgetReader

	// seen is how many entries the read has passed, the page's and those
	// before it; entries and bytes are the page's own.
	seen    int
	entries []TranscriptEntry
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
// raw its whole text as the harness recorded it. It answers errTranscriptPage
// once the page holds all it hands out, the cut said, so the read stops there.
//
// AN ENTRY BEFORE THE PAGE IS COUNTED AND NOTHING MORE: its text is not
// redacted, which is the expensive part, because nobody is shown it.
func (b *transcriptBuilder) add(e TranscriptEntry, raw string) error {
	ordinal := b.seen
	b.seen++
	if ordinal < b.from {
		return nil
	}
	if len(b.entries) == b.bounds.Entries {
		b.cut = CutEntries
		return errTranscriptPage
	}
	e = b.shape(e, raw)
	// THE FIRST ENTRY OF A PAGE IS ALWAYS KEPT: one entry is bounded on its
	// own, and a page that could not hold it would be a page no read gets
	// past.
	if len(b.entries) > 0 && b.bytes+len(e.Text) > b.bounds.Bytes {
		b.cut = CutBytes
		return errTranscriptPage
	}
	b.entries = append(b.entries, e)
	b.bytes += len(e.Text)
	return nil
}

// shape is an entry as it is handed out: its text redacted and then cut to the
// entry's bound, and its names redacted and bounded.
func (b *transcriptBuilder) shape(e TranscriptEntry, raw string) TranscriptEntry {
	e.Bytes = len(raw)
	e.Text, e.Cut = redactedWithin(raw, b.bounds.EntryBytes)
	e.Thread, _ = redactedWithin(e.Thread, transcriptNameMaxBytes)
	e.Tool, _ = redactedWithin(e.Tool, transcriptNameMaxBytes)
	return e
}

// redactedWithin is text redacted and then cut to at most bound bytes, marked
// where it was cut, and whether it was.
//
// WHAT IS REDACTED IS WHAT CAN REACH THE CUT, and [redactContext] past it: a
// credential that begins inside the kept bytes ends within that much of them —
// a private key's block is the longest shape the redaction knows — so it is
// read whole and redacted as one, where a key cut in half by the bound would be
// one the redaction no longer recognises. The rest of a long text is never
// scanned: a line of a record may run to [maxLineBytes], and redacting all of
// it to hand out its first [TranscriptEntryMaxBytes] was the whole cost of a
// read many times over.
func redactedWithin(text string, bound int) (string, bool) {
	window := textcut.Bytes(text, bound+redactContext)
	out := redact.Secrets(window)
	if len(window) == len(text) && len(out) <= bound {
		return out, false
	}
	return textcut.Within(out, bound), true
}

// prose adds one entry of words — a brief, the model's, a summary — unless it
// holds none. A brief that is a note — its words open with a note's heading,
// inside nothing but what a harness wraps one in — is the note, or the notes,
// it carries.
func (b *transcriptBuilder) prose(kind EntryKind, at time.Time, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if kind == EntryBrief && opensWithNote(text) {
		return b.notes(at, text)
	}
	return b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, text)
}

// notes adds an entry for every note text carries ([notesIn]).
func (b *transcriptBuilder) notes(at time.Time, text string) error {
	for _, note := range notesIn(text) {
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

// called remembers which tool the call id is of, for its result.
func (b *transcriptBuilder) called(id, tool string) {
	if id != "" && len(b.calls) < maxPendingCalls {
		b.calls[id] = tool
	}
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
// false where there is none: the heading's opening, a sender's name, and the
// rest of the heading on the same line ([noteHeadOpen], [noteHeadClose]).
func noteAt(text string, from int) (int, bool) {
	for from < len(text) {
		i := strings.Index(text[from:], noteHeadOpen)
		if i < 0 {
			return 0, false
		}
		at := from + i
		line, _, _ := strings.Cut(text[at:], "\n")
		if len(line) > len(noteHeadOpen)+len(noteHeadClose) && strings.HasSuffix(line, noteHeadClose) {
			return at, true
		}
		from = at + len(noteHeadOpen)
	}
	return 0, false
}

// notesIn is every note text carries, each from its heading to the next one's
// or to text's end — a hook's answer joins the notes of one step in one text
// (ClaudeHookAnswer) — trimmed of the system reminder a harness closes them
// in.
func notesIn(text string) []string {
	var notes []string
	at, ok := noteAt(text, 0)
	for ok {
		next, more := noteAt(text, at+len(noteHeadOpen))
		end := len(text)
		if more {
			end = next
		}
		note := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[at:end]), "</system-reminder>"))
		notes = append(notes, note)
		at, ok = next, more
	}
	return notes
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
	at, ok := noteAt(text, 0)
	return ok && noteFraming.MatchString(text[:at])
}

// ---- Claude Code ------------------------------------------------------- //

// transcript reads a Claude Code archive ([claudeArchiveFormat]).
func (ClaudeCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.claudeArchive(ctx, archive)
}

// claudeRecord is the part of one line of Claude Code's transcript a read
// takes: what kind of record it is, when it was written, whether it is one
// the harness wrote for itself (`isMeta`) or a compaction summary, and its
// message, raw — an object whose content is a string or a list of blocks. A
// system record's subtype and content are its own, read for the one line the
// run's own hook prints as it hands the model a note ([claudeSteerSaid]).
type claudeRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	Timestamp        string          `json:"timestamp"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
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
// skipped rather than losing the record.
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
	if err := json.Unmarshal(line, &rec); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return b.notARecord(len(line))
		}
	}
	at := claudeTime(rec.Timestamp)
	switch {
	case rec.Type == "assistant":
		return b.claudeMessage(rec.Message, EntryModel, at)
	case rec.Type == "user" && !rec.IsMeta:
		kind := EntryBrief
		if rec.IsCompactSummary {
			kind = EntrySummary
		}
		return b.claudeMessage(rec.Message, kind, at)
	case rec.Type == "system":
		// THE LINE THE RUN'S OWN HOOK PRINTS as it hands the model a note,
		// which the live transcript shows as `[steer]`: a note in its own
		// right here, since a release that keeps no hook context leaves it
		// the note's one trace.
		if said, ok := claudeSteerSaid(rec.Subtype, rec.Content); ok {
			if err := b.add(TranscriptEntry{Kind: EntryNote, At: at, Thread: b.thread}, said); err != nil {
				return err
			}
		}
	}
	return b.claudeNotes(line, at)
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
// does.
func (b *transcriptBuilder) claudeNotes(line []byte, at time.Time) error {
	if !bytes.Contains(line, []byte(noteHeadOpen)) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	// TO THE RECORD'S END: it read whole as a record already, so its end is
	// the only place this stops.
	for tok, err := dec.Token(); err == nil; tok, err = dec.Token() {
		text, ok := tok.(string)
		if !ok {
			continue
		}
		if notesErr := b.notes(at, text); notesErr != nil {
			return notesErr
		}
	}
	return nil
}

// claudeMessage reads a record's message — an object, its content a string or
// a list of blocks — as kind, and says one of no shape this read knows rather
// than reading it as a message that said nothing.
func (b *transcriptBuilder) claudeMessage(raw json.RawMessage, kind EntryKind, at time.Time) error {
	if absent(raw) {
		return nil
	}
	if !jsonObject(raw) {
		return b.unshaped()
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return b.unshaped()
	}
	return b.claudeContent(message.Content, kind, at)
}

// claudeContent reads a message's content: a string is one entry of kind, and
// a list of blocks ([claudeBlocks]) is an entry a block — its text as kind, a
// tool call, a tool's result. A thinking block is the model's working and no
// entry, and so is every block of a type this build does not know; content
// that is neither a string nor a list is said.
func (b *transcriptBuilder) claudeContent(raw json.RawMessage, kind EntryKind, at time.Time) error {
	raw = bytes.TrimSpace(raw)
	switch {
	case absent(raw):
		return nil
	case raw[0] == '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return b.unshaped()
		}
		return b.prose(kind, at, text)
	case raw[0] != '[':
		return b.unshaped()
	}
	for _, block := range claudeBlocks(raw) {
		var err error
		switch block.Type {
		case "text":
			err = b.prose(kind, at, block.Text)
		case "tool_use":
			b.called(block.ID, block.Name)
			err = b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread,
				Tool: block.Name}, compactJSON(block.Input))
		case "tool_result":
			err = b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread,
				Tool: b.answered(block.ToolUseID), Failed: block.IsError},
				claudeResultText(block.Content))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// claudeTime is a record's `timestamp`, an RFC 3339 instant, or zero where it
// carries none this read can parse.
func claudeTime(raw string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
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

// openCodeMessage is the part of one message of an OpenCode export a read
// takes: who wrote it, whether it is a compaction's summary — `summary` is
// `true` on such an assistant message, and an object on a user message, which
// is no summary — when, and its parts, raw: a message whose parts are no list
// is said rather than read as one that has none.
type openCodeMessage struct {
	Info struct {
		Role    string          `json:"role"`
		Summary json.RawMessage `json:"summary"`
		Time    struct {
			Created int64 `json:"created"`
		} `json:"time"`
	} `json:"info"`
	Parts json.RawMessage `json:"parts"`
}

// openCodePart is one part of a message: its text, or a tool's call — its
// state carrying the input, and the output or the error once it settled.
// Instants are milliseconds since the epoch.
type openCodePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
		Error  string          `json:"error"`
		Time   struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"time"`
	} `json:"state"`
	Time struct {
		Start int64 `json:"start"`
	} `json:"time"`
}

// openCodeExport reads the export document a message at a time, its other
// fields (the session's own) passed: `{"info": …, "messages": [{"info": …,
// "parts": […]}, …]}`.
//
// ONE MESSAGE IS HELD AT A TIME, to about [TranscriptBounds.LineBytes] — the
// bound a line of a record is read whole to — counted afresh before every
// step of the walk, so a message past it is said, and the read stops there: a
// JSON decoder cannot find where the next value begins without reading the
// one it is in.
func (b *transcriptBuilder) openCodeExport(ctx context.Context, archive io.Reader) error {
	held := &heldValue{r: archive, max: b.bounds.LineBytes}
	err := b.pastHeld(held, b.openCodeDocument(ctx, json.NewDecoder(held), held))
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Errorf("%w: %w", errNotExport, err)
	}
	return err
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
// that is no object, or whose parts are no list, is said; within one, a field
// whose type a release changed is skipped rather than losing the message, as
// the stream's events are ([decodeOpenCodeEvent]).
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
	var m openCodeMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return err
		}
	}
	parts, ok := openCodeParts(m.Parts)
	if !ok {
		return b.unread("a message of the export whose parts are not a list was not read")
	}
	return b.openCodeMessage(m, parts)
}

// openCodeParts is a message's parts, and false where they are there and are
// no list. Decoded loosely, a part whose field a release retyped kept with the
// rest of what it says.
func openCodeParts(raw json.RawMessage) ([]openCodePart, bool) {
	raw = bytes.TrimSpace(raw)
	switch {
	case absent(raw):
		return nil, true
	case raw[0] != '[':
		return nil, false
	}
	var parts []openCodePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return nil, false
		}
	}
	return parts, true
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

// openCodeMessage reads one message's parts: a person's text is the brief — or
// a note, where it is one — the model's is its own words, or a summary on a
// compaction's message, and a tool part is the call and, once it settled, its
// result. Reasoning, the steps' bookkeeping, files and patches are no entry.
func (b *transcriptBuilder) openCodeMessage(m openCodeMessage, parts []openCodePart) error {
	at := unixMillis(m.Info.Time.Created)
	summary := bytes.Equal(bytes.TrimSpace(m.Info.Summary), []byte("true"))
	for _, p := range parts {
		var err error
		switch p.Type {
		case "text":
			kind := EntryModel
			switch {
			case m.Info.Role == "user":
				kind = EntryBrief
			case m.Info.Role != "assistant":
				continue
			case summary:
				kind = EntrySummary
			}
			err = b.prose(kind, firstInstant(unixMillis(p.Time.Start), at), p.Text)
		case "tool":
			err = b.add(TranscriptEntry{Kind: EntryToolCall, Tool: p.Tool,
				At: firstInstant(unixMillis(p.State.Time.Start), at)}, compactJSON(p.State.Input))
			if err != nil {
				return err
			}
			ended := firstInstant(unixMillis(p.State.Time.End), at)
			switch p.State.Status {
			case "completed":
				err = b.add(TranscriptEntry{Kind: EntryToolResult, Tool: p.Tool, At: ended},
					p.State.Output)
			case "error":
				err = b.add(TranscriptEntry{Kind: EntryToolResult, Tool: p.Tool, At: ended,
					Failed: true}, p.State.Error)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// unixMillis is an export's instant, or zero where it carries none.
func unixMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
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
// refused as it is read rather than once the decoder has held all of it.
type heldValue struct {
	r    io.Reader
	max  int
	n    int
	over bool
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
	return n, err
}
