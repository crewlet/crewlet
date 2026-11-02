package mcp

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// stepOf is the report's step for stage, failing the test where it has none.
func stepOf(t *testing.T, r TestReport, stage TestStage) TestStep {
	t.Helper()
	for _, s := range r.Steps {
		if s.Stage == stage {
			return s
		}
	}
	t.Fatalf("the report has no %s step: %+v", stage, r.Steps)
	return TestStep{}
}

// failedAt is the stage the report stopped at, "" where none failed.
func failedAt(r TestReport) TestStage {
	for _, s := range r.Steps {
		if s.State == StepFailed {
			return s.Stage
		}
	}
	return ""
}

// A TEST NEVER CALLS A TOOL. A local server that would mark a call it received
// lists its tools to a test, and is never called; a remote one is asked for
// its handshake and its listing, and nothing else that names a tool.
func TestATestNeverCallsATool(t *testing.T) {
	t.Parallel()
	seen := filepath.Join(t.TempDir(), "called")
	spec := helperSpec(t, "local", "serve", map[string]string{
		helperToolsEnv:    toolsJSON([3]string{"search", "Searches.", `{"readOnlyHint":true}`}),
		helperCallSeenEnv: seen,
	})
	report := Test(t.Context(), spec, discardLogger())
	if !report.Serving() || len(report.Tools) != 1 {
		t.Fatalf("the local server did not test as serving one tool: %+v", report)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Fatalf("a test called a tool of the local server (%v)", err)
	}

	srv, h := newHTTPMCPServer(t, `[{"name":"remote_tool","inputSchema":{"type":"object"}}]`)
	report = Test(t.Context(), httpSpec("remote", srv.URL), discardLogger())
	if !report.Serving() {
		t.Fatalf("the remote server did not test as serving: %+v", report)
	}
	for _, r := range h.seen() {
		if r.RPCMethd == "tools/call" {
			t.Fatal("a test called a tool of the remote server")
		}
	}
}

// EACH FAILURE CLASS IS DECIDED AT ITS STAGE: the stage whose facts produced
// it, and every stage after it not reached. (blocked_address is an address
// a server's sign-in metadata names, which an address check reaches:
// TestAnAddressCheckFollowsWhatTheServerNamesOnlyThroughTheGuard.)
func TestEachFailureClassIsDecidedAtItsStage(t *testing.T) {
	t.Parallel()
	answering := func(status int, header map[string]string, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv.URL + "/mcp"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + listener.Addr().String() + "/mcp"
	_ = listener.Close()
	unsigned := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(unsigned.Close)
	empty, _ := newHTTPMCPServer(t, `[]`)

	exits := helperSpec(t, "exits", "serve", map[string]string{helperExitEnv: "3",
		helperStderrEnv: "Error: DATABASE_URI is not set"})
	mute := helperSpec(t, "mute", "mute", nil)
	mute.StartupTimeout = 300 * time.Millisecond

	cases := []struct {
		name  string
		spec  Spec
		stage TestStage
		class FailureClass
	}{
		{"a host nobody resolves", httpSpec("dns", "http://crewlet-test-no-such-host.invalid/mcp"), StageResolve, FailDNS},
		{"a port nothing listens on", httpSpec("closed", closed), StageConnect, FailUnreachable},
		{"a certificate nobody signed", httpSpec("tls", unsigned.URL+"/mcp"), StageTLS, FailTLS},
		{"a sign-in it asks for", httpSpec("signin", answering(401,
			map[string]string{"WWW-Authenticate": `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`}, "")),
			StageDiscover, FailNeedsSignIn},
		{"a credential it refuses", httpSpec("rejected", answering(401, nil, "")), StageDiscover, FailCredentialRejected},
		{"a scope missing", httpSpec("scope", answering(403, nil, "")), StageDiscover, FailMissingPermission},
		{"no endpoint", httpSpec("gone", answering(404, nil, "")), StageDiscover, FailNoEndpoint},
		{"a login page", httpSpec("page", answering(200, map[string]string{"Content-Type": "text/html"}, "<html></html>")),
			StageDiscover, FailNotMCP},
		{"an old server", httpSpec("old", answering(400, nil, "Unsupported protocol version")), StageDiscover, FailOldProtocol},
		{"slow down", httpSpec("busy", answering(429, nil, "")), StageDiscover, FailRateLimited},
		{"a gateway whose server is down", httpSpec("gateway", answering(503, nil, "")), StageDiscover, FailUnreachable},
		{"a process that never answers", mute, StageDiscover, FailUnreachable},
		{"a command not on PATH", Spec{Name: "absent", Command: "crewlet-test-no-such-command",
			StartupTimeout: 5 * time.Second}, StageLaunch, FailCommandNotFound},
		{"a process that exits before it answers", exits, StageLaunch, FailExited},
		{"a server listing no tool", httpSpec("empty", empty.URL), "", FailEmptyTools},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := Test(t.Context(), tc.spec, discardLogger())
			if report.Failure == nil || report.Failure.Class != tc.class {
				t.Fatalf("failure = %+v, want class %q", report.Failure, tc.class)
			}
			if got := failedAt(report); got != tc.stage {
				t.Fatalf("stopped at %q, want %q: %+v", got, tc.stage, report.Steps)
			}
			past := false
			for _, s := range report.Steps {
				if past && s.State != StepNotReached {
					t.Errorf("%s after the failed stage is %s, want not reached", s.Stage, s.State)
				}
				past = past || s.State == StepFailed
			}
			if tc.stage != "" && stepOf(t, report, tc.stage).Detail != report.Failure.Said {
				t.Errorf("the failed step says %q, the failure %q", stepOf(t, report, tc.stage).Detail,
					report.Failure.Said)
			}
		})
	}
	// EVERY CLASS IS COVERED: by a case here, or by the address check's.
	covered := map[FailureClass]bool{FailBlockedAddress: true}
	for _, tc := range cases {
		covered[tc.class] = true
	}
	for _, class := range FailureClasses {
		if !covered[class] {
			t.Errorf("no case decides %q at its stage", class)
		}
	}
}

// A PROCESS THAT EXITS SAYS WHY in the failure, from its own last words.
// (Two lines, so the line the process prints last is not the whole of a
// variable the spec carries, which the report would withhold.)
func TestALocalServerThatExitsIsReportedInItsOwnWords(t *testing.T) {
	t.Parallel()
	spec := helperSpec(t, "exits", "serve", map[string]string{helperExitEnv: "3",
		helperStderrEnv: "starting\nError: DATABASE_URI is not set"})
	report := Test(t.Context(), spec, discardLogger())
	if report.Failure == nil || !strings.Contains(report.Failure.Said, "DATABASE_URI is not set") {
		t.Fatalf("failure = %+v, want the process's last words in it", report.Failure)
	}
}

// A SERVER THAT WORKS IS WALKED THROUGH EVERY STAGE, each timed, the ones its
// address has no need of skipped, and its tools as the engine would offer
// them: the prefix applied, an operator's override over the server's hints,
// the ones excluded left out, and a name no model can be handed refused.
func TestAServerThatWorksPassesEveryStageItNeeds(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 60)
	srv, _ := newHTTPMCPServer(t, `[{"name":"search","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},`+
		`{"name":"drop","inputSchema":{"type":"object"}},`+
		`{"name":"hidden","inputSchema":{"type":"object"}},`+
		`{"name":"`+long+`","inputSchema":{"type":"object"}}]`)
	spec := httpSpec("remote", srv.URL)
	spec.ToolPrefix = "acme_"
	spec.ExcludeTools = []string{"hidden"}
	spec.AnnotationOverrides = map[string]Annotations{"acme_drop": {Destructive: No}}
	report := Test(t.Context(), spec, discardLogger())
	if report.Failure != nil {
		t.Fatalf("failure = %+v", report.Failure)
	}
	want := map[TestStage]StepState{StageAddress: StepPassed, StageResolve: StepSkipped,
		StageConnect: StepPassed, StageTLS: StepSkipped, StageDiscover: StepPassed, StageList: StepPassed}
	for _, s := range report.Steps {
		if s.State != want[s.Stage] {
			t.Errorf("%s is %s, want %s (%s)", s.Stage, s.State, want[s.Stage], s.Detail)
		}
	}
	if report.Identity.Name != "http-helper" {
		t.Errorf("identity = %+v, want the server's", report.Identity)
	}
	names := make([]string, 0, len(report.Tools))
	for _, tool := range report.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{"acme_search", "acme_drop", "acme_" + long}) {
		t.Fatalf("tools = %v, want the prefixed listing without the excluded one", names)
	}
	if report.Tools[1].Annotations.Destructive != No || report.Tools[0].Annotations.ReadOnly != Yes {
		t.Errorf("hints = %+v, want the server's with the override over them", report.Tools[:2])
	}
	if report.Tools[2].Refused != NameTooLong || report.Tools[0].Refused != "" {
		t.Errorf("refusals = %q, %q; want only the long name refused", report.Tools[0].Refused, report.Tools[2].Refused)
	}
}

// A TEST'S WORDS CARRY NO VALUE: a process that prints the credential it was
// started with has it withheld from the failure, and an address with a key in
// its query is written with the key withheld.
func TestATestsWordsCarryNoValue(t *testing.T) {
	t.Parallel()
	const token = "sk-test-0123456789abcdef"
	spec := helperSpec(t, "echo", "serve", map[string]string{helperExitEnv: "3",
		helperEchoEnv: "VENDOR_TOKEN", "VENDOR_TOKEN": token})
	report := Test(t.Context(), spec, discardLogger())
	if report.Failure == nil {
		t.Fatal("a process that exited tested as serving")
	}
	for _, s := range report.Steps {
		if strings.Contains(s.Detail, token) {
			t.Errorf("step %s repeats the credential: %q", s.Stage, s.Detail)
		}
	}
	if strings.Contains(report.Failure.Said, token) || !strings.Contains(report.Failure.Said, "[withheld]") {
		t.Errorf("failure = %q, want the credential withheld", report.Failure.Said)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String() + "/mcp?api_key=opaque-key-value-42"
	_ = listener.Close()
	report = Test(t.Context(), httpSpec("keyed", address), discardLogger())
	for _, s := range report.Steps {
		if strings.Contains(s.Detail, "opaque-key-value-42") {
			t.Errorf("step %s writes the address's key: %q", s.Stage, s.Detail)
		}
	}
}

// A TEST STOPPED BEFORE ANYTHING STARTED stops at its first stage, the
// address or the launch, with the caller's sentence.
func TestATestStoppedBeforeAnythingStartedStopsAtItsFirstStage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		spec  Spec
		stage TestStage
	}{
		{httpSpec("plain", "http://mcp.example.com/mcp"), StageAddress},
		{Spec{Name: "local", Command: "tracker-mcp"}, StageLaunch},
	} {
		report := Unstarted(tc.spec, "is plain http to a public host")
		if failedAt(report) != tc.stage || report.Failure.Said != "is plain http to a public host" {
			t.Fatalf("report = %+v, want a failure at %s", report, tc.stage)
		}
		if report.Serving() {
			t.Fatal("a test that started nothing tests as serving")
		}
	}
}
