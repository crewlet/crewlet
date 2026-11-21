package executor

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// CopilotSteerInstruction is the standing instruction about notes a Copilot run
// is given; where a note lands is copilot_steer.js's, measured on 1.0.114.
const CopilotSteerInstruction = steerInstructionHead +
	"Copilot adds it to the conversation as a message from the user: after the " +
	"result of the tool that was running when it came, or, when it came as you " +
	"finished, as a new message you answer before the run ends. " + steerInstructionTail

// copilotNotesExtension is the extension that hands a Copilot run its notes and
// holds it to its turn cap (copilot_steer.js). Embedded, so it matches this
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

// CopilotClientTurnID is the clientTurnId the extension sends the note admitted
// under inbox with: Copilot echoes it on the note's `user.message`, by which
// the runner reads, from Copilot's own record, whether the note reached the
// model ([copilotNotesHeld]).
func CopilotClientTurnID(inbox string) string { return copilotTurnPrefix + inbox }

// copilotExtensionSettings is what the extension is written with: the run's
// notes route, "" for a run wired to none, and its turn cap, 0 for none.
type copilotExtensionSettings struct {
	URL string `json:"url"`
	Cap int    `json:"cap"`
}

// copilotExtension is the extension's file as a run's box is written it: the
// embedded source with settings in its placeholder's place. JSON is a
// JavaScript expression, so the settings need no quoting of their own; `<` and
// friends are escaped by the encoder all the same.
func copilotExtension(settings copilotExtensionSettings) (string, error) {
	if n := strings.Count(copilotNotesExtension, copilotExtensionConfig); n != 1 {
		return "", fmt.Errorf("executor: the embedded Copilot extension holds its settings' placeholder %d times, not once", n)
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("executor: rendering the Copilot extension's settings: %w", err)
	}
	return strings.Replace(copilotNotesExtension, copilotExtensionConfig, string(raw), 1), nil
}
