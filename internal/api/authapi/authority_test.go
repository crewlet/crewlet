package authapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT THE TWO ENROLMENTS THIS SURFACE PERFORMS NAME AS THEIR AUTHORITY.
//
// Both run under the node's own writer, which holds fleet:operate and
// people:manage and nothing else — so the domain would refuse either one on
// the writer's own grants. What makes them land is the basis each NAMES: the
// invitation a redemption spends, and the one-time code the first person
// redeems. The domain holds the enrolment to that basis in its own snapshot;
// these cases hold the surface to naming it.

// enrolmentRecorder is a writer that keeps the enrolment it was asked for.
type enrolmentRecorder struct {
	stubWriter
	got *iamdomain.Enrolment
	err error
}

func (w enrolmentRecorder) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Position, error) {

	*w.got = in
	return statelog.Position{}, w.err
}

// A REDEMPTION NAMES ITS INVITATION. Mutation: drop the field and the
// enrolment names none, which the domain holds to the node's own grants.
func TestARedemptionNamesItsInvitationAsTheAuthority(t *testing.T) {
	t.Parallel()
	var got iamdomain.Enrolment
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
		o.Directory = liveInvitation{}
		o.Writer = enrolmentRecorder{got: &got}
	}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/auth/invite/inv-1", strings.NewReader(
			`{"login":"dana.sre","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Invitation != "inv-1" || got.BootstrapCode != "" {
		t.Errorf("the redemption's enrolment names invitation %q and code %q, "+
			"want the invitation it redeems and no code", got.Invitation,
			got.BootstrapCode)
	}
}

// THE FIRST PERSON NAMES THE CODE, and a record that refuses the exemption is
// the closed answer rather than an outage.
//
// Mutation: drop the field and the enrolment names no code; map the refusal to
// the shared helper and it answers 503, which tells the first operator to try
// again at a door that has closed for good.
func TestTheFirstPersonNamesTheCodeAsTheAuthority(t *testing.T) {
	t.Parallel()
	const code = "a-one-time-code-somebody-read-off-the-host"
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"the record accepts it", nil, http.StatusOK},
		{"the exemption closed at the record",
			fmt.Errorf("%w: this company already has somebody in it",
				iamdomain.ErrRefused),
			http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := bootstrapFor(t)
			dir := t.TempDir()
			b.Store.Path = filepath.Join(dir, "node.db")
			if err := os.WriteFile(filepath.Join(dir, authapi.BootstrapCodeFile),
				[]byte(code+"\n"), 0o600); err != nil {
				t.Fatalf("write the code: %v", err)
			}
			var got iamdomain.Enrolment
			mux := http.NewServeMux()
			buildWith(t, b, nil, func(o *authapi.Options) {
				o.Writer = enrolmentRecorder{got: &got, err: tc.err}
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/auth/bootstrap", strings.NewReader(`{"code":"`+code+
					`","login":"jane.doe","email":"jane@example.com",`+
					`"name":"Jane","password":"a-perfectly-fine-passphrase"}`)))
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status,
					rec.Body.String())
			}
			sum := sha256.Sum256([]byte(code))
			if got.BootstrapCode != hex.EncodeToString(sum[:]) ||
				got.Invitation != "" {
				t.Errorf("the first person's enrolment names code %q and "+
					"invitation %q, want the code's own id and no invitation",
					got.BootstrapCode, got.Invitation)
			}
		})
	}
}
