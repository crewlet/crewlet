package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
)

// Credentials of the three shapes [redact.Secrets] recognises none of by
// sight — an OAuth access token (a JWT), an OAuth refresh token and a Google
// API key — which are exactly what a subscription CLI's login holds. Every
// value is invented.
const (
	exampleJWT     = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJleGFtcGxlIn0.c2lnbmF0dXJlLWV4YW1wbGU"
	exampleRefresh = "rt_ExampleRefreshTokenAbCdEfGh0123456789"
	exampleGoogle  = "AIzaSyExampleExampleExampleExample0123"
)

// exampleSecrets is every credential above, for an assertion that none of them
// reached an error.
var exampleSecrets = []string{exampleJWT, exampleRefresh, exampleGoogle}

// exampleDocument is a login document holding all three, as a CLI's export
// would print one.
func exampleDocument(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal([]map[string]any{{
		"id": "cred_1", "integrationID": "openai", "label": "subscription",
		"value": map[string]string{"type": "oauth", "access": exampleJWT, "refresh": exampleRefresh},
	}, {
		"id": "cred_2", "integrationID": "google", "label": "API key",
		"value": map[string]string{"type": "key", "key": exampleGoogle},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// assertNoSecret fails when err repeats any of the example credentials — whole,
// or any piece of one [minTokenRun] characters long — or the document they came
// in.
func assertNoSecret(t *testing.T, err error, document string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error to inspect")
	}
	for _, secret := range exampleSecrets {
		for i := 0; i+minTokenRun <= len(secret); i++ {
			if strings.Contains(err.Error(), secret[i:i+minTokenRun]) {
				t.Errorf("the failure repeats a piece of the credential %q (from byte %d): %v",
					secret[:8], i, err)
				break
			}
		}
	}
	if document != "" && strings.Contains(err.Error(), document) {
		t.Errorf("the failure repeats the whole document: %v", err)
	}
}

// AN IMPORT THE CLI REFUSES REPEATS NONE OF THE LOGIN IT WAS HANDED, whatever
// its stderr quotes — including the credential shapes no redaction rule knows,
// because what an import is handed is exactly what the engine knows is secret.
//
// Mutation: drop the token runs of a text that is not JSON (secretsIn), and
// the truncated cases are red; match a token run only whole (maskShared), and
// the window that cuts one is.
func TestARefusedImportRepeatsNoneOfTheLogin(t *testing.T) {
	t.Parallel()
	document := exampleDocument(t)
	cutAfter := func(secret string) string {
		return document[:strings.Index(document, secret)+len(secret)]
	}
	for name, c := range map[string]struct {
		doc string
		env map[string]string
	}{
		// Quoted whole.
		"a document the CLI rejects": {document, map[string]string{"FAKE_STORE_IMPORT_REJECT": "1"}},
		// Truncated, so it is not JSON at all, and the fake quotes the window
		// it stopped in — the Google key whole, and none of the document
		// around it: what is scrubbed then is every credential-shaped run in
		// it, wherever stderr repeats one.
		"a truncated document": {cutAfter(exampleGoogle), nil},
		// Truncated at the end of the JWT, so the window begins INSIDE it and
		// keeps its last forty characters, which no whole run matches.
		"a document cut inside a credential": {cutAfter(exampleJWT), nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc := c.doc
			p := storeProvider(t, c.env, nil)
			writeDocument(t, p, doc)
			_, err := ask(t, p, llm.Request{})
			assertNoSecret(t, err, doc)
			if !strings.Contains(err.Error(), "invalid document") {
				t.Errorf("the CLI's own account of the refusal was scrubbed away: %v", err)
			}
			if kind := llm.KindOf(err); kind != llm.KindAuth {
				t.Errorf("kind = %v, want auth: the entry's login is unusable until somebody "+
					"signs in again, and the chain should move on", kind)
			}
		})
	}
}

// AN EXPORT THAT FAILS REPEATS NONE OF WHAT IT PRINTED, even where its stderr
// quotes it: the stdout of an export is every credential the CLI holds.
func TestAFailedExportRepeatsNoneOfWhatItPrinted(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{
		"FAKE_STORE_EXPORT_FAIL": "1", "FAKE_STORE_EXPORT_QUOTE": "1",
	}, nil)
	home := t.TempDir()
	db := filepath.Join(home, ".local", "share", "fake", "db.json")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	creds, err := json.Marshal([]map[string]string{
		{"id": "cred_1", "access": exampleJWT, "refresh": exampleRefresh},
		{"id": "cred_2", "key": exampleGoogle},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, creds, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = p.exportLogin(t.Context(), home, t.TempDir())
	assertNoSecret(t, err, string(creds))
	if !strings.Contains(err.Error(), "export broke") {
		t.Errorf("the CLI's own stderr was dropped: %v", err)
	}
	var exit *auxiliaryExitError
	if !errors.As(err, &exit) {
		t.Errorf("a non-zero exit is not an auxiliaryExitError: %T", err)
	}
}

// A COMMAND THAT RAN OUT OF TIME REPEATS NONE OF IT EITHER — its partial output
// on stdout and what it was handed on stdin are scrubbed from its stderr alike
// — and it is a DEADLINE to everything that classifies one.
func TestATimedOutCommandRepeatsNoneOfTheLogin(t *testing.T) {
	t.Parallel()
	document := exampleDocument(t)
	res := &rawResult{
		timedOut: true,
		// A partial export, cut off mid-document.
		stdout: document[:len(document)/2],
		stderr: "still working on " + document + "\nlast read " + exampleGoogle,
	}
	err := auxiliaryFailure("opencode auth import", res, document)
	assertNoSecret(t, err, document)
	var timeout *auxiliaryTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("a timed-out command is not an auxiliaryTimeoutError: %T", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("a timed-out command is not a context.DeadlineExceeded")
	}
	if kind := acquireFailureKind(fmt.Errorf("seeding: %w", err)); kind != llm.KindTimeout {
		t.Errorf("kind = %v, want timeout", kind)
	}
}

// THE SCRUB TAKES OUT WHAT WAS HANDED AND NOTHING ELSE, so a CLI's own account
// of a failure stays readable — and redact.Secrets still runs over what is
// left, for a credential the command was never handed.
func TestTheScrubLeavesTheHarnessOwnWords(t *testing.T) {
	t.Parallel()
	document := exampleDocument(t)
	stray := "sk-ant-api03-" + strings.Repeat("Z", 40)
	got := scrubAuxiliary("Missing key at [0][\"integrationID\"] in "+exampleJWT+" and "+stray, document)
	for _, want := range []string{"Missing key", "integrationID", scrubMarker} {
		if !strings.Contains(got, want) {
			t.Errorf("the scrubbed stderr lost %q: %q", want, got)
		}
	}
	if strings.Contains(got, exampleJWT) || strings.Contains(got, stray) {
		t.Errorf("a credential survived the scrub: %q", got)
	}
	if !strings.Contains(got, redact.Marker) {
		t.Errorf("redact.Secrets did not run over what was left: %q", got)
	}
}

// A PIECE OF A HANDED CREDENTIAL IS SCRUBBED, not only the whole of one. A CLI
// that cannot parse its input quotes the window it stopped in, which takes one
// token whole out of a truncated document — matched by no whole-text
// replacement, since it is not the document — or cuts one wherever the window
// begins, matched by no whole token either.
//
// Mutation: drop the token runs of a text that is not JSON (secretsIn), and
// both are red; match a token run only whole (maskShared), and the cut one is.
func TestAPieceOfAHandedCredentialIsScrubbed(t *testing.T) {
	t.Parallel()
	document := exampleDocument(t)
	truncated := document[:strings.Index(document, exampleRefresh)+len(exampleRefresh)/2]
	if json.Valid([]byte(truncated)) {
		t.Fatal("the premise: the truncated document is not JSON")
	}
	for name, quoted := range map[string]string{
		"a token whole":       exampleJWT,
		"a token cut by half": exampleJWT[len(exampleJWT)/2:],
	} {
		got := scrubAuxiliary("unexpected token near \""+quoted+"\" at offset 112", truncated)
		if strings.Contains(got, quoted) {
			t.Errorf("%s: the scrub left it: %q", name, got)
		}
		if !strings.Contains(got, scrubMarker) || !strings.Contains(got, "at offset 112") {
			t.Errorf("%s: the scrub did not leave a marker among the CLI's own words: %q", name, got)
		}
	}
	// A piece shorter than a token run is a word, and the CLI's own: kept.
	if got := scrubAuxiliary("near "+exampleJWT[:minTokenRun-1]+"!", truncated); !strings.Contains(got, exampleJWT[:minTokenRun-1]) {
		t.Errorf("a piece shorter than a token run was taken out: %q", got)
	}
}

// A CALL'S OWN DEADLINE DURING THE IMPORT IS THE CALL'S DEADLINE: classified as
// a timeout, as the same deadline during the call is, and never reported as
// the import "not finishing within 30s" — it was stopped long before.
func TestADeadlineDuringTheImportIsATimeout(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_SLEEP_MS": "5000"}, nil)
	p.termGrace = witnessGrace
	writeDocument(t, p, `[{"id":"cred_1","key":"k"}]`)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := p.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("a call whose import outlived its deadline succeeded")
	}
	if kind := llm.KindOf(err); kind != llm.KindTimeout {
		t.Errorf("kind = %v, want timeout: a slow host is not a request nobody can serve", kind)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the failure is not the deadline that caused it: %v", err)
	}
	if strings.Contains(err.Error(), auxiliaryTimeout.String()) {
		t.Errorf("the failure claims the import's own %s bound ran out: %v", auxiliaryTimeout, err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Errorf("the call took %s, not its own deadline", elapsed)
	}
}

// A SEAT THAT COULD NOT BE PREPARED IS CLASSIFIED BY WHAT FAILED, because the
// fallback chain stops on a fatal and moves on from anything else.
func TestAcquireFailuresAreClassifiedByWhatFailed(t *testing.T) {
	t.Parallel()
	exit := &auxiliaryExitError{command: "cli export", code: 1}
	for name, c := range map[string]struct {
		err  error
		want llm.ErrorKind
	}{
		"the auxiliary bound":     {&auxiliaryTimeoutError{command: "cli import"}, llm.KindTimeout},
		"the call's own deadline": {fmt.Errorf("x: %w", context.DeadlineExceeded), llm.KindTimeout},
		"the call cancelled":      {fmt.Errorf("x: %w", context.Canceled), llm.KindTimeout},
		"a lock held too long":    {&hostbox.LockWaitError{Path: "credentials.lock", Wait: time.Second}, llm.KindTimeout},
		"an import refused":       {&loginRefusedError{path: "doc", err: exit}, llm.KindAuth},
		"an export that failed":   {fmt.Errorf("carrying out: %w", exit), llm.KindServer},
		"an export not seen to its end": {
			fmt.Errorf("carrying out: %w", &auxiliaryUnfinishedError{command: "cli export", why: "held"}),
			llm.KindServer,
		},
		"the filesystem":           {os.ErrPermission, llm.KindFatal},
		"a shared file not a file": {errors.New("harness: shared login is not a regular file"), llm.KindFatal},
	} {
		if got := acquireFailureKind(c.err); got != c.want {
			t.Errorf("%s: kind = %v, want %v", name, got, c.want)
		}
	}
}

// A SHARED LOGIN THAT IS NOT A FILE IS THIS HOST'S PROBLEM, and stays fatal:
// no other member of the chain is refused it, but no retry of this one clears
// it either.
func TestASharedLoginThatIsNotAFileIsFatal(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, nil, nil)
	if err := os.MkdirAll(p.ws.documentPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := ask(t, p, llm.Request{})
	if err == nil {
		t.Fatal("a call seeded from a directory ran")
	}
	if kind := llm.KindOf(err); kind != llm.KindFatal {
		t.Errorf("kind = %v, want fatal: %v", kind, err)
	}
}
