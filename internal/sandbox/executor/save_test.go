package executor_test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

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

// boxHas reports whether anything is at path in box, a file, a directory or a
// link.
func boxHas(t *testing.T, box sandbox.Sandbox, path string) bool {
	t.Helper()
	res, err := box.Exec(t.Context(), "[ -e "+shQuote(path)+" ] || [ -L "+shQuote(path)+" ]", sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("asking the box for %s: %v", path, err)
	}
	return res.ExitCode == 0
}

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

// rewritingTar is a stand-in `tar` for a box's bin directory, which a pack's
// PATH puts first: every call but a pack's (`tar -czf <archive> <files...>`)
// goes to this machine's tar, and a pack's runs pack — a script with that tar
// as "$tar", the archive as "$1" and the files after it, run where the pack
// runs tar, in the harness's configuration directory — so the case's own writer
// changes the session at exactly the moment the case names, as a harness would
// while tar read it.
func rewritingTar(t *testing.T, pack string) string {
	t.Helper()
	tar, err := exec.LookPath("tar")
	if err != nil {
		t.Fatalf("no tar on this machine's PATH: %v", err)
	}
	return "#!/bin/sh\ntar=" + shQuote(tar) + "\n" +
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

// wholeSecondFind is a stand-in `find` for a box's bin directory that reads
// `-newer` as busybox's find does, by whole seconds — a file is newer than the
// reference only where its time is in a later second (measured on BusyBox
// 1.37.0: a file written 200 ms after the reference, within its second, is
// not) — through this machine's own find, `-newermt` a nanosecond short of the
// reference's next second; every other word goes to that find as it is.
func wholeSecondFind(t *testing.T) string {
	t.Helper()
	find, err := exec.LookPath("find")
	if err != nil {
		t.Fatalf("no find on this machine's PATH: %v", err)
	}
	return "#!/bin/sh\nfind=" + shQuote(find) + "\nn=$# prev=\n" +
		`for a; do if [ "$prev" = -newer ]; then s=$(date -r "$a" +%s) || exit 1; set -- "$@" "@$s.999999999"; ` +
		`elif [ "$a" = -newer ]; then set -- "$@" -newermt; else set -- "$@" "$a"; fi; prev=$a; done` + "\n" +
		`shift "$n"` + "\n" + `exec "$find" "$@"` + "\n"
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
// and then another: each save is moved into that taker's own place, named by a
// digest of the taker rather than the taker itself, its staging directory gone
// and its digest kept in the box; neither touches the job's capture, so the
// read a collection makes of the job's final archive finds none, and the final
// take packs the job's own; a take of a save reads that taker's alone, packs
// nothing, and finds nothing for a taker that saved none.
//
// Mutation: move a save over the job's capture, as a pack on demand moves its
// archive, and the collection's read finds it; name one path for every taker
// and the second save replaces the first; take a save by packing one, or the
// job's final archive from a save's path, and its check goes red.
func TestASaveIsPackedToItsTakersOwnPathAndNeverTakenAsTheJobsFinal(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	forgetCapture(t, b)

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
	first := []byte(boxFile(t, b, p.SessionSave(takerA)))
	if digests := boxFile(t, b, p.SessionDigests()); !strings.Contains(digests,
		ran.SessionID+" "+digestOf(t, first)+"\n") {
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

	// A TAKE OF A SAVE reads its taker's alone, and packs nothing.
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
	if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: takerA, Pack: true,
		MaxBytes: takeBound}); err == nil {
		t.Error("a take of a save that asks to pack was made")
	}
	if _, err := runner.PackSave(t.Context(), b, ""); err == nil {
		t.Error("a save naming no taker was packed")
	}
}

// A SAVE OF A FILE REWRITTEN WHILE IT WAS PACKED FAILS, AND SAYS WHICH CHECK
// REFUSED IT — and so does every pack made beside a harness that may still
// work, a job its box would not end packed on demand. Each case is a write the
// harness makes as tar reads its session, made at that moment by the case's own
// writer: a trim's rename over the transcript (its inode), a subagent's thread
// cut in place (a shrink), a task file touched (a file that is no transcript,
// written) — and touched again where the box's find compares whole seconds, as
// busybox's does, within a moment of the pack's marker — the transcript
// rewritten in place and grown back past its old size, as a withdrawn message
// rewrites it (no byte prefix), the transcript shrunk and grown back within the
// read as each tar packs that — GNU padding the member with NULs, busybox
// cutting the archive inside it — a file left out of an archive that still
// reads back, as busybox's listing reads the archive it cut (missing), and a
// spilled result the harness removed (gone). Every one is refused, whatever
// tar's exit said, naming its check and its file; and nothing is moved to the
// pack's destination or recorded as a digest the box holds.
//
// Mutation: take tar's exit 0 or 1 on its own word, drop a check, or move the
// archive before the checks, and a case goes red; drop the wait for the box's
// find to see the clock move past the marker, and the task file touched within
// the marker's second reads to a whole-second find as written before it; hold
// the pack on demand to the run's own end's rule and its cases do.
func TestASaveOfAMemberRewrittenWhileItWasPackedFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		pack  func(f sessionFiles) string
		check func(f sessionFiles) string
		// wholeSeconds runs the pack under a find that compares whole
		// seconds ([wholeSecondFind]).
		wholeSeconds bool
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
		{name: "a task file touched, to a find that compares whole seconds as busybox's does",
			pack: func(f sessionFiles) string { return afterTar(`touch ` + shQuote(f.task)) },
			check: func(f sessionFiles) string {
				return "a file was written while the session was packed: " + f.task
			},
			wholeSeconds: true},
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
				return `archive=$1; shift; n=$(wc -c < ` + tr + `); h=$((n / 2))` + "\n" +
					`"$tar" -cf - "$@" | head -c "$((512 + h))" | gzip > "$archive"` + "\n" +
					grownBack(f.transcript) + "\n" +
					`echo 'tar: short read' >&2; exit 1` + "\n"
			},
			check: func(sessionFiles) string { return "the archive the pack made could not be read back" }},
		{name: "a file left out of an archive that reads back, as busybox's tar leaves every member after one " +
			"it read shrinking",
			pack: func(sessionFiles) string {
				// THE ARCHIVE WHOLE BUT FOR THE TASK LIST, the pack's last
				// operand, which busybox's tar never reaches once a member
				// before it shrank; its own listing of such an archive reads
				// without an error.
				return `archive=$1; shift; "$tar" -czf "$archive" "$1" "$2" || exit 2` + "\n" +
					`echo 'tar: short read' >&2; exit 1` + "\n"
			},
			check: func(f sessionFiles) string { return "a file is not in the archive the pack made: " + f.task }},
		{name: "a spilled result removed, as the harness's own sweep removes one",
			pack: func(f sessionFiles) string { return afterTar(`rm -f ` + shQuote(f.spilled)) },
			check: func(f sessionFiles) string {
				return "a file was removed while the session was packed: " + f.spilled
			}},
	}
	packs := []struct {
		name string
		make func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error
		dest func(p executor.Paths) string
	}{
		{name: "a save",
			make: func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error {
				_, err := runner.PackSave(t.Context(), b, takerA)
				return err
			},
			dest: func(p executor.Paths) string { return p.SessionSave(takerA) }},
		{name: "a pack on demand",
			make: func(t *testing.T, runner *executor.Runner, b sandbox.Sandbox) error {
				_, body, err := runner.PackCapture(t.Context(), b)
				if body != nil {
					_ = body.Close()
				}
				return err
			},
			dest: func(p executor.Paths) string { return p.SessionCapture() }},
	}
	for _, tc := range cases {
		for _, pack := range packs {
			t.Run(pack.name+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				runner, b := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				p := paths(b)
				forgetCapture(t, b)
				files := filesOf(ran.SessionID)
				writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, tc.pack(files)))
				if tc.wholeSeconds {
					writeBoxStandIn(t, b, p.BinDir()+"/find", wholeSecondFind(t))
				}
				err := pack.make(t, runner, b)
				if want := tc.check(files); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("the pack answered %v, want it refused saying %q", err, want)
				}
				if boxHas(t, b, pack.dest(p)) {
					t.Errorf("the refused pack left an archive at %s", pack.dest(p))
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

// A SAVE OF A SESSION THAT ONLY GREW AS IT WAS PACKED HOLDS — the one change a
// pack beside a working harness takes. As tar reads the session, the harness
// appends to its transcript (its last line left torn, as an append cut short
// leaves it) and to a subagent's thread, and starts a thread of a new subagent
// in a directory of its own; tar says so as each tar does — GNU's exit 1, "file
// changed as we read it", beside a find that compares the clock's own grain,
// and busybox's exit 0 and nothing, beside a find that compares whole seconds.
// Either way the save is made, holding each transcript as tar first read it — a
// whole prefix of the live file — and no thread made after tar read its
// directory.
//
// Mutation: refuse tar's exit 1 where every check holds, hold a `.jsonl` to
// the check that a file was written since the marker, compare a grown
// transcript's whole live file rather than its first as many bytes as the
// archive holds, or count a thread made after the tar as one the archive lost,
// and a save is refused.
func TestASaveOfASessionThatOnlyGrewAsItWasPackedHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		wholeSeconds bool
		said         string
	}{
		{name: "as GNU's tar and find read it",
			said: `echo "tar: $grown: file changed as we read it" >&2; exit 1`},
		{name: "as busybox's tar and find read it", wholeSeconds: true, said: `exit 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, "2.1.310")
			_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			p := paths(b)
			f := filesOf(ran.SessionID)
			born := "projects/" + executor.ClaudeProjectDir + "/" + ran.SessionID + "/subagents/workflow/agent-9.jsonl"
			writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `"$tar" -czf "$@" || exit 2`+"\n"+
				`grown=`+shQuote(f.transcript)+"\n"+
				`printf '%s\n%s' '{"type":"user","grew":"after tar read it"}' '{"type":"assist' >> "$grown"`+"\n"+
				`echo '{"type":"user","more":1}' >> `+shQuote(f.thread)+"\n"+
				`mkdir -p "$(dirname `+shQuote(born)+`)" && echo '{"type":"user"}' > `+shQuote(born)+"\n"+
				tc.said+"\n"))
			if tc.wholeSeconds {
				writeBoxStandIn(t, b, p.BinDir()+"/find", wholeSecondFind(t))
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
			if _, ok := members[born]; ok {
				t.Error("the save holds a thread made after tar read its directory")
			}
		})
	}
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
	find, err := exec.LookPath("find")
	if err != nil {
		t.Fatalf("no find on this machine's PATH: %v", err)
	}
	// A FIND TO WHICH NOTHING IS EVER NEWER, and a sleep that takes no time,
	// so the wait reaches its bound at once.
	writeBoxStandIn(t, b, p.BinDir()+"/find", "#!/bin/sh\nfor a; do [ \"$a\" != -newer ] || exit 0; done\n"+
		"exec "+shQuote(find)+` "$@"`+"\n")
	writeBoxStandIn(t, b, p.BinDir()+"/sleep", "#!/bin/sh\nexit 0\n")
	asked := b.Home() + "/tar-asked"
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, `echo "$@" >> `+shQuote(asked)+"\n"+
		`exec "$tar" -czf "$@"`+"\n"))
	_, err = runner.PackSave(t.Context(), b, takerA)
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

// A SAVE THAT FAILS LEAVES THE LAST ONE STANDING. A taker's save lands; the
// harness goes on, and writes its task file as the taker's next save is
// packed, which is refused — and the taker's place still holds the save before
// it, byte for byte, with no digest added for an archive that never landed; a
// later save that holds replaces it.
//
// Mutation: move the staged archive over the taker's path before its checks,
// or on a failure, and the last save is gone.
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
