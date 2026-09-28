package authapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT THE ONE ENROLMENT THIS SURFACE PERFORMS NAMES AS ITS AUTHORITY.
//
// It runs under the node's own writer, which holds fleet:operate and
// people:manage and nothing else — so the domain would refuse it on the
// writer's own grants. What makes it land is the basis it NAMES: the
// invitation a redemption spends, which its issuer was held to their own
// grants for. The domain holds the enrolment to that basis in its own
// snapshot; this case holds the surface to naming it.

// enrolmentRecorder is a writer that keeps the enrolment it was asked for.
type enrolmentRecorder struct {
	stubWriter
	got *iamdomain.Enrolment
	err error
}

func (w enrolmentRecorder) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Result, error) {

	*w.got = in
	if w.err != nil {
		return statelog.Result{}, w.err
	}
	return applied(statelog.Position{}), nil
}

// A REDEMPTION NAMES ITS INVITATION. Mutation: drop the field and the
// enrolment names none, which the domain holds to the node's own grants.
func TestARedemptionNamesItsInvitationAsTheAuthority(t *testing.T) {
	t.Parallel()
	var got iamdomain.Enrolment
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Directory = liveInvitation{}
		o.Writer = enrolmentRecorder{got: &got}
	}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/auth/invite/"+invitationID, strings.NewReader(
			`{"secret":"`+invitationSecret+`","login":"dana.sre","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Invitation != invitationID {
		t.Errorf("the redemption's enrolment names invitation %q, want the "+
			"invitation it redeems", got.Invitation)
	}
}
