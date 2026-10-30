package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CODE TOOLS ARE OFFERED WHERE THE CODE HOST IS SERVED, through the running
// wiring: a company whose node's runtime serves its repositories holds the
// eleven in every seat's registry — equipped at the apply, after the runtime it
// starts — and the operator's halves read the same deps; a company that hosts
// no code is given none, whose tools would answer every call that no
// repository is served. The terms are [OperatorCode]'s, the code host's REST
// reads' own, and a node with no runtime serves none.
//
// Mutation: CodeToolDeps ignoring OperatorCode's answer (deps over the bare
// router) → the company hosting none holds the tools, red; equip dropping
// deps.Code, or CodeToolDeps leaving out who a turn is acting for or the chart
// that decides them (the requester ceiling) → the company hosting code holds
// none, red.
func TestTheCodeToolsAreOfferedWhereTheCodeHostIsServed(t *testing.T) {
	t.Parallel()
	t.Run("hosting code", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, codeHostCompany))
		if deps := CodeToolDeps(e); deps.Code == nil || deps.CloneURL == nil || deps.Requesters == nil ||
			deps.Chart == nil {
			t.Fatalf("a node serving the code host offers no code tools, or none decided again for whoever "+
				"woke a turn: %+v", deps)
		}
		for _, name := range builtin.CodeToolNames() {
			if _, ok := e.Company().Tools.Lookup(name); !ok {
				t.Errorf("a seat of a company hosting code holds no %s", name)
			}
		}
	})
	t.Run("hosting none", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, nativeCleanupCompany))
		if deps := CodeToolDeps(e); deps.Code != nil {
			t.Error("a node of a company hosting no code offers the code tools")
		}
		for _, name := range builtin.CodeToolNames() {
			if _, ok := e.Company().Tools.Lookup(name); ok {
				t.Errorf("a seat of a company hosting no code holds %s", name)
			}
		}
	})
	if deps := CodeToolDeps(nil); deps.Code != nil {
		t.Error("no engine offered the code tools")
	}
	if deps := CodeToolDeps(&Engine{}); deps.Code != nil {
		t.Error("an engine running no runtime offered the code tools")
	}
}

// A PERSON'S CLONE URL IS THE ONE THE CLI CONFIGURES GIT WITH: the deployment's
// `api.external_url` and the one path a repository is served at — and nothing
// where the deployment names no address, which no answer then invents.
func TestAClonesURLIsTheDeploymentsGitAddress(t *testing.T) {
	t.Parallel()
	e := &Engine{externalBase: "https://crewlet.example.com"}
	if got, want := e.cloneURL("site"), "https://crewlet.example.com/git/site.git"; got != want {
		t.Errorf("clone URL = %q, want %q", got, want)
	}
	if got := (&Engine{}).cloneURL("site"); got != "" {
		t.Errorf("a deployment with no external url handed out the clone URL %q", got)
	}
}

// codeTiersCompany is a company of three agent seats, one at each code tier:
// reader's left unset, which is read_only, reviewer's review and builder's
// full_access.
const codeTiersCompany = `
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: Reader
    handle: reader
    llm: zulu
  - name: Reviewer
    handle: reviewer
    llm: zulu
    code: {access: review}
  - name: Builder
    handle: builder
    llm: zulu
    code: {access: full_access}
`

// unwrittenCode is a code host's writes no case asks: registering the code
// writes needs one, and a hold's recorder with it.
type unwrittenCode struct{ builtin.CodeWriter }

// A SEAT'S MODEL IS SHOWN THE CODE WRITES ONLY WHERE ITS TIER WRITES. Every
// seat's registry holds the thirteen, as it holds every builtin; the runner
// the epoch builds for a turn leaves them off a seat whose code tier caps it
// below write — no repository's grant would let one of its calls through — and
// so does the executor's guidance on changing code, which is gated on holding
// them. Every seat is shown the reads. Read off what the executor sends its
// model, because the decision is the epoch's and the leaving out is the
// runner's, and either half alone could drop it.
//
// Mutation: withheldFrom leaving the code writes to the authority table alone
// → the read_only and review seats are shown all thirteen and told how to
// commit, red; withholding them at every tier → the full_access seat is shown
// none, red.
func TestASeatsModelIsShownTheCodeWritesOnlyWhereItsTierWrites(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, codeTiersCompany))
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	e := &Engine{}
	e.epoch.current.Store(c)
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Authorize: builtin.Decide(orgchart.Of(c.Org)),
		Code: builtin.CodeDeps{Code: unreadCode{}, Writer: unwrittenCode{}, Holder: unwrittenCode{},
			Requesters: turnRequesters{engine: e}, Chart: orgchart.Of(c.Org)},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range builtin.CodeWrites() {
		if _, ok := reg.Lookup(name); !ok {
			t.Fatalf("the premise: the registry holds no %s", name)
		}
	}
	const guidance = "## Changing the company's code"
	for _, seat := range []struct {
		handle string
		writes bool
	}{{"reader", false}, {"reviewer", false}, {"builder", true}} {
		shown, prompt := shownExecutor(t, c, seat.handle, reg)
		for _, name := range builtin.CodeToolNames() {
			if !slices.Contains(shown, name) {
				t.Errorf("%s was not shown the read %s: %v", seat.handle, name, shown)
			}
		}
		for _, name := range builtin.CodeWrites() {
			if slices.Contains(shown, name) != seat.writes {
				t.Errorf("%s (writes %v) was shown %s: %v", seat.handle, seat.writes, name,
					slices.Contains(shown, name))
			}
		}
		if strings.Contains(prompt, guidance) != seat.writes {
			t.Errorf("%s (writes %v) was told how to change the company's code: %v", seat.handle, seat.writes,
				strings.Contains(prompt, guidance))
		}
	}
}
