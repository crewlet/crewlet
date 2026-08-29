package codingagent

import (
	"strings"
)

// The in-box ask shim.
//
// A headless coding agent cannot pause to ask a person — `claude -p` runs to
// completion. So when it is blocked on something only a human can answer it
// runs one shim command, crewlet-ask, which records the question and audience
// to a file the runner reads on collect.
//
// The shim is SIGNAL-ONLY: it never posts anything itself. The engine routes
// the question on its audited per-role surface, so identity attribution,
// capability guards and delegation telemetry all stay on the engine rather
// than being delegated to a script inside a box.

// AskShim is the crewlet-ask script, pointed at this box's output path.
//
// Usage inside the box: crewlet-ask "the question" --to team
//
// Pure POSIX shell, deliberately: it must run in whatever image an operator
// built, and a shell that can start the coding CLI can certainly run this.
// The JSON is assembled by hand because the alternative is depending on
// python3 or jq being present, which is a claim about somebody else's image.
func AskShim(outputPath string) string {
	dir := outputPath
	if i := strings.LastIndex(outputPath, "/"); i > 0 {
		dir = outputPath[:i]
	}
	return `#!/bin/sh
# crewlet-ask — record a question for a person and stop.
set -eu
question=""
to="requester"
while [ $# -gt 0 ]; do
  case "$1" in
    --to) to="${2:-requester}"; shift 2 ;;
    --to=*) to="${1#--to=}"; shift ;;
    *) if [ -z "$question" ]; then question="$1"; fi; shift ;;
  esac
done
if [ -z "$question" ]; then
  echo "usage: crewlet-ask \"<question>\" [--to requester|team|manager|<name>]" >&2
  exit 2
fi
# Escape for JSON: backslashes first, then quotes, then newlines.
esc() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | awk 'BEGIN{ORS=""} {print sep $0; sep="\\n"}'
}
mkdir -p ` + shellQuote(dir) + `
printf '{"question":"%s","to":"%s"}' "$(esc "$question")" "$(esc "$to")" > ` + shellQuote(outputPath) + `
echo "Question recorded; stop now — a person will answer and your work will resume with their reply."
`
}

// AskInstruction is the brief addendum telling the agent how and when to ask.
//
// The roster and manager give it real names to target, so a --to decision can
// name a person rather than guess one.
func AskInstruction(roster []string, manager string) string {
	lines := []string{
		"\n## If you get blocked on a human decision",
		"You are running autonomously and CANNOT pause to ask interactively. " +
			"If you hit something only a person can answer — an ambiguous spec, a " +
			"missing detail, a design or framework decision above your remit — do " +
			"NOT guess:",
		"1. Commit and push your work-in-progress branch.",
		`2. Run: crewlet-ask "<a specific, self-contained question>" --to <audience>`,
		"   where <audience> is `requester` (the person who asked, for a spec " +
			"clarification), `team` (a design or technical decision), `manager`, or " +
			"a teammate's name.",
		"3. Stop. Your work resumes automatically once they reply.",
	}
	if len(roster) > 0 {
		lines = append(lines, "Teammates you can name: "+strings.Join(roster, ", ")+".")
	}
	if manager != "" {
		lines = append(lines, "Your manager: "+manager+".")
	}
	return strings.Join(lines, "\n")
}

// FindingsInstruction is the brief addendum requiring a durable report.
//
// The coding agent is NOT the last step: its findings go back to the Crewlet
// agent, which continues the same task. The streamed final message can be lost
// when an agent finishes but never exits, and a tool-only run leaves no parsed
// text at all — so a durable structured report at a known path is required,
// and Collect always reads it.
func FindingsInstruction(findingsPath string) string {
	return strings.Join([]string{
		"\n## Before you finish — write your report",
		"You are NOT the last step. Your output is handed back to the Crewlet " +
			"agent, which continues this task (replying to the requester, acting " +
			"on what you found). Before you stop, write your final structured " +
			"report to `" + findingsPath + "`:",
		"- Outcome: succeeded / partial / blocked.",
		"- What you did and verified (tests run and their results).",
		"- The pull request or branch you opened, if any (full URL).",
		"- What remains and what the Crewlet agent should do next.",
		"Write that file even if you also print a summary — it is the " +
			"authoritative report that gets read back to continue the task.",
	}, "\n")
}

// shellQuote wraps a value in single quotes for a POSIX shell, escaping any
// single quote it contains.
//
// Its own function rather than a fmt verb because every path this package
// interpolates into a shell command goes through it: a box home is derived
// from a provider's id and an operator's state_dir, neither of which this
// package gets to assume anything about.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
