package sandbox

import (
	"encoding/json"
	"errors"

	"github.com/crewlet/crewlet/internal/redact"
)

// A colleague's question is kept as asked on the run's row
// ([PendingRun.ColleagueAsk]): the answer carries it back to the asking seat's
// model, which wrote it. Every other reader is shown it redacted, so it is read
// through two accessors and written in two encodings. SERVED:
// [ColleagueAsk.MarshalJSON], which every JSON encoding of an ask or a row goes
// through, writes [ColleagueAsk.Question] and says so (`question_redacted`).
// STORED: [storedAsk], the run record's own codec ([encodeRun], [decodeRun]),
// writes the raw words under the same wire name.
//
// A SERVED COPY NEVER BECOMES A ROW: [ColleagueAsk.UnmarshalJSON] refuses every
// decode, and the record's codec refuses an ask carrying `question_redacted`, so
// such a record is skipped by every listing ([CoordStore.decodedOrSkipped]).

// errAskNotStored is a decode of an ask outside its run's record.
var errAskNotStored = errors.New("sandbox: a colleague's ask decodes only from its run's record " +
	"(the run store's codec); a served copy holds its question redacted")

// errServedAsk is a run record holding a served copy of its ask.
var errServedAsk = errors.New("sandbox: the record's colleague's ask is a served copy, its question " +
	"redacted, so no answer can be built from it; remove the record once its run is over")

// WithQuestion is the ask holding words as the colleague asked them.
func (a ColleagueAsk) WithQuestion(words string) ColleagueAsk {
	a.question = words
	return a
}

// Question is the colleague's question redacted ([redact.Secrets]): what every
// reader is shown but the one [ColleagueAsk.RawQuestion] names.
func (a ColleagueAsk) Question() string { return redact.Secrets(a.question) }

// RawQuestion is the colleague's question as asked. ONLY THE ANSWER TO THE
// ASKING SEAT reads it: the engine echoes it on the reply
// (internal/engine's answerColleague), back to the model that wrote it. Every
// other reader, the dashboard, the API, listings, live state, the observer and
// events included, is shown [ColleagueAsk.Question], so a new reader is
// redacted unless it asks, and `git grep RawQuestion` lists who reads it raw,
// tests and sandboxtest's shared cases aside. In this package the row's
// writers alone read the field itself: the launch's cut ([keptQuestion]) and
// the record's codec ([storedAsk]).
func (a ColleagueAsk) RawQuestion() string { return a.question }

// MarshalJSON writes the ask as it is served: its question redacted.
func (a ColleagueAsk) MarshalJSON() ([]byte, error) {
	type plain ColleagueAsk
	return json.Marshal(struct {
		plain
		Question         string `json:"question,omitempty"`
		QuestionRedacted bool   `json:"question_redacted"`
	}{plain(a), a.Question(), true})
}

// UnmarshalJSON refuses: an ask is read only from its run's record
// ([storedAsk]), never from a copy that was served.
func (a *ColleagueAsk) UnmarshalJSON([]byte) error { return errAskNotStored }

// storedAsk is a [ColleagueAsk] as its run's record stores it: the question as
// asked, under the wire name `question`.
type storedAsk ColleagueAsk

// MarshalJSON writes the ask as the record stores it.
func (s storedAsk) MarshalJSON() ([]byte, error) {
	type plain ColleagueAsk
	return json.Marshal(struct {
		plain
		Question string `json:"question,omitempty"`
	}{plain(s), s.question})
}

// UnmarshalJSON reads the ask as the record stores it, refusing a served copy.
func (s *storedAsk) UnmarshalJSON(raw []byte) error {
	type plain ColleagueAsk
	var w struct {
		plain
		Question         string `json:"question"`
		QuestionRedacted bool   `json:"question_redacted"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	if w.QuestionRedacted {
		return errServedAsk
	}
	*s = storedAsk(w.plain)
	s.question = w.Question
	return nil
}

// storedRun is a run as its record stores it: the row, its colleague's ask in
// the stored encoding. The ask's field here shadows the row's own, since
// encoding/json takes the shallower of two fields of one name.
type storedRun struct {
	PendingRun
	ColleagueAsk *storedAsk `json:"colleague_ask,omitempty"`
}

// storedRunOf is run as its record stores it.
func storedRunOf(run PendingRun) storedRun {
	stored := storedRun{PendingRun: run}
	stored.PendingRun.ColleagueAsk = nil
	if ask := run.ColleagueAsk; ask != nil {
		kept := storedAsk(*ask)
		stored.ColleagueAsk = &kept
	}
	return stored
}

// run is the row a record stored.
func (s storedRun) run() PendingRun {
	run := s.PendingRun
	run.ColleagueAsk = nil
	if ask := s.ColleagueAsk; ask != nil {
		asked := ColleagueAsk(*ask)
		run.ColleagueAsk = &asked
	}
	return run
}
