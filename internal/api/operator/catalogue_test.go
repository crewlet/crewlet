package operator_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY TRANSPORT SERVES THE ONE CATALOGUE, and this holds the two sides a
// transport has against each other: what it LISTS to a client over the wire,
// and what the dispatch — the path every transport takes into a tool —
// actually runs.
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
func TestEveryTransportServesTheSameCatalogue(t *testing.T) {
	t.Parallel()
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter, Merges: stubWorkMerger,
		},
		Pages: builtin.PageDeps{Reader: stubPageReader{}, Writer: stubPageWriter{}},
	})})
	ops := machine("token:ops-bot", iam.Apps...)
	sess := dialOperator(t, s, ops)

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
	asOps := iam.WithPrincipal(t.Context(), ops)
	for _, name := range names {
		if _, err := s.Dispatch(asOps, operator.Call{
			Transport: types.TransportMCP, Tool: name,
		}); errors.Is(err, operator.ErrNotServed) {
			t.Errorf("the MCP transport lists %q and the dispatch does not serve it", name)
		}
	}
	args := map[string]any{"item": "ENG-1"}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.GetWorkItemTool, Arguments: args,
	})
	if err != nil {
		t.Fatalf("call over MCP: %v", err)
	}
	direct, err := s.Dispatch(asOps, operator.Call{
		Transport: types.TransportMCP, Tool: tracker.GetWorkItemTool, Args: args,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := textOf(res); got != direct.Output || res.IsError != direct.Failed {
		t.Errorf("one call answered %q (failed=%v) over MCP and %q (failed=%v) "+
			"through the dispatch", got, res.IsError, direct.Output, direct.Failed)
	}

	// AND A NAME THE CATALOGUE DOES NOT HOLD IS NOT SERVED, rather than
	// resolved to something near it.
	if _, err := s.Dispatch(asOps, operator.Call{Tool: "run_sandbox"}); !errors.Is(err, operator.ErrNotServed) {
		t.Errorf("the dispatch answered %v for a tool that is in no operator's catalogue", err)
	}
}

// A PERSON'S RETRIED COMMENT IS ONE COMMENT: the operation a call's transport
// names reaches the knowledge base's write as that write's own key, so the
// ledger collapses the retry.
//
// And a call naming NONE — an assistant's, over MCP — carries none: a stable
// key there would collapse an assistant's second comment into its first.
func TestARetriedPageCommentCarriesItsOperation(t *testing.T) {
	t.Parallel()
	kb := &recordingPages{}
	s := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Pages: builtin.PageDeps{Reader: kb, Writer: kb},
	})})
	ctx := iam.WithPrincipal(t.Context(),
		person("jane.founder", "jane-founder", iam.GrantKnowledge))
	args := map[string]any{"page": "Runbook", "body": "the step is wrong"}
	key := statelog.NewOpID(time.Now(), "")
	comment := func(key string) {
		t.Helper()
		got, err := s.Dispatch(ctx, operator.Call{
			Transport: types.TransportAct, Key: key,
			Tool: builtin.CommentOnPageTool, Args: args,
		})
		if err != nil || got.Failed {
			t.Fatalf("comment: err=%v result=%+v", err, got)
		}
	}
	comment(key)
	comment(key)
	comment("")

	keys := kb.keys()
	if len(keys) != 3 {
		t.Fatalf("three comments wrote %d", len(keys))
	}
	if keys[0] != key || keys[1] != key {
		t.Errorf("a retried request commented under %q and then %q, want %q — the "+
			"ledger cannot collapse the retry, so it is a second comment",
			keys[0], keys[1], key)
	}
	if keys[2] != "" {
		t.Errorf("a call naming no operation carried the key %q", keys[2])
	}
}

// THE WORK ACTOR CARRIES THE OPERATION AT ITS OWN INSTANT, and carries nothing
// where none was named — an MCP call's operation is named later, by the tool,
// from what it was asked ([builtin.OperatorTools]).
//
// THE INSTANT IS THE KEY'S, because it is what the node's operation ledger
// judges a retry by: a key read as minted at the call's clock is one a retry
// after a ledger loss decides a second time.
func TestTheWorkActorCarriesTheOperation(t *testing.T) {
	t.Parallel()
	ctx := iam.WithPrincipal(t.Context(), machine("token:founder", iam.GrantTracker))
	key := statelog.NewOpID(time.Now().Add(-time.Hour), "")
	named, err := operator.WorkActor(operator.WithKey(ctx, key), nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	at, _ := statelog.OpMintedAt(key)
	if named.WorkKey != key || !named.WorkSince.Equal(at) || named.Operation != "" {
		t.Errorf("a call under %s resolved to %+v — its operation is the request's "+
			"key at the key's own instant %v", key, named, at)
	}
	if named.OperationSeed() != key || !named.OperationSince().Equal(at) {
		t.Errorf("the writes derive from seed %q at %v, want %q at %v",
			named.OperationSeed(), named.OperationSince(), key, at)
	}
	plain, err := operator.WorkActor(ctx, nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if plain.WorkKey != "" || plain.OperationSeed() != "" {
		t.Errorf("a call naming no operation resolved to %+v", plain)
	}
}

// ANY SERVER OF THE CATALOGUE ANSWERS ANY REQUEST: a client's requests spread
// over two servers built from the same options — two ingress nodes behind a
// load balancer with no affinity — are each answered, and no answer names a
// session for the client to come back to. The transport kept sessions once,
// each held by the one handler that opened it: of two nodes, every request
// reaching the other was refused 404 "session not found", which the MCP client
// this repository ships reads as the end of its connection.
func TestAnyServerOfTheCatalogueAnswersAnyRequest(t *testing.T) {
	t.Parallel()
	opts := operator.Options{Halves: fixed(operator.Halves{
		Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: stubWorkWriter},
	})}
	nodes := []*operator.Server{newSurface(t, opts), newSurface(t, opts)}
	ops := machine("token:ops-bot", iam.Apps...)
	var (
		mu       sync.Mutex
		served   [2]int
		sessions []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		node := (served[0] + served[1]) % len(nodes)
		served[node]++
		mu.Unlock()
		nodes[node].MCPHandler().ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), ops)))
		if id := w.Header().Get("Mcp-Session-Id"); id != "" {
			mu.Lock()
			sessions = append(sessions, id)
			mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	sess := connectOperator(t, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for i := range 4 {
		if _, err := sess.ListTools(ctx, nil); err != nil {
			t.Fatalf("listing %d, on whichever node it reached: %v", i, err)
		}
		// THE TOOL'S OWN ANSWER, whatever it says — the stub tracker holds
		// no item, so this one is a refusal — is what being served looks
		// like; a request no node served is an error on the transport.
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: tracker.GetWorkItemTool, Arguments: map[string]any{"item": "ENG-1"},
		})
		if err != nil {
			t.Fatalf("call %d, on whichever node it reached: %v", i, err)
		}
		if textOf(res) == "" {
			t.Errorf("call %d was answered nothing", i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if served[0] == 0 || served[1] == 0 {
		t.Errorf("the requests reached the nodes %v times; the case needs both", served)
	}
	if len(sessions) > 0 {
		t.Errorf("the transport handed the client sessions %v; a session is held by "+
			"one node, and every other node refuses it", sessions)
	}
}

// A LISTENER IS TOLD ITS TOOLS CHANGED, ON WHATEVER CATALOGUE ITS STREAM IS.
// With no session kept, the one way to reach a connected client between its
// requests is the stream a client of the 2026-07-28 protocol holds open —
// `subscriptions/listen`, on the catalogue its listen request was answered
// from, which no later call reads. The server keeps every such stream in the
// node's one registry, so an announcement after what the node serves has
// moved reaches the stream, and the client's next listing is the catalogue a
// call reads now. Nothing announced it once, and a client kept offering the
// tools it connected to until it was reconnected.
//
// And the registry counts the stream while it is open and forgets it once it
// ends, so a node holds nothing for a client that has gone.
func TestAListenerIsToldWhicheverCatalogueItIsOn(t *testing.T) {
	t.Parallel()
	listeners := operator.NewListeners()
	work := operator.Halves{Work: builtin.WorkDeps{Reader: stubWorkReader{}}}
	withPages := work
	withPages.Pages = builtin.PageDeps{Reader: stubPageReader{}}
	var current atomic.Pointer[operator.Halves]
	current.Store(&work)
	s := newSurface(t, operator.Options{
		Halves:    func() (operator.Halves, bool) { return *current.Load(), true },
		Listeners: listeners,
	})
	ops := machine("token:ops-bot", iam.Apps...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.MCPHandler().ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), ops)))
	}))
	t.Cleanup(srv.Close)
	if n := listeners.Listening(); n != 0 {
		t.Fatalf("the premise: a registry no client has reached holds %d streams", n)
	}
	sess, changed := listenOperator(t, srv.URL)
	if n := listeners.Listening(); n != 1 {
		t.Errorf("a registry holding one listener's stream counts %d", n)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("before the halves moved: %v", err)
	}
	if hasTool(listed, builtin.GetPageTool) {
		t.Fatalf("a catalogue with no knowledge base lists %s", builtin.GetPageTool)
	}

	current.Store(&withPages)
	listeners.Announce(t.Context())
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("the listener was never told the tools changed")
	}
	if listed, err = sess.ListTools(ctx, nil); err != nil {
		t.Fatalf("the listing after the announcement: %v", err)
	}
	if !hasTool(listed, builtin.GetPageTool) {
		t.Errorf("after the announcement the listener was listed the catalogue it "+
			"connected to, without %s", builtin.GetPageTool)
	}

	_ = sess.Close()
	for listeners.Listening() != 0 {
		if ctx.Err() != nil {
			t.Fatal("the registry still counts the stream of a listener that left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// listenOperator connects an MCP client to an operator transport served at
// base as the client this repository ships does when it is handed a tool-list
// handler — at the 2026-07-28 protocol, opening a `subscriptions/listen` stream
// at connect — and answers the session and a channel receiving once per
// notification that the tools changed. It returns once the server has
// acknowledged the stream and told it the once a stream is told as it opens
// ([operator.Listeners]), so nothing a case does next races the subscription
// and the channel starts quiet.
func listenOperator(t *testing.T, base string) (*mcp.ClientSession, <-chan struct{}) {
	t.Helper()
	changed := make(chan struct{}, 16)
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				changed <- struct{}{}
			},
		})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: base + operator.MCPPath, HTTPClient: httpxtest.Pool(t),
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("the listener's stream was never told the tools changed as it opened")
	}
	return sess, changed
}

// hasTool reports whether a listing names a tool.
func hasTool(listed *mcp.ListToolsResult, name string) bool {
	return slices.ContainsFunc(listed.Tools, func(tool *mcp.Tool) bool {
		return tool.Name == name
	})
}

// connectOperator connects an MCP client to an operator transport served at
// base, the way an operator's assistant does.
func connectOperator(t *testing.T, base string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: base + operator.MCPPath, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// dialOperator connects an MCP client to the surface as one principal.
//
// THE GUARD IS STOOD IN FOR by stamping the principal on the context, which is
// all the app's own guard does for a request it admits: this case is about
// what the transport serves, and the guard has cases of its own.
func dialOperator(t *testing.T, s *operator.Server, p iam.Principal) *mcp.ClientSession {
	t.Helper()
	inner := s.MCPHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	return connectOperator(t, srv.URL)
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

// recordingPages is a knowledge base holding one page, recording the operation
// key every comment's actor arrives with.
type recordingPages struct {
	stubPageWriter
	mu     sync.Mutex
	opKeys []string
}

func (*recordingPages) List(context.Context, pages.Filter,
	statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (*recordingPages) Get(context.Context, string,
	statelog.Freshness) (pages.Detail, error) {
	return pages.Detail{Page: pages.Page{ID: "p-1", Container: "ENG", Title: "Runbook"}}, nil
}

func (p *recordingPages) Comment(_ context.Context, actor pages.Actor, _ string,
	_ pages.NewComment) (pages.Comment, pages.Written, error) {

	p.mu.Lock()
	defer p.mu.Unlock()
	p.opKeys = append(p.opKeys, actor.OpKey)
	return pages.Comment{ID: "c-1"}, pages.Written{}, nil
}

func (p *recordingPages) keys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.opKeys...)
}
