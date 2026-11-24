package executor_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// ocSaveSession is the session an OpenCode save case's run worked in: the one
// the real export of 2.3.2 this package keeps holds.
const ocSaveSession = "ses_0123456789abcdef0123456789abcdef"

// openCodeSaving is an OpenCode box whose stand-in answers as [fakeOpenCode],
// recording in the directory it returns, with vars set beside it; its run's
// record names [ocSaveSession] at release, the stand-in holds that session,
// and OpenCode's database is at its place, a few bytes. The stand-in records
// the pins every call ran under (`pinned`), and its export prints export-doc
// where a case wrote one, FAKE_SESSION_EXPORT_BYTES zeros (sparse) where set,
// and breaks off part-written while export-fails exists.
func openCodeSaving(t *testing.T, release, vars string) (*executor.Runner, sandbox.Sandbox, string) {
	t.Helper()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
		strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir)+vars, 1))
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+ocSaveSession+"\ncontinued no\nrelease "+release+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", ocSaveSession), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteFile(t.Context(), openCodeDatabase(b), []byte("SQLite format 3")); err != nil {
		t.Fatal(err)
	}
	return runner, b, dir
}

// openCodeDatabase is where OpenCode keeps its database in b, under the box's
// XDG_DATA_HOME.
func openCodeDatabase(b sandbox.Sandbox) string {
	return b.Home() + "/.local/share/opencode/opencode.db"
}

// sparse sets the file at path, made where missing, to size bytes it holds no
// block of.
func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// exported reports whether dir's stand-in was asked for an export.
func exported(t *testing.T, dir string) bool {
	t.Helper()
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Contains(string(calls), "session export")
}

// AN OPENCODE SAVE IS EXPORTED BESIDE ITS SERVER TO ITS TAKER'S OWN PATH: by a
// private server (`--standalone`), every call with the models fetch and the
// updater off, to a place named
// after its taker, its digest kept, its staging gone and the job's capture as
// it was; the take that names it reads that export.
//
// Mutation: move it over the job's capture, export through the run's server,
// drop the digest or the pins, and a check goes red.
func TestAnOpenCodeSaveIsExportedBesideItsServerToItsTakersOwnPath(t *testing.T) {
	t.Parallel()
	runner, b, dir := openCodeSaving(t, "2.3.2", "")
	p := paths(b)
	const capture = "the job's own capture, as its script left it"
	if err := b.WriteFile(t.Context(), p.SessionCapture(), []byte(capture)); err != nil {
		t.Fatal(err)
	}
	saved, err := runner.PackSave(t.Context(), b, takerA, "")
	if err != nil {
		t.Fatalf("PackSave: %v", err)
	}
	if !strings.HasPrefix(saved.Path, p.SessionSave(takerA, "")) || saved.SessionID != ocSaveSession ||
		saved.Harness != executor.OpenCodeName || saved.Format != executor.OpenCodeArchiveFormat ||
		saved.Version != "2.3.2" || saved.Size <= 0 {
		t.Errorf("the save is described as %+v, want session %s of OpenCode 2.3.2 at a place of its own named "+
			"after its taker, %s…", saved, ocSaveSession, p.SessionSave(takerA, ""))
	}
	if got := boxFile(t, b, p.SessionCapture()); got != capture {
		t.Errorf("the save changed the job's capture to %q", got)
	}
	if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the save left its staging directory behind: %s", strings.Join(left, ", "))
	}
	archive := []byte(boxFile(t, b, saved.Path))
	if digests := boxFile(t, b, p.SessionDigests()); !strings.Contains(digests,
		ocSaveSession+" "+digestOf(t, archive)+"\n") {
		t.Errorf("the box keeps no digest of the save: %q", digests)
	}
	if export := gunzipped(t, archive); !json.Valid(export) ||
		!strings.Contains(string(export), `"id":"`+ocSaveSession+`"`) {
		t.Errorf("the save holds %q, want the session's export", export)
	}
	if calls := readIn(t, dir, "calls"); !strings.Contains(calls, "session export --standalone "+ocSaveSession) ||
		strings.Contains(calls, "--server") {
		t.Errorf("the save did not export through a private server alone; OpenCode was asked:\n%s", calls)
	}
	for line := range strings.Lines(readIn(t, dir, "pinned")) {
		pins := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(pins) != 3 || pins[0] != "1" || pins[1] != "1" {
			t.Errorf("a call of the save ran with the pins %q, want the models fetch and the updater off", line)
		}
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), Save: saved.Path, MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("the take of the save = %+v, %v", taken, err)
	}
	if _, err := io.Copy(io.Discard, taken.Body); err != nil {
		t.Errorf("reading the take of the save: %v", err)
	}
	_ = taken.Body.Close()
	if taken.Archive.Path != saved.Path {
		t.Errorf("the take read %s, want the save's own %s", taken.Archive.Path, saved.Path)
	}
}

// AN OPENCODE SAVE SAYS "NOTHING" ONLY ON OPENCODE'S OWN WORD: a session the
// box's OpenCode answers it does not hold is nothing to save, and a probe that
// could not say (its database locked) fails the save with what OpenCode said;
// neither exports.
//
// Mutation: read every failed probe as nothing to save, and the second case
// goes red.
func TestAnOpenCodeSaveSaysNothingOnlyOnOpenCodesOwnWord(t *testing.T) {
	t.Parallel()
	const answers = `held "${3#sessionID=}" || { echo "Error: Session not found: ${3#sessionID=}" >&2; exit 1; }`
	if !strings.Contains(fakeOpenCode, answers) {
		t.Fatal("the fake OpenCode no longer answers session.get as this case breaks it; follow it here")
	}
	for name, tc := range map[string]struct {
		edit func(t *testing.T, b sandbox.Sandbox, dir string)
		says string
	}{
		"OpenCode holds no such session": {edit: func(t *testing.T, _ sandbox.Sandbox, dir string) {
			if err := os.Remove(filepath.Join(dir, "sessions", ocSaveSession)); err != nil {
				t.Fatal(err)
			}
		}},
		"OpenCode could not be asked": {says: "database is locked", edit: func(t *testing.T, b sandbox.Sandbox,
			dir string) {
			broken := strings.Replace(fakeOpenCode, answers, `echo "Error: database is locked" >&2; exit 1`, 1)
			writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
				strings.Replace(broken, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b, dir := openCodeSaving(t, "2.3.2", "")
			tc.edit(t, b, dir)
			saved, err := runner.PackSave(t.Context(), b, takerA, "")
			switch {
			case tc.says == "" && (err != nil || saved != (sandbox.SessionArchive{})):
				t.Errorf("the save = %+v, %v; want nothing saved and no error", saved, err)
			case tc.says != "" && (err == nil || !strings.Contains(err.Error(), tc.says)):
				t.Errorf("the save = %+v, %v; want it failed with what OpenCode said, %q", saved, err, tc.says)
			}
			if exported(t, dir) {
				t.Error("the save exported a session its probe did not find")
			}
			if left := savesLeft(t, b, takerA); len(left) > 0 {
				t.Errorf("the save left %s", strings.Join(left, ", "))
			}
		})
	}
}

// AN OPENCODE EXPORT PAST WHAT THE SCRUB READS IS NEVER PACKED: a pack on
// demand, a take's or a save's, refuses an export one byte past the bound a
// kept session's scrub reads, or of none, before gzip, saying which, and moves
// nothing; an export of exactly the bound is packed, and so is a small export
// of a database far past it, since the bound is the export's alone (a take's,
// which no room holds).
//
// Mutation: bound the database rather than the export, admit the bound's next
// byte, drop the empty check or check after gzip, and a case goes red.
func TestAnOpenCodeExportPastWhatTheScrubReadsIsNeverPacked(t *testing.T) {
	t.Parallel()
	bound := int64(executor.HeldPlainMax)
	for name, tc := range map[string]struct {
		vars     string
		database int64
		says     string
		saveOnly bool
	}{
		"one byte past the bound": {vars: " FAKE_SESSION_EXPORT_BYTES=" + strconv.FormatInt(bound+1, 10),
			says: strconv.FormatInt(bound+1, 10) + " bytes, past the " + strconv.FormatInt(bound, 10)},
		"empty":                     {vars: " FAKE_SESSION_EXPORT_BYTES=0", says: "exported it empty"},
		"at the bound":              {vars: " FAKE_SESSION_EXPORT_BYTES=" + strconv.FormatInt(bound, 10)},
		"a database past the bound": {database: 2 * bound},
	} {
		for _, pack := range []string{"take", "save"} {
			if pack == "save" && tc.database > 0 {
				continue
			}
			t.Run(name+", a "+pack+"'s pack", func(t *testing.T) {
				t.Parallel()
				runner, b, _ := openCodeSaving(t, "2.3.2", tc.vars)
				if tc.database > 0 {
					sparse(t, openCodeDatabase(b), tc.database)
				}
				var err error
				if pack == "save" {
					_, err = runner.PackSave(t.Context(), b, takerA, "")
				} else {
					var body io.ReadCloser
					_, body, _, err = runner.PackCapture(t.Context(), b, true)
					if body != nil {
						_ = body.Close()
					}
				}
				switch {
				case tc.says == "" && err != nil:
					t.Errorf("the %s's pack failed: %v; want the export packed", pack, err)
				case tc.says != "" && (err == nil || !strings.Contains(err.Error(), tc.says)):
					t.Errorf("the %s's pack = %v; want it refused saying %q", pack, err, tc.says)
				}
				if tc.says != "" {
					if left := savesLeft(t, b, takerA); len(left) > 0 {
						t.Errorf("the refused export left a save: %s", strings.Join(left, ", "))
					}
					if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
						t.Errorf("the refused export left its staging: %s", strings.Join(left, ", "))
					}
				}
			})
		}
	}
}

// AN OPENCODE SAVE WHOSE EXPORT FAILS LEAVES NOTHING: an export that breaks off
// part-written fails the save with what OpenCode said, moves nothing over its
// place and leaves no staging, and the save before it stands byte for byte.
//
// Mutation: carry on past the export's status (`;` for `&&` after it), and the
// broken export lands as a save.
func TestAnOpenCodeSaveWhoseExportFailsLeavesNothing(t *testing.T) {
	t.Parallel()
	runner, b, dir := openCodeSaving(t, "2.3.2", "")
	before, err := runner.PackSave(t.Context(), b, takerA, "")
	if err != nil {
		t.Fatalf("the save before: %v", err)
	}
	last := boxFile(t, b, before.Path)
	digests := boxFile(t, b, paths(b).SessionDigests())
	if err := os.WriteFile(filepath.Join(dir, "export-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.PackSave(t.Context(), b, takerA, ""); err == nil ||
		!strings.Contains(err.Error(), "the export broke off") {
		t.Errorf("the save whose export broke off = %v, want it failed with what OpenCode said", err)
	}
	if left := savesLeft(t, b, takerA); len(left) != 1 || left[0] != before.Path {
		t.Errorf("the taker's saves are %s, want the one before alone, %s", strings.Join(left, ", "), before.Path)
	}
	if got := boxFile(t, b, before.Path); got != last {
		t.Error("the failed save changed the one before it")
	}
	if got := boxFile(t, b, paths(b).SessionDigests()); got != digests {
		t.Errorf("the failed save recorded a digest: %q, was %q", got, digests)
	}
	if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the failed save left its staging: %s", strings.Join(left, ", "))
	}
}

// AN OPENCODE SAVE OF A RELEASE NO BOX CONTINUES IS REFUSED UNPACKED: a record
// naming a release other than the one whose sessions continue in another box
// (one before it, one past it, none read) fails the save before its script
// runs, as one no box of this build continues, OpenCode asked nothing. The
// save's own gate, the releases a save beside a running server was measured on,
// is held at the harness (TestAnOpenCodeSaveOfAReleaseNoProbeMeasuredWritesNoScript).
//
// Mutation: drop the floor before the pack, and the save is refused as
// unmeasured instead.
func TestAnOpenCodeSaveOfAReleaseNoBoxContinuesIsRefusedUnpacked(t *testing.T) {
	t.Parallel()
	for _, release := range []string{"2.1.5", "2.4.0", ""} {
		t.Run("release "+strconv.Quote(release), func(t *testing.T) {
			t.Parallel()
			runner, b, dir := openCodeSaving(t, release, "")
			saved, err := runner.PackSave(t.Context(), b, takerA, "")
			var unrestorable *sandbox.UnrestorableError
			if !errors.As(err, &unrestorable) || unrestorable.Loss != executorkeep.LossReleaseFloor ||
				saved.Path != "" {
				t.Errorf("the save of OpenCode %q = %+v, %v; want it refused as no box continues it", release,
					saved, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "calls")); !os.IsNotExist(err) {
				t.Errorf("OpenCode was asked something for a save its release refuses: %s", readIn(t, dir, "calls"))
			}
		})
	}
}

// A TAKE OF AN OPENCODE SAVE COUNTS IT AND NAMES ITS NEWEST COMPACTION: the
// save's take meters its export as every take of an OpenCode session does, so
// its count names the export's newest compaction, and its archive states where
// the run began as the run's record says it (a session of its own).
//
// Mutation: build the meter only for a take that names no save.
func TestATakeOfAnOpenCodeSaveCountsItAndNamesItsNewestCompaction(t *testing.T) {
	t.Parallel()
	runner, b, dir := openCodeSaving(t, "2.3.2", "")
	doc, err := os.ReadFile(filepath.Join("testdata", "opencode", "2.3.2", "export.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "export-doc"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := runner.PackSave(t.Context(), b, takerA, "")
	if err != nil {
		t.Fatalf("PackSave: %v", err)
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), Save: saved.Path, MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("the take of the save = %+v, %v", taken, err)
	}
	if _, err := io.Copy(io.Discard, taken.Body); err != nil {
		t.Fatalf("reading the take: %v", err)
	}
	_ = taken.Body.Close()
	const newest = "msg_202bccee7001dpOFLBiNqIkIeY"
	if count := taken.Count(); count == nil || count.Mark != "" || count.Restart == nil || *count.Restart != newest {
		t.Errorf("the take of the save counts %+v, want the export counted, restarting at %s", count, newest)
	}
	if !taken.Archive.RunStart.Began {
		t.Errorf("the take of the save states the run's start %+v, want a session the run began, as its "+
			"record says", taken.Archive.RunStart)
	}
}

// AN OPENCODE SAVE WAITS FOR THE ROOM ITS EXPORT TAKES: where its box's room is
// read, a save whose need, twelve times OpenCode's database and its WAL plus
// 128 MiB, exceeds it fails wrapping [sandbox.ErrSaveNoRoom] before any OpenCode
// process, its probe's included, leaving nothing; a small database's is saved;
// a session held where no database is fails, saying so, unexported; and a
// take's pack on demand is never held to the room. Where the box reads no room
// (no /proc), nothing is refused for it.
//
// Mutation: drop the check, leave the WAL out of the need, probe first, export
// without a database, or apply it to the take's pack, and a case goes red.
func TestAnOpenCodeSaveWaitsForTheRoomItsExportTakes(t *testing.T) {
	t.Parallel()
	const huge = int64(1) << 40
	for name, tc := range map[string]struct {
		database, wal    int64
		take, noDatabase bool
	}{
		"a database past the room":         {database: huge},
		"a WAL past the room":              {wal: huge},
		"a small database":                 {},
		"no database where the session is": {noDatabase: true},
		"a take's pack of a huge database": {database: huge, take: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b, dir := openCodeSaving(t, "2.3.2", "")
			if tc.noDatabase {
				if err := os.Remove(openCodeDatabase(b)); err != nil {
					t.Fatal(err)
				}
				_, err := runner.PackSave(t.Context(), b, takerA, "")
				if err == nil || errors.Is(err, sandbox.ErrSaveNoRoom) || !strings.Contains(err.Error(), "database is not at") {
					t.Errorf("the save = %v; want it failed for the database it could not find", err)
				}
				if exported(t, dir) {
					t.Error("a save whose database was not found exported anyway")
				}
				return
			}
			if tc.database > 0 {
				sparse(t, openCodeDatabase(b), tc.database)
			}
			if tc.wal > 0 {
				sparse(t, openCodeDatabase(b)+"-wal", tc.wal)
			}
			if tc.take {
				_, body, _, err := runner.PackCapture(t.Context(), b, true)
				if err != nil || body == nil {
					t.Fatalf("the take's pack of a huge database = %v, %v; want it packed, held to no room",
						body, err)
				}
				_ = body.Close()
				return
			}
			res, err := b.Exec(t.Context(), sandbox.MemoryDomainScript+"\n"+executor.OpenCodeSaveRoom+
				"\ncrewlet_memory 2> /dev/null | crewlet_save_room", sandbox.ExecOptions{})
			if err != nil {
				t.Fatalf("reading the box's room: %v", err)
			}
			refused := tc.database+tc.wal > 0 && strings.TrimSpace(res.Stdout) != ""
			_, err = runner.PackSave(t.Context(), b, takerA, "")
			if !refused {
				if err != nil {
					t.Errorf("the save failed: %v; want it saved (the box's room reads %q)", err, res.Stdout)
				}
				return
			}
			if !errors.Is(err, sandbox.ErrSaveNoRoom) || !strings.Contains(err.Error(), "needs about") {
				t.Errorf("the save = %v; want it refused for the room its export needs", err)
			}
			if calls, err := os.ReadFile(filepath.Join(dir, "calls")); err == nil && len(calls) > 0 {
				t.Errorf("a save its box has no room for started OpenCode anyway: %q", calls)
			}
			if left := savesLeft(t, b, takerA); len(left) > 0 {
				t.Errorf("the refused save left %s", strings.Join(left, ", "))
			}
			if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
				t.Errorf("the refused save left its staging: %s", strings.Join(left, ", "))
			}
		})
	}
}

// A SAVE'S ROOM IS ITS DOMAIN'S LIMIT LESS WHAT THE KERNEL CANNOT RECLAIM,
// capped by the machine's MemAvailable; MemAvailable alone where no numeric
// limit and held are read (a limit of `max`, a VM's machine); and nothing where
// neither is, which refuses no save. The need, twelve times the database and
// its WAL plus 128 MiB, is admitted at the room and refused a byte short.
//
// Mutation: ignore held, take the larger of the two, read `max` as a number, or
// need eleven times or no base, and a case goes red.
func TestASavesRoomIsItsDomainsLimitLessWhatTheKernelCannotReclaim(t *testing.T) {
	t.Parallel()
	shell := func(t *testing.T, lines, call string) (string, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "sh", "-c", executor.OpenCodeSaveRoom+"\n"+call)
		cmd.Stdin = strings.NewReader(lines)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("running the room's shell: %v", err)
			}
			return strings.TrimSpace(stderr.String()), err
		}
		return strings.TrimSpace(string(out)), nil
	}
	for name, tc := range map[string]struct{ lines, room string }{
		"cgroup v2 under its limit": {lines: "oom_kill=0\nlimit=4718592000\nheld=1000000000\n" +
			"total_kb=32858020\navail_kb=30474420\n", room: "3718592000"},
		"cgroup v2, the machine tighter": {lines: "oom_kill=0\nlimit=4718592000\nheld=1000000000\n" +
			"total_kb=32858020\navail_kb=1000000\n", room: "1024000000"},
		"cgroup v2 at max": {lines: "oom_kill=0\nlimit=max\nheld=2990080\ntotal_kb=32858020\n" +
			"avail_kb=30474420\n", room: "31205806080"},
		"cgroup v1 unlimited": {lines: "oom_kill=0\nlimit=9223372036854771712\nheld=500000000\n" +
			"total_kb=8000000\navail_kb=3000000\n", room: "3072000000"},
		"held past its limit": {lines: "limit=1000\nheld=2000\navail_kb=5\n", room: "0"},
		"a limit and no held": {lines: "limit=1000\navail_kb=5\n", room: "5120"},
		"a VM's machine":      {lines: "oom_kill=3\ntotal_kb=8000000\navail_kb=5000\n", room: "5120000"},
		"nothing read":        {lines: "", room: ""},
	} {
		if got, err := shell(t, tc.lines, "crewlet_save_room"); err != nil || got != tc.room {
			t.Errorf("%s: the room is %q (%v), want %q", name, got, err, tc.room)
		}
	}

	const db, wal = 1 << 20, 1 << 19
	need := int64(12*(db+wal) + 128<<20)
	const held = 1000
	fits := func(room int64) (string, error) {
		return shell(t, "limit="+strconv.FormatInt(room+held, 10)+"\nheld="+strconv.Itoa(held)+
			"\navail_kb=1000000000\n", "crewlet_save_fits "+strconv.Itoa(db)+" "+strconv.Itoa(wal))
	}
	if said, err := fits(need); err != nil {
		t.Errorf("a need equal to its room was refused: %s", said)
	}
	if said, err := fits(need - 1); err == nil || !strings.Contains(said, strconv.FormatInt(need, 10)) {
		t.Errorf("a need a byte past its room = %v, %q; want it refused, saying the need %d", err, said, need)
	}
	if said, err := shell(t, "", "crewlet_save_fits "+strconv.FormatInt(int64(1)<<40, 10)+" 0"); err != nil {
		t.Errorf("a room not read refused a save: %s", said)
	}
}
