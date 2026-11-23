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
// admits can carry, as measured: every size up to 1,048,576 reached the model
// byte for byte on OpenCode 2.3.2, against a stub provider recording each
// request. Whether it arrives PLAIN or folded into a
// compaction's checkpoint depends on the session's room, not the note's size:
// plain iff R + round(N/4) <= P, P the point [openCodePointAt] models and R the
// last reported usage plus OpenCode's estimates since.
const openCodePollCeiling = 1 << 20

// pluginConfirms reports that OpenCode's plugin confirms each note itself: it
// posts the note's id to the bridge's read route once OpenCode's context hook
// shows a model request carrying it plain (opencode_steer.js).
func (OpenCode) pluginConfirms() bool { return true }

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
