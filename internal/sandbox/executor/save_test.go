package executor_test

import (
	"bytes"
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

// A save of a session its harness still works in, and every pack beside such a
// harness, made on a real box by the runner's own pack — the harness's writes
// under it played by a writer each case controls, never raced against a clock.

// takerA and takerB are two takers of one box's saves, named as a process names
// the lease it takes captures under: its incarnation, `<node>:<uuid>`.
const (
	takerA = "node-a:2a6c3f4e-0b1d-4c8e-9f7a-1b2c3d4e5f60"
	takerB = "node-b:7d8e9f0a-1b2c-4d3e-8f4a-5b6c7d8e9f01"
)

// forgetCapture removes the archive the run's script packed as its harness
// exited, and the digests beside it: a box whose job never got its own pack in,
// so whatever is found at a pack's destination after is that pack's doing.
func forgetCapture(t *testing.T, box sandbox.Sandbox) {
	t.Helper()
	p := paths(box)
	if _, err := box.Exec(t.Context(), "rm -f "+shQuote(p.SessionCapture())+" "+shQuote(p.SessionDigests()),
		sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
}

// hostTool is where this machine's own name is, for a stand-in that hands its
// calls on to it.
func hostTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("no %s on this machine's PATH: %v", name, err)
	}
	return path
}

// rewritingTar is a stand-in `tar` for a box's bin directory, which a pack's
// PATH puts first: every call but a pack's (`tar -czf <archive> <files...>`)
// goes to this machine's tar, and a pack's runs pack — a script with that tar
// as "$tar", the archive as "$1" and the files after it, run where the pack
// runs tar: in the harness's configuration directory for a pack beside a
// working harness, beside it for one made once the harness is gone — so the
// case's own writer changes the session at exactly the moment the case names,
// as a harness would while tar read it.
func rewritingTar(t *testing.T, pack string) string { return tarStandIn(t, pack, "") }

// tarStandIn is [rewritingTar] that hands every call to others first: shell
// that reads the call's words ("$1" the first, "$tar" this machine's tar) and
// exits where it answers that call itself — the read back of a member, say —
// or falls through.
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

// grownBack rewrites the file at file, IN PLACE — the same inode — down to its
// first "$h" bytes and then past its old size, as Claude Code's withdrawal of a
// message cuts its transcript and writes it again.
func grownBack(file string) string {
	f := shQuote(file)
	return `cp ` + f + ` ` + f + `.was && { head -c "$h" ` + f + `.was; printf '%s\n' ` +
		`'{"type":"user","grown":"back past its old size"}' '{"type":"user","after":"it shrank"}'; } > ` + f +
		` && rm -f ` + f + `.was`
}

// neverNewerFind is a stand-in `find` for a box's bin directory to which no
// file is ever newer than another: a box whose clock never moves, or that
// stamps its files with none. Every other word goes to this machine's find.
func neverNewerFind(t *testing.T) string {
	t.Helper()
	return "#!/bin/sh\nfor a; do [ \"$a\" != -newer ] || exit 0; done\nexec " + shQuote(hostTool(t, "find")) +
		` "$@"` + "\n"
}

// coarseClock puts in b's bin directory a stand-in `find` and a stand-in
// `sleep` that together play a find of a coarse grain, as busybox's is, which
// compares whole seconds (measured on BusyBox 1.37.0: a file written 200 ms
// after the reference, within its second, is not newer to it): to that find
// NO FILE IS NEWER than another until the box's sleep has slept — the box's
// clock gone past the grain its marker was written in — and after that it
// compares as this machine's own find does, to which every other word goes;
// the sleep sleeps as this machine's own. So a case decides which writes the
// grain hides — every one made before the pack's first sleep — rather than
// racing the second the wall clock is in.
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
// ARCHIVE. A box whose job's own exit pack never landed is saved by one taker
// and then another: each save is packed in that taker's own staging directory —
// a pack on demand's, and another taker's, left as they were — and moved into
// that taker's own place, named by a digest of the taker rather than the taker
// itself, its staging directory gone and its digest kept in the box. A take of
// a save reads that taker's alone and packs nothing: the job's capture is
// still not there after it, nor any digest added, and a taker that saved none
// is taken as nothing. Only then is the job's final archive read, and a
// collection's read finds none, so the final take packs the job's own.
//
// Mutation: move a save over the job's capture, as a pack on demand moves its
// archive, and the collection's read finds it; name one path for every taker
// and the second save replaces the first; stage a save where a pack on demand
// or another taker stages, and that one's archive is gone; take a save by
// packing one — over the job's capture or anywhere — or the job's final
// archive from a save's path, and its check goes red.
func TestASaveIsPackedToItsTakersOwnPathAndNeverTakenAsTheJobsFinal(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	forgetCapture(t, b)
	// WHAT A PACK ON DEMAND AND ANOTHER TAKER'S SAVE ARE STAGING as this one
	// packs.
	elsewhere := []string{p.SessionPackStaging("another-pack") + "/capture",
		p.SessionSaveStaging(takerB) + "/capture"}
	for _, staged := range elsewhere {
		if err := b.WriteFile(t.Context(), staged, []byte("another pack's archive, half made")); err != nil {
			t.Fatal(err)
		}
	}

	saved, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("PackSave: %v", err)
	}
	if saved.Path != p.SessionSave(takerA) || saved.SessionID != ran.SessionID ||
		saved.Harness != executor.ClaudeCodeName || saved.Format != executor.ClaudeArchiveFormat ||
		saved.Version != "2.1.310" || saved.Size <= 0 {
		t.Errorf("the save is described as %+v, want session %s of Claude Code 2.1.310 at %s", saved,
			ran.SessionID, p.SessionSave(takerA))
	}
	if strings.Contains(saved.Path, "node-a") {
		t.Errorf("the save's path names its taker's holder rather than a digest of it: %s", saved.Path)
	}
	if boxHas(t, b, p.SessionCapture()) {
		t.Error("the save was moved over the job's capture, which a collection takes as the job's final archive")
	}
	if boxHas(t, b, p.SessionSaveStaging(takerA)) {
		t.Error("the save left its staging directory behind")
	}
	for _, staged := range elsewhere {
		if !boxHas(t, b, staged) {
			t.Errorf("the save removed %s, which another pack was staging", staged)
		}
	}
	first := []byte(boxFile(t, b, p.SessionSave(takerA)))
	digests := boxFile(t, b, p.SessionDigests())
	if !strings.Contains(digests, ran.SessionID+" "+digestOf(t, first)+"\n") {
		t.Errorf("the box keeps no digest of the save it packed: %q", digests)
	}

	// ANOTHER TAKER'S SAVE of the same box is a file of its own.
	other, err := runner.PackSave(t.Context(), b, takerB)
	if err != nil {
		t.Fatalf("PackSave for the second taker: %v", err)
	}
	if other.Path == saved.Path || other.Path != p.SessionSave(takerB) {
		t.Errorf("the second taker's save is at %s, want its own %s, apart from %s", other.Path,
			p.SessionSave(takerB), saved.Path)
	}
	if again := []byte(boxFile(t, b, p.SessionSave(takerA))); !bytes.Equal(again, first) {
		t.Error("the second taker's save changed the first taker's")
	}

	// A TAKE OF A SAVE reads its taker's alone, and packs nothing: no capture
	// of the job's appears, and no digest is added.
	digests = boxFile(t, b, p.SessionDigests())
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: takerA, MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("the take of the first taker's save = %+v, %v", taken, err)
	}
	members := archiveMembers(t, taken.Body)
	_ = taken.Body.Close()
	if taken.Archive.Path != p.SessionSave(takerA) {
		t.Errorf("the take of a save read %s, want the taker's own %s", taken.Archive.Path, p.SessionSave(takerA))
	}
	if _, ok := members[filesOf(ran.SessionID).transcript]; !ok {
		t.Errorf("the save holds no transcript: %v", members)
	}
	none, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{
		Save: "node-c:0f1e2d3c-4b5a-4968-8776-655443322110", MaxBytes: takeBound})
	if err != nil || none.Body != nil {
		t.Errorf("the take of a save no taker made = %+v, %v; want nothing", none, err)
	}
	if boxHas(t, b, p.SessionCapture()) {
		t.Error("a take of a save packed the job's capture")
	}
	if got := boxFile(t, b, p.SessionDigests()); got != digests {
		t.Errorf("a take of a save added a digest, so it packed: %q, was %q", got, digests)
	}
	if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: takerA, Pack: true,
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
	if final.Archive.Path != p.SessionCapture() {
		t.Errorf("the final take read %s, want the job's own capture %s", final.Archive.Path, p.SessionCapture())
	}
}

// A SAVE OF A FILE REWRITTEN WHILE IT WAS PACKED FAILS, AND SAYS WHICH CHECK
// REFUSED IT — and so does every pack made beside a harness that may still
// work, a take of a job its box would not end, which packs it on demand
// (sandbox.SessionTake.Pack). Each case is a write the harness makes as tar
// reads its session, made at that moment by the case's own writer: a trim's
// rename over the transcript (its inode), a subagent's thread cut in place (a
// shrink), a task file touched (a file that is no transcript, written) — and
// touched again where the box's find compares at a coarse grain, as busybox's
// does whole seconds, after the pack's marker — the transcript rewritten in
// place and grown back past its old size, as a withdrawn message rewrites it
// (no byte prefix), the transcript shrunk and grown back within the read as
// each tar packs that — GNU padding the member with NULs, busybox cutting the
// archive inside it, which its listing refuses as GNU's does — a transcript
// whose read back fails part way, past a listing that held, a file tar could
// not read left out of an archive that still reads back (missing), an archive
// that holds no file at all, a spilled result the harness removed (gone), a
// file made and removed as tar read the session (gone, a member no check of the
// manifest reads), and a task file written under the tar and removed before
// the check of what was written looked at it (gone, found by the last check).
// Every one is refused, whatever tar's exit said, naming its check and its
// file; and nothing is moved to the pack's destination, recorded as a digest
// the box holds, or left in its staging directory.
//
// Mutation: take tar's exit 0 or 1 on its own word, drop a check, run the last
// look for a removed member before the others, read an extraction that failed
// as the bytes it gave, read a listing of nothing as an archive, move the
// archive before the checks, or leave the staging directory behind, and a case
// goes red; drop the wait for the box's find to see the clock move past the
// marker, and the task file touched under a coarse find reads as written before
// it; hold the take of a job still working to the run's own end's rule and its
// cases do.
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
				// BUSYBOX'S ARCHIVE: a whole gzip, the tar inside it cut part way
				// into the member it read shrinking — the transcript, the pack's
				// first file, behind its 512-byte header — and nothing after it.
				// Listed through gunzip, as the pack lists it, the cut is an
				// error to busybox's tar ("short read") as to GNU's, which this
				// machine lists it with ("Unexpected EOF in archive").
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
			// THE WRITER AT THE CHECK OF WHAT WAS WRITTEN, the one find that
			// asks for files that are not transcripts (`! -name`), never the
			// walk before the marker, which asks for what is no link
			// (`! -type`): the task file goes just before that find reads the
			// session.
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
				if boxHas(t, b, pack.dest(p)) {
					t.Errorf("the refused pack left an archive at %s", pack.dest(p))
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

// heldPacks are the packs made beside a harness that may still work, each held
// to the rule of such a pack: a save, and a take of a job its box would not
// end, which packs it on demand (sandbox.SessionTake.Pack) — each with the
// path it moves its archive over and what it left of the directory it stages
// it in.
var heldPacks = []struct {
	name   string
	make   func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error
	dest   func(p executor.Paths) string
	staged func(t *testing.T, b sandbox.Sandbox) []string
}{
	{name: "a save",
		make: func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error {
			_, err := runner.PackSave(t.Context(), b, takerA)
			return err
		},
		dest: func(p executor.Paths) string { return p.SessionSave(takerA) },
		staged: func(t *testing.T, b sandbox.Sandbox) []string {
			if staging := paths(b).SessionSaveStaging(takerA); boxHas(t, b, staging) {
				return []string{staging}
			}
			return nil
		}},
	{name: "a take of a job still working",
		make: func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error {
			taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, MaxBytes: takeBound})
			if taken.Body != nil {
				_ = taken.Body.Close()
			}
			return err
		},
		dest:   func(p executor.Paths) string { return p.SessionCapture() },
		staged: packStagingLeft},
}

// packStagingLeft is every staging directory a pack on demand left in box: each
// is named for its pack alone (executor.Paths.SessionPackStaging), so it is
// found by its pattern rather than named.
func packStagingLeft(t *testing.T, box sandbox.Sandbox) []string {
	t.Helper()
	res, err := box.Exec(t.Context(), "for d in "+shQuote(paths(box).WorkDir())+"/session-pack-staging.*; do "+
		`if [ -e "$d" ] || [ -L "$d" ]; then printf '%s\n' "$d"; fi; done`, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("asking the box for its packs' staging directories: %v", err)
	}
	return strings.FieldsFunc(res.Stdout, func(r rune) bool { return r == '\n' })
}

// A PACK BESIDE A WORKING HARNESS PROVES THE SESSION'S OWN TRANSCRIPT WHATEVER
// ITS FIND READS OF IT. Claude Code rewrites that transcript in place where it
// withdraws a message (2.1.310's removeTranscriptMessage, read in its shipped
// code, truncates it and writes it again in the same inode), and a clock that
// steps back while a pack runs stamps such a rewrite before the pack's marker,
// so the box's find reads the file as unwritten since. Here the transcript is
// rewritten in place and grown back past its old size once tar has read it,
// under a find that reads it as unwritten since any file: every way the pack
// is made — a save, and the take of a job still working — reads the
// transcript back out of its archive all the same, finds it is not the start
// of the live file, and is refused saying so, with nothing moved to its
// destination, recorded as a digest the box holds, or left in its staging
// directory.
//
// Mutation: read the session's own transcript back only where find reads it as
// written since the marker, as a subagent's thread is, and the transcript
// rewritten under the stepped-back clock is packed torn.
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
			// A FIND THAT READS THE TRANSCRIPT AS UNWRITTEN SINCE ANY FILE, as
			// under a clock stepped back while the pack ran: every listing of
			// files newer than another leaves it out, and every other word
			// goes to this machine's find.
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
			if boxHas(t, b, pack.dest(p)) {
				t.Errorf("the refused pack left an archive at %s", pack.dest(p))
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

// stampedEntry is a file or a directory of a session a case stamps, named as
// the pack names it — relative to the harness's configuration directory — with
// what a file holds, and whether it is stamped an hour AHEAD of the box's clock
// or an hour behind it.
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

// stampedSession is session s as a box whose clock ran an hour ahead of this
// one's leaves it: its transcript, a subagent's thread and metadata, a spilled
// result, its task list and their directories, the newest of them stamped in
// this box's future and the oldest in its past, every line of them whole.
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

// stampInBox files session s in box b as [stampedSession] stamps it against
// now, under the harness's configuration directory: the copy a box keeps of a
// session whose newest files were stamped in its future — by a box whose clock
// ran ahead, the session carried here with the times its archive keeps, or by
// this box before its clock stepped back. Beside them it files a LINK among the
// session's spilled results, stamped an hour ahead, to a file in the box's home
// outside the session, stamped an hour behind: a box's own copy may hold one,
// since anything run in the box can make it. It returns the link and the file
// it names.
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
	// link's own by a call that does not follow it, as os.Chtimes would, to
	// the file it names.
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

// boxNow is the box's clock as it stamps a file: the time of a file named name
// written in b's home now, read by the clock that stamps the session's files,
// never by the finer one a test reads, which runs ahead of it.
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
// has it, once a pack was made of it that began at began and was done by ended,
// each as the box's clock stamps a file ([boxNow]): every file and directory of
// it stamped an hour ahead now stamped within the pack — no earlier than began
// and no later than ended — and every one stamped an hour behind still stamped
// so, to the second.
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

// linkLeftAlone checks link, a link in a session a pack was made of, and
// outside, the file outside the session it names, stamped an hour behind now:
// the link is a link still, and the file keeps its time to the second, the pack
// having set no time through the link.
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
// FUTURE TO ITS OWN START, AND HOLDS. A box cannot hold a file from its own
// future, and the pack's checks read one as written since its marker until the
// box's clock passes it: a task file that a box whose clock ran ahead stamped,
// carried here with its time, or that this box stamped before its clock stepped
// back, would fail every such pack for that long. So every pack, before its
// marker, sets every file and directory of the session that the box's find
// reads as newer than a reference it writes to the box's present, and leaves
// every earlier time as it is, which the harness's own retention sweep,
// held off, deletes nothing by; and it sets no time through a link, which touch
// would follow out of the session. Here the box's own copy of the session
// holds its transcript, a task file, a subagent's metadata and their
// directories stamped an hour ahead of the box's clock, a subagent's thread, a
// spilled result and its directory stamped an hour behind it, and among the
// spilled results a link stamped an hour ahead to a file outside the session
// stamped an hour behind: every way the pack is made — a save, and the take of
// a job still working — holds, and leaves each one that was ahead stamped no
// earlier than the pack began and no later than it was done, each one behind
// with its time to the second, directories included, the link a link, and the
// file outside the session with its own time.
//
// Mutation: drop the step that sets the times, and the pack is refused for a
// file written while the session was packed; set them once the marker is
// written and the box's clock has moved past it, and every file set to that
// later time reads as written since the marker, so the pack is refused for one
// too; set every time rather than those in the box's future, and a file an
// hour behind loses its own; set those in the future to a time long past
// rather than the present, and they end stamped before the pack began; set
// files alone, and a directory stamped an hour ahead stays in the box's
// future; drop `! -type l`, and the file outside the session takes the pack's
// time through the link to it.
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
// STILL REFUSED. The pack sets a file stamped in its box's future to the box's
// present, before its marker, and sets no time back after it, so a write the
// harness makes as tar reads that file is newer than the marker to every check,
// whether the file was set before it or not. Here a task file stamped an hour
// ahead is rewritten in place once tar has read the session: every way the
// pack is made — a save, and the take of a job still working — is refused,
// saying a file was written while the session was packed and naming the task
// file, with nothing moved to its destination, recorded as a digest the box
// holds, or left in its staging directory.
//
// Mutation: once tar has read the session, set every file newer than the
// reference back to the reference's time, and the task file rewritten under
// the tar is set behind the marker, and the pack holds.
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
			if boxHas(t, b, pack.dest(p)) {
				t.Errorf("the refused pack left an archive at %s", pack.dest(p))
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
// of one box's session can run at once — two takers' saves, as when a box's
// waiter duty changes hands mid-save, or a save and the take of a job still
// working — each in a staging directory of its own; and each, before its
// marker, sets every file of the session that the box's find reads as newer
// than a reference it writes, as a file stamped in the box's future is, to the
// box's present: never to an earlier time, at which a walk that ran late would
// set a file behind the other pack's marker. Here pack B's walk picks a task
// file stamped an hour ahead and is held between picking it and setting it, as
// a find held up on a busy box is, until pack A — its reference and marker
// written after B's reference — has had the harness rewrite that task file in
// place once A's tar read the session; only then does B set it, and only then
// do A's checks run. A is refused, saying a file was written while the session
// was packed and naming the task file, and B, whose marker is written after
// that write, holds.
//
// Mutation: set the files the walk picks to the reference's time rather than
// the present (`touch -c -r`), and B sets the task file rewritten under A's tar
// back behind A's marker, and A holds.
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
	claimed, picked := shQuote(flags+"/b-claimed"), flags+"/b-picked"
	rewritten, set := flags+"/rewritten", flags+"/b-set"
	sleep, touch := shQuote(hostTool(t, "sleep")), shQuote(hostTool(t, "touch"))
	// await is shell that waits for flag, thirty seconds at most, and then
	// exits with status where the flag never came, so an interleaving that
	// did not happen fails the case rather than pass it.
	await := func(flag, status string) string {
		return `i=0; while [ ! -e ` + shQuote(flag) + ` ] && [ "$i" -lt 300 ]; do ` + sleep + ` 0.1; ` +
			`i=$((i + 1)); done; [ -e ` + shQuote(flag) + ` ] || exit ` + status
	}
	// THE FIRST TOUCH OF A WALK IS B'S — the walk's alone passes -c, the
	// clock's does not — and it says B has picked the task file, then holds
	// until A's tar has rewritten it; every other touch is this machine's.
	writeBoxStandIn(t, b, p.BinDir()+"/touch", "#!/bin/sh\n"+
		`case " $* " in *" -c "*) if mkdir `+claimed+` 2> /dev/null; then : > `+shQuote(picked)+"; "+
		await(rewritten, "3")+"; "+touch+` "$@"; st=$?; : > `+shQuote(set)+`; exit "$st"; fi;; esac`+"\n"+
		"exec "+touch+` "$@"`+"\n")
	// A'S TAR rewrites the task file in place once it has read the session,
	// and hands A's checks the session only once B has set it.
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `case "$1" in `+
		shQuote(p.SessionSaveStaging(takerA))+`/*) "$tar" -czf "$@"; st=$?; printf '%s\n' `+
		`'{"id":"1","status":"completed"}' > `+shQuote(task)+`; : > `+shQuote(rewritten)+"; "+
		await(set, "2")+`; exit "$st";; esac`+"\n"+`exec "$tar" -czf "$@"`+"\n"))
	packedB := make(chan error, 1)
	go func() {
		_, err := runner.PackSave(t.Context(), b, takerB)
		packedB <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(picked); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pack B's walk never picked the task file stamped in the box's future")
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
	_, errA := runner.PackSave(t.Context(), b, takerA)
	errB := <-packedB
	for _, flag := range []string{rewritten, set} {
		if _, err := os.Stat(flag); err != nil {
			t.Fatalf("the packs never ran as the case has them: no %s (pack A: %v; pack B: %v)", flag, errA,
				errB)
		}
	}
	if want := "a file was written while the session was packed: " + f.task; errA == nil ||
		!strings.Contains(errA.Error(), want) {
		t.Errorf("pack A, under whose tar the task file was rewritten, answered %v once pack B's walk had set "+
			"it; want it refused saying %q", errA, want)
	}
	if boxHas(t, b, p.SessionSave(takerA)) {
		t.Errorf("the refused pack A left a save at %s", p.SessionSave(takerA))
	}
	if errB != nil {
		t.Errorf("pack B, whose marker came after the task file was rewritten, was refused: %v", errB)
	}
}

// TWO PACKS ON DEMAND OF ONE BOX AT ONCE BOTH HOLD, each staged in a directory
// of its own. Two reads of one job at once — a stop's and a collection's — each
// take its session, and each packs it now where the job left no archive or its
// box would not end it. Here the first pack's tar, once it has packed, holds
// until the second pack's has packed too, so the second pack starts — and
// clears and fills its staging directory — while the first is still at work:
// beside a harness that may still work and once it is gone alike, each pack
// holds, each hands back a whole archive of the session, and neither leaves a
// staging directory behind.
//
// Mutation: stage every pack on demand in one directory, and the second pack's
// start clears the first's files — one moves the other's archive over the
// job's capture as its own, and the other's move finds nothing to move — so one
// of the two is refused.
func TestTwoPacksOnDemandOfOneBoxAtOnceBothHold(t *testing.T) {
	t.Parallel()
	for _, working := range []bool{true, false} {
		name := "beside a harness that may still work"
		if !working {
			name = "once its harness is gone"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, "2.1.310")
			_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			p := paths(b)
			forgetCapture(t, b)
			flags := filepath.Join(b.Home(), "two-at-once")
			if err := os.MkdirAll(flags, 0o700); err != nil {
				t.Fatal(err)
			}
			first, second := flags+"/first-packed", flags+"/second-packed"
			// THE FIRST TAR TO CLAIM IS THE FIRST PACK'S: it packs, says so, and
			// holds — thirty seconds at most, and then fails, so an interleaving
			// that did not happen fails the case — until the second pack's tar
			// has packed and said so.
			writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `if mkdir `+shQuote(flags+"/claimed")+
				` 2> /dev/null; then "$tar" -czf "$@"; st=$?; : > `+shQuote(first)+`; i=0; while [ ! -e `+
				shQuote(second)+` ] && [ "$i" -lt 300 ]; do `+shQuote(hostTool(t, "sleep"))+` 0.1; `+
				`i=$((i + 1)); done; [ -e `+shQuote(second)+` ] || exit 3; exit "$st"; fi`+"\n"+
				`"$tar" -czf "$@"; st=$?; : > `+shQuote(second)+`; exit "$st"`+"\n"))
			type packed struct {
				body io.ReadCloser
				err  error
			}
			packs := make(chan packed, 2)
			for range 2 {
				go func() {
					_, body, err := runner.PackCapture(t.Context(), b, working)
					packs <- packed{body: body, err: err}
				}()
			}
			results := []packed{<-packs, <-packs}
			for _, got := range results {
				if got.body != nil {
					t.Cleanup(func() { _ = got.body.Close() })
				}
			}
			for _, flag := range []string{first, second} {
				if _, err := os.Stat(flag); err != nil {
					t.Errorf("the packs never ran as the case has them: no %s", flag)
				}
			}
			for _, got := range results {
				switch {
				case got.err != nil:
					t.Errorf("a pack made beside another of the same box was refused: %v", got.err)
				case got.body == nil:
					t.Error("a pack made beside another of the same box handed back no archive")
				default:
					if _, ok := archiveMembers(t, got.body)[filesOf(ran.SessionID).transcript]; !ok {
						t.Error("a pack made beside another of the same box handed back no transcript")
					}
				}
			}
			if left := packStagingLeft(t, b); len(left) > 0 {
				t.Errorf("the packs left their staging directories: %s", strings.Join(left, ", "))
			}
		})
	}
}

// A SAVE OF A SESSION THAT ONLY GREW AS IT WAS PACKED HOLDS — the one change a
// pack beside a working harness takes. As tar reads the session, the harness
// appends to its transcript (its last line left torn, as an append cut short
// leaves it) and to a subagent's thread, starts a thread of a new subagent in a
// directory of its own with its metadata beside it, and spills a result to a
// file — the last two no transcripts, written after tar read their
// directories, so in no archive. Tar says so as each tar does — GNU's exit 1,
// "file changed as we read it", beside a find that compares the clock's own
// grain, and busybox's exit 0 and nothing, beside a find of a coarse grain —
// and both list a directory with the slash they end one in (measured on GNU
// tar 1.35 and BusyBox 1.37.0), which no file find names matches; a tar that
// lists one without it is played too, so that the check of what was written
// reads files alone, however a box's tar names a directory: the directories
// that gained a thread and a spilled result are newer than the marker, and
// listed so they match what find names. Every way the save is made, holding
// each transcript as tar first read it — a whole prefix of the live file — and
// nothing made after tar read its directory.
//
// Mutation: refuse tar's exit 1 where every check holds, hold a `.jsonl` to
// the check that a file was written since the marker, compare a grown
// transcript's whole live file rather than its first as many bytes as the
// archive holds, count a thread made after the tar as one the archive lost,
// or hold a file written after tar read its directory to the check of what
// was written though no archive holds it, and a save is refused; read a
// directory newer than the marker as a file written, and the save beside the
// tar that lists a directory without its slash is.
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
			if _, err := runner.PackSave(t.Context(), b, takerA); err != nil {
				t.Fatalf("the save of a session that only grew as it was packed was refused: %v", err)
			}
			members := archiveMembers(t, strings.NewReader(boxFile(t, b, p.SessionSave(takerA))))
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
// BY. No harness is left to write under it, so neither the run's own end nor a
// take of a job that is over whose script left no archive waits on the box's
// clock or refuses a file written as it packed — here the task file, touched
// once tar has read the session, as a process the harness started and that
// outlived it may touch it — and each lands where the job's final archive is
// read. (The take packs such a job by tar run beside the configuration
// directory, never in it, so the case's writer names the file whole.)
//
// Mutation: hold the run's own end to the rule of a pack beside a working
// harness, or route a take of a job that is over to it, and that pack is
// refused.
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
		if taken.Archive.Path != p.SessionCapture() {
			t.Errorf("the take packed to %s, want the job's capture %s", taken.Archive.Path, p.SessionCapture())
		}
	})
}

// A PACK BESIDE A WORKING HARNESS FAILS WHERE ITS BOX'S CLOCK NEVER MOVES,
// rather than pack blind. Its checks tell a file written as tar read it by
// the box's own find reading that file as newer than a marker written before,
// so the pack first waits for that find to read a file written now as newer
// than the marker; a box whose find never does — a clock that does not move,
// or files stamped with none — is refused once the wait's bound has passed,
// saying so, and tar is never asked to pack.
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
	if boxHas(t, b, p.SessionSave(takerA)) {
		t.Error("the refused save left an archive")
	}
}

// A PACK BESIDE A WORKING HARNESS FAILS ON A BOX WHOSE SLEEP TAKES NO TENTH OF
// A SECOND, and says so. Its wait for the box's clock counts in tenths, as
// every wait the engine's own scripts make does; a sleep that refuses one would
// run that count out at once, and read a box whose clock moves as one whose
// clock never did. So the refused tenth fails the pack by its own name, and tar
// is never asked to pack.
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
	if boxHas(t, b, p.SessionSave(takerA)) {
		t.Error("the refused save left an archive")
	}
}

// A PACK BESIDE A WORKING HARNESS NAMES A TOOL ITS BOX LACKS, rather than
// refuse on what that lack makes another check read. Without find, the wait for
// the box's clock would read a clock that never moved; without touch, the pack
// could set no file from the box's future back, nor write the file its wait
// reads the clock by, and would be refused as a pack that could not write its
// marker. So the pack asks the box for every tool it reads the session with,
// and a box whose PATH holds all of them but find, or all but touch, is told
// which it lacks, its session left unpacked. The pack runs here as the runner
// hands it to a box's login shell, under that PATH alone.
//
// Mutation: drop the pack's ask for its tools, and the box without find is
// refused as a box whose clock never moved; leave touch out of the tools it
// asks for, and the box without touch is refused as a pack that could not
// write its marker.
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
			cmd := exec.CommandContext(t.Context(), "sh", "-c", executor.ClaudeCode{}.PackSave(p, session, takerA)) //nolint:gosec // the script under test
			cmd.Env = []string{"PATH=" + bin}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || !strings.Contains(stderr.String(), "this box has no "+lacking) {
				t.Fatalf("the pack in a box without %s ended %v, want it refused naming %s:\n%s", lacking, err,
					lacking, stderr.String())
			}
			if _, err := os.Stat(p.SessionSave(takerA)); !os.IsNotExist(err) {
				t.Errorf("the pack in a box without %s left a save (%v)", lacking, err)
			}
		})
	}
}

// A SAVE THAT FAILS LEAVES THE LAST ONE STANDING. A taker's save lands; the
// harness goes on, and writes its task file as the taker's next save is
// packed, which is refused — and the taker's place still holds the save before
// it, byte for byte, with no digest added for an archive that never landed and
// no staging directory left behind; a later save that holds replaces it.
//
// Mutation: move the staged archive over the taker's path before its checks,
// or on a failure, and the last save is gone; remove the staging directory only
// where the pack held, and the refused one stays.
func TestAFailedSaveLeavesTheLastOneStanding(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	if _, err := runner.PackSave(t.Context(), b, takerA); err != nil {
		t.Fatalf("the first save: %v", err)
	}
	last := boxFile(t, b, p.SessionSave(takerA))
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
	if got := boxFile(t, b, p.SessionSave(takerA)); got != last {
		t.Error("the refused save replaced the taker's last one")
	}
	if got := boxFile(t, b, p.SessionDigests()); got != digests {
		t.Errorf("the refused save recorded a digest: %q, was %q", got, digests)
	}
	if boxHas(t, b, p.SessionSaveStaging(takerA)) {
		t.Error("the refused save left its staging directory behind")
	}

	if _, err := b.Exec(t.Context(), "rm -f "+shQuote(p.BinDir()+"/tar"), sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.PackSave(t.Context(), b, takerA); err != nil {
		t.Fatalf("the save after it: %v", err)
	}
	next := boxFile(t, b, p.SessionSave(takerA))
	members := archiveMembers(t, strings.NewReader(next))
	if next == last || !strings.Contains(members[files.transcript], "after the first save") {
		t.Errorf("the save that held did not replace the last one: %q", members[files.transcript])
	}
}

// A SAVE IS NOT PACKED PAST WHAT THE SCRUB WOULD READ. A session whose files
// add up past what one reading of an archive yields a kept session's scrub —
// its spilled results grown past it, sparse here so the case writes nothing
// near it — would be refused by the take once packed, so the pack refuses it
// first, saying so, and tar is never asked to pack it; the same session
// without that file is saved.
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
	if boxHas(t, b, p.SessionSave(takerA)) {
		t.Error("the refused save left an archive")
	}

	if _, err := b.Exec(t.Context(), "rm -f "+shQuote(big), sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.PackSave(t.Context(), b, takerA); err != nil {
		t.Fatalf("the session within the bound was not saved: %v", err)
	}
}

// THE CLEAR BEFORE EVERY RUN REMOVES THE SAVES' FILES, whoever took them: two
// takers' saves and the staging directory a save cut short left, none of which
// a later job's take may read as its own, and nothing the box keeps for its
// life — the digests of the archives its copy holds stay.
//
// Mutation: leave the saves out of the clear, or quote their pattern so it
// matches nothing, and a file outlives it.
func TestTheClearBeforeEveryRunRemovesTheSavesFiles(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	for _, taker := range []string{takerA, takerB} {
		if _, err := runner.PackSave(t.Context(), b, taker); err != nil {
			t.Fatalf("saving for %s: %v", taker, err)
		}
	}
	// A SAVE CUT SHORT, its node stopped as it packed.
	if err := b.WriteFile(t.Context(), p.SessionSaveStaging(takerA)+"/capture", []byte("part of a save")); err != nil {
		t.Fatal(err)
	}
	digests := boxFile(t, b, p.SessionDigests())
	if err := runner.ClearArtifacts(t.Context(), b); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	for _, left := range []string{p.SessionSave(takerA), p.SessionSave(takerB), p.SessionSaveStaging(takerA)} {
		if boxHas(t, b, left) {
			t.Errorf("%s outlived the clear", left)
		}
	}
	if got := boxFile(t, b, p.SessionDigests()); got != digests || got == "" {
		t.Errorf("the clear changed the digests the box keeps for its life: %q, was %q", got, digests)
	}
}

// THE CLEAR BEFORE EVERY RUN REMOVES EVERY PACK ON DEMAND'S STAGING DIRECTORY,
// whichever read made it: each is named for its pack alone
// (executor.Paths.SessionPackStaging), so the clear removes what matches their
// pattern, and two that packs cut short left — their node stopped as they
// packed — outlive no clear.
//
// Mutation: leave the packs' staging directories out of the clear, or quote
// their pattern so it matches nothing, and they outlive it.
func TestTheClearBeforeEveryRunRemovesEveryPacksStagingDirectory(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	p := paths(b)
	cut := []string{p.SessionPackStaging("a-pack-cut-short"), p.SessionPackStaging("another-cut-short")}
	for _, staged := range cut {
		if err := b.WriteFile(t.Context(), staged+"/capture", []byte("part of a pack")); err != nil {
			t.Fatal(err)
		}
	}
	if got := packStagingLeft(t, b); len(got) != len(cut) {
		t.Fatalf("the box holds %d packs' staging directories before the clear, want the %d the case left: %s",
			len(got), len(cut), strings.Join(got, ", "))
	}
	if err := runner.ClearArtifacts(t.Context(), b); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	if left := packStagingLeft(t, b); len(left) > 0 {
		t.Errorf("the clear left packs' staging directories: %s", strings.Join(left, ", "))
	}
}
