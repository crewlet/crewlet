package operator_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY TRANSPORT SERVES THE ONE CATALOGUE, and this holds the two sides a
// transport has against each other: what it LISTS to a client over the wire,
// and what the catalogue's own dispatch — the path every transport takes into
// a tool — actually runs.
//
// The failure it guards is the one the package doc names: a transport that
// assembled its own tool set would have a second chance to wire it
// differently, and the result is a verb one surface offers and another does
// not, or one listed with hints that are not the catalogue's. Each surface
// looks complete from inside, which is why nothing else would notice.
//
// THROUGH A REAL MCP CLIENT, because the listing a client receives is the SDK's
// serialisation of what was registered — the property is what arrives, not
// what this package believes it registered.
func TestBothTransportsServeTheSameCatalogue(t *testing.T) {
	t.Parallel()
	s := operator.New(operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Actor: operator.WorkActor(nil),
		},
		Pages: builtin.PageDeps{
			Reader: stubPageReader{}, Writer: stubPageWriter{},
			Actor: operator.PageActor,
		},
	})
	if s == nil {
		t.Fatal("a company on both native backends got no surface")
	}
	sess := dialOperator(t, s, "ops-bot")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		// THE HINTS A CLIENT RECEIVES ARE THE CATALOGUE'S, not a copy
		// the transport decided for itself.
		want := crewletmcp.SDKAnnotations(s.Annotations(tool.Name))
		if (tool.Annotations == nil) != (want == nil) ||
			(want != nil && !reflect.DeepEqual(*tool.Annotations, *want)) {

			t.Errorf("%s reaches a client with hints %+v, the catalogue "+
				"advertises %+v", tool.Name, tool.Annotations, want)
		}
	}
	slices.Sort(names)
	catalogue := s.Tools()
	slices.Sort(catalogue)
	if !slices.Equal(names, catalogue) {
		t.Fatalf("the MCP transport lists %v; the catalogue holds %v", names, catalogue)
	}

	// AND EVERY LISTED VERB IS ONE THE DISPATCH RUNS, with the same answer:
	// a listing and a dispatch that disagreed would be a verb a client sees
	// and cannot call, or one it can call and never sees.
	for _, name := range names {
		if _, served, _ := s.Dispatch(auth.WithOperator(t.Context(), "ops-bot"),
			name, nil); !served {

			t.Errorf("the MCP transport lists %q and the catalogue does not serve it", name)
		}
	}
	args := map[string]any{"item": "ENG-1"}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.GetWorkItemTool, Arguments: args,
	})
	if err != nil {
		t.Fatalf("call over MCP: %v", err)
	}
	direct, served, err := s.Dispatch(auth.WithOperator(t.Context(), "ops-bot"),
		tracker.GetWorkItemTool, args)
	if err != nil || !served {
		t.Fatalf("dispatch: served=%v err=%v", served, err)
	}
	if got := textOf(res); got != direct.Output || res.IsError != direct.Failed {
		t.Errorf("one call answered %q (failed=%v) over MCP and %q (failed=%v) "+
			"through the dispatch", got, res.IsError, direct.Output, direct.Failed)
	}

	// AND A NAME THE CATALOGUE DOES NOT HOLD IS NOT SERVED, rather than
	// resolved to something near it.
	if _, served, _ := s.Dispatch(t.Context(), "run_sandbox", nil); served {
		t.Error("the dispatch served a tool that is in no operator's catalogue")
	}
}

// A PERSON'S RETRIED COMMENT IS ONE COMMENT, and the page half of the request
// key is wired by the surface itself rather than by whoever builds its deps.
//
// The work tools carry the key on their actor; the page tools read it through
// [builtin.PageDeps.RequestKey], which [operator.New] sets because this package
// is the one that puts the key on the context. Left to the caller, one side is
// wired and the other is not, and a comment posted twice by a retry is two
// comments on somebody's page.
func TestARetriedPageCommentCarriesItsRequestKey(t *testing.T) {
	t.Parallel()
	kb := &recordingPages{}
	s := operator.New(operator.Options{
		Pages: builtin.PageDeps{Reader: kb, Writer: kb, Actor: operator.PageActor},
	})
	if s == nil {
		t.Fatal("a company on the native knowledge base got no surface")
	}
	comment := func(key string) {
		ctx := auth.WithOperator(t.Context(), "founder")
		if key != "" {
			ctx = operator.WithRequestKey(ctx, key)
		}
		got, served, err := s.Dispatch(ctx, builtin.CommentOnPageTool,
			map[string]any{"page": "Runbook", "body": "the step is wrong"})
		if err != nil || !served || got.Failed {
			t.Fatalf("comment: served=%v err=%v result=%+v", served, err, got)
		}
	}
	comment("0f7c1a4e-9b2d-4e51-8c3a-6d7e8f9a0b1c")
	comment("0f7c1a4e-9b2d-4e51-8c3a-6d7e8f9a0b1c")
	comment("")
	comment("")

	keys := kb.keys()
	if len(keys) != 4 {
		t.Fatalf("four comments wrote %d", len(keys))
	}
	if keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("a retried request commented under %q and then %q — the "+
			"ledger cannot collapse the retry, so it is a second comment",
			keys[0], keys[1])
	}
	if keys[2] != "" || keys[3] != "" {
		t.Errorf("a call naming no request carried the keys %q and %q — a "+
			"stable key collapses an assistant's second comment into its first",
			keys[2], keys[3])
	}
}

// AND THE WORK ACTOR CARRIES THE KEY, and carries nothing where none was set.
func TestTheWorkActorCarriesTheRequestKey(t *testing.T) {
	t.Parallel()
	ctx := auth.WithOperator(t.Context(), "founder")
	keyed, err := operator.WorkActor(nil)(operator.WithRequestKey(ctx, "r-1"), nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if keyed.RequestKey != "r-1" || keyed.OperationSeed() != "req-r-1" {
		t.Errorf("a request keyed r-1 resolved to %+v (seed %q)", keyed, keyed.OperationSeed())
	}
	plain, err := operator.WorkActor(nil)(ctx, nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if plain.RequestKey != "" || plain.OperationSeed() != "" {
		t.Errorf("a request naming no key resolved to %+v", plain)
	}
}

// dialOperator connects an MCP client to the surface as one token.
//
// THE GUARD IS STOOD IN FOR by stamping the operator on the context, which is
// all the app's own guard does for a request it admits: this case is about
// what the transport serves, and the guard has cases of its own.
func dialOperator(t *testing.T, s *operator.Server, token string) *mcp.ClientSession {
	t.Helper()
	inner := s.MCPHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), token)))
	}))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + operator.MCPPath, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// recordingPages is a knowledge base holding one page, recording the call key
// every comment arrives with.
type recordingPages struct {
	stubPageWriter
	mu       sync.Mutex
	callKeys []string
}

func (*recordingPages) List(context.Context, pages.Filter,
	statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (*recordingPages) Get(context.Context, string,
	statelog.Freshness) (pages.Detail, error) {
	return pages.Detail{Page: pages.Page{ID: "p-1", Title: "Runbook"}}, nil
}

func (p *recordingPages) Comment(_ context.Context, _ pages.Actor, _ string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {

	p.mu.Lock()
	defer p.mu.Unlock()
	p.callKeys = append(p.callKeys, in.CallKey)
	return pages.Comment{ID: "c-1"}, pages.Written{}, nil
}

func (p *recordingPages) keys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.callKeys...)
}
