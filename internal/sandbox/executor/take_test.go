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
	"github.com/crewlet/crewlet/internal/shellword"
	"github.com/crewlet/crewlet/internal/statelog"
)

// takeBound is the archive bound every take here is made within: a session's.
const takeBound = executorkeep.SessionMaxBytes

// testSpool is a take's spool in a directory of the test's own, in the room a
// take's scrub may spool at the kept bound, every byte given back by its end.
func testSpool(t testing.TB) sandbox.SessionSpool {
	t.Helper()
	room := executorkeep.GzipMostBytes(executorkeep.SessionKeptMaxBytes) + executorkeep.SpoolMembersMostBytes
	var (
		mu   sync.Mutex
		used int64
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if used != 0 {
			t.Errorf("the take's spool left %d bytes of its room taken", used)
		}
	})
	return sandbox.SessionSpool{Dir: t.TempDir(),
		Take: func(n int64) error {
			mu.Lock()
			defer mu.Unlock()
			if used+n > room {
				return errors.New("the spool's room is full")
			}
			used += n
			return nil
		},
		Give:    func(n int64) { mu.Lock(); used -= n; mu.Unlock() },
		Spooled: func() {}}
}

// keptAndOpened is a taken session as a later launch opens it: sealed into a
// spool, uploaded and read back through the session store, so a case greps what
// a later launch carries into its box, never the stream before its seal.
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

// plantingHarness is a stand-in `claude` that files its session as
// [sessionHarness] does, with a planted secret in every kind of file a session
// holds: the transcript (raw, JSON-escaped, and as a tool result's key), a
// subagent's transcript, a spilled tool result (and one named after the
// secret), and a task list.
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
// ARCHIVE ITS HARNESS RESTORES. Taken as the run's script packed it and packed
// on demand, then opened from the store as a later launch opens it, the
// archive holds no planted secret: not the engine's value raw, JSON-escaped or
// as a key, not a credential only its shape gives away, not a value under a
// password's key. Every transcript line stays JSON, every member stays, and
// the box keeps the digest of exactly the bytes that left it.
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
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t),
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
// keeps its digest in the box as the script's pack does. The archive is the
// whole session, at the pack's own place (never over the job's capture, which
// only the job's script writes), and gone once the read is done with it.
//
// Mutation: pack anywhere but the runner, or leave the digest out, and the box
// takes its own archive for another box's; move the pack over the job's
// capture and its archive is found there.
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
// of it is read, AS THE SESSION PAST ITS BOUND IT IS
// ([executorkeep.ErrSessionTooLarge]), which its refusal is stamped and said as
// ([executorkeep.LossTooLarge]); and a take that names no bound is refused
// outright.
//
// Mutation: drop the bound's check, or read an archive before it, and the
// archive a byte past its bound is taken; refuse it with an error that does not
// wrap the sentinel, and it is said as a take that failed.
func TestATakeIsBoundedByTheArchiveItKeeps(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	if res.Captured == nil {
		t.Fatalf("the run's session was not captured:\n%s", boxFile(t, b, paths(b).Err()))
	}
	if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: res.Captured.Size - 1}); !errors.Is(err, executorkeep.ErrSessionTooLarge) {
		t.Errorf("an archive a byte past the take's bound came to %v, want it refused as too large", err)
	}
	if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{}); err == nil {
		t.Error("a take naming no bound was made")
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: res.Captured.Size})
	if err != nil || taken.Body == nil {
		t.Fatalf("an archive at the take's bound: %+v, %v", taken, err)
	}
	if err := taken.Body.Close(); err != nil {
		t.Fatalf("closing the taken session: %v", err)
	}
}

// A SESSION NO BOX CONTINUES IS NEVER PACKED OR READ: a Claude Code session
// written below the oldest release whose sessions another box continues is
// refused on every road a take or a save has, as no box of this build
// continuing it (release_floor), before a pack runs or a byte of the archive
// its script packed is streamed; one written at that release is taken, and
// packed where its road packs.
//
// Mutation: drop the refusal before a pack, or the one before an archive the
// script packed is streamed, and a road packs or takes the session.
func TestASessionNoBoxContinuesIsNeverPackedOrRead(t *testing.T) {
	t.Parallel()
	roads := map[string]struct {
		packs bool
		take  func(*testing.T, *executor.Runner, sandbox.Sandbox) error
	}{
		"the archive its script packed": {take: func(t *testing.T, r *executor.Runner, b sandbox.Sandbox) error {
			taken, err := r.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound})
			if taken.Body != nil {
				_ = taken.Body.Close()
			}
			return err
		}},
		"a pack now": {packs: true, take: func(t *testing.T, r *executor.Runner, b sandbox.Sandbox) error {
			_, err := r.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), Pack: true,
				MaxBytes: takeBound})
			return err
		}},
		"a save": {packs: true, take: func(t *testing.T, r *executor.Runner, b sandbox.Sandbox) error {
			_, err := r.PackSave(t.Context(), b, takerA, "")
			return err
		}},
	}
	for release, refused := range map[string]bool{"2.1.299": true, "2.1.300": false} {
		for road, c := range roads {
			t.Run(release+", "+road, func(t *testing.T) {
				t.Parallel()
				runner, b := sessionBox(t, release)
				if _, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"}); res.Captured == nil {
					t.Fatalf("the premise: the run's script packed no archive:\n%s", boxFile(t, b, paths(b).Err()))
				}
				// A TAR THAT SAYS IT RAN, and fails: the run's own pack is made.
				ran := b.Home() + "/tar-ran"
				writeBoxStandIn(t, b, paths(b).BinDir()+"/tar", "#!/bin/sh\nprintf ran > "+shellword.Quote(ran)+
					"\nexit 2\n")
				err := c.take(t, runner, b)
				var unrestorable *sandbox.UnrestorableError
				floored := errors.As(err, &unrestorable)
				switch {
				case refused && (!floored || unrestorable.Loss != executorkeep.LossReleaseFloor ||
					!strings.Contains(unrestorable.Why, "older than 2.1.300")):
					t.Fatalf("the take of a session %s wrote came to %v, want it refused as no box continues it",
						release, err)
				case !refused && floored:
					t.Fatalf("the take of a session %s wrote was refused: %v", release, err)
				case !refused && !c.packs && err != nil:
					t.Fatalf("the take of a session %s wrote: %v", release, err)
				}
				if packed := boxFile(t, b, ran) != ""; packed != (c.packs && !refused) {
					t.Errorf("a session %s wrote was packed: %v, want %v", release, packed, c.packs && !refused)
				}
			})
		}
	}
}

// A PACK THAT FAILS IS AN ERROR, NEVER "NO SESSION"; A SESSION THERE IS NOTHING
// OF IS NOTHING. A box whose tar breaks answers why, so the capture is stamped
// refused and a later launch knows the session is missing rather than
// continuing an older one, and the error carries the pack's whole word even
// for a file nested deep in the session; a box whose harness wrote no
// transcript answers no session, which is no refusal.
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
		if _, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), Pack: true,
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
		writeBoxStandIn(t, b, paths(b).BinDir()+"/tar", "#!/bin/sh\necho "+shellword.Quote(said)+" >&2\nexit 2\n")
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
		if _, err := b.Exec(t.Context(), "rm -f "+shellword.Quote(transcriptOf(b, res.SessionID)),
			sandbox.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
		described, body, _, err := runner.PackCapture(t.Context(), b, false)
		if err != nil || body != nil {
			t.Fatalf("PackCapture with no transcript = %+v, %v, %v; want nothing", described, body, err)
		}
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), Pack: true, MaxBytes: takeBound})
		if err != nil || taken.Body != nil {
			t.Fatalf("a take with no transcript = %+v, %v; want nothing", taken, err)
		}
	})
}

// TWO READS OF ONE JOB AT ONCE NEVER MOVE A HALF-MADE ARCHIVE. A stop's read
// and a collection's may each pack a job's session at once; [meetingPacks]
// has them meet where it hurts most: the first pack holds, archive whole and
// unmoved; the second writes half of its own and holds; the first moves; the
// second ends once the first read is read whole. Both staging directories are
// there as the second writes, each read is the whole session, none is left.
// The job's harness has exited (one still working:
// TestATakeOfAJobStillWorkingNeverMovesItsArchiveOverTheJobsCapture).
//
// Mutation: stage both packs in one directory, or clear every pack's on start,
// and the box holds one as the second writes; leave a read's archive behind and
// it is found after (a script's own: TestTheRunsOwnEndPackLeavesNoStagingDirectoryBehind).
func TestTwoReadsOfOneJobAtOnceNeverMoveAHalfMadeArchive(t *testing.T) {
	t.Parallel()
	harnesses := []struct {
		name string
		// packer is the program the harness's pack writes its archive with,
		// and word the first word of the pack's call of it.
		packer, word string
		// aside is the shell that makes the call's archive whole at "$whole"
		// with this machine's packer, "$own", its status in st, and opens
		// descriptor 3 on where the call writes it.
		aside string
		// job is a box whose job left no archive of its session, and its id.
		job func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string)
		// lacks is what archive lacks of a whole archive of session, "" for
		// nothing.
		lacks func(t *testing.T, archive []byte, session string) string
	}{
		{name: "Claude Code", packer: "tar", word: "-czf",
			aside: `out=$2; shift 2; "$own" -czf "$whole" "$@"; st=$?; exec 3> "$out"`,
			job: func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
				runner, b := sessionBox(t, "2.1.310")
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
				// THE SCRIPT'S OWN ARCHIVE GONE, as a job killed before its
				// harness exited never left one.
				if _, err := b.Exec(t.Context(), "rm -f "+shellword.Quote(paths(b).SessionCapture())+" "+
					shellword.Quote(paths(b).SessionDigests()), sandbox.ExecOptions{}); err != nil {
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
					strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1))
				const session = "ses_0123456789abcdef0123456789"
				if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
					[]byte("id "+session+"\nrelease 2.3.2\n")); err != nil {
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
		{name: "a read of a job that left no archive", take: sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound}},
		{name: "a read that packs the job as it stands", take: sandbox.SessionTake{Spool: testSpool(t), Pack: true, MaxBytes: takeBound}},
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
					// THE WORDS PASSED: held (the first pack holds; the case
					// starts the second read), half (the second's archive is
					// half made; the first pack goes on), release (the first
					// read was read whole; the second pack goes on).
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
						// so in its stead, so nothing waits on a word not
						// coming; it never blocks, as the case holds every fifo
						// open to read.
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

// meetingPacks is a stand-in for packer (Claude Code's tar, OpenCode's gzip)
// in b's bin directory, first on a pack's PATH, marking flags and talking
// through the fifos held there ([heldFifo]); a call not starting with word runs
// the real packer. The FIRST pack's call makes its archive whole, says held,
// and holds for half, failing on any other word. The SECOND makes its archive
// aside, writes its first half, lists b's staging directories, says half, and
// writes the rest once release comes.
func meetingPacks(t *testing.T, b sandbox.Sandbox, packer, word, aside, flags string) string {
	t.Helper()
	tool := func(name string) string {
		found, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("no %s on this machine's PATH: %v", name, err)
		}
		return shellword.Quote(found)
	}
	at := func(name string) string { return shellword.Quote(filepath.Join(flags, name)) }
	return "#!/bin/sh\n" +
		"own=" + tool(packer) + " whole=" + at("whole") + "\n" +
		`if [ "$1" != ` + shellword.Quote(word) + ` ]; then exec "$own" "$@"; fi` + "\n" +
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
		`for d in ` + shellword.Quote(paths(b).WorkDir()) + `/session-pack-staging.*; do if [ -d "$d" ]; then ` +
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
// every archive one kept for its read (executor.Paths.SessionPackHeld), found
// by pattern since each is named for its pack (executor.Paths.SessionPackStaging).
func packStagingLeft(t *testing.T, box sandbox.Sandbox) []string {
	t.Helper()
	res, err := box.Exec(t.Context(), "for d in "+shellword.Quote(paths(box).WorkDir())+"/session-pack-staging.*; do "+
		`if [ -e "$d" ] || [ -L "$d" ]; then printf '%s\n' "$d"; fi; done`, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("asking the box for its packs' staging directories: %v", err)
	}
	return strings.FieldsFunc(res.Stdout, func(r rune) bool { return r == '\n' })
}

// THE CLEAR BEFORE EVERY RUN REMOVES EVERY PACK ON DEMAND'S STAGING DIRECTORY,
// whichever read made it, by pattern (executor.Paths.SessionPackStaging): two
// left by packs cut short outlive no clear, also where the box's home path
// holds a space.
//
// Mutation: leave the staging directories out of the clear, or quote their
// pattern so it matches nothing, and they outlive it; leave the work directory
// unquoted, and the path with a space splits and neither is removed.
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

// heldTake is a box a take reads through, held at one step (its pack's login
// shell where pack is set, or its at-th open of an archive), closing held there
// and going on once resume is closed, so a case orders two takes, or lands an
// archive, between steps. A Claude Code take's second open is its scrub's
// second pass, by path; an OpenCode take's first is its one pass.
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
func takeWhole(ctx context.Context, t testing.TB, runner *executor.Runner, b sandbox.Sandbox) readBack {
	taken, err := runner.TakeSession(ctx, b, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound})
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
	// at is the open at which the harness's take goes on to the pass that
	// streams its archive ([heldTake]).
	at int
	// job is a box whose job is over and left no archive, the session's id,
	// and goOn, which writes the session on past an earlier pack, as a process
	// that outlived the job's harness may.
	job func(t *testing.T) (runner *executor.Runner, b sandbox.Sandbox, session string, goOn func())
	// wentOn reports whether archive holds what goOn wrote, failing the case
	// where it is no whole archive of session.
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
					line := shellword.Quote(`{"type":"user","text":"` + wentOnMark + `"}`)
					if _, err := b.Exec(t.Context(), "printf '%s\\n' "+line+" >> "+
						shellword.Quote(transcriptOf(b, ran.SessionID)), sandbox.ExecOptions{}); err != nil {
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
					strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1))
				const session = "ses_0123456789abcdef0123456789"
				if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
					[]byte("id "+session+"\nrelease 2.3.2\n")); err != nil {
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
	go func() { read <- takeWhole(t.Context(), t, runner, held) }()
	select {
	case <-held.held:
	case got := <-read:
		t.Fatalf("a read ended before it held where the case holds it: %q, %v", got.archive, got.err)
	}
	return read
}

// THE RUN'S OWN END PACK LEAVES NO STAGING DIRECTORY BEHIND. The script must
// remove its staging directory (executor.Paths.SessionStaging) once the
// archive is over the capture: nothing else does before the box's next run's
// clear. A pack on demand's is removed by its take's release too, so only the
// run's own end shows whether a pack's script cleans up.
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
// Where the job left no archive, a stop's read and a collection's each pack it
// now. Here the second read holds before its pack; the first packs and holds
// before the pass that streams it ([overJob].at); the session is written on,
// as a process outliving the job's harness may; the second packs and streams
// whole; then the first goes on. Neither is refused, the first's archive lacks
// the later write and the second's holds it, nothing lands over the job's
// capture, and no file is left.
//
// Mutation: move a pack on demand over the job's capture and stream what the
// capture holds, and the first read streams the second's archive (refused on
// Claude Code, the later write on OpenCode) and the capture holds an archive.
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
// STREAMS. Only the job's script moves an archive over its capture, and,
// outliving what a read saw of its job, it may do so while a read that found
// no archive streams its own pack's. Here the read holds before that pass
// while an archive lands over the capture: the read hands back its own pack's
// whole archive, and the capture holds the end pack's after.
//
// Mutation: move a pack on demand over the job's capture and stream what the
// capture holds, and the read streams the end pack's archive, which its scrub
// cannot read: the take is refused.
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
			if res, err := b.Exec(t.Context(), "printf '%s' "+shellword.Quote(final)+" > "+shellword.Quote(p.SessionCapture()+".next")+
				" && mv -f "+shellword.Quote(p.SessionCapture()+".next")+" "+shellword.Quote(p.SessionCapture()),
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

// brokenDigestLists are the ways a box's digest list
// (executor.Paths.SessionDigests) keeps no appended line, each made by the
// shell's how on the list's path: a directory, which refuses the append, and a
// link to /dev/null, which takes it and keeps nothing, as a full disk can.
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
// The digest keeps the box from taking an archive of its own for another box's
// and restoring it over the copy that went on past it. Here the list keeps no
// line ([brokenDigestLists]), and every pack, on either harness, fails saying
// why and moves nothing to where its read reads it: on demand, of a job that
// is over or beside a harness that may still work, and the run's own end,
// whose error stream says why and whose collection finds no capture.
//
// Mutation: move the archive into place before its digest is recorded, or
// leave the append unchecked, and every pack here holds; take the append's
// exit for the line, never reading it back, and every pack beside the list
// that keeps nothing holds.
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

// releaseWatch is a box that records the deadline of every release of a pack
// on demand's files (the `rm -rf` of its staging directory and held archive).
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

// A PACK'S RELEASE IS HELD TO ITS TAKE'S OWN DEADLINE. A pack on demand removes
// its archive on the take's close path, which the read waits on; under a bound
// of its own (sandbox.SessionPackWait) a box that stopped answering would hold
// the read past its deadline, into the room left for the outcome stamp and the
// box's pause. Less than [executor.PackReleaseFloor] left gets the floor; no
// deadline gets the pack's own bound. The runner's clock is the case's
// ([executor.Clocked]): off the wall clock, a slow take got the floor and a
// healthy build failed.
//
// Mutation: bound the release by the pack's own wait from when it runs, and the
// takes with a deadline are asked under one their rule does not give; drop the
// floor, or read the wall clock, and the take with a second to go is too.
func TestAPacksReleaseIsHeldToItsTakesOwnDeadline(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	for _, tc := range []struct {
		name string
		// left is what the take has to go when its release is made, by the
		// runner's clock; zero is a take with no deadline.
		left time.Duration
		// gets is the release's deadline, past the moment it is made.
		gets time.Duration
	}{
		{name: "a take with a minute to go", left: time.Minute, gets: time.Minute},
		{name: "a take with a second to go", left: time.Second, gets: executor.PackReleaseFloor},
		{name: "a take with no deadline", gets: sandbox.SessionPackWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forgetCapture(t, b)
			ctx, made := t.Context(), time.Now()
			if tc.left > 0 {
				deadline := time.Now().Add(time.Minute)
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
				made = deadline.Add(-tc.left)
			}
			executor.Clocked(runner, func() time.Time { return made })
			watched := &releaseWatch{Sandbox: b}
			if got := takeWhole(ctx, t, runner, watched); got.err != nil {
				t.Fatalf("the take was refused: %v", got.err)
			}
			watched.mu.Lock()
			asked := append([]time.Time(nil), watched.deadlines...)
			watched.mu.Unlock()
			if len(asked) == 0 {
				t.Fatal("the take's pack was never released")
			}
			for _, at := range asked {
				if want := made.Add(tc.gets); !at.Equal(want) {
					t.Errorf("the release was asked under the deadline %v, want %v", at, want)
				}
			}
			if left := packStagingLeft(t, b); len(left) > 0 {
				t.Errorf("the release left the pack's files behind: %s", strings.Join(left, ", "))
			}
		})
	}
}

// A RESTORE IS CONTINUED ONLY ONCE ITS DIGEST IS RECORDED IN THE BOX. A run
// restoring a carried session over a box copy lacking it records the archive's
// digest, so the archive carried in again is never restored over the copy that
// went on past it. Here the list keeps no line ([brokenDigestLists]) and a
// session is carried into a box that holds none: the run starts the session
// minted for it instead and says why.
//
// Mutation: continue the restore whose digest went unrecorded, and the run
// takes up the carried session with no line of it in the box.
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
