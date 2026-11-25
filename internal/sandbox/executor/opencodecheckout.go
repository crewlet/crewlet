package executor

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// openCodeCheckoutsPlugin is opencode_checkouts.js, which hands a checkout
// cloned during an OpenCode run its instructions and skills. Written as its
// directory's `index` module ([Paths.OpenCodeCheckoutsPlugin],
// [openCodeSteerEntry]).
//
//go:embed opencode_checkouts.js
var openCodeCheckoutsPlugin []byte

// openCodeSkillDepth is how many directories below one of its skill
// directories OpenCode 2.3.2 lists a skill: one 6 deep is listed, one 7 deep
// is not (measured).
const openCodeSkillDepth = 6

// openCodeCheckouts is what OpenCode 2.3.2 reads from a repository's root
// started there (measured): its AGENTS.md, else its CLAUDE.md, whole (a
// 300,000-byte file reached the model whole), and the skills a walk of
// .opencode/skills, .opencode/skill, .claude/skills and .agents/skills reaches
// no deeper than [openCodeSkillDepth]. OpenCode follows a link out of the
// checkout; the walk does not (code-sandbox.md's OpenCode section).
func openCodeCheckouts() checkoutSpec {
	walked := func(dir string) checkoutSkillDir {
		return checkoutSkillDir{Dir: dir, Recursive: true, Depth: openCodeSkillDepth}
	}
	return checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"AGENTS.md", "CLAUDE.md"}}},
		Limit: checkoutUnlimited, FiringLimit: &openCodeFiringLimit, SkillDirs: []checkoutSkillDir{walked(".opencode/skills"), walked(".opencode/skill"),
			walked(".claude/skills"), walked(".agents/skills")}}
}

// openCodeFiringLimit bounds the files one opencode-nested firing hands, each
// counted with its frame: the most measured to reach OpenCode 2.3.2's model
// whole as a text part pushed onto a tool's result is 300,000 bytes (B2b's
// bigc row), and 192 KiB of framed files beside the listings' 64 KiB stays
// under it, as Qwen Code's ([qwenFiringLimit]). A file past what is left is
// named in the skip listing, to be read.
var openCodeFiringLimit = 192 << 10

// openCodeCheckoutHome is where an OpenCode run keeps its checkouts'
// instructions and skills ([checkoutHome]): no home file, since the run's
// configuration names the sections file in its `instructions`, which OpenCode
// reads after every other document's, and a skills directory of the run's
// own its `skills` names, each skill copied as `<repository>:<skill>`, which
// 2.3.2 lists and loads under that name (measured).
func openCodeCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.OpenCodeCheckoutState(), helper: paths.OpenCodeNoteHelper(),
		launch: paths.OpenCodeCheckoutLaunch(), sections: paths.OpenCodeCheckoutSections(),
		skills: paths.OpenCodeCheckoutSkills(), copied: paths.OpenCodeCheckoutSkillsCopied()}
}

// openCodeNestedTimeout bounds one opencode-nested firing, which the tool's
// result waits on and OpenCode bounds no further: Pi's bound
// ([piNestedTimeout]), for the same firing of the same helper.
const openCodeNestedTimeout = piNestedTimeout

// openCodeNestedMaxOutput is the most an opencode-nested firing may print:
// its files and listings stay within some 256 KiB ([openCodeFiringLimit]),
// so this is only the backstop Pi's runner sets ([piNestedMaxOutput]), past
// which the firing's answer is dropped and said.
const openCodeNestedMaxOutput = piNestedMaxOutput

// openCodeNestedHand is the checkouts plugin's options: the note helper, its
// nested config (written by checkout-launch), and the bounds above.
type openCodeNestedHand struct {
	Helper    string `json:"helper"`
	Config    string `json:"config"`
	TimeoutMs int64  `json:"timeoutMs"`
	MaxOutput int    `json:"maxOutput"`
}

// writeOpenCodeCheckouts writes what an OpenCode run hands its checkouts with:
// the note helper's build for the box (a box with none starts without them,
// [checkoutHome.gate] says so) and its launch config, and, unless off names
// what keeps it from loading ([openCodePluginStopped]), the checkouts plugin,
// returned as the configuration's entry for it; nil for none. Where off does,
// the plugin is not loaded and the run is not refused for it (it carries no
// notes, skills or bridge): its reasons go to [Paths.OpenCodeCheckoutsOffBy]
// for the run's skip log ([openCodeCheckoutsGate]).
func writeOpenCodeCheckouts(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths,
	off []string) (map[string]any, error) {
	if err := writeNoteHelper(ctx, box, paths.OpenCodeNoteHelper()); errors.Is(err, errNoHelperBuild) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	home := openCodeCheckoutHome(paths)
	if err := box.WriteFile(ctx, paths.OpenCodeCheckoutLaunch(), home.launchConfig(openCodeCheckouts(), req.ReportedRepos)); err != nil {
		return nil, fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	if len(off) > 0 {
		if err := box.WriteFile(ctx, paths.OpenCodeCheckoutsOffBy(), []byte(strings.Join(off, "; "))); err != nil {
			return nil, fmt.Errorf("executor: writing what keeps the run's checkouts plugin from loading: %w", err)
		}
		return nil, nil
	}
	if err := box.WriteFile(ctx, paths.OpenCodeCheckoutsPlugin()+"/"+openCodeSteerEntry, openCodeCheckoutsPlugin); err != nil {
		return nil, fmt.Errorf("executor: writing OpenCode's checkouts plugin: %w", err)
	}
	return map[string]any{"package": paths.OpenCodeCheckoutsPlugin(),
		"options": openCodeNestedHand{Helper: paths.OpenCodeNoteHelper(),
			Config:    paths.OpenCodeCheckoutState() + "/nested.json",
			TimeoutMs: openCodeNestedTimeout.Milliseconds(), MaxOutput: openCodeNestedMaxOutput}}, nil
}

// openCodeCheckoutsGate is the part of a run's script that hands OpenCode its
// checkouts' instructions and skills before its server starts
// ([checkoutHome.gate]) and, where the box holds the helper but a document
// keeps the checkouts plugin from loading ([Paths.OpenCodeCheckoutsOffBy]),
// says once in the run's skip log that a checkout cloned during the run is
// not handed them. The script's end prints the log ([checkoutHome.saySkipped])
// and runs [checkoutHome.restore].
func openCodeCheckoutsGate(paths Paths) string {
	q := shellword.Quote
	h := openCodeCheckoutHome(paths)
	by := q(paths.OpenCodeCheckoutsOffBy())
	return "mkdir -p " + q(paths.OpenCodeCheckoutSkills()) + " || exit 1\n" + h.gate() +
		`if [ -n "$hk" ] && [ -s ` + by + ` ]; then printf 'crewlet: checkouts cloned during the run are not ` +
		`handed their instructions and skills: %s, so OpenCode loads no checkouts plugin of this run\n' ` +
		`"$(cat ` + by + `)" >> ` + q(h.skipped()) + "; fi\n"
}

// openCodeCheckoutsEnd is the shell the script's every end runs once
// OpenCode's server has stopped: what a firing left out said, then the run
// marked ended and its copies removed, as every checkouts runner ends, each
// step whatever came of the other ([everyStep]); the clear before the next
// run removes the rest ([OpenCode.artifactDirs]).
func openCodeCheckoutsEnd(paths Paths) string {
	h := openCodeCheckoutHome(paths)
	return everyStep(h.saySkipped(), h.restore())
}
