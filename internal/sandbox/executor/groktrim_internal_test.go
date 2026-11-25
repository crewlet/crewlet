package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// grokTrimSession and grokTrimChild are the session [grokTrimRun] packs and the
// subagent its updates name.
const (
	grokTrimSession = "11111111-2222-4333-8444-0000000000d1"
	grokTrimChild   = "11111111-2222-4333-8444-0000000000d2"
)

// grokTrimLayout is a session directory as 1.0.92 leaves it after two
// compactions (compact-plan listing, stub bodies), each file's path under the
// session's directory: compacted names whether its compaction requests and
// checkpoints are there. terminal/compaction_requests/ is no directory of the
// session's own, kept as any call log is.
func grokTrimLayout(compacted bool, updates string) map[string]string {
	files := map[string]string{
		"chat_history.jsonl": `{"type":"system","content":"<crewlet_seat_instructions>rules</crewlet_seat_instructions>"}` +
			"\n" + `{"type":"user","content":"the summary","synthetic_reason":"compaction_meta"}` + "\n",
		"updates.jsonl":                           updates,
		"compaction/INDEX.md":                     "| segment | turns |\n| 000 | 1-4 |\n| 001 | 5-9 |\n",
		"compaction/segment_000.md":               strings.Repeat("the first window\n", 40),
		"compaction/segment_001.md":               strings.Repeat("the second window\n", 40),
		"terminal/call_4.log":                     "shell-ran\n",
		"terminal/compaction_requests/call_9.log": "a call log under a name of Grok's\n",
		"tool_definitions.json":                   `[{"name":"read_file"}]`,
		"summary.json":                            `{"title":"t"}`,
		"signals.json":                            `{"contextTokensUsed":2569}`,
		"events.jsonl":                            `{"turn_number":3}` + "\n",
		"rewind_points.jsonl":                     `{"prompt_index":3}` + "\n",
		"title_refresh_idx":                       "1",
		"system_prompt.txt":                       "you are grok\n",
	}
	if compacted {
		for _, id := range []string{"a385c36d-fa49-46ac-a7bd-fb1a3a2a0006", "575251a2-3bba-4c1b-8b70-2cd00b9e1526"} {
			files["compaction_requests/"+id+".json"] = strings.Repeat(`{"role":"user","content":"before"}`, 60)
			files["compaction_checkpoints/"+id+".json"] = strings.Repeat(`{"prompt_index_at_compaction":3}`, 9)
		}
	}
	return files
}

// grokTrimHome lays out a box home holding session [grokTrimSession] and its
// subagent [grokTrimChild], each as [grokTrimLayout] (compacted or not), and a
// lock no pack takes; and returns its paths and every member a whole pack
// holds, by its name under the sessions directory.
func grokTrimHome(t *testing.T, compacted bool) (Paths, map[string]string) {
	t.Helper()
	p := Paths{Home: t.TempDir() + "/home"}
	key := grokPathKey(p.Home)
	spawned := `{"timestamp":1,"method":"session/update","params":{"sessionId":"` + grokTrimSession +
		`","update":{"sessionUpdate":"subagent_spawned","child_session_id":"` + grokTrimChild + `"}}}` + "\n"
	members := map[string]string{}
	for session, updates := range map[string]string{grokTrimSession: spawned, grokTrimChild: "{}\n"} {
		for name, body := range grokTrimLayout(compacted, updates) {
			members[key+"/"+session+"/"+name] = body
		}
	}
	for name, body := range members {
		writeHere(t, filepath.Join(p.GrokSessions(), name))
		if err := os.WriteFile(filepath.Join(p.GrokSessions(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHere(t, filepath.Join(p.GrokSessions(), key, grokTrimSession, "chat_history.jsonl.lock"))
	return p, members
}

// grokTrimRun packs session [grokTrimSession] of a box laid out by
// [grokTrimHome] as the runner's pack does, written by release, at at, beside a
// working harness where working says so; and returns the members a whole pack
// holds, the archive's members in its order with their bodies, its size and
// the word left beside it ("" for none).
func grokTrimRun(t *testing.T, compacted bool, release string, at trimPoints, working bool) (whole map[string]string,
	order []string, members map[string]string, size int64, mark string) {
	t.Helper()
	p, whole := grokTrimHome(t, compacted)
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	script := "s=" + grokTrimSession + " " + trimRelVar + "=" + shellword.Quote(release) + " packed=\n" +
		grokPackAt(p, packAt{staged: staged, dest: dest, working: working}, at) + `[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	order, members = grokTrimMembers(t, archive)
	return whole, order, members, int64(len(archive)), string(word)
}

// grokTrimMembers is archive's regular files, in its order and by name.
func grokTrimMembers(t *testing.T, archive []byte) ([]string, map[string]string) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("the archive is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	var order []string
	members := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return order, members
		}
		if err != nil {
			t.Fatalf("reading the archive's members: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		order, members[h.Name] = append(order, h.Name), string(body)
	}
}

// grokTrimKept is whole but each member in a session's own
// compaction_requests/ or compaction_checkpoints/, written out apart from
// [grokTrimmedDirs], which the pack leaves out by.
func grokTrimKept(whole map[string]string) map[string]string {
	kept := map[string]string{}
	for name, body := range whole {
		if parts := strings.SplitN(name, "/", 4); parts[2] != "compaction_requests" &&
			parts[2] != "compaction_checkpoints" {
			kept[name] = body
		}
	}
	return kept
}

// grokTrimBytesOf is the bytes of members' bodies.
func grokTrimBytesOf(members map[string]string) int64 {
	var n int64
	for _, body := range members {
		n += int64(len(body))
	}
	return n
}

// A TRIMMED GROK PACK LEAVES OUT ONLY WHAT NO RESUME READS: past the plaintext
// point, or with an archive past the packed one, on 1.0.92, the archive holds
// every file of the session and of its subagent byte for byte, its record
// first, but those in each one's own compaction_requests/ and
// compaction_checkpoints/ (updates.jsonl, compaction/'s segments and a call
// log under a like name stay), beside a working harness as at the run's end;
// and the pack says the members' bytes before and kept beside the archive.
//
// Mutation: keep either directory, leave one out of a subagent only, match a
// directory anywhere in a member's path, leave out nothing past the packed
// point, or leave no word, and a case goes red.
func TestATrimmedGrokPackLeavesOutOnlyWhatNoResumeReads(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		at      trimPoints
		working bool
	}{
		"past the plaintext point":                      {trimEverything, false},
		"past the packed point":                         {trimPoints{plain: 1 << 40, packed: 1}, false},
		"past the plaintext point beside a working run": {trimEverything, true},
		"past the packed point beside a working run":    {trimPoints{plain: 1 << 40, packed: 1}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			whole, order, members, size, mark := grokTrimRun(t, true, grokSupported(), c.at, c.working)
			kept := grokTrimKept(whole)
			if len(kept) != len(whole)-8 {
				t.Fatalf("the layout leaves %d of %d members, want the eight of the two directories out", len(kept),
					len(whole))
			}
			if !maps.Equal(members, kept) {
				t.Errorf("the archive holds\n%s\nwant, each byte for byte,\n%s",
					strings.Join(slices.Sorted(maps.Keys(members)), "\n"), strings.Join(slices.Sorted(maps.Keys(kept)), "\n"))
			}
			if len(order) == 0 || strings.Count(order[0], "/") != 2 ||
				!strings.HasSuffix(order[0], "/"+grokTrimSession+"/chat_history.jsonl") {
				t.Errorf("the archive's members open with %q, want the session's record", order)
			}
			want := sandbox.ArchiveTrim{RecordBytes: grokTrimBytesOf(whole), KeptBytes: grokTrimBytesOf(kept)}
			if got := parseTrimMark(mark, size); got != want {
				t.Errorf("the pack's word %q reads as %+v, want the members' bytes before and kept, %+v", mark, got,
					want)
			}
		})
	}
}

// A GROK PACK BELOW BOTH POINTS, ON ANOTHER RELEASE, OR WITH NOTHING TO LEAVE
// OUT IS THE ONE IT WAS: every file of the session and its subagent, and no
// word.
//
// Mutation: trim at the plaintext point itself, drop the release's check,
// trim on a release 1.0.92 prefixes, or say a trim that left nothing out, and
// a case goes red.
func TestAGrokPackBelowThePointsOrOnAnotherReleaseIsWhole(t *testing.T) {
	t.Parallel()
	_, whole := grokTrimHome(t, true)
	at := grokTrimBytesOf(whole)
	for name, c := range map[string]struct {
		compacted bool
		release   string
		at        trimPoints
	}{
		"at the plaintext point":            {true, grokSupported(), trimPoints{plain: at, packed: 1 << 40}},
		"another release":                   {true, "1.0.91", trimEverything},
		"a newer release":                   {true, "1.0.93", trimEverything},
		"a release 1.0.92 prefixes":         {true, grokSupported() + "1", trimEverything},
		"no release":                        {true, "", trimEverything},
		"nothing to leave out":              {false, grokSupported(), trimEverything},
		"nothing to leave out, past packed": {false, grokSupported(), trimPoints{plain: 1 << 40, packed: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			whole, _, members, _, mark := grokTrimRun(t, c.compacted, c.release, c.at, false)
			if !maps.Equal(members, whole) || mark != "" {
				t.Errorf("the pack kept %d of %d members and said %q, want every one and no word", len(members),
					len(whole), mark)
			}
		})
	}
}

// EVERY GROK PACK TRIMS AT THE TAKE'S BOUNDS, BY THE RELEASE THAT WROTE ITS
// SESSION: the run's end, a pack on demand (a stop's) and a save each run the
// one trim at [trimAt], the run's end naming the box's bare release as the
// engine reads it, which is the release the trim was measured on.
//
// Mutation: pack one path at other points or untrimmed, or leave the run's
// end's release unset, and a case goes red.
func TestEveryGrokPackTrimsAtTheTakesBounds(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	save, err := Grok{}.PackSave(p, sessionRecord{id: grokTrimSession}, p.SessionSaveStaging("t", "1"),
		p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ script, want string }{
		"end": {grokCapture(p), bareReleaseShell(trimRelVar) +
			grokPackAt(p, packAt{staged: p.SessionStaging(), dest: p.SessionCapture()}, trimAt)},
		"stop": {Grok{}.Pack(p, grokTrimSession, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true),
			grokPackAt(p, packAt{staged: p.SessionPackStaging("1"), dest: p.SessionPackHeld("1"), working: true},
				trimAt)},
		"save": {save, grokPackAt(p, packAt{staged: p.SessionSaveStaging("t", "1"),
			dest: p.SessionSave("t", "1"), working: true}, trimAt)},
	} {
		if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, fmt.Sprint(trimPlainPoint)) {
			t.Errorf("the %s pack does not trim at the take's bounds by its release", name)
		}
	}
	printed := "grok " + grokSupported() + " (b06756fd6256)"
	out, _, code := codexShell(t, t.TempDir(), "v="+shellword.Quote(printed)+"\n"+bareReleaseShell(trimRelVar)+
		`printf '%s' "$`+trimRelVar+`"`)
	if code != 0 || out != releaseOf(printed) || out != (Grok{}).trimRelease() {
		t.Errorf("%q: the run's end trims by %q (exit %d), want %q as the engine reads it", printed, out, code,
			releaseOf(printed))
	}
}
