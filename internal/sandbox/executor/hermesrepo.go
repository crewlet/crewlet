package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesNestedTimeout is a nested firing's bound in seconds: two note helper
// runs and Hermes's own render of the checkouts, in the gateway. Codex's
// ([codexNestedTimeout]), a third of Hermes's own hook bound (30 s); a box
// whose bound is shorter starts without the nested hand ([hermesGateCommand]).
const hermesNestedTimeout = codexNestedTimeout

// hermesGateWait is the most the launch waits for Hermes to render the
// checkouts ([hermesGateCommand]), past which the run is refused: 60 s. The
// gate took 1.7 to 1.9 s on 0.23.3 for three checkouts, a 200 KB file among
// them, its interpreter and modules loading 1.6 s of it, and its window
// resolver 0.9 s offline for a model it could not place (measured); 60 s is
// some 30 times that, with room for the probes a resolver makes online.
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
	return h
}

// hermesNestedOff marks, in the run's checkout state, a run whose plugin
// hands no checkout cloned during it, which its plugin reads.
const hermesNestedOff = "nested-off"

// hermesGateCommand is the shell asking Hermes what the checkouts bring
// before checkout-launch claims them: hermes-peek names them, the carry
// script's `gate` checks every part of Hermes's own code it reads (its
// INTERNALS) and renders each at the window Hermes resolves for model, all
// within [hermesGateWait]. A release that moved one, a gate past its wait and
// one that fails refuse the run, saying which (wait, [hermesGateWait] but in
// a case about it); a hook bound under
// [hermesNestedTimeout] only turns the nested hand off, said once in the
// skip log, since nothing but checkout handing needs it.
func hermesGateCommand(paths Paths, model string, wait time.Duration) string {
	q := shellword.Quote
	state := paths.HermesCheckouts()
	peek, gate, answer := q(state+"/peek.json"), q(state+"/gate.json"), q(state+"/gate.answer")
	errs := q(paths.HermesCarryErrors())
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`"$rh" -adapter hermes-peek -config %s < /dev/null > %s || { echo 'crewlet: the run could not name its repositories for Hermes Agent, so it does not start' >&2; exit 1; }`,
		q(paths.HermesCheckoutLaunch()), peek)
	line(`  hermes --run-module cProfile -o /dev/null %s gate %s %s %s %d < /dev/null > %s 2> %s & hw=$!`,
		q(paths.HermesCarry()), peek, gate, q(model), hermesNestedTimeout, answer, errs)
	line(`  i=0; while kill -0 "$hw" 2> /dev/null && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); done`,
		int(wait/(100*time.Millisecond)))
	line(`  if kill -0 "$hw" 2> /dev/null; then kill -s KILL "$hw" 2> /dev/null; wait "$hw" 2> /dev/null; echo "crewlet: Hermes Agent $n did not say within %d seconds what the run's repositories bring, so the run does not start" >&2; exit 1; fi`,
		int(wait/time.Second))
	line(`  wait "$hw"; hg=$(tail -n 1 %s)`, answer)
	line(`  case "$hg" in`)
	line(`  'crewlet-carry ok') ;;`)
	line(`  'crewlet-carry off '*) printf 'crewlet: a repository cloned during the run is not handed its instructions and skills: %%s\n' "${hg#crewlet-carry off }" >> "$rr/skipped" && : > "$rr/%s" || exit 1;;`,
		hermesNestedOff)
	line(`  'crewlet-carry refused '*) printf 'crewlet: Hermes Agent %%s moved what the runner reads of its code to hand the run its repositories (%%s); the run was refused before Hermes Agent started\n' "$n" "${hg#crewlet-carry refused }" >&2; exit 1;;`)
	line(`  *) echo "crewlet: Hermes Agent $n could not say what the run's repositories bring (its error stream is in %s), so the run does not start" >&2; exit 1;;`,
		strings.Trim(errs, "'"))
	b.WriteString(`  esac`)
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
// and its bound in seconds.
type hermesNestedHand struct {
	Helper  string `json:"helper"`
	Config  string `json:"config"`
	Carry   string `json:"carry"`
	Timeout int    `json:"timeout"`
}

// hermesNested is the plugin's [hermesNestedHand] for a run in paths.
func hermesNested(paths Paths) *hermesNestedHand {
	return &hermesNestedHand{Helper: paths.HermesNoteHelper(), Config: paths.HermesCheckouts() + "/nested.json",
		Carry: paths.HermesCarry(), Timeout: hermesNestedTimeout}
}
