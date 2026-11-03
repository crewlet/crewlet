package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// readerOver is a record reader over a custom profile whose stdout record the
// rules read, telling a test host what it reads — the reader alone, fed one
// line at a time, for the rules a shipped profile composes.
func readerOver(t *testing.T, requests map[string]any, usage map[string]any) (*recordReader, *testHost, *[]string) {
	t.Helper()
	requests["source"] = "stdout"
	if _, ok := requests["host_prefix"]; !ok {
		requests["host_prefix"] = "mcp__crewlet__"
	}
	raw := map[string]any{
		"binary": "cli", "complete_args": []any{"-p"}, "output": "jsonl",
		"event_type_path": []any{"type"}, "text_events": []any{"result"}, "text_paths": []any{[]any{"result"}},
		"mcp": map[string]any{"args": []any{"--server", "{relay_command}", "{socket}"},
			"tool_timeout_seconds": 60, "concurrency": "at_once"},
		"requests": requests,
	}
	if usage != nil {
		raw["usage"] = usage
	}
	p, err := harnessprofile.Load("custom", raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	host := newTestHost(t)
	var deltas []string
	req := llm.Request{Tools: host, OnDelta: func(d llm.Delta) { deltas = append(deltas, d.Content) }}
	return newRecordReader(p, req, "the prompt"), host, &deltas
}

// feed hands the reader each event as one line of stdout.
func feed(t *testing.T, r *recordReader, events ...map[string]any) {
	t.Helper()
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		r.stdoutLine(string(raw))
	}
}

func is(path []any, value any) map[string]any { return map[string]any{"path": path, "equals": value} }

// A RECORD THAT MARKS WHERE EACH REQUEST ENDS BEGINS THE NEXT WITH WHAT FOLLOWS
// THE END. Qwen Code's stream gives a response's prose and its calls events of
// their own, each under an id of its own, and the results of its calls end it;
// the next assistant event is the next request — not the one the results
// ended, and not nothing.
//
// Mutation: drop the reopening (an event with nothing open belongs to no
// request), and the second response is never read; drop the close, and both
// responses are one request.
func TestARecordThatEndsRequestsBeginsTheNextWithWhatFollows(t *testing.T) {
	t.Parallel()
	r, host, _ := readerOver(t, map[string]any{"events": []any{
		map[string]any{"match": []any{is([]any{"type"}, "assistant")}, "blocks": map[string]any{
			"path": []any{"content"},
			"each": []any{
				map[string]any{"match": []any{is([]any{"type"}, "text")}, "text": []any{"text"}},
				map[string]any{"match": []any{is([]any{"type"}, "tool_use")}, "call": map[string]any{"name": []any{"name"}}},
			},
		}},
		map[string]any{"match": []any{is([]any{"type"}, "user")}, "closes": true},
	}}, nil)
	feed(t, r,
		map[string]any{"type": "user", "content": "the prompt, before any request"},
		map[string]any{"type": "assistant", "content": []any{map[string]any{"type": "text", "text": "looking"}}},
		map[string]any{"type": "assistant", "content": []any{map[string]any{"type": "tool_use", "name": "mcp__crewlet__search"}}},
		map[string]any{"type": "user", "content": "the result"},
		map[string]any{"type": "assistant", "content": []any{map[string]any{"type": "text", "text": "done"}}},
	)
	r.finish()
	turns := host.latestTurns()
	if len(turns) != 2 {
		t.Fatalf("the host was told of %d requests, want 2: %+v", len(turns), turns)
	}
	if turns[0].Text != "looking" || len(turns[0].Calls) != 1 || turns[0].Calls[0].Name != "search" {
		t.Errorf("first request = %+v, want the prose and the call before the results", turns[0])
	}
	if turns[1].Text != "done" || len(turns[1].Calls) != 0 {
		t.Errorf("second request = %+v, want what followed the results", turns[1])
	}
}

// AN ENTRY WRITTEN AGAIN WHOLE REPLACES WHAT ITS REQUEST READ. Gemini CLI writes
// a response's entry as it arrives and again, whole, once its calls have run:
// read as more of the request, its prose would be told twice and streamed
// twice.
//
// Mutation: read a whole event as more of its request, and the prose is
// "looking\nlooking" and streams twice.
func TestAnEntryWrittenAgainWholeReplacesWhatItsRequestRead(t *testing.T) {
	t.Parallel()
	r, host, deltas := readerOver(t, map[string]any{"events": []any{
		map[string]any{"match": []any{is([]any{"type"}, "gemini")}, "whole": true, "id": []any{"id"},
			"text": []any{"content"}, "blocks": map[string]any{"path": []any{"toolCalls"},
				"each": []any{map[string]any{"call": map[string]any{"id": []any{"id"}, "name": []any{"name"}, "args": []any{"args"}}}}}},
	}}, nil)
	feed(t, r,
		map[string]any{"type": "gemini", "id": "g1", "content": "looking"},
		map[string]any{"type": "gemini", "id": "g1", "content": "looking", "toolCalls": []any{
			map[string]any{"id": "c1", "name": "mcp__crewlet__search", "args": map[string]any{"q": "x"}}}},
		map[string]any{"type": "gemini", "id": "g2", "content": "done"},
	)
	r.finish()
	turns := host.latestTurns()
	if len(turns) != 2 {
		t.Fatalf("the host was told of %d requests, want 2: %+v", len(turns), turns)
	}
	if turns[0].Text != "looking" || len(turns[0].Calls) != 1 || turns[0].Calls[0].ID != "c1" {
		t.Errorf("first request = %+v, want its prose once and the call its second writing added", turns[0])
	}
	if got := strings.Join(*deltas, "|"); got != "looking|done" {
		t.Errorf("streamed %q, want each response's prose once", got)
	}
}

// A RECORD WHOSE CALLS SHOW ONLY ONCE THEY HAVE RUN IS TOLD A REQUEST ONCE IT
// HAS MOVED PAST IT — and charged its counts the moment they are read. The
// host places each call on the request that asked for it, so a request told
// before its calls are in the record is one the host takes to have asked for
// none, and the call it is running for one of a later request.
//
// Mutation: tell such a request as it is read, and the host is told the first
// request with no calls before the record shows them; charge its counts only
// when it is told, and nothing is charged before the call.
func TestARecordWithLateCallsTellsARequestOnceTheRecordMovesPastIt(t *testing.T) {
	t.Parallel()
	r, host, _ := readerOver(t, map[string]any{"late_calls": true, "events": []any{
		map[string]any{"match": []any{is([]any{"type"}, "step")}, "id": []any{"id"}, "text": []any{"text"},
			"usage": map[string]any{"input": []any{[]any{"in"}}, "output": []any{[]any{"out"}}}},
		map[string]any{"match": []any{is([]any{"type"}, "tool")}, "id": []any{"id"},
			"call": map[string]any{"name": []any{"name"}}},
		map[string]any{"match": []any{is([]any{"type"}, "finish")}, "id": []any{"id"}, "closes": true},
	}}, nil)
	feed(t, r, map[string]any{"type": "step", "id": "s1", "text": "looking", "in": 100, "out": 7})
	if turns, _, spent, _ := host.snapshot(); len(turns) != 0 || spent.Input != 100 {
		t.Fatalf("before its calls showed the host was told %+v and charged %+v, want no request "+
			"and its counts", turns, spent)
	}
	feed(t, r, map[string]any{"type": "tool", "id": "s1", "name": "mcp__crewlet__search"})
	if turns, _, _, _ := host.snapshot(); len(turns) != 0 {
		t.Fatalf("a request was told before the record moved past it: %+v", turns)
	}
	feed(t, r, map[string]any{"type": "finish", "id": "s1"},
		map[string]any{"type": "step", "id": "s2", "text": "done", "in": 120, "out": 3})
	if turns := host.latestTurns(); len(turns) != 1 || len(turns[0].Calls) != 1 || turns[0].Text != "looking" {
		t.Fatalf("once it closed the host was told %+v, want the first request whole", turns)
	}
	r.finish()
	if turns := host.latestTurns(); len(turns) != 2 || turns[1].Text != "done" {
		t.Errorf("at the end the host was told %+v, want the last request too", turns)
	}
}

// A CALL FILED IN THE HOST'S NAMESPACE IS THE HOST'S. Codex's rollout keeps a
// call's namespace apart from its name; the server's tools are the namespace's,
// reported by their own names, and a call outside it is the harness's own.
//
// Mutation: read the namespace as nothing, and the host's call reads as one of
// the harness's own.
func TestACallInTheHostsNamespaceIsTheHosts(t *testing.T) {
	t.Parallel()
	r, host, _ := readerOver(t, map[string]any{"host_prefix": "", "host_namespace": "mcp__crewlet", "events": []any{
		map[string]any{"match": []any{is([]any{"type"}, "call")}, "opens": true,
			"call": map[string]any{"name": []any{"name"}, "namespace": []any{"namespace"}}},
	}}, nil)
	feed(t, r,
		map[string]any{"type": "call", "name": "search", "namespace": "mcp__crewlet"},
		map[string]any{"type": "call", "name": "apply_patch"},
	)
	r.finish()
	turns := host.latestTurns()
	if len(turns) != 2 || len(turns[0].Calls) != 1 || len(turns[1].Calls) != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	if c := turns[0].Calls[0]; c.Name != "search" || !c.Host {
		t.Errorf("the namespaced call read as %+v, want the host's search", c)
	}
	if c := turns[1].Calls[0]; c.Name != "apply_patch" || c.Host {
		t.Errorf("the harness's own call read as %+v", c)
	}
}

// A RESPONSE'S PROSE IS WHAT ITS DELTAS SPELLED WHERE NOTHING WRITES IT WHOLE,
// and a condition can hold of a value's beginning. Muse Code streams a
// response's prose in fragments that no later event repeats whole, and files a
// tool's side effect under `tool:<name>` beside the model's
// `model.meta.response`: the fragments run together are the request's prose —
// and its answer — and a call is read from the side effects of tools alone.
//
// The request is told as soon as its call is read, and told again as its prose
// grows past what it was told.
//
// Mutation: read each fragment as a line of text as well, and the first
// request is told as "Look"; leave the prose to text rules alone, and the
// answer is empty; leave a fragment unmarked as news, and the first request
// stays "Look"; match a
// prefix as a whole value, and the call is never read; ignore the prefix
// condition, and the model's own side effect is read as a call.
func TestAResponsesProseIsWhatItsDeltasSpelled(t *testing.T) {
	t.Parallel()
	r, host, deltas := readerOver(t, map[string]any{"host_prefix": "mcp__crewlet__", "events": []any{
		map[string]any{"match": []any{is([]any{"type"}, "proposed"), is([]any{"kind"}, "model")}, "opens": true,
			"id": []any{"task"}},
		map[string]any{"match": []any{is([]any{"type"}, "delta")}, "delta": []any{"text"}},
		map[string]any{"match": []any{is([]any{"type"}, "intent"),
			map[string]any{"path": []any{"operation"}, "prefix": "tool:"}},
			"call": map[string]any{"id": []any{"key"}, "name": []any{"operation"}, "name_prefix": "tool:"}},
	}}, nil)
	feed(t, r,
		map[string]any{"type": "proposed", "kind": "model", "task": "m1"},
		map[string]any{"type": "intent", "operation": "model.meta.response", "key": "model:m1"},
		map[string]any{"type": "delta", "text": "Look"},
		map[string]any{"type": "intent", "operation": "tool:mcp__crewlet__search", "key": "tool:call_1"},
		map[string]any{"type": "delta", "text": "ing it up."},
		map[string]any{"type": "proposed", "kind": "model", "task": "m2"},
		map[string]any{"type": "delta", "text": "Done: "},
		map[string]any{"type": "delta", "text": "the notes are in."},
	)
	r.finish()
	turns := host.latestTurns()
	if len(turns) != 2 {
		t.Fatalf("the host was told of %d requests, want 2: %+v", len(turns), turns)
	}
	if turns[0].Text != "Looking it up." || len(turns[0].Calls) != 1 {
		t.Fatalf("first request = %+v, want its streamed prose and the one tool call", turns[0])
	}
	if c := turns[0].Calls[0]; c.Name != "search" || !c.Host || c.ID != "tool:call_1" {
		t.Errorf("the call read as %+v, want the host's search under the harness's key", c)
	}
	if turns[1].Text != "Done: the notes are in." {
		t.Errorf("second request = %+v, want its streamed prose", turns[1])
	}
	if last, _, _ := r.last(); last != "Done: the notes are in." {
		t.Errorf("the answer is %q, want the last response's prose", last)
	}
	if got := strings.Join(*deltas, "|"); got != "Look|ing it up.|Done: |the notes are in." {
		t.Errorf("streamed %q, want each fragment once", got)
	}
}

// A RECORD'S COUNTS ARE IN THE ARITHMETIC OF THE RULE THAT READ THEM, not of
// the harness's final report: Gemini CLI's chat records each request's WHOLE prompt
// with its cached part inside it, where its final report gives the prompt less
// the cached part. Read in the report's arithmetic, every cached token of every
// request was charged twice.
//
// Mutation: read a record's counts in the profile's usage arithmetic, and the
// request is charged 1,600 input.
func TestARecordsCountsAreInTheArithmeticOfTheRuleThatReadThem(t *testing.T) {
	t.Parallel()
	r, host, _ := readerOver(t, map[string]any{"events": []any{
		map[string]any{"match": []any{is([]any{"type"}, "response")}, "opens": true, "text": []any{"text"},
			"usage": map[string]any{"input": []any{[]any{"prompt"}}, "cache_read": []any{[]any{"cached"}},
				"output": []any{[]any{"out"}}, "input_includes_cache": true}},
	}}, map[string]any{"input": []any{[]any{"usage", "input"}}, "output": []any{[]any{"usage", "output"}}})
	feed(t, r, map[string]any{"type": "response", "text": "x", "prompt": 1000, "cached": 600, "out": 5})
	r.finish()
	if _, _, spent, _ := host.snapshot(); spent != (llm.Usage{Input: 1000, Output: 5, CacheRead: 600}) {
		t.Errorf("charged %+v, want the whole prompt once, the cached part inside it", spent)
	}
}

// A FILE RECORD WHOSE NAME DOES NOT SAY WHOSE IT IS IS TAKEN BY ITS FIRST LINE.
// Codex and Copilot name a run's record after an id they mint themselves, in a
// directory every concurrent call of the seat writes to: a file the glob
// finds whose first line names another run is passed over for good, and one
// still being written is looked at again rather than judged on half a line.
//
// Mutation: take the first new file the glob finds, and the record read is the
// other run's.
func TestAFileRecordIsTakenByAFirstLineNamingTheRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := recordOwner(&harnessprofile.RecordOwner{Path: harnessprofile.Path{"cwd"}, Equals: "{work}"}, "", work)
	var mu sync.Mutex
	var lines []string
	stop := make(chan struct{})
	wait := tailRecord(filepath.Join(dir, "*.jsonl"), owner, func(l string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, l)
	}, stop)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Another run's record, then ours begun but its first line unfinished.
	write("a-other.jsonl", `{"cwd":"/elsewhere"}`+"\n"+`{"line":"OTHER"}`+"\n")
	write("b-ours.jsonl", `{"cwd":`)
	time.Sleep(3 * recordPoll)
	write("b-ours.jsonl", `{"cwd":`+jsonString(work)+`}`+"\n"+`{"line":"OURS"}`+"\n")
	time.Sleep(3 * recordPoll)
	close(stop)
	wait()
	mu.Lock()
	defer mu.Unlock()
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "OTHER") || !strings.Contains(got, "OURS") {
		t.Errorf("the tail read %q, want our record and none of the other's", got)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, `"cwd"`) }) {
		t.Errorf("the tail did not read our record from its first line: %q", got)
	}
}

// A FILE RECORD THAT NAMES ITS RUN ONLY IN A LATER EVENT IS TAKEN BY THAT EVENT.
// Kimi Code's wire log opens with its protocol version, which every run's has,
// and names the call's working directory in the profile.bind event a few lines
// on: the owner's match picks that line, a file whose line names another run is
// passed over, and one that has not written it yet is looked at again rather
// than judged on its header.
//
// Mutation: judge a file by its first line whatever the owner's match, and no
// file is ever this run's.
func TestAFileRecordIsTakenByTheEventThatNamesTheRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := recordOwner(&harnessprofile.RecordOwner{
		Match: []harnessprofile.Match{{Path: harnessprofile.Path{"type"}, Equals: "profile.bind"}},
		Path:  harnessprofile.Path{"environmentDisclosure", "cwd"}, Equals: "{work}",
	}, "", work)
	var mu sync.Mutex
	var lines []string
	stop := make(chan struct{})
	wait := tailRecord(filepath.Join(dir, "*.jsonl"), owner, func(l string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, l)
	}, stop)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const header = `{"type":"metadata","protocol_version":"1.5"}` + "\n" + `{"type":"runtime.set_binding"}` + "\n"
	bind := func(cwd string) string {
		return `{"type":"profile.bind","environmentDisclosure":{"cwd":` + jsonString(cwd) + `}}` + "\n"
	}
	write("a-other.jsonl", header+bind("/elsewhere")+`{"line":"OTHER"}`+"\n")
	write("b-ours.jsonl", header)
	time.Sleep(3 * recordPoll)
	write("b-ours.jsonl", header+bind(work)+`{"line":"OURS"}`+"\n")
	time.Sleep(3 * recordPoll)
	close(stop)
	wait()
	mu.Lock()
	defer mu.Unlock()
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "OTHER") || !strings.Contains(got, "OURS") {
		t.Errorf("the tail read %q, want our record and none of the other's", got)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "protocol_version") }) {
		t.Errorf("the tail did not read our record from its first line: %q", got)
	}
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
