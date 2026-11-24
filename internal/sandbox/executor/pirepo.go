package executor

import "time"

// piInstructionNames is the instruction file Pi 1.6.0 loads from a directory
// it starts in, and from its agent directory: the first of these that exists,
// by exact name (measured).
var piInstructionNames = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// piCheckouts is what Pi 1.6.0 reads from a repository's root started there
// (measured): one instruction file ([piInstructionNames]), whole, since Pi
// bounds none (a 200,000-byte file reached the model whole), and the skills a
// walk of its .pi/skills and .agents/skills reaches, the first winning a name.
func piCheckouts() checkoutSpec {
	return checkoutSpec{Instructions: []checkoutSlot{{Names: piInstructionNames}}, Limit: checkoutUnlimited,
		SkillDirs: []checkoutSkillDir{{Dir: ".pi/skills", Recursive: true}, {Dir: ".agents/skills", Recursive: true}}}
}

// piCheckoutHome is where a Pi run keeps its checkouts' instructions and
// skills ([checkoutHome]): the agent directory's instruction file, the one of
// [piInstructionNames] Pi loads there, the home's own held beside the run;
// and a skills directory of the run's own, named with one more `--skill`
// ([piScript]), each skill copied as `<repository>:<skill>`.
func piCheckoutHome(paths Paths) checkoutHome {
	agents := make([]string, len(piInstructionNames))
	for i, name := range piInstructionNames {
		agents[i] = paths.PiAgentDir() + "/" + name
	}
	return checkoutHome{state: paths.PiCheckouts(), helper: paths.PiNoteHelper(), launch: paths.PiCheckoutLaunch(),
		agents: agents, held: paths.PiHomeInstructions(), sections: paths.PiCheckoutSections(),
		skills: paths.PiCheckoutSkills(), staging: checkoutStaging(paths.PiCheckoutSkills()),
		copied: paths.PiCheckoutSkillsCopied()}
}

var _ homeKeeper = Pi{}

// homeRestore puts the agent directory's own instruction file back where a
// killed Pi run held it ([checkoutHome.restore]), run by the clear before
// every run, whichever harness it runs.
func (Pi) homeRestore(p Paths) string { return piCheckoutHome(p).restore() }

// piNestedTimeout bounds one pi-nested firing, which Pi waits on with its
// whole process and bounds no further (measured on 1.6.0: a 30 s call held
// the tool's result 30 s). Codex's nested bound ([codexNestedTimeout]), some
// 80 times the slowest firing measured: 0.12 s at the note helper's full copy
// bound on the build VM.
const piNestedTimeout = codexNestedTimeout * time.Second

// piNestedMaxOutput is the most a pi-nested firing may print. Pi bounds no
// instruction file, so Node's 1 MiB default would lose a large one a session
// in the checkout reads whole; 64 MiB is past what any model's context holds
// (a million tokens is some 4 MB of text), so nothing past it reaches a model.
const piNestedMaxOutput = 64 << 20

// piNestedHand is the extension's settings for a pi-nested firing: the note
// helper, its config (written by checkout-launch), and the bounds above.
type piNestedHand struct {
	Helper    string `json:"helper"`
	Config    string `json:"config"`
	TimeoutMs int64  `json:"timeoutMs"`
	MaxOutput int    `json:"maxOutput"`
}

// piNested is the extension's piNestedHand for a run in paths.
func piNested(paths Paths) *piNestedHand {
	return &piNestedHand{Helper: paths.PiNoteHelper(), Config: paths.PiCheckouts() + "/nested.json",
		TimeoutMs: piNestedTimeout.Milliseconds(), MaxOutput: piNestedMaxOutput}
}
