package authapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iam"
)

// fifteen is a password fifteen characters long and on no blocklist: over the
// engine's floor of twelve, under a deployment's floor of twenty.
const fifteen = "fifteen-letters"

// withFloor is a Tier A stating the given
// `min_password_length` — zero being the engine's own floor.
func withFloor(o *authapi.Options, floor int) {
	o.Bootstrap.API.Auth.MinPasswordLength = floor
}

// THE DEPLOYMENT'S PASSWORD FLOOR IS THE ONE ENFORCED, AND THE ONE REPORTED.
//
// `api.auth.min_password_length` was validated, documented and enforced
// by nothing: every route that sets a password checked the engine's twelve, and
// both answers that tell a form what to refuse — `/auth/config` and the
// invitation's view — said twelve whatever the file said. So a company that
// asked for twenty accepted fifteen at every redemption, and a form built from
// the reported floor agreed with the route while both disagreed with the
// company.
//
// Mutation: pass the engine's floor at the redemption and it accepts
// fifteen; report the engine's floor from either answer and it says 12. The control is the same requests under the engine's own floor, which
// all succeed — so the refusals are about the floor and nothing else.
func TestTheDeploymentsPasswordFloorIsTheOneEnforcedAndReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		floor  int
		report int
		status int
	}{
		{"a floor of twenty refuses fifteen", 20, 20, http.StatusBadRequest},
		{"the engine's own floor takes fifteen (the control)", 0,
			iam.MinPasswordChars, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// THE REDEMPTION.
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				withFloor(o, tc.floor)
				o.Directory = sealedInvitation{}
				o.Sealer = stubSealer{address: "dana@example.com"}
			}).Routes(mux)
			redeemed := postJSON(t, mux, "/auth/invite/"+invitationID,
				map[string]string{"secret": invitationSecret,
					"login": "dana.sre", "name": "Dana", "password": fifteen})
			assertFloorAnswer(t, "the redemption", redeemed, tc.status, tc.floor)
			if got := reportedFloor(t, mux, "/auth/invite/"+invitationID); got != tc.report {
				t.Errorf("the invitation's view reports a floor of %d, want %d",
					got, tc.report)
			}
			if got := reportedFloor(t, mux, "/auth/config"); got != tc.report {
				t.Errorf("/auth/config reports a floor of %d, want %d", got,
					tc.report)
			}
		})
	}
}

// postJSON posts one body to a path and answers the whole response.
func postJSON(t *testing.T, mux *http.ServeMux, path string,
	body map[string]string) *httptest.ResponseRecorder {

	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(string(raw))))
	return rec
}

// assertFloorAnswer holds one password-setting answer to the status a floor
// case expects — and a refusal to naming the floor it enforced, because the
// person choosing a password has to know what would satisfy it.
func assertFloorAnswer(t *testing.T, what string, rec *httptest.ResponseRecorder,
	status, floor int) {

	t.Helper()
	if rec.Code != status {
		t.Errorf("%s answered %d, want %d (body %s)", what, rec.Code, status,
			rec.Body.String())
		return
	}
	if status == http.StatusOK {
		return
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	detail, _ := body["detail"].(string)
	if want := "minimum is " + strconv.Itoa(floor); !strings.Contains(detail, want) {
		t.Errorf("%s refused with %q, which does not say %q", what, detail, want)
	}
}

// reportedFloor is the `min_password_length` a GET answers.
//
// THE INVITATION'S SECRET RIDES ON EVERY ASK, which the view reads and the
// posture read ignores: one helper for both surfaces the floor is reported on.
func reportedFloor(t *testing.T, mux *http.ServeMux, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set(secretHeader, invitationSecret)
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d (body %s)", path, rec.Code, rec.Body.String())
	}
	var body struct {
		Floor int `json:"min_password_length"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return body.Floor
}

// AN UNSET SECOND FACTOR IS REPORTED AS REQUIRED, because it is: a sign-in
// form reads `/auth/config` before anybody signs in, and a posture read that
// said nothing for the unset value — as it did while a deployment could switch
// password sign-in off — would draw a form that never mentions the factor the
// sign-in is about to ask for. And the posture read names no "backend": password
// sign-in is always served.
//
// Mutation: report `api.auth.totp` as written rather than with its default and
// the unset case reads "".
func TestThePostureReadReportsTheSecondFactorWithItsDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state iam.SecondFactor
		want  iam.SecondFactor
	}{
		{"unset is required", "", iam.SecondFactorRequired},
		{"required as written", iam.SecondFactorRequired, iam.SecondFactorRequired},
		{"optional as written (the control)", iam.SecondFactorOptional,
			iam.SecondFactorOptional},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				requiring(o, tc.state)
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/config", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /auth/config answered %d", rec.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if got := body["second_factor"]; got != string(tc.want) {
				t.Errorf("second_factor = %v, want %q", got, tc.want)
			}
			if _, named := body["backend"]; named {
				t.Errorf("the posture read still names a backend: %s",
					rec.Body.String())
			}
		})
	}
}
