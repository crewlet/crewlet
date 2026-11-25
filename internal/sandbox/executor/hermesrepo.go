package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/shellword"
)

// A nested firing runs within Hermes's own hook bound (its
// plugins.hook_callback_timeout, 30 s by default; past it Hermes drops the
// answer and skips the hook 60 s, measured) less hermesHandMargin, its own
// work after the helper and the callback's thread; it claims only while
// hermesClaimReserve is left, the helper's claim, copies and answer (0.12 s at
// its full copy bound, measured), so no claim outlives its answer; Hermes's
// render of the checkouts takes what is before that, at least
// hermesRenderFloor, some 12 times its slowest (0.40 s for three checkouts,
// measured). A box whose
// hook bound is under hermesNestedTimeout, their sum, has no nested hand.
const (
	hermesHandMargin    = 2 * time.Second
	hermesClaimReserve  = 3 * time.Second
	hermesRenderFloor   = 5 * time.Second
	hermesNestedTimeout = int((hermesHandMargin + hermesClaimReserve + hermesRenderFloor) / time.Second)
)

// hermesGateWait is the most the launch waits for Hermes to render the
// checkouts ([hermesGateCommand]), past which the run starts without them:
// 60 s. Measured on 0.23.3: 1.7 to 1.9 s for three checkouts (a 200 KB file
// among them), 1.6 s of it loading Hermes's modules, which a busy box slows
// some fourfold (its gateway stop: 0.8 s idle, 3.1 s busy). Online, the window
// resolver may try a few providers' metadata, each bounded at 5 s in its
// model_metadata.py: some 25 s at worst together. 60 s is twice that.
const hermesGateWait = 60 * time.Second

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
// sections to the run's system prompt itself ([hermesSystemWithCheckouts]),
// and the home's skills, each copy `<repository>:<skill>`. A launch's gate is
// [hermesLaunchHome]'s.
func hermesCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.HermesCheckouts(), helper: paths.HermesNoteHelper(),
		launch: paths.HermesCheckoutLaunch(), sections: paths.HermesCheckoutSections(), skills: paths.HermesSkills(),
		copied: paths.HermesCheckoutSkillsCopied()}
}

// hermesLaunchHome is [hermesCheckoutHome] with the gate a launch of model
// asks Hermes for ([hermesGateCommand]).
func hermesLaunchHome(paths Paths, model string) checkoutHome {
	h := hermesCheckoutHome(paths)
	h.gateCmd = hermesGateCommand(paths, model, hermesGateWait)
	// EACH CHECKOUT THE LAUNCH LEFT FOR A FIRING where none will come (one that
	// waited for room or the copy bound, or every one Hermes could not render)
	// named in the skip log with what to read.
	h.afterLaunch = fmt.Sprintf(`if [ -e "$rr/%s" ]; then "$rh" -adapter hermes-peek -lines -config %s < /dev/null | while IFS= read -r hr; do printf 'crewlet: %%s is not handed its instructions and skills, since the run hands no checkout after its launch; read %%s for its instructions\n' "$hr" "$hr" >> "$rr/skipped"; done; fi`,
		hermesNestedOff, shellword.Quote(paths.HermesCheckoutLaunch()))
	return h
}

// hermesNestedOff marks, in the run's checkout state, a run whose plugin
// hands no checkout cloned during it, which its plugin reads.
const hermesNestedOff = "nested-off"

// hermesGateCommand is the shell asking Hermes what the checkouts bring
// before checkout-launch claims them: hermes-peek names them, the carry
// script's `gate` checks what checkout handing reads of Hermes's code
// (CHECKOUT_INTERNALS) and renders each at the window Hermes resolves for
// model, within wait ([hermesGateWait] but in a case about it). Nothing here
// refuses the run: a moved part, a late or failed gate hands no checkout and
// turns the nested hand off, a short hook bound turns the hand off, each said
// once. Another plugin rewriting the same tool result first takes a clone's
// hand's place: that clone is not handed during the run (never reordered).
func hermesGateCommand(paths Paths, model string, wait time.Duration) string {
	q := shellword.Quote
	state := paths.HermesCheckouts()
	peek, gate, answer := q(state+"/peek.json"), q(state+"/gate.json"), q(state+"/gate.answer")
	errs := q(paths.HermesCarryErrors())
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`hskip() { printf 'crewlet: %%s\n' "$1" >> "$rr/skipped" && : > "$rr/%s" || exit 1; }`, hermesNestedOff)
	line(`  hnone() { hskip "no repository below the working directory is handed its instructions and skills: $1" && echo '{}' > %s || exit 1; }`,
		gate)
	line(`  if ! "$rh" -adapter hermes-peek -config %s < /dev/null > %s; then hnone 'the note helper could not name them'; else`,
		q(paths.HermesCheckoutLaunch()), peek)
	line(`  hermes --run-module cProfile -o /dev/null %s gate %s %s %s %d < /dev/null > %s 2> %s & hw=$!`,
		q(paths.HermesCarry()), peek, gate, q(model), hermesNestedTimeout, answer, errs)
	line(`  i=0; while kill -0 "$hw" 2> /dev/null && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); done`,
		int(wait/(100*time.Millisecond)))
	line(`  if kill -0 "$hw" 2> /dev/null; then kill -s KILL "$hw" 2> /dev/null; wait "$hw" 2> /dev/null; hg=late; else wait "$hw"; hg=$(tail -n 1 %s); fi`,
		answer)
	line(`  case "$hg" in`)
	line(`  'crewlet-carry ok') ;;`)
	line(`  'crewlet-carry off '*) hskip "a repository cloned during the run is not handed its instructions and skills: ${hg#crewlet-carry off }";;`)
	line(`  'crewlet-carry moved '*) hnone "Hermes Agent $n moved what the runner reads of its code (${hg#crewlet-carry moved })";;`)
	line(`  late) hnone "Hermes Agent $n did not say within %d seconds what they bring";;`, int(wait/time.Second))
	line(`  *) hnone "Hermes Agent $n could not say what they bring (its error stream is in %s)";;`, strings.Trim(errs, "'"))
	line(`  esac`)
	b.WriteString(`  fi`)
	return b.String()
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
// helper, the config checkout-launch wrote (beside which the gate may mark the
// hand off, [hermesNestedOff]), the carry script it renders checkouts with,
// and the seconds it leaves of Hermes's hook bound and reserves for a claim
// ([hermesHandMargin], [hermesClaimReserve]).
type hermesNestedHand struct {
	Helper  string  `json:"helper"`
	Config  string  `json:"config"`
	Carry   string  `json:"carry"`
	Margin  float64 `json:"margin"`
	Reserve float64 `json:"reserve"`
}

// hermesNested is the plugin's [hermesNestedHand] for a run in paths.
func hermesNested(paths Paths) *hermesNestedHand {
	return &hermesNestedHand{Helper: paths.HermesNoteHelper(), Config: paths.HermesCheckouts() + "/nested.json",
		Carry: paths.HermesCarry(), Margin: hermesHandMargin.Seconds(), Reserve: hermesClaimReserve.Seconds()}
}
