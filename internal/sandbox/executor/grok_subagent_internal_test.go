package executor

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// grokSubagentRun is a real Grok Build 1.0.92 run whose main agent called
// spawn_subagent (testdata/grok/1.0.92/subagent-<name>.*): in the foreground,
// the child's lines inside the main agent's call; in the background, the child
// ending 20 s after the main agent's last words, then Grok's extra turn on its
// notice.
type grokSubagentRun struct {
	name, parent, child, answer string
	turns, in, cache, out       int
}

var grokSubagentRuns = []grokSubagentRun{
	{name: "fg", parent: "01a20584-739c-7ce3-bf2e-62e05be3785c", child: "01a20584-74bd-7af1-baf4-429bd365c3a7",
		answer: "Main done.", turns: 4, in: 2429 + 1600, cache: 1600, out: 200},
	{name: "bg", parent: "01a205c8-9a69-72d0-ab41-3b2aaea98fe5", child: "01a205c8-9b2b-79e3-9a56-a4594d1d97c3",
		answer: "Answer after the notice.", turns: 5, in: 3041 + 2000, cache: 2000, out: 250},
}

// A GROK RUN'S ANSWER IS ITS MAIN AGENT'S LAST WORDS, AND ITS SPEND AND TURNS
// ARE EVERY REQUEST'S, ITS SUBAGENTS' INCLUDED: a background subagent that ends
// after the main agent's last words is answered by Grok's extra turn on its
// notice, whose words are the answer, never the child's; a foreground one's
// words sit inside the main agent's call, which speaks after; and every
// `usage` line, the child's untagged among them, is charged and counted a
// turn, as Turns is the provider calls the launch's spend covers.
//
// Mutations, each red: count Turns off `end`'s num_turns or the main agent's
// requests alone; charge `end`'s totals; take the first answer, or the
// child's, for the run's.
func TestAGrokRunsAnswerIsItsMainAgentsLastWordsAndItsSpendEveryRequests(t *testing.T) {
	t.Parallel()
	for _, run := range grokSubagentRuns {
		got := Grok{}.Parse(grokFixture(t, "subagent-"+run.name+".stream.jsonl"))
		if !got.Success || got.Text != run.answer {
			t.Errorf("%s: success %v, answer %q (error %q), want %q", run.name, got.Success, got.Text, got.Error,
				run.answer)
		}
		if got.Turns != run.turns || got.InputTokens != run.in || got.CacheReadTokens != run.cache ||
			got.OutputTokens != run.out {
			t.Errorf("%s: turns %d, in %d, cache %d, out %d; want %d, %d, %d, %d", run.name, got.Turns,
				got.InputTokens, got.CacheReadTokens, got.OutputTokens, run.turns, run.in, run.cache, run.out)
		}
	}
}

// grokScriptStream is a run's stream as its script leaves it: the harness's
// lines, then what the script adds once the harness exits ([grokMainRecord])
// off a home whose session holds record.
func grokScriptStream(t *testing.T, stream []byte, session string, record []byte) []byte {
	t.Helper()
	paths := Paths{Home: t.TempDir() + "/home"}
	file := filepath.Join(paths.GrokSessionsKey(), session, "chat_history.jsonl")
	writeHere(t, file)
	if err := os.WriteFile(file, record, 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := exec.Command("sh", "-c", "s="+session+"\n"+grokMainRecord(paths)).Output() //nolint:gosec // the test's own script
	if err != nil {
		t.Fatalf("the script's record: %v", err)
	}
	return append(append([]byte(nil), stream...), added...)
}

// without is raw with every line equal to drop left out, and the last
// occurrence of quiet, a request's words in its record, emptied.
func without(raw []byte, drop, quiet string) []byte {
	var kept []string
	for _, l := range strings.SplitAfter(string(raw), "\n") {
		if strings.TrimSpace(l) != drop {
			kept = append(kept, l)
		}
	}
	out := strings.Join(kept, "")
	if i := strings.LastIndex(out, quiet); quiet != "" && i >= 0 {
		out = out[:i] + `"content":""` + out[i+len(quiet):]
	}
	return []byte(out)
}

// A GROK RUN'S ANSWER IS ITS MAIN AGENT'S, READ OFF ITS MAIN SESSION'S RECORD,
// NEVER A SUBAGENT'S WORDS: the stream's lines carry no agent, so where the
// main agent's last request says nothing, a child's words would stand last
// there. Off the record the script adds, a background child's run whose extra
// turn is silent answers with the main agent's words before it, Grok's notice
// never read as a brief; a foreground one whose last request is silent has no
// answer; and a continued session's earlier answer is never this run's.
//
// Mutations, each red: read the stream's words where the record was added;
// read the notice as a brief; start no answer over at a brief; leave the
// prompts out of the script's record, or add no record.
func TestAGrokRunsAnswerIsItsMainSessionsNeverASubagents(t *testing.T) {
	t.Parallel()
	fg, bg := grokSubagentRuns[0], grokSubagentRuns[1]
	for _, tc := range []struct {
		name, session, fixture, drop, quiet, answer string
	}{
		{"fg", fg.parent, "subagent-fg", "", "", "Main done."},
		{"bg", bg.parent, "subagent-bg", "", "", "Answer after the notice."},
		{"bg, its extra turn silent", bg.parent, "subagent-bg", `{"type":"text","data":"Answer after the notice."}`,
			`"content":"Answer after the notice."`, "Main done."},
		{"fg, its last request silent", fg.parent, "subagent-fg", `{"type":"text","data":"Main done."}`,
			`"content":"Main done."`, ""},
		{"continued, this run silent", "11111111-2222-4333-8444-0000000000c1", "resumed",
			`{"type":"text","data":"Plain answer."}`, `"content":"Plain answer."`, ""},
	} {
		stream := without(grokFixture(t, tc.fixture+".stream.jsonl"), tc.drop, "")
		record := without(grokFixture(t, tc.fixture+".chat.jsonl"), "", tc.quiet)
		got := Grok{}.Parse(grokScriptStream(t, stream, tc.session, record))
		if got.Text != tc.answer || got.Success != (tc.answer != "") {
			t.Errorf("%s: answer %q, success %v (error %q); want %q", tc.name, got.Text, got.Success, got.Error,
				tc.answer)
		}
		if tc.answer == "" && got.Error != "the harness produced no answer" {
			t.Errorf("%s: error %q, want no answer", tc.name, got.Error)
		}
	}
}

// grokChildElsewhere is the key of a directory below the box home a subagent
// can be started in (spawn_subagent's `cwd`), whose sessions Grok files under
// that directory's key, not the home's.
const grokChildElsewhere = "%2Fhome%2Fbox%2Fworkspace%2Frepo"

// grokSubagentHome lays run's sessions out under a box home's sessions as
// 1.0.92 left them, the child's under childKey ("" for the home's key), beside
// a session of the box's that the run's record names nowhere and a lock; and
// returns its paths.
func grokSubagentHome(t *testing.T, run grokSubagentRun, childKey string) Paths {
	t.Helper()
	paths := Paths{Home: t.TempDir() + "/home"}
	key := paths.GrokSessionsKey()
	child := key
	if childKey != "" {
		child = filepath.Join(paths.GrokSessions(), childKey)
	}
	for name, body := range map[string][]byte{
		key + "/" + run.parent + "/chat_history.jsonl":                     grokFixture(t, "subagent-"+run.name+".chat.jsonl"),
		key + "/" + run.parent + "/updates.jsonl":                          grokFixture(t, "subagent-"+run.name+".updates.jsonl"),
		child + "/" + run.child + "/chat_history.jsonl":                    grokFixture(t, "subagent-"+run.name+".child.chat.jsonl"),
		child + "/" + run.child + "/chat_history.jsonl.lock":               nil,
		child + "/" + run.child + "/terminal/call_7.log":                   []byte("shell-ran\n"),
		child + "/11111111-2222-4333-8444-0000000000ff/chat_history.jsonl": grokFixture(t, "plain.chat.jsonl"),
	} {
		writeHere(t, name)
		if err := os.WriteFile(name, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHere(t, paths.WorkDir()+"/x")
	return paths
}

// grokPackOf is run's session packed by the runner's own pack in paths.
func grokPackOf(t *testing.T, paths Paths, run grokSubagentRun) []byte {
	t.Helper()
	pack := exec.Command("sh", "-c", Grok{}.Pack(paths, run.parent, paths.WorkDir()+"/staged", //nolint:gosec // the test's own script
		paths.WorkDir()+"/held", false))
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("%s: the pack: %v %s", run.name, err, out)
	}
	archive, err := os.ReadFile(paths.WorkDir() + "/held")
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

// A GROK SUBAGENT IS A THREAD OF ITS OWN IN ITS RUN'S RECORD: the pack takes the
// main session's record first, then each subagent session its updates name,
// from the home's key or from the key of the directory it was started in, and
// no other session of the box nor any lock; the main thread's brief is the
// run's alone, Grok's notice that a background subagent completed never one;
// its last words are the run's answer; and the child's brief, calls and words
// are on a thread named by its session.
//
// Mutations, each red: pack the main session alone; pack every session under
// the key; look for a child under the home's key alone; read a prompt with a
// synthetic_reason as a brief; read every record as the main thread.
func TestAGrokSubagentIsAThreadOfItsOwnInItsRunsRecord(t *testing.T) {
	t.Parallel()
	for _, run := range grokSubagentRuns {
		for _, childKey := range []string{"", grokChildElsewhere} {
			paths := grokSubagentHome(t, run, childKey)
			archive := grokPackOf(t, paths, run)
			list, err := exec.Command("tar", "-tzf", paths.WorkDir()+"/held").Output()
			members := strings.Fields(string(list))
			home := grokPathKey(paths.Home)
			child := cmp.Or(childKey, home) + "/" + run.child
			if err != nil || len(members) == 0 || members[0] != home+"/"+run.parent+"/chat_history.jsonl" ||
				!slices.Contains(members, child+"/chat_history.jsonl") ||
				!slices.Contains(members, child+"/terminal/call_7.log") ||
				strings.Contains(string(list), "0000000000ff") || strings.Contains(string(list), ".lock") {
				t.Fatalf("%s, child under %q: the archive lists %q (%v), want the main record first, the "+
					"child's session, nothing else of the box's and no lock", run.name, childKey, members, err)
			}
			main := map[EntryKind][]string{}
			kid := map[EntryKind][]string{}
			for _, e := range grokTranscriptOf(t, archive) {
				switch e.Thread {
				case "":
					main[e.Kind] = append(main[e.Kind], e.Text)
				case run.child:
					kid[e.Kind] = append(kid[e.Kind], e.Text)
				default:
					t.Errorf("%s: an entry on thread %q, a session the run never spawned", run.name, e.Thread)
				}
			}
			if b := main[EntryBrief]; !slices.Equal(b, []string{"Do the task."}) {
				t.Errorf("%s: the main thread's briefs are %q, want the run's alone", run.name, b)
			}
			if m := main[EntryModel]; len(m) == 0 || m[len(m)-1] != run.answer {
				t.Errorf("%s: the main thread's words are %q, want %q last", run.name, m, run.answer)
			}
			if b := kid[EntryBrief]; len(b) != 1 || !strings.HasPrefix(b[0], "M8SUB:") {
				t.Errorf("%s: the child's briefs are %q, want the main agent's task for it", run.name, b)
			}
			if m := kid[EntryModel]; !slices.Equal(m, []string{"SUB-RAN"}) || len(kid[EntryToolCall]) != 1 {
				t.Errorf("%s: the child's words %q and calls %q, want its answer and its one call", run.name, m,
					kid[EntryToolCall])
			}
		}
	}
}

// A GROK SUBAGENT'S SESSION CARRIES WITH ITS RUN'S: a pack made in one box,
// restored by the gate under another box's key, puts the child's session
// beside the main one there where it was filed beside it, and under the key
// of the directory it was started in otherwise.
//
// Mutations, each red: restore the main session alone; restore every child
// under the box's key; restore under the writer's key.
func TestAGrokSubagentsSessionCarriesWithItsRuns(t *testing.T) {
	t.Parallel()
	run := grokSubagentRuns[1]
	for _, childKey := range []string{"", grokChildElsewhere} {
		from := grokSubagentHome(t, run, childKey)
		archive := grokPackOf(t, from, run)
		to := Paths{Home: t.TempDir() + "/b"}
		writeHere(t, to.SessionCarried())
		if err := os.WriteFile(to.SessionCarried(), archive, 0o600); err != nil {
			t.Fatal(err)
		}
		rules, _ := grokRulesJSON("RENEWED")
		_ = os.WriteFile(to.GrokRules(), rules, 0o600)
		// A BARE RUN'S RECORD HOLDS NO BOUNDS, so the session is restored and
		// then not continued ([grokRenewRules]): the restore is what this holds.
		gate := grokBoxVersion + grokSessionGate(Session{ID: run.parent, Continues: true,
			Fresh: "11111111-2222-4333-8444-0000000000a9"}, to, false)
		if _, err := exec.Command("sh", "-c", gate).Output(); err != nil { //nolint:gosec // the test's own script
			t.Fatalf("the gate: %v", err)
		}
		child := to.GrokSessionsKey()
		if childKey != "" {
			child = filepath.Join(to.GrokSessions(), childKey)
		}
		for _, name := range []string{filepath.Join(to.GrokSessionsKey(), run.parent, "chat_history.jsonl"),
			filepath.Join(child, run.child, "chat_history.jsonl"), filepath.Join(child, run.child, "terminal/call_7.log")} {
			if _, err := os.Stat(name); err != nil {
				t.Errorf("child under %q: %s is not where the continuing box files it: %v", childKey, name, err)
			}
		}
	}
}
