package executor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// trimRelease is the release Grok Build's trim was measured on ([trimmer]):
// the one release a session continues on ([grokSupported]).
func (Grok) trimRelease() string { return grokSupported() }

// grokTrimmedDirs are the directories of a session's own, the main one's and
// each subagent's, that a pack past the bound leaves out: each compaction's
// request (`compaction_requests/<id>.json`, the record before it and every
// tool) and checkpoint (`compaction_checkpoints/<id>.json`), which 1.0.92 reads
// on neither a resume nor a later compaction (measured: a session restored
// without them sent every request the whole one did, byte for byte). Every
// other file stays: updates.jsonl, which both read, and compaction/'s
// segments, which a summary points the model at.
var grokTrimmedDirs = []string{"compaction_requests", "compaction_checkpoints"}

// grokTrimKeep is the shell that leaves the positional parameters, a pack's
// members (`<key>/<session>/<path>`), all but those under a session's
// [grokTrimmedDirs], in their order: a member's path is matched past its key
// and session, neither of which holds a slash ([grokPathKey]).
func grokTrimKeep() string {
	drop := make([]string, len(grokTrimmedDirs))
	for i, dir := range grokTrimmedDirs {
		drop[i] = dir + "/*"
	}
	return `for gk in "$@"; do shift; gp=${gk#*/}; gp=${gp#*/}; case $gp in ` + strings.Join(drop, "|") +
		`) ;; *) set -- "$@" "$gk";; esac; done`
}

// grokTrimBytes is the shell that sets name to the bytes of the positional
// parameters' files under the shell word sessions, once the shell pre has run:
// the leading decimal of wc's last line, its total or its one file's count.
func grokTrimBytes(name, pre, sessions string) string {
	return name + `=$(` + pre + `cd ` + sessions + ` && wc -c "$@" < /dev/null 2> /dev/null | tail -n 1) && ` +
		name + `=${` + name + `#"${` + name + `%%[0-9]*}"} && ` + name + `=${` + name + `%%[!0-9]*}`
}

// grokTrimTar is the tar of a Grok Build pack, its positional parameters the
// members under the shell word sessions, made at the shell word capture: tar,
// the pack's own, over them all, unless the release in [trimRelVar] is Grok's,
// a member is in a session's [grokTrimmedDirs] and the members are past
// at.plain bytes. Then, and where tar's archive is past at.packed, tar runs
// over the rest alone ([grokTrimKeep]), held to its own rule and failing as it
// would, and trw says the members' bytes before and kept ([trimMarked]). ONLY
// PAST A POINT, so every other pack is the one it was. True where an archive
// was made.
func grokTrimTar(at trimPoints, sessions, capture, tar string) string {
	keep := grokTrimKeep()
	cut := `{ ` + grokTrimBytes("trz", "", sessions) + ` && ` + grokTrimBytes("trk", keep+"; ", sessions) +
		` && [ "$trk" -gt 0 ] && [ "$trk" -lt "$trz" ]; } 2> /dev/null`
	ours := fmt.Sprintf(`[ "$%s" = %s ]`, trimRelVar, shellword.Quote(Grok{}.trimRelease()))
	return fmt.Sprintf(`{ trw=; if %[1]s && %[2]s && [ "$trz" -gt %[3]d ]; then %[4]s; trw="$trz $trk"; fi; %[5]s; } && { [ -n "$trw" ] || ! %[1]s || [ "$(wc -c < %[6]s | tr -d ' ')" -le %[7]d ] 2> /dev/null || ! %[2]s || { %[4]s; trw="$trz $trk"; %[5]s; }; }`,
		ours, cut, at.plain, keep, tar, capture, at.packed)
}
