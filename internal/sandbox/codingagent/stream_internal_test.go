package codingagent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE ERROR STREAM IS READ TO WHAT A CONDENSATION CAN TAKE, and that figure is
// compaction's own: a read past it is text no model could be shown, and a
// bound written down twice is a bound that drifts.
func TestTheErrorStreamIsReadToWhatACondensationCanTake(t *testing.T) {
	t.Parallel()
	if want := compact.MaxChunks * compact.ChunkBytes; stderrKeep != want {
		t.Errorf("stderrKeep = %d; want compact.MaxChunks × compact.ChunkBytes = %d", stderrKeep, want)
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
func TestALineIsReadInOnePieceUpToTheBound(t *testing.T) {
	t.Parallel()
	var seen []string
	var skipped []int64
	dec := &recorder{line: func(l []byte) { seen = append(seen, string(l)) },
		skip: func(n int64) { skipped = append(skipped, n) }}
	huge := strings.Repeat("x", maxLineBytes+5)
	if err := eachLine(strings.NewReader("a\r\n"+huge+"\nb\nlast"), dec); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, "|") != "a|b|last" {
		t.Errorf("lines = %q; want the lines around the long one, and the unterminated last", seen)
	}
	if len(skipped) != 1 || skipped[0] != int64(len(huge)+1) {
		t.Errorf("skipped = %v; want one line of %d bytes", skipped, len(huge)+1)
	}
}

type recorder struct {
	line func([]byte)
	skip func(int64)
}

func (r *recorder) Line(l []byte)          { r.line(l) }
func (r *recorder) Skipped(n int64)        { r.skip(n) }
func (r *recorder) Result() sandbox.Result { return sandbox.Result{} }
