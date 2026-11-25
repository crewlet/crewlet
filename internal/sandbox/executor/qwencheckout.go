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
// a read (measured, B2b): its QWEN.md and AGENTS.md, then the always-on rules
// of .qwen/rules, each whole (no bound measured), and the skills of
// .qwen/skills and .agents/skills (.qwen winning a name, no recursion), copied
// as `<repository>-<skill>` beside the run's own `crewlet-<skill>`. Qwen Code
// imports no file from the home's QWEN.md by an absolute or relative path, so
// the text is copied; a checkout's own relative imports then resolve against
// the home.
var qwenCheckouts = checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"QWEN.md"}},
	{Names: []string{"AGENTS.md"}}, {Tree: &checkoutTree{Tree: ".qwen/rules", Suffix: ".md", Form: checkoutFormQwen,
		Scoped: &checkoutScope{Keys: []string{"paths"}, Whole: []string{}}}}},
	Limit: checkoutUnlimited, SkillDirs: []checkoutSkillDir{{Dir: ".qwen/skills"}, {Dir: ".agents/skills"}},
	SkillSep: checkoutSkillSepHyphen}

// qwenCheckoutHome is where a Qwen Code run keeps its checkouts' instructions
// and skills ([checkoutHome]): the home's ~/.qwen/QWEN.md, the home's own held
// beside the run, and ~/.qwen/skills.
func qwenCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.QwenCheckouts(), helper: paths.QwenNoteHelper(), launch: paths.QwenCheckoutLaunch(),
		agents: []string{paths.QwenDir() + "/QWEN.md"}, held: paths.QwenHomeInstructions(),
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
func qwenCheckoutsGate(paths Paths) string {
	h := qwenCheckoutHome(paths)
	return "qrestore() { " + h.saySkipped() + "; " + QwenCode{}.homeRestore(paths) + "; }; trap qrestore EXIT\n" +
		`trap 'end "$harness"; qrestore; trap - EXIT; exit 143' TERM; trap 'end "$harness"; qrestore; trap - EXIT; exit 130' INT; ` +
		`trap 'end "$harness"; qrestore; trap - EXIT; exit 129' HUP` + "\n" + h.gate()
}
