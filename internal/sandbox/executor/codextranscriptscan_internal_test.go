package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// codexRolloutMembers is a Codex archive whose rollout holds lines.
func codexRolloutMembers(lines []string) []boundedMember {
	return []boundedMember{{name: "rollout-" + codexCarriedStamp + "-" + ctSession + ".jsonl", lines: lines}}
}

// codexPersonVariants are rollout lines written every way a person's read on
// the walk reads apart from a line held whole, past a value bound of a
// kibibyte: a message's parts within and past it, blank, out of order, an
// image, each kind of writer; a call's arguments as JSON and not, its name and
// namespace past the word and value bounds; every output's shape; a
// compaction's summary; members of another kind than their field's; a type
// written last; and lines that are no record.
func codexPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	const stamp = `"timestamp":"2026-01-01T00:00:00.000Z"`
	item := func(payload string) string { return `{` + stamp + `,"type":"response_item","payload":` + payload + `}` }
	msg := func(role, content, more string) string {
		return item(`{"type":"message","role":"` + role + `","content":` + content + more + `}`)
	}
	part := func(kind, text string) string { return `{"type":"` + kind + `","text":` + q(text) + `}` }
	list := func(p ...string) string { return "[" + strings.Join(p, ",") + "]" }
	user := `,"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}`
	harness := `,"internal_chat_message_metadata_passthrough":{"content_item_metadata":[{"provenance":{"type":"harness"}}]}`
	call := func(fields string) string { return item(`{"type":"function_call",` + fields + `}`) }
	out := func(output string) string {
		return item(`{"type":"function_call_output","call_id":"c1","output":` + output + `}`)
	}
	long := strings.Repeat("t", 300)
	return append([]string{
		msg("user", list(part("input_text", "the brief")), user),
		msg("user", list(part("input_text", h.note+h.words)), user),
		msg("user", list(part("input_text", h.note+"short")), user),
		msg("user", list(part("input_text", h.blank)), user),
		msg("user", list(part("input_text", " ")), user),
		msg("user", list(part("input_text", "one"), `{"type":"input_image","image_url":"x"}`, part("input_text", "two")), ""),
		msg("user", list(`{"type":"input_image","image_url":`+q(h.words)+`}`, part("output_text", h.words)), user),
		msg("user", list(part("input_text", h.words), part("input_text", h.words+h.secret)), user),
		msg("user", list(`{"text":`+q(h.words)+`,"type":"input_text"}`), user),
		msg("user", list(`{"text":"short","type":"input_text"}`, `{"text":`+q(h.words)+`}`), user),
		msg("user", list(part("input_text", "x")), harness),
		msg("user", list(part("input_text", "x")), `,"internal_chat_message_metadata_passthrough":`+
			`{"content_item_kinds":["compaction.summary"]}`),
		msg("user", list(part("input_text", "x")), `,"internal_chat_message_metadata_passthrough":`+
			`{"content_item_kinds":[`+q(long)+`]}`),
		msg("user", list(part("input_text", "x")), `,"internal_chat_message_metadata_passthrough":{"content_item_kinds":[5]}`),
		msg("user", list(part("input_text", "x")), `,"internal_chat_message_metadata_passthrough":5`),
		msg("assistant", list(part("output_text", "the words")), ""),
		msg("assistant", list(part("output_text", h.words), part("reasoning_text", h.words)), ""),
		`{` + stamp + `,"type":"response_item","payload":{"type":"message","role":"assistant","content":` +
			list(part("output_text", "the compaction's answer")) + `},"metadata":{"compaction_output":true}}`,
		msg("developer", list(part("input_text", "x")), ""),
		msg("assistant", `"a string"`, ""), msg("assistant", list(`5`), ""), msg("assistant", `null`, ""),
		msg("assistant", list(`null`, part("output_text", "after a null")), ""),
		msg("assistant", list(`{"type":"output_text","text":5}`), ""),
		call(`"name":"shell","arguments":"{\"command\": [\"ls\"]}","call_id":"c1"`),
		call(`"name":"shell","arguments":` + q(`{"command": `+q(h.words)+`}`) + `,"call_id":"c1"`),
		call(`"name":"x","namespace":"mcp","arguments":"not json","call_id":"c2"`),
		call(`"name":` + q(long) + `,"namespace":"mcp","arguments":"{}","call_id":"c3"`),
		call(`"name":"x","namespace":` + q(long) + `,"arguments":"{}","call_id":"c3"`),
		call(`"name":` + q(h.words) + `,"arguments":"{}","call_id":"c4"`),
		call(`"name":"x","namespace":` + q(h.words) + `,"arguments":null`),
		call(`"name":5,"arguments":"{}"`), call(`"name":"x","arguments":{"a":1}`),
		call(`"name":"x","call_id":` + q(long)), call(`"name":"x","call_id":"c7","arguments":"  "`),
		item(`{"arguments":"{\"a\": 1}","name":"late","call_id":"c5","type":"function_call"}`),
		item(`{"type":"custom_tool_call","name":"apply_patch","input":` + q(h.words) + `,"call_id":"c6"}`),
		item(`{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin","call_id":"c6"}`),
		item(`{"input":"typed last","name":"apply_patch","type":"custom_tool_call"}`),
		item(`{"type":"web_search_call","action":{"query": "x", "n": [1, 2]}}`),
		item(`{"type":"local_shell_call","action":{"command":` + q(h.words) + `}}`),
		item(`{"type":"web_search_call"}`), item(`{"type":"local_shell_call","action":null}`),
		out(q("the result")), out(q(h.words + h.secret)), out(q(h.blank)), out(q("")),
		out(list(part("input_text", "a"), `{"type":"input_image","image_url":"x"}`, `{"text":"untyped"}`)),
		out(list(part("input_text", h.words), part("x", h.words))),
		out(list(`{"text":` + q(h.words) + `,"type":"input_text"}`)),
		out(list(`{"text":` + q(h.words) + `}`)),
		out(list(part("input_text", "a"), `5`)), out(list(`{"type":"input_text","text":5}`, `{"pad":`+q(h.words)+`}`)),
		out(`{"a": 1, "b": [1, 2]}`), out(`{"big":` + q(h.words) + `}`), out(`null`), out(`7`), out(`true`), out(`[]`),
		item(`{"type":"function_call_output","call_id":"c1"}`),
		item(`{"output":"typed last","call_id":"c1","type":"custom_tool_call_output"}`),
		item(`{"type":"reasoning","summary":[]}`), item(`{"type":"ghost_snapshot"}`),
		item(`{"type":"a_later_kind"}`), item(`{"type":` + q(long) + `}`), item(`null`), item(`5`), item(`{}`),
		`{"type":"response_item"}`, `{"type":"response_item","payload":{"type":"message","role":5}}`,
		`{` + stamp + `,"type":"compacted","payload":{"message":"the summary"}}`,
		`{"type":"compacted","payload":{"message":` + q(h.words) + `}}`,
		`{"type":"compacted","payload":{"message":` + q(h.blank) + `}}`,
		`{"type":"compacted","payload":{"message":` + q(h.note+"a note in a summary") + `}}`,
		`{"type":"compacted","payload":{"message":5}}`, `{"type":"compacted"}`, `{"type":"compacted","payload":null}`,
		`{"type":"compacted","payload":[1]}`,
		`{"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"typed last"}]},` +
			`"type":"response_item"}`,
		`{"payload":{"message":"summary first"},"type":"compacted"}`,
		`{"timestamp":5,"type":"response_item","payload":{"type":"a_later_kind"}}`,
		`{"type":"response_item","metadata":"x","payload":{"type":"a_later_kind"}}`,
		`{"type":"response_item","metadata":{"compaction_output":"yes"},"payload":{"type":"a_later_kind"}}`,
		`{"type":` + q(long) + `,"payload":{"type":"a_later_kind"}}`,
		`{"timestamp":` + q(long) + `,"type":"compacted","payload":{"message":"dated past the word bound"}}`,
		`{"timestamp":"not a time","type":"compacted","payload":{"message":"undated"}}`,
		`{"type":"event_msg","payload":{"type":"token_count"}}`, `{"type":"session_meta","payload":{"id":"x"}}`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A CODEX SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every variant, alone, between a brief and a call, and all together,
// and over every recorded rollout, the read on the walk, whole and in pieces
// of seventeen bytes, answers the page and the error the whole-line oracle
// answers ([wholeCodex]) under every bounded read.
//
// Mutation: say a fragment, read a torn last line, take a harness-written
// message as a brief, compact a call's arguments past the bound, or join an
// image's text: a variant fails.
func TestAPersonsReadOfACodexSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeCodex{}, archive, r)
			if got := personReadOf(t, Codex{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesCodex{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text",` +
		`"text":"the brief"}]}}`
	call := `{"type":"response_item","payload":{"type":"function_call","name":"bash","arguments":"{}","call_id":"c1"}}`
	variants := codexPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, codexRolloutMembers([]string{brief, call, v,
			brief})...))
		torn := codexRolloutMembers([]string{brief, call, v})
		torn[0].torn = true
		check(fmt.Sprintf("variant %d torn last", i), boundedArchiveOf(t, torn...))
	}
	check("every variant", boundedArchiveOf(t, codexRolloutMembers(variants)...))
	for _, fixture := range codexRolloutFixtures(t) {
		raw := string(codexFixture(t, fixture))
		members := codexRolloutMembers(strings.Split(strings.TrimSuffix(raw, "\n"), "\n"))
		members[0].torn = !strings.HasSuffix(raw, "\n")
		check(fixture, boundedArchiveOf(t, members...))
	}
}

// A COLLECTION'S READ OF A CODEX SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every recorded rollout, every person's variant and the lines that pick
// a note's receipt apart (a reply, a compaction and its answer, a note's
// parts in every order, members of another kind), each between a note and a
// reply, torn after them, and alone after a note, whole and torn, read whole
// and in pieces of seventeen bytes, from the start and past the first line, it
// holds the notes the whole-line read holds and fails where it fails.
//
// Mutation: digest a part whose type follows its text as an input_text, take
// a reply that answered a compaction, or read a torn last line: a case fails.
func TestACollectionsReadOfACodexSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	note := func(parts string) string {
		return `{"type":"response_item","payload":{"type":"message","role":"user","content":[` + parts + `]}}`
	}
	reply := `{"type":"response_item","payload":{"type":"function_call","name":"x","arguments":"{}"}}`
	records := map[string]string{}
	for _, fixture := range codexRolloutFixtures(t) {
		records[fixture] = string(codexFixture(t, fixture))
	}
	lines := append(codexPersonVariants(), reply, note(`{"type":"input_text","text":"one"}`),
		note(`{"text":"one","type":"input_text"}`), note(`{"text":"o"},{"type":"input_text","text":"ne"}`),
		note(`{"type":"input_text","text":"o"},{"type":"input_text","text":"ne"}`),
		note(`{"type":"input_text","text":"one","type":"output_text"}`),
		note(`{"type":"input_text","text":"x","text":"one"}`), note(`{"type":"input_text","text":null}`),
		note(`{"type":"input_text","text":"one"},5`), note(`{"type":"input_text","Text":"one"}`),
		`{"type":"compacted","payload":{"message":"x"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"},"metadata":{"compaction_output":true}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant"},"metadata":{"compaction_output":true}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":5}}`)
	first := note(`{"type":"input_text","text":"one"}`)
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = first + "\n" + l + "\n" + reply + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = first + "\n" + reply + "\n" + l
		records[fmt.Sprintf("line %d then a note", i)] = l + "\n" + first + "\n" + reply + "\n"
		records[fmt.Sprintf("line %d alone after the note", i)] = first + "\n" + l + "\n"
		records[fmt.Sprintf("line %d torn after the note", i)] = first + "\n" + l
	}
	records["every line"] = first + "\n" + strings.Join(lines, "\n") + "\n" + reply + "\n"
	takes := []ServedTake{{InboxID: "one", Digest: ItemDigest("one")}, {InboxID: "x", Digest: ItemDigest("x")},
		{InboxID: "brief", Digest: ItemDigest("the brief")}, {InboxID: "never", Digest: ItemDigest("never")}}
	for name, record := range records {
		for _, from := range []int{0, 1} {
			want, wantErr := codexNotesHeldWhole(strings.NewReader(record), takes, from)
			got, err := codexNotesHeld(t.Context(), strings.NewReader(record), takes, from)
			if !slices.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Errorf("%s from %d: held %v (%v), want %v (%v)", name, from, got, err, want, wantErr)
			}
			got, err = codexNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, takes, from)
			if !slices.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Errorf("%s from %d in pieces: held %v (%v), want %v (%v)", name, from, got, err, want, wantErr)
			}
		}
	}
}

// CODEX'S COUNT ON THE WALK IS ITS WHOLE-LINE READ'S: over every recorded
// rollout and every line that moves the count (a token count with and without
// its usage, a compaction with and without its window id, one past the word
// bound, members of another kind, a payload written twice), each after a token
// count and torn, and a line past the bound, the meter reads the figure, the count and the next request
// the whole-line read reads, and fails where it fails.
//
// Mutation: read a token count whose usage is null, name a compaction by a
// window id past the word bound, or read a line one member refuses: a case
// fails.
func TestACodexCountOnTheWalkIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	count := func(input, total, window string) string {
		return `{"type":"event_msg","ordinal":3,"payload":{"type":"token_count","info":{"last_token_usage":` +
			`{"input_tokens":` + input + `,"total_tokens":` + total + `},"model_context_window":` + window + `}}}`
	}
	records := map[string]string{}
	for _, fixture := range codexRolloutFixtures(t) {
		records[fixture] = string(codexFixture(t, fixture))
	}
	lines := append(codexPersonVariants(), count("100", "120", "190000"), count("0", "-5", "0"),
		count(`"1"`, "2", "3"), count("1.5", "2", "3"), count("1e2", "2", "3"), count("null", "2", "null"),
		`{"type":"event_msg","payload":{"type":"token_count","info":null}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":null}}}`,
		`{"type":"compacted","ordinal":9,"payload":{"window_id":"w1"}}`,
		`{"type":"compacted","ordinal":9,"payload":{"window_id":`+boundedQuoted(strings.Repeat("w", 300))+`}}`,
		`{"type":"compacted","ordinal":"9","payload":{}}`, `{"type":"compacted","ordinal":9,"payload":5}`,
		`{"type":"compacted","payload":{"window_id":5}}`,
		`{"payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":7}}},"type":"event_msg"}`,
		`{"type":"event_msg","payload":{"type":"x"},"payload":{"type":"token_count","info":{"last_token_usage":`+
			`{"total_tokens":8}}}}`)
	base := count("10", "20", "100")
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = base + "\n" + l + "\n"
		records[fmt.Sprintf("torn line %d", i)] = base + "\n" + l
	}
	records["every line"] = strings.Join(lines, "\n") + "\n"
	past := `{"type":"event_msg","payload":{"type":"token_count","pad":"` + strings.Repeat("x", codexRolloutLineMaxBytes) + `"}}`
	records["a line past the bound"] = base + "\n" + past + "\n" + base + "\n"
	records["a last line past the bound, torn"] = base + "\n" + past
	for name, record := range records {
		archive := codexArchiveOf(t, record)
		var walk, whole codexCounting
		err := codexArchiveRollout(bytes.NewReader(archive), func(r io.Reader) error { return walk.read(t.Context(), r) })
		wantErr := codexArchiveRollout(bytes.NewReader(archive), func(r io.Reader) error {
			return whole.readWhole(t.Context(), r)
		})
		if fmt.Sprint(err) != fmt.Sprint(wantErr) || fmt.Sprintf("%+v", walk) != fmt.Sprintf("%+v", whole) {
			t.Errorf("%s: counts %+v (%v), want %+v (%v)", name, walk, err, whole, wantErr)
		}
		read, err := codexMeter(t.Context(), &exportPieces{r: bytes.NewReader(archive), n: 17}, codexCarryRelease)
		if fmt.Sprint(err) != fmt.Sprint(wantErr) || err == nil && read.tokens != whole.figure {
			t.Errorf("%s in pieces: the meter reads %+v (%v), want the figure %d (%v)", name, read, err,
				whole.figure, wantErr)
		}
	}
}

// codexArchiveOf is a gzipped Codex archive whose one rollout is record.
func codexArchiveOf(t testing.TB, record string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	name := "rollout-" + codexCarriedStamp + "-" + ctSession + ".jsonl"
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(record)),
		Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(record)); err != nil {
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

// codexRolloutFixtures is every recorded rollout's name.
func codexRolloutFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "codex", "0.171.0", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no rollout fixtures (%v)", err)
	}
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return names
}
