package executor

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

// OpenCodeSteerInstruction is the standing instruction about notes an OpenCode
// run is given; where a note lands is opencode_steer.js's.
const OpenCodeSteerInstruction = steerInstructionHead +
	"OpenCode adds it to the conversation between your steps, as a message from " +
	"the user: after the tool results of a step, or after a step that called no " +
	"tool. " + steerInstructionTail

// inboxID is the id OpenCode admits a note under for a job: `msg_` and
// 32 hex digits of a hash of the job and the note.
//
// UNIQUE TO BOTH, since message ids are unique across the box's database, which
// a relaunch shares; STABLE, since OpenCode answers a repeat admission under a
// held id with the first (measured), so a reloaded poller admits a note once.
func (OpenCode) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return "msg_" + hex.EncodeToString(sum[:])[:32]
}

// openCodePollCeiling is the most UTF-16 units one message OpenCode's plugin
// admits can carry: AT LEAST 4,505, the engine's item bound
// (internal/engine's maxLateItemUnits) the note contract requires, pending a
// measurement. The plugin admits each note whole as a user message
// (opencode_steer.js); TestThePluginAdmitsNotesAtTheRunsNextStep admits one
// of 100,000 units, and the figure a real OpenCode release delivers to its
// model whole, with that release, replaces this one.
const openCodePollCeiling = 4505

// pollCeiling is [openCodePollCeiling].
func (OpenCode) pollCeiling() int { return openCodePollCeiling }

// openCodeSteerPlugin is the plugin that admits an OpenCode run's notes
// (opencode_steer.js). Embedded, so it matches this build's bridge.
//
//go:embed opencode_steer.js
var openCodeSteerPlugin []byte

// openCodeSteerEntry is the plugin's file ([Paths.OpenCodeSteerPlugin]): the
// `index` module OpenCode loads from a plugin directory with no `server` one.
const openCodeSteerEntry = "index.js"
