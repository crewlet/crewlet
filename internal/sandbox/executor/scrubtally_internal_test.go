package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A COUNT IS NEVER BELOW WHAT THE SCRUB TOOK OUT: a box may resume its own
// copy unscrubbed, so the take's count of the kept archive bounds the box's.
// Each case takes one removal through the take's scrub, tally and meter: a
// value far longer than its marker; seven CJK characters (21 bytes, 10.5
// Gemini CLI tokens beside the marker's six); a GitHub token; a credential's
// string and number; and a tool definition the cut takes out. A Codex count,
// a figure its rollout states, is never raised.
//
// Mutation: drop any tally, count length less kept length, have Claude Code
// or Qwen Code charge no member, and a case goes red.
func TestACountIsNeverBelowWhatTheScrubTookOut(t *testing.T) {
	t.Parallel()
	long := "sk-engine-" + strings.Repeat("Q7x", 100)
	const short = "秘密鍵値暗号語"
	const token = "ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	planted := "keys " + long + " and " + short + " and " + token + " go here"
	gemini := bytes.Join(geminiFixture(t, "thought"), nil)
	if !bytes.Contains(gemini, []byte(`"text":"Think.\n"`)) {
		t.Fatal("the Gemini CLI fixture holds no person's text to plant the secrets in")
	}
	chat := func(text string) []byte {
		return geminiChatArchive(t, bytes.Replace(gemini, []byte(`"text":"Think.\n"`),
			[]byte(`"text":"`+text+`\n"`), 1))
	}
	claude := func(tail string) []byte {
		return cntArchive(t, cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 0, 0, 50),
			tail))
	}
	answer := func(extra string) string {
		return cntUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok",`+extra+`}]`)
	}
	codex := append(bytes.TrimRight(codexRolloutFixture(t), "\n"), []byte("\n"+
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text",`+
		`"text":"`+planted+`"}]}}`+"\n")...)
	for name, tc := range map[string]struct {
		carries          carrier
		session, release string
		values           []string
		archive          []byte
		// stated says the count is a figure the session states, which the
		// scrub never moves: the take's count is the box's exactly.
		stated bool
	}{
		"a Gemini CLI chat file": {carries: GeminiCLI{}, release: geminiRelease, values: []string{long, short},
			archive: chat(planted)},
		"a Gemini CLI value shorter than its marker": {carries: GeminiCLI{}, release: geminiRelease,
			values: []string{short}, archive: chat("the key " + short + " is short")},
		"a Qwen Code chat file": {carries: QwenCode{}, release: qwenRelease, values: []string{long, short},
			archive: tarOfMembers(t, []tarMember{{h: qwenMember, body: qwenTallyChat(t, planted)}})},
		"an OpenCode export": {carries: OpenCode{}, release: "2.3.2", values: []string{long, short},
			archive: ocDoc(t, ocUser("msg_u", "go", "[]"), ocReply("msg_r", `"stub"`, `[]`, 100, 20, 3, 30, 7),
				ocUser("msg_v", planted, "[]"))},
		"a Claude Code credential's string": {carries: ClaudeCode{}, session: ctSession, release: "2.1.322",
			archive: claude(answer(`"api_key":"` + strings.Repeat("zq", 100) + `"`))},
		"a Claude Code credential's number": {carries: ClaudeCode{}, session: ctSession, release: "2.1.322",
			archive: claude(answer(`"token":` + strings.Repeat("1234567890", 6)))},
		"a Claude Code tool definition the cut takes out": {carries: ClaudeCode{}, session: ctSession,
			release: "2.1.322", values: []string{ltServer}, archive: claude(ltRecord("2.1.322", "", ltRenamed, ltEcho))},
		"a Codex rollout": {carries: Codex{}, release: codexCarryRelease, values: []string{long, short},
			archive: codexArchive(t, codex), stated: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			held, err := tc.carries.meter(t.Context(), bytes.NewReader(tc.archive), tc.session, tc.release,
				new(passBudget))
			raw, ok := held.count.Bound()
			if err != nil || !ok {
				t.Fatalf("the unscrubbed archive counts %+v (%v), no bound", held.count, err)
			}
			tally := newScrubTally(tc.carries, tc.session)
			meter := newContextMeter(t.Context(), tc.carries, tc.session, tc.release, new(passBudget))
			var kept bytes.Buffer
			err = scrubArchive(tc.carries.layout(), bytes.NewReader(tc.archive), reopenOf(tc.archive),
				io.MultiWriter(&kept, meter), newScrubber(tc.values), testBounds.tallying(tally))
			meter.finish(err, tally.taken)
			if err != nil {
				t.Fatalf("the scrub: %v", err)
			}
			alone, err := tc.carries.meter(t.Context(), bytes.NewReader(kept.Bytes()), tc.session, tc.release,
				new(passBudget))
			if n, ok := alone.count.Bound(); err != nil || !ok || (n >= raw) != tc.stated {
				t.Fatalf("the kept archive alone counts %+v (%v) against the box's %d, so this proves nothing",
					alone.count, err, raw)
			}
			got, ok := meter.count().Bound()
			switch {
			case tc.stated && got != raw:
				t.Errorf("the take counts %+v, not the %d the session states", meter.count(), raw)
			case !ok || got < raw:
				t.Errorf("the take counts %+v, below the %d its box's unscrubbed copy counts", meter.count(), raw)
			}
		})
	}
}

// A TALLY COUNTS WHAT EACH KEPT BYTE LOST ONCE: a line read both ways whose
// JSON spills past its way's room into the spool and is then let go, the line
// no JSON at its end, and read again as text, is tallied as the text alone.
//
// Mutation: leave the spool's tally standing where it cuts a way back out,
// and this goes red.
func TestATallyCountsWhatEachKeptByteLostOnce(t *testing.T) {
	t.Parallel()
	const value = "engine-value-0123456789abcdef"
	body := "no JSON opens the reading both ways\n" +
		`{"a":"` + value + strings.Repeat("y", wayRoomBytes+(8<<10)) + `"} and no JSON past it` + "\n"
	archive := tarOfMembers(t, []tarMember{{h: geminiMember, body: body}})
	tally := newScrubTally(GeminiCLI{}, "")
	var kept bytes.Buffer
	if err := scrubArchive(archiveTar, bytes.NewReader(archive), reopenOf(archive), &kept,
		newScrubber([]string{value}), testBounds.tallying(tally)); err != nil {
		t.Fatalf("the scrub: %v", err)
	}
	if plain := keptPlain(t, kept.Bytes()); bytes.Contains(plain, []byte(value)) ||
		!bytes.Contains(plain, []byte(`{"a":"`+scrubMarker+"yyy")) {
		t.Fatal("the line was not kept as its text, scrubbed, so this proves nothing")
	}
	if want := escCharge(value); tally.taken != want {
		t.Errorf("the scrub tallied %d, want the %d its one kept value lost", tally.taken, want)
	}
}

// THE SIZES A CHAT FILE'S FIRST REQUEST READS COUNT WHAT THE SCRUB TOOK OUT:
// it may have been any tool answer's or text's, so it alone carries answers
// a token short of Gemini CLI's masking, a history a byte short of its
// collapse, and answers a byte short of Qwen Code's clearing over the line.
//
// Mutation: leave taken out of any one of the three sizes, and a case goes
// red.
func TestWhatTheScrubTookOutCountsTowardAChatFilesSizes(t *testing.T) {
	t.Parallel()
	gemini := func(answers, view int64, n int) *geminiCounting {
		c := newGeminiCounting()
		c.order, c.quarters, c.bytes = []string{"r"}, map[string]int64{"r": 4}, map[string]int64{"r": view}
		for i := range n {
			c.answers[string(rune('a'+i))] = &geminiAnswer{tokens: answers / int64(n), carriers: []string{"r"}}
		}
		return c
	}
	qwen := &qwenCounting{answers: qwenClearsAt - 1}
	for name, c := range map[string]chatCounter{
		"answers a token short of masking": gemini(geminiMaskProtect-1, 100, 1),
		"a history a byte short of collapse": gemini(4,
			geminiCollapseBytes-geminiContextAllowance-1, geminiCollapseKeptTurns+1),
		"answers a byte short of clearing": qwen,
	} {
		if got := c.count(0, 0); got.Mark == sandbox.CountMasked {
			t.Fatalf("%s: the count is %+v with nothing taken, so this proves nothing", name, got)
		}
		if got := c.count(0, 1); got.Mark != sandbox.CountMasked {
			t.Errorf("%s: a byte taken counts %+v, want masked", name, got)
		}
	}
}

// geminiMember and qwenMember are a chat file's member as each harness's
// archive names it.
var (
	geminiMember = tar.Header{Name: "chats/session-2026-11-23T06-14-63e9014e.jsonl", Mode: 0o600,
		Typeflag: tar.TypeReg}
	qwenMember = tar.Header{Name: "projects/-work/chats/0b6f3c2e-0000-4000-8000-000000000001.jsonl", Mode: 0o600,
		Typeflag: tar.TypeReg}
)

// qwenTallyChat is a Qwen Code chat file of a brief, a reply recording its
// prompt, and a person's message holding text after it.
func qwenTallyChat(t *testing.T, text string) string {
	t.Helper()
	record := func(uuid, parent, kind string, fields map[string]any) string {
		r := map[string]any{"uuid": uuid, "parentUuid": parent, "sessionId": "s", "timestamp": "t", "type": kind}
		for k, v := range fields {
			r[k] = v
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	parts := func(role, text string) map[string]any {
		return map[string]any{"role": role, "parts": []any{map[string]any{"text": text}}}
	}
	return strings.Join([]string{
		record("q1", "", "user", map[string]any{"message": parts("user", "Run the probe round.")}),
		record("q2", "q1", "assistant", map[string]any{"message": parts("model", "Done."),
			"usageMetadata": map[string]any{"promptTokenCount": 1000, "candidatesTokenCount": 50,
				"totalTokenCount": 1050}}),
		record("q3", "q2", "user", map[string]any{"message": parts("user", text)}),
	}, "\n") + "\n"
}

// keptPlain is the gzip stream b, decompressed.
func keptPlain(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
