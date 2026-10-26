package assistantapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
)

// decline is `POST /assistant/proposals/{id}/decline`: the proposal declined,
// nothing sent, and the turn told.
func (s *Decisions) decline(w http.ResponseWriter, r *http.Request) {
	id, body, key, ok := s.decision(w, r)
	if !ok {
		return
	}
	answer, err := s.approver.Decline(r.Context(), approval.Approval{Proposal: id,
		Digest: body.Digest, Key: key, Page: body.PageContext})
	if err != nil {
		s.refuse(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}
