package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeCheckouts is what Claude Code 2.1.310 reads from a repository's root
// started there and hands it as it starts (measured): its CLAUDE.md,
// .claude/CLAUDE.md, CLAUDE.local.md and the rules of .claude/rules it loads
// at start, those whose frontmatter `paths` is absent or matches every path,
// imported by absolute path so Claude Code reads each whole and resolves its
// own imports against the checkout (a copy's resolved against the home; by
// import one level of its depth is spent: D1..D3 where a root session reaches
// D4), and the skills of its .claude/skills; a checkout cloned during the run
// is handed its skills alone (claude-nested), Claude Code loading its
// CLAUDE.md files and rules itself.
var claudeCheckouts = checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"CLAUDE.md"}},
	{Names: []string{".claude/CLAUDE.md"}}, {Names: []string{"CLAUDE.local.md"}},
	{Tree: &checkoutTree{Tree: ".claude/rules", Suffix: ".md", Form: checkoutFormClaude,
		Scoped: &checkoutScope{Keys: []string{"paths"}, Whole: []string{"**"}}}}},
	Limit: checkoutUnlimited, SkillDirs: []checkoutSkillDir{{Dir: ".claude/skills"}}, NestedSkillsOnly: true, ImportInstructions: true}

// checkoutHome is where a Claude Code run keeps its checkouts'
// instructions and skills ([checkoutHome]): the home's `.claude/CLAUDE.md`,
// the home's own held beside the run ([ClaudeCode.homeRestore]), and its
// `.claude/skills`, each copied as `<repository>:<skill>`, which 2.1.310 lists
// and loads under that name (measured).
func (ClaudeCode) checkoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.ClaudeRepo(), helper: paths.ClaudeNoteHelper(), launch: paths.ClaudeCheckoutLaunch(),
		agents: []string{paths.ClaudeConfigDir() + "/CLAUDE.md"}, held: paths.ClaudeHomeInstructions(),
		sections: paths.ClaudeRepoInstructions(), skills: paths.ClaudeConfigDir() + "/skills",
		copied: paths.ClaudeRepoSkills()}
}

var _ homeKeeper = ClaudeCode{}

// homeRestore puts the home's own `.claude/CLAUDE.md` back, run by the clear
// before every run whichever harness it runs, so a killed run's checkouts'
// sections are in no home file once the next run starts.
func (ClaudeCode) homeRestore(p Paths) string { return ClaudeCode{}.checkoutHome(p).restore() }

// claudeCheckoutsFloor is the oldest Claude Code release a run hands its
// checkouts on: 2.1.310, the release main pins, where a home skill named
// `<repository>:<skill>` is listed and loads, a plugin's PostToolUse command
// hook runs and its additional context reaches the next request, the
// checkout's imports resolve against it, and a skill copied mid-session is
// never listed (measured; 2.1.330 likewise). An older release starts without
// them, and the run says so.
const claudeCheckoutsFloor = "2.1.310"

// claudeCheckoutsPluginName is the checkouts' plugin's name: one of the run's
// own, as the skills' plugin is ([sandbox.SkillNamespace]).
const claudeCheckoutsPluginName = sandbox.SkillNamespace + "-checkouts"

// writeClaudeCheckouts writes what a Claude Code run hands its checkouts with:
// the note helper's build for the box (a box with none starts without them,
// [checkoutHome.gate] says so), its launch config ([claudeCheckouts]), and the
// plugin holding the PostToolUse hook that runs the helper's claude-nested
// adapter after every tool call, which hands each checkout cloned since the
// listing of its skills, since Claude Code lists none copied during a session
// (internal/notehelper's runClaudeNested). The hook is a command, the helper
// reading the box's files, which no call over the bridge could. It reports
// whether the box holds the helper, so the run may start the hook.
func writeClaudeCheckouts(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (bool, error) {
	if err := writeNoteHelper(ctx, box, paths.ClaudeNoteHelper()); errors.Is(err, errNoHelperBuild) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	home := ClaudeCode{}.checkoutHome(paths)
	if err := box.WriteFile(ctx, paths.ClaudeCheckoutLaunch(), home.launchConfig(claudeCheckouts, req.ReportedRepos)); err != nil {
		return false, fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	manifest, err := json.MarshalIndent(claudeSkillsManifest{Name: claudeCheckoutsPluginName,
		Description: "Hands this run the skills of a repository checked out during it."}, "", "  ")
	if err != nil {
		return false, err
	}
	if err = box.WriteFile(ctx, paths.ClaudeCheckoutsPlugin()+"/.claude-plugin/plugin.json", manifest); err != nil {
		return false, fmt.Errorf("executor: writing the run's checkouts plugin: %w", err)
	}
	q := shellword.Quote
	command := q(paths.ClaudeNoteHelper()) + " -adapter claude-nested -config " + q(paths.ClaudeRepo()+"/nested.json")
	hooks, err := json.MarshalIndent(map[string]any{"hooks": map[string]any{claudeEventToolUsed: []any{
		map[string]any{"matcher": "*", "hooks": []claudeCommandHook{{Type: "command", Command: command,
			Timeout: int(ClaudeHookTimeout.Seconds())}}},
	}}}, "", "  ")
	if err != nil {
		return false, err
	}
	if err = box.WriteFile(ctx, paths.ClaudeCheckoutsPlugin()+"/hooks/hooks.json", hooks); err != nil {
		return false, fmt.Errorf("executor: writing the run's checkouts hook: %w", err)
	}
	return true, nil
}

// claudeCheckoutsGate is the part of a run's script that, on a release at or
// after [claudeCheckoutsFloor], hands the harness its checkouts' instructions
// and skills ([checkoutHome.gate]) and, where the box holds the helper, starts
// the checkouts' plugin. It sets no hk of its own: where it would be the
// run's only hook of the engine's (no earlier gate set hk) and the box's
// settings switch hooks off ([Paths.ClaudeHooksOffBy]), it is not started and
// the run is not refused for it (D685); the launch still hands what is there,
// and the skip log says a repository cloned later is not handed its skills.
// After the skills', notes' and guard's gates, before the pin's
// ([claudePinGate]). The script's end prints the skip log before it puts the
// home back ([checkoutHome.saySkipped]), once this gate removed a killed run's.
func claudeCheckoutsGate(paths Paths) string {
	q := shellword.Quote
	h := ClaudeCode{}.checkoutHome(paths)
	by := q(paths.ClaudeHooksOffBy())
	return "if at " + floorArgs(claudeCheckoutsFloor) + "; then\nho=$hk\n" + h.gate() +
		"chome() { " + h.saySkipped() + "; " + h.restore() + "; }\n" +
		`if [ -n "$hk" ] && [ -z "$ho" ] && [ -s ` + by + ` ]; then hk=; printf 'crewlet: checkouts cloned ` +
		`during the run are not handed their skills: %s, so Claude Code runs no hook of this run\n' ` +
		`"$(cat ` + by + `)" >> ` + q(h.skipped()) + "; fi\n" +
		`if [ -n "$hk" ]; then set -- "$@" --plugin-dir ` + q(paths.ClaudeCheckoutsPlugin()) + "; fi; hk=$ho\n" +
		`else echo "crewlet: this box runs Claude Code $n, older than ` + claudeCheckoutsFloor + `, the oldest ` +
		`that is handed the instructions and skills of its repositories; the run starts without them" >&2; fi` + "\n"
}
