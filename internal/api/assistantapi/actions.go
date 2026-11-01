package assistantapi

import (
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// report is `PUT /assistant/actions/{id}`: a page claiming a page action it was
// sent ({state: applying}, exactly once across tabs), then reporting what
// became of it. The report is the page's own statement: it moves the
// transcript and the resumed turn, which reads it as client-reported, and
// nothing else.
//
// THE OPERATION IS OPTIONAL HERE, and minted when absent: a claim retried under
// the key it was first sent with is the same claim, and one sent without a key
// is a new one the arbitration decides.
func (s *Decisions) report(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body wire.ActionReport
	if !readBody(w, r, &body) || !speaks(w, body.ProtocolVersion) {
		return
	}
	if len(body.Reason) > assistantdomain.TitleMaxBytes ||
		len(body.Receipt) > assistantdomain.TitleMaxBytes ||
		len(body.Revision) > assistantdomain.TitleMaxBytes {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "a report's revision, reason and receipt are each " +
				"one short line"})
		return
	}
	if !pageValid(w, body.PageContext) {
		return
	}
	key, ok := opkey.Key(w, r, time.Now())
	if !ok {
		return
	}
	answer, err := s.approver.Report(r.Context(), approval.Report{Action: id, Report: body,
		Key: key})
	if err != nil {
		s.refuse(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}
