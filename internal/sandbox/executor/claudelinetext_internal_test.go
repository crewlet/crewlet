package executor

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE ORACLES: the whole-text rule each automaton of claudelinetext.go answers
// as, in any pieces ([TestATextsFactsAreReadAsItsWholeTextReadsThem]).

// handbackNotice reports whether a task notification's text names
// [claudeToolHandback] anywhere: [textHandback]'s rule.
func handbackNotice(text string) bool { return strings.Contains(text, claudeToolHandback) }

// taskNotification is the subagent and the leg a task notification's text
// names, read whole: [textNotice]'s rule.
func taskNotification(text string) (agent, leg string, ok bool) {
	line, rest := nextLine(text)
	if line != "<task-notification>" {
		return "", "", false
	}
	line, rest = nextLine(rest)
	if agent, ok = enclosed(line, "<task-id>", "</task-id>"); !ok || !validClaudeAgent(agent) {
		return "", "", false
	}
	line, rest = nextLine(rest)
	if leg, ok = enclosed(line, "<tool-use-id>", "</tool-use-id>"); !ok || !sandbox.ValidCallID(leg) {
		return "", "", false
	}
	for {
		line, rest = nextLine(rest)
		if _, file := enclosed(line, "<output-file>", "</output-file>"); !file {
			break
		}
	}
	if line != claudeNotificationCompleted {
		return "", "", false
	}
	return agent, leg, true
}

// nextLine is text's first line, its break dropped, and what follows it.
func nextLine(text string) (line, rest string) {
	line, rest, _ = strings.Cut(text, "\n")
	return line, rest
}

// enclosed is what line holds between opening and closing, where line is
// exactly that.
func enclosed(line, opening, closing string) (string, bool) {
	inner, ok := strings.CutPrefix(line, opening)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(inner, closing)
}

// utf16Units is text's length in UTF-16 code units, as JavaScript counts it:
// [textUnits]'s rule.
func utf16Units(text []byte) int {
	units := 0
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		units++
		if r > 0xffff {
			units++
		}
		text = text[size:]
	}
	return units
}

// ltFacts is every fact the automata read of one text.
type ltFacts struct {
	Says       bool
	Notice     bool
	Agent, Leg string
	Handback   bool
	Result     threadResultText
	Units      int
}

// ltWhole is what the oracles read of text, held whole.
func ltWhole(text string) ltFacts {
	f := ltFacts{
		Says:     strings.ContainsFunc(text, jsTrimKeeps),
		Handback: handbackNotice(text),
		Units:    min(utf16Units([]byte(text)), skillListingMaxNameUnits+1),
	}
	f.Agent, f.Leg, f.Notice = taskNotification(text)
	f.Result.size = int64(len(text)) + 1
	f.Result.Opening = sha256.Sum256([]byte(text[:min(sandbox.HoldingsOpeningBytes, len(text))]))
	if len(text) <= sandbox.MaxBridgeOutputBytes {
		f.Result.Whole = sha256.Sum256([]byte(text))
	}
	return f
}

// ltStreamed is what the automata read of a text handed them as pieces.
func ltStreamed(pieces [][]byte) ltFacts {
	var (
		says     textSays
		notice   textNotice
		handback textHandback
		result   textResult
		units    textUnits
	)
	for _, p := range pieces {
		says.write(p)
		notice.write(p)
		handback.write(p)
		result.write(p)
		units.write(p)
	}
	notice.end()
	f := ltFacts{Says: says.any, Handback: handback.named, Result: result.sum(), Units: units.units()}
	f.Agent, f.Leg, f.Notice = notice.notice()
	return f
}

// ltPieces is text cut into pieces of whole characters, the i-th at most
// size(i) bytes long but never shorter than one character, as jsonscan's Read
// hands a text into a buffer of that size.
func ltPieces(text string, size func(i int) int) [][]byte {
	var pieces [][]byte
	for i := 0; len(text) > 0; i++ {
		want, end := size(i), 0
		for end < len(text) {
			_, w := utf8.DecodeRuneInString(text[end:])
			if end > 0 && end+w > want {
				break
			}
			end += w
		}
		pieces = append(pieces, []byte(text[:end]))
		text = text[end:]
	}
	return pieces
}

// ltCut is text as [textCut] hands it out, each piece copied.
func ltCut(text string) [][]byte {
	var pieces [][]byte
	cut := textCut{text: text}
	for p := cut.next(); p != nil; p = cut.next() {
		pieces = append(pieces, slices.Clone(p))
	}
	return pieces
}

// ltNotification is a task notification naming ctAgent's leg leg, with files
// as its output files' lines, then status, then tail.
func ltNotification(leg string, files []string, status, tail string) string {
	var b strings.Builder
	b.WriteString("<task-notification>\n<task-id>" + ctAgent + "</task-id>\n<tool-use-id>" + leg + "</tool-use-id>\n")
	for _, f := range files {
		b.WriteString(f + "\n")
	}
	b.WriteString(status + tail)
	return b.String()
}

// ltSeeds are the texts every automaton is held to its rule on: the shapes a
// task notification's reading turns on, a handback's name, the characters
// trim() keeps and drops, the bounds of a result's digests and of a skill's
// name, and texts a cut ends inside a character of.
func ltSeeds() []string {
	const leg = "toolu_test_leg"
	done := claudeNotificationCompleted
	file := "<output-file>/tmp/box/" + ctAgent + ".output</output-file>"
	many := make([]string, 10_000)
	for i := range many {
		many[i] = fmt.Sprintf("<output-file>/tmp/box/%d.output</output-file>", i)
	}
	seeds := []string{
		"", " ", "\t\n\r\v\f", "\u00a0\u2028\u2029\ufeff\u3000", "\u2028", "\ufeff", "\u0085", " \u0085 ", "x",
		"   said   ",
		ltNotification(leg, nil, done, ""),
		ltNotification(leg, nil, done, "\n<summary>SubagentHandback</summary>"),
		ltNotification(leg, []string{file, file}, done, "\n<result>done</result>\n"),
		ltNotification(leg, many, done, "\n"),
		ltNotification(leg, []string{"<output-file>" + strings.Repeat("p", 9<<20) + "</output-file>"}, done, ""),
		ltNotification(leg, []string{"<output-file></output-file>"}, done, ""),
		ltNotification(leg, []string{"<output-file>/output-file>"}, done, ""),
		ltNotification(leg, []string{"<output-file>a\r</output-file>"}, done, ""),
		ltNotification(leg, []string{"<output-file>" + strings.Repeat("p", 200)}, done, ""),
		ltNotification(leg, []string{"<output-file>" + strings.Repeat("p", 20)}, done, ""),
		ltNotification(leg, []string{strings.Repeat("p", 200) + "</output-file>"}, done, ""),
		ltNotification(leg, []string{file + "\r"}, done, ""),
		ltNotification(leg, nil, done+"\r", ""),
		ltNotification(leg, nil, done+" ", ""),
		ltNotification(leg, nil, "<status>killed</status>", ""),
		ltNotification(leg, []string{file}, "", ""),
		ltNotification(leg, []string{file}, "", "\n"),
		ltNotification(leg, nil, "", ""),
		ltNotification(strings.Repeat("t", sandbox.MaxBridgeCallIDBytes), nil, done, ""),
		ltNotification(strings.Repeat("t", sandbox.MaxBridgeCallIDBytes+1), nil, done, ""),
		ltNotification("a b", nil, done, ""),
		strings.Replace(ltNotification(leg, nil, done, ""), ctAgent, strings.Repeat("a", claudeAgentMaxBytes), 1),
		strings.Replace(ltNotification(leg, nil, done, ""), ctAgent, strings.Repeat("a", claudeAgentMaxBytes+1), 1),
		strings.ReplaceAll(ltNotification(leg, nil, done, ""), "\n", "\r\n"),
		strings.Replace(ltNotification(leg, nil, done, ""), "<task-notification>", "<task-notification> ", 1),
		"<task-notification>", "<task-notification>\n", "\n<task-notification>\n",
		"SubagentHandback", "a SubagentHandbac", "ubagentHandback", "SubagentHandbackSubagentHandback",
		strings.Repeat("S", 40) + "SubagentHandback",
		strings.Repeat("a", textPieceBytes-1) + "\u20ac" + " SubagentHandback",
		strings.Repeat(" ", textPieceBytes-1) + "\u3000",
		strings.Repeat(" ", textPieceBytes-2) + "\U0001f600",
		strings.Repeat("\x80", textPieceBytes+7) + "\u3000",
		"a byte \xff here", "\xe2\x82", "\xf0\x9f\x98",
		strings.Repeat("a", skillListingMaxNameUnits), strings.Repeat("a", skillListingMaxNameUnits+1),
		strings.Repeat("\U0001f600", skillListingMaxNameUnits/2),
		strings.Repeat("\U0001f600", skillListingMaxNameUnits/2-1) + "ab",
		strings.Repeat("\U0001f600", skillListingMaxNameUnits/2) + "a",
		strings.Repeat("a", skillListingMaxNameUnits-1) + "\U0001f600",
		strings.Repeat("\u00e9", skillListingMaxNameUnits),
	}
	for _, n := range []int{255, 256, 257, 4095, 4096, 4097, 9 << 20} {
		seeds = append(seeds, strings.Repeat("r", n), strings.Repeat("\u00e9", n/2)+strings.Repeat("r", n%2))
	}
	return seeds
}

// ltTestdataTexts is every string every line of every thread under testdata
// holds, each once: the texts a release wrote.
func ltTestdataTexts(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var texts []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if !seen[v] {
				seen[v] = true
				texts = append(texts, v)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for k, e := range v {
				walk(k)
				walk(e)
			}
		}
	}
	err := filepath.WalkDir(filepath.Join("testdata", "claude"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		lines := bufio.NewScanner(f)
		lines.Buffer(nil, threadLineMax)
		for lines.Scan() {
			var v any
			if json.Unmarshal(lines.Bytes(), &v) == nil {
				walk(v)
			}
		}
		return lines.Err()
	})
	if err != nil {
		t.Fatalf("reading the testdata threads: %v", err)
	}
	if !slices.ContainsFunc(texts, func(text string) bool { return strings.HasPrefix(text, noticeHeader+"\n") }) {
		t.Fatalf("none of the %d texts the testdata threads hold is a task notification", len(texts))
	}
	return texts
}

// ltCheck holds what the automata read of text, in every way of cutting it
// this tries, to what the oracles read of it whole, and textCut's pieces to
// whole characters of it.
func ltCheck(t *testing.T, name, text string, rng *rand.Rand) {
	t.Helper()
	want := ltWhole(text)
	ways := map[string][][]byte{"whole": {[]byte(text)}, "cut": ltCut(text)}
	sizes := []int{51, 4 << 10, 64 << 10}
	if len(text) <= 16<<10 {
		sizes = append(sizes, 1, 2, 7, 8, 13)
	}
	for _, size := range sizes {
		ways[fmt.Sprintf("in pieces of %d", size)] = ltPieces(text, func(int) int { return size })
	}
	most := 300
	if len(text) > 16<<10 {
		most = 64 << 10
	}
	ways["in pieces of random sizes"] = ltPieces(text, func(int) int { return 1 + rng.IntN(most) })
	for way, pieces := range ways {
		if got := ltStreamed(pieces); got != want {
			t.Errorf("%s, read %s, reads as\n%+v\nwant it read as whole\n%+v", name, way, got, want)
		}
	}
	// A CUT NEVER ENDS INSIDE A CHARACTER: every piece holds at most a
	// buffer's bytes, and ends where the whole text's characters do.
	at, decoded := 0, 0 // where the pieces end, and where the characters decoded up to it do
	for i, p := range ways["cut"] {
		if len(p) == 0 || len(p) > textPieceBytes {
			t.Fatalf("%s: textCut's piece %d holds %d bytes, want 1 to %d", name, i, len(p), textPieceBytes)
		}
		at += len(p)
		for decoded < at {
			_, w := utf8.DecodeRuneInString(text[decoded:])
			decoded += w
		}
		if decoded != at {
			t.Errorf("%s: textCut's piece %d ends at byte %d, inside a character", name, i, at)
		}
	}
}

// ltBoundary reports whether at is where a character of text, decoded from
// its start, ends.
func ltBoundary(text string, at int) bool {
	for i := 0; i < at; {
		_, w := utf8.DecodeRuneInString(text[i:])
		i += w
		if i == at {
			return true
		}
	}
	return false
}

// A TEXT'S FACTS ARE READ AS ITS WHOLE TEXT READS THEM: each automaton, handed
// a text in pieces of whole characters (whole, as textCut cuts it, in fixed
// sizes from 1 byte to 64 KiB, and in random sizes), answers as its oracle
// reads the text whole; held on every testdata text, the shapes each rule turns
// on ([ltSeeds]), every two-piece cut of each short seed, and random texts; and
// textCut never cuts inside a character.
//
// Mutation: end a notification's line at a carriage return; read an output
// file's line by its opening alone; hash the whole of a text past 4 KiB; count
// a character past U+FFFF as one unit; drop the handback's carry; or cut a
// whole text inside a character: each turns this test red.
func TestATextsFactsAreReadAsItsWholeTextReadsThem(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewChaCha8([32]byte{'t', 'e', 'x', 't'}))
	seeds := ltSeeds()
	for i, text := range seeds {
		ltCheck(t, fmt.Sprintf("seed %d (%.40q)", i, text), text, rng)
		if len(text) > 300 {
			continue
		}
		want := ltWhole(text)
		for at := 1; at < len(text); at++ {
			if !ltBoundary(text, at) {
				continue
			}
			if got := ltStreamed([][]byte{[]byte(text[:at]), []byte(text[at:])}); got != want {
				t.Errorf("seed %d (%.40q), cut at byte %d, reads as\n%+v\nwant\n%+v", i, text, at, got, want)
			}
		}
	}
	for i, text := range ltTestdataTexts(t) {
		ltCheck(t, fmt.Sprintf("testdata text %d (%.40q)", i, text), text, rng)
	}
	// Random texts of the notification's lines, the handback's name, the
	// characters trim() reads, and bytes that are no UTF-8, in random order.
	parts := []string{"\n", "\r", " ", "\u3000", "\U0001f600", "\xff", "x", "<task-notification>",
		"<task-id>" + ctAgent + "</task-id>", "<tool-use-id>toolu_x</tool-use-id>", "<output-file>",
		"</output-file>", claudeNotificationCompleted, "Subagent", "Handback", claudeToolHandback}
	for i := range 2000 {
		var b strings.Builder
		for range rng.IntN(40) {
			b.WriteString(parts[rng.IntN(len(parts))])
		}
		ltCheck(t, fmt.Sprintf("random text %d (%.40q)", i, b.String()), b.String(), rng)
	}
}

// A RESULT'S TEXT'S ZERO VALUE IS NO TEXT: a tool_result whose content holds
// no text (absent, null, an image's blocks, two text blocks) reads as the zero
// value, -1 bytes, as does a tool_use's call; and a text of no bytes is a text,
// 0 bytes long with the empty text's digests, never the zero value.
//
// Mutation: read the zero value's length as 0, or store a text's length as
// written, and the two are confused.
func TestAResultTextsZeroValueIsNoText(t *testing.T) {
	t.Parallel()
	if n := (threadResultText{}).Bytes(); n != -1 {
		t.Errorf("the zero value's text is %d bytes long, want -1, no text", n)
	}
	empty := sha256.Sum256(nil)
	for content, want := range map[string]threadResultText{
		``:     {},
		`null`: {},
		`[{"type":"image","source":{"type":"base64","data":"AA=="}}]`: {},
		`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`:     {},
		`""`:                          {size: 1, Opening: empty, Whole: empty},
		`[{"type":"text","text":""}]`: {size: 1, Opening: empty, Whole: empty},
		`"b1"`:                        ltWhole("b1").Result,
	} {
		raw := json.RawMessage(content)
		if content == "" {
			raw = nil
		}
		if got := resultTextOf(raw); got != want {
			t.Errorf("a result whose content is %s reads as %d bytes, opening %x, want %d bytes, opening %x",
				content, got.Bytes(), got.Opening, want.Bytes(), want.Opening)
		}
	}
	c, err := blockCallOf('{', blockKeys{typ: json.RawMessage(`"tool_use"`), id: json.RawMessage(`"toolu_x"`),
		name: json.RawMessage(`"Bash"`)})
	if err != nil || c.Text != (threadResultText{}) {
		t.Errorf("a tool_use's call holds a text of %d bytes (%v), want none", c.Text.Bytes(), err)
	}
}

// FuzzATextsFactsAreReadAsItsWholeTextReadsThem holds the automata to their
// rules on any text in any pieces ([TestATextsFactsAreReadAsItsWholeTextReadsThem]).
func FuzzATextsFactsAreReadAsItsWholeTextReadsThem(f *testing.F) {
	for i, text := range ltSeeds() {
		if len(text) <= 64<<10 {
			f.Add(text, uint64(i))
		}
	}
	f.Fuzz(func(t *testing.T, text string, seed uint64) {
		ltCheck(t, "the text", text, rand.New(rand.NewPCG(seed, seed)))
	})
}
