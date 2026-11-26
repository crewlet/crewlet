package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// imageURL is an image's data URL n bytes long, its data base64 of zeros.
func imageURL(n int) string {
	const head = "data:image/png;base64,"
	return head + strings.Repeat("A", n-len(head))
}

// The lines that hold an image in every place Codex 0.171.0 writes one: a
// person's message's part, a call's output list, the event that repeats a
// person's message, an MCP call's end (its image raw, no data URL) and a
// compaction's replacement history.
func codexImageMessage(url string) string {
	return `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text",` +
		`"text":"look"},{"type":"input_image","image_url":"` + url + `"},{"type":"input_text","text":"after"}],` +
		`"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`
}

func codexImageOutput(url string) string {
	return `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_text","text":"seen"},{"type":"input_image","image_url":"` + url + `"}]}}`
}

func codexImageEvent(url string) string {
	return `{"type":"event_msg","payload":{"type":"user_message","message":"look","images":["` + url + `"]}}`
}

func codexMCPImageEnd(data string) string {
	return `{"type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"c9","result":{"Ok":{"content":` +
		`[{"type":"image","data":"` + data + `","mimeType":"image/png"}]}}}}`
}

func codexCompactedImage(url string) string {
	return `{"type":"compacted","payload":{"message":"the summary","replacement_history":[{"type":"message",` +
		`"role":"user","content":[{"type":"input_image","image_url":"` + url + `"}]}]}}`
}

// codexImageLines is every Codex line that holds the image url (MCP's raw).
func codexImageLines(url string) []string {
	return []string{codexImageMessage(url), codexImageOutput(url), codexImageEvent(url),
		codexMCPImageEnd(strings.TrimPrefix(url, "data:image/png;base64,")), codexCompactedImage(url)}
}

// hermesImageParts is a stored content's parts: words, and the image url.
func hermesImageParts(url string) string {
	return `[{"type": "text", "text": "seen"}, {"type": "image_url", "image_url": {"url": "` + url + `"}}]`
}

// hermesImageRow is a tool's row whose content is stored, holding the image
// url, as a compaction keeps the newest image-bearing results.
func hermesImageRow(id int, url string) string {
	return fmt.Sprintf(`{"t": "messages", "r": {"id": %d, "role": "tool", "tool_call_id": "c1", "content": %s}}`,
		id, hermesStored(hermesImageParts(url)))
}

// imageShown is a page as JSON with every image's sized place as its unsized
// one, each entry's size less what the size added: what the whole-line
// oracle, which counts no image's bytes, shows of it.
func imageShown(s string) string {
	return sizedEntry.ReplaceAllStringFunc(s, func(entry string) string {
		m := sizedEntry.FindStringSubmatch(entry)
		n, err := strconv.Atoi(m[2])
		if err != nil {
			panic(err)
		}
		text := m[1]
		for _, sized := range sizedImage.FindAllString(text, -1) {
			n -= len(sized) - len(claudeImagePlaceholder)
		}
		text = sizedImage.ReplaceAllLiteralString(text, claudeImagePlaceholder)
		return `"text":"` + text + `","bytes":` + strconv.Itoa(n)
	})
}

// noteOne is the take of the note "one", digested as a record holds it.
var noteOne = []ServedTake{{InboxID: "one", Digest: ItemDigest("one")}}

var (
	sizedImage = regexp.MustCompile(`\[an image of [0-9.]+ (?:bytes|KiB|MiB)\]`)
	sizedEntry = regexp.MustCompile(`"text":"((?:[^"\\]|\\.)*)","bytes":([0-9]+)`)
)

// A DATA URL IS TOLD BY ITS HEAD, OF ANY MEDIA: a media type holding a slash,
// printable ASCII parameters and `;base64` before the comma that opens its
// data; a string that could still be one is undecided, one that cannot be is
// none, and one whose head runs past [dataURLHeadMax] is none.
//
// Mutation: take a data URL of no media type, or one with no `;base64`, and a
// row fails.
func TestADataURLIsToldByItsHead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		head string
		want headVerdict
	}{
		{"", headUndecided}, {"dat", headUndecided}, {"data:im", headUndecided}, {"data:image/png", headUndecided},
		{"data:image/png;base64,", headMedia}, {"data:image/png;base64,AAAA", headMedia},
		{"data:image/svg+xml;charset=utf-8;base64,PHN2", headMedia},
		{"data:application/pdf;base64,JVBE", headMedia}, {"data:text/plain;base64,AAAA", headMedia},
		{"data:image/png,AAAA", headNot}, {"data:;base64,AAAA", headNot}, {"data:png;base64,AAAA", headNot},
		{"data:/png;base64,AAAA", headNot},
		{"data:image/png;base64x,", headNot}, {"Data:image/png;base64,", headNot},
		{"data:image/p ng;base64,", headNot}, {`data:image/png\;base64,`, headNot},
		{"look at data:image/png;base64,", headNot},
		{"data:image/" + strings.Repeat("p", dataURLHeadMax), headNot},
	} {
		if got := dataURLHead([]byte(c.head)); got != c.want {
			t.Errorf("%.40q: told %d, want %d", c.head, got, c.want)
		}
	}
}

// A READ THAT PASSES IMAGES IS ITS WHOLE-LINE READ'S BESIDE THEM: over every
// line Codex writes an image in, alone between a brief and a call and all
// together, every recorded rollout with them among its lines, and Hermes rows
// whose list content holds an image part, a person's read, whole and in pieces
// of seventeen bytes, answers the page the whole-line oracle answers but for
// each image's size in its place; a collection's notes and Codex's count are
// the oracle's exactly.
//
// Mutation: hold an image's bytes in its part's text, or show its place
// anywhere the oracle shows none: a case fails.
func TestAReadThatPassesImagesIsItsWholeLineReadBesideThem(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, whole, walk, pieces carrier, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, whole, archive, r)
			if got := imageShown(personReadOf(t, walk, archive, r)); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := imageShown(personReadOf(t, pieces, archive, r)); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text",` +
		`"text":"one"}]}}`
	call := `{"type":"response_item","payload":{"type":"function_call","name":"bash","arguments":"{}","call_id":"c1"}}`
	count := `{"type":"event_msg","ordinal":3,"payload":{"type":"token_count","info":{"last_token_usage":` +
		`{"input_tokens":10,"total_tokens":20},"model_context_window":100}}}`
	var lines []string
	for _, n := range []int{len("data:image/png;base64,"), 300, 3 << 10} {
		lines = append(lines, codexImageLines(imageURL(n))...)
	}
	records := map[string][]string{"every image line": append([]string{brief, call}, lines...)}
	for i, l := range lines {
		records[fmt.Sprintf("image line %d", i)] = []string{brief, call, l, count, brief}
	}
	for _, fixture := range codexRolloutFixtures(t) {
		raw := strings.Split(strings.TrimSuffix(string(codexFixture(t, fixture)), "\n"), "\n")
		records[fixture+" with images"] = append(slices.Clone(raw[:len(raw)/2]), append(lines, raw[len(raw)/2:]...)...)
	}
	takes := []ServedTake{{InboxID: "one", Digest: ItemDigest("one")}}
	for name, record := range records {
		check(name, wholeCodex{}, Codex{}, piecesCodex{n: 17}, boundedArchiveOf(t, codexRolloutMembers(record)...))
		body := strings.Join(record, "\n") + "\n"
		want, wantErr := codexNotesHeldWhole(strings.NewReader(body), takes, 0)
		got, err := codexNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(body), n: 17}, takes, 0, nil)
		if !slices.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("%s: a collection holds %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
		var walk, whole codexCounting
		err = walk.read(t.Context(), &exportPieces{r: strings.NewReader(body), n: 17}, nil)
		wantErr = whole.readWhole(t.Context(), strings.NewReader(body))
		if fmt.Sprint(err) != fmt.Sprint(wantErr) || fmt.Sprintf("%+v", walk) != fmt.Sprintf("%+v", whole) {
			t.Errorf("%s: counts %+v (%v), want %+v (%v)", name, walk, err, whole, wantErr)
		}
	}

	note := `{"t": "messages", "r": {"id": 3, "role": "user", "content": ` +
		boundedQuoted(hermesNoteOpen+"one"+hermesNoteClose) + `, "display_kind": "steer"}}`
	reply := `{"t": "messages", "r": {"id": 9, "role": "assistant", "content": "ok", "finish_reason": "stop"}}`
	listRow := func(id, n int) string {
		return fmt.Sprintf(`{"t": "messages", "r": {"id": %d, "role": "tool", "content": %s}}`, id,
			hermesImageParts(imageURL(n)))
	}
	rows := []string{note, listRow(4, 300), listRow(5, 3<<10), reply}
	check("hermes list contents", wholeHermes{}, Hermes{}, piecesHermes{n: 17},
		boundedArchiveOf(t, hermesMembers(rows)...))
	body := strings.Join(rows, "\n") + "\n"
	want, wantErr := hermesNotesHeldWhole(strings.NewReader(body), noteOne, 0)
	got, err := hermesNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(body), n: 17}, noteOne, 0, nil)
	if !slices.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
		t.Errorf("hermes list contents: a collection holds %v (%v), want %v (%v)", got, err, want, wantErr)
	}
}

// AN IMAGE IS SHOWN BY ITS PLACE AND SIZE, NEVER ITS BYTES, where the oracle
// shows its place alone or its bytes: a Codex message's image part, an output
// list's, a list shown as written where its parts' decode refuses it, an
// output or a text that is a data URL whole, and a Hermes content that is one;
// an image named by no data URL keeps its unsized place.
//
// Mutation: show an image's place unsized, or keep its bytes as written: a
// case fails.
func TestAnImageIsShownByItsPlaceAndSize(t *testing.T) {
	t.Parallel()
	url := imageURL(3 << 10)
	call := `{"type":"response_item","payload":{"type":"function_call","name":"bash","arguments":"{}","call_id":"c1"}}`
	output := func(o string) string {
		return `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":` + o + `}}`
	}
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"a message's image part", []string{codexImageMessage(url)}, "brief: look[an image of 3 KiB]after"},
		{"an image part of no data URL", []string{codexImageMessage("https://example.com/a.png")},
			"brief: look[an image]after"},
		{"an output list's image part", []string{call, codexImageOutput(url)}, "tool_result: seen[an image of 3 KiB]"},
		{"an output that is a data URL", []string{call, output(boundedQuoted(url))}, "tool_result: [an image of 3 KiB]"},
		{"a list shown as written", []string{call, output(`[{"type":"input_text","text":5},{"type":"input_image",` +
			`"image_url":"` + url + `"}]`)},
			`tool_result: [{"type":"input_text","text":5},{"type":"input_image","image_url":"[an image of 3 KiB]"}]`},
		{"an output object shown as written", []string{call, output(`{"img": "` + url + `", "n": 1}`)},
			`tool_result: {"img":"[an image of 3 KiB]","n":1}`},
	} {
		page, err := transcriptOf(t, Codex{}, boundedArchiveOf(t, codexRolloutMembers(c.lines)...))
		if got := texts(page); err != nil || !strings.Contains(got, c.want) || strings.Contains(got, "AAAA") {
			t.Errorf("%s: read\n%s\n(%v), want it to hold %q and no image's bytes", c.name, got, err, c.want)
		}
	}
	row := `{"t": "messages", "r": {"id": 5, "role": "tool", "tool_call_id": "c1", "content": ` + boundedQuoted(url) + `}}`
	page, err := transcriptOf(t, Hermes{}, boundedArchiveOf(t, hermesMembers([]string{row})...))
	if got := texts(page); err != nil || got != "tool_result: [an image of 3 KiB]" {
		t.Errorf("a Hermes content that is a data URL reads\n%s\n(%v), want its place", got, err)
	}
}

// A LINE PAST THE BOUND ON ITS IMAGES IS READ, THEM PASSED: each reader of a
// Codex rollout or a Hermes record reads a line whose images run to 12 MiB, or
// 100 MiB, past the 8 MiB bound, its words intact and each image in its place:
// a person's read, a collection's notes and Codex's count, in every place an
// image is written.
//
// Mutation: count an image's bytes against the bound, or drop the pass: every
// case fails.
func TestALinePastTheBoundOnItsImagesIsReadThemPassed(t *testing.T) {
	t.Parallel()
	for _, size := range []int{12 << 20, 100 << 20} {
		url := imageURL(size)
		shown := imagePlaceholder(int64(size))
		brief := codexImageMessage(url)
		page, err := transcriptOf(t, Codex{}, boundedArchiveOf(t, codexRolloutMembers([]string{brief})...))
		if got := texts(page); err != nil || got != "brief: look"+shown+"after" {
			t.Errorf("%d: a Codex message reads\n%.300s\n(%v)", size, got, err)
		}
		if size > 12<<20 {
			continue
		}
		call := `{"type":"response_item","payload":{"type":"function_call","name":"x","arguments":"{}"}}`
		note := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":` +
			`"input_text","text":"one"}]}}`
		count := `{"type":"event_msg","ordinal":3,"payload":{"type":"token_count","info":{"last_token_usage":` +
			`{"input_tokens":10,"total_tokens":20},"model_context_window":100}}}`
		for i, line := range codexImageLines(url) {
			// A compaction before the reply confirms the note before it never.
			var want []string
			if !strings.HasPrefix(line, `{"type":"compacted"`) {
				want = []string{"one"}
			}
			body := note + "\n" + line + "\n" + call + "\n" + count + "\n"
			held, err := codexNotesHeld(t.Context(), strings.NewReader(body), noteOne, 0, nil)
			if err != nil || !slices.Equal(held, want) {
				t.Errorf("line %d: a collection holds %v (%v), want %v", i, held, err, want)
			}
			var c codexCounting
			if err := c.read(t.Context(), strings.NewReader(body), nil); err != nil || c.figure != 10 {
				t.Errorf("line %d: the count reads %+v (%v), want the figure", i, c, err)
			}
		}
		page, err = transcriptOf(t, Codex{}, boundedArchiveOf(t, codexRolloutMembers([]string{call,
			codexImageOutput(url)})...))
		if got := texts(page); err != nil || !strings.HasSuffix(got, "tool_result: seen"+shown) {
			t.Errorf("a Codex output reads\n%.300s\n(%v)", got, err)
		}
		row := hermesImageRow(5, url)
		page, err = transcriptOf(t, Hermes{}, boundedArchiveOf(t, hermesMembers([]string{row})...))
		if got := texts(page); err != nil || got != "tool_result: seen\n"+shown {
			t.Errorf("a Hermes row reads\n%.300s\n(%v)", got, err)
		}
		hnote := `{"t": "messages", "r": {"id": 3, "role": "user", "content": ` +
			boundedQuoted(hermesNoteOpen+"one"+hermesNoteClose) + `, "display_kind": "steer"}}`
		reply := `{"t": "messages", "r": {"id": 9, "role": "assistant", "content": "ok", "finish_reason": "stop"}}`
		held, err := hermesNotesHeld(t.Context(), strings.NewReader(hnote+"\n"+row+"\n"+reply+"\n"), noteOne,
			0, nil)
		if err != nil || !slices.Equal(held, []string{"one"}) {
			t.Errorf("a Hermes collection holds %v (%v), want the note", held, err)
		}
	}
}

// A LINE WHOSE TEXT OUTSIDE ITS IMAGES PASSES THE BOUND IS STILL REFUSED, by
// every reader as it refused one before, a person's read saying so: the bound
// holds what a reader keeps.
//
// Mutation: pass every string as an image, or stop judging the bound: a case
// fails.
func TestALineWhoseTextOutsideItsImagesPassesTheBoundIsRefused(t *testing.T) {
	t.Parallel()
	url := imageURL(1 << 20)
	words := strings.Repeat("w", codexRolloutLineMaxBytes)
	codex := `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_text","text":"` + words + `"},{"type":"input_image","image_url":"` + url + `"}]}}`
	page, err := transcriptOf(t, Codex{}, boundedArchiveOf(t, codexRolloutMembers([]string{codex})...))
	if got := texts(page); err != nil || !strings.HasPrefix(got, "unread: ") ||
		!strings.Contains(got, "outside its images") {
		t.Errorf("a Codex line reads\n%.300s\n(%v), want it said past the bound outside its images", got, err)
	}
	if _, err := codexNotesHeld(t.Context(), strings.NewReader(codex+"\n"), nil, 0, nil); err == nil {
		t.Error("a Codex collection reads the line, want it refused")
	}
	var c codexCounting
	if err := c.read(t.Context(), strings.NewReader(codex+"\n"), nil); err == nil {
		t.Error("Codex's count reads the line, want it refused")
	}
	hermes := `{"t": "messages", "r": {"id": 5, "role": "tool", "content": ` + hermesStored(`[{"type": "text", `+
		`"text": "`+words+`"}, {"type": "image_url", "image_url": {"url": "`+url+`"}}]`) + `}}`
	page, err = transcriptOf(t, Hermes{}, boundedArchiveOf(t, hermesMembers([]string{hermes})...))
	if got := texts(page); err != nil || !strings.HasPrefix(got, "unread: ") {
		t.Errorf("a Hermes row reads\n%.300s\n(%v), want it said past the bound", got, err)
	}
	var s summaryRead
	if _, err := hermesNotesHeld(t.Context(), strings.NewReader(hermes+"\n"), nil, 0, &s); err != nil ||
		!strings.Contains(s.reading().Unread, "past the") {
		t.Errorf("a Hermes collection reads %+v (%v), want the row said past the bound", s.reading(), err)
	}
}

// A LINE OF THE BOUND BESIDE ITS IMAGES IS READ, AND ONE BYTE PAST IT IS NOT:
// whatever image bytes are in flight as its text reaches the bound, an image
// after its words included, the verdict is exact ([imageSlack]), on the
// person's walk and the notes walk alike.
//
// Mutation: take no slack, or count a line's break out of the bound: a case
// fails.
func TestALineOfTheBoundBesideItsImagesIsReadAndOnePastItNot(t *testing.T) {
	t.Parallel()
	url := imageURL(1 << 20)
	line := func(kept int) string {
		head := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":` +
			`"output_text","text":"at the bound"}]},"pad":"`
		tail := `","img":"` + url + `"}`
		return head + strings.Repeat("x", kept-1-len(head)-len(tail)+len(url)) + tail
	}
	note := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":` +
		`"input_text","text":"one"}]}}`
	reply := `{"type":"response_item","payload":{"type":"function_call","name":"x","arguments":"{}"}}`
	for _, c := range []struct {
		name string
		kept int
		read bool
	}{{"at the bound", codexWalkBound, true}, {"a byte past it", codexWalkBound + 1, false}} {
		l := line(c.kept)
		if got := len(l) + 1 - len(url); got != c.kept {
			t.Fatalf("%s: the line keeps %d bytes, want %d", c.name, got, c.kept)
		}
		page, err := transcriptOf(t, Codex{}, boundedArchiveOf(t, codexRolloutMembers([]string{l})...))
		if got := texts(page); err != nil || (got == "model: at the bound") != c.read {
			t.Errorf("%s: a person's read reads\n%.300s\n(%v)", c.name, got, err)
		}
		held, err := codexNotesHeld(t.Context(), strings.NewReader(note+"\n"+l+"\n"+reply+"\n"), noteOne, 0, nil)
		if c.read && (err != nil || !slices.Equal(held, []string{"one"})) || !c.read && err == nil {
			t.Errorf("%s: a collection holds %v (%v)", c.name, held, err)
		}
	}
}

// A CODEX RUN'S FIGURES ARE READ OFF THE LINES OF THE KINDS THEY ARE, told by
// the kind each opens with: an MCP call's end whose structured content names a
// token count, beside an image past the bound, never reaches the result, which
// reads the run's spend, model and window as the lines of their kinds state
// them.
//
// Mutation: match a kind anywhere in a line, as before: the output reaches
// the result, whose figures do not read.
func TestACodexRunsFiguresAreReadOffTheLinesOfTheirKinds(t *testing.T) {
	t.Parallel()
	const at = `{"timestamp":"2026-11-21T18:34:00.136Z","ordinal":`
	quoting := at + `16,"type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"c9","result":{"Ok":` +
		`{"content":[{"type":"image","data":"` + strings.Repeat("A", 9<<20) + `","mimeType":"image/png"}],` +
		`"structuredContent":{"type":"token_count"}}}}}`
	rollout := strings.Join([]string{
		at + `15,"type":"turn_context","payload":{"model":"gpt-6.2-sol"}}`,
		quoting,
		at + `17,"type":"token_usage_record","payload":{"turn_id":"t1","turn_token_usage":{"input_tokens":300,` +
			`"cached_input_tokens":200,"cache_write_input_tokens":10,"output_tokens":40,"reasoning_output_tokens":4,` +
			`"total_tokens":340}}}`,
		at + `18,"type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":258400}}}`,
		at + `19,"type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}`,
	}, "\n") + "\n"
	p := codexBox(t, []byte(rollout))
	out, errText, code := codexShell(t, p.Home, "sid="+codexThread+" tid= before=0\n"+codexFinish(p)+"finish\n")
	if code != 0 {
		t.Fatalf("the script exited %d: %s", code, errText)
	}
	if got := strings.Count(out, `{"type":"`+codexRolloutEvent+`"`); got != 4 || strings.Contains(out, "mcp_tool_call_end") {
		t.Fatalf("the script appended %d lines (the call's end among them: %v), want the four of their kinds",
			got, strings.Contains(out, "mcp_tool_call_end"))
	}
	d := &codexEvents{}
	decodeAll(d, append(codexStream(t, "stream-mcp.jsonl"), out...))
	figures, ok := d.figures()
	if spent, stated := d.runSpend(); !ok || !stated || spent.output != 40 || figures.model != "gpt-6.2-sol" ||
		!d.interrupted {
		t.Errorf("the run's figures read %+v (ok %v), spend %+v, interrupted %v", figures, ok, spent, d.interrupted)
	}
}

// READING AN IMAGE PAST THE BOUND HOLDS NONE OF IT: each reader's own
// allocation over a line whose image runs to 12 MiB, and to 48 MiB, is within
// a mebibyte of its allocation over one of a kibibyte, at its least of several
// runs, so what it holds does not grow with the image.
//
// Mutation: hold an image's bytes, in its part or as a word: a reader's
// allocation grows with it and a case fails.
func TestReadingAnImagePastTheBoundHoldsNoneOfIt(t *testing.T) {
	note := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":` +
		`"input_text","text":"one"}]}}`
	hnote := `{"t": "messages", "r": {"id": 3, "role": "user", "content": ` +
		boundedQuoted(hermesNoteOpen+"one"+hermesNoteClose) + `, "display_kind": "steer"}}`
	type reader struct {
		name string
		read func(ctx context.Context, url string) func() error
	}
	for _, r := range []reader{
		{"a person's read of Codex", func(ctx context.Context, url string) func() error {
			archive := boundedArchiveOf(t, codexRolloutMembers([]string{codexImageMessage(url)})...)
			return func() error {
				_, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(ctx, Codex{},
					bytes.NewReader(archive))
				return err
			}
		}},
		{"Codex's notes", func(ctx context.Context, url string) func() error {
			body := []byte(note + "\n" + codexImageEvent(url) + "\n")
			return func() error {
				_, err := codexNotesHeld(ctx, bytes.NewReader(body), noteOne, 0, nil)
				return err
			}
		}},
		{"Codex's count", func(ctx context.Context, url string) func() error {
			body := []byte(codexMCPImageEnd(strings.TrimPrefix(url, "data:image/png;base64,")) + "\n")
			return func() error {
				var c codexCounting
				return c.read(ctx, bytes.NewReader(body), nil)
			}
		}},
		{"a person's read of Hermes", func(ctx context.Context, url string) func() error {
			archive := boundedArchiveOf(t, hermesMembers([]string{hermesImageRow(5, url)})...)
			return func() error {
				_, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(ctx, Hermes{},
					bytes.NewReader(archive))
				return err
			}
		}},
		{"Hermes's notes", func(ctx context.Context, url string) func() error {
			body := []byte(hnote + "\n" + hermesImageRow(5, url) + "\n")
			return func() error {
				_, err := hermesNotesHeld(ctx, bytes.NewReader(body), noteOne, 0, nil)
				return err
			}
		}},
	} {
		small := leastAllocation(t, r.read(t.Context(), imageURL(1<<10)))
		for _, size := range []int{12 << 20, 48 << 20} {
			if got := leastAllocation(t, r.read(t.Context(), imageURL(size))); got > small+1<<20 {
				t.Errorf("%s allocates %d bytes over a %d-byte image, %d over a kibibyte's", r.name, got, size, small)
			}
		}
	}
}

// leastAllocation is the least read allocates of five runs.
func leastAllocation(t *testing.T, read func() error) uint64 {
	t.Helper()
	least := ^uint64(0)
	for range 5 {
		got, err := allocated(read)
		if err != nil {
			t.Fatal(err)
		}
		least = min(least, got)
	}
	return least
}

// allocated is the bytes read allocates.
func allocated(read func() error) (uint64, error) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := read()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, err
}

// A STRING READ FOR TWO READERS IS READ AS EACH READS IT: [lineWalk.readAlso]
// hands one reader the string as [lineWalk.read] reads it, an image's place
// on a walk that passes images, and the other every byte of it as written:
// short, past a data URL's head and a read's piece, escaped, and an image
// within its head and past it, on a walk that passes images and one that
// does not.
//
// Mutation: hand the second reader only the string's head, or the first an
// image's bytes: a case fails.
func TestAStringReadForTwoReadersIsReadAsEachReadsIt(t *testing.T) {
	t.Parallel()
	each := func(value string, images bool, read func(w *lineWalk) error) {
		var w lineWalk
		w.setupNotes(bufio.NewReaderSize(strings.NewReader(`{"s":`+value+"}\n"), lineChunkBytes), maxLineBytes)
		if images {
			w.passImages()
		}
		if _, err := w.noteLine(t.Context(), func() error {
			return w.members(func(_ string, _ jsonscan.Kind) error { return read(&w) })
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{`"short"`, boundedQuoted(strings.Repeat("words ", 30_000)),
		`"\"quoted\" \\ é 😀"`, boundedQuoted(imageURL(200)), boundedQuoted(imageURL(200_000))} {
		for _, images := range []bool{true, false} {
			var read, raw, also, alsoRaw bytes.Buffer
			each(value, images, func(w *lineWalk) error { return w.read(&read) })
			each(value, images, func(w *lineWalk) error {
				for {
					n, err := w.scan.Read(w.piece)
					raw.Write(w.piece[:n])
					switch {
					case errors.Is(err, io.EOF):
						return nil
					case err != nil:
						return err
					}
				}
			})
			each(value, images, func(w *lineWalk) error { return w.readAlso(&also, &alsoRaw) })
			if also.String() != read.String() || alsoRaw.String() != raw.String() {
				t.Errorf("%.40s (images %t): read for two reads %.60q and %.60q as written, want %.60q and %.60q",
					value, images, also.String(), alsoRaw.String(), read.String(), raw.String())
			}
		}
	}
}
