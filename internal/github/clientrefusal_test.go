package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/httpx"
)

// clientRefusalFor runs one ordinary API read against an endpoint that
// refuses it with the given body, and returns the typed error.
//
// [github.Client.Me] rather than an App call: this is the READ half of the
// package, the one every reconcile pass drives, and it is the half that went
// on capping-and-quoting while the App half was fixed. The two must not
// disagree about what a refusal says.
func clientRefusalFor(t *testing.T, status int, contentType, body string) *github.APIError {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c, err := github.NewClient(github.ClientOptions{APIBase: srv.URL, Token: "ghp-fake"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Me(context.Background())
	var apiErr *github.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("a refused call answered %v, not an *APIError", err)
	}
	return apiErr
}

// A REFUSAL BODY PAST THE CEILING IS NAMED, NEVER QUOTED FROM — ON THE READ
// HALF TOO.
//
// The invariant is the App half's, and the point of this case is that ONE
// vendor package holds ONE rule: `io.ReadAll(io.LimitReader(body,
// RefusalBytes))` fed straight to the refusal helper is a cut wearing a cap's
// name, and it survived here for a whole change while app.go asserted in
// prose that it had been removed. Concretely it failed twice over: a JSON
// error envelope longer than the ceiling arrived severed, failed to
// Unmarshal and was pasted in as a broken prefix, and a page whose </title>
// sat past the ceiling yielded no title at all and an empty Detail reading
// as "GitHub said nothing".
func TestAClientRefusalPastTheCeilingIsNamedRatherThanQuoted(t *testing.T) {
	t.Parallel()
	detail := clientRefusalFor(t, http.StatusBadGateway, "text/html; charset=utf-8",
		oversizedGatewayPage(t, httpx.RefusalBytes)).Detail

	if detail == "" {
		t.Fatal("an endpoint that said too much is reported exactly like one " +
			"that said nothing, which is the distinction this exists to make")
	}
	if !utf8.ValidString(detail) {
		t.Errorf("the detail is not valid UTF-8: %q", detail)
	}
	if !strings.Contains(detail, strconv.Itoa(httpx.RefusalBytes)) {
		t.Errorf("the detail names no ceiling, so a reader cannot tell a "+
			"refused body from a quoted one: %q", detail)
	}
	if strings.Contains(detail, "<div") || strings.Contains(detail, "<html") {
		t.Errorf("the page was pasted into the error: %q", detail)
	}
}

// AN OVERSIZED JSON ENVELOPE IS NOT SILENTLY SEVERED. The cap-then-decode
// shape produced a Detail that was a prefix of an object — unparseable, and
// pasted in as though GitHub had written it.
func TestAClientRefusalDoesNotPasteASeveredEnvelope(t *testing.T) {
	t.Parallel()
	body := `{"message":"` + strings.Repeat("why ", httpx.RefusalBytes) + `"}`
	detail := clientRefusalFor(t, http.StatusUnprocessableEntity,
		"application/json", body).Detail
	if strings.HasPrefix(detail, `{"message":"why`) {
		t.Errorf("a severed JSON prefix reached the detail: %q", detail)
	}
	if !strings.Contains(detail, strconv.Itoa(httpx.RefusalBytes)) {
		t.Errorf("the detail names no ceiling: %q", detail)
	}
}

// WITHIN THE CEILING THE ENDPOINT'S OWN WORDS SURVIVE, AND ITS MARKUP DOES
// NOT — the same answer the App half gives for the same page.
func TestAClientRefusalWithinTheCeilingIsWordsRatherThanMarkup(t *testing.T) {
	t.Parallel()
	page := `<!doctype html><html><head><title>403 Forbidden</title></head><body>` +
		strings.Repeat("<div>nginx</div>", 50) + `</body></html>`
	if len(page) >= httpx.RefusalBytes {
		t.Fatalf("the fixture is %d bytes, past the ceiling this case is about", len(page))
	}
	if got := clientRefusalFor(t, http.StatusForbidden, "text/html", page).Detail; got != "403 Forbidden" {
		t.Errorf("detail = %q, want the page's own title", got)
	}
}

// GITHUB'S OWN JSON ENVELOPE IS NOT COLLATERAL: it is the one body that
// genuinely explains the refusal.
func TestAClientRefusalKeepsGithubsOwnEnvelope(t *testing.T) {
	t.Parallel()
	const body = `{"message":"Bad credentials","documentation_url":"https://example.com"}`
	got := clientRefusalFor(t, http.StatusUnauthorized, "application/json", body).Detail
	if !strings.Contains(got, "Bad credentials") {
		t.Errorf("detail = %q, which lost what GitHub said", got)
	}
}

// AN ERROR LINE IS BOUNDED BY A NAMED CONSTANT AND MARKED WHERE IT BITES.
//
// [httpx.RefusalDetail] is a CEILING — this case is one of the three that
// assert it — so the marker has to fit INSIDE it rather than push past it,
// which is [textcut.Within] rather than Ellipsis. Unmarked, a sentence cut
// at 400 bytes reads as GitHub's complete answer, and the rest of the body
// is unrecoverable: it is read once and dropped, and only GitHub's own logs
// still hold it.
func TestAClientRefusalLineIsBoundedMarkedAndRuneSafe(t *testing.T) {
	t.Parallel()
	// Multi-byte throughout, so a byte slice anywhere inside it is invalid
	// UTF-8 — the property a plain s[:n] has no defence against.
	body := strings.Repeat("é€𝄞 ", 80)
	if len(body) <= httpx.RefusalDetail {
		t.Fatalf("the fixture is %d bytes, inside the bound this case is about", len(body))
	}
	if len(body) >= httpx.RefusalBytes {
		t.Fatalf("the fixture is %d bytes, past the READ ceiling: this case is "+
			"about the LINE bound, not the read one", len(body))
	}
	detail := clientRefusalFor(t, http.StatusInternalServerError,
		"text/plain; charset=utf-8", body).Detail
	if len(detail) > httpx.RefusalDetail {
		t.Errorf("the detail is %d bytes, past httpx.RefusalDetail", len(detail))
	}
	if !utf8.ValidString(detail) {
		t.Errorf("the detail is not valid UTF-8: %q", detail)
	}
	if !strings.HasSuffix(detail, "…") {
		t.Errorf("the detail was cut with nothing saying so: %q", detail)
	}
}

// A TITLELESS PAGE IS A PAGE, NOT SILENCE.
//
// HAProxy's default error page and many WAF pages carry no <title> at all.
// Dropping the markup is right — it is layout — but reporting the drop as ""
// says the endpoint was silent, which is byte for byte what an empty body
// reports and calls for the opposite next step: a silent endpoint is the
// endpoint's problem, a page is the proxy in front of it.
func TestATitlelessPageIsDistinguishableFromAnEmptyBody(t *testing.T) {
	t.Parallel()
	const page = `<html><body><h1>503 Service Unavailable</h1>` +
		`<p>No server is available to handle this request.</p></body></html>`

	silent := clientRefusalFor(t, http.StatusServiceUnavailable, "text/html", "").Detail
	if silent != "" {
		t.Errorf("an empty body produced %q; empty must keep meaning "+
			"\"the endpoint said nothing\"", silent)
	}
	titleless := clientRefusalFor(t, http.StatusServiceUnavailable, "text/html", page).Detail
	if titleless == silent {
		t.Fatal("an endpoint that answered a page is reported exactly like one " +
			"that answered nothing")
	}
	if !strings.Contains(titleless, strconv.Itoa(len(page))) {
		t.Errorf("the detail does not say how much arrived: %q", titleless)
	}
	if strings.Contains(titleless, "<h1") || strings.Contains(titleless, "<body") {
		t.Errorf("the page was pasted into the error: %q", titleless)
	}
}
