package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// Kimi Code 2.7.3's own lines, as measured against a stub answering 429, 401
// and 500 (see the profile): what a failed run leaves on stdout and stderr,
// and the turn.ended line its wire log ends on, naming the model each step
// ran on (`modelSteps`, and `replyModel` once it answered).
const (
	kimi27Version = `{"role":"meta","type":"system.version","version":"2.7.3"}`
	kimi27Retry   = `{"role":"meta","type":"turn.step.retrying","failed_attempt":1,"next_attempt":2,` +
		`"max_attempts":3,"delay_ms":539.4,"error_name":"rate_limit",` +
		`"error_message":"You've reached your 5-hour usage limit. Resets in 2h.","status_code":429}`
	kimi27Stderr = "error: failed to run prompt: Prompt turn ended with reason: failed\n" +
		"See log: /home/seat/.kimi-code/logs/kimi-code.log\n"
	kimi27Spent = `{"type":"turn.ended","agentId":"main","turnId":0,"reason":"failed","error":` +
		`{"code":"provider.rate_limit","message":"You've reached your 5-hour usage limit. Resets in 2h.",` +
		`"name":"APIProviderRateLimitError","details":{"requestId":null,"statusCode":429},"retryable":true},` +
		`"modelSteps":[{"model":"m","provider":"openai","isKfc":false,"step":1,"role":"step"}]}`
	kimi27Refused = `{"type":"turn.ended","agentId":"main","turnId":0,"reason":"failed","error":` +
		`{"code":"provider.auth_error","message":"Invalid Authentication","name":"APIStatusError",` +
		`"details":{"requestId":null,"statusCode":401},"retryable":false},` +
		`"modelSteps":[{"model":"m","provider":"openai","isKfc":false,"step":1,"role":"step"}]}`
	kimi27ServerError = `{"type":"turn.ended","agentId":"main","turnId":0,"reason":"failed","error":` +
		`{"code":"provider.api_error","message":"internal failure","name":"APIStatusError",` +
		`"details":{"requestId":null,"statusCode":500},"retryable":true},` +
		`"modelSteps":[{"model":"m","provider":"openai","isKfc":false,"step":1,"role":"step"}]}`
	kimi27Completed = `{"type":"turn.ended","agentId":"main","turnId":0,"reason":"completed","durationMs":753,` +
		`"replyModel":{"provider":"openai","model":"m"},"modelSteps":[{"model":"m","provider":"openai","isKfc":false,"step":1,"role":"step"}]}`
)

// A KIMI CODE RUN IS JUDGED BY THE CLASS ITS RECORD ENDS ON, because its
// stdout and stderr carry none: a spent plan sits the entry out and a
// refused sign-in is said as one, while a 429 retried to an answer, or one
// whose last attempt met a 500, is neither, though stdout named `rate_limit`.
//
// The record is a file a process of the run can write, so it is read only for
// a run the harness failed: a planted report on a run that answered is nothing.
//
// Mutation: drop the profile's limit_events or auth_events, and the spent plan
// or the refusal is a server failure; read the record's events from stdout,
// and nothing classifies; match the retry line, or read the record of a run
// that answered, and the answered run benches.
func TestAKimiCodeRunIsJudgedByTheClassItsRecordEndsOn(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("kimi-code")
	if !ok {
		t.Fatal("no kimi-code profile")
	}
	stdout := kimi27Version + "\n" + kimi27Retry + "\n"
	for _, tc := range []struct {
		name     string
		out      harnessOutput
		classify bool
		want     llm.ErrorKind
		saying   string
	}{
		{"a spent plan", harnessOutput{stdout: stdout, stderr: kimi27Stderr, record: kimi27Spent, failed: true},
			true, llm.KindRateLimit, "5-hour usage limit"},
		{"a refused sign-in", harnessOutput{stdout: kimi27Version, stderr: kimi27Stderr, record: kimi27Refused,
			failed: true}, true, llm.KindAuth, "Invalid Authentication"},
		{"a 429 retried to an answer", harnessOutput{stdout: stdout + `{"role":"assistant","content":"pong"}`,
			record: kimi27Completed}, false, 0, ""},
		{"a 429, then a 500", harnessOutput{stdout: stdout, stderr: kimi27Stderr, record: kimi27ServerError,
			failed: true}, false, 0, ""},
		{"a spent plan planted in the record of a run that answered", harnessOutput{
			stdout: kimi27Version + "\n" + `{"role":"assistant","content":"pong"}`,
			record: kimi27Completed + "\n" + kimi27Spent}, false, 0, ""},
		{"a refusal planted in the record of a run that answered", harnessOutput{
			stdout: kimi27Version + "\n" + `{"role":"assistant","content":"pong"}`,
			record: kimi27Refused}, false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hit, fired := classifyMarkers(p, tc.out)
			if !tc.classify {
				if fired {
					t.Fatalf("classified %v (%q), want nothing: the run did not end on a limit or a sign-in",
						hit.Kind, hit.Said)
				}
				return
			}
			if !fired || hit.Kind != tc.want || hit.Source != sourceEvent || !strings.Contains(hit.Said, tc.saying) {
				t.Fatalf("fired=%v kind=%v source=%v said=%q, want %v from the record saying %q",
					fired, hit.Kind, hit.Source, hit.Said, tc.want, tc.saying)
			}
		})
	}
}

// A RECORD EVENT IS LOOKED FOR IN THE RECORD ALONE, and a stdout event on stdout
// alone: a line the model could not write in one place is not read from the
// other, nor from stderr.
//
// Mutation: drop the source test in limitEventHit or the auth scan, and the
// turn.ended line on stdout benches the entry; give the profile a stderr
// marker for `provider.rate_limit` again, and the same line on stderr does.
func TestARecordEventIsLookedForInTheRecordAlone(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("kimi-code")
	for name, out := range map[string]harnessOutput{
		"stdout": {stdout: kimi27Spent, failed: true},
		"stderr": {stderr: kimi27Spent, failed: true},
	} {
		if hit, fired := classifyMarkers(p, out); fired {
			t.Errorf("classified %v from %s, want the record's event read from the record only", hit.Kind, name)
		}
	}
}

// THE RECORD'S REPORTS ARE THE LINES A RECORD EVENT MATCHES, the last few: the
// bulk of a session's record (its prompt, tools, steps, and an end no event
// reads) is never kept.
//
// Mutation: keep every line, and the step and the prompt come back too.
func TestTheRecordsReportsAreTheLinesARecordEventMatches(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("kimi-code")
	r := &recordEvents{p: p}
	r.line(`{"type":"profile.bind","agentId":"main","systemPrompt":"You are Kimi Code CLI"}`)
	r.line(`{"type":"context.append_loop_event","event":{"type":"step.end","finishReason":"error"}}`)
	r.line(kimi27ServerError)
	for range maxFailureEvents {
		r.line(kimi27Refused)
	}
	r.line(kimi27Spent)
	lines := strings.Split(r.text(), "\n")
	if len(lines) != maxFailureEvents || lines[len(lines)-1] != kimi27Spent {
		t.Fatalf("kept %d lines ending %q, want the last %d reports, the spent plan last",
			len(lines), lines[len(lines)-1], maxFailureEvents)
	}
	if strings.Contains(r.text(), "profile.bind") || strings.Contains(r.text(), "step.end") ||
		strings.Contains(r.text(), "provider.api_error") {
		t.Fatalf("kept %q, want only the reports", r.text())
	}
}

// A TEXT FUNCTION'S RECORD IS FOLLOWED FOR ITS REPORTS: the file the profile's
// requests name, written once the run started, owned by the call's directory.
// Another call's record in the same home, and one there before the run, are
// not this run's.
//
// Mutation: drop the owner, and the other call's spent plan is this run's;
// follow nothing for a text function, and its spent plan is never read.
func TestATextFunctionsRecordIsFollowedForItsReports(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("kimi-code")
	root := t.TempDir()
	checkout := &Checkout{Home: filepath.Join(root, "home"), Work: filepath.Join(root, "work")}
	if err := os.MkdirAll(checkout.Work, 0o700); err != nil {
		t.Fatal(err)
	}
	wire := func(dir, cwd string, lines ...string) {
		path := filepath.Join(checkout.Home, ".kimi-code", "sessions", dir, "session_x", "agents", "main", "wire.jsonl")
		bind := `{"type":"profile.bind","agentId":"main","environmentDisclosure":{"cwd":"` + cwd + `"}}`
		writeFile(t, path, strings.Join(append([]string{bind}, lines...), "\n")+"\n")
	}
	wire("wd_before", checkout.Work, kimi27Refused)
	end := followRecordEvents(p, checkout, "")
	wire("wd_another", filepath.Join(root, "another-call"), kimi27Refused)
	wire("wd_work", checkout.Work, kimi27Spent)
	if got := end(); got != kimi27Spent {
		t.Fatalf("followed %q, want this run's own report alone", got)
	}
}
