package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// qwenCheckouts is what Qwen Code 0.28.4 loads from a repository's root
// started there and never from a checkout below where it starts, not even on
// a read (measured, B2b): its context files, QWEN.md and AGENTS.md unless
// context.fileName names others (the script's helper sets them,
// [qwenCheckoutsGate]), then the always-on rules of .qwen/rules, each as it
// loads one (its body, no bound measured; the helper's qwen form), and the
// skills of .qwen/skills and .agents/skills (.qwen winning a name, no recursion), copied
// as `<repository>-<skill>` beside the run's own `crewlet-<skill>`. Qwen Code
// imports no file from the home's QWEN.md by an absolute or relative path, so
// the text is copied; a checkout's own relative imports then resolve against
// the home.
var qwenCheckouts = checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"QWEN.md"}},
	{Names: []string{"AGENTS.md"}}, {Tree: &checkoutTree{Tree: ".qwen/rules", Suffix: ".md", Form: checkoutFormQwen,
		Scoped: &checkoutScope{Keys: []string{"paths"}}}}},
	Limit: checkoutUnlimited, FiringLimit: &qwenFiringLimit,
	SkillDirs: []checkoutSkillDir{{Dir: ".qwen/skills"}, {Dir: ".agents/skills"}}, SkillSep: checkoutSkillSepHyphen}

// qwenFiringLimit bounds the files one qwen-nested firing hands, each counted
// with its frame: the most measured to reach Qwen Code 0.28.4's next request
// whole through a hook's additionalContext is 300,000 bytes (B2b's hookbig
// row), and 192 KiB of framed files beside the listings' 64 KiB stays under
// it. A file past what is left is named in the skip listing, to be read.
var qwenFiringLimit = 192 << 10

// qwenCheckoutHome is where a Qwen Code run keeps its checkouts' instructions
// and skills ([checkoutHome]): the first of the home's own context files that
// is there, else the first ([Paths.QwenContextHomes]), the home's own held
// beside the run, and ~/.qwen/skills.
func qwenCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.QwenCheckouts(), helper: paths.QwenNoteHelper(), launch: paths.QwenCheckoutLaunch(),
		agentsFrom: paths.QwenContextHomes(), held: paths.QwenHomeInstructions(),
		sections: paths.QwenCheckoutSections(), skills: paths.QwenDir() + "/skills", copied: paths.QwenCheckoutSkills()}
}

var _ homeKeeper = QwenCode{}

// homeRestore puts the home's own QWEN.md back and removes the checkouts'
// copies ([checkoutHome.restore]), dropping a skip log no script printed, run
// by the clear before every run whichever harness it runs.
func (QwenCode) homeRestore(p Paths) string {
	h := qwenCheckoutHome(p)
	return everyStep(h.restore(), h.dropSkipped())
}

// writeQwenCheckouts writes what a Qwen Code run hands its checkouts with: the
// note helper's build for the box and its launch config. It reports whether
// the box got the helper: one whose machine the engine carries no build for
// starts without them, its gate saying so.
func writeQwenCheckouts(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (bool, error) {
	if err := writeNoteHelper(ctx, box, paths.QwenNoteHelper()); errors.Is(err, errNoHelperBuild) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	launch := qwenCheckoutHome(paths).launchConfig(qwenCheckouts, req.ReportedRepos)
	if err := box.WriteFile(ctx, paths.QwenCheckoutLaunch(), launch); err != nil {
		return false, fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	return true, nil
}

// qwenCheckoutsHook is how the guard names the run's checkouts' hook.
const qwenCheckoutsHook = "PostToolUse(checkouts)"

// qwenCheckoutsEntry is the PostToolUse entry that runs the helper's
// qwen-nested adapter after each tool call ([qwenSettings]).
func qwenCheckoutsEntry(paths Paths) map[string]any {
	q := shellword.Quote
	return map[string]any{"matcher": "", "hooks": []any{map[string]any{
		"type": "command", "timeout": int(qwenHookTimeout / time.Second),
		"command": q(paths.QwenNoteHelper()) + " -adapter qwen-nested -config " + q(paths.QwenCheckouts()+"/nested.json"),
	}}}
}

// qwenCheckoutsGate is the part of a run's script that hands its checkouts'
// instructions and skills before Qwen Code starts ([checkoutHome.gate], which
// sets hk where the helper ran), the home's own put back however the script
// ends, a teardown's signal included, after what the hook left out is said.
// The run's helper first names the context files context.fileName settles
// on, in the launch config and the home's list (qwen_helper.mjs `checkouts`),
// its line where they are none going into the skip log
// ([qwenBlankNamesSaid]).
func qwenCheckoutsGate(paths Paths) string {
	h := qwenCheckoutHome(paths)
	q := shellword.Quote
	return "qrestore() { " + h.saySkipped() + "; " + QwenCode{}.homeRestore(paths) + "; }; trap qrestore EXIT\n" +
		`trap 'end "$harness"; qrestore; trap - EXIT; exit 143' TERM; trap 'end "$harness"; qrestore; trap - EXIT; exit 130' INT; ` +
		`trap 'end "$harness"; qrestore; trap - EXIT; exit 129' HUP` + "\n" +
		"qn=$(node " + q(paths.QwenHelper()) + " checkouts " + q(paths.QwenContextHomes()) + " " +
		q(paths.QwenCheckoutLaunch()) + " < /dev/null) || exit 1\n" + h.gate() + qwenBlankNamesSaid(h)
}

// qwenBlankNamesSaid is the shell putting the helper's line on blank-only
// context names ($qn) into h's skip log, once the gate has made the state
// afresh, and only where the helper ran ($hk): without it no hook hands
// rules mid-run, and the gate has said nothing is handed.
func qwenBlankNamesSaid(h checkoutHome) string {
	return `[ -z "$qn" ] || [ -z "$hk" ] || printf '%s\n' "$qn" >> ` + shellword.Quote(h.skipped()) + " || exit 1\n"
}
