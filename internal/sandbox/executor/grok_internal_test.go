package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A GROK RUN'S STREAM READS AS THE RUN ENDED, off real 1.0.92 captures: the
// answer is the last request's words that called no tool; the spend is every
// request's counts, the prompt less its cached part beside the cache; a capped
// run is capped; a refusal is its error, a 402 and both 429s its limit too, one
// past the model's context its context exhausted alone; a failed call is no
// failed run; a run cancelled, which this runner's is only on a signal, idle on
// a subagent too, and one cut before its `end` failed, their spend counted.
//
// Mutations, each red: read the answer as every text line joined; count the
// cached part twice; take max_turn_requests for an answer; drop "Some resource
// has been exhausted" from grokLimitWords; read no overflow, or every failure as
// one; read a stream without `end` as a success; read a cancel as a success.
func TestAGrokStreamReadsAsItsRunEnded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                string
		success, capped, limited, exhausted bool
		text, why                           string
		turns, in, cache, out               int
	}{
		{name: "plain", success: true, text: "Plain answer.", turns: 1, in: 605 + 400, cache: 400, out: 50},
		{name: "notes", success: true, text: "Final answer.", turns: 5, in: 3055 + 2000, cache: 2000, out: 250},
		{name: "timeout", success: true, text: "Called.", turns: 2, in: 1235 + 800, cache: 800, out: 100},
		{name: "resumed", success: true, text: "Plain answer.", turns: 1, in: 637 + 400, cache: 400, out: 50},
		{name: "capped", capped: true, turns: 4, in: 2474 + 1600, cache: 1600, out: 200},
		{name: "billing", limited: true},
		{name: "ratelimit", limited: true},
		{name: "quota", limited: true},
		{name: "overflow", exhausted: true},
		{name: "cancelled", why: "signalled to stop", turns: 1, in: 699 + 400, cache: 400, out: 50},
		{name: "signalled", why: "signalled to stop", turns: 1, in: 655 + 400, cache: 400, out: 50},
		{name: "signalled-idle", why: "signalled to stop", turns: 3, in: 1819 + 1200, cache: 1200, out: 150},
		{name: "cut", why: "stopped before", turns: 1, in: 605 + 400, cache: 400, out: 50},
	} {
		var stream []byte
		if tc.name == "cut" { // plain's stream without its last line, the `end`
			lines := bytes.SplitAfter(grokFixture(t, "plain.stream.jsonl"), []byte("\n"))
			stream = bytes.Join(lines[:len(lines)-2], nil)
		} else {
			stream = grokFixture(t, tc.name+".stream.jsonl")
		}
		got := Grok{}.Parse(stream)
		if got.Success != tc.success || got.RoundCapped != tc.capped || (got.Limits != "") != tc.limited ||
			got.ContextExhausted != tc.exhausted {
			t.Errorf("%s: success %v, capped %v, limits %q, context exhausted %v (error %q)", tc.name, got.Success,
				got.RoundCapped, got.Limits, got.ContextExhausted, got.Error)
		}
		if tc.success && got.Text != tc.text {
			t.Errorf("%s: answer %q, want %q", tc.name, got.Text, tc.text)
		}
		if !tc.success && (got.Error == "" || !strings.Contains(got.Error, tc.why)) {
			t.Errorf("%s: failed with error %q, want one naming %q", tc.name, got.Error, tc.why)
		}
		if got.Turns != tc.turns || got.InputTokens != tc.in || got.CacheReadTokens != tc.cache ||
			got.OutputTokens != tc.out {
			t.Errorf("%s: turns %d, in %d, cache %d, out %d; want %d, %d, %d, %d", tc.name, got.Turns,
				got.InputTokens, got.CacheReadTokens, got.OutputTokens, tc.turns, tc.in, tc.cache, tc.out)
		}
	}
	notes := Grok{}.Parse(grokFixture(t, "notes.stream.jsonl"))
	for _, want := range []string{"crewlet_ab12cd__echo", "crewlet_ab12cd__fail", "read_file"} {
		if !strings.Contains(notes.Transcript, want) {
			t.Errorf("the transcript names no call of %s:\n%s", want, notes.Transcript)
		}
	}
}

// A GROK TAKE IS HELD ONCE ITS TOKEN ENDS A HOOK'S ANSWER ON THE RECORD AND A
// MODEL'S ANSWER FOLLOWS: the PostToolUse note and the Stop block of a real
// run each; a record that ends before the model answered holds none, and a
// token anywhere else (the model's own words) holds nothing.
//
// Mutation: hold a token without the answer after it, or read one out of any
// line.
func TestAGrokTakeIsHeldOnceAModelAnsweredAfterItsHook(t *testing.T) {
	t.Parallel()
	record := grokFixture(t, "notes.chat.jsonl")
	held, err := grokHeld(t.Context(), bytes.NewReader(record))
	if err != nil {
		t.Fatal(err)
	}
	for _, take := range []string{"0f0e0d0c-0000-4000-8000-000000000001", "0f0e0d0c-0000-4000-8000-000000000002"} {
		if !held[claudeTakeToken(take)] {
			t.Errorf("the take %s is not held: %v", take, held)
		}
	}
	lines := bytes.SplitAfter(record, []byte("\n"))
	stop := slices.IndexFunc(lines, func(l []byte) bool { return bytes.Contains(l, []byte("stop_hook_feedback")) })
	cut, err := grokHeld(t.Context(), bytes.NewReader(bytes.Join(lines[:stop+1], nil)))
	if err != nil || cut[claudeTakeToken("0f0e0d0c-0000-4000-8000-000000000002")] {
		t.Errorf("a finish's take is held before the model answered it: %v (%v)", cut, err)
	}
	forged, _ := json.Marshal(map[string]any{"type": "assistant",
		"content": "Done. " + claudeTakeToken("0f0e0d0c-0000-4000-8000-0000000000ff")})
	mine, _ := grokHeld(t.Context(), bytes.NewReader(append(append(forged, '\n'), forged...)))
	if len(mine) != 0 {
		t.Errorf("the model's own words hold %v", mine)
	}
}

// A GROK NOTE A COMPACTION LEFT ONLY IN ITS SUMMARY IS NOT HELD (ADR-0177): 1.0.92
// rewrites the record on an automatic compaction, the hook's line gone and its
// words, token and all, inside the summary's `compaction_meta` line, and the
// model answers after it; the note handed after the compaction, with a model's
// answer after its hook line, is held (a real run, its window made small).
//
// Mutation: read a token out of any user line Grok adds, the summary included.
func TestAGrokNoteLeftOnlyInACompactionSummaryIsNotHeld(t *testing.T) {
	t.Parallel()
	record := grokFixture(t, "compacted.chat.jsonl")
	summary, kept := claudeTakeToken("0f0e0d0c-0000-4000-8000-000000000001"),
		claudeTakeToken("0f0e0d0c-0000-4000-8000-000000000002")
	if !bytes.Contains(record, []byte("compaction_meta")) || !bytes.Contains(record, []byte(summary)) {
		t.Fatal("the capture holds no summary quoting the first note")
	}
	held, err := grokHeld(t.Context(), bytes.NewReader(record))
	if err != nil || held[summary] || !held[kept] || len(held) != 1 {
		t.Errorf("held %v (%v), want the note after the compaction alone", held, err)
	}
}

// A GROK ARCHIVE READS AS ITS SESSION'S TRANSCRIPT, off a real run's record: the
// brief, each call by the tool the model named through use_tool, each result,
// a note a hook handed over without its token, and the answer; Grok's own
// context and reminders not at all.
//
// Mutation: file a hook's answer as a brief, keep its token, or read
// use_tool's calls as use_tool's.
func TestAGrokArchiveReadsAsItsTranscript(t *testing.T) {
	t.Parallel()
	got := grokTranscriptOf(t, museArchive(t, map[string][]byte{
		"%2Ftmp%2Fbox/11111111-2222-4333-8444-0000000000a2/chat_history.jsonl": grokFixture(t, "notes.chat.jsonl"),
		"%2Ftmp%2Fbox/11111111-2222-4333-8444-0000000000a2/summary.json":       []byte(`{}`),
	}))
	kinds := map[EntryKind][]string{}
	for _, e := range got {
		kinds[e.Kind] = append(kinds[e.Kind], e.Tool+"|"+e.Text)
		if strings.Contains(e.Text, "[crewlet take") || strings.Contains(e.Text, "MCP server connected") {
			t.Errorf("entry %+v carries a token or Grok's own reminder", e)
		}
	}
	if b := kinds[EntryBrief]; len(b) != 1 || b[0] != "|Do the task." {
		t.Errorf("briefs %q, want the task", b)
	}
	if c := kinds[EntryToolCall]; len(c) != 4 || !strings.HasPrefix(c[0], "crewlet_ab12cd__echo|") {
		t.Errorf("calls %q, want the four the model made, the first the bridge's echo", c)
	}
	if n := kinds[EntryNote]; len(n) != 2 {
		t.Errorf("notes %q, want the batch's and the finish's", n)
	}
	if m := kinds[EntryModel]; len(m) == 0 || m[len(m)-1] != "|Final answer." {
		t.Errorf("the model's words %q, want the answer last", m)
	}
}

// A GROK RUN'S OWN TABLES HOLD WHAT DRIVES IT AND NOTHING THAT LIMITS IT: the
// key 1.0.92 would add, so no call rewrites them; each server under its name
// and the run's nonce, with the call wait, an HTTP server's headers and a
// command's arguments and environment, every value byte for byte as TOML reads
// it back; no other table, so every other vendor's files, the shell's
// environment and Grok's features stay at Grok's defaults; and a name Grok
// would skip refused.
//
// Mutations, each red: drop the marketplace key; write a compat cell, a shell
// policy or a feature; drop grokTOMLString's escaping; skip the headers table;
// write `env` for an HTTP server; drop `args`.
func TestAGrokRunsSettingsAreItsOwnWhole(t *testing.T) {
	t.Parallel()
	const header, value = "a\"b\\c\nd", "v\"1\t\\"
	config, err := grokConfig(map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:1/mcp", CallTimeout: 1500 * time.Millisecond,
			Headers: map[string]string{"Authorization": header}, Env: map[string]string{"UNUSED": "x"}},
		"local": {Command: "node", Args: []string{"server.mjs", "--name=\"q\""}, Env: map[string]string{"K": value}},
	}, "ab12cd", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[marketplace]\ndefault_skills_installs_purged = true\n",
		"[mcp_servers.crewlet_ab12cd]\nurl = \"http://127.0.0.1:1/mcp\"\ntool_timeout_sec = 2\n",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("the settings lack %q:\n%s", want, config)
		}
	}
	var whole map[string]any
	if _, err := toml.Decode(config, &whole); err != nil {
		t.Fatalf("the settings are no TOML: %v\n%s", err, config)
	}
	if got := keysOf(whole); got != "marketplace,mcp_servers" {
		t.Errorf("the settings hold %s, want the marketplace key and the servers alone:\n%s", got, config)
	}
	var read struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(config, &read); err != nil {
		t.Fatalf("the settings are no TOML: %v\n%s", err, config)
	}
	http, local := read.Servers["crewlet_ab12cd"], read.Servers["local_ab12cd"]
	if got := keysOf(http); got != "headers,tool_timeout_sec,url" {
		t.Errorf("the HTTP server holds %s, want headers, tool_timeout_sec and url", got)
	}
	if got := keysOf(local); got != "args,command,env" {
		t.Errorf("the command's server holds %s, want args, command and env", got)
	}
	headers, _ := http["headers"].(map[string]any)
	env, _ := local["env"].(map[string]any)
	if headers["Authorization"] != header || env["K"] != value || local["command"] != "node" ||
		fmt.Sprint(local["args"]) != `[server.mjs --name="q"]` {
		t.Errorf("the values read back as headers %q, env %q, command %v, args %v", headers, env, local["command"],
			local["args"])
	}
	for _, name := range []string{"9lives", "my__server", "a.b", ""} {
		if _, err := grokConfig(map[string]sandbox.MCPServer{name: {Transport: sandbox.TransportHTTP}}, "ab12cd",
			""); err == nil {
			t.Errorf("a server named %q was written", name)
		}
	}
}

// A GROK RUN'S SETTINGS ARE THE BOX'S WITH ITS OWN LAID IN: every key the box
// set stays, its servers, compatibility cells, tool settings and hooks among
// them; the run's servers join the box's; the run's hook is first in its event,
// the box's after it under any spelling of that event, so the receipts' places
// hold; the box's own marketplace value stands; and settings that do not read
// as TOML refuse the run, naming the file.
//
// Mutations, each red: write the run's tables alone; append the run's hooks
// after the box's; keep a second spelling of an event apart; set the
// marketplace key over the box's.
func TestAGrokRunsSettingsAreTheBoxsWithItsOwnLaidIn(t *testing.T) {
	t.Parallel()
	const boxOwn = `model = "grok-4.6"

[compat.claude]
mcps = false

[toolset.bash]
login_shell_capture = true

[marketplace]
default_skills_installs_purged = false

[mcp_servers.docs]
url = "https://docs.example.com/mcp"

[[hooks.post_tool_use]]
hooks = [{ type = "command", command = "box-snake" }]

[[hooks.PostToolUse]]
hooks = [{ type = "command", command = "box-pascal" }]

[[hooks.SessionStart]]
hooks = [{ type = "command", command = "box-start" }]
`
	paths := Paths{Home: "/home/box"}
	own, err := grokConfig(map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
		URL: "http://127.0.0.1:1/mcp"}}, "ab12cd", grokHooks(paths, true, false, nil))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := grokMergeConfig([]byte(boxOwn), own)
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		Model   string
		Compat  map[string]map[string]bool
		Toolset map[string]map[string]bool
		Market  map[string]bool           `toml:"marketplace"`
		Servers map[string]map[string]any `toml:"mcp_servers"`
		Hooks   map[string][]struct{ Hooks []struct{ Command string } }
	}
	if _, err := toml.Decode(string(merged), &read); err != nil {
		t.Fatalf("the settings are no TOML: %v\n%s", err, merged)
	}
	if read.Model != "grok-4.6" || read.Compat["claude"]["mcps"] || !read.Toolset["bash"]["login_shell_capture"] ||
		read.Market["default_skills_installs_purged"] || read.Servers["docs"]["url"] != "https://docs.example.com/mcp" ||
		read.Servers["crewlet_ab12cd"]["url"] != "http://127.0.0.1:1/mcp" {
		t.Errorf("the box's keys or the run's server did not stand:\n%s", merged)
	}
	commands := map[string][]string{}
	for event, entries := range read.Hooks {
		for _, e := range entries {
			for _, h := range e.Hooks {
				commands[event] = append(commands[event], h.Command)
			}
		}
	}
	post := commands["PostToolUse"]
	if len(post) != 3 || !strings.Contains(post[0], "grok-hook") || !slices.Contains(post, "box-snake") ||
		!slices.Contains(post, "box-pascal") || len(commands["post_tool_use"]) != 0 ||
		!slices.Equal(commands["SessionStart"], []string{"box-start"}) || len(commands["Stop"]) != 1 {
		t.Errorf("the hooks are %v, want the run's first in PostToolUse, the box's after it and its own kept", commands)
	}
	if _, err := grokMergeConfig([]byte("[mcp_servers\n"), own); err == nil ||
		!strings.Contains(err.Error(), "~/.grok/config.toml") {
		t.Errorf("settings that are no TOML were merged: %v", err)
	}
}

// A GROK SESSION'S DIRECTORY IS ITS RUN'S DIRECTORY'S NAME AS 1.0.92 WRITES IT:
// every byte but a letter, a digit, `-`, `_`, `.` and `~` as `%XX` (measured
// on a home of every such byte).
//
// Mutation: keep `/`, or write a byte's hex in lowercase.
func TestAGrokSessionsKeyIsItsDirectoryAsGrokNamesIt(t *testing.T) {
	t.Parallel()
	const measured = `%2Ftmp%2Fh%20A-b_c.d~e%40f%2Bg%25h%3Ai%2Cj%3Dk%28l%29%21m`
	if got := grokPathKey("/tmp/h A-b_c.d~e@f+g%h:i,j=k(l)!m"); got != measured {
		t.Errorf("key %q, want %q", got, measured)
	}
}

// A BOX RUNS GROK BUILD 1.0.92 OR NEWER, AND A RELEASE IT CANNOT READ IS OLDER:
// its pre-release, an older release, no version at all; a newer one runs.
// A session continues only where box and writer both run 1.0.92, the copy a
// box holds included ([inPlaceGate]).
//
// Mutations, each red: take an unreadable release for a current one, or a
// pre-release of 1.0.92 for the release; continue in place on any release.
func TestABoxRunsGrok1092OrNewer(t *testing.T) {
	t.Parallel()
	gate := grokRelease + `if [ -z "$n" ] || ! at ` + floorArgs(grokSupported()) + `; then exit 1; fi`
	for version, runs := range map[string]bool{
		"grok 1.0.92 (b06756fd6256)": true, "grok 1.0.93 (92384ee02e2d)": true, "grok 2.0.0": true,
		"grok 1.0.92-rc1 (x)": false, "grok 1.0.91 (x)": false, "grok 0.9.99": false, "": false, "grok dev": false,
	} {
		cmd := exec.Command("sh", "-c", "grok() { printf '%s\\n' \"$V\"; }\n"+gate) //nolint:gosec // the test's own script
		cmd.Env = append(os.Environ(), "V="+version)
		if err := cmd.Run(); (err == nil) != runs {
			t.Errorf("%q: ran %v, want %v", version, err == nil, runs)
		}
	}
	for box, ok := range map[[2]string]bool{{"1.0.92", "1.0.92"}: true, {"1.0.93", "1.0.92"}: false,
		{"1.0.92", "1.0.93"}: false, {"1.0.92", ""}: false, {"", "1.0.92"}: false} {
		if got, why := (Grok{}).Continues(box[0], box[1]); got != ok || ok != (why == "") {
			t.Errorf("Continues(%q, %q) = %v, %q; want %v", box[0], box[1], got, why, ok)
		}
	}
	if ok, _ := (Grok{}).continuesInPlace("1.0.93"); ok {
		t.Error("a box on 1.0.93 continues the copy it holds, which its own gate would start afresh")
	}
}

// A CONTINUED GROK SESSION IS HANDED THIS LAUNCH'S STANDING INSTRUCTIONS: 1.0.92
// resumes on its record's first line, so the script replaces what that line
// carries from the runner's bounds on, leaving every other line as it was and
// the line one JSON object holding the bounds once; a real record a first run
// wrote, and one a continued run already renewed.
//
// Mutations, each red: write the renewal unescaped; replace from the record's
// start; append the renewal after the old one.
func TestAContinuedGrokSessionIsHandedThisLaunchsInstructions(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"plain.chat.jsonl", "resumed.chat.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			paths := Paths{Home: t.TempDir()}
			const session = "11111111-2222-4333-8444-0000000000a1"
			record := filepath.Join(paths.GrokSessionsKey(), session, "chat_history.jsonl")
			writeHere(t, record)
			original := grokFixture(t, fixture)
			if err := os.WriteFile(record, original, 0o600); err != nil {
				t.Fatal(err)
			}
			rules, _ := grokRulesJSON("RENEWED \"quoted\" text\nwith a line")
			writeHere(t, paths.GrokRules())
			if err := os.WriteFile(paths.GrokRules(), rules, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", "w= s="+session+"\n"+grokRenewRules(paths)+`printf '%s' "$w"`) //nolint:gosec // the test's own script
			if out, err := cmd.CombinedOutput(); err != nil || len(out) > 0 {
				t.Fatalf("the renewal: %v, said %q", err, out)
			}
			renewed, _ := os.ReadFile(record)
			oldLines, newLines := bytes.SplitN(original, []byte("\n"), 2), bytes.SplitN(renewed, []byte("\n"), 2)
			if !bytes.Equal(oldLines[1], newLines[1]) {
				t.Error("the renewal changed a line past the first")
			}
			var first struct{ Type, Content string }
			if err := json.Unmarshal(newLines[0], &first); err != nil || first.Type != "system" {
				t.Fatalf("the first line is no system message: %v", err)
			}
			if !strings.HasSuffix(first.Content, grokRulesOpen+"\nRENEWED \"quoted\" text\nwith a line\n"+grokRulesClose) ||
				strings.Contains(first.Content, "MARK-SYSTEM") || !strings.HasPrefix(first.Content, "You are Grok") ||
				strings.Count(first.Content, grokRulesOpen) != 1 || strings.Count(first.Content, grokRulesClose) != 1 {
				t.Errorf("the first line carries %q", first.Content[max(0, len(first.Content)-200):])
			}
		})
	}
}

// A GROK RUN'S OWN ARCHIVE READS AS ITS RECORD ALONE: a real 1.0.92 pack, its
// record first beside the session's other files, reads as the record does by
// itself (the brief, then the answer); every other member is passed unread.
//
// Mutation: read every member as a record.
func TestAGrokRunsOwnArchiveReadsAsItsRecordAlone(t *testing.T) {
	t.Parallel()
	archive := grokFixture(t, "plain.session.tgz")
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	head, err := tar.NewReader(zr).Next()
	if err != nil || !strings.HasSuffix(head.Name, "/chat_history.jsonl") {
		t.Fatalf("the archive's first member is %v (%v), want the record", head, err)
	}
	got := grokTranscriptOf(t, archive)
	alone := grokTranscriptOf(t, museArchive(t, map[string][]byte{head.Name: grokFixture(t, "plain.chat.jsonl")}))
	if !slices.EqualFunc(got, alone, func(a, b TranscriptEntry) bool { return a.Kind == b.Kind && a.Text == b.Text }) {
		t.Errorf("the archive reads as %+v, its record alone as %+v", got, alone)
	}
	if len(got) < 2 || got[0].Kind != EntryBrief || got[0].Text != "Do the task." ||
		got[len(got)-1].Kind != EntryModel || got[len(got)-1].Text != "Plain answer." {
		t.Errorf("the transcript is %+v, want the brief first and the answer last", got)
	}
}

// A CONTINUED GROK SESSION THE RUN CANNOT HAND THIS LAUNCH'S INSTRUCTIONS IS NOT
// CONTINUED: a record whose first line holds none of the runner's bounds (a
// real 1.0.92 session started with `--rules`), or a launch whose renewal is not
// there to write, starts the minted session instead, says why on the run's
// error stream, and leaves the record as it was.
//
// Mutations, each red: resume where the first line holds no bounds; resume
// where the renewal could not be written; drop the reason.
func TestAGrokSessionItCannotRenewStartsASessionOfItsOwn(t *testing.T) {
	t.Parallel()
	const session, fresh = "11111111-2222-4333-8444-00000000000a", "11111111-2222-4333-8444-0000000000b9"
	for name, tc := range map[string]struct {
		record string
		rules  bool
		why    string
	}{
		"a record without the runner's bounds": {"rules.chat.jsonl", true, grokRulesMissing},
		"no renewal to write":                  {"plain.chat.jsonl", false, grokRulesUnwritten},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := Paths{Home: t.TempDir()}
			record := filepath.Join(paths.GrokSessionsKey(), session, "chat_history.jsonl")
			writeHere(t, record)
			original := grokFixture(t, tc.record)
			if err := os.WriteFile(record, original, 0o600); err != nil {
				t.Fatal(err)
			}
			writeHere(t, paths.WorkDir()+"/x")
			if tc.rules {
				rules, _ := grokRulesJSON("RENEWED")
				if err := os.WriteFile(paths.GrokRules(), rules, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			gate := grokBoxVersion + grokSessionGate(Session{ID: session, Continues: true, Fresh: fresh},
				paths, false) + `printf '%s|%s|%s|%s' "$s" "$sf" "$k" "$w"`
			var stderr bytes.Buffer
			cmd := exec.Command("sh", "-c", gate) //nolint:gosec // the test's own script
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if want := fresh + "|-s|no|" + tc.why; err != nil || string(out) != want {
				t.Fatalf("the gate answered %q (%v), want %q", out, err, want)
			}
			if !strings.Contains(stderr.String(), "starts a new session, "+fresh+", instead: "+tc.why) {
				t.Errorf("the run said %q, want why it starts a session of its own", stderr.String())
			}
			if after, _ := os.ReadFile(record); !bytes.Equal(after, original) {
				t.Error("the record was rewritten")
			}
		})
	}
}

// A GROK SESSION PACKED IN ONE BOX IS RESTORED UNDER ANOTHER BOX'S KEY, ITS
// LOCKS AND LINKS LEFT BEHIND: the pack takes the session's directory alone,
// record first, its `terminal/` call logs with it (1.0.92 writes them there,
// and a resume from such an archive continues, measured); the gate restores
// it under the continuing box's home and continues it, recording its digest.
//
// Mutations, each red: pack the key's directory; pack the top level alone;
// pack a lock or a link; restore under the writer's key.
func TestAGrokSessionCarriesToAnotherBoxsHome(t *testing.T) {
	t.Parallel()
	const session = "11111111-2222-4333-8444-0000000000a1"
	from, to := Paths{Home: t.TempDir() + "/a"}, Paths{Home: t.TempDir() + "/b"}
	dir := filepath.Join(from.GrokSessionsKey(), session)
	for name, body := range map[string][]byte{"chat_history.jsonl": grokFixture(t, "plain.chat.jsonl"),
		"summary.json": []byte("{}"), "chat_history.jsonl.lock": nil, "terminal/call_23.log": []byte("shell-ran\n")} {
		writeHere(t, filepath.Join(dir, name))
		_ = os.WriteFile(filepath.Join(dir, name), body, 0o600)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "terminal", "linked.log")); err != nil {
		t.Fatal(err)
	}
	writeHere(t, from.WorkDir()+"/x")
	pack := exec.Command("sh", "-c", Grok{}.Pack(from, session, from.WorkDir()+"/staged", from.WorkDir()+"/held", false)) //nolint:gosec // the test's own script
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("the pack: %v %s", err, out)
	}
	list, err := exec.Command("tar", "-tzf", from.WorkDir()+"/held").Output()
	member := grokPathKey(from.Home) + "/" + session
	if err != nil || !strings.HasPrefix(string(list), member+"/chat_history.jsonl\n") ||
		!strings.Contains(string(list), member+"/terminal/call_23.log\n") ||
		!strings.Contains(string(list), member+"/summary.json\n") ||
		strings.Contains(string(list), ".lock") || strings.Contains(string(list), "linked") {
		t.Fatalf("the archive lists %q (%v), want the record first, every file under it, no lock or link", list, err)
	}
	writeHere(t, to.SessionCarried())
	archive, _ := os.ReadFile(from.WorkDir() + "/held")
	_ = os.WriteFile(to.SessionCarried(), archive, 0o600)
	rules, _ := grokRulesJSON("RENEWED")
	_ = os.WriteFile(to.GrokRules(), rules, 0o600)
	gate := grokBoxVersion + grokSessionGate(Session{ID: session, Continues: true,
		Fresh: "11111111-2222-4333-8444-0000000000a9"}, to, false) + `printf '%s %s %s %s' "$s" "$sf" "$k" "$w"`
	out, err := exec.Command("sh", "-c", gate).Output() //nolint:gosec // the test's own script
	if err != nil || string(out) != session+" -r yes " {
		t.Fatalf("the gate answered %q (%v), want the session continued", out, err)
	}
	for _, name := range []string{"chat_history.jsonl", "terminal/call_23.log"} {
		if _, err := os.Stat(filepath.Join(to.GrokSessionsKey(), session, name)); err != nil {
			t.Errorf("the session's %s is not under the continuing box's key: %v", name, err)
		}
	}
}

// ONE GROK HOOK ANSWER CARRIES ITS NOTES AND TOKEN WITHIN THE 10,000
// CHARACTERS 1.0.92 HANDS ITS MODEL, the token whole at its end; a note that
// fills the budget goes alone.
//
// Mutation: drop the token from the budget.
func TestAGrokHookAnswerFitsWhatGrokHandsOn(t *testing.T) {
	t.Parallel()
	wide := strings.Repeat("é", grokContextBudget)
	if n := (Grok{}).notesThatFit([]string{wide, "a second"}); n != 1 {
		t.Errorf("%d notes fit beside one that fills the budget, want 1", n)
	}
	answer := Grok{}.hookAnswer(claudeEventToolBatch, "0f0e0d0c-0000-4000-8000-000000000001",
		[]HookNote{{Message: wide}})
	var got struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(answer), &got); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got.Output.Context)); n > grokContextChars ||
		grokTokenOf(got.Output.Context) != claudeTakeToken("0f0e0d0c-0000-4000-8000-000000000001") {
		t.Errorf("the context is %d characters, its token %q", n, grokTokenOf(got.Output.Context))
	}
}

// keysOf is m's keys, sorted and joined by commas.
func keysOf(m map[string]any) string {
	return strings.Join(slices.Sorted(maps.Keys(m)), ",")
}

// grokBoxVersion is a box's `grok --version` on 1.0.92 as a run's script holds
// it, for a gate case to run on.
const grokBoxVersion = "v='grok 1.0.92 (b06756fd6256)'\n"

// writeHere makes the file path and its parents.
func writeHere(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}
