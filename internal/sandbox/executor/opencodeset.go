package executor

import (
	"slices"
	"strings"
)

// openCodeCountReleases are the OpenCode releases whose count of a session
// this build reads ([openCodeCountRead]), each read in its shipped code and
// measured at the boundaries its rules predict: a first request whose count
// sits one token under its point sends the conversation whole, and one at its
// point compacts first, for a limit the release defaults, a limit a document
// states, an integer compaction buffer and a tool result after the reply.
var openCodeCountReleases = []string{"2.0.32", "2.1.4", "2.2.7", "2.3.0"}

// openCodeCountRead reports whether release is an OpenCode release whose
// count of a session this build reads before its next request: one of
// [openCodeCountReleases], the box's word on its harness (`opencode --version`)
// with the blanks around it trimmed. A SET, NEVER A RANGE: any release may
// move a rule the count rests on, and nothing here can tell which one did;
// and the writer and the box are one release, since no probe has continued a
// pair's sessions. The package doc ("The OpenCode releases the count reads")
// says how a release is added.
func openCodeCountRead(release string) bool {
	return slices.Contains(openCodeCountReleases, strings.TrimSpace(release))
}
