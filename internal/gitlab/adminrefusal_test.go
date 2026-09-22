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

// adminRefusalFor runs one write call against an instance that refuses it
// with the given body, and returns the typed error.
//
// The write half is the subject: [gitlab.Client.DeleteGroupHook] goes through
// `send`, which is the path a provisioning run takes and the one that pasted
// a rendered page into an operator's error.
func adminRefusalFor(t *testing.T, status int, contentType, body string) *gitlab.APIError {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	err := client(t, srv.URL).DeleteGroupHook(context.Background(), 1, 2)
	var apiErr *gitlab.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("a refused call answered %v, not an *APIError", err)
	}
	return apiErr
}

// A REFUSAL BODY PAST THE CEILING IS NAMED, NEVER QUOTED FROM.
//
// The invariant: an instance that answers more than this build reads must
// produce a Detail that is present, valid UTF-8, and says so — carrying the
// ceiling that refused it. Reading UP TO [httpx.RefusalBytes] and quoting the
// result is what this replaced, and a self-managed GitLab behind a proxy is
// exactly where it bit: the quote was a prefix of a rendered document with
// nothing marking it as a prefix, the prefix was taken with a byte slice so a
// multi-byte rune straddling the boundary left invalid UTF-8, and the read
// error was discarded so a body that failed halfway became an empty Detail.
func TestAnAdminRefusalPastTheCeilingIsNamedRatherThanQuoted(t *testing.T) {
	t.Parallel()
	detail := adminRefusalFor(t, http.StatusBadGateway, "text/html; charset=utf-8",
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
// NOT. A proxy puts the reason in the document's <title>; the rest is layout,
// and pasting it into an error puts a rendered page in a log around a
// sentence nobody can find.
func TestAnAdminRefusalWithinTheCeilingIsWordsRatherThanMarkup(t *testing.T) {
	t.Parallel()
	page := `<!doctype html><html><head><title>403 Forbidden</title></head><body>` +
		strings.Repeat("<div>nginx</div>", 50) + `</body></html>`
	if len(page) >= httpx.RefusalBytes {
		t.Fatalf("the fixture is %d bytes, past the ceiling this case is about", len(page))
	}
	if got := adminRefusalFor(t, http.StatusForbidden, "text/html", page).Detail; got != "403 Forbidden" {
		t.Errorf("detail = %q, want the page's own title", got)
	}
}

// GITLAB'S OWN ENVELOPE IS NOT COLLATERAL. A provisioning run decides what a
// refusal MEANS from the status, but an operator reads the message — and it
// is the one body that genuinely explains the refusal.
func TestAnAdminRefusalKeepsGitlabsOwnEnvelope(t *testing.T) {
	t.Parallel()
	const body = `{"message":"403 Forbidden - Your account has been blocked"}`
	got := adminRefusalFor(t, http.StatusForbidden, "application/json", body).Detail
	if !strings.Contains(got, "Your account has been blocked") {
		t.Errorf("detail = %q, which lost what GitLab said", got)
	}
}

// AN ERROR LINE IS BOUNDED BY A NAMED CONSTANT AND MARKED WHERE IT BITES.
//
// [httpx.RefusalDetail] is that constant and its reason is at its definition:
// a refusal that needs more than this to be understood needs the instance's own
// logs. It is a CEILING rather than a guide — this case is what makes it one
// — so the marker fits INSIDE it, which is [textcut.Within] rather than
// Ellipsis. The marker is not decoration: the body is read once and dropped,
// nothing in this process can recover the rest, and an unmarked cut is a
// severed sentence read as the instance's complete answer.
func TestAnAdminRefusalLineIsBoundedMarkedAndRuneSafe(t *testing.T) {
	t.Parallel()
	// Multi-byte throughout, so a byte slice anywhere inside it is invalid
	// UTF-8 — the property the old string(detail) had no defence against.
	body := strings.Repeat("é€𝄞 ", 200)
	if len(body) <= httpx.RefusalDetail {
		t.Fatalf("the fixture is %d bytes, inside the bound this case is about", len(body))
	}
	detail := adminRefusalFor(t, http.StatusInternalServerError,
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

// oversizedProxyPage is an HTML error page longer than ceiling, arranged so
// that a byte slice AT the ceiling lands inside a four-byte rune.
//
// The straddle is the point of the fixture rather than decoration: a cut that
// happens to land on a rune boundary proves nothing about rune safety, and
// ASCII fixtures are why four separate truncation helpers in this tree
// shipped with the same bug for as long as they did.
func oversizedProxyPage(t *testing.T, ceiling int) string {
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
			"would not prove the old byte slice was unsafe")
	}
	return page
}
