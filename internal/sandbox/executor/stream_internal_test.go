package executor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A FAILURE IS HELD TO ITS BOUND EXACTLY, as it leaves — redacted whole. Two
// things grew a failure composed to the bound past it: the mark KeepEnd puts on
// a single line it keeps only the end of, which is outside the bytes it
// counts, and a credential's name closing one piece meeting the next across
// the `:\n` between them, one match neither piece held when each was redacted
// and measured alone. The slack between the unread note's widest form and the
// one it gets usually hid both; an unread start whose note is as wide as the
// widest takes the slack away.
//
// Mutation: return the first composition without measuring it redacted, and
// the key-name case is fifteen bytes past the bound; leave the mark out of the
// room as well, and the single-line case is three past it.
//
// At a bound of 64 KiB with a line half again past it, as the three
// mebibytes this was are of the real one: what grows a composition past its
// bound is a marker and a join, which are the same bytes at any bound.
func TestAFailureIsHeldToItsBoundAsItLeaves(t *testing.T) {
	t.Parallel()
	// Thirteen digits of MiB, the width of the widest note.
	const unread = int64(2e18)
	const bound = 64 << 10
	oneLine := strings.Repeat("e", bound*3/2)
	for name, sentences := range map[string][]string{
		"a single line kept by its end":     {"the harness exited with status 1"},
		"a key name meeting the next piece": {"the harness exited with status 1", "the harness could not read pwd"},
	} {
		got := failureDetail(sentences, oneLine, unread, bound)
		if len(got) > bound {
			t.Errorf("%s: the failure is %d bytes, %d past its bound", name, len(got), len(got)-bound)
		}
		if redact.Secrets(got) != got {
			t.Errorf("%s: the failure was not redacted whole, so redacting it again would grow it", name)
		}
		if !strings.HasPrefix(got, "the harness exited with status 1") || !strings.HasSuffix(got, "eee") {
			t.Errorf("%s: the failure lost its opening or the stream's end", name)
		}
	}
}

// A KEY THAT BEGAN BEFORE THE WINDOW IS STILL REDACTED. A private key is the
// one credential shape that spans lines, and a window opening inside its block
// shows a base64 body with no BEGIN line for the rule to anchor on — so the
// end of a stream is read with context before what it keeps, and redacted
// over all of it, before anything is dropped.
func TestAKeyThatBeganBeforeTheWindowIsStillRedacted(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n", 40)
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "-----END RSA PRIVATE KEY-----\n"
	keep := 1024
	// The key's BEGIN sits before the last `keep` bytes, its body and END
	// inside them.
	stream := strings.Repeat("noise\n", 2000) + key + "done\n"
	data := []byte(stream)
	window := keep + redactContext
	tail := sandbox.FileTail{Data: data[max(0, len(data)-window):], Size: int64(len(data))}

	got, unread := streamEnd(tail, keep)
	if strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Errorf("a key body reached the window unredacted: %q", got)
	}
	if !strings.HasSuffix(got, "done\n") || unread <= 0 {
		t.Errorf("the window = …%q with %d unread; want the stream's end and its start counted",
			got[max(0, len(got)-40):], unread)
	}
	if redact.Contains(got) {
		t.Error("the window still holds a credential shape")
	}
}

// THE WINDOW STARTS ON A LINE, past the context a read that began inside the
// stream opens with, and everything before it is counted; a line longer than
// the window keeps its own end on a character, marked — and one a read began
// inside keeps its end past the context too.
func TestTheEndOfAStreamIsWholeLines(t *testing.T) {
	t.Parallel()
	const keep = 1 << 10
	const line = "a line the process wrote\n"
	stream := strings.Repeat(line, (keep+redactContext)/len(line)+100)
	read := keep + redactContext
	if (len(stream)-read)%len(line) == 0 {
		t.Fatal("the read opens on a line; the case needs one that opens inside a line, as a byte count lands")
	}
	tail := sandbox.FileTail{Data: []byte(stream[len(stream)-read:]), Size: int64(len(stream))}
	got, unread := streamEnd(tail, keep)
	if got == "" || len(got) > keep || !strings.HasPrefix(got, line) || !strings.HasSuffix(stream, got) ||
		unread != int64(len(stream)-len(got)) {
		t.Errorf("streamEnd = %d bytes opening %.30q, %d unread; want whole lines of the stream's end, at "+
			"most %d bytes, and every byte before them counted", len(got), got, unread, keep)
	}

	long := []byte(strings.Repeat("日", 2000))
	got, _ = streamEnd(sandbox.FileTail{Data: long, Size: int64(len(long))}, 100)
	kept, marked := strings.CutPrefix(got, "…")
	if !marked || len(kept) > 100 || !utf8.ValidString(kept) {
		t.Errorf("a line longer than the window = %q; want its end, marked, on a character", got)
	}

	one := strings.Repeat("x", 3*redactContext)
	tail = sandbox.FileTail{Data: []byte(one[len(one)-read:]), Size: int64(len(one))}
	got, unread = streamEnd(tail, keep)
	kept, marked = strings.CutPrefix(got, "…")
	if !marked || len(kept) != keep || unread != int64(len(one)-keep) {
		t.Errorf("a line a read began inside = %d bytes, marked %v, %d unread; want its last %d bytes, "+
			"marked, and the rest counted", len(kept), marked, unread, keep)
	}
}

// A KEY THAT BEGAN BEFORE THE READ IS NEVER SHOWN, however much the redaction
// shrinks what comes after it. Read from inside, its BEGIN not in the text, a
// key is redacted only as far back as a read from its END reaches, and three
// lines in a row that are not the key's stop that read: the lines above them
// are published (package redact's doc). The context a read takes before what
// it keeps is there to read such a key whole; but bounded by the redacted
// text's bytes, what was kept reached back into the context whenever the keys
// after it collapsed into their markers, and brought the published lines.
//
// Mutation: keep the end of the whole read's redaction (sandbox.KeepEnd over
// redact.Secrets of every whole line read), and the key's body is shown.
func TestAKeyThatBeganBeforeTheReadIsNeverShown(t *testing.T) {
	t.Parallel()
	const body = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	const other = "MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGy0AHB7MaV0gM9g7x3E8Mq1"
	begin, end := "-----BEGIN RSA PRIVATE KEY-----\n", "-----END RSA PRIVATE KEY-----\n"
	var s strings.Builder
	s.WriteString(strings.Repeat("compiling a package of the build\n", 50))
	s.WriteString(begin)
	opened := s.Len()
	s.WriteString(strings.Repeat(body+"\n", 40))
	s.WriteString(strings.Repeat("retrying the upload in two seconds\n", 3))
	s.WriteString(strings.Repeat(body+"\n", 5) + end)
	// Keys enough after it to fill the context, each a twenty-two byte
	// marker once redacted.
	for range 45 {
		s.WriteString(begin + strings.Repeat(other+"\n", 25) + end)
	}
	s.WriteString("done\n")
	stream := s.String()
	// The read opens inside the key's body, ten lines and a few bytes in,
	// and keeps what is past the context.
	data := []byte(stream[opened+10*(len(body)+1)+7:])
	keep := len(data) - redactContext
	if keep <= 0 {
		t.Fatalf("the read is %d bytes; the case needs more than the %d of context", len(data), redactContext)
	}
	got, unread := streamEnd(sandbox.FileTail{Data: data, Size: int64(len(stream))}, keep)
	if strings.Contains(got, body[:24]) || strings.Contains(got, other[:24]) || redact.Contains(got) {
		t.Errorf("a key's body reached what is shown:\n%s", got)
	}
	if !strings.HasSuffix(got, "done\n") || unread <= int64(len(stream)-len(data)) {
		t.Errorf("shown = …%q with %d unread; want the stream's end, and its start and the context counted",
			got[max(0, len(got)-40):], unread)
	}
}

// ONE LINE IS HELD AT MOST: a line past the bound is counted as it passes and
// never kept, and the lines around it are read as they were.
//
// At a bound of 64 KiB, which is larger than the reader's own 64 KiB buffer
// — so a line at it still arrives in more than one piece, the case the
// counting is for — and a five-hundredth of [maxLineBytes].
func TestALineIsReadInOnePieceUpToTheBound(t *testing.T) {
	t.Parallel()
	const bound = 64<<10 + 1
	var seen []string
	var skipped []int64
	var bounds []int
	dec := &recorder{line: func(l []byte) { seen = append(seen, string(l)) },
		skip: func(n int64, at int) { skipped, bounds = append(skipped, n), append(bounds, at) }}
	huge := strings.Repeat("x", bound+5)
	whole := strings.Repeat("y", bound-1)
	if err := eachLine(strings.NewReader("a\r\n"+huge+"\n"+whole+"\nb\nlast"), dec, bound); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, "|") != "a|"+whole+"|b|last" {
		t.Errorf("read %d lines; want the lines around the long one, one exactly at the bound, "+
			"and the unterminated last", len(seen))
	}
	if len(skipped) != 1 || skipped[0] != int64(len(huge)+1) || bounds[0] != bound {
		t.Errorf("skipped = %v past %v; want one line of %d bytes past %d", skipped, bounds, len(huge)+1, bound)
	}
}

// A RUNNER READS TO THE ENGINE'S OWN BOUNDS: a failure to what one
// condensation can take beside the coordinator's prefix, and a line to what
// a whole read takes. The cases that go past either run at a bound of their
// own ([Bounded] in export_test.go), so this is what ties the bounds they skip
// building to the ones every runner New makes reads to.
func TestARunnerReadsToTheEnginesOwnBounds(t *testing.T) {
	t.Parallel()
	if maxLineBytes != sandbox.MaxFileBytes {
		t.Errorf("maxLineBytes = %d, want sandbox.MaxFileBytes (%d)", maxLineBytes, sandbox.MaxFileBytes)
	}
	for _, r := range []*Runner{NewClaudeCode(), NewOpenCode()} {
		if r.failureBound != sandbox.MaxFailureBytes || r.lineBound != maxLineBytes {
			t.Errorf("%s reads a failure to %d and a line to %d; want sandbox.MaxFailureBytes (%d) "+
				"and maxLineBytes (%d)", r.Name(), r.failureBound, r.lineBound,
				sandbox.MaxFailureBytes, maxLineBytes)
		}
		if f, ok := r.Follow(sandbox.RunHandle{}).(*follower); !ok || f.lineBound != r.lineBound {
			t.Errorf("%s's live reading does not read lines to its runner's bound", r.Name())
		}
	}
}

type recorder struct {
	line func([]byte)
	skip func(int64, int)
}

func (r *recorder) Line(l []byte)              { r.line(l) }
func (r *recorder) Skipped(n int64, bound int) { r.skip(n, bound) }
func (r *recorder) Result() sandbox.Result     { return sandbox.Result{} }
func (r *recorder) Entries() []string          { return nil }
