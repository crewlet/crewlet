package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// Saves, and every pack beside a working harness, made on a real box by the
// runner's own pack: the harness's writes are played by a writer each case
// controls, never raced against a clock.

// takerA and takerB are two takers of one box's saves, named as a lease
// holder's incarnation is, `<node>:<uuid>`.
const (
	takerA = "node-a:2a6c3f4e-0b1d-4c8e-9f7a-1b2c3d4e5f60"
	takerB = "node-b:7d8e9f0a-1b2c-4d3e-8f4a-5b6c7d8e9f01"
)

// forgetCapture removes the job's capture and the digests beside it, so what a
// pack's destination holds after is that pack's doing.
func forgetCapture(t *testing.T, box sandbox.Sandbox) {
	t.Helper()
	p := paths(box)
	if _, err := box.Exec(t.Context(), "rm -f "+shQuote(p.SessionCapture())+" "+shQuote(p.SessionDigests()),
		sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
}

// saveStagingLeft is every staging directory taker's saves left in box, found
// by pattern: each is named for its save (executor.Paths.SessionSaveStaging).
func saveStagingLeft(t *testing.T, box sandbox.Sandbox, taker string) []string {
	t.Helper()
	res, err := box.Exec(t.Context(), "for d in "+shQuote(paths(box).SessionSaveStaging(taker, ""))+"*; do "+
		`if [ -e "$d" ] || [ -L "$d" ]; then printf '%s\n' "$d"; fi; done`, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("asking the box for its saves' staging directories: %v", err)
	}
	return strings.FieldsFunc(res.Stdout, func(r rune) bool { return r == '\n' })
}

// savesLeft is every save of taker's left in box, found by pattern: each is
// named for its save (executor.Paths.SessionSave).
func savesLeft(t *testing.T, box sandbox.Sandbox, taker string) []string {
	t.Helper()
	res, err := box.Exec(t.Context(), "for d in "+shQuote(paths(box).SessionSave(taker, ""))+"*; do "+
		`if [ -e "$d" ] || [ -L "$d" ]; then printf '%s\n' "$d"; fi; done`, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("asking the box for its saves: %v", err)
	}
	return strings.FieldsFunc(res.Stdout, func(r rune) bool { return r == '\n' })
}

// hostTool is the path of this machine's own name, for a stand-in to hand
// calls on to.
func hostTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("no %s on this machine's PATH: %v", name, err)
	}
	return path
}

// rewritingTar is a stand-in `tar` for a box's bin directory, first on a pack's
// PATH: every call but a pack's (`tar -czf <archive> <files...>`) goes to this
// machine's tar, and a pack's runs pack with that tar as "$tar", the archive as
// "$1" and the files after it, where the pack runs tar (in the configuration
// directory beside a working harness, beside it once the harness is gone). So
// the case's writer changes the session at the moment the case names.
func rewritingTar(t *testing.T, pack string) string { return tarStandIn(t, pack, "") }

// tarStandIn is [rewritingTar] with others run first on every call: shell that
// reads the call's words ("$1" the first, "$tar" this machine's tar) and exits
// where it answers the call itself (a member's read back, say), or falls
// through.
func tarStandIn(t *testing.T, pack, others string) string {
	t.Helper()
	return "#!/bin/sh\ntar=" + shQuote(hostTool(t, "tar")) + "\n" + others + "\n" +
		`if [ "$1" != -czf ]; then exec "$tar" "$@"; fi` + "\nshift\n" + pack
}

// afterTar is a pack that runs this machine's tar and then change: a harness
// that writes once tar has read its files, and so before the pack's checks.
func afterTar(change string) string {
	return `"$tar" -czf "$@"; st=$?` + "\n" + change + "\nexit $st\n"
}

// grownBack rewrites file IN PLACE (the same inode) to its first "$h" bytes and
// on past its old size, as Claude Code withdrawing a message rewrites a
// transcript.
func grownBack(file string) string {
	f := shQuote(file)
	return `cp ` + f + ` ` + f + `.was && { head -c "$h" ` + f + `.was; printf '%s\n' ` +
		`'{"type":"user","grown":"back past its old size"}' '{"type":"user","after":"it shrank"}'; } > ` + f +
		` && rm -f ` + f + `.was`
}

// neverNewerFind is a stand-in `find` to which no file is ever newer than
// another, as in a box whose clock never moves; every other call goes to this
// machine's find.
func neverNewerFind(t *testing.T) string {
	t.Helper()
	return "#!/bin/sh\nfor a; do [ \"$a\" != -newer ] || exit 0; done\nexec " + shQuote(hostTool(t, "find")) +
		` "$@"` + "\n"
}

// coarseClock puts a stand-in `find` and `sleep` in b's bin directory that play
// a find comparing whole seconds, as busybox's does (on BusyBox 1.37.0 a file
// written 200 ms after the reference is not newer): NO FILE IS NEWER than
// another until the box's sleep has slept, and then find compares as this
// machine's does. So a case decides which writes the grain hides (every one
// before the pack's first sleep) rather than racing the wall clock's second.
func coarseClock(t *testing.T, b sandbox.Sandbox) {
	t.Helper()
	slept := shQuote(b.Home() + "/coarse-clock-slept")
	writeBoxStandIn(t, b, paths(b).BinDir()+"/find", "#!/bin/sh\nif [ ! -e "+slept+" ]; then "+
		`for a; do [ "$a" != -newer ] || exit 0; done; fi`+"\nexec "+shQuote(hostTool(t, "find"))+` "$@"`+"\n")
	writeBoxStandIn(t, b, paths(b).BinDir()+"/sleep", "#!/bin/sh\n: > "+slept+"\nexec "+
		shQuote(hostTool(t, "sleep"))+` "$@"`+"\n")
}

// sessionFiles are the files of session s a case changes under a pack, named
// as the pack names them, relative to the harness's configuration directory.
type sessionFiles struct{ transcript, thread, task, spilled string }

func filesOf(s string) sessionFiles {
	project := "projects/" + executor.ClaudeProjectDir + "/"
	return sessionFiles{transcript: project + s + ".jsonl", thread: project + s + "/subagents/agent-1.jsonl",
		task: "tasks/" + s + "/1.json", spilled: project + s + "/tool-results/out.txt"}
}

// A SAVE IS PACKED TO ITS TAKER'S OWN PATH, AND NEVER TAKEN AS THE JOB'S FINAL
// ARCHIVE. A box whose job's exit pack never landed is saved by two takers:
// each save stages in a directory of its own, leaving a pack on demand's and
// another taker's alone, and moves to a place named by a digest of its taker,
// its digest kept in the box. A take of a save reads that save alone, packs
// nothing, adds no digest and removes it once read; a save gone is taken as
// nothing, a path that is no save is refused unread. The job's final archive
// is then still none, so the final take packs the job's own at its own place.
//
// Mutation: move a save over the capture, name one path for every taker, share
// a staging directory, or have a take pack (anywhere) or read any path but the
// save's, and a check goes red.
func TestASaveIsPackedToItsTakersOwnPathAndNeverTakenAsTheJobsFinal(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	forgetCapture(t, b)
	// WHAT A PACK ON DEMAND AND ANOTHER TAKER'S SAVE ARE STAGING as this one
	// packs.
	elsewhere := []string{p.SessionPackStaging("another-pack") + "/capture",
		p.SessionSaveStaging(takerB, "another-save") + "/capture"}
	for _, staged := range elsewhere {
		if err := b.WriteFile(t.Context(), staged, []byte("another pack's archive, half made")); err != nil {
			t.Fatal(err)
		}
	}

	saved, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("PackSave: %v", err)
	}
	if !strings.HasPrefix(saved.Path, p.SessionSave(takerA, "")) || saved.SessionID != ran.SessionID ||
		saved.Harness != executor.ClaudeCodeName || saved.Format != executor.ClaudeArchiveFormat ||
		saved.Version != "2.1.310" || saved.Size <= 0 {
		t.Errorf("the save is described as %+v, want session %s of Claude Code 2.1.310 at a place of its own "+
			"named after its taker, %s…", saved, ran.SessionID, p.SessionSave(takerA, ""))
	}
	if strings.Contains(saved.Path, "node-a") {
		t.Errorf("the save's path names its taker's holder rather than a digest of it: %s", saved.Path)
	}
	if boxHas(t, b, p.SessionCapture()) {
		t.Error("the save was moved over the job's capture, which a collection takes as the job's final archive")
	}
	if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the save left its staging directory behind: %s", strings.Join(left, ", "))
	}
	for _, staged := range elsewhere {
		if !boxHas(t, b, staged) {
			t.Errorf("the save removed %s, which another pack was staging", staged)
		}
	}
	first := []byte(boxFile(t, b, saved.Path))
	digests := boxFile(t, b, p.SessionDigests())
	if !strings.Contains(digests, ran.SessionID+" "+digestOf(t, first)+"\n") {
		t.Errorf("the box keeps no digest of the save it packed: %q", digests)
	}

	// ANOTHER TAKER'S SAVE of the same box is a file of its own.
	other, err := runner.PackSave(t.Context(), b, takerB)
	if err != nil {
		t.Fatalf("PackSave for the second taker: %v", err)
	}
	if other.Path == saved.Path || !strings.HasPrefix(other.Path, p.SessionSave(takerB, "")) {
		t.Errorf("the second taker's save is at %s, want a place of its own after its taker, %s…, apart from %s",
			other.Path, p.SessionSave(takerB, ""), saved.Path)
	}
	if again := []byte(boxFile(t, b, saved.Path)); !bytes.Equal(again, first) {
		t.Error("the second taker's save changed the first taker's")
	}

	// A TAKE OF A SAVE reads that save alone, packs nothing and removes it
	// once read: no capture of the job's appears, and no digest is added.
	digests = boxFile(t, b, p.SessionDigests())
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: saved.Path, MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("the take of the first taker's save = %+v, %v", taken, err)
	}
	members := archiveMembers(t, taken.Body)
	_ = taken.Body.Close()
	if taken.Archive.Path != saved.Path {
		t.Errorf("the take of a save read %s, want that save's own %s", taken.Archive.Path, saved.Path)
	}
	if _, ok := members[filesOf(ran.SessionID).transcript]; !ok {
		t.Errorf("the save holds no transcript: %v", members)
	}
	if boxHas(t, b, saved.Path) {
		t.Errorf("the take of a save left it at %s once read", saved.Path)
	}
	if !boxHas(t, b, other.Path) {
		t.Errorf("the take of one save removed another, at %s", other.Path)
	}
	unsaved := p.SessionSave("node-c:0f1e2d3c-4b5a-4968-8776-655443322110", "0f1e2d3c-4b5a-4968-8776-655443322110")
	none, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: unsaved, MaxBytes: takeBound})
	if err != nil || none.Body != nil {
		t.Errorf("the take of a save no taker made = %+v, %v; want nothing", none, err)
	}
	if notSave, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: p.SessionRecord(),
		MaxBytes: takeBound}); err == nil {
		if notSave.Body != nil {
			_ = notSave.Body.Close()
		}
		t.Errorf("a take of a save named by a path that is no save's read %s", notSave.Archive.Path)
	}
	if boxHas(t, b, p.SessionCapture()) {
		t.Error("a take of a save packed the job's capture")
	}
	if got := boxFile(t, b, p.SessionDigests()); got != digests {
		t.Errorf("a take of a save added a digest, so it packed: %q, was %q", got, digests)
	}
	if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: other.Path, Pack: true,
		MaxBytes: takeBound}); err == nil {
		t.Error("a take of a save that asks to pack was made")
	}
	if _, err := runner.PackSave(t.Context(), b, ""); err == nil {
		t.Error("a save naming no taker was packed")
	}

	// THE JOB'S FINAL ARCHIVE is none of them: a collection's read finds none,
	// and the final take packs the job's own.
	if described, body, err := runner.OpenCapture(t.Context(), b); err != nil || body != nil {
		if body != nil {
			_ = body.Close()
		}
		t.Fatalf("the read of the job's final archive found %+v, %v: a save read as the job's last", described, err)
	}
	final, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{MaxBytes: takeBound})
	if err != nil || final.Body == nil {
		t.Fatalf("the final take = %+v, %v", final, err)
	}
	_ = final.Body.Close()
	if !strings.HasPrefix(final.Archive.Path, p.WorkDir()+"/session-pack-staging.") || boxHas(t, b, p.SessionCapture()) {
		t.Errorf("the final take read %s, want the job's own session packed at its pack's own place, and nothing "+
			"over the job's capture", final.Archive.Path)
	}
}

// A SAVE OF A FILE REWRITTEN WHILE IT WAS PACKED FAILS, AND SAYS WHICH CHECK
// REFUSED IT, as does every pack beside a harness that may still work
// (heldPacks). Each case, named for it, is a write the harness makes as tar
// reads the session, made at that moment by the case's own writer. Every one
// is refused whatever tar's exit said, naming its check and its file, and
// nothing is moved to the destination, recorded as a digest or left staged.
//
// Mutation: take tar's exit on its word, drop a check, run the last look for a
// removed member first, read a failed extraction's bytes or an empty listing as
// an archive, move the archive before the checks, leave staging behind, read
// back the session's own transcript alone, drop the coarse find's clock wait,
// or hold a working job's take to the run's own end's rule: a case goes red.
func TestASaveOfAMemberRewrittenWhileItWasPackedFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		pack  func(f sessionFiles) string
		check func(f sessionFiles) string
		// readBack, where set, answers the calls tar's stand-in hands it
		// before a pack's ([tarStandIn]).
		readBack string
		// coarse runs the pack under a find of a coarse grain ([coarseClock]).
		coarse bool
		// find, where set, is the script of a stand-in `find` for the case's
		// box: a writer at a moment no tar's stand-in reaches.
		find func(t *testing.T, f sessionFiles) string
	}{
		{name: "its transcript replaced, as a trim renames a new file over it",
			pack: func(f sessionFiles) string {
				tr := shQuote(f.transcript)
				return afterTar(`cp ` + tr + ` ` + tr + `.new && printf '%s\n' '{"type":"user","trimmed":true}' >> ` +
					tr + `.new && mv -f ` + tr + `.new ` + tr)
			},
			check: func(f sessionFiles) string {
				return "a file was replaced by another while the session was packed: " + f.transcript
			}},
		{name: "a subagent's thread cut in place",
			pack: func(f sessionFiles) string {
				a := shQuote(f.thread)
				return afterTar(`head -c 4 ` + a + ` > ` + a + `.cut && cat ` + a + `.cut > ` + a + ` && rm -f ` + a +
					`.cut`)
			},
			check: func(f sessionFiles) string { return "a file shrank while the session was packed: " + f.thread }},
		{name: "a task file touched",
			pack: func(f sessionFiles) string { return afterTar(`touch ` + shQuote(f.task)) },
			check: func(f sessionFiles) string {
				return "a file was written while the session was packed: " + f.task
			}},
		{name: "a task file touched, to a find of a coarse grain as busybox's is",
			pack: func(f sessionFiles) string { return afterTar(`touch ` + shQuote(f.task)) },
			check: func(f sessionFiles) string {
				return "a file was written while the session was packed: " + f.task
			},
			coarse: true},
		{name: "its transcript rewritten in place and grown back, as a withdrawn message rewrites it",
			pack: func(f sessionFiles) string {
				tr := shQuote(f.transcript)
				return afterTar(`cp ` + tr + ` ` + tr + `.was && { printf '%s\n' ` +
					`'{"type":"user","withdrawn":"and written again"}'; cat ` + tr + `.was; } > ` + tr + ` && rm -f ` +
					tr + `.was`)
			},
			check: rewrittenInPlace},
		{name: "a subagent's thread rewritten in place once tar read it, no start of the live file",
			pack: func(f sessionFiles) string {
				a := shQuote(f.thread)
				return afterTar(`cp ` + a + ` ` + a + `.was && { printf '%s\n' ` +
					`'{"type":"user","withdrawn":"and written again"}'; cat ` + a + `.was; } > ` + a + ` && rm -f ` +
					a + `.was`)
			},
			check: func(f sessionFiles) string {
				return "a transcript was rewritten in place while the session was packed, so what the archive " +
					"holds of it is not the start of the file: " + f.thread
			}},
		{name: "its transcript shrunk and grown back as tar read it, as GNU tar packs that",
			pack: func(f sessionFiles) string {
				tr := shQuote(f.transcript)
				// GNU'S MEMBER: what tar read before the file shrank, padded with
				// NULs to the size it first saw, packed from a copy of the files
				// as they were — while the live transcript is cut and grown back.
				return `archive=$1; shift; n=$(wc -c < ` + tr + `); h=$((n / 2)); shadow="$archive.shadow"` + "\n" +
					`for f; do mkdir -p "$shadow/$(dirname "$f")" && cp -R "$f" "$shadow/$f" || exit 2; done` + "\n" +
					`{ head -c "$h" ` + tr + `; head -c "$((n - h))" /dev/zero; } > "$shadow/"` + tr + "\n" +
					grownBack(f.transcript) + "\n" +
					`( cd "$shadow" && "$tar" -czf "$archive" "$@" ) || exit 2` + "\n" +
					`rm -rf "$shadow"; echo "tar: ` + f.transcript + `: File shrank by $((n - h)) bytes; padding ` +
					`with zeros" >&2; exit 1` + "\n"
			},
			check: rewrittenInPlace},
		{name: "its transcript shrunk and grown back as tar read it, as busybox's tar packs that",
			pack: func(f sessionFiles) string {
				tr := shQuote(f.transcript)
				// BUSYBOX'S ARCHIVE: a whole gzip whose tar is cut part way into
				// the shrinking member (the transcript, first, behind its 512-byte
				// header). Listed through gunzip, as the pack lists it, the cut is
				// an error to busybox's tar ("short read") and to GNU's, which
				// lists it here ("Unexpected EOF in archive").
				return `archive=$1; shift; n=$(wc -c < ` + tr + `); h=$((n / 2))` + "\n" +
					`"$tar" -cf - "$@" | head -c "$((512 + h))" | gzip > "$archive"` + "\n" +
					grownBack(f.transcript) + "\n" +
					`echo 'tar: short read' >&2; exit 1` + "\n"
			},
			check: func(sessionFiles) string { return "the archive the pack made could not be read back" }},
		{name: "a transcript grown as tar read it whose read back fails part way, past a listing that held",
			pack: func(f sessionFiles) string {
				return afterTar(`printf '%s\n' '{"type":"user","grew":"after tar read it"}' >> ` + shQuote(f.transcript))
			},
			// THE READ BACK YIELDS THE MEMBER'S FIRST BYTES AND FAILS, as an
			// extraction that dies in the archive does: those bytes are the
			// start of the live file too, so only the failure says the member
			// was not read whole.
			readBack: `if [ "$1" = -xzOf ]; then "$tar" -xzOf "$2" "$3" | head -c 16; exit 2; fi`,
			check: func(f sessionFiles) string {
				return "a transcript could not be read back out of the archive the pack made: " + f.transcript
			}},
		{name: "a file tar could not read, left out of an archive that still reads back, as busybox's tar leaves " +
			"one with exit 1",
			pack: func(f sessionFiles) string {
				// THE ARCHIVE WHOLE BUT FOR THE TASK LIST, the pack's last
				// operand, which this tar could not open.
				return `archive=$1; shift; "$tar" -czf "$archive" "$1" "$2" || exit 2` + "\n" +
					`echo 'tar: ` + f.task + `: Permission denied' >&2; exit 1` + "\n"
			},
			check: func(f sessionFiles) string { return "a file is not in the archive the pack made: " + f.task }},
		{name: "an archive that holds no file, as tar leaves one over files it could not read",
			pack: func(sessionFiles) string {
				return `"$tar" -czf "$1" -T /dev/null || exit 2` + "\n" +
					`echo 'tar: projects: Permission denied' >&2; exit 1` + "\n"
			},
			check: func(sessionFiles) string { return "the archive the pack made holds no file" }},
		{name: "a spilled result removed, as the harness's own sweep removes one",
			pack: func(f sessionFiles) string { return afterTar(`rm -f ` + shQuote(f.spilled)) },
			check: func(f sessionFiles) string {
				return "a file was removed while the session was packed: " + f.spilled
			}},
		{name: "a file made and removed as tar read the session, which no manifest listed",
			pack: func(f sessionFiles) string {
				made := shQuote(madeAndRemoved(f))
				return `echo spilling > ` + made + "\n" + afterTar(`rm -f `+made)
			},
			check: func(f sessionFiles) string {
				return "a file was removed while the session was packed: " + madeAndRemoved(f)
			}},
		{name: "a task file written under the tar and removed before what was written was looked for",
			pack: func(f sessionFiles) string { return afterTar(`touch ` + shQuote(f.task)) },
			// THE WRITER AT THE CHECK OF WHAT WAS WRITTEN: the task file goes
			// just before the one find asking for non-transcripts (`! -name`),
			// never the walk before the marker (`! -type`).
			find: func(t *testing.T, f sessionFiles) string {
				return "#!/bin/sh\ncase \" $* \" in *\" ! -name \"*) rm -f " + shQuote(f.task) + ";; esac\nexec " +
					shQuote(hostTool(t, "find")) + ` "$@"` + "\n"
			},
			check: func(f sessionFiles) string {
				return "a file was removed while the session was packed: " + f.task
			}},
	}
	for _, tc := range cases {
		for _, pack := range heldPacks {
			t.Run(pack.name+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				runner, b := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				p := paths(b)
				forgetCapture(t, b)
				files := filesOf(ran.SessionID)
				writeBoxStandIn(t, b, p.BinDir()+"/tar", tarStandIn(t, tc.pack(files), tc.readBack))
				if tc.coarse {
					coarseClock(t, b)
				}
				if tc.find != nil {
					writeBoxStandIn(t, b, p.BinDir()+"/find", tc.find(t, files))
				}
				err := pack.make(t, runner, b)
				if want := tc.check(files); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("the pack answered %v, want it refused saying %q", err, want)
				}
				if left := pack.dest(t, b); len(left) > 0 {
					t.Errorf("the refused pack left an archive at %s", strings.Join(left, ", "))
				}
				if left := pack.staged(t, b); len(left) > 0 {
					t.Errorf("the refused pack left its staging directory: %s", strings.Join(left, ", "))
				}
				if digests := boxFile(t, b, p.SessionDigests()); digests != "" {
					t.Errorf("the refused pack recorded a digest the box holds: %q", digests)
				}
			})
		}
	}
}

// rewrittenInPlace is the check a transcript rewritten under a pack is refused
// by: what the archive holds of it is not the start of the live file.
func rewrittenInPlace(f sessionFiles) string {
	return "a transcript was rewritten in place while the session was packed, so what the archive holds of " +
		"it is not the start of the file: " + f.transcript
}

// madeAndRemoved is a spilled result the harness makes once the pack has
// listed the session, and removes once tar has read it.
func madeAndRemoved(f sessionFiles) string {
	return strings.TrimSuffix(f.spilled, "out.txt") + "made.txt"
}

// heldPacks are the packs made beside a harness that may still work: a save,
// and a take that packs on demand (sandbox.SessionTake.Pack), each with what it
// left where it moves its archive and where it stages it.
var heldPacks = []struct {
	name   string
	make   func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error
	dest   func(t *testing.T, b sandbox.Sandbox) []string
	staged func(t *testing.T, b sandbox.Sandbox) []string
}{
	{name: "a save",
		make: func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error {
			_, err := runner.PackSave(t.Context(), b, takerA)
			return err
		},
		dest:   func(t *testing.T, b sandbox.Sandbox) []string { return savesLeft(t, b, takerA) },
		staged: func(t *testing.T, b sandbox.Sandbox) []string { return saveStagingLeft(t, b, takerA) }},
	{name: "a take of a job still working",
		make: func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error {
			taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, MaxBytes: takeBound})
			if taken.Body != nil {
				_ = taken.Body.Close()
			}
			return err
		},
		dest: func(t *testing.T, b sandbox.Sandbox) []string {
			if capture := paths(b).SessionCapture(); boxHas(t, b, capture) {
				return []string{capture}
			}
			return nil
		},
		staged: packStagingLeft},
}

// A PACK BESIDE A WORKING HARNESS PROVES THE SESSION'S OWN TRANSCRIPT WHATEVER
// ITS FIND READS OF IT. Claude Code withdraws a message by rewriting the
// transcript in the same inode (2.1.310's removeTranscriptMessage), and a clock
// stepped back mid-pack stamps that rewrite before the marker, so find reads it
// as unwritten. Here it is rewritten and grown back once tar read it, under a
// find that never lists it as newer: every held pack still reads it back, finds
// it is not the start of the live file, and is refused saying so, leaving no
// archive, digest or staging directory.
//
// Mutation: read the session's own transcript back only where find reads it as
// written since the marker, as a subagent's thread is, and it is packed torn.
func TestAPackProvesTheSessionsOwnTranscriptWhateverItsFindReads(t *testing.T) {
	t.Parallel()
	for _, pack := range heldPacks {
		t.Run(pack.name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, "2.1.310")
			_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			p := paths(b)
			forgetCapture(t, b)
			f := filesOf(ran.SessionID)
			tr := shQuote(f.transcript)
			// A FIND THAT NEVER LISTS THE TRANSCRIPT AS NEWER, as under a clock
			// stepped back mid-pack; every other call is this machine's find.
			find := shQuote(hostTool(t, "find"))
			writeBoxStandIn(t, b, p.BinDir()+"/find", "#!/bin/sh\ncase \" $* \" in *\" -newer \"*) "+find+
				` "$@" | grep -vxF `+tr+"; exit 0;; esac\nexec "+find+` "$@"`+"\n")
			writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, afterTar(`cp `+tr+` `+tr+`.was && { printf `+
				`'%s\n' '{"type":"user","withdrawn":"and written again"}'; cat `+tr+`.was; } > `+tr+` && rm -f `+
				tr+`.was`)))
			err := pack.make(t, runner, b)
			if want := rewrittenInPlace(f); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("the pack answered %v, want it refused saying %q", err, want)
			}
			if left := pack.dest(t, b); len(left) > 0 {
				t.Errorf("the refused pack left an archive at %s", strings.Join(left, ", "))
			}
			if left := pack.staged(t, b); len(left) > 0 {
				t.Errorf("the refused pack left its staging directory: %s", strings.Join(left, ", "))
			}
			if digests := boxFile(t, b, p.SessionDigests()); digests != "" {
				t.Errorf("the refused pack recorded a digest the box holds: %q", digests)
			}
		})
	}
}

// A HELD PACK WHOSE CHECKS CANNOT BE MADE IS REFUSED, AND SAYS WHICH. Each
// check reads a list kept in the staging directory or compares two with grep,
// and one that cannot be made is a refusal, never a pass; none takes a tool's
// word that a write landed: the shell writes each list or answer from a pipe
// and reads it back. Each case, named for it, removes a list, fails a find or a
// grep, or loses or halves a write as a full disk does (halved by a cksum that
// reads one line); each is refused, for both held packs, saying what could not
// be done, and leaves no archive, digest or staging directory.
//
// Mutation: read grep's exit 2, or its exit 0 naming nothing, as no
// difference; take a list as written unread, or read back only that it holds
// something; or leave any find's exit or a read back unchecked; a case goes red.
func TestAHeldPackWhoseChecksCannotBeMadeIsRefused(t *testing.T) {
	t.Parallel()
	// grew appends to a subagent's thread once tar has read the session, so the
	// pack has a transcript besides its own to read back.
	grew := func(f sessionFiles) string {
		return `printf '%s\n' '{"type":"user","more":1}' >> ` + shQuote(f.thread)
	}
	cases := []struct {
		name string
		// tar, where set, is what tar's stand-in does once it has packed
		// ([afterTar]), "$1" the archive in the pack's staging directory, the
		// session's files named as the pack names them; readBack answers the
		// calls it hands others first ([tarStandIn]).
		tar      func(f sessionFiles) string
		readBack string
		// find, grep and cksum, where set, are the scripts of stand-ins for the
		// box, whose home is home.
		find, grep, cksum func(t *testing.T, home string) string
		check             string
	}{
		{name: "the files it listed and its marker removed under the tar",
			tar: func(sessionFiles) string {
				return `d=$(dirname "$1"); rm -f "$d/files" "$d/inodes" "$d/sizes" "$d/marker"`
			},
			check: "the pack could not read back its list of the session's files"},
		{name: "the inodes it listed removed under the tar",
			tar:   func(sessionFiles) string { return `rm -f "$(dirname "$1")/inodes"` },
			check: "the pack could not compare the files' inodes before and after tar"},
		{name: "its marker removed under the tar",
			tar:   func(sessionFiles) string { return `rm -f "$(dirname "$1")/marker"` },
			check: "the pack could not list the files written since its marker"},
		{name: "a find that could not walk the whole session as it listed its files",
			// THE MANIFEST'S FIND, the one whose words end in -type f.
			find:  func(t *testing.T, _ string) string { return faultyFind(t, `*" -type f "`) },
			check: "the files of the session could not be listed"},
		{name: "a find that could not walk the whole session as it listed their inodes",
			find:  func(t *testing.T, home string) string { return faultyFindFirst(t, home, `*" -exec ls -i "*`) },
			check: "the files of the session could not be listed"},
		{name: "a find that could not walk the whole session as it listed their sizes",
			find:  func(t *testing.T, home string) string { return faultyFindFirst(t, home, `*" -exec wc -c "*`) },
			check: "the files of the session could not be listed"},
		{name: "a find that could not walk the whole session as it listed their inodes again after the tar",
			find:  func(t *testing.T, home string) string { return faultyFindAgain(t, home, `*" -exec ls -i "*`) },
			check: "the files of the session could not be listed again"},
		{name: "a find that could not walk the whole session as it listed their sizes again after the tar",
			find:  func(t *testing.T, home string) string { return faultyFindAgain(t, home, `*" -exec wc -c "*`) },
			check: "the files of the session could not be listed again"},
		{name: "a find that could not walk the whole session as it listed the transcripts it reads back",
			// THE TRANSCRIPTS' FIND, the one asking for what is named as one
			// (`-name`), never the look for what was written (`! -name`).
			find:  func(t *testing.T, _ string) string { return faultyFind(t, `*" -type f -name "*`) },
			check: "the pack could not list the transcripts it reads back"},
		{name: "what its archive holds removed as a transcript is read back",
			readBack: `if [ "$1" = -xzOf ]; then rm -f "$(dirname "$2")/members"; exec "$tar" "$@"; fi`,
			check:    "the pack could not read back the list of what its archive holds"},
		{name: "the sizes it listed before and after tar compared by a grep that could not answer",
			grep:  func(t *testing.T, _ string) string { return faultyGrep(t, "sizes.after", "sizes", "exit 2") },
			check: "the pack could not compare the files' sizes before and after tar"},
		{name: "the files it listed and what its archive holds compared by a grep that could not answer",
			grep:  func(t *testing.T, _ string) string { return faultyGrep(t, "members", "files", "exit 2") },
			check: "the pack could not compare the files it listed with what its archive holds"},
		{name: "what was written and what its archive holds compared by a grep that could not answer",
			grep:  func(t *testing.T, _ string) string { return faultyGrep(t, "members", "written", "exit 2") },
			check: "the pack could not compare the files written since its marker with what its archive holds"},
		{name: "the transcripts it reads back and what its archive holds compared by a grep that could not answer",
			tar:   grew,
			grep:  func(t *testing.T, _ string) string { return faultyGrep(t, "transcripts", "members", "exit 2") },
			check: "the pack could not compare the transcripts it reads back with what its archive holds"},
		{name: "the transcripts it reads back and what its archive holds compared by a grep that found a " +
			"difference and named none",
			tar:   grew,
			grep:  func(t *testing.T, _ string) string { return faultyGrep(t, "transcripts", "members", "exit 0") },
			check: "the pack could not compare the transcripts it reads back with what its archive holds"},
		{name: "what was written since its marker lost as the shell wrote its list, beside a task file touched",
			tar: func(f sessionFiles) string {
				return `touch ` + shQuote(f.task) + ` && ln -s /dev/null "$(dirname "$1")/written"`
			},
			check: "the pack could not list the files written since its marker"},
		{name: "the sizes found differing lost as the shell wrote them, beside a subagent's thread cut",
			tar: func(f sessionFiles) string {
				a := shQuote(f.thread)
				return `head -c 4 ` + a + ` > ` + a + `.cut && cat ` + a + `.cut > ` + a + ` && rm -f ` + a +
					`.cut && ln -s /dev/null "$(dirname "$1")/resized"`
			},
			check: "the pack could not compare the files' sizes before and after tar"},
		{name: "a list read back holding half of what was written to it",
			cksum: func(t *testing.T, _ string) string {
				cksum := shQuote(hostTool(t, "cksum"))
				return "#!/bin/sh\nif [ -f /dev/stdin ]; then " + shQuote(hostTool(t, "head")) + " -n 1 | " + cksum +
					"; exit; fi\nexec " + cksum + ` "$@"` + "\n"
			},
			check: "the files of the session could not be listed"},
	}
	for _, tc := range cases {
		for _, pack := range heldPacks {
			t.Run(pack.name+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				runner, b := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				p := paths(b)
				forgetCapture(t, b)
				packed := `exec "$tar" -czf "$@"` + "\n"
				if tc.tar != nil {
					packed = afterTar(tc.tar(filesOf(ran.SessionID)))
				}
				writeBoxStandIn(t, b, p.BinDir()+"/tar", tarStandIn(t, packed, tc.readBack))
				for tool, script := range map[string]func(t *testing.T, home string) string{
					"find": tc.find, "grep": tc.grep, "cksum": tc.cksum} {
					if script != nil {
						writeBoxStandIn(t, b, p.BinDir()+"/"+tool, script(t, b.Home()))
					}
				}
				err := pack.make(t, runner, b)
				if err == nil || !strings.Contains(err.Error(), tc.check) {
					t.Fatalf("the pack answered %v, want it refused saying %q", err, tc.check)
				}
				if left := pack.dest(t, b); len(left) > 0 {
					t.Errorf("the refused pack left an archive at %s", strings.Join(left, ", "))
				}
				if left := pack.staged(t, b); len(left) > 0 {
					t.Errorf("the refused pack left its staging directory: %s", strings.Join(left, ", "))
				}
				if digests := boxFile(t, b, p.SessionDigests()); digests != "" {
					t.Errorf("the refused pack recorded a digest the box holds: %q", digests)
				}
			})
		}
	}
}

// faultyFind is a stand-in `find` that, on a call whose words match pattern (a
// case pattern over them, padded with a space each side), walks as this
// machine's find and exits 1, as a find that met an error mid-walk; every
// other call goes to this machine's find.
func faultyFind(t *testing.T, pattern string) string {
	t.Helper()
	find := shQuote(hostTool(t, "find"))
	return "#!/bin/sh\ncase \" $* \" in " + pattern + ") " + find + ` "$@"; exit 1;; esac` + "\nexec " + find +
		` "$@"` + "\n"
}

// faultyFindFirst is [faultyFind] on the FIRST matching call alone, recorded in
// a file under home (the box's), so a pack that finds before and after its tar
// fails before only. Failing both would let a pack that took the first find's
// exit on its word still be refused, since "could not be listed again" holds
// the words of the refusal before the tar.
func faultyFindFirst(t *testing.T, home, pattern string) string {
	t.Helper()
	find := shQuote(hostTool(t, "find"))
	failed := filepath.Join(home, "find-failed-once")
	return "#!/bin/sh\ncase \" $* \" in " + pattern + ") if " + claim(failed) + "; then " + find +
		` "$@"; exit 1; fi;; esac` + "\nexec " + find + ` "$@"` + "\n"
}

// faultyFindAgain is [faultyFind] on the SECOND matching call: the first walks
// as usual, recorded in a file under home (the box's), so a pack that finds
// before and after its tar fails after.
func faultyFindAgain(t *testing.T, home, pattern string) string {
	t.Helper()
	find := shQuote(hostTool(t, "find"))
	seen := shQuote(filepath.Join(home, "find-matched-once"))
	return "#!/bin/sh\ncase \" $* \" in " + pattern + ") if [ -e " + seen + " ]; then " + find +
		` "$@"; exit 1; fi; : > ` + seen + ";; esac\nexec " + find + ` "$@"` + "\n"
}

// faultyGrep is a stand-in `grep` that, on the one comparison of the pack's
// lists patterns and input (`-f <staging>/<patterns> <staging>/<input>`), runs
// answer instead (exit 2, say, as a grep that could not read or write); every
// other call goes to this machine's grep.
func faultyGrep(t *testing.T, patterns, input, answer string) string {
	t.Helper()
	return "#!/bin/sh\ncase \" $* \" in *\"/" + patterns + " \"*\"/" + input + " \"*) " + answer + ";; esac\nexec " +
		shQuote(hostTool(t, "grep")) + ` "$@"` + "\n"
}

// stampedEntry is a file or directory of a session a case stamps, named
// relative to the configuration directory as the pack names it, with a file's
// body and whether it is stamped an hour AHEAD of the box's clock or behind it.
type stampedEntry struct {
	name, body string
	dir, ahead bool
}

// stamp is the time e is stamped with, against the box's now.
func (e stampedEntry) stamp(now time.Time) time.Time {
	if e.ahead {
		return now.Add(time.Hour)
	}
	return now.Add(-time.Hour)
}

// stampedSession is session s as a box whose clock ran an hour ahead left it:
// its transcript, a subagent's thread and metadata, a spilled result, its task
// list and their directories, the newest stamped in this box's future and the
// oldest in its past, every line whole.
func stampedSession(s string) []stampedEntry {
	project := "projects/" + executor.ClaudeProjectDir + "/"
	return []stampedEntry{
		{name: project + s + ".jsonl", body: `{"type":"user","sessionId":"` + s + `"}` + "\n", ahead: true},
		{name: project + s, dir: true, ahead: true},
		{name: project + s + "/subagents", dir: true, ahead: true},
		{name: project + s + "/subagents/agent-1.jsonl", body: `{"type":"user","n":1}` + "\n"},
		{name: project + s + "/subagents/agent-1.meta.json", body: `{"agentType":"general-purpose"}` + "\n",
			ahead: true},
		{name: project + s + "/tool-results", dir: true},
		{name: project + s + "/tool-results/out.txt", body: "spilled\n"},
		{name: "tasks/" + s, dir: true, ahead: true},
		{name: "tasks/" + s + "/1.json", body: `{"id":"1"}` + "\n", ahead: true},
	}
}

// stampInBox files session s in box b's configuration directory as
// [stampedSession] stamps it against now: a session whose newest files were
// stamped in the box's future, carried from a box whose clock ran ahead or
// stamped before this one's stepped back. Among the spilled results it files a
// LINK, stamped ahead, to a file in the box's home stamped behind, since
// anything run in the box can make one. It returns the link and that file.
func stampInBox(t *testing.T, b sandbox.Sandbox, s string, now time.Time) (link, outside string) {
	t.Helper()
	cfg := executor.PathsFor(b).ClaudeConfigDir()
	for _, e := range stampedSession(s) {
		at := filepath.Join(cfg, e.name)
		if e.dir {
			if err := os.MkdirAll(at, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(at, []byte(e.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside = filepath.Join(b.Home(), "outside.txt")
	if err := os.WriteFile(outside, []byte("no file of the session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(cfg, "projects", executor.ClaudeProjectDir, s, "tool-results", "linked.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// THE TIMES LAST, once nothing more is written into a directory, and the
	// link's own by a call that does not follow it to its file, as os.Chtimes
	// would.
	for _, e := range stampedSession(s) {
		if err := os.Chtimes(filepath.Join(cfg, e.name), e.stamp(now), e.stamp(now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(outside, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	ahead := unix.NsecToTimespec(now.Add(time.Hour).UnixNano())
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, link, []unix.Timespec{ahead, ahead},
		unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatalf("stamping the link itself: %v", err)
	}
	return link, outside
}

// boxNow is the box's clock as it stamps a file: the time of a file name
// written in b's home now, never the finer clock a test reads, which runs ahead
// of it.
func boxNow(t *testing.T, b sandbox.Sandbox, name string) time.Time {
	t.Helper()
	at := filepath.Join(b.Home(), name)
	if err := os.WriteFile(at, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(at)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// noneAhead checks session s in box b, stamped against now as [stampedSession]
// has it, after a pack from began to ended ([boxNow]): every entry stamped an
// hour ahead is now stamped within [began, ended], and every one stamped an
// hour behind keeps its time to the second.
func noneAhead(t *testing.T, b sandbox.Sandbox, s string, now, began, ended time.Time) {
	t.Helper()
	cfg := executor.PathsFor(b).ClaudeConfigDir()
	for _, e := range stampedSession(s) {
		info, err := os.Stat(filepath.Join(cfg, e.name))
		if err != nil {
			t.Errorf("%s is gone after the pack: %v", e.name, err)
			continue
		}
		switch at := info.ModTime(); {
		case at.After(ended):
			t.Errorf("%s is stamped %s after the pack, which was done by %s: in the box's future", e.name, at,
				ended)
		case e.ahead && at.Before(began):
			t.Errorf("%s, stamped in the box's future, is stamped %s after the pack, before the pack began at "+
				"%s: not the pack's own time", e.name, at, began)
		case !e.ahead && !at.Equal(e.stamp(now)):
			t.Errorf("%s is stamped %s after the pack, want the %s it was stamped with", e.name, at,
				e.stamp(now))
		}
	}
}

// linkLeftAlone checks that link, in a packed session, is still a link, and
// that outside, the file it names, keeps its time (an hour behind now) to the
// second: the pack set no time through the link.
func linkLeftAlone(t *testing.T, link, outside string, now time.Time) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the link in the session is gone after the pack: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is %s after the pack, no longer a link", link, info.Mode())
	}
	if info, err = os.Stat(outside); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-time.Hour); !info.ModTime().Equal(want) {
		t.Errorf("%s, outside the session, was stamped %s through the link to it in the session, want the "+
			"%s it was stamped with", outside, info.ModTime(), want)
	}
}

// A PACK BESIDE A WORKING HARNESS SETS EVERY FILE OF ITS SESSION FROM ITS BOX'S
// FUTURE TO ITS OWN START, AND HOLDS. The checks read a file from the box's
// future as written since the marker until the clock passes it, so before its
// marker every pack sets each file and directory find reads as newer than a
// reference to the present, leaves earlier times alone (the harness's
// retention sweep reads them) and sets none through a link (touch follows it).
// Both held packs hold over [stampInBox]'s session ([noneAhead]).
//
// Mutation: drop the step or run it after the marker (refused); set every time
// (a time behind is lost) or future ones to a time long past (stamped before
// the pack); set files alone (a directory stays ahead); drop `! -type l` (the
// file outside takes the pack's time through the link).
func TestAPackSetsEveryFileOfItsSessionFromItsBoxsFutureToItsStartAndHolds(t *testing.T) {
	t.Parallel()
	for _, pack := range heldPacks {
		t.Run(pack.name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, "2.1.310")
			_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			forgetCapture(t, b)
			now := time.Now().Truncate(time.Second)
			link, outside := stampInBox(t, b, ran.SessionID, now)
			began := boxNow(t, b, "pack-began")
			if err := pack.make(t, runner, b); err != nil {
				t.Fatalf("the pack of a session stamped in its box's future was refused: %v", err)
			}
			noneAhead(t, b, ran.SessionID, now, began, boxNow(t, b, "pack-ended"))
			linkLeftAlone(t, link, outside, now)
		})
	}
}

// A FILE FROM ITS BOX'S FUTURE THAT ITS HARNESS REWRITES AS IT IS PACKED IS
// STILL REFUSED. The pack sets such a file to the present before its marker
// and sets no time back after it, so a write under tar is newer than the
// marker to every check. Here a task file stamped an hour ahead is rewritten
// once tar has read the session: both held packs are refused naming it,
// leaving no archive, digest or staging directory.
//
// Mutation: once tar has read the session, set every file newer than the
// reference back to its time, and the rewritten task file falls behind the
// marker, and the pack holds.
func TestAFileFromItsBoxsFutureRewrittenAsItIsPackedIsStillRefused(t *testing.T) {
	t.Parallel()
	for _, pack := range heldPacks {
		t.Run(pack.name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, "2.1.310")
			_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			p := paths(b)
			forgetCapture(t, b)
			f := filesOf(ran.SessionID)
			ahead := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(p.ClaudeConfigDir(), f.task), ahead, ahead); err != nil {
				t.Fatal(err)
			}
			writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t,
				afterTar(`printf '%s\n' '{"id":"1","status":"completed"}' > `+shQuote(f.task))))
			err := pack.make(t, runner, b)
			if want := "a file was written while the session was packed: " + f.task; err == nil ||
				!strings.Contains(err.Error(), want) {
				t.Fatalf("the pack answered %v, want it refused saying %q", err, want)
			}
			if left := pack.dest(t, b); len(left) > 0 {
				t.Errorf("the refused pack left an archive at %s", strings.Join(left, ", "))
			}
			if left := pack.staged(t, b); len(left) > 0 {
				t.Errorf("the refused pack left its staging directory: %s", strings.Join(left, ", "))
			}
			if digests := boxFile(t, b, p.SessionDigests()); digests != "" {
				t.Errorf("the refused pack recorded a digest the box holds: %q", digests)
			}
		})
	}
}

// TWO PACKS BESIDE ONE WORKING HARNESS HIDE NO WRITE FROM EACH OTHER. Two packs
// of one session can run at once (two takers' saves as waiter duty changes
// hands, or a save and a working job's take), and each sets future-stamped
// files to the present, never an earlier time, which a late walk would put
// behind the other's marker. Here B's walk picks a future task file and holds,
// as a find on a busy box does, until A (reference and marker after B's
// reference) has the harness rewrite it under A's tar; then B sets it and A
// checks. A is refused naming the task file; B, its marker after the write,
// holds. Words pass through fifos the case holds open, on no clock.
//
// Mutation: set the files the walk picks to the reference's time (`touch -c -r`),
// and B sets the rewritten task file back behind A's marker, and A holds.
func TestTwoPacksBesideOneWorkingHarnessHideNoWriteFromEachOther(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	forgetCapture(t, b)
	f := filesOf(ran.SessionID)
	task := filepath.Join(p.ClaudeConfigDir(), f.task)
	ahead := time.Now().Add(time.Hour)
	if err := os.Chtimes(task, ahead, ahead); err != nil {
		t.Fatal(err)
	}
	flags := filepath.Join(b.Home(), "two-packs")
	if err := os.MkdirAll(flags, 0o700); err != nil {
		t.Fatal(err)
	}
	// THE WORDS PASSED: B's walk's "picked" (the case then starts A), A's tar's
	// "rewritten" (B's walk waits on it) and B's walk's "set" (A's tar waits on
	// it). A pack that ends says so in every fifo it owes a word in (wordInFifo).
	picked := heldFifo(t, flags, "picked")
	heldFifo(t, flags, "rewritten")
	heldFifo(t, flags, "set")
	at := func(name string) string { return shQuote(filepath.Join(flags, name)) }
	touch := shQuote(hostTool(t, "touch"))
	// THE FIRST TOUCH OF A WALK IS B'S — the walk's alone passes -c, the
	// clock's does not — and it says B has picked the task file, then holds
	// until A's tar has had it rewritten; every other touch is this machine's.
	writeBoxStandIn(t, b, p.BinDir()+"/touch", "#!/bin/sh\n"+
		`case " $* " in *" -c "*) if `+claim(filepath.Join(flags, "b-claimed"))+`; then printf 'picked\n' > `+
		at("picked")+`; read said < `+at("rewritten")+`; [ "$said" = rewritten ] || exit 3; `+touch+` "$@"; `+
		`st=$?; : > `+at("b-set")+`; printf 'set\n' > `+at("set")+`; exit "$st"; fi;; esac`+"\n"+
		"exec "+touch+` "$@"`+"\n")
	// A'S TAR rewrites the task file in place once it has read the session,
	// and hands A's checks the session only once B has set it.
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `case "$1" in `+
		shQuote(p.SessionSaveStaging(takerA, ""))+`*) "$tar" -czf "$@"; st=$?; printf '%s\n' `+
		`'{"id":"1","status":"completed"}' > `+shQuote(task)+`; : > `+at("a-rewrote")+`; printf 'rewritten\n' > `+
		at("rewritten")+`; read said < `+at("set")+`; [ "$said" = set ] || exit 2; exit "$st";; esac`+"\n"+
		`exec "$tar" -czf "$@"`+"\n"))
	packedB := make(chan error, 1)
	go func() {
		_, err := runner.PackSave(t.Context(), b, takerB)
		wordInFifo(flags, "picked", "ended")
		wordInFifo(flags, "set", "ended")
		packedB <- err
	}()
	word := make([]byte, 64)
	n, err := picked.Read(word)
	if err != nil || !strings.HasPrefix(string(word[:n]), "picked") {
		t.Fatalf("pack B's walk never picked the task file stamped in the box's future: it said %q (%v)",
			word[:n], err)
	}
	_, errA := runner.PackSave(t.Context(), b, takerA)
	wordInFifo(flags, "rewritten", "ended")
	errB := <-packedB
	for _, flag := range []string{"a-rewrote", "b-set"} {
		if _, err := os.Stat(filepath.Join(flags, flag)); err != nil {
			t.Fatalf("the packs never ran as the case has them: no %s (pack A: %v; pack B: %v)", flag, errA,
				errB)
		}
	}
	if want := "a file was written while the session was packed: " + f.task; errA == nil ||
		!strings.Contains(errA.Error(), want) {
		t.Errorf("pack A, under whose tar the task file was rewritten, answered %v once pack B's walk had set "+
			"it; want it refused saying %q", errA, want)
	}
	if left := savesLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the refused pack A left a save at %s", strings.Join(left, ", "))
	}
	if errB != nil {
		t.Errorf("pack B, whose marker came after the task file was rewritten, was refused: %v", errB)
	}
}

// TWO PACKS ON DEMAND OF ONE BOX AT ONCE BOTH HOLD BESIDE A HARNESS STILL
// WRITING. Two reads of a job its box would not end (a stop's and a
// collection's) each pack it by the working rule, its lists, marker and archive
// in a place of its own (executor.Paths.SessionPackHeld). Here the harness
// appends after each tar, and the first tar holds until the second has packed:
// each hands back a whole archive whose transcript is a byte prefix of the live
// one, and neither leaves a file once released, on no clock. An exited harness
// is TestTwoReadsOfOneJobAtOnceNeverMoveAHalfMadeArchive's.
//
// Mutation: stage every pack on demand in one directory (refused there too),
// or compare a transcript's whole live file rather than its first bytes (and
// in TestASaveOfASessionThatOnlyGrewAsItWasPackedHolds), and a pack is refused.
func TestTwoPacksOnDemandOfOneBoxAtOnceBothHold(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	forgetCapture(t, b)
	f := filesOf(ran.SessionID)
	transcript, thread := p.ClaudeConfigDir()+"/"+f.transcript, p.ClaudeConfigDir()+"/"+f.thread
	flags := filepath.Join(b.Home(), "two-at-once")
	if err := os.MkdirAll(flags, 0o700); err != nil {
		t.Fatal(err)
	}
	// THE SECOND PACK'S WORD that its tar has packed, which the first pack's
	// tar waits on; a pack that ends says so in its stead (wordInFifo).
	heldFifo(t, flags, "second")
	at := func(name string) string { return shQuote(filepath.Join(flags, name)) }
	// THE HARNESS WRITES once a pack's tar has read the session: a line
	// appended to its transcript and to a subagent's thread, the one change a
	// pack beside a working harness takes; a write that fails fails the tar.
	writes := func(pack string) string {
		line := shQuote(`{"type":"user","written":"beside the ` + pack + ` pack"}`)
		return `printf '%s\n' ` + line + ` >> ` + shQuote(transcript) + ` && printf '%s\n' ` + line + ` >> ` +
			shQuote(thread) + ` || exit 4`
	}
	// THE FIRST TAR TO CLAIM IS THE FIRST PACK'S: it packs, the harness
	// writes, and it holds until the second pack's tar has packed and the
	// harness has written again — failing where the word it is given is any
	// other, so an interleaving that did not happen fails the case.
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `if `+claim(filepath.Join(flags, "claimed"))+
		`; then "$tar" -czf "$@"; st=$?; `+writes("first")+`; : > `+at("first-packed")+`; `+
		`read said < `+at("second")+`; [ "$said" = packed ] || exit 3; exit "$st"; fi`+"\n"+
		`"$tar" -czf "$@"; st=$?; `+writes("second")+`; : > `+at("second-packed")+`; printf 'packed\n' > `+
		at("second")+`; exit "$st"`+"\n"))
	type packed struct {
		body    io.ReadCloser
		release func(context.Context)
		err     error
	}
	packs := make(chan packed, 2)
	for range 2 {
		go func() {
			_, body, release, err := runner.PackCapture(t.Context(), b, true)
			wordInFifo(flags, "second", "ended")
			packs <- packed{body: body, release: release, err: err}
		}()
	}
	results := []packed{<-packs, <-packs}
	for _, flag := range []string{"first-packed", "second-packed"} {
		if _, err := os.Stat(filepath.Join(flags, flag)); err != nil {
			t.Errorf("the packs never ran as the case has them: no %s", flag)
		}
	}
	live := boxFile(t, b, transcript)
	for _, got := range results {
		switch {
		case got.err != nil:
			t.Errorf("a pack made beside another of the same box, its harness writing, was refused: %v", got.err)
		case got.body == nil:
			t.Error("a pack made beside another of the same box handed back no archive")
		default:
			kept, ok := archiveMembers(t, got.body)[f.transcript]
			_ = got.body.Close()
			got.release(t.Context())
			switch {
			case !ok:
				t.Error("a pack made beside another of the same box handed back no transcript")
			case kept == "" || !strings.HasPrefix(live, kept):
				t.Errorf("a pack made beside another of the same box kept the transcript as %q, which is no "+
					"byte prefix of what its harness wrote, %q", kept, live)
			}
		}
	}
	if left := packStagingLeft(t, b); len(left) > 0 {
		t.Errorf("the packs left their files behind: %s", strings.Join(left, ", "))
	}
}

// A SAVE OF A SESSION THAT ONLY GREW AS IT WAS PACKED HOLDS: the one change a
// held pack takes. As tar reads, the harness appends to the transcript (last
// line torn) and a subagent's thread, and starts a new thread and a spilled
// result after tar read their directories, in no archive. GNU's tar says exit
// 1 beside a fine find, busybox's exit 0 beside a coarse one; both end a listed
// directory in a slash (measured on GNU tar 1.35 and BusyBox 1.37.0), and a tar
// without it is played: the check of what was written reads files alone. The
// save holds each transcript as first read.
//
// Mutation: refuse tar's exit 1, check `.jsonl` for writes, compare a whole live
// file, count a later thread as lost, check a file no archive holds, or read a
// newer directory as a file written, and a save is refused.
func TestASaveOfASessionThatOnlyGrewAsItWasPackedHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		coarse bool
		said   string
		others string
	}{
		{name: "as GNU's tar and find read it",
			said: `echo "tar: $grown: file changed as we read it" >&2; exit 1`},
		{name: "as busybox's tar and find read it", coarse: true, said: `exit 0`},
		{name: "as a tar that lists a directory without the slash GNU's and busybox's end one in",
			said:   `exit 0`,
			others: `if [ "$1" = -tzf ]; then "$tar" -tzf "$2" | sed 's,/$,,'; exit 0; fi`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, "2.1.310")
			_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			p := paths(b)
			f := filesOf(ran.SessionID)
			project := "projects/" + executor.ClaudeProjectDir + "/" + ran.SessionID
			born, meta, spilt := project+"/subagents/workflow/agent-9.jsonl",
				project+"/subagents/workflow/agent-9.meta.json", project+"/tool-results/new.txt"
			writeBoxStandIn(t, b, p.BinDir()+"/tar", tarStandIn(t, `"$tar" -czf "$@" || exit 2`+"\n"+
				`grown=`+shQuote(f.transcript)+"\n"+
				`printf '%s\n%s' '{"type":"user","grew":"after tar read it"}' '{"type":"assist' >> "$grown"`+"\n"+
				`echo '{"type":"user","more":1}' >> `+shQuote(f.thread)+"\n"+
				`mkdir -p "$(dirname `+shQuote(born)+`)" && echo '{"type":"user"}' > `+shQuote(born)+"\n"+
				`echo '{"agentType":"workflow"}' > `+shQuote(meta)+"\n"+
				`echo spilled > `+shQuote(spilt)+"\n"+
				tc.said+"\n", tc.others))
			if tc.coarse {
				coarseClock(t, b)
			}
			before := boxFile(t, b, transcriptOf(b, ran.SessionID))
			saved, err := runner.PackSave(t.Context(), b, takerA)
			if err != nil {
				t.Fatalf("the save of a session that only grew as it was packed was refused: %v", err)
			}
			members := archiveMembers(t, strings.NewReader(boxFile(t, b, saved.Path)))
			if got := members[f.transcript]; got != before {
				t.Errorf("the save holds the transcript as %q, want it as tar first read it, %q", got, before)
			}
			if got := members[f.thread]; got != "subagent\n" {
				t.Errorf("the save holds the subagent's thread as %q, want it as tar first read it", got)
			}
			for _, made := range []string{born, meta, spilt} {
				if _, ok := members[made]; ok {
					t.Errorf("the save holds %s, made after tar read its directory", made)
				}
			}
		})
	}
}

// A PACK MADE ONCE ITS HARNESS IS GONE KEEPS THE RULE THE RUN'S OWN END PACKS
// BY. No harness is left to write, so neither the run's own end nor a take of
// a finished job that left no archive waits on the clock or refuses a file
// written as it packs (the task file, as a process that outlived the harness
// may touch it); each lands where its read reads it, the end over the capture
// and the take at a place of its own. The take runs tar beside the
// configuration directory, so the writer names the file whole.
//
// Mutation: hold the run's own end to the working rule, or route a finished
// job's take to it, and that pack is refused.
func TestAPackMadeOnceItsHarnessIsGoneKeepsTheEndsRule(t *testing.T) {
	t.Parallel()
	t.Run("the run's own end", func(t *testing.T) {
		t.Parallel()
		runner, b := sessionBox(t, "2.1.310")
		p := paths(b)
		// WHICHEVER SESSION THE RUN FILES: the runner names it as the run starts.
		writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t,
			afterTar(`for f in `+shQuote(p.ClaudeConfigDir()+"/tasks")+`/*/1.json; do touch "$f"; done`)))
		_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
		if ran.Captured == nil || !boxHas(t, b, p.SessionCapture()) {
			t.Fatalf("the run's own end packed no session beside a file written as it packed:\n%s",
				boxFile(t, b, p.Err()))
		}
	})
	t.Run("a take of a job that is over, whose script left no archive", func(t *testing.T) {
		t.Parallel()
		runner, b := sessionBox(t, "2.1.310")
		_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
		p := paths(b)
		forgetCapture(t, b)
		writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t,
			afterTar(`touch `+shQuote(p.ClaudeConfigDir()+"/"+filesOf(ran.SessionID).task))))
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{MaxBytes: takeBound})
		if err != nil || taken.Body == nil {
			t.Fatalf("the take of a job that is over = %+v, %v; want its session packed as its own end packs it",
				taken, err)
		}
		_ = taken.Body.Close()
		if !strings.HasPrefix(taken.Archive.Path, p.WorkDir()+"/session-pack-staging.") || boxHas(t, b, p.SessionCapture()) {
			t.Errorf("the take packed to %s, want a place of its own pack's, and nothing over the job's capture",
				taken.Archive.Path)
		}
	})
}

// A PACK BESIDE A WORKING HARNESS FAILS WHERE ITS BOX'S CLOCK NEVER MOVES,
// rather than pack blind. Its checks see a write by find reading the file as
// newer than a marker, so it first waits for find to read a fresh file as
// newer than the marker; a box whose find never does is refused once the
// wait's bound passes, saying so, and tar is never asked to pack.
//
// Mutation: drop the wait's bound, and the pack waits until its exec is cut
// off and says nothing of the clock; pack once the bound has passed, and the
// session is packed with no check able to see a write.
func TestAPackWhoseBoxClockNeverMovesIsRefused(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	// A FIND TO WHICH NOTHING IS EVER NEWER, and a sleep that takes no time,
	// so the wait reaches its bound at once.
	writeBoxStandIn(t, b, p.BinDir()+"/find", neverNewerFind(t))
	writeBoxStandIn(t, b, p.BinDir()+"/sleep", "#!/bin/sh\nexit 0\n")
	asked := b.Home() + "/tar-asked"
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `echo "$@" >> `+shQuote(asked)+"\n"+
		`exec "$tar" -czf "$@"`+"\n"))
	_, err := runner.PackSave(t.Context(), b, takerA)
	if err == nil || !strings.Contains(err.Error(), "the box's clock did not move") {
		t.Fatalf("the save in a box whose clock never moves answered %v, want it refused saying so", err)
	}
	if boxHas(t, b, asked) {
		t.Errorf("tar was asked to pack the session: %s", boxFile(t, b, asked))
	}
	if left := savesLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the refused save left an archive at %s", strings.Join(left, ", "))
	}
}

// A PACK BESIDE A WORKING HARNESS FAILS ON A BOX WHOSE SLEEP TAKES NO TENTH OF
// A SECOND, and says so. Its clock wait counts in tenths, as every wait in the
// engine's scripts does; a sleep that refuses one would run the count out at
// once and read a moving clock as one that never moved. So the refused tenth
// fails the pack by its own name, and tar is never asked to pack.
//
// Mutation: sleep a second where the tenth is refused, or go on as though it
// had been slept, and the pack is refused as a box whose clock never moved.
func TestAPackOnABoxWhoseSleepTakesNoTenthIsRefused(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	// A FIND TO WHICH NOTHING IS NEWER, so the wait sleeps, and a sleep that
	// refuses a fraction and takes a whole second at once.
	writeBoxStandIn(t, b, p.BinDir()+"/find", neverNewerFind(t))
	writeBoxStandIn(t, b, p.BinDir()+"/sleep", "#!/bin/sh\ncase $1 in *.*) exit 1;; esac\nexit 0\n")
	asked := b.Home() + "/tar-asked"
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `echo "$@" >> `+shQuote(asked)+"\n"+
		`exec "$tar" -czf "$@"`+"\n"))
	_, err := runner.PackSave(t.Context(), b, takerA)
	if err == nil || !strings.Contains(err.Error(), "this box's sleep takes no tenth of a second") {
		t.Fatalf("the save in a box whose sleep takes no tenth answered %v, want it refused saying so", err)
	}
	if boxHas(t, b, asked) {
		t.Errorf("tar was asked to pack the session: %s", boxFile(t, b, asked))
	}
	if left := savesLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the refused save left an archive at %s", strings.Join(left, ", "))
	}
}

// A PACK BESIDE A WORKING HARNESS NAMES A TOOL ITS BOX LACKS, rather than
// refuse on what the lack makes another check read: without find its clock
// wait reads a clock that never moved, and without touch it can neither set
// future files back nor write its marker. So it asks the box for every tool it
// reads the session with, and a box lacking find or touch is told which, its
// session unpacked. The pack runs as the runner hands it to a login shell,
// under that PATH alone.
//
// Mutation: drop the ask, and the box without find is refused as a clock that
// never moved; leave touch out of it, and the box without touch is refused as a
// pack that could not write its marker.
func TestAPackBesideAWorkingHarnessNamesAToolItsBoxLacks(t *testing.T) {
	t.Parallel()
	// THE BOX'S PATH holds every tool the pack reads a session with but the
	// one each case leaves out.
	tools := []string{"tar", "gzip", "rm", "mkdir", "mv", "cat", "find", "ls", "wc", "sed", "grep", "head",
		"cksum", "touch", "sleep"}
	for _, lacking := range []string{"find", "touch"} {
		t.Run("without "+lacking, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			p := executor.Paths{Home: filepath.Join(dir, "home")}
			const session = "0b6e2a1c-3d4f-4a5b-8c7d-9e0f1a2b3c4d"
			project := filepath.Join(p.ClaudeConfigDir(), "projects", executor.ClaudeProjectDir)
			for _, d := range []string{bin, p.WorkDir(), filepath.Join(project, session, "subagents"),
				filepath.Join(p.ClaudeConfigDir(), "tasks", session)} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(project, session+".jsonl"), []byte(`{"type":"user"}`+"\n"),
				0o600); err != nil {
				t.Fatal(err)
			}
			for _, tool := range tools {
				if tool == lacking {
					continue
				}
				if err := os.Symlink(hostTool(t, tool), filepath.Join(bin, tool)); err != nil {
					t.Fatal(err)
				}
			}
			script, err := executor.ClaudeCode{}.PackSave(p, executor.SessionRecordOf(session),
				p.SessionSaveStaging(takerA, "a-save"), p.SessionSave(takerA, "a-save"))
			if err != nil {
				t.Fatalf("a Claude Code save's script was refused: %v", err)
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", script) //nolint:gosec // the script under test
			cmd.Env = []string{"PATH=" + bin}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || !strings.Contains(stderr.String(), "this box has no "+lacking) {
				t.Fatalf("the pack in a box without %s ended %v, want it refused naming %s:\n%s", lacking, err,
					lacking, stderr.String())
			}
			if _, err := os.Stat(p.SessionSave(takerA, "a-save")); !os.IsNotExist(err) {
				t.Errorf("the pack in a box without %s left a save (%v)", lacking, err)
			}
		})
	}
}

// A SAVE THAT FAILS LEAVES THE LAST ONE STANDING, AND NOTHING OF ITS OWN. A
// taker's save lands; the harness writes its task file under the taker's next
// save, which is refused, and the last save stays at its place byte for byte,
// with no archive, digest or staging directory of the refused one's; a later
// save that holds lands at a place of its own, the session as it went on.
//
// Mutation: move the staged archive into place before its checks or on a
// failure, move a save over the taker's last, or remove the staging directory
// only where the pack held, and a check goes red.
func TestAFailedSaveLeavesTheLastOneStanding(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	saved, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("the first save: %v", err)
	}
	last := boxFile(t, b, saved.Path)
	digests := boxFile(t, b, p.SessionDigests())

	files := filesOf(ran.SessionID)
	transcript := transcriptOf(b, ran.SessionID)
	grown := boxFile(t, b, transcript) + `{"type":"user","line":"after the first save"}` + "\n"
	if err := b.WriteFile(t.Context(), transcript, []byte(grown)); err != nil {
		t.Fatal(err)
	}
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, afterTar(`touch `+shQuote(files.task))))
	if _, err := runner.PackSave(t.Context(), b, takerA); err == nil {
		t.Fatal("a save of a task file written while it was packed was made")
	}
	if got := boxFile(t, b, saved.Path); got != last {
		t.Error("the refused save changed the taker's last one")
	}
	if left := savesLeft(t, b, takerA); len(left) != 1 || left[0] != saved.Path {
		t.Errorf("the taker's saves are %s, want the last one alone, %s", strings.Join(left, ", "), saved.Path)
	}
	if got := boxFile(t, b, p.SessionDigests()); got != digests {
		t.Errorf("the refused save recorded a digest: %q, was %q", got, digests)
	}
	if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the refused save left its staging directory behind: %s", strings.Join(left, ", "))
	}

	if _, err := b.Exec(t.Context(), "rm -f "+shQuote(p.BinDir()+"/tar"), sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	again, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("the save after it: %v", err)
	}
	members := archiveMembers(t, strings.NewReader(boxFile(t, b, again.Path)))
	if again.Path == saved.Path || !strings.Contains(members[files.transcript], "after the first save") {
		t.Errorf("the save that held is at %s, beside the last one at %s, holding %q: want a place of its own "+
			"and the session as it went on", again.Path, saved.Path, members[files.transcript])
	}
	if got := boxFile(t, b, saved.Path); got != last {
		t.Error("the save that held changed the one before it")
	}
}

// A SAVE IS NOT PACKED PAST WHAT THE SCRUB WOULD READ. A session whose files
// sum past what a kept session's scrub reads of one archive (a sparse spilled
// result here) would be refused by the take once packed, so the pack refuses
// it first, saying so, without asking tar; without that file it is saved.
//
// Mutation: drop the sum before the tar, or sum less than every file, and tar
// packs the quarter of a gibibyte.
func TestASaveIsNotPackedPastWhatTheScrubWouldRead(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	big := p.ClaudeConfigDir() + "/projects/" + executor.ClaudeProjectDir + "/" + ran.SessionID +
		"/tool-results/large.txt"
	if err := b.WriteFile(t.Context(), big, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, executor.HeldPlainMax); err != nil {
		t.Fatal(err)
	}
	asked := b.Home() + "/tar-asked"
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `echo "$@" >> `+shQuote(asked)+"\n"+
		`exec "$tar" -czf "$@"`+"\n"))
	_, err := runner.PackSave(t.Context(), b, takerA)
	if err == nil || !strings.Contains(err.Error(), "a kept session's scrub reads, so it would be refused once packed") {
		t.Fatalf("the save of a session past the scrub's bound answered %v, want it refused saying so", err)
	}
	if boxHas(t, b, asked) {
		t.Errorf("tar was asked to pack the session: %s", boxFile(t, b, asked))
	}
	if left := savesLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the refused save left an archive at %s", strings.Join(left, ", "))
	}

	if _, err := b.Exec(t.Context(), "rm -f "+shQuote(big), sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.PackSave(t.Context(), b, takerA); err != nil {
		t.Fatalf("the session within the bound was not saved: %v", err)
	}
}

// THE CLEAR BEFORE EVERY RUN REMOVES THE SAVES' FILES, whoever took them (two
// takers' saves and a cut-short save's staging directory), which a later job's
// take must not read as its own, and keeps what the box keeps for its life,
// the digests. A home whose path holds a space, which the clear quotes, too.
//
// Mutation: leave the saves out of the clear, or quote their pattern so it
// matches nothing, and a file outlives it; leave the work directory unquoted,
// and a path holding a space splits into words and nothing is removed.
func TestTheClearBeforeEveryRunRemovesTheSavesFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, root string }{
		{name: "a box", root: "boxes"},
		{name: "a box whose home's path holds a space", root: "the boxes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct,
				StateDir: filepath.Join(t.TempDir(), tc.root)})
			if err != nil {
				t.Fatalf("NewLocal: %v", err)
			}
			b, err := local.Create(t.Context(), sandbox.Spec{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			t.Cleanup(func() { b.Close(t.Context()) })
			p := paths(b)
			if strings.Contains(p.WorkDir(), " ") != strings.Contains(tc.root, " ") {
				t.Fatalf("the box's work directory is %q, under the root %q", p.WorkDir(), tc.root)
			}
			// TWO TAKERS' SAVES, AND A SAVE CUT SHORT, its node stopped as it
			// packed.
			saved := []string{p.SessionSave(takerA, "a-save"), p.SessionSave(takerB, "b-save"),
				p.SessionSaveStaging(takerA, "a-save-cut-short") + "/capture"}
			for _, file := range saved {
				if err := b.WriteFile(t.Context(), file, []byte("an archive of the session")); err != nil {
					t.Fatal(err)
				}
			}
			const digests = "0b6e2a1c-3d4f-4a5b-8c7d-9e0f1a2b3c4d 4294967295 42\n"
			if err := b.WriteFile(t.Context(), p.SessionDigests(), []byte(digests)); err != nil {
				t.Fatal(err)
			}
			if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), b); err != nil {
				t.Fatalf("ClearArtifacts: %v", err)
			}
			for _, file := range saved {
				if boxHas(t, b, file) {
					t.Errorf("%s outlived the clear", file)
				}
			}
			if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
				t.Errorf("the clear left a save's staging directory: %s", strings.Join(left, ", "))
			}
			if got := boxFile(t, b, p.SessionDigests()); got != digests {
				t.Errorf("the clear changed the digests the box keeps for its life: %q, was %q", got, digests)
			}
		})
	}
}

// TWO SAVES OF ONE TAKER AT ONCE BOTH HOLD. One taker can save a box twice at
// once (the drain's save beside the waiter's), so each save stages in a
// directory named for it alone (executor.Paths.SessionSaveStaging) and moves
// its archive whole to a place of its own (executor.Paths.SessionSave). Here
// the first tar holds until the second has packed: both hold, each place holds
// a whole archive, and neither leaves its staging directory. Words pass
// through a fifo the case holds open, so nothing waits on a clock.
//
// Mutation: stage every save of a taker in one directory, and the two share an
// archive and lists: one's move finds nothing, or one's lists go under the
// other's checks, so one is refused.
func TestTwoSavesOfOneTakerAtOnceBothHold(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	forgetCapture(t, b)
	flags := filepath.Join(b.Home(), "two-saves")
	if err := os.MkdirAll(flags, 0o700); err != nil {
		t.Fatal(err)
	}
	// THE SECOND SAVE'S WORD that its tar has packed, which the first save's
	// tar waits on; a save that ends says so in its stead (wordInFifo).
	heldFifo(t, flags, "second")
	at := func(name string) string { return shQuote(filepath.Join(flags, name)) }
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `if `+claim(filepath.Join(flags, "claimed"))+
		`; then "$tar" -czf "$@"; st=$?; : > `+at("first-packed")+`; read said < `+at("second")+
		`; [ "$said" = packed ] || exit 3; exit "$st"; fi`+"\n"+
		`"$tar" -czf "$@"; st=$?; : > `+at("second-packed")+`; printf 'packed\n' > `+at("second")+
		`; exit "$st"`+"\n"))
	type savedAs struct {
		archive sandbox.SessionArchive
		err     error
	}
	saved := make(chan savedAs, 2)
	for range 2 {
		go func() {
			archive, err := runner.PackSave(t.Context(), b, takerA)
			wordInFifo(flags, "second", "ended")
			saved <- savedAs{archive: archive, err: err}
		}()
	}
	var places []string
	for range 2 {
		got := <-saved
		if got.err != nil {
			t.Errorf("a save made beside another of the same taker was refused: %v", got.err)
			continue
		}
		places = append(places, got.archive.Path)
	}
	for _, flag := range []string{"first-packed", "second-packed"} {
		if _, err := os.Stat(filepath.Join(flags, flag)); err != nil {
			t.Errorf("the saves never ran as the case has them: no %s", flag)
		}
	}
	if len(places) == 2 && places[0] == places[1] {
		t.Errorf("the two saves were moved to one place, %s", places[0])
	}
	for _, place := range places {
		if _, ok := archiveMembers(t, strings.NewReader(boxFile(t, b, place)))[filesOf(
			ran.SessionID).transcript]; !ok {
			t.Errorf("the save at %s holds no transcript", place)
		}
	}
	if left := saveStagingLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the saves left their staging directories: %s", strings.Join(left, ", "))
	}
}

// wordInFifo writes word as a line into fifo name in dir, held open both ways
// (heldFifo) so the write never blocks: what a pack that has ended says in the
// stead of a word it owed, so nothing waits on that word.
func wordInFifo(dir, name, word string) {
	if fifo, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
		_, _ = fifo.WriteString(word + "\n")
		_ = fifo.Close()
	}
}

// takeSave takes the save at path of b's job, as a take of a save does, and
// reads it to its end.
func takeSave(ctx context.Context, runner *executor.Runner, b sandbox.Sandbox, path string) readBack {
	taken, err := runner.TakeSession(ctx, b, sandbox.SessionTake{Save: path, MaxBytes: takeBound})
	switch {
	case err != nil:
		return readBack{err: err}
	case taken.Body == nil:
		return readBack{err: errors.New("the take handed back no archive")}
	}
	archive, err := io.ReadAll(taken.Body)
	_ = taken.Body.Close()
	return readBack{archive: archive, err: err}
}

// A TAKE OF A SAVE STREAMS THAT SAVE'S OWN ARCHIVE, WHATEVER ANOTHER SAVE OF ITS
// TAKER LANDS AS IT READS. One taker can save a box twice at once, and a
// Claude Code take opens its archive again by path for a second pass. Here the
// first save's take holds before that pass while the session grows and a
// second save lands; the first take is not refused and hands back its own
// archive, without the later write; the second hands back its own, with it;
// neither save is left once taken. A box holds the take by its own steps, on
// no clock.
//
// Mutation: move every save of a taker to one path, and the second lands over
// the archive the first take is reading, which its second pass refuses.
func TestATakeOfASaveStreamsItsOwnWhateverAnotherSaveOfItsTakerLands(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	forgetCapture(t, b)
	first, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("the first save: %v", err)
	}
	held := newHeldTake(b, false, 2)
	read := make(chan readBack, 1)
	go func() { read <- takeSave(t.Context(), runner, held, first.Path) }()
	select {
	case <-held.held:
	case got := <-read:
		t.Fatalf("the first save's take ended before its second pass: %q, %v", got.archive, got.err)
	}
	line := shQuote(`{"type":"user","text":"` + wentOnMark + `"}`)
	if _, err := b.Exec(t.Context(), "printf '%s\\n' "+line+" >> "+shQuote(transcriptOf(b, ran.SessionID)),
		sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	second, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("the second save: %v", err)
	}
	close(held.resume)
	firstGot := <-read
	for _, r := range []struct {
		which  string
		got    readBack
		wentOn bool
	}{{"the first save's take", firstGot, false}, {"the second save's take", takeSave(t.Context(), runner, b,
		second.Path), true}} {
		if r.got.err != nil {
			t.Errorf("%s was refused: %v", r.which, r.got.err)
			continue
		}
		transcript, ok := archiveMembers(t, bytes.NewReader(r.got.archive))[filesOf(ran.SessionID).transcript]
		switch {
		case !ok:
			t.Errorf("%s handed back an archive with no transcript", r.which)
		case strings.Contains(transcript, wentOnMark) != r.wentOn:
			t.Errorf("%s handed back an archive that holds what was written after the first save: %v, want %v "+
				"— another save's archive, not its own", r.which, !r.wentOn, r.wentOn)
		}
	}
	if left := savesLeft(t, b, takerA); len(left) > 0 {
		t.Errorf("the takes left their saves in the box: %s", strings.Join(left, ", "))
	}
}

// A TAKE OF A JOB STILL WORKING NEVER MOVES ITS ARCHIVE OVER THE JOB'S CAPTURE.
// The capture is every finished job's last read, and the job's end pack moves
// its final archive there the moment the harness exits, which can fall within
// a held pack's seconds of checks. Here, on either harness, the program the
// pack writes with (Claude Code's tar, OpenCode's gzip) lands the job's final
// archive over the capture once the take's pack has written its own: the take
// hands back a whole archive of its own (executor.Paths.SessionPackHeld), the
// capture keeps the final archive, and nothing is left once the reader closes.
//
// Mutation: move a working job's archive over the capture, or have either pack
// move it there whatever it is handed, and the final archive is replaced; leave
// the take's archive behind once done, and it is found after.
func TestATakeOfAJobStillWorkingNeverMovesItsArchiveOverTheJobsCapture(t *testing.T) {
	t.Parallel()
	const final = "the job's own final archive, moved over the capture as its harness exited"
	harnesses := []struct {
		name string
		// packer is the program the harness's pack writes its archive with,
		// and word the first word of the pack's call of it.
		packer, word string
		// job is a box whose job is still working, and its session's id.
		job func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string)
		// lacks is what archive lacks of a whole archive of session: "" where
		// it lacks nothing.
		lacks func(t *testing.T, archive []byte, session string) string
	}{
		{name: "Claude Code", packer: "tar", word: "-czf",
			job: func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
				runner, b := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				forgetCapture(t, b)
				return runner, b, ran.SessionID
			},
			lacks: func(t *testing.T, archive []byte, session string) string {
				if _, ok := archiveMembers(t, bytes.NewReader(archive))[filesOf(session).transcript]; !ok {
					return "its transcript"
				}
				return ""
			}},
		{name: "OpenCode", packer: "gzip", word: "-c",
			job: func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
				runner, b := openCodeBox(t, t.TempDir())
				dir := t.TempDir()
				writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
					strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shQuote(dir), 1))
				const session = "ses_0123456789abcdef0123456789"
				if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
					[]byte("id "+session+"\nrelease 2.0.32\n")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return runner, b, session
			},
			lacks: func(t *testing.T, archive []byte, session string) string {
				if export := gunzipped(t, archive); !json.Valid(export) ||
					!strings.Contains(string(export), `"id":"`+session+`"`) {
					return "the one JSON document of the session"
				}
				return ""
			}},
	}
	for _, h := range harnesses {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			runner, b, session := h.job(t)
			p := paths(b)
			// THE JOB'S OWN END PACK LANDS as the take's pack has written its
			// archive and has yet to move it.
			own := shQuote(hostTool(t, h.packer))
			writeBoxStandIn(t, b, p.BinDir()+"/"+h.packer, "#!/bin/sh\n"+
				`if [ "$1" != `+shQuote(h.word)+` ]; then exec `+own+` "$@"; fi`+"\n"+
				own+` "$@"; st=$?`+"\n"+
				`printf '%s' `+shQuote(final)+` > `+shQuote(p.SessionCapture())+` || exit 5`+"\n"+
				`exit "$st"`+"\n")
			taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, MaxBytes: takeBound})
			if err != nil || taken.Body == nil {
				t.Fatalf("the take of a job still working = %+v, %v; want its session packed as it stands", taken, err)
			}
			archive, readErr := io.ReadAll(taken.Body)
			_ = taken.Body.Close()
			switch {
			case readErr != nil:
				t.Errorf("the take was not read whole: %v", readErr)
			case h.lacks(t, archive, session) != "":
				t.Errorf("the take handed back an archive of the session without %s", h.lacks(t, archive, session))
			}
			if got := boxFile(t, b, p.SessionCapture()); got != final {
				t.Errorf("the job's capture holds %d bytes after the take, not the job's own final archive: the "+
					"take moved its archive over it", len(got))
			}
			if left := packStagingLeft(t, b); len(left) > 0 {
				t.Errorf("the take left its pack's files behind: %s", strings.Join(left, ", "))
			}
		})
	}
}
