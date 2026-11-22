package executor

import (
	"crypto/sha256"
	"encoding/hex"
)

// A Qwen Code run takes its notes ON THE NOTE CONTRACT'S POLL CHANNEL (ADR-0177,
// section 2): its helper polls the notes route while the turn runs and hands a
// note to each of Qwen Code's mid-turn asks, which come after every tool batch
// of the main agent (craft/drainMidTurnQueue), and confirms it on the route
// once the session's chat file records it (qwen_helper.mjs has the
// mechanism). A note that arrives after the turn's last tool batch is never
// asked for in that run: it waits on the run's record, as an OpenCode note
// sent after the last step does.

// QwenCodeSteerInstruction is the standing instruction about notes a Qwen Code
// run is given, saying where its harness puts one: a message from the user
// after a step's tool results (measured under ACP on 0.28.3 and 0.26.1).
const QwenCodeSteerInstruction = steerInstructionHead +
	"Qwen Code adds it to the conversation as a message from the user once the " +
	"tool calls of a step have returned, after the words \"" + qwenNoteFraming + "\". " +
	steerInstructionTail

// qwenNoteFraming is what Qwen Code puts before a drained message on its line,
// in the request and in its record (measured on 0.28.3 and 0.26.1; the
// helper's `framing`). The transcript reader knows it ([headingFraming]).
const qwenNoteFraming = "[User message received during tool execution]: "

// NoteInstruction is a Qwen Code run's instruction about notes ([noteReader]).
func (QwenCode) NoteInstruction() string { return QwenCodeSteerInstruction }

// HandsSkills reports that a Qwen Code run takes skills of its own
// ([skillHolder]).
func (QwenCode) HandsSkills() bool { return true }

// inboxID is the id the notes route names a note under for a job's helper:
// `qwen_` and 32 hex digits of a hash of the job and the note, unique to both
// and stable, as the route requires ([inboxDelivery]). The helper hands a
// note by its text, never by this id: Qwen Code admits no id of a caller's.
func (QwenCode) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer.qwen\x00" + launch + "\x00" + noteID))
	return "qwen_" + hex.EncodeToString(sum[:])[:32]
}

// pluginConfirms reports that a Qwen Code run's helper confirms each note
// itself ([pluginConfirmer]): on the notes route, within a second of the
// session's chat file recording it (qwen_helper.mjs), so a note reads as read
// while the run works, as the poll channel's own confirm. The engine's read of
// the record ([QwenCode.notesHeld]) settles what the helper could not post: a
// note recorded as the run was killed, or a post the bridge turned away at the
// run's end. Neither alone covers both.
func (QwenCode) pluginConfirms() bool { return true }

// qwenPollCeiling is the most UTF-16 units one message the helper hands can
// carry: 4,505, the engine's item bound (internal/engine's maxLateItemUnits),
// the least the note contract admits and the most any item is. Measured on
// 0.28.3: a drained note of 4,683 units reached the next request whole, framed
// and nothing cut, and its record held it whole.
const qwenPollCeiling = 4505

// pollCeiling is [qwenPollCeiling].
func (QwenCode) pollCeiling() int { return qwenPollCeiling }
