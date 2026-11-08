package executorkeep

import (
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// TurnRow is what continuing a turn's kept session reads of the turn's row on
// the executor log: whose turn it is, which session of which harness and
// archive format the row names, the launch and the capture it was kept from,
// when, and the sealed object holding it, whether the turn has ended and gone
// on past that session, the conversation it served and why it names no object
// where it names none.
//
// HERE, NOT AS internal/executordomain's Session, because the restore that
// reads it is the sandbox's (its restore.go), and the sandbox links no state
// log (this package's doc): the row type of the log's domain carries the log's
// positions and so its graph. One method of that domain makes it
// (executordomain.Session.Kept), which the engine's restore and the read
// surface both call, so the fields a relaunch decides on are copied in one
// place and a reader is told what the launch would decide.
type TurnRow struct {
	Handle string
	Turn   string

	Harness        string
	HarnessVersion string
	SessionID      string
	ArchiveFormat  int

	LaunchSeq  int64
	CaptureSeq int64

	// Object is the sealed session, its Key zero on a row that names none.
	Object objstore.Object

	// Ended is a turn whose end applied: no relaunch of it continues its
	// session, and only its conversation's next turn may, where the row is
	// that conversation's head (executordomain.Head: of its ended turns that
	// name a session, the one that read the conversation last).
	Ended bool

	// ConversationKey is the conversation the turn served, "" for none: a
	// session is continued past its turn's end only by the next turn of the
	// conversation it served, so a row serving none is continued by nobody
	// once its turn ends.
	ConversationKey string

	// NotKept is why the row names no session's object, in a reader's words
	// (executordomain.Session.NotKept, the domain's one account of it), ""
	// where it names one: the restore tells a reader why nothing is continued.
	NotKept string

	// ContentKey is the keyring key that sealed the object, and Home and
	// Workspace where the session was last worked: what a launch continuing
	// it after its turn ended is told, having no run record to read them
	// from.
	ContentKey string
	Home       string
	Workspace  string

	// ContextTokens is what the session's last request carried, which a
	// launch continuing it sends first; Interrupted that its harness was cut
	// off when it was saved.
	ContextTokens int64
	Interrupted   bool

	// CapturedAt is when the session the row names was kept, zero where it
	// names none.
	CapturedAt time.Time

	// WentOn says the turn went on past the session the row names, or may
	// have (executordomain.Session.WentOn): its end said so, or it was lost,
	// and Lost says the latter. A conversation's next turn that continues the
	// session is told that it was kept while the turn still worked, and what
	// that turn did after it is not in it — or, of a lost turn, that it may not
	// hold the last of what the turn did — never that it runs through the
	// turn's end.
	WentOn bool
	Lost   bool

	// EndedAt is when the turn ended, zero while it is live.
	EndedAt time.Time

	// BaseTurn is the turn whose session this turn's begin holds against its
	// release ("" where it holds none): a relaunch of a turn that has kept
	// nothing continues that base only while its own row holds it.
	BaseTurn string
}

// ConversationHead is a read of a conversation's head as a turn's first launch
// freezes it (internal/sandbox's NewConversationBase): where it was read, the
// turns it handed out, the siblings that ran beside the head, and the head.
//
// HERE, NOT AS internal/executordomain's Head, for [TurnRow]'s reason: the
// sandbox freezes it and links no state log, and the log's read carries its
// position as the state log's type. One method of the domain makes it
// (executordomain.Head.Kept).
type ConversationHead struct {
	// ReadThrough is where the head was read on the executor log, packed (the
	// state log's Position.Packed), 0 for a read made at no position: a log
	// with no record yet, which handed out nothing a record could name.
	ReadThrough int64

	// Absorbs are the turns the read handed out in full
	// (executordomain.Head.Absorbs): what the reading turn's end absorbs.
	Absorbs []string

	// Siblings are the ended turns of the conversation that still name a
	// session and ran beside the head.
	Siblings []TurnRow

	// Session is the head: of the conversation's ended turns whose rows still
	// name a session, the one that read the conversation last, nil where none
	// does.
	Session *TurnRow
}

// HasObject reports a row that names a session's object.
func (r TurnRow) HasObject() bool { return !r.Object.Key.IsZero() }
