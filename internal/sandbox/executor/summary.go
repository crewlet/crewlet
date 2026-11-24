package executor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// keptSummary is text, the newest compaction summary a harness's main-thread
// read found, as the box read keeps it ([sandbox.ThreadSummary]): scrubbed of
// the capture's values and by shape as a capture's text is ([keptText]), and
// then held to executorkeep.SummaryMaxBytes by its end. A summary of nothing
// but space is none.
func keptSummary(ask sandbox.MainThreadAsk, text string) sandbox.ThreadSummary {
	if strings.TrimSpace(text) == "" {
		return sandbox.ThreadSummary{}
	}
	whole := keptText(newScrubber(ask.Scrub), text)
	kept, cut := executorkeep.SummaryTail(whole)
	return sandbox.ThreadSummary{Text: kept, Bytes: int64(len(whole)), Cut: cut}
}

// summaryUnread is a read that could not say whether the thread holds a
// summary, and why.
func summaryUnread(why string) sandbox.ThreadSummary { return sandbox.ThreadSummary{Unread: why} }
