package operator_test

import (
	"context"
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
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Merges: stubWorkMerger, Actor: operator.WorkActor(nil, nil),
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

// A PERSON'S RETRIED COMMENT IS ONE COMMENT, and the page half of the call's
// operation is wired by the surface itself rather than by whoever builds its
// deps.
//
// The work tools carry the operation on their actor; the page tools read it
// through [builtin.PageDeps.Operation], which [operator.New] sets because this
// package is the one that puts the operation on the context. Left to the
// caller, one side is wired and the other is not, and a comment posted twice
// by a retry is two comments on somebody's page.
func TestARetriedPageCommentCarriesItsOperation(t *testing.T) {
	t.Parallel()
	kb := &recordingPages{}
	s := newSurface(t, operator.Options{
		Pages: builtin.PageDeps{Reader: kb, Writer: kb, Actor: operator.PageActor},
	})
	if s == nil {
		t.Fatal("a company on the native knowledge base got no surface")
	}
	args := map[string]any{"page": "Runbook", "body": "the step is wrong"}
	op, err := builtin.RequestOperation("founder", requestA, builtin.CommentOnPageTool, args)
	if err != nil {
		t.Fatalf("RequestOperation: %v", err)
	}
	comment := func(op string) {
		ctx := auth.WithOperator(t.Context(), "founder")
		if op != "" {
			ctx = operator.WithOperation(ctx, op)
		}
		got, served, err := s.Dispatch(ctx, builtin.CommentOnPageTool, args)
		if err != nil || !served || got.Failed {
			t.Fatalf("comment: served=%v err=%v result=%+v", served, err, got)
		}
	}
	comment(op)
	comment(op)
	comment("")
	comment("")

	keys := kb.keys()
	if len(keys) != 4 {
		t.Fatalf("four comments wrote %d", len(keys))
	}
	if keys[0].Seed == "" || keys[0] != keys[1] {
		t.Errorf("a retried request commented under %+v and then %+v — the "+
			"ledger cannot collapse the retry, so it is a second comment",
			keys[0], keys[1])
	}
	// AND AT THE REQUEST'S OWN INSTANT, which is what the node's operation
	// ledger judges the retry by: a key with none reads as minted before
	// every row the ledger ever lost, and is answered `unknown` unmade.
	if want, _ := statelog.OpMintedAt(op); !keys[0].Since.Equal(want) || want.IsZero() {
		t.Errorf("the comment's key carries the instant %v, want the request's %v",
			keys[0].Since, want)
	}
	if keys[2].Seed != "" || keys[3].Seed != "" {
		t.Errorf("a call naming no request carried the keys %+v and %+v — a "+
			"stable key collapses an assistant's second comment into its first",
			keys[2], keys[3])
	}
}

// AND THE WORK ACTOR CARRIES THE OPERATION, and carries nothing where none was
// set — an MCP call's operation is named later, by the tool, from what it was
// asked ([builtin.OperatorTools]).
func TestTheWorkActorCarriesTheOperation(t *testing.T) {
	t.Parallel()
	ctx := auth.WithOperator(t.Context(), "founder")
	op, err := builtin.RequestOperation("founder", requestA, builtin.CreateWorkItemTool,
		map[string]any{"title": "x"})
	if err != nil {
		t.Fatalf("RequestOperation: %v", err)
	}
	named, err := operator.WorkActor(nil, nil)(operator.WithOperation(ctx, op), nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if named.Operation != op || named.OperationSeed() != "" {
		t.Errorf("a call under %s resolved to %+v (seed %q) — a person's call is "+
			"its operation, never a turn's seed", op, named, named.OperationSeed())
	}
	plain, err := operator.WorkActor(nil, nil)(ctx, nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if plain.Operation != "" || plain.OperationSeed() != "" {
		t.Errorf("a call naming no operation resolved to %+v", plain)
	}
}

// ANY SERVER OF THE CATALOGUE ANSWERS ANY REQUEST: a client's requests spread
// over two servers built from the same deps — two ingress nodes behind a load
// balancer with no affinity, or a catalogue a node built again between two
// requests — are each answered, and no answer names a session for the client
// to come back to. The transport kept sessions once, each held by the one
// handler that opened it: of two nodes, every request reaching the other was
// refused 404 "session not found", which the MCP client this repository ships
// reads as the end of its connection.
func TestAnyServerOfTheCatalogueAnswersAnyRequest(t *testing.T) {
	t.Parallel()
	opts := operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Actor: operator.WorkActor(nil, nil),
		},
	}
	nodes := []*operator.Server{newSurface(t, opts), newSurface(t, opts)}
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
		nodes[node].MCPHandler().ServeHTTP(w,
			r.WithContext(auth.WithOperator(r.Context(), "ops-bot")))
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

// A CATALOGUE SWAPPED MID-CONVERSATION KEEPS ITS CLIENT. The app asks its
// operator surface for the catalogue on every request, and a node builds it
// again when what it serves changes — its first apply, a knowledge backend
// added or removed, a rename. A client connected before the swap goes on
// calling through it, and its next listing is the new catalogue's; with
// sessions, every connection opened on the replaced catalogue ended at its next
// request.
func TestACatalogueSwappedMidConversationKeepsItsClient(t *testing.T) {
	t.Parallel()
	work := builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)}
	reads := newSurface(t, operator.Options{Work: work})
	steers := newSurface(t, operator.Options{
		Work:  work,
		Steer: builtin.SteerDeps{Asker: struct{ builtin.FleetAsker }{}, Actor: operator.WorkActor(nil, nil)},
	})
	var current atomic.Pointer[operator.Server]
	current.Store(reads)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current.Load().MCPHandler().ServeHTTP(w,
			r.WithContext(auth.WithOperator(r.Context(), "ops-bot")))
	}))
	t.Cleanup(srv.Close)
	sess := connectOperator(t, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("before the swap: %v", err)
	}
	if hasTool(listed, builtin.SteerTurnTool) {
		t.Fatalf("the catalogue of reads lists %s", builtin.SteerTurnTool)
	}

	current.Store(steers)
	listed, err = sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("the first request after the swap: %v", err)
	}
	if !hasTool(listed, builtin.SteerTurnTool) {
		t.Errorf("after the swap the client was listed the old catalogue, without %s",
			builtin.SteerTurnTool)
	}
	// A CALL AFTER IT, served whatever the tool answers.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.GetWorkItemTool, Arguments: map[string]any{"item": "ENG-1"},
	}); err != nil {
		t.Fatalf("a call after the swap: %v", err)
	}
}

// A LISTENER IS TOLD ITS TOOLS CHANGED, WHICHEVER CATALOGUE ITS STREAM IS ON.
// The server advertises `tools.listChanged`, and with no session kept the one
// way to reach a connected client between its requests is the stream a client
// of the 2026-07-28 protocol holds open — `subscriptions/listen`, on the
// catalogue that answered it. Every catalogue a node builds keeps its streams
// in the node's one registry, so an announcement after a swap reaches a stream
// parked on the catalogue the swap replaced, and the client's next listing is
// the catalogue serving now. Nothing announced it once, and a client kept
// offering the replaced catalogue's tools until it was reconnected.
//
// And the registry counts the stream while it is open and forgets it once it
// ends, so a node holds nothing for a client that has gone.
func TestAListenerIsToldWhicheverCatalogueItIsOn(t *testing.T) {
	t.Parallel()
	listeners := operator.NewListeners()
	work := builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)}
	reads := newSurface(t, operator.Options{Work: work, Listeners: listeners})
	steers := newSurface(t, operator.Options{
		Work:      work,
		Steer:     builtin.SteerDeps{Asker: struct{ builtin.FleetAsker }{}, Actor: operator.WorkActor(nil, nil)},
		Listeners: listeners,
	})
	var current atomic.Pointer[operator.Server]
	current.Store(reads)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current.Load().MCPHandler().ServeHTTP(w,
			r.WithContext(auth.WithOperator(r.Context(), "ops-bot")))
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
		t.Fatalf("before the swap: %v", err)
	}
	if hasTool(listed, builtin.SteerTurnTool) {
		t.Fatalf("the catalogue of reads lists %s", builtin.SteerTurnTool)
	}

	current.Store(steers)
	listeners.Announce(t.Context())
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("the listener on the replaced catalogue was never told the tools changed")
	}
	if listed, err = sess.ListTools(ctx, nil); err != nil {
		t.Fatalf("the listing after the announcement: %v", err)
	}
	if !hasTool(listed, builtin.SteerTurnTool) {
		t.Errorf("after the announcement the listener was listed the replaced catalogue, "+
			"without %s", builtin.SteerTurnTool)
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

// recordingPages is a knowledge base holding one page, recording the call key
// every comment arrives with.
type recordingPages struct {
	stubPageWriter
	mu       sync.Mutex
	callKeys []pages.CallKey
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

func (p *recordingPages) keys() []pages.CallKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pages.CallKey(nil), p.callKeys...)
}
