package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE FOUNDER'S WAY IN, END TO END: a real node, its own identity log and its
// own sign-in surface, driven the way an install that sat over a weekend is.
//
// Every half has its own cases over fakes; what only a real node can say is
// that they compose — that the boot offer, the route and the re-issue ask the
// same log the record decides on, so a code the route calls live is one the
// record honours and a refused attempt leaves the route open.

// founderRig is one node serving the first-person route.
type founderRig struct {
	engine *engine.Engine
	auth   *authapi.Service
	mux    *http.ServeMux
	file   string
}

func newFounderRig(t *testing.T) founderRig {
	t.Helper()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if e.IAMWriter() == nil {
		t.Fatal("the node runs no identity domain")
	}
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	surface, _, err := signInSurface(boot, e, cipher)
	if err != nil || surface == nil {
		t.Fatalf("sign-in surface: %v", err)
	}
	mux := http.NewServeMux()
	surface.Routes(mux)
	return founderRig{engine: e, auth: surface, mux: mux,
		file: filepath.Join(filepath.Dir(boot.Store.Path), authapi.BootstrapCodeFile)}
}

// code is what the node's file holds now.
func (r founderRig) code(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(r.file)
	if err != nil {
		t.Fatalf("read the code file: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// found posts one bootstrap with the given code.
func (r founderRig) found(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"code": code, "login": "jane.founder", "email": "jane@example.com",
		"name": "Jane Founder", "password": "a-perfectly-fine-passphrase",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/bootstrap",
		strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.7:4711"
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// started is the one bit every party asks.
func (r founderRig) started(t *testing.T) bool {
	t.Helper()
	held, err := r.engine.IAM().AnyPerson(t.Context())
	if err != nil {
		t.Fatalf("AnyPerson: %v", err)
	}
	return held
}

// A WEEKEND-OLD CODE, A RESTART, A RE-ISSUE AND THE FOUNDER.
//
// The code in the file aged out on the log. Before: the boot kept the file and
// advertised it, the route refused it as a wrong code, the record — reached
// after the founder's address and login were claimed — refused it as a started
// company, and that reservation closed the route for good. Now the refusal is
// `410 bootstrap_code_stale` with nothing claimed, the route stays open, a
// restart replaces the dead file, a re-issue ends it, and the founder walks in
// with the code the file holds at the end.
func TestAWeekendOldFounderCodeIsReplacedAndTheFounderWalksIn(t *testing.T) {
	t.Parallel()
	r := newFounderRig(t)

	// THE FILE HOLDS A CODE THE LOG AGED OUT — what a node restarted a
	// day after its first boot found.
	const aged = "a-code-minted-on-friday-that-the-log-has-aged-out"
	sum := sha256.Sum256([]byte(aged))
	id := hex.EncodeToString(sum[:])
	if _, err := r.engine.IAMWriter().MintBootstrap(t.Context(), iamdomain.BootstrapMint{
		ID: id, Verifier: id, MintedBy: "node-a",
		ExpiresAt: time.Now().Add(-time.Hour),
		OpID:      "bootstrap-mint:" + id, Reason: "a fresh estate",
	}); err != nil {
		t.Fatalf("mint the Friday code: %v", err)
	}
	if err := os.WriteFile(r.file, []byte(aged+"\n"), 0o600); err != nil {
		t.Fatalf("write the Friday code: %v", err)
	}

	rec := r.found(t, aged)
	if rec.Code != http.StatusGone ||
		!strings.Contains(rec.Body.String(), "bootstrap_code_stale") {
		t.Fatalf("the aged-out code answered %d %s, want 410 "+
			"bootstrap_code_stale", rec.Code, rec.Body.String())
	}
	if r.started(t) {
		t.Fatal("a refused bootstrap started the company, closing the only " +
			"way into it")
	}

	// THE RESTART: a dead file is replaced, never advertised.
	openBootstrap(t.Context(), r.auth, "node-a")
	restarted := r.code(t)
	if restarted == aged {
		t.Fatal("the restart kept the aged-out code in the file")
	}

	// THE RE-ISSUE: exactly one live code, and it is in the file.
	if _, err := r.auth.ReissueBootstrapCode(t.Context(), "node-a"); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	reissued := r.code(t)
	if rec := r.found(t, restarted); rec.Code != http.StatusGone {
		t.Errorf("the code the re-issue withdrew answered %d %s, want 410",
			rec.Code, rec.Body.String())
	}
	if rec := r.found(t, reissued); rec.Code != http.StatusOK {
		t.Fatalf("the founder was refused on the re-issued code: %d %s",
			rec.Code, rec.Body.String())
	}
	if !r.started(t) {
		t.Error("the founder's bootstrap did not start the company")
	}

	// AND THE HOST KEEPS NO CLAIM: the redemption removed the file, and a
	// later boot on a started company writes none.
	openBootstrap(t.Context(), r.auth, "node-a")
	if _, err := os.Stat(r.file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a started company's node holds a code file (%v)", err)
	}
	if _, err := r.auth.ReissueBootstrapCode(t.Context(),
		"node-a"); !errors.Is(err, iamdomain.ErrBootstrapClosed) {
		t.Errorf("a re-issue on a started company answered %v", err)
	}
}
