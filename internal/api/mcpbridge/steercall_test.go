package mcpbridge_test

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A hook's call and its cancel, as the bridge's front reads them off the POST
// before the SDK does (Bridge.steerFront): raw POSTs shaped as Claude Code
// sends them, on a session the SDK holds and on one it does not.

// rawPost POSTs body to url as a box's MCP client does, on session where it
// names one, with extra headers besides; it answers the status and the body.
func rawPost(t *testing.T, url, session, body string, extra map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	res, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// versionOf is the protocol version sess negotiated, which its every request
// names.
func versionOf(sess *mcp.ClientSession) string { return sess.InitializeResult().ProtocolVersion }

// steerCall is a tools/call of steer_take under id, at Stop, its arguments
// padded by pad bytes.
func steerCall(id string, pad int) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + mcpbridge.SteerTool +
		`","arguments":{"event":"Stop","agent":"","pad":"` + strings.Repeat("x", pad) + `"}}}`
}

// lastCall is the call the inbox's newest take was handed.
func (i *fakeInbox) lastCall() *sandbox.NoteCall {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.calls) == 0 {
		return nil
	}
	return i.calls[len(i.calls)-1]
}

// cancelled is every cancel the inbox heard.
func (i *fakeInbox) cancelled() []sandbox.NoteCall {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]sandbox.NoteCall(nil), i.cancels...)
}

// A HOOK'S CALL CARRIES ITS IDENTITY TO THE INBOX: the MCP session it came on
// and its JSON-RPC id, which the front reads off the POST and the middleware
// hands the take; and a header a box sends in the front's name is dropped, so
// a box can never name a call its harness did not make.
//
// Mutation: keep a box-sent header, or hand the take no call, and a check
// goes red.
func TestAHookCallCarriesItsIdentityToTheInbox(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, 0)
	f.inbox.answer = "ok"
	sess := dial(t, f.url)
	if res := callTool(t, sess, mcpbridge.SteerTool, map[string]any{"event": "Stop", "agent": ""}); text(res) != "ok" {
		t.Fatalf("steer_take answered %q", text(res))
	}
	call := f.inbox.lastCall()
	if call == nil || call.Session != sess.ID() || !regexp.MustCompile(`^[0-9]+$`).MatchString(call.ID) {
		t.Errorf("the take was handed the call %+v, want session %s and the call's integer id", call, sess.ID())
	}
	status, _ := rawPost(t, f.url, sess.ID(), steerCall(`"not an id!"`, 0),
		map[string]string{"Crewlet-Steer-Call": "999", "Mcp-Protocol-Version": versionOf(sess)})
	if status != http.StatusOK {
		t.Fatalf("a steer_take with an id outside the charset answered %d", status)
	}
	if call := f.inbox.lastCall(); call != nil {
		t.Errorf("a box-sent header named the call %+v, want none read", call)
	}
}

// A CANCEL REACHES THE INBOX BEFORE THE SDK, whatever session it names: a cancel
// relayed to the seat's next holder names a session that node's SDK never
// held, which the SDK answers 404 before any middleware, and the inbox hears
// it all the same; one on a session the SDK holds is answered 202.
//
// Mutation: hear cancels in the steer middleware alone, and neither reaches
// the inbox.
func TestACancelReachesTheInboxBeforeTheSDK(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, 0)
	sess := dial(t, f.url)
	cancel := func(id string) string {
		return `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id +
			`,"reason":"hook timed out"}}`
	}
	if status, _ := rawPost(t, f.url, "never-held-1", cancel("7"), nil); status != http.StatusNotFound {
		t.Errorf("a cancel on a session the SDK never held answered %d, want its 404", status)
	}
	if status, _ := rawPost(t, f.url, sess.ID(), cancel(`"a-1"`),
		map[string]string{"Mcp-Protocol-Version": versionOf(sess)}); status != http.StatusAccepted {
		t.Errorf("a cancel on the box's session answered %d, want 202", status)
	}
	got := f.inbox.cancelled()
	want := []sandbox.NoteCall{{Session: "never-held-1", ID: "7"}, {Session: sess.ID(), ID: `"a-1"`}}
	if len(got) != len(want) || got[0].Session != want[0].Session || got[0].ID != want[0].ID ||
		got[1].Session != want[1].Session || got[1].ID != want[1].ID {
		t.Errorf("the inbox heard %+v, want %+v", got, want)
	}
}

// THE PEEK LEAVES THE REQUEST WHOLE: a hook's call the front read is handed to
// the SDK byte for byte, so it is answered as ever, under the id it carried.
//
// Mutation: hand the SDK the body past the peek alone, and the call fails.
func TestThePeekLeavesTheRequestWhole(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, 0)
	f.inbox.answer = "ok"
	sess := dial(t, f.url)
	status, body := rawPost(t, f.url, sess.ID(), steerCall("41", 3000),
		map[string]string{"Mcp-Protocol-Version": versionOf(sess)})
	if status != http.StatusOK || !strings.Contains(body, `"text":"ok"`) {
		t.Fatalf("a hook's call within the peek answered %d: %s", status, body)
	}
	if call := f.inbox.lastCall(); call == nil || call.ID != "41" || call.Session != sess.ID() {
		t.Errorf("the take was handed %+v, want call 41 on the box's session", call)
	}
}

// A BODY PAST THE PEEK IS HANDED ON UNREAD: no hook's call is that long, so the
// front reads nothing of it and the SDK reads it whole, answering as ever.
//
// Mutation: read a body past the peek, or cut it there, and a check goes red.
func TestABodyPastThePeekIsHandedOnUnread(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, 0)
	f.inbox.answer = "ok"
	sess := dial(t, f.url)
	status, body := rawPost(t, f.url, sess.ID(), steerCall("42", 5000),
		map[string]string{"Mcp-Protocol-Version": versionOf(sess)})
	if status != http.StatusOK || !strings.Contains(body, `"text":"ok"`) {
		t.Fatalf("a call past the peek answered %d: %s", status, body)
	}
	if call := f.inbox.lastCall(); call != nil {
		t.Errorf("a body past the peek was read: the take was handed %+v", call)
	}
}

// THE BRIDGE KEEPS THE ERA WHOSE HARNESS CANCELS A HOOK BY NOTIFICATION. Claude
// Code gives a hook's call up by `notifications/cancelled` in the protocol the
// bridge negotiates today, and by aborting its request in 2026-07-28's, where a
// cancel never reaches the inbox. A client offering 2026-07-28 is answered an
// older version, and its cancel is heard.
//
// Mutation: serve the handler stateless, or a go-sdk whose stateful transport
// serves the new era, and the negotiated version is 2026-07-28.
func TestTheBridgeKeepsTheEraWhoseHarnessCancelsAHookByNotification(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, 0)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.url, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28",`+
			`"capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.310"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	m := regexp.MustCompile(`"protocolVersion":"([0-9-]+)"`).FindStringSubmatch(string(raw))
	if m == nil || m[1] >= "2026-07-28" {
		t.Fatalf("initialize offering 2026-07-28 was answered %s", raw)
	}
	session := res.Header.Get("Mcp-Session-Id")
	if session == "" {
		t.Fatalf("initialize named no session: %s", raw)
	}
	rawPost(t, f.url, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		map[string]string{"Mcp-Protocol-Version": m[1]})
	if status, _ := rawPost(t, f.url, session,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`,
		map[string]string{"Mcp-Protocol-Version": m[1]}); status != http.StatusAccepted {
		t.Errorf("a cancel in the negotiated era answered %d", status)
	}
	if got := f.inbox.cancelled(); len(got) != 1 || got[0].Session != session || got[0].ID != "3" {
		t.Errorf("the inbox heard %+v, want the cancel of call 3", got)
	}
}
