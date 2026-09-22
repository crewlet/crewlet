package jira_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/jira"
)

// refusalFor runs one call against an instance that refuses it with the given
// body, and returns the typed error.
//
// Its own server rather than the [recorder] fixture, which sets
// `Content-Type: application/json` on every reply: a 502 does not come from
// Jira at all, it comes from whatever sits in front of it, and what that
// announces is the whole input to the distilling these cases are about.
func refusalFor(t *testing.T, status int, contentType, body string) *jira.APIError {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c, err := jira.NewClient(jira.ClientOptions{URL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_, err = c.Me(context.Background())
	var apiErr *jira.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("a refused call answered %v, not an *APIError", err)
	}
	return apiErr
}

// A REFUSAL BODY PAST THE CEILING IS NAMED, NEVER QUOTED FROM.
//
// A CAP IS NOT A CUT. Read UP TO the ceiling through an io.LimitReader, an
// oversized body is indistinguishable from one that ended there: the page is
// byte-sliced, so a multi-byte rune straddling the boundary leaves invalid
// UTF-8, and nothing in the error says a document was cut in half.
func TestARefusalPastTheCeilingIsNamedRatherThanQuoted(t *testing.T) {
	t.Parallel()
	detail := refusalFor(t, http.StatusBadGateway, "text/html; charset=utf-8",
		oversizedGatewayPage(t, httpx.RefusalBytes)).Detail

	if detail == "" {
		t.Fatal("an instance that said too much is reported exactly like one " +
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

// AN OVERSIZED JIRA ENVELOPE IS NAMED TOO, NOT QUOTED FROM.
//
// This is the failure a capped read produces on the shape Jira itself sends:
// the envelope arrives severed, no longer parses, and what an operator reads
// is half a JSON object presented as Jira's complete answer, with nothing in
// the error saying the rest was never read.
func TestAnOversizedEnvelopeIsNamedRatherThanQuotedSevered(t *testing.T) {
	t.Parallel()
	body := `{"errorMessages":["` + strings.Repeat("why ", httpx.RefusalBytes) + `"]}`

	apiErr := refusalFor(t, http.StatusForbidden, "application/json", body)

	if apiErr.Detail == "" {
		t.Fatal("an oversized envelope arrived as an empty Detail, which is " +
			"what an instance that answered nothing produces")
	}
	if !strings.Contains(apiErr.Detail, strconv.Itoa(httpx.RefusalBytes)) {
		t.Errorf("the detail names no ceiling: %q", apiErr.Detail)
	}
	if !strings.Contains(apiErr.Error(), apiErr.Detail) {
		t.Errorf("Error() = %q, which dropped the detail", apiErr.Error())
	}
}

// WITHIN THE CEILING THE INSTANCE'S OWN WORDS SURVIVE, AND ITS MARKUP DOES
// NOT.
func TestARefusalWithinTheCeilingIsWordsRatherThanMarkup(t *testing.T) {
	t.Parallel()
	page := `<!doctype html><html><head><title>403 Forbidden</title></head><body>` +
		strings.Repeat("<div>sso</div>", 50) + `</body></html>`
	if len(page) >= httpx.RefusalBytes {
		t.Fatalf("the fixture is %d bytes, past the ceiling this case is about", len(page))
	}
	if got := refusalFor(t, http.StatusForbidden, "text/html", page).Detail; got != "403 Forbidden" {
		t.Errorf("detail = %q, want the page's own title", got)
	}
}

// AN ERROR LINE IS BOUNDED BY A NAMED CONSTANT AND MARKED WHERE IT BITES.
//
// [httpx.RefusalDetail] is a CEILING, so the marker fits inside it rather
// than pushing past it. Unmarked, a message cut at 400 bytes reads as the
// instance's complete answer — and the rest is unrecoverable here, because
// the body is read once and dropped and only the instance's own logs hold it.
func TestARefusalLineIsBoundedMarkedAndRuneSafe(t *testing.T) {
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
	detail := refusalFor(t, http.StatusInternalServerError,
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

// A TITLELESS PAGE IS A PAGE, NOT SILENCE. An SSO wall or a WAF in front of a
// Data Center instance frequently sends one, and reporting it as "" is byte
// for byte what an empty body reports while calling for the opposite next
// step.
func TestATitlelessPageIsNotReportedAsSilence(t *testing.T) {
	t.Parallel()
	const page = `<html><body><h1>503 Service Unavailable</h1>` +
		`<p>No server is available to handle this request.</p></body></html>`

	silent := refusalFor(t, http.StatusServiceUnavailable, "text/html", "").Detail
	if silent != "" {
		t.Errorf("an empty body produced %q; empty must keep meaning "+
			"\"the instance said nothing\"", silent)
	}
	titleless := refusalFor(t, http.StatusServiceUnavailable, "text/html", page).Detail
	if titleless == silent {
		t.Fatal("an instance that answered a page is reported exactly like one " +
			"that answered nothing")
	}
	if !strings.Contains(titleless, strconv.Itoa(len(page))) {
		t.Errorf("the detail does not say how much arrived: %q", titleless)
	}
	if strings.Contains(titleless, "<h1") || strings.Contains(titleless, "<body") {
		t.Errorf("the page was pasted into the error: %q", titleless)
	}
}

// oversizedGatewayPage is a page past the ceiling whose bytes straddle a rune
// exactly there, so a case built on it fails if the read ever goes back to
// slicing.
func oversizedGatewayPage(t *testing.T, ceiling int) string {
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
