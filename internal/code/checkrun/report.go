package checkrun

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
)

// WHAT A FAILED RUN'S REPORTS NAME: the failures a check's own report files
// say — a JUnit XML file (junit.go) and a SARIF log (sarif.go), each a path
// in its checkout its definition names — read once its command ends, each at
// most sandbox.MaxFileBytes, and held to what an ending carries: at most
// code.MaxCheckFailures failures weighing at most code.MaxCheckFailureBytes
// encoded, each message at most code.CheckFailureMessageBytes, and every one
// past either counted rather than named (FailuresOmitted). FIRST COME FIRST
// KEPT, in the report's own order: the order a test runner writes is the order
// a person reads, and the first failure of a cascade is the one that says
// why.

// failures is what a run's reports named, held to an ending's bounds.
type failures struct {
	kept    []code.CheckFailure
	bytes   int
	omitted int
	// tests and failed are the JUnit report's counts, and results how many
	// SARIF results were failures — each -1 where no report of its kind was
	// read.
	tests, failed, results int
}

func newFailures() *failures { return &failures{tests: -1, failed: -1, results: -1} }

// failureOverhead is what one failure's encoding adds to its fields' bytes
// beyond them — its keys, quotes, braces and the comma between two — so the
// running weight never undercounts the encoded list.
var failureOverhead = func() int {
	b, _ := json.Marshal([]code.CheckFailure{{Path: "", Line: 1, TestID: "", Message: ""}})
	return len(b)
}()

// add keeps f where both bounds hold it, its fields valid UTF-8 and its
// message cut to CheckFailureMessageBytes, and counts it omitted otherwise.
func (fs *failures) add(f code.CheckFailure) {
	f.Path = strings.ToValidUTF8(f.Path, "")
	f.TestID = strings.ToValidUTF8(f.TestID, "")
	f.Message = cut(strings.ToValidUTF8(f.Message, ""), code.CheckFailureMessageBytes)
	if f.Line < 0 {
		f.Line = 0
	}
	weight := failureOverhead + jsonBytes(f.Path) + jsonBytes(f.TestID) + jsonBytes(f.Message) + 20
	if len(fs.kept) >= code.MaxCheckFailures || fs.bytes+weight > code.MaxCheckFailureBytes {
		fs.omitted++
		return
	}
	fs.kept = append(fs.kept, f)
	fs.bytes += weight
}

// jsonBytes is the most bytes s encodes to as a JSON string's contents:
// json.Marshal's escaping, read off the encoder rather than estimated.
func jsonBytes(s string) int {
	b, _ := json.Marshal(s)
	return len(b) - 2
}

// cut is s cut to at most n bytes at a character's boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
