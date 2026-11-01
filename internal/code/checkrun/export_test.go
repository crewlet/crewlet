package checkrun

import (
	"context"
	"io"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Reported is what a report read into an ending's failures holds: the
// failures kept, how many were omitted past the bounds, and the counts a
// report stated.
type Reported struct {
	Kept                   []code.CheckFailure
	Omitted                int
	Tests, Failed, Results int
}

// reported is fs as a case reads it.
func reported(fs *failures) Reported {
	return Reported{Kept: fs.kept, Omitted: fs.omitted, Tests: fs.tests, Failed: fs.failed, Results: fs.results}
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
func attemptOf(def code.CheckDef, repo string, logs Uploader) *attempt {
	return &attempt{r: &Runner{o: Options{Logs: logs}, log: logging.Get("code.checkrun")},
		c: code.RunnableCheck{Repo: repo, Def: &def}}
}

// Reports reads box's copies of the reports def names as a run's ending does,
// and what of them it could not read.
func Reports(ctx context.Context, box sandbox.Sandbox, def code.CheckDef, repo string) (Reported, []string) {
	fs, unread := attemptOf(def, repo, nil).reports(ctx, box)
	if fs == nil {
		return Reported{}, unread
	}
	return reported(fs), unread
}

// KeepLog keeps box's output as a failed run's log through logs, as a run's
// ending does.
func KeepLog(ctx context.Context, box sandbox.Sandbox, logs Uploader) (*code.CheckLog, string) {
	return attemptOf(code.CheckDef{}, "web", logs).keepLog(ctx, box)
}
