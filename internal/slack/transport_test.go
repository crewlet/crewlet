package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
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
	// paged serves one canned answer per call to a method, in order, and
	// falls through to replies once it runs out. A cursor walk is the one
	// shape a single canned body cannot express.
	paged map[string][]string
}

// queryMethods are the Slack methods that read their parameters from the
// query string and ignore a JSON body, answering ok with nothing when one is
// posted instead. See [callQuery].
var queryMethods = map[string]bool{"bots.info": true, "conversations.replies": true}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	w := &workspace{
		refuse: map[string]string{}, refuseOnce: map[string]string{},
		replies: map[string]string{}, paged: map[string][]string{},
	}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		method := strings.TrimPrefix(req.URL.Path, "/api/")
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		// SLACK READS A JSON BODY FOR SOME METHODS AND SILENTLY IGNORES
		// IT FOR THE REST, answering ok with nothing at all: measured on
		// bots.info, which is why a query parameter posted as JSON
		// produced a seat that carried no app and no error. The fake
		// reproduces that rather than being generous, so a caller using
		// the wrong encoding fails here instead of in production.
		for name, values := range req.URL.Query() {
			if body == nil {
				body = map[string]any{}
			}
			body[name] = values[0]
		}
		if queryMethods[method] && len(req.URL.Query()) == 0 {
			// The parameter was sent the way this method will not read.
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"ok":true}`))
			w.mu.Lock()
			w.calls = append(w.calls, method)
			w.mu.Unlock()
			return
		}

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
		if pages := w.paged[method]; len(pages) > 0 {
			reply, canned = pages[0], true
			w.paged[method] = pages[1:]
		}
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
//
// IT TAKES THE SERVER, not its URL, so the pool it uses is that server's own:
// see [httpxtest] for what sharing the process-global one costs a package
// whose tests run in parallel, which this one's emphatically do.
func rewriting(t *testing.T, ws *workspace) *http.Client {
	t.Helper()
	return &http.Client{Timeout: 5 * time.Second, Transport: httpxtest.Rewrite(t, ws.Server)}
}

func transport(t *testing.T, ws *workspace, mutate func(*slack.TransportOptions)) *slack.Transport {
	t.Helper()
	opts := slack.TransportOptions{
		Config: slack.Config{
			Status: notify.StatusAddressed,
			Seats:  []slack.SeatConfig{{Handle: "swe", Token: "xoxb-swe"}},
		},
		Follows: newFollows(),
		HTTP:    rewriting(t, ws),
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

// THE INDICATOR AND THE PROMPT READ ONE RULE, and the transport is the door
// the indicator reads it through. A second declaration here would raise a
// spinner on messages the prompt tells the agent it may ignore.
//
// Its DM prefix is exact on this backend, which is what lets the indicator
// answer for an app_mention — whose payload omits the channel type.
func TestTheIndicatorReadsThePromptsOwnAddressRule(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`
	tr := transport(t, ws, nil)
	if got := tr.AddressRule(); !reflect.DeepEqual(got, slack.Prompt().Address) {
		t.Fatalf("the transport addresses by %+v, the prompt by %+v", got, slack.Prompt().Address)
	}
	if got := tr.AddressRule().DMPrefix; got != "D" {
		t.Fatalf("DM prefix = %q", got)
	}
	// An app_mention omits channel_type entirely, so the prefix is the
	// only thing that can answer for one.
	if !tr.AddressRule().IsDirect(map[string]string{notify.ChannelField: "D0ANA"}) {
		t.Error("a DM id was not read as direct")
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

// WHICH APP A SEAT IS, learned where Slack actually states it.
//
// auth.test answers with the bot's user id, its team and its bot id, and no
// app id at all: the field was decoded from a response Slack does not send,
// so it was empty for every seat this engine ever wired. bots.info is where
// it lives, keyed on the bot id auth.test does return. Knowing it is what
// lets a screen say which of an operator's apps an agent is, and link to the
// page that deletes it.
func TestASeatLearnsWhichAppItIs(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser +
		`","team_id":"T0ACME","bot_id":"B0ACME"}`
	ws.replies["bots.info"] = `{"ok":true,"bot":{"app_id":"A0ACME"}}`

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := tr.Apps(); len(got) != 1 || got["swe"] != "A0ACME" {
		t.Errorf("Apps() = %v, want the app this seat authenticates as", got)
	}
}

// AND A SEAT WHOSE APP CANNOT BE NAMED STILL WORKS.
//
// Knowing the app is a label on a screen; the token working is an agent that
// can speak. Failing the seat over the second request would trade one for the
// other, and the delivery envelope names the app anyway wherever the parser
// needs it.
func TestASeatWhoseAppIsUnknownStillRuns(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser +
		`","team_id":"T0ACME","bot_id":"B0ACME"}`
	ws.refuse["bots.info"] = "missing_scope"

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("a seat with a working token was dropped over a label: %v", err)
	}
	if got := tr.Handles(); len(got) != 1 || got[0] != "swe" {
		t.Fatalf("handles = %v, want the seat running", got)
	}
	if got := tr.Apps(); len(got) != 0 {
		t.Errorf("Apps() = %v, want no claim about an app nothing could name", got)
	}
}

// THE THREAD IS READ AS THE SEAT, on that app's own bot token, and the seat's
// own replies come back MARKED — by EITHER id, because a bot_message echo of
// its own post carries the app id and no user id at all.
func TestASeatsThreadComesBackMarkedWithItsOwnReplies(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `","bot_id":"B0SWE"}`
	ws.replies["bots.info"] = `{"ok":true,"bot":{"app_id":"` + botApp + `"}}`
	ws.replies["conversations.replies"] = `{"ok":true,"messages":[
		{"ts":"1.1","user":"` + human + `","text":"staging redirects in a loop"},
		{"ts":"1.2","user":"` + botUser + `","text":"on it"},
		{"ts":"1.3","subtype":"bot_message","app_id":"` + botApp + `","username":"agent-swe","text":"fixed in 4.2.4"},
		{"ts":"1.4","subtype":"channel_join","user":"` + human + `","text":"ana joined"},
		{"ts":"1.5","user":"` + colleague + `","text":"nice"}]}`

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var _ notify.ThreadReader = tr
	if tr.ThreadBackend() != slack.Backend {
		t.Fatalf("ThreadBackend = %q", tr.ThreadBackend())
	}

	got, ok := tr.ReadThread(context.Background(), "swe", "C0ENG", "1.1")
	if !ok {
		t.Fatal("a running seat could not read its own thread")
	}
	// The join line is gone: it wakes nobody, so it belongs in a thread
	// rendered for a seat no more than it belongs in a notification.
	if len(got.Messages) != 4 {
		t.Fatalf("the thread came back as %+v", got)
	}
	for i, want := range []struct {
		text string
		own  bool
	}{
		{"staging redirects in a loop", false},
		{"on it", true},
		// THE APP ID ALONE. Marked by the bot user id only, this one
		// would read as a colleague's — and the agent would answer its
		// own reply.
		{"fixed in 4.2.4", true},
		{"nice", false},
	} {
		if got.Messages[i].Text != want.text || got.Messages[i].Own != want.own {
			t.Errorf("message %d came back as %+v, want %+v", i, got.Messages[i], want)
		}
	}
	// The username a legacy bot message carries is kept, so a sender the
	// registry cannot resolve still renders as a name.
	if got.Messages[2].SenderName != "agent-swe" {
		t.Errorf("the bot message lost its username: %+v", got.Messages[2])
	}
	// A thread that ended on its first page is not short at either end, and
	// says so: the block renders a drop notice and a "go and read the rest"
	// preamble off these two, and a complete read that set either would
	// send every seat back to its chat tools for messages it already has.
	if got.Older != 0 || got.StoppedShort {
		t.Errorf("a complete thread came back claiming to be short: %+v", got)
	}
}

// THE ROOT KEEPS ITS PLACE EVEN WITH NOTHING IN IT.
//
// [notify.ThreadReader] promises the root FIRST, and the renderer exempts
// that first message from every bound and frames it as what the thread is
// about. Dropping a root for want of a body hands over a transcript that
// starts at the oldest surviving REPLY — kept by every bound, rendered first,
// and described to the seat as the opening. An alert app posting its payload
// in blocks or attachments carries no `text` and no `files`, so a thread
// rooted on one is the shape of an alert channel rather than an edge case.
func TestAThreadRootWithNoReadableTextStillComesBackFirst(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`
	ws.replies["conversations.replies"] = `{"ok":true,"messages":[
		{"ts":"1.1","subtype":"bot_message","bot_id":"B0ALERT","username":"alertbot"},
		{"ts":"1.2","user":"` + human + `","text":"is this the billing job"},
		{"ts":"1.3","subtype":"channel_join","user":"` + human + `","text":"ana joined"},
		{"ts":"1.4","user":"` + colleague + `","text":"billing"}]}`

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok := tr.ReadThread(context.Background(), "swe", "C0ENG", "1.1")
	if !ok {
		t.Fatal("a thread rooted on an attachment-only post could not be read")
	}
	// Three: the root's own slot and the two replies. The join line is
	// still gone — the exemption is the ROOT's, not every message with
	// nothing in it.
	if len(got.Messages) != 3 {
		t.Fatalf("the thread came back as %+v", got)
	}
	if got.Messages[0].Text != "" || got.Messages[0].SenderID != "B0ALERT" {
		t.Errorf("the root came back as %+v, want its slot with no body", got.Messages[0])
	}
	if got.Messages[1].Text != "is this the billing job" ||
		got.Messages[2].Text != "billing" {
		t.Errorf("the replies came back as %+v", got.Messages[1:])
	}
}

// A SEAT'S CLIENT IS REACHED BY HANDLE, and a seat whose token was refused has
// none to reach — so a thread read for it reports not-found rather than
// dereferencing a nil client.
//
// ASSERTED THROUGH THE READ, which is what production reaches a seat's client
// through. An accessor exported for a test to call answers a different
// question — whether the map has an entry — and a read that never looked
// there would pass it.
func TestARefusedSeatHasNoClientAndNoThread(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`

	tr := transport(t, ws, func(o *slack.TransportOptions) {
		o.Config.Seats = append(o.Config.Seats, slack.SeatConfig{Handle: "pm", Token: ""})
	})
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The seat that came up reads on the client its own token resolved.
	if _, ok := tr.ReadThread(context.Background(), "swe", "C0ENG", "1.1"); !ok {
		t.Fatal("a running seat could not reach its own client")
	}
	if got := ws.called("conversations.replies"); got != 1 {
		t.Fatalf("the running seat's read made %d calls, want 1", got)
	}
	// Neither of the two seats this node has no client for reaches the
	// workspace at all — and neither dereferences the nil one.
	if _, ok := tr.ReadThread(context.Background(), "pm", "C0ENG", "1.1"); ok {
		t.Fatal("a thread was read for a seat with no client")
	}
	if _, ok := tr.ReadThread(context.Background(), "nobody", "C0ENG", "1.1"); ok {
		t.Fatal("a thread was read for a seat this node does not run")
	}
	if got := ws.called("conversations.replies"); got != 1 {
		t.Fatalf("a seat with no client called conversations.replies anyway (%d calls)", got)
	}
}

// A WORKSPACE THAT REFUSES THE READ — a missing history scope, a channel the
// app is not in — is reported as unreadable, never as an empty thread.
func TestARefusedWorkspaceReadIsNotAnEmptyThread(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["auth.test"] = `{"ok":true,"user_id":"` + botUser + `"}`
	ws.refuse["conversations.replies"] = "missing_scope"

	tr := transport(t, ws, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := tr.ReadThread(context.Background(), "swe", "C0ENG", "1.1"); ok {
		t.Fatalf("a refused read reported success: %+v", got)
	}
}
