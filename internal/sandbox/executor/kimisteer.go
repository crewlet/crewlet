package executor

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

// KimiSteerInstruction is the standing instruction about notes a Kimi Code run
// is given; where a note lands is Kimi's, measured on 2.7.3: a steer
// opens a turn of its own after the result of the tool that was running, and a
// request it arrives during is cut short and asked again with it.
const KimiSteerInstruction = steerInstructionHead +
	"Kimi Code adds it to the conversation as a message from the user: after the " +
	"result of the tool that was running when it came, or, when it came as you " +
	"finished, as a new message you answer before the run ends. " + steerInstructionTail

// kimiDriverSource is the run's driver (kimi_driver.js), embedded so it matches
// this build's bridge, decoder and confirm reader.
//
//go:embed kimi_driver.js
var kimiDriverSource string

// kimiNotePrefix opens the prompt_id every note is steered under
// (kimi_driver.js's notePrefix), which Kimi keeps as the note's message id, so
// the confirm reader takes no other message for a note ([kimiNotesHeld]).
const kimiNotePrefix = "crewlet-note-"

// KimiNoteID is the message id the note admitted under inbox carries in the
// session's record: the prompt_id the driver steered it under.
func KimiNoteID(inbox string) string { return kimiNotePrefix + inbox }

// inboxID is the id a Kimi run's driver steers a note under for a job (its
// prompt_id, [KimiNoteID]): 32 hex digits of a hash of the job and the note.
// UNIQUE TO BOTH and STABLE, for [Copilot.inboxID]'s reasons; KAP answers a
// reused prompt_id with the prompt it already holds (measured on 2.7.3), so a
// note admitted twice lands once.
func (Kimi) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return hex.EncodeToString(sum[:])[:32]
}

// kimiPollCeiling is the most UTF-16 units one note the driver steers may
// carry: 4,505, the engine's item bound (internal/engine's maxLateItemUnits)
// the note contract holds every poll runner to. Kimi carries far more whole
// (measured on 2.7.3: notes of 4,505, 20,000 and 100,000 units, two-unit
// characters among them, each reached the next request as one user message
// identical to the one steered), so the shared bound holds and is the ceiling.
const kimiPollCeiling = 4505

// pollCeiling is [kimiPollCeiling].
func (Kimi) pollCeiling() int { return kimiPollCeiling }
