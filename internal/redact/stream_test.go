package redact_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
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

// A WINDOW LIVES ONLY FOR THE STEP THAT READS IT: a stage reads its input in
// place, a view of one buffer that later input slides down or replaces, so
// nothing a step keeps may be a view of it. With every stage's buffer replaced
// after each take and the old one filled with bytes no text holds
// ([redact.NewPoisonedStream]), a stream still writes what Secrets does: over
// the corpus, a value of escaped breaks across several windows at each of its
// alignments, and an AWS secret's key whose separator's spaces run to a
// stage's first window's end at every byte of the key, the spaces and past.
//
// Mutation: keep a view of the window as the key a separator's spaces wait on
// (ruleStage.openSeparator's head), and that key reads other bytes.
func TestAWindowLivesOnlyForTheStepThatReadsIt(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 70))
	texts := streamCorpus(r)
	unit := `ab\r\n`
	for i := range len(unit) {
		texts[fmt.Sprintf("a value of escaped breaks, %d bytes in", i)] = "pwd=" + unit[i:] +
			strings.Repeat(unit, 4*redact.StageAdvance/len(unit)) + " \n" + redact.StreamText(r, redact.MaxReach)
	}
	filler := strings.Repeat("the build passed ok\n", 2*redact.StageAdvance/20)
	b64 := strings.Repeat("Ab3+", 10)
	for _, key := range []string{"SessionToken\"", "aws_secret_access_key"} {
		shape := key + "      = " + b64 + " "
		for k := 1; k <= len(shape)-len(b64); k++ {
			texts[fmt.Sprintf("%s, a window's end %d bytes in", key, k)] = filler[:redact.StageAdvance-k] + shape + filler
		}
	}
	// The stages take the same pieces however the text is cut, so one cut
	// of each text is all of them.
	for name, text := range texts {
		var out bytes.Buffer
		s := redact.NewPoisonedStream(&out)
		for at := 0; at < len(text); {
			n := 1 + r.IntN(64<<10)
			_, _ = s.Write([]byte(text[at:min(len(text), at+n)]))
			at += n
		}
		if err := s.Close(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, want := out.String(), redact.Secrets(text); got != want {
			t.Fatalf("%s (%d bytes): %s", name, len(text), streamParted(got, want))
		}
	}
}

// A STREAM ALLOCATES ITS BUFFERS, NOT ITS TEXT: past the buffers its stages
// read from, allocated once a stream, it allocates at most a quarter of a byte
// a byte of the text, measured as what streaming 16 MiB allocates more than
// 4 MiB of the same text in 64 KiB writes: a kept session, one base64 line (a
// PDF's Read), and 200-byte tokens back to back, each a marker, written to a
// writer with no WriteString of its own. Counted from the heap profile, less
// what package regexp allocates ([outsideRegexp]). Not parallel: the profile
// is the process's.
//
// Mutation: hand the stages each piece as a string ([pipeline.feed]), have the
// keys' stage build each piece it writes ([keyStage.write]), or write the end
// through io.WriteString ([endSink]), and a text allocates a byte a byte.
func TestAStreamAllocatesItsBuffersNotItsText(t *testing.T) {
	rate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = rate }()
	allocated := func(text []byte) (uint64, map[string]int64) {
		w := struct{ io.Writer }{io.Discard}
		var err error
		n, by := outsideRegexp(func() {
			s := redact.NewStream(w)
			for at := 0; at < len(text); at += 64 << 10 {
				_, _ = s.Write(text[at:min(len(text), at+64<<10)])
			}
			err = s.Close()
		})
		if err != nil {
			t.Fatal(err)
		}
		return n, by
	}
	for _, c := range []struct {
		name string
		text func(int) []byte
	}{
		{"a kept session", keptSession},
		{"a base64 line", base64Line},
		{"dense tokens", denseTokens},
	} {
		small, large := c.text(4<<20), c.text(16<<20)
		a, byA := allocated(small)
		b, byB := allocated(large)
		perByte := (float64(b) - float64(a)) / float64(len(large)-len(small))
		if perByte > 0.25 {
			top, most := "", int64(0)
			for fn, n := range byB {
				if n-byA[fn] > most {
					top, most = fn, n-byA[fn]
				}
			}
			t.Errorf("%s: streaming %d MiB allocated %d KiB, %d KiB more than %d MiB of it: %.2f bytes a byte, past a quarter; %d KiB more in %s",
				c.name, len(large)>>20, b>>10, (int64(b)-int64(a))>>10, len(small)>>20, perByte, most>>10, top)
		}
		t.Logf("%s: %d KiB for %d MiB, %d KiB for %d MiB: %.3f bytes a byte", c.name, a>>10, len(small)>>20, b>>10, len(large)>>20, perByte)
	}
}

// A STREAM THAT NEVER WAITS ON ITS CLAIMS NEVER ALLOCATES THE HOLD: its keys'
// stage reads from a buffer doubled no further than its own bytes
// ([redact.KeysBytes]), and its stages allocate no more to read from than
// [redact.StreamBuffers] derives, over a kept session of a base take's 256 KiB
// and of 4 MiB, and a base64 line of 4 MiB: the pool of takes is sized on it.
// Not parallel: the heap profile is the process's ([outsideRegexp]).
//
// Mutation: allocate the keys' stage's buffer at its bound, the hold in it
// ([feed.reserve]), and the stream holds and allocates the hold at once.
func TestAStreamThatNeverWaitsOnItsClaimsNeverAllocatesTheHold(t *testing.T) {
	rate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = rate }()
	for _, c := range []struct {
		name string
		text []byte
	}{
		{"a kept session of 256 KiB", keptSession(256 << 10)},
		{"a kept session of 4 MiB", keptSession(4 << 20)},
		{"a base64 line of 4 MiB", base64Line(4 << 20)},
	} {
		var keys int
		allocated, by := outsideRegexp(func() { keys = redact.KeysBufferMost(c.text) })
		buffers := by["github.com/crewlet/crewlet/internal/redact.(*feed).reserve"]
		if keys > redact.KeysBytes {
			t.Errorf("%s: the keys' stage read from a buffer of %d KiB, past its own %d KiB", c.name, keys>>10, redact.KeysBytes>>10)
		}
		if buffers == 0 || buffers > int64(redact.StreamBuffers()) {
			t.Errorf("%s: the stages allocated %d KiB to read from, where %d KiB is derived", c.name, buffers>>10,
				redact.StreamBuffers()>>10)
		}
		t.Logf("%s: the keys' stage read from a buffer of %d KiB; the stages allocated %d KiB to read from of the %d KiB derived, %d KiB in all",
			c.name, keys>>10, buffers>>10, redact.StreamBuffers()>>10, allocated>>10)
	}
}

// outsideRegexp is what f allocates, in bytes, from the heap profile, which
// MemProfileRate 1 makes whole, less what package regexp allocates: the
// matches its searches return, and its machines, which a sync.Pool keeps
// under `go test` and the race detector's pool drops at random, so that under
// `-race` a search allocates a machine now and then. And the rest by the
// function that allocated it.
func outsideRegexp(f func()) (uint64, map[string]int64) {
	profile := func() map[[32]uintptr]int64 {
		runtime.GC()
		runtime.GC()
		var records []runtime.MemProfileRecord
		n, _ := runtime.MemProfile(nil, true)
		for {
			records = make([]runtime.MemProfileRecord, n+64)
			var ok bool
			if n, ok = runtime.MemProfile(records, true); ok {
				break
			}
		}
		sum := map[[32]uintptr]int64{}
		for _, r := range records[:n] {
			sum[r.Stack0] += r.AllocBytes
		}
		return sum
	}
	before := profile()
	f()
	after := profile()
	var total uint64
	by := map[string]int64{}
	for stack, n := range after {
		n -= before[stack]
		if n <= 0 {
			continue
		}
		frames := runtime.CallersFrames(stack[:])
		first, inRegexp := "", false
		for {
			frame, more := frames.Next()
			if first == "" && !strings.HasPrefix(frame.Function, "runtime.") {
				first = frame.Function
			}
			inRegexp = inRegexp || strings.HasPrefix(frame.Function, "regexp.")
			if !more {
				break
			}
		}
		if inRegexp {
			continue
		}
		total += uint64(n)
		by[first] += n
	}
	return total, by
}

// base64Line is n bytes of one line of base64, as a PDF a tool read is kept.
func base64Line(n int) []byte {
	r := rand.New(rand.NewPCG(1, 3))
	const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := append(make([]byte, 0, n), `{"type":"document","source":{"data":"`...)
	for len(b) < n {
		b = append(b, b64[r.IntN(64)])
	}
	return b
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

// BenchmarkStreamAtThePlaintextBound streams 256 MiB through a [redact.Stream]
// in 64 KiB writes, the most the take's scrub reads of an archive at once: the
// plaintext bound of one reading (internal/sandbox/executor's scrubPlainMax),
// which a take's scrub redacts once within SessionScrubWait. Three texts: a
// kept session (JSON lines, credentials, base64 images); a key's armour then
// 200-byte tokens, which the later claims stages step at as markers; a
// password's value every seven bytes. Two runs on two
// cores of the build machine: the kept session at 17.6 MB/s, 15.3 s a reading,
// allocating its 18 MB of buffers; the dense tokens at 6.9 to 7.0 MB/s, 38 to
// 39 s; the dense passwords at 1.9 MB/s, 142 s, within the wait.
// Secrets, which reads a text whole, ran 4.3 times as fast.
func BenchmarkStreamAtThePlaintextBound(b *testing.B) {
	for _, c := range []struct {
		name string
		text func(int) []byte
	}{
		{"kept session", keptSession},
		{"dense tokens", denseTokens},
		{"dense passwords", densePasswords},
	} {
		b.Run(c.name, func(b *testing.B) {
			text := c.text(256 << 20)
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				s := redact.NewStream(io.Discard)
				for at := 0; at < len(text); at += 64 << 10 {
					_, _ = s.Write(text[at:min(len(text), at+64<<10)])
				}
				_ = s.Close()
			}
			b.ReportMetric(float64(b.Elapsed())/float64(b.N)/float64(time.Second), "s/reading")
		})
	}
}

// denseTokens is n bytes of a key's armour and then 200-byte tokens back to
// back, a space apart.
func denseTokens(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	b := make([]byte, 0, n+256)
	b = append(b, "a key: -----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n\n"...)
	for len(b) < n {
		b = append(b, "sk-"...)
		for range 197 {
			b = append(b, b64url[r.IntN(64)])
		}
		b = append(b, ' ')
	}
	return b[:n]
}

// densePasswords is n bytes of a file's `pwd=x` lines as a JSON string holds
// them, joined by escaped breaks: a password's value every seven bytes.
func densePasswords(n int) []byte {
	return []byte(strings.Repeat(`pwd=x\n`, n/7+1)[:n])
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

// AN ORDINARY TEXT NEVER DECIDES EARLY at the stream's own hold: no window of
// the package's keys in every wrapping set is decided before the claims over
// it are known ([redact.EarlyWindows]), among texts of every shape a stage
// carries ([redact.StreamText]), keys' armours among them, tokens back to
// back after an armour (each longer than [redact.EarlyReach], the least text
// in which a window can be decided early), or a kept session. So the early
// path runs only on texts built against the password's rule, an AWS secret's
// quoted key or a Google token's dots, and only those can be refused.
//
// Mutation: hold less than the claims' lag ([holdBytes]), or decide a window
// with an armour in it early once the text has run the hold past it, whatever
// the claims' frontier says ([keyStage.step]).
func TestAnOrdinaryTextNeverDecidesEarly(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 5))
	texts := streamCorpus(r)
	for i := range 3 {
		texts[fmt.Sprintf("shapes %d", i)] = redact.StreamText(r, 3*redact.MaxReach+r.IntN(8*redact.MaxReach))
	}
	texts["a kept session"] = string(keptSession(1 << 20))
	// Past the least text that can decide a window early, so a hold or a
	// trigger that decides one here is seen.
	long := map[string]string{
		"an armour and every shape, past the hold":         "-----BEGIN RSA PRIVATE KEY-----\n" + redact.StreamText(r, 8<<20),
		"an armour and tokens back to back, past the hold": string(denseTokens(8 << 20)),
	}
	for name, text := range long {
		if len(text) <= redact.EarlyReach() {
			t.Errorf("%s (%d KiB): no longer than the %d KiB in which a window can be decided early", name, len(text)>>10, redact.EarlyReach()>>10)
		}
		texts[name] = text
	}
	for name, text := range texts {
		if n, err := redact.EarlyWindows(text); n > 0 || err != nil {
			t.Errorf("%s (%d KiB): %d windows decided early, Close answered %v", name, len(text)>>10, n, err)
		}
	}
}

// A STREAM WRITES WHAT SECRETS WRITES ON THE CORPUS TEXT THAT CRASHED IT: in
// the fourth 8 MiB text of this seed the keys' stage places a password key's
// value one past its window, inside a key's block, and claims.readsAValue
// sliced past the window: Write panicked in every piecing, while Secrets of
// the text did not. TestAKeyWhoseBlanksRunToTheEndNamesNoValue holds the least
// such text; this one holds the shape as the generator writes it, in 64 KiB
// writes.
//
// Mutation: drop readsAValue's guard of a start past the text, and Write panics.
func TestAStreamWritesWhatSecretsWritesOnTheTextThatCrashedIt(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 110))
	var text string
	for range 4 {
		text = redact.StreamText(r, 8<<20)
	}
	// The span around that window: the whole text streams in 44 s under the
	// race detector, this in 6, most of it the generator's.
	text = text[4_730_880:5_398_528]
	var out bytes.Buffer
	s := redact.NewStream(&out)
	for piece := range slices.Chunk([]byte(text), 64<<10) {
		if _, err := s.Write(piece); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if want := redact.Secrets(text); out.String() != want {
		t.Fatalf("the stream (%d bytes) wrote %s", len(text), streamParted(out.String(), want))
	}
}
