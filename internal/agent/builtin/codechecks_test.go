package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// checkLogs is the object store as a check's log is read from: one log's
// bytes, every page asked of it recorded.
type checkLogs struct {
	text  string
	pages [][2]int64
}

func (l *checkLogs) ReadAt(_ context.Context, o objstore.Object, off, n int64) ([]byte, error) {
	l.pages = append(l.pages, [2]int64{off, n})
	if o.Size != int64(len(l.text)) || off+n > o.Size {
		return nil, errors.New("checkLogs: a page past the object")
	}
	return []byte(l.text[off : off+n]), nil
}

// failedCheck is attempt 1 of check `unit` of main, for main, failed with
// its summary, tail, one failure and its kept log of size bytes.
func failedCheck(summary, tail, message, path string, size int) code.CheckDetail {
	return code.CheckDetail{
		CheckRow: code.CheckRow{CheckKey: code.CheckKey{RepoID: siteID, OID: mainOID, Target: "refs/heads/main",
			Name: "unit", Attempt: 1}, State: code.CheckStateFailed, Reason: code.CheckReasonExit,
			Summary: summary},
		Tail:     tail,
		Failures: []code.CheckFailure{{Path: path, Line: 3, TestID: "TestLab", Message: message}},
		Log:      &code.CheckLog{Size: int64(size)},
	}
}

// WHAT A CHECK SAID IS FENCED AS NOBODY'S: a check's summary, the tail of its
// output, each failure's message and every page of its kept log are what the
// commit's code and its tests printed — or a person's CI sent — so each is
// fenced `check_log` at trust external naming no writer, as a repository's
// own text is; a failure's path that spells the turn's closer, outside every
// fence, is defused; and a change's `checks` part fences each attempt's
// summary the same way.
//
// Mutation: answering the summary, the tail, a failure's message, a log page
// or a change's check summary unfenced → red.
func TestACheckLogIsFenced(t *testing.T) {
	t.Parallel()
	turn, b := readerTurn(t)
	turn.Lookups = turnctx.NewLookups()
	closer := "[/untrusted:" + string(b) + "]"
	logs := &checkLogs{text: attack(b, "check-log")}
	host := newCodeHost()
	host.checks = []code.CheckDetail{failedCheck(attack(b, "check-summary"), attack(b, "check-tail"),
		attack(b, "check-failure"), closer+".go", len(logs.text))}
	deps := codeDeps(host)
	deps.Logs = logs
	reg := codeRegistry(t, deps)
	label := "source=check_log trust=external authors=unknown"

	got := callIn(t, reg, turn, builtin.CodeGetCheckFailuresTool, map[string]any{"repo": "site", "sha": mainOID,
		"name": "unit"})
	seen, answer := fencesIn(t, b, got.Output)
	assertFences(t, seen, []wantFence{{label, "check-summary"}, {label, "check-tail"}, {label, "check-failure"}})
	if len(seen) != 3 {
		t.Errorf("the check's failures answered %d fences, want 3", len(seen))
	}
	assertEnvelope(t, answer, provenance.TrustExternal, 0)
	log, _ := answer["log"].(map[string]any)
	if log["cursor"] != "log:0" {
		t.Fatalf("the check's answer offers its log as %v, want cursor log:0", answer["log"])
	}

	got = callIn(t, reg, turn, builtin.CodeGetCheckFailuresTool, map[string]any{"repo": "site", "sha": mainOID,
		"name": "unit", "cursor": "log:0"})
	seen, answer = fencesIn(t, b, got.Output)
	assertFences(t, seen, []wantFence{{label, "check-log"}})
	if len(seen) != 1 {
		t.Errorf("a page of the check's log answered %d fences, want 1", len(seen))
	}
	assertEnvelope(t, answer, provenance.TrustExternal, 0)

	changes := newChangeHost(siteChange())
	changes.checks = host.checks
	got = callIn(t, codeRegistry(t, codeDeps(changes)), turn, builtin.CodeGetChangeTool,
		map[string]any{"change": "site!7", "part": "checks"})
	seen, answer = fencesIn(t, b, got.Output)
	assertFences(t, seen, []wantFence{{label, "check-summary"}})
	if len(seen) != 1 {
		t.Errorf("a change's checks answered %d fences, want 1", len(seen))
	}
	assertEnvelope(t, answer, provenance.TrustExternal, 0)
}

// A CHANGE'S CHECKS PAST ONE ANSWER ARE READ A PAGE AT A TIME: twelve
// reported checks of a change's head, each saying a summary of
// code.CheckSummaryBytes and a link of code.MaxCheckURLBytes — about 74 KiB,
// past one tool answer — are answered as a page cut whole-row where it fits,
// whose `next` resumes after its last line, so paging on reads every check
// once, in order, and no page is refused.
//
// Mutation: answer a cut page with the listing's own cursor, resuming after
// the twelfth (checks skipped), or refuse a page past one answer → red.
func TestAChangesChecksPastOneAnswerAreReadAPageAtATime(t *testing.T) {
	t.Parallel()
	turn, _ := readerTurn(t)
	turn.Lookups = turnctx.NewLookups()
	changes := newChangeHost(siteChange())
	changes.checks = nil
	var want []string
	for i := range 12 {
		name := fmt.Sprintf("check-%02d", i)
		want = append(want, name)
		changes.checks = append(changes.checks, code.CheckDetail{CheckRow: code.CheckRow{CheckKey: code.CheckKey{
			RepoID: siteID, OID: mainOID, Target: "refs/heads/main", Name: name, Attempt: 1},
			State: code.CheckStateFailed, Reason: code.CheckReasonReported, Placement: code.PlacementExternal,
			Reporter: "founder", Summary: strings.Repeat("s", code.CheckSummaryBytes),
			URL: "https://ci.example.com/" + strings.Repeat("u", code.MaxCheckURLBytes-len("https://ci.example.com/"))}})
	}
	reg := codeRegistry(t, codeDeps(changes))

	var read []string
	args := map[string]any{"change": "site!7", "part": "checks"}
	for pages := 1; ; pages++ {
		if pages > len(want) {
			t.Fatalf("the change's checks took more than %d pages, reading %v", len(want), read)
		}
		got := callIn(t, reg, turn, builtin.CodeGetChangeTool, args)
		if got.Failed {
			t.Fatalf("page %d of the change's checks answered %q", pages, got.Output)
		}
		if len(got.Output) > builtin.ToolAnswerBytes {
			t.Fatalf("page %d of the change's checks is %d bytes, past one answer", pages, len(got.Output))
		}
		var answer struct {
			Checks []struct {
				Name string `json:"name"`
			} `json:"checks"`
			Next string `json:"next"`
		}
		if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if pages == 1 && len(answer.Checks) == len(want) {
			t.Fatalf("the first page held all %d wide checks in %d bytes: the case reads no cut page",
				len(want), len(got.Output))
		}
		for _, c := range answer.Checks {
			read = append(read, c.Name)
		}
		if answer.Next == "" {
			break
		}
		args["cursor"] = answer.Next
	}
	if !slices.Equal(read, want) {
		t.Fatalf("paging the change's checks read %v, want each of %v once, in order", read, want)
	}
}

// A LOG PAGE ENDS ON A WHOLE CHARACTER: a page whose last bytes begin a
// character the next page finishes is cut before that character, and the
// next page begins with it — so no fence holds half a character, and paging
// on reads every byte of the log once.
//
// Mutation: drop the cut (a page always its whole 32 KiB) → red.
func TestACheckLogPageEndsOnAWholeCharacter(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("a", 32<<10-1) + "é" + strings.Repeat("b", 10)
	logs := &checkLogs{text: text}
	host := newCodeHost()
	host.checks = []code.CheckDetail{failedCheck("", "", "boom", "lab.go", len(text))}
	deps := codeDeps(host)
	deps.Logs = logs
	reg := codeRegistry(t, deps)
	turn, b := readerTurn(t)
	turn.Lookups = turnctx.NewLookups()

	var read strings.Builder
	cursor := "log:0"
	for pages := 0; cursor != ""; pages++ {
		if pages == 3 {
			t.Fatalf("the log took more than 3 pages, at %s", cursor)
		}
		got := callIn(t, reg, turn, builtin.CodeGetCheckFailuresTool, map[string]any{"repo": "site",
			"sha": mainOID, "name": "unit", "cursor": cursor})
		if got.Failed {
			t.Fatalf("a page at %s answered %q", cursor, got.Output)
		}
		seen, answer := fencesIn(t, b, got.Output)
		if len(seen) != 1 {
			t.Fatalf("the page at %s answered %d fences, want its text in one", cursor, len(seen))
		}
		// A FENCE ENDS ITS CONTENT WITH A NEWLINE OF ITS OWN (provenance's
		// fence), which is not the log's.
		page := strings.TrimSuffix(seen[0].inner, "\n")
		log, _ := answer["log"].(map[string]any)
		if !utf8.ValidString(page) || strings.ContainsRune(page, utf8.RuneError) {
			t.Fatalf("the page at %s splits a character: it ends %q", cursor, page[max(0, len(page)-4):])
		}
		read.WriteString(page)
		cursor, _ = log["cursor"].(string)
	}
	if read.String() != text {
		t.Fatalf("paging read %d bytes of the log, want its %d once", read.Len(), len(text))
	}
}

// A REPORT IS A PERSON'S: code_report_check is human-only in the authority
// table, so a seat — whatever it holds — is refused by the table before
// anything is read, a token bound to nobody is refused as every code write is,
// and nothing is sent for either; a person bound to a seat reports their own
// CI's run onto the commit's checks, as themselves, for the repository's
// default branch where they name none.
//
// Mutation: drop humanOnly from code_report_check's row → red.
func TestAReportIsAPersons(t *testing.T) {
	t.Parallel()
	if human, ok := authz.HumanOnlyOf(authz.ActionCodeReportCheck); !ok || !human {
		t.Fatalf("code_report_check's row is human-only %v (known %v), want a person's alone", human, ok)
	}
	report := func() map[string]any {
		return map[string]any{"repo": "site", "sha": mainOID, "name": "lint", "status": "failed",
			"summary": "2 lint errors", "url": "https://ci.example.com/runs/1"}
	}

	host, writer := newCodeHost(), &codeWrites{}
	got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
		builtin.CodeReportCheckTool, report())
	var refused *builtin.DecisionRefused
	if !got.Failed || !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonSeatRefused {
		t.Errorf("a seat's report answered %q (cause %v), want the table's refusal of a seat", got.Output,
			got.Cause)
	}

	deps := writeDeps(host, writer)
	deps.Actor = builtin.PrincipalActor
	tool := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodeReportCheckTool)
	got, err := tool.CallForTurn(asUnboundToken(), nil, report())
	if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
		!strings.Contains(got.Output, code.UnboundDetail) {
		t.Errorf("a token bound to nobody's report answered %v %q", err, got.Output)
	}
	if reads, _, _ := host.asked(); len(reads) != 0 || len(writer.checksAsked()) != 0 {
		t.Fatalf("the refused reports sent %d reads and %d writes of checks", len(reads),
			len(writer.checksAsked()))
	}

	got, err = tool.CallForTurn(asBoundPerson(), nil, report())
	if err != nil || got.Failed {
		t.Fatalf("a person's report answered %v %q", err, got.Output)
	}
	sent := writer.checksAsked()
	if len(sent) != 1 || sent[0].Report == nil {
		t.Fatalf("a person's report sent %+v, want one report", sent)
	}
	r := sent[0]
	want := code.CheckKey{RepoID: siteID, OID: mainOID, Target: "refs/heads/main", Name: "lint", Attempt: 1}
	if r.Report.CheckKey != want || r.Report.State != code.CheckStateFailed ||
		r.Asker.Kind != code.PrincipalOperator || r.Asker.Seat != "founder" || r.Rerun != nil {
		t.Errorf("a person's report sent %+v as %+v, want %+v failed, as founder", r.Report.CheckKey, r.Asker, want)
	}
}

// A RERUN ASKS FOR THE ATTEMPT AFTER THE ONE READ: a seat naming a check and
// the attempt it read as its latest sends that check's next attempt, as
// itself; all_failed sends only what a seat may rerun — an attempt its
// infrastructure ended, never a failure; and a rerun the writer refuses as a
// measurement a seat may not rerun is answered forbidden.
//
// Mutation: rerun at the attempt read (no +1), or let a seat's all_failed
// name a failure → red.
func TestARerunAsksForTheAttemptAfterTheOneRead(t *testing.T) {
	t.Parallel()
	host, writer := newCodeHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))

	got := callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeRerunChecksTool, map[string]any{
		"repo": "site", "sha": mainOID, "checks": []any{map[string]any{"name": "unit", "attempt": 1}}})
	sent := writer.checksAsked()
	if got.Failed || len(sent) != 1 || sent[0].Rerun == nil {
		t.Fatalf("a seat's rerun answered %q, sending %+v", got.Output, sent)
	}
	want := code.CheckKey{RepoID: siteID, OID: mainOID, Target: "refs/heads/main", Name: "unit", Attempt: 2}
	if sent[0].Rerun.CheckKey != want || sent[0].Asker.Kind != code.PrincipalAgent ||
		sent[0].Asker.Seat != "agent-cto" {
		t.Errorf("a seat's rerun asked for %+v as %+v, want %+v as agent-cto", sent[0].Rerun.CheckKey,
			sent[0].Asker, want)
	}

	host.checks = append(host.checks, failedCheck("", "", "boom", "lab.go", 0))
	host.checks[1].Name, host.checks[1].Log = "lint", nil
	got = callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeRerunChecksTool, map[string]any{
		"repo": "site", "sha": mainOID, "all_failed": true})
	sent = writer.checksAsked()[1:]
	if got.Failed || len(sent) != 1 || sent[0].Rerun.Name != "unit" {
		t.Errorf("a seat's all_failed answered %q, sending %d reruns, want unit's alone", got.Output, len(sent))
	}

	writer.check = func(code.ChecksRequest) (code.ChecksResult, error) {
		return code.ChecksResult{Refusal: code.CheckRefusalRerunGenuine, Detail: "a measurement stands"}, nil
	}
	got = callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeRerunChecksTool, map[string]any{
		"repo": "site", "sha": mainOID, "checks": []any{map[string]any{"name": "lint", "attempt": 1}}})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
		!strings.Contains(got.Output, string(code.CheckRefusalRerunGenuine)) {
		t.Errorf("a refused rerun answered %q as %s, want forbidden naming rerun_genuine", got.Output,
			tools.RefusalOf(got))
	}
}

// A CHECK WRITE IS THE OPERATION ITS OP_ID NAMES: on the operator's surface,
// code_rerun_checks and code_report_check each answer the operation they were
// made as, to bring back, and refuse — writing nothing — an `op_id` answered
// for another call, as every write that offers one does.
//
// Mutation: leave a check write's operation unbound → red.
func TestACheckWriteIsTheOperationItsOpIDNames(t *testing.T) {
	t.Parallel()
	rerun := func(attempt int) map[string]any {
		return map[string]any{"repo": "site", "sha": mainOID,
			"checks": []any{map[string]any{"name": "unit", "attempt": attempt}}}
	}
	report := func(status string) map[string]any {
		return map[string]any{"repo": "site", "sha": mainOID, "name": "lint", "status": status}
	}
	for _, c := range []struct {
		tool       string
		one, other map[string]any
	}{
		{builtin.CodeRerunChecksTool, rerun(1), rerun(2)},
		{builtin.CodeReportCheckTool, report("running"), report("passed")},
	} {
		t.Run(c.tool, func(t *testing.T) {
			t.Parallel()
			host, writer := newCodeHost(), &codeWrites{}
			deps := writeDeps(host, writer)
			deps.Actor = builtin.PrincipalActor
			tool := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
				c.tool)
			got, err := tool.CallForTurn(asBoundPerson(), nil, c.one)
			if err != nil || got.Failed {
				t.Fatalf("%s answered %v %q", c.tool, err, got.Output)
			}
			var answer struct {
				OpID string `json:"op_id"`
			}
			if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
				t.Fatal(err)
			}
			if answer.OpID == "" {
				t.Errorf("%s answered no op_id to bring back: %s", c.tool, got.Output)
			}
			sent := len(writer.checksAsked())

			c.other["op_id"] = answer.OpID
			got, err = tool.CallForTurn(asBoundPerson(), nil, c.other)
			if err != nil || !got.Failed || len(writer.checksAsked()) != sent {
				t.Errorf("%s brought an op_id answered for another call answered %v %q, writing %d more", c.tool,
					err, got.Output, len(writer.checksAsked())-sent)
			}
		})
	}
}
