package codingagent

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
// the key-name case is ten bytes past the bound; leave the mark out of the
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
		"a single line kept by its end":     {"the coding agent exited with status 1"},
		"a key name meeting the next piece": {"the coding agent exited with status 1", "the CLI could not read pwd"},
	} {
		got := failureDetail(sentences, oneLine, unread, bound)
		if len(got) > bound {
			t.Errorf("%s: the failure is %d bytes, %d past its bound", name, len(got), len(got)-bound)
		}
		if redact.Secrets(got) != got {
			t.Errorf("%s: the failure was not redacted whole, so redacting it again would grow it", name)
		}
		if !strings.HasPrefix(got, "the coding agent exited with status 1") || !strings.HasSuffix(got, "eee") {
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

// THE WINDOW STARTS ON A LINE, and a line longer than the window keeps its own
// end on a character, marked.
func TestTheEndOfAStreamIsWholeLines(t *testing.T) {
	t.Parallel()
	stream := []byte("first line\nsecond line\nthird line\n")
	tail := sandbox.FileTail{Data: stream[5:], Size: int64(len(stream))}
	got, unread := streamEnd(tail, 1<<10)
	if got != "second line\nthird line\n" || unread != int64(len("first line\n")) {
		t.Errorf("streamEnd = %q, %d unread; want the partial first line dropped and counted", got, unread)
	}

	long := []byte(strings.Repeat("日", 2000))
	got, _ = streamEnd(sandbox.FileTail{Data: long, Size: int64(len(long))}, 100)
	kept, marked := strings.CutPrefix(got, "…")
	if !marked || len(kept) > 100 || !utf8.ValidString(kept) {
		t.Errorf("a line longer than the window = %q; want its end, marked, on a character", got)
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
