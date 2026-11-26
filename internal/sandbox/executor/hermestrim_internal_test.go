package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesLine is one line of what a 0.23.3 resume loads of a dump: the dump's
// own line, or a settled admission's result as its slim stores it (the row,
// its value decoded).
type hermesLine struct {
	line string
	slim map[string]any
}

// hermesRow is one line of a dump, decoded.
type hermesRow struct {
	T string         `json:"t"`
	R map[string]any `json:"r"`
}

// hermesLinesOf is dump's lines, each with its break.
func hermesLinesOf(dump string) []string {
	lines := strings.SplitAfter(dump, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// hermesResumeLoads is what a 0.23.3 resume of dump loads, read as the
// measurement read it (its trim.py, rows+results): every line but a session's
// inactive message rows below its newest active compaction summary, whose ids
// it returns, each settled admission's result slimmed ([hermesSlim]).
func hermesResumeLoads(t *testing.T, dump string) ([]hermesLine, []int64) {
	t.Helper()
	lines := hermesLinesOf(dump)
	rows := make([]hermesRow, len(lines))
	newest, settled := map[any]float64{}, map[any]bool{}
	for i, l := range lines {
		if err := json.Unmarshal([]byte(l), &rows[i]); err != nil {
			t.Fatalf("the dump's line %d: %v", i+1, err)
		}
		r := rows[i].R
		switch {
		case rows[i].T == "messages" && r["active"] == 1.0 && r["_compressed_summary"] == 1.0:
			newest[r["session_id"]] = max(newest[r["session_id"]], r["id"].(float64))
		case rows[i].T == "session_admissions" && r["status"] == "terminal":
			settled["gateway.admission.result.v1."+r["admission_id"].(string)] = true
		}
	}
	var want []hermesLine
	var cut []int64
	for i, l := range lines {
		r := rows[i].R
		switch {
		case rows[i].T == "messages" && r["active"] == 0.0 && r["id"].(float64) < newest[r["session_id"]]:
			cut = append(cut, int64(r["id"].(float64)))
		case rows[i].T == "state_meta" && settled[r["key"]]:
			want = append(want, hermesSlim(t, l))
		default:
			want = append(want, hermesLine{line: l})
		}
	}
	return want, cut
}

// hermesSlim is a settled admission's result line as its slim stores it: its
// value read as 0.23.3 reads one (no envelope, or one of version 1 naming one
// of its messages), the envelope and the transcript's copies dropped and its
// messages left empty; the line itself where the value does not read so. No
// fixture holds a lone surrogate, which Go's decode cannot see.
func hermesSlim(t *testing.T, line string) hermesLine {
	t.Helper()
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatal(err)
	}
	r := row["r"].(map[string]any)
	text, _ := r["value"].(string)
	var v map[string]any
	if json.Unmarshal([]byte(text), &v) != nil {
		return hermesLine{line: line}
	}
	result, ok := v["result"].(map[string]any)
	if !ok {
		return hermesLine{line: line}
	}
	if env, held := v["_stored"].(map[string]any); held || v["_stored"] != nil {
		messages, listed := result["messages"].([]any)
		ref, numbered := env["reply_ref"].(float64)
		if !held || len(env) != 2 || env["v"] != 1.0 || !listed || !numbered || ref != math.Trunc(ref) || ref < 0 ||
			int(ref) >= len(messages) {
			return hermesLine{line: line}
		}
		if _, isRow := messages[int(ref)].(map[string]any); !isRow {
			return hermesLine{line: line}
		}
	}
	delete(v, "_stored")
	for _, k := range []string{"messages", "last_reasoning", "tools", "_turn_user_row"} {
		delete(result, k)
	}
	result["messages"] = []any{}
	r["value"] = v
	return hermesLine{slim: row}
}

// hermesSlimRow is a slimmed line of a trimmed dump, its value decoded.
func hermesSlimRow(t *testing.T, line string) map[string]any {
	t.Helper()
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("a slimmed line does not decode: %v", err)
	}
	r, _ := row["r"].(map[string]any)
	text, _ := r["value"].(string)
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("a slimmed result's value does not decode: %v", err)
	}
	r["value"] = v
	return row
}

// hermesSameDump says where got, a dump the trim wrote, differs from want,
// line for line: a slimmed line by what it holds, every other byte for byte.
func hermesSameDump(t *testing.T, got string, want []hermesLine) {
	t.Helper()
	lines := hermesLinesOf(got)
	if len(lines) != len(want) {
		t.Errorf("the trim kept %d lines, want %d", len(lines), len(want))
		return
	}
	for i, w := range want {
		switch {
		case w.slim == nil && lines[i] != w.line:
			t.Errorf("line %d is %.200q, want it kept as %.200q", i+1, lines[i], w.line)
		case w.slim != nil && !reflect.DeepEqual(hermesSlimRow(t, lines[i]), w.slim):
			t.Errorf("line %d holds %.300v, want the slim %.300v", i+1, hermesSlimRow(t, lines[i]), w.slim)
		}
	}
}

// hermesTrim runs the carry script's trim of dump under the machine's
// python3, and returns its answer and what it wrote ("" where it wrote
// nothing).
func hermesTrim(t *testing.T, dump string) (string, string) {
	t.Helper()
	python := prereqtest.OnPath(t, "python3", "the case runs the run's carry script", "install python3")
	dir := t.TempDir()
	carry, src, dst := filepath.Join(dir, "carry.py"), filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for path, body := range map[string][]byte{carry: hermesCarryScript, src: []byte(dump)} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	answer, err := exec.Command(python, "-I", carry, "trim", src, dst).Output()
	if err != nil {
		t.Fatalf("the trim failed: %v: %s", err, answer)
	}
	raw, err := os.ReadFile(dst)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(answer), string(raw)
}

// hermesIDsOf is the ids of dump's message rows, in order.
func hermesIDsOf(t *testing.T, dump string) []int64 {
	t.Helper()
	var ids []int64
	for _, l := range hermesLinesOf(dump) {
		var row hermesRow
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatal(err)
		}
		if row.T == "messages" {
			ids = append(ids, int64(row.R["id"].(float64)))
		}
	}
	return ids
}

// hermesIDs is the ids from through to, in order.
func hermesIDs(from, to int64) []int64 {
	var ids []int64
	for id := from; id <= to; id++ {
		ids = append(ids, id)
	}
	return ids
}

// A TRIMMED HERMES DUMP IS WHAT ITS RESUME LOADS: each recorded compacted
// session leaves out exactly the inactive rows the measurement dropped below
// its newest active summary (1-9 below 10; 1-46 below 47, the folded summary
// at 21 among them), keeps every other line byte for byte, and slims each
// settled result that reads as a result (the one-compaction fixture's second
// was cut short where it was recorded).
//
// Mutation: drop only the compacted rows, cut at the oldest summary, keep the
// folded one, slim nothing, or keep the envelope, and a case goes red.
func TestATrimmedHermesDumpKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	for fixture, c := range map[string]struct {
		cut   []int64
		slims int
	}{
		"session-compacted.jsonl":       {hermesIDs(1, 9), 2},
		"session-compacted-twice.jsonl": {hermesIDs(1, 46), 7},
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			dump := string(hermesFixture(t, "0.23.3", fixture))
			want, cut := hermesResumeLoads(t, dump)
			slims := 0
			for _, w := range want {
				if w.slim != nil {
					slims++
				}
			}
			if !slices.Equal(cut, c.cut) || slims != c.slims {
				t.Fatalf("the measured rule leaves out %v and slims %d, want %v and %d", cut, slims, c.cut, c.slims)
			}
			answer, got := hermesTrim(t, dump)
			if answer != "crewlet-carry ok\n" {
				t.Fatalf("the trim answered %q, want ok", answer)
			}
			hermesSameDump(t, got, want)
			if !slices.Equal(hermesIDsOf(t, got), slices.DeleteFunc(hermesIDsOf(t, dump), func(id int64) bool {
				return slices.Contains(c.cut, id)
			})) {
				t.Errorf("the trim kept the rows %v", hermesIDsOf(t, got))
			}
		})
	}
}

// A HERMES TRIM LEAVES OUT ONLY A SESSION'S INACTIVE ROWS BELOW ITS OWN
// NEWEST SUMMARY: an undo's inactive row above the summary stays, so the
// restored ids go on where they did; another session's inactive rows, below
// this one's summary but under none of their own, stay; a folded summary below
// goes with the rest.
//
// Mutation: drop every inactive row, read one summary for every session, or
// keep a summary row whatever it is, and a case goes red.
func TestAHermesTrimLeavesOutOnlyInactiveRowsBelowTheNewestSummary(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		edit func(id int64, row map[string]any)
		cut  []int64
	}{
		"an undo's row above the summary": {func(id int64, row map[string]any) {
			if id == 25 {
				row["active"] = 0
			}
		}, hermesIDs(1, 9)},
		"another session's inactive rows": {func(id int64, row map[string]any) {
			if id >= 4 && id <= 9 {
				row["session_id"] = "local-another"
			}
		}, hermesIDs(1, 3)},
		"a folded summary below": {func(id int64, row map[string]any) {
			if id == 3 {
				row["_compressed_summary"] = 1
			}
		}, hermesIDs(1, 9)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dump := string(hermesFixtureRows(t, "session-compacted.jsonl", c.edit))
			answer, got := hermesTrim(t, dump)
			if answer != "crewlet-carry ok\n" {
				t.Fatalf("the trim answered %q, want ok", answer)
			}
			kept := slices.DeleteFunc(hermesIDsOf(t, dump), func(id int64) bool { return slices.Contains(c.cut, id) })
			if ids := hermesIDsOf(t, got); !slices.Equal(ids, kept) {
				t.Errorf("the trim kept the rows %v, want %v", ids, kept)
			}
			want, _ := hermesResumeLoads(t, dump)
			hermesSameDump(t, got, want)
		})
	}
}

// hermesAdmissionEdit is the dump with each admission's rows (its
// session_admissions row and its stored result) edited by edit, given the
// admission's place among them, every row written back as Go writes it.
func hermesAdmissionEdit(t *testing.T, dump string, edit func(n int, table string, row map[string]any)) (string,
	[]string) {
	t.Helper()
	var ids []string
	for _, l := range hermesLinesOf(dump) {
		var row hermesRow
		if json.Unmarshal([]byte(l), &row) == nil && row.T == "session_admissions" {
			ids = append(ids, row.R["admission_id"].(string))
		}
	}
	var out strings.Builder
	for _, l := range hermesLinesOf(dump) {
		var row hermesRow
		if err := json.Unmarshal([]byte(l), &row); err != nil || row.T == "" {
			out.WriteString(l)
			continue
		}
		for n, id := range ids {
			if row.T == "session_admissions" && row.R["admission_id"] == id ||
				row.T == "state_meta" && row.R["key"] == "gateway.admission.result.v1."+id {
				edit(n, row.T, row.R)
			}
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(append(raw, '\n'))
	}
	return out.String(), ids
}

// hermesResultEdit is a stored result's value with edit applied to it, decoded.
func hermesResultEdit(t *testing.T, row map[string]any, edit func(v map[string]any)) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(row["value"].(string)), &v); err != nil {
		t.Fatal(err)
	}
	edit(v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	row["value"] = string(raw)
}

// A HERMES TRIM SLIMS ONLY A SETTLED RESULT IT READS AS HERMES DOES: a
// terminal admission's result, with or without the envelope that stores its
// reply once, loses its transcript's copies (messages, last_reasoning, tools,
// _turn_user_row) and the envelope, every other key kept; one not yet settled,
// one whose envelope names no message of its own or another version, a value
// that is no result, and one whose slim cannot be written as text stay whole.
//
// Mutation: slim whatever its status, read any envelope, keep the envelope or
// a transcript key, or drop the text check, and a case goes red.
func TestAHermesTrimSlimsOnlySettledResultsItReads(t *testing.T) {
	t.Parallel()
	whole := string(hermesFixture(t, "0.23.3", "session-compacted-twice.jsonl"))
	slim := map[int]bool{0: true, 5: true}
	dump, ids := hermesAdmissionEdit(t, whole, func(n int, table string, row map[string]any) {
		switch {
		case table == "session_admissions" && n == 1:
			row["status"] = "running"
		case table != "state_meta":
		case n == 0:
			hermesResultEdit(t, row, func(v map[string]any) {
				result := v["result"].(map[string]any)
				result["tools"] = []any{map[string]any{"name": "terminal"}}
				result["_turn_user_row"] = map[string]any{"id": 1}
			})
		case n == 2:
			hermesResultEdit(t, row, func(v map[string]any) { v["_stored"].(map[string]any)["reply_ref"] = 99 })
		case n == 3:
			hermesResultEdit(t, row, func(v map[string]any) { v["_stored"].(map[string]any)["v"] = 2 })
		case n == 4:
			row["value"] = `{"result": [`
		case n == 5:
			hermesResultEdit(t, row, func(v map[string]any) { delete(v, "_stored") })
		case n == 6:
			hermesResultEdit(t, row, func(v map[string]any) { v["result"].(map[string]any)["final_response"] = "LONE" })
			row["value"] = strings.Replace(row["value"].(string), `"LONE"`, `"\ud800"`, 1)
		}
	})
	if len(ids) != 7 {
		t.Fatalf("the fixture holds %d admissions, want 7", len(ids))
	}
	answer, got := hermesTrim(t, dump)
	if answer != "crewlet-carry ok\n" {
		t.Fatalf("the trim answered %q, want ok", answer)
	}
	results := func(dump string) map[string]string {
		out := map[string]string{}
		for _, l := range hermesLinesOf(dump) {
			var row hermesRow
			if json.Unmarshal([]byte(l), &row) == nil && row.T == "state_meta" {
				key, _ := row.R["key"].(string)
				out[strings.TrimPrefix(key, "gateway.admission.result.v1.")] = l
			}
		}
		return out
	}
	before, after := results(dump), results(got)
	for n, id := range ids {
		if !slim[n] {
			if after[id] != before[id] {
				t.Errorf("admission %d's result became %.300q, want it whole", n, after[id])
			}
			continue
		}
		was := hermesSlimRow(t, before[id])["r"].(map[string]any)["value"].(map[string]any)
		result := hermesSlimRow(t, after[id])["r"].(map[string]any)["value"]
		wantResult := hermesWithout(was["result"].(map[string]any), "messages", "last_reasoning", "tools", "_turn_user_row")
		wantResult["messages"] = []any{}
		want := hermesWithout(was, "_stored")
		want["result"] = wantResult
		if !reflect.DeepEqual(result, want) {
			t.Errorf("admission %d's result slimmed to %.400v, want %.400v", n, result, want)
		}
	}
}

// hermesWithout is a copy of m without the keys drop.
func hermesWithout(m map[string]any, drop ...string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if !slices.Contains(drop, k) {
			out[k] = v
		}
	}
	return out
}

// A HERMES DUMP WITH NOTHING INACTIVE BELOW A SUMMARY IS NOT TRIMMED: a
// session never compacted, one whose turn failed, one whose inactive rows are
// all above its summary and one with an inactive row but no summary answer
// none, and the trim writes nothing.
//
// Mutation: answer ok where no row is left out, or cut where there is no
// summary, and a case goes red.
func TestAHermesDumpWithNothingInactiveIsNotTrimmed(t *testing.T) {
	t.Parallel()
	for name, dump := range map[string]string{
		"never compacted": string(hermesFixture(t, "0.23.3", "session-steered.jsonl")),
		"a failed turn":   string(hermesFixture(t, "0.23.3", "session-failed.jsonl")),
		"nothing below its summary": string(hermesFixtureRows(t, "session-compacted.jsonl", func(id int64, row map[string]any) {
			row["active"] = 1
			if id == 25 {
				row["active"] = 0
			}
		})),
		"no summary": string(hermesFixtureRows(t, "session-steered.jsonl", func(id int64, row map[string]any) {
			if id == 2 {
				row["active"] = 0
			}
		})),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if answer, got := hermesTrim(t, dump); answer != "crewlet-carry none\n" || got != "" {
				t.Errorf("the trim answered %q and wrote %d bytes, want none and nothing", answer, len(got))
			}
		})
	}
}

// hermesTitleOf is the title of the session dump holds.
func hermesTitleOf(t *testing.T, dump []byte) string {
	t.Helper()
	for line := range bytes.Lines(dump) {
		var row hermesRow
		if json.Unmarshal(line, &row) == nil && row.T == "sessions" {
			if title, ok := row.R["title"].(string); ok {
				return title
			}
		}
	}
	t.Fatal("the dump holds no titled session")
	return ""
}

// hermesTrimBox is a box home whose Hermes store holds dump's session,
// restored as a carried one is, beside the carry script a run writes; and the
// session's title.
func hermesTrimBox(t *testing.T, dump []byte) (Paths, string) {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	for _, dir := range []string{p.WorkDir(), p.HermesHome()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(t.TempDir(), hermesArchiveMember)
	for path, body := range map[string][]byte{p.HermesCarry(): hermesCarryScript, src: dump} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	title := hermesTitleOf(t, dump)
	hermesCarry(t, "restore", src, p.HermesStateDB(), p.HermesPolicy(title), title)
	return p, title
}

// hermesWholeDump is the dump every pack of the box at p makes of the session
// titled title, before any trim.
func hermesWholeDump(t *testing.T, p Paths, title string) string {
	t.Helper()
	return hermesCarry(t, "dump", p.HermesStateDB(), title, p.HermesPolicy(title))
}

// hermesPackShell runs script with sh in the box at p, its Hermes the
// stand-in launcher ([hermesLauncherStandIn]).
func hermesPackShell(t *testing.T, p Paths, script string) {
	t.Helper()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	bin := hermesLauncherStandIn(t)
	cmd := exec.Command(sh, "-c", script)
	cmd.Dir = p.Home
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HERMES_HOME="+p.HermesHome())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the pack failed: %v: %s", err, out)
	}
}

// hermesPackRun packs the session titled title in the box at p, at at, the
// session written by release (its pack's [trimRelVar]), and returns the
// archive's one member, the archive's size and the word the pack left beside
// it ("" for none).
func hermesPackRun(t *testing.T, p Paths, title, release string, at trimPoints) (string, int64, string) {
	t.Helper()
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	hermesPackShell(t, p, "sid="+shellword.Quote(title)+" "+trimRelVar+"="+shellword.Quote(release)+" packed=\n"+
		hermesCarryRun(p)+hermesPackAt(p, staged, dest, fmt.Sprintf("exit %d", packNothing), at)+
		`[ -n "$packed" ] || exit 9`)
	archive := codexFile(t, dest)
	members := codexMembers(t, archive)
	if len(members) != 1 {
		t.Fatalf("the archive holds %d members, want the dump alone", len(members))
	}
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return members[hermesArchiveMember], int64(len(archive)), string(word)
}

// A TRIMMED HERMES PACK KEEPS WHAT ITS RESUME LOADS: past the point, the
// packed dump is the session's as its resume loads it ([hermesResumeLoads]),
// and the pack says so beside the archive in a word naming the archive's size
// and the dump's bytes before and kept, which the take reads back.
//
// Mutation: pack the dump uncut, cut it and tar the whole, or leave no word,
// and a case goes red.
func TestATrimmedHermesPackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"session-compacted.jsonl", "session-compacted-twice.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			p, title := hermesTrimBox(t, hermesFixture(t, "0.23.3", fixture))
			whole := hermesWholeDump(t, p, title)
			want, cut := hermesResumeLoads(t, whole)
			if len(cut) == 0 {
				t.Fatal("the fixture holds no row to trim")
			}
			member, size, mark := hermesPackRun(t, p, title, hermesFloor(), trimEverything)
			hermesSameDump(t, member, want)
			if got := parseTrimMark(mark, size); got != (sandbox.ArchiveTrim{RecordBytes: int64(len(whole)),
				KeptBytes: int64(len(member))}) {
				t.Errorf("the pack's word %q reads as %+v, want the dump's bytes before and kept", mark, got)
			}
		})
	}
}

// A HERMES PACK NOT PAST THE POINT, OR WITH NOTHING TO TRIM, IS PACKED WHOLE:
// a compacted session's dump at the plaintext point and a session never
// compacted keep every line, and no word says otherwise.
//
// Mutation: trim at the point rather than past it, or pack the trim's empty
// output where it answered none, and a case goes red.
func TestAHermesPackWithNothingToTrimIsPackedWhole(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		fixture string
		at      func(whole string) trimPoints
	}{
		"at the point": {"session-compacted.jsonl", func(whole string) trimPoints {
			return trimPoints{plain: int64(len(whole)), packed: 1 << 40}
		}},
		"never compacted": {"session-steered.jsonl", func(string) trimPoints { return trimEverything }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, title := hermesTrimBox(t, hermesFixture(t, "0.23.3", c.fixture))
			whole := hermesWholeDump(t, p, title)
			member, _, mark := hermesPackRun(t, p, title, hermesFloor(), c.at(whole))
			if member != whole || mark != "" {
				t.Errorf("the pack kept %d of %d bytes and said %q, want it whole and unsaid", len(member),
					len(whole), mark)
			}
		})
	}
}

// ONLY THE MEASURED HERMES RELEASE IS TRIMMED: the trim was measured on
// 0.23.3, the one release whose sessions Hermes continues, so a floor moved
// breaks this test for whoever moves it to measure again; a session another
// release wrote, or one whose writer the pack was not told, is packed whole.
//
// Mutation: drop the release's check, or trim on a release that reads as
// 0.23.3's prefix, and a case goes red.
func TestOnlyTheMeasuredHermesReleaseIsTrimmed(t *testing.T) {
	t.Parallel()
	if got := (Hermes{}).trimRelease(); got != "0.23.3" || got != hermesFloor() {
		t.Fatalf("Hermes trims sessions %q wrote, with its floor at %q; the trim was measured on 0.23.3, the "+
			"floor: measure it again on the new floor", got, hermesFloor())
	}
	p, title := hermesTrimBox(t, hermesFixture(t, "0.23.3", "session-compacted.jsonl"))
	whole := hermesWholeDump(t, p, title)
	for _, release := range []string{"", "0.23.2", "0.23.4", "0.23.31"} {
		member, _, mark := hermesPackRun(t, p, title, release, trimEverything)
		if member != whole || mark != "" {
			t.Errorf("a session %q wrote was packed as %d of %d bytes, saying %q; want it whole", release,
				len(member), len(whole), mark)
		}
	}
}

// A HERMES PACK PAST THE PACKED POINT IS TRIMMED AND PACKED AGAIN: its dump
// under the plaintext point, an archive past its own is cut and tarred again,
// holding the trimmed dump, the word naming that archive.
//
// Mutation: skip the second tar, or check the plaintext alone, and this goes
// red.
func TestAHermesPackPastThePackedPointIsTrimmed(t *testing.T) {
	t.Parallel()
	p, title := hermesTrimBox(t, hermesFixture(t, "0.23.3", "session-compacted.jsonl"))
	want, _ := hermesResumeLoads(t, hermesWholeDump(t, p, title))
	member, size, mark := hermesPackRun(t, p, title, hermesFloor(), trimPoints{plain: 1 << 40, packed: 1})
	hermesSameDump(t, member, want)
	if !parseTrimMark(mark, size).Trimmed() {
		t.Errorf("the pack's word %q does not name its archive as trimmed", mark)
	}
}

// EVERY HERMES PACK TRIMS AT THE TAKE'S BOUNDS, BY THE RELEASE THAT WROTE ITS
// SESSION: the run's end, a pack on demand (a stop's) and a save each run the
// one trim at [trimAt]; the run's end names the box's release, and every pack
// made on demand the release its record names.
//
// Mutation: pack one path at other points or untrimmed, or leave a path's
// release unset, and a case goes red.
func TestEveryHermesPackTrimsAtTheTakesBounds(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	save, err := Hermes{}.PackSave(p, sessionRecord{id: hermesFixtureTitle}, p.SessionSaveStaging("t", "1"),
		p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	nothing := fmt.Sprintf("exit %d", packNothing)
	for name, c := range map[string]struct{ script, staged, want string }{
		"end": {hermesFinish(p), p.SessionStaging(), "finish() {\npacked= " + trimRelVar + "=$n\n" +
			hermesPackAt(p, p.SessionStaging(), p.SessionCapture(), ":", trimAt)},
		"stop": {Hermes{}.Pack(p, hermesFixtureTitle, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true),
			p.SessionPackStaging("1"), hermesPackAt(p, p.SessionPackStaging("1"), p.SessionPackHeld("1"), nothing, trimAt)},
		"save": {save, p.SessionSaveStaging("t", "1"), hermesPackAt(p, p.SessionSaveStaging("t", "1"),
			p.SessionSave("t", "1"), nothing, trimAt)},
	} {
		member := shellword.Quote(c.staged + "/in/" + hermesArchiveMember)
		if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, trimStaged(Hermes{}, trimAt, member)) {
			t.Errorf("the %s pack does not trim at the take's bounds by its release", name)
		}
	}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(PathsFor(box).SessionRecord(), "id "+hermesFixtureTitle+
		"\ncontinued yes\nwhy \nrelease Hermes Agent v0.23.3 (2026.9.24)\n")
	box.ExecFunc = func(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: packNothing}, nil
	}
	if _, err := NewHermes().PackSave(t.Context(), box, "node:1", hermesFixtureTitle); err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(trimRelVar+"="+shellword.Quote(hermesFloor())+"\n", "'", `'\''`)
	if execs := box.Execs(); len(execs) == 0 || !strings.Contains(execs[len(execs)-1].Command, want) {
		t.Errorf("a save names no release off its record to trim by: %+v", execs)
	}
}

// A TRIMMED HERMES TAKE IS THE TRIMMED ARCHIVE, SAID SO, AND STILL BOUNDED:
// the take describes the archive its pack trimmed as trimmed, and one a byte
// past the take's bound once trimmed is refused as too large.
//
// Mutation: read no word for Hermes, or exempt a trimmed archive from the
// bound, and a check goes red.
func TestATrimmedHermesTakeIsSaidSoAndStillBounded(t *testing.T) {
	t.Parallel()
	p, title := hermesTrimBox(t, hermesFixture(t, "0.23.3", "session-compacted.jsonl"))
	whole := hermesWholeDump(t, p, title)
	hermesPackShell(t, p, "sid="+shellword.Quote(title)+" "+trimRelVar+"="+shellword.Quote(hermesFloor())+
		" packed=\n"+hermesCarryRun(p)+hermesPackAt(p, p.SessionStaging(), p.SessionCapture(), "exit 3",
		trimEverything)+`[ -n "$packed" ] || exit 9`)
	archive := codexFile(t, p.SessionCapture())
	member := codexMembers(t, archive)[hermesArchiveMember]
	box := sandbox.NewFakeSandbox("box-1")
	paths := PathsFor(box)
	box.Put(paths.SessionRecord(), "id "+title+"\ncontinued yes\nwhy \nrelease Hermes Agent v0.23.3 (2026.9.24)\n")
	box.Put(paths.SessionCapture(), string(archive))
	box.Put(trimMarkOf(paths.SessionCapture()), string(codexFile(t, trimMarkOf(p.SessionCapture()))))
	spool := unboundSpool(t.TempDir())
	size := int64(len(archive))
	_, err := NewHermes().TakeSession(t.Context(), box, sandbox.SessionTake{Spool: spool, MaxBytes: size - 1,
		Session: title})
	if !errors.Is(err, executorkeep.ErrSessionTooLarge) {
		t.Errorf("a trimmed archive a byte past the take's bound came to %v, want it refused as too large", err)
	}
	taken, err := NewHermes().TakeSession(t.Context(), box, sandbox.SessionTake{Spool: spool, MaxBytes: size,
		Session: title})
	if err != nil || taken.Body == nil {
		t.Fatalf("the trimmed archive at the take's bound: %+v, %v", taken, err)
	}
	if _, err := io.Copy(io.Discard, taken.Body); err != nil {
		t.Errorf("reading the take: %v", err)
	}
	_ = taken.Body.Close()
	want := sandbox.ArchiveTrim{RecordBytes: int64(len(whole)), KeptBytes: int64(len(member))}
	if !want.Trimmed() || taken.Archive.Trim != want {
		t.Errorf("the take describes its archive's trim as %+v, want %+v", taken.Archive.Trim, want)
	}
}
