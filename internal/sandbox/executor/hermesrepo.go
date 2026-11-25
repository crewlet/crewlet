package executor

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesContextWindow is the context window, in tokens, the launch has
// Hermes cut a checkout's instructions for: Hermes keeps 6% of its model's
// window, four characters a token, 20,000 to 500,000 characters
// (agent/prompt_builder.py's _dynamic_context_file_max_chars, the window
// agent.context_compressor.context_length, which model_metadata's
// get_model_context_length resolves as the session starts, over the network
// for most providers). The launch runs before Hermes, so it takes Hermes's own
// figure for a model it cannot place (DEFAULT_FALLBACK_CONTEXT, 256,000:
// 61,440 characters); a firing during the run takes the run's own window, off
// its post_api_request hook ([hermesNestedHand]).
const hermesContextWindow = 256_000

// hermesNestedTimeout is a nested firing's bound in seconds: two note helper
// runs and Hermes's own render of the checkouts, in the gateway. Codex's
// ([codexNestedTimeout]), a third of Hermes's own hook bound (30 s), which
// the guard holds the box's to ([hermesGuard]).
const hermesNestedTimeout = codexNestedTimeout

// hermesCheckouts is what a Hermes run hands a checkout below its start
// directory: what Hermes Agent 0.23.3's own code makes of it, read through the
// gate (internal/notehelper's checkoutGate; the carry script's checkout_gate),
// since Hermes scans every project file for injection and loads a checkout's
// skills only where its configuration trusts the root; the sections in the
// run's own system prompt, within the one environment variable it rides
// ([hermesSystemMaxBytes]), after the run's own text.
func hermesCheckouts(paths Paths) checkoutSpec {
	return checkoutSpec{Limit: checkoutUnlimited, Gated: true,
		Budget: &checkoutBudget{Form: "hermes", Own: paths.System(), Bytes: hermesSystemMaxBytes}}
}

// hermesCheckoutHome is where a Hermes run keeps its checkouts' instructions
// and skills ([checkoutHome]): no home file, since the script adds the
// sections to the run's system prompt itself ([hermesSystemWithCheckouts]);
// the home's skills, each copy `<repository>:<skill>`; and the gate the
// launch asks Hermes for: hermes-peek names the checkouts, the carry script's
// `gate` renders them at [hermesContextWindow], asked only where there are
// any, so no launch without a checkout starts Hermes's interpreter for it.
func hermesCheckoutHome(paths Paths) checkoutHome {
	q := shellword.Quote
	state := paths.HermesCheckouts()
	peek, gate := q(state+"/peek.json"), q(state+"/gate.json")
	cmd := fmt.Sprintf(`"$rh" -adapter hermes-peek -config %[1]s < /dev/null > %[2]s && { if [ "$(cat %[2]s)" = '{"roots":[]}' ]; then echo '{}' > %[3]s; else [ "$(hcarry gate %[2]s %[3]s %[4]d)" = 'crewlet-carry ok' ]; fi; }`,
		q(paths.HermesCheckoutLaunch()), peek, gate, hermesContextWindow)
	return checkoutHome{state: state, helper: paths.HermesNoteHelper(), launch: paths.HermesCheckoutLaunch(),
		sections: paths.HermesCheckoutSections(), skills: paths.HermesSkills(),
		copied: paths.HermesCheckoutSkillsCopied(), gateCmd: cmd}
}

// hermesSystemWithCheckouts is the shell writing the system prompt the run
// launches and renews its session with ([Paths.HermesSystem]): its own text,
// then the checkouts' sections apart by a blank line, as the launch's budget
// counts them; nothing where neither is there.
func hermesSystemWithCheckouts(paths Paths) string {
	q := shellword.Quote
	own, sections, all := q(paths.System()), q(paths.HermesCheckoutSections()), q(paths.HermesSystem())
	return fmt.Sprintf(`rm -f %[3]s; if [ -s %[2]s ]; then { if [ -s %[1]s ]; then printf '%%s\n\n' "$(cat %[1]s)"; fi; cat %[2]s; } > %[3]s || exit 1; elif [ -s %[1]s ]; then cp %[1]s %[3]s || exit 1; fi
`, own, sections, all)
}

var _ homeKeeper = Hermes{}

// homeRestore removes the copies a killed Hermes run left in the home's
// skills ([checkoutHome.restore]) and drops its skip log
// ([checkoutHome.dropSkipped]), run by the clear before every run, whichever
// harness it runs.
func (Hermes) homeRestore(p Paths) string {
	h := hermesCheckoutHome(p)
	return everyStep(h.restore(), h.dropSkipped())
}

// hermesNestedHand is the plugin's settings for a nested firing: the note
// helper, the config checkout-launch wrote, the carry script it renders
// checkouts with, the window it falls back to where the run's own is not
// known yet, and its bound in seconds.
type hermesNestedHand struct {
	Helper  string `json:"helper"`
	Config  string `json:"config"`
	Carry   string `json:"carry"`
	Window  int    `json:"window"`
	Timeout int    `json:"timeout"`
}

// hermesNested is the plugin's [hermesNestedHand] for a run in paths.
func hermesNested(paths Paths) *hermesNestedHand {
	return &hermesNestedHand{Helper: paths.HermesNoteHelper(), Config: paths.HermesCheckouts() + "/nested.json",
		Carry: paths.HermesCarry(), Window: hermesContextWindow, Timeout: hermesNestedTimeout}
}
