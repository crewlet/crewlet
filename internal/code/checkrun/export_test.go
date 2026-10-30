package checkrun

import (
	"io"

	"github.com/crewlet/crewlet/internal/code"
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
