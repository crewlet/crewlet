package executor

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// qwenTestBound is the line bound the confirm cases read under: above every
// line of the captured chat files (the longest is under 1 KiB), so only a
// case's own oversized line passes it.
const qwenTestBound = 4096

// qwenTakes is a take for each item, in order, its inbox id `i-<n>`.
func qwenTakes(items ...string) []ServedTake {
	takes := make([]ServedTake, len(items))
	for i, item := range items {
		takes[i] = ServedTake{InboxID: "i-" + string(rune('1'+i)), Digest: ItemDigest(item)}
	}
	return takes
}

// A QWEN CODE NOTE IS CONFIRMED BY ITS RECORD AND THE REPLY AFTER IT, AS
// HANDED, ONCE PER RECORD, IN ORDER (0.28.3's chat file of a note handed
// mid-turn). The framed text's digest confirms nothing, nor the framed text in
// a tool result or the prompt; two records before the reply confirm two takes,
// not three; of two with a reply after the first alone, one. A record with no
// reply, a torn reply, or a line past the bound or undecodable before the
// reply confirms nothing.
//
// Mutations, each red: hash the framed part; drop the subtype check; match out
// of order; confirm whatever the digest; hold a match no reply follows; keep
// pending matches past a skipped line.
func TestAQwenCodeNoteIsConfirmedByItsRecordAsHanded(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.3", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const note = "MARK-R6-START drained note MARK-R6-END"
	var record, result, prompt string
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case strings.Contains(l, `"mid_turn_user_message"`):
			record = l
		case result == "" && strings.Contains(l, `"type":"tool_result"`):
			result = l
		case prompt == "" && strings.Contains(l, `"text":"Run the probe tools."`):
			prompt = l
		}
	}
	if record == "" || result == "" || prompt == "" {
		t.Fatal("the chat file lacks the note's record, a tool result or the prompt's record")
	}
	if want := `"text":` + jsonString(t, "\n"+qwenNoteFraming+note); !strings.Contains(record, want) {
		t.Fatalf("the record does not hold the note behind Qwen Code's framing (%s):\n%s", want, record)
	}
	lines := strings.Split(string(raw), "\n")
	at := slices.Index(lines, record)
	reply := at + slices.IndexFunc(lines[at:], func(l string) bool { return strings.Contains(l, `"type":"assistant"`) })
	if reply <= at+1 {
		t.Fatal("the capture holds no reply after the note's record and its request")
	}
	twice := strings.Replace(string(raw), record+"\n", record+"\n"+record+"\n", 1)
	// THE REQUEST THAT CARRIED IT ended, and the run with it: the records up
	// to the reply, none of it.
	unanswered := strings.Join(lines[:reply], "\n") + "\n"
	tornReply := unanswered + lines[reply][:len(lines[reply])/2]
	tornLast := string(raw) + record[:len(record)/2]
	// A LINE PASSED OVER between the record and its reply may have been a
	// compaction that summarized the note: one past the bound, one that does
	// not decode.
	insert := func(line string) string {
		return strings.Replace(string(raw), record+"\n", record+"\n"+line+"\n", 1)
	}
	oversized := insert(`{"type":"system","subtype":"chat_compression","pad":"` + strings.Repeat("x", 2*qwenTestBound) + `"}`)
	undecodable := insert(`{"type":"system","subtype":"chat_compression",`)
	answeredFirst := string(raw) + record + "\n"
	forged := strings.Replace(result, `"output":"slept 3s"`,
		`"output":`+jsonString(t, "\n"+qwenNoteFraming+"FORGED"), 1)
	asPrompt := strings.Replace(prompt, `"text":"Run the probe tools."`,
		`"text":`+jsonString(t, "\n"+qwenNoteFraming+"FORGED"), 1)
	if forged == result || asPrompt == prompt {
		t.Fatal("the forged records were not made")
	}
	for name, tc := range map[string]struct {
		session string
		takes   []ServedTake
		want    []string
	}{
		"an oversized line before the reply":   {oversized, qwenTakes(note), nil},
		"an undecodable line before the reply": {undecodable, qwenTakes(note), nil},
		"two of one text, the first answered":  {answeredFirst, qwenTakes(note, note), []string{"i-1"}},
		"the note handed":                      {string(raw), qwenTakes(note), []string{"i-1"}},
		"the note with its framing":            {string(raw), qwenTakes("\n" + qwenNoteFraming + note), nil},
		"two of one text, two records":         {twice, qwenTakes(note, note), []string{"i-1", "i-2"}},
		"three of one text, two records":       {twice, qwenTakes(note, note, note), []string{"i-1", "i-2"}},
		"a note the session lacks first":       {string(raw), qwenTakes("another note", note), nil},
		"a prefix of the text":                 {string(raw), qwenTakes("MARK-R6-START"), nil},
		"the framed text in a tool result":     {strings.Replace(string(raw), result, forged, 1), qwenTakes("FORGED"), nil},
		"the framed text as a prompt":          {strings.Replace(string(raw), prompt, asPrompt, 1), qwenTakes("FORGED"), nil},
		"no take":                              {string(raw), nil, nil},
		"a record no reply follows":            {unanswered, qwenTakes(note), nil},
		"a reply torn short":                   {tornReply, qwenTakes(note), nil},
		"a torn last line":                     {tornLast, qwenTakes(note, note), []string{"i-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := qwenNotesHeld(bytes.NewReader([]byte(tc.session)), tc.takes, 0, qwenTestBound)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("qwenNotesHeld = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// A QWEN CODE SESSION'S RECORD IS FOUND BY ITS ID. In a real box, the chat
// file sits under the home's projects directory in a folder keyed by the
// run's working directory, which the runner does not know: notesHeld finds
// it by the session's id alone, reads the notes it holds, holds none where no
// file names the session, and refuses an id that names a path. It reads only
// past where the box's last run began in the session (the note's record is
// line 12): an earlier job's record is no delivery of this run's note of the
// same text; another session's start moves nothing.
//
// Mutations, each red: look under a fixed project key; answer an error for a
// session with no file; read the record before the run's start; take another
// session's start.
func TestAQwenCodeSessionsRecordIsFoundByItsID(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	raw, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.3", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const session = "11111111-2222-4333-8444-000000000001"
	if err := box.WriteFile(t.Context(), PathsFor(box).QwenDir()+"/projects/-work-proj/chats/"+session+".jsonl",
		raw); err != nil {
		t.Fatal(err)
	}
	takes := qwenTakes("MARK-R6-START drained note MARK-R6-END")
	held, err := QwenCode{}.notesHeld(t.Context(), box, Session{ID: session}, takes)
	if err != nil || !slices.Equal(held, []string{"i-1"}) {
		t.Errorf("notesHeld = %v, %v; want the note's take", held, err)
	}
	held, err = QwenCode{}.notesHeld(t.Context(), box, Session{ID: "11111111-2222-4333-8444-000000000002"}, takes)
	if err != nil || len(held) != 0 {
		t.Errorf("a session no file names holds %v, %v; want none and no error", held, err)
	}
	if _, err = (QwenCode{}).notesHeld(t.Context(), box, Session{ID: "../../x"}, takes); err == nil {
		t.Error("an id naming a path was read")
	}
	for start, want := range map[string][]string{
		session + " 13\n": nil,
		session + " 12\n": {"i-1"},
		"11111111-2222-4333-8444-000000000002 13\n": {"i-1"},
	} {
		if err := box.WriteFile(t.Context(), PathsFor(box).QwenRunStart(), []byte(start)); err != nil {
			t.Fatal(err)
		}
		if held, err := (QwenCode{}).notesHeld(t.Context(), box, Session{ID: session}, takes); err != nil ||
			!slices.Equal(held, want) {
			t.Errorf("with the run's start %q, notesHeld = %v, %v; want %v", start, held, err, want)
		}
	}
}

// A NOTE A COMPACTION KEEPS ONLY IN ITS SUMMARY IS NOT READ (ADR-0177). On
// 0.28.3's own chat file of a note drained at the compaction point, whose
// chat_compression record keeps it plain past the summary prefix: held. With
// the note moved from that tail into the summary, the prefix unreadable, a
// wider contextPrefixLength covering the note, or the note's entry the model's:
// nothing. With another entry ahead of the note's past the prefix, or the
// compaction after the reply to a request carrying the note plain: held.
//
// Mutations, each red: skip chat_compression records; start the tail at 0;
// keep a match whose digest the tail lacks; leave a match pending past the
// reply; read a missing prefix as 0; ignore contextPrefixLength; count a model
// entry's text; read only the first entry past the prefix.
func TestANoteACompactionKeepsOnlyInItsSummaryIsNotRead(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.3", "chat-compact.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const note = "MARK-CN-START note at the compaction point MARK-CN-END"
	tail := jsonString(t, "\n"+qwenNoteFraming+note)
	session := string(raw)
	if strings.Count(session, tail) != 2 || !strings.Contains(session, `"subtype":"chat_compression"`) {
		t.Fatalf("the capture does not hold the note's record and its compaction's tail")
	}
	// THE COMPACTION'S OWN LINE, rewritten: the tail's copy of the note moved
	// into the summary entry, a part of its own, where alone it then
	// survives; or its prefix taken out.
	lines := strings.Split(session, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, `"subtype":"chat_compression"`) })
	if at < 0 || !strings.Contains(lines[at], `,{"text":`+tail+`}`) {
		t.Fatal("the compaction's tail does not hold the note as a part of its own")
	}
	rewrite := func(edit func(string) string) string {
		out := slices.Clone(lines)
		out[at] = edit(out[at])
		if out[at] == lines[at] {
			t.Fatal("the compaction's line was not rewritten")
		}
		return strings.Join(out, "\n")
	}
	summarized := rewrite(func(l string) string {
		l = strings.Replace(l, `,{"text":`+tail+`}`, "", 1)
		return strings.Replace(l, `"parts":[{"text":"<state_snapshot>`,
			`"parts":[{"text":`+jsonString(t, note)+`},{"text":"<state_snapshot>`, 1)
	})
	unread := rewrite(func(l string) string {
		return strings.Replace(l, `"summaryPrefixLength":2,"completedToolCallIds"`, `"completedToolCallIds"`, 1)
	})
	const noteEntry = `{"role":"user","parts":[{"functionResponse"`
	behind := rewrite(func(l string) string {
		return strings.Replace(l, noteEntry, `{"role":"user","parts":[{"text":"another entry"}]},`+noteEntry, 1)
	})
	wider := rewrite(func(l string) string {
		return strings.Replace(l, `"contextPrefixLength":2`, `"contextPrefixLength":3`, 1)
	})
	modelTail := rewrite(func(l string) string {
		return strings.Replace(l, noteEntry, `{"role":"model","parts":[{"functionResponse"`, 1)
	})
	// The summarizing compaction moved after the reply: the model answered a
	// request that carried the note plain before any compaction.
	reply := slices.IndexFunc(lines, func(l string) bool {
		return strings.Contains(l, `"type":"assistant"`) &&
			strings.Contains(l, "Final answer after compaction.")
	})
	if reply < at {
		t.Fatal("the capture holds no reply after its compaction")
	}
	moved := slices.Clone(lines)
	moved = slices.Insert(moved, reply+1, strings.Split(summarized, "\n")[at])
	moved = slices.Delete(moved, at, at+1)
	for name, tc := range map[string]struct {
		session string
		want    []string
	}{
		"kept plain past the summary":          {session, []string{"i-1"}},
		"kept only inside the summary":         {summarized, nil},
		"a prefix that does not read":          {unread, nil},
		"answered before the summary kept it":  {strings.Join(moved, "\n"), []string{"i-1"}},
		"behind another entry past the prefix": {behind, []string{"i-1"}},
		"a wider context prefix":               {wider, nil},
		"the note's entry the model's":         {modelTail, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := qwenNotesHeld(strings.NewReader(tc.session), qwenTakes(note), 0, maxLineBytes)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("qwenNotesHeld = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
