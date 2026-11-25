package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// geminiCheckouts is what Gemini CLI 0.70.0 loads from a repository's root
// started there that a session started above it does not (measured, B2b): the
// skills of its .agents/skills and .gemini/skills (.agents winning a name, no
// recursion), handed as copies `<repository>-<skill>`, the name it lists `a:b`
// under. Its context files Gemini CLI reads itself: the script names each
// checkout with --include-directories, which brings them with their imports
// and nothing else of the checkout (no settings, hooks, servers, commands,
// extensions, policies or skills, measured), and a clone's on the first read
// under it; so the spec names none.
var geminiCheckouts = checkoutSpec{SkillDirs: []checkoutSkillDir{{Dir: ".agents/skills"}, {Dir: ".gemini/skills"}},
	NestedSkillsOnly: true, SkillSep: checkoutSkillSepHyphen}

// geminiCheckoutsHook is the name of the run's AfterTool hook that hands a
// checkout cloned during it its skills, the name a `hooksConfig.disabled`
// would list ([geminiGuard]).
const geminiCheckoutsHook = "crewlet-checkouts"

// geminiCheckoutHome is where a Gemini CLI run keeps its checkouts' skills
// ([checkoutHome]): the home's ~/.gemini/skills, and the roots the script
// names. No home file: the run writes no instruction text, so the home's
// GEMINI.md, which save_memory writes, is never held.
func geminiCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.GeminiCheckouts(), helper: paths.GeminiNoteHelper(),
		launch: paths.GeminiCheckoutLaunch(), sections: paths.GeminiCheckoutSections(), skills: paths.GeminiSkills(),
		copied: paths.GeminiCheckoutSkills(), roots: paths.GeminiCheckoutRoots()}
}

// writeGeminiCheckouts writes what a Gemini CLI run hands its checkouts with:
// the note helper's build for the box and its launch config. It reports
// whether the box got the helper: one whose machine the engine carries no
// build for starts without them, its gate saying so.
func writeGeminiCheckouts(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (bool, error) {
	if err := writeNoteHelper(ctx, box, paths.GeminiNoteHelper()); errors.Is(err, errNoHelperBuild) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	launch := geminiCheckoutHome(paths).launchConfig(geminiCheckouts, req.ReportedRepos)
	if err := box.WriteFile(ctx, paths.GeminiCheckoutLaunch(), launch); err != nil {
		return false, fmt.Errorf("executor: writing what hands the run's repositories their skills: %w", err)
	}
	return true, nil
}

// geminiCheckoutsEntry is the AfterTool entry that runs the helper's
// gemini-nested adapter after each tool call ([geminiSettings]).
func geminiCheckoutsEntry(paths Paths) map[string]any {
	q := shellword.Quote
	return map[string]any{"matcher": "", "hooks": []any{map[string]any{
		"type": "command", "name": geminiCheckoutsHook, "timeout": geminiHookTimeout.Milliseconds(),
		"command": q(paths.GeminiNoteHelper()) + " -adapter gemini-nested -config " +
			q(paths.GeminiCheckouts()+"/nested.json"),
	}}}
}

// geminiCheckoutsGate is the part of a run's script that hands its checkouts'
// skills before Gemini CLI starts ([checkoutHome.gate], which sets hk where
// the helper ran), after the run's settings are in the home's.
func geminiCheckoutsGate(paths Paths) string { return geminiCheckoutHome(paths).gate() }

// geminiIncludeRoots is the shell adding `--include-directories <root>` to the
// run's arguments for each checkout the launch handed ([Paths.GeminiCheckoutRoots]).
func geminiIncludeRoots(paths Paths) string {
	return fmt.Sprintf(`if [ -s %[1]s ]; then while IFS= read -r rr; do set -- "$@" --include-directories "$rr"; done < %[1]s; fi`,
		shellword.Quote(paths.GeminiCheckoutRoots()))
}
