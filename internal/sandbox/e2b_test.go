package sandbox_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// What these tests protect: E2B is out of a test's reach, so they pin THE
// WIRE — every request shape, every header, and the framing of the streaming
// call. A misspelled field fails only against the real API, minutes into a
// launch, so assertions read "the request carries X", not "the method returns Y".

// e2bStub is a fake E2B answering both planes, so a whole create-exec-kill
// round trip runs in a test. In production the planes are different hosts
// (api.<domain>, <port>-<box>.<domain>); one server serves both here because
// the client reaches them through one round-tripper, and their paths are disjoint.
type e2bStub struct {
	mu sync.Mutex

	// requests is every path the client asked for, in order: the assertion
	// for a call whose only job is to be made.
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
	// domain is the domain a create, a connect and a read answer for the box,
	// as E2B does for a team on a cluster of its own; empty answers none.
	domain string
	// envdToken is the box's envd access token, answered by a create, a connect
	// and a read of the box, and required by envd on every request, as a secured
	// box's is. Empty is a box made without `secure`, whose envd requires nothing.
	envdToken string
	// refused counts the envd requests refused for their token.
	refused int
	// trafficToken is the box's traffic access token, answered by a create
	// whose request turned public traffic off, as E2B mints one, and again by
	// a connect; a read answers none, as E2B's SandboxDetail carries none.
	trafficToken string
	// answersTraffic is false for an API that answers no traffic token
	// whatever the request asked, as one below e2b-dev/infra 2025.46 does.
	answersTraffic bool
	// trafficSeen counts the requests, on any path, that carried a header
	// named e2b-traffic-access-token.
	trafficSeen int
	// state is what a read of the box reports: "running" when empty.
	state string
	// trailingGarbage appends an unreadable frame after the real ones, so
	// a client that should have stopped reading is caught doing so.
	trailingGarbage bool
	// streamDelay is how long the stub waits between frames, standing in
	// for a command that takes real time.
	streamDelay time.Duration
	// trailer is the payload of the stream's last frame; nil is the clean
	// close, `{}`.
	trailer any
	// ignoreRange serves every download whole with 200, standing in for an
	// envd that does not honour a Range request.
	ignoreRange bool
	// ranges is every Range header a download carried, in order.
	ranges []string

	// uploads is every path an upload wrote, in order; moves every Move's
	// source and destination; removes every path a Remove named.
	uploads []string
	moves   [][2]string
	removes []string
	// uploadStatus fails every upload with it once half its body is
	// written, as envd's does when the disk fills mid-write; moveStatus
	// fails every Move with it. Zero is success.
	uploadStatus int
	moveStatus   int
	// halfWritten, when set, is told of every upload once half its body is
	// written, and the upload then waits for its client to give up on it —
	// a caller whose context ended while the body was crossing.
	halfWritten chan string
}

// uploadGap is how long the stub's in-place upload holds the file empty, then
// half written: the network an upload's body crosses while envd writes it
// ([e2bStub.serveFiles]).
const uploadGap = time.Millisecond

func newE2BStub() *e2bStub {
	return &e2bStub{
		bodies:         map[string]map[string]any{},
		headers:        map[string]http.Header{},
		files:          map[string][]byte{},
		clientID:       "cl1",
		envdToken:      "envd-token-1",
		trafficToken:   "traffic-token-1",
		answersTraffic: true,
	}
}

// boxJSON is the sandbox the control plane answers, with extra fields.
func (s *e2bStub) boxJSON(extra string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	fields := ""
	if s.envdToken != "" {
		fields = `, "envdAccessToken": "` + s.envdToken + `"`
	}
	if s.domain != "" {
		fields += `, "domain": "` + s.domain + `"`
	}
	return `{"sandboxID": "sbx1", "clientID": "` + s.clientID +
		`", "templateID": "claude", "envdVersion": "0.1.0"` + fields + extra + `}`
}

// envdAdmits is a secured envd's own check (WithAuthorization in
// packages/envd/internal/api/auth.go, e2b-dev/infra): with a token set, a
// request without it is refused 401. A /files call may present a signature
// instead, which this client never does, so here the header is the rule.
func (s *e2bStub) envdAdmits(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.envdToken == "" || r.Header.Get("X-Access-Token") == s.envdToken {
		return true
	}
	s.refused++
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized access, please provide a valid ` +
		`access token or method signing if supported"}`))
	return false
}

func (s *e2bStub) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.headers[r.URL.Path] = r.Header.Clone()
	if _, carried := r.Header[http.CanonicalHeaderKey("e2b-traffic-access-token")]; carried {
		s.trafficSeen++
	}
}

// trafficFor is what a create or a connect answers beside the box: its traffic
// token, where the API mints one.
func (s *e2bStub) trafficFor(private bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !private || !s.answersTraffic {
		return ""
	}
	return `, "trafficAccessToken": "` + s.trafficToken + `"`
}

func (s *e2bStub) saw(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.requests, want)
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
		// AS A CLUSTER FROM BEFORE E2B SECURED EVERY BOX ITSELF CREATES (e2b-dev/infra
		// 315a94e737; sandbox_create.go): an envd token is minted only for
		// `secure: true`, and an unsecured box's envd requires nothing, so a
		// client that stopped asking is caught. A traffic
		// token is minted only for a box whose request turned public traffic off
		// (create_instance.go), so a client that stopped asking is refused its box.
		s.mu.Lock()
		body := s.bodies[r.URL.Path]
		if body["secure"] != true {
			s.envdToken = ""
		}
		network, _ := body["network"].(map[string]any)
		allow, set := network["allowPublicTraffic"]
		s.mu.Unlock()
		_, _ = w.Write([]byte(s.boxJSON(s.trafficFor(set && allow == false))))

	case strings.HasSuffix(r.URL.Path, "/timeout"),
		strings.HasSuffix(r.URL.Path, "/pause"):
		s.capture(r)
		w.WriteHeader(http.StatusNoContent)

	// /connect answers 200 for a running box and 201 for a woken one, with the
	// sandbox either way, so the client needs no second call.
	case strings.HasSuffix(r.URL.Path, "/connect"):
		s.capture(r)
		if s.connectStatus >= 400 {
			w.WriteHeader(s.connectStatus)
			return
		}
		// 201 carries the sandbox too: a woken box is returned like a running one.
		if s.connectStatus != 0 {
			w.WriteHeader(s.connectStatus)
		}
		_, _ = w.Write([]byte(s.boxJSON(s.trafficFor(true))))

	// A read of one box: E2B's SandboxDetail, its state among it.
	case strings.HasPrefix(r.URL.Path, "/sandboxes/") && r.Method == http.MethodGet:
		s.mu.Lock()
		state := s.state
		s.mu.Unlock()
		if state == "" {
			state = "running"
		}
		_, _ = w.Write([]byte(s.boxJSON(`, "state": "` + state + `"`)))

	case strings.HasPrefix(r.URL.Path, "/sandboxes/") && r.Method == http.MethodDelete:
		if s.killStatus != 0 {
			w.WriteHeader(s.killStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case r.URL.Path == "/process.Process/Start":
		if !s.envdAdmits(w, r) {
			return
		}
		if s.startStatus != 0 {
			w.WriteHeader(s.startStatus)
			_, _ = w.Write([]byte(`envd said no`))
			return
		}
		s.captureStart(r)
		w.Header().Set("Content-Type", "application/connect+json")
		for _, frame := range s.frames {
			if s.streamDelay > 0 {
				// Flushed, so the client sees each frame as it lands and the gap
				// between them is real.
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				time.Sleep(s.streamDelay) //fixedwait:simulated a command that takes real time, its frames streamDelay apart
			}
			_, _ = w.Write(connectFrame(0, frame))
		}
		if s.trailingGarbage {
			_, _ = w.Write([]byte{0, 0, 0, 0, 8, 'n', 'o', 't', ' ', 'j', 's', 'o', 'n'})
			return
		}
		trailer := s.trailer
		if trailer == nil {
			trailer = map[string]any{}
		}
		_, _ = w.Write(connectFrame(0x02, trailer))

	case r.URL.Path == "/files":
		if !s.envdAdmits(w, r) {
			return
		}
		s.serveFiles(w, r)

	case r.URL.Path == "/filesystem.Filesystem/Move", r.URL.Path == "/filesystem.Filesystem/Remove":
		if !s.envdAdmits(w, r) {
			return
		}
		s.serveFilesystem(w, r)

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

// captureStart un-frames the Connect request, to read what a command asked
// envd to run.
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
		if r.Header.Get("Range") != "" {
			s.ranges = append(s.ranges, r.Header.Get("Range"))
		}
		s.mu.Unlock()
		if !held {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if s.ignoreRange {
			_, _ = w.Write(content)
			return
		}
		// THROUGH ServeContent, as envd serves /files
		// (packages/envd/internal/api/download.go, e2b-dev/infra), so a Range
		// request gets 206 and Content-Range from the code the real box runs.
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(content))
	case http.MethodPost:
		form, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := form.NextPart()
		if err != nil || part.FormName() != "file" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		content, err := io.ReadAll(part)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.uploads = append(s.uploads, path)
		failWith := s.uploadStatus
		s.mu.Unlock()
		// IN PLACE, AS ENVD WRITES AN UPLOAD: it opens the path O_TRUNC and
		// copies the body in as it arrives (processFile in
		// packages/envd/internal/api/upload.go, e2b-dev/infra), so a read
		// meanwhile finds the file empty, then half written.
		s.hold(path, nil)
		s.hold(path, content[:len(content)/2])
		s.mu.Lock()
		halfWritten := s.halfWritten
		s.mu.Unlock()
		if halfWritten != nil {
			halfWritten <- path
			<-r.Context().Done()
			return
		}
		if failWith != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failWith)
			_, _ = w.Write([]byte(`{"code":507,"message":"not enough disk space available"}`))
			return
		}
		s.mu.Lock()
		s.files[path] = content
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

// hold puts content at path, where a read finds it, for [uploadGap]: one
// state an upload envd writes in place passes through.
func (s *e2bStub) hold(path string, content []byte) {
	s.mu.Lock()
	s.files[path] = slices.Clone(content)
	s.mu.Unlock()
	time.Sleep(uploadGap) //fixedwait:simulated the network an upload's body crosses while envd writes it in place
}

// serveFilesystem answers envd's filesystem Move and Remove as Connect unary
// calls in JSON, as envd does: a rename over the destination (os.Rename in
// packages/envd/internal/services/filesystem/move.go) and a removal that
// tolerates a missing path (os.RemoveAll in remove.go). An error is Connect's:
// its code and message in a JSON body.
func (s *e2bStub) serveFilesystem(w http.ResponseWriter, r *http.Request) {
	var call map[string]string
	if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
		connectRefusal(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[r.URL.Path] = map[string]any{}
	for key, value := range call {
		s.bodies[r.URL.Path][key] = value
	}
	if strings.HasSuffix(r.URL.Path, "/Remove") {
		s.removes = append(s.removes, call["path"])
		delete(s.files, call["path"])
		_, _ = w.Write([]byte(`{}`))
		return
	}
	s.moves = append(s.moves, [2]string{call["source"], call["destination"]})
	content, held := s.files[call["source"]]
	switch {
	case s.moveStatus != 0:
		connectRefusal(w, s.moveStatus, "internal", "error renaming: rename: is a directory")
	case !held:
		connectRefusal(w, http.StatusNotFound, "not_found", "source file not found")
	default:
		s.files[call["destination"]] = content
		delete(s.files, call["source"])
		_, _ = w.Write([]byte(`{"entry":{"name":"x","type":"FILE_TYPE_FILE"}}`))
	}
}

// connectRefusal writes a Connect error the way a unary call's is written.
func connectRefusal(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	blob, _ := json.Marshal(map[string]string{"code": code, "message": message})
	_, _ = w.Write(blob)
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

// signalEnd is the end event envd sends for a process a signal ended, as
// os.ProcessState reports one: no exit code, and the signal in its status and
// its error alike. `exited` is false, which protobuf's JSON leaves out.
func signalEnd(status string) any {
	return map[string]any{"event": map[string]any{
		"end": map[string]any{"exitCode": -1, "status": status, "error": status}}}
}

// trailerError is the last frame of a stream envd ended with an error.
func trailerError(code, message string) any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}

// newE2B points a provider at the stub through the round-tripper, not a
// base-URL option: the two planes' addresses are derived from the domain, and
// that derivation is under test.
func newE2B(t *testing.T, stub *e2bStub) (*sandbox.E2BProvider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "e2b_secret", Domain: "test.invalid",
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider, server
}

// A KEYLESS PROVIDER IS REFUSED AT CONSTRUCTION, so an apply fails rather
// than every launch: the API authenticates every call, a self-hosted
// cluster's included.
func TestE2BRefusesToBuildWithoutAKey(t *testing.T) {
	t.Parallel()
	if _, err := sandbox.NewE2B(sandbox.E2BOptions{Domain: "cluster.example"}); err == nil {
		t.Fatal("a keyless provider was built, so every create would 401")
	}
}

// THE CONTROL PLANE CARRIES THE KEY AS X-API-KEY: E2B accepts a bearer only
// for a user-scoped access token, and refuses the dashboard's key under it.
func TestE2BCreateSendsTheDocumentedRequest(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)

	box, err := provider.Create(context.Background(), sandbox.Spec{
		Harness:    "claude-code",
		TimeoutSec: 900,
		Env:        map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
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
	// THE TEMPLATE IS THE HARNESS'S: a bare box has no harness, so the run
	// fails at its first command with "command not found" — a template
	// problem that reads as a broken runner.
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
	// SECURED: unasked, a cluster from before E2B secured every box itself
	// makes a box whose envd requires no credential.
	if body["secure"] != true {
		t.Errorf("secure = %v; want a box created with secured envd access", body["secure"])
	}
}

// everyEnvdCall makes every kind of envd call (a command, one detached, a file
// written whole and streamed, and one read whole, from its end and streamed
// both ways) on a box reached by a create, a connect and a read, through a
// transport of its own, which it returns for the addresses the client dialled.
func everyEnvdCall(t *testing.T, stub *e2bStub) *httpxtest.Rewriter {
	t.Helper()
	stub.frames = []any{startEvent(7), dataEvent("ok", ""), endEvent(0)}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	transport := httpxtest.Rewrite(t, server)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	created, err := provider.Create(ctx, sandbox.Spec{Harness: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := provider.Connect(ctx, "sbx1")
	if err != nil {
		t.Fatal(err)
	}
	attached, err := provider.Attach(ctx, "sbx1")
	if err != nil {
		t.Fatal(err)
	}
	read := func(r io.ReadCloser, err error) error {
		if err == nil {
			_, err = io.ReadAll(r)
			_ = r.Close()
		}
		return err
	}
	for name, box := range map[string]sandbox.Sandbox{"created": created, "connected": connected, "attached": attached} {
		if err := box.WriteFile(ctx, "/home/user/.crewlet/done", []byte("0\n")); err != nil {
			t.Errorf("%s: WriteFile: %v", name, err)
		}
		if err := box.WriteStream(ctx, "/home/user/.crewlet/streamed", strings.NewReader("1\n")); err != nil {
			t.Errorf("%s: WriteStream: %v", name, err)
		}
		if got, err := box.ReadFile(ctx, "/home/user/.crewlet/done"); err != nil || string(got) != "0\n" {
			t.Errorf("%s: ReadFile = %q, %v", name, got, err)
		}
		if _, err := box.ReadTail(ctx, "/home/user/.crewlet/done", 1); err != nil {
			t.Errorf("%s: ReadTail: %v", name, err)
		}
		if err := read(box.OpenFile(ctx, "/home/user/.crewlet/done")); err != nil {
			t.Errorf("%s: OpenFile: %v", name, err)
		}
		tail, _, err := box.OpenTail(ctx, "/home/user/.crewlet/done", 1)
		if err = read(tail, err); err != nil {
			t.Errorf("%s: OpenTail: %v", name, err)
		}
		if res, err := box.Exec(ctx, "true", sandbox.ExecOptions{}); err != nil || res.ExitCode != 0 {
			t.Errorf("%s: Exec = %+v, %v", name, res, err)
		}
		if _, err := box.StartBackground(ctx, "sleep 1", sandbox.ExecOptions{}); err != nil {
			t.Errorf("%s: StartBackground: %v", name, err)
		}
	}
	return transport
}

// EVERY ENVD REQUEST CARRIES THE BOX'S ACCESS TOKEN, each call
// [everyEnvdCall] makes. The stub refuses each without it, as a secured envd
// does.
//
// Mutation: drop the header, and every call here is refused 401.
func TestE2BSendsTheBoxsAccessTokenOnEveryEnvdRequest(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	everyEnvdCall(t, stub)
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.refused != 0 {
		t.Errorf("envd refused %d requests for their token", stub.refused)
	}
}

// THE ENGINE SENDS NO TRAFFIC TOKEN, AND REACHES NOTHING ONE GUARDS: it dials
// a box only on envd's port, which E2B exempts, so a private box costs it
// nothing. Each call [everyEnvdCall] makes, on a box whose create and connect
// answered a token, carries none, and every host dialled is the control plane
// or envd. Nor does it hold one: a create's answer is read for the token's
// presence alone (TestATrafficTokenIsReadAsPresentAndNeverKept).
//
// Mutation: send the header on envd's calls, or reach a box on any port but
// envd's, and this goes red.
func TestTheEngineSendsNoTrafficTokenAndReachesOnlyEnvd(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	transport := everyEnvdCall(t, stub)
	stub.mu.Lock()
	seen := stub.trafficSeen
	stub.mu.Unlock()
	if seen != 0 {
		t.Errorf("%d requests carried a traffic token", seen)
	}
	for _, host := range transport.Hosts() {
		if host != "https://api.test.invalid" && !strings.HasPrefix(host, "https://49983-") {
			t.Errorf("the engine dialled %s, a port of the box that its traffic token guards", host)
		}
	}
}

// EVERY BOX IS CREATED WITH PUBLIC TRAFFIC OFF, said outright: a false left
// out is the project's default, which on a project older than E2B's private
// default is public. Nothing else in `network` is set, the host mask among it:
// it rewrites the Host header a box's service sees, which nothing here needs.
//
// Mutation: drop the field, tag it omitempty, or set maskRequestHost, and this
// goes red.
func TestEveryE2BBoxIsCreatedWithPublicTrafficOff(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	_, err := provider.Create(context.Background(), sandbox.Spec{Harness: "claude-code"})

	body := stub.body("/sandboxes")
	network, ok := body["network"].(map[string]any)
	if !ok {
		t.Fatalf("network = %v; want an object", body["network"])
	}
	if allow, sent := network["allowPublicTraffic"]; !sent || allow != false {
		t.Errorf("allowPublicTraffic = %v (sent: %v); want false, sent", allow, sent)
	}
	for key := range network {
		if key != "allowPublicTraffic" {
			t.Errorf("network carries %q, which nothing here sets: %v", key, network)
		}
	}
	if err != nil {
		t.Fatalf("a create that asked for a private box failed: %v", err)
	}
}

// A CREATE ANSWERED WITH NO TRAFFIC TOKEN IS KILLED AND REFUSED: E2B mints one
// for every box made private, so its box came from an API that ignored the
// request and is public. It is killed and the launch refused as configuration,
// naming the domain and never quoting the box's envd token: a cluster is told
// the release that mends it, and E2B's cloud, which no setting mends, is told
// so and nothing of a cluster.
//
// Mutation: drop the check (the box is handed out), skip the kill (no DELETE),
// give the cloud a cluster's remedy, or wrap the kill's failure outside the
// mark (the subtest reads provider_unavailable), and this goes red.
func TestACreateAnsweredWithNoTrafficTokenIsKilledAndRefused(t *testing.T) {
	t.Parallel()
	for _, deployment := range []struct {
		name, domain string
		says, never  []string
	}{
		{"a self-hosted cluster", "test.invalid",
			[]string{"api.test.invalid", "sbx1", "2025.46", "providers.sandbox.e2b.domain"},
			[]string{"report it to E2B"}},
		{"E2B's cloud", "",
			[]string{"api.e2b.dev", "sbx1", "no setting", "report it to E2B", "run_in"},
			[]string{"2025.46", "cluster", "providers.sandbox.e2b.domain"}},
	} {
		t.Run(deployment.name, func(t *testing.T) {
			t.Parallel()
			stub := newE2BStub()
			stub.answersTraffic = false
			server := httptest.NewServer(stub)
			t.Cleanup(server.Close)
			provider, err := sandbox.NewE2B(sandbox.E2BOptions{
				APIKey: "k", Domain: deployment.domain,
				HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
			})
			if err != nil {
				t.Fatal(err)
			}
			box, err := provider.Create(context.Background(), sandbox.Spec{})
			if err == nil {
				t.Fatalf("a box E2B made public was handed out: %s", box.ID())
			}
			if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
				t.Errorf("a box made public is classed %q (%v), want %q: %v", why, ok, sandbox.WhyConfiguration, err)
			}
			if !stub.saw("DELETE /sandboxes/sbx1") {
				t.Errorf("the public box was left running: %v", err)
			}
			msg := err.Error()
			for _, want := range deployment.says {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not name %q: %s", want, msg)
				}
			}
			for _, wrong := range deployment.never {
				if strings.Contains(msg, wrong) {
					t.Errorf("the refusal says %q, which does not apply here: %s", wrong, msg)
				}
			}
			if strings.Contains(msg, "envd-token") {
				t.Errorf("the refusal quotes the box's envd token: %s", msg)
			}
		})
	}

	// A KILL THAT FAILS IS TOLD, with the box to look for, and the launch is
	// still configuration: the kill's own 500 is no outage a wait clears here.
	t.Run("a kill that fails", func(t *testing.T) {
		t.Parallel()
		stub := newE2BStub()
		stub.answersTraffic = false
		stub.killStatus = http.StatusInternalServerError
		provider, _ := newE2B(t, stub)
		_, err := provider.Create(context.Background(), sandbox.Spec{})
		var killed *sandbox.E2BError
		if !errors.As(err, &killed) || killed.Method != http.MethodDelete || killed.Status != http.StatusInternalServerError {
			t.Errorf("the refusal does not carry the kill's failure: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "sbx1") || !strings.Contains(err.Error(), "may still be running") {
			t.Errorf("the refusal does not send the operator to the box left running: %v", err)
		}
		if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
			t.Errorf("a box made public whose kill failed is classed %q (%v), want %q: %v", why, ok,
				sandbox.WhyConfiguration, err)
		}
	})
}

// A PUBLIC BOX IS KILLED THOUGH ITS CALLER GAVE UP as the create answered (a
// drain, a lost lease, a stopped turn): the kill is the create's own rollback,
// and one sent under the caller's context would never leave, so the public box
// would run until its TTL. Here the caller's context ends as the create's
// answer is closed, once it is read and before the box is refused.
//
// Mutation: kill under the create's own context, and the DELETE is never sent.
func TestAPublicBoxIsKilledThoughItsCallerGaveUp(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.answersTraffic = false
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: givesUpAtTheAnswer{httpxtest.Rewrite(t, server), cancel}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Create(ctx, sandbox.Spec{})
	if ctx.Err() == nil {
		t.Fatalf("the caller's context outlived the create's answer, so nothing here gave up: %v", err)
	}
	if !stub.saw("DELETE /sandboxes/sbx1") {
		t.Errorf("the caller gave up as the create answered, and the public box was left running: %v", err)
	}
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("a public box whose caller gave up is classed %q (%v), want %q: %v", why, ok,
			sandbox.WhyConfiguration, err)
	}
}

// givesUpAtTheAnswer ends its caller's context as the create's answer is
// closed: a caller that gave up just as E2B answered.
type givesUpAtTheAnswer struct {
	http.RoundTripper
	cancel context.CancelFunc
}

func (g givesUpAtTheAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := g.RoundTripper.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
		resp.Body = cancelOnClose{resp.Body, g.cancel}
	}
	return resp, err
}

// cancelOnClose is a response body whose Close ends a context.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// THE TEMPLATE FALLS BACK IN ORDER: the company's, then the harness's own.
// There is no per-run template: sizing is a property of the template, which
// is configured on the backend.
func TestE2BPicksATemplateInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, agent, want string
	}{
		{"the company's wins", "company-box", "claude-code", "company-box"},
		{"then the agent's own", "", "claude-code", "claude"},
		{"opencode has its own", "", "opencode", "opencode"},
		// An agent this build does not know gets E2B's bare image, and the run
		// fails: inventing a template would hide a config naming an agent
		// nothing here can run behind a runtime error further in.
		{"an unknown agent gets the bare image", "", "cursor", "base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newE2BStub()
			server := httptest.NewServer(stub)
			t.Cleanup(server.Close)
			provider, err := sandbox.NewE2B(sandbox.E2BOptions{
				APIKey: "k", Domain: "test.invalid", Template: tc.company,
				HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Create(context.Background(), sandbox.Spec{
				Harness: tc.agent,
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
// client id and the domain. The client id is not bookkeeping: a reconnect
// that kept only the sandbox id could kill the box but never talk to it.
func TestE2BTalksToTheBoxOnItsDerivedHost(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.frames = []any{startEvent(41), endEvent(0)}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	transport := httpxtest.Rewrite(t, server)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}

	box, err := provider.Create(context.Background(), sandbox.Spec{Harness: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Exec(context.Background(), "true", sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if !transport.SawHost("https://api.test.invalid") {
		t.Errorf("the control plane was not addressed as api.<domain>: %v", transport.Hosts())
	}
	if !transport.SawHost("https://49983-sbx1-cl1.test.invalid") {
		t.Errorf("the box was not addressed on its own hostname: %v", transport.Hosts())
	}

	// A BOX WITH NO CLIENT ID takes the short hostname: the long form would
	// be `49983-sbx1-.domain`, which resolves to nothing.
	stub.clientID = ""
	short, err := provider.Create(context.Background(), sandbox.Spec{Harness: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := short.Exec(context.Background(), "true", sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if !transport.SawHost("https://49983-sbx1.test.invalid") {
		t.Errorf("a box with no client id got a hostname with a trailing "+
			"dash: %v", transport.Hosts())
	}
}

// A BOX IS REACHED AT THE DOMAIN E2B ANSWERS FOR IT: a team on a cluster of its
// own has its boxes served there, answered on a create, a connect and a read
// alike, and a box addressed on the deployment's domain answers no envd call.
// The control plane stays the deployment's.
//
// Mutation: build the host on the deployment's domain alone, and every handle
// here dials test.invalid.
func TestE2BReachesABoxAtTheDomainE2BAnswersForIt(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.domain = "cluster.example"
	stub.frames = []any{startEvent(41), endEvent(0)}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	transport := httpxtest.Rewrite(t, server)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, reached := range []struct {
		how string
		box func() (sandbox.Sandbox, error)
	}{
		{"created", func() (sandbox.Sandbox, error) { return provider.Create(ctx, sandbox.Spec{Harness: "opencode"}) }},
		{"connected", func() (sandbox.Sandbox, error) { return provider.Connect(ctx, "sbx1") }},
		{"attached", func() (sandbox.Sandbox, error) { return provider.Attach(ctx, "sbx1") }},
	} {
		box, err := reached.box()
		if err != nil {
			t.Fatalf("%s: %v", reached.how, err)
		}
		before := len(transport.Hosts())
		if _, err := box.Exec(ctx, "true", sandbox.ExecOptions{}); err != nil {
			t.Fatalf("%s: Exec: %v", reached.how, err)
		}
		dialled := transport.Hosts()[before:]
		if len(dialled) == 0 || slices.ContainsFunc(dialled, func(host string) bool {
			return host != "https://49983-sbx1-cl1.cluster.example"
		}) {
			t.Errorf("the %s box's command dialled %v; want its envd on the domain E2B answered for it",
				reached.how, dialled)
		}
	}
	for _, host := range transport.Hosts() {
		if !strings.HasPrefix(host, "https://49983-") && host != "https://api.test.invalid" {
			t.Errorf("the control plane was addressed at %s; want the deployment's api.test.invalid", host)
		}
	}
}

// A DOMAIN WRITTEN AS A URL IS THE OBVIOUS MISTAKE, and taken at face value
// it produces "https://https://…" with nothing naming the cause.
func TestE2BNormalisesTheDomain(t *testing.T) {
	t.Parallel()
	for _, domain := range []string{
		"test.invalid", "https://test.invalid", "http://test.invalid", "test.invalid/",
	} {
		stub := newE2BStub()
		server := httptest.NewServer(stub)
		t.Cleanup(server.Close)
		transport := httpxtest.Rewrite(t, server)
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
		if !transport.SawHost("https://api.test.invalid") {
			t.Errorf("domain %q addressed %v", domain, transport.Hosts())
		}
	}
}

// A COMMAND IS FRAMED AS CONNECT AND RUN THROUGH A SHELL: every caller hands
// over a shell line (pipes, redirects, `&&`), and envd's API takes an argv,
// so the shell has to be named.
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

// A DETACHED START RETURNS AS SOON AS THE PID IS KNOWN: envd owns the
// process, so reading on would only block the turn for the length of the job.
func TestE2BStartBackgroundReturnsAtTheFirstFrame(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.frames = []any{startEvent(4242)}
	// AND THEN GARBAGE: a detached job sends no end frame for hours, so "it
	// returned" would also pass a client that read to EOF. Only a client
	// that kept reading past the pid reaches the unreadable frame.
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
// envd closes the connection when a box is paused or reclaimed mid-command;
// exit 0 would have a setup step carry on into a box that is not provisioned.
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
	if !strings.Contains(err.Error(), "went away") {
		t.Errorf("error = %q, want it to say the box went away", err)
	}
	// WHAT WAS CAPTURED IS KEPT: it is the only evidence of how far the
	// command got.
	if res.Stdout != "half a log" {
		t.Errorf("the partial output was dropped: %q", res.Stdout)
	}
}

// A COMMAND A SIGNAL ENDED REPORTS 128 PLUS THE SIGNAL, AS A SHELL DOES.
// When a signal ends the shell envd waits on (`kill -9 $$`, or an OOM kill of
// the program it exec'd into), the end event carries exit code -1 and the
// signal in its status. Read as a missing verdict, a live box was reported
// gone and a setup step as having no exit status. Each case is a status envd
// writes; mutate the signal reading out and every one goes red.
func TestE2BExecReadsASignalAsAShellReportsOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status string
		exit   int
		named  string
	}{
		{"signal: killed", 137, "killed by signal 9 (killed)"},
		{"signal: terminated", 143, "killed by signal 15 (terminated)"},
		{"signal: segmentation fault (core dumped)", 139, "killed by signal 11 (segmentation fault)"},
		// Go's own spelling of a signal it has no description for.
		{"signal: signal 34", 162, "killed by signal 34 (signal 34)"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			stub := newE2BStub()
			stub.frames = []any{startEvent(9), dataEvent("", "installing\n"), signalEnd(tc.status)}
			provider, _ := newE2B(t, stub)
			box, err := provider.Create(context.Background(), sandbox.Spec{})
			if err != nil {
				t.Fatal(err)
			}
			res, err := box.Exec(context.Background(), "npm ci", sandbox.ExecOptions{})
			if err != nil {
				t.Fatalf("a signalled command was reported as no verdict: %v", err)
			}
			if res.ExitCode != tc.exit {
				t.Errorf("exit = %d, want %d", res.ExitCode, tc.exit)
			}
			if want := "installing\n" + tc.named; res.Stderr != want {
				t.Errorf("stderr = %q, want what it printed and then the signal: %q", res.Stderr, want)
			}

			// AND A SETUP STEP SAYS SO: the status, not "no exit status".
			err = sandbox.ApplySetup(context.Background(), box,
				[]sandbox.SetupStep{{Name: "deps", Commands: []string{"npm ci"}}}, nil)
			var setup *sandbox.SetupError
			if !errors.As(err, &setup) || !setup.Exited || setup.Exit != tc.exit {
				t.Errorf("setup = %v, want command #1 failed (exit %d)", err, tc.exit)
			}
		})
	}
}

// A COMMAND THAT OUTLIVED ITS TIMEOUT REPORTS 124, AS A LOCAL BOX DOES.
// timeout_seconds travels as Connect-Timeout-Ms, and envd ends the stream with
// `deadline_exceeded`, usually before any end event. Read as a missing
// verdict, it sent the operator to the box rather than to the step's timeout.
func TestE2BExecReadsATimeoutAsALocalBoxReportsOne(t *testing.T) {
	t.Parallel()
	cases := map[string][]any{
		"the stream closed before any end event": {startEvent(9), dataEvent("", "pulling image\n")},
		"an end event reporting the kill beside it": {
			startEvent(9), dataEvent("", "pulling image\n"), signalEnd("signal: killed"),
		},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newE2BStub()
			stub.frames = frames
			stub.trailer = trailerError("deadline_exceeded", "context deadline exceeded")
			provider, _ := newE2B(t, stub)
			box, err := provider.Create(context.Background(), sandbox.Spec{})
			if err != nil {
				t.Fatal(err)
			}
			res, err := box.Exec(context.Background(), "docker pull x", sandbox.ExecOptions{TimeoutSec: 5})
			if err != nil {
				t.Fatalf("a command that ran out its timeout was reported as no verdict: %v", err)
			}
			if res.ExitCode != 124 {
				t.Errorf("exit = %d, want 124", res.ExitCode)
			}
			if want := "timed out after 5s\npulling image"; res.Stderr != want {
				t.Errorf("stderr = %q, want the timeout and then what it printed: %q", res.Stderr, want)
			}
		})
	}
}

// A STREAM ENVD ENDED WITH ANOTHER ERROR SAYS WHICH, rather than claiming the
// box went away: the agent answered, and its code is the account of what
// happened.
func TestE2BExecNamesTheErrorAStreamEndedWith(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.frames = []any{startEvent(9)}
	stub.trailer = trailerError("unavailable", "the box is pausing")
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := box.Exec(context.Background(), "make", sandbox.ExecOptions{})
	if err == nil || res.ExitCode == 0 {
		t.Fatalf("a stream that ended on an error reported success: %+v", res)
	}
	if msg := err.Error(); !strings.Contains(msg, "unavailable") ||
		!strings.Contains(msg, "the box is pausing") || strings.Contains(msg, "went away") {
		t.Errorf("error = %q, want envd's own code and message", msg)
	}
}

// A FRAME LENGTH PAST THE CAP IS A DESYNC, and allocating it is how a desync
// becomes an out-of-memory instead of an error.
func TestE2BRefusesAnImpossibleFrameLength(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sandboxes" {
			_, _ = w.Write([]byte(`{"sandboxID": "sbx1", "clientID": "cl1", "trafficAccessToken": "t"}`))
			return
		}
		header := make([]byte, 5)
		binary.BigEndian.PutUint32(header[1:], 1<<30)
		_, _ = w.Write(header)
	}))
	t.Cleanup(server.Close)
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
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
	// afterwards anyway: uncapped, the length is allocated first and a desync
	// becomes an out-of-memory.
	if !strings.Contains(err.Error(), "out of frame") {
		t.Errorf("the frame was allocated before it was refused: %v", err)
	}
}

// FILES ROUND-TRIP, and a missing one reads as empty: the detached runner
// polls for a done marker and a result file that exist only once the job
// finishes, so an error there would make every poll tick a failure.
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

// A FILE'S PATH MEANS IN AN E2B BOX WHAT IT MEANS IN EVERY OTHER CELL. envd
// joins a relative or `~/` path onto the home and takes an absolute one
// verbatim, so `/home/user/.npmrc/` sent as written makes `.npmrc` a
// directory. What reaches envd is the cleaned path, a `~/` form cleaned only
// after its tilde, which envd expands.
func TestE2BWritesAFileAtItsCleanedPath(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	for written, sent := range map[string]string{
		"/home/user/.npmrc/":         "/home/user/.npmrc",
		"/home/user/./tools/../.pip": "/home/user/.pip",
		"~/.config/tool.toml/":       "~/.config/tool.toml",
		// Cleaned AFTER the tilde, never across it: a lexical clean of the
		// whole path drops `~` as an ordinary element, and envd would join
		// `user/.npmrc` onto the home rather than resolve `..` above it.
		"~/../user/.npmrc": "~/../user/.npmrc",
		"~/a/../../x":      "~/../x",
		// A relative path goes as written: cleaned here, this one would
		// become the `~/x` envd expands to the home, while every other cell
		// and validation place it at <home>/~/x.
		"a/../~/x": "a/../~/x",
	} {
		if err := box.WriteFile(context.Background(), written, []byte(written)); err != nil {
			t.Errorf("WriteFile(%q): %v", written, err)
			continue
		}
		stub.mu.Lock()
		got, held := stub.files[sent]
		stub.mu.Unlock()
		if !held || string(got) != written {
			t.Errorf("WriteFile(%q) did not reach envd as %q", written, sent)
		}
	}
}

// A FILE IS REPLACED, NEVER UPLOADED OVER. envd writes an upload in place
// (truncate, then fill), so a file a run is reading — the token file the poll
// keeps current — would read empty or part written meanwhile. The content is
// uploaded under a fresh name beside the path and moved over it by envd's
// rename, as the account every file call names, with the box's token; nothing
// staged is left.
//
// Mutation: upload to the path itself, and the path is an upload's destination.
func TestE2BReplacesAFileItNeverUploadsOver(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/home/user/.config/crewlet/github-token"
	for _, token := range []string{"ghs_first", "ghs_renewed"} {
		if err := box.WriteFile(t.Context(), path, []byte(token)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if got, err := box.ReadFile(t.Context(), path); err != nil || string(got) != token {
			t.Fatalf("the path holds %q, %v; want %q", got, err, token)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.uploads) != 2 || len(stub.moves) != 2 {
		t.Fatalf("two writes made %d uploads and %d moves; want one of each apiece", len(stub.uploads),
			len(stub.moves))
	}
	for i, upload := range stub.uploads {
		if upload == path {
			t.Errorf("write %d uploaded over the path itself, which envd writes in place", i)
		}
		if !strings.HasPrefix(upload, "/home/user/.config/crewlet/.crewlet-tmp-") {
			t.Errorf("write %d staged its content at %s, not under a fresh name beside the path", i, upload)
		}
		if move := stub.moves[i]; move != [2]string{upload, path} {
			t.Errorf("write %d moved %v; want what it staged moved over the path", i, move)
		}
	}
	if stub.uploads[0] == stub.uploads[1] {
		t.Errorf("both writes staged at %s; want a fresh name each", stub.uploads[0])
	}
	// AS THE ACCOUNT THE UPLOAD WROTE AS: envd resolves a relative or `~/`
	// path in the home of the user a Connect call authenticates as.
	if got, want := stub.headers["/filesystem.Filesystem/Move"].Get("Authorization"),
		"Basic "+base64.StdEncoding.EncodeToString([]byte("user:")); got != want {
		t.Errorf("the move authenticated as %q; want %q, the account every file call names", got, want)
	}
	if got := stub.headers["/filesystem.Filesystem/Move"].Get("Content-Type"); got != "application/json" {
		t.Errorf("the move was sent as %q; want a Connect unary call in JSON", got)
	}
	for held := range stub.files {
		if strings.Contains(held, ".crewlet-tmp-") {
			t.Errorf("the box still holds %s, a staged file nothing reads", held)
		}
	}
}

// THE CONTENT IS STAGED IN THE DIRECTORY ENVD WRITES THE PATH IN, so the
// move is a rename within one directory. envd resolves a relative or `~/`
// path against the home before cleaning it ([envdResolve]), so a staged name
// made from a clean here could land elsewhere: `a/../~/x` is <home>/~/x to
// envd, while its clean, `~/x`, is <home>/x.
//
// Mutation: stage at path.Dir of the path joined with the fresh name, and the
// tilde forms and a trailing slash are staged in another directory.
func TestE2BStagesAWriteInTheDirectoryEnvdWritesThePathIn(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	for written, lands := range map[string]string{
		"/home/user/.npmrc":          "/home/user/.npmrc",
		"/home/user/.npmrc/":         "/home/user/.npmrc",
		".npmrc":                     "/home/user/.npmrc",
		".local/bin/tool":            "/home/user/.local/bin/tool",
		"dir/":                       "/home/user/dir",
		"~/.config/tool.toml":        "/home/user/.config/tool.toml",
		"~/../user/.npmrc":           "/home/user/.npmrc",
		"~/a/../../x":                "/home/x",
		"a/../~/x":                   "/home/user/~/x",
		"/home/user/./tools/../.pip": "/home/user/.pip",
	} {
		stub.mu.Lock()
		stub.uploads, stub.moves = nil, nil
		stub.mu.Unlock()
		_ = box.WriteFile(t.Context(), written, []byte("x"))
		stub.mu.Lock()
		uploads, moves := slices.Clone(stub.uploads), slices.Clone(stub.moves)
		stub.mu.Unlock()
		if len(uploads) != 1 || len(moves) != 1 {
			t.Errorf("WriteFile(%q) made %d uploads and %d moves; want one each", written, len(uploads), len(moves))
			continue
		}
		staged, dest := envdResolve(uploads[0]), envdResolve(moves[0][1])
		if dest != lands {
			t.Errorf("WriteFile(%q) moved its content to %s; want %s", written, dest, lands)
		}
		if path.Dir(staged) != path.Dir(dest) {
			t.Errorf("WriteFile(%q) staged its content in %s and moved it to %s: another directory",
				written, path.Dir(staged), dest)
		}
	}
}

// envdResolve is where envd puts a path a file call names, for the account
// whose home is /home/user (ExpandAndResolve in
// packages/envd/internal/permissions/path.go, e2b-dev/infra): an absolute path
// as written, a `~` one expanded to the home, a relative one joined onto the
// home — those two cleaned by the join.
func envdResolve(p string) string {
	const home = "/home/user"
	switch {
	case path.IsAbs(p):
		return p
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return path.Join(home, p[1:])
	}
	return path.Join(home, p)
}

// A FAILED WRITE LEAVES NOTHING STAGED, AND THE FILE AS IT WAS. An upload
// refused part way (the disk filled) or a refused move leaves the staged file,
// which is removed as the account that wrote it. The error names the path and
// what failed.
//
// Mutation: return the upload's or the move's error without removing what was
// staged, and the box keeps a file nothing reads.
func TestE2BAFailedWriteLeavesNothingStaged(t *testing.T) {
	t.Parallel()
	const path = "/home/user/.config/crewlet/github-token"
	for name, fail := range map[string]func(*e2bStub){
		"the upload": func(s *e2bStub) { s.uploadStatus = http.StatusInsufficientStorage },
		"the move":   func(s *e2bStub) { s.moveStatus = http.StatusInternalServerError },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newE2BStub()
			provider, _ := newE2B(t, stub)
			box, err := provider.Create(t.Context(), sandbox.Spec{})
			if err != nil {
				t.Fatal(err)
			}
			if err := box.WriteFile(t.Context(), path, []byte("ghs_held")); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			stub.mu.Lock()
			fail(stub)
			stub.mu.Unlock()
			err = box.WriteFile(t.Context(), path, []byte("ghs_renewed"))
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("a write whose %s failed = %v; want an error naming %s", name, err, path)
			}
			if got, err := box.ReadFile(t.Context(), path); err != nil || string(got) != "ghs_held" {
				t.Errorf("the path holds %q, %v; want the file it had", got, err)
			}
			stub.mu.Lock()
			defer stub.mu.Unlock()
			staged := stub.uploads[len(stub.uploads)-1]
			if !slices.Contains(stub.removes, staged) {
				t.Errorf("the staged file %s was not removed (removes: %v)", staged, stub.removes)
			}
			if _, held := stub.files[staged]; held {
				t.Errorf("the box still holds %s, a staged file nothing reads", staged)
			}
			if got, want := stub.headers["/filesystem.Filesystem/Remove"].Get("Authorization"),
				"Basic "+base64.StdEncoding.EncodeToString([]byte("user:")); got != want {
				t.Errorf("the removal authenticated as %q; want %q", got, want)
			}
		})
	}
}

// A WRITE WHOSE CALLER GAVE UP LEAVES NOTHING STAGED EITHER. A context that
// ends mid-upload (a poll past its bound, a node shutting down) is the
// commonest failure, and a removal under it would never be sent, so the staged
// file is removed whatever became of that context.
//
// Mutation: remove the staged file under the write's own context, and the
// removal is never sent.
func TestE2BAWriteItsCallerGaveUpOnLeavesNothingStaged(t *testing.T) {
	t.Parallel()
	const path = "/home/user/.config/crewlet/github-token"
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.WriteFile(t.Context(), path, []byte("ghs_held")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	halfWritten := make(chan string, 1)
	stub.mu.Lock()
	stub.halfWritten = halfWritten
	stub.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	written := make(chan error, 1)
	go func() { written <- box.WriteFile(ctx, path, []byte("ghs_renewed")) }()
	var staged string
	select {
	case staged = <-halfWritten:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the upload never reached envd")
	}
	cancel()
	if err := <-written; err == nil {
		t.Fatal("a write whose caller gave up mid-upload succeeded")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !slices.Contains(stub.removes, staged) {
		t.Errorf("the staged file %s was not removed (removes: %v)", staged, stub.removes)
	}
	if _, held := stub.files[staged]; held {
		t.Errorf("the box still holds %s, a staged file nothing reads", staged)
	}
	if got := string(stub.files[path]); got != "ghs_held" {
		t.Errorf("the path holds %q; want the file it had", got)
	}
}

// THE FILE CONTRACT, against a fake envd that serves downloads through
// ServeContent as envd does. At the real cap, unlike the in-process backends:
// a read crosses envd's HTTP body, its 32 KiB pieces and their idle bound, and
// a limit written against [sandbox.MaxFileBytes] anywhere on that path — whole
// read, stream or tail — is what the cap cases catch; a smaller file would
// pass beneath it.
func TestE2BKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, sandbox.MaxFileBytes, func(t *testing.T) sandbox.Sandbox {
		provider, _ := newE2B(t, newE2BStub())
		box, err := provider.Create(t.Context(), sandbox.Spec{})
		if err != nil {
			t.Fatal(err)
		}
		return box
	})
}

// A TAIL IS A SUFFIX RANGE, so asking a running job what it last wrote costs
// the window, not the whole stream, on every poll.
func TestE2BReadsATailAsASuffixRange(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("0123456789\n"), 10_000)
	stub.files["/home/user/.crewlet/result.json"] = content

	tail, err := box.ReadTail(t.Context(), "/home/user/.crewlet/result.json", 64)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if !bytes.Equal(tail.Data, content[len(content)-64:]) || tail.Size != int64(len(content)) {
		t.Errorf("tail = %d bytes of %d; want the last 64 of %d", len(tail.Data), tail.Size, len(content))
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !slices.Equal(stub.ranges, []string{"bytes=-64"}) {
		t.Errorf("the download asked for %q; want one suffix range of 64", stub.ranges)
	}
}

// AN ENVD THAT IGNORES THE RANGE IS STILL ANSWERED RIGHT: its 200 carries the
// whole file, read through keeping only the end — the same answer at a higher
// cost, since a box's envd version is not this engine's to choose.
func TestE2BReadsATailFromAnEnvdThatIgnoresTheRange(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.ignoreRange = true
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("abcdefghij"), 50_000)
	stub.files["/home/user/.crewlet/err.log"] = content

	tail, err := box.ReadTail(t.Context(), "/home/user/.crewlet/err.log", 1000)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if !bytes.Equal(tail.Data, content[len(content)-1000:]) || tail.Size != int64(len(content)) {
		t.Errorf("tail = %d bytes of %d; want the last 1000 of %d", len(tail.Data), tail.Size, len(content))
	}
}

// A STREAMED END IS A SUFFIX RANGE, so keeping a failed check's log costs the
// bytes kept and never the output before them — and an envd that ignores the
// range, sending the whole file with no length, is still answered the end.
func TestE2BStreamsATailAsASuffixRange(t *testing.T) {
	t.Parallel()
	for _, ignoreRange := range []bool{false, true} {
		stub := newE2BStub()
		stub.ignoreRange = ignoreRange
		provider, _ := newE2B(t, stub)
		box, err := provider.Create(t.Context(), sandbox.Spec{})
		if err != nil {
			t.Fatal(err)
		}
		content := bytes.Repeat([]byte("0123456789\n"), 10_000)
		stub.files["/home/user/.crewlet-check/output"] = content

		r, size, err := box.OpenTail(t.Context(), "/home/user/.crewlet-check/output", 64)
		if err != nil {
			t.Fatalf("OpenTail (range ignored %v): %v", ignoreRange, err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(got, content[len(content)-64:]) || size != int64(len(content)) {
			t.Errorf("range ignored %v: the streamed end = %d bytes of %d, %v; want the last 64 of %d",
				ignoreRange, len(got), size, err, len(content))
		}
		stub.mu.Lock()
		if !slices.Equal(stub.ranges, []string{"bytes=-64"}) {
			t.Errorf("range ignored %v: the download asked for %q; want one suffix range of 64", ignoreRange,
				stub.ranges)
		}
		stub.mu.Unlock()
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

// A BOX THAT IS ALREADY GONE IS A SUCCESSFUL KILL: the reaper targets boxes it
// believes orphaned, most already reclaimed, so raising would error every tick.
func TestE2BKillingAGoneBoxSucceeds(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	stub.killStatus = http.StatusNotFound
	provider, _ := newE2B(t, stub)

	if err := provider.Kill(context.Background(), "sbx-gone"); err != nil {
		t.Fatalf("killing an already-gone box raised: %v", err)
	}
	// AND A REAL REFUSAL STILL RAISES, or a permissions problem would read
	// as a reclaim and the box would bill for ever.
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

// CONNECT IS ONE CALL, and "already running" is a success status. GET then
// the deprecated POST /resume was two round trips on the completion path that
// raced a reaper between them.
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
	// THE DEPRECATED ENDPOINT IS NOT CALLED AT ALL, not even as a fallback:
	// /resume can say "already running" only as an error (409).
	if stub.saw("POST /sandboxes/sbx1/resume") {
		t.Errorf("the deprecated resume endpoint was called: %v", stub.requests)
	}
	// A FRESH TTL: a snapshot has no kill timer, so a box woken without one
	// keeps what remained of the timer it was paused with — overnight,
	// nothing. The API requires the field.
	if got := stub.body("/sandboxes/sbx1/connect")["timeout"]; got == nil || got == float64(0) {
		t.Errorf("connect gave the box no TTL: %v", got)
	}

	// 201: the box was paused and was woken. Both are success.
	stub.connectStatus = http.StatusCreated
	if _, err := provider.Connect(context.Background(), "sbx1"); err != nil {
		t.Fatalf("a resumed box was reported as a failure: %v", err)
	}

	// AND A CONNECT THAT REALLY FAILED IS NOT SWALLOWED: a handle to a
	// still-paused box fails every command one layer further in, with
	// envd's message rather than the control plane's.
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
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Connect(context.Background(), "sbx-gone"); !errors.Is(err, sandbox.ErrBoxGone) {
		t.Fatalf("connecting to a vanished box = %v; want ErrBoxGone", err)
	}
	if _, err := provider.Attach(context.Background(), "sbx-gone"); !errors.Is(err, sandbox.ErrBoxGone) {
		t.Fatalf("attaching to a vanished box = %v; want ErrBoxGone", err)
	}
}

// A READER NEVER WAKES A BOX. Attach reads the box (`GET /sandboxes/{id}`):
// a running one is reached through envd, a paused one refused. /connect would
// resume a paused box and move its timer, so a peek racing the collection's
// pause would wake a box nothing renews.
//
// Mutation: attach through /connect, and the box is woken.
func TestE2BAttachReadsTheBoxAndNeverWakesIt(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	ctx := context.Background()

	box, err := provider.Attach(ctx, "sbx1")
	if err != nil {
		t.Fatalf("attaching to a running box: %v", err)
	}
	stub.mu.Lock()
	stub.files["/home/user/.crewlet/done"] = []byte("0\n")
	stub.mu.Unlock()
	if got, err := box.ReadFile(ctx, "/home/user/.crewlet/done"); err != nil || string(got) != "0\n" {
		t.Fatalf("an attached box read %q, %v; want its file through envd", got, err)
	}

	stub.mu.Lock()
	stub.state = "paused"
	stub.mu.Unlock()
	if _, err := provider.Attach(ctx, "sbx1"); !errors.Is(err, sandbox.ErrBoxPaused) {
		t.Fatalf("attaching to a paused box = %v; want ErrBoxPaused", err)
	}

	if !stub.saw("GET /sandboxes/sbx1") {
		t.Errorf("the box was never read: %v", stub.requests)
	}
	for _, woke := range []string{"POST /sandboxes/sbx1/connect", "POST /sandboxes/sbx1/timeout"} {
		if stub.saw(woke) {
			t.Errorf("an attach called %s: %v", woke, stub.requests)
		}
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
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
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
		t.Fatalf("Kind() = %q, which is not what providers.sandbox names the backend",
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
// http.Client.Timeout covers the whole exchange, body included, so one client
// for both planes would cap every command at the bound right for minting a
// VM. What bounds a command is the gap BETWEEN frames, not the call.
func TestE2BACommandOutlivesTheControlPlaneTimeout(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	// THE RATIO IS THE PROPERTY: three frames at this delay is a 1.5 s
	// stream against the 1 s bound below, so the command is still running
	// when that bound would have fired.
	stub.streamDelay = 500 * time.Millisecond
	stub.frames = []any{startEvent(1), dataEvent("still going", ""), endEvent(0)}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	provider, err := sandbox.NewE2B(sandbox.E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		// A SMALL overall bound, standing in for the control plane's, that the
		// command below outlives. Not tiny: it also covers the Create round trip
		// below, which must not flake in a suite running every package in
		// parallel under -race. One second is ~200x a healthy local round trip
		// and still well inside the stream above.
		HTTP: &http.Client{
			Timeout:   time.Second,
			Transport: httpxtest.Rewrite(t, server),
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
