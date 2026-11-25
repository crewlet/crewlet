package executor

import (
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
)

// museMembers is a Muse Code archive whose main session.jsonl holds lines.
func museMembers(lines []string) []boundedMember {
	return []boundedMember{{name: "2026/01/01/s1/session.jsonl", lines: lines}}
}

// museEventLine is a runtime record of event, recorded at a microsecond.
func museEventLine(event string) string {
	return `{"payload_type":"runtime.session","recorded_at":1767225600000000,"payload":{"event":` + event + `}}`
}

// musePersonVariants are session.jsonl lines written every way a person's
// read on the walk reads apart from a line held whole, past a value bound of a
// kibibyte: a prompt within and past it, opening with a heading and not, a
// model's words, a hook's context and a summary within and past it, blank; a
// batch's calls, their arguments a string and a value within and past it and
// none, naming tools past it; a batch's results; a batch's hook contexts
// before their kind, as Muse writes them; a kind after its texts; members of
// another kind than their field's, in the envelope and in the event; and
// lines that are no record or no runtime record.
func musePersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	ev := museEventLine
	call := func(id, name, args string) string {
		return `{"args":` + args + `,"call_id":"` + id + `","name":` + q(name) + `}`
	}
	calls := func(c ...string) string {
		return ev(`{"kind":"assistant_tool_calls_committed","tool_calls":[` + strings.Join(c, ",") + `]}`)
	}
	results := func(r ...string) string {
		return ev(`{"kind":"tool_result_batch_committed","results":[` + strings.Join(r, ",") + `]}`)
	}
	block := func(source, text string) string {
		return ev(`{"kind":"context_block_updated","source":"` + source + `","text":` + q(text) + `}`)
	}
	batch := func(u ...string) string {
		return ev(`{"entries":[` + strings.Join(u, ",") + `],"kind":"context_block_update_batch_committed"}`)
	}
	update := func(source, text string) string {
		return `{"update":{"source":"` + source + `","text":` + q(text) + `}}`
	}
	return append([]string{
		ev(`{"kind":"started","prompt":"the brief"}`), ev(`{"kind":"started","prompt":` + q(h.note+"the note") + `}`),
		ev(`{"kind":"started","prompt":` + q(h.note+h.words) + `}`), ev(`{"kind":"started","prompt":` + q(h.blank) + `}`),
		ev(`{"prompt":` + q(h.words+h.secret) + `,"kind":"started"}`), ev(`{"kind":"started","prompt":null}`),
		ev(`{"kind":"assistant_message_committed","text":"the words"}`),
		ev(`{"kind":"assistant_message_committed","text":` + q(h.words+h.secret) + `}`),
		ev(`{"text":` + q(h.words) + `,"kind":"assistant_message_committed"}`),
		ev(`{"kind":"assistant_message_committed","text":` + q(h.blank) + `}`),
		ev(`{"kind":"assistant_message_committed","text":null}`),
		calls(call("c1", "bash", q(`{"command": "ls"}`)), call("c2", "read", `{"path": "/a"}`)),
		calls(call("c3", "bash", q(`{"command":`+q(h.words)+`}`)), call("c4", "bash", `{"command":`+q(h.words)+`}`)),
		calls(call("c5", "bash", `null`), call("c6", "x", q("not json")), `{"call_id":"c7","name":"y"}`, `null`),
		calls(call(strings.Repeat("i", 300), strings.Repeat("t", 300), `[1, 2]`), call("c8", h.words, `{}`)),
		calls(call("c9", "bash", `5`), call("c9", "bash", `true`)), calls(`"x"`), calls(`{"call_id":5}`),
		results(`{"text":"the result","tool_call_id":"c1"}`, `{"text":`+q(h.words+h.secret)+`,"tool_call_id":"c3"}`),
		results(`{"text":`+q(h.blank)+`,"tool_call_id":"c4"}`, `{"text":null}`, `null`, `{"tool_call_id":"c5"}`),
		results(`{"tool_call_id":"c6","text":"id first"}`), results(`{"text":5}`), results(`7`),
		block("runtime_hook", h.note+"the note"), block("runtime_hook", h.note+h.words), block("runtime_hook", "x"),
		block("other", h.note+"not a hook's"), block("runtime_hook", h.blank),
		ev(`{"text":` + q(h.note+"source last") + `,"kind":"context_block_updated","source":"runtime_hook"}`),
		batch(update("runtime_hook", h.note+"one"), update("other", h.note+"two"), update("runtime_hook", h.note+h.words),
			`{"update":null}`, `null`, `{"update":{"text":`+q(h.note+"three")+`,"source":"runtime_hook"}}`),
		batch(`{"update":"x"}`), batch(`5`), batch(`{"update":{"source":5}}`),
		ev(`{"kind":"context_compaction_installed","replacement":{"kind":"x","text":"the summary"}}`),
		ev(`{"kind":"context_compaction_installed","replacement":{"text":` + q(h.words) + `}}`),
		ev(`{"kind":"context_compaction_installed","replacement":{"text":` + q(h.blank) + `}}`),
		ev(`{"kind":"context_compaction_installed","replacement":null}`),
		ev(`{"kind":"context_compaction_installed","replacement":"x"}`),
		ev(`{"kind":"started","prompt":"x","text":5}`), ev(`{"kind":5}`), ev(`"x"`), ev(`null`), ev(`[1]`),
		ev(`{"kind":"model_completed","usage":{"input_tokens":"x"}}`),
		`{"payload_type":"runtime.session"}`, `{"payload_type":"runtime.session","payload":"x"}`,
		`{"payload_type":"runtime.session","payload":null}`, `{"payload_type":"runtime.session","payload":{}}`,
		`{"payload_type":"runtime.session","recorded_at":"1","payload":{"event":{"kind":"started","prompt":"x"}}}`,
		`{"payload_type":"runtime.session","recorded_at":1.5,"payload":{"event":{"kind":"started","prompt":"x"}}}`,
		`{"payload_type":"runtime.session","recorded_at":-5,"payload":{"event":{"kind":"started","prompt":"x"}}}`,
		`{"payload_type":"runtime.session","recorded_at":9223372036854775807,"payload":{"event":{"kind":"started",` +
			`"prompt":"far"}}}`,
		`{"payload_type":"runtime.session","stream":"x","payload":{"event":{"kind":"started","prompt":"x"}}}`,
		`{"payload_type":"runtime.session","stream":{"kind":5},"payload":{"event":{"kind":"started","prompt":"x"}}}`,
		`{"payload":{"event":{"kind":"started","prompt":"typed last"}},"payload_type":"runtime.session"}`,
		`{"payload_type":5,"payload":{"event":{"kind":"started","prompt":"x"}}}`,
		`{"payload_type":"run.terminal.completed","payload":{"text":"not read"}}`,
		`{"retained_frame":"x","children":[]}`, `{}`, `true`, `[{"payload_type":"runtime.session"}]`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A MUSE CODE SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every variant, alone, between a brief and a call, and all together,
// and over every fixture, a subagent's record beside its main one, the read on
// the walk, whole and in pieces of seventeen bytes, answers the page and the
// error the whole-line oracle answers ([wholeMuse]) under every bounded read.
//
// Mutation: split a prompt past the bound into notes, read a hook's context
// past the bound for notes, read a string's arguments past the bound as
// JSON, or say a line of a JSON value but an object as no record: a variant
// fails.
func TestAPersonsReadOfAMuseSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeMuse{}, archive, r)
			if got := personReadOf(t, Muse{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesMuse{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := museEventLine(`{"kind":"started","prompt":"the brief"}`)
	call := museEventLine(`{"kind":"assistant_tool_calls_committed","tool_calls":[{"args":"{}","call_id":"c1",` +
		`"name":"bash"}]}`)
	variants := musePersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, museMembers([]string{brief, call, v,
			brief})...))
	}
	check("every variant", boundedArchiveOf(t, museMembers(variants)...))
	for _, fixture := range museSessionFixtures(t) {
		lines := strings.Split(strings.TrimSuffix(fixtureAt(t, fixture), "\n"), "\n")
		check(fixture, boundedArchiveOf(t, boundedMember{name: "2026/01/01/s1/session.jsonl", lines: lines},
			boundedMember{name: "2026/01/01/s1/subagent/t1/session.jsonl", lines: lines}))
	}
}

// museNoteRun is the lines of a hook's answer to a call a record holds:
// the call's terminal effect, the run of the plugin's first note hook naming
// token in its stderr, and the block the run awaits.
func museNoteRun(call, token string) string {
	return `{"payload_type":"tool_batch.effect.terminal","payload":{"record":{"call_id":` + boundedQuoted(call) +
		`,"kind":"terminal"}}}` + "\n" + museEventLine(`{"effects":["context"],"event":"PostToolUse","hook_key":"`+
		museHookKey(museNoteHooks[0])+`","kind":"hook_run_terminal","origin":{"source_digest":"`+museHooksDigest+
		`"},"status":"completed","stderr":`+boundedQuoted("a line\n"+token+"\n"+token+"\n")+`}`) + "\n" +
		museEventLine(`{"id":`+boundedQuoted("hook:post_tool_use:"+call+":0")+`,"kind":"context_block_updated",`+
			`"source":"runtime_hook"}`)
}

// A COLLECTION'S READ OF A MUSE CODE SESSION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every fixture, every person's variant and the lines that pick
// a hook's receipt apart (block ids past the word bound, a call past it, a
// stderr of tokens not asked and asked again, members of another kind in a
// run's end and a block, a restart, a compaction), each between a hook's
// answer and the model's and torn after them, read whole and in pieces of
// seventeen bytes, it holds the asked takes the whole-line read holds, and
// fails where it fails, in its words.
//
// Mutation: tell a block id past the word bound by its opening, keep a token
// not asked, or read a run's end one of whose members is of another kind:
// a case fails.
func TestACollectionsReadOfAMuseSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	ev := museEventLine
	run := func(stderr, more string) string {
		return ev(`{"effects":["context"],"event":"PostToolUse","hook_key":"` + museHookKey(museNoteHooks[0]) +
			`","kind":"hook_run_terminal","origin":{"source_digest":"` + museHooksDigest + `"},"status":"completed",` +
			`"stderr":` + boundedQuoted(stderr) + more + `}`)
	}
	completed := ev(`{"kind":"model_completed"}`)
	records := map[string]string{}
	digests := map[string]string{}
	for _, fixture := range museSessionFixtures(t) {
		records[fixture] = fixtureAt(t, fixture)
	}
	for name, digest := range map[string]string{"launched-notes": museLaunchedDigest,
		"launched-repository": museRepositoryDigest, "failed-request": museFailedDigest} {
		digests[filepath.Join("testdata", "muse", "1.4.25", name+".session.jsonl")] = digest
	}
	long := strings.Repeat("c", 250)
	lines := append(musePersonVariants(),
		museNoteRun(long, museToken("2")), museNoteRun(long+strings.Repeat("d", 20), museToken("3")),
		run(museToken("4")+"\n"+museToken("4")+"\n"+museToken("9")+"\n"+museToken("4"), ""),
		run(strings.Repeat(museToken("5")+"\n", 3000)+museToken("6"), `,"effects":["context","blocked"]`),
		run("x", `,"effects":[5]`), run("x", `,"origin":"x"`), run("x", `,"stderr":5`), run("x", `,"hook_key":7`),
		ev(`{"id":5,"kind":"context_block_updated","source":"runtime_hook"}`),
		ev(`{"id":"hook:post_tool_use:c1:0","kind":"context_block_updated","source":7}`),
		`{"payload_type":"tool_batch.effect.terminal","payload":{"record":{"call_id":5,"kind":"terminal"}}}`,
		`{"payload_type":"tool_batch.effect.terminal","payload":{"record":"x"}}`,
		`{"payload_type":"tool_batch.effect.terminal","payload":{"record":{"call_id":"c2","kind":"other"}}}`,
		`{"payload_type":"session.started","payload":{"kind":"x"}}`, ev(`{"kind":"context_compaction_installed"}`),
		ev(`{"kind":"hook_run_terminal","event":"Stop","status":"blocked","effects":["blocked"],"hook_key":"`+
			museHookKey(museNoteHooks[3])+`","origin":{"source_digest":"`+museHooksDigest+`"},"stderr":`+
			boundedQuoted(museToken("7"))+`}`),
		ev(`{"id":"hook:stop:stop:0","kind":"context_block_updated","source":"runtime_hook"}`), completed,
		`{"payload_type":"runtime.session","payload":5}`, `{"payload_type":7}`, `null`, `7`)
	note := museNoteRun("call_1", museToken("1"))
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = note + "\n" + l + "\n" + completed + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = note + "\n" + completed + "\n" + l
		records[fmt.Sprintf("line %d last", i)] = note + "\n" + completed + "\n" + l + "\n"
		records[fmt.Sprintf("line %d before its request", i)] = l + "\n" + completed + "\n"
	}
	records["every line"] = note + "\n" + strings.Join(lines, "\n") + "\n" + completed + "\n"
	for name, record := range records {
		digest := museHooksDigest
		if d, ok := digests[name]; ok {
			digest = d
		}
		asked := museTokensIn([]byte(record))
		delete(asked, museToken("9"))
		want, wantErr := museHeldWhole(strings.NewReader(record), digest, asked)
		got, err := museHeld(t.Context(), strings.NewReader(record), digest, asked, nil)
		if !maps.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("%s: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
		got, err = museHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, digest, asked, nil)
		if !maps.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("%s in pieces: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
	}
}

// museSessionFixtures is every recorded session record's path, a subagent's
// among them.
func museSessionFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "muse", "*", "*.session.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no session fixtures (%v)", err)
	}
	return append(paths, filepath.Join("testdata", "muse", "1.4.25", "subagent.child.jsonl"))
}
