package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// liveInvitation is a directory holding one invitation that can still be
// redeemed.
type liveInvitation struct{ stubDirectory }

func (liveInvitation) InvitationByID(context.Context, string) (iamdomain.InvitationRow, error) {
	return iamdomain.InvitationRow{
		ID: "inv-1", Blind: "email:dana@example.com", InvitedBy: "founder",
		ExpiresAt: clock.Add(time.Hour),
	}, nil
}

// refusingWriter is a writer whose enrolment the domain refuses with err.
type refusingWriter struct {
	stubWriter
	err error
}

func (w refusingWriter) Enrol(context.Context, iamdomain.Enrolment) (statelog.Position, error) {
	return statelog.Position{}, w.err
}

// A REFUSED REDEMPTION SAYS WHOSE PROBLEM IT IS.
//
// Every enrolment failure used to answer `503 unavailable`, which says "the
// engine is having a moment, try again" — false for a login the domain refused,
// which no retry changes. The person holding the link was left resubmitting a
// name at a form that never said what was wrong with it; and with the login
// grammar now held per kind, `token:ops` is one of those refusals. So a value
// they typed is 400 naming the rule, a taken name is 409, and only what is
// left is 503.
//
// THE 409 DOES NOT NAME THE HOLDER. The domain's own refusal carries the
// holder's person id, which is right for an administrator and wrong for a
// caller whose only credential is an invitation link.
func TestARefusedRedemptionSaysWhoseProblemItIs(t *testing.T) {
	t.Parallel()
	const holder = "018f3a9c-0000-7000-8000-0000000000a1"
	for _, tc := range []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{"a login outside a person's grammar",
			fmt.Errorf("%w: \"token:ops\" is not a login a person may hold",
				iamdomain.ErrInvalidLogin),
			http.StatusBadRequest, "token:ops"},
		{"a login somebody holds",
			&iamdomain.ErrClaimed{Kind: iamdomain.KindLogin, Token: "dana.sre",
				Holder: holder},
			http.StatusConflict, "login is already taken"},
		{"an address somebody holds",
			&iamdomain.ErrClaimed{Kind: iamdomain.KindEmail, Token: "email:x",
				Holder: holder},
			http.StatusConflict, "address already belongs"},
		// A LINK THE RECORD REFUSES — spent or aged out between the
		// lookup and the enrolment — answers what the lookup would have:
		// one refusal for every way a link stops working.
		{"an invitation the record no longer honours",
			fmt.Errorf("%w: invitation inv-1 has already been used",
				iamdomain.ErrRefused),
			http.StatusGone, ""},
		// THE CONTROL: a failure that is not the caller's stays 503, or the
		// cases above would pass on a surface that answered 400 to
		// everything.
		{"a record that could not be published",
			errors.New("the broker did not acknowledge"),
			http.StatusServiceUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
				o.Directory = liveInvitation{}
				o.Writer = refusingWriter{err: tc.err}
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/auth/invite/inv-1", strings.NewReader(
					`{"login":"token:ops","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.status,
					rec.Body.String())
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			detail, _ := body["detail"].(string)
			if !strings.Contains(detail, tc.detail) {
				t.Errorf("detail %q, want it to carry %q", detail, tc.detail)
			}
			if strings.Contains(rec.Body.String(), holder) {
				t.Errorf("the refusal names the holder %s to somebody holding "+
					"only an invitation link: %s", holder, rec.Body.String())
			}
		})
	}
}
