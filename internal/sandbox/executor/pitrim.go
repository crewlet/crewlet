package executor

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/shellword"
)

// trimRelease is the release Pi's trim was measured on ([trimmer]): the one
// release a session continues on ([piSupported]).
func (Pi) trimRelease() string { return piSupported }

// piTrimAwk is the cut [Pi.trimRecord] runs over the session file twice: the
// first pass reads each line's own id and the newest compaction's
// firstKeptEntryId; the second prints the header, then before the firstKept
// line only the lines 1.6.0 reads off the branch there (its model and thinking
// level changes, its system messages and a router model's state), each linked
// onto the one kept before it (the first onto null), then the firstKept line
// linked onto the last of them, and every line after it as written. A line's
// own id and parentId are read where 1.6.0 writes them (second, or third from
// the end for a custom entry), never inside its payload. It fails where the
// header is no v3 session's, there is no compaction, its firstKept line is not
// at or before it, a line up to it is of a shape 1.6.0 does not write, or
// nothing would be left out.
var piTrimAwk = `function shape(l) {
  if (l ~ /^[{]"type":"custom(_message)?",/) {
    if (!match(l, /,"id":"[0-9a-f-]+","parentId":(null|"[0-9a-f-]+"),"timestamp":"[^"]*"[}]$/)) return 0
    is = RSTART + 7
  } else if (l ~ /^[{]"type":"[a-z_]+","id":"[0-9a-f-]+","parentId":(null|"[0-9a-f-]+"),"timestamp":"/) {
    is = index(l, ",\"id\":\"") + 7
  } else return 0
  id = substr(l, is); id = substr(id, 1, index(id, "\"") - 1)
  ps = is + length(id) + 13
  pl = (substr(l, ps, 4) == "null") ? 4 : index(substr(l, ps + 1), "\"") + 1
  return 1 }
function point(l, v) { shape(l); return substr(l, 1, ps - 1) v substr(l, ps + pl) }
function keep(l) {
  if (l ~ /^[{]"type":"(model_change|thinking_level_change)",/) return 1
  if (l ~ /^[{]"type":"message","id":"[0-9a-f-]+","parentId":(null|"[0-9a-f-]+"),"timestamp":"[^"]*","message":[{]"role":"system",/) return 1
  return index(l, "{\"type\":\"custom\",\"customType\":\"pi.virtual-model-state\",") == 1 }
BEGIN { h = "{\"type\":\"session\",\"version\":3,\"id\":\""; c = "{\"type\":\"compaction\",\"id\":\""
  f = "\"firstKeptEntryId\":\""; p = "null" }
NR == FNR { if (FNR == 1) hd = (index($0, h) == 1)
  else if (!shape($0)) { if (!bad) bad = FNR }
  else { if (!(id in at)) at[id] = FNR
    if (index($0, c) == 1) { cn = FNR; fk = ""; i = index($0, f)
      if (i) { fk = substr($0, i + length(f)); fk = substr(fk, 1, index(fk, "\"") - 1) } } }
  next }
FNR == 1 { k = (fk ~ /^[0-9a-f-]+$/ && (fk in at)) ? at[fk] : 0
  ok = hd && k && k <= cn && (!bad || bad > k)
  if (!ok) exit
  print; next }
FNR < k { if (keep($0)) { print point($0, p); p = "\"" id "\"" } else cut++; next }
FNR == k { print point($0, p); next }
{ print }
END { exit !(ok && cut > 0) }`

// trimRecord cuts a session file to what 1.6.0's resume loads
// ([piTrimAwk]): pi walks the branch back from the last line by parentId,
// rebuilds the tools from every system message on it and reads the thinking
// level and model off it, so the lines before the firstKept one that it reads
// are kept and re-linked. A resume of the cut file sent the request a resume of
// the whole one did, byte for byte (measured; docs/concepts/code-sandbox.md).
func (Pi) trimRecord(src, dst string) string {
	return fmt.Sprintf(`awk %[3]s %[1]s %[1]s > %[2]s`, src, dst, shellword.Quote(piTrimAwk))
}
