package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY HARNESS THAT CONTINUES A SESSION STATES HOW ITS FIRST REQUEST IS
// JUDGED: a resumer that states none reads as FirstRequestNone, held whatever
// its brief, which a harness that compresses at its first request turns into
// calls never told again (Gemini CLI and Qwen Code did, on main).
//
// Mutation: delete Gemini CLI's sessionTraits, and its row goes red.
func TestEveryHarnessThatContinuesASessionStatesHowItsFirstRequestIsJudged(t *testing.T) {
	t.Parallel()
	for _, h := range Registered() {
		c, ok := h.(resumer)
		if !ok || !c.Resumes() {
			continue
		}
		if got := SessionTraits(h.Name()).FirstRequest; got == sandbox.FirstRequestNone || !got.Valid() {
			t.Errorf("%s continues a session and states its first request as %q: it is held whatever its "+
				"brief", h.Name(), got)
		}
	}
}

// A GEMINI CLI OR QWEN CODE SESSION IS JUDGED IN ITS BOX: a box's settings can
// move either's point, so only the launch can judge it; the calls of each are
// keyed by their place in the log, which its reading by content asks by
// (qwenholdings.go, geminiholdings.go); a brief is charged a token a byte, at
// or above either's estimate.
//
// Mutation: state FirstRequestWindow, a charge of 4, or no keying for either,
// and a row goes red.
func TestAGeminiOrQwenSessionIsJudgedInItsBox(t *testing.T) {
	t.Parallel()
	for name, keying := range map[string]sandbox.CallKeying{GeminiCLIName: sandbox.CallKeyingByLogPlace,
		QwenCodeName: sandbox.CallKeyingByLogPlace} {
		want := sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: keying, Charge: 1}
		if got := SessionTraits(name); got != want {
			t.Errorf("%s is judged as %+v, want %+v", name, got, want)
		}
	}
}

// A GEMINI CLI SESSION WHOSE TOOL OUTPUT ITS FIRST REQUEST WOULD MASK IS
// MARKED, whatever its size: 50,000 estimated tokens of answers (the least
// that masks is 50,001) is masked, and one fewer is a bound, in a history far
// below the size it collapses at.
//
// Mutation: raise geminiMaskProtect, or compare strictly, and the first row
// goes red; drop the synthetic answers from the sum, and the second does.
func TestAGeminiCountItsAnswersWouldMaskIsMarked(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		answers int64
		calls   map[string]int
		masked  bool
	}{
		"answers at the line":            {answers: 50_000, masked: true},
		"a synthetic answer reaching it": {answers: 49_999, calls: map[string]int{"c": 4}, masked: true},
		"answers just under it":          {answers: 49_999},
	} {
		c := newGeminiCounting()
		c.order, c.quarters, c.bytes = []string{"r"}, map[string]int64{"r": 4}, map[string]int64{"r": 100}
		c.answers["a"] = &geminiAnswer{tokens: tc.answers, carriers: []string{"r"}}
		for id, n := range tc.calls {
			c.calls[id] = &geminiCall{name: n, carriers: []string{"r"}}
		}
		count := c.count(0, 0)
		if masked := count.Mark == sandbox.CountMasked; masked != tc.masked {
			t.Errorf("%s: answers of %d tokens count as %+v, want masked %v", name, tc.answers, count, tc.masked)
		}
	}
}

// geminiFixture is the Gemini CLI count fixture name's chat file, its lines.
func geminiFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "gemini", "0.70.0", "count", name+".chat.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.SplitAfter(chat, []byte("\n"))
}

// geminiChatArchive is a Gemini CLI archive of the one chat file chat.
func geminiChatArchive(t *testing.T, chat []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "chats/session-2026-11-23T06-14-63e9014e.jsonl", Mode: 0o600,
		Size: int64(len(chat)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(chat); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// geminiCountOf is the count of a walk over lines, each ending in its break.
func geminiCountOf(t *testing.T, lines [][]byte) (*geminiCounting, *sandbox.ResumedCount) {
	t.Helper()
	c := newGeminiCounting()
	if err := chatFileLines(t.Context(), bytes.NewReader(bytes.Join(lines, nil)), c, new(passBudget)); err != nil {
		t.Fatal(err)
	}
	return c, c.count(0, 0)
}

// A GEMINI CLI LINE SEPARATOR IS UNREAD AS THE TAKE'S METER SEES IT: the
// meter reads the scrub's output, which writes U+2028 and U+2029 escaped, so
// a session holding them (separators.run0, whose lines Gemini CLI's loader
// split, its calls then answered as interrupted) is unread once scrubbed,
// never a bound.
//
// Mutation: look for the raw characters alone, and this goes red.
func TestAGeminiLineSeparatorIsUnreadAsTheTakesMeterSeesIt(t *testing.T) {
	t.Parallel()
	archive := geminiChatArchive(t, bytes.Join(geminiFixture(t, "separators.run0"), nil))
	var kept bytes.Buffer
	if err := scrubArchive(archiveTar, bytes.NewReader(archive), reopenOf(archive), &kept, newScrubber(nil),
		testBounds); err != nil {
		t.Fatalf("the scrub: %v", err)
	}
	chat := chatMemberOf(t, kept.Bytes(), geminiChatMember)
	if bytes.ContainsAny(chat, "\u2028\u2029") {
		t.Error("the scrub kept a raw line separator")
	}
	escaped := bytes.Count(chat, []byte(`\u2028`)) + bytes.Count(chat, []byte(`\u2029`))
	if escaped == 0 {
		t.Fatal("the scrub kept no escaped line separator, so this proves nothing")
	}
	read, err := GeminiCLI{}.meter(t.Context(), bytes.NewReader(kept.Bytes()), "", geminiRelease, new(passBudget))
	if err != nil || read.count.Mark != sandbox.CountUnread {
		t.Errorf("the scrubbed session counts %+v (%v), want unread", read.count, err)
	}
}

// A GEMINI CLI PATCH THAT ORDERS ITS RECORDS IS UNREAD: what `orderIds` does
// to the history a resume restores was not measured, so the patch Gemini CLI
// wrote (lines 1 and 22 of the round-u2028 capture's second run) is unread,
// never a bound; the same patch without its order is a bound.
//
// Mutation: read past `orderIds`, and the first row goes red; mark any
// patch, and the second does.
func TestAGeminiPatchThatOrdersItsRecordsIsUnread(t *testing.T) {
	t.Parallel()
	lines := geminiFixture(t, "orderids.run1")
	if !bytes.Contains(lines[1], []byte(`"orderIds":[`)) {
		t.Fatal("the fixture's patch orders nothing")
	}
	unordered := [][]byte{lines[0], bytes.Replace(lines[1], []byte(`"orderIds":[`), []byte(`"otherIds":[`), 1)}
	for name, tc := range map[string]struct {
		lines  [][]byte
		unread bool
	}{"ordered": {lines, true}, "unordered": {unordered, false}} {
		_, count := geminiCountOf(t, tc.lines)
		if _, bounded := count.Bound(); (count.Mark == sandbox.CountUnread) != tc.unread || bounded == tc.unread {
			t.Errorf("%s: the patch counts %+v, want unread %v", name, count, tc.unread)
		}
	}
}

// A GEMINI CLI CALL ANSWERED ONLY IN REMOVED RECORDS GETS ITS SYNTHETIC
// ANSWER: a compression keeps a live call as a content copy and removes the
// reply that holds its answer, writing the answer's record only after the
// removal (compressed.run2, lines 33, 34 and 36); a file cut between them
// resumes with the call unanswered, so its count holds the synthetic answer a
// resume hands it, as it would were the call never answered.
//
// Mutation: count an answer whose carriers are all removed as answering its
// call, or keep no call a content part carries, and this goes red.
func TestAGeminiCallAnsweredOnlyInRemovedRecordsGetsItsSyntheticAnswer(t *testing.T) {
	t.Parallel()
	const callID = "mcp_crewlet_echo__mcp_crewlet_echo_1795403268146_0"
	lines := geminiFixture(t, "compressed.run2")
	if !bytes.Contains(lines[32], []byte(`"functionCall"`)) || !bytes.Contains(lines[33], []byte(`"removeIds"`)) ||
		!bytes.Contains(lines[35], []byte(`"functionResponse"`)) {
		t.Fatal("the fixture's compression is not where this test cuts it")
	}
	c, cut := geminiCountOf(t, lines[:35])
	got, ok := cut.Bound()
	if !ok {
		t.Fatalf("the cut counts %+v, no bound", cut)
	}
	// THE SAME CUT WITH THE CALL ANSWERED, at no length, by a kept record.
	c.answers[callID] = &geminiAnswer{carriers: []string{"02a534d0-d7e0-4cbf-97e8-5cda8fc43d6d"}}
	answered, ok := c.count(0, 0).Bound()
	want := int64(len("mcp_crewlet_echo")+geminiSyntheticAnswerBytes+3) / 4
	if !ok || got-answered != want {
		t.Errorf("the cut counts %d, %d past the call answered (%v), want the synthetic answer's %d", got,
			got-answered, ok, want)
	}
}

// chatMemberOf is the one member of the gzipped tar archive that member names.
func chatMemberOf(t *testing.T, archive []byte, member func(string) bool) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("the archive holds no member the harness reads: %v", err)
		}
		if member(h.Name) {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}
	}
}
