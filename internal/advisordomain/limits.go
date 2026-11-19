package advisordomain

import "time"

// THE BOUNDS ON WHAT IS STORED AND READ, held by the writer before a record is
// published so the log never refuses one as too large after the fact. A
// finding's body is the advisor's own words and numbers, a few kibibytes; the
// bounds below are what no detector's finding comes near.
const (
	// MaxKeyBytes bounds a finding key (class, `:`, subject key); MaxClassBytes
	// its class.
	MaxKeyBytes   = 512
	MaxClassBytes = 128

	// MaxWordBytes bounds an area, a subject kind, a hold's reason and a
	// person's handle; MaxSubjectIDBytes a subject's id.
	MaxWordBytes      = 128
	MaxSubjectIDBytes = 512

	// MaxBodyBytes bounds a finding's body: its title, why, evidence (up to
	// five cited records and their facts) and fix, as JSON.
	MaxBodyBytes = 48 << 10

	// MaxMovedBytes bounds the facts a revision or a move says moved.
	MaxMovedBytes = 8 << 10

	// MaxExplanationBytes bounds the model's words for one verdict: one to
	// three sentences.
	MaxExplanationBytes = 2 << 10

	// MaxClassesBytes bounds a run's per-class summary, a few hundred bytes a
	// class for each of the classes a run reads.
	MaxClassesBytes = 32 << 10

	// MaxModelBytes bounds a run's model pass summary.
	MaxModelBytes = 4 << 10

	// MaxReportBytes bounds a weekly report's body: its metrics, counts, top
	// five findings and summary.
	MaxReportBytes = 128 << 10

	// MaxPageSize is the most rows a list page answers, DefaultPageSize what a
	// page that names none takes.
	MaxPageSize     = 100
	DefaultPageSize = 50

	// MaxNonClosed bounds the duty's read of every open and applied episode:
	// a hundred times the reference company's fifty, past which the read
	// refuses rather than reconciling a slice of the list as if it were all.
	MaxNonClosed = 5000

	// MaxPending bounds the read of runs requested and not yet recorded: a
	// press during a run joins it, so a handful at most.
	MaxPending = 100

	// MaxAlarmEpisodes bounds one read of alarm episodes: thirty days of a
	// flapping alarm on every node of a large fleet.
	MaxAlarmEpisodes = 10_000

	// MaxEpisodes bounds one read of a key's kept episodes: ninety days of a
	// condition that returns every day.
	MaxEpisodes = 200
)

// THE RETENTION (package doc, "Retention"), swept by one expiry record a day
// ([Writer.Expire], [ExpiryFor]).
const (
	// ClosedRetention is how long a closed episode and its history are kept.
	ClosedRetention = 90 * 24 * time.Hour

	// RunRetention is how long a run is kept.
	RunRetention = 30 * 24 * time.Hour

	// ReportRetention is how long a weekly report is kept: 26 weeks.
	ReportRetention = 26 * 7 * 24 * time.Hour

	// AlarmRetention is how long a cleared alarm episode is kept.
	AlarmRetention = 30 * 24 * time.Hour
)
