package opsmcp_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tracker"
)

// directory resolves a bearer to a principal the way the guard does, from a
// table a case can change BETWEEN two calls — which is exactly what a grant
// withdrawn by an administrator, a lowered ceiling or a revoked session is to
// a request arriving after it.
type directory struct {
	mu     sync.Mutex
	people map[string]iam.Principal
}

func (d *directory) set(bearer string, p iam.Principal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.people[bearer] = p
}

// middleware is the guard's contract as this surface reads it: every request
// leaves carrying an answer, resolved or anonymous.
func (d *directory) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		d.mu.Lock()
		p, ok := d.people[bearer]
		d.mu.Unlock()
		if !ok {
			next.ServeHTTP(w, r.WithContext(iam.WithAnonymous(r.Context())))
			return
		}
		next.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	})
}

// machine is a credential holding exactly the grants named.
func machine(login string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(login)), Login: login,
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: grants,
	}
}

// bearer presents one credential on every request a client sends.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// operatorServer serves the surface behind the directory, over the real
// authority decision.
func operatorServer(t *testing.T, dir *directory) *httptest.Server {
	t.Helper()
	s := opsmcp.New(opsmcp.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			Actor: builtin.PrincipalActor,
		},
		Authorize: builtin.Decide(authz.NoChart{}),
	})
	if s == nil {
		t.Fatal("a company on the native tracker got no surface")
	}
	server := httptest.NewServer(dir.middleware(s.Handler()))
	t.Cleanup(server.Close)
	return server
}

// connectAs opens an MCP client that presents one credential.
func connectAs(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool := httpxtest.Pool(t)
	pool.Transport = bearer{token: token, base: pool.Transport}
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: pool,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// list calls list_work_items and reports whether it was refused, with what.
func list(t *testing.T, sess *mcp.ClientSession) (bool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.ListWorkItemsTool, Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			out.WriteString(text.Text)
		}
	}
	return res.IsError, out.String()
}

// A GRANT WITHDRAWN MID-SESSION IS WITHDRAWN FROM THE NEXT CALL.
//
// The stateful handler decided every call in a session as the request that
// OPENED it, so an assistant that connected while its credential could read
// the board went on reading it after an administrator took that away — or
// lowered the ceiling, or revoked the session — for as long as it stayed
// connected. Each call is decided by the credential the request carrying it
// resolved to, on that request.
func TestAGrantWithdrawnMidSessionStopsTheNextCall(t *testing.T) {
	t.Parallel()
	dir := &directory{people: map[string]iam.Principal{
		"ops": machine("token:ops", iam.GrantStateRead),
	}}
	server := operatorServer(t, dir)
	sess := connectAs(t, server.URL+opsmcp.Path, "ops")

	if refused, out := list(t, sess); refused {
		t.Fatalf("a credential holding state:read was refused the board: %s", out)
	}
	dir.set("ops", machine("token:ops", iam.GrantWorkWrite))
	refused, out := list(t, sess)
	if !refused {
		t.Fatalf("the board was served after state:read was withdrawn, on the "+
			"session opened while it was held: %s", out)
	}
	if !strings.Contains(out, "refused") {
		t.Errorf("the refusal does not read as one: %s", out)
	}

	// AND RESTORED, so the refusal above is the grant and not a session
	// that simply stopped working.
	dir.set("ops", machine("token:ops", iam.GrantStateRead))
	if refused, out := list(t, sess); refused {
		t.Fatalf("the board was refused after state:read came back: %s", out)
	}
}

// ANOTHER CREDENTIAL CANNOT RIDE A SESSION IT DID NOT OPEN.
//
// A session id is a handle a client echoes, not a secret anybody proved, and
// the stateful handler served any request presenting one AS the credential
// that opened it: a token holding no grant at all, sent with the operator's
// Mcp-Session-Id, read the board as the operator. A request is decided as the
// credential it presents, whatever session id it carries.
func TestAnotherCredentialCannotRideTheOpenersSession(t *testing.T) {
	t.Parallel()
	dir := &directory{people: map[string]iam.Principal{
		"founder":  machine("token:founder", iam.GrantStateRead),
		"stranger": machine("token:stranger"),
	}}
	server := operatorServer(t, dir)
	sess := connectAs(t, server.URL+opsmcp.Path, "founder")
	if refused, out := list(t, sess); refused {
		t.Fatalf("the founder was refused their own board: %s", out)
	}

	// THE OPENER'S SESSION ID, where there is one, and a guessed one where
	// there is not — a surface that issued none must still decide a request
	// presenting one as its own credential.
	id := sess.ID()
	if id == "" {
		id = "the-founders-session"
	}
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` +
		tracker.ListWorkItemsTool + `","arguments":{}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.URL+opsmcp.Path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	req.Header.Set("Mcp-Session-Id", id)
	req.Header.Set("Authorization", "Bearer stranger")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	answer := string(raw)
	if res.StatusCode == http.StatusOK && !strings.Contains(answer, `"isError":true`) {
		t.Fatalf("a credential holding no grant read the board by presenting the "+
			"founder's session id (%d): %s", res.StatusCode, answer)
	}
	if res.StatusCode == http.StatusOK && !strings.Contains(answer, "state:read") &&
		!strings.Contains(answer, "refused") {
		t.Errorf("the stranger's call failed for a reason other than its own "+
			"authority: %s", answer)
	}
}

// THERE IS NO STREAM AND NO SESSION, so only a POST is served.
//
// The drain gate in internal/api gives this route no rule of its own on that
// premise: a GET passes it as a read and meets this 405, and a POST is refused
// like any write. A stateful handler would hold a server-to-client stream open
// on GET and end a session on DELETE — work a draining node would then serve
// through a gate that reads the one as harmless — so the premise is asserted
// here, where the handler that makes it true is built.
func TestTheOperatorSurfaceServesOnlyPOST(t *testing.T) {
	t.Parallel()
	dir := &directory{people: map[string]iam.Principal{}}
	dir.set("ops", machine("token:ops", iam.GrantStateRead))
	server := operatorServer(t, dir)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ops")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Mcp-Session-Id", "a-session-somebody-remembers")
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d, want 405: a stateless surface holds no "+
				"stream to open and no session to end", method, res.StatusCode)
		}
	}
}

// AN ARGUMENT THE TOOL DOES NOT READ IS REFUSED BY NAME, over MCP too.
//
// The HTTP routes refused one and this surface dropped it: the tool reads what
// its schema declares and nothing else, so an assistant still sending an
// argument a tool has retired — `save_work_view`'s `owner` was the one that
// saved a personal view as a shared tab — or one it misspelt was answered as
// though it had asked for less, and never told which. The tools' own gate
// refuses it, on this surface as on every other.
func TestAnArgumentTheToolDoesNotReadIsRefusedOverMCP(t *testing.T) {
	t.Parallel()
	dir := &directory{people: map[string]iam.Principal{
		"ops": machine("token:ops", iam.GrantStateRead),
	}}
	server := operatorServer(t, dir)
	sess := connectAs(t, server.URL+opsmcp.Path, "ops")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.ListWorkItemsTool, Arguments: map[string]any{"stauts": "todo"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			out.WriteString(text.Text)
		}
	}
	if !res.IsError || !strings.Contains(out.String(), `"stauts"`) {
		t.Errorf("a misspelt argument answered isError=%v %q, want it refused "+
			"by name", res.IsError, out.String())
	}
}
