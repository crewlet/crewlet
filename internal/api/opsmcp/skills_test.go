package opsmcp_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// skillPages serves one page in the tool-skills container and records every
// write it is asked for, so a case can tell a refusal from a write that
// landed.
type skillPages struct {
	mu    sync.Mutex
	wrote []string
}

func (k *skillPages) List(context.Context, pages.Filter, statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (k *skillPages) Get(_ context.Context, ref string, _ statelog.Freshness) (pages.Detail, error) {
	if ref != "p-skill" {
		return pages.Detail{}, pages.ErrNotFound
	}
	return pages.Detail{Page: pages.Page{
		ID: "p-skill", Container: "TS", Title: "Chat conventions", Version: 1,
	}}, nil
}

func (k *skillPages) did(what string) (pages.Written, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.wrote = append(k.wrote, what)
	return pages.Written{Page: pages.Page{ID: "p-skill", Container: "TS"},
		Outcome: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (k *skillPages) writes() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.wrote...)
}

func (k *skillPages) Create(_ context.Context, _ pages.Actor, in pages.NewPage) (pages.Written, error) {
	return k.did("create " + in.Container + "/" + in.Title)
}

func (k *skillPages) SavePage(_ context.Context, _ pages.Actor, id string, _ pages.Save) (pages.Written, error) {
	return k.did("save " + id)
}

func (k *skillPages) Rename(_ context.Context, _ pages.Actor, id, title string, _ bool) (pages.Written, error) {
	return k.did("rename " + id + " " + title)
}

func (k *skillPages) Comment(_ context.Context, _ pages.Actor, id string,
	_ pages.NewComment) (pages.Comment, pages.Written, error) {

	written, err := k.did("comment " + id)
	return pages.Comment{ID: "c-1"}, written, err
}

func (k *skillPages) EditComment(_ context.Context, _ pages.Actor, id, cid, _ string) (
	pages.Comment, pages.Written, error) {

	written, err := k.did("edit " + id + " " + cid)
	return pages.Comment{ID: cid}, written, err
}

// A TOOL SKILL IS CONFIGURATION, AND THE OPERATOR'S ASSISTANT NEEDS THE GRANT.
//
// internal/pages refuses an AGENT every write into the skills container and
// exempts every person, because whether a person may is a capability and the
// store holds no grants. So over this surface — whose every caller is a person
// or a credential — a token holding only knowledge:write rewrote the
// instructions injected into every seat's turn. A skill page takes config:write
// on top of the colleague write, and an ordinary container is untouched.
func TestAToolSkillWriteOverTheOperatorSurfaceNeedsConfigWrite(t *testing.T) {
	t.Parallel()
	kb := &skillPages{}
	dir := &directory{people: map[string]iam.Principal{
		"writer": machine("token:writer", iam.GrantStateRead, iam.GrantKnowledgeWrite),
		"admin": machine("token:admin", iam.GrantStateRead, iam.GrantKnowledgeWrite,
			iam.GrantConfigWrite),
	}}
	s := opsmcp.New(opsmcp.Options{
		Pages: builtin.PageDeps{
			Reader: kb, Writer: kb, Actor: builtin.PrincipalPageActor,
			SkillsContainer: func() string { return "ts" },
		},
		Authorize: builtin.Decide(authz.NoChart{}),
	})
	if s == nil {
		t.Fatal("a company on the native knowledge base got no surface")
	}
	server := httptest.NewServer(dir.middleware(s.Handler()))
	t.Cleanup(server.Close)

	call := func(who, tool string, args map[string]any) (bool, string) {
		t.Helper()
		sess := connectAs(t, server.URL+opsmcp.Path, who)
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
			t.Errorf("%s: knowledge:write alone wrote into the skills container: %s",
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
		t.Errorf("config:write could not publish a tool skill: %s", out)
	}
	if refused, out := call("admin", builtin.SavePageTool, save); refused {
		t.Errorf("config:write could not change a tool skill: %s", out)
	}

	// AN ORDINARY CONTAINER IS THE COLLEAGUE WRITE IT ALWAYS WAS, or the
	// rule would be a ban on writing pages without config:write.
	if refused, out := call("writer", builtin.WritePageTool,
		map[string]any{"container": "ENG", "title": "Runbook", "body": "x"}); refused {
		t.Errorf("knowledge:write could not write an ordinary page: %s", out)
	}
	if got, want := kb.writes(), []string{
		"create TS/Deploys", "save p-skill", "create ENG/Runbook",
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the store saw %v, want %v", got, want)
	}
}
