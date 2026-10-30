package checkrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
)

// A SARIF LOG (OASIS SARIF 2.1.0), as a linter or a static analyser writes
// one: `runs`, each with `results`, each a rule's finding — its rule
// (`ruleId`), its level, its message and where (`locations[0]
// .physicalLocation`: the artifact's uri and the region's startLine). A result
// at level `error` or `warning` — SARIF's default where a result names none —
// is a failure; `note` and `none` are a tool's remarks, which judge nothing.
// A location's uri is the path a person opens: a `file://` scheme and the
// checkout's own directory are taken off where a tool wrote them.
//
// Decoded into only the fields read — a log carries rules, artifacts and
// taxonomies besides, some megabytes of them — and at most sandbox.MaxFileBytes
// of it, which ReadFile refuses past.

// sarifLog is what a SARIF log is read for.
type sarifLog struct {
	Runs []struct {
		Results []struct {
			RuleID  string `json:"ruleId"`
			Level   string `json:"level"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
			Locations []struct {
				Physical struct {
					Artifact struct {
						URI string `json:"uri"`
					} `json:"artifactLocation"`
					Region struct {
						StartLine int `json:"startLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
			} `json:"locations"`
		} `json:"results"`
	} `json:"runs"`
}

// readSARIF reads the SARIF log r into fs: every result at level error or
// warning, a failure, the path in its checkout repo where it names one. A log
// that is no JSON, or holds no run, is an error naming what is wrong.
func readSARIF(r io.Reader, repo string, fs *failures) error {
	var log sarifLog
	if err := json.NewDecoder(r).Decode(&log); err != nil {
		return fmt.Errorf("the SARIF log is no JSON a tool writes: %w", err)
	}
	if len(log.Runs) == 0 {
		return errors.New("the SARIF log holds no run")
	}
	fs.results = max(fs.results, 0)
	for _, run := range log.Runs {
		for _, res := range run.Results {
			switch res.Level {
			case "", "error", "warning":
			default:
				continue
			}
			f := code.CheckFailure{TestID: res.RuleID, Message: res.Message.Text}
			if len(res.Locations) > 0 {
				loc := res.Locations[0].Physical
				f.Path, f.Line = sarifPath(loc.Artifact.URI, repo), loc.Region.StartLine
			}
			fs.results++
			fs.add(f)
		}
	}
	return nil
}

// sarifPath is a result's artifact uri as a path in the checkout: a file
// scheme taken off, and a leading directory the checkout is in — a tool run
// from its parent writes `<repo>/…` — taken off too.
func sarifPath(uri, repo string) string {
	p := strings.TrimPrefix(uri, "file://")
	if rest, ok := strings.CutPrefix(p, repo+"/"); ok {
		return rest
	}
	return p
}
