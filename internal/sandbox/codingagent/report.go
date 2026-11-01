package codingagent

import (
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// FindingsInstruction is the standing instruction requiring a durable report,
// the last part of a run's system text ([finalSystem]).
//
// The coding agent is NOT the last step: its findings go back to the Crewlet
// agent, which continues the same task. The streamed final message can be lost
// when a run's process group dies before it is written, and a tool-only run
// leaves no parsed text at all — so a durable structured report at a known path is required,
// and Collect always reads it.
//
// WHAT THE RUN DELIVERED IS NAMED, one `Delivered:` line per branch pushed and
// per pull or merge request opened, because that is the only account of it
// the engine can read on any code host: which hosts a box pushes to is the
// seat's own environment's business, and a pushed branch has no URL at all.
// Those lines are the run's delivered refs ([deliveredRefs]); a report that
// writes none is scraped for pull-request URLs instead, which cannot tell one
// the run opened from one it only read.
//
// EXCEPT ON THE ENGINE'S OWN CODE HOST, whose pushes the engine reads from
// its push record and reports for the run (sandbox.Result.Pushed): native is
// the repositories there the run may push to, by name, and where it names any
// the run is told so and asked for lines only for what it delivered somewhere
// else — a line for a push the record names is the same delivery said twice,
// and the record's is the account nothing in the box can write.
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
		lines = append(lines, "  Your pushes to "+words.And(native)+" on this company's code host are "+
			"recorded and reported for you: write a `Delivered:` line only for what you delivered "+
			"somewhere else.")
	}
	return strings.Join(append(lines,
		"- What remains and what the Crewlet agent should do next.",
		"Write that file even if you also print a summary — it is the "+
			"authoritative report that gets read back to continue the task.",
	), "\n")
}
