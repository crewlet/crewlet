package sandbox

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// THE SESSION A CONVERSATION'S NEXT TURN CONTINUES, FROZEN AT ITS FIRST LAUNCH.
//
// A turn's dispatch reads its conversation's head once, at `linearizable`
// (internal/engine; executordomain.Head). The first launch freezes that read on
// the run record ([PendingRun.Base]), written by [PendingStore.BeginLaunch] on
// the write that creates the record and no other, so every relaunch on any node
// reads the first launch's read; and records it on the executor log as the
// turn's `begin` ([SessionWrites.Begin]), whose row HOLDS the base against every
// release while the turn is live and has kept no session, so a relaunch can
// still fetch it. Readers: [Coordinator.recordCapture], [Coordinator.continueBase],
// [Coordinator.ContinuedCapture], [Coordinator.overBudget] (counts every live
// base) and [ReadContinuityOf], [TargetAsk].

// ConversationBaseMaxBytes bounds the freeze as the run's record carries it,
// as JSON: what [NewConversationBase] can make of any read the executor log
// answers.
//
// TWENTY-FOUR KIBIBYTES, from the log's bounds: at most executordomain's
// MaxAbsorbs and MaxSiblings turns (under 2.5 KB) and one base with free text
// bounded to 2,527 bytes, JSON escaping six bytes a byte: about fifteen
// kilobytes, under nineteen with keys. internal/engine's
// TestAFrozenReadFitsItsBound holds the largest admissible read here. It rides
// beside the frozen endings (internal/agent/prompts' EndingsMaxBytes).
const ConversationBaseMaxBytes = 24 << 10

// ConversationBase is the read a turn's first launch made of its conversation,
// frozen on the turn's run record ([PendingRun.Base]).
//
// A WIRE FORMAT like the rest of the record, and a section that grows: a key a
// later build adds is carried in Extra (wirekeys.go).
type ConversationBase struct {
	// ReadThrough is where the head was read on the executor log, packed
	// (statelog.Position.Packed): what the turn's begin, and every capture
	// of the turn where no begin landed, records as its first read.
	ReadThrough int64 `json:"read_through"`

	// Absorbs are the turns the read handed out in full and the turn's brief
	// told ([executorkeep.ConversationHead.Absorbs], restricted to the told): what the
	// turn's end absorbs once it has landed a capture. A turn the brief's
	// bound left out is not among them, and stays for the next read.
	Absorbs []string `json:"absorbs,omitempty"`

	// Siblings are the ended turns of the conversation that still name a
	// session, ran beside the head, and the brief told: each its turn and its
	// object. Told and absorbed; none is held, and this piece opens none.
	Siblings []BaseSibling `json:"siblings,omitempty"`

	// Session is the BASE: the head the read named, which the turn's launches
	// continue while the turn has kept no session of its own. Nil where the
	// read named none.
	Session *BaseSession `json:"session,omitempty"`

	// Extra is every key of the section this build does not know, kept
	// verbatim and written back by every write of the record (wirekeys.go).
	Extra map[string]json.RawMessage `json:"-"`
}

// BaseSession is the head a turn's first read froze: what a launch fetches,
// opens, restores and tells its run of it.
type BaseSession struct {
	// Turn is the base row's turn: the seal's associated data is its handle,
	// this turn and its conversation's key, so a launch reads its row by it.
	Turn string `json:"turn"`

	// Object, ObjectHash and ObjectSize are the whole sealed object its row
	// named when it was read (the store checks the size on open), and
	// ContentKey the keyring key that sealed it.
	Object     string `json:"object"`
	ObjectHash string `json:"object_hash"`
	ObjectSize int64  `json:"object_size"`
	ContentKey string `json:"content_key,omitempty"`

	// Harness, HarnessVersion, SessionID and ArchiveFormat are the session as
	// its box described it: what a runner's restore reads ([ArchiveOf]).
	Harness        string `json:"harness"`
	HarnessVersion string `json:"harness_version,omitempty"`
	SessionID      string `json:"session_id"`
	ArchiveFormat  int    `json:"archive_format"`

	// Entry is the providers.llm entry the session ran under
	// ([SessionArchive.Entry]), "" where its row names none.
	Entry string `json:"entry,omitempty"`

	// Home and Workspace are where it was last worked, which the run that
	// continues it in a new box is told its absolute paths were.
	Home      string `json:"home,omitempty"`
	Workspace string `json:"workspace,omitempty"`

	// ContextTokens is what its last request carried, which a launch that
	// continues it sends first and its floor is made of; Interrupted that its
	// harness was cut off when it was saved.
	ContextTokens int64 `json:"context_tokens,omitempty"`
	Interrupted   bool  `json:"interrupted,omitempty"`

	// CapturedAt is when it was kept; WentOn that its turn's end said the turn
	// went on past it, so the run that continues it is told it was kept while
	// that turn still worked, not that it runs through the turn's end; and
	// Lost that the turn ended lost, which no frame of it saw end, so the run
	// is told the session may not hold the last of what that turn did
	// ([executorkeep.TurnRow.WentOn]).
	CapturedAt time.Time `json:"captured_at,omitzero"`
	WentOn     bool      `json:"went_on,omitempty"`
	Lost       bool      `json:"lost,omitempty"`

	// EndedAt is when its turn ended: where the turn that continues it
	// begins.
	EndedAt time.Time `json:"ended_at,omitzero"`

	// Extra is every key of the section this build does not know (wirekeys.go).
	Extra map[string]json.RawMessage `json:"-"`
}

// BaseSibling is one sibling a turn's brief told: its turn and its object.
type BaseSibling struct {
	Turn       string `json:"turn"`
	Object     string `json:"object"`
	ObjectHash string `json:"object_hash"`
	ObjectSize int64  `json:"object_size"`

	// Extra is every key of the section this build does not know (wirekeys.go).
	Extra map[string]json.RawMessage `json:"-"`
}

// NewConversationBase is what a turn's first launch freezes of head, the read
// its dispatch made, given told, the turns its brief told
// (internal/agent/prompts' BuildConversationEndings): where the read was made,
// the turns it handed out that the brief told, the siblings among them, and the
// head as the base where the brief told it — which it always does. Nil where
// the read was made at no position, a log with no record yet, which handed out
// nothing a record could name.
func NewConversationBase(head executorkeep.ConversationHead, told []string) *ConversationBase {
	if head.ReadThrough == 0 {
		return nil
	}
	isTold := make(map[string]bool, len(told))
	for _, turn := range told {
		isTold[turn] = true
	}
	base := &ConversationBase{ReadThrough: head.ReadThrough}
	for _, turn := range head.Absorbs {
		if isTold[turn] {
			base.Absorbs = append(base.Absorbs, turn)
		}
	}
	for _, s := range head.Siblings {
		if isTold[s.Turn] && s.HasObject() {
			base.Siblings = append(base.Siblings, BaseSibling{Turn: s.Turn, Object: s.Object.Key.String(),
				ObjectHash: string(s.Object.Hash), ObjectSize: s.Object.Size})
		}
	}
	if s := head.Session; s != nil && s.HasObject() && isTold[s.Turn] {
		session := baseSessionOf(*s)
		base.Session = &session
	}
	return base
}

// baseSessionOf is row — an ended turn's row that names a session — as the base
// a launch continues: what a turn's first launch freezes of the head its
// dispatch read ([NewConversationBase]), and what a reader of a seat's sessions
// decides a conversation's head by ([ReadContinuityOf]), so the two decide one
// shape by one decision ([BaseContinuityOf]).
func baseSessionOf(row executorkeep.TurnRow) BaseSession {
	return BaseSession{Turn: row.Turn,
		Object: row.Object.Key.String(), ObjectHash: string(row.Object.Hash), ObjectSize: row.Object.Size,
		ContentKey: row.ContentKey, Harness: row.Harness, HarnessVersion: row.HarnessVersion,
		SessionID: row.SessionID, ArchiveFormat: row.ArchiveFormat, Entry: row.Entry,
		Home: row.Home, Workspace: row.Workspace,
		ContextTokens: row.ContextTokens, Interrupted: row.Interrupted, CapturedAt: row.CapturedAt,
		WentOn: row.WentOn, Lost: row.Lost, EndedAt: row.EndedAt}
}

// archive is the base as a runner's restore takes it ([ArchiveOf]).
func (s BaseSession) archive() SessionArchive {
	return SessionArchive{Harness: s.Harness, SessionID: s.SessionID, Entry: s.Entry, Format: s.ArchiveFormat,
		Version: s.HarnessVersion}
}

// namedBy is why row — the base turn's row on the executor log, read again —
// no longer names the session the read froze, "" where it does: the row holding
// the base is what keeps its object from the collector, and a launch fetches it
// under the row it reads.
func (s BaseSession) namedBy(row executorkeep.TurnRow) string {
	switch {
	case row.Turn != s.Turn:
		return fmt.Sprintf("the executor log answered turn %s's row for turn %s's", row.Turn, s.Turn)
	case !row.HasObject():
		return fmt.Sprintf("turn %s's row on the executor log names no session any more, since the turn's "+
			"first launch read it: %s", s.Turn, row.NotKept)
	case row.Object.Key.String() != s.Object || string(row.Object.Hash) != s.ObjectHash ||
		row.Object.Size != s.ObjectSize:
		return fmt.Sprintf("turn %s's row on the executor log names another session than the one the turn's "+
			"first launch read", s.Turn)
	}
	return ""
}

// BaseContinuityOf is THE decision whether a launch continues the session that
// ended another turn of its conversation (base, as frozen, or a head's). own is
// the turn's executor-log row (nil where unread), run its record (nil before
// its first launch). [ContinuityOf]'s sibling: that decides a session the turn
// kept, of a live row; a base is always another turn's ended row.
//
// A BASE IS CONTINUED ONLY WHILE THE TURN HAS KEPT NOTHING: once a capture of
// its own landed, which session a relaunch continues is [ContinuityOf]'s alone.
// AND ONLY WHILE THE TURN'S ROW HOLDS IT: one naming another base, or that
// landed a capture, holds nothing. A refusal says why the base is not
// continued, never which session continues instead; the box's release and the
// seat's budget are the launch's to ask.
func BaseContinuityOf(base BaseSession, own *executorkeep.TurnRow, run *PendingRun,
	restorer ArchiveRestorer) SessionContinuity {

	if why := baseStanding(base, own, run); why != "" {
		return SessionContinuity{Why: why}
	}
	switch {
	case run != nil && run.sessionExhausted(base.SessionID):
		return SessionContinuity{Why: executorkeep.ExhaustedWhy(base.SessionID, run.TurnID)}
	case restorer == nil:
		return SessionContinuity{Why: fmt.Sprintf("no runner of this build restores a session %s wrote",
			base.Harness)}
	}
	if ok, refused := restorer.Restores(base.archive()); !ok {
		return SessionContinuity{Why: refused}
	}
	return SessionContinuity{Continues: true}
}

// baseStanding is "" where base is still the turn's to continue, whoever would
// take it up (own: the turn's row, nil where unread; run: its record, nil
// before its first launch), or why not: [BaseContinuityOf] less the runner's
// answer and less whether a job of the turn ran out of its model's context in
// it. Where own was read and this is "", the turn's row HOLDS the base: its
// begin named it and the turn has kept nothing since, so every release passes
// it over but a retirement, which takes a context-exhausted session from every
// ended row naming it, held or not, as a session a relaunch may read
// ([Coordinator.ContinuedCapture]). A base a job ran out in is one: no launch
// continues it, another would meet the same end, but its row names it until an
// end retires it, and a run that starts its own session reads it there.
func baseStanding(base BaseSession, own *executorkeep.TurnRow, run *PendingRun) string {
	switch {
	case base.Object == "" || base.Turn == "":
		return "the conversation's last turn left no session to continue"
	case run != nil && run.endDecided():
		return "the turn is ending: its run's ending is decided, and no launch of it follows to continue " +
			"its conversation's session"
	case run != nil && run.Kept != nil:
		return "the turn has kept a session of its own, which holds everything the session its " +
			"conversation's last turn left does, so no launch of the turn continues that one any more"
	case own != nil && own.Ended:
		return "the turn has ended, so no launch of it continues its conversation's session"
	case own != nil && own.LaunchSeq > 0:
		return "the turn's row on the executor log has landed a capture of its own, which lifted its hold on " +
			"the session its conversation's last turn left, so no launch of the turn continues that one any more"
	case own != nil && own.BaseTurn != base.Turn:
		return fmt.Sprintf("the turn's row on the executor log does not hold turn %s's session against its "+
			"release: the turn's begin did not land in time, or found that session gone", base.Turn)
	}
	return ""
}

// MarshalJSON writes the section with the keys a later build added to it.
func (b ConversationBase) MarshalJSON() ([]byte, error) {
	type plain ConversationBase
	return marshalCarrying(plain(b), wireKeys(reflect.TypeFor[ConversationBase]()), b.Extra)
}

// UnmarshalJSON reads the section and keeps the keys this build does not know.
func (b *ConversationBase) UnmarshalJSON(raw []byte) error {
	type plain ConversationBase
	p := plain(*b)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[ConversationBase]()))
	if err != nil {
		return err
	}
	*b = ConversationBase(p)
	b.Extra = extra
	return nil
}

// MarshalJSON writes the base with the keys a later build added to it.
func (s BaseSession) MarshalJSON() ([]byte, error) {
	type plain BaseSession
	return marshalCarrying(plain(s), wireKeys(reflect.TypeFor[BaseSession]()), s.Extra)
}

// UnmarshalJSON reads the base and keeps the keys this build does not know.
func (s *BaseSession) UnmarshalJSON(raw []byte) error {
	type plain BaseSession
	p := plain(*s)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[BaseSession]()))
	if err != nil {
		return err
	}
	*s = BaseSession(p)
	s.Extra = extra
	return nil
}

// MarshalJSON writes the sibling with the keys a later build added to it.
func (s BaseSibling) MarshalJSON() ([]byte, error) {
	type plain BaseSibling
	return marshalCarrying(plain(s), wireKeys(reflect.TypeFor[BaseSibling]()), s.Extra)
}

// UnmarshalJSON reads the sibling and keeps the keys this build does not know.
func (s *BaseSibling) UnmarshalJSON(raw []byte) error {
	type plain BaseSibling
	p := plain(*s)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[BaseSibling]()))
	if err != nil {
		return err
	}
	*s = BaseSibling(p)
	s.Extra = extra
	return nil
}
