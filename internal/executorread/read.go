package executorread

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// TranscriptState is what reading a session's object came to.
type TranscriptState string

const (
	// TranscriptRead is a session read: the page holds its entries.
	TranscriptRead TranscriptState = "read"

	// TranscriptNone is a row naming no object, for one of the four reasons
	// [executordomain.Session.NotKept] tells apart: a launch ran its session
	// out of its model's context, which retired it; its turn captured
	// nothing (yet, while it runs); its newest capture was not kept; or the
	// seat's session budget released it.
	TranscriptNone TranscriptState = "none"

	// TranscriptMissing is an object the store no longer holds.
	TranscriptMissing TranscriptState = "missing"

	// TranscriptUnreadable is an object that does not open: sealed under a
	// key the keyring no longer holds, or bytes that are not the ones sealed
	// for this turn.
	TranscriptUnreadable TranscriptState = "unreadable"

	// TranscriptMalformed is an object that opened and is not an archive its
	// format describes.
	TranscriptMalformed TranscriptState = "malformed"

	// TranscriptUnsupported is an archive of a harness or a format this build
	// does not read.
	TranscriptUnsupported TranscriptState = "unsupported"
)

// TranscriptStates are every state a transcript's read can come to.
var TranscriptStates = []TranscriptState{
	TranscriptRead, TranscriptNone, TranscriptMissing, TranscriptUnreadable,
	TranscriptMalformed, TranscriptUnsupported,
}

// Valid reports whether a state off the wire is one this build answers.
func (s TranscriptState) Valid() bool { return slices.Contains(TranscriptStates, s) }

// Rows is the executor log's read of one turn's row, at the level asked, and
// where it was read (executordomain.Reader.Session, through the estate router,
// so a node without `data` answers as a data node does).
//
// Consumer-defined, over the domain's own row type: the row stays one shape.
type Rows interface {
	Session(ctx context.Context, handle, turn string, fresh statelog.Freshness) (executordomain.SessionRead, error)
}

// Objects opens a session's sealed object, streaming, its associated data
// binding it to the seat, the turn and the object's key
// (executordomain.SessionStore): what a transcript is read from.
type Objects interface {
	Open(ctx context.Context, session executordomain.Session) (io.ReadCloser, error)
}

// Source is what a read of a kept session reads: the turn's row, and the
// object it names. Both are required.
type Source struct {
	Rows    Rows
	Objects Objects
}

// Ask is one read of a seat's kept session: the seat's turn, the page, and
// the level the row is read at.
type Ask struct {
	Handle string
	Turn   string

	// Cursor is the `next_cursor` an earlier page of the transcript handed
	// out ([Cursor]), "" for its first page.
	Cursor string

	// Fresh is the level the turn's row is read at: a person's read
	// surface's own, or the one its caller asked.
	Fresh statelog.Freshness

	// By is whose read it is, which holds the page to that reader's bounds
	// and its read to that reader's slots ([executor.Reader]); Filter narrows
	// the page to the entries it keeps, every entry counted all the same.
	By     executor.Reader
	Filter executor.TranscriptFilter
}

// Page is one read of a kept session: the row it was read from, what reading
// its object came to, and a page of its transcript.
type Page struct {
	// Read is the turn's row and where it was read.
	Read executordomain.SessionRead

	// State is what reading the session's object came to, and Why says it in
	// the engine's own words wherever it is not [TranscriptRead].
	State TranscriptState
	Why   string

	// Transcript is the page: its entries — none unless State is
	// [TranscriptRead] — the ordinal they start from, and why it ends where
	// it does.
	Transcript executor.Transcript

	// Next is the cursor the page after this one is read from, "" where none
	// follows.
	Next string

	// Reread says the session's object failed and the page is answered from
	// the turn's row read again, at [ReadAgain]'s level: Read is that read.
	Reread bool
}

// ReadAgain is the level a read whose object failed reads its turn's row again
// at: `linearizable`, whatever the first read's. The row is the authority on
// whether the session is still kept, and a copy of the log behind it would
// still name the object a release took; and the read is made only where an
// object failed, so the barrier it costs the executor log is paid on a
// failure alone.
func ReadAgain() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadLinearizable} }

// The ways a read is answered with no page. Each is a fact a surface says
// differently, which is why they are apart: the sentence after the sentinel
// is the one to say.
var (
	// ErrNoSession reports a turn the seat keeps no row of.
	ErrNoSession = errors.New("executorread: the seat keeps no session of the turn")

	// ErrCursor reports a cursor no page of this transcript hands out, or one
	// a read of it can no longer continue: the caller's to change.
	ErrCursor = errors.New("executorread: the cursor does not continue this transcript")

	// ErrUnavailable reports a session that could not be read now, which the
	// caller asks again for: the object store did not answer for it, or —
	// [ErrKeptUnknown] — its object failed and the executor log did not answer
	// whether it is still kept.
	ErrUnavailable = errors.New("executorread: the session could not be read now")

	// ErrKeptUnknown is the [ErrUnavailable] of a session whose object failed
	// — not found, not opening, or its store not answering — and whose row
	// could not be read again to say whether the session is still kept: the
	// executor log is what did not answer, so a reader is told that, never
	// that the store did not, which may have answered that the object is
	// gone.
	ErrKeptUnknown = fmt.Errorf("%w: the executor log did not say whether it is still kept", ErrUnavailable)
)

// howRowsGo is how a turn's row goes from the executor log, as every reader of
// a session it went from is told.
const howRowsGo = "with the end of a later turn of its conversation that was handed it, past the " +
	"retention, or with the seat"

// nothingMore is what a reader is told of a session released while it was
// read: the pages already read stand, and the rest is gone with its object.
const nothingMore = "nothing more of its transcript can be read"

// noSession is the turn of ask, of which the seat keeps no row: said as the
// ways a row goes, the first of them the one a reader following a conversation
// meets — an end absorbs every turn its read handed out, deleting their rows,
// and an absorbed row leaves no word of the turn that took it. Where the
// session was being read — a later page, or the row read again after its
// object failed — its row went while it was, and that is said.
func noSession(ask Ask, reading bool) error {
	if reading {
		return fmt.Errorf("%w: %s keeps no executor session of the turn %s any more: its row went "+
			"while the session was read, %s, and %s", ErrNoSession, ask.Handle, ask.Turn, howRowsGo,
			nothingMore)
	}
	return fmt.Errorf("%w: %s keeps no executor session of the turn %s: the turn may have recorded "+
		"none, or its row went: %s", ErrNoSession, ask.Handle, ask.Turn, howRowsGo)
}

// Read reads a page of the session of the seat's turn ask names: the turn's
// row at ask's level, and the transcript its object holds from ask's cursor on,
// held to the bounds of whose read it is ([executor.Reader.Bounds]) and
// narrowed by ask's filter.
//
// A ROW THAT NAMES NO SESSION, AND AN OBJECT THAT DOES NOT READ, ARE ANSWERS:
// the row is still the turn's account of how it ended, so a page says why no
// entries are shown, in the engine's words ([TranscriptStates]) — never a
// decoder's, which would quote what a box's own agent packed. Only a turn with
// no row ([ErrNoSession]), a cursor the page cannot continue ([ErrCursor]) and
// a store that did not answer ([ErrUnavailable]) are no page; a caller that
// left is returned as it left — beside the row its read had read, where it had
// read one ([Page.Read]), so a caller whose own bound ended the read can say
// how large the session it was reading is — and a refusal of the log's own — a
// turn id that is none — as the log said it. A store that did not answer and a
// log that did not say whether a failed session is still kept are both
// [ErrUnavailable], and the second is [ErrKeptUnknown] too, so a caller tells a
// reader which of the two did not answer.
//
// A SESSION RELEASED WHILE IT IS READ IS SAID AS RELEASED. A read holds
// nothing: the session a page reads can go between the read of its row and
// the end of its object's stream — absorbed by a later turn's end, released by
// the seat's budget, retired by the end of a turn that ran out of its model's
// context in it, past the retention, or captured again — and its object with it
// once the collector passes. So an object that fails, as it opens or
// as it streams, has the turn's row read once more, at [ReadAgain]'s level,
// and the page says what the row says now: no row is [ErrNoSession] saying the
// row went while the session was read; a row naming no object is
// [TranscriptNone] in the domain's reason and that nothing more can be read;
// a row naming a newer capture is read from it on a first page, and refuses a
// later page's cursor as a session captured again. Only where the row still
// names the object is the failure the store's or the archive's own — missing,
// unreadable, or a store that did not answer — since the store reports a
// collected object and bytes that are not the sealed ones alike as unreadable,
// and a release read as either was said as a loss or as corruption it is not.
// The row is read again once: a newer capture that fails too is said as it
// fails. And a later page whose row went, or names no object, before its read
// began is said the same way, the pages before it standing.
func Read(ctx context.Context, ask Ask, src Source) (Page, error) {
	if !ask.By.Valid() {
		return Page{}, fmt.Errorf("executorread: a read of a session says whose it is, one of %s, "+
			"and %q is none", words.Or(executor.Readers), ask.By)
	}
	reading := ask.Cursor != ""
	read, err := src.Rows.Session(ctx, ask.Handle, ask.Turn, ask.Fresh)
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		return Page{}, noSession(ask, reading)
	case err != nil:
		return Page{}, err
	}
	from, err := ordinalOf(ask.Cursor, read.Session)
	if err != nil {
		return Page{}, err
	}
	page, failed := readPage(ctx, ask, read, from, reading, src.Objects)
	if failed == nil || !releasable(failed) {
		return unread(page, failed)
	}
	again, err := src.Rows.Session(ctx, ask.Handle, ask.Turn, ReadAgain())
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		return Page{}, noSession(ask, true)
	case err != nil && ctx.Err() != nil:
		return Page{Read: read}, ctx.Err()
	case err != nil:
		// UNKNOWN, NEVER A LOSS: the object failed and the row could not be
		// asked whether it still names it, so the failure may be a release
		// as much as a loss; the caller asks again.
		// The object's failure is said, never wrapped: wrapped, it would class
		// the page as the loss this cannot tell it from.
		return Page{}, fmt.Errorf("%w: %s's session of the turn %s did not read (%v), and its row "+
			"could not be read again: %w", ErrKeptUnknown, ask.Handle, ask.Turn, failed, err)
	case again.Session.HasObject() && again.Session.Object.Key == read.Session.Object.Key:
		// STILL NAMED: the failure is the store's or the archive's own.
		page.Read, page.Reread = again, true
		return unread(page, failed)
	}
	if from, err = ordinalOf(ask.Cursor, again.Session); err != nil {
		return Page{}, err
	}
	page, failed = readPage(ctx, ask, again, from, true, src.Objects)
	page.Reread = true
	return unread(page, failed)
}

// releasable reports a failure of a session's object that a release of its
// row could explain: the object gone or not opening, or the store failing as
// it was opened or read — never a caller that left, an archive that read
// whole and is malformed, or one this build does not read, which no release
// makes.
func releasable(failed error) bool {
	var malformed *executor.MalformedArchiveError
	return !errors.Is(failed, context.Canceled) && !errors.Is(failed, context.DeadlineExceeded) &&
		!errors.As(failed, &malformed) && !errors.Is(failed, executor.ErrUnsupportedArchive)
}

// readPage reads the page of read's transcript from the entry from on, as ask
// reads it: its object opened and read through the runner's own format
// knowledge — or the page saying why it holds no entries, or what kept the
// object from being read ([unread] says it). reading says the session was
// being read before this page: a row naming no object then names none any
// more.
func readPage(ctx context.Context, ask Ask, read executordomain.SessionRead, from int, reading bool,
	objects Objects) (Page, error) {

	row := read.Session
	page := Page{Read: read, Transcript: executor.Transcript{Entries: []executor.TranscriptEntry{},
		Places: []executor.EntryPlace{}, From: from, Next: from}}
	if !row.HasObject() {
		// REDACTED: why a capture was not kept quotes what refused it.
		page.State, page.Why = TranscriptNone, redact.Secrets(row.NotKept())
		if reading {
			page.Why += "; " + nothingMore
		}
		return page, nil
	}
	if ok, why := executor.Carries(row.Harness, row.ArchiveFormat); !ok {
		page.State, page.Why = TranscriptUnsupported, why
		return page, nil
	}
	// THE READ OPENS THE OBJECT, once it holds one of the node's transcript
	// slots ([executor.TranscriptReadsAtOnce]), and closes it before it gives
	// the slot back: an object opened here would hold its store's connection
	// for as long as the read waited for one.
	open := func(ctx context.Context) (io.ReadCloser, error) { return objects.Open(ctx, row) }
	transcript, err := executor.ReadTranscriptAs(ctx, ask.By, ask.Filter, row.Harness, row.ArchiveFormat,
		open, from)
	if err != nil {
		return page, err
	}
	page.State, page.Transcript = TranscriptRead, transcript
	if transcript.More() {
		page.Next = Cursor(row.Object.Key, transcript.Next)
	}
	return page, nil
}

// unread is page as what kept its session's object from being read says it,
// and page itself where nothing did (err nil): gone, sealed out of reach,
// malformed or of a format this build does not read are each a state of the
// page; a caller that left is returned as it left; anything else is a store
// that did not answer, which a caller retries. Said of an object the turn's
// row still names ([Read]).
func unread(page Page, err error) (Page, error) {
	if err == nil {
		return page, nil
	}
	row := page.Read.Session
	var malformed *executor.MalformedArchiveError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// THE ROW IT WAS READING, and no page of it.
		return Page{Read: page.Read}, err
	// GONE BEFORE UNREADABLE: the store's own absence is wrapped in the
	// domain's unreadable, and a reader acts on the two differently.
	case errors.Is(err, objstore.ErrNotFound):
		page.State, page.Why = TranscriptMissing, "the object store no longer holds this "+
			"session's object, which the turn's row still names"
	case errors.Is(err, executordomain.ErrUnreadable):
		page.State, page.Why = TranscriptUnreadable, "this session's object does not open: it "+
			"is sealed under a key this fleet's keyring no longer holds, or its bytes are "+
			"not the ones sealed for this turn"
	case errors.As(err, &malformed):
		// THE ENGINE'S SENTENCE ALONE, never the decoder's beneath it: a
		// decoder quotes what it read, and what it read is a box's.
		page.State, page.Why = TranscriptMalformed, "this session opened, and is not an archive "+
			"of its harness's format: "+malformed.Why
	case errors.Is(err, executor.ErrUnsupportedArchive):
		page.State, page.Why = TranscriptUnsupported, errorTail(err, executor.ErrUnsupportedArchive)
	default:
		return Page{}, fmt.Errorf("%w: the object store did not answer for %s's session of the turn "+
			"%s: %w", ErrUnavailable, row.Handle, row.Turn, err)
	}
	page.Transcript.Entries, page.Transcript.Places = []executor.TranscriptEntry{}, []executor.EntryPlace{}
	return page, nil
}

// errorTail is what err says after sentinel's own words, which name a package
// rather than what happened.
func errorTail(err, sentinel error) string {
	text := err.Error()
	if at := strings.LastIndex(text, sentinel.Error()+": "); at >= 0 {
		return text[at+len(sentinel.Error())+2:]
	}
	return text
}

// Cursor is the cursor a transcript's next page is read from: the object the
// page was read from, the version of what counts as a transcript's entry
// ([executor.TranscriptVersion]), and the ordinal of the entry the next page
// starts at. See the package doc for why each is in it.
func Cursor(key objstore.Key, next int) string {
	return key.String() + "." + strconv.Itoa(executor.TranscriptVersion) + "." + strconv.Itoa(next)
}

// ordinalOf is the ordinal a page of row's transcript starts at: 0 for no cursor,
// and the cursor's own for one the row's object handed out under this build's
// count of entries. A cursor of an object the row has since replaced with a
// newer capture, or of another count, is refused, saying to read again from
// the start.
//
// A ROW THAT NAMES NO OBJECT ANY MORE IS NOT CAPTURED AGAIN: the seat's
// session budget released its object, or the turn's end did, its newest
// capture not kept — each only of an ended turn — and an ended turn is never
// captured again. So its cursor is the page's to answer, as the first page is
// answered — no session kept, and why ([readPage]) — where a refusal saying
// the session was captured since, and to read it from its start, said what was
// false and sent the reader to a first page saying something else.
func ordinalOf(cursor string, row executordomain.Session) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 {
		return 0, notACursor(cursor)
	}
	version, versionErr := strconv.Atoi(parts[1])
	at, atErr := strconv.Atoi(parts[2])
	switch {
	case versionErr != nil || atErr != nil || version < 1 || at < 0:
		return 0, notACursor(cursor)
	case version != executor.TranscriptVersion:
		return 0, fmt.Errorf("%w: the cursor %q was handed out by a build of the engine that "+
			"counts a transcript's entries otherwise: read the transcript again from its start",
			ErrCursor, cursor)
	case row.HasObject() && parts[0] != row.Object.Key.String():
		return 0, fmt.Errorf("%w: the turn %s's session was captured again since the cursor "+
			"%q was handed out: read its transcript again from the start", ErrCursor,
			row.Turn, cursor)
	}
	return at, nil
}

// notACursor refuses a cursor no page of a transcript hands out.
func notACursor(cursor string) error {
	return fmt.Errorf("%w: the cursor parameter is one a page of this transcript "+
		"handed out, and %q isn't", ErrCursor, cursor)
}
