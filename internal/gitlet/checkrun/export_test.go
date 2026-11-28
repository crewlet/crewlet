package checkrun

import (
	"context"
	"io"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Reported is what a report read into an ending's failures holds: the
// failures kept, how many were omitted past the bounds, and the counts a
// report stated.
type Reported struct {
	Kept                            []gitlet.CheckFailure
	Omitted                         int
	Tests, Failed, Skipped, Results int
	// Counts are the tests an ending carries of the report.
	Counts *gitlet.CheckTests
}

// reported is fs as a case reads it.
func reported(fs *failures) Reported {
	return Reported{Kept: fs.kept, Omitted: fs.omitted, Tests: fs.tests, Failed: fs.failed, Skipped: fs.skipped,
		Results: fs.results, Counts: fs.counts()}
}

// ReadJUnit reads a JUnit report as a run's ending does.
func ReadJUnit(r io.Reader) (Reported, error) {
	fs := newFailures()
	err := readJUnit(r, fs)
	return reported(fs), err
}

// ReadSARIF reads a SARIF log of the checkout repo as a run's ending does.
func ReadSARIF(r io.Reader, repo string) (Reported, error) {
	fs := newFailures()
	err := readSARIF(r, repo, fs)
	return reported(fs), err
}

// The files of a run in its box, as a case writes them.
const (
	OutputFile = outputFile
	JUnitFile  = junitFile
	SARIFFile  = sarifFile
)

// attemptOf is an attempt of def in the repository repo on a runner keeping
// its logs through logs, as far as an ending's reads need one.
func attemptOf(def gitlet.CheckDef, repo string, logs Uploader) *attempt {
	return &attempt{r: &Runner{o: Options{Logs: logs}, log: logging.Get("gitlet.checkrun")},
		c: gitlet.RunnableCheck{Repo: repo, Def: &def}}
}

// Reports reads box's copies of the reports def names as a run's ending does,
// and what of them it could not read.
func Reports(ctx context.Context, box sandbox.Sandbox, def gitlet.CheckDef, repo string) (Reported, []string) {
	fs, unread := attemptOf(def, repo, nil).reports(ctx, box)
	if fs == nil {
		return Reported{}, unread
	}
	return reported(fs), unread
}

// KeepLog keeps box's output as a failed run's log through logs, as a run's
// ending does.
func KeepLog(ctx context.Context, box sandbox.Sandbox, logs Uploader) (*gitlet.CheckLog, string) {
	return attemptOf(gitlet.CheckDef{}, "web", logs).keepLog(ctx, box)
}

// KeepLogWithin keeps box's output as a failed run's log of at most limit
// bytes, as a run's ending keeps one of gitlet.MaxCheckLogBytes.
func KeepLogWithin(ctx context.Context, box sandbox.Sandbox, logs Uploader, limit int64) (*gitlet.CheckLog,
	string) {
	return attemptOf(gitlet.CheckDef{}, "web", logs).keepLogWithin(ctx, box, limit)
}

// SummaryOf is a summary as an ending carries it.
func SummaryOf(s string) string { return summaryOf(s) }

// AttemptRecord is attempt as a check's box records it.
func AttemptRecord(attempt gitlet.CheckKey) (string, error) { return attemptRecord(attempt) }

// KeepLogAndTail keeps logBox's output as a failed run's log within limit, as a
// run's ending does, and reads tailBox's into its summary, counting what it
// withholds into rec.
func KeepLogAndTail(ctx context.Context, logBox, tailBox sandbox.Sandbox, logs Uploader, limit int64,
	rec *metrics.Recorder) (*gitlet.CheckLog, string, string) {
	a := attemptOf(gitlet.CheckDef{}, "web", logs)
	a.r.o.Metrics = rec
	log, why := a.keepLogWithin(ctx, logBox, limit)
	return log, why, a.tail(ctx, tailBox).text
}
