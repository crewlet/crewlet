package executor_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// Where an OpenCode run began in the session it continues, recorded by its
// script ([fakeOpenCode] answering its server's message list) and read back
// off its record and its capture.

// startCursor is the cursor OpenCode's message list ends a page with as `next`
// where the page's message is id: the unpadded base64url of JSON.stringify of
// {id, order, direction}, as the 2.0.32 and 2.2.7 pages hold it
// (testdata/opencode/<version>/context).
func startCursor(id, order, direction string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(`{"id":"` + id + `","order":"` + order + `","direction":"` + direction + `"}`))
}

// newestPage is the one-message page a run's server answers its script's ask
// for the session's newest message, the message being id's.
func newestPage(id string) string {
	return `{"data":[{"id":"` + id + `","sessionID":"ses_x","type":"user","text":"x","time":{"created":1}}],` +
		`"cursor":{"previous":"` + startCursor(id, "desc", "previous") + `","next":"` +
		startCursor(id, "desc", "next") + `"}}`
}

// recordLines is the session record the run's script left, a line each.
func recordLines(t *testing.T, j *scriptJob) []string {
	t.Helper()
	raw, err := os.ReadFile(j.paths.SessionRecord())
	if err != nil {
		t.Fatalf("reading the session record: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// fromLine is the record's `from` line, or "" where it has none.
func fromLine(lines []string) string {
	for _, line := range lines {
		if from, ok := strings.CutPrefix(line, "from "); ok {
			return from
		}
	}
	return ""
}

// A CONTINUED RUN RECORDS WHERE ITS SESSION STOOD BEFORE IT BEGAN, under every sh
// a box may run its script with: once its server is up, and before the run, it
// asks the server for the session's newest message, newest first by name, one
// message, and writes its record anew with that message's cursor as `from`,
// before the release, which the record's reader reads last. A run that began
// its session records none, its `continued no` saying the whole session is its
// own, nor one asked to continue a session its box could not, which began one
// in its place; nor does one whose server answered no cursor; and a `next` key
// inside the message is never taken for the answer's own.
//
// Mutation: write the line for a run that began its session in its place,
// after the release, or unanchored, or ask the list in its default order, and
// a case goes red.
func TestAContinuedRunRecordsWhereItsSessionStoodBeforeItBegan(t *testing.T) {
	t.Parallel()
	const newest = "msg_1d8699fae001Qp6E4C1MPr6kP4"
	continued := scriptOpts{session: &continuingOpenCode, held: []string{carriedOpenCode},
		req: sandbox.RunRequest{Resume: carriedOpenCode}}
	// A RUN ASKED TO CONTINUE A SESSION ITS BOX DOES NOT HOLD, and no export of
	// it carried in, starts the one minted in its place (`continued no`): it
	// began its session, and asks nothing of where.
	fellBack := scriptOpts{session: &continuingOpenCode, req: sandbox.RunRequest{Resume: carriedOpenCode}}
	nested := `{"data":[{"id":"` + newest + `","type":"assistant","content":[{"type":"text",` +
		`"text":"\"cursor\":{\"next\":\"` + startCursor("msg_bogus", "desc", "next") + `\"}}"}]}],` +
		`"cursor":{"previous":"cHJldmlvdXM","next":"` + startCursor(newest, "desc", "next") + `"}}`
	for name, tc := range map[string]struct {
		opts scriptOpts
		page string
		want string
	}{
		"a run that continues its session":  {opts: continued, page: newestPage(newest), want: startCursor(newest, "desc", "next")},
		"a run that began its session":      {page: newestPage(newest)},
		"a run that began one in its place": {opts: fellBack, page: newestPage(newest)},
		"a server that answered no cursor":  {opts: continued, page: `{"data":[]}`},
		"a next key inside the message":     {opts: continued, page: nested, want: startCursor(newest, "desc", "next")},
		"a server that answered nothing":    {opts: continued},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			forEachShell(t, func(t *testing.T, sh boxShell) {
				o := tc.opts
				o.shell = sh
				if tc.page != "" {
					o.pages = map[string]string{"first": tc.page}
				}
				j := startScript(t, o)
				run := j.wait()
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				lines := recordLines(t, j)
				if got := fromLine(lines); got != tc.want {
					t.Fatalf("the record says the run began after %q, want %q:\n%s", got, tc.want,
						strings.Join(lines, "\n"))
				}
				if tc.want == "" {
					return
				}
				if len(lines) != 5 || lines[3] != "from "+tc.want || !strings.HasPrefix(lines[4], "release ") {
					t.Errorf("the record is\n%s\nwant its four keys and `from` before the release",
						strings.Join(lines, "\n"))
				}
				asked := listed(t, run)
				if len(asked) == 0 || !strings.HasSuffix(asked[0], " --param order=desc --param limit=1") {
					t.Errorf("the list was asked\n%s\nwant the newest message first, named so, one message",
						strings.Join(asked, "\n"))
				}
				if at, before := run.callIndex("api --server http://127.0.0.1:4242 session.message.list"),
					run.callIndex("run "); at < 0 || at > before {
					t.Errorf("the session was asked where it stood at call %d, the run started at %d", at, before)
				}
			})
		})
	}
}

// AN END REQUEST BEFORE THE RUN RECORDS NO START. A request that comes while the
// run waits for its MCP servers starts no run, so there is no run to have begun
// anywhere, and the script asks nothing more of its server before its tail.
//
// Mutation: ask for the start as a call no end request ends, and the record
// names a start of a run that never ran.
func TestAnEndRequestBeforeTheRunRecordsNoStart(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: []string{carriedOpenCode},
			req:   sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly},
			times: &executor.OpenCodeTimes{Call: time.Minute, MCP: time.Minute, Sync: time.Hour},
			env:   []string{"FAKE_HANG=mcp.list", "FAKE_HANG_ONCE=1"},
			pages: map[string]string{"first": newestPage("msg_1d8699fae001Qp6E4C1MPr6kP4")}})
		j.await("the readiness check to wait on its server", func() bool {
			return strings.Contains(readIn(t, j.dir, "hung"), "mcp.list ")
		})
		j.signalScript(syscall.SIGUSR1)
		run := j.waitWithin(promptEnd, "an end request before the run waited on something")
		if from := fromLine(recordLines(t, j)); from != "" || run.callIndex("run ") >= 0 {
			t.Errorf("a request before the run left a start %q (run started: %v)", from, run.callIndex("run ") >= 0)
		}
	})
}

// A RUN'S START ASKED PAST ITS BOUND RECORDS NOTHING. The ask for the session's
// newest message is a page of its message list, bounded as every page is
// ([executor.OpenCodeTimes.Page]): a server that takes it and never answers
// costs the bound, the run starts all the same, and its record names no start
// — not known, which no reading of the capture reads.
//
// Mutation: ask unbounded, and the run never starts.
func TestARunStartAskedPastItsBoundRecordsNothing(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		times := boundedTimes
		j := startScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: []string{carriedOpenCode},
			req: sandbox.RunRequest{Resume: carriedOpenCode}, times: &times,
			env:   []string{"FAKE_HANG=session.message.list", "FAKE_HANG_ONCE=1"},
			pages: map[string]string{"first": newestPage("msg_1d8699fae001Qp6E4C1MPr6kP4")}})
		run := j.waitWithin(promptEnd, "an ask for the run's start its server never answered held the script")
		if run.code != 0 || run.callIndex("run ") < 0 {
			t.Fatalf("the run did not start past the ask (exit %d):\n%s", run.code, run.stderr)
		}
		if from := fromLine(recordLines(t, j)); from != "" {
			t.Errorf("the record names a start, %q, of an ask its server never answered", from)
		}
		assertHungEnded(t, run.dir, "session.message.list")
	})
}

// A RECORD'S START IS READ WHOLE OR NOT AT ALL. The cursor is the box's word,
// as the record's id and release are: read only as a `next` cursor of the list
// asked newest first, an object of exactly its three keys, each once and each
// a string, naming a message by an id a box may name, at most
// [sandbox.MaxMessageIDBytes] and never cut. Anything else names no start.
//
// Mutation: cut a long id to its bound, take a cursor of another order or
// direction, or let a key go unchecked, and a row goes red.
func TestARecordsStartIsReadWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	const id = "msg_1d8699fae001Qp6E4C1MPr6kP4"
	at64, at65 := "msg_"+strings.Repeat("a", 60), "msg_"+strings.Repeat("a", 61)
	enc := func(object string) string { return base64.RawURLEncoding.EncodeToString([]byte(object)) }
	for name, tc := range map[string]struct{ cursor, want string }{
		"OpenCode's own cursor":        {cursor: startCursor(id, "desc", "next"), want: id},
		"an id at its bound":           {cursor: startCursor(at64, "desc", "next"), want: at64},
		"an id past its bound":         {cursor: startCursor(at65, "desc", "next")},
		"an id holding a dot":          {cursor: startCursor("msg_a.b", "desc", "next")},
		"an id that is empty":          {cursor: startCursor("", "desc", "next")},
		"the oldest first":             {cursor: startCursor(id, "asc", "next")},
		"the page before":              {cursor: startCursor(id, "desc", "previous")},
		"no id":                        {cursor: enc(`{"order":"desc","direction":"next"}`)},
		"a key more":                   {cursor: enc(`{"id":"` + id + `","order":"desc","direction":"next","x":1}`)},
		"an id written twice":          {cursor: enc(`{"id":"msg_a","id":"` + id + `","order":"desc","direction":"next"}`)},
		"an id that is no string":      {cursor: enc(`{"id":7,"order":"desc","direction":"next"}`)},
		"a key spelled in other case":  {cursor: enc(`{"ID":"` + id + `","order":"desc","direction":"next"}`)},
		"padded":                       {cursor: base64.URLEncoding.EncodeToString([]byte(`{"id":"` + id + `","order":"desc","direction":"next"}`))},
		"not base64url":                {cursor: "not a cursor"},
		"no object":                    {cursor: enc(`["` + id + `"]`)},
		"past the longest a cursor is": {cursor: startCursor(id, "desc", "next") + strings.Repeat("A", 200)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			record := "id " + carriedOpenCode + "\ncontinued yes\nwhy \nfrom " + tc.cursor + "\nrelease opencode v2.2.7\n"
			if began, after := executor.RecordStart(record); began || after != tc.want {
				t.Errorf("the record's start reads (began %v, after %q), want after %q", began, after, tc.want)
			}
		})
	}
}

// A RECORD WITHOUT ITS CONTINUED LINE NAMES NO START. Only a run that continued
// its session (`continued yes`) began after a message, and only a run whose line
// reads `no` began its session: a record with no such line, or one reading
// anything else, says neither — not known, never the whole session — and a
// `from` beside `continued no` is no start.
//
// Mutation: read a missing or garbled line as a run that began its session, or
// read `from` whatever the line says, and a row goes red.
func TestARecordWithoutItsContinuedLineNamesNoStart(t *testing.T) {
	t.Parallel()
	const id = "msg_1d8699fae001Qp6E4C1MPr6kP4"
	from := "from " + startCursor(id, "desc", "next") + "\n"
	for name, tc := range map[string]struct {
		record    string
		began     bool
		wantAfter string
	}{
		"the id alone":                  {record: "id " + carriedOpenCode + "\n"},
		"a garbled line":                {record: "id " + carriedOpenCode + "\ncontinued maybe\nwhy \n" + from + "release v\n"},
		"a run that began":              {record: "id " + carriedOpenCode + "\ncontinued no\nwhy \nrelease v\n", began: true},
		"a start beside began":          {record: "id " + carriedOpenCode + "\ncontinued no\nwhy \n" + from + "release v\n", began: true},
		"a run that continued":          {record: "id " + carriedOpenCode + "\ncontinued yes\nwhy \n" + from + "release v\n", wantAfter: id},
		"a run that continued, no from": {record: "id " + carriedOpenCode + "\ncontinued yes\nwhy \nrelease v\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if began, after := executor.RecordStart(tc.record); began != tc.began || after != tc.wantAfter {
				t.Errorf("the record's start reads (began %v, after %q), want (%v, %q)", began, after, tc.began,
					tc.wantAfter)
			}
		})
	}
}

// A `FROM` LINE AFTER THE RELEASE IS NEVER READ. The script writes the release
// last, as the harness printed it, and a `--version` printed over several lines
// runs on after it: a `from` there is the harness's text, as is one the box
// appended. The record is read no further than its four keys.
//
// Mutation: take the last `from`, or read past the release, and the start is
// the box's.
func TestAFromLineAfterTheReleaseIsNeverRead(t *testing.T) {
	t.Parallel()
	from := "from " + startCursor("msg_1d8699fae001Qp6E4C1MPr6kP4", "desc", "next") + "\n"
	for name, record := range map[string]string{
		"a release of two lines":   "id " + carriedOpenCode + "\ncontinued yes\nwhy \nrelease opencode v2.2.7\n" + from,
		"a from the box appended":  "id " + carriedOpenCode + "\ncontinued yes\nwhy \nrelease v\n\n" + from,
		"a record missing its why": "id " + carriedOpenCode + "\ncontinued yes\nrelease v\n" + from,
	} {
		if began, after := executor.RecordStart(record); began || after != "" {
			t.Errorf("%s: the record's start reads (began %v, after %q), want none", name, began, after)
		}
	}
}

// THE TAKE SPELLS THE RUN'S START AS ITS SCRUB SPELLS THE EXPORT. The capture
// names the message its run began after by the id the kept session holds that
// message under: the start passes the very rewrite every message id of the
// export passes, so it is found in the kept session, once. Where that rewrite
// leaves no message id — a value the take names inside it — the start is
// dropped and the capture kept, naming none.
//
// Mutation: keep the start as the box wrote it, and the scrubbed case names an
// id the kept session does not hold; refuse the capture over it, and its take
// fails.
func TestTheTakeSpellsTheRunsStartAsItsScrubSpellsTheExport(t *testing.T) {
	t.Parallel()
	const (
		session = "ses_0123456789abcdef0123456789"
		start   = "msg_1d8699fae001Qp6E4C1MPr6kP4"
	)
	for name, tc := range map[string]struct {
		scrub []string
		want  string
	}{
		"an id the scrub keeps":       {want: start},
		"an id holding a value taken": {scrub: []string{"fae001Qp6E4C"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := openCodeBox(t, t.TempDir())
			dir := t.TempDir()
			writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
				strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shQuote(dir), 1))
			record := "id " + session + "\ncontinued yes\nwhy \nfrom " + startCursor(start, "desc", "next") +
				"\nrelease opencode v2.2.7\n"
			if err := b.WriteFile(t.Context(), paths(b).SessionRecord(), []byte(record)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			messages := `[{"id":"` + start + `","type":"user","text":"before the run"},` +
				`{"id":"msg_1d869a0e1001rKyh9iFaHwqLzD","type":"assistant","content":[]}]`
			if err := os.WriteFile(filepath.Join(dir, "export-messages"), []byte(messages), 0o600); err != nil {
				t.Fatal(err)
			}
			taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, Scrub: tc.scrub,
				MaxBytes: takeBound})
			if err != nil || taken.Body == nil {
				t.Fatalf("TakeSession = %+v, %v; want the session taken whatever its start", taken, err)
			}
			kept := string(gunzipped(t, keptAndOpened(t, taken)))
			if err := taken.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if got := taken.Archive.RunStart; got != (sandbox.RunStart{After: tc.want}) {
				t.Fatalf("the take keeps the run's start as %+v, want after %q", got, tc.want)
			}
			if tc.want != "" && strings.Count(kept, `"id":"`+tc.want+`"`) != 1 {
				t.Errorf("the kept session holds the message the start names %d times, want once:\n%s",
					strings.Count(kept, `"id":"`+tc.want+`"`), kept)
			}
		})
	}
}
