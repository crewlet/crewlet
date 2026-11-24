package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A MODEL CHECK is the doctor over the API (`POST /llm/providers/{key}/check`)
// for one model entry: running, or how it ended. Its harness runs outlast what
// a proxy lets a request stay silent, so the page polls, and a poll may reach
// any node: the check is one record in the coordination store, written by the
// node running it. Filed under the ENTRY's key, so one check per entry across
// the fleet is the store's create-if-absent, not a lock: a check asked while
// one runs is handed it or refused, and one asked over an ended, lapsed or
// lost check replaces it at the version read.

// A record lives [ModelCheckLinger] past its end, or past its deadline while
// running: a per-record lifetime a bucket age cannot hold (an update restarts
// it; internal/coord/kv's package doc), so it lives in the positions register
// and every reader answers a [ModelCheck.Lapsed] record absent. A check whose
// node stopped reads [ModelCheck.Lost] past its deadline. Its answer is the
// API's rendering, cut to [MaxModelCheckAnswerBytes] by its writer
// ([ModelCheck.Validate] refuses a longer one), and never holds a credential
// value: the doctor names variables, not what they hold.

// ModelCheckClass is the key class a check is filed under in the positions
// register.
const ModelCheckClass = "llm_check"

// ModelCheckKey is an entry's check key in the register.
func ModelCheckKey(entry string) string { return DocumentKey(ModelCheckClass, entry) }

// ModelCheckEntry recovers the entry's key from a key of the check class.
func ModelCheckEntry(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != ModelCheckClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// ModelCheckLinger is how long a check answers after it ends, or after its
// deadline while it runs: ten minutes, a device sign-in's linger, so a page
// that polls every few seconds, or a person who looked away, still reads how
// it ended.
const ModelCheckLinger = 10 * time.Minute

// MaxModelCheckAnswerBytes bounds a check's answer: 64 KiB, the doctor's
// dozen lines with room for each to quote what the harness said, and a
// sixteenth of the 1 MiB a NATS server carries by default, so a record is
// read in one message beside every other record of the register.
const MaxModelCheckAnswerBytes = 64 << 10

// MaxModelCheckFailureBytes bounds why a check ended without an answer: one
// sentence, the node that stopped or the check that could not run, as an MCP
// check's sentences are bounded.
const MaxModelCheckFailureBytes = 1 << 10

// ModelCheck is one entry's check, as the coordination store holds it.
type ModelCheck struct {
	// ID is the check's id, what an answer names it by.
	ID uuid.UUID `json:"id"`

	// Entry is the model entry's key, and the record's key.
	Entry string `json:"entry"`

	// Node is the node running the check, whose harness it runs.
	Node string `json:"node"`

	// Draft is whether the check is of a draft of the entry, as a create or
	// an edit would write it, rather than of the entry as it runs.
	Draft bool `json:"draft"`

	// Smoke is whether the check makes the harness's real calls, rather
	// than reading only what the node holds.
	Smoke bool `json:"smoke"`

	// StartedAt is when the check started, and Deadline when the node
	// running it gives it up: its budget past the start.
	StartedAt time.Time `json:"started_at"`
	Deadline  time.Time `json:"deadline"`

	// EndedAt is when the check ended, zero while it runs.
	EndedAt time.Time `json:"ended_at,omitzero"`

	// Answer is what an ended check found, as the API renders it. Failure
	// is why one ended without an answer instead. Neither is set while the
	// check runs, and exactly one is once it has ended.
	Answer  json.RawMessage `json:"answer,omitempty"`
	Failure string          `json:"failure,omitempty"`

	// By, ByKind and OperatorID are who asked for it, as [SeatPause]
	// records them.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// Running reports whether the check has not ended. A running check past its
// deadline is [ModelCheck.Lost], not running, to a reader.
func (c ModelCheck) Running() bool { return c.EndedAt.IsZero() }

// Lost reports whether the check was still running at its deadline as now
// reads it: the node running it stopped, or could not record its end, and
// nothing will end it.
func (c ModelCheck) Lost(now time.Time) bool { return c.Running() && now.After(c.Deadline) }

// Lapsed reports whether the record is past its lifetime at now: more than
// [ModelCheckLinger] after the check ended, or after its deadline while it
// still runs. A lapsed record is read as absent everywhere.
func (c ModelCheck) Lapsed(now time.Time) bool {
	end := c.EndedAt
	if c.Running() {
		end = c.Deadline
	}
	return now.After(end.Add(ModelCheckLinger))
}

// Validate reports why a check cannot be written.
func (c ModelCheck) Validate() error {
	switch {
	case c.ID == uuid.Nil:
		return errors.New("coord: a model check needs its id")
	case c.Entry == "":
		return errors.New("coord: a model check needs the key of the entry it checks")
	case c.Node == "":
		return errors.New("coord: a model check needs the node running it: a poll names it " +
			"when the check is lost")
	case c.StartedAt.IsZero() || !c.Deadline.After(c.StartedAt):
		return errors.New("coord: a model check needs when it started and a deadline after that")
	case c.By == "" || !c.ByKind.Valid():
		return errors.New("coord: a model check needs who asked for it")
	case c.Running() && (len(c.Answer) > 0 || c.Failure != ""):
		return errors.New("coord: a model check that has not ended has no answer and no failure")
	case !c.Running() && c.EndedAt.Before(c.StartedAt):
		return errors.New("coord: a model check cannot end before it started")
	case !c.Running() && (len(c.Answer) > 0) == (c.Failure != ""):
		return errors.New("coord: an ended model check carries its answer or why it has none, " +
			"and not both")
	case encodedJSON(c.Answer) > MaxModelCheckAnswerBytes:
		return fmt.Errorf("coord: a model check's answer is %d bytes as stored, past its %d: its "+
			"writer cuts the lines that would take it past", encodedJSON(c.Answer), MaxModelCheckAnswerBytes)
	case len(c.Answer) > 0 && !json.Valid(c.Answer):
		return errors.New("coord: a model check's answer is not JSON")
	case len(c.Failure) > MaxModelCheckFailureBytes:
		return fmt.Errorf("coord: a model check's failure is %d bytes, past its %d", len(c.Failure),
			MaxModelCheckFailureBytes)
	case len(c.Entry) > MaxModelEntryBytes || !plainWithin(c.Node, MaxNodeIDBytes):
		return fmt.Errorf("coord: a model check names an entry past %d bytes or a node that is no "+
			"node id", MaxModelEntryBytes)
	}
	return checkActor("a model check", c.By, c.OperatorID)
}

// MaxModelCheckBytes bounds a check's record as stored: its answer at
// [MaxModelCheckAnswerBytes], or its failure, beside an entry of
// [MaxModelEntryBytes] escaped at its widest and its fixed fields, under 2 KiB
// together; 4 KiB is room for them. Every read of it decides a write a person
// waits on, so it crosses a leaf's link at the slowest pace in 2.7 s.
const MaxModelCheckBytes = MaxModelCheckAnswerBytes + 4<<10

// ModelChecks is the fleet's record of each model entry's latest check.
//
// The store answers what it holds; whether a record is [ModelCheck.Lost] or
// has [ModelCheck.Lapsed] is the reader's to judge against its own clock,
// since the store has none.
type ModelChecks interface {
	// ModelCheck reads one entry's check.
	ModelCheck(ctx context.Context, entry string) (ModelCheck, bool, error)

	// ListModelChecks returns every check, ordered by entry key.
	ListModelChecks(ctx context.Context) ([]ModelCheck, error)

	// CreateModelCheck writes a check for an entry that has no record,
	// returning it as stored. An entry that has one is left alone and
	// reports false: only an update at the version its caller read may
	// replace it.
	CreateModelCheck(ctx context.Context, c ModelCheck) (ModelCheck, bool, error)

	// UpdateModelCheck writes c at c.Version, reporting false when that
	// version no longer holds, including when the record is gone.
	UpdateModelCheck(ctx context.Context, c ModelCheck) (ModelCheck, bool, error)

	// DeleteModelCheck removes an entry's check at a version, reporting
	// whether that version still held. A version of zero never holds.
	DeleteModelCheck(ctx context.Context, entry string, version uint64) (bool, error)
}
