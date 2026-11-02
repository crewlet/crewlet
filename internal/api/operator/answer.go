package operator

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/crewlet/crewlet/internal/events/types"
)

// AnswerRecorder is a ResponseWriter that keeps the status a route answered
// with and the head of an error answer, where its code is, so the route's
// runtime audit record reads its outcome off the answer the route wrote
// rather than off a value each handler would have to remember to set.
//
// ONE, for every surface whose gestures are audited this way (the model
// routes, the MCP server routes): two copies of how an answer becomes an
// outcome are two audits that disagree about the same status.
type AnswerRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

// RecordAnswer wraps w.
func RecordAnswer(w http.ResponseWriter) *AnswerRecorder {
	return &AnswerRecorder{ResponseWriter: w}
}

// WriteHeader records the first status written.
func (w *AnswerRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write records the head of an error answer, where its code is: an envelope's
// `error` is one of its first keys whatever else it carries.
func (w *AnswerRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && w.body.Len() < maxRecordedAnswer {
		w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// maxRecordedAnswer bounds the head of an error answer kept: 64 KiB, far past
// any envelope's `error` key, which is among its first.
const maxRecordedAnswer = 64 << 10

// Outcome is what the answer says became of the call: applied below 400,
// failed for a server error (a 502 excepted, which is a vendor's refusal the
// route relayed), and refused otherwise, with the envelope's code.
func (w *AnswerRecorder) Outcome() (types.AuditOutcome, string) {
	switch {
	case w.status < 400:
		return types.AuditApplied, ""
	case w.status >= 500 && w.status != http.StatusBadGateway:
		return types.AuditFailed, ""
	}
	var envelope struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.body.Bytes(), &envelope)
	return types.AuditRefused, envelope.Error
}
