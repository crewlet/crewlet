package executor

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

// A COUNT IS NEVER BELOW WHAT THE SCRUB TOOK OUT: a box that resumes its own
// copy resumes it unscrubbed, so the count a take's meter reads off the kept
// archive must bound the archive as the box holds it. A session holds an
// engine value far longer than its marker, one shorter than its marker whose
// Gemini CLI charge is higher (seven CJK characters, 21 bytes, a token and a
// half each beside the 24-byte marker's six tokens), alone in a case of its
// own, and a GitHub token the shape pass redacts, all in what the count reads;
// the take's own scrub and meter, its tally between them, count it at least as
// the unscrubbed archive counts. Mutation: leave the tally off the meter's
// reading, or count a text's length less its kept text's, and a case goes red.
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
	for name, tc := range map[string]struct {
		carries carrier
		release string
		archive []byte
	}{
		"a Gemini CLI chat file": {carries: GeminiCLI{}, release: geminiRelease, archive: chat(planted)},
		"a Gemini CLI chat file holding a value shorter than its marker": {carries: GeminiCLI{},
			release: geminiRelease, archive: chat("the key " + short + " is short")},
		"an OpenCode export": {carries: OpenCode{}, release: "2.3.2",
			archive: ocDoc(t, ocUser("msg_u", "go", "[]"), ocReply("msg_r", `"stub"`, `[]`, 100, 20, 3, 30, 7),
				ocUser("msg_v", planted, "[]"))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			held, err := tc.carries.meter(t.Context(), bytes.NewReader(tc.archive), "", tc.release, new(passBudget))
			raw, ok := held.count.Bound()
			if err != nil || !ok {
				t.Fatalf("the unscrubbed archive counts %+v (%v), no bound", held.count, err)
			}
			tally := newScrubTally(tc.carries, "")
			meter := newContextMeter(t.Context(), tc.carries, "", tc.release, new(passBudget))
			var kept bytes.Buffer
			err = scrubArchive(tc.carries.layout(), bytes.NewReader(tc.archive), reopenOf(tc.archive),
				io.MultiWriter(&kept, meter), newScrubber([]string{long, short}), testBounds.tallying(tally))
			meter.finish(err, tally.taken)
			if err != nil {
				t.Fatalf("the scrub: %v", err)
			}
			for _, secret := range []string{long, short, token} {
				if bytes.Contains(keptPlain(t, kept.Bytes()), []byte(secret)) {
					t.Fatalf("the kept archive still holds %.20q", secret)
				}
			}
			alone, err := tc.carries.meter(t.Context(), bytes.NewReader(kept.Bytes()), "", tc.release, new(passBudget))
			if n, ok := alone.count.Bound(); err != nil || !ok || n >= raw {
				t.Fatalf("the kept archive alone counts %+v (%v) against the box's %d, so this proves nothing",
					alone.count, err, raw)
			}
			got, ok := meter.count().Bound()
			if !ok || got < raw {
				t.Errorf("the take counts %+v, below the %d its box's unscrubbed copy counts", meter.count(), raw)
			}
		})
	}
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
