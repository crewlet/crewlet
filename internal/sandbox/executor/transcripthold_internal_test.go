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

// A TRANSCRIPT PAST ITS HOLD IS THE RECORD OF ITS WHOLE REDACTION: what a run
// sends once it streamed is exactly what the coordinator would keep of the
// transcript held whole, in text and counts, keys split across entries
// wherever they fall. Below the hold it is the entries joined, as before.
func TestATranscriptPastItsHoldIsTheRecordOfItsWholeRedaction(t *testing.T) {
	t.Parallel()
	const hold = 64 << 10
	for name, entries := range map[string][]string{
		"below the hold":                   runEntries(200, 3),
		"past the hold, within the record": runEntries(900, 0, 300, 890),
		"past the record":                  runEntries(6000, 0, 380, 3000, 4400, 5999),
	} {
		d := &claudeEvents{}
		d.transcript.hold = hold
		for _, e := range entries {
			// Plain lines: Claude Code's decoder keeps a line that is no
			// event as an entry of its own.
			d.Line([]byte("  " + e + " "))
		}
		got := d.Result()
		join := strings.Join(entries, "\n")
		if len(join) <= hold {
			if got.Transcript != join || got.TranscriptElidedLines != 0 || got.TranscriptElidedBytes != 0 {
				t.Errorf("%s: kept %d bytes and counted %d lines, %d bytes; want the %d bytes of its entries "+
					"joined", name, len(got.Transcript), got.TranscriptElidedLines, got.TranscriptElidedBytes,
					len(join))
			}
			continue
		}
		want, lines, bytes := recordOf(join)
		if got.Transcript != want || got.TranscriptElidedLines != lines || got.TranscriptElidedBytes != bytes {
			t.Errorf("%s: kept %d bytes and counted %d lines, %d bytes; the record of the whole keeps %d, "+
				"%d and %d", name, len(got.Transcript), got.TranscriptElidedLines, got.TranscriptElidedBytes,
				len(want), lines, bytes)
		}
		if strings.Contains(got.Transcript, "MIIEowIBAAKCAQEA") || strings.Contains(got.Transcript, "hunter2") {
			t.Errorf("%s: a credential survived", name)
		}
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

// A LIVE READING TAKES ITS ENTRIES BEFORE THEY COULD STREAM: one read is
// well under the hold, and a transcript taken once is held raw however long
// it runs, so the live view never needs the halves.
func TestALiveReadingTakesItsEntriesBeforeTheyCouldStream(t *testing.T) {
	t.Parallel()
	if liveWindow > transcriptHoldBytes/2 || liveBacklog+liveReadFloor > transcriptHoldBytes/2 {
		t.Errorf("one live read (%d from the end, or %d) is not under half the hold %d",
			liveWindow, liveBacklog+liveReadFloor, transcriptHoldBytes)
	}
	tr := transcriptLines{hold: 1 << 10}
	tr.add("first")
	if got := tr.take(); len(got) != 1 {
		t.Fatalf("took %q", got)
	}
	entries := runEntries(100)
	for _, e := range entries {
		tr.add(e)
	}
	if tr.stream != nil {
		t.Fatal("a transcript taken live streamed")
	}
	if got := tr.take(); strings.Join(got, "\n") != strings.Join(entries, "\n") {
		t.Errorf("a live reading took %d entries of %d", len(got), len(entries))
	}
}
