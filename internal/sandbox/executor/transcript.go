package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
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
// the run was briefed, what its model wrote, every tool it called and what the
// tool answered, and the compaction summaries the harness wrote in place of
// the conversation before them. The run's own conversation comes first and
// then each subagent's, in the order the archive holds them, every entry of a
// subagent naming it. What is NOT an entry is the harness's bookkeeping — a
// session's title, a snapshot of the files it edited, its system notices, the
// model's thinking (its working, not what the run did, as the live transcript
// treats it), and a tool result Claude Code spilled to a file, which the
// result's own entry names. A part of the record this read could not read is
// an entry of its own ([EntryUnread]), said where it was, so a gap is never
// read as continuity.
//
// # Bounded, and redacted at the read
//
// Every read is held to [TranscriptReadBounds]: the entries one read hands out
// and their text together, each entry's text, one line of the record, and the
// archive's decompressed bytes. A read hands out a page — the entries from an
// ordinal on — and says why it ends where it does ([TranscriptCut]); the next
// page is read again from the archive's start, so the work one read costs is
// bounded by the archive rather than by how deep a reader pages. Every entry's
// text is passed through [redact.Secrets] WHOLE, before it is cut, whatever the
// capture did: a session is everything the run read and printed, and a key cut
// in half by a bound is a key the redaction no longer recognises.
//
// # What is the archive's fault, and what is its source's
//
// A failure of the reader the archive came through — an object the store no
// longer holds, bytes that do not open — is returned as it came, wrapped, for
// the caller to tell apart ([sourceReader]). An archive that read whole and is
// not one its format describes is [ErrMalformedArchive]: a box's own agent can
// rewrite what its run packs before a collection takes it.

// EntryKind is who one entry of a session's transcript is from: the words a
// read surface groups and labels a transcript by.
type EntryKind string

const (
	// EntryBrief is what the run was handed to do: the engine's brief, a
	// later launch's on the same session, or — in a subagent's thread — the
	// brief the run's own model wrote it.
	EntryBrief EntryKind = "brief"

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
	// message of an export past its bound — said where it was.
	EntryUnread EntryKind = "unread"
)

// EntryKinds are every kind an entry can be.
var EntryKinds = []EntryKind{
	EntryBrief, EntryModel, EntryToolCall, EntryToolResult, EntrySummary, EntryUnread,
}

// Valid reports whether a kind off the wire is one this build writes.
func (k EntryKind) Valid() bool { return slices.Contains(EntryKinds, k) }

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
	// later read can go further: the archive's decompressed bytes ran past
	// the read budget, or a message of an export past the bound one may hold
	// left what follows it unreadable. Its entries are all that can be shown.
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

	// ReadBytes bounds the archive's decompressed bytes one read takes.
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

	// TranscriptReadBytes is the most of the archive's decompressed bytes one
	// read takes.
	//
	// TWO HUNDRED AND FIFTY-SIX MEBIBYTES: eight times the most an archive is
	// stored as (internal/executordomain's SessionMaxBytes, 32 MiB), which is
	// gzip's ratio over JSON text, so an ordinary session's archive reads
	// whole within it — and one that inflates past it, as anything a box
	// packs can, stops there rather than holding the engine for as long as
	// it inflates.
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
	// and is not one its format describes: not gzip, not a tar or an export,
	// or cut short.
	ErrMalformedArchive = errors.New("executor: the session's archive is not one its format describes")
)

// The sentinels a read stops on, which never leave this file.
var (
	// errTranscriptPage stops a read that holds all it hands out, its cut
	// already said.
	errTranscriptPage = errors.New("executor: the read holds all it hands out")

	// errReadBudget is the archive's decompressed bytes past the read
	// budget.
	errReadBudget = errors.New("executor: the transcript's read budget is spent")

	// errValuePastBound is one value of an export past the bound one may
	// hold.
	errValuePastBound = errors.New("executor: a value of the export is past the bound one may hold")
)

// carriers are the harnesses whose session this build packs, restores and
// reads, by name: every one [Names] lists.
var carriers = map[string]carrier{ClaudeCodeName: ClaudeCode{}, OpenCodeName: OpenCode{}}

// Carries reports whether this build reads, and restores, a session archive
// harness wrote in format — the one format each harness's runner packs
// ([carrier]) — and, where it does not, why in a person's words.
func Carries(harness string, format int) (bool, string) {
	c, ok := carriers[harness]
	switch {
	case !ok:
		return false, fmt.Sprintf("this build carries no session of the harness %q", harness)
	case format != c.ArchiveFormat():
		return false, fmt.Sprintf("the session's archive is format %d, and this build reads "+
			"%s's format %d alone", format, harness, c.ArchiveFormat())
	}
	return true, ""
}

// ReadTranscript reads the transcript of one session archive harness wrote in
// format — the bytes a run packed when its harness exited, as a collection
// keeps them — from its entry from on, held to [TranscriptReadBounds]: every
// entry's text redacted whole and then cut, the read's page and its cut said
// ([Transcript.Cut]).
//
// A failure of archive itself is returned wrapped, as it came; an archive this
// build does not read is [ErrUnsupportedArchive]; one that is not what its
// format describes is [ErrMalformedArchive].
func ReadTranscript(harness string, format int, archive io.Reader, from int) (Transcript, error) {
	return readTranscript(harness, format, archive, from, TranscriptReadBounds())
}

// readTranscript is [ReadTranscript] held to bounds.
func readTranscript(harness string, format int, archive io.Reader, from int,
	bounds TranscriptBounds) (Transcript, error) {

	if ok, why := Carries(harness, format); !ok {
		return Transcript{}, fmt.Errorf("%w: %s", ErrUnsupportedArchive, why)
	}
	if from < 0 {
		return Transcript{}, fmt.Errorf("executor: a transcript is read from its entry 0 on, "+
			"or a later one, not from %d", from)
	}
	src := &sourceReader{r: archive}
	budget := &budgetReader{left: bounds.ReadBytes}
	b := &transcriptBuilder{from: from, bounds: bounds, entries: []TranscriptEntry{},
		calls: map[string]string{}}
	err := b.read(carriers[harness], src, budget)
	switch {
	case src.err != nil:
		// THE SOURCE'S OWN FAILURE FIRST, whatever the decoding made of it:
		// gzip, tar and JSON each report a stream that stopped in their own
		// words, and an object that did not open is not a malformed archive.
		return Transcript{}, fmt.Errorf("executor: reading the session's archive: %w", src.err)
	case err == nil, errors.Is(err, errTranscriptPage):
	case budget.spent:
		b.cut = CutRead
	default:
		return Transcript{}, fmt.Errorf("%w: %w", ErrMalformedArchive, err)
	}
	return Transcript{Entries: b.entries, From: from, Next: from + len(b.entries), Cut: b.cut}, nil
}

// transcriptBuilder is one read's page as it is built: every entry the
// archive holds is counted, and those from the page's first ordinal on are
// shaped and kept until the page holds all it hands out.
type transcriptBuilder struct {
	from   int
	bounds TranscriptBounds

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

// read decompresses the archive and hands it to its harness's reader, the
// decompressed bytes held to the read budget.
func (b *transcriptBuilder) read(c carrier, src io.Reader, budget *budgetReader) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	budget.r = zr
	return c.transcript(b, budget)
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

// shape is an entry as it is handed out: its text redacted WHOLE and then cut
// to the entry's bound, and its names redacted and bounded.
func (b *transcriptBuilder) shape(e TranscriptEntry, raw string) TranscriptEntry {
	e.Bytes = len(raw)
	text := redact.Secrets(raw)
	if len(text) > b.bounds.EntryBytes {
		text, e.Cut = textcut.Within(text, b.bounds.EntryBytes), true
	}
	e.Text = text
	e.Thread = textcut.Within(redact.Secrets(e.Thread), transcriptNameMaxBytes)
	e.Tool = textcut.Within(redact.Secrets(e.Tool), transcriptNameMaxBytes)
	return e
}

// prose adds one entry of words — a brief, the model's, a summary — unless it
// holds none.
func (b *transcriptBuilder) prose(kind EntryKind, at time.Time, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, text)
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

// ---- Claude Code ------------------------------------------------------- //

// transcript reads a Claude Code archive ([claudeArchiveFormat]).
func (ClaudeCode) transcript(b *transcriptBuilder, archive io.Reader) error {
	return b.claudeArchive(archive)
}

// claudeRecord is the part of one line of Claude Code's transcript a read
// takes: what kind of record it is, when it was written, whether it is one
// the harness wrote for itself (`isMeta`) or a compaction summary, and its
// message's content — a string, or a list of content blocks.
type claudeRecord struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudeRecordBlock is one content block of a record's message: text, a tool
// call with its input, or a tool's result — its content a string or a list of
// blocks of its own.
type claudeRecordBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// claudeArchive reads the session's transcript and each subagent's, a member
// at a time, and passes every other member — the tool results spilled to
// files, the task list — unread.
func (b *transcriptBuilder) claudeArchive(archive io.Reader) error {
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		thread, ok := claudeThread(h)
		if !ok {
			continue
		}
		b.thread = thread
		clear(b.calls)
		if readErr := readLines(tr, b.bounds.LineBytes, b.claudeLine,
			b.claudeLinePastBound); readErr != nil {
			return readErr
		}
	}
}

// claudeThread is whose transcript a member of the archive is: the session's
// own, `projects/crewlet/<session>.jsonl` (the empty thread), or a
// subagent's, `projects/crewlet/<session>/subagents/<name>.jsonl` (the
// thread named after its file) — and false for every other member.
func claudeThread(h *tar.Header) (string, bool) {
	if h.Typeflag != tar.TypeReg {
		return "", false
	}
	rest, ok := strings.CutPrefix(path.Clean(h.Name), "projects/"+claudeProjectDir+"/")
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
// brief, a compaction summary, or the results of the model's tool calls — or
// the model's own, its text and its calls. Every other record is the harness's
// bookkeeping and no entry.
//
// DECODED LOOSELY, as the stream is ([decodeOpenCodeEvent]'s reason): a field
// whose type a release changed is skipped rather than losing the record. A
// line that is no JSON at all is said, where it was.
func (b *transcriptBuilder) claudeLine(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var rec claudeRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return b.unread(fmt.Sprintf("a line of %s of the harness's record is not a record, "+
				"and was not read", humanSize(int64(len(line)))))
		}
	}
	at := claudeTime(rec.Timestamp)
	switch rec.Type {
	case "user":
		// A RECORD THE HARNESS WROTE FOR ITSELF — a command's caveat, a
		// reminder — is not what the run was handed.
		if rec.IsMeta {
			return nil
		}
		kind := EntryBrief
		if rec.IsCompactSummary {
			kind = EntrySummary
		}
		return b.claudeContent(rec.Message.Content, kind, at)
	case "assistant":
		return b.claudeContent(rec.Message.Content, EntryModel, at)
	}
	return nil
}

// claudeContent reads a message's content: a string is one entry of kind, and
// a list of blocks is an entry a block — its text as kind, a tool call, a
// tool's result. A thinking block is the model's working and no entry, and so
// is every block of a type this build does not know.
func (b *transcriptBuilder) claudeContent(raw json.RawMessage, kind EntryKind, at time.Time) error {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return b.prose(kind, at, text)
	}
	blocks, ok := claudeBlocks(raw)
	if !ok {
		return nil
	}
	for _, block := range blocks {
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

// claudeResultText is a tool result's content as text: a string as it is, or
// its blocks' text a line each, an image named where it was.
func claudeResultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	blocks, _ := claudeBlocks(raw)
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "image":
			parts = append(parts, "[an image]")
		}
	}
	return strings.Join(parts, "\n")
}

// claudeBlocks is content read as a list of blocks, and false where it is no
// list: decoded loosely, a block whose field a release retyped kept with the
// rest of what it says.
func claudeBlocks(raw json.RawMessage) ([]claudeRecordBlock, bool) {
	var blocks []claudeRecordBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		var mismatch *json.UnmarshalTypeError
		return blocks, errors.As(err, &mismatch)
	}
	return blocks, true
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

// ---- OpenCode ------------------------------------------------------------ //

// transcript reads an OpenCode archive ([openCodeArchiveFormat]).
func (OpenCode) transcript(b *transcriptBuilder, archive io.Reader) error {
	return b.openCodeExport(archive)
}

// openCodeMessage is the part of one message of an OpenCode export a read
// takes: who wrote it, whether it is a compaction's summary — `summary` is
// `true` on such an assistant message, and an object on a user message, which
// is no summary — when, and its parts.
type openCodeMessage struct {
	Info struct {
		Role    string          `json:"role"`
		Summary json.RawMessage `json:"summary"`
		Time    struct {
			Created int64 `json:"created"`
		} `json:"time"`
	} `json:"info"`
	Parts []openCodePart `json:"parts"`
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
func (b *transcriptBuilder) openCodeExport(archive io.Reader) error {
	held := &heldValue{r: archive, max: b.bounds.LineBytes}
	return b.pastHeld(held, b.openCodeDocument(json.NewDecoder(held), held))
}

// openCodeDocument walks the document, its messages handed to
// [transcriptBuilder.openCodeMessage] one at a time.
func (b *transcriptBuilder) openCodeDocument(dec *json.Decoder, held *heldValue) error {
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
			if messageErr := b.openCodeNext(dec); messageErr != nil {
				return messageErr
			}
		}
		if closeErr := expectDelim(dec, ']'); closeErr != nil {
			return closeErr
		}
	}
	return expectDelim(dec, '}')
}

// openCodeNext decodes the next message of the list and reads it. A field whose
// type a release changed is skipped rather than losing the message, as the
// stream's events are ([decodeOpenCodeEvent]).
func (b *transcriptBuilder) openCodeNext(dec *json.Decoder) error {
	var m openCodeMessage
	if err := dec.Decode(&m); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return err
		}
	}
	return b.openCodeMessage(m)
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

// openCodeMessage reads one message's parts: a person's text is the brief, the
// model's is its own words — or a summary, on a compaction's message — and a
// tool part is the call and, once it settled, its result. Reasoning, the
// steps' bookkeeping, files and patches are no entry.
func (b *transcriptBuilder) openCodeMessage(m openCodeMessage) error {
	at := unixMillis(m.Info.Time.Created)
	summary := bytes.Equal(bytes.TrimSpace(m.Info.Summary), []byte("true"))
	for _, p := range m.Parts {
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

// expectDelim reads the next token of dec, which must be want.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if got, ok := tok.(json.Delim); !ok || got != want {
		return fmt.Errorf("executor: the export holds %v where %v belongs", tok, want)
	}
	return nil
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

// budgetReader is the archive's decompressed bytes, held to the read budget:
// past it, every read answers errReadBudget and says the budget is spent.
type budgetReader struct {
	r     io.Reader
	left  int64
	spent bool
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// AT THE BUDGET EXACTLY, an archive that ends there was read whole:
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
