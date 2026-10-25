package operator_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// skillPages serves one page in the tool-skills container and records every
// write it is asked for, so a case can tell a refusal from a write that
// landed.
type skillPages struct {
	mu    sync.Mutex
	wrote []string
}

func (k *skillPages) List(context.Context, doclet.Filter, statelog.Freshness) (doclet.Listing, error) {
	return doclet.Listing{}, nil
}

func (k *skillPages) Get(_ context.Context, ref string, _ statelog.Freshness) (doclet.Detail, error) {
	if ref != "p-skill" {
		return doclet.Detail{}, doclet.ErrNotFound
	}
	return doclet.Detail{Page: doclet.Page{
		ID: "p-skill", Container: "TS", Title: "Chat conventions", Version: 1,
	}}, nil
}

func (k *skillPages) did(what string) (doclet.Written, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.wrote = append(k.wrote, what)
	return doclet.Written{Page: doclet.Page{ID: "p-skill", Container: "TS"},
		Outcome: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (k *skillPages) writes() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.wrote...)
}

func (k *skillPages) Create(_ context.Context, _ doclet.Actor, in doclet.NewPage) (doclet.Written, error) {
	return k.did("create " + in.Container + "/" + in.Title)
}

func (k *skillPages) SavePage(_ context.Context, _ doclet.Actor, id string, _ doclet.Save) (doclet.Written, error) {
	return k.did("save " + id)
}

func (k *skillPages) Rename(_ context.Context, _ doclet.Actor, id, title string, _ bool) (doclet.Written, error) {
	return k.did("rename " + id + " " + title)
}

func (k *skillPages) Comment(_ context.Context, _ doclet.Actor, id string,
	_ doclet.NewComment) (doclet.Comment, doclet.Written, error) {

	written, err := k.did("comment " + id)
	return doclet.Comment{ID: "c-1"}, written, err
}

func (k *skillPages) EditComment(_ context.Context, _ doclet.Actor, id, cid, _ string) (
	doclet.Comment, doclet.Written, error) {

	written, err := k.did("edit " + id + " " + cid)
	return doclet.Comment{ID: cid}, written, err
}

// A TOOL SKILL IS CONFIGURATION, AND THE OPERATOR'S ASSISTANT NEEDS ADMIN.
//
// internal/doclet refuses an AGENT every write into the skills container and
// exempts every person, because whether a person may is a capability and the
// store holds no grants. So over this surface — whose every caller is a person
// or a credential — a token holding only the knowledge base rewrote the
// instructions injected into every seat's turn. A skill page takes admin, and
// an ordinary container is the knowledge base's as ever.
func TestAToolSkillWriteOverTheOperatorSurfaceNeedsAdmin(t *testing.T) {
	t.Parallel()
	kb := &skillPages{}
	dir := &directory{people: map[string]iam.Principal{
		"writer": machine("token:writer", iam.GrantDoclet),
		"admin":  machine("token:admin", iam.GrantAdmin),
	}}
	s := newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{Pages: builtin.PageDeps{
			Reader: kb, Writer: kb,
			SkillsContainer: func() string { return "ts" },
		}}),
		Authorize: builtin.Decide(authz.NoChart{}),
	})
	server := httptest.NewServer(dir.middleware(s.MCPHandler()))
	t.Cleanup(server.Close)

	call := func(who, tool string, args map[string]any) (bool, string) {
		t.Helper()
		sess := connectAs(t, server.URL+operator.MCPPath, who)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", tool, err)
		}
		var out strings.Builder
		for _, c := range res.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				out.WriteString(text.Text)
			}
		}
		return res.IsError, out.String()
	}
	create := map[string]any{"container": "TS", "title": "Deploys", "body": "x"}
	save := map[string]any{"page": "p-skill", "base_version": 1, "body": "y"}

	for name, args := range map[string]struct {
		tool string
		args map[string]any
	}{
		"a new skill":     {builtin.WritePageTool, create},
		"a skill's edits": {builtin.SavePageTool, save},
	} {
		refused, out := call("writer", args.tool, args.args)
		if !refused {
			t.Errorf("%s: the knowledge base alone wrote into the skills container: %s",
				name, out)
		}
		if !strings.Contains(out, string(authz.ActionSkillPageWrite)) {
			t.Errorf("%s: the refusal does not name the verb it needed: %s", name, out)
		}
	}
	if got := kb.writes(); len(got) != 0 {
		t.Fatalf("a refused caller reached the store: %v", got)
	}

	if refused, out := call("admin", builtin.WritePageTool, create); refused {
		t.Errorf("admin could not publish a tool skill: %s", out)
	}
	if refused, out := call("admin", builtin.SavePageTool, save); refused {
		t.Errorf("admin could not change a tool skill: %s", out)
	}

	// AN ORDINARY CONTAINER IS THE KNOWLEDGE BASE'S AS EVER, or the rule
	// would be a ban on writing pages without admin.
	if refused, out := call("writer", builtin.WritePageTool,
		map[string]any{"container": "ENG", "title": "Runbook", "body": "x"}); refused {
		t.Errorf("the knowledge base could not write an ordinary page: %s", out)
	}
	if got, want := kb.writes(), []string{
		"create TS/Deploys", "save p-skill", "create ENG/Runbook",
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the store saw %v, want %v", got, want)
	}
}
