package iamapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// inviteBody is what issuing an invitation accepts.
type inviteBody struct {
	Email  string      `json:"email"`
	Grants []iam.Grant `json:"grants"`

	// Seat is the human seat redeeming the invitation binds the new person
	// to, by its handle. REQUIRED: a person holds a human seat for as long
	// as they are here (ADR-0026), and the invitation holds this one — as
	// it holds its address — from its issue until it is redeemed,
	// cancelled or ages out, so it must be a seat nobody holds and no
	// other open invitation holds.
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
// # It binds a seat, and holds it until it is redeemed
//
// `seat` names the human seat the invitee will hold, and redeeming the
// invitation creates them bound to it in one record. It is REQUIRED — `400
// seat_required` before anything is minted where it names none — because a
// person holds a human seat for as long as they are here (ADR-0026). From its
// issue until it is redeemed, cancelled or ages out the invitation HOLDS that
// seat as it holds its address, so nothing else is bound to it or invited onto
// it meanwhile, and a company write cannot take it away. A seat that is not a
// human seat and one the chart does not hold are refused here; one somebody is
// bound to, or another open invitation holds, is `409` naming which.
//
// The link lives [credential.EnrolmentLinkLifetime].
//
// # What it confers is decided HERE, by whoever issues it
//
// The grants travel on the invitation, so the redemption hands
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
				"is for: it is what the directory holds the invitation against " +
				"every person and every open invitation by"})
		return
	}
	// TRIMMED, and asked BEFORE THE KEY IS READ, so a refusal the caller
	// fixes by typing a seat mints nothing and publishes nothing.
	seat := strings.TrimSpace(in.Seat)
	if seat == "" {
		refuseSeatless(w, "an invitation names the human seat its invitee "+
			"will hold — name a vacant one (GET /iam/seats?unheld=true lists "+
			"them)")
		return
	}
	if !s.linksPoint(w, "an invitation") {
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
		Email: address, Grants: in.Grants,
		Seat:      seat,
		ExpiresAt: s.now().Add(credential.EnrolmentLinkLifetime),
		// THE SEED, which the domain derives the invitation's id from and
		// publishes the issue under; the answer hands back the scoped key,
		// whose seed a retry reproduces — see [Service.createKey].
		OpID:   seed,
		Reason: reasonOr(in.Reason, byCaller(r.Context(), "invited")),
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
		"expires_at": issued.ExpiresAt, "seat": seat,
		"detail": "this link is shown once and cannot be read back; what the " +
			"estate holds is the invitation's id and a verifier of the secret " +
			"the link carries beside it, which is what redeeming it presents",
	})
}

// linksPoint reports whether this node can build a link a person follows —
// whether it has an `api.external_url` — and refuses the gesture naming the
// setting, false, where it has none. what is the link's kind, for the sentence.
//
// NAMED RATHER THAN A BROKEN LINK. The alternative is answering a relative
// path, which looks like a working link right up to the moment somebody clicks
// it in their mail client.
//
// A 500 AND NOT A 503: this node's own configuration lacks the setting, and no
// amount of waiting supplies it — a 503 told every client to retry in two
// seconds for ever.
func (s *Service) linksPoint(w http.ResponseWriter, what string) bool {
	if s.external != "" {
		return true
	}
	httpjson.FailWith(w, http.StatusInternalServerError,
		httpjson.CodeNoExternalURL, map[string]string{
			"config_path": "api.external_url",
			"detail": "this deployment has no api.external_url, so there " +
				"is no address " + what + " link could point at. Set it " +
				"in this node's own configuration file and restart it.",
		})
	return false
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
