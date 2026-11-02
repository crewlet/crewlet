package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// purgePlacement is the fleet's placement as a purge's answer reads it: git-2
// maintains every repository, and git-2 and git-1 serve git.
type purgePlacement struct{}

func (purgePlacement) Maintainer(string, int64) (string, error) { return "git-2", nil }
func (purgePlacement) GitNodes(string) ([]string, error)        { return []string{"git-2", "git-1"}, nil }

// asProvenPerson is asBoundPerson inside their step-up window: a proof of who
// they are recent enough for a gesture that asks a fresh one.
func asProvenPerson() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee3"),
		Kind: iam.KindPerson, Login: "jane.founder", Seat: "founder", Via: "session:jane",
		Stage: iam.StageActive, Grants: iam.Apps, ReauthAt: time.Now().Add(time.Hour).UTC(),
	})
}

// purgeTool is the operator catalogue's purge over host and writer, as a
// person's act transport and assistant reach it.
func purgeTool(t *testing.T, host *codeHost, writer *codeWrites) tools.SeatCallable {
	t.Helper()
	deps := writeDeps(host, writer)
	deps.Actor, deps.Placement = builtin.PrincipalActor, purgePlacement{}
	return operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodePurgeUnreachableTool)
}

// ONLY A PERSON PURGES, with a fresh proof of who they are: the authority
// table's row is human-only and asks a step-up, so a seat — whatever it holds
// — is refused by the table before anything is read; a token bound to nobody
// is refused as every code write is (code.UnboundDetail), and a person whose
// proof is older than their step-up window is told to prove it again; nothing
// is sent for any of them. A person bound to a seat, proved, records one
// request of the repository, as themselves, and is answered with it: where
// its record landed, the git node that serves it, and what still holds the
// objects after it is served.
//
// Mutation: drop humanOnly from code_purge_unreachable's row, or let the tool
// send a request for a token bound to nobody → red.
func TestOnlyAPersonPurges(t *testing.T) {
	t.Parallel()
	if human, ok := authz.HumanOnlyOf(authz.ActionCodePurgeUnreachable); !ok || !human {
		t.Fatalf("code_purge_unreachable's row is human-only %v (known %v), want a person's alone", human, ok)
	}
	if r, ok := authz.RecencyOf(authz.ActionCodePurgeUnreachable); !ok || r != iam.RecencyStepUp {
		t.Fatalf("code_purge_unreachable's row asks recency %q (known %v), want a step-up", r, ok)
	}
	args := func() map[string]any { return map[string]any{"repo": "site"} }

	host, writer := newCodeHost(), &codeWrites{}
	deps := writeDeps(host, writer)
	deps.Placement = purgePlacement{}
	got := callWrite(t, everyGrant(), codeRegistry(t, deps), writerTurn(t), builtin.CodePurgeUnreachableTool,
		args())
	var refused *builtin.DecisionRefused
	if !got.Failed || !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonSeatRefused {
		t.Errorf("a seat's purge answered %q (cause %v), want the table's refusal of a seat", got.Output, got.Cause)
	}

	tool := purgeTool(t, host, writer)
	got, err := tool.CallForTurn(asUnboundToken(), nil, args())
	if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
		!strings.Contains(got.Output, code.UnboundDetail) {
		t.Errorf("a token bound to nobody's purge answered %v %q", err, got.Output)
	}
	got, err = tool.CallForTurn(asBoundPerson(), nil, args())
	if err != nil || !got.Failed || !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonStepUp {
		t.Errorf("a purge on a proof past its step-up window answered %v %q (cause %v), want a step-up", err,
			got.Output, got.Cause)
	}
	if len(writer.expunges) != 0 {
		t.Fatalf("the refused purges sent %d requests", len(writer.expunges))
	}

	got, err = tool.CallForTurn(asProvenPerson(), nil, args())
	if err != nil || got.Failed {
		t.Fatalf("a person's purge answered %v %q", err, got.Output)
	}
	if len(writer.expunges) != 1 || writer.expunges[0].Request == nil {
		t.Fatalf("a person's purge sent %+v, want one request", writer.expunges)
	}
	r := writer.expunges[0].Request
	if r.RepoID != siteID || r.By.Kind != code.PrincipalOperator || r.By.Seat != "founder" ||
		statelog.CheckCallerOpID(r.RequestID) != nil {
		t.Errorf("a person's purge sent %+v, want site's, by founder, under an operation id", r)
	}
	var answer struct {
		Request    string `json:"request"`
		Position   string `json:"position"`
		Maintainer string `json:"maintainer"`
		Holds      struct {
			GitNodes      []string `json:"git_nodes"`
			BackupsBefore string   `json:"backups_before"`
			StoreUntil    string   `json:"store_until"`
		} `json:"holds"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer %q: %v", got.Output, err)
	}
	if answer.Request != r.RequestID || answer.Position == "" || answer.Maintainer != "git-2" ||
		strings.Join(answer.Holds.GitNodes, ",") != "git-2,git-1" ||
		!strings.Contains(answer.Holds.BackupsBefore, "every backup taken before the purge commits") ||
		!strings.Contains(answer.Holds.BackupsBefore, "nats object store every backup taken before the store deletes them") ||
		!strings.Contains(answer.Holds.StoreUntil, "24 hours") ||
		!strings.Contains(answer.Holds.StoreUntil, "versioned S3 bucket") {
		t.Errorf("a person's purge answered %+v, want its request, its position, git-2 serving it, and what "+
			"still holds the objects", answer)
	}
}

// TWO PURGE REQUESTS ARE ONE: the request's id is the call's operation, so
// the same call made again — a person's assistant bringing back the op_id an
// earlier answer carried, after an `unknown` or a lost answer — sends the
// same request, which the committer's ledger collapses and answers as the
// first: one request, at one position.
//
// Mutation: mint the request's id afresh at every call (statelog.NewOpID in
// place of opIDFor) → red.
func TestTwoPurgeRequestsAreOne(t *testing.T) {
	t.Parallel()
	host, writer := newCodeHost(), &codeWrites{}
	tool := purgeTool(t, host, writer)
	answerOf := func(got tools.Result) (request, position, op string) {
		var a struct {
			Request  string `json:"request"`
			Position string `json:"position"`
			OpID     string `json:"op_id"`
		}
		if got.Failed {
			t.Fatalf("a purge answered %q", got.Output)
		}
		if err := json.Unmarshal([]byte(got.Output), &a); err != nil {
			t.Fatalf("the answer %q: %v", got.Output, err)
		}
		return a.Request, a.Position, a.OpID
	}
	first, err := tool.CallForTurn(asProvenPerson(), nil, map[string]any{"repo": "site"})
	if err != nil {
		t.Fatal(err)
	}
	request, position, op := answerOf(first)
	if op == "" {
		t.Fatalf("the first purge answered no op_id to ask again with: %q", first.Output)
	}
	again, err := tool.CallForTurn(asProvenPerson(), nil, map[string]any{"repo": "site", "op_id": op})
	if err != nil {
		t.Fatal(err)
	}
	request2, position2, _ := answerOf(again)
	if len(writer.expunges) != 2 ||
		writer.expunges[0].Request.RequestID != writer.expunges[1].Request.RequestID {
		t.Fatalf("the same purge asked twice sent %+v, want one request id twice", writer.expunges)
	}
	if request2 != request || position2 != position {
		t.Errorf("the same purge asked again answered %s at %s, want the first's %s at %s", request2, position2,
			request, position)
	}
}
