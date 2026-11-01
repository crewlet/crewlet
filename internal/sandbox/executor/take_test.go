package executor_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

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
// file a session holds: the transcript (raw, and JSON-escaped), a subagent's
// transcript, a tool result spilled to a text file, and a task list.
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
EOF
echo '{"type":"user","text":"` + plantedSecret + `"}' > "$p/$s/subagents/agent-1.jsonl"
printf 'output %s and %s done\n' '` + plantedSecret + `' '` + plantedShape + `' > "$p/$s/tool-results/out.txt"
echo '{"subject":"` + plantedSecret + `","password":"` + plantedKeyed + `"}' > "$CLAUDE_CONFIG_DIR/tasks/$s/1.json"
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// A KEPT SESSION CARRIES NO SECRET THE ENGINE PUT IN ITS BOX, AND STAYS THE
// ARCHIVE ITS HARNESS RESTORES. A run on a real box files its session with a
// planted secret in a transcript line, a subagent's transcript, a spilled tool
// result and a task list; taken as the run's script packed it, and packed again
// on demand, the archive streamed out holds none of them — not the value the
// engine named, raw or as JSON escapes it, not a credential only its shape gives
// away, not a value under a password's key — while every transcript line is
// still JSON, every member still there, and the box keeps the digest of exactly
// the bytes that left it.
//
// Mutation: skip the by-value pass, its JSON-escaped form, the redaction, the
// keyed mask, or the digest's record, or let the rewrite break a line, and a
// check here goes red.
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
			Pack: pack, Scrub: []string{plantedSecret, plantedQuoted}})
		if err != nil || taken.Body == nil {
			t.Fatalf("pack %v: TakeSession = %+v, %v", pack, taken, err)
		}
		archive, err := io.ReadAll(taken.Body)
		if err != nil {
			t.Fatalf("pack %v: reading the taken session: %v", pack, err)
		}
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
				if strings.Contains(body, secret) {
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
