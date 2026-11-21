package googlecloud

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// The cloud identity's tokens are issued by [googleauth.SeatIssuer] with
// [googleauth.ScopeCloudPlatform]; what follows reads its refusals in the
// Google Cloud connection's words.

// GrantSettled is when Google has surely applied Crewlet's grant on rec's
// account: [GrantSettle] after the account was made or after the lane's last
// recorded grant, whichever is later. Before it, Google refusing the token is
// Google still applying the grant ([StillGranting]).
func GrantSettled(rec seataccess.Record) time.Time {
	var at time.Time
	if rec.Account != nil {
		at = rec.Account.CreatedAt.Add(GrantSettle)
	}
	if s := rec.Lanes[seataccess.LaneGoogleCloud].Settling; s != nil && s.Reason == ReasonGranting && s.Until.After(at) {
		at = s.Until
	}
	return at
}

// StillGranting reports a seat token Google refused as it refuses one whose
// grant it has not applied yet: 403 or 404 from the mint itself, never from
// reaching Google as the provisioner, which is the connection's fault.
func StillGranting(err error) bool {
	if googleauth.IsStage(err) {
		return false
	}
	status := googleauth.Status(err)
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// TokenRefusal is why Google would not issue a seat's token, as a clause that
// says what to fix, and whether a wait clears it: Google not reached, its rate
// limit or a 5xx. Never the token: an [*googleauth.APIError] carries none.
func TokenRefusal(err error) (string, bool) {
	if st, ok := errors.AsType[*googleauth.StageError](err); ok {
		if googleauth.Passes(st.Err) {
			return "Google did not answer Crewlet signing in as the provisioner: " + st.Err.Error(), true
		}
		said := "Google refused Crewlet signing in as the provisioner at " + string(st.Stage) + " (" +
			st.Err.Error() + "): run the Cloud Shell block on the Google Cloud connection again, then press Test"
		return said, false
	}
	if googleauth.Passes(err) {
		return err.Error(), true
	}
	api, ok := errors.AsType[*googleauth.APIError](err)
	switch {
	case !ok:
		return googleauth.Sentence(err), false
	case api.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(api.Message), "disabled"):
		return "Google says the agent's service account is disabled: turn the agent back on from the " +
			"Google Cloud Agents tab", false
	case StillGranting(err):
		return "Google refused the agent's token (" + api.Answer() + "): Crewlet's grant on its service " +
			"account is missing, or the account is gone; Retry the agent on the Google Cloud Agents tab", false
	}
	return "Google refused the agent's token: " + api.Answer(), false
}
