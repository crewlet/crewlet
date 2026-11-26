package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// inlineData is an `inlineData` object of mime whose data is n bytes of
// base64, its data first where first says so, as Gemini CLI 0.70.0's read_file
// writes one (an MCP tool's writes its mimeType first).
func inlineData(mime string, n int, first bool) string {
	data := `"data":"` + strings.Repeat("A", n) + `"`
	if first {
		return `{"inlineData":{` + data + `,"mimeType":"` + mime + `"}}`
	}
	return `{"inlineData":{"mimeType":"` + mime + `",` + data + `}}`
}

// geminiMediaLines are Gemini CLI session lines holding media of n bytes in
// every place 0.70.0 writes it that a person's read, a collection's and the
// count read alike: a person's part and a reply's, of an image and of another
// medium, either member first; a call's result whose answer carries it, and
// one beside it.
func geminiMediaLines(n int) []string {
	answer := func(parts string) string {
		return `{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"Binary content provided ` +
			`(1 item(s))."}` + parts + `}}`
	}
	return []string{
		geminiSessionLine("user", "u5", `[{"text":"look"},`+inlineData("image/png", n, false)+`]`, ""),
		geminiSessionLine("gemini", "m5", `[`+inlineData("application/pdf", n, true)+`,{"text":"read"}]`, ""),
		geminiSessionLine("gemini", "m6", `"reading"`, `,"toolCalls":[{"id":"c1","name":"read_file","args":{},`+
			`"result":[`+answer(`,"parts":[`+inlineData("image/png", n, true)+`]`)+`]}]`),
		geminiSessionLine("gemini", "m7", `"reading"`, `,"toolCalls":[{"id":"c1","name":"read_file","args":{},`+
			`"result":[`+answer("")+`,`+inlineData("video/mp4", n, false)+`]}]`),
	}
}

// geminiCountMediaLines are the lines the count reads alike, and a person's
// read shows otherwise (its place where the oracle shows its bytes): an answer
// whose response carries media a model refuses (`__binary_injection__`), and a
// text that is a data URL.
func geminiCountMediaLines(n int) []string {
	return []string{
		geminiSessionLine("gemini", "m8", `"reading"`, `,"toolCalls":[{"id":"c1","name":"read_file","args":{},`+
			`"result":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"o",`+
			`"__binary_injection__":[`+inlineData("audio/x-aiff", n, true)+`]}}}]}]`),
		geminiSessionLine("user", "u6", `[{"text":"`+imageURL(n)+`"}]`, ""),
	}
}

// qwenMediaLines are Qwen Code chat-file records holding media of n bytes in
// every place 0.28.4 writes it that a person's read, a collection's and the
// count read alike: a person's part and a reply's, either member first; a
// tool's answer carrying it; and a compression's kept history holding it.
func qwenMediaLines(n int) []string {
	return []string{
		qwenRecord("user", `{"text":"look"}`, inlineData("image/png", n, false)),
		qwenRecord("assistant", inlineData("application/pdf", n, true), `{"text":"read"}`),
		qwenRecord("tool_result", `{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"o"},`+
			`"parts":[`+inlineData("image/jpeg", n, true)+`]}}`),
		qwenCompression(`{"info":{"newTokenCount":500},"compressedHistory":[{"role":"user","parts":[{"text":` +
			`"<state_snapshot>s</state_snapshot>"}]},{"role":"model","parts":[{"text":"Got it."}]},{"role":"user",` +
			`"parts":[` + inlineData("image/png", n, false) + `]}],"summaryPrefixLength":2}`),
	}
}

// qwenCountMediaLines are the lines the count reads alike, and a person's read
// shows otherwise: an answer whose response carries media, and a text that is
// a data URL.
func qwenCountMediaLines(n int) []string {
	return []string{
		qwenRecord("tool_result", `{"functionResponse":{"id":"c1","response":{"media":`+
			inlineData("image/png", n, true)+`}}}`),
		qwenRecord("user", `{"text":"`+imageURL(n)+`"}`),
	}
}

// chatMediaKB is the size of the media the differentials' lines hold.
const chatMediaKB = 3 << 10

// A CHAT LINE PAST THE BOUND ON ITS MEDIA IS READ, IT PASSED: Qwen Code's and
// Gemini CLI's counts, the meter and a collection's read read a line whose
// media run to 12 MiB, past the count's 8 MiB bound, in every place each
// writes it, and a line of 100 MiB of media to the count, as does a Qwen Code
// summary's read of a compaction's kept history; their figures are
// those of the same line holding a kibibyte, but where a medium's size is the
// count's (an answer's, which the walk measures as the oracle does).
//
// Mutation: count media against the bound, or drop the pass: a case fails.
func TestAChatLinePastTheBoundOnItsMediaIsReadItPassed(t *testing.T) {
	t.Parallel()
	for _, h := range chatCountHarnesses(t) {
		lines, sized := qwenMediaLines, qwenCountMediaLines
		if h.name == "gemini" {
			lines, sized = geminiMediaLines, geminiCountMediaLines
		}
		small := append(lines(1<<10), sized(1<<10)...)
		for i, line := range append(lines(12<<20), sized(12<<20)...) {
			record := h.base + "\n" + line + "\n" + h.base + "\n"
			walk := h.walk()
			marks, err := countChat(h.release, h.release, func() error {
				return chatFileLines(t.Context(), strings.NewReader(record), walk, new(passBudget), nil)
			})
			if err != nil {
				t.Errorf("%s line %d: the count fails: %v", h.name, i, err)
				continue
			}
			want := h.whole()
			wantMarks, wantErr := countWholeOf(t.Context(), want, []byte(h.base+"\n"+small[i]+"\n"+h.base+"\n"),
				h.release, h.release)
			got, wanted := walk.count(marks, 0), want.count(wantMarks, 0)
			measured := i >= len(lines(0)) || strings.Contains(line, "functionResponse")
			if !measured && (wantErr != nil || !sameCount(got, wanted)) {
				t.Errorf("%s line %d: counts %s, want the kibibyte's %s (%v)", h.name, i, countString(got),
					countString(wanted), wantErr)
			}
		}
	}
	h := chatCountHarnessOf(t, "qwen")
	record := h.base + "\n" + qwenRecord("user", inlineData("image/png", 100<<20, true)) + "\n"
	if _, err := countChat(h.release, h.release, func() error {
		return chatFileLines(t.Context(), strings.NewReader(record), h.walk(), new(passBudget), nil)
	}); err != nil {
		t.Errorf("a 100 MiB medium fails the count: %v", err)
	}
	for _, line := range geminiMediaLines(12 << 20) {
		takes := []geminiAskedTake{{digest: ItemDigest("one"), until: -1}}
		note := geminiSessionLine("user", "n", `[{"text":"one"}]`, "")
		got, err := geminiConfirmed(t.Context(), strings.NewReader(line+"\n"+note+"\n"), takes, maxLineBytes)
		if err != nil || len(got) != 1 || !got[0] {
			t.Errorf("a Gemini CLI collection confirms %v (%v) past %.60q, want the note", got, err, line)
		}
	}
	var s summaryRead
	if _, err := qwenNotesHeld(t.Context(), strings.NewReader(qwenMediaLines(12 << 20)[3]+"\n"), nil, 0, maxLineBytes,
		&s); err != nil || s.reading().Unread != "" || s.reading().Text == "" {
		t.Errorf("a Qwen Code compaction holding media reads its summary %+v (%v), want it read", s.reading(), err)
	}
	for _, line := range qwenMediaLines(40 << 20) {
		note := `{"type":"user","subtype":"mid_turn_user_message","message":{"role":"user","parts":[{"text":` +
			boundedQuoted("\n"+qwenNoteFraming+"one") + `}]}}`
		record := line + "\n" + note + "\n" + qwenRecord("assistant", `{"text":"ok"}`) + "\n"
		held, err := qwenNotesHeld(t.Context(), strings.NewReader(record), noteOne, 0, maxLineBytes, nil)
		if err != nil || len(held) != 1 {
			t.Errorf("a Qwen Code collection holds %v (%v) past %.60q, want the note", held, err, line)
		}
	}
}

// A PERSON'S READ OF A CHAT LINE PAST THE BOUND ON ITS MEDIA IS READ: a line of
// 40 MiB of media in every place each harness writes it, past the 32 MiB a
// read reads, makes the entries its text makes, the medium shown nowhere it
// was shown before, and a response holding one shows it as its place.
//
// Mutation: count media against the bound, or keep a response's medium as
// written: a case fails.
func TestAPersonsReadOfAChatLinePastTheBoundOnItsMediaIsRead(t *testing.T) {
	t.Parallel()
	n := 40 << 20
	type read struct {
		name    string
		carrier carrier
		members []boundedMember
		want    string
	}
	var reads []read
	for i, line := range geminiMediaLines(n) {
		reads = append(reads, read{fmt.Sprintf("gemini line %d", i), GeminiCLI{}, geminiMembers([]string{line}), ""})
	}
	for i, line := range qwenMediaLines(n) {
		reads = append(reads, read{fmt.Sprintf("qwen line %d", i), QwenCode{}, qwenMembers([]string{line}), ""})
	}
	for _, c := range append(reads, []read{
		{"gemini", GeminiCLI{}, geminiMembers([]string{geminiMediaLines(n)[0]}), "brief: look"},
		{"qwen", QwenCode{}, qwenMembers([]string{qwenMediaLines(n)[0]}), "brief: look"},
		{"qwen answer", QwenCode{}, qwenMembers([]string{qwenRecord("assistant",
			`{"functionCall":{"id":"c1","name":"read_file","args":{}}}`), qwenCountMediaLines(n)[0]}),
			`tool_result: {"media":{"inlineData":{"data":"[inline data of 40.0 MiB]","mimeType":"image/png"}}}`},
	}...) {
		page, err := transcriptOf(t, c.carrier, boundedArchiveOf(t, c.members...))
		if got := texts(page); err != nil || !strings.Contains(got, c.want) || strings.Contains(got, "AAAA") ||
			strings.Contains(got, "unread") {
			t.Errorf("%s: read\n%.400s\n(%v), want it to hold %q", c.name, got, err, c.want)
		}
	}
}

// A MEDIUM'S PLACE NAMES IT AS IT IS: a data URL of an image as an image, of
// any other medium by its type as written, and an inline medium, whose type
// its text need not name first, as data; each sized.
//
// Mutation: show every data URL as an image, or drop its size: a case fails.
func TestAMediumsPlaceNamesItAsItIs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		head string
		want string
	}{
		{"data:image/png;base64,", "[an image of 2 KiB]"},
		{"data:IMAGE/png;base64,", "[an image of 2 KiB]"},
		{"data:application/pdf;base64,", "[inline application/pdf of 2 KiB]"},
		{"data:text/plain;charset=utf-8;base64,", "[inline text/plain of 2 KiB]"},
		{"", "[inline data of 2 KiB]"},
	} {
		head := []byte(c.head)
		if c.head == "" {
			head = nil
		}
		if got := mediaPlaceholder(head, 2<<10); got != c.want {
			t.Errorf("%q: placed as %q, want %q", c.head, got, c.want)
		}
	}
	var out bytes.Buffer
	a := asWrittenImages{w: &out}
	_, _ = a.Write([]byte(`{"a":"data:application/pdf;base64,AAAA","inlineData":{"data":"AAAA","mimeType":"m"},` +
		`"b":{"data":"AAAA"},"c":["inlineData",{"data":"AAAA"}],"d":"x"}`))
	if want := `{"a":"[inline application/pdf of 32 bytes]","inlineData":{"data":"[inline data of 4 bytes]",` +
		`"mimeType":"m"},"b":{"data":"AAAA"},"c":["inlineData",{"data":"AAAA"}],"d":"x"}`; out.String() != want {
		t.Errorf("a value as written reads\n%s\nwant\n%s", out.String(), want)
	}
}

// A CHAT LINE WHOSE TEXT OUTSIDE ITS MEDIA PASSES THE BOUND IS STILL REFUSED by
// the count: the bound holds what the reader keeps.
//
// Mutation: pass every string as media: the count reads the line.
func TestAChatLineWhoseTextOutsideItsMediaPassesTheBoundIsRefused(t *testing.T) {
	t.Parallel()
	words := strings.Repeat("w", codexRolloutLineMaxBytes)
	for _, h := range chatCountHarnesses(t) {
		line := qwenRecord("user", `{"text":"`+words+`"}`, inlineData("image/png", 1<<20, true))
		if h.name == "gemini" {
			line = geminiSessionLine("user", "u7", `[{"text":"`+words+`"},`+inlineData("image/png", 1<<20, true)+`]`, "")
		}
		_, err := countChat(h.release, h.release, func() error {
			return chatFileLines(t.Context(), strings.NewReader(h.base+"\n"+line+"\n"), h.walk(), new(passBudget), nil)
		})
		if !errors.Is(err, errChatLine) {
			t.Errorf("%s: the count reads the line (%v), want it refused", h.name, err)
		}
	}
}

// A CHAT LINE OF THE BOUND BESIDE ITS MEDIA IS COUNTED, AND ONE BYTE PAST IT IS
// NOT, a medium after its words in flight as they reach the bound
// ([imageSlack]).
//
// Mutation: take no slack: a case fails.
func TestAChatLineOfTheBoundBesideItsMediaIsCountedAndOnePastItNot(t *testing.T) {
	t.Parallel()
	h := chatCountHarnessOf(t, "qwen")
	media := inlineData("image/png", 1<<20, false)
	data := 1 << 20
	for _, c := range []struct {
		name string
		kept int
		read bool
	}{{"at the bound", codexWalkBound, true}, {"a byte past it", codexWalkBound + 1, false}} {
		head, tail := `{"type":"user","message":{"role":"user","parts":[{"text":"`, `"},`+media+`]}}`
		line := head + strings.Repeat("x", c.kept-1-len(head)-len(tail)+data) + tail
		if got := len(line) + 1 - data; got != c.kept {
			t.Fatalf("%s: the line keeps %d bytes, want %d", c.name, got, c.kept)
		}
		_, err := countChat(h.release, h.release, func() error {
			return chatFileLines(t.Context(), strings.NewReader(h.base+"\n"+line+"\n"), h.walk(), new(passBudget), nil)
		})
		if (err == nil) != c.read {
			t.Errorf("%s: the count reads it %v (%v)", c.name, err == nil, err)
		}
	}
}

// READING A CHAT LINE'S MEDIA HOLDS NONE OF IT: the count's, a Gemini CLI
// collection's and a Qwen Code person's read's own allocation over media of
// 12 MiB and 48 MiB is within a mebibyte of its allocation over a kibibyte's,
// at its least of five runs.
//
// Mutation: hold a medium's bytes: an allocation grows with it.
func TestReadingAChatLinesMediaHoldsNoneOfIt(t *testing.T) {
	qwen := chatCountHarnessOf(t, "qwen")
	for _, r := range []struct {
		name string
		read func(ctx context.Context, n int) func() error
	}{
		{"Qwen Code's count", func(ctx context.Context, n int) func() error {
			body := []byte(qwen.base + "\n" + qwenMediaLines(n)[2] + "\n")
			return func() error {
				return chatFileLines(ctx, bytes.NewReader(body), qwen.walk(), new(passBudget), nil)
			}
		}},
		{"Gemini CLI's collection", func(ctx context.Context, n int) func() error {
			body := []byte(geminiMediaLines(n)[2] + "\n")
			takes := []geminiAskedTake{{digest: ItemDigest("one"), until: -1}}
			return func() error {
				_, err := geminiConfirmed(ctx, bytes.NewReader(body), takes, maxLineBytes)
				return err
			}
		}},
		{"Qwen Code's person's read", func(ctx context.Context, n int) func() error {
			archive := boundedArchiveOf(t, qwenMembers([]string{qwenMediaLines(n)[0]})...)
			return func() error {
				_, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(ctx, QwenCode{},
					bytes.NewReader(archive))
				return err
			}
		}},
	} {
		small := leastAllocation(t, r.read(t.Context(), 1<<10))
		for _, size := range []int{12 << 20, 48 << 20} {
			if got := leastAllocation(t, r.read(t.Context(), size)); got > small+1<<20 {
				t.Errorf("%s allocates %d bytes over %d bytes of media, %d over a kibibyte's", r.name, got, size, small)
			}
		}
	}
}
