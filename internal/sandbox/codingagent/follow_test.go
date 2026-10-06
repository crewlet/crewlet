package codingagent_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// read is one read of a live reading, failing the case on an error.
func read(t *testing.T, reading sandbox.LiveReading, b sandbox.Sandbox) sandbox.LiveRead {
	t.Helper()
	got, err := reading.Read(t.Context(), b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return got
}

func textEvent(text string) string {
	return `{"type":"text","part":{"text":"` + text + `"}}` + "\n"
}

// A LIVE READING IS THE PARSED TRANSCRIPT, REDACTED, and touches nothing.
//
// What a person watching a run wants is what it is doing now; the box's
// environment holds the seat's credentials, so a key the agent echoed must not
// reach a screen; and a read racing the completion poll must not change what
// the poll sees.
func TestALiveReadingIsTheTranscriptRedacted(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	secret := "sk-ant-api03-" + strings.Repeat("x", 40)
	b.Put(p.Result(), textEvent("cloning the repository")+textEvent("exporting "+secret+" and running go test"))
	b.Put(p.Err(), "stderr is not what a parsed transcript run shows")

	got := read(t, runner.Follow(sandbox.RunHandle{}), b)
	if got.Source != sandbox.SourceTranscript || got.Origin != "transcript@0" || got.Front {
		t.Errorf("source %q, origin %q, front %v; want the transcript, read from its start",
			got.Source, got.Origin, got.Front)
	}
	if strings.Contains(got.Text, secret) {
		t.Error("a credential the agent echoed reached the live output unredacted")
	}
	if got.Text != "cloning the repository\nexporting "+redact.Marker+"api-key] and running go test\n" {
		t.Errorf("text = %q; want every entry, a line each", got.Text)
	}
	if got.Finished || got.AsOf.IsZero() {
		t.Errorf("finished %v, as of %v; want a running job, stamped", got.Finished, got.AsOf)
	}
	if done, _ := b.ReadFile(t.Context(), p.Done()); len(done) != 0 {
		t.Error("a read wrote the done marker")
	}
}

// A READING TAKES ONLY WHAT WAS WRITTEN SINCE: every read adds the entries the
// stream gained, under the same origin, and never repeats one.
//
// Mutation: decode the stream's window again on every read, and the second
// read repeats the first entry.
func TestALiveReadingTakesOnlyWhatWasWrittenSince(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	reading := runner.Follow(sandbox.RunHandle{})
	stream := textEvent("first")
	b.Put(p.Result(), stream)
	if got := read(t, reading, b); got.Text != "first\n" {
		t.Fatalf("first read = %q", got.Text)
	}
	// A partial event is not a line yet: it waits for its line break.
	b.Put(p.Result(), stream+textEvent("second")+`{"type":"text","part":{"te`)
	second := read(t, reading, b)
	if second.Text != "second\n" || second.Origin != "transcript@0" {
		t.Errorf("second read = %q under %q; want only the new entry, same origin", second.Text, second.Origin)
	}
	b.Put(p.Result(), stream+textEvent("second")+textEvent("third"))
	if got := read(t, reading, b); got.Text != "third\n" {
		t.Errorf("third read = %q; want the event that finished writing", got.Text)
	}
}

// WHAT A READING SHOWS IS THE WHOLE TEXT REDACTED, however the reads fell: an
// error stream written a few bytes at a time, with a private key and a
// password on a later line in it, settles to exactly what redacting the whole
// stream gives — and shows neither secret in clear at any read.
//
// Mutation: show every complete line as it lands, and the key's body is on
// screen before its END line redacts it.
func TestALiveReadingIsTheWholeTextRedactedHoweverItArrives(t *testing.T) {
	t.Parallel()
	const body = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	stderr := "cloning\n-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat(body+"\n", 6) +
		"-----END RSA PRIVATE KEY-----\nlogin password:\n  hunter2\nbuilding\nlast line, unfinished"
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	reading := runner.Follow(sandbox.RunHandle{})

	var shown strings.Builder
	for at := 0; at < len(stderr); at += 37 {
		b.Put(p.Err(), stderr[:min(at+37, len(stderr))])
		got := read(t, reading, b)
		shown.WriteString(got.Text)
		if strings.Contains(shown.String(), body) || strings.Contains(shown.String(), "hunter2") {
			t.Fatalf("a secret was shown before its shape was whole: %q", shown.String())
		}
		if got.Source != sandbox.SourceStderr || got.Origin != "stderr@0" {
			t.Fatalf("source %q, origin %q; want the error stream", got.Source, got.Origin)
		}
	}
	b.Put(p.Done(), "0")
	final := read(t, reading, b)
	shown.WriteString(final.Text)
	if !final.Finished || final.Held != 0 {
		t.Errorf("finished %v, held %d; want a done job with nothing held back", final.Finished, final.Held)
	}
	if want := redact.Secrets(stderr); shown.String() != want {
		t.Errorf("shown:\n%q\nwant the whole stream redacted:\n%q", shown.String(), want)
	}
}

// A READING OF A LONG STREAM BEGINS AT ITS LAST WINDOW, ON A LINE, and says
// so: what came before was never read. The stream here is past the cap a whole
// read refused, which made the live view an error for the rest of the run.
func TestALiveReadingOfALongStreamBeginsAtItsEnd(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	b.Put(paths(b).Result(), bigToolStream(
		`{"type":"tool_use","part":{"tool":"bash","state":{"input":{"command":"go vet ./..."}}}}`))

	got := read(t, runner.Follow(sandbox.RunHandle{}), b)
	if got.Source != sandbox.SourceTranscript || got.Text != "[tool] bash: go vet ./...\n" {
		t.Errorf("source %q, text %q; want the stream's newest event", got.Source, got.Text)
	}
	if !got.Front || got.Origin == "transcript@0" {
		t.Errorf("front %v, origin %q; want a reading that began after the stream did", got.Front, got.Origin)
	}
}

// THE ERROR STREAM'S READING STARTS ON A LINE, and its unfinished last line is
// held, not shown: half a line read as a whole one says something its writer
// did not, and a token half written is a line that changes.
func TestALiveReadingOfTheErrorStreamStartsOnALine(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	b.Put(paths(b).Err(), strings.Repeat("progress 0123456789\n", 80_000)+"now compiling")

	got := read(t, runner.Follow(sandbox.RunHandle{}), b)
	if !strings.HasPrefix(got.Text, "progress 0123456789\n") || !strings.HasSuffix(got.Text, "progress 0123456789\n") {
		t.Errorf("the reading = %.40q … %q; want whole lines", got.Text, tailOf(got.Text, 20))
	}
	if !got.Front || got.Held != len("now compiling") {
		t.Errorf("front %v, held %d; want the reading's start said and the unfinished line held",
			got.Front, got.Held)
	}
}

// A STREAM THAT OUTRUNS THE READING BEGINS AGAIN, UNDER ANOTHER NAME. A burst
// past what one read catches up — a tool's echoed output of a large file —
// is read from the stream's end again rather than in full, and the reading's
// origin moves, so whoever holds the old one is reset rather than handed text
// that does not follow what it holds.
func TestAStreamThatOutrunsTheReadingBeginsAgain(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	reading := runner.Follow(sandbox.RunHandle{})
	b.Put(p.Result(), textEvent("before the burst"))
	first := read(t, reading, b)

	b.Put(p.Result(), textEvent("before the burst")+bigToolStream(textEvent("after the burst")))
	again := read(t, reading, b)
	if again.Origin == first.Origin || !again.Front {
		t.Errorf("origin %q after %q, front %v; want a new reading, begun at the end",
			again.Origin, first.Origin, again.Front)
	}
	if !strings.HasSuffix(again.Text, "after the burst\n") || strings.Contains(again.Text, "before the burst") {
		t.Errorf("the new reading = %q; want what the stream's end says", again.Text)
	}
}

// THE READING SWITCHES TO THE TRANSCRIPT WHEN IT SPEAKS: a job whose stream
// has said nothing shows its error stream, and the moment the transcript has
// an entry it is the account shown, under its own origin.
func TestAReadingSwitchesToTheTranscriptWhenItSpeaks(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	reading := runner.Follow(sandbox.RunHandle{})

	if got := read(t, reading, b); got.Source != sandbox.SourceNone || got.Text != "" {
		t.Fatalf("an empty box = %+v; want nothing to show", got)
	}
	b.Put(p.Err(), "starting the agent\n")
	early := read(t, reading, b)
	if early.Source != sandbox.SourceStderr || early.Text != "starting the agent\n" {
		t.Fatalf("a box with only stderr = %+v; want its stderr", early)
	}
	b.Put(p.Result(), textEvent("on it"))
	spoke := read(t, reading, b)
	if spoke.Source != sandbox.SourceTranscript || spoke.Origin == early.Origin || spoke.Text != "on it\n" {
		t.Errorf("once the transcript speaks = %+v; want it, under its own origin", spoke)
	}
}

// TWO READINGS OF ONE STREAM AGREE: the same origin and the same text, which is
// what lets a run's next owner continue a viewer where its last owner left it.
func TestTwoReadingsOfOneStreamAgree(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	var stream strings.Builder
	for i := range 50 {
		stream.WriteString(textEvent(fmt.Sprintf("step %d", i)))
	}
	b.Put(paths(b).Result(), stream.String())

	one := read(t, runner.Follow(sandbox.RunHandle{}), b)
	other := read(t, runner.Follow(sandbox.RunHandle{}), b)
	if one.Origin != other.Origin || one.Text != other.Text {
		t.Errorf("two readings disagree: %q/%d bytes and %q/%d bytes",
			one.Origin, len(one.Text), other.Origin, len(other.Text))
	}
}

// A READING SHOWS WHAT A CLAUDE RUN IS DOING — the transcript of the stream so
// far, which under `json` was nothing at all until the run ended.
func TestALiveReadingShowsARunningClaudeRun(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	// Mid-run: the stream so far, no result and no done marker.
	b.Put(p.Stream(), strings.Join(claudeRunStream[:7], "\n")+"\n")
	b.Put(p.Err(), "not what a run that streams shows")

	got := read(t, runner.Follow(sandbox.RunHandle{}), b)
	want := "I'll run the suite first.\n[tool] Bash: go test ./...\n[tool] Read: /home/user/repo/flaky_test.go\n" +
		"[tool] Bash: go test -run TestFlaky -count=20 ./...\n"
	if got.Source != sandbox.SourceTranscript || got.Text != want || got.Finished {
		t.Errorf("read = source %q, finished %v, text\n%s\nwant the stream's transcript so far",
			got.Source, got.Finished, got.Text)
	}
}
