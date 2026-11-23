package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// chatCountArchive is the count fixture name of harness ("gemini" or "qwen")
// packed as its take packs it: a gzipped tar of its one chat file under the
// member name its harness's archive names it by.
func chatCountArchive(t *testing.T, harness, name string) []byte {
	t.Helper()
	dir := map[string]string{"gemini": "gemini/0.70.0", "qwen": "qwen/0.28.4"}[harness]
	f, err := os.Open(filepath.Join("testdata", dir, "count", name+".chat.jsonl.gz"))
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
	member := map[string]string{"gemini": "chats/session-2026-11-23T06-14-63e9014e.jsonl",
		"qwen": "projects/-work/chats/0b6f3c2e-0000-4000-8000-000000000001.jsonl"}[harness]
	return tarGz(t, member, chat)
}

// tarGz is a gzipped tar of one regular member, name, holding body.
func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)),
		Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
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

// chatCount is the count harness's take meters fixture name at.
func chatCount(t *testing.T, harness, name string) *sandbox.ResumedCount {
	t.Helper()
	h, release := executor.Harness(executor.GeminiCLI{}), "0.70.0"
	if harness == "qwen" {
		h, release = executor.QwenCode{}, "0.28.4"
	}
	return executor.MeterCount(h, release, bytes.NewReader(chatCountArchive(t, harness, name)))
}

// bound is count's bound, failing where there is none.
func bound(t *testing.T, name string, count *sandbox.ResumedCount) int64 {
	t.Helper()
	n, ok := count.Bound()
	if !ok {
		t.Fatalf("%s: the count is %+v, no bound", name, count)
	}
	return n
}

// A GEMINI CLI COUNT IS NEVER BELOW WHAT ITS RESUME HANDS: each kept chat
// file's bound is at least Gemini CLI 0.70.0's own estimate of the history its
// resume restores (its loader, converter and estimator run on the file,
// MEASURE-P2's oracle) beside the session context the file's resume held
// (thought's 193 and g3as's 104,858 are the figures Gemini CLI itself
// reported and compressed at): calls with their ids, thoughts, the largest
// copy of each record and answer, and the synthetic answers a resume adds.
//
// Mutations, each red: count a call without its id; drop the thoughts; keep a
// record's first copy, not its largest; drop the synthetic answers.
func TestAGeminiCountIsNeverBelowWhatItsResumeHands(t *testing.T) {
	t.Parallel()
	for name, estimate := range map[string]int64{
		"round.run1": 20915, "kill-mid-call.run0": 291, "kill-mid-call.run1": 315,
		"kill-mid-answer.run1": 293, "compressed.run1": 255, "compressed.run2": 261, "agent-denied.run0": 236,
		"thought": 193, "first-request-compressed.run0": 570114, "g3as": 104858,
	} {
		if got := bound(t, name, chatCount(t, "gemini", name)); got < estimate {
			t.Errorf("%s: the count bounds the session at %d, below the %d Gemini CLI estimates its "+
				"restored history at", name, got, estimate)
		}
	}
}

// A GEMINI CLI SESSION THAT COMPRESSED AT ITS FIRST REQUEST COUNTS PAST ITS
// POINT: the sessions measured compressing at half the window (a session
// past it whole, and one at 524,288 with its context) are never judged whole
// in a box at that point, and one whose replies recorded 600,000 input tokens
// but whose history is small is: the count is the history's estimate, never a
// recorded figure.
//
// Mutation: read the replies' recorded tokens.input, or drop a record's text,
// and a row goes red.
func TestAGeminiSessionThatCompressedAtItsFirstRequestCountsPastItsPoint(t *testing.T) {
	t.Parallel()
	point := sandbox.BoxPoint{Tokens: executor.GeminiPoint("gemini-2.5-pro"),
		BoxBytes: executor.GeminiContextBase, Charge: 1}
	for name, whole := range map[string]bool{
		"first-request-compressed.run0": false, "g2hi": false, "compressed.run1": true,
	} {
		count := chatCount(t, "gemini", name)
		if got := point.SendsWhole(count, "", "", "Continue."); got != whole {
			t.Errorf("%s: counted %+v, sends whole at %d: %v, want %v", name, count, point.Tokens, got, whole)
		}
	}
}

// A GEMINI CLI SESSION ITS FIRST REQUEST WOULD MASK OR COLLAPSE IS MARKED:
// answers past the 50,000 tokens masking protects (m4a, measured masking
// four), a history whose bytes with the session context the count allows for
// reach the 256 KiB it collapses at (c5hi, measured collapsing six), each
// CountMasked, never a bound; a history well below both is a bound.
//
// Mutation: drop the collapse's bytes, or the context's allowance, and c5hi
// goes red; mark nothing, and m4a does.
func TestAGeminiSessionItsFirstRequestWouldMaskIsMarked(t *testing.T) {
	t.Parallel()
	for name, masked := range map[string]bool{"m4a": true, "c5hi": true, "round.run1": false} {
		count := chatCount(t, "gemini", name)
		if got := count.Mark == sandbox.CountMasked; got != masked {
			t.Errorf("%s: counted %+v, want masked %v", name, count, masked)
		}
	}
}

// A QWEN CODE COUNT IS ITS NEWEST REPLY'S USAGE AND ALL AFTER: the newest
// reply whose prompt is above zero (the larger of its total and its three
// parts), or a newer compression's size; every message after it a token a
// byte; a failure for each call no answer followed; so with the brief and the
// dated reminder it is at least what 0.28.4 compared with its point (each
// figure measured: the boundary that compressed, a total above the parts, a
// zeroed newest reply passed over, a compression with no reply after it).
// Exact here, as the walk over each fixture gives it.
//
// Mutations, each red: read the parts and not the total; anchor on a zeroed
// reply; drop the later bytes or the unanswered calls' failures.
func TestAQwenCountIsItsNewestReplysUsageAndAllAfter(t *testing.T) {
	t.Parallel()
	const brief = "Continue the task."
	for name, tc := range map[string]struct{ bound, compared int64 }{
		"boundary-at":         {167001, 167000},
		"usage-total-over":    {168099, 168098},
		"usage-zero-newest":   {170338, 170098},
		"compressed-no-reply": {169199, 169199 + 48},
		"killed-midbatch":     {1552, 1051},
		"compacted":           {1099, 1053},
	} {
		got := bound(t, name, chatCount(t, "qwen", name))
		if got != tc.bound {
			t.Errorf("%s: the count bounds the session at %d, want %d", name, got, tc.bound)
		}
		if got+int64(len(brief))+executor.QwenReminderBytes < tc.compared {
			t.Errorf("%s: the count %d with its brief and reminder is below the %d Qwen Code compared", name,
				got, tc.compared)
		}
	}
	point := sandbox.BoxPoint{Tokens: executor.QwenPoint("stub-model"), BoxBytes: executor.QwenReminderBytes,
		Charge: 1}
	if point.SendsWhole(chatCount(t, "qwen", "boundary-at"), "", "", brief) {
		t.Errorf("the session measured compressing at its first request is judged whole at %d", point.Tokens)
	}
	for name, media := range map[string]bool{"images-19": false, "images-20": true} {
		count := chatCount(t, "qwen", name)
		if got := count.Mark == sandbox.CountMedia; got != media {
			t.Errorf("%s: counted %+v, want marked for its images %v", name, count, media)
		}
	}
}

// THE COUNT NAMES THE NEWEST RESTART: a Gemini CLI session's newest summary
// record a removal left, a Qwen Code session's newest compression.
//
// Mutation: take the oldest summary, or one a removal dropped, and the first
// row goes red.
func TestTheCountNamesTheNewestRestart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ harness, name, restart string }{
		{"gemini", "compressed.run2", "d81356db-2799-4cfd-a2f0-ddbab699a36d"},
		{"gemini", "round.run1", ""},
		{"qwen", "compacted", "e3a937d3-ab5f-48d1-aeb8-747bc9f249fd"},
		{"qwen", "compressed-no-reply", "e4f9b8f1-3bee-405d-907e-909d6563e200"},
		{"qwen", "boundary-at", ""},
	} {
		if got, read := chatCount(t, tc.harness, tc.name).Restarted(); !read || got != tc.restart {
			t.Errorf("%s %s: restarts at %q (read %v), want %q", tc.harness, tc.name, got, read, tc.restart)
		}
	}
}

// A CHAT COUNT IS READ ONLY WHOLE AND ONLY FOR ITS RELEASE: a line holding a
// line separator (U+2028, which Gemini CLI's loader splits at) is unread, as is
// an archive of another release, never a bound.
//
// Mutation: read the separators' line, or count any release, and a row goes
// red.
func TestAChatCountIsReadOnlyWholeAndOnlyForItsRelease(t *testing.T) {
	t.Parallel()
	if got := chatCount(t, "gemini", "separators.run0"); got.Mark != sandbox.CountUnread {
		t.Errorf("a session holding a line separator counts %+v, want unread", got)
	}
	for _, h := range []executor.Harness{executor.GeminiCLI{}, executor.QwenCode{}} {
		harness := map[string]string{executor.GeminiCLIName: "gemini", executor.QwenCodeName: "qwen"}[h.Name()]
		name := map[string]string{"gemini": "round.run1", "qwen": "boundary-at"}[harness]
		got := executor.MeterCount(h, "9.9.9", bytes.NewReader(chatCountArchive(t, harness, name)))
		if got.Mark != sandbox.CountRelease {
			t.Errorf("%s written by 9.9.9 counts %+v, want its release's mark", h.Name(), got)
		}
	}
}

// THE CHAT POINT IS THE THRESHOLD OF ITS WINDOW: Gemini CLI 0.70.0 at half its
// table's window (Gemma 4's 256,000, 1,048,576 for any other); Qwen Code 0.28.4
// at 85% of its window held 33,000 below it (167,000 at its default 200,000,
// 98,072 for deepseek-chat, 850,000 for qwen3-coder-plus, all measured), its
// model normalized as it normalizes one, and no model at its table's least.
//
// Mutation: take the default window for a model the table names, drop the
// reserve, or skip the normalization, and a row goes red.
func TestTheChatPointIsTheThresholdOfItsWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		point func(string) int64
		model string
		want  int64
	}{
		{executor.GeminiPoint, "gemma-4-31b-it", 128_000},
		{executor.GeminiPoint, "gemini-2.5-pro", 524_288},
		{executor.QwenPoint, "stub-model", 167_000},
		{executor.QwenPoint, "deepseek-chat", 98_072},
		{executor.QwenPoint, "qwen3-coder-plus", 850_000},
		{executor.QwenPoint, "openrouter/qwen/qwen3-coder-plus:free", 850_000},
		{executor.QwenPoint, "kimi-k3-256k", 222_823},
		{executor.QwenPoint, "kimi-k3", 850_000},
		{executor.QwenPoint, "", 98_072},
	} {
		if got := tc.point(tc.model); got != tc.want {
			t.Errorf("model %q: the point is %d, want %d", tc.model, got, tc.want)
		}
	}
}
