package builtin_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// importTool is the operator catalogue's import over writer, as a person's
// act transport and assistant reach it.
func importTool(t *testing.T, writer *codeWrites) tools.SeatCallable {
	t.Helper()
	deps := writeDeps(newCodeHost(), writer)
	deps.Actor = builtin.PrincipalActor
	return operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodeImportRepoTool)
}

// ONLY A PERSON IMPORTS: the authority table's row is human-only — an import
// has the code host dial another host for the company, which a model's say-so
// would point anywhere (U49 a) — so a seat, whatever it holds, is refused by
// the table before anything is read; a token bound to nobody is refused as
// every code write is (code.UnboundDetail); and a credential that is not a
// variable's NAME — a token pasted where its name belongs — is refused before
// anything is sent. Nothing is sent for any of them. A person bound to a seat
// creates the project, as themselves, with its source on the create: an
// import's URL and the `${NAME}` reference the record carries, or — asked for
// a mirror — the same on its settings' pull mirror and no import beside it;
// and is answered with the repository `importing`.
//
// Mutation: drop humanOnly from code_import_repo's row, or let `mirror` put
// the source on the import → red.
func TestOnlyAPersonImports(t *testing.T) {
	t.Parallel()
	if human, ok := authz.HumanOnlyOf(authz.ActionCodeImportRepo); !ok || !human {
		t.Fatalf("code_import_repo's row is human-only %v (known %v), want a person's alone", human, ok)
	}
	if r, ok := authz.RecencyOf(authz.ActionCodeImportRepo); !ok || r != iam.RecencyAny {
		t.Fatalf("code_import_repo's row asks recency %q (known %v), want none, as a create asks", r, ok)
	}
	const source = "https://git.example.com/acme/web.git"
	args := func(more map[string]any) map[string]any {
		a := map[string]any{"name": "web", "url": source, "credential": "GITHUB_TOKEN",
			"owner": map[string]any{"kind": "unit", "ref": "eng"}}
		for k, v := range more {
			a[k] = v
		}
		return a
	}

	writer := &codeWrites{}
	got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newCodeHost(), writer)), writerTurn(t),
		builtin.CodeImportRepoTool, args(nil))
	var refused *builtin.DecisionRefused
	if !got.Failed || !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonSeatRefused {
		t.Errorf("a seat's import answered %q (cause %v), want the table's refusal of a seat", got.Output, got.Cause)
	}

	tool := importTool(t, writer)
	got, err := tool.CallForTurn(asUnboundToken(), nil, args(nil))
	if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
		!strings.Contains(got.Output, code.UnboundDetail) {
		t.Errorf("a token bound to nobody's import answered %v %q", err, got.Output)
	}
	const pasted = "ghp_0123456789abcdefABCDEF0123456789abcd"
	got, err = tool.CallForTurn(asBoundPerson(), nil, args(map[string]any{"credential": pasted}))
	if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
		strings.Contains(got.Output, pasted) {
		t.Errorf("an import naming a token for its credential answered %v %q, want it refused unquoted", err,
			got.Output)
	}
	if len(writer.lifecycles) != 0 {
		t.Fatalf("the refused imports sent %d requests", len(writer.lifecycles))
	}

	got, err = tool.CallForTurn(asBoundPerson(), nil, args(nil))
	if err != nil || got.Failed {
		t.Fatalf("a person's import answered %v %q", err, got.Output)
	}
	if len(writer.lifecycles) != 1 {
		t.Fatalf("a person's import sent %d requests, want one create", len(writer.lifecycles))
	}
	r := writer.lifecycles[0]
	want := code.ImportSource{URL: source, Credential: "${GITHUB_TOKEN}"}
	if r.Kind != code.LifecycleCreate || r.Name != "web" || r.Actor.Kind != code.PrincipalOperator ||
		r.Actor.Seat != "founder" || r.Import == nil || *r.Import != want || r.Create.MirrorFrom != nil ||
		r.Create.Owner != (code.Owner{Kind: code.OwnerUnit, Ref: "eng"}) ||
		statelog.CheckCallerOpID(r.OpID) != nil {
		t.Errorf("a person's import sent %+v (import %+v), want founder's create of web importing %+v", r,
			r.Import, want)
	}
	var answer struct {
		Repository struct {
			Name   string          `json:"name"`
			Status code.RepoStatus `json:"status"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer %q: %v", got.Output, err)
	}
	if answer.Repository.Name != "web" || answer.Repository.Status != code.RepoStatusImporting {
		t.Errorf("a person's import answered %+v, want web importing", answer.Repository)
	}

	got, err = tool.CallForTurn(asBoundPerson(), nil, args(map[string]any{"name": "web-mirror", "mirror": true}))
	if err != nil || got.Failed || len(writer.lifecycles) != 2 {
		t.Fatalf("a person's mirror answered %v %q", err, got.Output)
	}
	m := writer.lifecycles[1]
	if m.Import != nil || m.Create.MirrorFrom == nil ||
		*m.Create.MirrorFrom != (code.MirrorSource{URL: source, Credential: "${GITHUB_TOKEN}"}) {
		t.Errorf("a person's mirror sent import %+v and mirror_from %+v, want the source on the pull mirror "+
			"alone", m.Import, m.Create.MirrorFrom)
	}
}
