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

// OpenCodeInboxID is the id OpenCode admits a note under for a job: `msg_` and
// 32 hex digits of a hash of the job and the note.
//
// UNIQUE TO BOTH, since message ids are unique across the box's database, which
// a relaunch shares; STABLE, since OpenCode answers a repeat admission under a
// held id with the first (measured), so a reloaded poller admits a note once.
func OpenCodeInboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return "msg_" + hex.EncodeToString(sum[:])[:32]
}

// openCodeSteerPlugin is the plugin that admits an OpenCode run's notes
// (opencode_steer.js). Embedded, so it matches this build's bridge.
//
//go:embed opencode_steer.js
var openCodeSteerPlugin []byte

// openCodeSteerEntry is the plugin's file ([Paths.OpenCodeSteerPlugin]): the
// `index` module OpenCode loads from a plugin directory with no `server` one.
const openCodeSteerEntry = "index.js"
