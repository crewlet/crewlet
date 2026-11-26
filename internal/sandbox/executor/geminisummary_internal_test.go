package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// geminiSummaryWhole is the whole-line replay a Gemini CLI summary read on its
// count's walk is held to ([geminiSummaryCapture]): each line decoded whole,
// every record kept by id as the read before the walk took the summary (#2020)
// kept them, with what the walk decides applied where it decides it: a record
// the count takes no copy of has no place (a line holding a separator or no
// object, an entry of no id or of a type but `user` and `gemini`, a patch
// removing a value that is no id); one gone stays gone, a summary of it
// unsaid; a summary on a line the count could not take unsaid. No chat file it
// reads reaches the count's bounds. The snapshots' bound and the newest's choice
// are the capture's own ([geminiSummaryCapture.add]), held by the named cases.
type geminiSummaryWhole struct {
	geminiSummaryCapture
	order                         []string
	removed, snapshot, gone, seen map[string]bool
	compressions                  geminiCompressionLines[string]
	pending                       string
	pendingWords                  summaryText
	pendingOn, refused            bool
	pendingAt                     int
}

func newGeminiSummaryWhole() *geminiSummaryWhole {
	return &geminiSummaryWhole{geminiSummaryCapture: geminiSummaryCapture{first: -1, unknown: -1},
		removed: map[string]bool{}, snapshot: map[string]bool{}, gone: map[string]bool{}, seen: map[string]bool{}}
}

// geminiLineWhole is a chat file line as the replay decodes it.
type geminiLineWhole struct {
	ID        json.RawMessage `json:"id"`
	Type      json.RawMessage `json:"type"`
	Content   json.RawMessage `json:"content"`
	Patch     json.RawMessage `json:"$patch"`
	Set       json.RawMessage `json:"$set"`
	Rewind    json.RawMessage `json:"$rewindTo"`
	SessionID json.RawMessage `json:"sessionId"`
}

// geminiPatchWhole is a `$patch` as the replay decodes it: its updates and
// removals, and whether it is of a shape the count reads at all.
type geminiPatchWhole struct {
	updates, removes []json.RawMessage
	shaped           bool
}

func geminiPatchOf(raw json.RawMessage) geminiPatchWhole {
	var p struct {
		Updates   json.RawMessage `json:"updates"`
		RemoveIDs json.RawMessage `json:"removeIds"`
		OrderIDs  json.RawMessage `json:"orderIds"`
	}
	var out geminiPatchWhole
	out.shaped = wireObject(raw) && wireAlias(raw, &p) == nil && absent(p.OrderIDs) &&
		(absent(p.Updates) || wireDecode(p.Updates, &out.updates) == nil) &&
		(absent(p.RemoveIDs) || wireDecode(p.RemoveIDs, &out.removes) == nil) &&
		!slices.ContainsFunc(out.updates, func(u json.RawMessage) bool { return !wireObject(u) })
	return out
}

// geminiIDWhole is raw as a record's id the replay reads: a string within the
// walk's word bound, else "".
func geminiIDWhole(raw json.RawMessage) string {
	if id := looseString(raw); len(id) <= transcriptNameMaxBytes {
		return id
	}
	return ""
}

// geminiWordsOf is content's words as a summary's ([geminiWords]).
func geminiWordsOf(content json.RawMessage) summaryText {
	var words summaryText
	for _, part := range geminiParts(content) {
		if part.Text != nil && !part.Thought {
			words.set = true
			if text := chatTrim(*part.Text, TranscriptValueMaxBytes); len(text) > summaryReadMaxBytes {
				words.past = true
			} else {
				words.buf = []byte(text)
			}
			break
		}
	}
	return words
}

// take takes the copies of records the count takes off line, w its decode
// where object, and is the id of the entry it took, "" for none.
func (g *geminiSummaryWhole) take(raw []byte, w *geminiLineWhole, object bool) string {
	place := func(id string) {
		if !g.seen[id] {
			g.seen[id] = true
			g.order = append(g.order, id)
		}
	}
	if !object || lineSeparatorsWhole(raw) {
		return ""
	}
	switch {
	case !absent(w.Patch):
		p := geminiPatchOf(w.Patch)
		if !p.shaped || slices.ContainsFunc(p.removes, func(r json.RawMessage) bool {
			return !absent(r) && r[0] != '"'
		}) {
			return ""
		}
		for _, u := range p.updates {
			var update geminiLineWhole
			if wireAlias(u, &update) == nil && looseString(update.ID) != "" {
				place(looseString(update.ID))
			}
		}
		for _, r := range p.removes {
			g.removed[looseString(r)] = true
		}
	case !absent(w.Set), !absent(w.Rewind), !absent(w.SessionID) && absent(w.Type):
	default:
		if kind := looseString(w.Type); kind == "user" || kind == "gemini" {
			if id := looseString(w.ID); id != "" {
				place(id)
				return id
			}
		}
	}
	return ""
}

// line replays raw, the next line, object where it is one JSON object.
func (g *geminiSummaryWhole) line(raw []byte, object bool) {
	g.lineStart = len(g.order)
	var w geminiLineWhole
	object = object && json.Valid(raw) && wireObject(raw) && wireAlias(raw, &w) == nil
	opens := g.compressions.set && geminiCompressionUser.holds(raw)
	took := g.take(raw, &w, object)
	user := ""
	if took != "" && looseString(w.Type) == "user" {
		user = took
	}
	prior, summary := g.compressions.line(raw, user, user != "")
	if summary {
		g.snapshot[prior] = true
	}
	if g.pendingOn {
		g.pendingOn = false
		switch {
		case !g.pendingWords.set:
		case g.refused && geminiCompressionAck.holds(raw):
			g.unsaid(g.pending, g.pendingAt, "a compression's summary is on a line the count could not read")
		case !g.refused && summary && prior == g.pending:
			g.summary(prior, g.pendingWords)
		}
	}
	switch {
	case !object:
	case len(w.Patch) > 0:
		g.patch(w.Patch)
	case len(w.Rewind) > 0:
		g.rewind(geminiIDWhole(w.Rewind))
	default:
		id := geminiIDWhole(w.ID)
		if id == "" {
			return
		}
		g.drop(func(n geminiSnap) bool { return n.id == id })
		if opens && (user != "" || took != id) {
			g.pending, g.pendingOn, g.refused, g.pendingAt = id, true, took != id, 2*g.lineStart
			g.pendingWords = geminiWordsOf(w.Content)
		}
	}
}

// patch replays a `$patch`: each update of a summary a copy of it, each
// removal gone; one of no shape loses the replay.
func (g *geminiSummaryWhole) patch(raw json.RawMessage) {
	p := geminiPatchOf(raw)
	if !p.shaped {
		g.lost("a patch of the chat file is of no shape this read follows")
		return
	}
	for _, raw := range p.updates {
		var u geminiLineWhole
		if wireAlias(raw, &u) != nil {
			continue
		}
		if id := geminiIDWhole(u.ID); id != "" && g.snapshot[id] {
			g.drop(func(n geminiSnap) bool { return n.id == id })
			if looseString(u.Type) == "user" {
				g.summary(id, geminiWordsOf(u.Content))
			}
		}
	}
	for _, raw := range p.removes {
		if id := geminiIDWhole(raw); id != "" {
			g.drop(func(n geminiSnap) bool { return n.id == id })
		}
	}
}

// rewind replays a `$rewindTo` id: it and every later record gone.
func (g *geminiSummaryWhole) rewind(id string) {
	at := slices.Index(g.order, id)
	if id == "" || at < 0 || g.removed[id] || g.gone[id] {
		g.lost("the chat file rewinds to a record its history does not hold")
		return
	}
	g.drop(func(n geminiSnap) bool { return n.place > 2*at })
	for _, later := range g.order[at:] {
		g.gone[later] = true
	}
}

// summary makes words, where a text was read, the snapshot of the record
// id, unsaid where it was gone.
func (g *geminiSummaryWhole) summary(id string, words summaryText) {
	if !words.set {
		return
	}
	at := slices.Index(g.order, id)
	if at < 0 || g.removed[id] || g.gone[id] {
		g.unsaid(id, 2*g.lineStart, "a compression's summary is a record the chat file had let go of")
		return
	}
	snap := geminiSnap{id: id, place: 2*at + 1, text: string(words.buf)}
	if words.past {
		snap.text, snap.unsaid = "", errSummaryPast.Error()
	}
	g.add(snap)
}

// geminiSummaryWholeOf is chat's newest summary as the whole-line replay
// reads it, each line held whole to [codexRolloutLineMaxBytes] as the count's
// whole-line read holds it ([chatFileLinesWhole]): a line with white space
// about it JSON takes no blank for is no object.
func geminiSummaryWholeOf(chat string) sandbox.ThreadSummary {
	g := newGeminiSummaryWhole()
	br := bufio.NewReaderSize(strings.NewReader(chat), 64<<10)
	var err error
	for n := 1; err == nil; n++ {
		line, past, readErr := codexRolloutLine(br)
		switch {
		case errors.Is(readErr, io.EOF):
			return g.reading()
		case readErr != nil:
			err = fmt.Errorf("executor: reading chat file line %d: %w", n, readErr)
		case past:
			err = fmt.Errorf("%w: line %d", errChatLine, n)
		default:
			if body := bytes.TrimSpace(line); len(body) > 0 {
				g.line(body, bytes.Equal(body, bytes.Trim(line, " \t\r\n")))
			}
		}
	}
	g.read.fail("the session's chat file was not read to its end: " + err.Error())
	return g.reading()
}

// geminiSummaryOnTheWalk is chat's newest summary as a box read's count reads
// it on its walk ([GeminiCLI.readMainThread]).
func geminiSummaryOnTheWalk(ctx context.Context, chat string) sandbox.ThreadSummary {
	var large passBudget
	defer large.release()
	c := newGeminiCounting()
	c.summarize(newSummaryClaim(ctx, &large))
	return c.summary(chatFileLines(ctx, strings.NewReader(chat), c, &large))
}

// geminiSummaryString is a summary in a line: its size and digest, why it is
// unread, or none.
func geminiSummaryString(s sandbox.ThreadSummary) string {
	switch {
	case s.Unread != "":
		return "unread: " + s.Unread
	case s.Text == "":
		return "none"
	}
	sum := sha256.Sum256([]byte(s.Text))
	return fmt.Sprintf("text:%d:%x", len(s.Text), sum[:8])
}

// A GEMINI CLI SUMMARY ON THE COUNT'S WALK IS ITS WHOLE-LINE REPLAY: the
// newest summary a box read finds on its count's walk ([geminiSummaryCapture])
// is the whole-line replay's ([geminiSummaryWhole]) over every chat file the
// count's differential reads (eachChatCountRecord): each recording, each line
// of every shape the count reads alone, torn, after each recording and all at
// once, and lines at and past the bound.
//
// Mutation: place a record by its newest copy, keep a rewind's records, take
// a thought for a summary's words, or let a removal pass: a case fails.
func TestAGeminiSummaryOnTheCountsWalkIsItsWholeLineReplay(t *testing.T) {
	t.Parallel()
	eachChatCountRecord(chatCountHarnessOf(t, "gemini"), func(name, record string) {
		got, want := geminiSummaryOnTheWalk(t.Context(), record), geminiSummaryWholeOf(record)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the summary on the walk reads %s, want %s", name, geminiSummaryString(got),
				geminiSummaryString(want))
		}
	})
}

// A GEMINI CLI SUMMARY IS THE NEWEST STANDING IN ITS HISTORY: a removal or a
// rewind past the newer reads the one before; a patch's copy as a user entry
// is it, one of another kind takes it away; its words are its first text a
// thought is not, trimmed; a blank newest is none, one past the bound unread;
// a patch of no shape or a rewind to no record standing leaves those before
// it unsaid; its reply is judged as the line trimmed whole, its white space
// past the head too, never on a torn last line (a case named so). A summary
// on a line the count cannot read, or of a record gone and written again
// (never written by Gemini CLI), is unsaid.
//
// Mutation: drop the rewind's spans, the removal of a summary, the trim, the
// unsaid summary, or judge a torn last line: a case fails.
func TestAGeminiSummaryIsTheNewestStandingInItsHistory(t *testing.T) {
	t.Parallel()
	user := func(id, content string) string {
		return `{"id":"` + id + `","timestamp":"t","type":"user","content":` + content + `}`
	}
	text := func(s string) string { return `[{"text":"` + s + `"}]` }
	ack := func(id string) string {
		return `{"id":"` + id + `a","timestamp":"t","type":"gemini","content":[{"text":"` + geminiAck + `"}]}`
	}
	comp := func(id, content string) string {
		return `{"$set":{"sessionId":"s"}}` + "\n" + user(id, content) + "\n" + ack(id)
	}
	spaces := func(n int) string { return strings.Repeat(" ", n-len(ack("s1"))) }
	rec := func(id string) string { return `{"id":"` + id + `","type":"user","content":"x"}` }
	update := func(id, rest string) string { return `{"$patch":{"updates":[{"id":"` + id + `"` + rest + `}]}}` }
	big := strings.Repeat("w", 1_500_000)
	const (
		unshaped = "unread: a patch of the chat file is of no shape this read follows"
		rewound  = "unread: the chat file rewinds to a record its history does not hold"
		unread   = "unread: a compression's summary is on a line the count could not read"
		regone   = "unread: a compression's summary is a record the chat file had let go of"
	)
	words := func(s string) string { return geminiSummaryString(sandbox.ThreadSummary{Text: s}) }
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"two compressions", []string{rec("a"), comp("s1", text("one")), rec("b"), comp("s2", text("two"))},
			words("two")},
		{"the newer removed", []string{comp("s1", text("one")), comp("s2", text("two")),
			`{"$patch":{"removeIds":["s2"]}}`}, words("one")},
		{"a rewind past the newer", []string{rec("a"), comp("s1", text("one")), rec("b"), comp("s2", text("two")),
			`{"$rewindTo":"b"}`}, words("one")},
		{"a rewind past both", []string{rec("a"), comp("s1", text("one")), `{"$rewindTo":"a"}`, rec("c")}, "none"},
		{"a rewind, then a newer", []string{rec("a"), comp("s1", text("one")), rec("b"), comp("s2", text("two")),
			`{"$rewindTo":"b"}`, rec("c"), comp("s3", text("three"))}, words("three")},
		{"a rewind to a record rewound", []string{rec("a"), comp("s1", text("one")), rec("b"),
			comp("s2", text("two")), `{"$rewindTo":"b"}`, rec("c"), comp("s3", text("three")),
			`{"$rewindTo":"b"}`}, rewound},
		{"a rewind to no record", []string{comp("s1", text("one")), `{"$rewindTo":"zz"}`}, rewound},
		{"a rewind to null", []string{comp("s1", text("one")), `{"$rewindTo":null}`}, rewound},
		{"a patch's copy as a user entry", []string{comp("s1", text("one")),
			update("s1", `,"type":"user","content":[{"text":"one again"}]`)}, words("one again")},
		{"a patch's copy, its id last", []string{comp("s1", text("one")),
			`{"$patch":{"updates":[{"content":[{"text":"one again"}],"id":"s1","type":"user"}]}}`},
			words("one again")},
		{"a patch's copy of no type", []string{comp("s1", text("one")), comp("s2", text("two")),
			update("s2", `,"content":[{"text":"x"}]`)}, words("one")},
		{"a full line's copy", []string{comp("s1", text("one")), rec("s1")}, "none"},
		{"a thought before its words", []string{
			comp("s1", `[{"text":"thinking","thought":true},{"text":"  real\n"}]`)}, words("real")},
		{"a blank newer", []string{comp("s1", text("one")), comp("s2", text(" \\n "))}, "none"},
		{"a newer of no text", []string{comp("s1", text("one")), comp("s2", `[{"functionCall":{"name":"n"}}]`)},
			words("one")},
		{"one past the bound", []string{comp("s1", text(strings.Repeat("w", summaryReadMaxBytes+1)))},
			"unread: " + errSummaryPast.Error()},
		{"the older let go for the newer", []string{comp("s1", text(big)), comp("s2", text("two"+big))},
			words("two" + big)},
		{"the older let go, the newer removed", []string{comp("s1", text(big)), comp("s2", text("two"+big)),
			`{"$patch":{"removeIds":["s2"]}}`}, "unread: " + errSummaryPast.Error()},
		{"a summary's opening and no reply", []string{`{"$set":{"sessionId":"s"}}`, user("s1", text("one")),
			rec("x")}, "none"},
		{"a patch of no shape", []string{comp("s1", text("one")), `{"$patch":null,"id":"r","type":"user"}`},
			unshaped},
		{"a patch that orders, then a newer", []string{comp("s1", text("one")), `{"$patch":{"orderIds":["s1"]}}`,
			comp("s2", text("two"))}, words("two")},
		{"a removal of no id", []string{comp("s1", text("one")), comp("s2", text("two")),
			`{"$patch":{"removeIds":["s2",5]}}`}, words("one")},
		{"a patch holding a separator", []string{comp("s1", text("one")), comp("s2", text("two")),
			`{"$patch":{"removeIds":["s2"]},"x":" "}`}, words("one")},
		{"a separator in the newer", []string{comp("s1", text("one")), comp("s2", text(`two `))}, unread},
		{"a key twice in the newer", []string{comp("s1", text("one")), comp("s2", `[{"text":"a","text":"b"}]`)},
			unread},
		{"a summary written again once removed", []string{comp("s1", text("one")), comp("s2", text("two")),
			`{"$patch":{"removeIds":["s2"]}}`, comp("s2", text("two again"))}, regone},
		{"a summary written again once rewound", []string{rec("a"), comp("s1", text("one")),
			`{"$rewindTo":"a"}`, comp("s1", text("again"))}, regone},
		{"a reply led past the head", []string{`{"$set":{"sessionId":"s"}}`, user("s1", text("one")),
			spaces(3*lineFormMaxBytes) + ack("s1")}, words("one")},
		{"a reply trailed past the head", []string{`{"$set":{"sessionId":"s"}}`, user("s1", text("one")),
			ack("s1") + spaces(lineFormMaxBytes+1)}, words("one")},
		{"a reply led past the head, torn", []string{`{"$set":{"sessionId":"s"}}`, user("s1", text("one")),
			spaces(lineFormMaxBytes+1) + ack("s1")}, "none"},
	} {
		chat := `{"sessionId":"s","projectHash":"h","startTime":"t","kind":"main"}` + "\n" +
			strings.Join(c.lines, "\n")
		if !strings.HasSuffix(c.name, ", torn") {
			chat += "\n"
		}
		if got := geminiSummaryString(geminiSummaryOnTheWalk(t.Context(), chat)); got != c.want {
			t.Errorf("%s: the summary reads %s, want %s", c.name, got, c.want)
		}
	}
}

// EVERY RECORDED GEMINI CLI SESSION'S SUMMARY READS AS IT DID BEFORE THE
// COUNT'S WALK TOOK IT: each chat file under testdata/gemini reads the answer
// the separate replay (#2075) gave it, pinned here when #2020 removed it:
// the newer of the two summaries of 0.70.0's preamble session (line 94, its
// `<scratchpad>` first), each compressed run's newest, and none elsewhere.
//
// Mutation: find a summary by its `<state_snapshot>` opening: a recording
// fails.
func TestEveryRecordedGeminiSummaryReadsAsBeforeTheCountsWalk(t *testing.T) {
	t.Parallel()
	pinned := map[string]string{
		"count/compressed.run1.chat.jsonl.gz":    "text:68:a81f0666814edded",
		"count/compressed.run2.chat.jsonl.gz":    "text:68:a81f0666814edded",
		"holdings/compressed.run1.chat.jsonl.gz": "text:68:a81f0666814edded",
		"holdings/compressed.run2.chat.jsonl.gz": "text:68:a81f0666814edded",
		"holdings/round.snapshot.chat.jsonl.gz":  "text:42:666999391d14a21c",
		"session-compressed.jsonl":               "text:59:42d77b665f1f641b",
		"session-compressed-preamble.jsonl":      "text:117:49460fc75dae3953",
	}
	fixtures := chatFixtures(t, "gemini/0.70.0", "session-*.jsonl")
	met := 0
	for _, f := range fixtures {
		name := filepath.ToSlash(strings.TrimPrefix(f.name, filepath.Join("testdata", "gemini", "0.70.0")+"/"))
		want, ok := pinned[name]
		if ok {
			met++
		} else {
			want = "none"
		}
		got := geminiSummaryString(geminiSummaryOnTheWalk(t.Context(), strings.Join(f.lines, "\n")+"\n"))
		if got != want {
			t.Errorf("%s: the summary reads %s, want %s", name, got, want)
		}
	}
	if met != len(pinned) || len(fixtures) < 59 {
		t.Errorf("read %d recordings, %d of the %d pinned; want every one", len(fixtures), met, len(pinned))
	}
}

// What a Gemini CLI box read asking the newest summary may allocate beyond
// its count alone ([TestAGeminiSummaryAddsOnlyItsWordsToTheCount]): six times
// the summary's words, as their buffer grows by a quarter at a time a trimmed
// word at a time (Go's append past 256 bytes: at most five times what it
// ends at) and the string the answer holds is one more; and the capture's own
// state, 456 bytes measured beside a summary of 33, at 4 KiB for the heap's
// size classes. A place kept for each of the case's 2,040 ids is past both.
const (
	geminiSummaryWordsAllocates = 6
	geminiSummaryStateAllocates = 4 << 10
)

// A GEMINI CLI SUMMARY ADDS ONLY ITS WORDS TO THE COUNT: a box read asking the
// newest summary of a session of 2,040 records, each id at the walk's word
// bound, allocates at most what the count alone does, the summary's words
// [geminiSummaryWordsAllocates] times over, and [geminiSummaryStateAllocates]:
// no place of any id of its own, which the replay apart from the count kept
// (1,041,048 bytes more than the count for a summary of 33, measured).
//
// Not parallel: it reads the process's allocation count, the fewest bytes of
// three reads ([allocationsOf]).
//
// Mutation: keep each record's place by its id in the capture: this goes red.
func TestAGeminiSummaryAddsOnlyItsWordsToTheCount(t *testing.T) {
	const session = "2053a11c-0c0c-4d4d-8e8e-000000002020"
	summary := "<state_snapshot>" + strings.Repeat("the work so far ", 512) + "</state_snapshot>"
	lines := []string{`{"sessionId":"` + session + `","projectHash":"h","startTime":"t","kind":"main"}`}
	for i := range geminiCountBaseIDs - 8 {
		lines = append(lines, `{"id":"`+chatBenchID(i, transcriptNameMaxBytes)+`","type":"user","content":"x"}`)
	}
	lines = append(lines, `{"$set":{"sessionId":"`+session+`"}}`,
		`{"id":"s1","timestamp":"t","type":"user","content":[{"text":"`+summary+`"}]}`,
		`{"id":"s1a","timestamp":"t","type":"gemini","content":[{"text":"`+geminiAck+`"}]}`)
	box := sandbox.NewFakeSandbox("b")
	name := "session-2026-11-25T00-00-" + session[:8] + ".jsonl"
	box.Put(PathsFor(box).GeminiChats()+"/"+name, strings.Join(lines, "\n")+"\n")
	answering(box, map[string]string{"ls -1 ": name + "\n"})
	read := func(summarize bool) (sandbox.MainThreadReading, uint64) {
		var reading sandbox.MainThreadReading
		ask := sandbox.MainThreadAsk{Session: session, Release: geminiRelease, Count: true, Summary: summarize}
		_, allocated := allocationsOf(t, func() {
			reading = GeminiCLI{}.readMainThread(context.Background(), box, PathsFor(box), ask)
		})
		return reading, allocated
	}
	counted, alone := read(false)
	summarized, both := read(true)
	if counted.Count == nil || counted.Count.Mark != "" || summarized.Summary.Text != summary {
		t.Fatalf("the read counts %s and reads the summary %.40q (%s); want a count and the summary",
			countString(counted.Count), summarized.Summary.Text, summarized.Summary.Unread)
	}
	limit := alone + geminiSummaryWordsAllocates*uint64(len(summary)) + geminiSummaryStateAllocates
	t.Logf("the count alone allocates %d bytes, with the summary of %d bytes %d (%d more)", alone, len(summary),
		both, int64(both)-int64(alone))
	if both > limit {
		t.Errorf("with the summary the read allocates %d bytes, %d past the count's %d; want at most %d", both,
			both-alone, alone, limit)
	}
}
