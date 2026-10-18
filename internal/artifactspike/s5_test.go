package artifactspike

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
)

// SPIKE S5: `old_str`/`new_str` edits, through the shipped edit application
// ([edits.Normalize], [edits.Apply]) at the publish's own limits.
//
// testdata/s5/cases.json is a set of republishes in the shapes a model's edits
// take: a unique quote of the page it read, several edits in one publish, a
// quote widened until it is unique, a quote too short to be unique, a quote
// made from memory (indentation, quotes, entities or wording drifted), and an
// edit quoting text an earlier edit of the same publish already replaced.
// Each records what the application must answer. The spike reports how many
// apply, why the rest are refused, and the bytes an edit carries against
// republishing the whole file.
//
// The SHARE THAT APPLIES here is the committed mix's, not a model's. The rate
// a real agent achieves is measured by replaying captured republishes through
// this same harness: the procedure is S5 in docs/reference/artifact-capacity.md,
// and a capture is a cases file of the same shape.
//
// Run: go test -run '^$' -bench S5 -benchtime 1x ./internal/artifactspike

// editCase is one republish.
type editCase struct {
	ID     string       `json:"id"`
	Kind   string       `json:"kind"`
	Base   string       `json:"base"`
	Edits  []edits.Edit `json:"edits"`
	Expect string       `json:"expect"`
}

// The outcomes a republish can have.
const (
	editApplied   = "applied"
	editZeroMatch = "zero_match"
	editManyMatch = "many_match"
	editOther     = "refused_other"
)

// casesFile is the committed republishes; CREWLET_SPIKE_S5_CASES names a
// capture to replay instead (the procedure's input), whose bases are read
// from the same corpus directory unless a base is an absolute path.
func casesFile() string {
	if p := os.Getenv("CREWLET_SPIKE_S5_CASES"); p != "" {
		return p
	}
	return filepath.Join("testdata", "s5", "cases.json")
}

func loadEditCases(tb testing.TB, path string) []editCase {
	tb.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read the edit cases: %v", err)
	}
	var out []editCase
	if err := json.Unmarshal(raw, &out); err != nil {
		tb.Fatalf("decode the edit cases: %v", err)
	}
	return out
}

func readBase(tb testing.TB, base string) []byte {
	tb.Helper()
	p := base
	if !filepath.IsAbs(p) {
		p = filepath.Join(corpusDir, base)
	}
	text, err := os.ReadFile(p)
	if err != nil {
		tb.Fatalf("read the base %s: %v", base, err)
	}
	return text
}

// applyCase applies one republish's edits to its base, as the publish
// pipeline does, and answers the outcome.
func applyCase(c editCase, base []byte) string {
	const entry = "index.html"
	list, err := edits.Normalize(c.Edits, entry)
	if err == nil {
		_, err = edits.Apply(map[string][]byte{entry: base}, list,
			func(string) int64 { return artifacts.MaxEntryBytes })
	}
	var refused *edits.Refused
	switch {
	case err == nil:
		return editApplied
	case errors.As(err, &refused) && refused.Matches == 0:
		return editZeroMatch
	case errors.As(err, &refused) && refused.Matches > 1:
		return editManyMatch
	}
	return editOther
}

// TestTheS5EditsApplyAsRecorded holds every committed republish to its
// recorded outcome.
func TestTheS5EditsApplyAsRecorded(t *testing.T) {
	t.Parallel()
	cases := loadEditCases(t, filepath.Join("testdata", "s5", "cases.json"))
	for _, c := range cases {
		if len(c.Edits) > artifacts.MaxEditsPerPublish {
			t.Errorf("%s carries %d edits, past the %d one publish takes", c.ID, len(c.Edits),
				artifacts.MaxEditsPerPublish)
		}
		if got := applyCase(c, readBase(t, c.Base)); got != c.Expect {
			t.Errorf("%s (%s): %s, recorded %s", c.ID, c.Kind, got, c.Expect)
		}
	}
}

// BenchmarkS5Edits reports the share of republishes that apply, each refusal's
// share, the edits a publish carried at most, and the bytes an applied
// publish's edits carry against the whole file.
func BenchmarkS5Edits(b *testing.B) {
	cases := loadEditCases(b, casesFile())
	bases := make([][]byte, len(cases))
	for i, c := range cases {
		bases[i] = readBase(b, c.Base)
	}
	outcomes := map[string]int{}
	var editBytes, wholeBytes, mostEdits int
	b.ResetTimer()
	for range b.N {
		clear(outcomes)
		editBytes, wholeBytes, mostEdits = 0, 0, 0
		for i, c := range cases {
			got := applyCase(c, bases[i])
			outcomes[got]++
			mostEdits = max(mostEdits, len(c.Edits))
			if got != editApplied {
				continue
			}
			for _, e := range c.Edits {
				editBytes += len(e.Old) + len(e.New)
			}
			wholeBytes += len(bases[i])
		}
	}
	b.StopTimer()
	n := float64(len(cases))
	b.ReportMetric(n, "republishes")
	b.ReportMetric(float64(outcomes[editApplied])/n, "applied-share")
	b.ReportMetric(float64(outcomes[editZeroMatch])/n, "zero-match-share")
	b.ReportMetric(float64(outcomes[editManyMatch])/n, "many-match-share")
	b.ReportMetric(float64(outcomes[editOther])/n, "other-refusal-share")
	b.ReportMetric(float64(mostEdits), "most-edits-per-publish")
	if wholeBytes > 0 {
		b.ReportMetric(float64(editBytes)/float64(wholeBytes), "edit-bytes/whole-file-bytes")
	}
}
