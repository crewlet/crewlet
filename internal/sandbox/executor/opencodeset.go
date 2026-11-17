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
// count of a session this build reads before its next request: whose count, point,
// compaction check, model-message conversion, context sources and plugin
// locations were read in its shipped code and measured at their boundaries
// ([openCodeCountReleases]). A SET, NEVER A RANGE: a release between two of
// these is none of them, since any release may move a rule the count rests on
// (2.1.4 raised the share of the window held back from a tenth to fifteen
// hundredths, and 2.2.7 took the smaller of a model's input and context
// limits), and nothing here can tell which one did. The release is the box's
// word on its harness (`opencode --version`), read with the blanks around it
// trimmed.
//
// TO ADD A RELEASE, re-probe it: read its bundle, as data, at the point
// function, the count, the estimator, the compaction check (whether `auto`
// skips the overflow route too), the catalog record's builder and a
// document's overlay of it (what states a limit, what sets `time.released`),
// the default model record, the anchor's qualification and its provider, the
// compaction statuses and the native compaction's mark, the model-message
// conversion of every message kind, the seven context sources and their
// renders, the instruction discovery, the plugin locations, and the overflow
// phrases; then continue, in a box of that release, exports placed one token
// under and at the predicted point for a limit it defaults, a limit stated,
// an integer buffer and a tool result after the reply, each of which must
// split there; a first request refused in the overflow phrases; and read its
// model.list and config.get answers as the launch reads them. Only then is it
// added here.
//
// THE WRITER AND THE BOX ARE ONE RELEASE: a session one release wrote is
// judged in a box of another only once a probe has continued that pair's
// sessions (a text step, a tool step and a completed compaction) at the box's
// point, which none has yet. So the set admits each release alone, and a pair
// of two differing releases is none.
func openCodeCountRead(release string) bool {
	return slices.Contains(openCodeCountReleases, strings.TrimSpace(release))
}
