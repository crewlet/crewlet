package executor

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/shellword"
)

// trimRelease is the release Muse Code's trim was measured on ([trimmer]): the
// one release a session continues on ([museSupported]).
func (Muse) trimRelease() string { return museSupported() }

// museInstalledLine is how 1.4.25 starts the line of a compaction it installed,
// an anchored ERE: the envelope and the event's machine values before its kind,
// its keys sorted. Over the 113 records measured it matched every installed
// line and no other; the kind alone also matches each checkpoint, which embeds
// the installed event.
const museInstalledLine = `^[{]"schema_version":1,"id":"[^"]*","stream":[{]"kind":"session","id":"[^"]*"[}],` +
	`"sequence":[0-9]+,"recorded_at":[0-9]+,"record_type":"event","durability":"durable",` +
	`"causation_id":(null|"[^"]*"),"payload_type":"runtime[.]session","payload_schema_version":1,` +
	`"payload":[{]"event":[{]"budget_after":[{][^{}]*[}],"budget_before":[{][^{}]*[}],"candidate_id":"[^"]*",` +
	`("context_supplement_source":(null|[{][^{}]*[}]),)?"install_id":"[^"]*","kind":"context_compaction_installed",`

// museCheckpointKind is the kind of the checkpoint 1.4.25 writes on the line
// after each compaction it installs, the context a resume loads. Its event's
// kind follows free text, so no line start matches it; bare-quoted, it is JSON
// structure, never a string's text.
const museCheckpointKind = `"kind":"context_projection_checkpoint"`

// trimRecord keeps every line from the record's newest installed compaction
// on, where the next line is that compaction's checkpoint, whole: all 1.4.25's
// resume loads, since it takes its context from that checkpoint and needs no
// line before it (measured: a resume of the cut record sent the requests a
// resume of the whole one did; docs/concepts/code-sandbox.md). An install with
// no whole checkpoint after it, a record killed between the two, is no place to
// cut: a record with no checkpoint is refused, its permission history read
// from its first sequence. Nor is one on the first line, a record cut before.
func (Muse) trimRecord(src, dst string) string {
	q := shellword.Quote
	return fmt.Sprintf(`trb=$(grep -n -o -E %[3]s %[1]s | tail -n 1) && trb=${trb%%%%:*} && [ "$trb" -gt 1 ] 2> /dev/null && [ "$(wc -l < %[1]s | tr -d ' ')" -gt "$trb" ] && head -n "$((trb + 1))" %[1]s | tail -n 1 | grep -q -F %[4]s && tail -n "+$trb" %[1]s > %[2]s`,
		src, dst, q(museInstalledLine), q(museCheckpointKind))
}
