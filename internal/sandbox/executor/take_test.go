package executor_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
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
// the session's whole, and the box knows it holds it.
//
// Mutation: pack anywhere but the runner, or leave the digest out of the pack,
// and the box takes its own archive for another box's.
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
	described, body, err := runner.PackCapture(t.Context(), b)
	if err != nil || body == nil {
		t.Fatalf("PackCapture = %v, %v", body, err)
	}
	defer func() { _ = body.Close() }()
	if described.SessionID != res.SessionID || described.Path != paths(b).SessionCapture() {
		t.Errorf("the pack describes %+v, want session %s at %s", described, res.SessionID,
			paths(b).SessionCapture())
	}
	members := archiveMembers(t, body)
	if _, ok := members["projects/"+executor.ClaudeProjectDir+"/"+res.SessionID+".jsonl"]; !ok {
		t.Errorf("the pack holds no transcript: %v", members)
	}
	capture := []byte(boxFile(t, b, paths(b).SessionCapture()))
	if digests := boxFile(t, b, paths(b).SessionDigests()); !strings.Contains(digests,
		res.SessionID+" "+digestOf(t, capture)+"\n") {
		t.Errorf("the pack kept no digest of the archive it made: %q", digests)
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
		described, body, err := runner.PackCapture(t.Context(), b)
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
		_, body, err := runner.PackCapture(t.Context(), b)
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
		described, body, err := runner.PackCapture(t.Context(), b)
		if err != nil || body != nil {
			t.Fatalf("PackCapture with no transcript = %+v, %v, %v; want nothing", described, body, err)
		}
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, MaxBytes: takeBound})
		if err != nil || taken.Body != nil {
			t.Fatalf("a take with no transcript = %+v, %v; want nothing", taken, err)
		}
	})
}
