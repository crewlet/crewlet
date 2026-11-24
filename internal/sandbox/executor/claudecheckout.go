package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeCheckouts is what Claude Code 2.1.330 reads from a repository's root
// started there and hands it as it starts (measured): its CLAUDE.md,
// .claude/CLAUDE.md and CLAUDE.local.md, each whole, however long (a
// 200,000-byte CLAUDE.md reached the first request whole; only the load on a
// later read under a directory is cut, at 40,000 characters), and the skills
// of its .claude/skills; a checkout cloned during the run is handed its skills
// alone (claude-nested), Claude Code loading its CLAUDE.md files itself.
var claudeCheckouts = checkoutSpec{Instructions: [][]string{{"CLAUDE.md"}, {".claude/CLAUDE.md"}, {"CLAUDE.local.md"}},
	Limit: checkoutUnlimited, SkillDirs: []string{".claude/skills"}, NestedSkillsOnly: true}

// claudeCheckoutHome is where a Claude Code run keeps its checkouts'
// instructions and skills ([checkoutHome]): the home's `.claude/CLAUDE.md`,
// the home's own held beside the run ([ClaudeCode.homeRestore]), and its
// `.claude/skills`, each copied as `<repository>:<skill>`, which 2.1.330 lists
// and loads under that name (measured).
func claudeCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.ClaudeRepo(), helper: paths.ClaudeNoteHelper(), launch: paths.ClaudeCheckoutLaunch(),
		agents: paths.ClaudeConfigDir() + "/CLAUDE.md", held: paths.ClaudeHomeInstructions(),
		sections: paths.ClaudeRepoInstructions(), skills: paths.ClaudeConfigDir() + "/skills",
		staging: checkoutStaging(paths.ClaudeConfigDir() + "/skills"), copied: paths.ClaudeRepoSkills()}
}

var _ homeKeeper = ClaudeCode{}

// homeRestore puts the home's own `.claude/CLAUDE.md` back, run by the clear
// before every run whichever harness it runs, so a killed run's checkouts'
// sections are in no home file once the next run starts.
func (ClaudeCode) homeRestore(p Paths) string { return claudeCheckoutHome(p).restore() }

// claudeCheckoutsFloor is the oldest Claude Code release a run hands its
// checkouts on: 2.1.330, where a home skill named `<repository>:<skill>`, the
// home CLAUDE.md's sections and a command hook's additional context were
// measured. An older release starts without them, and the run says so.
const claudeCheckoutsFloor = "2.1.330"

// claudeCheckoutsPluginName is the checkouts' plugin's name: one of the run's
// own, as the skills' plugin is ([sandbox.SkillNamespace]).
const claudeCheckoutsPluginName = sandbox.SkillNamespace + "-checkouts"

// claudeCommandHook is a command hook of a plugin's `hooks/hooks.json`.
type claudeCommandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// writeClaudeCheckouts writes what a Claude Code run hands its checkouts with:
// the note helper's build for the box (a box with none starts without them,
// [checkoutHome.gate] says so), its launch config ([claudeCheckouts]), and the
// plugin holding the PostToolUse hook that runs the helper's claude-nested
// adapter after every tool call, which hands each checkout cloned since the
// listing of its skills, since Claude Code lists none copied during a session
// (internal/notehelper's runClaudeNested). The hook is a command, the helper
// reading the box's files, which no call over the bridge could.
func writeClaudeCheckouts(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) error {
	if err := writeNoteHelper(ctx, box, paths.ClaudeNoteHelper()); err != nil && !errors.Is(err, errNoHelperBuild) {
		return err
	}
	home := claudeCheckoutHome(paths)
	if err := box.WriteFile(ctx, paths.ClaudeCheckoutLaunch(), home.launchConfig(claudeCheckouts, req.ReportedRepos)); err != nil {
		return fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	manifest, err := json.MarshalIndent(claudeSkillsManifest{Name: claudeCheckoutsPluginName,
		Description: "Hands this run the skills of a repository checked out during it."}, "", "  ")
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, paths.ClaudeCheckoutsPlugin()+"/.claude-plugin/plugin.json", manifest); err != nil {
		return fmt.Errorf("executor: writing the run's checkouts plugin: %w", err)
	}
	q := shellword.Quote
	command := q(paths.ClaudeNoteHelper()) + " -adapter claude-nested -config " + q(paths.ClaudeRepo()+"/nested.json")
	hooks, err := json.MarshalIndent(map[string]any{"hooks": map[string]any{claudeEventToolUsed: []any{
		map[string]any{"matcher": "*", "hooks": []claudeCommandHook{{Type: "command", Command: command,
			Timeout: int(ClaudeHookTimeout.Seconds())}}},
	}}}, "", "  ")
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, paths.ClaudeCheckoutsPlugin()+"/hooks/hooks.json", hooks); err != nil {
		return fmt.Errorf("executor: writing the run's checkouts hook: %w", err)
	}
	return nil
}

// claudeCheckoutsGate is the part of a run's script that, on a release at or
// after [claudeCheckoutsFloor], hands the harness its checkouts' instructions
// and skills ([checkoutHome.gate]) and, where the box holds the helper, starts
// the checkouts' plugin with hooks switched on: the every-run settings give
// way to [Paths.ClaudeHooksSettings], the skills' or the notes' settings
// switching them on already. After the skills' and notes' gates, before the
// pin's ([claudePinGate]).
func claudeCheckoutsGate(paths Paths) string {
	q := shellword.Quote
	return "if at " + floorArgs(claudeCheckoutsFloor) + "; then\n" + claudeCheckoutHome(paths).gate() +
		`if [ -n "$hk" ]; then set -- "$@" --plugin-dir ` + q(paths.ClaudeCheckoutsPlugin()) + "; case $cs in " +
		q(paths.ClaudeSettings()) + ") cs=" + q(paths.ClaudeHooksSettings()) + " ;; esac; fi\n" +
		`else echo "crewlet: this box runs Claude Code $n, older than ` + claudeCheckoutsFloor + `, the oldest ` +
		`that is handed the instructions and skills of its repositories; the run starts without them" >&2; fi` + "\n"
}
