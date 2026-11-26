package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// geminiPreambleSession is 0.70.0's record of two compressions, the newer
// one's summary (line 94) opening with a `<scratchpad>` before its
// `<state_snapshot>`, as a model may write it (measured, #2020).
const geminiPreambleSession = "session-compressed-preamble.jsonl"

// geminiRecord is testdata/gemini/0.70.0's chat file name, whole.
func geminiRecord(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "gemini", "0.70.0", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// geminiEntryOf is line n of record (from 1), decoded: its id, type and first
// text, "" for none.
func geminiEntryOf(t *testing.T, record []byte, n int) (id, typ, text string) {
	t.Helper()
	var e struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(bytes.Split(record, []byte("\n"))[n-1], &e); err != nil {
		t.Fatalf("line %d: %v", n, err)
	}
	if len(e.Content) > 0 {
		text = e.Content[0].Text
	}
	return e.ID, e.Type, text
}

// A GEMINI CLI COMPRESSION IS FOUND BY ITS LINES, NOT ITS WORDS: on 0.70.0's
// record of two, the newer summary opening with a `<scratchpad>`, every
// reader of the chat file finds both summaries and nothing else: the
// transcript reads each as a summary and its restated brief never again, the
// confirm takes the restated brief for no second delivery, the count and the
// holdings walk restart at each, and a box read's newest summary is the newer
// one, the older having been patched away.
//
// Mutation: find a summary by its `<state_snapshot>` opening, by its `$set`
// alone or by its reply alone, in any one reader, and a case goes red.
func TestAGeminiCompressionIsFoundByItsLinesNotItsWords(t *testing.T) {
	t.Parallel()
	record := geminiRecord(t, geminiPreambleSession)
	older, _, _ := geminiEntryOf(t, record, 49)
	newer, typ, text := geminiEntryOf(t, record, 94)
	const brief = "PRBPROBE brief of r5\n"
	_, _, restated := geminiEntryOf(t, record, 96)
	if typ != "user" || !strings.HasPrefix(text, "<scratchpad>") || restated != brief {
		t.Fatal("line 94 is no summary opening with a preamble, or 96 no restated brief: the case proves nothing")
	}
	t.Run("transcript", func(t *testing.T) {
		t.Parallel()
		b := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{})
		if err := b.geminiSession(t.Context(), bytes.NewReader(record)); err != nil &&
			!errors.Is(err, errTranscriptPage) {
			t.Fatal(err)
		}
		var summaries []string
		briefs := 0
		for _, e := range b.entries {
			switch {
			case e.Kind == EntrySummary:
				summaries = append(summaries, e.Text)
			case e.Kind == EntryBrief && e.Text == strings.TrimSpace(brief):
				briefs++
			}
		}
		if len(summaries) != 2 || summaries[1] != text || briefs != 1 {
			t.Errorf("read %d summaries (%.40q) and r5's brief %d times; want two, the newer line 94's, and "+
				"the brief once", len(summaries), summaries, briefs)
		}
	})
	t.Run("confirm", func(t *testing.T) {
		t.Parallel()
		takes := []geminiAskedTake{{digest: ItemDigest(brief), until: -1}, {digest: ItemDigest(brief), until: -1},
			{digest: ItemDigest(text), until: -1}}
		confirmed, err := geminiConfirmed(t.Context(), bytes.NewReader(record), takes, maxLineBytes)
		if err != nil || !slices.Equal(confirmed, []bool{true, false, false}) {
			t.Errorf("confirmed %v (%v); want the brief's one delivery alone, never its restatement or the "+
				"summary", confirmed, err)
		}
	})
	t.Run("count", func(t *testing.T) {
		t.Parallel()
		c := newGeminiCounting()
		if err := chatFileLines(t.Context(), bytes.NewReader(record), c, new(passBudget)); err != nil {
			t.Fatal(err)
		}
		var got []string
		for id := range c.snapshot {
			got = append(got, id)
		}
		slices.Sort(got)
		want := []string{older, newer}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("the count restarts at %q, want at the two summaries %q", got, want)
		}
	})
	t.Run("newest summary", func(t *testing.T) {
		t.Parallel()
		c := newGeminiCounting()
		c.summarize()
		// A REPLY AFTER A USER ENTRY NO `$set` OPENED is no compression's.
		after := `{"id":"late","timestamp":"t","type":"user","content":[{"text":"no summary"}]}` + "\n" +
			`{"id":"late-ack","timestamp":"t","type":"gemini","content":[{"text":"` + geminiAck + `"}]}` + "\n"
		err := chatFileLines(t.Context(), bytes.NewReader(append(append([]byte(nil), record...), after...)), c,
			new(passBudget))
		if got := c.summary(err); got.Text != strings.TrimSpace(text) {
			t.Errorf("the newest summary reads %.60q (%s), want line 94's", got.Text, got.Unread)
		}
	})
	t.Run("holdings", func(t *testing.T) {
		t.Parallel()
		w := newGeminiHoldingsWalk(context.Background(), "7c0ffee0-5b5b-4a4a-8c8c-000000002020",
			sandbox.RunStart{Began: true}, sandbox.HoldingsAsk{Resumer: "0.70.0"})
		if _, err := chatLines(t.Context(), bytes.NewReader(record), w.line); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, id := range []string{older, newer} {
			var key geminiID
			sum := sha256.Sum256([]byte(id))
			copy(key[:], sum[:])
			i, known := w.ids[key]
			if !known || !w.recs[i].snapshot {
				got = append(got, id)
			}
		}
		summaries := 0
		for _, r := range w.recs {
			if r.snapshot {
				summaries++
			}
		}
		if len(got) != 0 || summaries != 2 {
			t.Errorf("the walk reads %d summaries, missing %q; want exactly the two", summaries, got)
		}
	})
}

// A GEMINI COMPRESSION'S FORMS HOLD ONLY ITS OWN LINES: the `$set` of the
// session alone and the reply whole, the summary's entry by its opening; a
// payload naming them, a key out of order, a field more, an escaped quote's
// string read whole, a line past the forms' bound, are none of them.
//
// Mutation: match a whole form by its opening, a hole past a quote, or read
// past the bound, and a row goes red.
func TestAGeminiCompressionsFormsHoldOnlyItsOwnLines(t *testing.T) {
	t.Parallel()
	ack := `","type":"gemini","content":[{"text":"` + geminiAck + `"}]}`
	long := strings.Repeat("x", lineFormMaxBytes)
	for _, c := range []struct {
		form lineForm
		line string
		want bool
	}{
		{geminiCompressionSet, `{"$set":{"sessionId":"7c0ffee0"}}`, true},
		{geminiCompressionSet, `{"$set":{"sessionId":"7c0ffee0"}} `, false},
		{geminiCompressionSet, `{"$set":{"sessionId":"7c0ffee0","lastUpdated":"t"}}`, false},
		{geminiCompressionSet, `{"$set":{"lastUpdated":"t"}}`, false},
		{geminiCompressionSet, `{"$set":{"sessionId":"a\"}}"}}`, true},
		{geminiCompressionSet, `{"$set":{"sessionId":"` + long + `"}}`, false},
		{geminiCompressionUser, `{"id":"u","timestamp":"t","type":"user","content":[{"text":"anything"}]}`, true},
		{geminiCompressionUser, `{"id":"u","timestamp":"t","type":"user","content":"a string"}`, false},
		{geminiCompressionUser, `{"type":"user","id":"u","timestamp":"t","content":[]}`, false},
		{geminiCompressionUser, `{"id":"u\",\"timestamp\":\"t","timestamp":"t","type":"user","content":[`, true},
		{geminiCompressionUser, `{"id":"` + long + `","timestamp":"t","type":"user","content":[`, false},
		{geminiCompressionUser, `{"id":"u","timestamp":"t","type":"user","content":[` + long, true},
		{geminiCompressionAck, `{"id":"a","timestamp":"t` + ack, true},
		{geminiCompressionAck, `{"id":"a","timestamp":"t` + ack + `x`, false},
		{geminiCompressionAck, `{"id":"a","timestamp":"t","type":"user","content":[{"text":"` + geminiAck + `"}]}`, false},
		{geminiCompressionAck, `{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"Got it."}]}`, false},
		{geminiCompressionAck, `{"id":"a","timestamp":"t","type":"gemini","content":"` + geminiAck + `"}`, false},
		{geminiCompressionAck, `{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"x"}],"q":"` + ack, false},
	} {
		if got := c.form.holds([]byte(c.line)); got != c.want {
			t.Errorf("%q holds %.80q: %v, want %v", c.form.pieces[0], c.line, got, c.want)
		}
	}
}

// A LINE'S FORMS ARE JUDGED ON THE WHOLE LINE TRIMMED: the walk's head of a
// line ([lineFormHead]), written a piece at a time, judges each compression
// form as the whole-line read judges the line cut of its white space at both
// ends: a form led or trailed by spaces, tabs or no-break spaces to a byte
// short of [lineFormMaxBytes], to it, a byte past it and far past it, a
// character split at the head's edge, and a byte that is no character.
//
// Mutation: keep a head past the bound untrimmed at its end, or hold the
// line's leading white space: a case fails.
func TestALinesFormsAreJudgedOnTheWholeLineTrimmed(t *testing.T) {
	t.Parallel()
	forms := []lineForm{geminiCompressionSet, geminiCompressionUser, geminiCompressionAck}
	set := `{"$set":{"sessionId":"s"}}`
	ack := `{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"` + geminiAck + `"}]}`
	user := `{"id":"c","timestamp":"t","type":"user","content":[{"text":"<state_snapshot>s"}]}`
	pad := func(by string, n int) string { return strings.Repeat(by, max(n, 0)/len(by)+1)[:max(n, 0)] }
	var lines []string
	for _, form := range []string{set, ack, user} {
		for _, by := range []string{" ", "\t", " ", "  "} {
			for _, n := range []int{lineFormMaxBytes - 1, lineFormMaxBytes, lineFormMaxBytes + 1, 3 * lineFormMaxBytes} {
				lines = append(lines, pad(by, n-len(form))+form, form+pad(by, n-len(form)),
					pad(by, n/2)+form+pad(by, n/2))
			}
		}
	}
	long := strings.Repeat("é", lineFormMaxBytes/2)
	lines = append(lines, `{"id":"c","timestamp":"`+long+`","type":"user","content":[`+"  ",
		"\xe2\x80"+ack, ack+" \xe2\x80", " \xff "+ack, pad(" ", lineFormMaxBytes)+"é"+ack)
	for _, line := range lines {
		whole := bytes.TrimSpace([]byte(line))
		for _, piece := range []int{1, 2, 3, 4096, len(line)} {
			var h lineFormHead
			for rest := []byte(line); len(rest) > 0; rest = rest[min(piece, len(rest)):] {
				_, _ = h.Write(rest[:min(piece, len(rest))])
			}
			for _, f := range forms {
				if got, want := f.holds(h.form()), f.holds(whole); got != want {
					t.Errorf("%.40q of %d bytes in pieces of %d: %q holds %v, want %v as the line trimmed whole",
						line, len(line), piece, f.pieces[0], got, want)
				}
			}
		}
	}
}
