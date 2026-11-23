package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// HermesSteerInstruction is the standing instruction about notes a Hermes run
// is given. The runner's plugin steers each note into the running turn
// (`session.steer`), and Hermes 0.23.3 adds it after the newest tool result as
// a user message it frames as out of band (measured).
const HermesSteerInstruction = steerInstructionHead +
	"Hermes adds it to the conversation between your steps, as a message from the user after the " +
	"tool results of a step, framed as an out-of-band message. " + steerInstructionTail

// NoteInstruction is the standing instruction about notes a Hermes run is
// given: it takes a person's notes between its steps through the runner's
// plugin ([hermesPluginSource]).
func (Hermes) NoteInstruction() string { return HermesSteerInstruction }

// notesOffWhy is what can switch a Hermes run's reading of notes off.
func (Hermes) notesOffWhy() string {
	return "a plugin that could not reach the run's notes route or the run's gateway, which the box's " +
		"Hermes gateway log names"
}

// inboxID is the id a note is admitted under for a job: `note_` and 32 hex
// digits of a hash of the job and the note. Hermes records no id of the
// engine's, so the confirm matches by the item's digest instead
// ([Hermes.notesHeld]).
func (Hermes) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return "note_" + hex.EncodeToString(sum[:])[:32]
}

// hermesPollCeiling is the most UTF-16 units one note the plugin steers may
// carry: 4,505, the engine's item bound (internal/engine's maxLateItemUnits)
// the note contract holds every poll runner to.
const hermesPollCeiling = 4505

// pollCeiling is [hermesPollCeiling].
func (Hermes) pollCeiling() int { return hermesPollCeiling }

// hermesNoteOpen and hermesNoteClose frame a steered note in the user message
// Hermes 0.23.3 records it as (measured): the note's text between them, byte
// for byte.
const (
	hermesNoteOpen = "[OUT-OF-BAND USER MESSAGE — a direct message from the user, delivered once at this " +
		"position; not tool output and not a new delivery when replayed from conversation history]\n"
	hermesNoteClose = "\n[/OUT-OF-BAND USER MESSAGE]"
)

// hermesSteered is the note a user message of the record carries, and false
// for a message Hermes did not frame as a steered note.
func hermesSteered(text string) (string, bool) {
	inner, ok := strings.CutPrefix(text, hermesNoteOpen)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(inner, hermesNoteClose)
}

// notesHeld is the inbox ids of takes whose item Hermes's record of session
// holds as a steered note, matched by its text's digest ([ItemDigest]): the
// plugin never confirms, so the record is the one confirm path
// ([recordConfirmer]). The record is the box's session store, dumped as the
// pack dumps it ([hermesCarryScript]).
func (Hermes) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {
	if !sandbox.ValidSessionID(session.ID) {
		return nil, fmt.Errorf("executor: %q is no Hermes session the run named", session.ID)
	}
	paths := PathsFor(box)
	q := shellword.Quote
	read := paths.HermesRecordRead()
	res, err := box.Exec(ctx, hermesCarryRun(paths)+fmt.Sprintf("hcarry dump %s %s /dev/null %s",
		q(paths.HermesStateDB()), q(session.ID), q(read)), sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: reading Hermes's record of session %s: %w", session.ID, err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "crewlet-carry ok" {
		return nil, fmt.Errorf("executor: reading Hermes's record of session %s answered %q: %s", session.ID,
			firstLine(got, transcriptDetailLimit), firstLine(res.Stderr, transcriptDetailLimit))
	}
	body, err := box.OpenFile(ctx, read)
	if err != nil {
		return nil, fmt.Errorf("executor: opening Hermes's record of session %s: %w", session.ID, err)
	}
	defer func() { _ = body.Close() }()
	recorded := map[string]bool{}
	err = hermesRows(body, func(row hermesRow) error {
		if row.Table != "messages" || row.Message.Role != "user" {
			return nil
		}
		if note, ok := hermesSteered(row.Message.text()); ok {
			recorded[ItemDigest(note)] = true
		}
		return nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("executor: reading Hermes's record of session %s: %w", session.ID, err)
	}
	var held []string
	for _, take := range takes {
		if recorded[take.Digest] {
			held = append(held, take.InboxID)
		}
	}
	return held, nil
}
