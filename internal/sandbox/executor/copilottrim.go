package executor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// trimRelease is the release Copilot's trim was measured on ([trimmer]): the
// one release a session continues on ([copilotFloor]).
func (Copilot) trimRelease() string { return copilotFloor() }

// copilotTrimmedKinds are the events a 1.0.114 resume never reads before its
// newest applied compaction: the turns its summary replaced, their tools and
// usage, and the bookkeeping of the processes and compactions before it.
// Every other event before it stays: the resume rebuilds the summary's request
// from every `user.message` and reads every `instruction.update` (measured).
var copilotTrimmedKinds = []string{"assistant.message", "assistant.turn_start", "assistant.turn_end",
	"tool.execution_start", "tool.execution_complete", "session.usage_record", "system.message", "hook.start",
	"hook.end", "session.shutdown", "session.resume", "session.usage_checkpoint", "session.compaction_start",
	"session.compaction_complete"}

// copilotTrimAwk is the cut [Copilot.trimRecord] runs over the journal twice:
// the first pass finds the `session.compaction_start` of the newest
// `session.compaction_complete` that applied (a cancelled or failed one never
// counts), the second keeps every line from it on and every line before it
// but those of [copilotTrimmedKinds]. It fails where there is no such
// compaction or nothing to leave out. Each line is matched by how 1.0.114
// starts it, so no payload naming a kind matches.
var copilotTrimAwk = `BEGIN { n = split("` + strings.Join(copilotTrimmedKinds, " ") + `", k, " ")
  for (i = 1; i <= n; i++) d["{\"type\":\"" k[i] "\""] = 1
  ps = "{\"type\":\"session.compaction_start\",\"data\":{\"compactionAttemptId\":\""
  pc = "{\"type\":\"session.compaction_complete\",\"data\":{\"success\":true,\"outcome\":\"applied\",\"compactionAttemptId\":\"" }
NR == FNR { if (index($0, ps) == 1) { id = substr($0, length(ps) + 1); sub(/".*/, "", id); s[id] = FNR }
  else if (index($0, pc) == 1) { id = substr($0, length(pc) + 1); sub(/".*/, "", id); if (id in s) b = s[id] }
  next }
b > 1 && FNR < b && (substr($0, 1, index($0, ",") - 1) in d) { cut++; next }
{ print }
END { exit !(b > 1 && cut > 0) }`

// trimRecord cuts a journal to what 1.0.114's resume loads ([copilotTrimAwk]):
// a resume of the cut journal sent the request a resume of the whole one did,
// byte for byte (measured; docs/concepts/code-sandbox.md). Only a journal that
// opens with its `session.start` is cut: a rotated head opens with its link
// and was not measured ([copilotLastRotated]).
func (Copilot) trimRecord(src, dst string) string {
	return fmt.Sprintf(`head -n 1 %[1]s | grep -q '^{"type":"session[.]start","data":{' && awk %[3]s %[1]s %[1]s > %[2]s`,
		src, dst, shellword.Quote(copilotTrimAwk))
}
