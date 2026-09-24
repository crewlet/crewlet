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
	"sync"
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

// found posts one bootstrap with the given code, as Jane.
func (r founderRig) found(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	return r.foundAs(t, code, "jane.founder", "jane@example.com")
}

// foundAs posts one bootstrap with the given code, login and address.
func (r founderRig) foundAs(t *testing.T, code, login, email string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"code": code, "login": login, "email": email,
		"name": "A Founder", "password": "a-perfectly-fine-passphrase",
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

	// THE RE-ISSUE: the one code that works, and it is in the file.
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

// TWO FOUNDERS AT ONCE FOUND ONE COMPANY.
//
// A fleet offers one code per node, so two people can each hold a live one,
// and an enrolment is a sequence whose person records never share a subject:
// both used to land, and the company got two founders carrying the whole
// ceiling. Through the real route, the real writer and the real log, the
// founding takes the exemption on one subject now — exactly one lands, and the
// other is told why without a failed attempt counted against it.
//
// THE INTERLEAVING IS A RACE HERE, so against the old founding (no take, a
// live code honoured at the person record) this goes red on most runs rather
// than every one — measured at eight of eleven, each with both founders
// landed. The interleavings that make it certain are held in place by
// internal/iamdomain's TestASecondCodeWaitsWhileAFoundingIsInProgress and
// TestAFoundingThatLapsedMidRequestNeverLandsBesideTheNext, which stop one
// founding at a chosen step; this is the same rule reached through every
// layer a founder's request crosses.
func TestTwoFoundersAtOnceFoundOneCompany(t *testing.T) {
	t.Parallel()
	r := newFounderRig(t)
	openBootstrap(t.Context(), r.auth, "node-a")

	// A SECOND NODE'S CODE on the same empty log — what a fleet's boot
	// offers beside this node's own.
	const second = "a-code-another-node-wrote-for-the-same-empty-company"
	sum := sha256.Sum256([]byte(second))
	id := hex.EncodeToString(sum[:])
	if _, err := r.engine.IAMWriter().MintBootstrap(t.Context(), iamdomain.BootstrapMint{
		ID: id, Verifier: id, MintedBy: "node-b",
		ExpiresAt: time.Now().Add(time.Hour),
		OpID:      "bootstrap-mint:" + id, Reason: "a fresh estate",
	}); err != nil {
		t.Fatalf("mint the second node's code: %v", err)
	}

	founders := []struct{ code, login, email string }{
		{r.code(t), "jane.founder", "jane@example.com"},
		{second, "john.founder", "john@example.com"},
	}
	answers := make([]*httptest.ResponseRecorder, len(founders))
	var wg sync.WaitGroup
	for i, f := range founders {
		wg.Go(func() { answers[i] = r.foundAs(t, f.code, f.login, f.email) })
	}
	wg.Wait()

	landed := 0
	for i, rec := range answers {
		switch {
		case rec.Code == http.StatusOK:
			landed++
		case rec.Code == http.StatusConflict &&
			(strings.Contains(rec.Body.String(), "bootstrap_in_progress") ||
				strings.Contains(rec.Body.String(), "bootstrap_closed")):
		default:
			t.Errorf("founder %d answered %d %s, want 200 or 409 in progress "+
				"or closed", i, rec.Code, rec.Body.String())
		}
	}
	if landed != 1 {
		t.Fatalf("%d founders landed, want exactly one", landed)
	}
	page, err := r.engine.IAM().People(t.Context(), iamdomain.PeopleQuery{})
	if err != nil {
		t.Fatalf("list the directory: %v", err)
	}
	enrolled := 0
	for _, row := range page.People {
		if row.Kind != "" {
			enrolled++
		}
	}
	if enrolled != 1 {
		t.Errorf("the directory holds %d enrolled people, want the one founder",
			enrolled)
	}
}
