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
// STREAMED, as a JUnit report is (junit.go): the log's tokens are walked down
// to `runs[].results[]` and each result decoded alone, into only the fields
// read, then added and dropped, so the failures an ending keeps and the one
// result in hand are all it holds. Everything else a log carries — rules,
// artifacts, taxonomies, some megabytes of them — is walked past token by
// token, never held. Decoded whole, a log up to sandbox.MaxFileBytes was held
// as bytes and then as every result's struct, for every failed check ending
// on a node at once.

// sarifResult is what a SARIF result is read for.
type sarifResult struct {
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
}

// readSARIF reads the SARIF log r into fs: every result at level error or
// warning, a failure, the path in its checkout repo where it names one. A log
// that is no JSON, or holds no run, is an error naming what is wrong, and fs
// keeps the failures it named before it, as readJUnit's does; the count of
// results is the log's only once it was read whole.
func readSARIF(r io.Reader, repo string, fs *failures) error {
	d := json.NewDecoder(r)
	runs, results := 0, 0
	err := sarifObject(d, func(key string) error {
		if key != "runs" {
			return skipJSON(d)
		}
		return sarifArray(d, func() error {
			runs++
			return sarifObject(d, func(key string) error {
				if key != "results" {
					return skipJSON(d)
				}
				return sarifArray(d, func() error {
					var res sarifResult
					if err := d.Decode(&res); err != nil {
						return err
					}
					if f, failed := sarifFailure(res, repo); failed {
						results++
						fs.add(f)
					}
					return nil
				})
			})
		})
	})
	if err != nil {
		return fmt.Errorf("the SARIF log is no JSON a tool writes: %w", err)
	}
	if runs == 0 {
		return errors.New("the SARIF log holds no run")
	}
	fs.results = max(fs.results, 0) + results
	return nil
}

// sarifFailure is res as a failure, false for a result that judges nothing
// (a note, or none).
func sarifFailure(res sarifResult, repo string) (code.CheckFailure, bool) {
	switch res.Level {
	case "", "error", "warning":
	default:
		return code.CheckFailure{}, false
	}
	f := code.CheckFailure{TestID: res.RuleID, Message: res.Message.Text}
	if len(res.Locations) > 0 {
		loc := res.Locations[0].Physical
		f.Path, f.Line = sarifPath(loc.Artifact.URI, repo), loc.Region.StartLine
	}
	return f, true
}

// sarifObject reads the object d is at, calling member at each key with d
// before its value, which member reads or skips.
func sarifObject(d *json.Decoder, member func(key string) error) error {
	if err := delim(d, '{'); err != nil {
		return err
	}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		// Asserted, never a type switch's variable, as readJUnit says why.
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("an object's key is %v, not a string", tok)
		}
		if err := member(key); err != nil {
			return err
		}
	}
	return delim(d, '}')
}

// sarifArray reads the array d is at, calling element with d before each
// element, which element reads — or a null, which SARIF writes for the
// results of a run whose tool could not analyse, as an array of none.
func sarifArray(d *json.Decoder, element func() error) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if open, ok := tok.(json.Delim); !ok || open != '[' {
		return fmt.Errorf("found %v where [ opens", tok)
	}
	for d.More() {
		if err := element(); err != nil {
			return err
		}
	}
	return delim(d, ']')
}

// delim reads d's next token, which must be want.
func delim(d *json.Decoder, want json.Delim) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if got, ok := tok.(json.Delim); !ok || got != want {
		return fmt.Errorf("found %v where %v opens or closes", tok, want)
	}
	return nil
}

// skipJSON reads past the value d is at, a token at a time, holding none of
// it.
func skipJSON(d *json.Decoder) error {
	depth := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if open, ok := tok.(json.Delim); ok {
			if open == '{' || open == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
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
