package executor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// FindingsInstruction is the standing instruction requiring a durable report,
// the last part of the system text [finalSystem] composes, followed only by
// [withNoteInstruction]. The harness is not the last step (the Crewlet agent
// continues the task), and its streamed final message can be lost when the
// process group dies or never exist on a tool-only run, so Collect reads a
// report at a known path instead.
//
// It asks for a `Delivered:` line per branch pushed and pull or merge request
// opened ([deliveredRefs]), the only account the engine can read on any host:
// a pushed branch has no URL, and a scraped URL may be one the run only read.
// Pushes to the Gitlet repositories in native are left out: the engine reports
// them from its push record (sandbox.Result.Pushed), which the box cannot write.
func FindingsInstruction(findingsPath string, native []string) string {
	lines := []string{
		"\n## Before you finish — write your report",
		"You are NOT the last step. Your output is handed back to the Crewlet " +
			"agent, which continues this task (replying to the requester, acting " +
			"on what you found). Before you stop, write your final structured " +
			"report to `" + findingsPath + "`:",
		"- Outcome: succeeded / partial / blocked.",
		"- What you did and verified (tests run and their results).",
		"- What you delivered: a line of its own for each branch you pushed and " +
			"each pull or merge request you opened, written exactly as " +
			"`Delivered: <branch name or full URL>` with nothing else on it — " +
			"for example `Delivered: fix/retry-backoff` and " +
			"`Delivered: https://git.example.com/acme/api/pull/42`. Write no such " +
			"line for anything you did not push or open yourself.",
	}
	if len(native) > 0 {
		lines = append(lines, "  Your pushes to "+words.And(native)+" on Gitlet are "+
			"recorded and reported for you: write a `Delivered:` line only for what you delivered "+
			"somewhere else.")
	}
	return strings.Join(append(lines,
		"- What remains and what the Crewlet agent should do next.",
		"Write that file even if you also print a summary — it is the "+
			"authoritative report that gets read back to continue the task.",
	), "\n")
}
