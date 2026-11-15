package redact_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
)

// streamCorpus is the package's keys in every wrapping it reads them in
// ([wrappings], [realKeys]), each set among the words and the shapes of
// [redact.StreamText] at a place drawn from r, so a stage's window ends inside
// a block, beside it, or nowhere near it: four long texts, each holding every
// wrapping of one key in turn.
func streamCorpus(r *rand.Rand) map[string]string {
	keys := realKeys()
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range keys {
			if !yield(name) {
				return
			}
		}
	})
	all := wrappings()
	forms := slices.Sorted(func(yield func(string) bool) {
		for name := range all {
			if !yield(name) {
				return
			}
		}
	})
	corpus := map[string]string{}
	for i, kname := range names {
		if i == 4 {
			break
		}
		var b strings.Builder
		for _, wname := range forms {
			b.WriteString(redact.StreamText(r, r.IntN(redact.MaxReach/2)))
			b.WriteString("\n")
			b.WriteString(all[wname].wrap(keys[kname]).text)
			b.WriteString("\n")
		}
		b.WriteString(redact.StreamText(r, redact.MaxReach))
		corpus[kname] = b.String()
	}
	return corpus
}

// cut is text written to a [redact.Stream] in pieces of 1 to most bytes, drawn
// from r.
func cut(r *rand.Rand, text string, most int) string {
	var out bytes.Buffer
	s := redact.NewStream(&out)
	for at := 0; at < len(text); {
		n := 1 + r.IntN(most)
		if _, err := s.Write([]byte(text[at:min(len(text), at+n)])); err != nil {
			panic(err)
		}
		at += n
	}
	if err := s.Close(); err != nil {
		panic(err)
	}
	return out.String()
}

// streamParted is where got and want part, and each around it.
func streamParted(got, want string) string {
	j := 0
	for j < len(got) && j < len(want) && got[j] == want[j] {
		j++
	}
	lo := max(0, j-80)
	return fmt.Sprintf("at byte %d (%d against %d bytes):\n got  …%q…\n want …%q…", j, len(got), len(want),
		got[lo:min(len(got), j+80)], want[lo:min(len(want), j+80)])
}

// A STREAM WRITES WHAT SECRETS DOES, HOWEVER THE TEXT IS CUT: what it writes,
// put together, is [redact.Secrets] of everything written to it, whether the
// text arrives a byte at a time, in pieces of any size, or whole: over texts
// of every shape a stage carries across its window ([redact.StreamText]), the
// package's keys in every wrapping set at random among them, and texts a byte
// either side of the length below which a text is redacted whole.
//
// Mutation: let the keys' stage read a block before the claims through its
// window are decided ([keyStage.step]), or keep no claim whose end is still to
// come ([claimSource.token]), and a text here is redacted otherwise in pieces.
func TestAStreamWritesWhatSecretsDoesHoweverTheTextIsCut(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 3))
	texts := streamCorpus(r)
	for i := range 3 {
		texts[fmt.Sprintf("shapes %d", i)] = redact.StreamText(r, 3*redact.MaxReach+r.IntN(8*redact.MaxReach))
	}
	for name, text := range texts {
		want := redact.Secrets(text)
		for _, most := range []int{4 << 10, 64 << 10, len(text)} {
			if got := cut(r, text, most); got != want {
				t.Fatalf("%s (%d bytes), written in pieces of at most %d bytes: %s", name, len(text), most, streamParted(got, want))
			}
		}
	}
	// A byte at a time, around the length below which a text is redacted
	// whole: a stream that holds it until it passes that length writes
	// what one that never does writes.
	text := redact.StreamText(r, 4*redact.MaxReach)
	for _, n := range []int{3*redact.MaxReach + 1536, 3*redact.MaxReach + 1537} {
		piece := text[:min(len(text), n)]
		if got, want := cut(r, piece, 1), redact.Secrets(piece); got != want {
			t.Fatalf("%d bytes written a byte at a time: %s", len(piece), streamParted(got, want))
		}
	}
}

// WHAT A STREAM WRITES IS REDACTED ALREADY: redacting it again changes
// nothing, as redacting a whole text twice changes nothing ([redact.Secrets]),
// whether the text is short enough to be redacted whole or runs through the
// stages.
//
// Mutation: write a short text as it was written ([redact.Stream.Close]), and
// redacting what the stream wrote takes what it published.
func TestWhatAStreamWritesIsRedactedAlready(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 4))
	for i := range 4 {
		size := 3*redact.MaxReach + r.IntN(8*redact.MaxReach)
		if i == 0 {
			size = redact.MaxReach
		}
		text := redact.StreamText(r, size)
		once := cut(r, text, 64<<10)
		if twice := redact.Secrets(once); twice != once {
			t.Fatalf("text %d (%d bytes): redacting what a stream wrote changed it %s", i, len(text), streamParted(twice, once))
		}
		if twice := cut(r, once, 64<<10); twice != once {
			t.Fatalf("text %d (%d bytes): a second stream changed the first's %s", i, len(text), streamParted(twice, once))
		}
	}
}

// failingWriter answers every write with err.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

// A STREAM ANSWERS ITS WRITER'S ERROR, and writes nothing more once its writer
// has failed: the first error, from every write after the one that met it, and
// from Close. And it takes no write once closed.
func TestAStreamAnswersItsWritersErrorAndRefusesAWriteOnceClosed(t *testing.T) {
	t.Parallel()
	failed := errors.New("disk full")
	s := redact.NewStream(failingWriter{failed})
	piece := []byte(strings.Repeat("the build passed ", 64<<10))
	var got error
	for range 64 {
		if _, got = s.Write(piece); got != nil {
			break
		}
	}
	if !errors.Is(got, failed) {
		t.Fatalf("64 MiB written to a failing writer, and a write answered %v, not its error", got)
	}
	if n, err := s.Write(piece); n != 0 || !errors.Is(err, failed) {
		t.Errorf("a write after the writer failed took %d bytes and answered %v", n, err)
	}
	if err := s.Close(); !errors.Is(err, failed) {
		t.Errorf("Close after a failed writer answered %v, not its error", err)
	}

	s = redact.NewStream(io.Discard)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Write([]byte("pwd=x")); n != 0 || err == nil {
		t.Errorf("a write after Close took %d bytes and answered %v", n, err)
	}
}

// BenchmarkStreamAtThePlaintextBound streams 256 MiB of what a kept session
// holds (prose and tool output in JSON lines, a credential now and then, and
// base64 images) through a [redact.Stream] in 64 KiB writes, the most the
// take's scrub reads of an archive at once: the plaintext bound of one reading
// (internal/sandbox/executor's scrubExpansion times the archive's), which
// internal/sandbox's SessionScrubWait reads twice within its two minutes.
//
// Recorded on two cores of a runner shared with other work, 2026-11-15: 19.7
// MB/s, 13.6 s a reading at the bound, under a quarter of the minute each of
// the scrub's two readings has. Over 64 MiB of the same text, Secrets, which
// reads it whole, ran at about 95 MB/s, and the stages with no key's armour to
// read (fed a whole text that holds none) at about 43.
func BenchmarkStreamAtThePlaintextBound(b *testing.B) {
	text := keptSession(256 << 20)
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		s := redact.NewStream(io.Discard)
		for at := 0; at < len(text); at += 64 << 10 {
			_, _ = s.Write(text[at:min(len(text), at+64<<10)])
		}
		_ = s.Close()
	}
	b.ReportMetric(float64(b.Elapsed())/float64(b.N)/float64(time.Second), "s/reading")
}

// keptSession is n bytes of what a kept session holds: JSON lines of prose and
// tool output, a password or a token now and then, and base64 images.
func keptSession(n int) []byte {
	r := rand.New(rand.NewPCG(1, 1))
	const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	words := strings.Fields("the a file read write ok done test build go run error line function value key token session user model tool result")
	b := make([]byte, 0, n+1<<20)
	for len(b) < n {
		switch r.IntN(20) {
		case 0:
			b = append(b, `{"type":"image","source":{"data":"`...)
			for range 1 << (14 + r.IntN(8)) {
				b = append(b, b64[r.IntN(64)])
			}
			b = append(b, "\"}}\n"...)
		case 1:
			b = append(b, `{"text":"export API_KEY=sk-`+strings.Repeat("Ab3x", 10)+` and password: hunter2\n"}`+"\n"...)
		default:
			b = append(b, `{"type":"assistant","text":"`...)
			for k := r.IntN(200); k > 0; k-- {
				b = append(b, words[r.IntN(len(words))]...)
				b = append(b, ' ')
			}
			b = append(b, "\"}\n"...)
		}
	}
	return b[:n]
}
