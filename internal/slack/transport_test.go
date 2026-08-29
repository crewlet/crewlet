package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/slack"
)

// workspace is a Slack that records what it was asked.
type workspace struct {
	*httptest.Server

	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
	tokens []string
	refuse map[string]string
	// refuseOnce refuses a method exactly once and then lets it through,
	// which is how a run that recovers from a refusal is told apart from
	// one that never hit it.
	refuseOnce map[string]string
	replies    map[string]string
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	w := &workspace{
		refuse: map[string]string{}, refuseOnce: map[string]string{},
		replies: map[string]string{},
	}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		method := strings.TrimPrefix(req.URL.Path, "/api/")
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)

		w.mu.Lock()
		w.calls = append(w.calls, method)
		w.bodies = append(w.bodies, body)
		w.tokens = append(w.tokens, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		code, refused := w.refuse[method]
		if once, only := w.refuseOnce[method]; only {
			code, refused = once, true
			delete(w.refuseOnce, method)
		}
		reply, canned := w.replies[method]
		w.mu.Unlock()

		rw.Header().Set("Content-Type", "application/json")
		switch {
		case refused:
			_, _ = rw.Write([]byte(`{"ok":false,"error":"` + code + `"}`))
		case canned:
			_, _ = rw.Write([]byte(reply))
		default:
			_, _ = rw.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(w.Close)
	return w
}

func (w *workspace) called(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var n int
	for _, c := range w.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (w *workspace) lastBody(method string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.calls) - 1; i >= 0; i-- {
		if w.calls[i] == method {
			return w.bodies[i]
		}
	}
	return nil
}

// client points a Slack client at the fake workspace.
//
// The API base is a package constant, so the fake is reached by giving the
// transport an http.Client whose RoundTripper rewrites the host — which is
// also what proves the client builds the right PATH for each method.
func rewriting(target string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: rewriteHost(target)}
}

type rewriteHost string

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	rewritten := req.Clone(req.Context())
	base := strings.TrimPrefix(string(r), "http://")
	rewritten.URL.Scheme = "http"
	rewritten.URL.Host = base
	return http.DefaultTransport.RoundTrip(rewritten)
}

func transport(t *testing.T, ws *workspace, mutate func(*slack.TransportOptions)) *slack.Transport {
	t.Helper()
	opts := slack.TransportOptions{
		Config: slack.Config{
			Status: notify.StatusAddressed,
			Seats:  []slack.SeatConfig{{Handle: "swe", Token: "xoxb-swe", Channel: "C0DEFAULT"}},
		},
		Follows: newFollows(),
		HTTP:    rewriting(ws.URL),
		Now:     func() time.Time { return pinned },
	}
	if mutate != nil {
		mutate(&opts)
	}
	tr, err := slack.NewTransport(opts)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// EVERY SEAT'S IDENTITY IS RESOLVED AT START, because a Slack payload names
// a seat by its bot user id and nothing in the org model declares it.
func TestStartResolvesEverySeatsIdentity(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `","team_id":"T0ACME"}`

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := tr.Handles(); len(got) != 1 || got[0] != "swe" {
		t.Fatalf("handles = %v", got)
	}
	if ws.called("auth.test") != 1 {
		t.Errorf("auth.test called %d times", ws.called("auth.test"))
	}
}

// A SEAT WHOSE TOKEN IS REFUSED IS LEFT OUT rather than run half-configured:
// without an identity it cannot recognise its own messages, so it would
// answer itself for ever.
func TestARefusedTokenDropsThatSeatAndNoOther(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.refuse["auth.test"] = "invalid_auth"

	tr := transport(t, ws, nil)
	err := tr.Start(context.Background())
	if err == nil {
		t.Fatal("a transport with no usable app started cleanly")
	}
	if len(tr.Handles()) != 0 {
		t.Errorf("a seat with a refused token is running: %v", tr.Handles())
	}
}

// A MESSAGE POSTS AS THE SEAT'S OWN APP, into the thread it names.
func TestSendPostsAsTheSeatIntoItsThread(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`
	ws.replies["chat.postMessage"] = `{"ok":true,"ts":"1700000010.001000"}`
	store := newFollows()

	tr := transport(t, ws, func(o *slack.TransportOptions) { o.Follows = store })
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts, err := tr.Send(context.Background(), "swe", "C0ENG", "1700000000.000000", "on it")
	if err != nil {
		t.Fatal(err)
	}
	if ts != "1700000010.001000" {
		t.Errorf("ts = %q", ts)
	}
	body := ws.lastBody("chat.postMessage")
	if body["channel"] != "C0ENG" || body["thread_ts"] != "1700000000.000000" {
		t.Errorf("posted %v", body)
	}
	// REPLYING SUBSCRIBES THE SEAT, which is what every chat client does
	// when a person replies — the seat hears what comes back without
	// being named again.
	if store.reason("swe", "C0ENG", "1700000000.000000") != string(notify.FollowParticipated) {
		t.Error("posting into a thread did not subscribe the seat to it")
	}
}

// A SEAT WITH NO EXPLICIT CHANNEL FALLS BACK TO ITS OWN.
func TestSendFallsBackToTheSeatsChannel(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`
	ws.replies["chat.postMessage"] = `{"ok":true,"ts":"1.1"}`

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Send(context.Background(), "swe", "", "", "hello"); err != nil {
		t.Fatal(err)
	}
	if got := ws.lastBody("chat.postMessage")["channel"]; got != "C0DEFAULT" {
		t.Errorf("channel = %v", got)
	}
}

// THE WORKING INDICATOR CARRIES TEXT ON THIS BACKEND, which is what makes
// the phrase pools worth having — and it clears by setting an empty one.
func TestTheWorkingIndicatorIsRaisedAndCleared(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !tr.SupportsStatusText() {
		t.Fatal("Slack's indicator renders text and the poster says it does not")
	}
	if !tr.SetStatus(context.Background(), "swe", "C0ENG", "1.1", "is thinking...") {
		t.Fatal("the indicator was not raised")
	}
	if got := ws.lastBody("assistant.threads.setStatus")["status"]; got != "is thinking..." {
		t.Errorf("status = %v", got)
	}
	if !tr.ClearStatus(context.Background(), "swe", "C0ENG", "1.1") {
		t.Fatal("the indicator was not cleared")
	}
	if got := ws.lastBody("assistant.threads.setStatus")["status"]; got != "" {
		t.Errorf("clear sent status %v, want an empty one", got)
	}
}

// A FAILED INDICATOR CALL IS A COSMETIC LOSS, never a turn's problem.
func TestAFailedIndicatorIsReportedNotRaised(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`
	ws.refuse["assistant.threads.setStatus"] = "channel_not_found"

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.SetStatus(context.Background(), "swe", "C0ENG", "1.1", "is thinking...") {
		t.Fatal("a refused status reported success")
	}
}

// THE DM PREFIX IS EXACT ON THIS BACKEND, which is what lets the indicator
// answer for an app_mention — whose payload omits the channel type.
func TestTheDMPrefixIsDeclared(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`
	tr := transport(t, ws, nil)
	if got := tr.DMChannelPrefix(); got != "D" {
		t.Fatalf("DM prefix = %q", got)
	}
	if got := tr.StatusRefresh(); got <= 0 || got >= 2*time.Minute {
		t.Errorf("refresh = %s, want well inside Slack's two-minute expiry", got)
	}
}

// THE SEAT LIST COMES FROM THE ORG, and a ${VAR} that answered nothing is
// SKIPPED rather than passed through as its literal text — which would
// authenticate as nobody.
func TestSeatsFromSkipsUnresolvedTokens(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "SWE", DeclaredHandle: "swe",
			Slack: org.SlackIdentity{BotToken: "${SWE_TOKEN}", SigningSecret: "${S}"}},
		{Name: "QA", DeclaredHandle: "qa",
			Slack: org.SlackIdentity{BotToken: "${QA_TOKEN_NEVER_SET}", SigningSecret: "${S}"}},
		{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
			Contact: &org.HumanContact{SlackUserID: "U0FOUNDER"}},
	}}
	o.Normalize()

	got := slack.SeatsFrom(o, func(name string) (string, bool) {
		if name == "SWE_TOKEN" {
			return "xoxb-swe", true
		}
		return "", false
	})
	if len(got) != 1 || got[0].Handle != "swe" || got[0].Token != "xoxb-swe" {
		t.Fatalf("seats = %+v", got)
	}
}
