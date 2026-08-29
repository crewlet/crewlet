package sandbox_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// What these tests protect.
//
// This backend talks to a service no test here can reach, so what is pinned
// is THE WIRE: every request shape, every header, and the framing of the one
// streaming call. A field spelled differently from what E2B expects fails
// only against the real API, minutes into a coding run, with a message from
// the vendor rather than from us — so the assertions are written as "the
// request carries X", not as "the method returns Y".

// e2bStub is a fake E2B: one server standing in for both planes, which is
// what lets a whole create-exec-kill round trip be driven in a test.
//
// The two planes are different HOSTS in production — api.<domain> and
// <port>-<box>.<domain> — and one server answers both here because the
// client reaches them through the same round-tripper. The paths are
// disjoint, so nothing is conflated.
type e2bStub struct {
	mu sync.Mutex

	// requests is every path the client asked for, in order. The record
	// IS the assertion for the calls whose only job is to be made.
	requests []string
	// bodies is the decoded JSON body of each control-plane write.
	bodies map[string]map[string]any
	// headers is the header set of the last request on each path.
	headers map[string]http.Header

	// files is the box's filesystem.
	files map[string][]byte

	// frames is what Process/Start streams back, in order.
	frames []any
	// startStatus overrides the streaming call's status when non-zero.
	startStatus int
	// killStatus overrides DELETE /sandboxes/{id} when non-zero.
	killStatus int
	// connectStatus overrides the connect call when non-zero.
	connectStatus int
	// clientID is what create reports; empty exercises the short hostname.
	clientID string
	// trailingGarbage appends an unreadable frame after the real ones, so
	// a client that should have stopped reading is caught doing so.
	trailingGarbage bool
	// streamDelay is how long the stub waits between frames, standing in
	// for a command that takes real time.
	streamDelay time.Duration
}

func newE2BStub() *e2bStub {
	return &e2bStub{
		bodies:   map[string]map[string]any{},
		headers:  map[string]http.Header{},
		files:    map[string][]byte{},
		clientID: "cl1",
	}
}

func (s *e2bStub) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.headers[r.URL.Path] = r.Header.Clone()
}

func (s *e2bStub) saw(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, got := range s.requests {
		if got == want {
			return true
		}
	}
	return false
}

func (s *e2bStub) body(path string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[path]
}

func (s *e2bStub) header(path, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[path].Get(name)
}

func (s *e2bStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	switch {
	case r.URL.Path == "/sandboxes" && r.Method == http.MethodPost:
		s.capture(r)
		_, _ = w.Write([]byte(`{"sandboxID": "sbx1", "clientID": "` + s.clientID +
			`", "templateID": "claude", "envdVersion": "0.1.0"}`))

	case strings.HasSuffix(r.URL.Path, "/timeout"),
		strings.HasSuffix(r.URL.Path, "/pause"):
		s.capture(r)
		w.WriteHeader(http.StatusNoContent)

	// /connect answers 200 when the box was already running and 201 when it
	// had to be woken, and returns the sandbox either way — which is why the
	// client needs no second call and no conflict to swallow.
	case strings.HasSuffix(r.URL.Path, "/connect"):
		s.capture(r)
		if s.connectStatus >= 400 {
			w.WriteHeader(s.connectStatus)
			return
		}
		// 201 still carries the sandbox — a woken box is returned just
		// like an already-running one, which is the whole point of the
		// endpoint.
		if s.connectStatus != 0 {
			w.WriteHeader(s.connectStatus)
		}
		_, _ = w.Write([]byte(`{"sandboxID": "sbx1", "clientID": "` + s.clientID +
			`", "templateID": "claude", "envdVersion": "0.1.0"}`))

	case strings.HasPrefix(r.URL.Path, "/sandboxes/") && r.Method == http.MethodDelete:
		if s.killStatus != 0 {
			w.WriteHeader(s.killStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case r.URL.Path == "/process.Process/Start":
		if s.startStatus != 0 {
			w.WriteHeader(s.startStatus)
			_, _ = w.Write([]byte(`envd said no`))
			return
		}
		s.captureStart(r)
		w.Header().Set("Content-Type", "application/connect+json")
		for _, frame := range s.frames {
			if s.streamDelay > 0 {
				// Flushed, so the client sees each frame as it lands
				// rather than in one buffered write at the end — which
				// is what makes the gap between them real.
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				time.Sleep(s.streamDelay)
			}
			_, _ = w.Write(connectFrame(0, frame))
		}
		if s.trailingGarbage {
			_, _ = w.Write([]byte{0, 0, 0, 0, 8, 'n', 'o', 't', ' ', 'j', 's', 'o', 'n'})
			return
		}
		_, _ = w.Write(connectFrame(0x02, map[string]any{}))

	case r.URL.Path == "/files":
		s.serveFiles(w, r)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// capture decodes a JSON body so a test can assert what was sent.
func (s *e2bStub) capture(r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[r.URL.Path] = body
}

// captureStart un-frames the Connect request, which is how the test reads
// what a command actually asked envd to run.
func (s *e2bStub) captureStart(r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if len(raw) < 5 {
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw[5:], &body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[r.URL.Path] = body
}

func (s *e2bStub) serveFiles(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		content, held := s.files[path]
		s.mu.Unlock()
		if !held {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(content)
	case http.MethodPost:
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		content, _ := io.ReadAll(file)
		s.mu.Lock()
		s.files[path] = content
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

// connectFrame builds one Connect envelope: a flag byte, a big-endian
// uint32 length, then the JSON payload.
func connectFrame(flag byte, payload any) []byte {
	body, _ := json.Marshal(payload)
	out := make([]byte, 5+len(body))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)))
	copy(out[5:], body)
	return out
}

// startEvent, dataEvent and endEvent are envd's three frame shapes.
func startEvent(pid int) any {
	return map[string]any{"event": map[string]any{
		"start": map[string]any{"pid": pid}}}
}

func dataEvent(stdout, stderr string) any {
	event := map[string]any{}
	if stdout != "" {
		event["stdout"] = []byte(stdout)
	}
	if stderr != "" {
		event["stderr"] = []byte(stderr)
	}
	return map[string]any{"event": map[string]any{"data": event}}
}

func endEvent(exit int) any {
	return map[string]any{"event": map[string]any{
		"end": map[string]any{"exitCode": exit}}}
}

// newE2B points a provider at the stub.
//
// THE ROUND-TRIPPER IS THE REDIRECTION, not a base-URL option, because the
// two planes' addresses are DERIVED from the domain — that derivation is
// part of what these tests check, so overriding it would test nothing.
func newE2B(t *testing.T, stub *e2bStub) (*sandbox.E2BProvider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "e2b_secret", Domain: "test.invalid",
		HTTP: &http.Client{Transport: &toStub{target: server.URL, seen: stub}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider, server
}

// toStub sends every request to the stub, remembering the host the client
// derived so a test can assert it.
type toStub struct {
	target string
	seen   *e2bStub

	mu    sync.Mutex
	hosts []string
}

func (r *toStub) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Scheme+"://"+req.URL.Host)
	r.mu.Unlock()

	routed := req.Clone(req.Context())
	target := strings.TrimPrefix(r.target, "http://")
	routed.URL.Scheme, routed.URL.Host, routed.Host = "http", target, target
	return http.DefaultTransport.RoundTrip(routed)
}

func (r *toStub) sawHost(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.hosts {
		if got == want {
			return true
		}
	}
	return false
}

// A KEYLESS PROVIDER IS REFUSED AT CONSTRUCTION, so an apply fails rather
// than every coding run: the API authenticates every call, including against
// a self-hosted cluster.
func TestE2BRefusesToBuildWithoutAKey(t *testing.T) {
	t.Parallel()
	if _, err := sandbox.NewE2B(sandbox.E2BOptions{Domain: "cluster.example"}); err == nil {
		t.Fatal("a keyless provider was built, so every create would 401")
	}
}

// THE CONTROL PLANE CARRIES THE KEY AS X-API-KEY.
//
// E2B accepts a bearer only for a user-scoped access token; the key from the
// dashboard is refused under that scheme — the same credential rejected
// purely on which header carried it.
func TestE2BCreateSendsTheDocumentedRequest(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)

	box, err := provider.Create(context.Background(), sandbox.Spec{
		CodingAgent: "claude-code",
		TimeoutSec:  900,
		Env:         map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if box.ID() != "sbx1" {
		t.Fatalf("sandbox id = %q", box.ID())
	}
	if got := stub.header("/sandboxes", "X-API-KEY"); got != "e2b_secret" {
		t.Errorf("X-API-KEY = %q", got)
	}
	if got := stub.header("/sandboxes", "Authorization"); got != "" {
		t.Errorf("a bearer was sent as well as the key: %q", got)
	}

	body := stub.body("/sandboxes")
	// THE TEMPLATE IS THE CODING AGENT'S. A bare box has no coding-agent
	// CLI in it, so the run would fail at its first command with "command
	// not found" — a template problem that reads as a broken runner.
	if body["templateID"] != "claude" {
		t.Errorf("templateID = %v, want the claude-code template", body["templateID"])
	}
	if body["timeout"] != float64(900) {
		t.Errorf("timeout = %v", body["timeout"])
	}
	envs, _ := body["envVars"].(map[string]any)
	if envs["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("the run environment did not reach the box: %v", body["envVars"])
	}
	// STAMPED SO A LEAK IS ATTRIBUTABLE: a box outlives the process that
	// made it, so an operator looking at a running VM has nothing else.
	meta, _ := body["metadata"].(map[string]any)
	if meta["crewlet"] == nil {
		t.Errorf("a box was minted with nothing marking it as ours: %v", body["metadata"])
	}
}

// THE TEMPLATE FALLS BACK IN ORDER: the spec's, the company's, the coding
// agent's own.
func TestE2BPicksATemplateInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, spec, agent, want string
	}{
		{"the spec wins", "company-box", "spec-box", "claude-code", "spec-box"},
		{"then the company's", "company-box", "", "claude-code", "company-box"},
		{"then the agent's own", "", "", "claude-code", "claude"},
		{"opencode has its own", "", "", "opencode", "opencode"},
		// An agent this build does not know gets E2B's bare image, on
		// which the run fails — which is honest: the config names an
		// agent nothing here can run, and inventing a template would
		// hide that behind a runtime error further in.
		{"an unknown agent gets the bare image", "", "", "cursor", "base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newE2BStub()
			server := httptest.NewServer(stub)
			t.Cleanup(server.Close)
			provider, err := sandbox.NewE2B(sandbox.E2BOptions{
				APIKey: "k", Domain: "test.invalid", Template: tc.company,
				HTTP: &http.Client{Transport: &toStub{target: server.URL, seen: stub}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Create(context.Background(), sandbox.Spec{
				CodingAgent: tc.agent, Template: tc.spec,
			}); err != nil {
				t.Fatal(err)
			}
			if got := stub.body("/sandboxes")["templateID"]; got != tc.want {
				t.Errorf("templateID = %v, want %q", got, tc.want)
			}
		})
	}
}

// THE BOX IS REACHED ON ITS OWN HOSTNAME, derived from the sandbox id, the
// client id and the domain.
//
// The client id is the field most easily mistaken for bookkeeping: a
// reconnect that kept only the sandbox id can kill the box and can never talk
// to it again.
func TestE2BTalksToTheBoxOnItsDerivedHost(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.frames = []any{startEvent(41), endEvent(0)}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	transport := &toStub{target: server.URL, seen: stub}
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}

	box, err := provider.Create(context.Background(), sandbox.Spec{CodingAgent: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Exec(context.Background(), "true", sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if !transport.sawHost("https://api.test.invalid") {
		t.Errorf("the control plane was not addressed as api.<domain>: %v", transport.hosts)
	}
	if !transport.sawHost("https://49983-sbx1-cl1.test.invalid") {
		t.Errorf("the box was not addressed on its own hostname: %v", transport.hosts)
	}

	// A BOX WITH NO CLIENT ID takes the short hostname. Building the long
	// form unconditionally produces `49983-sbx1-.domain`, which resolves
	// to nothing.
	stub.clientID = ""
	short, err := provider.Create(context.Background(), sandbox.Spec{CodingAgent: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := short.Exec(context.Background(), "true", sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if !transport.sawHost("https://49983-sbx1.test.invalid") {
		t.Errorf("a box with no client id got a hostname with a trailing "+
			"dash: %v", transport.hosts)
	}
}

// A DOMAIN WRITTEN AS A URL IS THE OBVIOUS MISTAKE, and taking it at face
// value produces "https://https://…" with nothing naming the cause.
func TestE2BNormalisesTheDomain(t *testing.T) {
	t.Parallel()
	for _, domain := range []string{
		"test.invalid", "https://test.invalid", "http://test.invalid", "test.invalid/",
	} {
		stub := newE2BStub()
		server := httptest.NewServer(stub)
		t.Cleanup(server.Close)
		transport := &toStub{target: server.URL, seen: stub}
		provider, err := sandbox.NewE2B(sandbox.E2BOptions{
			APIKey: "k", Domain: domain,
			HTTP: &http.Client{Transport: transport},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Create(context.Background(), sandbox.Spec{}); err != nil {
			t.Fatalf("%s: %v", domain, err)
		}
		if !transport.sawHost("https://api.test.invalid") {
			t.Errorf("domain %q addressed %v", domain, transport.hosts)
		}
	}
}

// A COMMAND IS FRAMED AS CONNECT AND RUN THROUGH A SHELL.
//
// Every caller hands over a shell line — pipes, redirects, `&&` — because
// that is what a setup step and a coding-agent invocation are. envd's API
// takes an argv, so the shell has to be named.
func TestE2BExecSendsAFramedShellCommand(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.frames = []any{
		startEvent(7),
		dataEvent("out-one ", ""),
		dataEvent("out-two", "warned"),
		endEvent(3),
	}
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := box.Exec(context.Background(), "make test && echo ok",
		sandbox.ExecOptions{Env: map[string]string{"CI": "1"}, Cwd: "/home/user/work"})
	if err != nil {
		t.Fatal(err)
	}
	// A NON-ZERO EXIT IS DATA, NOT AN ERROR. The liveness probe reads a
	// failed `kill -0` as "the process is gone", and setup turns a failed
	// provisioning command into a reportable failure — both need the code.
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want the command's own", res.ExitCode)
	}
	// THE FRAMES ARE CONCATENATED, not overwritten: output arrives in
	// chunks, and keeping only the last loses most of a build log.
	if res.Stdout != "out-one out-two" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "warned" {
		t.Errorf("stderr = %q", res.Stderr)
	}

	body := stub.body("/process.Process/Start")
	process, _ := body["process"].(map[string]any)
	if process["cmd"] != "/bin/bash" {
		t.Errorf("cmd = %v, want a shell — a bare argv cannot run `a && b`", process["cmd"])
	}
	args, _ := process["args"].([]any)
	if len(args) != 3 || args[2] != "make test && echo ok" {
		t.Errorf("args = %v", args)
	}
	if process["cwd"] != "/home/user/work" {
		t.Errorf("cwd = %v", process["cwd"])
	}
	envs, _ := process["envs"].(map[string]any)
	if envs["CI"] != "1" {
		t.Errorf("envs = %v", envs)
	}
	// envd HAS NO DEFAULT USER, and a call that names none fails with a
	// permission error naming nobody — which reads as a broken template.
	if got := stub.header("/process.Process/Start", "X-User"); got == "" {
		t.Error("the command named no user")
	}
	if got := stub.header("/process.Process/Start", "Connect-Protocol-Version"); got != "1" {
		t.Errorf("Connect-Protocol-Version = %q", got)
	}
}

// A DETACHED START RETURNS AS SOON AS THE PID IS KNOWN.
//
// This is the whole of "start and walk away": the process keeps running
// because envd owns it, not because anything here is still listening. Reading
// on would block the turn for the length of the coding run.
func TestE2BStartBackgroundReturnsAtTheFirstFrame(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.frames = []any{startEvent(4242)}
	// AND THEN GARBAGE. A real detached job sends no end frame for hours,
	// so "it returned" alone would pass for a client that read to EOF as
	// well as for one that stopped at the pid. An unreadable frame after
	// the pid separates them: only a client that kept reading sees it.
	stub.trailingGarbage = true
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}

	pid, err := box.StartBackground(context.Background(), "long-job", sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("the detached start read past the pid: %v", err)
	}
	if pid != "4242" {
		t.Fatalf("pid = %q", pid)
	}
}

// A STREAM THAT ENDS WITHOUT A VERDICT IS REPORTED, NOT CLAIMED AS SUCCESS.
//
// envd closes the connection when a box is paused or reclaimed mid-command.
// Returning exit 0 would have the caller treat an abandoned command as one
// that worked — and for a setup step, carry on into a box that is not
// provisioned.
func TestE2BExecWithNoVerdictIsAFailure(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.frames = []any{startEvent(9), dataEvent("half a log", "")}
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := box.Exec(context.Background(), "make", sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("a command whose stream died reported success")
	}
	if res.ExitCode == 0 {
		t.Errorf("exit = 0 on an abandoned command")
	}
	// WHAT WAS CAPTURED IS KEPT: it is the only evidence of how far the
	// command got.
	if res.Stdout != "half a log" {
		t.Errorf("the partial output was dropped: %q", res.Stdout)
	}
}

// A FRAME LENGTH PAST THE CAP IS A DESYNC, and allocating it is how a desync
// becomes an out-of-memory instead of an error.
func TestE2BRefusesAnImpossibleFrameLength(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sandboxes" {
			_, _ = w.Write([]byte(`{"sandboxID": "sbx1", "clientID": "cl1"}`))
			return
		}
		header := make([]byte, 5)
		binary.BigEndian.PutUint32(header[1:], 1<<30)
		_, _ = w.Write(header)
	}))
	t.Cleanup(server.Close)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: &toStub{target: server.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = box.Exec(context.Background(), "true", sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("a 1 GiB frame length was accepted")
	}
	// THE CAP IS WHAT MUST HAVE REFUSED IT, not the read that fails
	// afterwards anyway: without the cap the length is allocated first,
	// and a desync becomes an out-of-memory rather than an error.
	if !strings.Contains(err.Error(), "out of frame") {
		t.Errorf("the frame was allocated before it was refused: %v", err)
	}
}

// FILES ROUND-TRIP, and a missing one reads as empty.
//
// The detached runner polls for a done marker and a result file that do not
// exist until the job finishes, so "not there yet" is the ordinary answer on
// most calls — an error there would make every poll tick look like a failure.
func TestE2BFilesRoundTripAndMissingIsEmpty(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}

	if err := box.WriteFile(context.Background(),
		"/home/user/.crewlet/brief.md", []byte("do the thing")); err != nil {
		t.Fatal(err)
	}
	got, err := box.ReadFile(context.Background(), "/home/user/.crewlet/brief.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "do the thing" {
		t.Fatalf("read back %q", got)
	}

	missing, err := box.ReadFile(context.Background(), "/home/user/.crewlet/done")
	if err != nil {
		t.Fatalf("a missing file raised: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("a missing file returned %q", missing)
	}
}

// THE KEEPALIVE, THE SNAPSHOT AND THE TEARDOWN each hit their own endpoint.
func TestE2BLifecycleCallsAreDistinct(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}

	if err := box.SetTimeout(context.Background(), 600); err != nil {
		t.Fatal(err)
	}
	if !stub.saw("POST /sandboxes/sbx1/timeout") {
		t.Errorf("the keepalive hit %v", stub.requests)
	}
	if got := stub.body("/sandboxes/sbx1/timeout")["timeout"]; got != float64(600) {
		t.Errorf("timeout = %v", got)
	}
	// A NON-POSITIVE TTL IS A NO-OP rather than a call that sets zero,
	// which E2B reads as "reclaim now".
	stub.mu.Lock()
	stub.requests = nil
	stub.mu.Unlock()
	if err := box.SetTimeout(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if stub.saw("POST /sandboxes/sbx1/timeout") {
		t.Error("a zero TTL was sent, which reclaims the box immediately")
	}

	if err := box.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stub.saw("POST /sandboxes/sbx1/pause") {
		t.Errorf("pause hit %v", stub.requests)
	}
	if err := box.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stub.saw("DELETE /sandboxes/sbx1") {
		t.Errorf("close hit %v", stub.requests)
	}
}

// A BOX THAT IS ALREADY GONE IS A SUCCESSFUL KILL.
//
// The reaper runs against boxes it believes are orphaned, and most of the
// time something already reclaimed them. Raising would turn the ordinary case
// into an error on every tick.
func TestE2BKillingAGoneBoxSucceeds(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.killStatus = http.StatusNotFound
	provider, _ := newE2B(t, stub)

	if err := provider.Kill(context.Background(), "sbx-gone"); err != nil {
		t.Fatalf("killing an already-gone box raised: %v", err)
	}
	// AND A REAL REFUSAL STILL RAISES, or a permissions problem would read
	// as a successful reclaim and the box would bill for ever.
	stub.killStatus = http.StatusForbidden
	err := provider.Kill(context.Background(), "sbx1")
	if err == nil {
		t.Fatal("a refused kill was reported as success")
	}
	var apiErr *sandbox.E2BError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Errorf("the refusal is not typed: %v", err)
	}
}

// CONNECT IS ONE CALL, and "already running" is a success status rather than
// the conflict the deprecated /resume reported.
//
// The pair this replaced — GET the box, then POST /resume — was two round
// trips on the completion path and raced a reaper between them.
func TestE2BConnectWakesABoxAndToleratesARunningOne(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)

	// 200: the box was already running.
	box, err := provider.Connect(context.Background(), "sbx1")
	if err != nil {
		t.Fatalf("connecting to a running box failed: %v", err)
	}
	if box.ID() != "sbx1" {
		t.Fatalf("id = %q", box.ID())
	}
	if !stub.saw("POST /sandboxes/sbx1/connect") {
		t.Errorf("connect never called: %v", stub.requests)
	}
	// THE DEPRECATED ENDPOINT IS NOT CALLED AT ALL — not as a fallback, not
	// as a second leg. /resume cannot express "already running" as anything
	// but an error, which is what made the old path need a 409 workaround.
	if stub.saw("POST /sandboxes/sbx1/resume") {
		t.Errorf("the deprecated resume endpoint was called: %v", stub.requests)
	}
	// A FRESH TTL: a snapshot has no kill timer running, so a box woken
	// without one carries whatever remained of the timer it was paused with
	// — for a run parked overnight, nothing. The API requires the field.
	if got := stub.body("/sandboxes/sbx1/connect")["timeout"]; got == nil || got == float64(0) {
		t.Errorf("connect gave the box no TTL: %v", got)
	}

	// 201: the box was paused and was woken. Both are success.
	stub.connectStatus = http.StatusCreated
	if _, err := provider.Connect(context.Background(), "sbx1"); err != nil {
		t.Fatalf("a resumed box was reported as a failure: %v", err)
	}

	// AND A CONNECT THAT REALLY FAILED IS NOT SWALLOWED. Handing back a
	// handle to a box that is still paused makes every command on it fail
	// one layer further in, with envd's message rather than the control
	// plane's.
	stub.connectStatus = http.StatusInternalServerError
	if _, err := provider.Connect(context.Background(), "sbx1"); err == nil {
		t.Fatal("a failed connect handed back a handle to a paused box")
	}
}

// A BOX THE CONTROL PLANE NO LONGER HAS IS REPORTED AS GONE, so the waiter
// can fire completion rather than polling a dead box for ever.
func TestE2BConnectToAVanishedBoxFails(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "sandbox not found"}`))
	}))
	t.Cleanup(server.Close)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: &toStub{target: server.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Connect(context.Background(), "sbx-gone"); err == nil {
		t.Fatal("connecting to a vanished box succeeded")
	}
}

// A CREATE THAT NAMES NO SANDBOX IS THE WORST OUTCOME AVAILABLE: a VM may be
// running that nothing can address, so nothing can kill it either.
func TestE2BCreateWithoutAnIDIsRefused(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"clientID": "cl1"}`))
	}))
	t.Cleanup(server.Close)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: &toStub{target: server.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Create(context.Background(), sandbox.Spec{})
	if err == nil {
		t.Fatal("a create with no sandbox id was accepted")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("the error does not tell the operator to go looking: %v", err)
	}
}

// AN ENVD REFUSAL SURFACES rather than being read as an empty result.
func TestE2BAnEnvdRefusalIsAnError(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.startStatus = http.StatusInternalServerError
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Exec(context.Background(), "true", sandbox.ExecOptions{}); err == nil {
		t.Fatal("a 500 from envd was read as a successful command")
	}
}

// THE BACKEND NAMES ITSELF as the operator surface's value.
func TestE2BKindMatchesTheConfigValue(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	if provider.Kind() != "e2b" {
		t.Fatalf("Kind() = %q, which is not what providers.sandbox.type takes",
			provider.Kind())
	}
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	// EVERY E2B TEMPLATE RUNS AS ONE ACCOUNT WITH ONE HOME, unlike the
	// local backend where many boxes share a filesystem.
	if box.Home() != sandbox.DefaultHome {
		t.Errorf("home = %q", box.Home())
	}
}

// A LONG COMMAND IS NOT KILLED BY THE CONTROL PLANE'S CLOCK.
//
// http.Client.Timeout covers the whole exchange INCLUDING reading the body.
// Sharing one client between the two planes therefore capped every command at
// whatever bound was right for minting a VM — so a build that took longer
// than that died mid-stream, reported as a box that went away. What bounds a
// command is the gap BETWEEN frames, not the call.
func TestE2BACommandOutlivesTheControlPlaneTimeout(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.streamDelay = 250 * time.Millisecond
	stub.frames = []any{startEvent(1), dataEvent("still going", ""), endEvent(0)}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		// A DELIBERATELY TINY overall bound, standing in for the
		// control plane's: the command below runs longer than it.
		HTTP: &http.Client{
			Timeout:   100 * time.Millisecond,
			Transport: &toStub{target: server.URL},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := box.Exec(context.Background(), "slow-build", sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("a command outliving the control plane's timeout was killed: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "still going" {
		t.Errorf("result = %+v", res)
	}
}
