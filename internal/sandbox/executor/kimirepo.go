package executor

import "time"

// kimiCheckouts is what Kimi Code 2.7.2 reads from a repository's root started
// there (measured): its .kimi-code/AGENTS.md, then its AGENTS.md else
// agents.md, by exact case, a file of whitespace alone passed over for the
// next name; whole, since Kimi bounds none (a 200,000-byte file reached the
// model whole); and the skills a walk of its .kimi-code/skills and
// .agents/skills reaches, .kimi-code's winning a name. A copy is left out where
// a person's skill in a user root Kimi lists, the run's skills or
// ~/.agents/skills, takes its name by Kimi's rule (the helper's "kimi"), so
// the copy never hides it. A firing's text is held to [kimiFiringLimit].
func kimiCheckouts(paths Paths) checkoutSpec {
	agents := paths.Home + "/.agents/skills"
	return checkoutSpec{
		Instructions: []checkoutSlot{{Names: []string{".kimi-code/AGENTS.md"}},
			{Names: []string{"AGENTS.md", "agents.md"}, SkipBlank: true}},
		Limit: checkoutUnlimited, FiringLimit: &kimiFiringLimit,
		SkillDirs: []checkoutSkillDir{{Dir: ".kimi-code/skills", Recursive: true}, {Dir: ".agents/skills", Recursive: true}},
		ListedAs:  "kimi", HomeSkills: []checkoutSkillDir{
			{Dir: agents, Recursive: true, checkoutWalk: checkoutWalk{Every: true, Depth: 8}},
			{Dir: agents, checkoutWalk: checkoutWalk{Commands: true}}}}
}

// kimiFiringLimit bounds the files one kimi-nested firing hands, each counted
// with its frame. The driver steers the firing's text in one request to KAP,
// which refuses a body past 1,048,576 bytes (measured on 2.7.2: 1,048,577
// refused, "Request body is too large"; 1,048,576 steered), and JSON writes a
// byte as at most six (a control character as \u00XX): six times this and the
// listings' 64 KiB is 983,040 bytes, the body's own fields beside it. A file
// past what is left is named in the firing's listing, to be read; a 169,846-
// byte steer reached the model whole (measured).
var kimiFiringLimit = 96 << 10

// kimiCheckoutHome is where a Kimi run keeps its checkouts' instructions and
// skills ([checkoutHome]): KIMI_CODE_HOME/AGENTS.md, which Kimi loads ahead of
// a project's, the home's own held beside the run, and KIMI_CODE_HOME/skills,
// the first user skill root, each skill copied as `<repository>:<skill>`.
func kimiCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.KimiCheckouts(), helper: paths.KimiNoteHelper(), launch: paths.KimiCheckoutLaunch(),
		agents: []string{paths.KimiHomeAgents()}, held: paths.KimiHomeInstructions(),
		sections: paths.KimiCheckoutSections(), skills: paths.KimiSkills(), copied: paths.KimiCheckoutSkillsCopied()}
}

var _ homeKeeper = Kimi{}

// homeRestore puts the home's own AGENTS.md back and removes the checkouts'
// copies where a killed Kimi run held them ([checkoutHome.restore]), and drops
// the skip log it left ([checkoutHome.dropSkipped]), run by the clear before
// every run, whichever harness it runs.
func (Kimi) homeRestore(p Paths) string {
	h := kimiCheckoutHome(p)
	return everyStep(h.restore(), h.dropSkipped())
}

// kimiNestedTimeout bounds one kimi-nested firing: Codex's nested bound
// ([codexNestedTimeout]), some 80 times the slowest firing measured (0.12 s
// at the note helper's full copy bound on the build VM).
const kimiNestedTimeout = codexNestedTimeout * time.Second

// kimiNestedMaxOutput is the most a kimi-nested firing may print: KAP's body
// bound, past which no steer of it could be posted ([kimiFiringLimit] keeps a
// firing well inside it).
const kimiNestedMaxOutput = 1 << 20

// kimiNestedHand is the driver's settings for a kimi-nested firing: the note
// helper, its config (written by checkout-launch), the bounds above, and the
// run's skip log, where the driver says once that a hook kept a hand from
// the model (kimi_driver.js, handsKept).
type kimiNestedHand struct {
	Helper    string `json:"helper"`
	Config    string `json:"config"`
	TimeoutMs int64  `json:"timeoutMs"`
	MaxOutput int    `json:"maxOutput"`
	Skipped   string `json:"skipped"`
}

// kimiNested is the driver's kimiNestedHand for a run in paths.
func kimiNested(paths Paths) *kimiNestedHand {
	h := kimiCheckoutHome(paths)
	return &kimiNestedHand{Helper: paths.KimiNoteHelper(), Config: h.state + "/nested.json",
		TimeoutMs: kimiNestedTimeout.Milliseconds(), MaxOutput: kimiNestedMaxOutput, Skipped: h.skipped()}
}
