package atlassian_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
// message cut mid-clause reading as Atlassian's complete answer. The rest is
// asserted in [TestTheWholeOfAShortenedRefusalIsLogged].
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

// EVERY ERROR IN THE LIST IS AN ANSWER, so none is dropped without a count.
//
// The `errors` arm is a list of things Atlassian refused. Quoting the first
// and returning sent an operator to fix one of them and learn about the next
// only from the next refused call.
func TestEveryErrorInTheEnvelopeIsQuotedOrCounted(t *testing.T) {
	t.Parallel()
	t.Run("all of them fit, so all of them are quoted", func(t *testing.T) {
		t.Parallel()
		got := adminRefusalFor(t, http.StatusBadRequest, "application/json",
			`{"errors":[{"title":"Bad Request","detail":"userIds is empty"},`+
				`{"title":"Invalid role"},{"detail":""}]}`).Detail
		if want := "userIds is empty; Invalid role"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
	})
	t.Run("those past the line are counted", func(t *testing.T) {
		t.Parallel()
		// Each entry whole is a sentence; together they are well past one
		// line, so the line keeps what fits and says how many it did not.
		var entries []string
		for i := range 12 {
			entries = append(entries, fmt.Sprintf(
				`{"detail":"permission rule %02d names a resource this key cannot grant"}`, i))
		}
		got := adminRefusalFor(t, http.StatusBadRequest, "application/json",
			`{"errors":[`+strings.Join(entries, ",")+`]}`).Detail
		if len(got) > httpx.RefusalDetail {
			t.Errorf("the detail is %d bytes, past httpx.RefusalDetail", len(got))
		}
		if !strings.HasPrefix(got, "permission rule 00 ") {
			t.Errorf("the first error is not quoted first: %q", got)
		}
		quoted := strings.Count(got, "permission rule ")
		if want := fmt.Sprintf(" (+%d more)", 12-quoted); !strings.HasSuffix(got, want) {
			t.Errorf("detail = %q, which quotes %d of 12 errors and does not "+
				"end %q", got, quoted, want)
		}
		if strings.Contains(got, "…") {
			t.Errorf("an entry was cut to make room for the next one: %q", got)
		}
	})
	t.Run("a first error too long to fit alone is cut, and the rest counted", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("é€𝄞 ", 80)
		got := adminRefusalFor(t, http.StatusBadRequest, "application/json",
			`{"errors":[{"detail":"`+long+`"},{"title":"Invalid role"}]}`).Detail
		if len(got) > httpx.RefusalDetail {
			t.Errorf("the detail is %d bytes, past httpx.RefusalDetail", len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("the detail is not valid UTF-8: %q", got)
		}
		if !strings.HasSuffix(got, "… (+1 more)") {
			t.Errorf("detail = %q, want the first error cut and marked, and the "+
				"second counted", got)
		}
	})
}

// WHAT THE LINE COULD NOT HOLD IS LOGGED WHOLE, because the log is the only
// place it outlives the call: the body is read once and dropped.
func TestTheWholeOfAShortenedRefusalIsLogged(t *testing.T) {
	t.Parallel()
	first := "the first of several: " + strings.Repeat("é", 300)
	second := "and the second, which the line could only count"
	detail := adminRefusalFor(t, http.StatusBadRequest, "application/json",
		`{"errors":[{"detail":"`+first+`"},{"detail":"`+second+`"}]}`).Detail
	if !strings.HasSuffix(detail, "(+1 more)") {
		t.Fatalf("the fixture was not shortened, so this case asserts nothing: %q", detail)
	}

	var found map[string]any
	for _, record := range logs.records(t, "atlassian_refusal_shortened") {
		if record["detail"] == detail {
			found = record
		}
	}
	if found == nil {
		t.Fatalf("no atlassian_refusal_shortened record carries the line %q, so "+
			"what it left out exists nowhere", detail)
	}
	said, _ := found["said"].([]any)
	if len(said) != 2 || said[0] != first || said[1] != second {
		t.Errorf("the logged record says %v, want both errors whole", found["said"])
	}
}

// AND A LINE THAT HOLDS EVERYTHING LOGS NOTHING, or the record stops meaning
// that something was left out.
func TestAWholeRefusalIsNotLoggedAsShortened(t *testing.T) {
	t.Parallel()
	const whole = "a refusal short enough to quote in full, and unique to this case"
	if got := adminRefusalFor(t, http.StatusForbidden, "application/json",
		`{"message":"`+whole+`"}`).Detail; got != whole {
		t.Fatalf("detail = %q, want %q unchanged", got, whole)
	}
	for _, record := range logs.records(t, "atlassian_refusal_shortened") {
		if record["detail"] == whole {
			t.Errorf("a refusal quoted whole was logged as shortened: %v", record)
		}
	}
}

// A MESSAGE BESIDE THE LIST IS NOT THE WHOLE ANSWER. An envelope carrying a
// top-level `message`, a top-level `detail` and an `errors` list said all
// three; a line built from the message alone would drop the rest with no
// count and no record of it.
func TestAMessageBesideTheErrorsListDropsNeither(t *testing.T) {
	t.Parallel()
	t.Run("all of it fits, so all of it is quoted", func(t *testing.T) {
		t.Parallel()
		got := adminRefusalFor(t, http.StatusBadRequest, "application/json",
			`{"message":"Validation failed","detail":"two problems",`+
				`"errors":[{"detail":"userIds is empty"},{"title":"Invalid role"}]}`).Detail
		if want := "Validation failed; two problems; userIds is empty; Invalid role"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
	})
	t.Run("what the line cannot hold is counted and logged whole", func(t *testing.T) {
		t.Parallel()
		const message = "a message beside a list, unique to this case"
		entries := make([]string, 12)
		said := []any{message}
		for i := range entries {
			entry := fmt.Sprintf("rule %02d names a resource this key cannot grant", i)
			entries[i] = `{"detail":"` + entry + `"}`
			said = append(said, entry)
		}
		detail := adminRefusalFor(t, http.StatusBadRequest, "application/json",
			`{"message":"`+message+`","errors":[`+strings.Join(entries, ",")+`]}`).Detail
		if !strings.HasPrefix(detail, message+"; rule 00 ") {
			t.Errorf("detail = %q, want the message first and the list after it", detail)
		}
		if !strings.HasSuffix(detail, " more)") {
			t.Fatalf("the fixture was not shortened, so this case asserts nothing: %q", detail)
		}
		record := shortenedRecordFor(t, detail)
		if got, _ := record["said"].([]any); !slices.Equal(got, said) {
			t.Errorf("the logged record says %v, want the message and all 12 errors whole",
				record["said"])
		}
	})
}

// THE OTHER SHAPES KEEP THEIR REST TOO. A body that is not Atlassian's own
// envelope is cut by [httpx.QuoteRefusal], and what that line left out is
// logged whole by the same record an envelope's is.
func TestAShortenedRefusalInAnotherShapeIsLoggedWhole(t *testing.T) {
	t.Parallel()
	said := "a gateway's sentence, unique to this case: " + strings.Repeat("é", 300)
	detail := adminRefusalFor(t, http.StatusForbidden, "text/plain", said+"\n").Detail
	if !strings.HasSuffix(detail, "…") {
		t.Fatalf("the fixture was not cut, so this case asserts nothing: %q", detail)
	}
	record := shortenedRecordFor(t, detail)
	if got, _ := record["said"].([]any); len(got) != 1 || got[0] != said {
		t.Errorf("the logged record says %v, want the sentence whole", record["said"])
	}
}

// shortenedRecordFor is the atlassian_refusal_shortened record carrying line.
func shortenedRecordFor(t *testing.T, line string) map[string]any {
	t.Helper()
	for _, record := range logs.records(t, "atlassian_refusal_shortened") {
		if record["detail"] == line {
			return record
		}
	}
	t.Fatalf("no atlassian_refusal_shortened record carries the line %q, so "+
		"what it left out exists nowhere", line)
	return nil
}

// grantRefusedWith runs one Grant against an org whose invite answers 404
// with the given body.
func grantRefusedWith(t *testing.T, contentType, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := atlassian.NewClient(atlassian.ClientOptions{BaseURL: srv.URL})
	return c.Grant(context.Background(), "org-key", "org-id", "acct-1",
		atlassian.GrantsFor("cloud-1"))
}

// A NOT-READY ACCOUNT IS RECOGNISED IN EVERYTHING ATLASSIAN SAID, never in
// the line bounded for a log.
//
// The line can cut the entry carrying the words, or only count it. Decided
// there, Grant answered the raw 404 and the seat was reported as failed
// rather than as still coming up. Every case but the first is built so the
// line does NOT carry the words, which is asserted before Grant is: a case
// whose line happened to carry them would pass on the wrong decision.
func TestANotReadyAccountIsRecognisedPastTheLine(t *testing.T) {
	t.Parallel()
	const words = "the account was not found in the directory"
	var counted []string
	for i := range 12 {
		counted = append(counted, fmt.Sprintf(
			`{"detail":"permission rule %02d names a resource this key cannot grant"}`, i))
	}
	counted = append(counted, `{"detail":"`+words+`"}`)

	for _, c := range []struct {
		name, contentType, body string
		inLine                  bool
	}{
		{"said whole on the line", "application/json",
			`{"detail":"` + words + `"}`, true},
		{"in an error the line only counted", "application/json",
			`{"errors":[` + strings.Join(counted, ",") + `]}`, false},
		{"past the cut of a message longer than the line", "application/json",
			`{"message":"` + strings.Repeat("é", 250) + ` ` + words + `"}`, false},
		{"past the cut of an answer in another shape", "text/plain",
			strings.Repeat("é", 250) + " " + words, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			line := adminRefusalFor(t, http.StatusNotFound, c.contentType, c.body).Detail
			if strings.Contains(line, words) != c.inLine {
				t.Fatalf("the line %q is not the fixture this case is about", line)
			}
			if err := grantRefusedWith(t, c.contentType, c.body); !errors.Is(err, atlassian.ErrAccountNotReady) {
				t.Errorf("Grant = %v, want ErrAccountNotReady", err)
			}
		})
	}

	// And the control: a 404 that does not say it is an ordinary refusal.
	err := grantRefusedWith(t, "application/json", `{"message":"no such organization"}`)
	var apiErr *atlassian.APIError
	if errors.Is(err, atlassian.ErrAccountNotReady) || !errors.As(err, &apiErr) {
		t.Errorf("Grant = %v, want the 404 itself", err)
	}
}
