package slack_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/slack"
)

// The Web API calls the transport makes on a seat's own token, exercised
// directly rather than only through the transport — there was no such file,
// and `conversations.replies` is the one call here whose WRONG ENCODING
// produces no error at all.

// A READ METHOD GOES IN THE QUERY STRING, NEVER IN A JSON BODY.
//
// Slack reads a JSON body for some methods and silently ignores it for the
// rest, answering `{"ok":true}` with nothing — measured on bots.info. So a
// thread read posted as JSON returns an empty thread from a call that
// reported working, which the block above renders as "this thread has nothing
// in it" and nobody ever sees a failure. The fake workspace reproduces that
// refusal for every method in its `queryMethods` list, which is why this test
// is a test of the encoding rather than of the fixture.
func TestAThreadReadAsksInTheQueryString(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["conversations.replies"] = `{"ok":true,"messages":[
		{"ts":"1.1","user":"` + human + `","text":"staging redirects in a loop"},
		{"ts":"1.2","user":"` + botUser + `","text":"on it"}]}`

	client, err := slack.NewClient("xoxb-swe", rewriting(t, ws))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Replies(t.Context(), "C0ENG", "1.1")
	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the thread came back as %+v", got)
	}
	body := ws.lastBody("conversations.replies")
	// THE PARAMETER IS `ts`, not `thread_ts`: the argument is the PARENT
	// message's timestamp, and Slack answers an error for a name it does
	// not take.
	if body["ts"] != "1.1" || body["channel"] != "C0ENG" {
		t.Fatalf("the read asked for %v", body)
	}
	if body["thread_ts"] != nil {
		t.Errorf("the read sent thread_ts, which this method does not take: %v", body)
	}
}

// A LONG THREAD IS WALKED TO THE END, and the parent Slack repeats on every
// page is not repeated in the answer.
//
// conversations.replies pages from the OLDEST end and its cursors are opaque,
// so there is no way to ask for the newest N — walking is the only honest way
// to reach the end. Slack includes the parent in every page, so a walk that
// did not dedupe would render the thread's first message once per page.
func TestALongThreadIsWalkedOnceThrough(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.paged["conversations.replies"] = []string{
		`{"ok":true,"has_more":true,"messages":[
			{"ts":"1.1","user":"` + human + `","text":"the question"},
			{"ts":"1.2","user":"` + human + `","text":"first"}],
			"response_metadata":{"next_cursor":"c2"}}`,
		`{"ok":true,"messages":[
			{"ts":"1.1","user":"` + human + `","text":"the question"},
			{"ts":"1.3","user":"` + human + `","text":"last"}]}`,
	}

	client, err := slack.NewClient("xoxb-swe", rewriting(t, ws))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Replies(t.Context(), "C0ENG", "1.1")
	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	var texts []string
	for _, r := range got {
		texts = append(texts, r.Text)
	}
	if strings.Join(texts, ",") != "the question,first,last" {
		t.Fatalf("the walk produced %v", texts)
	}
	if ws.called("conversations.replies") != 2 {
		t.Fatalf("the walk made %d calls", ws.called("conversations.replies"))
	}
	if ws.lastBody("conversations.replies")["cursor"] != "c2" {
		t.Fatalf("the second page did not carry the cursor: %v",
			ws.lastBody("conversations.replies"))
	}
}

// A THREAD THAT ENDS ON ONE PAGE COSTS ONE REQUEST. The page size is Slack's
// own recommended ceiling for this family, so the ordinary turn — a thread of
// tens of messages — never pages at all.
func TestAnOrdinaryThreadCostsOneRequest(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["conversations.replies"] = `{"ok":true,"messages":[{"ts":"1.1","text":"hi"}]}`

	client, _ := slack.NewClient("xoxb-swe", rewriting(t, ws))
	if _, err := client.Replies(t.Context(), "C0ENG", "1.1"); err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if n := ws.called("conversations.replies"); n != 1 {
		t.Fatalf("one page cost %d requests", n)
	}
}

// A REFUSAL IS AN ERROR, never an empty thread. Slack reports every failure
// as HTTP 200 with a code in the body, so a caller that did not read the
// envelope would render a missing history scope as a conversation nobody had.
func TestARefusedThreadReadIsAnError(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.refuse["conversations.replies"] = "not_in_channel"

	client, _ := slack.NewClient("xoxb-swe", rewriting(t, ws))
	got, err := client.Replies(t.Context(), "C0ENG", "1.1")
	if err == nil {
		t.Fatalf("a refused read returned %+v and no error", got)
	}
	if !strings.Contains(err.Error(), "not_in_channel") {
		t.Fatalf("the refusal does not name the cause: %v", err)
	}
}

// An incomplete call is refused before it reaches the network, where it would
// become a Slack error code rather than a caller's mistake.
func TestAThreadReadNeedsAChannelAndAThread(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	client, _ := slack.NewClient("xoxb-swe", rewriting(t, ws))
	for name, args := range map[string][2]string{
		"no channel": {"", "1.1"},
		"no thread":  {"C0ENG", ""},
	} {
		if _, err := client.Replies(t.Context(), args[0], args[1]); err == nil {
			t.Errorf("%s was sent anyway", name)
		}
	}
	if ws.called("conversations.replies") != 0 {
		t.Error("an incomplete read reached the workspace")
	}
}

// ONE MESSAGE RULE ACROSS BOTH DECODERS.
//
// The Events API delivers a message as an untyped map and conversations.replies
// returns it as a [slack.Reply]. A join line that wakes nobody but renders
// into a seat's thread block is a divergence nothing would report, because
// the block degrades silently by design.
func TestOneMessageRuleBothWays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reply slack.Reply
		raw   map[string]any
		skip  bool
		body  string
	}{
		"an ordinary message": {
			reply: slack.Reply{TS: "1.1", Text: "hello"},
			raw:   map[string]any{"ts": "1.1", "text": "hello"},
			body:  "hello",
		},
		"an agent's own echo": {
			reply: slack.Reply{TS: "1.2", Subtype: "bot_message", Text: "on it"},
			raw:   map[string]any{"ts": "1.2", "subtype": "bot_message", "text": "on it"},
			body:  "on it",
		},
		"a join line": {
			reply: slack.Reply{TS: "1.3", Subtype: "channel_join", Text: "ana joined"},
			raw:   map[string]any{"ts": "1.3", "subtype": "channel_join", "text": "ana joined"},
			skip:  true, body: "ana joined",
		},
		"a hidden edit": {
			reply: slack.Reply{TS: "1.4", Hidden: true, Text: "x"},
			raw:   map[string]any{"ts": "1.4", "hidden": true, "text": "x"},
			skip:  true, body: "x",
		},
		"a file with no comment": {
			reply: slack.Reply{TS: "1.5", Files: []struct {
				Name  string `json:"name"`
				Title string `json:"title"`
			}{{Name: "trace.log"}}},
			raw: map[string]any{"ts": "1.5", "files": []any{
				map[string]any{"name": "trace.log"}}},
			body: "(shared file: trace.log)",
		},
	} {
		if got := tc.reply.Skip() != ""; got != tc.skip {
			t.Errorf("%s: Reply.Skip says skip=%v", name, got)
		}
		if got := slack.SkipReason(tc.raw) != ""; got != tc.skip {
			t.Errorf("%s: SkipReason says skip=%v", name, got)
		}
		if got := tc.reply.Body(); got != tc.body {
			t.Errorf("%s: Reply.Body = %q, want %q", name, got, tc.body)
		}
		if got := slack.Text(tc.raw); got != tc.body {
			t.Errorf("%s: Text = %q, want %q", name, got, tc.body)
		}
	}
}

// OWN-MESSAGE DETECTION NEEDS BOTH IDS.
//
// A bot_message echo of this seat's own post carries the APP id and NO user
// id at all, so a check on the bot user id alone misses exactly the messages
// a seat most has to recognise — its own earlier replies. One method, because
// the parser suppresses them and the thread reader MARKS them, and the two
// disagreeing would present an agent's own replies back to it as a
// colleague's.
func TestASeatRecognisesItselfByEitherID(t *testing.T) {
	t.Parallel()
	seat := slack.Seat{Handle: "swe", BotUserID: botUser, AppID: botApp}
	for name, tc := range map[string]struct {
		user, app string
		own       bool
	}{
		"its own user id":            {botUser, "", true},
		"its own app, no user id":    {"", botApp, true},
		"a colleague agent":          {colleague, "A0APPQA", false},
		"a person":                   {human, "", false},
		"nothing identifying at all": {"", "", false},
	} {
		if got := seat.Owns(tc.user, tc.app); got != tc.own {
			t.Errorf("%s: Owns(%q, %q) = %v", name, tc.user, tc.app, got)
		}
	}

	// A seat with no resolved identity owns nothing, rather than owning
	// every message with an empty id — which would suppress the whole
	// channel.
	if (slack.Seat{Handle: "swe"}).Owns("", "") {
		t.Fatal("an unresolved seat claimed a message as its own")
	}
}
