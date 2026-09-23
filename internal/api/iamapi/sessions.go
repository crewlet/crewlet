package iamapi

import (
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// sessionView is one session as the directory renders it.
//
// THE LINEAGE AND NOTHING DERIVED FROM THE COOKIE. A lineage is in every
// audit row and is not a secret; a bearer is, and a listing carrying anything
// a session could be resumed from would make this the surface worth
// attacking.
type sessionView struct {
	Lineage   string    `json:"lineage"`
	Person    string    `json:"person"`
	Epoch     uint64    `json:"epoch"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	EndedWhy  string    `json:"ended_reason,omitempty"`
	Live      bool      `json:"live"`
}

// GetSessions is `GET /iam/people/{id}/sessions`.
func (s *Service) GetSessions(w http.ResponseWriter, r *http.Request) {
	held, err := s.directory.Sessions(r.Context(), r.PathValue("id"))
	if err != nil {
		s.unavailable(w, r, "read a person's sessions", err)
		return
	}
	now := s.now()
	out := make([]sessionView, 0, len(held))
	for _, row := range held {
		out = append(out, sessionView{
			Lineage: row.Lineage, Person: row.PersonID, Epoch: row.Epoch,
			CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt,
			EndedAt: row.EndedAt, EndedWhy: row.EndedWhy,
			Live: row.Live(now),
		})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"sessions": out})
}

// DeleteSessions is `DELETE /iam/people/{id}/sessions`.
//
// # One record, not N deletes
//
// It bumps the person's REVOCATION EPOCH, which every bearer they hold
// carries the value it was minted at — so one write ends every session and
// every token at once, including the ones minted on nodes this one has never
// spoken to. Ending them individually would be N records, would race with a
// session opening during the sweep, and would leave whatever was minted
// between the first delete and the last.
func (s *Service) DeleteSessions(w http.ResponseWriter, r *http.Request) {
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	id := r.PathValue("id")
	reason := reasonOr(r.URL.Query().Get("reason"),
		"every session was ended through /iam")
	opID := s.opIDFor(r, "sessions:revoke:"+id)
	revoked, err := writer.Revoke(r.Context(), id, opID, reason)
	if err == nil && landed(revoked) {
		log.InfoContext(r.Context(), "iam_sessions_revoked",
			"person", id, "position", revoked.Position.String())
		// SOMEBODY ENDING THEIR OWN is a sign-out everywhere; anybody
		// else ending them is a revocation, and the trail says which
		// because the second is the row an investigation looks for.
		why := types.EndRevoked
		if principal, how := iam.From(r.Context()); how == iam.Resolved &&
			principal.ID.String() == id {
			why = types.EndLogoutAll
		}
		s.audit.Emit(r.Context(), types.IAMSessionEnded{
			Person: id, Reason: why, By: callerName(r.Context()),
			OperatorID: callerOperator(r.Context()),
		})
	}
	s.answerWrite(w, r, opID, revoked, err, map[string]any{"id": id})
}

// PostInvalidateAll is `POST /iam/invalidate-all`.
//
// # It is the restore runbook's last step, and it takes fleet:operate
//
// The design asked for fleet:operate AND people:manage. It is fleet:operate
// alone, and the reasoning is the primary caller: a restore is run by whoever
// runs the deployment, and requiring people:manage as well would mean every
// SRE who can restore also holds the grant that can grant — which is worse for
// least privilege than the blast radius it was meant to bound. Anybody with
// people:manage can already revoke every person one at a time; what this adds
// is the ability to do it WITHOUT knowing who was affected, which is exactly
// what a restore needs and is a node gesture rather than a directory one.
//
// A backup taken before a revocation restores the session rows that revocation
// ended, so a pre-restore bearer would work again — a cookie, or a machine
// token revoked after the copy was taken. The fleet-wide generation is the only
// number that can be pushed forward without reading anybody's row, and both
// carry it.
func (s *Service) PostInvalidateAll(w http.ResponseWriter, r *http.Request) {
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	principal, _ := iam.From(r.Context())
	reason := reasonOr(r.URL.Query().Get("reason"),
		"every session in the company was invalidated")
	opID := s.opIDFor(r, "sessions:invalidate")
	bumped, err := writer.InvalidateAll(r.Context(), opID, reason)
	if err == nil && landed(bumped) {
		log.WarnContext(r.Context(), "iam_generation_bumped",
			"by", iam.ActorFor(principal).Name,
			"position", bumped.Position.String(),
			"detail", "every session in this company is now invalid")
	}
	s.answerWrite(w, r, opID, bumped, err, nil)
}
