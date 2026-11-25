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
// A firing's answer is bounded by the room the extension finds at its tool
// result ([piNestedHand]), not by the spec.
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
		skills: paths.PiCheckoutSkills(), copied: paths.PiCheckoutSkillsCopied()}
}

var _ homeKeeper = Pi{}

// homeRestore puts the agent directory's own instruction file back where a
// killed Pi run held it ([checkoutHome.restore]) and drops the skip log it
// left ([checkoutHome.dropSkipped]), run by the clear before every run,
// whichever harness it runs.
func (Pi) homeRestore(p Paths) string {
	h := piCheckoutHome(p)
	return everyStep(h.restore(), h.dropSkipped())
}

// piNestedTimeout bounds one pi-nested firing, which the tool's result waits
// on and Pi bounds no further (measured on 1.6.0: a 30 s call held the
// result 30 s). Codex's nested bound ([codexNestedTimeout]), some 80 times
// the slowest firing measured: 0.12 s at the note helper's full copy bound on
// the build VM.
const piNestedTimeout = codexNestedTimeout * time.Second

// piNestedMaxOutput is the most a pi-nested firing may print: 64 MiB, the
// most measured to reach Pi 1.6.0's next request whole through the
// extension; one byte past it, the extension hands nothing of the answer.
const piNestedMaxOutput = 64 << 20

// piNestedMaxRoom is the most room a firing is handed, in UTF-16 units: an
// answer within it prints at most three bytes a unit, so within
// [piNestedMaxOutput]. Some 5.6 million tokens of Pi's estimate, so only a
// window past that is held below its room.
const piNestedMaxRoom = piNestedMaxOutput / 3

// piNestedFallbackRoom is the most room where Pi gives the extension no
// tokens (its code gives none after a compaction until a model answer that
// reports usage): half what an empty context leaves in the 128,000-token
// window Pi gives a model whose config names none, with both reserves
// ([piNestedHand]) held; the same half of the window Pi names, where smaller.
const piNestedFallbackRoom = (128_000 - 2*16_384) * 4 / 2

// piNestedMinRoom is the least room a firing runs in: below it the checkouts
// wait for a tool result with more, as after the compaction a full context
// brings, since some 1,000 tokens names a few files left out and no more.
const piNestedMinRoom = 4 << 10

// piNestedHand is the extension's settings for a pi-nested firing: the note
// helper, its config (written by checkout-launch), its bounds, and the rooms
// above. A firing runs at a batch's last result (Pi runs a message's calls in
// parallel and adds their results together, measured), its -room what Pi's
// own estimate (ctx.getContextUsage, measured equal to the figure its
// compaction checks) leaves below the window less Pi's compaction reserve
// taken twice, once Pi's for the reply and once for what the next turn adds,
// less every result of the batch, as Pi counts a message, ceil(characters/4)
// (measured: at that room no compaction, a character more compacts).
type piNestedHand struct {
	Helper       string `json:"helper"`
	Config       string `json:"config"`
	TimeoutMs    int64  `json:"timeoutMs"`
	MaxOutput    int    `json:"maxOutput"`
	MaxRoom      int    `json:"maxRoom"`
	FallbackRoom int    `json:"fallbackRoom"`
	MinRoom      int    `json:"minRoom"`
}

// piNested is the extension's piNestedHand for a run in paths.
func piNested(paths Paths) *piNestedHand {
	return &piNestedHand{Helper: paths.PiNoteHelper(), Config: paths.PiCheckouts() + "/nested.json",
		TimeoutMs: piNestedTimeout.Milliseconds(), MaxOutput: piNestedMaxOutput, MaxRoom: piNestedMaxRoom,
		FallbackRoom: piNestedFallbackRoom, MinRoom: piNestedMinRoom}
}
