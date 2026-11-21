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
	Email string `json:"email"`

	// Grants is what the invitee will hold once they redeem it. REQUIRED,
	// and `[]` is the overview alone — see [requireGrants] for why an
	// omission is refused rather than read as a default.
	Grants *[]iam.Grant `json:"grants"`

	// Seat is the human seat redeeming the invitation binds the new person
	// to, by its handle. REQUIRED: a person holds a human seat for as long
	// as they are here (ADR-0032), and the invitation holds this one — as
	// it holds its address — from its issue until it is redeemed,
	// cancelled or ages out, so it must be a seat nobody holds and no
	// other open invitation holds.
	Seat   string `json:"seat"`
	Reason string `json:"reason"`
}

// PostInvite is `POST /iam/invitations`.
//
// The URL is returned by the issuing operation and nothing else: the estate
// holds only a VERIFIER of the secret, so a lost invitation is re-issued. A
// RETRY under the same Idempotency-Key carries it again (the id derives from
// the key, the secret from the id). It lives [credential.EnrolmentLinkLifetime].
//
// `seat` is REQUIRED (`400 seat_required` before anything is minted): the
// invitee is bound to that human seat (ADR-0032), which the invitation HOLDS
// until redeemed, cancelled or aged out. A non-human or unknown seat is
// refused; a bound or held one is `409`. The grants are decided HERE, by the
// issuer, and travel on the invitation ([requireGrants]).
func (s *Service) PostInvite(w http.ResponseWriter, r *http.Request) {
	in, ok := readBody[inviteBody](w, r)
	if !ok {
		return
	}
	address := strings.TrimSpace(in.Email)
	if address == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "An invitation needs an email " +
				"address."})
		return
	}
	// TRIMMED, and asked BEFORE THE KEY IS READ, so a refusal the caller
	// fixes by typing a seat mints nothing and publishes nothing.
	seat := strings.TrimSpace(in.Seat)
	if seat == "" {
		refuseSeatless(w, "An invitation needs a human seat. Pick an empty "+
			"one (GET /iam/seats?unheld=true lists them).")
		return
	}
	// WHAT THEY WILL BE ABLE TO DO, STATED, before the key is read for the seat's
	// own reason.
	if !requireGrants(w, in.Grants, "an invitation") {
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
		Email: address, Grants: *in.Grants,
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
		"detail": "This link is shown only once. Send it to the person " +
			"yourself (Crewlet doesn't send email).",
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
			"detail": "No `api.external_url` is set, so " + what + " link " +
				"can't be made. Set it in this node's config file and restart.",
		})
	return false
}

// inviteRoute is the dashboard's invitation screen, as a fragment route of its
// shell — the screen that renders the invitation and redeems it.
const inviteRoute = "#/invite/"

// inviteURL is the link a person follows: the dashboard's invitation screen,
// carrying the id and the secret as `<id>.<secret>` in the FRAGMENT.
//
// THE DASHBOARD AND NOT THE API: the JSON route would show a person a document,
// no form. IN THE FRAGMENT because a browser never sends one, so no proxy log
// sees either half; the dot joining them is in neither (a uuid; unpadded
// URL-safe base64).
//
// BUILT FROM api.external_url, never from the request: the engine sits behind
// a TLS-terminating proxy and reads no scheme or host off one, so a request's
// would carry whatever an internal load balancer called itself.
func (s *Service) inviteURL(id, secret string) string {
	return strings.TrimRight(s.external, "/") + auth.PathDashboard +
		inviteRoute + id + "." + secret
}
