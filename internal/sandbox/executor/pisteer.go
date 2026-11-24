package executor

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// PiSteerInstruction is the standing instruction about notes a Pi run is
// given; where a note lands is pi_extension.mjs's, measured on 1.6.0.
const PiSteerInstruction = steerInstructionHead +
	"Pi adds it to the conversation as a message of its own between your steps: after the results of the " +
	"tools you called, or, when it came as you finished, as a new message you answer before the run ends. " +
	steerInstructionTail

// piExtensionSource is the extension every Pi run loads (pi_extension.mjs).
// Embedded, so it matches this build's bridge and decoder.
//
//go:embed pi_extension.mjs
var piExtensionSource string

// piExtensionConfig is the placeholder pi_extension.mjs holds once, which
// [piExtension] replaces with the run's own settings: a JavaScript comment
// before `null`, so the embedded file is valid as it stands and does nothing.
const piExtensionConfig = "/*crewlet:config*/ null"

// The custom types pi_extension.mjs records on the run's session, each a
// `custom` entry pi prints on its stream ([piEvents]) and never sends to the
// model, and the type of the notes it hands over (a `custom_message`, whose
// details carry the note's inbox id, [piNotesHeld]).
const (
	piNoteType    = "crewlet-note"
	piArmedType   = "crewlet-armed"
	piCapType     = "crewlet-cap"
	piBridgeType  = "crewlet-bridge"
	piContextType = "crewlet-context"
)

// piExtensionSettings is what the extension is written with: the servers it
// registers and those that must offer a tool ([piRequired]), the seat's
// standing instructions, the notes route ("" for none), the turn cap (0 for
// none), the run's skill files by absolute path with the name each is listed
// under, and the skill route ("" for none).
type piExtensionSettings struct {
	Servers  []piServer        `json:"servers"`
	Required []string          `json:"required"`
	System   string            `json:"system,omitempty"`
	Notes    string            `json:"notes,omitempty"`
	Cap      int               `json:"cap,omitempty"`
	Skills   map[string]string `json:"skills,omitempty"`
	SkillURL string            `json:"skillURL,omitempty"`
	// Listed is whether the run hands pi skills of its own (`--skill`), which
	// pi lists only while read or bash is among its tools ([piUnlisted]).
	Listed bool `json:"listed,omitempty"`
	// MCPFiles is the mcp.json files whose entry would win over a server the
	// extension registers under its name ([piMCPFiles]); a relative one is the
	// working directory's.
	MCPFiles []string `json:"mcpFiles,omitempty"`
	// SettingsFiles is the settings files pi merges, the later winning
	// ([piSettingsFiles]), which the extension names a codemode mode from.
	SettingsFiles []string `json:"settingsFiles,omitempty"`
}

// piServer is one MCP server the extension registers: its name and the
// configuration `pi.registerMcpServer` takes ([piMCP]).
type piServer struct {
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
}

// piExtension is the extension's file as a run's box is written it: the
// embedded source with settings in its placeholder's place. JSON is a
// JavaScript expression, so the settings need no quoting of their own; `<` and
// friends are escaped by the encoder all the same.
func piExtension(settings piExtensionSettings) (string, error) {
	if n := strings.Count(piExtensionSource, piExtensionConfig); n != 1 {
		return "", fmt.Errorf("executor: the embedded Pi extension holds its settings' placeholder %d times, not once", n)
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("executor: rendering the Pi extension's settings: %w", err)
	}
	return strings.Replace(piExtensionSource, piExtensionConfig, string(raw), 1), nil
}

// inboxID is the id a Pi run's extension hands a note over under for a job
// (the details of the note's custom message): 32 hex digits of a hash of the
// job and the note. UNIQUE TO BOTH, since one session carries every relaunch's
// notes and the confirm reader keys on the id ([piNotesHeld]); STABLE, so a
// note served again is read by the same id.
func (Pi) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return hex.EncodeToString(sum[:])[:32]
}

// piPollCeiling is the most UTF-16 units one note the extension hands over may
// carry: 4,505, the engine's item bound (internal/engine's maxLateItemUnits) the
// note contract holds every poll runner to. Pi bounds no custom message (93,000
// units reached the model's request whole, measured on 1.6.0), so the shared
// bound holds and is the ceiling.
const piPollCeiling = 4505

// pollCeiling is [piPollCeiling].
func (Pi) pollCeiling() int { return piPollCeiling }
