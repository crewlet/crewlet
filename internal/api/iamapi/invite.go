package iamapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// InviteWindow is how long an invitation stays redeemable.
//
// A HUNDRED AND SIXTY-EIGHT HOURS — one week — and the number is the SECURITY
// HORIZON rather than a convenience: the link — its secret — is a bearer
// credential sitting in somebody's mailbox, so the window is how long a
// compromised mailbox yields an account. A week survives somebody being away without making the
// link a standing way in, and an administrator whose invitation aged out
// issues another in one call.
const InviteWindow = 168 * time.Hour

// inviteBody is what issuing an invitation accepts.
type inviteBody struct {
	Email     string        `json:"email"`
	Grants    []iam.Grant   `json:"grants"`
	Colleague iam.Colleague `json:"colleague"`

	// Seat is a seat redeeming the invitation BINDS the new person to, by
	// any handle the chart answers to it by, or empty. It must be a HUMAN
	// seat nobody is bound to; the invitation records the seat's identity,
	// so a rename before the redemption binds the same seat.
	Seat   string `json:"seat"`
	Reason string `json:"reason"`
}

// PostInvite is `POST /iam/invitations`.
//
// # The URL is returned by the operation that issued it, and by nothing else
//
// The link is `<api.external_url>/dashboard#/invite/<id>.<secret>` — the
// dashboard's invitation screen, with the credential in the fragment a
// browser never sends — and what the estate holds of the secret is its
// VERIFIER, so no route can read a link back and an invitation an
// administrator lost is re-issued rather than recovered. The one answer that
// carries it again is a RETRY of the issue itself — the same request under the
// same Idempotency-Key, which is what an unknown answer tells a caller to send
// — because the id is derived from that key, the secret from the id, and the
// retry is the same operation rather than a read.
//
// # It may bind a seat
//
// `seat` names a human seat nobody holds, and redeeming the invitation then
// binds the person it creates to that seat as the first step of the same
// enrolment — so an administrator onboarding somebody into a seat sends one
// link rather than inviting them and binding them afterwards. A seat that is
// not a human seat, one the chart does not hold, and one somebody is already
// bound to are refused here, naming the seat.
//
// # What it confers is decided HERE, by whoever issues it
//
// The grants and the reach travel on the invitation, so the redemption hands
// out exactly what was offered rather than deciding again — which is also what
// stops a redemption being a way to ask for more.
func (s *Service) PostInvite(w http.ResponseWriter, r *http.Request) {
	in, ok := readBody[inviteBody](w, r)
	if !ok {
		return
	}
	address := strings.TrimSpace(in.Email)
	if address == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "an invitation needs the address it " +
				"is for: that address is what it arbitrates on, so one with " +
				"none would contend with nothing and two would both win"})
		return
	}
	if s.external == "" {
		// NAMED RATHER THAN A BROKEN LINK. The alternative is answering
		// a relative path, which looks like a working invitation right
		// up to the moment somebody clicks it in their mail client.
		//
		// A 500 AND NOT A 503: this node's own configuration lacks the
		// setting, and no amount of waiting supplies it — a 503 told
		// every client to retry in two seconds for ever.
		httpjson.FailWith(w, http.StatusInternalServerError,
			httpjson.CodeNoExternalURL, map[string]string{
				"config_path": "api.external_url",
				"detail": "this deployment has no api.external_url, so there " +
					"is no address an invitation link could point at. Set it " +
					"in this node's own configuration file and restart it.",
			})
		return
	}
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	// THE INVITATION IS THE OPERATION'S: its id is derived from the key
	// under the company's own, so a retry under the key an unknown answer
	// handed back hands back the link to the invitation its first attempt
	// issued — see [Service.createKey].
	opID, seed, ok := s.createKey(w, r)
	if !ok {
		return
	}
	issued, err := writer.Invite(r.Context(), iamdomain.InviteMint{
		Email: address, Grants: in.Grants, Colleague: in.Colleague,
		Seat:      strings.TrimSpace(in.Seat),
		ExpiresAt: s.now().Add(InviteWindow),
		// THE SEED, which the domain derives the invitation's id from and
		// publishes the issue under; the answer hands back the scoped key,
		// whose seed a retry reproduces — see [Service.createKey].
		OpID:   seed,
		Reason: reasonOr(in.Reason, "invited through /iam"),
	})
	invited, id := issued.Result, issued.ID
	if err != nil || !landed(invited) {
		// NO LINK FOR AN INVITATION NOBODY CAN CONFIRM: it would be a
		// URL that answers 410 the first time somebody follows it.
		s.answerWrite(w, r, opID, invited, err, nil)
		return
	}
	// THE ADDRESS IS LOGGED AS A HASH AND NEVER IN THE CLEAR. An
	// invitation's whole point is that the address is sealed at rest, and
	// a log line carrying it would put it back in every shipped log — but
	// an operator still needs to be able to confirm which invitation is
	// which, which a stable digest answers.
	digest := sha256.Sum256([]byte(iam.NormalizeEmail(address)))
	log.InfoContext(r.Context(), "iam_invitation_issued",
		"invitation", id, "address_digest", hex.EncodeToString(digest[:8]),
		"position", invited.Position.String())
	s.answer(w, r, opID, invited, nil, http.StatusCreated, map[string]any{
		"id": id, "url": s.inviteURL(id, issued.Secret),
		"expires_at": issued.ExpiresAt,
		"detail": "this link is shown once and cannot be read back; what the " +
			"estate holds is the invitation's id and a verifier of the secret " +
			"the link carries beside it, which is what redeeming it presents",
	})
}

// inviteRoute is the dashboard's invitation screen, as a fragment route of its
// shell — the screen that renders the invitation and redeems it.
const inviteRoute = "#/invite/"

// inviteURL is the link a person follows: the dashboard's invitation screen,
// carrying the id and the secret as `<id>.<secret>` in the FRAGMENT.
//
// THE DASHBOARD AND NOT THE API. The link used to be the JSON route itself,
// `/auth/invite/<id>`, so a person clicking it in their mail was shown a JSON
// document and no form to redeem it with. The screen calls that route itself,
// with the secret beside the id rather than in the path.
//
// IN THE FRAGMENT because a browser never sends one: neither half reaches a
// proxy's access log on the way to the page, where a path or a query string
// would have put the whole credential in every one of them. The two halves
// are joined by a dot, which neither can contain — the id is a uuid and the
// secret is unpadded URL-safe base64.
//
// BUILT FROM api.external_url, never from the request: the engine sits behind
// a TLS-terminating proxy and reads no scheme or host off one, so a link built
// from a request would carry whatever an internal load balancer called itself.
func (s *Service) inviteURL(id, secret string) string {
	return strings.TrimRight(s.external, "/") + auth.PathDashboard +
		inviteRoute + id + "." + secret
}
