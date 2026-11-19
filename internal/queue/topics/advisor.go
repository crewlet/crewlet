package topics

// THE ADVISOR LOG: what the Advisor found and what became of it (each finding's
// episodes and their history, the runs, the weekly reports, the alarm
// episodes), ordered on one stream and applied into every data node's
// replicated estate (internal/advisordomain).
//
// A LOG OF ITS OWN rather than records on another domain's: its history is
// kept on horizons of its own, and a record one build cannot decode here must
// never freeze another domain's apply.
const (
	// AdvisorLogStream is the advisor domain's ordered stream.
	AdvisorLogStream = "CREWLET_ADVISOR_LOG"

	// AdvisorLogPrefix prefixes every subject on it. A record's own path, its
	// kind and its id, is appended to this with a dot.
	AdvisorLogPrefix = "crewlet.advisor.log"

	// AdvisorLogWildcard is the subject space the stream is created with.
	AdvisorLogWildcard = AdvisorLogPrefix + ".>"
)

// AdvisorLogSubject is the subject one advisor record is published on.
func AdvisorLogSubject(kind, id string) string {
	return LogSubject(AdvisorLogPrefix, kind, id)
}

// AdvisorLogPath is [AdvisorLogSubject]'s inverse.
func AdvisorLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(AdvisorLogPrefix, subject)
}
