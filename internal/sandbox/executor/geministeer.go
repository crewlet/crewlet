package executor

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A Gemini CLI run takes its notes AS IT IS ABOUT TO FINISH, ONLY (ruling N1
// of #1818): its AfterAgent hook blocks the finish with the note as the
// block's reason, which Gemini CLI hands the model as a message from the user.
// The AfterTool hook reports skill loads alone, which carry no note.

// GeminiCLISteerInstruction is the standing instruction about notes a Gemini
// CLI run is given, saying where its harness puts one: a message from the
// user, as the run is about to finish (measured on 0.70.0).
const GeminiCLISteerInstruction = steerInstructionHead +
	"Gemini CLI adds it to the conversation as a message from the user when " +
	"you are about to finish. " + steerInstructionTail

// NoteInstruction is a Gemini CLI run's instruction about notes ([noteReader]).
func (GeminiCLI) NoteInstruction() string { return GeminiCLISteerInstruction }

// HandsSkills reports that a Gemini CLI run takes skills of its own
// ([skillHolder]).
func (GeminiCLI) HandsSkills() bool { return true }

// geminiTakeScript is gemini_take.mjs, which both of a run's hooks run: at
// AfterTool it reports a skill the model activated, at AfterAgent it takes the
// run's notes. Embedded so the box runs the script this build tests.
//
//go:embed gemini_take.mjs
var geminiTakeScript []byte

// geminiHookAnswerUnits bounds a Gemini CLI run's hook answer, in UTF-16
// units: 9,000, the least that holds the engine's shared bounds, a widest note
// (4,493) and its widest item (4,505) a blank line apart (ADR-0177, section
// 2). The AfterAgent block's reason was measured whole to 1 MB on 0.70.0, so
// it fits with room to spare.
const geminiHookAnswerUnits = 9000

// geminiNoteSeparator is what one answer's messages are joined with: a blank
// line, as Claude Code's are ([claudeNoteSeparator]).
const geminiNoteSeparator = "\n\n"

// geminiFinishBoundary is the boundary the take script names its AfterAgent
// take by: the bridge's own for a run about to finish (mcpbridge's
// SteerEventFinish), the one boundary a Gemini CLI take is answered at.
const geminiFinishBoundary = "Stop"

// geminiTakeField is the field of a hook's answer carrying its take's token
// to the take script, which records it beside the answer's text
// ([Paths.GeminiTakes]) and hands Gemini CLI the answer without it. Never in
// front of the model: the receipt is read off the session's words.
const geminiTakeField = "crewlet_take"

var _ hookDelivery = GeminiCLI{}

// notesThatFit is how many of messages, from the first, one Gemini CLI answer
// carries within [geminiHookAnswerUnits]: at least one, or an oversized note
// would wait for ever.
func (GeminiCLI) notesThatFit(messages []string) int {
	total := 0
	for i, msg := range messages {
		size := UTF16Len(msg)
		if i > 0 {
			size += UTF16Len(geminiNoteSeparator)
		}
		if i > 0 && total+size > geminiHookAnswerUnits {
			return i
		}
		total += size
	}
	return len(messages)
}

// roomBeside is how many UTF-16 units one Gemini CLI answer has left for a
// second message once it carries first.
func (GeminiCLI) roomBeside(first string) int {
	return geminiHookAnswerUnits - UTF16Len(first) - UTF16Len(geminiNoteSeparator)
}

// hookAnswer is the answer to a Gemini CLI run's AfterAgent hook handing its
// model notes, wakes and late calls: a block whose reason is their messages
// joined, which Gemini CLI hands the model as a message from the user, and
// take's token in [geminiTakeField]. Empty for none, and for any boundary but
// the finish, which no Gemini CLI hook takes at (ruling N1 of #1818).
func (GeminiCLI) hookAnswer(event, take string, notes []HookNote) string {
	if len(notes) == 0 || event != geminiFinishBoundary {
		return ""
	}
	messages := make([]string, len(notes))
	for i, n := range notes {
		messages[i] = n.Message
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// A person's "<" or "&" reaches the model as itself, not escaped.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{"decision": "block",
		"reason": strings.Join(messages, geminiNoteSeparator), geminiTakeField: take}); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// geminiTake is what the take script reads ([Paths.GeminiTakeConfig]): the
// bridge's endpoint and headers, the tool a take calls, the skill route, and
// where it records each take ([Paths.GeminiTakes]).
// A file, so no token is on a hook's command line or in any process's
// environment.
type geminiTake struct {
	Endpoint  string            `json:"endpoint,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Tool      string            `json:"tool,omitempty"`
	Skills    string            `json:"skills,omitempty"`
	Takes     string            `json:"takes,omitempty"`
	Namespace string            `json:"namespace"`
}

// geminiTakeConfig renders the run's [geminiTake]: refused where the notes
// name a server the run is not handed, which would leave every take without
// an endpoint.
func geminiTakeConfig(req sandbox.RunRequest, paths Paths) ([]byte, error) {
	take := geminiTake{Namespace: sandbox.SkillNamespace}
	if req.SkillLoads.Set() {
		take.Skills = req.SkillLoads.URL
	}
	if w := req.Steer; w.Set() {
		server, ok := req.MCPServers[w.Server]
		if !ok || server.Transport != sandbox.TransportHTTP {
			return nil, fmt.Errorf("executor: the run's notes name the %q server, which the run is not "+
				"handed over HTTP, so no take could reach it", w.Server)
		}
		take.Endpoint, take.Headers, take.Tool, take.Takes = server.URL, server.Headers, w.Tool, paths.GeminiTakes()
	}
	return json.Marshal(take)
}
