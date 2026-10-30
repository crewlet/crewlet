package checkrun_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
)

// junitOf is a JUnit report of one suite: failing cases failing, each with a
// message of messageBytes, beside passing ones passing.
func junitOf(failing, passing, messageBytes int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
		`<testsuites><testsuite name="pkg" tests="%d" failures="%d" errors="0">`, failing+passing, failing)
	for i := range passing {
		fmt.Fprintf(&b, `<testcase classname="pkg" name="TestPass%d" time="0.01"/>`, i)
	}
	for i := range failing {
		fmt.Fprintf(&b, `<testcase classname="pkg" name="TestFail%d" file="pkg/a_test.go" line="%d">`+
			`<failure message="want 1, got 2" type="assert">%s</failure></testcase>`, i, 10+i,
			strings.Repeat("x", messageBytes))
	}
	b.WriteString(`</testsuite></testsuites>`)
	return b.String()
}

// A JUNIT REPORT YIELDS ITS FAILURES: each failed case's place, test id and
// message, in the report's order; its suites' counts; at most
// code.MaxCheckFailures of them kept and the rest counted omitted; each
// message at most code.CheckFailureMessageBytes; and the list within
// code.MaxCheckFailureBytes encoded, so an ending carrying it is one its
// record admits. A report that is no JUnit is an error, never a pass.
//
// Mutation: keep every failure past the cap — 250 kept, red; drop the
// message's cut — a 5 KiB message kept whole, red.
func TestAJUnitReportYieldsItsFailures(t *testing.T) {
	t.Parallel()
	got, err := checkrun.ReadJUnit(strings.NewReader(junitOf(3, 7, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Tests != 10 || got.Failed != 3 || len(got.Kept) != 3 || got.Omitted != 0 {
		t.Fatalf("a report of 3 failures in 10 = %+v", got)
	}
	want := code.CheckFailure{Path: "pkg/a_test.go", Line: 10, TestID: "pkg.TestFail0",
		Message: "want 1, got 2\nxxxxxxxxxx"}
	if got.Kept[0] != want {
		t.Errorf("the first failure = %+v, want %+v", got.Kept[0], want)
	}

	many, err := checkrun.ReadJUnit(strings.NewReader(junitOf(250, 0, 5<<10)))
	if err != nil {
		t.Fatal(err)
	}
	if len(many.Kept)+many.Omitted != 250 || len(many.Kept) > code.MaxCheckFailures {
		t.Fatalf("250 failures kept %d and omitted %d, want at most %d kept and the rest counted",
			len(many.Kept), many.Omitted, code.MaxCheckFailures)
	}
	encoded, err := json.Marshal(many.Kept)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > code.MaxCheckFailureBytes {
		t.Errorf("the kept failures weigh %d bytes encoded, past code.MaxCheckFailureBytes (%d)", len(encoded),
			code.MaxCheckFailureBytes)
	}
	for _, f := range many.Kept {
		if len(f.Message) > code.CheckFailureMessageBytes {
			t.Fatalf("a message of %d bytes was kept, past code.CheckFailureMessageBytes", len(f.Message))
		}
	}
	one := 1
	small, err := checkrun.ReadJUnit(strings.NewReader(junitOf(250, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if len(small.Kept) != code.MaxCheckFailures || small.Omitted != 50 {
		t.Errorf("250 small failures kept %d and omitted %d, want %d and 50", len(small.Kept), small.Omitted,
			code.MaxCheckFailures)
	}
	if f := (code.CheckFinished{CheckKey: code.CheckKey{RepoID: "01920000-0000-7000-8000-00000000000a",
		OID: strings.Repeat("a", 40), Target: "refs/heads/main", Name: "unit", Attempt: 1},
		State: code.CheckStateFailed, Reason: code.CheckReasonExit, ExitCode: &one, Failures: many.Kept,
		FailuresOmitted: many.Omitted, FinishedAt: time.Now()}); f.Validate() != nil {
		t.Errorf("an ending carrying the kept failures is refused: %v", f.Validate())
	}

	for name, raw := range map[string]string{
		"not xml":       "not a report",
		"no testsuite":  `<html><body/></html>`,
		"cut off":       `<testsuites><testsuite tests="1"><testcase name="a"><failure>`,
		"an empty file": "",
	} {
		if _, err := checkrun.ReadJUnit(strings.NewReader(raw)); err == nil {
			t.Errorf("%s: read as a report", name)
		}
	}
}

// A SARIF REPORT YIELDS ITS RESULTS: each result at level error or warning —
// SARIF's default where a result names none — a failure with its rule, its
// message and its place, the checkout's directory and a file scheme taken off
// its path; a note judges nothing; and a log that is no SARIF is an error.
//
// Mutation: count notes as failures — three kept, red.
func TestASARIFReportYieldsItsResults(t *testing.T) {
	t.Parallel()
	raw := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"lint"}},"results":[
		{"ruleId":"SA1000","level":"error","message":{"text":"bad regexp"},
		 "locations":[{"physicalLocation":{"artifactLocation":{"uri":"file://web/cmd/main.go"},"region":{"startLine":12}}}]},
		{"ruleId":"ST1003","message":{"text":"naming"},
		 "locations":[{"physicalLocation":{"artifactLocation":{"uri":"internal/x.go"},"region":{"startLine":3}}}]},
		{"ruleId":"NOTE1","level":"note","message":{"text":"a remark"}}]}]}`
	got, err := checkrun.ReadSARIF(strings.NewReader(raw), "web")
	if err != nil {
		t.Fatal(err)
	}
	want := []code.CheckFailure{
		{Path: "cmd/main.go", Line: 12, TestID: "SA1000", Message: "bad regexp"},
		{Path: "internal/x.go", Line: 3, TestID: "ST1003", Message: "naming"},
	}
	if len(got.Kept) != len(want) || got.Kept[0] != want[0] || got.Kept[1] != want[1] || got.Results != 2 {
		t.Fatalf("the log's failures = %+v, want %+v", got, want)
	}
	for name, raw := range map[string]string{"not json": "<xml/>", "no run": `{"runs":[]}`} {
		if _, err := checkrun.ReadSARIF(strings.NewReader(raw), "web"); err == nil {
			t.Errorf("%s: read as a log", name)
		}
	}
}

// FuzzJUnit: whatever bytes a check's command leaves where its JUnit report
// should be, the reader neither panics nor keeps what an ending cannot carry —
// at most code.MaxCheckFailures failures, within code.MaxCheckFailureBytes
// encoded, each field valid UTF-8 and each message within
// code.CheckFailureMessageBytes.
func FuzzJUnit(f *testing.F) {
	f.Add(junitOf(3, 2, 10))
	f.Add(`<testsuite tests="1" failures="1"><testcase classname="a" name="b"><error message="m">` +
		"\xff\xfe" + `</error></testcase></testsuite>`)
	f.Add(`<testsuites><testsuite><testcase><failure><![CDATA[x]]></failure></testcase></testsuite></testsuites>`)
	f.Fuzz(func(t *testing.T, raw string) {
		got, _ := checkrun.ReadJUnit(strings.NewReader(raw))
		if len(got.Kept) > code.MaxCheckFailures || got.Omitted < 0 {
			t.Fatalf("kept %d, omitted %d", len(got.Kept), got.Omitted)
		}
		encoded, err := json.Marshal(got.Kept)
		if err != nil || len(encoded) > code.MaxCheckFailureBytes {
			t.Fatalf("the kept failures encode to %d bytes (%v)", len(encoded), err)
		}
		for _, k := range got.Kept {
			if len(k.Message) > code.CheckFailureMessageBytes || !utf8.ValidString(k.Message) ||
				!utf8.ValidString(k.Path) || !utf8.ValidString(k.TestID) || k.Line < 0 {
				t.Fatalf("a failure an ending refuses was kept: %+v", k)
			}
		}
	})
}
