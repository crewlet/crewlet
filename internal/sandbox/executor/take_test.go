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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// takeBound is the archive bound every take here is made within: a session's.
const takeBound = executorkeep.SessionMaxBytes

// keptAndOpened is a taken session as the fleet keeps it and a later launch
// opens it: sealed under the keyring into a spool, uploaded to the object store,
// and read back whole through the session store — so what a case greps is what
// a later launch would carry into its box, and never the stream before its
// seal.
func keptAndOpened(t *testing.T, taken sandbox.TakenSession) []byte {
	t.Helper()
	at := func() time.Time { return time.Now().UTC() }
	objects, err := objstore.NewStoreAt(memobj.NewAt(at), at)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewStreamSealer(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": bytes.Repeat([]byte("1"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := executordomain.NewSessionStore(objects, sealer)
	if err != nil {
		t.Fatal(err)
	}
	turn := statelog.NewOpID(time.Now(), "")
	var spool bytes.Buffer
	sealed, err := store.Seal("ana", turn, taken.Body, &spool)
	if err != nil {
		t.Fatalf("sealing the taken session: %v", err)
	}
	put, err := store.Upload(t.Context(), sealed, &spool)
	if err != nil {
		t.Fatalf("uploading the sealed session: %v", err)
	}
	opened, err := store.Open(t.Context(), executordomain.Session{Handle: "ana", Turn: turn, Object: put.Object})
	if err != nil {
		t.Fatalf("opening the kept session: %v", err)
	}
	defer func() { _ = opened.Close() }()
	body, err := io.ReadAll(opened)
	if err != nil {
		t.Fatalf("reading the kept session: %v", err)
	}
	return body
}

// The secrets a planting harness writes into every kind of file a Claude Code
// session holds.
const (
	// plantedSecret is a value the engine put in the box — an operator's
	// opaque token — which no shape redact knows matches.
	plantedSecret = "planted-token-0123456789abcdef"
	// plantedQuoted is one holding a quote, which a transcript holds only
	// escaped.
	plantedQuoted = `planted"quote-0123456789`
	// plantedShape is a credential the engine never saw, which only its
	// shape gives away.
	plantedShape = "ghp_" + "Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4zAb7d"
	// plantedKeyed is an opaque value under a password's own key.
	plantedKeyed = "opaque-but-keyed-value"
)

// plantingHarness is a stand-in `claude` that files its session where the pinned
// layout names, as [sessionHarness] does, with a planted secret in every kind of
// file a session holds: the transcript (raw, JSON-escaped, and as the key a
// tool's structured result names a member by), a subagent's transcript, a tool
// result spilled to a text file — and one named after the secret — and a task
// list.
func plantingHarness() string {
	return `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
p=$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME
mkdir -p "$p/$s/subagents" "$p/$s/tool-results" "$CLAUDE_CONFIG_DIR/tasks/$s"
printf '{"type":"user","sessionId":"%s"}\n' "$s" >> "$p/$s.jsonl"
cat >> "$p/$s.jsonl" <<'EOF'
{"type":"assistant","message":{"content":[{"type":"text","text":"the token is ` + plantedSecret + `"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"value: ` + strings.ReplaceAll(plantedQuoted, `"`, `\"`) + ` end"}]}}
{"type":"user","toolUseResult":{"` + plantedSecret + `":"named by it"}}
EOF
echo '{"type":"user","text":"` + plantedSecret + `"}' > "$p/$s/subagents/agent-1.jsonl"
printf 'output %s and %s done\n' '` + plantedSecret + `' '` + plantedShape + `' > "$p/$s/tool-results/out.txt"
echo spilled > "$p/$s/tool-results/` + plantedSecret + `.txt"
echo '{"subject":"` + plantedSecret + `","password":"` + plantedKeyed + `"}' > "$CLAUDE_CONFIG_DIR/tasks/$s/1.json"
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// A KEPT SESSION CARRIES NO SECRET THE ENGINE PUT IN ITS BOX, AND STAYS THE
// ARCHIVE ITS HARNESS RESTORES. A run on a real box files its session with a
// planted secret in a transcript line, a subagent's transcript, a spilled tool
// result, a file's name and a task list; taken as the run's script packed it,
// and packed again on demand, then sealed, uploaded and OPENED from the store
// as a later launch opens it, the archive holds none of them — not the value
// the engine named, raw, as JSON escapes it or as a key, not a credential only
// its shape gives away, not a value under a password's key — while every
// transcript line is still JSON, every member still there, and the box keeps
// the digest of exactly the bytes that left it.
//
// Mutation: skip the by-value pass, its JSON-escaped form, the keys' rewrite,
// the members' names, the redaction, the keyed mask, or the digest's record,
// or let the rewrite break a line, and a check here goes red.
func TestAKeptSessionCarriesNoSecretTheEnginePutInItsBox(t *testing.T) {
	t.Parallel()
	runner, b := realBox(t)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", plantingHarness())
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	if res.Captured == nil {
		t.Fatalf("the run's session was not captured:\n%s", boxFile(t, b, paths(b).Err()))
	}
	for _, pack := range []bool{false, true} {
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{
			Pack: pack, Scrub: []string{plantedSecret, plantedQuoted}, MaxBytes: takeBound})
		if err != nil || taken.Body == nil {
			t.Fatalf("pack %v: TakeSession = %+v, %v", pack, taken, err)
		}
		archive := keptAndOpened(t, taken)
		if err = taken.Body.Close(); err != nil {
			t.Fatalf("pack %v: closing the taken session: %v", pack, err)
		}
		members := archiveMembers(t, bytes.NewReader(archive))
		session := res.SessionID
		project := "projects/" + executor.ClaudeProjectDir + "/"
		for _, want := range []string{project + session + ".jsonl", project + session + "/subagents/agent-1.jsonl",
			project + session + "/tool-results/out.txt", "tasks/" + session + "/1.json"} {
			if _, ok := members[want]; !ok {
				t.Errorf("pack %v: the kept session lost %s", pack, want)
			}
		}
		for name, body := range members {
			for _, secret := range []string{plantedSecret, plantedQuoted, `planted\"quote`, plantedShape, plantedKeyed} {
				if strings.Contains(body, secret) || strings.Contains(name, secret) {
					t.Errorf("pack %v: %s still holds %q:\n%s", pack, name, secret, body)
				}
			}
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
				if !json.Valid([]byte(line)) {
					t.Errorf("pack %v: %s has a line that is no longer JSON: %q", pack, name, line)
				}
			}
		}
		if !strings.Contains(members[project+session+".jsonl"], `"sessionId":"`+session+`"`) {
			t.Errorf("pack %v: the transcript lost what it was: %q", pack, members[project+session+".jsonl"])
		}
		if err = taken.Keep(t.Context()); err != nil {
			t.Fatalf("pack %v: Keep: %v", pack, err)
		}
		if digests := boxFile(t, b, paths(b).SessionDigests()); !strings.Contains(digests,
			session+" "+digestOf(t, archive)+"\n") {
			t.Errorf("pack %v: the box keeps no digest of the archive that left it: %q", pack, digests)
		}
	}
}

// A JOB THAT NEVER PACKED IS PACKED ON DEMAND BY THE RUNNER'S OWN PACK, which
// keeps its digest in the box as the run's script's pack does: the archive is
// the session's whole, the box knows it holds it, and it is the read's own —
// at the pack's own place, never over the job's capture, which only the job's
// script writes — and gone once the read is done with it.
//
// Mutation: pack anywhere but the runner, or leave the digest out of the pack,
// and the box takes its own archive for another box's; move the pack over the
// job's capture, as a pack of a job that is over once did, and its archive is
// found there.
func TestAPackOnDemandKeepsItsDigestInTheBox(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	// THE SCRIPT'S OWN ARCHIVE GONE, as a job killed before its harness
	// exited never left one.
	if _, err := b.Exec(t.Context(), "rm -f "+paths(b).SessionCapture()+" "+paths(b).SessionDigests(),
		sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	described, body, release, err := runner.PackCapture(t.Context(), b, false)
	if err != nil || body == nil {
		t.Fatalf("PackCapture = %v, %v", body, err)
	}
	if described.SessionID != res.SessionID || !strings.HasPrefix(described.Path,
		paths(b).WorkDir()+"/session-pack-staging.") || !strings.HasSuffix(described.Path, ".held") {
		t.Errorf("the pack describes %+v, want session %s at a place of the pack's own", described, res.SessionID)
	}
	members := archiveMembers(t, body)
	_ = body.Close()
	if _, ok := members["projects/"+executor.ClaudeProjectDir+"/"+res.SessionID+".jsonl"]; !ok {
		t.Errorf("the pack holds no transcript: %v", members)
	}
	archive := []byte(boxFile(t, b, described.Path))
	if digests := boxFile(t, b, paths(b).SessionDigests()); !strings.Contains(digests,
		res.SessionID+" "+digestOf(t, archive)+"\n") {
		t.Errorf("the pack kept no digest of the archive it made: %q", digests)
	}
	if left := boxFile(t, b, paths(b).SessionCapture()); left != "" {
		t.Errorf("the job's capture holds %d bytes after a pack on demand: the pack moved its archive there",
			len(left))
	}
	release(t.Context())
	if left := packStagingLeft(t, b); len(left) > 0 {
		t.Errorf("the pack left its files once its read was done: %s", strings.Join(left, ", "))
	}
}

// A TAKE IS NEVER READ OUT OF ITS BOX UNBOUNDED: it names the bound its archive
// is kept at, an archive the box describes as past it is refused before a byte
// of it is read, and a take that names no bound is refused outright.
//
// Mutation: drop the bound's check, or read an archive before it, and the
// archive a byte past its bound is taken.
func TestATakeIsBoundedByTheArchiveItKeeps(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	if res.Captured == nil {
		t.Fatalf("the run's session was not captured:\n%s", boxFile(t, b, paths(b).Err()))
	}
	if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{MaxBytes: res.Captured.Size - 1}); err == nil {
		t.Error("an archive a byte past the take's bound was taken")
	}
	if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{}); err == nil {
		t.Error("a take naming no bound was made")
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{MaxBytes: res.Captured.Size})
	if err != nil || taken.Body == nil {
		t.Fatalf("an archive at the take's bound: %+v, %v", taken, err)
	}
	if err := taken.Body.Close(); err != nil {
		t.Fatalf("closing the taken session: %v", err)
	}
}

// A PACK THAT FAILS IS AN ERROR, NEVER "NO SESSION"; A SESSION THERE IS NOTHING
// OF IS NOTHING. A box whose tar breaks — its record naming a session its
// harness wrote — answers why, so the capture is stamped refused and a later
// launch knows the job's session is missing rather than continuing an older one
// as its last, and the error carries the pack's whole word, though it names a
// file nested deep in the session; a box whose harness wrote no transcript
// answers no session at all, which is no refusal.
//
// Mutation: read a failed pack's exit as nothing to pack, or a missing
// transcript as a failure, or cut the pack's word at a transcript line's
// bound, and a case here goes red.
func TestAPackThatFailsIsAnErrorAndNoTranscriptIsNoSession(t *testing.T) {
	t.Parallel()
	t.Run("a tar that breaks", func(t *testing.T) {
		t.Parallel()
		runner, b := sessionBox(t, "2.1.310")
		runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
		writeBoxStandIn(t, b, paths(b).BinDir()+"/tar", "#!/bin/sh\necho 'tar: the disk broke' >&2\nexit 2\n")
		described, body, _, err := runner.PackCapture(t.Context(), b, false)
		if err == nil || body != nil || !strings.Contains(err.Error(), "the disk broke") {
			t.Fatalf("PackCapture with a tar that breaks = %+v, %v, %v; want an error saying why", described, body, err)
		}
		if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true,
			MaxBytes: takeBound}); err == nil {
			t.Fatal("a take whose pack failed answered no error")
		}
	})
	t.Run("a tar that breaks on a file nested deep in the session", func(t *testing.T) {
		t.Parallel()
		runner, b := sessionBox(t, "2.1.310")
		_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
		said := "tar: projects/" + executor.ClaudeProjectDir + "/" + ran.SessionID +
			"/subagents/workflow/agent-a1b2c3d4e5f6a7b8c.jsonl: Cannot open: Permission denied"
		writeBoxStandIn(t, b, paths(b).BinDir()+"/tar", "#!/bin/sh\necho "+shQuote(said)+" >&2\nexit 2\n")
		_, body, _, err := runner.PackCapture(t.Context(), b, false)
		if body != nil {
			_ = body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), said) {
			t.Fatalf("PackCapture with a tar that breaks on a nested file = %v; want an error carrying its "+
				"whole word, %q", err, said)
		}
	})
	t.Run("no transcript", func(t *testing.T) {
		t.Parallel()
		runner, b := sessionBox(t, "2.1.310")
		_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
		if _, err := b.Exec(t.Context(), "rm -f "+shQuote(transcriptOf(b, res.SessionID)),
			sandbox.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
		described, body, _, err := runner.PackCapture(t.Context(), b, false)
		if err != nil || body != nil {
			t.Fatalf("PackCapture with no transcript = %+v, %v, %v; want nothing", described, body, err)
		}
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, MaxBytes: takeBound})
		if err != nil || taken.Body != nil {
			t.Fatalf("a take with no transcript = %+v, %v; want nothing", taken, err)
		}
	})
}

// TWO READS OF ONE JOB AT ONCE NEVER MOVE A HALF-MADE ARCHIVE. A stop's read and
// a collection's of one job may take its session at once, and each packs it now
// where the job left no archive of it, or where its box would not end it. Here
// the program a pack writes its archive with (Claude Code's tar, an OpenCode
// export's gzip) is played so that the two packs meet where they can do each
// other the most harm. The first read starts alone, and its pack, its archive
// made whole and not yet moved, says so and holds. The second read starts only
// once the first pack holds, and its pack writes the first half of its archive
// where it is told, says so and holds there. That word lets the first pack go
// on to move its archive, and the second pack goes on only once the first read
// has been read whole. So the second pack starts, and half makes its archive,
// while the first pack's waits to be moved, and the first pack moves while the
// second's is half made. The box holds both packs' staging directories as the
// second pack writes, each read hands back the whole session, and neither pack
// leaves its directory behind.
//
// On either harness, for a read of a job that left no archive, which looks for
// one first, and for a read that packs the job as it stands
// (sandbox.SessionTake.Pack), as a read of a job its box would not end does. In
// both the job's harness has exited, so the case holds a read's way to its pack
// and not a job still working, whose session can change between two packs:
// that each read of one streams its own archive, never moved over the job's
// capture, TestATakeOfAJobStillWorkingNeverMovesItsArchiveOverTheJobsCapture
// holds.
//
// Mutation: stage both packs in one directory — the runner naming every pack's
// alike, or a harness's pack staging in a directory of its own choosing rather
// than the one it is handed — and the second pack's start clears the first's
// archive: the first pack moves the second's, half made, into place — over the
// job's capture, or where a read of a job still working reads its own — and
// its read is not taken whole; and the second pack's move finds nothing to
// move. Have a pack's start clear every pack's staging directory, and the first
// pack's move finds nothing to move. Either way the box holds one staging
// directory as the second pack writes. Leave behind the archive a read was
// handed, once it is read, and it is found after; a staging directory a pack's
// script leaves is removed by the take's release too, so the run's own end,
// which has no release, holds the script to removing it
// (TestTheRunsOwnEndPackLeavesNoStagingDirectoryBehind).
func TestTwoReadsOfOneJobAtOnceNeverMoveAHalfMadeArchive(t *testing.T) {
	t.Parallel()
	harnesses := []struct {
		name string
		// packer is the program the harness's pack writes its archive with,
		// and word the first word of the pack's call of it.
		packer, word string
		// aside is the shell that makes the archive the call asks for, whole,
		// at "$whole" with this machine's packer, "$own", its status in st,
		// and opens descriptor 3 on where the call writes it.
		aside string
		// job is a box whose job left no archive of its session, and that
		// session's id.
		job func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string)
		// lacks is what archive, as a read handed it back, lacks of a whole
		// archive of session: "" where it lacks nothing.
		lacks func(t *testing.T, archive []byte, session string) string
	}{
		{name: "Claude Code", packer: "tar", word: "-czf",
			aside: `out=$2; shift 2; "$own" -czf "$whole" "$@"; st=$?; exec 3> "$out"`,
			job: func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
				runner, b := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				// THE SCRIPT'S OWN ARCHIVE GONE, as a job killed before its
				// harness exited never left one.
				if _, err := b.Exec(t.Context(), "rm -f "+shQuote(paths(b).SessionCapture())+" "+
					shQuote(paths(b).SessionDigests()), sandbox.ExecOptions{}); err != nil {
					t.Fatal(err)
				}
				return runner, b, ran.SessionID
			},
			lacks: func(t *testing.T, archive []byte, session string) string {
				members := archiveMembers(t, bytes.NewReader(archive))
				project := "projects/" + executor.ClaudeProjectDir + "/"
				for _, want := range []string{project + session + ".jsonl",
					project + session + "/subagents/agent-1.jsonl", project + session + "/tool-results/out.txt",
					"tasks/" + session + "/1.json"} {
					if _, ok := members[want]; !ok {
						return want
					}
				}
				if !strings.Contains(members[project+session+".jsonl"], `"sessionId":"`+session+`"`) {
					return "its transcript's own first line"
				}
				return ""
			}},
		{name: "OpenCode", packer: "gzip", word: "-c",
			aside: `"$own" "$@" > "$whole"; st=$?; exec 3>&1`,
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
				// THE BOX'S OPENCODE HOLDS THE SESSION, and no run's script
				// exported it.
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
	reads := []struct {
		name string
		take sandbox.SessionTake
	}{
		{name: "a read of a job that left no archive", take: sandbox.SessionTake{MaxBytes: takeBound}},
		{name: "a read that packs the job as it stands", take: sandbox.SessionTake{Pack: true, MaxBytes: takeBound}},
	}
	for _, h := range harnesses {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			for _, read := range reads {
				t.Run(read.name, func(t *testing.T) {
					t.Parallel()
					runner, b, session := h.job(t)
					flags := filepath.Join(b.Home(), "two-reads")
					if err := os.MkdirAll(flags, 0o700); err != nil {
						t.Fatal(err)
					}
					// THE WORDS THE PACKS AND THE CASE PASS: the first pack's
					// that it holds, which the case waits on before it starts
					// the second read; the second pack's that its archive is
					// half made, which the first pack waits on; and the case's
					// that the first read was read whole, which the second pack
					// waits on.
					heard, release := heldFifo(t, flags, "held"), heldFifo(t, flags, "release")
					heldFifo(t, flags, "half")
					writeBoxStandIn(t, b, paths(b).BinDir()+"/"+h.packer,
						meetingPacks(t, b, h.packer, h.word, h.aside, flags))
					type took struct {
						first   bool
						taken   sandbox.TakenSession
						err     error
						archive []byte
						readErr error
					}
					done := make(chan took, 2)
					take := func(first bool) {
						taken, takeErr := runner.TakeSession(t.Context(), b, read.take)
						// A READ THAT ENDS WITH ITS PACK'S WORD NEVER SAID says
						// so in its stead, so nothing waits on a word that is
						// not coming — the case on the first pack's, the first
						// pack on the second's — and never waits itself: the
						// case holds every fifo open to read.
						fifo := "half"
						if first {
							fifo = "held"
						}
						if ending, openErr := os.OpenFile(filepath.Join(flags, fifo),
							os.O_WRONLY|syscall.O_NONBLOCK, 0); openErr == nil {
							_, _ = ending.WriteString("ended\n")
							_ = ending.Close()
						}
						done <- took{first: first, taken: taken, err: takeErr}
					}
					go take(true)
					word := make([]byte, 64)
					n, err := heard.Read(word)
					if err != nil || !strings.HasPrefix(string(word[:n]), "held") {
						t.Fatalf("the first read ended with its pack never held: it said %q (%v)", word[:n], err)
					}
					go take(false)
					// THE READ THAT ENDS FIRST IS READ WHOLE BEFORE THE SECOND
					// PACK GOES ON, so an archive moved half made is read so.
					results := make([]took, 0, 2)
					for range 2 {
						got := <-done
						if got.taken.Body != nil {
							got.archive, got.readErr = io.ReadAll(got.taken.Body)
							_ = got.taken.Body.Close()
						}
						results = append(results, got)
						if len(results) == 1 {
							if _, writeErr := release.WriteString("read\n"); writeErr != nil {
								t.Fatalf("telling the second pack the first read was read: %v", writeErr)
							}
						}
					}
					for _, flag := range []string{"first-packed", "second-half", "second-packed"} {
						if _, statErr := os.Stat(filepath.Join(flags, flag)); statErr != nil {
							t.Errorf("the packs never met as the case has them: %v", statErr)
						}
					}
					// EACH PACK IN A STAGING DIRECTORY OF ITS OWN: the first's
					// still there, its archive in it, as the second writes.
					listed, err := os.ReadFile(filepath.Join(flags, "staging"))
					if err != nil {
						t.Fatalf("the second pack listed no staging directories: %v", err)
					}
					staged := strings.FieldsFunc(string(listed), func(r rune) bool { return r == '\n' })
					if len(staged) != 2 {
						t.Errorf("as the second pack wrote, the box held the staging directories %q; want two, "+
							"the first pack's beside the second's", strings.Join(staged, ", "))
					}
					for _, dir := range staged {
						if !strings.HasPrefix(dir, paths(b).WorkDir()+"/session-pack-staging.") {
							t.Errorf("a pack staged in %s, which is no pack's own staging directory", dir)
						}
					}
					for _, got := range results {
						which := "the second read"
						if got.first {
							which = "the first read"
						}
						switch {
						case got.err != nil:
							t.Errorf("%s was refused: %v", which, got.err)
						case got.taken.Body == nil:
							t.Errorf("%s handed back no archive", which)
						case got.readErr != nil:
							t.Errorf("%s was not taken whole: %v", which, got.readErr)
						default:
							if lacks := h.lacks(t, got.archive, session); lacks != "" {
								t.Errorf("%s handed back an archive of the session without %s", which, lacks)
							}
						}
					}
					if left := packStagingLeft(t, b); len(left) > 0 {
						t.Errorf("the packs left their staging directories: %s", strings.Join(left, ", "))
					}
				})
			}
		})
	}
}

// heldFifo is a fifo at dir/name that the case holds open both ways, so a
// writer never waits on it, and a read of it waits for the first word written.
func heldFifo(t *testing.T, dir, name string) *os.File {
	t.Helper()
	at := filepath.Join(dir, name)
	if err := syscall.Mkfifo(at, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", at, err)
	}
	fifo, err := os.OpenFile(at, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", at, err)
	}
	t.Cleanup(func() { _ = fifo.Close() })
	return fifo
}

// meetingPacks is a stand-in for packer, the program a harness's pack writes its
// archive with (Claude Code's tar, an OpenCode export's gzip), for b's bin
// directory, which a pack's PATH puts first. The packs and the case leave their
// marks in flags and pass their words through the fifos the case holds there
// (heldFifo). A call whose first word is not word runs this machine's own
// packer. The FIRST pack's call makes its archive whole where it is told, says
// so in the fifo held, and holds until a word comes in the fifo half, failing
// on any but the second pack's. The SECOND pack's call makes its archive whole
// aside (aside), writes the first half of it where it is told, lists the packs'
// staging directories b holds as it finds them, says so in half, and holds
// until the case's word comes in the fifo release; then it writes the rest.
func meetingPacks(t *testing.T, b sandbox.Sandbox, packer, word, aside, flags string) string {
	t.Helper()
	tool := func(name string) string {
		found, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("no %s on this machine's PATH: %v", name, err)
		}
		return shQuote(found)
	}
	at := func(name string) string { return shQuote(filepath.Join(flags, name)) }
	return "#!/bin/sh\n" +
		"own=" + tool(packer) + " whole=" + at("whole") + "\n" +
		`if [ "$1" != ` + shQuote(word) + ` ]; then exec "$own" "$@"; fi` + "\n" +
		`if ` + claim(filepath.Join(flags, "claimed")) + `; then` + "\n" +
		`  "$own" "$@"; st=$?` + "\n" +
		`  : > ` + at("first-packed") + "\n" +
		`  printf 'held\n' > ` + at("held") + "\n" +
		`  read said < ` + at("half") + "\n" +
		`  [ "$said" = half ] || exit 3` + "\n" +
		`  exit "$st"` + "\n" +
		"fi\n" +
		aside + "\n" +
		`[ "$st" = 0 ] || exit "$st"` + "\n" +
		`size=$(` + tool("wc") + ` -c < "$whole"); cut=$((size / 2))` + "\n" +
		tool("head") + ` -c "$cut" "$whole" >&3` + "\n" +
		`for d in ` + shQuote(paths(b).WorkDir()) + `/session-pack-staging.*; do if [ -d "$d" ]; then ` +
		`printf '%s\n' "$d"; fi; done > ` + at("staging") + "\n" +
		`: > ` + at("second-half") + "\n" +
		`printf 'half\n' > ` + at("half") + "\n" +
		`read said < ` + at("release") + "\n" +
		tool("tail") + ` -c +"$((cut + 1))" "$whole" >&3` + "\n" +
		`exec 3>&-` + "\n" +
		`: > ` + at("second-packed") + "\n" +
		`exit "$st"` + "\n"
}

// packStagingLeft is every staging directory a pack on demand left in box, and
// every archive one kept for its read (executor.Paths.SessionPackHeld): each is
// named for its pack alone (executor.Paths.SessionPackStaging), so it is found
// by its pattern rather than named.
func packStagingLeft(t *testing.T, box sandbox.Sandbox) []string {
	t.Helper()
	res, err := box.Exec(t.Context(), "for d in "+shQuote(paths(box).WorkDir())+"/session-pack-staging.*; do "+
		`if [ -e "$d" ] || [ -L "$d" ]; then printf '%s\n' "$d"; fi; done`, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("asking the box for its packs' staging directories: %v", err)
	}
	return strings.FieldsFunc(res.Stdout, func(r rune) bool { return r == '\n' })
}

// THE CLEAR BEFORE EVERY RUN REMOVES EVERY PACK ON DEMAND'S STAGING DIRECTORY,
// whichever read made it: each is named for its pack alone
// (executor.Paths.SessionPackStaging), so the clear removes what matches their
// pattern, and two that packs cut short left — their node stopped as they
// packed — outlive no clear: in a box whose home's path holds a space, which
// the clear quotes, as in any other.
//
// Mutation: leave the packs' staging directories out of the clear, or quote
// their pattern so it matches nothing, and they outlive it; leave the work
// directory unquoted, and in the box whose home's path holds a space the clear
// splits that path into words and removes neither.
func TestTheClearBeforeEveryRunRemovesEveryPacksStagingDirectory(t *testing.T) {
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
			cut := []string{p.SessionPackStaging("a-pack-cut-short"), p.SessionPackStaging("another-cut-short")}
			for _, staged := range cut {
				if writeErr := b.WriteFile(t.Context(), staged+"/capture", []byte("part of a pack")); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			if got := packStagingLeft(t, b); len(got) != len(cut) {
				t.Fatalf("the box holds %d packs' staging directories before the clear, want the %d the case left: %s",
					len(got), len(cut), strings.Join(got, ", "))
			}
			if clearErr := executor.NewClaudeCode().ClearArtifacts(t.Context(), b); clearErr != nil {
				t.Fatalf("ClearArtifacts: %v", clearErr)
			}
			if left := packStagingLeft(t, b); len(left) > 0 {
				t.Errorf("the clear left packs' staging directories: %s", strings.Join(left, ", "))
			}
		})
	}
}

// heldTake is a box a take of its job's session reads through, held at one
// step of the take — its pack, the login shell a pack on demand runs in (pack),
// or its at-th open of an archive (at) — closing held as it gets there, and
// going on only once resume is closed: so a case orders two takes of one job,
// or lands an archive, between the steps it holds them at. A Claude Code
// take's second open is its scrub's, opening its archive again by its path for
// the second pass; an OpenCode take's first is the one pass it reads.
type heldTake struct {
	sandbox.Sandbox
	pack         bool
	at           int
	held, resume chan struct{}

	mu             sync.Mutex
	packed, opened int
}

// newHeldTake is a [heldTake] through b, held at its pack where pack is set
// and at its at-th open where at is above zero.
func newHeldTake(b sandbox.Sandbox, pack bool, at int) *heldTake {
	return &heldTake{Sandbox: b, pack: pack, at: at, held: make(chan struct{}), resume: make(chan struct{})}
}

// wait says the take is held, and waits for its word to go on.
func (b *heldTake) wait(ctx context.Context) error {
	close(b.held)
	select {
	case <-b.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *heldTake) Exec(ctx context.Context, cmd string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	b.mu.Lock()
	hold := b.pack && b.packed == 0 && strings.HasPrefix(cmd, "sh -lc ")
	if hold {
		b.packed++
	}
	b.mu.Unlock()
	if hold {
		if err := b.wait(ctx); err != nil {
			return sandbox.ExecResult{}, err
		}
	}
	return b.Sandbox.Exec(ctx, cmd, opts)
}

func (b *heldTake) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	b.mu.Lock()
	b.opened++
	hold := b.at > 0 && b.opened == b.at
	b.mu.Unlock()
	if hold {
		if err := b.wait(ctx); err != nil {
			return nil, err
		}
	}
	return b.Sandbox.OpenFile(ctx, path)
}

// readBack is one take of a box's session as its reader read it: the archive,
// whole, or why the take was refused or not read whole.
type readBack struct {
	archive []byte
	err     error
}

// takeWhole takes the session of b's job, which is over, as a read of it does
// (packed now where the job left no archive), and reads it to its end.
func takeWhole(ctx context.Context, runner *executor.Runner, b sandbox.Sandbox) readBack {
	taken, err := runner.TakeSession(ctx, b, sandbox.SessionTake{MaxBytes: takeBound})
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

// wentOnMark is what a session written on past a pack holds and that pack's
// archive does not.
const wentOnMark = "written on once the first read had packed"

// overJob is a harness's job that is over and left no archive of its session,
// as the cases of two reads of one job have it.
type overJob struct {
	name string
	// at is the open of its archive at which a take of the harness's goes on
	// to the pass that streams it ([heldTake]).
	at int
	// job is a box whose job is over and left no archive: its runner, the
	// box, the session's id, and goOn, which writes the session on past what a
	// pack made before it holds, as a process the job's harness started and
	// that outlived it may.
	job func(t *testing.T) (runner *executor.Runner, b sandbox.Sandbox, session string, goOn func())
	// wentOn reports whether archive, as a read handed it back, holds what
	// goOn wrote, failing the case where it is no whole archive of session.
	wentOn func(t *testing.T, archive []byte, session string) bool
}

// overJobs are [overJob] on either harness.
func overJobs() []overJob {
	return []overJob{
		{name: "Claude Code", at: 2,
			job: func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string, func()) {
				runner, b := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				forgetCapture(t, b)
				goOn := func() {
					line := shQuote(`{"type":"user","text":"` + wentOnMark + `"}`)
					if _, err := b.Exec(t.Context(), "printf '%s\\n' "+line+" >> "+
						shQuote(transcriptOf(b, ran.SessionID)), sandbox.ExecOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				return runner, b, ran.SessionID, goOn
			},
			wentOn: func(t *testing.T, archive []byte, session string) bool {
				transcript, ok := archiveMembers(t, bytes.NewReader(archive))[filesOf(session).transcript]
				if !ok || !strings.Contains(transcript, `"sessionId":"`+session+`"`) {
					t.Errorf("a read handed back an archive without the session's transcript, its first line "+
						"its own: %q", transcript)
				}
				return strings.Contains(transcript, wentOnMark)
			}},
		{name: "OpenCode", at: 1,
			job: func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string, func()) {
				runner, b := openCodeBox(t, t.TempDir())
				dir := t.TempDir()
				writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
					strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shQuote(dir), 1))
				const session = "ses_0123456789abcdef0123456789"
				if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
					[]byte("id "+session+"\nrelease 2.0.32\n")); err != nil {
					t.Fatal(err)
				}
				// THE BOX'S OPENCODE HOLDS THE SESSION, and no run's script
				// exported it.
				if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				goOn := func() {
					messages := `[{"id":"msg_1","type":"user","text":"fix the flake"},` +
						`{"id":"msg_2","type":"user","text":"` + wentOnMark + `"}]`
					if err := os.WriteFile(filepath.Join(dir, "export-messages"), []byte(messages), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return runner, b, session, goOn
			},
			wentOn: func(t *testing.T, archive []byte, session string) bool {
				export := gunzipped(t, archive)
				if !json.Valid(export) || !strings.Contains(string(export), `"id":"`+session+`"`) {
					t.Errorf("a read handed back %q, want one JSON document of the session", export)
				}
				return strings.Contains(string(export), wentOnMark)
			}},
	}
}

// startTake starts a take of the session of the job of held's box, and answers
// once it holds where held holds it, with where its whole read lands; a take
// that ends before it holds fails the case.
func startTake(t *testing.T, runner *executor.Runner, held *heldTake) <-chan readBack {
	t.Helper()
	read := make(chan readBack, 1)
	go func() { read <- takeWhole(t.Context(), runner, held) }()
	select {
	case <-held.held:
	case got := <-read:
		t.Fatalf("a read ended before it held where the case holds it: %q, %v", got.archive, got.err)
	}
	return read
}

// THE RUN'S OWN END PACK LEAVES NO STAGING DIRECTORY BEHIND. A run's script
// packs its session as its harness exits in a staging directory
// (executor.Paths.SessionStaging), and removes it once the archive is moved
// over the job's capture: nothing else removes it before the clear that comes
// with the box's next run, which may be long after. A pack on demand's
// staging directory is removed by its take's release as well, so only the
// run's own end shows whether a pack's script removes what it staged. Here,
// on either harness, a run ends by itself: its capture is there, and its
// staging directory is not.
//
// Mutation: have either harness's pack leave its staging directory, and the
// run's own end leaves it behind.
func TestTheRunsOwnEndPackLeavesNoStagingDirectoryBehind(t *testing.T) {
	t.Parallel()
	t.Run("Claude Code", func(t *testing.T) {
		t.Parallel()
		runner, b := sessionBox(t, "2.1.310")
		_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
		if res.Captured == nil {
			t.Fatalf("the run's own end packed nothing:\n%s", boxFile(t, b, paths(b).Err()))
		}
		if boxHas(t, b, paths(b).SessionStaging()) {
			t.Error("the run's own end pack left its staging directory behind")
		}
	})
	t.Run("OpenCode", func(t *testing.T) {
		t.Parallel()
		j := startScript(t, scriptOpts{env: []string{"FAKE_RUN_UNTIL=ended"}})
		if err := os.WriteFile(filepath.Join(j.dir, "ended"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		run := j.wait()
		if _, err := os.Stat(j.paths.SessionCapture()); err != nil {
			t.Fatalf("the run's own end exported nothing: %v\n%s", err, run.stderr)
		}
		if _, err := os.Stat(j.paths.SessionStaging()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the run's own end export left its staging directory behind: %v", err)
		}
	})
}

// TWO READS OF A JOB THAT IS OVER EACH STREAM THE ARCHIVE THEIR OWN PACK MADE.
// A stop's read and a collection's of one job may take its session at once,
// and where the job left no archive each packs it now. Here, on either
// harness, the two meet where a shared destination does them the most harm:
// the second read finds no archive the job left and holds before its pack;
// the first read then finds none either, packs the job, and holds before the
// pass that streams what its pack made — a Claude Code take's second, which
// opens its archive again by its path; an OpenCode take's one. The session is
// written on, as a process the job's harness started and that outlived it may
// write it, and only then does the second read pack and stream it, whole; then
// the first goes on. Neither read is refused, each hands back a whole archive
// of the session — the first's without what was written after its pack and
// the second's with it — nothing of either lands over the job's capture, and
// neither pack leaves a file behind.
//
// Mutation: move a pack on demand of a job that is over over the job's
// capture and stream what the capture holds, as such packs once did, and the
// first read streams the second's archive, moved there between its pack and
// its pass: Claude Code's second pass finds a member changed under it and the
// take is refused, and OpenCode's hands back what was written after its own
// pack; and the capture holds an archive after.
func TestTwoReadsOfAJobThatIsOverEachStreamTheirOwnPacksArchive(t *testing.T) {
	t.Parallel()
	for _, h := range overJobs() {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			runner, b, session, goOn := h.job(t)
			second := newHeldTake(b, true, 0)
			secondRead := startTake(t, runner, second)
			first := newHeldTake(b, false, h.at)
			firstRead := startTake(t, runner, first)
			goOn()
			close(second.resume)
			secondGot := <-secondRead
			close(first.resume)
			reads := []struct {
				which  string
				got    readBack
				wentOn bool
			}{{"the first read", <-firstRead, false}, {"the second read", secondGot, true}}
			for _, r := range reads {
				switch {
				case r.got.err != nil:
					t.Errorf("%s was refused: %v", r.which, r.got.err)
				case h.wentOn(t, r.got.archive, session) != r.wentOn:
					t.Errorf("%s handed back an archive that holds what was written after the first pack: %v, "+
						"want %v — another read's archive, not the one its own pack made", r.which, !r.wentOn,
						r.wentOn)
				}
			}
			if boxHas(t, b, paths(b).SessionCapture()) {
				t.Error("a read's pack moved its archive over the job's capture, which only the job's own script writes")
			}
			if left := packStagingLeft(t, b); len(left) > 0 {
				t.Errorf("the packs left their files behind: %s", strings.Join(left, ", "))
			}
		})
	}
}

// THE JOB'S OWN END PACK LANDING AS A READ STREAMS CHANGES NOTHING THAT READ
// STREAMS. The capture is the job's own script's, moved into place once as its
// harness exits, and a read that finds the job over with no archive packs it
// now — which the script, outliving what the read saw of its job, can still
// land its own over while that read streams. Here, on either harness, the read
// holds before the pass that streams what its pack made, and an archive is
// moved over the job's capture, as the job's end pack moves its own: the read
// goes on, hands back the whole archive its own pack made, and the capture
// holds the end pack's archive after it.
//
// Mutation: move a pack on demand of a job that is over over the job's capture
// and stream what the capture holds, as such packs once did, and the read
// streams the end pack's archive, which is no archive its scrub can read: the
// take is refused.
func TestTheJobsOwnEndPackLandingAsAReadStreamsChangesNothingItReads(t *testing.T) {
	t.Parallel()
	const final = "the job's own final archive, moved over the capture as its harness exited"
	for _, h := range overJobs() {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			runner, b, session, _ := h.job(t)
			p := paths(b)
			first := newHeldTake(b, false, h.at)
			read := startTake(t, runner, first)
			if res, err := b.Exec(t.Context(), "printf '%s' "+shQuote(final)+" > "+shQuote(p.SessionCapture()+".next")+
				" && mv -f "+shQuote(p.SessionCapture()+".next")+" "+shQuote(p.SessionCapture()),
				sandbox.ExecOptions{}); err != nil || res.ExitCode != 0 {
				t.Fatalf("landing the job's own end pack: %v (exit %d) %s", err, res.ExitCode, res.Stderr)
			}
			close(first.resume)
			switch got := <-read; {
			case got.err != nil:
				t.Errorf("the read was refused: %v", got.err)
			case h.wentOn(t, got.archive, session):
				t.Error("the read handed back what no pack of its made")
			}
			if got := boxFile(t, b, p.SessionCapture()); got != final {
				t.Errorf("the job's capture holds %d bytes after the read, not the job's own end pack's archive",
					len(got))
			}
			if left := packStagingLeft(t, b); len(left) > 0 {
				t.Errorf("the read's pack left its files behind: %s", strings.Join(left, ", "))
			}
		})
	}
}

// brokenDigestLists are the ways a box's list of the digests of the archives
// its copy holds (executor.Paths.SessionDigests) keeps no line a pack appends,
// each made by the shell's how, handed the list's path: a directory where the
// list belongs, which refuses the append; and a link to /dev/null, which takes
// the append — its writer told it landed — and keeps nothing, as a full disk
// can.
var brokenDigestLists = []struct{ name, how string }{
	{name: "a list the box refuses the line", how: "mkdir"},
	{name: "a list that keeps nothing it is handed", how: "ln -s /dev/null"},
}

// breakDigestList makes path, where a local box keeps its digest list on this
// host, the list how makes ([brokenDigestLists]).
func breakDigestList(t *testing.T, path, how string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "sh", "-c", `mkdir -p "${1%/*}" && rm -rf "$1" && `+how+` "$1"`,
		"sh", path).CombinedOutput()
	if err != nil {
		t.Fatalf("making the digest list at %s keep nothing: %v\n%s", path, err, out)
	}
}

// saysDigestUnrecorded is what a pack whose digest the box did not record says.
const saysDigestUnrecorded = "digest could not be recorded"

// A PACK REPORTS ITS ARCHIVE MADE ONLY ONCE ITS DIGEST IS RECORDED IN THE BOX.
// Every pack of a box's session records its archive's digest in the box's
// list of the archives its copy holds, so the box never takes an archive of
// its own for another box's and restores it over the copy that went on past
// it; and a pack reported its archive made — moved to where its read reads
// it — before it appended that line, whose write nothing checked. Here the
// list keeps no line ([brokenDigestLists]), and every pack, on either harness,
// fails saying why and moves nothing to where its read reads it: a pack on
// demand of a job that is over and one beside a harness that may still work,
// and the run's own as its harness exits, whose run's error stream says why
// and whose collection finds no capture.
//
// Mutation: move the archive into place and report it made before its digest
// is recorded, the line's append unchecked, as the packs once did, and every
// pack here holds; take the append's exit for the line, never reading it
// back, and every pack beside the list that keeps nothing holds.
func TestAPackReportsItsArchiveMadeOnlyOnceItsDigestIsRecorded(t *testing.T) {
	t.Parallel()
	for _, list := range brokenDigestLists {
		t.Run(list.name, func(t *testing.T) {
			t.Parallel()
			for _, h := range overJobs() {
				t.Run(h.name+" packed on demand", func(t *testing.T) {
					t.Parallel()
					runner, b, _, _ := h.job(t)
					breakDigestList(t, paths(b).SessionDigests(), list.how)
					for _, working := range []bool{false, true} {
						_, body, _, err := runner.PackCapture(t.Context(), b, working)
						if body != nil {
							_ = body.Close()
						}
						if err == nil || !strings.Contains(err.Error(), saysDigestUnrecorded) {
							t.Errorf("a pack (beside a working harness: %v) whose digest the box kept no line of "+
								"answered %v, want it refused saying %q", working, err, saysDigestUnrecorded)
						}
					}
					if left := packStagingLeft(t, b); len(left) > 0 {
						t.Errorf("the refused packs left their files behind: %s", strings.Join(left, ", "))
					}
				})
			}
			t.Run("a Claude Code run's own end", func(t *testing.T) {
				t.Parallel()
				runner, b := sessionBox(t, "2.1.310")
				breakDigestList(t, paths(b).SessionDigests(), list.how)
				_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				if res.Captured != nil || boxHas(t, b, paths(b).SessionCapture()) {
					t.Errorf("the run's own end packed its session with no digest of it recorded: %+v", res.Captured)
				}
				if said := boxFile(t, b, paths(b).Err()); !strings.Contains(said, "was not packed") ||
					!strings.Contains(said, saysDigestUnrecorded) {
					t.Errorf("the run's error stream says %q, want that the session was not packed, and why", said)
				}
			})
			t.Run("an OpenCode run's own end", func(t *testing.T) {
				t.Parallel()
				j := startScript(t, scriptOpts{env: []string{"FAKE_RUN_UNTIL=ended"}})
				breakDigestList(t, j.paths.SessionDigests(), list.how)
				if err := os.WriteFile(filepath.Join(j.dir, "ended"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				run := j.wait()
				if _, err := os.Stat(j.paths.SessionCapture()); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the run's own end exported its session with no digest of it recorded: %v", err)
				}
				if !strings.Contains(run.stderr, "was not exported") || !strings.Contains(run.stderr, saysDigestUnrecorded) {
					t.Errorf("the run's error stream says %q, want that the session was not exported, and why", run.stderr)
				}
			})
		})
	}
}

// releaseWatch is a box that records the deadline every release of a pack on
// demand's own files is asked under: the `rm -rf` of the pack's staging
// directory and the archive it kept for its read.
type releaseWatch struct {
	sandbox.Sandbox

	mu        sync.Mutex
	deadlines []time.Time
}

func (b *releaseWatch) Exec(ctx context.Context, cmd string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	if strings.HasPrefix(cmd, "rm -rf ") && strings.Contains(cmd, "/session-pack-staging.") {
		deadline, _ := ctx.Deadline()
		b.mu.Lock()
		b.deadlines = append(b.deadlines, deadline)
		b.mu.Unlock()
	}
	return b.Sandbox.Exec(ctx, cmd, opts)
}

// A PACK'S RELEASE IS HELD TO ITS TAKE'S OWN DEADLINE. A pack on demand keeps
// its archive for its take alone and removes it once the take has read it, on
// the take's close path, which the read the take is part of waits on; so the
// removal is held to the take's own deadline — the time that read leaves it —
// and not to a bound of its own from the moment it runs, under which a box that
// stopped answering once its archive was read held the read up to the two
// minutes a pack may take past it, into the room left for the outcome stamp and
// the box's pause. Here a take with a minute to go packs a job that left no
// archive, and is read whole and closed: the release ran, under a deadline no
// later than the take's, and left nothing behind.
//
// Mutation: bound the release by the pack's own wait from the moment it runs,
// as it was, and it is asked under a deadline past the take's.
func TestAPacksReleaseIsHeldToItsTakesOwnDeadline(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	forgetCapture(t, b)
	watched := &releaseWatch{Sandbox: b}
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	if got := takeWhole(ctx, runner, watched); got.err != nil {
		t.Fatalf("the take was refused: %v", got.err)
	}
	watched.mu.Lock()
	asked := append([]time.Time(nil), watched.deadlines...)
	watched.mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("the take's pack was never released")
	}
	for _, at := range asked {
		if at.IsZero() || deadline.Before(at) {
			t.Errorf("a release was asked under the deadline %v, past the take's own %v", at, deadline)
		}
	}
	if left := packStagingLeft(t, b); len(left) > 0 {
		t.Errorf("the release left the pack's files behind: %s", strings.Join(left, ", "))
	}
}

// A RESTORE IS CONTINUED ONLY ONCE ITS DIGEST IS RECORDED IN THE BOX. A run that
// continues a session carried into its box restores the archive over the box's
// own copy where that copy does not hold it, and records the archive's digest
// in the box's list of the archives its copy holds, so the archive carried in
// again is never restored over the copy that went on past it; that line was
// appended once the restore was done, and nothing checked it. Here the list
// keeps no line ([brokenDigestLists]) and a session is carried into a box that
// holds none, on either harness: the run does not continue what it restored, it
// starts the session minted for it in its place and says why.
//
// Mutation: continue the restore whose digest went unrecorded, as the restores
// once did, and the run takes up the carried session with no line of it in the
// box.
func TestARestoreIsContinuedOnlyOnceItsDigestIsRecorded(t *testing.T) {
	t.Parallel()
	for _, list := range brokenDigestLists {
		t.Run(list.name, func(t *testing.T) {
			t.Parallel()
			t.Run("Claude Code", func(t *testing.T) {
				t.Parallel()
				first, a := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
				described, body, err := first.OpenCapture(t.Context(), a)
				if err != nil || body == nil {
					t.Fatalf("OpenCapture = %v, %v", body, err)
				}
				defer func() { _ = body.Close() }()
				second, b := sessionBox(t, "2.1.310")
				breakDigestList(t, paths(b).SessionDigests(), list.how)
				_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
					Carried: &sandbox.CarriedSession{Archive: described, Body: body}})
				if argv := argvOf(t, b); after(argv, "--resume") != "" || res.Continued || res.SessionID == ran.SessionID {
					t.Errorf("the run continued %q (argv %q) with no digest of its archive recorded in the box",
						res.SessionID, argv)
				}
				if !strings.Contains(res.FellBack, saysDigestUnrecorded) {
					t.Errorf("the run reports it fell back because %q, want %q", res.FellBack, saysDigestUnrecorded)
				}
			})
			t.Run("OpenCode", func(t *testing.T) {
				t.Parallel()
				run := runScript(t, scriptOpts{session: &continuingOpenCode, carried: openCodeExport(carriedOpenCode),
					req:           sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly, Steer: steerWiring},
					brokenDigests: list.how})
				if got := sessionOf(t, run); got != freshOpenCode {
					t.Errorf("the run worked in %q with no digest of its export recorded in the box, want the "+
						"session minted for it, %q", got, freshOpenCode)
				}
				if why := recordedWhy(t, run); !strings.Contains(why, saysDigestUnrecorded) {
					t.Errorf("the run's record says it started anew because %q, want %q", why, saysDigestUnrecorded)
				}
			})
		})
	}
}
