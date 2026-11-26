package checkrun_test

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// deployToken is a check secret of a shape the redaction knows: a GitHub
// token, ghp_ and 36 letters and digits.
const deployToken = "ghp_aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5"

// redacts fails t unless the redaction takes secret out of a line holding it.
func redacts(t *testing.T, secret string) {
	t.Helper()
	if strings.Contains(redact.Secrets("using "+secret+"\n"), secret) {
		t.Fatalf("the redaction keeps %q; the case needs a secret it knows", secret)
	}
}

// EVERY OUTPUT A CHECK'S BOX HANDS ITS ENDING IS REDACTED BEFORE IT IS CUT OR
// KEPT: a code.checks.env secret and the box's minted token, printed where the
// tail, the summary's bound and a failure message's bound each cut, reach no
// tail, summary, failure or kept log, whole or in part.
//
// Mutations, each red: the tail read raw and cut at CheckTailBytes (the
// token's end and the minted token show); summaryOf cutting without redacting
// (the secret's head shows); failures.add cutting without redacting, or the
// JUnit body held to the message's bound alone (the secret's head shows, cut
// short of a shape the redaction knows); the log streamed raw (both show).
func TestEveryOutputOfACheckIsRedactedBeforeItIsCutOrKept(t *testing.T) {
	t.Parallel()
	redacts(t, deployToken)
	f := newFleet(t)
	f.company.Code.Checks.Env = map[string]string{"DEPLOY_TOKEN": deployToken}
	store := storeOver(t, memobj.New())
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Logs = store
	f.runner(o)
	d := def("unit", time.Minute)
	d.Env = []string{"DEPLOY_TOKEN"}
	d.Report.JUnit = "out/junit.xml"
	key := f.queue(oidOf("a"), d)
	box := f.container.box(t)
	// THE TOKEN ITS FILE HOLDS, written by the box's setup, which is over once
	// the attempt names its box.
	f.await(key, "named its box", func(r gitlet.CheckRow) bool { return r.Box != "" })
	raw, err := box.ReadFile(t.Context(), sandbox.GitAccessFile)
	if err != nil || len(raw) == 0 {
		t.Fatalf("the box's token file: %q, %v", raw, err)
	}
	minted := string(raw)
	redacts(t, minted)

	// The old tail's cut, CheckTailBytes from the end, falls 20 bytes into the
	// secret; the minted token is printed whole on the line after it.
	rest := "\nthe box's own: " + minted + "\n"
	rest += strings.Repeat("FAIL web/cmd\n", (gitlet.CheckTailBytes-20-len(rest))/13-1)
	rest += strings.Repeat("!", gitlet.CheckTailBytes-20-len(rest)-1) + "\n"
	output := strings.Repeat("compiling a package of the build\n", 200) + "deploying with " + deployToken + rest
	if cut := len(output) - gitlet.CheckTailBytes; output[cut-20:cut+20] != deployToken {
		t.Fatal("the output's arithmetic")
	}
	// The summary's and the message's bounds each fall 10 bytes into it.
	box.Put(sandbox.DefaultHome+"/.crewlet-check/summary",
		strings.Repeat("s", gitlet.CheckSummaryBytes-11)+" "+deployToken)
	const said = "want 1, got 2"
	box.Put(sandbox.DefaultHome+"/.crewlet-check/junit.xml", `<testsuite name="pkg" tests="1" failures="1">`+
		`<testcase classname="pkg" name="TestDeploy"><failure message="`+said+`">`+
		strings.Repeat("x", gitlet.CheckFailureMessageBytes-len(said)-1-11)+" "+deployToken+
		`</failure></testcase></testsuite>`)
	f.container.end(box, 1, output)
	f.ended(key)

	got := f.detail(key)
	head := deployToken[:10]
	if strings.Contains(got.Tail, deployToken[20:]) || strings.Contains(got.Tail, minted) ||
		!strings.Contains(got.Tail, redact.Marker) {
		t.Errorf("the tail holds a secret or no marker: %.200q", got.Tail)
	}
	if strings.Contains(got.Summary, head) {
		t.Errorf("the summary ends %q, the secret's head", got.Summary[len(got.Summary)-40:])
	}
	if len(got.Failures) != 1 || strings.Contains(got.Failures[0].Message, head) {
		t.Errorf("the failures %.300v hold the secret's head; want one, without it", got.Failures)
	}
	if got.Log == nil {
		t.Fatalf("no log was kept: %q", got.Summary)
	}
	rc, err := store.Open(t.Context(), got.Log.Content())
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	kept, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || bytes.Contains(kept, []byte(deployToken)) || bytes.Contains(kept, []byte(minted)) ||
		!bytes.HasSuffix(kept, []byte("!\n")) {
		t.Errorf("the kept log (%v) holds a secret, or not the output's end", err)
	}
}

// A FAILURE'S MESSAGE IS REDACTED WHOLE, from a JUnit report and a SARIF log
// alike, before it is cut to CheckFailureMessageBytes. A test id is redacted
// too (a parametrised test's spells its values), and one holding no secret, and
// the path, are kept as written.
//
// Mutation: cut the message without redacting it, or keep the test id as
// written, and the secret shows (red).
func TestAFailuresMessageIsRedactedBeforeItIsCut(t *testing.T) {
	t.Parallel()
	redacts(t, deployToken)
	junit, err := checkrun.ReadJUnit(strings.NewReader(`<testsuite name="pkg" tests="1" failures="1">` +
		`<testcase classname="pkg" name="TestDeploy" file="pkg/deploy_test.go"><failure message="pushing with ` +
		deployToken + `">` + strings.Repeat("y", 5000) + `</failure></testcase></testsuite>`))
	if err != nil {
		t.Fatalf("ReadJUnit: %v", err)
	}
	sarif, err := checkrun.ReadSARIF(strings.NewReader(`{"version":"2.1.0","runs":[{"results":[`+
		`{"ruleId":"secrets/leak","level":"error","message":{"text":"found `+deployToken+`"},`+
		`"locations":[{"physicalLocation":{"artifactLocation":{"uri":"deploy.sh"},"region":{"startLine":3}}}]}]}]}`),
		"web")
	if err != nil {
		t.Fatalf("ReadSARIF: %v", err)
	}
	for _, r := range []checkrun.Reported{junit, sarif} {
		if len(r.Kept) != 1 || strings.Contains(r.Kept[0].Message, deployToken[:10]) ||
			!strings.Contains(r.Kept[0].Message, redact.Marker) || len(r.Kept[0].Message) >
			gitlet.CheckFailureMessageBytes {
			t.Errorf("the failures read %+v; want one, its message redacted and within its bound", r.Kept)
		}
	}
	if junit.Kept[0].TestID != "pkg.TestDeploy" || junit.Kept[0].Path != "pkg/deploy_test.go" {
		t.Errorf("the JUnit failure is %q at %q; want its test id and path as written", junit.Kept[0].TestID,
			junit.Kept[0].Path)
	}
	param, err := checkrun.ReadJUnit(strings.NewReader(`<testsuite name="pkg" tests="1" failures="1">` +
		`<testcase classname="pkg" name="test_push[` + deployToken + `]"><failure message="refused"/>` +
		`</testcase></testsuite>`))
	if err != nil {
		t.Fatalf("ReadJUnit: %v", err)
	}
	if len(param.Kept) != 1 || strings.Contains(param.Kept[0].TestID, deployToken[:10]) ||
		!strings.Contains(param.Kept[0].TestID, redact.Marker) {
		t.Errorf("a parametrised failure reads %+v; want its test id redacted", param.Kept)
	}
}

// keptOf is the log KeepLogWithin keeps of output within limit bytes, the
// bytes it read from the box, and why it kept none.
func keptOf(t *testing.T, output string, limit int64) (string, int64, string) {
	t.Helper()
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(checkrun.OutputFile, output)
	store := storeOver(t, memobj.New())
	log, why := checkrun.KeepLogWithin(t.Context(), box, store, limit)
	if log == nil {
		return "", box.Delivered(), why
	}
	rc, err := store.Open(t.Context(), log.Content())
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	defer func() { _ = rc.Close() }()
	kept, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	return string(kept), box.Delivered(), why
}

// A LOG BEGUN INSIDE THE OUTPUT SHOWS WHAT ITS CONTEXT SETTLES: a password's
// key in the bytes read before the log, its value on the first line after
// them, and a token inside the log, are all redacted as the whole output
// redacts them, the log beginning past the value's line and ending at the
// output's end.
//
// Mutations, each red: stream the log raw (both show); drop the context
// unread and redact from the first line past it (the value shows).
func TestALogBegunInsideTheOutputShowsWhatItsContextSettles(t *testing.T) {
	t.Parallel()
	const limit = 1 << 20
	const value = "hunter2x9"
	key := "the database password:\n"
	valueLine := value + "\n"
	if strings.Contains(redact.Secrets("starting\n"+key+valueLine), value) {
		t.Fatal("the redaction keeps the value under its key; the case needs one it takes out")
	}
	var b strings.Builder
	for b.Len() < limit-len(valueLine)-200 {
		fmt.Fprintf(&b, "compiling package %d of the build\n", b.Len())
	}
	body := b.String() + "deploying with " + deployToken + "\n"
	body += strings.Repeat("-", limit-len(valueLine)-len(body)-len("FAIL\n")) + "FAIL\n"
	prefix := strings.Repeat("linking\n", (2*redact.LookBack)/8)
	output := prefix + key + valueLine + body
	kept, read, why := keptOf(t, output, limit)
	if why != "" || strings.Contains(kept, value) || strings.Contains(kept, deployToken) ||
		!strings.HasSuffix(kept, "-FAIL\n") || len(kept) > limit {
		t.Fatalf("the log is %d bytes opening %.60q (%q); want within %d, the output's end, no secret", len(kept),
			kept, why, limit)
	}
	if read != limit+redact.LookBack {
		t.Errorf("keeping the log read %d bytes from the box; want the bound and the redaction's look-back, %d",
			read, limit+redact.LookBack)
	}
}

// A LOG'S BOUND HOLDS AFTER ITS REDACTION: an output within the bound whose
// markers are longer than the values they replace is kept from the first
// line that leaves the rest within it, through the output's end, read twice.
//
// Mutation: hand the store the redaction past the bound (the store refuses
// it and no log is kept, red); keep its first limit bytes (the end is lost,
// red).
func TestALogsBoundHoldsAfterItsRedaction(t *testing.T) {
	t.Parallel()
	const limit = 1 << 20
	line := "password: hunter2\n"
	if r := redact.Secrets(line); len(r) <= len(line) {
		t.Fatalf("the line redacts to %q, no longer than itself; the case needs one that grows", r)
	}
	output := strings.Repeat(line, (limit-10)/len(line)) + "FAIL web\n"
	kept, read, why := keptOf(t, output, limit)
	end := redact.Secrets(line) + "FAIL web\n"
	if why != "" || len(kept) > limit || len(kept) < limit-len(end) || !strings.HasSuffix(kept, end) ||
		!strings.HasPrefix(kept, "password: ") || strings.Contains(kept, "hunter2") {
		t.Fatalf("the log is %d bytes opening %.40q (%q); want whole redacted lines within %d through the end",
			len(kept), kept, why, limit)
	}
	if read != 2*int64(len(output)) {
		t.Errorf("keeping the log read %d bytes from the box; want the output twice, %d", read, 2*len(output))
	}
}

// A RUN'S OUTPUT IS KEPT AND SUMMED UP ONLY AS ONE VERSION OF IT: an output cut
// or grown in place while its log or its tail is read keeps no log, saying
// why, and leaves the tail empty, since what was read may join two versions of
// it; one left alone, or written anew over its path, is kept and read.
//
// Mutation: leave the check of the output after the log's read, or the tail's,
// out.
func TestARunsOutputIsKeptOnlyAsOneVersionOfIt(t *testing.T) {
	t.Parallel()
	output := strings.Repeat("compiling a package of the build\n", 2000) + "FAIL web\n"
	for _, c := range []struct {
		name    string
		edit    func(b *sandbox.FakeSandbox)
		changed bool
	}{
		{"left alone", func(*sandbox.FakeSandbox) {}, false},
		{"written anew", func(b *sandbox.FakeSandbox) { b.Put(checkrun.OutputFile, output) }, false},
		{"grown in place", func(b *sandbox.FakeSandbox) { b.AppendInPlace(checkrun.OutputFile, []byte("ok\n")) }, true},
		{"cut in place", func(b *sandbox.FakeSandbox) { b.TruncateInPlace(checkrun.OutputFile, 100) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// A box of its own for each read, edited as its first open reads it.
			fresh := func() *sandbox.FakeSandbox {
				box := sandbox.NewFakeSandbox("box-1")
				box.Put(checkrun.OutputFile, output)
				var once sync.Once
				box.Streams = func(path string, r io.Reader) io.Reader {
					once.Do(func() { c.edit(box) })
					return r
				}
				return box
			}
			log, why := checkrun.KeepLogWithin(t.Context(), fresh(), storeOver(t, memobj.New()), 1<<20)
			tail := checkrun.TailOf(t.Context(), fresh())
			switch {
			case c.changed && (log != nil || why != "its log was not kept: its output changed while it was read"):
				t.Errorf("an output %s while read kept log %v (%q); want none, saying why", c.name, log, why)
			case c.changed && tail != "":
				t.Errorf("an output %s while read sums up as %q; want nothing", c.name, tail)
			case !c.changed && (log == nil || !strings.HasSuffix(tail, "FAIL web\n")):
				t.Errorf("an output %s kept log %v (%q) and sums up as …%q; want both", c.name, log, why,
					tail[max(0, len(tail)-40):])
			}
		})
	}
}
