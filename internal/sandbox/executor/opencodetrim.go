package executor

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/shellword"
)

var _ trimmer = OpenCode{}

// trimRelease is the release OpenCode's trim was measured on ([trimmer]): the
// one this runner supports ([openCodeRelease]).
func (OpenCode) trimRelease() string { return openCodeRelease }

// openCodeExportHead is how a 2.3.2 export opens (`JSON.stringify` of `info`
// and `messages`, two spaces an indent), its line break written `~`: only an
// export that opens so is cut.
const openCodeExportHead = `{~  "info": {`

// openCodeTrimAwk is the cut [OpenCode.trimRecord] runs over an export twice,
// each line matched as 2.3.2 indents it, so no payload (a string, its breaks
// escaped, or an object nested deeper) matches. The first pass keeps each
// message's first line and id, and finds the newest compaction that completed
// and is no provider's own (`native`, `providerContext`: a resume on another
// model skips one) and its `metadata.keepID`. The second keeps every line
// through `"messages": [` (`info` whole) and every message from the keep
// point on, or from the run's start where that is older: the message the
// cursor in [trimStartVar] names, decoded as [runStartOf] decodes it, none
// where it is 0. It fails where the start is not known or not in the export,
// no such compaction completed, its keepID names no older message, or nothing
// is left out. A line is never split into fields: a text runs to mebibytes.
const openCodeTrimAwk = `function unb64(s,   a, i, k, v, w, out) {
  a = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
  for (i = 1; i <= length(s); i++) {
    if (!(k = index(a, substr(s, i, 1)))) return ""
    v = v * 64 + k - 1; w += 6
    if (w >= 8) { w -= 8; k = int(v / 2 ^ w); v -= k * 2 ^ w; out = out sprintf("%c", k) }
  }
  return out
}
BEGIN { FS = "\n"; t = ENVIRON["` + trimStartVar + `"]
  if (t != "0") { c = unb64(t)
    if (match(c, /"id"[ \t\r\n]*:[ \t\r\n]*"[A-Za-z0-9_-]+"/)) {
      A = substr(c, RSTART, RLENGTH); sub(/^"id"[ \t\r\n]*:[ \t\r\n]*"/, "", A); sub(/"$/, "", A) }
    if (A == "") exit 1 } }
NR == FNR { if (!L) { if ($0 == "  \"messages\": [") L = FNR; next }
  if ($0 == "    {") { b = FNR; id = kp = ""; cp = st = nat = meta = 0; next }
  if ($0 == "    }," || $0 == "    }") { n++; S[n] = b; I[n] = id; if (cp && st && !nat) { C = n; K = kp }; next }
  if (meta) { if ($0 ~ /^      [}],?$/) meta = 0
    else if ($0 ~ /^        "keepID": "/) { kp = $0; sub(/^        "keepID": "/, "", kp); sub(/",?$/, "", kp) }
    next }
  if ($0 ~ /^      "id": "/) { id = $0; sub(/^      "id": "/, "", id); sub(/",?$/, "", id) }
  else if ($0 ~ /^      "type": "compaction",?$/) cp = 1
  else if ($0 ~ /^      "status": "completed",?$/) st = 1
  else if ($0 == "      \"metadata\": {") meta = 1
  else if ($0 ~ /^      "(native|providerContext)": /) nat = 1
  next }
FNR == 1 { if (C) { f = S[C]; if (K != "") { f = 0; for (i = 1; i < C; i++) if (I[i] == K) { f = S[i]; break } } }
  if (f && A != "") { for (i = 1; i <= n; i++) if (I[i] == A) break
    if (i > n) exit 1
    if (S[i] < f) f = S[i] }
  if (f <= S[1]) exit 1 }
FNR <= L || FNR >= f`

// trimRecord cuts an export to what 2.3.2's resume loads, and the run's own
// messages ([openCodeTrimAwk]): `info`, which the import restores the
// session's totals from, and every message from the newest compaction's keep
// point on, which its resume renders as the summary and the turns it kept. A
// resume of the export cut so sent the request a resume of the whole one did,
// byte for byte, on the session's model and another (measured;
// docs/concepts/code-sandbox.md). Only an export that opens as 2.3.2 writes one
// is read ([openCodeExportHead]), under the C locale.
func (OpenCode) trimRecord(src, dst string) string {
	q := shellword.Quote
	return fmt.Sprintf(`[ "$(head -c %[3]d %[1]s | tr '\n' '~')" = %[4]s ] && %[5]s="$%[5]s" LC_ALL=C awk %[6]s %[1]s %[1]s > %[2]s`,
		src, dst, len(openCodeExportHead), q(openCodeExportHead), trimStartVar, q(openCodeTrimAwk))
}
