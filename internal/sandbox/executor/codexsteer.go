package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// CodexSteerInstruction is the standing instruction about notes a Codex run is
// given. The note helper steers each note into the running turn
// (`turn/steer`), and Codex 0.171.0 adds it to the next request as a user
// message after the tool output of the step in flight (measured).
const CodexSteerInstruction = steerInstructionHead +
	"Codex adds it to the conversation between your steps, as a message from the " +
	"user after the tool results of a step. " + steerInstructionTail

// NoteInstruction is the standing instruction about notes a Codex run is given:
// it takes a person's notes between its steps through the note helper its
// script starts beside it ([Codex.Command]).
func (Codex) NoteInstruction() string { return CodexSteerInstruction }

// inboxID is the id the note helper admits a note under for a job, so a
// repeat serve is steered once: `note_` and 32 hex digits of a hash of the job
// and the note. Codex records no id of the engine's, so the confirm matches by
// the item's digest instead ([Codex.notesHeld]).
func (Codex) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return "note_" + hex.EncodeToString(sum[:])[:32]
}

// pollCeiling is the most UTF-16 units one message the note helper steers can
// carry: [codexSteerMaxCodePoints], since a text never holds more code points
// than UTF-16 units, so a message held to as many units is within turn/steer's
// bound. The model's window, not the call, is the real bound: a steer past it
// is compacted (ADR-0177's "Confirmed").
func (Codex) pollCeiling() int { return codexSteerMaxCodePoints }

// notesHeld is the inbox ids of takes whose item Codex's rollout of session
// holds as a user message, matched by its rendered text's digest
// ([ItemDigest]): the helper never confirms, so the record is the one confirm
// path ([recordConfirmer]). A torn last line is never read, and a note found
// only inside a compaction's summary is no user message ([codexRollout]).
func (Codex) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {

	find := codexRolloutPath(PathsFor(box), session.ID)
	if find == "" {
		return nil, fmt.Errorf("executor: %q is no Codex session the run reported", session.ID)
	}
	found, err := box.Exec(ctx, find, sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: finding Codex's rollout of session %s: %w", session.ID, err)
	}
	path := strings.TrimSpace(found.Stdout)
	if path == "" {
		return nil, fmt.Errorf("executor: the box holds no Codex rollout of session %s", session.ID)
	}
	body, err := box.OpenFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("executor: opening Codex's rollout of session %s: %w", session.ID, err)
	}
	defer func() { _ = body.Close() }()
	rollout, err := readCodexRollout(body)
	if err != nil {
		return nil, fmt.Errorf("executor: reading Codex's rollout of session %s: %w", session.ID, err)
	}
	recorded := make(map[string]bool, len(rollout.UserMessages()))
	for _, text := range rollout.UserMessages() {
		recorded[ItemDigest(text)] = true
	}
	var held []string
	for _, take := range takes {
		if recorded[take.Digest] {
			held = append(held, take.InboxID)
		}
	}
	return held, nil
}
