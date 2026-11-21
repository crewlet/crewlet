package runner

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/tools"
)

// liveFrame is the frame a phase holding res publishes, words what its earlier
// frames redacted, and the frame as the socket sends it.
func liveFrame(t *testing.T, words *liveWords, res toolloop.Result) (*types.AgentTurnProgress, string) {
	t.Helper()
	pub := &collector{}
	var mu sync.Mutex
	quietEmitter(pub, &mu).progress(context.Background(), phase.Review, 1, words, res, roundCaps{})
	ev, _ := published(t, pub)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := ev.Data.(*types.AgentTurnProgress)
	if !ok {
		t.Fatalf("published %T, want a live frame", ev.Data)
	}
	return frame, string(raw)
}

// endOf fails where got, a text's end as a frame shows it within bound, is not
// the end of whole, the text's redaction, or begins inside a marker of it.
func endOf(t *testing.T, where, got, whole string, bound int) {
	t.Helper()
	body := strings.TrimPrefix(got, tailMarker)
	if !strings.HasSuffix(whole, body) {
		t.Errorf("%s shows %q…, not the end of the text's redaction", where, body[:min(len(body), 80)])
		return
	}
	if _, _, inside := redact.MarkersAround(whole, len(whole)-len(body)); inside {
		t.Errorf("%s begins inside a marker: %q…", where, body[:min(len(body), 80)])
	}
	if len(body) > bound {
		t.Errorf("%s shows %d bytes, past its %d", where, len(body), bound)
	}
}

// liveField is one of a frame's growing texts: the result that holds said,
// where the frame shows it, and whether it is still being written.
type liveField struct {
	res  func(said string) toolloop.Result
	get  func(*types.AgentTurnProgress) string
	open bool
}

// liveFields is every growing text a frame shows the end of.
func liveFields() map[string]liveField {
	call := func(failed bool) func(string) toolloop.Result {
		return func(said string) toolloop.Result {
			return toolloop.Result{Executions: []toolloop.Execution{{Round: 1, Name: "read_file",
				Output: said, Failed: failed}}}
		}
	}
	row := func(key string) func(*types.AgentTurnProgress) string {
		return func(f *types.AgentTurnProgress) string { s, _ := f.ToolExecutions[0][key].(string); return s }
	}
	return map[string]liveField{
		"the answer": {res: func(said string) toolloop.Result { return toolloop.Result{Text: said} },
			get: func(f *types.AgentTurnProgress) string { return f.Response }},
		"the round in flight": {res: func(said string) toolloop.Result {
			return toolloop.Result{Partial: &toolloop.Partial{Round: 1, Content: said}}
		}, get: func(f *types.AgentTurnProgress) string { s, _ := f.PartialRound["content"].(string); return s },
			open: true},
		"a call's answer":       {res: call(false), get: row("result")},
		"a failed call's error": {res: call(true), get: row("error")},
	}
}

// planting is a text of p's credential on its own line, lines of prose after
// it (so a text still being written shows it settled, redact.Settled), and a
// last line of n bytes, which puts a tail's start where a case needs it.
func planting(lead string, p plant, n int) string {
	return lead + p.text + "\n" + settling + strings.Repeat(".", n) + "\n"
}

// settling is the lines after a credential that settle it.
var settling = strings.Repeat("the step is done\n", 6)

// around is the sizes of a planting's last line that put the start of an end
// of bound bytes at every offset of p's credential and of its marker.
func around(p plant, bound int) (from, to int) {
	span := max(len(p.text), redact.LongestMarker()) + 4
	return bound - span - len(settling), bound + 1 - len(settling)
}

// redactionOf is the redaction a frame shows the end of: of text whole, or of
// its settled lines where it is still being written.
func redactionOf(text string, open bool) string {
	if open {
		text = text[:redact.Settled(text)]
	}
	return redact.Secrets(text)
}

// A PHASE'S LIVE FRAME SHOWS NO SECRET ITS WORDS CARRIED, WHEREVER ITS TAIL
// BEGINS. A provider's key, a password and a bearer token, planted in the
// answer, the round in flight, a call's answer and a failed call's error with
// the frame's tail beginning at every offset of the credential and of its
// marker, reach the frame as the end of the text's redaction: no piece of the
// credential, no marker in part, within the tail's bound; planted inside the
// kept end, as a marker beside the words around it. The model's copy is kept.
//
// Mutation: take the tail before redacting at any one path (tail, writingTail,
// liveOutput), and a tail beginning inside the credential keeps a piece of it;
// drop the move past a marker (heldEnd), and one beginning inside it keeps it
// in part.
func TestAPhasesLiveFrameShowsNoSecretItsWordsCarriedWhereverItsTailBegins(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		lead := "the build started\n"
		for name, f := range liveFields() {
			from, to := around(p, partialTail)
			for n := from; n <= to; n++ {
				said := planting(lead, p, n)
				where := p.name + " in " + name + " at " + strconv.Itoa(n)
				res := f.res(said)
				frame, _ := liveFrame(t, new(liveWords), res)
				got := f.get(frame)
				holdsNone(t, where, got, p)
				endOf(t, where, got, redactionOf(said, f.open), partialTail)
				if !reflect.DeepEqual(res, f.res(said)) {
					t.Fatalf("%s: the loop's own result was changed: the model's copy must stay as it came", where)
				}
			}
			said := lead + p.text + "\nthen check the logs\n" + settling
			frame, _ := liveFrame(t, new(liveWords), f.res(said))
			got := f.get(frame)
			holdsNone(t, p.name+" inside "+name, got, p)
			if !strings.Contains(got, "then check the logs") || !strings.Contains(got, "[REDACTED:") {
				t.Errorf("%s inside %s: the frame shows %q, want the words around a marker", p.name, name, got)
			}
		}
	}
}

// A LONG TEXT'S END ON A LIVE FRAME SHOWS NO SECRET WHEREVER ITS TAIL BEGINS:
// past what is read whole, a text's end is read [redact.LookBack] before the
// tail, and the tail beginning at every offset of a credential shows no piece
// of it and no marker in part, the cut said, whether the text is whole or
// still being written.
//
// Mutation: read only the tail's bytes (no look-back), or tail before
// redacting, and a tail beginning inside the credential keeps a piece of it.
func TestALongTextsEndOnALiveFrameShowsNoSecretWhereverItsTailBegins(t *testing.T) {
	t.Parallel()
	const line = "the build is running its next step\n"
	filler := strings.Repeat(line, (redact.LookBack+partialTail)/len(line)+1)
	for _, p := range planted(t) {
		if from, _ := around(p, partialTail); len(planting(filler, p, from)) <= partialTail+redact.LookBack {
			t.Fatalf("%s: the text is read whole; this test is of one read from inside it", p.name)
		}
		for _, open := range []bool{false, true} {
			t.Run(p.name+" open="+strconv.FormatBool(open), func(t *testing.T) {
				t.Parallel()
				show := tail
				if open {
					show = writingTail
				}
				from, to := around(p, partialTail)
				for n := from; n <= to; n++ {
					said := planting(filler, p, n)
					got := show(said)
					where := p.name + " at " + strconv.Itoa(n)
					holdsNone(t, where, got, p)
					if body, cut := strings.CutPrefix(got, tailMarker); !cut || len(body) > partialTail {
						t.Errorf("%s: the end shows %d bytes, want its cut said, within %d", where, len(body),
							partialTail)
					}
				}
			})
		}
	}
}

// A LONG TEXT'S END IS SHOWN AS THE WHOLE TEXT REDACTS IT, whatever the
// reading of its end begins inside: a credential the reading's start cuts,
// its key or its head before the start, is in no rule's shape from there on,
// and an end whose redaction is short (keys of tens of KiB, each a marker)
// would show it. The reading's first [redact.LookBack] bytes decide what
// follows and are never shown ([redact.SettledEnd]).
//
// Mutation: show the reading's whole redaction (SettledEnd(reading, 0) in
// shownEnd), and the credential's rest shows past the cut.
func TestALongTextsEndIsShownOnlyWhereTheTextBeforeItSettlesIt(t *testing.T) {
	t.Parallel()
	key := privateKey(t)
	lines := strings.SplitAfter(key, "\n")
	reach := partialTail + redact.LookBack
	armour := len(lines[0]) + len(lines[len(lines)-1]) + 1
	block := lines[0] + strings.Repeat(lines[1], ((reach-2048)/7-armour)/len(lines[1])) + lines[len(lines)-1] + "\n"
	keys := strings.Repeat(block, 7)
	if len(block) > redact.MaxKeyBlockBytes || len(keys) > reach-1024 || len(keys) < reach-4096 {
		t.Fatalf("the keys weigh %d bytes each, %d in all: not one key each, or not the reading's bulk",
			len(block), len(keys))
	}
	lead := "the build started\n"
	for _, p := range planted(t) {
		for _, open := range []bool{false, true} {
			show := tail
			if open {
				show = writingTail
			}
			for _, at := range readingStarts(p) {
				rest := reach - (len(p.text) - at) - 1 - len(keys) - len(settling) - 1
				said := lead + p.text + "\n" + keys + strings.Repeat(".", rest) + "\n" + settling
				if len(said)-reach != len(lead)+at {
					t.Fatalf("%s: the reading begins at %d, want %d into the credential", p.name,
						len(said)-reach-len(lead), at)
				}
				got := show(said)
				where := p.name + " cut at " + strconv.Itoa(at) + " open=" + strconv.FormatBool(open)
				holdsNone(t, where, got, p)
				if !strings.HasPrefix(got, tailMarker) {
					t.Errorf("%s: the end shows %.80q, want its cut said", where, got)
				}
			}
		}
	}
}

// readingStarts is the places in p's text a reading of a long text's end
// begins at: its start, inside and after its key, and through its value.
func readingStarts(p plant) []int {
	head := strings.Index(p.text, p.value)
	out := []int{0, head / 2, head}
	for at := head + 1; at+pieceBytes <= len(p.text); at += 5 {
		out = append(out, at)
	}
	return out
}

// privateKey is redact's corpus's private key, of several lines.
func privateKey(t *testing.T) string {
	t.Helper()
	for _, s := range redacttest.Shapes() {
		if s.Name == "private-key" && strings.Count(s.Sample, "\n") >= 2 {
			return s.Sample
		}
	}
	t.Fatal("redact's corpus no longer has a private key of several lines")
	return ""
}

// AN ABANDONED ATTEMPT SAYS IT HELD WORDS BACK: its last line, not settled
// when it died, is held back (half a token is no shape a rule knows) and its
// end is marked, so an attempt of one such line reads as one whose words were
// held, not as one that said nothing.
//
// Mutation: drop the end's mark (liveTail), and the attempt shows as "".
func TestAnAbandonedAttemptSaysItHeldWordsBack(t *testing.T) {
	t.Parallel()
	for said, want := range map[string]string{
		"the provider died mid-sent": tailMarker,
		"one line\nand half":         "one line\n" + tailMarker,
		"one line\n":                 "one line\n",
	} {
		out := new(liveWords).partialRound(&toolloop.Partial{Round: 2,
			Abandoned: []toolloop.Narration{{Round: 2, Content: said}}})
		rows, _ := out["abandoned"].([]types.RoundNarration)
		if got, _ := rows[0]["content"].(string); got != want {
			t.Errorf("an attempt of %q shows %q, want %q", said, got, want)
		}
	}
}

// A ROUND'S ABANDONED ATTEMPT ON A LIVE FRAME SHOWS NO SECRET WHEREVER ITS END
// IS CUT: its end, held to its part of the attempts' share, begins at every
// offset of a credential and of its marker, and shows no piece of it, no marker
// in part, within its part.
//
// Mutation: cut the attempt before redacting it (liveTail), and an end
// beginning inside the credential keeps a piece of it; drop the move past a
// marker, and one beginning inside it keeps it in part.
func TestARoundsAbandonedAttemptShowsNoSecretWhereverItsEndIsCut(t *testing.T) {
	t.Parallel()
	each := liveAbandonedBytes - narrationRowRoom
	for _, p := range planted(t) {
		from, to := around(p, each)
		for n := from - 4; n <= to; n++ {
			said := planting("the build started\n", p, n)
			where := p.name + " in an attempt at " + strconv.Itoa(n)
			out := new(liveWords).partialRound(&toolloop.Partial{Round: 2, Content: "now\n",
				Abandoned: []toolloop.Narration{{Round: 2, Content: said}}})
			rows, _ := out["abandoned"].([]types.RoundNarration)
			if len(rows) != 1 {
				t.Fatalf("%s: the round keeps %d attempts, want 1", where, len(rows))
			}
			got, _ := rows[0]["content"].(string)
			holdsNone(t, where, got, p)
			endOf(t, where, got, redactionOf(said, true), each)
			if kept.Quoted(got) > each {
				t.Errorf("%s: the attempt keeps %d bytes, past its %d", where, kept.Quoted(got), each)
			}
		}
	}
}

// A PHASE'S LIVE FRAME SHOWS NO SECRET A CALL'S ARGUMENTS OR ITS ROUNDS' PROSE
// CARRIED: a finished call's arguments and a running call's, redacted as
// values and still one JSON object, and a committed round's prose, redacted
// whole. The model's copy is as it came.
//
// Mutation: show a call's arguments as they came (no redact.Args in
// liveWords.executions or runningCalls), or a round's prose (none in
// liveWords.narration), and its case holds the plant.
func TestAPhasesLiveFrameShowsNoSecretItsCallsOrProseCarried(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		said := "deploy with\n" + p.text + "\nthen check the logs"
		result := func() toolloop.Result {
			args := map[string]any{"command": said, "env": map[string]any{"note": said}, "argv": []any{said}}
			return toolloop.Result{
				Executions: []toolloop.Execution{{Round: 1, Name: "run_command", Output: "ok", Args: args}},
				Running:    []toolloop.RunningCall{{ID: "c2", Round: 2, Name: "run_command", Args: args}},
				Narration:  []toolloop.Narration{{Round: 1, Content: said}},
			}
		}
		res := result()
		frame, _ := liveFrame(t, new(liveWords), res)
		if !reflect.DeepEqual(res, result()) {
			t.Errorf("%s: the loop's own result was changed: the model's copy must stay as it came", p.name)
		}
		for where, got := range map[string]string{
			"a call's arguments":         frame.ToolExecutions[0]["arguments"].(string),
			"a running call's arguments": frame.RunningCalls[0].Arguments,
		} {
			holdsNone(t, p.name+" in "+where, got, p)
			if back := tools.ReadArgs(got); back == nil || !strings.Contains(got, "then check the logs") {
				t.Errorf("%s in %s: the frame shows %s, want one object with the words around a marker", p.name,
					where, got)
			}
		}
		prose, _ := frame.RoundNarration[0]["content"].(string)
		holdsNone(t, p.name+" in a round's prose", prose, p)
	}
}

// A PHASE'S OPENING FRAME SHOWS NO SECRET ITS PROMPTS CARRIED, as its record
// holds them: each message's text redacted, its outline still tiling it.
//
// Mutation: hold the prompts to their share unredacted (keptPrompt in
// prompted), and the frame holds the plant.
func TestAPhasesOpeningFrameShowsNoSecretItsPromptsCarried(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		said := "deploy with\n" + p.text + "\nthen check the logs"
		pub := &collector{}
		var mu sync.Mutex
		system, user := outlined(said), outlined(said)
		quietEmitter(pub, &mu).prompted(context.Background(), phase.Review, 1, system, user, roundCaps{})
		if !reflect.DeepEqual(system, outlined(said)) || !reflect.DeepEqual(user, outlined(said)) {
			t.Errorf("%s: the prompts were changed: the model's copy must stay as it came", p.name)
		}
		ev, _ := published(t, pub)
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		holdsNone(t, p.name+" in the opening frame", string(raw), p)
		frame := ev.Data.(*types.AgentTurnProgress)
		for _, m := range frame.PromptMessages {
			size := 0
			for _, s := range m.Sections {
				size += s.Bytes
			}
			if len(m.Sections) != 3 || size != len(m.Content) || !strings.Contains(m.Content, "[REDACTED:") {
				t.Errorf("%s: the %s message's outline of %d parts spans %d of %d bytes, want 3 tiling its "+
					"redacted text", p.name, m.Role, len(m.Sections), size, len(m.Content))
			}
		}
	}
}

// A LIVE FRAME SHOWS EACH OF ITS WORDS AS THEY ARE NOW, whatever a frame
// before it showed: what a phase's frames redacted is kept, and a text that
// is not the one it was redacted from (another of the same size at the same
// place), or a call's arguments that are another map, is redacted anew.
//
// Mutation: tell what is kept apart by size alone (liveText.of), or a call's
// arguments by their call (liveArgs.of), and a frame shows the words before.
func TestALiveFrameShowsEachOfItsWordsAsTheyAreNow(t *testing.T) {
	t.Parallel()
	frameOf := func(answer, output, prose, attempt, path string, failed bool) toolloop.Result {
		return toolloop.Result{
			Text:       answer,
			Executions: []toolloop.Execution{{Round: 1, Name: "read_file", Output: output, Failed: failed}},
			Narration:  []toolloop.Narration{{Round: 1, Content: prose}},
			Running:    []toolloop.RunningCall{{ID: "c1", Round: 2, Name: "read_file", Args: map[string]any{"path": path}}},
			Partial: &toolloop.Partial{Round: 2, Content: "now\n",
				Abandoned: []toolloop.Narration{{Round: 2, Content: attempt}}},
		}
	}
	shows := func(f *types.AgentTurnProgress) []any {
		attempts, _ := f.PartialRound["abandoned"].([]types.RoundNarration)
		return []any{f.Response, f.ToolExecutions[0]["result"], f.ToolExecutions[0]["error"],
			f.RoundNarration[0]["content"], attempts[0]["content"], f.RunningCalls[0].Arguments}
	}
	words := new(liveWords)
	for _, step := range []struct {
		res  toolloop.Result
		want []any
	}{
		{frameOf("answer one", "the first answer", "the first round", "attempt one\n", "a", false),
			[]any{"answer one", "the first answer", nil, "the first round", "attempt one\n", `{"path":"a"}`}},
		{frameOf("answer one", "the first answer", "the first round", "attempt one\n", "a", false),
			[]any{"answer one", "the first answer", nil, "the first round", "attempt one\n", `{"path":"a"}`}},
		{frameOf("answer two", "the other answer", "the other round", "attempt two\n", "b", false),
			[]any{"answer two", "the other answer", nil, "the other round", "attempt two\n", `{"path":"b"}`}},
		{frameOf("answer two", "the other answer", "the other round", "attempt two\n", "b", true),
			[]any{"answer two", "the other answer", "the other answer", "the other round", "attempt two\n",
				`{"path":"b"}`}},
	} {
		frame, _ := liveFrame(t, words, step.res)
		if got := shows(frame); !reflect.DeepEqual(got, step.want) {
			t.Errorf("the frame shows %q, want %q", got, step.want)
		}
	}
}

// A ROUND IN FLIGHT SHOWS NO SECRET IT IS STILL WRITING: a credential written
// a byte at a time, and a private key a line at a time before its END, show no
// piece of it on any frame, since half a token is no shape a rule knows and a
// key's body is one only once its END arrives; once whole, it shows as a
// marker.
//
// Mutation: show the round's end as it stands (no redact.Settled in
// shownEnd), and a frame shows the credential's head.
func TestARoundInFlightShowsNoSecretItIsStillWriting(t *testing.T) {
	t.Parallel()
	key := privateKey(t)
	body := strings.TrimSuffix(strings.SplitAfter(key, "\n")[1], "\n")
	plants := append(planted(t), plant{"a private key", key, body})
	lead := "the build started\n"
	for _, p := range plants {
		for n := 1; n <= len(p.text); n++ {
			if p.name == "a private key" && n < len(p.text) && !strings.HasSuffix(p.text[:n], "\n") {
				continue
			}
			got := writingTail(lead + p.text[:n])
			holdsNone(t, p.name+" written to "+strconv.Itoa(n), got, p)
		}
		got := writingTail(lead + p.text + "\n" + settling)
		holdsNone(t, p.name+" written whole", got, p)
		if !strings.Contains(got, "[REDACTED:") {
			t.Errorf("%s written whole shows %q, want its marker", p.name, got)
		}
	}
}
