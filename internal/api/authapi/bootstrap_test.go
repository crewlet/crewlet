package authapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// theCode is the one-time code these cases write beside the store.
const theCode = "a-bootstrap-code-with-enough-entropy-to-be-one"

// codeDirectory is an empty estate holding the given outstanding codes.
type codeDirectory struct {
	stubDirectory
	codes []iamdomain.BootstrapCode
}

func (d codeDirectory) OutstandingBootstrapCodes(context.Context, time.Time) (
	[]iamdomain.BootstrapCode, error) {

	return d.codes, nil
}

// bootstrapSurface is the sign-in surface over a store directory holding the
// code file, a directory answering codes, and a writer that records.
func bootstrapSurface(t *testing.T, codes []iamdomain.BootstrapCode,
	writer *recordingWriter) *http.ServeMux {

	t.Helper()
	b := bootstrapFor(t)
	b.Store.Path = filepath.Join(t.TempDir(), "node.db")
	if err := os.WriteFile(filepath.Join(filepath.Dir(b.Store.Path),
		authapi.BootstrapCodeFile), []byte(theCode+"\n"), 0o600); err != nil {
		t.Fatalf("write the code: %v", err)
	}
	mux := http.NewServeMux()
	buildWith(t, b, nil, func(o *authapi.Options) {
		o.Directory = codeDirectory{codes: codes}
		o.Writer = writer
	}).Routes(mux)
	return mux
}

// bootstrapOnce posts the code with a login and answers the status.
func bootstrapOnce(t *testing.T, mux *http.ServeMux, login string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/bootstrap",
		strings.NewReader(`{"code":"`+theCode+`","login":"`+login+`",`+
			`"email":"founder@example.com","name":"Founder",`+
			`"password":"a-perfectly-fine-passphrase"}`)))
	return rec.Code
}

// codeID is the digest the log carries for theCode.
func codeID() string {
	sum := sha256.Sum256([]byte(theCode))
	return hex.EncodeToString(sum[:])
}

// A FOUNDER'S RETRY FINISHES THE BOOTSTRAP ITS FIRST ATTEMPT STARTED.
//
// The first operator was minted per request, so a bootstrap refused after its
// address claim — a login outside the grammar, a record that did not land —
// left the founder's address held for an id no retry named, and every
// corrected retry was refused as "that address belongs to somebody". The
// founder is DERIVED from the code and the instant the log minted it, so every
// attempt with one code names one person.
func TestAFoundersRetryFinishesTheBootstrapItStarted(t *testing.T) {
	t.Parallel()
	minted := clock.Add(-time.Hour)
	writer := &recordingWriter{refusals: []error{&iamdomain.ErrClaimed{
		Kind: iamdomain.KindLogin, Token: "founder.one", Holder: "somebody"}}}
	mux := bootstrapSurface(t, []iamdomain.BootstrapCode{{
		ID: codeID(), MintedBy: "node-a", ExpiresAt: clock.Add(time.Hour),
		MintedAt: minted,
	}}, writer)

	if got := bootstrapOnce(t, mux, "founder.one"); got != http.StatusConflict {
		t.Fatalf("the first attempt answered %d, want 409", got)
	}
	if got := bootstrapOnce(t, mux, "founder.two"); got != http.StatusOK {
		t.Fatalf("the retry answered %d, want 200", got)
	}
	want := iamdomain.BootstrappedPersonID(codeID(), minted)
	if len(writer.enrolled) != 2 {
		t.Fatalf("enrolments %d, want 2", len(writer.enrolled))
	}
	for i, in := range writer.enrolled {
		if in.PersonID != want {
			t.Errorf("attempt %d enrolled %s, want the code's founder %s",
				i+1, in.PersonID, want)
		}
	}
	if len(writer.bootstrap) != 1 || writer.bootstrap[0].Person != want {
		t.Errorf("the code was spent as %+v, want once naming %s",
			writer.bootstrap, want)
	}
}

// A CODE THE LOG DOES NOT HOLD AS OUTSTANDING CREATES NOBODY.
//
// The file was the whole check: a code a day past its lifetime, or one a
// re-issue had withdrawn while its file survived, still created the company's
// first operator carrying the whole ceiling. The mint record is the other half
// — minted, not withdrawn, not spent, not aged out — and a code without one is
// refused exactly as a wrong code is.
func TestACodeTheLogDoesNotHoldCreatesNobody(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	mux := bootstrapSurface(t, nil, writer)
	if got := bootstrapOnce(t, mux, "founder.one"); got != http.StatusUnauthorized {
		t.Errorf("a code the log does not hold answered %d, want 401", got)
	}
	if len(writer.enrolled) != 0 {
		t.Errorf("a code the log does not hold enrolled %+v", writer.enrolled)
	}
}
