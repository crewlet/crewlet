package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A KIMI SESSION IS CONTINUED ONLY FROM 2.7.3 TO 2.7.3, the release Crewlet
// supports: an older or newer box or writer, or one whose release cannot be
// read, starts fresh, saying so and naming 2.7.3.
//
// Mutation: admit a newer box, or a release that cannot be read — a row goes
// red.
func TestAKimiSessionContinuesOnlyOnTheSupportedRelease(t *testing.T) {
	for _, tc := range []struct {
		box, writer string
		continues   bool
	}{
		{"2.7.3", "2.7.3", true}, {"2.7.4", "2.7.3", false}, {"2.7.3", "2.7.4", false}, {"2.7.3", "2.7.2", false},
		{"2.7.2", "2.7.2", false}, {"", "2.7.3", false}, {"2.7.3", "", false},
	} {
		got, why := (Kimi{}).Continues(tc.box, tc.writer)
		if got != tc.continues || (!got && !strings.Contains(why, "2.7.3, the release Crewlet supports")) {
			t.Errorf("box %q, writer %q: continues %v (%q), want %v", tc.box, tc.writer, got, why, tc.continues)
		}
	}
}

// A KIMI SESSION'S START NAMES THE SESSION IT CONTINUES, and a carried archive
// that replaces the box's copy says so; a fresh run names none.
//
// Mutation: name an id on a fresh run, or drop Replaces — the case fails.
func TestAKimiResumeNamesTheSessionItContinues(t *testing.T) {
	got := (Kimi{}).Session(sandbox.RunRequest{Resume: kimiFixtureSession,
		Carried: &sandbox.CarriedSession{Replace: true}})
	if got != (Session{ID: kimiFixtureSession, Continues: true, Replaces: true}) {
		t.Fatalf("session = %+v", got)
	}
	if got := (Kimi{}).Session(sandbox.RunRequest{}); got != (Session{}) {
		t.Fatalf("a fresh run's session = %+v, want none", got)
	}
}

// A KIMI SESSION'S TRANSCRIPT FILES EACH LINE BY WHAT IT IS: the brief, a note
// under its heading, the model's words, a call and its result, a compaction's
// summary; never Kimi's own injected reminders.
//
// Mutation: read every user message (the reminders become briefs), or drop
// the summary — the case fails.
func TestAKimiTranscriptFilesEachLineByWhatItIs(t *testing.T) {
	steered := kimiTranscriptOf(t, kimiArchive(t, map[string][]byte{
		kimiMainMember: kimiFixture(t, kimiSupported, "steered.wire.jsonl")}))
	kinds := map[EntryKind]string{}
	briefs := 0
	for _, e := range steered {
		kinds[e.Kind] = e.Text
		if e.Kind == EntryBrief {
			briefs++
		}
	}
	if briefs != 1 || kinds[EntryBrief] != "Do the task." || !strings.Contains(kinds[EntryNote], "also check X") ||
		kinds[EntryModel] != "answer-2" || !strings.Contains(kinds[EntryToolResult], "done:") ||
		!strings.Contains(kinds[EntryToolCall], "5000") {
		t.Fatalf("entries = %+v", steered)
	}
	compacted := kimiTranscriptOf(t, kimiArchive(t, map[string][]byte{
		strings.Replace(kimiMainMember, kimiFixtureSession, "session_x", 1): kimiFixture(t, kimiSupported, "compacted.wire.jsonl")}))
	if !slices.ContainsFunc(compacted, func(e TranscriptEntry) bool { return e.Kind == EntrySummary }) {
		t.Fatalf("a compacted session reads no summary: %+v", compacted)
	}
}

// AN ARCHIVE'S RECORD IS AN AGENT'S WIRE.JSONL UNDER ITS SESSION, and nothing
// else is read as one.
//
// Mutation: take a member at any depth — a row goes red.
func TestAKimiArchivesRecordIsAnAgentsWireLog(t *testing.T) {
	for name, want := range map[string]string{
		"wd_a_0/session_x/agents/main/wire.jsonl":    "main",
		"wd_a_0/session_x/agents/agent-0/wire.jsonl": "agent-0",
		"wd_a_0/session_x/logs/kimi-code.log":        "",
		"session_x/agents/main/wire.jsonl":           "",
		"wd_a_0/other/agents/main/wire.jsonl":        "",
		"wd_a_0/session_x/agents/main/wire.jsonl/x":  "",
		"a/wd_a_0/session_x/agents/main/wire.jsonl":  "",
	} {
		if got, ok := kimiWireMember(name); got != want || ok != (want != "") {
			t.Errorf("%s: agent %q (%v), want %q", name, got, ok, want)
		}
	}
}

// kimiBox is a box home on the test's disk with the run's driver and settings
// written, and the directory a run works in.
func kimiBox(t *testing.T) (Paths, string) {
	t.Helper()
	paths := Paths{Home: t.TempDir()}
	work := filepath.Join(paths.Home, "work")
	for name, body := range map[string]string{paths.KimiDriver(): kimiDriverSource, paths.KimiSettings(): `{}`} {
		kimiWrite(t, name, body)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	return paths, work
}

func kimiWrite(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// kimiSessionFiles are a session's files as Kimi files them, its state.json
// naming dir as its cwd.
func kimiSessionFiles(session, dir string) map[string]string {
	return map[string]string{
		"state.json":             `{"id":"` + session + `","version":2,"cwd":"` + dir + `"}`,
		"agents/main/wire.jsonl": `{"type":"metadata"}` + "\n",
		"notify/state.json":      `{}`,
	}
}

// kimiGate runs the session gate in box, its `kimi --version` having printed
// printed, which the gate reads as the run's script hands it (v), and returns
// s, k, w and the driver's arguments as the gate left them.
func kimiGate(t *testing.T, paths Paths, work, printed string, session Session) []string {
	t.Helper()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	prereqtest.OnPath(t, "node", "the gate files a session through the run's driver", "install any current node")
	script := "v=" + shellword.Quote(printed) + "\nset --\n" + kimiSessionGate(session, paths, false) +
		`printf '%s|%s|%s|%s' "$s" "$k" "$w" "$*"`
	cmd := exec.Command(sh, "-c", script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "KIMI_CODE_HOME="+paths.KimiHome())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the gate failed: %v", err)
	}
	return strings.SplitN(string(out), "|", 4)
}

// A KIMI SESSION THE BOX HOLDS, OR A CARRIED ARCHIVE OF IT, IS CONTINUED FILED
// UNDER THE RUN'S OWN DIRECTORY: moved to the directory Kimi files that cwd's
// sessions in, its state.json naming the run's cwd, on 2.7.3 alone. Neither
// held nor carried, one that cannot be filed, or a box on another release or
// one it cannot read, is not continued, and the run says why.
//
// Mutation: skip the rekey, leave the cwd, continue on a release but 2.7.3
// (a pre-release or build of it read loosely included), or continue a session
// the box does not hold — a row goes red.
func TestAKimiSessionIsContinuedFiledUnderTheRunsDirectory(t *testing.T) {
	const session = "session_11111111-1111-4111-8111-111111111111"
	continues := Session{ID: session, Continues: true}

	t.Run("held", func(t *testing.T) {
		paths, work := kimiBox(t)
		for name, body := range kimiSessionFiles(session, "/elsewhere") {
			kimiWrite(t, paths.KimiSessions()+"/wd_elsewhere_000000000000/"+session+"/"+name, body)
		}
		got := kimiGate(t, paths, work, kimiSupported, continues)
		if got[0] != session || got[1] != "yes" || got[2] != "" || got[3] != "resume "+session {
			t.Fatalf("gate = %q", got)
		}
		state, err := filepath.Glob(paths.KimiSessions() + "/wd_work_*/" + session + "/state.json")
		if err != nil || len(state) != 1 {
			t.Fatalf("the session is filed at %v (%v), want under the run's directory", state, err)
		}
		raw, _ := os.ReadFile(state[0])
		var s struct{ Cwd string }
		if json.Unmarshal(raw, &s) != nil || s.Cwd != work {
			t.Errorf("state.json = %s, want its cwd the run's", raw)
		}
		for _, release := range []string{"2.7.4", "2.7.2", "", "2.7.3-rc.1", "2.7.3+x", "2.7.3.."} {
			got := kimiGate(t, paths, work, release, continues)
			if got[1] != "no" || got[3] != "" || !strings.Contains(got[2], "continued only on 2.7.3") {
				t.Errorf("release %q's gate = %q, want a session of the run's own, naming 2.7.3", release, got)
			}
		}
	})
	t.Run("carried", func(t *testing.T) {
		paths, work := kimiBox(t)
		files := map[string][]byte{}
		for name, body := range kimiSessionFiles(session, "/elsewhere") {
			files["wd_elsewhere_000000000000/"+session+"/"+name] = []byte(body)
		}
		kimiWrite(t, paths.SessionCarried(), string(kimiArchive(t, files)))
		got := kimiGate(t, paths, work, kimiSupported, continues)
		if got[1] != "yes" || got[2] != "" || got[3] != "resume "+session {
			t.Fatalf("gate = %q", got)
		}
		if digests, _ := os.ReadFile(paths.SessionDigests()); !strings.HasPrefix(string(digests), session+" ") {
			t.Errorf("the restored archive's digest is not recorded: %q", digests)
		}
	})
	for name, tc := range map[string]struct {
		says  string
		setup func(t *testing.T, paths Paths)
	}{
		"neither held nor carried": {"not in this box", func(*testing.T, Paths) {}},
		"one that cannot be filed": {"filed under", func(t *testing.T, paths Paths) {
			for n, body := range kimiSessionFiles("session_another", "/elsewhere") {
				kimiWrite(t, paths.KimiSessions()+"/wd_elsewhere_000000000000/"+session+"/"+n, body)
			}
		}},
		"an archive holding a link": {"could not be restored", func(t *testing.T, paths Paths) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			for n, body := range kimiSessionFiles(session, "/elsewhere") {
				_ = tw.WriteHeader(&tar.Header{Name: "wd_e_0/" + session + "/" + n, Mode: 0o600, Size: int64(len(body)),
					Typeflag: tar.TypeReg})
				_, _ = io.WriteString(tw, body)
			}
			_ = tw.WriteHeader(&tar.Header{Name: "wd_e_0/" + session + "/agents/escape", Linkname: "/etc",
				Typeflag: tar.TypeSymlink})
			_ = tw.Close()
			_ = gz.Close()
			kimiWrite(t, paths.SessionCarried(), buf.String())
		}},
	} {
		t.Run(name, func(t *testing.T) {
			paths, work := kimiBox(t)
			tc.setup(t, paths)
			got := kimiGate(t, paths, work, kimiSupported, continues)
			if got[0] != "" || got[1] != "no" || !strings.Contains(got[2], tc.says) || got[3] != "" {
				t.Fatalf("gate = %q, want a session of the run's own, saying %q", got, tc.says)
			}
		})
	}
}

// A KIMI PACK TAKES THE SESSION'S OWN FILES ALONE, named from the sessions
// directory down: its state.json, agents/ and notify/, never logs/.
//
// Mutation: pack the whole session directory, or name members from the home —
// the case fails.
func TestAKimiPackTakesTheSessionsOwnFilesAlone(t *testing.T) {
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	paths, _ := kimiBox(t)
	const session = "session_22222222-2222-4222-8222-222222222222"
	dir := paths.KimiSessions() + "/wd_work_0123456789ab/" + session
	for name, body := range kimiSessionFiles(session, "/w") {
		kimiWrite(t, dir+"/"+name, body)
	}
	kimiWrite(t, dir+"/logs/kimi-code.log", "a log")
	script := "s=" + session + " packed=\n" + kimiCapture(paths) + `[ -n "$packed" ]`
	if out, err := exec.Command(sh, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("the pack failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(paths.SessionCapture())
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Typeflag == tar.TypeReg {
			members = append(members, h.Name)
		}
	}
	slices.Sort(members)
	want := []string{"wd_work_0123456789ab/" + session + "/agents/main/wire.jsonl",
		"wd_work_0123456789ab/" + session + "/notify/state.json", "wd_work_0123456789ab/" + session + "/state.json"}
	if !slices.Equal(members, want) {
		t.Fatalf("members = %q, want %q", members, want)
	}
}

// A PACK THE ENGINE MAKES TAKES ONLY THE SESSION IT NAMES: a save or a take
// of a run whose report the engine read as none packs nothing, though the box
// reports a session now, since only a report the bind held names the run's.
//
// Mutation: fall back to the box's report where no session is named — the
// case fails.
func TestAKimiPackOnDemandTakesOnlyTheSessionTheEngineNames(t *testing.T) {
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	paths, _ := kimiBox(t)
	const session = "session_33333333-3333-4333-8333-333333333333"
	for name, body := range kimiSessionFiles(session, "/w") {
		kimiWrite(t, paths.KimiSessions()+"/wd_work_0123456789ab/"+session+"/"+name, body)
	}
	kimiWrite(t, paths.SessionReport(), session+"\n")
	staged, dest := paths.Home+"/staged", paths.Home+"/save"
	save, err := (Kimi{}).PackSave(paths, sessionRecord{}, staged, dest)
	if err != nil {
		t.Fatal(err)
	}
	if code := exitOf(exec.Command(sh, "-c", save).Run()); code != packNothing {
		t.Errorf("a save of no named session exited %d, want %d (nothing packed)", code, packNothing)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("a save of no named session packed the reported one (%v)", err)
	}
	if out, err := exec.Command(sh, "-c", (Kimi{}).Pack(paths, session, staged, dest, false)).CombinedOutput(); err != nil {
		t.Fatalf("the named session's pack failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the named session was not packed: %v", err)
	}
}

// exitOf is err's exit status: 0 for nil, -1 for an error that is no exit.
func exitOf(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// A CONTINUED RUN IS CHARGED ITS OWN RECORDS ALONE: the lines each agent's
// record held before the run are read past, so an earlier run's spend is never
// charged again.
//
// Mutation: read every line of the record — the case fails.
func TestAContinuedKimiRunIsChargedItsOwnRecordsAlone(t *testing.T) {
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	prereqtest.OnPath(t, "awk", "the case runs the box's script", "install awk")
	paths, _ := kimiBox(t)
	const session = "session_33333333-3333-4333-8333-333333333333"
	wire := paths.KimiSessions() + "/wd_w_0/" + session + "/agents/main/wire.jsonl"
	record := func(in string) string {
		return `{"type":"usage.record","agentId":"main","usage":{"inputOther":` + in + `},"usageScope":"turn"}` + "\n"
	}
	kimiWrite(t, wire, record("1"))
	script := "s=" + session + "\n" + kimiLinesBefore(paths) + "printf '%s' '" + record("2") + "' >> " +
		shellword.Quote(wire) + "\n" + kimiUsage(paths)
	out, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != `{"type":"crewlet.usage"}`+"\n"+record("2") {
		t.Fatalf("the script added:\n%s\nwant the mark and the run's own record", got)
	}
}

// A SESSION CARRIED TO ANOTHER BOX IS CONTINUED THERE WITH THE RUN'S OWN
// INSTRUCTIONS, ITS BIND RENEWED: the engine's rendered
// run, packed in one box and restored in a fresh one with another directory,
// sent each request with the earlier brief in its history and a system prompt
// naming this run's text and directory, never the first run's (the stand-in
// model's log, each system prompt cut to those lines); charged its own two
// requests; its record holding the first run's bind and the one appended.
//
// Mutation: skip the renewal, or charge the record whole — the case fails.
func TestACarriedKimiSessionIsRenderedWithTheRunsOwnInstructions(t *testing.T) {
	for _, release := range kimiReleases {
		var requests int
		for l := range bytes.Lines(kimiFixture(t, release, "carried.requests.jsonl")) {
			var r struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(l, &r); err != nil {
				t.Fatal(err)
			}
			requests++
			system := string(r.Messages[0].Content)
			if !strings.Contains(system, "STANDING-B") || !strings.Contains(system, "/tmp/wk-b") ||
				strings.Contains(system, "STANDING-A") || strings.Contains(system, "/tmp/wk-a") {
				t.Errorf("%s: request %d's system prompt is %s, want the run's own", release, requests, system)
			}
			if r.Messages[1].Role != "user" || string(r.Messages[1].Content) != `"first-brief"` {
				t.Errorf("%s: request %d does not carry the earlier run's brief", release, requests)
			}
		}
		res := (Kimi{}).Parse(kimiFixture(t, release, "carried.result.jsonl"))
		if !res.Success || res.Text != "second-2" || res.Turns != 2 || res.InputTokens != 200 || res.OutputTokens != 10 {
			t.Errorf("%s: result %+v, want the continued turn's answer and its own two requests", release, res)
		}
		var profiles []string
		for l := range bytes.Lines(kimiFixture(t, release, "carried.wire.jsonl")) {
			if bytes.HasPrefix(l, []byte(`{"type":"turn.prompt"`)) && bytes.Contains(l, []byte("second-brief")) {
				break
			}
			var bind struct {
				Type    string `json:"type"`
				Profile string `json:"profileName"`
			}
			if json.Unmarshal(l, &bind) == nil && bind.Type == "profile.bind" {
				profiles = append(profiles, bind.Profile)
			}
		}
		if requests != 2 || len(profiles) != 2 || profiles[0] == profiles[1] {
			t.Errorf("%s: %d requests; binds before the run %q, want the first run's then this run's", release,
				requests, profiles)
		}
	}
}
