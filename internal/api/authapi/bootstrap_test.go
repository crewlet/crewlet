package authapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// codeDirectory is an empty estate holding the given codes, in whatever state
// each one is — read through the domain's own predicate, so what this fake
// calls live is what the log would.
type codeDirectory struct {
	stubDirectory
	codes []iamdomain.BootstrapCode
}

func (d codeDirectory) OutstandingBootstrapCodes(_ context.Context, now time.Time) (
	[]iamdomain.BootstrapCode, error) {

	var out []iamdomain.BootstrapCode
	for _, code := range d.codes {
		if code.State(now) == iamdomain.CodeLive {
			out = append(out, code)
		}
	}
	return out, nil
}

func (d codeDirectory) BootstrapCode(_ context.Context, id string) (
	iamdomain.BootstrapCode, error) {

	for _, code := range d.codes {
		if code.ID == id {
			return code, nil
		}
	}
	return iamdomain.BootstrapCode{}, nil
}

// bootstrapSurface is the sign-in surface over a store directory holding the
// code file, a directory answering codes, and a writer that records.
func bootstrapSurface(t *testing.T, codes []iamdomain.BootstrapCode,
	writer *recordingWriter) *http.ServeMux {

	t.Helper()
	return bootstrapSurfaceWith(t, codes, writer, true, nil)
}

// bootstrapSurfaceWith is [bootstrapSurface] on a node that may or may not
// hold the code file, with further fakes replaced.
func bootstrapSurfaceWith(t *testing.T, codes []iamdomain.BootstrapCode,
	writer *recordingWriter, file bool,
	replace func(*authapi.Options)) *http.ServeMux {

	t.Helper()
	b := bootstrapFor(t)
	b.Store.Path = filepath.Join(t.TempDir(), "node.db")
	if file {
		if err := os.WriteFile(filepath.Join(filepath.Dir(b.Store.Path),
			authapi.BootstrapCodeFile), []byte(theCode+"\n"), 0o600); err != nil {
			t.Fatalf("write the code: %v", err)
		}
	}
	mux := http.NewServeMux()
	buildWith(t, b, nil, func(o *authapi.Options) {
		o.Directory = codeDirectory{codes: codes}
		o.Writer = writer
		if replace != nil {
			replace(o)
		}
	}).Routes(mux)
	return mux
}

// bootstrapOnce posts the code with a login and answers the status.
func bootstrapOnce(t *testing.T, mux *http.ServeMux, login string) int {
	t.Helper()
	return postBootstrap(t, mux, theCode, login).Code
}

// postBootstrap posts one code with a login and answers the whole response.
func postBootstrap(t *testing.T, mux *http.ServeMux, code, login string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"code": code, "login": login, "email": "founder@example.com",
		"name": "Founder", "password": "a-perfectly-fine-passphrase",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/bootstrap",
		strings.NewReader(string(body))))
	return rec
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

// A CODE THE LOG DOES NOT HOLD AS LIVE CREATES NOBODY, AND SAYS WHY.
//
// The file was the whole check once: a code a day past its lifetime, or one a
// re-issue had withdrawn while its file survived, still created the company's
// first operator carrying the whole ceiling. Then the log became the other
// half, and a real code the log no longer honoured was refused exactly as a
// wrong one — a founder who installed on Friday and opened the dashboard on
// Monday was told they had typed it wrong, and nothing said to re-issue it.
//
// Each dead code is `410 bootstrap_code_stale` now, counted like every failed
// attempt, and never the closed answer; only a code nobody holds is the
// uniform refusal.
//
// Mutation: answer the stale arm with `sign_in_refused` and every row but the
// last fails; answer it with `bootstrap_closed` and the founder is told their
// company has started.
func TestADeadCodeSaysItIsStaleAndCreatesNobody(t *testing.T) {
	t.Parallel()
	dead := func(state iamdomain.CodeState) []iamdomain.BootstrapCode {
		code := iamdomain.BootstrapCode{ID: codeID(), MintedBy: "node-a",
			MintedAt:  clock.Add(-25 * time.Hour),
			ExpiresAt: clock.Add(time.Hour)}
		switch state {
		case iamdomain.CodeAgedOut:
			code.ExpiresAt = clock.Add(-time.Hour)
		case iamdomain.CodeWithdrawn:
			code.SpentAt = clock.Add(-time.Minute)
		case iamdomain.CodeAbsent:
			return nil
		}
		return []iamdomain.BootstrapCode{code}
	}
	for _, tc := range []struct {
		name  string
		codes []iamdomain.BootstrapCode
		file  bool
		want  int
		code  string
	}{
		{"aged out", dead(iamdomain.CodeAgedOut), true, http.StatusGone,
			"bootstrap_code_stale"},
		{"withdrawn by a re-issue", dead(iamdomain.CodeWithdrawn), true,
			http.StatusGone, "bootstrap_code_stale"},
		// SERVED BY A NODE THAT DOES NOT HOLD THE FILE: the log is the
		// check, so it answers exactly as the node that wrote it does.
		{"aged out, on another node", dead(iamdomain.CodeAgedOut), false,
			http.StatusGone, "bootstrap_code_stale"},
		// THIS NODE WROTE IT and its hash never reached the log.
		{"never published", dead(iamdomain.CodeAbsent), true,
			http.StatusGone, "bootstrap_code_stale"},
		// NOBODY HOLDS IT: the uniform refusal, like any wrong code.
		{"nothing anywhere", dead(iamdomain.CodeAbsent), false,
			http.StatusUnauthorized, "sign_in_refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writer := &recordingWriter{}
			audit := &recordingAudit{}
			mux := bootstrapSurfaceWith(t, tc.codes, writer, tc.file,
				func(o *authapi.Options) { o.Audit = audit })
			rec := postBootstrap(t, mux, theCode, "founder.one")
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(),
				`"error":"`+tc.code+`"`) {
				t.Errorf("answered %d %s, want %d %s", rec.Code,
					rec.Body.String(), tc.want, tc.code)
			}
			if len(writer.enrolled) != 0 {
				t.Errorf("a dead code enrolled %+v", writer.enrolled)
			}
			// A FAILED ATTEMPT, whichever arm refused it.
			if _, failures := audit.snapshot(); len(failures) != 1 {
				t.Errorf("the refusal reached the failure tally %d times, "+
					"want once", len(failures))
			}
		})
	}
}

// ANY NODE REDEEMS ANY NODE'S CODE.
//
// The route compared the code against the SERVING node's own file, so on a
// fleet behind a load balancer every node but the one that wrote it refused a
// live code as a wrong one. The log is the check.
//
// Mutation: compare against this node's file before the log and the founder
// is refused here.
func TestAnyNodeRedeemsACodeAnotherNodeWrote(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	mux := bootstrapSurfaceWith(t, []iamdomain.BootstrapCode{{
		ID: codeID(), MintedBy: "node-b", MintedAt: clock.Add(-time.Hour),
		ExpiresAt: clock.Add(time.Hour),
	}}, writer, false, nil)
	if rec := postBootstrap(t, mux, theCode, "founder.one"); rec.Code != http.StatusOK {
		t.Fatalf("a live code written on another node answered %d: %s",
			rec.Code, rec.Body.String())
	}
	if len(writer.enrolled) != 1 || writer.enrolled[0].BootstrapCode != codeID() {
		t.Errorf("the founder was enrolled as %+v, want once naming the code",
			writer.enrolled)
	}
}

// A CODE PASTED WITH ITS NEWLINE IS THE CODE IN THE FILE. The file's own read
// trims surrounding whitespace, so the presented one is trimmed too.
func TestACodePastedWithItsNewlineIsTheCode(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	mux := bootstrapSurfaceWith(t, []iamdomain.BootstrapCode{{
		ID: codeID(), MintedAt: clock.Add(-time.Hour),
		ExpiresAt: clock.Add(time.Hour),
	}}, writer, true, nil)
	if rec := postBootstrap(t, mux, " "+theCode+"\n", "founder.one"); rec.Code != http.StatusOK {
		t.Errorf("a code pasted with surrounding whitespace answered %d: %s",
			rec.Code, rec.Body.String())
	}
}
