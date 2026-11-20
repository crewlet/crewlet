package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// plant is a credential a case plants in a record's words, as redact's own
// corpus spells it (internal/redact/redacttest): the text planted, and the
// value no record may hold any piece of.
type plant struct{ name, text, value string }

// planted is a provider's key, a password's value and a bearer token.
func planted(t *testing.T) []plant {
	t.Helper()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	out := []plant{
		{"a provider's key", sample["anthropic-key"], sample["anthropic-key"]},
		{"a password", sample["password"], strings.TrimPrefix(sample["password"], "password: ")},
		{"a bearer token", "Authorization: Bearer " + sample["jwt"], sample["jwt"]},
	}
	for _, p := range out {
		if len(p.value) < 16 || p.value == p.text && p.name == "a password" {
			t.Fatalf("redact's corpus no longer has %s", p.name)
		}
	}
	return out
}

// pieceBytes is the shortest piece of a credential a record may not hold:
// six bytes, below which a piece is as likely any word's.
const pieceBytes = 6

// holdsNone fails where text holds a piece of p's value, or a marker the
// redaction wrote cut in part, which reads as the text's own words.
func holdsNone(t *testing.T, where, text string, p plant) {
	t.Helper()
	for i := 0; i+pieceBytes <= len(p.value); i++ {
		if piece := p.value[i : i+pieceBytes]; strings.Contains(text, piece) {
			at := strings.Index(text, piece)
			t.Errorf("%s holds %q of %s: …%s…", where, piece, p.name, text[max(0, at-40):min(len(text), at+40)])
			return
		}
	}
	for _, words := range redact.OwnWords(text) {
		if strings.Contains(words, "REDACTED") {
			t.Errorf("%s holds a marker cut in part: %q", where, words[:min(len(words), 120)])
			return
		}
	}
}

// encoded is the one event pub holds, as the store keeps it.
func encoded(t *testing.T, pub *collector) (string, *types.AgentPhaseCompleted) {
	t.Helper()
	ev, _ := published(t, pub)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := ev.Data.(*types.AgentPhaseCompleted)
	if !ok {
		t.Fatalf("published %T, want a phase's record", ev.Data)
	}
	return string(raw), rec
}

// quietEmitter is an emitter publishing into pub.
func quietEmitter(pub *collector, mu *sync.Mutex) emitter {
	return emitter{pub: pub, turn: Turn{RunID: "run-1", AgentID: "agent-1"}, role: "Engineer",
		tally: &Spend{}, mu: mu}
}

// outlined is a prompt of three parts, the middle holding said.
func outlined(said string) prompts.Prompt {
	parts := []string{"# Task\nShip the release.\n", "# Context\n" + said + "\n", "# Rules\nBe brief.\n"}
	var p prompts.Prompt
	for i, part := range parts {
		p.Sections = append(p.Sections, prompts.Section{Key: string(rune('a' + i)), Title: part[2:8],
			Bytes: len(part), Headed: true})
		p.Text += part
	}
	return p
}

// A PHASE'S RECORD HOLDS NO SECRET ITS WORDS CARRIED. A provider's key, a
// password and a bearer token, planted in each of the words a record keeps
// (its answer, notes, failure, both prompts, prose, each kind of tool's
// answer and a failed call's error, a call's arguments, a worker's record
// whole and a judge's reason), reach the store as markers, the words around
// them kept; and a prompt's outline still tiles its redacted text.
//
// Mutation: drop the redaction at any one path (recordedText for the answer,
// recordedPrompt, recordedNarration, recordedOutput, recordedArgs,
// events.Diagnostic), and its case holds the plant; misstate a part's size
// in redactedPrompt, and the outline is lost.
func TestAPhasesRecordHoldsNoSecretItsWordsCarried(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		said := "deploy with\n" + p.text + "\nthen check the logs"
		call := func(name, origin string, failed bool) phaseRecord {
			return phaseRecord{Phase: phase.Review, Iteration: 1, Result: toolloop.Result{
				Executions: []toolloop.Execution{{Round: 1, Name: name, Origin: origin, Output: said, Failed: failed}}}}
		}
		for name, rec := range map[string]phaseRecord{
			"the answer":           {Phase: phase.Review, Result: toolloop.Result{Text: said}},
			"the notes":            {Phase: phase.Review, Notes: said},
			"the failure":          {Phase: phase.Review, Failed: true, Err: errors.New("the deploy failed: " + said)},
			"the system prompt":    {Phase: phase.Review, System: outlined(said)},
			"the user prompt":      {Phase: phase.Review, User: outlined(said)},
			"the prose":            {Phase: phase.Review, Result: toolloop.Result{Narration: []toolloop.Narration{{Round: 1, Content: said}}}},
			"a builtin's answer":   call("read_file", tools.OriginBuiltin, false),
			"an MCP tool's answer": call("search_issues", "mcp:github", false),
			"an agent's answer":    call("a2a_ask", tools.OriginBuiltin, false),
			"a failed call":        call("post_message", "mcp:slack", true),
			"a call's arguments": {Phase: phase.Review, Result: toolloop.Result{Executions: []toolloop.Execution{{
				Round: 1, Name: "run_command", Output: "ok",
				Args: map[string]any{"command": said, "env": map[string]any{"note": said}, "argv": []any{said}},
			}}}},
		} {
			pub := &collector{}
			var mu sync.Mutex
			quietEmitter(pub, &mu).completed(context.Background(), rec)
			text, got := encoded(t, pub)
			holdsNone(t, p.name+" in "+name, text, p)
			if !strings.Contains(text, "then check the logs") || !strings.Contains(text, "[REDACTED:") {
				t.Errorf("%s in %s: the record lost the words around it or holds no marker", p.name, name)
			}
			for _, prompt := range []struct {
				text    string
				outline []types.PromptSection
				planted bool
			}{
				{got.SystemPrompt, got.SystemSections, name == "the system prompt"},
				{got.UserPrompt, got.UserSections, name == "the user prompt"},
			} {
				size := 0
				for _, s := range prompt.outline {
					size += s.Bytes
				}
				if prompt.planted && (len(prompt.outline) != 3 || size != len(prompt.text)) {
					t.Errorf("%s in %s: an outline of %d parts spans %d bytes of a %d-byte prompt, want 3 "+
						"tiling it", p.name, name, len(prompt.outline), size, len(prompt.text))
				}
			}
		}

		pub := &collector{}
		var mu sync.Mutex
		quietEmitter(pub, &mu).subagentCompleted(context.Background(), subagent.Result{ID: "t1", Worker: "w",
			Status: subagent.StatusOK, Text: said, SystemPrompt: outlined(said), UserPrompt: outlined(said),
			Narration:  []toolloop.Narration{{Round: 1, Content: said}},
			Executions: []toolloop.Execution{{Round: 1, Name: "read_file", Args: map[string]any{"q": said}, Output: said}},
			Error:      said})
		text, _ := encoded(t, pub)
		holdsNone(t, p.name+" in a worker's record", text, p)

		pub = &collector{}
		quietEmitter(pub, &mu).judged(context.Background(), phase.Execute, 1, 3, 2, extension.Decision{Extend: true,
			Reason: said}, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), 0)
		text, _ = encoded(t, pub)
		holdsNone(t, p.name+" in a judge's reason", text, p)
	}
}

// A PHASE'S RECORD HOLDS NO SECRET WHEREVER ITS CUT FALLS. Each of the words
// a record cuts to a share (the answer, the notes, the failure, a prompt, the
// prose, a call's answer as a long list's row and as one past the whole
// list's share) is planted past its share with credentials back to back, so
// the cut falls across one, as it may fall anywhere: the record holds no
// piece of any, no marker in part, and its cut states the redaction's size.
//
// Mutation: cut before redacting at any one path (the raw text in
// recordedText, a raw answer in recordedOutput), and its case holds the head
// of the credential its cut ran across; cut inside a marker (no move back in
// keptOpening), and it holds the marker in part.
func TestAPhasesRecordHoldsNoSecretWhereverItsCutFalls(t *testing.T) {
	t.Parallel()
	for i, p := range planted(t) {
		// PAST THE SHARE ONCE REDACTED, which a marker shorter than what it
		// replaces makes shorter than the text.
		redacted := len(redact.Secrets(p.text + "\n"))
		dense := func(share int) string { return strings.Repeat(p.text+"\n", share/redacted+2) }
		// THE MEGABYTES ONCE: every plant's cut at every offset is the case
		// above, and these differ from the smaller shares only in where.
		answer, row, big := "", "", ""
		var rows []toolloop.Execution
		if i == 0 {
			answer, row, big = dense(responseBytes), dense(toolListBytes/2), dense(toolListBytes+1<<10)
			rows = []toolloop.Execution{{Round: 1, Name: "read_file", Output: row},
				{Round: 1, Name: "read_file", Output: row}, {Round: 1, Name: "read_file", Output: row}}
		}
		notes, prose := dense(notesBytes), dense(narrationBytes)
		for name, c := range map[string]struct {
			rec   phaseRecord
			field func(*types.AgentPhaseCompleted) string
			// whole is the text whose redaction's size the cut states; a
			// diagnostic's clip states none.
			whole string
		}{
			"the answer": {rec: phaseRecord{Phase: phase.Review, Result: toolloop.Result{Text: answer}},
				field: func(r *types.AgentPhaseCompleted) string { return r.Response }, whole: answer},
			"the notes": {rec: phaseRecord{Phase: phase.Review, Notes: notes},
				field: func(r *types.AgentPhaseCompleted) string { return r.Notes }, whole: notes},
			"the failure": {rec: phaseRecord{Phase: phase.Review, Failed: true,
				Err: errors.New(dense(events.MaxDiagnosticBytes))},
				field: func(r *types.AgentPhaseCompleted) string { return r.Error }},
			"a prompt": {rec: phaseRecord{Phase: phase.Review, User: outlined(dense(promptBytes))},
				field: func(r *types.AgentPhaseCompleted) string { return r.UserPrompt }},
			"the prose": {rec: phaseRecord{Phase: phase.Review, Result: toolloop.Result{Narration: []toolloop.Narration{
				{Round: 1, Content: "a short round"}, {Round: 2, Content: prose}}}},
				field: func(r *types.AgentPhaseCompleted) string { return r.RoundNarration[1]["content"].(string) }},
			"a long list's answers": {rec: phaseRecord{Phase: phase.Review, Result: toolloop.Result{Executions: rows}},
				field: func(r *types.AgentPhaseCompleted) string { return r.ToolExecutions[0]["result"].(string) },
				whole: row},
			"an answer past the list's share": {rec: phaseRecord{Phase: phase.Review, Result: toolloop.Result{
				Executions: []toolloop.Execution{{Round: 1, Name: "read_file", Output: big}}}},
				field: func(r *types.AgentPhaseCompleted) string { return r.ToolExecutions[0]["result"].(string) },
				whole: big},
		} {
			if i > 0 && (name == "the answer" || strings.Contains(name, "list")) {
				continue
			}
			pub := &collector{}
			var mu sync.Mutex
			quietEmitter(pub, &mu).completed(context.Background(), c.rec)
			_, rec := encoded(t, pub)
			got := c.field(rec)
			if !strings.Contains(got, "not kept on") && !strings.Contains(got, "diagnostic truncated") {
				t.Errorf("%s: %s was not cut, so no cut was tried", p.name, name)
			}
			holdsNone(t, p.name+" in "+name, got, p)
			if c.whole == "" {
				continue
			}
			if _, ok := recordTally.Opening(got, len(redact.Secrets(c.whole))); !ok {
				t.Errorf("%s in %s: the cut does not state the redaction's size: …%q", p.name, name,
					got[max(0, len(got)-80):])
			}
		}
	}
}

// A RECORDED TEXT HOLDS NO SECRET HOWEVER ITS CUT FALLS: at every share from
// none to past the whole, the cut before, inside and after a credential, the
// text's record holds no piece of it and no marker in part, stays within its
// share where the marker fits, and is the whole redaction where it all fits.
//
// Mutation: cut before redacting (keptText), and a share ending inside the
// credential keeps its head; drop the move back from a marker
// (redact.MarkersAround), and a share ending inside the marker keeps it in part.
func TestARecordedTextHoldsNoSecretHoweverItsCutFalls(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		// A TAIL PAST THE CUT'S MARKER, so some share's cut falls in the
		// credential's.
		text := "the build passed.\n" + p.text + "\n" + strings.Repeat("and then it shipped. ", 8)
		whole := redact.Secrets(text)
		for share := range kept.Quoted(text) + 8 {
			got := recordedText(text, share)
			holdsNone(t, p.name+" cut at "+strconv.Itoa(share), got, p)
			if share >= 96 && kept.Quoted(got) > share {
				t.Errorf("%s cut at %d: kept %d bytes", p.name, share, kept.Quoted(got))
			}
			if kept.Quoted(whole) <= share && got != whole {
				t.Errorf("%s cut at %d: kept %q, want the whole redaction %q", p.name, share, got, whole)
			}
		}
	}
}

// A CALL'S ARGUMENTS ON A RECORD HOLD NO SECRET AND STAY ONE OBJECT. A
// credential in a string anywhere in them, a value under a password's key, a
// number under one, an AWS secret its key alone names, values under a
// container a password's key names, and keys that are credentials themselves
// are each redacted as values: the text the record holds is one JSON object
// with every other value as it was, and two keys a redaction makes one keep
// both values.
//
// Mutation: read a value without its key (no takenByKey), and the AWS secret
// is kept and the password's marker is not redact's; drop the renaming, and
// one of two token keys' values is lost.
func TestACallsArgumentsOnARecordHoldNoSecretAndStayOneObject(t *testing.T) {
	t.Parallel()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	aws := strings.TrimPrefix(sample["aws-secret-access-key"], "aws_secret_access_key = ")
	password := strings.TrimPrefix(sample["password"], "password: ")
	key := sample["anthropic-key"]
	args := map[string]any{
		"command":     "curl -H 'x-api-key: " + key + "' https://example.com",
		"password":    password,
		"db_pwd":      json.Number("9876543210987"),
		"credentials": map[string]any{"SecretAccessKey": aws, "AccessKeyId": "AKIDEXAMPLE"},
		"passwords":   map[string]any{"staging": password, "list": []any{password}},
		"tokens":      map[string]any{sample["github-token"]: "one", "ghp_" + strings.Repeat("Zz9", 12): "two"},
		"path":        "internal/a.go",
		"count":       json.Number("12345678901234567890"),
	}
	raw := encodeArgs(recordedArgs(args))
	for _, secret := range []string{aws, password, key, "9876543210987", sample["github-token"], "ghp_Zz9Zz9Zz9"} {
		if strings.Contains(raw, secret[:min(len(secret), 12)]) {
			t.Errorf("the arguments hold %q: %s", secret[:12], raw)
		}
	}
	back := tools.ReadArgs(raw)
	if back == nil {
		t.Fatalf("the arguments are no longer one object: %s", raw)
	}
	tokens, _ := back["tokens"].(map[string]any)
	if back["path"] != "internal/a.go" || back["count"] != json.Number("12345678901234567890") ||
		len(tokens) != 2 || back["password"] != "[REDACTED:password]" ||
		back["credentials"].(map[string]any)["AccessKeyId"] != "AKIDEXAMPLE" {
		t.Errorf("the arguments kept %s, want every other value as it was and both tokens' values", raw)
	}
	if args["password"] != password {
		t.Error("the call's own arguments were changed: the model's copy must stay as it came")
	}
}

// AN EXECUTOR RUN'S RECORD HOLDS NO SECRET ITS LOG'S CUT RAN ACROSS. A call's
// answer its run's log kept only the opening of, raw, the log's cut falling at
// every offset of a credential, reaches the record as the redaction of the
// opening's settled lines: no piece of the credential, the lines before it
// kept, and a cut that says the rest is left out.
//
// Mutation: redact the log's text as it stands (no recordedCut), and the head
// of the credential the log's cut ran across is kept.
func TestAnExecutorRunsRecordHoldsNoSecretItsLogsCutRanAcross(t *testing.T) {
	t.Parallel()
	const runLog kept.Record = "the run's record"
	for _, p := range planted(t) {
		lead := "fetched the page\n"
		// THE LOG'S CUT, for an answer of this size of no byte it escapes.
		size := 3 * sandbox.MaxBridgeOutputBytes
		probe, _ := runLog.Text(lead+strings.Repeat("a", size-len(lead)), sandbox.MaxBridgeOutputBytes)
		opening, ok := sandbox.OutputOpening(probe, size)
		if !ok {
			t.Fatal("the run's log no longer reads back the cut this test writes as its own")
		}
		for at := 1; at < len(p.text); at++ {
			before := len(opening) - at - len(lead)
			raw := lead + strings.Repeat("b", before) + p.text + "\n"
			raw += strings.Repeat("c", size-len(raw))
			logged, _ := runLog.Text(raw, sandbox.MaxBridgeOutputBytes)
			if cut, _ := sandbox.OutputOpening(logged, size); len(cut) != len(opening) {
				t.Fatalf("%s: the log kept %d bytes, not the %d the cases place it by", p.name, len(cut), len(opening))
			}
			res := agentRunResult("", []BridgedCall{{Call: ledger.Call{Name: "fetch", Result: logged},
				OutputBytes: size}}, nil, RunRecord{}, time.Time{}, time.Time{})
			pub := &collector{}
			var mu sync.Mutex
			quietEmitter(pub, &mu).completed(context.Background(), phaseRecord{Phase: phase.Execute,
				Result: res.Result})
			_, rec := encoded(t, pub)
			got := rec.ToolExecutions[0]["result"].(string)
			holdsNone(t, p.name+" cut "+strconv.Itoa(at)+" bytes in", got, p)
			if !strings.HasPrefix(got, lead) || !strings.Contains(got, "not kept on") {
				t.Errorf("%s cut %d bytes in: kept %q, want the lines before it and the cut said", p.name, at,
					got[:min(len(got), 120)])
			}
		}
	}
}

// A TURN'S SPEND HOLDS NO SECRET A REVIEW'S NOTES CARRIED: the notes of a
// review that sent the work back, which a task's turn card quotes on a record
// of its own, are held redacted, as the phase's record holds them.
//
// Mutation: hold the notes as they came (Spend.record), and the spend holds
// the plant.
func TestATurnsSpendHoldsNoSecretAReviewsNotesCarried(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		pub := &collector{}
		var mu sync.Mutex
		e := quietEmitter(pub, &mu)
		e.completed(context.Background(), phaseRecord{Phase: phase.Review,
			Decision: string(phase.SelfIterate), Notes: "redo it:\n" + p.text + "\nthen check the logs"})
		holdsNone(t, p.name+" in the spend's review", e.tally.Review, p)
		if !strings.Contains(e.tally.Review, "then check the logs") {
			t.Errorf("%s: the spend's review is %q, want the words around the marker", p.name, e.tally.Review)
		}
	}
}
