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

// ReadJUnit reads a JUnit report as a run's ending does.
func ReadJUnit(r io.Reader) (Reported, error) {
	fs := newFailures()
	err := readJUnit(r, fs)
	return Reported{Kept: fs.kept, Omitted: fs.omitted, Tests: fs.tests, Failed: fs.failed}, err
}

// ReadSARIF reads a SARIF log of the checkout repo as a run's ending does.
func ReadSARIF(r io.Reader, repo string) (Reported, error) {
	fs := newFailures()
	err := readSARIF(r, repo, fs)
	return Reported{Kept: fs.kept, Omitted: fs.omitted, Results: fs.results}, err
}

// OutputFile is where a run's output is in its box, as a case writes it.
const OutputFile = outputFile

// KeepLog keeps box's output as a failed run's log through logs, as a run's
// ending does.
func KeepLog(ctx context.Context, box sandbox.Sandbox, logs Uploader) (*code.CheckLog, string) {
	a := &attempt{r: &Runner{o: Options{Logs: logs}, log: logging.Get("code.checkrun")},
		c: code.RunnableCheck{Repo: "web", Def: &code.CheckDef{}}}
	return a.keepLog(ctx, box)
}
