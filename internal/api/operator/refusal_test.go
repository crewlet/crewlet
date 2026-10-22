package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/modelnote"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// diskFault is a store's error as a driver words one: a database path, which
// is exactly what must reach the node's log and never a caller.
const diskFault = "open /var/lib/crewlet/replicated.db: disk I/O error"

// faultyReader is the stub tracker reader with its item read broken by err.
type faultyReader struct {
	stubWorkReader
	err error
}

func (f faultyReader) Task(context.Context, string, worklet.DetailWants,
	statelog.Freshness) (worklet.TaskDetail, error) {

	return worklet.TaskDetail{}, f.err
}

// faultySurface is the act route over a tracker whose item reads fail with err.
func faultySurface(t *testing.T, err error) http.Handler {
	t.Helper()
	return guarded(newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{Reader: faultyReader{err: err},
				Writer: (&recordingWork{}).writer},
		}),
		Org: company,
	}))
}

// A FAULT OF THE NODE IS `500 internal_error`, NOT A 503. A store this node
// cannot read is not cleared by waiting, so the `503` with a two-second
// Retry-After it used to answer had a client polling a broken node for ever —
// and the sentence it carried was the store's own error, a database path, in
// the client's `detail`. The control beside it is the state log's refusal,
// which keeps its `503` and the hint the refusal derived from this node's
// own backlog.
//
// Mutation: map the fault class to 503 in classAnswers, or let readFailure
// class an unmarked error `unavailable`, and the first half goes red; class a
// state-log refusal as a fault and the second does.
func TestAStoreFaultIsAFiveHundredWithNoRetryAfter(t *testing.T) {
	t.Parallel()
	body := `{"args":{"item":"ENG-1","status":"done"}}`

	status, answer, header := actAnswer(t, faultySurface(t, errors.New(diskFault)),
		worklet.UpdateWorkItemTool, body)
	if status != http.StatusInternalServerError ||
		answer["error"] != string(crewletmcp.RefusalInternalError) {
		t.Fatalf("a store fault answered %d %v, want 500 internal_error", status, answer)
	}
	if after := header.Get("Retry-After"); after != "" {
		t.Errorf("a fault told the client to come back in %s seconds", after)
	}
	detail, _ := answer["detail"].(string)
	if strings.Contains(detail, "/var/lib") || strings.Contains(fmt.Sprint(answer), "/var/lib") {
		t.Errorf("the store's own error reached the client: %v", answer)
	}
	message, _ := answer["message"].(string)
	if detail != "" || !strings.Contains(message, "log") ||
		answer["tool"] != worklet.UpdateWorkItemTool {

		t.Errorf("the fault's answer %v does not say, in its code's own words, "+
			"where its reason is, or for which tool", answer)
	}

	behind := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession,
		RetryAfter: 9 * time.Second, Detail: "this node is 40 records behind"}
	status, answer, header = actAnswer(t, faultySurface(t, behind),
		worklet.UpdateWorkItemTool, body)
	if status != http.StatusServiceUnavailable ||
		answer["error"] != string(crewletmcp.RefusalUnavailable) {
		t.Fatalf("a read the state log refused answered %d %v, want 503 unavailable",
			status, answer)
	}
	if after := header.Get("Retry-After"); after != "9" {
		t.Errorf("Retry-After = %q, want the refusal's own 9 seconds", after)
	}
	// IN ITS CODE'S OWN WORDS, and the refusal's own text nowhere: the
	// tool's sentence for it carried the refusal's message, framework,
	// detail and all, and nobody has worded the condition for a person yet
	// (ADR-0060), so the answer carries no detail and no tool_detail.
	said, has := answer["detail"]
	if has || answer["message"] != httpjson.CodeUnavailable.Message() ||
		answer["tool_detail"] != nil || strings.Contains(fmt.Sprint(answer), "40 records") ||
		strings.Contains(fmt.Sprint(answer), "statelog") {

		t.Errorf("a refusal the state log made answered %v (detail %v), want its "+
			"code's own message and nothing of its own text", answer, said)
	}
}

// A WRITER'S ERROR IS NEVER THE DETAIL, on the surfaces that call a writer
// without a tool. The human write surface handed [operator.Fail] the error's
// own text whatever it was, so a disk error's path was the `detail`, and a
// domain refusal's sentence (written for a model) after it. [operator.FailWrite]
// answers each in its code's own sentence, a condition with its hint, and only
// a refusal the writer worded for a person with a detail: those words
// (ADR-0060).
func TestAWritersErrorIsWordedForWhoeverReadsIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   httpjson.Code
		says   string
		retry  bool
	}{
		{"a fault of the node", fmt.Errorf("worklet: read the ledger: %s", diskFault),
			http.StatusInternalServerError, httpjson.CodeInternalError, "", false},
		{"a coordination store that did not answer",
			fmt.Errorf("dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable),
			http.StatusServiceUnavailable, httpjson.CodeUnavailable, "", true},
		{"a refusal the domain composed for a model",
			fmt.Errorf("%w: the `body` is longer than a comment may be", worklet.ErrInvalid),
			http.StatusUnprocessableEntity, httpjson.CodeInvalid, "", false},
		{"a refusal the domain worded for a person",
			caveat.Saying(fmt.Errorf("%w: the `body` is longer than a comment may be",
				worklet.ErrInvalid), caveat.CommentTooLong()),
			http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			caveat.CommentTooLong().Person(), false},
		// THE ESTATE'S ROUTER FINDING NO DATA NODE TO RUN IT ON, which a
		// write through this surface meets whenever every copy is out of
		// service: a condition of the fleet, never a fault of this node,
		// and its detail names the nodes it asked.
		{"an estate nobody served",
			&estate.ErrUnserved{Detail: "10.0.0.7: out_of_service: applier halted"},
			http.StatusServiceUnavailable, httpjson.CodeUnavailable, "", true},
		// A STATE-LOG REFUSAL, in its code's own words and never its own
		// text, which names the framework and its detail.
		{"a log at its ceiling", fmt.Errorf("worklet: %w", &statelog.Unavailable{
			Reason: statelog.ReasonLogFull, Detail: "raise stream.tracker_max_bytes on 10.0.0.7"}),
			http.StatusServiceUnavailable, httpjson.CodeUnavailable, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			operator.FailWrite(t.Context(), rec, tc.err, newKey(), nil)
			body := decode(t, rec)
			if rec.Code != tc.status || body["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", rec.Code, body, tc.status, tc.code)
			}
			if detail, _ := body["detail"].(string); detail != tc.says {
				t.Errorf("detail = %q, want %q", detail, tc.says)
			}
			if body["message"] != tc.code.Message() {
				t.Errorf("message = %v, want the code's own sentence", body["message"])
			}
			for _, never := range []string{"/var/lib", "10.0.0.7", "`body`", "statelog",
				"tracker_max"} {

				if strings.Contains(fmt.Sprint(body), never) {
					t.Errorf("the answer %v carries the error's own %q", body, never)
				}
			}
			after := rec.Header().Get("Retry-After")
			if seconds, err := strconv.Atoi(after); tc.retry != (err == nil && seconds > 0) {
				t.Errorf("Retry-After = %q, want one only for a condition", after)
			}
		})
	}
}

// A REFUSAL A DOMAIN WROTE IS TOLD IN ITS CODE'S OWN WORDS on a verb with no
// tool, and never in its error's. The human write surface's purge, rename and
// destructions answered the error's own text, so a person read "chatlet:
// invalid: reason: …" — the package the error came from, before what to
// change. Without that prefix it is the domain's own sentence
// ([builtin.WriterSentence]), the one its tools word the same refusal with,
// and that is written for a model: nobody has worded these refusals for a
// person yet, so each is answered in the class its domain decided with no
// detail, and its code's own message stands alone (ADR-0060), as the same
// refusal through a tool is.
//
// Mutation: answer err.Error(), or the domain's sentence, as a classified
// refusal's detail again and every row goes red; word chat's refusal with
// another domain's sentence function and its row does.
func TestADomainsRefusalIsToldInItsCodesOwnWords(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		code   httpjson.Code
		model  string
	}{
		"the tracker's": {&worklet.Refusal{Mark: worklet.ErrForbidden,
			Err: errors.New("only its author may rewrite a remark")},
			http.StatusForbidden, httpjson.CodeForbidden,
			"only its author may rewrite a remark"},
		"the knowledge base's": {&doclet.InvalidError{Field: "title",
			Why: "a title is at most 200 bytes"},
			http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			"`title`: a title is at most 200 bytes"},
		"chat's": {&chatlet.InvalidError{Field: "reason",
			Why: "an erase's reason is 700 bytes against a 600 cap"},
			http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			"`reason`: an erase's reason is 700 bytes against a 600 cap"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// THE MODEL'S WORDS ARE THE DOMAIN'S OWN, so what this row holds
			// the answer clear of is what a model is really told.
			if said, ok := builtin.WriterSentence(tc.err); !ok || said != tc.model {
				t.Errorf("the domain words it %q for a model, want %q", said, tc.model)
			}
			rec := httptest.NewRecorder()
			operator.FailWrite(t.Context(), rec, tc.err, newKey(), nil)
			body := decode(t, rec)
			if rec.Code != tc.status || body["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", rec.Code, body, tc.status, tc.code)
			}
			if detail, has := body["detail"]; has || body["message"] != tc.code.Message() {
				t.Errorf("answered %v with detail %v, want its code's own message alone",
					body, detail)
			}
			// CASE-BLIND, since a sentence that opens with the clause
			// capitalizes it.
			answer := strings.ToLower(fmt.Sprint(body))
			for _, never := range []string{tc.model, tc.err.Error(), "worklet", "doclet",
				"chatlet"} {

				if strings.Contains(answer, strings.ToLower(never)) {
					t.Errorf("the answer %v carries the error's own %q", body, never)
				}
			}
		})
	}
}

// A STATE OR A COLLISION A DOMAIN DECIDED IS TOLD TO A PERSON IN ITS CODE'S
// OWN WORDS, and the tool's own sentence to the model alone, in process.
//
// The tool's sentence for a refusal is a model's — "post_message refused that:
// … say it somewhere that is still open, or say nothing", "write_project on ENG
// with tags_add: …" — and so is the domain's sentence it carries: the room
// archived, the task in the trash, the label another tag holds. A person is
// shown neither (ADR-0060), and nobody has worded these refusals for a person
// yet, so each is answered in its class — a state, a race and a collision a
// `409` alike — with no detail and its code's own message standing alone, and
// never with the tool's sentence beside it as a `tool_detail`. That sentence
// reaches the one reader it is worded for through [operator.FailTool], which
// tells it to the in-process model that made the call (internal/modelnote).
//
// Mutation: answer result.Output as the detail, or drop FailTool's
// modelnote.Tell, and every row goes red; answer the domain's sentence and
// every row a domain decided does.
func TestAStateOrACollisionIsToldToAPersonInItsCodesOwnWords(t *testing.T) {
	t.Parallel()
	model := "post_message refused that: the model's sentence, naming the tool to call next."
	for name, tc := range map[string]struct {
		cause  error
		status int
		code   httpjson.Code
		words  string
	}{
		"an archived room": {crewletmcp.Classify(crewletmcp.RefusalConflict,
			fmt.Errorf("%w: this room is archived, so it takes no new messages. "+
				"Only whoever created it, the lead of its team or of a team above it, "+
				"or an admin can reopen it", chatlet.ErrArchived)), http.StatusConflict,
			httpjson.CodeConflict, "reopen it"},
		"a task in the trash": {crewletmcp.Classify(crewletmcp.RefusalConflict,
			&worklet.Refusal{Mark: worklet.ErrState,
				Err: errors.New("this task is in the trash, put there by ana. Restore it first")}),
			http.StatusConflict, httpjson.CodeConflict, "put there by ana"},
		"a lost race": {crewletmcp.Classify(crewletmcp.RefusalConflict,
			fmt.Errorf("%w: crewlet.tracker.task.x kept changing", statelog.ErrConflict)),
			http.StatusConflict, httpjson.CodeConflict, "kept changing"},
		"a label another tag carries": {crewletmcp.Classify(crewletmcp.RefusalExists,
			&worklet.TagClash{Project: "ENG", Slug: "api", Label: "API",
				Other: worklet.Tag{Slug: "backend-api", Label: "API"}}), http.StatusConflict,
			httpjson.CodeExists, "backend-api"},
		"an argument refused": {crewletmcp.Classify(crewletmcp.RefusalInvalid,
			&chatlet.InvalidError{Field: "body", Why: "a message is at most 32768 bytes"}),
			http.StatusUnprocessableEntity, httpjson.CodeInvalid, "32768 bytes"},
		"a turn's note box": {crewletmcp.RefusalConflict, http.StatusConflict,
			httpjson.CodeConflict, "naming the tool"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, heard := modelnote.Expect(t.Context())
			r := httptest.NewRequestWithContext(ctx, http.MethodPost,
				operator.ActPathPrefix+"post_message", nil)
			rec := httptest.NewRecorder()
			operator.FailTool(rec, r, crewletmcp.Result{Failed: true, Output: model,
				Cause: tc.cause}, newKey(), httpjson.Detail{"tool": "post_message"})
			body := decode(t, rec)
			if rec.Code != tc.status || body["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", rec.Code, body, tc.status, tc.code)
			}
			if detail, has := body["detail"]; has || body["message"] != tc.code.Message() {
				t.Errorf("answered %v with detail %v, want its code's own message alone",
					body, detail)
			}
			if said, has := body["tool_detail"]; has {
				t.Errorf("the answer carries the tool's sentence as tool_detail: %v", said)
			}
			if body["tool"] != "post_message" {
				t.Errorf("the answer lost what it was about: %v", body)
			}
			for key, value := range body {
				for _, never := range []string{"refused that", tc.words} {
					if key != "tool" && strings.Contains(fmt.Sprint(value), never) {
						t.Errorf("%s carries the model's own %q: %v", key, never, value)
					}
				}
			}
			if heard.Said() != model {
				t.Errorf("the model heard %q, want the tool's own sentence", heard.Said())
			}
		})
	}
}

// A DELETION AND A REUSED KEY ARE TOLD WITHOUT THE FRAMEWORK'S WORDS, through
// a tool and around one alike.
//
// Both are the state log's refusals, and its error names the subject each met
// and the operation each was about: "statelog: unavailable (deleted):
// crewlet.tracker.task.t-1 was deleted and stays deleted". A tool-less write
// handed that to a person as its `detail`, and a tool-backed one handed them
// the tool's restatement of it for a model; a reused key's answer opened with
// either before saying which header to change. Nobody has worded a deletion
// for a person yet, so it answers `404 not_found` with no detail, in its
// code's own message (ADR-0060); a reused key is told in the one sentence
// every surface taking a key answers it with ([opkey.Reused]), alone.
//
// Mutation: answer a deletion's `detail` with the writer's error or the tool's
// sentence again, or put the sentence back in front of the reused key's
// words, and a row goes red.
func TestADeletionAndAReusedKeyAreToldWithoutTheFrameworksWords(t *testing.T) {
	t.Parallel()
	deleted := &statelog.Unavailable{Reason: statelog.ReasonDeleted,
		Detail: "crewlet.tracker.task.t-1 was deleted and stays deleted"}
	reused := &statelog.Unavailable{Reason: statelog.ReasonOpReused,
		Detail: `operation "op" landed on crewlet.tracker.task.t-2 at 3:4, and ` +
			`this write is to crewlet.tracker.task.t-1`}
	model := func(err error) string {
		return "update_work_item did not land (" + err.Error() + ")."
	}
	tool := func(w http.ResponseWriter, err error) {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			operator.ActPathPrefix+"update_work_item", nil)
		operator.FailTool(w, r, crewletmcp.Result{Failed: true, Output: model(err),
			Cause: crewletmcp.Classify(crewletmcp.RefusalUnavailable, err)},
			newKey(), httpjson.Detail{"tool": "update_work_item"})
	}
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		status int
		code   httpjson.Code
		says   string
	}{
		{"a deletion, around a tool", func(w http.ResponseWriter) {
			operator.FailWrite(t.Context(), w, deleted, newKey(), nil)
		}, http.StatusNotFound, httpjson.CodeNotFound, ""},
		{"a deletion, through a tool", func(w http.ResponseWriter) {
			tool(w, deleted)
		}, http.StatusNotFound, httpjson.CodeNotFound, ""},
		{"a reused key, around a tool", func(w http.ResponseWriter) {
			operator.FailWrite(t.Context(), w, reused, newKey(), nil)
		}, http.StatusConflict, httpjson.CodeInvalidInput, opkey.Reused},
		{"a reused key, through a tool", func(w http.ResponseWriter) {
			tool(w, reused)
		}, http.StatusConflict, httpjson.CodeInvalidInput, opkey.Reused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			tc.answer(rec)
			body := decode(t, rec)
			if rec.Code != tc.status || body["error"] != string(tc.code) ||
				body["message"] != tc.code.Message() {

				t.Errorf("answered %d %v, want %d %s in its code's own message", rec.Code,
					body, tc.status, tc.code)
			}
			if detail, _ := body["detail"].(string); detail != tc.says {
				t.Errorf("detail = %q, want %q", detail, tc.says)
			}
			for key, value := range body {
				for _, leak := range []string{"statelog", "crewlet.", "update_work_item"} {
					if key != "tool" && strings.Contains(fmt.Sprint(value), leak) {
						t.Errorf("%s, which a person reads, carries %q: %v", key, leak, value)
					}
				}
			}
		})
	}
}

// THE CLASS OF A WRITER'S ERROR IS THE TOOLS' OWN RULE: a content refusal the
// domain marked keeps its class, a condition waiting clears is `unavailable` —
// an estate the router found nobody to serve among them — and anything
// unmarked is a fault. And a write that MAY HAVE LANDED is an unknown, by the
// tools' own rule for that ([builtin.UnknownCause]): a walk that stopped at a
// step nobody can confirm, and a write the router sent to a node that never
// answered — `unavailable` by class, and never read as a refusal that wrote
// nothing, which is what the audit recorded the first as and the surface
// answered the second as a fault.
//
// Mutation: drop the UnknownCause step from ClassifyWrite and the two
// unknowns go red; drop the ErrUnserved arm from builtin.Condition and the
// unserved estate does; class by a list of this package's own instead of
// builtin.WriterClass and the domain rows do.
func TestAWritersErrorIsClassedByTheToolsRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err     error
		want    crewletmcp.Refusal
		unknown bool
	}{
		{err: worklet.ErrNoTask, want: crewletmcp.RefusalNotFound},
		// EVERY CLASS A DOMAIN DECIDES, by the tools' own table
		// (builtin.WriterClass) rather than a shorter list here: each of
		// these was a 500 on the surface while its tool classed it.
		{err: fmt.Errorf("x: %w", worklet.ErrInboxFull), want: crewletmcp.RefusalInboxFull},
		{err: fmt.Errorf("x: %w", worklet.ErrAlreadyAnswered),
			want: crewletmcp.RefusalAlreadyAnswered},
		{err: fmt.Errorf("x: %w", worklet.ErrReassignmentBudget),
			want: crewletmcp.RefusalReassignmentBudget},
		{err: fmt.Errorf("x: %w", &worklet.TagClash{Project: "OPS", Slug: "api"}),
			want: crewletmcp.RefusalExists},
		{err: &statelog.Unavailable{Reason: statelog.ReasonLogFull},
			want: crewletmcp.RefusalUnavailable},
		{err: fmt.Errorf("x: %w", coord.ErrUnavailable), want: crewletmcp.RefusalUnavailable},
		{err: &estate.ErrUnserved{Detail: "data-b: out_of_service"},
			want: crewletmcp.RefusalUnavailable},
		{err: fmt.Errorf("stopped: %w", worklet.ErrStepUnresolved),
			want: crewletmcp.RefusalUnavailable, unknown: true},
		{err: fmt.Errorf("%w (pages.trash, asked of data-b)", estate.ErrOutcomeUnknown),
			want: crewletmcp.RefusalUnavailable, unknown: true},
		{err: errors.New(diskFault), want: crewletmcp.RefusalInternalError},
	} {
		result := crewletmcp.Result{Failed: true, Cause: operator.ClassifyWrite(tc.err)}
		if got := crewletmcp.RefusalOf(result); got != tc.want {
			t.Errorf("ClassifyWrite(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if got := crewletmcp.UnknownOf(result); got != tc.unknown {
			t.Errorf("ClassifyWrite(%v) is an unknown outcome: %v, want %v", tc.err,
				got, tc.unknown)
		}
		if !errors.Is(result.Cause, tc.err) {
			t.Errorf("ClassifyWrite(%v) lost the writer's error beneath it", tc.err)
		}
	}
}

// A WRITE THE ROUTER SENT AND NEVER HEARD BACK ABOUT IS ANSWERED AS AN
// UNKNOWN: `503` with `outcome: "unknown"` under the request's own key, so the
// client reads back before sending it again — never the `500` it was, which
// told a person sending it again could not help and named no key. Its words
// are fixed: the router's name the node it asked.
//
// Mutation: drop the UnknownCause step from ClassifyWrite and this answers
// 500; answer the writer's error as the detail and the node's name reaches the
// answer.
func TestAWriteNobodyAnsweredForIsAnsweredAsAnUnknown(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	key := newKey()
	operator.FailWrite(t.Context(), rec,
		fmt.Errorf("%w (pages.trash, asked of 10.0.0.7)", estate.ErrOutcomeUnknown),
		key, nil)
	body := decode(t, rec)
	if rec.Code != http.StatusServiceUnavailable || body["outcome"] != "unknown" ||
		body["op_id"] != key {
		t.Fatalf("answered %d %v, want 503 with outcome unknown under the key", rec.Code, body)
	}
	if said, has := body["tool_detail"]; has {
		t.Errorf("an unknown answered a sentence of the router's: %v", said)
	}
	if strings.Contains(fmt.Sprint(body), "10.0.0.7") {
		t.Errorf("the node the router asked reached the answer: %v", body)
	}
}

// decode is a recorded answer's JSON body.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("answered %d with a body that is not JSON: %q", rec.Code, rec.Body)
	}
	return body
}

// actAnswer is [act] as the founder, under a fresh key, with the headers.
func actAnswer(t *testing.T, h http.Handler, tool, body string) (int, map[string]any,
	http.Header) {

	t.Helper()
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+tool, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer founder")
	r.Header.Set(opkey.Header, newKey())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, decode(t, rec), rec.Header()
}

// AN OBJECT NAMED BY A LIST IS NAMED BY THE LIST. A direct conversation is its
// participants, and `send_dm` and `open_dm` address it by them alone, so a
// refusal's detail names who it was with — the handles as the call gave them,
// its empty and non-text entries dropped — beside the text arguments every
// other tool names its object by. Read as text only, a failed DM named
// nothing at all.
//
// Mutation: drop About's list arm and the participants go missing; keep an
// entry that is blank and the first case does.
func TestAnObjectNamedByAListIsNamedInTheRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool string
		args map[string]any
		want httpjson.Detail
	}{
		{"a conversation, by its participants", "send_dm",
			map[string]any{"to": []any{"bob", " ", 7, "ana"}, "body": "hi"},
			httpjson.Detail{"tool": "send_dm", "to": []string{"bob", "ana"}}},
		{"a list naming nobody names nothing", "open_dm",
			map[string]any{"to": []any{"", "  "}},
			httpjson.Detail{"tool": "open_dm"}},
		{"a room, by its text", "post_message",
			map[string]any{"channel": "#launch", "body": "hi"},
			httpjson.Detail{"tool": "post_message", "channel": "#launch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := operator.About(tc.tool, tc.args)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("About(%s, %v) = %v, want %v", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}

// A REFUSAL A SURFACE DECIDED IS ANSWERED IN THE AUTHORITY TABLE'S OWN CODE.
//
// [operator.RefuseDecision] answers a decision the human write surface took on
// a row it read, and it mapped a refusal to its code with a copy of the
// table's mapping that knew a stale proof and nothing else: a machine token on
// a verb that needs a person present was answered `unauthorized`, whose
// sentence sends the caller to ask for a permission no token could carry,
// where the router answers the same refusal `token_refused`. Each refusal is
// held to the code [authz.RefusalFields] gives it. Mutation: map the refusals
// here by a table of this package's own and the token's case fails.
func TestARefusalASurfaceDecidedIsAnsweredInTheTablesCode(t *testing.T) {
	t.Parallel()
	for _, d := range []authz.Decision{
		{Reason: authz.ReasonTokenRefused},
		{Reason: authz.ReasonStepUp, Recency: iam.RecencyStepUp,
			Grants: []iam.Grant{iam.GrantAdmin}},
		{Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantAdmin}},
	} {
		t.Run(string(d.Reason), func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost, "/worklet/items/ENG-1/purge", nil)
			r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{
				Kind: iam.KindPerson, Login: "jane.doe", Seat: "jane",
				Stage: iam.StageActive,
			}))
			rec := httptest.NewRecorder()
			operator.RefuseDecision(rec, r, authz.ActionWorkPurge, d)
			var answer map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
				t.Fatal(err)
			}
			status, code, _ := authz.RefusalFields(d)
			if rec.Code != status || answer["error"] != string(code) ||
				answer[authz.DetailReason] != string(d.Reason) {
				t.Errorf("answered %d %v, want %d %s", rec.Code, answer, status, code)
			}
		})
	}
}

// AN If-Match HEADER CHANGES ONE ANSWER AND NO OTHER: a stale version the
// header stated is HTTP's failed precondition, `412 version_moved` (RFC 9110
// §13.1.1), and every other class — and every arm decided before the class,
// an outcome nobody can establish among them — is answered byte for byte as
// [operator.Fail] answers it, so a route that folds the header into a tool's
// version cannot answer anything but its staleness differently from the act
// route the dashboard writes through.
//
// Mutation: answer the stale class 409 under the header, or any other class
// differently, and this goes red naming it.
func TestAnIfMatchHeaderRefinesOnlyAStaleVersion(t *testing.T) {
	t.Parallel()
	answer := func(fail func(http.ResponseWriter, error, caveat.Refusal, string, httpjson.Detail),
		cause error) *httptest.ResponseRecorder {

		// ANY PERSON'S WORDS: what is held is that both answers carry them.
		rec := httptest.NewRecorder()
		fail(rec, cause, caveat.NotAnOption(), "the-key",
			httpjson.Detail{"tool": "update_work_item"})
		return rec
	}
	causes := map[string]error{
		"an outcome nobody can establish": crewletmcp.ErrOutcomeUnknown,
		"an unclassified failure":         errors.New("a tool that forgot to classify"),
	}
	for _, class := range crewletmcp.Refusals {
		causes[string(class)] = crewletmcp.Classify(class, errors.New("the reason"))
	}
	for name, cause := range causes {
		plain, matched := answer(operator.Fail, cause), answer(operator.FailIfMatched, cause)
		if name == string(crewletmcp.RefusalStaleVersion) {
			var body, class map[string]any
			if json.Unmarshal(matched.Body.Bytes(), &body) != nil ||
				json.Unmarshal(plain.Body.Bytes(), &class) != nil {
				t.Fatalf("a stale version answered %q and %q", matched.Body, plain.Body)
			}
			if matched.Code != http.StatusPreconditionFailed ||
				body["error"] != string(httpjson.CodeVersionMoved) ||
				body["detail"] != class["detail"] || body["tool"] != "update_work_item" {
				t.Errorf("a stale version under If-Match answered %d %v, want 412 "+
					"version_moved carrying the person's words and what it was about",
					matched.Code, body)
			}
			if plain.Code != http.StatusConflict {
				t.Errorf("a stale version stated as an argument answered %d, want the "+
					"class's 409", plain.Code)
			}
			continue
		}
		if matched.Code != plain.Code || matched.Body.String() != plain.Body.String() ||
			matched.Header().Get("Retry-After") != plain.Header().Get("Retry-After") {
			t.Errorf("%s: under If-Match %d %s, and as an argument %d %s: only a stale "+
				"version may differ", name, matched.Code, matched.Body, plain.Code, plain.Body)
		}
	}
}
