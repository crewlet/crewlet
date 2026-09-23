package iamapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

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
// # The URL is returned exactly once
//
// The estate holds the invitation's ID, which IS the verifier: holding the
// link is holding the id. Nothing stores the URL and no route reads one back,
// so an invitation an administrator lost is re-issued rather than recovered —
// which is the same promise a minted token makes and for the same reason.
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
		httpjson.FailWith(w, http.StatusServiceUnavailable,
			httpjson.CodeNoExternalURL, map[string]string{
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
	id := uuid.Must(uuid.NewV7()).String()
	at, err := writer.Invite(r.Context(), iamdomain.InviteMint{
		ID: id, Email: address, Grants: in.Grants, Colleague: in.Colleague,
		ExpiresAt: s.now().Add(InviteWindow),
		OpID:      s.opIDFor(r, "invite:"+id),
		Reason:    reasonOr(in.Reason, "invited through /iam"),
	})
	if err != nil {
		s.answerWrite(w, r, at, err, nil)
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
		"position", at.String())
	httpjson.Write(w, http.StatusCreated, map[string]any{
		"id": id, "url": s.inviteURL(id),
		"expires_at": s.now().Add(InviteWindow),
		"position":   at.String(),
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
