package chartapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/chart"
)

// THE STRUCTURAL WRITE, and the one thing a caller has to understand about it.
//
// ONE BATCH IS ONE RECORD, and the domain serialises it against every other
// structural write in the company — deliberately, because two reparents
// through a common ancestor can each be locally valid and jointly produce a
// cycle no node could see from the subject it arbitrated on. So a caller that
// means to move three seats sends three operations in ONE batch rather than
// three requests: the batch is the unit that is ordered, and three requests
// are three chances to land half a reorganisation.

// batchBody is one structural change as a caller sends it.
type batchBody struct {
	Operations []batchOp `json:"operations"`

	// Reason rides a removal into its tombstone, so somebody asking where
	// their team went reads "merged into infrastructure" rather than an
	// absence.
	Reason string `json:"reason,omitempty"`
}

type batchOp struct {
	Kind   string `json:"kind"`
	Object struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"object"`
	Parent string `json:"parent,omitempty"`
	Lead   string `json:"lead,omitempty"`
}

// postBatch applies one structural change.
//
// # Why this route chooses between two domain verbs
//
// internal/chart publishes a placement and a removal as DIFFERENT RECORDS,
// and each refuses a batch stating the other: a removal installs a GATE, and
// a record that installed one for some of its objects and not others would
// make "does this record install a gate" a question about its payload —
// which is the one question that has to be answerable from the envelope
// alone.
//
// So the route reads what the batch carries and calls the verb that matches.
// A batch carrying BOTH is handed to the placement verb and refused there, in
// the domain's own words: this surface deciding that one for itself would be
// a second statement of a rule that already has one, and the two would
// disagree the day the domain's changed.
func (s *Service) postBatch(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[batchBody](w, r)
	if !ok {
		return
	}
	if len(body.Operations) == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "this batch names no operations, so it " +
				"would publish a record that changes nothing"})
		return
	}
	ops := make([]chart.Operation, 0, len(body.Operations))
	removals := 0
	for _, op := range body.Operations {
		kind := chart.OperationKind(op.Kind)
		if kind == chart.OpRemoveObject {
			removals++
		}
		ops = append(ops, chart.Operation{
			Kind:   kind,
			Object: chart.ObjectRef{Kind: chart.ObjectKind(op.Object.Kind), ID: op.Object.ID},
			Parent: op.Parent, Lead: op.Lead,
		})
	}
	batch := chart.Batch{Operations: ops, Reason: body.Reason}
	writer := s.writerFor(r)
	if removals == len(ops) {
		result, err := writer.WriteRemoval(r.Context(), s.opID(r), batch)
		s.answerWrite(w, result, err)
		return
	}
	result, err := writer.WriteBatch(r.Context(), s.opID(r), batch)
	s.answerWrite(w, result, err)
}
