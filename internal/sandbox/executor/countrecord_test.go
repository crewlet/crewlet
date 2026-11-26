package executor_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// recordFixture is one runner's own record of a session, in the shape its
// pinned release writes it (measured), with how the run's script turns the
// run's own lines of it into the lines its stream carries.
type recordFixture struct {
	// path is where the record is in a box; pointed whether the script names
	// it in the box's pointer ([executor.Paths.CountRecord]).
	path    func(p executor.Paths) string
	pointed bool
	// depth is how many directories down from its root the record lies.
	depth int
	// earlier is an earlier run's lines, which open the record; request is
	// request i's lines, one of them the request.
	earlier []string
	request func(i int) []string
	// stream is what the script appends to the stream of the run's own lines;
	// nil where a helper of the harness's counts them ([geminiHelperCount]).
	stream func(own []string) string
}

const fixtureSession = "0b5e0000-0000-4000-8000-000000000001"

func codexTokenCount(i int) string {
	return fmt.Sprintf(`{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":3}}}}`, i)
}

func copilotUsage(i int) string {
	return fmt.Sprintf(`{"type":"session.usage_record","data":{"usage":{"inputTokens":%d,"outputTokens":1}}}`, i)
}

func museEvent(i int, kind string) string {
	return fmt.Sprintf(`{"schema_version":1,"id":"e%[1]d","stream":{"kind":"session","id":"s"},"sequence":%[1]d,"payload_type":"runtime.session","payload":{"event":{"base_instructions":"b","kind":"%[2]s"},"kind":"run"}}`, i, kind)
}

func geminiEntry(id string, tokens string) string {
	return fmt.Sprintf(`{"id":"%s","timestamp":"t","type":"gemini","content":"step","tokens":%s,"model":"gemini-2.5-pro"}`, id, tokens)
}

// wrapEach is each line of own that keep holds, wrapped as a script's sed wraps it.
func wrapEach(own []string, keep func(string) bool, prefix string) string {
	var b strings.Builder
	for _, l := range own {
		if keep(l) {
			b.WriteString(prefix + l + "}\n")
		}
	}
	return b.String()
}

var recordFixtures = map[string]recordFixture{
	executor.CodexName: {
		path: func(p executor.Paths) string {
			return p.CodexSessions() + "/2026/11/25/rollout-2026-11-25T00-00-00-01a20286-faf2-7490-a21d-7f1215273287.jsonl"
		},
		pointed: true, depth: 3,
		earlier: []string{codexTokenCount(91), codexTokenCount(92)},
		request: func(i int) []string {
			return []string{`{"timestamp":"t","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}`,
				codexTokenCount(i)}
		},
		stream: func(own []string) string {
			kept := regexp.MustCompile(executor.CodexRolloutKinds)
			return wrapEach(own, func(l string) bool { return kept.MatchString(l) }, `{"type":"crewlet.rollout","line":`)
		},
	},
	executor.HermesName: {
		path: func(p executor.Paths) string { return p.HermesRecord() },
		request: func(i int) []string {
			return []string{fmt.Sprintf(`{"api_request_id":"r%d","turn_id":"t_1","model":"m","finish_reason":"tool_calls","context_length":10,"tool_calls":1,"usage":{"input_tokens":1,"output_tokens":1,"cache_read_tokens":0,"cache_write_tokens":0}}`, i)}
		},
		stream: func(own []string) string {
			return wrapEach(own, func(string) bool { return true }, `{"type":"crewlet.hermes_request","line":`)
		},
	},
	executor.CopilotName: {
		path: func(p executor.Paths) string {
			return p.CopilotHome() + "/session-state/" + fixtureSession + "/events.jsonl"
		},
		pointed: true, depth: 1,
		earlier: []string{copilotUsage(91), copilotUsage(92)},
		request: func(i int) []string {
			return []string{fmt.Sprintf(`{"type":"assistant.turn_start","data":{"turnId":"%d"}}`, i), copilotUsage(i)}
		},
		stream: func(own []string) string {
			var b strings.Builder
			b.WriteString(`{"type":"crewlet.usage"}` + "\n")
			for _, l := range own {
				if strings.HasPrefix(l, `{"type":"session.usage_record"`) {
					b.WriteString(l + "\n")
				}
			}
			return b.String()
		},
	},
	executor.MuseName: {
		path: func(p executor.Paths) string {
			return p.MuseSessions() + "/2026/11/25/" + fixtureSession + "/session.jsonl"
		},
		pointed: true, depth: 4,
		earlier: []string{museEvent(91, "model_input_trace_recorded"), museEvent(92, "model_input_trace_recorded")},
		request: func(i int) []string {
			return []string{museEvent(10*i, "model_input_trace_recorded"), museEvent(10*i+1, "model_completed")}
		},
		stream: func(own []string) string {
			return wrapEach(own, func(l string) bool { return strings.Contains(l, executor.MuseTraceNeedle) },
				`{"payload_type":"crewlet.muse_trace","line":`)
		},
	},
	executor.GeminiCLIName: {
		path: func(p executor.Paths) string {
			return p.GeminiChats() + "/session-2026-11-25T00-00-" + fixtureSession[:8] + ".jsonl"
		},
		pointed: true,
		request: func(i int) []string {
			id := fmt.Sprintf("r-%d", i)
			return []string{fmt.Sprintf(`{"id":"u%d","timestamp":"t","type":"user","content":[{"text":"go"}]}`, i),
				geminiEntry(id, `{"input":1,"output":1}`), geminiEntry(id, `{"input":1,"output":2}`)}
		},
	},
}

// fixtureRecord is fixture's record of made requests, the request line of
// request long grown past [countLineBound] in its middle (0 for none), and
// the run's own lines of it.
func fixtureRecord(fixture recordFixture, made, long int) (string, []string) {
	var own []string
	for i := 1; i <= made; i++ {
		lines := fixture.request(i)
		if i == long {
			last := lines[len(lines)-1]
			lines[len(lines)-1] = last[:len(last)/2] + strings.Repeat("x", 3*countLineBound) + last[len(last)/2:]
		}
		own = append(own, lines...)
	}
	all := append(append([]string{}, fixture.earlier...), own...)
	if strings.Contains(fixture.path(executor.Paths{Home: "/h"}), "/chats/") {
		all = append([]string{`{"sessionId":"` + fixtureSession + `","kind":"main"}`}, all...)
	}
	return strings.Join(all, "\n") + "\n", own
}

// pointTo names path in b's pointer, with fixture's earlier lines.
func pointTo(b *sandbox.FakeSandbox, fixture recordFixture, path string) {
	skip := len(fixture.earlier)
	if strings.Contains(path, "/chats/") {
		skip = 0
	}
	b.Put(executor.PathsFor(b).CountRecord(), path+"\n"+strconv.Itoa(skip)+"\n")
}

// recordBox is a box for runner with fixture's record, of record, at its
// place, pointed at where the script names it.
func recordBox(t *testing.T, runner *executor.Runner, fixture recordFixture) *sandbox.FakeSandbox {
	t.Helper()
	b := box(t, runner)
	p := executor.PathsFor(b)
	if fixture.pointed {
		pointTo(b, fixture, fixture.path(p))
	}
	b.Put(p.GeminiUsageBefore(), "")
	return b
}

// EVERY RUNNER IS COUNTED LIVE: off its stream, held to a stream of requests
// ([requestStreams]), or, where its stream shows its requests only once its
// harness exits, off the harness's own record of the session, held to a
// record of requests ([recordFixtures]). A runner neither counts nothing while
// it runs, and fails.
//
// Mutation: answer no count for a runner, or count one off its stream that
// shows its requests only at its end, and its row fails.
func TestEveryRunnerIsCountedLive(t *testing.T) {
	t.Parallel()
	for _, h := range executor.Registered() {
		if executor.New(h).Count(sandbox.RunHandle{TurnCap: 3}) == nil {
			t.Errorf("%s answers no live count", h.Name())
			continue
		}
		_, recorded := recordFixtures[h.Name()]
		switch {
		case executor.RecordCounted(h) && !recorded:
			t.Errorf("%s is counted off its own record and has no record of requests to hold its count to", h.Name())
		case !executor.RecordCounted(h) && recorded:
			t.Errorf("%s has a record of requests and is counted off its stream, which shows its requests only "+
				"once it exits", h.Name())
		}
	}
}

// A LIVE COUNT OFF A HARNESS'S OWN RECORD IS ITS COLLECTION'S COUNT, HOWEVER
// THE RECORD ARRIVES: for every runner counted off its record, the record read
// as it grows (split mid-line, and inside a line past the bound), a few bytes a
// read, and after a rewrite that shrank it, ends on exactly what its collection
// counts of the lines its script appends of the same record, an earlier run's
// lines left out; no read's count is below the one before; and a read takes
// only the bytes the record grew by.
//
// Mutation: count an earlier run's lines, judge a line past the bound
// otherwise than the collection, keep no partial line between reads, let a
// shrink lower the count, or read the record from its start each time, and a
// runner's row fails.
func TestALiveRecordCountIsItsCollectionsCountHoweverTheRecordArrives(t *testing.T) {
	t.Parallel()
	const made = 6
	for name, fixture := range recordFixtures {
		if fixture.stream == nil {
			continue // TestAGeminiLiveCountIsItsHelpersCount holds it
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.Bounded(executor.New(harnessNamed(t, name)), sandbox.MaxFailureBytes, countLineBound)
			record, own := fixtureRecord(fixture, made, 2)
			want, err := executor.CollectedCount(runner, []byte(fixture.stream(own)))
			if err != nil {
				t.Fatalf("the collection's count: %v", err)
			}
			if want.Requests < made-1 {
				t.Fatalf("the premise: the collection counts %+v of a record of %d requests", want, made)
			}
			b := recordBox(t, runner, fixture)
			at := fixture.path(executor.PathsFor(b))
			read := recordReader(t, b)

			t.Run("as it grows", func(t *testing.T) {
				c := executor.CountingBy(runner, executor.CountReadBytes)
				var got sandbox.RequestCount
				for end := 0; end < len(record); end += 97 {
					b.Put(at, record[:end])
					got = read(c, got, "grown to "+strconv.Itoa(end))
				}
				b.Put(at, record)
				if got = read(c, got, "whole"); got != want {
					t.Fatalf("read as it grew: %+v; the collection counts %+v", got, want)
				}
			})
			t.Run("a few bytes a read", func(t *testing.T) {
				c := executor.CountingBy(runner, 61)
				b.Put(at, record)
				var got sandbox.RequestCount
				for range len(record)/61 + 1 {
					got = read(c, got, "a read")
				}
				if got != want {
					t.Fatalf("read 61 bytes at a time: %+v; the collection counts %+v", got, want)
				}
			})
			t.Run("a read takes what grew", func(t *testing.T) {
				c := executor.CountingBy(runner, executor.CountReadBytes)
				b.Put(at, record)
				got := read(c, sandbox.RequestCount{}, "whole")
				grew := fixture.request(made + 1)[0] + "\n"
				before := b.Delivered()
				b.Put(at, record+grew)
				read(c, got, "grown")
				if took := b.Delivered() - before; took != int64(len(grew)) {
					t.Fatalf("a read after the record grew %d bytes took %d of its %d", len(grew), took,
						len(record+grew))
				}
			})
			t.Run("rewritten shorter", func(t *testing.T) {
				c := executor.CountingBy(runner, executor.CountReadBytes)
				b.Put(at, record)
				before := read(c, sandbox.RequestCount{}, "whole")
				b.Put(at, record[:len(record)/4])
				read(c, before, "shrunk")
				b.Put(at, record)
				if got := read(c, before, "written again"); got != want {
					t.Fatalf("read again after a shrink: %+v; the collection counts %+v", got, want)
				}
			})
		})
	}
}

// recordReader is one read of a count of b, failing the case where the count
// went down.
func recordReader(t *testing.T, b *sandbox.FakeSandbox) func(sandbox.Counter, sandbox.RequestCount, string) sandbox.RequestCount {
	return func(c sandbox.Counter, last sandbox.RequestCount, what string) sandbox.RequestCount {
		t.Helper()
		got, err := c.Read(t.Context(), b)
		if err != nil {
			t.Fatalf("%s: Read: %v", what, err)
		}
		if got.Requests < last.Requests || got.Undecided < last.Undecided {
			t.Fatalf("%s: the count went from %+v down to %+v", what, last, got)
		}
		return got
	}
}

// harnessNamed is the registered harness named name.
func harnessNamed(t *testing.T, name string) executor.Harness {
	t.Helper()
	for _, h := range executor.Registered() {
		if h.Name() == name {
			return h
		}
	}
	t.Fatalf("no harness %s is registered", name)
	return nil
}

// A GEMINI CLI RUN'S LIVE COUNT IS ITS HELPER'S COUNT of the same chat file:
// the distinct responses with tokens, but those the session held as the run
// began, each counted once however often the file writes it again, a line past
// the bound included.
//
// Mutation: count each entry rather than each response, or count one with no
// tokens or one the session held already, and the count is not the helper's.
func TestAGeminiLiveCountIsItsHelpersCount(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	fixture := recordFixtures[executor.GeminiCLIName]
	runner := executor.Bounded(executor.New(executor.GeminiCLI{}), sandbox.MaxFailureBytes, countLineBound)
	record, _ := fixtureRecord(fixture, 5, 2)
	record += geminiEntry("held-1", `{"input":1}`) + "\n" + geminiEntry("no-tokens", "null") + "\n"
	b := recordBox(t, runner, fixture)
	p := executor.PathsFor(b)
	b.Put(fixture.path(p), record)
	b.Put(p.GeminiUsageBefore(), "held-1\n")

	chats := t.TempDir()
	if err := os.WriteFile(filepath.Join(chats, filepath.Base(fixture.path(p))), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	before := filepath.Join(t.TempDir(), "before")
	if err := os.WriteFile(before, []byte("held-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join(".", "gemini_helper.mjs"), "usage", chats,
		fixtureSession, before).Output()
	if err != nil {
		t.Fatalf("the helper's usage: %v", err)
	}
	var usage struct {
		Requests int `json:"requests"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &usage); err != nil || usage.Requests != 5 {
		t.Fatalf("the premise: the helper counts %s (%v), want the five responses", out, err)
	}
	got, err := executor.CountingBy(runner, executor.CountReadBytes).Read(t.Context(), b)
	if err != nil || got.Requests != usage.Requests {
		t.Fatalf("the live count reads %+v (%v); the helper counts %d", got, err, usage.Requests)
	}
}

// A POINTER OUTSIDE ITS RECORD'S PLACE COUNTS NOTHING: a run's script names its
// record in the box's pointer, which the run can write too, so the count reads
// the file it names only where it is the harness's own record's place (under
// its root, at its depth, by its name, its path already clean), whatever that
// file holds; a pointer refused leaves the run to its collection.
//
// Mutation: read any file the pointer names, or hold it to the root alone, and
// a row counts the requests its file holds.
func TestAPointerOutsideItsRecordsPlaceCountsNothing(t *testing.T) {
	t.Parallel()
	for name, fixture := range recordFixtures {
		if !fixture.pointed {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.New(harnessNamed(t, name))
			record, _ := fixtureRecord(fixture, 4, 0)
			valid := recordBox(t, runner, fixture)
			valid.Put(fixture.path(executor.PathsFor(valid)), record)
			if got, err := runner.Count(sandbox.RunHandle{}).Read(t.Context(), valid); err != nil || got.Requests != 4 {
				t.Fatalf("the premise: the record at its place counts %+v (%v), want 4", got, err)
			}
			at := fixture.path(executor.PathsFor(valid))
			dir, base := filepath.Dir(at), filepath.Base(at)
			root := dir
			for range fixture.depth {
				root = filepath.Dir(root)
			}
			named := map[string]string{
				"outside the home's record root": executor.PathsFor(valid).WorkDir() + "/" + base,
				"climbing out of it":             dir + "/../../" + base,
				"a level deeper":                 dir + "/x/" + base,
				"under another name":             dir + "/other.jsonl",
				"relative":                       strings.TrimPrefix(at, "/"),
			}
			if fixture.depth > 0 {
				// AT ITS OWN DEPTH, each directory a climb: out of the root.
				named["climbing out at its own depth"] = root + strings.Repeat("/..", fixture.depth) + "/" + base
			}
			for why, named := range named {
				b := recordBox(t, runner, fixture)
				b.Put(named, record)
				pointTo(b, fixture, named)
				if got, err := runner.Count(sandbox.RunHandle{}).Read(t.Context(), b); err != nil || got.Requests != 0 {
					t.Errorf("a pointer %s (%s) counts %+v (%v), want nothing", why, named, got, err)
				}
			}
			b := recordBox(t, runner, fixture)
			b.Put(at, record)
			b.Put(executor.PathsFor(b).CountRecord(), at+"\n-1\n")
			if got, err := runner.Count(sandbox.RunHandle{}).Read(t.Context(), b); err != nil || got.Requests != 0 {
				t.Errorf("a pointer with a negative line count counts %+v (%v), want nothing", got, err)
			}
		})
	}
}

// A COPILOT JOURNAL'S CHECKPOINTS ARE NO REQUESTS: a rotation copies the
// session's earlier records into a new head as checkpoints, which the
// collection's usage records never include, so a live count reading the new
// head counts none of them, a compaction's start included.
//
// Mutation: count a checkpoint, and the count takes the copies too.
func TestACopilotJournalsCheckpointsAreNoRequests(t *testing.T) {
	t.Parallel()
	fixture := recordFixtures[executor.CopilotName]
	runner := executor.New(executor.Copilot{})
	b := recordBox(t, runner, fixture)
	record, _ := fixtureRecord(fixture, 2, 0)
	record += `{"type":"session.compaction_start","checkpoint":true,"data":{}}` + "\n" +
		`{"type":"session.usage_record","checkpoint":true,"data":{"usage":{"inputTokens":1,"outputTokens":1}}}` + "\n"
	b.Put(fixture.path(executor.PathsFor(b)), record)
	if got, err := runner.Count(sandbox.RunHandle{}).Read(t.Context(), b); err != nil || got.Requests != 2 {
		t.Fatalf("a journal of two requests and two checkpoints counts %+v (%v), want the two", got, err)
	}
}
