package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/mattermost"
)

// refusalFor runs one call against an instance that refuses it with the given
// body, and returns the typed error.
//
// 403 rather than one of [mattermost.RetryStatuses], so the case measures
// what the refusal SAYS rather than waiting out a retry budget.
func refusalFor(t *testing.T, contentType, body string) *mattermost.Error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c, err := mattermost.NewClient(mattermost.ClientOptions{URL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Me(context.Background())
	var apiErr *mattermost.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("a refused call answered %v, not a *mattermost.Error", err)
	}
	return apiErr
}

// "THE SERVER SAID NOTHING" AND "THE SERVER SAID TOO MUCH" ARE DIFFERENT
// FACTS, and an empty Message may only ever mean the first.
//
// This is the defect the cap produced rather than prevented. The body was
// read through an io.LimitReader at [httpx.RefusalBytes] and then
// UNMARSHALLED: a body past the cap arrived as a truncated object,
// json.Unmarshal failed on it, and the Error carried Message: "" — the same
// answer as a server that sent nothing at all. The two call for opposite
// next steps, so conflating them is worse than either.
func TestAnOversizedRefusalIsDistinguishableFromAnEmptyOne(t *testing.T) {
	t.Parallel()
	silent := refusalFor(t, "application/json", "").Message
	if silent != "" {
		t.Errorf("an empty body produced %q; empty must keep meaning "+
			"\"the server said nothing\"", silent)
	}

	oversized := refusalFor(t, "text/html; charset=utf-8",
		oversizedInstancePage(t, httpx.RefusalBytes)).Message
	if oversized == silent {
		t.Fatal("a server that answered past the ceiling is reported exactly " +
			"like one that answered nothing")
	}
	if !utf8.ValidString(oversized) {
		t.Errorf("the message is not valid UTF-8: %q", oversized)
	}
	if !strings.Contains(oversized, strconv.Itoa(httpx.RefusalBytes)) {
		t.Errorf("the message names no ceiling, so a reader cannot tell a "+
			"refused body from a quoted one: %q", oversized)
	}
	if strings.Contains(oversized, "<div") || strings.Contains(oversized, "<html") {
		t.Errorf("the page was pasted into the error: %q", oversized)
	}
}

// MATTERMOST'S OWN ENVELOPE STILL WINS. Its `message` is written for a
// person, which is what turns "403 on /users/me" into "Invalid or expired
// session".
func TestARefusalPrefersTheInstancesOwnMessage(t *testing.T) {
	t.Parallel()
	const body = `{"id":"api.context.session_expired.app_error",` +
		`"message":"Invalid or expired session, please login again."}`
	got := refusalFor(t, "application/json", body).Message
	if !strings.Contains(got, "Invalid or expired session") {
		t.Errorf("message = %q, which lost what the instance said", got)
	}
}

// A SHAPE THIS BUILD DID NOT KNOW IS STILL AN ANSWER. A Mattermost behind a
// proxy answers HTML and a load balancer answers a sentence; dropping either
// for not being the expected envelope reports it as silence.
func TestARefusalInAnUnknownShapeIsNotReportedAsSilence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, contentType, body, want string
	}{
		{"a gateway's page", "text/html",
			`<!doctype html><html><head><title>502 Bad Gateway</title></head>` +
				`<body>` + strings.Repeat("<div>x</div>", 20) + `</body></html>`,
			"502 Bad Gateway"},
		{"a load balancer's sentence", "text/plain",
			"no healthy upstream", "no healthy upstream"},
		{"json that is not the envelope", "application/json",
			`{"error":"nope"}`, `{"error":"nope"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := refusalFor(t, c.contentType, c.body).Message; got != c.want {
				t.Errorf("message = %q, want %q", got, c.want)
			}
		})
	}
}

// AN ERROR LINE IS BOUNDED BY A NAMED CONSTANT, and MARKED where it bites.
// The read ceiling already bounds the body, but [httpx.RefusalBytes] is five
// times what an error line should carry — and a severed sentence with nothing
// saying it was severed reads as a complete one.
//
// The bound is a CEILING rather than a guide, so the marker fits INSIDE it:
// [textcut.Within], not Ellipsis. One marking policy for one value class —
// this arm and the shared fallback beside it cut the same way, because a
// commit shipping two policies for one value is how they stop agreeing.
func TestARefusalMessageIsBoundedMarkedAndRuneSafe(t *testing.T) {
	t.Parallel()
	// Multi-byte throughout, so a byte slice anywhere inside it is invalid
	// UTF-8 — the property a plain s[:n] has no defence against.
	long := strings.Repeat("é€𝄞 ", 100)
	if len(long) <= httpx.RefusalDetail {
		t.Fatalf("the fixture is %d bytes, inside the bound this case is about", len(long))
	}
	body, err := jsonEnvelope(long)
	if err != nil {
		t.Fatal(err)
	}
	got := refusalFor(t, "application/json", body).Message
	if len(got) > httpx.RefusalDetail {
		t.Errorf("the message is %d bytes, past httpx.RefusalDetail — the "+
			"marker has to fit inside the ceiling, not push past it", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("the message is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("the message was cut with nothing saying so: %q", got)
	}
}

// A TITLELESS PAGE IS A PAGE, NOT SILENCE.
//
// This is the conflation the change above removed from the oversized case and
// left in this one. HAProxy's default error page and many WAF pages carry no
// <title> at all, so `titleOf` finds nothing and — by design — yields "":
// measured, a 115-byte titleless page reported byte for byte like an empty
// body. Dropping the markup stays right; reporting the drop as silence does
// not, because a silent instance is the instance's problem and a page is the
// proxy in front of it, and they call for opposite next steps.
func TestATitlelessPageIsDistinguishableFromAnEmptyBody(t *testing.T) {
	t.Parallel()
	const page = `<html><body><h1>503 Service Unavailable</h1>` +
		`<p>No server is available to handle this request.</p></body></html>`

	silent := refusalFor(t, "text/html", "").Message
	if silent != "" {
		t.Errorf("an empty body produced %q; empty must keep meaning "+
			"\"the server said nothing\"", silent)
	}
	titleless := refusalFor(t, "text/html", page).Message
	if titleless == silent {
		t.Fatal("a server that answered a page is reported exactly like one " +
			"that answered nothing — the Error.Message field doc says empty " +
			"means an empty body and nothing else")
	}
	if !strings.Contains(titleless, strconv.Itoa(len(page))) {
		t.Errorf("the message does not say how much arrived: %q", titleless)
	}
	if strings.Contains(titleless, "<h1") || strings.Contains(titleless, "<body") {
		t.Errorf("the page was pasted into the error: %q", titleless)
	}
}

// THE SHARED FALLBACK MARKS ITS CUT TOO, not only the envelope arm above.
//
// A load balancer's plain sentence does not go through Mattermost's envelope,
// so it is cut by the shared helper — which used the NO-MARKER variant, whose
// own doc scopes it to a fixed-width field or a fingerprint input. One value
// class, one marking policy.
func TestAPlainRefusalIsCutWithAMarkerToo(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é€𝄞 ", 80)
	if len(long) <= httpx.RefusalDetail {
		t.Fatalf("the fixture is %d bytes, inside the bound this case is about", len(long))
	}
	if len(long) >= httpx.RefusalBytes {
		t.Fatalf("the fixture is %d bytes, past the READ ceiling: this case is "+
			"about the LINE bound, not the read one", len(long))
	}
	got := refusalFor(t, "text/plain; charset=utf-8", long).Message
	if len(got) > httpx.RefusalDetail {
		t.Errorf("the message is %d bytes, past httpx.RefusalDetail", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("the message is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("the message was cut with nothing saying so: %q", got)
	}
}

// oversizedInstancePage is an HTML error page longer than ceiling, arranged
// so that a byte slice AT the ceiling lands inside a four-byte rune.
//
// The straddle is the point of the fixture rather than decoration: a cut that
// happens to land on a rune boundary proves nothing about rune safety.
func oversizedInstancePage(t *testing.T, ceiling int) string {
	t.Helper()
	const head = `<!doctype html><html><head><title>502 Bad Gateway</title></head><body>`
	const wide = "𝄞" // four bytes
	filler := ceiling - len(head) - 2
	if filler <= 0 {
		t.Fatalf("ceiling %d is too small to build a page around", ceiling)
	}
	page := head + strings.Repeat("x", filler) + wide +
		strings.Repeat("y", 64) + `</body></html>`
	if len(page) <= ceiling {
		t.Fatalf("the fixture is %d bytes, inside the ceiling it must exceed", len(page))
	}
	if utf8.ValidString(page[:ceiling]) {
		t.Fatal("the fixture does not straddle a rune at the ceiling, so it " +
			"would not prove a byte slice there was unsafe")
	}
	return page
}

// jsonEnvelope wraps text in Mattermost's error shape, encoded rather than
// concatenated so the fixture cannot be the thing that is malformed.
func jsonEnvelope(message string) (string, error) {
	encoded, err := json.Marshal(struct {
		Message string `json:"message"`
	}{message})
	return string(encoded), err
}

// WHAT THE LINE COULD NOT HOLD IS LOGGED WHOLE, on both arms, because the
// log is the only place it outlives the attempt: the body is read once and
// dropped, and [mattermost.Error] carries the line alone.
func TestTheWholeOfAShortenedRefusalIsLogged(t *testing.T) {
	t.Parallel()
	envelope, err := jsonEnvelope("Mattermost's own words, unique to this case: " +
		strings.Repeat("é", 300))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		arm, contentType, body, said string
	}{
		{"Mattermost's own envelope", "application/json", envelope,
			"Mattermost's own words, unique to this case: " + strings.Repeat("é", 300)},
		{"a shape this build does not know", "text/plain",
			"a load balancer's sentence, unique to this case: " + strings.Repeat("é", 300) + "\n",
			"a load balancer's sentence, unique to this case: " + strings.Repeat("é", 300)},
	} {
		t.Run(c.arm, func(t *testing.T) {
			t.Parallel()
			line := refusalFor(t, c.contentType, c.body).Message
			if !strings.HasSuffix(line, "…") {
				t.Fatalf("the fixture was not cut, so this case asserts nothing: %q", line)
			}
			var found map[string]any
			for _, record := range logs.records(t, "mattermost_refusal_shortened") {
				if record["message"] == line {
					found = record
				}
			}
			if found == nil {
				t.Fatalf("no mattermost_refusal_shortened record carries the line %q, "+
					"so what it left out exists nowhere", line)
			}
			if found["said"] != c.said {
				t.Errorf("the logged record says %v, want the whole of it", found["said"])
			}
		})
	}
}

// AND A LINE THAT HOLDS EVERYTHING LOGS NOTHING, or the record stops meaning
// that something was left out.
func TestAWholeRefusalIsNotLoggedAsShortened(t *testing.T) {
	t.Parallel()
	const whole = "a refusal short enough to quote in full, unique to this case"
	body, err := jsonEnvelope(whole)
	if err != nil {
		t.Fatal(err)
	}
	if got := refusalFor(t, "application/json", body).Message; got != whole {
		t.Fatalf("message = %q, want %q unchanged", got, whole)
	}
	for _, record := range logs.records(t, "mattermost_refusal_shortened") {
		if record["message"] == whole {
			t.Errorf("a refusal quoted whole was logged as shortened: %v", record)
		}
	}
}

// "ALREADY A MEMBER" IS RECOGNISED IN THE WHOLE OF WHAT THE SERVER SAID,
// never in the line bounded for a log.
//
// A reconcile's second run gets a 400 for the membership its first run made,
// and reads it as success by the word "already". Decided on
// [mattermost.Error.Message], the word past the line's cut failed that run.
// The fixture is built so the line does NOT carry the word, which is asserted
// first: a line that happened to carry it would pass on the wrong decision.
func TestAnExistingMembershipIsRecognisedPastTheLine(t *testing.T) {
	t.Parallel()
	body, err := jsonEnvelope(strings.Repeat("é", 250) + " This user is already a team member.")
	if err != nil {
		t.Fatal(err)
	}
	if line := refusalFor(t, "application/json", body).Message; strings.Contains(line, "already") {
		t.Fatalf("the line %q carries the word, so this case asserts nothing", line)
	}

	joinAgainst := func(t *testing.T, body string) error {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		c, err := mattermost.NewClient(mattermost.ClientOptions{URL: srv.URL, Token: "tok"})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		return c.AddTeamMember(context.Background(), "team-1", "user-1")
	}
	if err := joinAgainst(t, body); err != nil {
		t.Errorf("AddTeamMember = %v, want the existing membership read as success", err)
	}
	// And the control: a 400 that does not say so is still a refusal.
	if err := joinAgainst(t, `{"message":"Invalid or missing team_id."}`); mattermost.Status(err) != http.StatusBadRequest {
		t.Errorf("AddTeamMember = %v, want the 400 itself", err)
	}
}
