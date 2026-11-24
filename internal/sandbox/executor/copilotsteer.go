package executor

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// CopilotSteerInstruction is the standing instruction about notes a Copilot run
// is given; where a note lands is copilot_steer.js's, measured on 1.0.114.
const CopilotSteerInstruction = steerInstructionHead +
	"Copilot adds it to the conversation as a message from the user: after the " +
	"result of the tool that was running when it came, or, when it came as you " +
	"finished, as a new message you answer before the run ends. " + steerInstructionTail

// copilotNotesExtension is the extension that hands a Copilot run its notes,
// holds it to its turn cap and guards the bridge's names (copilot_steer.js). Embedded, so it matches this
// build's bridge and decoder.
//
//go:embed copilot_steer.js
var copilotNotesExtension string

// copilotExtensionConfig is the placeholder copilot_steer.js holds once, which
// [copilotExtension] replaces with the run's own settings. A JavaScript comment
// before `null`, so the embedded file is valid as it stands and does nothing.
const copilotExtensionConfig = "/*crewlet:config*/ null"

// copilotTurnPrefix opens the clientTurnId every note is sent under
// (copilot_steer.js's turnPrefix), so the confirm reader takes no other
// message for a note ([copilotNotesHeld]).
const copilotTurnPrefix = "crewlet-note:"

// copilotCapMessage opens the line copilot_steer.js logs as the turn cap stops
// its run (its capMessage), which labels a run's abort as the cap's
// ([copilotEvents]).
const copilotCapMessage = "crewlet: this run reached its turn cap of "

// copilotUnkeptMessage is the line copilot_steer.js logs as it stops a capped
// run whose turn count it cannot keep or read back, which the run's failure
// says ([copilotEvents]).
const copilotUnkeptMessage = "crewlet: this run's turn count could not be kept, so the run was stopped"

// copilotNotesUnkeptMessage is the line copilot_steer.js logs, once, where a
// note's id cannot be kept, so no note is sent: the notes wait unconfirmed and
// are served again after the run, and the stream says why none arrived.
const copilotNotesUnkeptMessage = "crewlet: this run's notes could not be kept, so none is sent until they can be"

// copilotArmedMessage is the line copilot_steer.js logs once it runs in the
// session (its armedMessage): a run with a turn cap whose stream lacks it ran
// with the cap unenforced ([copilotEvents]).
const copilotArmedMessage = "crewlet: the run's extension is running"

// copilotCapMark is the type of the line a capped run's script prints before
// its harness starts, naming the cap ([copilotCapLine]), by which the decoder
// knows the run had one to enforce.
const copilotCapMark = "crewlet.cap"

// copilotCapLine is that line for a cap of turns.
func copilotCapLine(turns int) string {
	return `{"type":"` + copilotCapMark + `","data":{"turns":` + strconv.Itoa(turns) + `}}`
}

// CopilotClientTurnID is the clientTurnId the extension sends the note admitted
// under inbox with: Copilot echoes it on the note's `user.message`, by which
// the runner reads, from Copilot's own record, whether the note reached the
// model ([copilotNotesHeld]).
func CopilotClientTurnID(inbox string) string { return copilotTurnPrefix + inbox }

// copilotExtensionSettings is what the extension is written with: the run's
// notes route, "" for a run wired to none, and its turn cap, 0 for none; the
// file it keeps its count, cursor and sent notes in across a restart
// ([Paths.CopilotNotesState]), and the launch that state belongs to, a key
// minted for each launch: a state another launch left counts nothing; and the
// bridge's servers it guards the names of ([crewletServers]), none for none.
type copilotExtensionSettings struct {
	URL     string   `json:"url"`
	Cap     int      `json:"cap"`
	State   string   `json:"state"`
	Run     string   `json:"run"`
	Bridges []string `json:"bridges,omitempty"`
}

// copilotUnbridgedMessage opens the error copilot_steer.js logs as it stops a
// run where an agent connected a server of its own under a bridge's name (its
// unbridgedMessage), which fails the run in its words ([copilotEvents]).
const copilotUnbridgedMessage = "crewlet: " + copilotShadowed

// copilotShadowed is the sentence such a run fails with.
const copilotShadowed = "a repository's or home's MCP server may not take a name inside Crewlet's own " +
	"server's tool names, or its place"

// copilotBridgeGuardMark is the type of the line a run handed a bridge prints before
// its harness starts, by which the decoder fails the run whose extension, which
// guards the bridge's names, never ran.
const copilotBridgeGuardMark = "crewlet.guard"

// copilotExtension is the extension's file as a run's box is written it: the
// embedded source with settings in its placeholder's place. JSON is a
// JavaScript expression, so the settings need no quoting of their own; `<` and
// friends are escaped by the encoder all the same.
func copilotExtension(settings copilotExtensionSettings) (string, error) {
	// A WIRED EXTENSION KEEPS ITS STATE: without a file and a launch it would
	// send no note and stop a capped run at its first request.
	if (settings.URL != "" || settings.Cap > 0) && (settings.State == "" || settings.Run == "") {
		return "", fmt.Errorf("executor: the Copilot extension is wired to notes or a cap with no state file or "+
			"launch (state %q, launch %q)", settings.State, settings.Run)
	}
	if n := strings.Count(copilotNotesExtension, copilotExtensionConfig); n != 1 {
		return "", fmt.Errorf("executor: the embedded Copilot extension holds its settings' placeholder %d times, not once", n)
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("executor: rendering the Copilot extension's settings: %w", err)
	}
	return strings.Replace(copilotNotesExtension, copilotExtensionConfig, string(raw), 1), nil
}

// inboxID is the id a Copilot run's extension sends a note under for a job
// (its clientTurnId, [CopilotClientTurnID]): 32 hex digits of a hash of the job
// and the note. UNIQUE TO BOTH, since one session carries every relaunch's
// notes and the confirm reader keys on the id ([copilotNotesHeld]); STABLE, so
// a note served again is read by the same id.
func (Copilot) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return hex.EncodeToString(sum[:])[:32]
}

// copilotPollCeiling is the most UTF-16 units one note the extension sends may
// carry: 4,505, the engine's item bound (internal/engine's maxLateItemUnits)
// the note contract holds every poll runner to. Copilot carries far more whole
// (measured on 1.0.114: notes of 4,505, 9,000, 20,000 and 100,000 units, two-unit
// characters among them, each reached the model's next request as one message
// identical to the one sent), so the shared bound holds and is the ceiling.
const copilotPollCeiling = 4505

// pollCeiling is [copilotPollCeiling].
func (Copilot) pollCeiling() int { return copilotPollCeiling }
