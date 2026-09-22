package atlassian_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/httpx"
)

// adminRefusalFor runs one admin-API read against an org that refuses it with
// the given body, and returns the typed error.
func adminRefusalFor(t *testing.T, status int, contentType, body string) *atlassian.APIError {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := atlassian.NewClient(atlassian.ClientOptions{BaseURL: srv.URL})
	_, err := c.ListServiceAccounts(context.Background(), "org-key", "org-id")
	var apiErr *atlassian.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("a refused call answered %v, not an *APIError", err)
	}
	return apiErr
}

// ATLASSIAN'S OWN ENVELOPE IS BOUNDED AND MARKED, not returned verbatim.
//
// `message` is a string Atlassian chooses and this value becomes
// [atlassian.APIError.Detail], which reaches a log line and a reconcile
// finding. Returned verbatim it could put a multi-megabyte string into both —
// the refusal body used to be read at [httpx.MaxResponseBody], five orders of
// magnitude above what an error LINE should carry. [httpx.RefusalDetail] is
// the tree's named answer for that length, and the marker is what stops a
// message cut mid-clause reading as Atlassian's complete answer: the body is
// read once and dropped, so only Atlassian's own admin audit log still holds
// the rest.
func TestTheVendorEnvelopeIsBoundedMarkedAndRuneSafe(t *testing.T) {
	t.Parallel()
	// Multi-byte throughout, so a byte slice anywhere inside it is invalid
	// UTF-8 — the property a plain s[:n] has no defence against.
	long := strings.Repeat("é€𝄞 ", 80)
	if len(long) <= httpx.RefusalDetail {
		t.Fatalf("the fixture is %d bytes, inside the bound this case is about", len(long))
	}
	detail := adminRefusalFor(t, http.StatusForbidden, "application/json",
		`{"message":"`+long+`"}`).Detail

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

// A SHORT ENVELOPE IS UNTOUCHED. The bound must not start marking answers
// that were never cut, or the marker stops meaning anything.
func TestAShortVendorEnvelopeIsNotMarked(t *testing.T) {
	t.Parallel()
	const want = "The organization does not have this product."
	got := adminRefusalFor(t, http.StatusForbidden, "application/json",
		`{"message":"`+want+`"}`).Detail
	if got != want {
		t.Errorf("detail = %q, want %q unchanged", got, want)
	}
}

// A REFUSAL IS READ AT THE REFUSAL CEILING, NOT THE PAYLOAD ONE.
//
// The status is known before the body is, so the ceiling follows it: a
// refused answer is an explanation, and past [httpx.RefusalBytes] it is a
// rendered document that arrived instead of one. Read at the SUCCESS ceiling
// — which is what happened while one ReadBody served both — an Atlassian
// admin 403 behind an SSO wall could put 32 MiB of markup through the
// distiller. The overrun is NAMED, carrying the ceiling that refused it,
// rather than quoted from.
func TestARefusalIsReadAtTheRefusalCeiling(t *testing.T) {
	t.Parallel()
	page := `<!doctype html><html><head><title>403 Forbidden</title></head><body>` +
		strings.Repeat("<div>sso</div>", httpx.RefusalBytes) + `</body></html>`
	detail := adminRefusalFor(t, http.StatusForbidden, "text/html", page).Detail

	if !strings.Contains(detail, strconv.Itoa(httpx.RefusalBytes)) {
		t.Errorf("the detail names no ceiling, so the body was read at the "+
			"payload ceiling rather than the refusal one: %q", detail)
	}
	if strings.Contains(detail, "<div") || strings.Contains(detail, "<html") {
		t.Errorf("the page was pasted into the error: %q", detail)
	}
	if !utf8.ValidString(detail) {
		t.Errorf("the detail is not valid UTF-8: %q", detail)
	}
}

// WITHIN THE CEILING A PAGE STILL YIELDS ITS TITLE AND NOT ITS MARKUP.
func TestARefusalWithinTheCeilingIsWordsRatherThanMarkup(t *testing.T) {
	t.Parallel()
	page := `<!doctype html><html><head><title>403 Forbidden</title></head><body>` +
		strings.Repeat("<div>sso</div>", 50) + `</body></html>`
	if len(page) >= httpx.RefusalBytes {
		t.Fatalf("the fixture is %d bytes, past the ceiling this case is about", len(page))
	}
	if got := adminRefusalFor(t, http.StatusForbidden, "text/html", page).Detail; got != "403 Forbidden" {
		t.Errorf("detail = %q, want the page's own title", got)
	}
}

// A TITLELESS PAGE IS A PAGE, NOT SILENCE — the same answer the other three
// vendors on this rule give, because it is one rule in one place.
func TestATitlelessPageIsDistinguishableFromAnEmptyBody(t *testing.T) {
	t.Parallel()
	const page = `<html><body><h1>503 Service Unavailable</h1>` +
		`<p>No server is available to handle this request.</p></body></html>`

	silent := adminRefusalFor(t, http.StatusServiceUnavailable, "text/html", "").Detail
	if silent != "" {
		t.Errorf("an empty body produced %q; empty must keep meaning "+
			"\"Atlassian said nothing\"", silent)
	}
	titleless := adminRefusalFor(t, http.StatusServiceUnavailable, "text/html", page).Detail
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
