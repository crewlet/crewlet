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
// HORIZON rather than a convenience: the link is a bearer credential sitting
// in somebody's mailbox, so the window is how long a compromised mailbox
// yields an account. A week survives somebody being away without making the
// link a standing way in, and an administrator whose invitation aged out
// issues another in one call.
const InviteWindow = 168 * time.Hour

// inviteBody is what issuing an invitation accepts.
type inviteBody struct {
	Email     string        `json:"email"`
	Grants    []iam.Grant   `json:"grants"`
	Colleague iam.Colleague `json:"colleague"`
	Reason    string        `json:"reason"`
}

// PostInvite is `POST /iam/invitations`.
//
// # The URL is returned by the operation that issued it, and by nothing else
//
// The estate holds the invitation's ID, which IS the verifier: holding the
// link is holding the id. No route reads one back, so an invitation an
// administrator lost is re-issued rather than recovered. The one answer that
// carries it again is a RETRY of the issue itself — the same request under the
// same Idempotency-Key, which is what an unknown answer tells a caller to send
// — because the id is derived from that key and the retry is the same
// operation rather than a read.
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
		// every client to retry in two seconds for ever, which is the
		// same reason authapi's provider start answers a fault as one.
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
	opID, ok := s.createKey(w, r)
	if !ok {
		return
	}
	issued, err := writer.Invite(r.Context(), iamdomain.InviteMint{
		Email: address, Grants: in.Grants, Colleague: in.Colleague,
		ExpiresAt: s.now().Add(InviteWindow),
		OpID:      opID,
		Reason:    reasonOr(in.Reason, "invited through /iam"),
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
		"id": id, "url": s.inviteURL(id),
		"expires_at": issued.ExpiresAt,
		"detail": "this link is shown once and cannot be read back; what the " +
			"estate holds is the invitation's id, which is what redeeming it " +
			"presents",
	})
}

// inviteURL is the link a person follows.
//
// BUILT FROM api.external_url, never from the request: the engine sits behind
// a TLS-terminating proxy and reads no scheme or host off one, so a link built
// from a request would carry whatever an internal load balancer called itself.
func (s *Service) inviteURL(id string) string {
	return strings.TrimRight(s.external, "/") + auth.AuthInvitePrefix + id
}
