package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// chatCountArchive is the count fixture name of harness ("gemini" or "qwen")
// packed as its take packs it: a gzipped tar of its one chat file under the
// member name its harness's archive names it by.
func chatCountArchive(t *testing.T, harness, name string) []byte {
	t.Helper()
	member := map[string]string{"gemini": "chats/session-2026-11-23T06-14-63e9014e.jsonl",
		"qwen": "projects/-work/chats/0b6f3c2e-0000-4000-8000-000000000001.jsonl"}[harness]
	return tarGz(t, member, chatCountFixture(t, harness, name))
}

// chatCountFixture is the count fixture name of harness's chat file.
func chatCountFixture(t *testing.T, harness, name string) []byte {
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
	return chat
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
// four; masked.run0, whose resume masked 70 of 75 outputs), a history whose
// bytes with the session context the count allows for reach the 256 KiB it
// collapses at (c5hi, measured collapsing six), each CountMasked, never a
// bound; a history well below both is a bound.
//
// Mutation: drop the collapse's bytes, or the context's allowance, and c5hi
// goes red; mark nothing, and m4a does.
func TestAGeminiSessionItsFirstRequestWouldMaskIsMarked(t *testing.T) {
	t.Parallel()
	for name, masked := range map[string]bool{"m4a": true, "c5hi": true, "masked.run0": true, "round.run1": false} {
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

// qwenRecord is one Qwen Code 0.28.4 chat file record of kind (its type) and
// fields beside it, as the count reads one.
func qwenRecord(t *testing.T, kind string, fields map[string]any) string {
	t.Helper()
	r := map[string]any{"uuid": "u-" + kind, "parentUuid": nil, "sessionId": "s", "timestamp": "t", "type": kind}
	for k, v := range fields {
		r[k] = v
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// qwenAnswer is a tool answer to call id of tool name holding output, as a
// message's part.
func qwenAnswer(id, name, output string) map[string]any {
	return map[string]any{"functionResponse": map[string]any{"id": id, "name": name,
		"response": map[string]any{"output": output}}}
}

// qwenAnswerSize is the bytes qwenAnswer's part's functionResponse takes in a
// line, as the count adds them.
func qwenAnswerSize(t *testing.T, id, name, output string) int64 {
	t.Helper()
	raw, err := json.Marshal(qwenAnswer(id, name, output)["functionResponse"])
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(raw))
}

// qwenSession is a Qwen Code chat file of a brief, a call and an answer of
// name for each of outputs, and a reply recording 150,000 prompt tokens,
// then more.
func qwenSession(t *testing.T, name string, outputs []string, more ...string) []byte {
	t.Helper()
	lines := []string{qwenRecord(t, "user", map[string]any{"message": map[string]any{"role": "user",
		"parts": []any{map[string]any{"text": "Run the probe round."}}}})}
	for i, out := range outputs {
		id := fmt.Sprintf("call_%d", i)
		lines = append(lines,
			qwenRecord(t, "assistant", map[string]any{"message": map[string]any{"role": "model",
				"parts": []any{map[string]any{"functionCall": map[string]any{"id": id, "name": name}}}}}),
			qwenRecord(t, "tool_result", map[string]any{"message": map[string]any{"role": "user",
				"parts": []any{qwenAnswer(id, name, out)}}}))
	}
	lines = append(lines, qwenRecord(t, "assistant", map[string]any{
		"message":       map[string]any{"role": "model", "parts": []any{map[string]any{"text": "Done."}}},
		"usageMetadata": map[string]any{"promptTokenCount": 150_000, "candidatesTokenCount": 50, "totalTokenCount": 150_050}}))
	return []byte(strings.Join(append(lines, more...), "\n") + "\n")
}

// qwenSessionCount is the count Qwen Code 0.28.4's take meters chat at.
func qwenSessionCount(t *testing.T, chat []byte) *sandbox.ResumedCount {
	t.Helper()
	member := "projects/-work/chats/0b6f3c2e-0000-4000-8000-000000000001.jsonl"
	return executor.MeterCount(executor.QwenCode{}, "0.28.4", bytes.NewReader(tarGz(t, member, chat)))
}

// A QWEN CODE SESSION ITS FIRST REQUEST WOULD CLEAR IS MARKED: tool answers
// since the restart whose bytes, with a failure for each call left unanswered,
// reach the 500,000 characters past which 0.28.4 clears the oldest (25 shell
// answers of 24,508 measured clearing 15, MEASURE-P2 Q6.2) are CountMasked,
// never a bound, an MCP tool's answers (their clearing not measured) and those
// a compression kept in its history among them; a byte below is a bound, as
// are answers a compression dropped. Sessions are built to measured sizes.
//
// Mutations, each red: mark past the size, not at it; leave MCP answers out;
// drop a compression's history's answers, or keep the ones before it; drop the
// unanswered calls' failures.
func TestAQwenSessionItsFirstRequestWouldClearIsMarked(t *testing.T) {
	t.Parallel()
	const shell, mcp = "run_shell_command", "mcp__crewlet__shell"
	measured := make([]string, 25)
	for i := range measured {
		measured[i] = strings.Repeat("x", 24_508)
	}
	// TO THE BYTE: nineteen measured answers, the twentieth sized so the
	// answers total size.
	upTo := func(size int64) []string {
		outs := slices.Clone(measured[:19])
		var total int64
		for i, out := range outs {
			total += qwenAnswerSize(t, fmt.Sprintf("call_%d", i), shell, out)
		}
		last := size - total - qwenAnswerSize(t, "call_19", shell, "")
		return append(outs, strings.Repeat("x", int(last)))
	}
	unanswered := qwenRecord(t, "assistant", map[string]any{"message": map[string]any{"role": "model",
		"parts": []any{map[string]any{"functionCall": map[string]any{"id": "open", "name": shell}}}}})
	history := make([]any, 25)
	for i := range history {
		history[i] = map[string]any{"role": "user", "parts": []any{qwenAnswer(fmt.Sprintf("h%d", i), shell, measured[i])}}
	}
	compression := func(history []any) string {
		return qwenRecord(t, "system", map[string]any{"subtype": "chat_compression", "systemPayload": map[string]any{
			"info": map[string]any{"newTokenCount": 100_000}, "compressedHistory": history}})
	}
	// Thirteen MCP answers after twelve shell ones, which alone stay below.
	var mcpRounds []string
	for i, out := range measured[12:] {
		id := fmt.Sprintf("mcp_%d", i)
		mcpRounds = append(mcpRounds,
			qwenRecord(t, "assistant", map[string]any{"message": map[string]any{"role": "model",
				"parts": []any{map[string]any{"functionCall": map[string]any{"id": id, "name": mcp}}}}}),
			qwenRecord(t, "tool_result", map[string]any{"message": map[string]any{"role": "user",
				"parts": []any{qwenAnswer(id, mcp, out)}}}))
	}
	for _, tc := range []struct {
		name   string
		chat   []byte
		masked bool
	}{
		{"the measured shape", qwenSession(t, shell, measured), true},
		{"the measured shape as MCP answers", qwenSession(t, mcp, measured), true},
		{"the measured shape, thirteen of it MCP answers", qwenSession(t, shell, measured[:12], mcpRounds...), true},
		{"at the size", qwenSession(t, shell, upTo(executor.QwenClearsAt)), true},
		{"a byte below it", qwenSession(t, shell, upTo(executor.QwenClearsAt-1)), false},
		{"an unanswered call's failure reaching it",
			qwenSession(t, shell, upTo(executor.QwenClearsAt-executor.QwenSyntheticFailureBytes), unanswered), true},
		{"answers a compression dropped", qwenSession(t, shell, measured, compression([]any{})), false},
		{"answers a compression kept", qwenSession(t, shell, nil, compression(history)), true},
	} {
		count := qwenSessionCount(t, tc.chat)
		if got := count.Mark == sandbox.CountMasked; got != tc.masked {
			t.Errorf("%s: counted %+v, want masked %v", tc.name, count, tc.masked)
		}
		if _, bounded := count.Bound(); !tc.masked && !bounded {
			t.Errorf("%s: counted %+v, want a bound", tc.name, count)
		}
	}
}

// A QWEN CODE COMPRESSION'S HISTORY IS COUNTED WHATEVER ITS FIGURE: one whose
// new size does not read still has its kept history's images counted, so
// twenty there are CountMedia after a reply anchors the figure again; one
// whose payload or history does not read is CountCompacted, which no later
// reply clears.
//
// Mutations, each red: return before the history where the size does not
// read; let a later reply clear an unread history's mark.
func TestAQwenCompressionsHistoryIsCountedWhateverItsFigure(t *testing.T) {
	t.Parallel()
	image := map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="}}
	images := make([]any, 20)
	for i := range images {
		images[i] = image
	}
	shot := map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{
		"id": "c", "name": "mcp__crewlet__shot", "response": map[string]any{"output": "o"}, "parts": images}}}}
	reply := qwenRecord(t, "assistant", map[string]any{
		"message":       map[string]any{"role": "model", "parts": []any{map[string]any{"text": "Done."}}},
		"usageMetadata": map[string]any{"promptTokenCount": 1_000, "candidatesTokenCount": 50, "totalTokenCount": 1_050}})
	compression := func(payload any) string {
		return qwenRecord(t, "system", map[string]any{"subtype": "chat_compression", "systemPayload": payload})
	}
	for _, tc := range []struct {
		name, compression string
		mark              sandbox.CountMark
	}{
		{"no figure, twenty images kept", compression(map[string]any{"info": map[string]any{},
			"compressedHistory": []any{shot}}), sandbox.CountMedia},
		{"a payload that does not read", compression("a payload"), sandbox.CountCompacted},
		{"no history", compression(map[string]any{"info": map[string]any{"newTokenCount": 900}}),
			sandbox.CountCompacted},
	} {
		count := qwenSessionCount(t, qwenSession(t, "run_shell_command", nil, tc.compression, reply))
		if count.Mark != tc.mark {
			t.Errorf("%s, then a reply: counted %+v, want %q", tc.name, count, tc.mark)
		}
	}
}

// A GEMINI CLI OR QWEN CODE JOB'S COLLECTION COUNTS ITS BOX'S COPY AS ITS TAKE
// WOULD: the one chat file naming the job's session, read in its box, counts
// exactly what the take's meter counts the same file at, a torn last line
// passed over as the gate cuts it, so a relaunch continuing the copy, or a
// capture of it taken whole, is judged by a bound; a release but the measured
// one, a session with no file or one with two is no bound, and a read that
// asks no count reads none.
//
// Mutations, each red: count nothing at a collection (the copy's count nil, or
// unread); read the first of two files naming the session; count another
// release; count a torn line.
func TestAGeminiOrQwenCollectionCountsItsBoxsCopyAsItsTakeWould(t *testing.T) {
	t.Parallel()
	const geminiSession, qwenSession = "4759e486-b48b-459f-81aa-9b61a3bc8b09", "33333333-1818-4333-8444-000000000001"
	for _, tc := range []struct {
		name, harness, fixture, release string
		files                           func(p executor.Paths, chat []byte) map[string][]byte
		uncounted                       bool
		mark                            sandbox.CountMark
	}{
		{name: "gemini, its one file", harness: "gemini", fixture: "round.run1",
			files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.GeminiChats() + "/session-2026-11-23T03-07-4759e486.jsonl": chat}
			}},
		{name: "gemini, a torn last line", harness: "gemini", fixture: "round.run1",
			files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.GeminiChats() + "/session-2026-11-23T03-07-4759e486.jsonl": append(
					slices.Clone(chat), `{"id":"torn","type":"user","content":[{"text":"`+strings.Repeat("x", 4096)...)}
			}},
		{name: "gemini, two files naming it", harness: "gemini", fixture: "round.run1", mark: sandbox.CountUnread,
			files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.GeminiChats() + "/session-2026-11-23T03-07-4759e486.jsonl": chat,
					p.GeminiChats() + "/session-2026-11-23T03-01-4759e486.jsonl": chat[:bytes.IndexByte(chat, '\n')+1]}
			}},
		{name: "gemini, another release", harness: "gemini", fixture: "round.run1", release: "0.71.0",
			mark: sandbox.CountRelease, files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.GeminiChats() + "/session-2026-11-23T03-07-4759e486.jsonl": chat}
			}},
		{name: "gemini, no file", harness: "gemini", fixture: "round.run1", mark: sandbox.CountUnread,
			files: func(executor.Paths, []byte) map[string][]byte { return nil }},
		{name: "gemini, no count asked", harness: "gemini", fixture: "round.run1", uncounted: true,
			files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.GeminiChats() + "/session-2026-11-23T03-07-4759e486.jsonl": chat}
			}},
		{name: "qwen, its one file", harness: "qwen", fixture: "boundary-at",
			files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.QwenDir() + "/projects/-work/chats/" + qwenSession + ".jsonl": chat}
			}},
		{name: "qwen, a torn last line", harness: "qwen", fixture: "boundary-at",
			files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.QwenDir() + "/projects/-work/chats/" + qwenSession + ".jsonl": append(
					slices.Clone(chat), `{"type":"user","message":{"parts":[{"text":"`+strings.Repeat("x", 4096)...)}
			}},
		{name: "qwen, two files naming it", harness: "qwen", fixture: "boundary-at", mark: sandbox.CountUnread,
			files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.QwenDir() + "/projects/-work/chats/" + qwenSession + ".jsonl": chat,
					p.QwenDir() + "/projects/-other/chats/" + qwenSession + ".jsonl": chat}
			}},
		{name: "qwen, another release", harness: "qwen", fixture: "boundary-at", release: "0.28.5",
			mark: sandbox.CountRelease, files: func(p executor.Paths, chat []byte) map[string][]byte {
				return map[string][]byte{p.QwenDir() + "/projects/-work/chats/" + qwenSession + ".jsonl": chat}
			}},
		{name: "qwen, no file", harness: "qwen", fixture: "boundary-at", mark: sandbox.CountUnread,
			files: func(executor.Paths, []byte) map[string][]byte { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				runner *executor.Runner
				box    sandbox.Sandbox
			)
			session, release := geminiSession, "0.70.0"
			if tc.harness == "qwen" {
				session, release = qwenSession, "0.28.4"
				runner, box = qwenBox(t, "exit 0")
			} else {
				runner, box = geminiBoxAt(t, release, "exit 0")
			}
			chat := chatCountFixture(t, tc.harness, tc.fixture)
			for path, content := range tc.files(executor.PathsFor(box), chat) {
				if err := box.WriteFile(t.Context(), path, content); err != nil {
					t.Fatal(err)
				}
			}
			ask := sandbox.MainThreadAsk{Session: session, Release: release, Count: !tc.uncounted, Ended: true}
			if tc.release != "" {
				ask.Release = tc.release
			}
			got := runner.ReadMainThread(t.Context(), box, ask).Count
			switch {
			case tc.uncounted:
				if got != nil {
					t.Fatalf("a read asking no count counted %+v", got)
				}
			case tc.mark != "":
				if got == nil || got.Mark != tc.mark {
					t.Fatalf("the copy counts %+v, want %q", got, tc.mark)
				}
			default:
				want := chatCount(t, tc.harness, tc.fixture)
				if got == nil || got.Mark != "" || got.Tokens != want.Tokens || !reflect.DeepEqual(got.Restart, want.Restart) {
					t.Fatalf("the copy counts %+v, want the take's %+v", got, want)
				}
			}
		})
	}
}
