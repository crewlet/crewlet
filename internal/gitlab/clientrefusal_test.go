package gitlab_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/httpx"
)

// readRefusalFor runs one READ call against an instance that refuses it with
// the given body, and returns the typed error.
//
// [gitlab.Client.Me] goes through `get`, which is the half every reconcile
// pass drives — and the half that went on capping-and-quoting for a whole
// change while `send` beside it was fixed and its doc asserted the rule.
func readRefusalFor(t *testing.T, status int, contentType, body string) *gitlab.APIError {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	_, err := client(t, srv.URL).Me(context.Background())
	var apiErr *gitlab.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("a refused call answered %v, not an *APIError", err)
	}
	return apiErr
}

// A REFUSAL BODY PAST THE CEILING IS NAMED, NEVER QUOTED FROM — ON THE READ
// HALF TOO.
//
// One instance, one proxy, one rule. The self-managed-GitLab-behind-a-proxy
// case the write half's doc is written around reaches THIS line on every read
// call, and here the cap was still being read up to and quoted: a prefix of a
// rendered document, byte-sliced so a multi-byte rune straddling the boundary
// left invalid UTF-8, with the read error discarded into `_`.
func TestAReadRefusalPastTheCeilingIsNamedRatherThanQuoted(t *testing.T) {
	t.Parallel()
	detail := readRefusalFor(t, http.StatusBadGateway, "text/html; charset=utf-8",
		oversizedProxyPage(t, httpx.RefusalBytes)).Detail

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

// WITHIN THE CEILING THE INSTANCE'S OWN WORDS SURVIVE, AND ITS MARKUP DOES
// NOT — the same answer the write half gives for the same page.
func TestAReadRefusalWithinTheCeilingIsWordsRatherThanMarkup(t *testing.T) {
	t.Parallel()
	page := `<!doctype html><html><head><title>403 Forbidden</title></head><body>` +
		strings.Repeat("<div>nginx</div>", 50) + `</body></html>`
	if len(page) >= httpx.RefusalBytes {
		t.Fatalf("the fixture is %d bytes, past the ceiling this case is about", len(page))
	}
	if got := readRefusalFor(t, http.StatusForbidden, "text/html", page).Detail; got != "403 Forbidden" {
		t.Errorf("detail = %q, want the page's own title", got)
	}
}

// GITLAB'S OWN ENVELOPE IS NOT COLLATERAL. The status decides what a refusal
// MEANS; the message is what an operator reads, and it is the one body that
// genuinely explains the refusal.
func TestAReadRefusalKeepsGitlabsOwnEnvelope(t *testing.T) {
	t.Parallel()
	const body = `{"message":"403 Forbidden - Your account has been blocked"}`
	got := readRefusalFor(t, http.StatusForbidden, "application/json", body).Detail
	if !strings.Contains(got, "Your account has been blocked") {
		t.Errorf("detail = %q, which lost what GitLab said", got)
	}
}

// AN ERROR LINE IS BOUNDED BY A NAMED CONSTANT AND MARKED WHERE IT BITES.
//
// [httpx.RefusalDetail] is a CEILING, so the marker fits inside it rather
// than pushing past it. Unmarked, a GitLab message cut at 400 bytes reads as
// the instance's complete answer — and the rest of it is unrecoverable here,
// because the body is read once and dropped and only the instance's own logs
// still hold it.
func TestAReadRefusalLineIsBoundedMarkedAndRuneSafe(t *testing.T) {
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
	detail := readRefusalFor(t, http.StatusInternalServerError,
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

// A TITLELESS PAGE IS A PAGE, NOT SILENCE. A proxy in front of a self-managed
// instance frequently sends one — HAProxy's own default error page carries no
// <title> — and reporting it as "" is byte for byte what an empty body
// reports, while calling for the opposite next step.
func TestAReadRefusalTitlelessPageIsNotReportedAsSilence(t *testing.T) {
	t.Parallel()
	const page = `<html><body><h1>503 Service Unavailable</h1>` +
		`<p>No server is available to handle this request.</p></body></html>`

	silent := readRefusalFor(t, http.StatusServiceUnavailable, "text/html", "").Detail
	if silent != "" {
		t.Errorf("an empty body produced %q; empty must keep meaning "+
			"\"the instance said nothing\"", silent)
	}
	titleless := readRefusalFor(t, http.StatusServiceUnavailable, "text/html", page).Detail
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
