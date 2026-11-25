package executor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// copilotCheckouts is what a Copilot 1.0.114 session started in a checkout
// loads from its root at launch beside .github/instructions, which the run
// names in COPILOT_CUSTOM_INSTRUCTIONS_DIRS ([copilotInstructionDirs]):
// .github/copilot-instructions.md, AGENTS.md, CLAUDE.md and GEMINI.md, each
// whole, then .claude/rules' unscoped files (measured, row c1; scope as
// Copilot's line reader reads it, row F1), with no bound of its own (Copilot
// never cuts; it refuses a launch past its static budget, which the run's
// relaunch answers); and the skills of .github/skills, .agents/skills and
// .claude/skills, .github's winning a name (row c1).
func copilotCheckouts() checkoutSpec {
	return checkoutSpec{Instructions: []checkoutSlot{{Names: []string{".github/copilot-instructions.md"}},
		{Names: []string{"AGENTS.md"}}, {Names: []string{"CLAUDE.md"}}, {Names: []string{"GEMINI.md"}},
		{Tree: &checkoutTree{Tree: ".claude/rules", Suffix: ".md", Scoped: &checkoutScope{
			Keys: []string{"applyTo", "paths"}, Whole: []string{"**", "**/*", "*"}}}}},
		Limit: checkoutUnlimited, SkillDirs: []string{".github/skills", ".agents/skills", ".claude/skills"}}
}

// copilotCheckoutHome is where a Copilot run keeps its checkouts'
// instructions and skills ([checkoutHome]): the home's copilot-instructions.md,
// the run's standing instructions first and the home's own held beside the
// run, and the home's skills, which Copilot lists at launch, each copy named
// `<repository>:<skill>` within the 64 characters past which 1.0.114 drops a
// skill without a word (measured, row k2).
func copilotCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.CopilotRepo(), helper: paths.CopilotNoteHelper(),
		launch: paths.CopilotCheckoutLaunch(), agents: paths.CopilotInstructions(),
		held: paths.CopilotHeldInstructions(), sections: paths.CopilotRepoInstructions(),
		skills: paths.CopilotSkills(), copied: paths.CopilotRepoSkills(), own: paths.CopilotRunInstructions()}
}

// copilotInstructionDirs is the shell setting COPILOT_CUSTOM_INSTRUCTIONS_DIRS
// for one launch attempt: the person's own value as the script found it
// (pi, and ps whether it was set), with the attempt's directories (the
// launch's dirs file) appended after a `,`, which keeps the person's
// directories loading beside them (measured, row Nd). Never `=<checkout>`,
// which loads stray `*.instructions.md` from anywhere below it (row N7).
func copilotInstructionDirs(state string) string {
	return fmt.Sprintf(`cd=; [ ! -f %s ] || cd=$(cat %[1]s); if [ -n "$cd" ]; then COPILOT_CUSTOM_INSTRUCTIONS_DIRS=${pi:+$pi,}$cd; `+
		`export COPILOT_CUSTOM_INSTRUCTIONS_DIRS; elif [ -n "$ps" ]; then COPILOT_CUSTOM_INSTRUCTIONS_DIRS=$pi; `+
		`export COPILOT_CUSTOM_INSTRUCTIONS_DIRS; else unset COPILOT_CUSTOM_INSTRUCTIONS_DIRS; fi`,
		shellword.Quote(state+"/dirs"))
}

// copilotLaunchCompose is the shell composing one launch attempt's
// instructions: copilot-launch keeping args' sets where the box holds the
// helper (hk), n the sets there are, then the home file and the instruction
// directories.
func copilotLaunchCompose(paths Paths, args string) string {
	h := copilotCheckoutHome(paths)
	q := shellword.Quote
	var b strings.Builder
	fmt.Fprintf(&b, `if [ -n "$hk" ]; then n=$(%s -adapter copilot-launch -config %s %s < /dev/null) || `+
		`{ echo 'crewlet: the run could not compose its repositories'"'"' instructions' >&2; exit 1; }; fi`+"\n",
		q(h.helper), q(h.launch), args)
	b.WriteString(h.compose())
	b.WriteString(copilotInstructionDirs(h.state) + "\n")
	return b.String()
}
