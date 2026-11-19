package executor

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// keyEntries is a private key, one transcript entry per line.
func keyEntries() []string {
	key := []string{"-----BEGIN RSA PRIVATE KEY-----"}
	for range 16 {
		key = append(key, "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun")
	}
	return append(key, "-----END RSA PRIVATE KEY-----")
}

// runEntries is n tool lines with credentials among them and a key split
// across entries at each place in keysAt.
func runEntries(n int, keysAt ...int) []string {
	var out []string
	for i := range n {
		for _, at := range keysAt {
			if at == i {
				out = append(out, keyEntries()...)
			}
		}
		switch i % 97 {
		case 13:
			out = append(out, fmt.Sprintf("[tool %d] bash: export PASSWORD=hunter2hunter%d", i, i))
		case 51:
			out = append(out, fmt.Sprintf("[tool %d] bash: gh auth login --with-token ghp_%036d", i, i))
		default:
			out = append(out, fmt.Sprintf("[tool %d] bash: go test ./pkg/%06d/... %s", i, i, strings.Repeat("-", 80)))
		}
	}
	return out
}

// recordOf is what the run's record keeps of a transcript held whole: its
// redaction written to the halves, as the coordinator bounds one.
func recordOf(text string) (string, int, int) {
	var h sandbox.TranscriptHalves
	_, _ = h.WriteString(redact.Secrets(text))
	return h.Bounded()
}

// heldDecoders are each harness's decoder with its transcript held to hold,
// and how its stream carries an entry: Claude Code's keeps a line that is no
// event as an entry of its own, OpenCode's a text event's words.
func heldDecoders(t *testing.T, hold int) map[string]struct {
	dec  Decoder
	line func(entry string) []byte
} {
	claude, openCode := &claudeEvents{}, &openCodeEvents{}
	claude.transcript.hold, openCode.transcript.hold = hold, hold
	return map[string]struct {
		dec  Decoder
		line func(entry string) []byte
	}{
		"Claude Code": {claude, func(e string) []byte { return []byte("  " + e + " ") }},
		"OpenCode":    {openCode, func(e string) []byte { return openCodeText(t, "m1", e) }},
	}
}

// A TRANSCRIPT PAST ITS HOLD IS THE RECORD OF ITS WHOLE REDACTION: what a run
// sends once it streamed is exactly what the coordinator would keep of the
// transcript held whole, in text and counts, from either harness's decoder,
// keys split across entries wherever they fall. Below the hold it is the
// entries joined, as before.
//
// Mutation: leave the counts out of either decoder's Result, and the record
// past the bound reads as whole.
func TestATranscriptPastItsHoldIsTheRecordOfItsWholeRedaction(t *testing.T) {
	t.Parallel()
	const hold = 64 << 10
	for name, entries := range map[string][]string{
		"below the hold":                   runEntries(200, 3),
		"past the hold, within the record": runEntries(900, 0, 300, 890),
		"past the record":                  runEntries(6000, 0, 380, 3000, 4400, 5999),
	} {
		join := strings.Join(entries, "\n")
		for harness, d := range heldDecoders(t, hold) {
			for _, e := range entries {
				d.dec.Line(d.line(e))
			}
			got := d.dec.Result()
			if len(join) <= hold {
				if got.Transcript != join || got.TranscriptElidedLines != 0 || got.TranscriptElidedBytes != 0 {
					t.Errorf("%s, %s: kept %d bytes and counted %d lines, %d bytes; want the %d bytes of its "+
						"entries joined", harness, name, len(got.Transcript), got.TranscriptElidedLines,
						got.TranscriptElidedBytes, len(join))
				}
				continue
			}
			want, lines, bytes := recordOf(join)
			if got.Transcript != want || got.TranscriptElidedLines != lines || got.TranscriptElidedBytes != bytes {
				t.Errorf("%s, %s: kept %d bytes and counted %d lines, %d bytes; the record of the whole keeps "+
					"%d, %d and %d", harness, name, len(got.Transcript), got.TranscriptElidedLines,
					got.TranscriptElidedBytes, len(want), lines, bytes)
			}
			if strings.Contains(got.Transcript, "MIIEowIBAAKCAQEA") || strings.Contains(got.Transcript, "hunter2") {
				t.Errorf("%s, %s: a credential survived", harness, name)
			}
		}
	}
}

// heldClaudeCode is Claude Code with its decoders' transcripts held to hold.
type heldClaudeCode struct {
	ClaudeCode
	hold int
}

func (h heldClaudeCode) Events() Decoder {
	d := &claudeEvents{}
	d.transcript.hold = h.hold
	return d
}

// heldRun is a box a Claude Code run held to hold was installed in, its event
// stream the entries, one plain line each, and the runner.
func heldRun(t *testing.T, hold int, entries []string) (*Runner, *sandbox.FakeSandbox, Paths) {
	t.Helper()
	runner := New(heldClaudeCode{hold: hold})
	b := sandbox.NewFakeSandbox("box-1")
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := PathsFor(b)
	b.Put(p.Stream(), strings.Join(entries, "\n")+"\n")
	b.Put(p.Err(), "")
	return runner, b, p
}

// A COLLECTED RUN CARRIES ITS TRANSCRIPT'S COUNTS where its result file
// replaces what the stream said, as every Claude Code run's does: the record
// says how much of the transcript's middle was left out, as the stream's
// decoder counted it.
//
// Mutation: drop the counts where the parsed result file takes the stream's
// transcript, and the run's record reads as whole.
func TestACollectedRunCarriesItsTranscriptsCounts(t *testing.T) {
	t.Parallel()
	const hold = 64 << 10
	entries := runEntries(6000, 0, 3000)
	runner, b, p := heldRun(t, hold, entries)
	const result = `{"type":"result","subtype":"success","is_error":false,"result":"done"}`
	b.Put(p.Result(), result)
	b.Put(p.ExitCode(), "0")
	b.Put(p.SessionRecord(), "id 9a8b7c6d-0000-4000-8000-000000000003\ncontinued no\n")
	got, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want, lines, bytes := recordOf(strings.Join(entries, "\n"))
	if lines == 0 || got.TranscriptElidedLines != lines || got.TranscriptElidedBytes != bytes {
		t.Errorf("the collected run counts %d lines and %d bytes left out; its stream's record counts %d and %d",
			got.TranscriptElidedLines, got.TranscriptElidedBytes, lines, bytes)
	}
	if got.Transcript != redact.Secrets(want) {
		t.Errorf("the collected transcript is %d bytes, not the %d of its stream's record", len(got.Transcript),
			len(want))
	}
}

// discardStream is a stream that counts what it is fed and writes nothing,
// or, refusing, writes a line and then answers [redact.ErrDecidedEarly].
type discardStream struct {
	w      io.Writer
	fed    int
	refuse bool
}

const refusedWrite = "written before the refusal"

func (s *discardStream) Write(p []byte) (int, error) {
	s.fed += len(p)
	if s.refuse {
		_, _ = io.WriteString(s.w, refusedWrite+"\n")
	}
	return len(p), nil
}

func (s *discardStream) Close() error {
	if s.refuse {
		return fmt.Errorf("refused: %w", redact.ErrDecidedEarly)
	}
	return nil
}

// A TRANSCRIPT PAST ITS HOLD HOLDS ONLY ITS HALVES: however long a whole
// reading runs, once it streams no entry is held raw.
func TestATranscriptPastItsHoldHoldsOnlyItsHalves(t *testing.T) {
	t.Parallel()
	stream := &discardStream{}
	tr := transcriptLines{hold: 1 << 20, newStream: func(w io.Writer) transcriptStream {
		stream.w = w
		return stream
	}}
	entry := strings.Repeat("x", 4<<10)
	total := 0
	for total < 64<<20 {
		tr.add(entry)
		total += len(entry) + 1
		if len(tr.lines) > 0 && tr.held > 1<<20 {
			t.Fatalf("%d bytes of entries held raw, past the hold", tr.held)
		}
	}
	if len(tr.lines) != 0 || tr.held != 0 {
		t.Errorf("after 64 MiB, %d entries (%d bytes) are held raw", len(tr.lines), tr.held)
	}
	if stream.fed != total-1 {
		t.Errorf("the stream was fed %d bytes of %d", stream.fed, total-1)
	}
}

// A TRANSCRIPT THE STREAM REFUSES IS WITHHELD: a note says so, the counts
// are all it was fed, and nothing the stream wrote is kept.
func TestATranscriptTheStreamRefusesIsWithheld(t *testing.T) {
	t.Parallel()
	tr := transcriptLines{hold: 1 << 10, newStream: func(w io.Writer) transcriptStream {
		return &discardStream{w: w, refuse: true}
	}}
	entries := runEntries(200)
	for _, e := range entries {
		tr.add(e)
	}
	text, lines, bytes := tr.result()
	join := strings.Join(entries, "\n")
	if strings.Contains(text, refusedWrite) || strings.Contains(text, "[tool") {
		t.Errorf("a withheld transcript kept what the stream wrote or was fed: %.200q", text)
	}
	if !strings.Contains(text, "is not kept") || strings.Contains(text, "\n") {
		t.Errorf("a withheld transcript is not the one note saying so: %q", text)
	}
	if lines != len(entries) || bytes != len(join) {
		t.Errorf("counted %d lines and %d bytes; want the %d lines and %d bytes fed", lines, bytes,
			len(entries), len(join))
	}
}

// A LIVE READING IS LIVE FROM ITS FIRST READ: however many entries the read
// that begins it hands the decoder, past the hold or not, every one is shown,
// and the decoder holds none for a Result.
//
// Mutation: leave the reading to mark itself live at its first take, and a
// first read past the hold streams: nothing is shown, then or after.
func TestALiveReadingIsLiveFromItsFirstRead(t *testing.T) {
	t.Parallel()
	const hold = 4 << 10
	entries := runEntries(400, 100)
	join := strings.Join(entries, "\n")
	if len(join) <= 4*hold {
		t.Fatalf("the read of %d bytes is not well past the hold of %d", len(join), hold)
	}
	runner, b, p := heldRun(t, hold, entries)
	b.Put(p.Done(), "0")
	reading := runner.Follow(sandbox.RunHandle{})
	got, err := reading.Read(t.Context(), b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if want := redact.Secrets(join + "\n"); got.Source != sandbox.SourceTranscript || got.Text != want {
		t.Errorf("the first read shows %d bytes from %q; want every entry, %d bytes of the transcript",
			len(got.Text), got.Source, len(want))
	}
	if tr := &reading.(*follower).dec.(*claudeEvents).transcript; tr.stream != nil || len(tr.lines) != 0 {
		t.Errorf("the reading's decoder streamed (%v) or holds %d entries", tr.stream != nil, len(tr.lines))
	}
}
